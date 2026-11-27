# Requirements Quality Checklist: `026-merge-actor-queue`

- [x] Actor Type `"MergeActor"`, `EffectiveTargetBranch`, and 64-char lowercase hex `MergeActorID` specified.
- [x] Idempotent `EnqueueCandidate` on `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` and `SourceEpoch + 1` rework re-enqueue specified.
- [x] Strict single `IN_FLIGHT` execution, non-blocking dispatch, `SlotRetryReminder` on saturation/failure, and explicit `Set` + `Save` persistence in `ReminderCall` specified.
- [x] `KindMergeBranch` pre-registration for `!SharedTargetBranch` and `MergedIntoSharedBranch` flag for `SharedTargetBranch` specified.
- [x] `MergeStatusReworkSource` reporting `LatestTargetSHA` without blocking subsequent FIFO queue items specified.
- [x] `OnWorkerDead` head-of-queue reset + re-dispatch and `CancelByRun` (`RunID`, optional `MergeNodeAddr` / `SourceNodeAddr`) specified.
- [x] `daemon.RunMergeCandidateRequest.ResolvedEnv` isolation in `runMergeTask` specified.
- [x] Strict TDD task ordering (Red -> Green -> Refactor) documented in `tasks.md`.
