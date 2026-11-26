# Specification Quality Checklist: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog

- [x] Structured `DiaryInput`, `HumanInterjection`, `DecisionRequest`, `EscalationRequest`, and `ControlHooks` contracts are specified
- [x] Host-owned cross-round `QCLedger` (`QC-001` IDs, `OpenIssues`, `ResolveIssues`, `SubmitVerdict`, `Snapshot`/`Restore`) is specified
- [x] Thread-safe `QuotaTracker` (`0`-limit rejection, clamped rollback, `PostToolUse` failure rollback, `HandlerError` vs `Accepted: false`) is specified
- [x] Four host gates (interjections & `WaitForConfirmation` locking, `checkpointPending`, `roundComplete` + `activeToolCalls` + `ValidateWorkspaceForHandoff`, `QuotaTracker`) are specified
- [x] All Work and QC `sofa_*` protocol tool schemas (`additionalProperties: false`) and conditional mounting rules are specified
- [x] `RetryExecutor` (`SESSION_NOT_FOUND` diary replay fallback, permanent vs retryable categories, injectable `Sleep`/`Jitter`) and dual-barrier `StallWatchdog` are specified
- [x] `RunWorkPhase` and `RunQCPhase` phase drivers and Tier 1 TDD scenarios are specified
