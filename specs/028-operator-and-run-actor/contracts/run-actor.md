# Contract: `OperatorActor` & `RunActor` (`028-operator-and-run-actor`)

## 1. `OperatorActor` (`ActorType = "OperatorActor"`, ID = `"global"`)

| Method | Request | Response | Semantics |
| :--- | :--- | :--- | :--- |
| `UpsertRun` | `*UpsertRunReq` | `*UpsertRunResp` | Persists or updates `RunSummary` in `"operator_actor_v1"` |
| `GetRunSummary` | `*GetRunSummaryReq` | `*GetRunSummaryResp` | Returns `Found` + `Summary` (or `ErrorCode: "NOT_FOUND"`) |
| `ListRuns` | `*ListRunsReq` | `*ListRunsResp` | Lists `RunSummary` records filtered by `Namespace`/`Status` |
| `RemoveRun` | `*RemoveRunReq` | `*RemoveRunResp` | Deletes `run_id` from index on `DeleteRun` |
| `RecoverActiveRuns` | `*RecoverActiveRunsReq` | `*RecoverActiveRunsResp` | Wakes all `RUNNING`/`PAUSING` `RunActor`s via `Reconcile` |

## 2. `RunActor` (`ActorType = "RunActor"`, ID = `<run_id>`)

| Method | Request | Response | Semantics |
| :--- | :--- | :--- | :--- |
| `StartRun` | `*StartRunReq` | `*StartRunResp` | Loads `.sofaplan` from `BlobStore`, pins `source` SHAs, initializes `git.Ledger`, upserts `OperatorActor`, registers reconcile reminder, dispatches initial ready nodes |
| `Reconcile` | `*ReconcileReq` | `*ReconcileResp` | Non-blocking single-step DAG progression (modules, static workers, one-time `dynamic_worker` expansion, streaming `merge`, `preserve`, `EvalPlanOutputs`) |
| `RegisterManagedBranch` | `*RegisterManagedBranchReq` | `*RegisterManagedBranchResp` | Enforces `git.Ledger` protected-source & ownership rules, persists snapshot |
| `OnNodeEvent` | `*node.NodeEventNotification` | `*OnNodeEventResp` | Processes `SUCCEEDED`, `WAITING_HUMAN`, `FALLBACK_TRIGGERED`, `FAILED`, `PAUSED`, `QUEUED_SLOT`, `RUNNING`; triggers `fallback` cascade or DAG advancement |
| `OnMergeCandidateResult` | `*merge.MergeCandidateNotification` | `*OnMergeCandidateResultResp` | Advances `merge` progress, enforces `MergedIntoSharedBranch`, triggers `MergeReworkSource` single-source rework or `FAILED` on `max_reworks` exhaustion |
| `PauseRun` | `*PauseRunReq` | `*PauseRunResp` | Supports `mode = "planned" | "immediate"` |
| `ResumeRun` | `*ResumeRunReq` | `*ResumeRunResp` | Clears manual pause, resets paused/failed nodes, re-enqueues unmerged candidates, reconciles |
| `RerunNode` | `*RerunNodeReq` | `*RerunNodeResp` | Rejects with `AlreadyMergedSharedBranch: true` / `ErrCodeAlreadyMergedSharedBranch` if target or transitive downstream merged into shared `target_branch`; otherwise cascades `epoch++` reset & dispatches |
| `DeleteRun` | `*DeleteRunReq` | `*DeleteRunResp` | Fences active nodes/merges, cleans `git.Ledger.PlanCleanup` intermediate branches, deletes `runs/<run_id>/` blobs, deletes Run Config scope, removes `OperatorActor` entry |
| `GetRunState` | `*GetRunStateReq` | `*GetRunStateResp` | Returns authoritative Run summary, `[]NodeRunSummary`, `Outputs` (with `Sensitive` metadata), `Sources`, `Preserves`, `Ledger` |
| `ListDecisions` | `*ListDecisionsReq` | `*ListDecisionsResp` | Aggregates `[]node.DecisionCard` from Run nodes |

## 3. Lossless Domain Error Translation

- `ErrAlreadyMergedSharedBranch` (`ErrorCode = "ALREADY_MERGED_SHARED_BRANCH"`, `AlreadyMergedSharedBranch = true`) -> HTTP `409 Conflict`
- `ErrRunNotFound` / `ErrNodeNotFound` (`ErrorCode = "NOT_FOUND"`) -> HTTP `404 Not Found`
- `ErrRunConflict` (`ErrorCode = "CONFLICT"`) -> HTTP `409 Conflict`
- Both `RunClient` (direct & Dapr-backed) and exported `Decode*Error` helpers guarantee `errors.Is(err, ErrAlreadyMergedSharedBranch)` across in-process and remote Dapr RPC calls.
