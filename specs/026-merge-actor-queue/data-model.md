# Data Model: `MergeActor` (`026-merge-actor-queue`)

## 1. Actor Addressing

- **ActorType**: `"MergeActor"`
- **Effective Target Branch**:
  - Explicit `target_branch != nil` (non-empty after trim): `git.NormalizeBranchName(*explicitTarget)`, `SharedTargetBranch = true`.
  - Unset (`nil` or whitespace): `git.WorkBranchName(runID, mergeNodeCanonicalAddr)`, `SharedTargetBranch = false`.
- **ActorID**: `MergeActorID(repoURL, effectiveTargetBranch)` = lowercase 64-char hex `sha256(git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch)`.

## 2. Ticket Lifecycle (`TicketStatus`)

- `PENDING`: Queued in FIFO `Queue`, awaiting single in-flight slot and worker capacity.
- `IN_FLIGHT`: Currently leased to a worker (`LeaseID`, `LeaseKey`, `WorkerProcessID`, `WorkerAppID`) and executing `run_merge_candidate`.
- `COMPLETED`: Merged cleanly (`MergedCommitSHA` / `LatestTargetSHA` updated; `MergedIntoSharedBranch = SharedTargetBranch`).
- `REWORK_REQUIRED`: Rebase conflict or `merge.gatekeeper` failed; target branch unchanged, `LatestTargetSHA` and failure details reported to `RunActor` for `SourceEpoch + 1` repair.
- `FAILED`: Non-conflict/non-gatekeeper infrastructure or credential resolution error; reported to `RunActor` and dequeued so subsequent items proceed.
- `CANCELLED`: Cancelled via `CancelByRun` or `Aborted: true` report; does not block future re-enqueueing upon Run resume.

## 3. Core Persisted State (`persistedMergeState` under key `"merge_actor_v1"`)

- `RepoURL string`
- `TargetBranch string`
- `SharedTargetBranch bool`
- `LatestTargetSHA string`
- `InFlight *CandidateTicket` (at most 1 at any time)
- `Queue []CandidateTicket` (FIFO of `PENDING` tickets)
- `History []CandidateTicket` (terminal tickets for auditability and `(RunID, MergeNodeAddr, MergeEpoch, SourceNodeAddr, SourceEpoch)` deduplication across `COMPLETED` and `REWORK_REQUIRED`)
- `RegisteredMergeBranches map[string]bool` (tracks `(RunID, MergeNodeAddr, MergeEpoch)` tuples already registered as `git.KindMergeBranch` via `RunMergeNotifier.RegisterManagedBranch`)

## 4. `CandidateTicket`

- Identity & Idempotency Key: `RequestID` (UUID), `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)`, `MergeEpoch`
- Target & Source: `RepoURL`, `BasedSource`, `BaseSHA`, `TargetBranch`, `ExplicitTarget *string`, `SharedTargetBranch`, `CandidateBranch`
- Credentials & Execution Spec: `Namespace`, `RunOverrides`, `RequiredEnvKeys`, `TokenRef`, `SSHKeyRef`, `Auth`, `RequiredArch`, `Sandbox`, `Gatekeepers`, `BundleDir`, `ImportsDir`, `HostOS`, `TempRoot`
- Runtime & Result: `Status`, `Attempt`, `LeaseID`, `LeaseKey`, `WorkerProcessID`, `WorkerAppID`, `MergedCommitSHA`, `LatestTargetSHA`, `ErrorMessage`, `EnqueuedAt`, `UpdatedAt`

## 5. `MergeCandidateNotification`

Sent to `RunMergeNotifier.OnMergeCandidateResult`:
- `RequestID`, `RunID`, `MergeNodeAddr`, `MergeEpoch`, `SourceNodeAddr`, `SourceEpoch`
- `CandidateBranch`, `TargetBranch`, `SharedTargetBranch`, `MergedIntoSharedBranch`
- `Result runner.MergeCandidateResult`
- `ErrorMessage`, `OccurredAt`
