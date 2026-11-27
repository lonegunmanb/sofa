# Contract: `MergeActor` RPCs & Worker Credential Alignment (`026-merge-actor-queue`)

## 1. Actor Identity & Addressing

- `ActorType`: `"MergeActor"`
- `EffectiveTargetBranch(runID, mergeNodeCanonicalAddr string, explicitTarget *string) (branch string, shared bool, err error)`
- `MergeActorID(repoURL, effectiveTargetBranch string) (string, error)` — lowercase 64-char hex `sha256(git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch)`

## 2. `MergeActor` RPCs (`func(context.Context, *Req) (*Resp, error)`)

| Method | Request | Response | Key Semantics |
| :--- | :--- | :--- | :--- |
| `EnqueueCandidate` | `*EnqueueCandidateReq` | `*EnqueueCandidateResp` | Deduplicates on `(RunID, MergeNodeAddr, MergeEpoch, SourceNodeAddr, SourceEpoch)`; appends to persistent FIFO `Queue`; triggers non-blocking `tryDispatchNext` and returns immediately. |
| `ReportCandidateResult` | `*daemon.MergeOutcomeReport` | `*ReportCandidateResultResp` | Validates active `InFlight` `RequestID` (and `LeaseID`); ignores stale/aborted reports (`IgnoredStale: true`); updates `LatestTargetSHA` on `MergeStatusSucceeded`; preserves target on `MergeStatusReworkSource`; releases slot, notifies `RunMergeNotifier.OnMergeCandidateResult`, and advances queue. |
| `CancelByRun` | `*CancelByRunReq` | `*CancelByRunResp` | Filters by `RunID` (and optional `MergeNodeAddr`, `SourceNodeAddr`); removes matching `PENDING` tickets; aborts + releases any matching `IN_FLIGHT` ticket; immediately dispatches remaining queued items. |
| `OnWorkerDead` | `*registry.WorkerDeadNotification` | `*OnWorkerDeadResp` | If active `InFlight` matches dead `(LeaseID, WorkerProcessID)`, resets ticket to head of `Queue` as `PENDING` (clearing old lease), clears `InFlight`, persists state, and re-dispatches onto a healthy worker. |
| `GetState` | `*GetMergeStateReq` | `*GetMergeStateResp` | Returns defensive snapshot of `RepoURL`, `TargetBranch`, `SharedTargetBranch`, `LatestTargetSHA`, `InFlight`, `Queue`, and `History`. |

## 3. External Interfaces Consumed by `MergeActor`

- `ConfigResolver`: `ResolveForNode(ctx, *config.ResolveForNodeReq) (*config.ResolveForNodeResp, error)`
- `SlotManager`: `ClaimSlot`, `ReleaseSlot`, `ReportSuspect`
- `MergeWorkerDispatcher`:
  - `RunMergeCandidate(ctx, appID, daemon.RunMergeCandidateRequest) (daemon.RunMergeCandidateResponse, error)`
  - `AbortMergeCandidate(ctx, appID, daemon.AbortMergeRequest) (daemon.AbortMergeResponse, error)`
- `RunMergeNotifier`:
  - `RegisterManagedBranch(ctx, RegisterManagedBranchReq) error`
  - `OnMergeCandidateResult(ctx, MergeCandidateNotification) error`
- `ReminderScheduler`:
  - `RegisterSlotRetryReminder(ctx, actorType, actorID, reminderName, dueTime, period) error`
  - `UnregisterSlotRetryReminder(ctx, actorType, actorID, reminderName) error`

## 4. `WorkerDaemon` Alignment (`internal/worker/daemon`)

- `RunMergeCandidateRequest` adds `ResolvedEnv map[string]string` (`json:"resolved_env,omitempty"`).
- `runMergeTask` constructs `git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: makeIsolatedLookupEnv(req.ResolvedEnv)})` whenever `Options.GitAdapter == nil`, preventing any fallback to worker host `os.LookupEnv`.
