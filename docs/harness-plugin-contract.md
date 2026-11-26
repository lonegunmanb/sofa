# sofa AgentHarness 插件交互协议契约（Frozen Draft）

本文档定义宿主机工作进程（`sofa-worker`）与沙箱容器内多语言 Agent Harness 插件（如 Go 实现的 `sofa-harness-copilot`、Python 实现的 `sofa-harness-antigravity`）之间的完整交互契约。

本契约基于对 `r42` 生产源码（`internal/copilot/session.go`、`mcp_resource_tool.go`、`internal/cli/runtime.go`、`runtime_workflow_tools.go`）与 `google-antigravity` Python SDK 的逐行核对设计，确保通过统一的 `hashicorp/go-plugin`（gRPC）边界即可 **100% 无损复现 `r42` 操控 `copilot-sdk/go` → `copilot` CLI 的全部会话、钩子、配额回滚、流式事件、卡死恢复与错误分类能力**，同时让非 Go 语言插件保持极简实现。

---

## 1. 核心架构原则：薄插件（Thin Plugin）+ 厚宿主（Smart Host）

为避免每个语言的 Harness 插件重复实现复杂的编排与容错状态机（导致行为漂移），职责边界严格划分如下：

### 1.1 宿主机 `sofa-worker`（Go）独占的编排与安全职责
1. **统一重试退避策略（`provider.RetryPolicy`）**：`lifecycle_retries`、`model_call_retries`、指数退避与随机抖动、`error_message_regex` 匹配全部在宿主机 Go 层执行；插件不做重试循环，仅在失败时返回结构化 `ErrorDetail`（含 `ErrorCategory` 与 `http_status_code`）。
2. **统一卡死看门狗与自愈（`StallWatchdog`）**：宿主机监听 `RunTurn` 流上报的所有 `SessionEvent`（增量文本、思考流、工具启动/进度/Shell 输出、子 Agent 状态）及宿主侧 `typedToolActivity` 刷新最近活跃时间；一旦超过 `session_stall_timeout` 无任何进展，宿主机调用旁路 `AbortTurn` → 等待在途回调归零 → 调用 `ResumeSession` 并注入续跑提示词恢复（完整保留 `r42` 的双屏障保护）。
3. **统一工具门禁与配额账本（`ToolCallQuota` + 四道门禁）**：
   - `sofa_*` 会话协议工具、`go_tool`、`external_tool`、`sofa_read_mcp_resource` 统一注册为 **`HostToolSpec`**（Schema 下发给插件，真实 Handler 在宿主机拦截校验；其中 `external_tool` 经宿主机完成输入类型校验与配额预扣后，由宿主机通过容器运行时在工作区容器内执行程序并校验 `postcondition`）；
   - 插件侧内置工具（如 `bash`、`view`、`edit`、`grep`、`web_fetch`）与 MCP 工具在执行前通过 `PreToolUseRequest` 询问宿主机（检查人工日记插话、`checkpointPending`、`roundComplete` 锁以及内置工具配额预扣），执行结束后通过 `PostToolUseRequest` 通知宿主机（**若内置工具执行失败，宿主机自动回滚 `tool_call_quota` 计数**，1:1 对齐 `r42` 的 `OnPostToolUseFailure`）。
4. **凭据零环境变量泄漏（In-Memory Credential Delivery）**：
   - `model_provider` 的 API Key / Bearer Token、`mcp_server` 的认证头与 `stdio` 环境变量、以及 `harness.env_refs` 映射的宿主机密钥，全部由宿主机 `sofa-worker` 在内存中读取后，**经 `AutoMTLS` 加密的 `OpenSessionRequest` RPC 消息下发至插件进程内存**；
   - **严禁通过 `podman run -e` / `docker run -e` 传递任何密钥**（防止宿主机 `podman inspect` 或容器 `/proc/1/environ` 暴露凭据）。

### 1.2 容器内 `AgentHarness` 插件的极简职责
1. 完成 `hashicorp/go-plugin` `AutoMTLS` 握手并立即从自身进程环境擦除 `PLUGIN_*` 变量（防止泄漏给后续派生的 `bash` 子进程）；
2. 将 `OpenSessionRequest` 映射为底层 SDK 会话（注入模型、推理强度、系统提示词、可选正交 `ProviderConfig`、MCP Servers、Skills、工具白名单/黑名单、将 `host_tools` 包装为流上回调工具、挂载非宿主工具的 `PreToolUse` / `PostToolUse` 钩子）；
3. 在 `RunTurn` 双向流上驱动单轮模型对话，实时转发流式事件、钩子问答与宿主工具调用，并将底层 SDK 错误映射为标准 `ErrorDetail`；
4. 确保 `OpenSessionRequest.harness_env`（如 `GITHUB_TOKEN`、`GEMINI_API_KEY`）仅传递给底层模型 SDK/CLI 连接，绝不注入给模型可执行的 `bash` Shell 工具环境。

---

## 2. `hashicorp/go-plugin` 握手与容器挂载约定

### 2.1 握手常量（`pkg/harnessplugin`）
- `ProtocolVersion`: `1`
- `MagicCookieKey`: `"SOFA_HARNESS_PLUGIN"`
- `MagicCookieValue`: `"sofa-harness-v1"`
- `PluginMap` 键名: `"agent_harness"`
- 传输协议: 仅允许 `plugin.ProtocolGRPC`，强制 `AutoMTLS: true`

### 2.2 容器内标准挂载布局
宿主机 `sofa-worker` 通过自定义 `plugin.ClientConfig.RunnerFunc` 拉起沙箱容器，容器内固定路径约定如下：

| 容器内路径 | 权限 | 用途说明 |
| :--- | :---: | :--- |
| `/.sofa/harness/` | `ro` | 解压并核验过 `plugins.lock.json` 锁定 SHA-256 的目标容器架构（`linux/<arch>`）插件包目录，可执行入口固定为 `/.sofa/harness/sofa-harness-<name>` |
| `/.sofa/plugin/` | `rw` | 宿主机为当前容器独占创建的 `0700` 临时目录，插件在此绑定 Unix Domain Socket `/.sofa/plugin/harness.sock` |
| `/.sofa/session-store/` | `rw` | 容器私有会话存储目录（供 Copilot CLI 等保存同容器 `ResumeSession` 所需的会话事件状态） |
| `/.sofa/bundle/` | `ro` | `.sofaplan` 解压的静态资源快照（如 `${path.module}/skills`） |
| `/workspace` | 按策略 | 项目代码工作树（Work 容器按 `writable_paths` 限制可写范围；QC 容器整体 `ro` 只读挂载） |

---

## 3. gRPC 协议定义（`proto/sofa/harness/v1/harness.proto`）

> **跨语言设计约束**：全程仅使用标准 Protobuf `proto3` 与单条原生双向流 `RunTurn`，**不使用 `hashicorp/go-plugin` 私有的 `GRPCBroker` 子流多路复用**，使得 Python（`grpcio`）、TypeScript、Rust 插件无需移植 HashiCorp mux 即可原生实现。

```protobuf
syntax = "proto3";

package sofa.harness.v1;

option go_package = "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1;harnessv1";

service AgentHarness {
  // 1. 能力自省握手：返回插件元信息与可选特性支持矩阵，供宿主机前置快速校验
  rpc GetCapabilities(GetCapabilitiesRequest) returns (GetCapabilitiesResponse);

  // 2. 创建或按 ID 恢复会话（对齐 r42 Factory.Open：若指定 resume_session_id 但不存在，返回 ERROR_CATEGORY_SESSION_NOT_FOUND）
  rpc OpenSession(OpenSessionRequest) returns (OpenSessionResponse);

  // 3. 执行单轮对话（对齐 r42 Session.SendAndWait + On 事件流 + SessionHooks + sdk.Tool 回调）
  // 同一 session_id 在同一时刻至多存在一个活跃 RunTurn 流；多轮对话（如协议修复重试、QC 返工）按序发起多次 RunTurn。
  rpc RunTurn(stream HostTurnFrame) returns (stream PluginTurnFrame);

  // 4. 旁路即时中止当前轮次（对齐 r42 Session.Abort，供 StallWatchdog 或 immediate pause 抢占式打断阻塞中的 RunTurn）
  rpc AbortTurn(AbortTurnRequest) returns (AbortTurnResponse);

  // 5. 原地重连恢复会话（对齐 r42 Session.Resume：在 AbortTurn 并确认在途工具归零后重连同一 session_id）
  rpc ResumeSession(ResumeSessionRequest) returns (ResumeSessionResponse);

  // 6. 读取已挂载 MCP Server 的声明资源（对齐 r42 ReadMCPResource，由宿主机 sofa_read_mcp_resource 验权计额后调用）
  rpc ReadMCPResource(ReadMCPResourceRequest) returns (ReadMCPResourceResponse);

  // 7. 销毁并清理会话资源（对齐 r42 Session.Close / Disconnect）
  rpc CloseSession(CloseSessionRequest) returns (CloseSessionResponse);
}

message GetCapabilitiesRequest {}

message GetCapabilitiesResponse {
  string harness_name = 1;
  string harness_version = 2;
  uint32 protocol_version = 3;
  repeated string supported_provider_types = 4; // 如 ["openai", "azure", "anthropic"]；空列表表示仅支持自身原生提供方
  bool supports_session_resume = 5;
  bool supports_mcp_servers = 6;
  bool supports_mcp_resources = 7;
  bool supports_skills = 8;
}

// ---------------------------------------------------------------------------
// OpenSession / ResumeSession / CloseSession
// ---------------------------------------------------------------------------

message OpenSessionRequest {
  string session_id = 1;                  // 宿主机生成的会话唯一 ID（新建时使用）
  string resume_session_id = 2;           // 非空时优先尝试恢复已有会话（对齐 r42 ResumeSessionID）

  string model = 3;                       // 必填：模型标识
  string profile = 4;                     // 可选：为空时默认等于 model（对齐 r42 Profile）
  string reasoning_effort = 5;            // 可选："low" | "medium" | "high" 等
  string system_prompt = 6;               // 宿主机组装完毕的系统提示词（含协议约束与内置工具配额说明）
  string working_directory = 7;           // 容器内工作目录（通常为 "/workspace"）
  string session_store_directory = 8;     // 容器内会话状态目录（通常为 "/.sofa/session-store"）
  string permission_mode = 9;             // 如 "approve_all" | "read_only"

  // 正交模型提供方配置：Root/Worker 未绑定或显式 model_provider = null 时不传，由插件使用原生认证
  ProviderConfig provider = 10;

  // 插件专属配置与密钥（来自 harness 块的 env_refs 与 options，经 AutoMTLS 内存直传，绝不走容器启动 env）
  map<string, string> harness_env = 11;
  bytes harness_options_json = 12;

  // 宿主机托管工具声明（含 sofa_* 协议工具、go_tool、external_tool、sofa_read_mcp_resource）
  repeated HostToolSpec host_tools = 13;

  // 工具过滤列表：使用包装类型严格区分 HCL 中的 null（未配置）与 []（显式空列表）
  ToolFilter available_tools = 14;
  ToolFilter excluded_tools = 15;

  // Skills 配置（路径已重定位至容器内只读挂载点 /.sofa/bundle/... 或 /workspace/...）
  repeated string skill_directories = 16;
  repeated string skills = 17;
  repeated string disabled_skills = 18;

  // 已具象化凭据的 MCP Server 列表
  repeated MCPServerConfig mcp_servers = 19;
}

message ProviderConfig {
  string type = 1;         // "openai" | "azure" | "anthropic"
  string base_url = 2;     // API Root Endpoint
  string wire_api = 3;     // 可选："completions" | "responses"
  string transport = 4;    // 可选："http" | "websockets"
  map<string, string> headers = 5;
  string model_id = 6;     // profile
  string wire_model = 7;   // model
  oneof auth {
    string api_key = 8;
    string bearer_token = 9;
  }
}

message HostToolSpec {
  string name = 1;
  string description = 2;
  bytes parameters_json_schema = 3; // 标准 JSON Schema 对象字节
}

message ToolFilter {
  bool configured = 1;
  repeated string names = 2;
}

message MCPServerConfig {
  string name = 1;
  repeated string tools = 2;
  int32 timeout_ms = 3;
  oneof transport {
    MCPHTTPTransport http = 4;
    MCPStdioTransport stdio = 5;
  }
}

message MCPHTTPTransport {
  string url = 1;
  map<string, string> headers = 2;
}

message MCPStdioTransport {
  string command = 1;
  repeated string args = 2;
  map<string, string> env = 3;
  string working_directory = 4;
}

message OpenSessionResponse {
  string session_id = 1;     // 实际生效的底层会话 ID
  bool resumed = 2;          // 是否从已有会话成功恢复
  ErrorDetail error = 3;     // 非空表示业务级失败（供宿主机 RetryPolicy 判定是否可重试或回退新建）
}

message AbortTurnRequest {
  string session_id = 1;
  string turn_id = 2;
}

message AbortTurnResponse {
  ErrorDetail error = 1;
}

message ResumeSessionRequest {
  string session_id = 1;
}

message ResumeSessionResponse {
  ErrorDetail error = 1;
}

message CloseSessionRequest {
  string session_id = 1;
}

message CloseSessionResponse {
  ErrorDetail error = 1;
}

// ---------------------------------------------------------------------------
// ReadMCPResource（对齐 r42 session.RPC.MCP.Resources().Read）
// ---------------------------------------------------------------------------

message ReadMCPResourceRequest {
  string session_id = 1;
  string server_name = 2;
  string uri = 3;
}

message MCPResourceContent {
  string uri = 1;
  string mime_type = 2;
  string text = 3;
  bytes blob = 4;
  map<string, string> meta = 5;
}

message ReadMCPResourceResponse {
  repeated MCPResourceContent contents = 1;
  ErrorDetail error = 2;
}

// ---------------------------------------------------------------------------
// RunTurn 双向流帧定义
// ---------------------------------------------------------------------------

message HostTurnFrame {
  oneof payload {
    StartTurnRequest start_turn = 1;
    PreToolUseDecision pre_tool_use_decision = 2;
    PostToolUseAck post_tool_use_ack = 3;
    InvokeHostToolResponse invoke_host_tool_response = 4;
  }
}

message StartTurnRequest {
  string session_id = 1;
  string turn_id = 2;
  string prompt = 3;
}

message PreToolUseDecision {
  string hook_call_id = 1;
  bool allow = 2;
  string deny_reason = 3;             // allow=false 时反馈给大模型的拒绝原因（如配额耗尽、需先调用 sofa_checkpoint、等待人工确认）
  bytes modified_arguments_json = 4;  // 可选：宿主机改写后的参数 JSON
  string additional_context = 5;      // 可选：宿主机注入的人工日记插话上下文
}

message PostToolUseAck {
  string hook_call_id = 1;
}

enum HostToolResultType {
  HOST_TOOL_RESULT_TYPE_UNSPECIFIED = 0;
  HOST_TOOL_RESULT_TYPE_SUCCESS = 1;  // 正常返回（含 {"accepted": false, "issues": [...]} 等可由模型自修的协议级拒绝）
  HOST_TOOL_RESULT_TYPE_FAILURE = 2;  // 门禁硬拒绝（对齐 r42 ResultType="failure"）
}

message InvokeHostToolResponse {
  string tool_call_id = 1;
  HostToolResultType result_type = 2;
  string text_result_for_llm = 3;
  string handler_error = 4;            // 非空表示工具自身执行异常（非业务级可修复响应），宿主机同时计入失败日志并回滚配额
}

message PluginTurnFrame {
  oneof payload {
    SessionEvent event = 1;
    PreToolUseRequest pre_tool_use = 2;
    PostToolUseRequest post_tool_use = 3;
    InvokeHostToolRequest invoke_host_tool = 4;
    TurnResult turn_result = 5;
  }
}

// ⚠️ 防双重拦截硬约束：
// PreToolUseRequest 与 PostToolUseRequest **仅针对非宿主机工具**（即插件内置工具如 bash/view/edit 及 MCP 工具）发送！
// 凡名字属于 OpenSessionRequest.host_tools 的工具，插件直接发送 InvokeHostToolRequest 交由宿主机统一拦截处理，不得重复触发 PreToolUse/PostToolUse。
message PreToolUseRequest {
  string hook_call_id = 1;
  string tool_call_id = 2;
  string tool_name = 3;
  bytes arguments_json = 4;
}

message PostToolUseRequest {
  string hook_call_id = 1;
  string tool_call_id = 2;
  string tool_name = 3;
  bool succeeded = 4;         // false 时宿主机触发 quota.rollback(tool_name)，1:1 对齐 r42 OnPostToolUseFailure
  string error_message = 5;
}

message InvokeHostToolRequest {
  string tool_call_id = 1;
  string tool_name = 2;
  bytes arguments_json = 3;
}

enum SessionEventKind {
  SESSION_EVENT_KIND_UNSPECIFIED = 0;
  SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA = 1;
  SESSION_EVENT_KIND_ASSISTANT_MESSAGE = 2;
  SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA = 3;
  SESSION_EVENT_KIND_ASSISTANT_REASONING = 4;
  SESSION_EVENT_KIND_ASSISTANT_USAGE = 5;
  SESSION_EVENT_KIND_ASSISTANT_TOOL_CALL_DELTA = 6;
  SESSION_EVENT_KIND_TOOL_EXECUTION_START = 7;
  SESSION_EVENT_KIND_TOOL_EXECUTION_PROGRESS = 8;
  SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT = 9;
  SESSION_EVENT_KIND_TOOL_EXECUTION_COMPLETE = 10;
  SESSION_EVENT_KIND_SUBAGENT_STARTED = 11;
  SESSION_EVENT_KIND_SUBAGENT_COMPLETED = 12;
  SESSION_EVENT_KIND_SUBAGENT_FAILED = 13;
  SESSION_EVENT_KIND_SESSION_WARNING = 14;
  SESSION_EVENT_KIND_SESSION_IDLE = 15;
}

message TokenUsage {
  string model = 1;
  string api_call_id = 2;
  int64 input_tokens = 3;
  int64 output_tokens = 4;
  int64 reasoning_tokens = 5;
  int64 cache_read_tokens = 6;
  int64 cache_write_tokens = 7;
}

message SessionEvent {
  string event_id = 1;
  int64 timestamp_unix_nano = 2;
  SessionEventKind kind = 3;
  string message_id = 4;       // assistant message_id 或 reasoning_id
  string tool_call_id = 5;
  string tool_name = 6;
  string subagent_name = 7;
  string text = 8;             // 增量/全量文本、思考内容、进度说明、shell 输出块或警告信息
  bytes arguments_json = 9;    // TOOL_EXECUTION_START 时的参数快照
  bytes result_json = 10;      // TOOL_EXECUTION_COMPLETE 时的结果快照
  string error_message = 11;   // 工具或子 Agent 失败原因
  TokenUsage usage = 12;       // ASSISTANT_USAGE 时的 Token 统计
  bytes raw_sdk_event_json = 13; // 可选：底层 SDK 原始事件 JSON（供 BlobStore 调试日志存档）
}

message TurnResult {
  string turn_id = 1;
  string assistant_message_id = 2;
  string final_content = 3;
  ErrorDetail error = 4;       // 非空表示本轮对话失败（含模型限流、鉴权错误、上下文超限等）
}

// ---------------------------------------------------------------------------
// 统一错误分类（供宿主机 provider.RetryPolicy 精确判定可重试/永久失败）
// ---------------------------------------------------------------------------

enum ErrorCategory {
  ERROR_CATEGORY_UNSPECIFIED = 0;
  ERROR_CATEGORY_TRANSIENT = 1;              // 网络抖动、5xx、连接断开等可重试错误
  ERROR_CATEGORY_RATE_LIMIT = 2;             // 429 速率限制（可重试，对齐 r42 ErrorType=="rate_limit"）
  ERROR_CATEGORY_AUTHENTICATION = 3;         // 401 凭据无效（永久失败，不重试）
  ERROR_CATEGORY_AUTHORIZATION = 4;          // 403 无权限（永久失败，不重试）
  ERROR_CATEGORY_QUOTA = 5;                  // 账户余额/配额耗尽（永久失败，不重试）
  ERROR_CATEGORY_CONTEXT_LIMIT = 6;          // 上下文窗口超限（永久失败，不重试）
  ERROR_CATEGORY_SESSION_NOT_FOUND = 7;      // Resume 时底层会话不存在（宿主机可回退新建会话重放日记）
  ERROR_CATEGORY_UNSUPPORTED_CAPABILITY = 8; // 当前 Harness 不支持请求的提供方类型或特性（永久失败）
  ERROR_CATEGORY_INTERNAL = 9;               // 其他未分类错误（交由宿主机 error_message_regex / HTTP 状态码规则兜底判定）
}

message ErrorDetail {
  ErrorCategory category = 1;
  int32 http_status_code = 2;
  string message = 3;
}
```

---

## 4. `RunTurn` 时序闭环（单轮对话与门禁交互）

```mermaid
sequenceDiagram
    participant Host as 宿主机 sofa-worker (Go)
    participant Plugin as 容器内 AgentHarness 插件 (gRPC Server)
    participant SDK as 底层 SDK / CLI (copilot 或 antigravity)

    Host->>Plugin: RunTurn: StartTurnRequest(session_id, turn_id, prompt)
    Plugin->>SDK: 发起本轮对话 (SendAndWait / agent.chat)

    loop 模型流式思考与工具调用
        SDK-->>Plugin: ReasoningDelta / MessageDelta / ShellOutput
        Plugin-->>Host: SessionEvent (实时推送 WebSocket 看板 + 刷新 StallWatchdog)

        alt 模型调用插件内置工具 (如 bash / edit) 或 MCP 工具
            SDK->>Plugin: OnPreToolUse(tool_name, args)
            Plugin-->>Host: PreToolUseRequest(hook_call_id, tool_name, args)
            Note over Host: 1. 查 NodeActor 人工日记插话<br/>2. 查 checkpointPending / roundComplete 门禁<br/>3. 预扣内置 tool_call_quota
            Host->>Plugin: PreToolUseDecision(allow / deny + reason / additional_context)
            alt allow == true
                Plugin->>SDK: 放行执行内置工具
                SDK-->>Plugin: OnPostToolUse / OnPostToolUseFailure
                Plugin-->>Host: PostToolUseRequest(succeeded, error_message)
                Note over Host: succeeded==false 时回滚内置配额;<br/>succeeded==true 时递增检查点轮次计数
                Host->>Plugin: PostToolUseAck
            else allow == false
                Plugin->>SDK: 拒绝执行并将 deny_reason 反馈给模型
            end
        else 模型调用宿主机工具 (sofa_checkpoint / sofa_request_decision / go_tool / external_tool)
            SDK->>Plugin: HostTool Wrapper Handler(tool_name, args)
            Plugin-->>Host: InvokeHostToolRequest(tool_call_id, tool_name, args)
            Note over Host: 1. 门禁检查与 cty 输入约束校验<br/>2. 执行宿主机 Handler（或容器内运行 external_tool）<br/>3. 校验 postcondition，若为完成/暂停工具则置位 roundComplete
            Host->>Plugin: InvokeHostToolResponse(result_type, text_result_for_llm)
            Plugin->>SDK: 返回 ToolResult 给模型
        end
    end

    SDK-->>Plugin: 本轮回复结束 (SessionIdle)
    Plugin-->>Host: TurnResult(final_content) & 关闭服务端流
```

---

## 5. 宿主机 Go 领域层抽象接口（`internal/harness/runtime`）

在 `sofa` 主仓内部，业务编排代码（Work Runner、QC Runner、Stall Watchdog）不直接依赖 Protobuf 生成结构体，而是面向如下纯 Go 领域接口编程（Tier 1 单元测试可纯内存 Mock，无需容器或 gRPC）：

```go
package runtime

type ToolHandler func(ctx context.Context, call ToolInvocation) (ToolOutput, error)

type PreToolUseHook func(ctx context.Context, input PreToolUseInput) (PreToolUseOutput, error)
type PostToolUseHook func(ctx context.Context, input PostToolUseInput) error

type Session interface {
    ID() string
    RunTurn(ctx context.Context, prompt string, onEvent func(Event)) (TurnSummary, error)
    Abort(ctx context.Context) error
    Resume(ctx context.Context) error
    ReadMCPResource(ctx context.Context, serverName, uri string) ([]MCPResourceContent, error)
    Close(ctx context.Context) error
}

type Opener interface {
    Capabilities(ctx context.Context) (Capabilities, error)
    Open(ctx context.Context, cfg SessionConfig) (Session, error)
}
```

---

## 6. 与 `sofa-harness-copilot`（对比 `r42`）及 `sofa-harness-antigravity` 的 1:1 映射表

| 协议能力点 | `sofa-harness-copilot`（Go + `copilot-sdk/go`）映射方式 | `sofa-harness-antigravity`（Python + `google-antigravity`）映射方式 |
| :--- | :--- | :--- |
| **底层进程启动** | `sdk.NewClient(&sdk.ClientOptions{Connection: sdk.StdioConnection{Path: cliPath}})`（`harness_env` 仅传给 `copilot` CLI 进程环境） | `async with Agent(LocalAgentConfig(...))`（`harness_env` 注入 SDK 初始化配置，隔离于命令工具环境） |
| **`OpenSession` 模型与推理** | `SessionConfig{Model, ReasoningEffort, SystemMessage{Mode: "append"}, WorkingDirectory, ConfigDirectory, Streaming: true}` | `LocalAgentConfig(system_instructions=..., capabilities=CapabilitiesConfig(...))` |
| **正交 `ProviderConfig`** | 1:1 映射为 `&sdk.ProviderConfig{Type, BaseURL, WireAPI, Transport, Headers, ModelID, WireModel, APIKey/BearerToken}`；未传时为 `nil` | 未传时使用原生认证（如 `GEMINI_API_KEY`）；若传入不支持的第三方 `ProviderConfig.type`，返回 `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY` |
| **`host_tools` 挂载** | 每个 `HostToolSpec` 构造一个 `sdk.Tool{Name, Description, Parameters, Handler}`，`Handler` 向当前活跃 `RunTurn` 流发 `InvokeHostToolRequest` 并阻塞等待 `InvokeHostToolResponse` | 每个 `HostToolSpec` 动态包装为异步 Python Tool 函数，通过当前 `RunTurn` 的 `asyncio.Queue` 发送 `InvokeHostToolRequest` 并 `await` 对应 `Future` |
| **`PreToolUse` / `PostToolUse`** | 绑定 `sdk.SessionHooks{OnPreToolUse, OnPostToolUse, OnPostToolUseFailure}`（若 `input.ToolName` 在 `host_tools` 集合中则直接放行跳过，避免与 `InvokeHostTool` 双重拦截） | 在内置工具执行包装层前置发送 `PreToolUseRequest`（支持 `deny` 拦截与上下文注入），捕获成功/异常后发送 `PostToolUseRequest` |
| **Skills 与过滤列表** | `SkillDirectories`、`DisabledSkills`、`AvailableTools`、`ExcludedTools`；当 `len(skills) > 0` 时与 `r42` 一致构造 `CustomAgents` | 映射至 `LocalAgentConfig` 技能路径与工具启用开关 |
| **MCP Servers & Resources** | `MCPHTTPServerConfig` / `MCPStdioServerConfig`；`ReadMCPResource` 调 `session.RPC.MCP.Resources().Read` | 映射至 SDK MCP 配置（或插件内 MCP Client 读取资源） |
| **流式事件与 Token 统计** | `session.On(...)` 转发 `AssistantMessageDelta`、`AssistantReasoningDelta`、`AssistantUsage`、`ToolExecutionStart/Progress/ShellOutput/Complete`、`Subagent*` | 并发迭代 `response`（文本增量）、`response.thoughts`（思考增量）与 `response.tool_calls`（工具事件）写出 `SessionEvent` |
| **`AbortTurn` & `ResumeSession`** | 分别调用 `session.Abort(ctx)` 与 `Disconnect()` + `client.ResumeSession(ctx, sessionID, resumeConfig)` | `AbortTurn` 取消当前轮次 `asyncio.Task`；`ResumeSession` 校验或重建底层会话句柄 |
| **错误分类归一化** | 读取 `sdk.SessionErrorData{ErrorType, StatusCode}`（`rate_limit` → `RATE_LIMIT`；`authentication`/`authorization`/`quota`/`context_limit` 逐一对应） | 捕获 SDK/HTTP 异常状态码（429/401/403/400 token limit）映射至对应 `ErrorCategory` |
