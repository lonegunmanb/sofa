# 分片 013：Harness 插件自包含缓存核验与 Podman 容器沙箱/`gatekeeper` 执行器

## 1. 目标与边界

实现容器启动前的插件资产准备与容器运行时物理隔离执行层：
1. **Harness 插件本地发现与远端锁定缓存（`internal/harness/installer`）**：
   - 本地模式（省略 `source` 与 `version`）：按优先级查找目标容器架构（如 `linux_amd64`）的自包含插件目录 `<dir>/sofa-harness-<name>/sofa-harness-<name>`；
   - 远端模式（声明 `source` + `version`）：严格对照 `plugins.lock.json` 中锁定的目标容器架构资产文件名与 `SHA-256` 下载、强核验并安全解压至宿主机缓存目录（确保入口可执行权限 `0755`）。
2. **Podman/容器运行时适配、`podman exec -i` 与 `gatekeeper` 执行器（`internal/sandbox/runtime`）**：
   - 构造阶段二总纲第 2 节定义的全部标准只读/读写挂载（含 `/workspace/.git:ro`、`writable_paths` 顶层目录子挂载、`/.sofa/{harness,tools,bundle,imports,artifacts,session-store,plugin}`）；
   - 提供供 `015` 复用的容器内命令执行原语 `Exec`（`podman exec -i`）以及独立容器 `RunGatekeepers` 执行器（校验退出码与 `require_clean_worktree`）。

**前置依赖**：分片 `010`、`011`、`012`。

---

## 2. 涉及包与文件清单

- `internal/harness/installer/installer.go`：`Installer`、`ResolveOptions`、`PreparedPlugin`、本地四级目录查找与远端缓存解压
- `internal/harness/installer/installer_test.go`：Tier 1 离线单测（覆盖本地优先级、远端锁定 SHA-256 命中/不匹配拒载、Zip-Slip 拦截）
- `internal/sandbox/runtime/spec.go`：`Engine` 接口、`Platform`、`SessionMountLayout`、`BuildRunArgs`（纯函数构造 CLI 参数列表，方便 Tier 1 精确断言！）
- `internal/sandbox/runtime/podman.go`：`PodmanEngine`（封装 `podman` / `docker` / `nerdctl` CLI）、`DetectPlatform`、`Exec`、`RunGatekeepers`
- `internal/sandbox/runtime/podman_test.go`：Tier 1 参数生成、`writable_paths` 子挂载计算与平台校验单测
- `internal/sandbox/runtime/podman_integration_test.go`（`//go:build integration`）：Tier 2 真实 Podman 容器只读/可写挂载隔离与 `gatekeeper` 集成测试

---

## 3. 保姆级实现细则

### 3.1 Harness 插件准备器（`internal/harness/installer/installer.go`）

```go
type Platform struct {
    OS   string // 固定为容器目标系统 "linux"
    Arch string // 如 "amd64" 或 "arm64"
}
func (p Platform) Key() string { return p.OS + "_" + p.Arch } // "linux_amd64"

type PreparedPlugin struct {
    Name          string // 如 "copilot"
    HostDir       string // 宿主机自包含插件目录绝对路径（将只读挂至 /.sofa/harness）
    ContainerPath string // 固定为 "/.sofa/harness/sofa-harness-" + Name
}
```

1. **本地插件目录发现（`cfg.Source == ""`）**：
   - 按严格优先级搜索以下候选根目录（跳过空路径）：
     1) CLI 显式传入的 `PluginDir`（对应 `--plugin-dir`）
     2) 环境变量 `SOFA_PLUGIN_DIR`（通过注入的 `LookupEnv` 读取）
     3) `<workingDir>/.sofa/plugins`
     4) `<userHomeDir>/.sofa/plugins`
   - 在每个候选 `<base>` 下依次检查 `<base>/sofa-harness-<name>/<platform.Key()>/sofa-harness-<name>` 与 `<base>/sofa-harness-<name>/sofa-harness-<name>`（兼容多架构子目录与单目录开发布局），找到常规文件即返回其所在父目录作为 `HostDir`；全部未找到则返回清晰错误列出已搜索路径。
2. **远端插件下载缓存（`cfg.Source != ""`）**：
   - 先调用 `harness.CanSatisfyHarnessLock(cfg, lockFile)` 校验锁表，再从锁条目 `Assets[platform.Key()]` 取出期望的 `Filename` 与 64 位小写 `SHA256`（缺失该架构资产则报错）；
   - 缓存目标路径：`<cacheRoot>/<cfg.SourceHost>/<cfg.SourceOwner>/<cfg.SourceRepo>/<cfg.Version>/<platform.Key()>/`；
   - 若缓存目录下 `.verified.sha256` 内容等于锁定 `SHA256` 且入口文件 `sofa-harness-<name>` 存在，直接命中返回；
   - 否则调用可注入的 `harness.ReleaseFetcher` 拉取 Release 资产字节 → 计算实际 `SHA-256` 与锁定值比对（不一致立即拒绝报错）→ 在临时目录通过 `os.OpenRoot` 安全解压 ZIP（拒绝 `..`、绝对路径、符号链接、超大小上限；保留常规文件可执行位并对顶层 `sofa-harness-<name>` 强制 `os.Chmod(..., 0o755)`）→ 写入 `.verified.sha256` 后原子 `os.Rename` 激活缓存目录。

### 3.2 容器挂载与 CLI 参数构造（`internal/sandbox/runtime/spec.go`）

1. **零环境变量密钥铁律**：
   - `BuildRunArgs` **严禁**接收任何模型 API Key、MCP 认证头或 Git Token 参数！仅设置固定非敏感运行时环境变量：`HOME=/tmp/sofa-home`、`TMPDIR=/tmp`、`GIT_OPTIONAL_LOCKS=0`（使容器内只读 `.git` 下 `git status` 不尝试写 `.git/index.lock`！）。
2. **`/workspace` 与 `writable_paths` 挂载计算（纯函数 `ComputeWorkspaceMounts`）**：
   - **QC 模式（`ReadOnlyWorkspace == true`）**：仅挂 `-v <hostWorkspace>:/workspace:ro`。
   - **Work 模式未设 `writable_paths`（`WritablePaths == nil`）**：
     - 先挂 `-v <hostWorkspace>:/workspace:rw`；
     - 紧接着追加覆盖挂载 `-v <hostWorkspace>/.git:/workspace/.git:ro`（顺序不可颠倒，后挂载覆盖前挂载！）。
   - **Work 模式设置了 `writable_paths`（非 `nil`）**：
     - 先挂根 `-v <hostWorkspace>:/workspace:ro`；
     - 提取每条白名单 glob 的受控挂载锚点：
       - 若形如 `"cmd/**"` 或 `"docs/spec.md"`（首段是子目录 `"cmd"` 或 `"docs"`）：若宿主机 `<hostWorkspace>/<topDir>` 尚不存在则先在宿主机 `os.MkdirAll` 创建该空目录（拒绝越出 `hostWorkspace` 或等于 `.git`），然后追加 `-v <hostWorkspace>/<topDir>:/workspace/<topDir>:rw`（自动去重）；
       - 若形如根级单文件 `"go.mod"`：若宿主机 `<hostWorkspace>/go.mod` 已存在且为普通文件，追加 `-v <hostWorkspace>/go.mod:/workspace/go.mod:rw`；
       - 若包含根级通配符（如 `"*.md"`）：回退为 `-v <hostWorkspace>:/workspace:rw` + `-v <hostWorkspace>/.git:/workspace/.git:ro`（交由宿主机 `012` 的 `FindUnauthorizedPaths` 门禁精确把关）；
     - 最后确保 `/workspace/.git` 始终以 `:ro` 挂载（禁止 `writable_paths` 包含 `.git` 或 `.git/**`）。
3. **固定系统挂载与安全标志**：
   - 统一追加：`--rm`、`--init`、`-w /workspace`、`--network <cfg.Network>`；
   - 若配置了 `cfg.Resources.CPUs` / `Memory`，追加 `--cpus` / `--memory`；
   - 追加用户声明的只读 `cfg.Mounts`（已在阶段一校验不冲突 `/workspace` 与 `/.sofa`）；
   - 按需挂载 `/.sofa/bundle:ro`、`/.sofa/imports:ro`、`/.sofa/artifacts:{rw|ro}`、`/.sofa/tools:ro`、`/.sofa/harness:ro`、`/.sofa/session-store:rw`；
   - **跨平台网络前置检查**：当容器用于运行 Agent 会话且 `hostOS != "linux"`（如 Windows / macOS）时，若 `cfg.Network == "none"`，立即返回明确错误（提示非 Linux 宿主机需通过 `127.0.0.1` 端口映射连接容器内插件，不支持 `network = "none"`）。

### 3.3 `Exec` 与 `RunGatekeepers`（`internal/sandbox/runtime/podman.go`）
1. **`Exec(ctx, runtimeBin, containerName, workDir string, command []string, env map[string]string, stdin io.Reader) (ExecResult, error)`**：
   - 执行 `<runtimeBin> exec -i -w <containerWorkDir> [-e K=V ...] <containerName> <command...>`，分离捕获 `Stdout`、`Stderr` 与真实 `ExitCode`（注意：非 0 退出码属于正常业务返回 `ExecResult{ExitCode: code}`，不要直接吞掉 `Stdout`/`Stderr`，仅在进程无法启动或 `ctx` 超时取消时返回 `error`）。
2. **`RunGatekeepers(ctx, spec GatekeeperRunSpec, gatekeepers []workerspec.GatekeeperConfig)`**：
   - 按声明顺序逐个执行 `gatekeeper`：
     - 为每个 `gatekeeper` 按其 `Timeout` 创建带超时子 `Context`；
     - 启动短生命周期独立容器：`--rm --init -w /workspace/<gk.WorkingDir> --entrypoint <gk.Command[0]>`（挂载 `/workspace:rw` + `/workspace/.git:ro` + `/.sofa/{bundle,imports,artifacts}:ro`），传入 `gk.Command[1:]`；
     - 核对真实退出码是否等于 `gk.ExpectExitCode`；
     - 若 `gk.RequireCleanTree == true`，紧接着在宿主机调用 `git status --porcelain=v1 -uall` 确认工作树与执行前一致无残留脏文件（若有脏文件则记录违例并调用 `RevertPaths` 清理恢复冻结状态！）；
     - 任一 `gatekeeper` 失败立即停止后续守门员，返回结构化 `GatekeeperFailure{Name, ExitCode, ExpectedExitCode, DirtyFiles, Stdout, Stderr}` 供打回 Work Session。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 插件准备器与沙箱参数单测**：
   - 本地插件 4 级目录覆盖优先级（`--plugin-dir` > `SOFA_PLUGIN_DIR` > `./.sofa/plugins` > `~/.sofa/plugins`）；
   - 远端插件首次下载校验锁定 `SHA-256` 成功、二次调用命中本地缓存不触发网络、篡改 ZIP 字节触发哈希不匹配报错、含 `../../evil` 条目拒载；
   - `BuildRunArgs` 全矩阵断言：验证无任何敏感 `-e` 参数、`/workspace/.git:ro` 永远位于挂载列表末尾覆盖保护、`writable_paths = ["docs/spec.md", "internal/**"]` 自动创建缺失的 `docs` 目录并生成 `:ro` 根 + `:rw` 子挂载、Windows 下 Agent 容器设 `network = "none"` 报错。
2. **Tier 2 真实 Podman 集成测试（`podman_integration_test.go`，`//go:build integration`）**：
   - 使用轻量基础镜像（如 `alpine:latest`）测试真实 Podman 挂载隔离：
     - 配置 `writable_paths = ["allowed/**"]`：容器内写 `/workspace/allowed/ok.txt` 成功，写 `/workspace/forbidden.txt` 和 `/workspace/.git/HEAD` 均真实触发 Linux 内核 `Read-only file system` 非 0 退出码！
     - 测试运行中容器调用 `Exec` 通过 `stdin` 传入 JSON 并读取 `stdout`；
     - 测试 `RunGatekeepers`：退出码匹配且工作树干净时通过；命令退出码不符、超时、或在 `require_clean_worktree = true` 下往 `/workspace` 偷写未跟踪垃圾文件时均精确拦截并自动清理残留文件。
