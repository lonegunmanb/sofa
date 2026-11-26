---
description: "Stage 2 Slice 016 TDD tasks: diary/hooks, cross-round QC ledger, four host gates, quota rollback, retry/watchdog, and Work/QC phase runners"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 tests in `internal/session/session_test.go` covering:
  - Normal human interjection (`AdditionalContext` injection without locking tools) vs `WaitForConfirmation: true` (locking `bash`/`edit`, allowing read-only inspection + `sofa_checkpoint` + `sofa_pause_for_human`, and exiting `OutcomeWaitingHuman`); plus pre-turn `WaitForConfirmation: true` holding scheduled prompt and pausing;
  - `checkpoint_every_rounds` locking tools after N business-tool turns (and mid-turn runaway threshold `N * 4`), unlocking after `sofa_checkpoint` + `OnCheckpointRequested` `nil`, remaining locked when `OnCheckpointRequested` returns an error;
  - Handoff workspace validation: `ValidateWorkspaceForHandoff` returning unauthorized paths rejects `sofa_complete_work` and custom `terminate` tool with model-visible issues; fixing paths in the next turn succeeds; trailing tool call after `roundComplete` is rejected; `activeToolCalls > 1` concurrent handoff is rejected; exhausting `max_protocol_attempts` returns `ErrProtocolAttemptsExhausted`; `sofa_escalate` conditional mounting and fallback validation;
  - `tool_call_quota = {"bash": 2}`: 1st succeeds, 2nd `PostToolUse(Succeeded: false)` rolls back, 3rd succeeds, 4th rejected in `PreToolUse`; `limit == 0` rejected immediately; host tool `HandlerError != ""` rolls back quota while `Accepted: false` (`HandlerError == ""`) consumes quota;
  - `QCLedger` cross-round anti-amnesia: Round 1 opens `QC-001` & `QC-002` and submits `revise`; Round 2 resolves only `QC-001` and tries `pass` -> hard rejected listing `QC-002`; resolves `QC-002` then `pass` -> succeeds; empty ledger `revise` -> hard rejected; unknown `criterion` or explicit `id` rejected; `Snapshot`/`Restore` preserves `QC-003` next ID and open issues;
  - `RetryExecutor` & `StallWatchdog`: `RATE_LIMIT` retries with backoff and succeeds, `AUTHENTICATION` fails immediately with 0 retries, `SESSION_NOT_FOUND` falls back to fresh session with diary replay, and `RunTurn` stalling past `50ms` `stallTimeout` triggers `Abort` -> waits for `activeToolCalls == 0` -> `Resume` -> continuation prompt -> completes.
- [X] T002 [Green] Implement `internal/session/diary.go`, `internal/session/qc_ledger.go`, `internal/session/quota.go`, `internal/session/gates.go`, `internal/session/protocol_tools.go`, `internal/session/watchdog.go`, and `internal/session/runner.go` so `go test ./internal/session/... -count=1` passes.
- [X] T003 [Verify] Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, resolve any issues, and mark all tasks `[X]`.
