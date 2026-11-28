# 分片 039：跨平台插件安装器、宿主子进程启动器与 `HostEngine`

## 1. 目标与边界

1. 扩展 `internal/harness/installer`，放开 `Platform.OS` 支持 `"linux" | "darwin" | "windows"`（`amd64` / `arm64`），支持 Windows 可执行入口后缀（`.exe` / `.cmd`）并在 `PreparedPlugin` 上暴露宿主机可执行绝对路径 `HostPath`。
2. 在 `internal/harness/runtime` 中新增面向 `runtime = "host"` 的 `NewHostSubprocessOpener`，继承过滤掉内部控制令牌后的宿主机进程环境变量，通过 `hashicorp/go-plugin` `AutoMTLS` 启动原生插件进程。
3. 在 `internal/sandbox/runtime` 中实现 `HostEngine` 与按 `sandbox.Runtime` 自动分发的 `DispatchEngine`（`host` 走 `HostEngine`，容器运行时走 `PodmanEngine`），支持宿主机原生执行 `gatekeeper`、按宿主平台编译执行的 `go_tool`（含 Windows `.exe` 后缀），以及带捆绑脚本宿主机路径重定位的 `external_tool`。
4. 扩展 `scripts/install-local-plugins.go`（及对应构建辅助逻辑），支持 `-host`（或 `-os <linux|darwin|windows>`）一键构建安装当前宿主机原生 `sofa-harness-copilot` 与 `sofa-harness-antigravity` 插件至 `<plugin-dir>/sofa-harness-<name>/<os>_<arch>/`。

**前置依赖**：`038`。

---

## 2. 涉及包与文件清单

- `internal/harness/installer/{installer.go,installer_test.go}`
- `internal/harness/runtime/{client.go,client_test.go}`
- `internal/sandbox/runtime/{host.go,dispatch.go,host_test.go,spec.go}`
- `internal/tool/runtime/{compiler.go,executor.go,executor_test.go}`
- `plugins/sofa-harness-copilot/scripts/build-bundle.go` & `scripts/install-local-plugins.go` & `examples/examples_test.go`

---

## 3. 实现细则与防坑约束

1. **`installer.Platform` 与 Windows 入口解析（`internal/harness/installer/installer.go`）**：
   - `Platform.Validate()` 允许 `OS` 为 `"linux"`、`"darwin"`、`"windows"`，`Arch` 为 `"amd64"`、`"arm64"`；
   - `PreparedPlugin` 新增 `HostPath string` 字段；
   - 在 `prepareLocalPlugin` 与 `prepareRemotePlugin` 查找入口文件时：
     - 当 `platform.OS == "windows"` 时，按优先级依次探测 `sofa-harness-<name>.exe`、`sofa-harness-<name>.cmd`、`sofa-harness-<name>`（均须为常规文件、禁止符号链接）；
     - 非 Windows 探测 `sofa-harness-<name>` 并 `chmod 0755`；
     - `PreparedPlugin.HostPath` 填入命中的宿主机入口文件绝对路径，`ContainerPath` 保持 `"/.sofa/harness/" + filepath.Base(hit)`。
2. **宿主机环境变量清洗与 `NewHostSubprocessOpener`（`internal/harness/runtime/client.go`）**：
   - 实现 `SanitizeHostBaseEnv(environ []string) []string`（公共复用给插件启动器、`HostEngine.Exec` 与 `HostEngine.RunGatekeepers`）：保留宿主机环境变量，精确剔除 `APP_API_TOKEN`、`SOFA_API_TOKEN`、`SOFA_HARNESS_PLUGIN`、`PLUGIN_*`、`GIT_DIR`、`GIT_WORK_TREE`、`GIT_INDEX_FILE`、`GIT_TERMINAL_PROMPT`；
   - `NewHostSubprocessOpener(binPath, workDir string, baseEnv []string, opts OpenerOptions)`：构造 `exec.Command(binPath)`，设置 `cmd.Dir = workDir`、`cmd.Env = SanitizeHostBaseEnv(baseEnv)`，仍保持 `SkipHostEnv: true` 与 `AutoMTLS: true`（由我们显式传入清洗后的 `cmd.Env`）。
3. **`HostEngine`、`DispatchEngine` 与工具执行（`internal/sandbox/runtime/` + `internal/tool/runtime/`）**：
   - `HostEngine.DetectPlatform` 直接返回 `Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}`；
   - `HostEngine.RunGatekeepers`：直接在宿主机 `filepath.Join(spec.HostWorkspaceDir, filepath.FromSlash(gk.WorkingDir))`（校验不得利用 `..` 逃逸 `HostWorkspaceDir`！）下以 `SanitizeHostBaseEnv` 执行命令；若参数含 `config/...` 或 `modules/...` 且存在于 `spec.BundleDir` 而不在 `HostWorkspaceDir`，自动重定位为 `filepath.Join(spec.BundleDir, ...)` 宿主机真实路径；完整保留 `RequireCleanTree` 前后快照比对与自动还原能力；
   - `HostEngine.Exec`（供 `go_tool` / `external_tool` 调用）：支持在宿主机指定工作目录下带 `stdin` 与合并后的环境变量执行进程；
   - `toolruntime.Compiler`：当 `TargetOS == "windows"` 时，编译产物与 `StageToolsDir` 输出文件名带 `.exe` 后缀；`toolruntime.Executor` 新增可选宿主机执行路径配置（`ToolsDir` 宿主机路径与宿主机 `BundleDir` 参数重定位），使 `go_tool` 与 `external_tool` 在容器和宿主机两种模式下无缝同构工作！
   - `DispatchEngine`：封装 `HostEngine` 与 `PodmanEngine`，使同一 `WorkerDaemon` 可同时承载 `host` 与容器沙箱。
4. **`scripts/install-local-plugins.go` 支持 `-host` / `-os`**：
   - `plugins/sofa-harness-copilot/scripts/build-bundle.go` 新增 `-os` flag（默认 `"linux"`，支持 `"windows"` / `"darwin"` / `"linux"`，在 `"windows"` 下输出 `sofa-harness-copilot.exe` 并调用 `cmd/bundler` 打包对应平台 `embeddedcli`）；
   - `scripts/install-local-plugins.go` 新增 `-host` bool flag（等价于 `-os runtime.GOOS -arch runtime.GOARCH`）：
     - `copilot` 安装到 `<plugin-dir>/sofa-harness-copilot/<os>_<arch>/`；
     - `antigravity` 在 `-host` 下生成无符号链接的宿主机启动脚本（Windows 生成 `sofa-harness-antigravity.cmd`、Unix 生成 `0755` `sofa-harness-antigravity` shell 脚本，指向插件 `.venv` 或 `uv run`），满足 `installer.Prepare` 校验。

---

## 4. 必测 TDD Red → Green 用例清单

- `installer_test.go`：`windows_amd64`（`.exe` / `.cmd`）、`darwin_arm64`、`linux_amd64` 本地与远端锁文件安装解析测试。
- `client_test.go`：`SanitizeHostBaseEnv` 保留 `PATH`/`SYSTEMROOT`/`USERPROFILE` 等宿主变量并剥离 `APP_API_TOKEN`/`SOFA_API_TOKEN`/`PLUGIN_*`，`NewHostSubprocessOpener` 真实拉起本机测试 gRPC 插件完成握手与 `GetCapabilities`。
- `host_test.go` & `executor_test.go`：`HostEngine.RunGatekeepers` 超时/退出码/`RequireCleanTree` 脏文件还原、捆绑脚本宿主机重定位、宿主机 `go_tool`（含当前 OS 编译运行）与 `external_tool` 密钥脱敏执行测试。
- `examples_test.go`：`install-local-plugins.go -host -only copilot` 在当前宿主机 OS 下产物能被 `installer.Prepare` 正常识别。
