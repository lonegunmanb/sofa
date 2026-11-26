# Implementation Plan: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog (`016-session-gates-and-watchdog`)

**Branch**: `016-session-gates-and-watchdog` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `016` under `internal/session/`:
1. `diary.go`: `DiaryInput`, `HumanInterjection`, `DecisionRequest`, `EscalationRequest`, `ControlHooks`, and diary summary formatting helpers.
2. `qc_ledger.go`: thread-safe cross-round `QCLedger` with deterministic `QC-001` IDs, atomic batch `OpenIssues`/`ResolveIssues`, strict `SubmitVerdict` invariants, and `Snapshot`/`Restore`.
3. `quota.go`: thread-safe `QuotaTracker` with `Reserve`, clamped `Rollback`, and `Remaining`.
4. `gates.go`: `GateController` implementing Gates 1–4, `activeToolCalls` + `WaitForZeroActiveTools` barrier, `mustPauseForHuman` read-only tool locking, `checkpointPending` lock, and `roundComplete` barrier.
5. `protocol_tools.go`: strict JSON Schema definitions and host `ToolHandler`s for Work (`sofa_checkpoint`, `sofa_complete_work`, `sofa_request_decision`, `sofa_pause_for_human`, `sofa_escalate`) and QC (`sofa_qc_open_issues`, `sofa_qc_resolve_issues`, `sofa_qc_submit_verdict`) tools.
6. `watchdog.go`: `RetryExecutor` (with injectable `Sleep`/`Jitter`, `SESSION_NOT_FOUND` diary-replay fallback, permanent vs retryable error handling) and `StallWatchdog` (dual-barrier `Abort` -> `WaitForZeroActiveTools` -> `Resume`).
7. `runner.go`: `RunWorkPhase` and `RunQCPhase` multi-turn phase drivers integrating `harnessruntime.Opener`, `toolruntime.Executor`, `toolruntime.MCPResourceTool`, gates, quota, retry, and watchdog.
8. `session_test.go`: fast, deterministic Tier 1 unit tests covering all gate, quota, ledger, retry, watchdog, and phase-runner scenarios.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **New Direct Dependencies**: None (uses Go stdlib + existing repo packages).
- **Existing Packages Reused**:
  - `internal/harness/runtime` (`Opener`, `Session`, `SessionConfig`, `HostToolSpec`, `ToolHandler`, `ToolInvocation`, `ToolOutput`, `HostToolResultSuccess`, `HostToolResultFailure`, `PreToolUseInput`, `PreToolUseOutput`, `PostToolUseInput`, `Event`, `TurnSummary`, `HarnessError`, `ErrorCategory`)
  - `internal/tool/runtime` (`Executor`, `ToolBinding`, `InvocationResult`, `HostToolSpec`, `MCPResourceSpec`, `NewMCPResourceReader`)
  - `internal/provider` (`RetryPolicy`, `DefaultRetryPolicy`)
  - `internal/worker/spec` (`DefaultMaxProtocolAttempts`, `DefaultCheckpointRounds`, `FallbackConfig`, `QCConfig`)
- **Testing**: Tier 1 offline unit tests (`go test ./internal/session/... -count=1` and `go test ./... -count=1`).

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored in `specs/016-session-gates-and-watchdog/` prior to production Go implementation.
- **Principle II (Strict TDD)**: Failing Tier 1 tests in `internal/session/session_test.go` written and verified Red before implementing production files in `internal/session/`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: Pure deterministic domain package with zero `os/exec`, Git CLI, container, or Dapr imports; external interactions abstracted behind `harnessruntime.Opener`, `toolruntime.ContainerExecFunc`, and `ControlHooks`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Host deterministically enforces interjection locking, periodic checkpoints, `writable_paths` handoff checks, quota reservation/rollback, and cross-round QC issue clearance rather than relying on LLM prose.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `harness/runtime/{types,client}.go`, `tool/runtime/{executor,mcp_resource}.go`, `provider/retry.go`, `worker/spec/{qc,fallback,schema}.go`, and `docs/stage2/016..018` before planning; scoped strictly to Slice `016`.

## Files

```text
internal/session/diary.go
internal/session/qc_ledger.go
internal/session/quota.go
internal/session/gates.go
internal/session/protocol_tools.go
internal/session/watchdog.go
internal/session/runner.go
internal/session/session_test.go
specs/016-session-gates-and-watchdog/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
