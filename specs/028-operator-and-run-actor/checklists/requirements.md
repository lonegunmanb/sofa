# Requirements Quality Checklist: `028-operator-and-run-actor`

- [x] `OperatorActor("global")` (`UpsertRun`, `GetRunSummary`, `ListRuns`, `RemoveRun`, `RecoverActiveRuns`) specified.
- [x] `RunActor(<run_id>)` cold `.sofaplan` re-hydration, `source` SHA pinning, and `git.LedgerSnapshot` registration (`RegisterManagedBranch`) specified.
- [x] Non-blocking DAG traversal for root + nested `module` plans (`EvalModuleInputsAndValidate`, `module.parallelism`, `dynamic_worker.parallelism`/`serial`), one-time persisted `dynamic_worker` keys, streaming `dynamic_worker -> merge` (`EvalMergeBase`), and `EvalPlanOutputs` (`Sensitive` metadata) specified.
- [x] Three rework/rerun `epoch` cascade protocols (`FallbackTriggered` with `max_reworks`, `MergeReworkSource` single-source rework with `BaseSHA = LatestTargetSHA`, and `RerunNode` `ErrAlreadyMergedSharedBranch` rejection) specified.
- [x] `PauseRun("planned" | "immediate")`, `ResumeRun`, deferred cleanup (`SUCCEEDED`/`FAILED`/`PAUSED` delete zero branches/blobs), and `DeleteRun` complete cleanup specified.
- [x] Lossless Dapr RPC error translation (`AlreadyMergedSharedBranch` / `ErrorCode` / `ErrorMessage`) and `dapr/go-sdk@v1.16.0` persistence rules specified.
- [x] Strict TDD task ordering (Red -> Green -> Refactor) documented in `tasks.md`.
