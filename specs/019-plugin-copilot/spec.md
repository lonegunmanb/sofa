# Feature Specification: Isolated In-Repo `sofa-harness-copilot` Plugin (`019-plugin-copilot`)

**Feature Branch**: `019-plugin-copilot`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `019` blueprint (`docs/stage2/019-plugin-copilot.md`), `docs/harness-plugin-contract.md`, `proto/sofa/harness/v1/harness.proto`, `pkg/harnessplugin/plugin.go`, `internal/harness/installer/installer.go`, `internal/worker/runner/worker_runner.go`, and reference implementation `r42/internal/copilot`.

## User Scenarios & Testing

### User Story 1 - Compiler-Isolated `sofa-harness-copilot` Sub-Module & 7-RPC Server (`plugins/sofa-harness-copilot/`) (Priority: P1)

`plugins/sofa-harness-copilot` incubates the official Go Copilot AgentHarness plugin as an independent Go module (`github.com/lonegunmanb/sofa-harness-copilot`) with `replace github.com/lonegunmanb/sofa => ../..`, importing only `github.com/lonegunmanb/sofa/pkg/harnessplugin` (+ `v1`) and `github.com/github/copilot-sdk/go v1.0.19`:
- Root `d:\project\sofa\go.mod` MUST NOT depend on `github.com/github/copilot-sdk/go` or import `plugins/sofa-harness-copilot`.
- `plugins/sofa-harness-copilot` MUST NOT import any `github.com/lonegunmanb/sofa/internal/...` package.
- `cmd/sofa-harness-copilot/main.go` boots the plugin via `harnessplugin.Serve(...)`.
- `GetCapabilities` returns `HarnessName: "copilot"`, `ProtocolVersion: 1`, `SupportedProviderTypes: ["openai", "azure", "anthropic"]`, `SupportsSessionResume: true`, `SupportsMcpServers: true`, `SupportsMcpResources: true`, `SupportsSkills: true`.
- `copilot` CLI binary discovery resolves `<executableDir>/bin/copilot`, falling back to `harness_options_json["cli_path"]`, then `exec.LookPath("copilot")`.
- `OpenSessionRequest.HarnessEnv` (e.g., `GITHUB_TOKEN`) is merged exclusively into `sdk.StdioConnection.Env` for the spawned `copilot` CLI process and NEVER written to the plugin's process environment via `os.Setenv`.

**Independent Test**: Offline unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` using an injected fake `sdkClient` / `sdkSession` verify capabilities, CLI path discovery, `StdioConnection.Env` isolation, orthogonal `ProviderConfig` mapping, `Provider == nil` native auth, unsupported provider rejection (`ERROR_CATEGORY_UNSUPPORTED_CAPABILITY`), `ToolFilter.Configured` (`nil` vs `[]string{}`), `Skills` -> `CustomAgents` (`"sofa_worker"`), and `ResumeSessionID` missing (`ERROR_CATEGORY_SESSION_NOT_FOUND` without silent new-session fallback).

**Acceptance Scenarios**:

1. **Given** `Provider` with `Type: "openai"|"azure"|"anthropic"`, `BaseUrl`, `WireApi`, `Transport`, `Headers`, `ModelId`, `WireModel`, and `APIKey`/`BearerToken`, **When** `OpenSession` runs, **Then** the SDK receives an exact 1:1 `sdk.ProviderConfig`; when `Provider == nil`, `sdk.SessionConfig.Provider` is `nil`; when `Provider.Type` is unsupported, `OpenSession` returns `OpenSessionResponse{Error: {Category: ERROR_CATEGORY_UNSUPPORTED_CAPABILITY}}`.
2. **Given** `ResumeSessionId != ""` and `client.ResumeSession` fails with `"Session not found"`, **When** `OpenSession` runs, **Then** it MUST NOT call `CreateSession` and MUST return `OpenSessionResponse{Error: {Category: ERROR_CATEGORY_SESSION_NOT_FOUND}}`.
3. **Given** `AvailableTools` or `ExcludedTools` with `Configured: true` and empty `Names`, **When** `OpenSession` runs, **Then** the SDK receives non-nil `[]string{}` rather than `nil`; when `Configured: false`, the SDK receives `nil`.
4. **Given** `len(req.Skills) > 0`, **When** `OpenSession` runs, **Then** `sdk.SessionConfig` sets `CustomAgents: [{Name: "sofa_worker", Prompt: req.SystemPrompt, Skills: req.Skills, Model: profile, ReasoningEffort: req.ReasoningEffort}]` and `Agent: "sofa_worker"`.

---

### User Story 2 - Bidirectional `RunTurn` Streaming, Host Tool Wrapping & Anti-Double-Interception Hooks (Priority: P1)

`RunTurn` bridges a single Copilot turn over the bidirectional `PluginTurnFrame` / `HostTurnFrame` stream:
1. Every `HostToolSpec` in `OpenSessionRequest.HostTools` is tracked in `hostToolNames` and wrapped as an `sdk.Tool` whose `Handler` emits `InvokeHostToolRequest`, awaits the matching `InvokeHostToolResponse`, and maps `HOST_TOOL_RESULT_TYPE_SUCCESS` -> `"success"` and `HOST_TOOL_RESULT_TYPE_FAILURE` -> `"failure"`.
2. `SessionHooks` (`OnPreToolUse`, `OnPostToolUse`, `OnPostToolUseFailure`) inspect `hostToolNames` on their **first line** and immediately allow/return without sending `PreToolUseRequest` or `PostToolUseRequest` for host tools (preventing double quota deduction). Non-host tools (e.g., `bash`, MCP tools) send `PreToolUseRequest` (applying `allow`, `deny_reason`, `modified_arguments_json`, `additional_context`) and `PostToolUseRequest{Succeeded: true/false}` (1:1 with `r42` `OnPostToolUseFailure`).
3. `session.On(...)` streams `AssistantMessageDelta`, `AssistantMessage`, `AssistantReasoningDelta`, `AssistantReasoning`, `AssistantUsage` (all 5 token counters), `AssistantToolCallDelta`, `ToolExecutionStart`, `ToolExecutionProgress`, `ToolShellOutput` / `ToolExecutionPartialResult` -> `TOOL_SHELL_OUTPUT`, `ToolExecutionComplete`, `SubagentStarted/Completed/Failed`, `SessionWarning`, and `SessionIdle` safely under a stream-send mutex.
4. `AbortTurn` cancels/aborts the active turn without acquiring the `RunTurn` execution lock; `ResumeSession`, `ReadMCPResource`, and `CloseSession` delegate cleanly and normalize errors via `errors.go` (`RATE_LIMIT`, `AUTHENTICATION`, `AUTHORIZATION`, `QUOTA`, `CONTEXT_LIMIT`, `SESSION_NOT_FOUND`, `UNSUPPORTED_CAPABILITY`, `TRANSIENT`, `INTERNAL`).

**Independent Test**: Offline unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` verify `bash` triggers `PreToolUse` -> `PostToolUse(succeeded=false)` while `sofa_checkpoint` triggers only `InvokeHostTool` and zero `PreToolUse`/`PostToolUse` frames; concurrent `AbortTurn` unblocks `SendAndWait` cleanly; `ReadMCPResource` and all error categories map deterministically.

**Acceptance Scenarios**:

1. **Given** an active `RunTurn` where the model invokes `bash` (built-in) and `sofa_checkpoint` (host tool), **When** SDK hooks and tool handlers run, **Then** `bash` emits `PreToolUseRequest` and `PostToolUseRequest(succeeded=false)`, while `sofa_checkpoint` emits only `InvokeHostToolRequest` and never emits `PreToolUseRequest` or `PostToolUseRequest`.
2. **Given** a blocked `SendAndWait` in `RunTurn`, **When** `AbortTurn` is invoked concurrently, **Then** `session.Abort` runs immediately, the active turn context cancels, and `RunTurn` exits cleanly with a `TurnResult` error.
3. **Given** `sdk.SessionErrorData` or HTTP/transport errors (`429`/`rate_limit`, `401`/`authentication`, `403`/`authorization`, `quota`, `context_limit`, `Session not found`, `5xx`/network), **When** `classifyError` runs, **Then** it produces the exact matching `harnessv1.ErrorCategory` and `HttpStatusCode`.
4. **Given** an incoming `RunTurn` stream context with no gRPC deadline (`_, ok := ctx.Deadline(); !ok`), **When** `RunTurn` / `officialSession.SendAndWait` invokes `copilot-sdk/go` `Session.SendAndWait`, **Then** it attaches a far-future safety deadline (`>= 24h`) so `copilot-sdk/go` never falls back to its hardcoded `60s` default timeout while still honoring explicit caller deadlines and immediate cancellation via `AbortTurn` or stream teardown.

---

### User Story 3 - Self-Contained Linux Bundle Script & Tier 2 Podman Container End-to-End Integration (`scripts/build-bundle.go`, `internal/worker/runner/copilot_plugin_integration_test.go`) (Priority: P1)

- `plugins/sofa-harness-copilot/scripts/build-bundle.go` builds a self-contained Linux plugin directory `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}` (`CGO_ENABLED=0 GOOS=linux GOARCH=<arch>`), supporting `-copilotsrc` (to compile a Go helper/stub as `bin/copilot`) or `-copilotbin` (to copy a prebuilt Linux `copilot` binary).
- `internal/worker/runner/copilot_plugin_integration_test.go` (`//go:build integration`) cross-compiles `sofa-harness-copilot` and a lightweight Go stdio JSON-RPC `bin/copilot` stub into `<plugin_dir>/sofa-harness-copilot/linux_<arch>/{sofa-harness-copilot, bin/copilot}`, configures `harness "copilot"` with zero hardcoded binary paths (exercising `013` local plugin directory discovery), and runs `018` `WorkerRunner.Run` end-to-end inside Podman `alpine:latest` (`AutoMTLS`, `PreToolUse`/`PostToolUse`, `sofa_checkpoint`, `sofa_complete_work`, Gatekeeper, Read-Only QC, `work_branch` + `BlobStore` artifacts, zero leftover containers).

**Independent Test**: `go test -tags=integration ./internal/worker/runner/... -count=1` passes against real Podman `alpine:latest` and real local Git bare repositories with zero external internet or live `GITHUB_TOKEN` required.

## Requirements

- **FR-001**: `plugins/sofa-harness-copilot/go.mod` MUST declare `module github.com/lonegunmanb/sofa-harness-copilot` with `replace github.com/lonegunmanb/sofa => ../..`; root `d:\project\sofa\go.mod` MUST remain completely free of `github.com/github/copilot-sdk/go`.
- **FR-002**: `plugins/sofa-harness-copilot/internal/server/sdk_iface.go` MUST define thin `sdkClient` / `sdkSession` interfaces and the `copilot-sdk/go v1.0.19` adapter (`officialClient` / `officialSession`).
- **FR-003**: `plugins/sofa-harness-copilot/internal/server/errors.go` MUST map `sdk.SessionErrorData`, `sdk.RPCError`, HTTP status codes, and error substrings into `harnessv1.ErrorDetail`.
- **FR-004**: `plugins/sofa-harness-copilot/internal/server/server.go` MUST implement all 7 `harnessv1.AgentHarnessServer` RPCs with `<execDir>/bin/copilot` -> `options["cli_path"]` -> `LookPath("copilot")` discovery, `StdioConnection.Env`-only `HarnessEnv` injection, non-reentrant `hostToolNames` hook guard, thread-safe `stream.Send`, non-blocking `AbortTurn`, and unbounded-turn deadline protection (`ensureTurnDeadline`) bypassing `copilot-sdk/go`'s `60s` no-deadline fallback.
- **FR-005**: `plugins/sofa-harness-copilot/scripts/build-bundle.go` MUST produce `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}`.
- **FR-006**: `internal/worker/runner/copilot_plugin_integration_test.go` (`//go:build integration`) MUST exercise the full `WorkerRunner.Run` loop inside Podman using `013` local plugin discovery (`harness "copilot"`) and real `copilot-sdk/go` stdio JSON-RPC over a stub `bin/copilot`.

## Success Criteria

- **SC-001**: `git diff go.mod` in `d:\project\sofa` is empty.
- **SC-002**: In `plugins/sofa-harness-copilot`, `gofmt -l .` is empty, `go vet ./...` passes, and `go test ./... -count=1` passes all offline unit tests.
- **SC-003**: In `d:\project\sofa`, `gofmt -l .` is empty, `go vet ./...` passes, `go test ./... -count=1` passes, and `go test -tags=integration ./internal/worker/runner/... -count=1` passes with zero leftover Podman containers.
