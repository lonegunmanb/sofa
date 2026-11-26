# Research: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog (`016-session-gates-and-watchdog`)

## Decision 1: Hook & Tool Call Correlation (`ToolCallID` vs `HookCallID`)

- **Decision**: Correlate `PreToolUse` and `PostToolUse` calls primarily by `ToolCallID`, falling back to `HookCallID` when `ToolCallID == ""` (and tracking an anonymous per-tool counter only if both IDs are empty). Track both `activeNonHostCalls` and `quotaReservedCalls` inside `GateController`.
- **Rationale**: In `proto/sofa/harness/v1/harness.proto` and `internal/harness/runtime/client.go`, `PreToolUseRequest` and `PostToolUseRequest` carry distinct RPC `hook_call_id` values while sharing `tool_call_id`. Keying by `ToolCallID` first ensures real gRPC plugins correlate cleanly while allowing minimal unit tests that omit `ToolCallID` to correlate by `HookCallID`.

## Decision 2: `DiaryInput` Strict Validation

- **Decision**: Require `"completed"` and `"next_steps"` as JSON array fields (`additionalProperties: false`), reject whitespace-only strings in any slice, and require at least one non-empty item in `Completed` or `NextSteps`.
- **Rationale**: Prevents empty or malformed diary submissions while allowing valid checkpoints both mid-task (non-empty `NextSteps`) and immediately prior to completion (non-empty `Completed`).

## Decision 3: Dual-Barrier `StallWatchdog` Synchronization

- **Decision**: Track `lastActivity` (nanosecond timestamp) updated on every `SessionEvent` and on every tool lifecycle entry/exit, and consider active tool executions (`activeToolCalls > 0`) as live work that prevents stall aborts. When `activeToolCalls == 0` and idle time exceeds `stallTimeout`, call `session.Abort(ctx)`, wait for both the active `RunTurn` goroutine to return AND `gates.WaitForZeroActiveTools(ctx)`, call `session.Resume(ctx)`, and re-issue `RunTurn` with a continuation prompt (bounded by `LifecycleRetries`).
- **Rationale**: Mirrors `r42` dual-barrier recovery so long-running `go_tool`/`external_tool` or `bash` executions bounded by command timeouts are not spuriously aborted, while hung model streams recover cleanly without overlapping turns or orphaned tool callbacks.

## Decision 4: Classification of Host Tool Rejections vs Execution Failures

- **Decision**: Protocol/business rejections (`sofa_complete_work` / `terminate` unauthorized paths, `QCLedger` verdict rejections, `activeToolCalls > 1` handoff rejection, invalid tool JSON arguments) return `HostToolResultSuccess` with `HandlerError == ""` and JSON `{"accepted":false,"issues":[...]}` so quota is consumed and the LLM can self-repair. Infrastructure/execution failures (`HandlerError != ""`) trigger `QuotaTracker.Rollback` and preserve gate locks (e.g., `OnCheckpointRequested` error keeps `checkpointPending = true`). Gate 1–3 denials occur before `QuotaTracker.Reserve`, so denied calls consume zero quota.
- **Rationale**: Aligns `015` (`executor.go`), `016`, and `017` (`SaveCheckpoint`) cleanly without quota leaks or false rollbacks.
