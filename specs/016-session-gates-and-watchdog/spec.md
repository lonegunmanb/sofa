# Feature Specification: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog (`016-session-gates-and-watchdog`)

**Feature Branch**: `016-session-gates-and-watchdog`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `016` blueprint (`docs/stage2/016-session-gates-and-watchdog.md`), `docs/stage2/017-two-phase-checkpoint-and-recovery.md`, `docs/stage2/018-worker-qc-and-merge-runner.md`, `docs/harness-plugin-contract.md`, `internal/harness/runtime/types.go`, `internal/tool/runtime/{executor,mcp_resource}.go`, `internal/provider/retry.go`, and `internal/worker/spec/`.

## User Scenarios & Testing

### User Story 1 - Structured Diary, Control Hooks & Four Host Gates (`internal/session/diary.go`, `gates.go`, `protocol_tools.go`) (Priority: P1)

`GateController` and Work protocol tools (`sofa_checkpoint`, `sofa_complete_work`, `sofa_request_decision`, `sofa_pause_for_human`, and conditional `sofa_escalate`) enforce host-side safety invariants across `PreToolUse`, `PostToolUse`, and `InvokeHostTool`: draining human interjections (locking mutating tools on `WaitForConfirmation`), locking tools when `checkpointPending` is set, enforcing `activeToolCalls` handoff barriers and `ValidateWorkspaceForHandoff` path checks before `roundComplete`, and driving protocol repair retries up to `max_protocol_attempts`.

**Independent Test**: Tier 1 tests in `internal/session/session_test.go` exercise normal vs `WaitForConfirmation` interjections (both in-turn and pre-turn), `checkpoint_every_rounds` locking/unlocking and failed `OnCheckpointRequested` retention, unauthorized workspace path rejection and next-turn repair on `sofa_complete_work` and custom `terminate` tools, concurrent `activeToolCalls > 1` handoff rejection, trailing tool rejection after `roundComplete`, and `max_protocol_attempts` exhaustion.

**Acceptance Scenarios**:

1. **Given** a Work session receiving a normal `HumanInterjection` (`WaitForConfirmation == false`), **When** `PreToolUse` runs, **Then** the interjection is surfaced via `PreToolUseOutput.AdditionalContext` without locking mutating tools.
2. **Given** a `HumanInterjection` with `WaitForConfirmation == true`, **When** drained in `PreToolUse` or before `RunTurn`, **Then** `mustPauseForHuman` is set, mutating tools (`bash`, `edit`, `write`, custom tools, completion tools) are rejected while read-only inspection tools (`view`, `read_file`, `grep`, `glob`, `sofa_read_mcp_resource`), `sofa_checkpoint`, and `sofa_pause_for_human` remain permitted, and pre-turn confirmation holds the scheduled prompt and exits with `OutcomeWaitingHuman` once `sofa_pause_for_human` succeeds.
3. **Given** `checkpoint_every_rounds = N`, **When** `N` turns complete with at least 1 approved business tool call each (or single-turn business tool calls exceed `N * 4`), **Then** `checkpointPending` is set and all built-in/host tools except `sofa_checkpoint` are rejected until `ValidateWorkspaceForHandoff` and `OnCheckpointRequested` succeed; if `OnCheckpointRequested` returns an error, `checkpointPending` remains locked.
4. **Given** a Work completion call (`sofa_complete_work` when no custom `terminate` tool exists, or a custom `terminate` tool when configured), **When** `ValidateWorkspaceForHandoff` returns unauthorized paths, **Then** a model-visible rejection (`Accepted: false`, `HandlerError: ""`) listing unauthorized paths and `writable_paths` is returned without setting `roundComplete`, allowing the model to fix paths in a subsequent turn; once accepted, `roundComplete` blocks any subsequent tool call in the same turn.
5. **Given** a worker declaring one or more `fallback` blocks with `"agent_escalated"` in `On`, **When** the Work session is configured, **Then** `sofa_escalate` is mounted and validates the requested fallback name; when no `"agent_escalated"` fallback is declared, `sofa_escalate` is omitted.

### User Story 2 - Host-Owned Cross-Round QC Issue Ledger (`internal/session/qc_ledger.go`, `protocol_tools.go`) (Priority: P1)

`QCLedger` maintains a thread-safe, serializable cross-round issue ledger with host-assigned deterministic IDs (`QC-001`, `QC-002`, ...) keyed against declared `qc.criteria`, backing `sofa_qc_open_issues`, `sofa_qc_resolve_issues`, and `sofa_qc_submit_verdict` so QC cannot declare `"pass"` while open issues remain or `"revise"` with zero open issues.

**Independent Test**: Tier 1 tests exercise multi-round QC anti-amnesia (`QC-001` & `QC-002` opened -> `revise` -> only `QC-001` resolved -> `pass` hard rejected listing `QC-002` -> `QC-002` resolved -> `pass` succeeds), empty-ledger `revise` rejection, unknown `criterion` / model-supplied `id` rejection, duplicate/unknown/already-resolved ID rejection in `ResolveIssues`, and `Snapshot`/`Restore` preserving next ID (`QC-003`) and open issues.

**Acceptance Scenarios**:

1. **Given** a `QCLedger` initialized with `criteria`, **When** `sofa_qc_open_issues` is invoked with valid entries, **Then** the host assigns sequential `"QC-001"`, `"QC-002"`, ... IDs and rejects any input referencing an unknown `criterion`, blank `code`/`message`, empty `issues`, or an explicit `"id"` field.
2. **Given** open issues `QC-001` and `QC-002`, **When** `sofa_qc_resolve_issues` resolves only `QC-001` and `sofa_qc_submit_verdict` submits `"pass"`, **Then** the verdict is hard rejected listing `QC-002`; after `QC-002` is resolved with non-empty `resolution_evidence`, `"pass"` succeeds.
3. **Given** zero active open issues, **When** `sofa_qc_submit_verdict` submits `"revise"`, **Then** it is hard rejected requiring at least one issue opened via `sofa_qc_open_issues`.
4. **Given** a `QCLedger` with open and resolved issues, **When** `Snapshot()` is restored into a new `QCLedger`, **Then** active open issues, history, and the next monotonic ID counter (`QC-003`) are preserved verbatim.

### User Story 3 - Thread-Safe Quota Tracking & Automatic Rollback (`internal/session/quota.go`) (Priority: P1)

`QuotaTracker` enforces per-tool `tool_call_quota` limits after Gates 1–3, permitting unlimited calls for unlisted tools, rejecting exhausted tools (including `limit == 0` on the first call), rolling back built-in/MCP quota on `PostToolUse(Succeeded: false)` for calls that actually reserved quota, and rolling back host tool quota when `ToolOutput.HandlerError != ""` (while consuming quota normally on model-repairable `Accepted: false` responses with `HandlerError == ""`).

**Independent Test**: Tier 1 tests verify `tool_call_quota = {"bash": 2}` across success, `PostToolUse(Succeeded: false)` rollback, second success, and 4th-call `PreToolUse` rejection; `tool_call_quota = {"bash": 0}` immediate rejection; and host tool `HandlerError != ""` rollback vs `Accepted: false` (`HandlerError == ""`) quota consumption.

**Acceptance Scenarios**:

1. **Given** `tool_call_quota = {"bash": 2}`, **When** call 1 succeeds, call 2 reports `PostToolUse(Succeeded: false)`, call 3 succeeds, **Then** remaining quota is `1 -> 0 -> 1 -> 0` and call 4 is rejected in `PreToolUse`.
2. **Given** a `PreToolUse` call denied by Gates 1–3 or unreserved, **When** `PostToolUse(Succeeded: false)` is delivered, **Then** `QuotaTracker` does not over-rollback above the configured initial limit.
3. **Given** a host tool (`go_tool`, `external_tool`, `sofa_read_mcp_resource`), **When** execution returns `HandlerError != ""`, **Then** reserved quota is rolled back; when it returns `Accepted == false` with `HandlerError == ""`, quota remains consumed.

### User Story 4 - Unified Retry Executor, Stall Watchdog & Phase Runners (`internal/session/watchdog.go`, `runner.go`) (Priority: P1)

`RetryExecutor` and `StallWatchdog` wrap `Opener.Open` and `Session.RunTurn` with `provider.RetryPolicy` exponential backoff + jitter (using injectable `Sleep`/`Jitter`), automatic `ErrorCategorySessionNotFound` fallback to a fresh session with historical diary replay, immediate failure on permanent error categories, and dual-barrier stall recovery (`Abort` -> `RunTurn` exit + `WaitForZeroActiveTools` -> `Resume` -> continuation prompt), wired together by `RunWorkPhase` and `RunQCPhase`.

**Independent Test**: Tier 1 tests with injectable `Sleep` and `50ms` `stallTimeout` verify `RATE_LIMIT` retries and succeeds, `AUTHENTICATION` fails immediately with 0 retries, `SESSION_NOT_FOUND` clears `ResumeSessionID` and injects diary history into a fresh session, and a stalled `RunTurn` triggers `Abort`, waits for in-flight tool completion (`activeToolCalls == 0`), calls `Resume`, and completes via continuation prompt.

**Acceptance Scenarios**:

1. **Given** `ErrorCategoryRateLimit` or `ErrorCategoryTransient` or a message matching `ErrorMessageRegex`, **When** `RunTurn` fails, **Then** `RetryExecutor` retries with capped exponential backoff (`Interval * 2^attempt` capped at `MaxInterval` + jitter) up to `ModelCallRetries`.
2. **Given** `ErrorCategoryAuthentication`, `ErrorCategoryAuthorization`, `ErrorCategoryQuota`, `ErrorCategoryContextLimit`, or `ErrorCategoryUnsupportedCapability`, **When** `Open` or `RunTurn` fails, **Then** `RetryExecutor` fails immediately with 0 retries.
3. **Given** `ResumeSessionID != ""` and `Opener.Open` returning `ErrorCategorySessionNotFound`, **When** `OpenWithRetry` runs, **Then** `ResumeSessionID` is cleared, historical diary context is injected into `SystemPrompt`, and a fresh session is opened.
4. **Given** `stallTimeout > 0` and no `SessionEvent` or active tool calls for longer than `stallTimeout`, **When** `RunTurn` is blocked, **Then** `StallWatchdog` calls `session.Abort`, waits for `RunTurn` return AND `WaitForZeroActiveTools`, calls `session.Resume`, and re-issues `RunTurn` with a continuation prompt within `LifecycleRetries`.

### Edge Cases

- `PreToolUseInput` and `PostToolUseInput` correlate via `ToolCallID` (falling back to `HookCallID` when `ToolCallID` is empty) so mismatched hook-level IDs still track reservations and `activeToolCalls` accurately.
- Single-turn runaway guard triggers `checkpointPending` mid-turn when consecutive business tool calls exceed `checkpointEveryRounds * 4`.
- Empty/nil `ControlHooks` defaults to no-op implementations so standalone unit tests and QC sessions without workspace handoff validation operate cleanly.
- `activeToolCalls` is decremented via `defer` on every `InvokeHostTool` and on `PostToolUse` for allowed non-host tools so panics or early errors never deadlock `WaitForZeroActiveTools`.

## Requirements

- **FR-001**: `internal/session/diary.go` MUST define `DiaryInput`, `HumanInterjection`, `DecisionRequest`, `EscalationRequest`, and `ControlHooks` with strict validation and JSON tags matching `016` §3.1.
- **FR-002**: `internal/session/qc_ledger.go` MUST implement thread-safe `QCLedger` with `OpenIssues`, `ResolveIssues`, `SubmitVerdict`, `OpenIssuesSnapshot`, `AllIssues`, `Snapshot`, and `Restore`, enforcing `criteriaKeys`, host-generated `QC-001` IDs, and pass/revise ledger invariants.
- **FR-003**: `internal/session/quota.go` MUST implement thread-safe `QuotaTracker` with `Reserve`, `Rollback`, and `Remaining`, clamped against initial limits and supporting `0`-limit rejection.
- **FR-004**: `internal/session/gates.go` MUST implement `GateController` enforcing Gates 1–4, `activeToolCalls` tracking + `WaitForZeroActiveTools`, round/turn accounting, and `mustPauseForHuman` tool locking.
- **FR-005**: `internal/session/protocol_tools.go` MUST define strict `additionalProperties: false` JSON Schemas and host handlers for all Work (`sofa_checkpoint`, `sofa_complete_work`, `sofa_request_decision`, `sofa_pause_for_human`, `sofa_escalate`) and QC (`sofa_qc_open_issues`, `sofa_qc_resolve_issues`, `sofa_qc_submit_verdict`) protocol tools.
- **FR-006**: `internal/session/watchdog.go` MUST implement `RetryExecutor` and `StallWatchdog` with injectable `Sleep`/`Jitter`, permanent vs retryable classification, `SESSION_NOT_FOUND` fresh-session fallback with diary replay, and dual-barrier stall recovery.
- **FR-007**: `internal/session/runner.go` MUST implement `RunWorkPhase` and `RunQCPhase` wiring `Opener`, `GateController`, `QuotaTracker`, `ProtocolTools`, `toolruntime.Executor`, `toolruntime.MCPResourceTool`, `RetryExecutor`, and `StallWatchdog`.

## Success Criteria

- **SC-001**: `gofmt -l .` produces empty output.
- **SC-002**: `go vet ./...` passes with zero diagnostics.
- **SC-003**: `go test ./... -count=1` passes offline with all Tier 1 tests in `internal/session/session_test.go` green in sub-second time.
