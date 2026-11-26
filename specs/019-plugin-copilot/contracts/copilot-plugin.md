# Contract: `sofa-harness-copilot` gRPC Plugin (`AgentHarness` v1)

## 1. Module Isolation Boundary
- `plugins/sofa-harness-copilot/go.mod` declares `module github.com/lonegunmanb/sofa-harness-copilot` and `replace github.com/lonegunmanb/sofa => ../..`.
- Imports from `github.com/lonegunmanb/sofa` are restricted strictly to `pkg/harnessplugin` and `pkg/harnessplugin/v1`.
- Root `github.com/lonegunmanb/sofa` MUST NOT import `github.com/github/copilot-sdk/go` or `plugins/sofa-harness-copilot`.

## 2. RPC Contracts

### `GetCapabilities`
Returns:
- `harness_name`: `"copilot"`
- `protocol_version`: `1`
- `supported_provider_types`: `["openai", "azure", "anthropic"]`
- `supports_session_resume`: `true`
- `supports_mcp_servers`: `true`
- `supports_mcp_resources`: `true`
- `supports_skills`: `true`

### `OpenSession`
- **CLI Path Resolution**: `<execDir>/bin/copilot` (if regular file) -> `harness_options_json.cli_path` -> `exec.LookPath("copilot")`.
- **Env Isolation**: `HarnessEnv` is merged into `StdioConnection.Env` only; `os.Setenv` MUST NOT be called.
- **Provider Mapping**:
  - `Provider == nil` -> `sdk.SessionConfig.Provider = nil`.
  - `Provider.Type` not in `{"openai", "azure", "anthropic"}` -> `OpenSessionResponse{Error: {Category: ERROR_CATEGORY_UNSUPPORTED_CAPABILITY, HttpStatusCode: 400}}`.
  - Otherwise 1:1 mapping of `Type`, `BaseURL`, `WireAPI`, `Transport`, `Headers`, `ModelID` (fallback `profile`), `WireModel` (fallback `req.Model`), `APIKey` / `BearerToken`.
- **Tool Filters**: `ToolFilter{Configured: true, Names: []}` -> `[]string{}`; `ToolFilter{Configured: false}` or `nil` -> `nil`.
- **Skills & Custom Agent**: `EnableSkills = Bool(true)`, `SkillDirectories`, `DisabledSkills`; when `len(Skills) > 0`, `CustomAgents = [{Name: "sofa_worker", Prompt: SystemPrompt, Skills: Skills, Model: profile, ReasoningEffort: ReasoningEffort}]`, `Agent = "sofa_worker"`.
- **Resume**: When `ResumeSessionId != ""`, invokes `client.ResumeSession`. If error contains `"Session not found"` (case-insensitive `"session not found"`), returns `OpenSessionResponse{Error: {Category: ERROR_CATEGORY_SESSION_NOT_FOUND, HttpStatusCode: 404}}` without calling `CreateSession`.

### `RunTurn`
- First frame MUST be `StartTurnRequest`.
- Concurrent reader loop dispatches incoming `PreToolUseDecision`, `PostToolUseAck`, and `InvokeHostToolResponse` frames by `hook_call_id` / `tool_call_id`.
- `HostToolSpec` wrappers invoke `InvokeHostToolRequest` and map `HOST_TOOL_RESULT_TYPE_SUCCESS` -> `"success"`, `HOST_TOOL_RESULT_TYPE_FAILURE` -> `"failure"`.
- `OnPreToolUse`, `OnPostToolUse`, `OnPostToolUseFailure` skip `hostToolNames` on their first line; non-host tools round-trip `PreToolUseRequest` / `PostToolUseRequest` over the stream.
- All `stream.Send` calls are serialized via `activeTurn.sendMu`.

### `AbortTurn`, `ResumeSession`, `ReadMCPResource`, `CloseSession`
- `AbortTurn` cancels the active turn context and calls `sdkSession.Abort(ctx)` without waiting on `turnMu`.
- `ResumeSession` disconnects the current `sdkSession` and calls `client.ResumeSession` with cloned `resumeCfg`.
- `ReadMCPResource` calls `sdkSession.ReadMCPResource(ctx, serverName, uri)`.
- `CloseSession` disconnects `sdkSession`, stops `sdkClient`, and removes the session from the map.
