# 分片 040：`WorkerRunner` 宿主机执行闭环、QC `depth=1` 浅克隆隔离与单机端到端集成

## 1. 目标与边界

1. 在 `WorkerRunner` 中打通 `sandbox.Runtime == "host"` 的完整 Work 与 QC 会话生命周期：
   - Work 工作区采用 `<workspace>/.sofa/{artifacts,imports}` 布局，自动将 `/.sofa/` 写入 `<workspace>/.git/info/exclude`（并在冻结校验时禁止 `.sofa/**` 被强行 `git add -f` 入库）；
   - Work / QC 会话在 `host` 沙箱下直接按当前宿主机 `runtime.GOOS/GOARCH` 解析插件与编译 `go_tool`，使用 `NewHostSubprocessOpener` 启动插件，向 `OpenSessionRequest` 传递宿主机真实 `WorkingDirectory`、`SessionStoreDirectory` 与宿主机 `SkillDirectories`（含 `disabled_skills` 宿主机过滤目录与宽松模式 `skills` 过滤）；
   - `toolruntime.Executor` 的 `QuotePaths` 在 `host` 模式下支持校验工作区相对路径、`.sofa/imports/...` 以及宿主机 `BundleDir` / `SkillDirectories` 路径。
2. 实现 **QC 阶段 `git clone --depth=1 --no-local` 浅克隆物理隔离**：
   - 当 `qcSandbox.Runtime == "host"` 时，每次进入 QC 阶段（先跑 `qc.gatekeeper`、再跑只读 LLM `qc`）前，从当前节点 `workspaceDir` 通过 `git clone --depth=1 --no-local` 创建独立的 `<execRoot>/qc-workspace-r<n>`，将 `workspaceDir` 中尚未提交的修改/新增/删除文件精确同步至 `qc-workspace-r<n>`，并复制 `.sofa/{artifacts,imports}`（同样写入 `.git/info/exclude`）；
   - 在只读 LLM `qc` 会话前后对 `qc-workspace-r<n>` 及 `.sofa/artifacts` 做快照校验，若发生任何意外文件篡改立即判该轮失败，确保主 `workspaceDir` 100% 物理零触碰。
3. 打通 `sofa server --dev` 无容器依赖启动：
   - 在 `internal/cli/server_cmd.go`（及 `worker_cmd.go`）接入 `DispatchEngine` 与按沙箱分发的 `SessionLauncher`；即使宿主机未安装 Podman，只要工作流使用 `runtime = "host"`，`sofa server --dev` 即可零报错启动并跑通全流程；同时支持同一工作流内 `worker` 与 `qc` 混搭 `host` 与 `podman`！

**前置依赖**：`038`、`039`。

---

## 2. 涉及包与文件清单

- `internal/git/{adapter.go,cli.go,cli_integration_test.go}`（`.git/info/exclude` 辅助与 `CloneShallowSnapshot` / 工作树同步辅助）
- `internal/tool/runtime/{executor.go,executor_test.go}`（`host` 模式 `.sofa/imports/...` 与宿主机绝对路径 `QuotePaths` 校验）
- `internal/worker/runner/{types.go,worker_runner.go,skills.go,host_qc.go,runner_test.go,runner_integration_test.go}`
- `internal/cli/{server_cmd.go,worker_cmd.go,cli_integration_test.go}`

---

## 3. 实现细则与防坑约束

1. **`.git/info/exclude` 注入与 `.sofa/**` 防入库门禁**：
   - 在 `PrepareWorkspace` 或 `WorkerRunner` 准备 `host` 工作区后，幂等确保 `<workspace>/.git/info/exclude` 包含独立行 `/.sofa/`；
   - 在 `StatusPaths` 之后的 `writable_paths` 门禁中，无论用户 `writable_paths` 是否写了 `"**"`，任何以 `.sofa/` 开头的跟踪/暂存变更一律视为未授权路径拦截回滚，杜绝把 `.sofa/artifacts` 或 `.sofa/imports` 提交进代码分支。
2. **宿主机 `skill_directories` 重定位与过滤（`internal/worker/runner/skills.go`）**：
   - 新增 `hostMode bool` 分支：
     - `"config/..."`、`"modules/..."`、`"external_skills/..."` 重定位为宿主机 `filepath.Join(BundleDir, ...)` 绝对路径；
     - `".sofa/imports/..."` / `"/.sofa/imports/..."` 重定位为宿主机 `ImportsDir` 下对应路径；
     - 其他工作区相对路径重定位为 `filepath.Join(WorkspaceDir, ...)`；
     - 当 `disabled_skills` 命中目录内技能时，同样裁剪拷贝至 `<StageRootDir>/dir-<idx>`（保留 `+x` 权限），并将宿主机 `<StageRootDir>/dir-<idx>` 绝对路径直接放进 `SkillDirectories`（无需 `/.sofa/skills` 容器挂载前缀）。
3. **QC `depth=1` 浅克隆工作区（`internal/worker/runner/host_qc.go`）**：
   - 使用 `file://` URL（Windows 下规范化为 `file:///D:/...`）配合 `git clone --depth=1 --no-local <fileURL> <qcWorkspaceDir>`，确保 Git 真正执行 `depth=1` 浅克隆而不做全量硬链接/对象拷贝；
   - 调用 `GitAdapter.StatusPaths(ctx, workspaceDir)` 获取 `workspaceDir` 相对 `HEAD` 的全部未提交变更（修改/新增文件按常规文件复制、已删除路径在 `qcWorkspaceDir` 删除），再将 `<workspaceDir>/.sofa/{artifacts,imports}` 拷贝至 `<qcWorkspaceDir>/.sofa/{artifacts,imports}` 并写入 `.git/info/exclude`；
   - `qc.gatekeeper` 与只读 LLM `qc` 全部针对 `<qcWorkspaceDir>` 运行；LLM `qc` 前后对比工作树状态与 `.sofa/artifacts` 文件哈希，篡改即报错。
4. **`sofa server --dev` 平台匹配兼容**：
   - `sofa server --dev` 内置单机 Worker 与 `RunStartDefaults` 在单机模式下不因宿主未装 `podman` 而阻塞派发；`WorkerRunner` 按每个会话阶段实际选中的 `sandbox.Runtime` 动态决定插件与 `go_tool` 的目标 `Platform`（`host` 用 `runtime.GOOS/GOARCH`，容器用 `SandboxEngine.DetectPlatform`）。

---

## 4. 必测 TDD Red → Green 用例清单

- **Tier 1 单测（`runner_test.go` & `executor_test.go`）**：
  - `host` 模式下 `OpenSession` 收到宿主机真实 `WorkingDirectory`、宿主机重定位 `SkillDirectories` 与 `disabled_skills` 裁剪目录；
  - `.sofa/artifacts` 与 `.sofa/imports` 在 `<workspace>/.sofa/` 下读写正常且不被 `git status` 计入脏文件；
  - `QuotePaths` 在 `host` 模式下校验工作区文件与 `.sofa/imports/...` 通过、伪造引文拦截。
- **Tier 2 真实 Git + 宿主机插件集成测试（`cli_integration_test.go` / `runner_integration_test.go`）**：
  - 本机编译测试用原生 gRPC Harness 插件（无需启动任何容器），跑通完整 `sofa server --dev` + `runtime = "host"` 的 `sofa apply` -> Work 修改代码并写出 `.sofa/artifacts/summary.md` -> `depth=1` 隔离目录执行 `qc.gatekeeper` 与只读 `qc`（验证 QC 目录的 `.git` 确实为 shallow repository `.git/shallow` 存在，且主工作区不受影响）-> 推回本地裸仓 `sofa/work/...` 分支 -> `sofa delete` 清理。
