# Research Notes: `019-plugin-copilot`

## Findings from Direct Source Inspection

1. **`copilot-sdk/go@v1.0.19` Client & Env Isolation (`client.go`)**:
   - `validateEnvironmentOptions` panics if both `StdioConnection.Env` and `ClientOptions.Env` are non-nil. Therefore `HarnessEnv` MUST be merged into `StdioConnection{Path: cliPath, Env: childEnv}` with `ClientOptions.Env == nil`.
   - `startCLIServer` always launches `<cliPath> --headless --no-auto-update --stdio` (plus `--no-auto-login` if `UseLoggedInUser` is `false`).
   - Protocol handshake sends JSON-RPC `"connect"` expecting `rpc.ConnectResult{Ok: true, ProtocolVersion: 3, Version: "..."}` (`minProtocolVersion = 3`, `SDKProtocolVersion = 3`), falling back to `"ping"` if `"connect"` returns `-32601` MethodNotFound.

2. **`copilot-sdk/go@v1.0.19` Session, Hooks, Tools & Events (`session.go`, `types.go`, `rpc/zsession_events.go`, `rpc/zrpc.go`)**:
   - `SessionConfig` supports `SessionID`, `Model`, `ReasoningEffort`, `SystemMessage` (`Mode: "append"`), `WorkingDirectory`, `ConfigDirectory`, `Streaming`, `Provider`, `Tools`, `AvailableTools`, `ExcludedTools`, `OnPermissionRequest`, `EnableSkills`, `SkillDirectories`, `DisabledSkills`, `EnableSessionStore`, `SkipEmbeddingRetrieval`, `LargeOutput`, `Hooks`, `MCPServers`, `CustomAgents`, `Agent`.
   - `SessionHooks` has `OnPreToolUse`, `OnPostToolUse`, and `OnPostToolUseFailure` (invoked via JSON-RPC `hooks.invoke` with `hookType: "preToolUse" | "postToolUse" | "postToolUseFailure"`).
   - External tools (`sdk.Tool` with `Handler`) are triggered by `session.event` notifications of type `"external_tool.requested"` (`ExternalToolRequestedData{RequestID, SessionID, ToolCallID, ToolName, Arguments}`), and answered via `session.tools.handlePendingToolCall` (`HandlePendingToolCallRequest{SessionID, RequestID, Result: &ExternalToolTextResultForLlm{TextResultForLlm, ResultType}}`).
   - `session.On` dispatches `SessionEvent` asynchronously via a buffered `eventCh` + `processEvents` goroutine; `SendAndWait` completes on root `session.idle` or `session.error` after registered `On` handlers finish the completing event.
   - `rpc.MCPResourceContent` has `URI string`, `MIMEType *string`, `Text *string`, `Blob *string` (base64-encoded), and `Meta map[string]any`. Conversion to `harnessv1.MCPResourceContent` dereferences pointers, decodes base64 `Blob` (falling back to raw bytes if non-base64), and stringifies `Meta` values.
   - `ToolShellOutputData` (`Text`, `ToolCallID`) and `ToolExecutionPartialResultData` (`PartialOutput`, `ToolCallID`) both map cleanly to `SESSION_EVENT_KIND_TOOL_SHELL_OUTPUT`.

3. **JSON-RPC 2.0 Framing (`internal/jsonrpc2/frame.go`)**:
   - Uses LSP-style `Content-Length: <N>\r\n\r\n<JSON>` framing over stdin/stdout pipes.
   - Our Tier 2 `testdata/fakecopilot` stub can speak this exact framing using only Go stdlib so the real `copilot-sdk/go@v1.0.19` client runs unmodified inside Podman `alpine:latest`.

4. **Host Installer Discovery (`internal/harness/installer/installer.go`)**:
   - `prepareLocalPlugin` searches `<PluginDir>/sofa-harness-<name>/linux_<arch>/sofa-harness-<name>` first, then `<PluginDir>/sofa-harness-<name>/sofa-harness-<name>`.
   - `ContainerRunner` mounts `filepath.Dir(candidate)` read-only at `/.sofa/harness/`, so `<dir>/bin/copilot` is located at `/.sofa/harness/bin/copilot` (`filepath.Join(filepath.Dir(exePath), "bin", "copilot")`).
