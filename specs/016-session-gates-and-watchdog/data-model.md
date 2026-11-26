# Data Model: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog (`016-session-gates-and-watchdog`)

## `diary.go`

| Entity.field | Rules |
|---|---|
| `DiaryInput` | `Completed`, `Hypotheses`, `Decisions`, `TestEvidence`, `OpenQuestions`, `NextSteps` (`[]string`); no blank items; `len(Completed) > 0 \|\| len(NextSteps) > 0` |
| `HumanInterjection` | `ID`, `Message` (`string`), `WaitForConfirmation` (`bool`) |
| `DecisionRequest` | `Question` (required non-blank), `Context`, `Options`, `Recommended` |
| `EscalationRequest` | `FallbackName` (JSON `"fallback"`), `Summary` (required non-blank) |
| `ControlHooks` | `DrainInterjections`, `OnCheckpointRequested`, `ValidateWorkspaceForHandoff` |

## `qc_ledger.go`

| Entity.field | Rules |
|---|---|
| `QCIssueInput` | `Criterion` (must exist in `criteriaKeys`), `Code` (non-blank), `Message` (non-blank), `Path`, `Evidence`, `RepairHint`; no `id` allowed |
| `QCIssue` | Host-assigned `ID` (`"QC-001"`, `"QC-002"`, ...), `Resolved` (`bool`), `ResolutionEvidence` (`string`) |
| `QCVerdict` | `Decision` (`"pass"` or `"revise"`), `Summary` (non-blank) |
| `QCLedger` | Thread-safe; `"pass"` requires `0` open issues; `"revise"` requires `>= 1` open issues; `Snapshot()` / `Restore()` JSON-serializable |

## `quota.go`

| Entity.field | Rules |
|---|---|
| `QuotaTracker.Reserve(toolName)` | Unlimited if not in map; fails when remaining `== 0` (including initial `0`); otherwise decrements |
| `QuotaTracker.Rollback(toolName)` | Increments remaining up to configured initial limit (never overflows) |
| `QuotaTracker.Remaining(toolName)` | Returns `(remaining, true)` if configured, `(0, false)` if unlimited |

## `gates.go` & `protocol_tools.go`

| Entity.field | Rules |
|---|---|
| `GateController` | Enforces Gate 1 (interjections & `mustPauseForHuman`), Gate 2 (`checkpointPending`), Gate 3 (`roundComplete` & `activeToolCalls` barrier), Gate 4 (`QuotaTracker`) |
| `OutcomeStatus` | `OutcomeSucceeded`, `OutcomeWaitingHuman`, `OutcomeAgentEscalated`, `OutcomeQCCompleted` |
| `PhaseOutcome` | Captures terminal/pause outcome (`Status`, `Summary`, `ResultJSON`, `DecisionRequest`, `PauseReason`, `Escalation`, `QCVerdict`) |

## `watchdog.go` & `runner.go`

| Entity.field | Rules |
|---|---|
| `RetryExecutor` | Injectable `Sleep`/`Jitter`; retries `Transient`/`RateLimit`/`ErrorMessageRegex` up to `ModelCallRetries` (`RunTurn`) or `LifecycleRetries` (`Open`); `SessionNotFound` falls back to fresh session with diary summary |
| `StallWatchdog` | Dual-barrier recovery (`Abort` -> `RunTurn` exit + `WaitForZeroActiveTools` -> `Resume` -> continuation prompt) bounded by `LifecycleRetries` |
| `RunWorkPhase` / `RunQCPhase` | End-to-end session lifecycle (`OpenWithRetry` -> multi-turn gate/watchdog loop -> `Close`) returning `WorkResult` / `QCResult` |
