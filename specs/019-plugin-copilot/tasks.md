# Tasks: `019-plugin-copilot`

**Input**: Design documents from `specs/019-plugin-copilot/`

## Phase 1: Sub-Module Setup

- [X] T001 Initialize `plugins/sofa-harness-copilot/go.mod` (`module github.com/lonegunmanb/sofa-harness-copilot`, `replace github.com/lonegunmanb/sofa => ../..`, `github.com/github/copilot-sdk/go v1.0.19`) without modifying root `d:\project\sofa\go.mod`.

## Phase 2: Offline Plugin Unit Tests (TDD Red First)

- [X] T002 [Red] Author comprehensive offline unit tests in `plugins/sofa-harness-copilot/internal/server/server_test.go` covering: `GetCapabilities`, CLI discovery precedence (`<execDir>/bin/copilot` -> `options["cli_path"]` -> `LookPath`), `HarnessEnv` injected only into `StdioConnection.Env` (not `os.Getenv`), orthogonal `ProviderConfig` mapping, `Provider == nil` native auth, unsupported `Provider.Type` -> `UNSUPPORTED_CAPABILITY`, `ToolFilter.Configured` (`nil` vs `[]string{}`), `Skills` -> `CustomAgents("sofa_worker")`, `ResumeSession` `"Session not found"` -> `SESSION_NOT_FOUND` without silent create, `bash` `PreToolUse` (allow/deny/modifiedArgs/additionalContext) -> `PostToolUse(succeeded=false)` vs `sofa_checkpoint` host tool skipping `PreToolUse`/`PostToolUse`, streaming event forwarding (`AssistantMessageDelta`, `AssistantReasoningDelta`, `AssistantUsage` 5 counters, `ToolExecution*`, `ToolShellOutput`, `Subagent*`, `SessionWarning`, `SessionIdle`), concurrent `AbortTurn`, `ResumeSession` & `ReadMCPResource` & `CloseSession`, and all 6+ `ErrorCategory` mappings.
- [X] T003 Verify tests fail before implementation (`go test ./...` fails to compile).

## Phase 3: `sofa-harness-copilot` Implementation (Green + Refactor)

- [X] T004 Implement `plugins/sofa-harness-copilot/internal/server/sdk_iface.go` (`sdkClient`, `sdkSession`, `officialClient`, `officialSession`, `modelCallError`, `ReadMCPResource`).
- [X] T005 Implement `plugins/sofa-harness-copilot/internal/server/errors.go` (`classifyError` mapping `sdk.SessionErrorData`, `RPCError`, HTTP status codes, and error patterns to `harnessv1.ErrorDetail`).
- [X] T006 Implement `plugins/sofa-harness-copilot/internal/server/server.go` (7-RPC `AgentHarnessServer` with serialized `stream.Send`, `hostToolNames` hook guard, and non-blocking `AbortTurn`).
- [X] T007 Implement `plugins/sofa-harness-copilot/cmd/sofa-harness-copilot/main.go` calling `harnessplugin.Serve(server.New())`.
- [X] T008 Implement `plugins/sofa-harness-copilot/scripts/build-bundle.go` generating `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}`.
- [X] T009 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` inside `plugins/sofa-harness-copilot` until green.

## Phase 4: Tier 2 Podman Container End-to-End Integration (TDD Red -> Green)

- [X] T010 [Red] Author `internal/worker/runner/testdata/fakecopilot/main.go` (stdlib-only JSON-RPC 2.0 stdio CLI stub speaking `connect`, `session.create`, `session.send`, `session.event`, `hooks.invoke`, `session.tools.handlePendingToolCall`, `session.detach`, `runtime.shutdown`) and `internal/worker/runner/copilot_plugin_integration_test.go` (`//go:build integration`) staging `<plugin_dir>/sofa-harness-copilot/linux_<arch>/{sofa-harness-copilot, bin/copilot}`, configuring `harness "copilot"` with zero hardcoded paths, and running `WorkerRunner.Run` end-to-end inside Podman `alpine:latest`.
- [X] T011 Run `go test -tags=integration ./internal/worker/runner/... -count=1` and verify Work -> `PreToolUse`/`PostToolUse` -> `sofa_checkpoint` -> `sofa_complete_work` -> Freeze -> Gatekeeper -> Read-Only QC -> `work_branch` + `BlobStore` artifacts pass with zero leftover Podman containers.

## Phase 5: Final Isolation & Verification Gate

- [X] T012 Verify `git diff go.mod` in `d:\project\sofa` is empty, run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1` in both `plugins/sofa-harness-copilot` and `d:\project\sofa`, and leave working tree uncommitted for parent review.

## Phase 6: Unbounded Turn Deadline Fix (`copilot-sdk/go` 60s Default Timeout Bypass)

- [X] T013 [Red] Add `TestRunTurnSuppliesFarFutureDeadlineToBypassSDK60sDefault` in `plugins/sofa-harness-copilot/internal/server/server_test.go` verifying that `RunTurn` without an incoming stream deadline passes a context with `Deadline() > 24h` to `sdkSession.SendAndWait` while preserving explicit caller deadlines and immediate `AbortTurn` cancellation; confirm test fails before implementation.
- [X] T014 [Green] Implement `ensureTurnDeadline` in `plugins/sofa-harness-copilot/internal/server/sdk_iface.go` and wire it in both `Server.RunTurn` and `officialSession.SendAndWait`.
- [X] T015 Verify `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` in `plugins/sofa-harness-copilot` and root `d:\project\sofa`, and reinstall local Linux `sofa-harness-copilot` bundle.
