# Contract: Session Protocol Tools, QC Ledger, Host Gates, Quota Rollback, Retry & Watchdog (`016`)

## 1. `internal/session/diary.go`

- `DiaryInput` (`Completed`, `Hypotheses`, `Decisions`, `TestEvidence`, `OpenQuestions`, `NextSteps`) with `Validate() error`.
- `HumanInterjection` (`ID`, `Message`, `WaitForConfirmation`).
- `DecisionRequest` (`Question`, `Context`, `Options`, `Recommended`) with `Validate() error`.
- `EscalationRequest` (`FallbackName`, `Summary`) with `Validate() error`.
- `ControlHooks` interface (`DrainInterjections`, `OnCheckpointRequested`, `ValidateWorkspaceForHandoff`).
- `FormatDiarySummary(diaries []DiaryInput) string`.

## 2. `internal/session/qc_ledger.go`

- `NewQCLedger(criteria map[string]string) (*QCLedger, error)`.
- `OpenIssues(inputs []QCIssueInput) ([]QCIssue, error)`: atomic validation, assigns `"QC-001"`, `"QC-002"`, ...
- `ResolveIssues(ids []string, resolutionEvidence string) ([]QCIssue, error)`: atomic validation rejecting empty, duplicate, unknown, or already-resolved IDs.
- `SubmitVerdict(decision, summary string) (QCVerdict, error)`: `"pass"` rejected if open issues > 0; `"revise"` rejected if open issues == 0.
- `Snapshot() ([]byte, error)` & `Restore(data []byte) error`.

## 3. `internal/session/quota.go`

- `NewQuotaTracker(limits map[string]int) (*QuotaTracker, error)`.
- `Reserve(toolName string) error`, `Rollback(toolName string)`, `Remaining(toolName string) (int, bool)`.

## 4. `internal/session/gates.go` & `protocol_tools.go`

- `GateController`:
  - `PreToolUse(ctx, input) (runtime.PreToolUseOutput, error)`
  - `PostToolUse(ctx, input) error`
  - `WrapHostTool(toolName string, spec HostToolBehavior, handler runtime.ToolHandler) runtime.ToolHandler`
  - `DrainTurnInterjections(ctx) ([]HumanInterjection, error)`
  - `BeginTurn()`, `EndTurn() TurnAccounting`
  - `ActiveToolCalls() int`, `WaitForZeroActiveTools(ctx) error`, `LastActivity() time.Time`
- Work protocol tools: `sofa_checkpoint`, `sofa_complete_work` (when no custom terminate tool), `sofa_request_decision`, `sofa_pause_for_human`, `sofa_escalate` (when `agent_escalated` fallback declared).
- QC protocol tools: `sofa_qc_open_issues`, `sofa_qc_resolve_issues`, `sofa_qc_submit_verdict`.

## 5. `internal/session/watchdog.go` & `runner.go`

- `RetryExecutor.OpenWithRetry(ctx, opener, cfg, diarySummary) (runtime.Session, runtime.SessionConfig, error)`
- `RetryExecutor.RunTurnWithRetryAndWatchdog(ctx, sess, prompt, opts) (runtime.TurnSummary, error)`
- `RunWorkPhase(ctx, cfg WorkSessionConfig) (WorkResult, error)`
- `RunQCPhase(ctx, cfg QCSessionConfig) (QCResult, error)`
