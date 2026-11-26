# 分片 014：`harness.proto`、`buf` 多语言桩、`pkg/harnessplugin` 与容器化 `go-plugin` `AutoMTLS` 桥接

## 1. 目标与边界

建立宿主机 `sofa-worker` 与任意语言外部 `AgentHarness` 插件之间的密码学安全 gRPC 通信桥梁：
1. **落地协议真源与 `buf` 声明式生成（`proto/`、`buf.yaml`、`buf.gen.yaml`）**：
   - 将 [docs/harness-plugin-contract.md](../harness-plugin-contract.md) 第 3 节定义的 `proto/sofa/harness/v1/harness.proto`（7 个 RPC + 单条原生双向流 `RunTurn`）落盘；
   - 配置 `buf.yaml` 与 `buf.gen.yaml` 生成 Go 桩（`pkg/harnessplugin/v1/*.pb.go`）与 Python 桩（供 `020` 使用，未来加 TypeScript 仅需在 `buf.gen.yaml` 加 `ts-proto`/`@grpc/grpc-js`），并将生成的桩文件提交入库以确保日常 `go test ./...` 零外部工具依赖。
2. **公开 Go 插件 SDK（`pkg/harnessplugin` — 零 `internal/` 依赖）**：
   - 固化 `HandshakeConfig`（`ProtocolVersion = 1`，`MagicCookieKey = "SOFA_HARNESS_PLUGIN"`，`MagicCookieValue = "sofa-harness-v1"`，`ProtocolGRPC`，强制 `AutoMTLS: true`）与插件启动后擦除 `PLUGIN_*` 环境变量的标准入口 `Serve`。
3. **宿主机领域接口与容器 `RunnerFunc` 桥接（`internal/harness/runtime`）**：
   - 定义供上层会话引擎面向接口编程的 `Opener` 与 `Session`（Tier 1 可纯内存 Mock）；
   - 实现自定义 `plugin.ClientConfig.RunnerFunc`：以 `--init --rm --entrypoint /.sofa/harness/sofa-harness-<name>` 直接拉起 `sandbox` 容器主进程（Linux 宿主机走 `0700` 共享目录 UDS `/.sofa/plugin/harness.sock`；Windows / macOS 宿主机走 `-p 127.0.0.1::<port>` 随机端口映射 + `AddrTranslator` 地址翻译），完整驱动 `RunTurn` 双向流分发。

**前置依赖**：分片 `013`。

---

## 2. 涉及包与文件清单

- 新增依赖：`github.com/hashicorp/go-plugin`、`google.golang.org/grpc`、`google.golang.org/protobuf`
- `buf.yaml`、`buf.gen.yaml`、`proto/sofa/harness/v1/harness.proto`
- `pkg/harnessplugin/v1/harness.pb.go` & `harness_grpc.pb.go`
- `pkg/harnessplugin/plugin.go`：`Handshake`、`PluginName = "agent_harness"`、`GRPCPlugin`、`ScrubPluginEnv()`、`Serve(impl harnessv1.AgentHarnessServer)`
- `internal/harness/runtime/types.go`：领域层 `Opener`、`Session`、`SessionConfig`、`Event`、`PreToolUseHook`、`PostToolUseHook`、`ToolHandler`、`ErrorDetail`
- `internal/harness/runtime/container_runner.go`：实现 `github.com/hashicorp/go-plugin/runner.Runner`（含 `AddrTranslator`）
- `internal/harness/runtime/client.go`：`GRPCClientOpener`、凭据内存具象化（`model_provider`、`mcp_server`、`harness.env_refs`）与 `RunTurn` 双向流状态循环
- `internal/harness/runtime/bridge_test.go`：Tier 1 本地子进程回环 `AutoMTLS` 测试 + 双向流并发帧调度单测
- `internal/harness/runtime/testdata/loopbackplugin/main.go` & `bridge_integration_test.go`（`//go:build integration`）：Tier 2 编译静态 Linux 回环插件挂入真实 Podman 容器验证 `--init --entrypoint` + `AutoMTLS` 全链路

---

## 3. 保姆级实现细则（含 `go-plugin@v1.6.0` 源码级防坑指南！）

### 3.1 `RunnerFunc` 与跨平台地址翻译（`container_runner.go`）
1. **从 `cmd.Env` 提取 `go-plugin` 握手变量**：
   - `go-plugin` 调用 `RunnerFunc(logger, cmd, tmpDir)` 时，`cmd.Env` 中已自动填好单次公钥证书 `PLUGIN_CLIENT_CERT=<PEM>`、魔数 `SOFA_HARNESS_PLUGIN=sofa-harness-v1`、`PLUGIN_PROTOCOL_VERSIONS=1` 等（若未开 `SkipHostEnv: true` 会混入宿主全部环境变量——**因此创建 `ClientConfig` 时必须显式设置 `SkipHostEnv: true`！** 这样 `cmd.Env` 里就**只有** `go-plugin` 生成的那几个公开握手变量，绝不会把宿主机的任何环境变量带进容器！）。
2. **Linux vs Windows/macOS 双模式启动**：
   - **Linux 宿主机（`runtime.GOOS == "linux"`）**：
     - 将 `go-plugin` 创建的 `0700` `tmpDir` 挂载为 `-v <tmpDir>:/.sofa/plugin:rw`；
     - 将 `cmd.Env` 中的 `PLUGIN_UNIX_SOCKET_DIR` 改写为容器内路径 `"/.sofa/plugin"`；
     - `PluginToHost("unix", "/.sofa/plugin/xxx")` 返回 `("unix", filepath.Join(tmpDir, "xxx"), nil)`。
   - **Windows / macOS 宿主机（`runtime.GOOS != "linux"`）**：
     - 过滤掉 `PLUGIN_UNIX_SOCKET_DIR`（让插件走 TCP 监听固定容器内端口，如 `const containerPluginPort = 19500`，传入 `PLUGIN_MIN_PORT=19500`、`PLUGIN_MAX_PORT=19500`；注意：Go 插件 `go-plugin` 默认 TCP 绑 `127.0.0.1`，在 Podman 端口转发下容器内需绑 `0.0.0.0:19500` 或在 `pkg/harnessplugin/Serve` 中通过自定义 `ServerConfig` / listener 监听 `0.0.0.0:19500`！）；
     - `podman run` 参数加入 `-p 127.0.0.1::19500`（仅绑宿主机回环 `127.0.0.1` 随机空闲端口）与 `--name <uniqueContainerName>`；
     - 当 `PluginToHost("tcp", pluginAddr)` 被调用时，执行 `podman port <uniqueContainerName> 19500` 解析出宿主机映射地址 `"127.0.0.1:<hostPort>"` 返回给 `go-plugin` 建立 `AutoMTLS` 连接！
3. **容器生命周期与清理**：
   - `Start(ctx)` 启动 `exec.Command("podman", "run", ...)` 并连接 `StdoutPipe` / `StderrPipe`；
   - `ID()` 返回 `<uniqueContainerName>`（同时供 `015` 的 `podman exec -i` 执行 `go_tool` / `external_tool` 直接使用！）；
   - `Kill(ctx)` 执行 `podman rm -f -t 2 <uniqueContainerName>` 确保容器彻底停止并清理。

### 3.2 非 Go 语言插件（Python / TypeScript / Rust）标准握手模板（供 `020` 与未来插件照抄）

经核对 `hashicorp/go-plugin@v1.6.0` 官方源码（`examples/grpc/plugin-python/plugin.py` 与 `server.go`），非 Go 语言插件**无需任何 HashiCorp 私有 `.proto`**（`GRPCStdio`/`GRPCBroker`/`GRPCController` 返回标准 `UNIMPLEMENTED` 时宿主均自动忽略），只需严格遵守以下 5 步：
1. 校验环境变量 `os.environ.get("SOFA_HARNESS_PLUGIN") == "sofa-harness-v1"`；
2. 读取 `client_cert_pem = os.environ["PLUGIN_CLIENT_CERT"]`（标准 PEM 格式），紧接着**立即清空进程 `os.environ` 中所有 `PLUGIN_*` 与 `SOFA_HARNESS_PLUGIN` 键**（防止泄漏给后续 `bash` 子进程）；
3. 动态生成一对单次 ECDSA P-256 自签名服务器证书（SAN 包含 `DNS:localhost` 与 `IP:127.0.0.1`），用 `client_cert_pem` 作为根 CA 构造强制双向认证 `ssl_server_credentials(..., require_client_auth=True)`；
4. 创建 gRPC Server，注册 `AgentHarnessServicer` 与各语言官方标准健康检查服务 `grpc.health.v1.Health`（**必须设置 service 名 `"plugin"` 的状态为 `SERVING`**！）；若存在 `PLUGIN_UNIX_SOCKET_DIR` 则监听 `unix:///.sofa/plugin/harness.sock`，否则监听 `0.0.0.0:<PLUGIN_MIN_PORT>`；
5. 将服务器公钥证书编码为**裸 DER 字节的 Base64（无 PEM 头尾、无换行符！）**，向 `stdout` 打印唯一一行握手串并立即 `flush()`：
   - Unix 模式：`1|1|unix|/.sofa/plugin/harness.sock|grpc|<base64_server_cert_der>\n`
   - TCP 模式：`1|1|tcp|0.0.0.0:19500|grpc|<base64_server_cert_der>\n`

### 3.3 `RunTurn` 双向流并发与安全不变量（`client.go`）
1. 同一 `Session` 用互斥锁保证同一时刻至多 1 个活跃 `RunTurn` 流，同时 `Abort(ctx)` 走独立 Unary RPC `AbortTurn`（绝不阻塞在 `RunTurn` 锁上！）。
2. 流启动后首先发送 `HostTurnFrame{StartTurn}`，随后循环 `stream.Recv()`：
   - 收到 `SessionEvent`：同步回调 `onEvent(ev)`（供上层刷新 `StallWatchdog` 和推送日志）；
   - 收到 `PreToolUseRequest`：调用注入的 `PreToolUseHook` 并发回 `PreToolUseDecision`；
   - 收到 `PostToolUseRequest`：调用注入的 `PostToolUseHook` 并发回 `PostToolUseAck`；
   - 收到 `InvokeHostToolRequest`：在独立 goroutine（用流发送互斥锁 `sendMu` 保护 `stream.Send`！）调用宿主机 `ToolHandler` 并写回 `InvokeHostToolResponse`，从而支持单回合内模型并行发起多个工具调用！
   - 收到 `TurnResult`：等待所有在途 `ToolHandler` goroutine 完成后调用 `stream.CloseSend()` 并返回（若 `TurnResult.Error != nil`，转换为携带 `ErrorCategory` 与 `HTTPStatusCode` 的结构化 `*HarnessError`）。

---

## 4. 必测 TDD Red → Green 用例清单

1. **Tier 1 本地子进程 `go-plugin` 回环测试（`bridge_test.go`）**：
   - 使用 `SkipHostEnv: true` + `AutoMTLS: true` 在本机启动测试插件子进程：
     - 验证插件启动后已擦除 `PLUGIN_*` 环境变量，且未携带宿主机自定义的敏感测试环境变量；
     - 覆盖全部 7 个 RPC：`GetCapabilities`、`OpenSession`（校验内存传入的 `ProviderConfig` / `harness_env` / `MCPServerConfig` 完整到达插件）、`RunTurn`（流式增量事件、`PreToolUse` 允许/拒绝、`PostToolUse` 失败通知、并发 2 个 `InvokeHostTool` 回调）、`AbortTurn` 打断阻塞中的 `RunTurn`、`ResumeSession`、`ReadMCPResource`、`CloseSession`；
     - 验证无客户端私钥的外部裸 gRPC 客户端尝试直连插件监听地址时，在 TLS 握手层即被拒绝！
2. **Tier 2 真实 Podman 容器 `RunnerFunc` 集成测试（`bridge_integration_test.go`，`//go:build integration`）**：
   - 将 `testdata/loopbackplugin` 交叉编译为 `CGO_ENABLED=0 GOOS=linux` 静态二进制放入临时目录，作为 `/.sofa/harness/sofa-harness-loopback` 挂载进真实 Podman `alpine` 容器（`--init --entrypoint`）；
   - 验证在当前宿主机（Windows 或 Linux）上通过 `GRPCClientOpener` 成功完成 `AutoMTLS` 握手、`podman inspect` 容器环境变量不含任何业务 API Key、`RunTurn` 往返成功，且 `Close` 后容器已被彻底清理不存在残留。
