# 分片 019：同仓隔离孵化外部插件 `sofa-harness-copilot`（Go + `copilot-sdk/go`）

## 1. 目标与边界

严格遵循**“零内置特例、物理同仓孵化、编译与运行期严格外部化”**铁律，在独立 Go 子模块 `plugins/sofa-harness-copilot/` 中实现官方 Copilot Agent Harness 插件（1:1 无损迁移 `r42/internal/copilot` 的全部会话、钩子、MCP 资源与错误归一化能力）：
1. **编译器级物理隔离**：
   - 使用独立 `plugins/sofa-harness-copilot/go.mod`（模块名 `github.com/lonegunmanb/sofa-harness-copilot`，孵化期通过 `replace github.com/lonegunmanb/sofa => ../..` 仅引用公开包 `pkg/harnessplugin`）；
   - `sofa` 主仓 `go.mod` 严禁引入 `github.com/github/copilot-sdk/go`，主仓任何包严禁 `import` `plugins/sofa-harness-copilot`。
2. **完整协议实现与防双重拦截保证**：
   - 完整实现 `harnessv1.AgentHarnessServer` 的 7 个 RPC；
   - 对非 `host_tools` 工具挂载 `OnPreToolUse` / `OnPostToolUse` / `OnPostToolUseFailure`，对属于 `host_tools` 的工具在钩子内直接放行跳过（交由 `sdk.Tool` 回调直接走 `InvokeHostToolRequest`，防双重扣配额）；
   - 确保 `harness_env`（如 `GITHUB_TOKEN`）仅注入给 `copilot` CLI 连接环境，不污染容器全局环境。
3. **自包含 Linux 目录打包与三层测试**：
   - 提供自包含构建脚本生成 `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}`；
   - 通过底层 `sdkClient` 接口注入完成 100% 离线单测 + 真实 Podman 容器 `AutoMTLS` 联调测试（+ 可选 `SOFA_LIVE_COPILOT_TEST=1` 真机 API 冒烟测试）。

**前置依赖**：分片 `014`、`018`。

---

## 2. 目录与文件清单

```text
plugins/sofa-harness-copilot/
├── go.mod & go.sum                         # module github.com/lonegunmanb/sofa-harness-copilot
├── cmd/
│   └── sofa-harness-copilot/
│       └── main.go                         # 调用 harnessplugin.Serve(...) 启动 go-plugin Server
├── internal/
│   └── server/
│       ├── sdk_iface.go                    # 薄封装 sdkClient / sdkSession 接口（对齐 r42/internal/copilot）
│       ├── server.go                       # AgentHarnessServer 7 个 RPC 完整实现
│       ├── errors.go                       # sdk.SessionErrorData -> harnessv1.ErrorCategory 映射
│       └── server_test.go                  # 100% 离线协议契约单测
└── scripts/
    └── build-bundle.go                     # 打包 Linux 自包含目录 (sofa-harness-copilot + bin/copilot)
```
以及主仓集成测试：`internal/worker/runner/copilot_plugin_integration_test.go`（`//go:build integration`）。

---

## 3. 保姆级实现细则

### 3.1 `GetCapabilities` 与 `OpenSession` 逐字段映射（对齐 `r42/internal/copilot/session.go`）
1. **`GetCapabilities`**：
   - 返回 `HarnessName: "copilot"`、`ProtocolVersion: 1`、`SupportedProviderTypes: []string{"openai", "azure", "anthropic"}`、`SupportsSessionResume: true`、`SupportsMcpServers: true`、`SupportsMcpResources: true`、`SupportsSkills: true`。
2. **`copilot` CLI 路径发现优先级**：
   - 优先读取同目录内置运行时 `<executableDir>/bin/copilot`；若不存在且处于开发/测试模式，允许读取 `options` 中的 `cli_path` 或回退容器 `PATH` 查找 `copilot`。
   - 启动 `sdk.NewClient` 时，仅将 `OpenSessionRequest.HarnessEnv` 合并进 `StdioConnection.Env` 传给 `copilot` CLI 进程，绝不调用 `os.Setenv` 污染当前进程环境！
3. **`OpenSession` 配置构造**：
   - `Provider != nil` 时：校验 `Provider.Type` 在支持列表内（否则返回 `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY`），1:1 映射 `Type`、`BaseURL`、`WireAPI`、`Transport`、`Headers`、`ModelID`（`Profile`）、`WireModel`（`Model`）以及 `APIKey` / `BearerToken`；`Provider == nil` 时传 `nil` 使用 Copilot 原生认证；
   - `AvailableTools` / `ExcludedTools`：严格按 `ToolFilter.Configured` 区分 `nil`（未配置）与 `[]string{}`（显式空列表）；
   - `Skills`：当 `len(req.Skills) > 0` 时，对齐 `r42` 构造 `CustomAgents: []sdk.CustomAgentConfig{{Name: "sofa_worker", Prompt: req.SystemPrompt, Skills: req.Skills, Model: profile, ReasoningEffort: req.ReasoningEffort}}` 并设 `Agent: "sofa_worker"`；
   - `ResumeSessionID != ""` 时：优先调用 `client.ResumeSession`；若底层报错包含 `"Session not found"`，**不要在插件内自行新建会话**，直接返回 `OpenSessionResponse{Error: &ErrorDetail{Category: ERROR_CATEGORY_SESSION_NOT_FOUND, Message: ...}}` 交由宿主机 `016` 决定回退新建并注入完整结构化日记！

### 3.2 `RunTurn` 流式桥接与钩子防重入（`server.go`）
1. **宿主工具包装（`host_tools`）**：
   - 记录集合 `hostToolNames = map[string]struct{}{...}`；
   - 为每个 `HostToolSpec` 创建 `sdk.Tool{Name, Description, Parameters}`，其 `Handler` 在当前活跃 `RunTurn` 上下文中生成唯一 `toolCallID`，向宿主流发送 `PluginTurnFrame{InvokeHostToolRequest}` 并阻塞在专属 `chan *InvokeHostToolResponse` 上等待宿主机响应（或流取消），将 `ResultType` 映射为 `"success"` / `"failure"` 返回给 SDK。
2. **内置/MCP 工具钩子（`SessionHooks`）**：
   - `OnPreToolUse`：**第一行检查 `if _, isHost := hostToolNames[input.ToolName]; isHost { return allow, nil }`**（严禁对宿主工具重复发 `PreToolUseRequest`！）；非宿主工具发送 `PreToolUseRequest` 等待 `PreToolUseDecision`：若 `!allow` 返回 `PermissionDecision: "deny"` + `PermissionDecisionReason: denyReason`，否则透传 `ModifiedArguments` 与 `AdditionalContext`。
   - `OnPostToolUse` / `OnPostToolUseFailure`：同样跳过 `hostToolNames`；非宿主工具发送 `PostToolUseRequest{Succeeded: true/false}` 等待 `PostToolUseAck`。
3. **事件流与错误归一化（`errors.go`）**：
   - 通过 `session.On(...)` 将 `AssistantMessageDelta`、`AssistantReasoningDelta`、`AssistantUsage`（含 `InputTokens`、`OutputTokens`、`ReasoningTokens`、`CacheReadTokens`、`CacheWriteTokens`）、`ToolExecutionStart/Progress/PartialResult(Shell)/Complete`、`Subagent*` 实时转换为 `SessionEvent` 发出；
   - 错误分类：`rate_limit` / HTTP `429` → `RATE_LIMIT`；`authentication` / `401` → `AUTHENTICATION`；`authorization` / `403` → `AUTHORIZATION`；`quota` → `QUOTA`；`context_limit` → `CONTEXT_LIMIT`；网络/5xx → `TRANSIENT`；其余 → `INTERNAL`。

---

## 4. 必测 TDD Red → Green 用例清单

1. **插件子模块独立离线单测（在 `plugins/sofa-harness-copilot` 目录执行 `go test ./... -count=1`）**：
   - 注入假 `sdkClient` 覆盖：正交 `ProviderConfig` 映射、`Provider == nil` 原生模式、不支持的 `Provider.Type` 返回 `UNSUPPORTED_CAPABILITY`、`ResumeSession` 报 `"Session not found"` 返回 `SESSION_NOT_FOUND`；
   - 验证内置工具 `bash` 触发 `PreToolUse` → `PostToolUse(succeeded=false)`，而宿主工具 `sofa_checkpoint` 只触发 `InvokeHostTool` **绝不触发** `PreToolUse`；
   - 验证并发 `AbortTurn` 立即取消阻塞中的 `SendAndWait` 并干净退出 `RunTurn`；验证 `ReadMCPResource` 与 6 类 `ErrorCategory` 映射。
2. **Tier 2 真实 Podman 容器外部插件加载测试（`copilot_plugin_integration_test.go`，`//go:build integration`）**：
   - 将 `plugins/sofa-harness-copilot` 打包到临时 `<plugin_dir>/sofa-harness-copilot/`（配合内置轻量假 `bin/copilot` JSON-RPC 桩进程，无需外网 Key 即可驱动真实 `copilot-sdk/go` 协议栈！），在 HCL 中写 `harness "copilot" {}`（零硬编码路径，纯走 `013` 本地插件目录发现！），在真实 Podman 容器内跑通 `018` 的 `WorkerRunner` 全闭环！
