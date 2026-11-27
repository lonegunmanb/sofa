# Feature Specification: `MergeActor` Persistent Target-Branch Queue & Source-Responsible Serial Integration (`026-merge-actor-queue`)

**Feature Branch**: `026-merge-actor-queue`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `026` blueprint (`docs/stage3/026-merge-actor-queue.md`), `docs/stage3/028-operator-and-run-actor.md`, `docs/stage3/README.md`, `docs/architecture-contract.md`, `docs/hcl-contract.md`, `internal/worker/runner/{types.go,merge_primitive.go}`, `internal/git/{adapter.go,ledger.go,cli.go}`, `internal/actor/config/`, `internal/actor/registry/`, `internal/actor/node/`, and `internal/worker/daemon/daemon.go`.

## User Scenarios & Testing

### User Story 1 - Deterministic Target-Branch Addressing & Idempotent Persistent FIFO Queue (Priority: P1)

Multiple concurrent `worker` / `dynamic_worker` nodes—or multiple concurrent Runs sharing an explicit `merge.target_branch`—stream finished candidate branches into a single `MergeActor` (`ActorType = "MergeActor"`) scoped to `(repo_url, effective_target_branch)`.

- `EffectiveTargetBranch(runID, mergeNodeCanonicalAddr, explicitTarget)` resolves `explicitTarget` (normalized via `git.NormalizeBranchName`, with `shared = true`) when `explicitTarget != nil` and non-empty, or `git.WorkBranchName(runID, mergeNodeCanonicalAddr)` (`shared = false`) when `explicitTarget == nil`.
- `MergeActorID(repoURL, effectiveTargetBranch)` computes the deterministic 64-character lowercase hex `sha256(git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch)`, guaranteeing 100% URL safety regardless of slashes or special characters in `repo_url` or branch names.
- `EnqueueCandidate(ctx, req)` deduplicates strictly by `(RunID, MergeNodeAddr, MergeEpoch, SourceNodeAddr, SourceEpoch)`:
  - Re-enqueueing an identical tuple while the ticket is `PENDING`, `IN_FLIGHT`, `COMPLETED`, or `REWORK_REQUIRED` idempotently returns the existing ticket (`Deduplicated: true`) without creating duplicate queue entries.
  - Incrementing `SourceEpoch` after source rework (`SourceEpoch + 1`) or incrementing `MergeEpoch` on `merge` node rerun (`MergeEpoch + 1` with unchanged `SourceEpoch`) creates a new ticket that enqueues normally.
- New tickets are persisted immediately to the actor state document (`"merge_actor_v1"` via `Set` + `Save`) and trigger non-blocking dispatch (`tryDispatchNext`), returning immediately to the caller without waiting for Git rebase or container `gatekeeper` execution.

**Independent Test**: Tier 1 unit tests in `internal/actor/merge/merge_test.go` verifying `EffectiveTargetBranch` (`nil` vs explicit shared target), deterministic 64-char lowercase hex `MergeActorID` across URL normalization variants, idempotent deduplication on `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)`, and `SourceEpoch + 1` re-enqueue after rework.

### User Story 2 - Strict Single In-Flight Serial Execution, Source-Responsible Rework & Queue Progression (Priority: P1)

At any point in time, a `MergeActor` allows at most **one** `CandidateTicket` in `IN_FLIGHT` state:
- Before dispatching a Run-exclusive target branch (`!SharedTargetBranch`), `MergeActor` synchronously registers the target branch with `RunMergeNotifier.RegisterManagedBranch(..., Kind: git.KindMergeBranch)` (once per `(RunID, MergeNodeAddr, MergeEpoch)`) so `DeleteRun` can clean it up later, while NEVER registering explicit shared `target_branch` values as Run-owned branches.
- `MergeActor` resolves any required Git/env credentials via `ConfigResolver.ResolveForNode`, materializes them into `daemon.RunMergeCandidateRequest.ResolvedEnv` and `Auth` (with `daemon.runMergeTask` binding `makeIsolatedLookupEnv(req.ResolvedEnv)` onto default `git.CLIAdapter`), claims a worker slot via `SlotManager.ClaimSlot`, commits `IN_FLIGHT` state before invoking `MergeWorkerDispatcher.RunMergeCandidate`, and falls back to `PENDING` + `SlotRetryReminder` if no worker slot is available or if `RunMergeCandidate` fails (`ReleaseSlot` + `ReportSuspect`).
- On `ReportCandidateResult(ctx, report)`:
  - Matching `RequestID` (and active `LeaseID` when set) is validated; stale/cancelled reports are safely ignored (`IgnoredStale: true`).
  - On `MergeStatusSucceeded`: `LatestTargetSHA` advances to `MergedCommitSHA` (merged commits are NEVER rolled back), slot is released, `RunMergeNotifier.OnMergeCandidateResult` is called with `MergedIntoSharedBranch = ticket.SharedTargetBranch`, and the next `PENDING` ticket in the FIFO queue is immediately dispatched.
  - On `MergeStatusReworkSource` (rebase conflict or `merge.gatekeeper` failure): the target branch pointer is preserved unchanged, `LatestTargetSHA` and failure/conflict details are sent via `RunMergeNotifier.OnMergeCandidateResult` (`MergedIntoSharedBranch = false`), the slot is released, and the next `PENDING` ticket in the FIFO queue is **immediately dispatched** so one conflicting branch never stalls subsequent unrelated candidates.
  - On infrastructure error (`ErrMessage != ""` and not `Aborted`): error is persisted and reported via `RunMergeNotifier.OnMergeCandidateResult`, slot is released, and the queue advances to the next candidate.

**Independent Test**: Tier 1 unit tests concurrently enqueueing `W1, W2, W3` and proving only `W1` is `IN_FLIGHT` at a time, `W1` returning `MergeStatusReworkSource` notifies `RunActor` and immediately advances `W2` then `W3` serially, and `SharedTargetBranch` controls `KindMergeBranch` registration and `MergedIntoSharedBranch`; plus Tier 2 integration test `internal/actor/merge/merge_integration_test.go` (`//go:build integration`) running 3 concurrent candidates over real `daprhost.Manager`, `ConfigActor`, `ProcessRegistryActor`, `WorkerDaemon`, bare Git repo, and Podman `alpine:latest` `gatekeeper` containers (2 linear rebase successes + 1 conflict/gatekeeper rejection receiving exact `LatestTargetSHA` of the first 2 merges).

### User Story 3 - Worker Failover (`OnWorkerDead`) & Run Cancellation (`CancelByRun`) (Priority: P1)

- `OnWorkerDead(ctx, notice)`: When the worker executing the current `IN_FLIGHT` ticket is declared `DEAD` by `ProcessRegistryActor`, `MergeActor` resets that ticket back to `PENDING` at the **head** of `Queue` (clearing old `LeaseID`/`LeaseKey`/`WorkerProcessID`/`WorkerAppID`, incrementing `Attempt` on next claim so `ExecuteMergeCandidate`'s `--force-with-lease` remains safe and zombie reports with the old `LeaseID` are rejected), clears `InFlight`, persists state, and immediately re-dispatches on a healthy worker.
- `CancelByRun(ctx, req)`: Filters by `RunID` (plus optional `MergeNodeAddr` and `SourceNodeAddr`) for `Pause("immediate")`, `DeleteRun`, and `fallback`/`rerun` cascades (`028`):
  - Removes all matching `PENDING` tickets from `Queue` while preserving FIFO order of remaining tickets (e.g. from other Runs sharing the target branch);
  - If the current `IN_FLIGHT` ticket matches, clears `InFlight` (recording `CANCELLED` in history), calls `MergeWorkerDispatcher.AbortMergeCandidate` and `SlotManager.ReleaseSlot`, and immediately dispatches the next remaining `PENDING` ticket.

**Independent Test**: Tier 1 unit tests verifying `OnWorkerDead` resets the `IN_FLIGHT` ticket to the queue head and re-claims/re-dispatches onto a healthy worker while rejecting late stale reports, and `CancelByRun` purges matching `PENDING` tickets, aborts matching `IN_FLIGHT` ticket, releases its slot, and immediately dispatches queued tickets belonging to another Run.

### Edge Cases

- Calling `ReportCandidateResult` after `CancelByRun` cleared `InFlight` or after `OnWorkerDead` rotated `LeaseID` returns `IgnoredStale: true` without corrupting the active `IN_FLIGHT` ticket.
- `CancelByRun` occurring concurrently while `tryDispatchNext` is calling `ClaimSlot` re-checks queue head identity under `MergeActor.mu` before committing `InFlight`; if cancelled mid-flight, the newly claimed slot is immediately released and the next non-cancelled ticket is dispatched.
- A previously `CANCELLED` tuple `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` does NOT block re-enqueueing if the Run is resumed later without changing `SourceEpoch`.
- `WorkerDaemon.runMergeTask` with nil `Options.GitAdapter` binds `makeIsolatedLookupEnv(req.ResolvedEnv)` onto its default `git.CLIAdapter`, never reading worker host `os.LookupEnv`.

## Requirements

### Functional Requirements

- **FR-001**: `internal/actor/merge` MUST define `ActorType = "MergeActor"`, `EffectiveTargetBranch`, `MergeActorID` (`sha256(git.NormalizeRepoURL(repoURL) + "\x00" + cleanBranch)` 64-char lowercase hex), `CandidateTicket`, `MergeCandidateNotification`, `RegisterManagedBranchReq`, and injectable boundary interfaces (`ConfigResolver`, `SlotManager`, `MergeWorkerDispatcher`, `RunMergeNotifier`, `ReminderScheduler`).
- **FR-002**: `MergeActor` MUST embed `actor.ServerImplBaseCtx`, implement `actor.ReminderCallee`, persist state under `"merge_actor_v1"` explicitly via `Set` + `Save` (including inside `ReminderCall`), and release `MergeActor.mu` before every external RPC.
- **FR-003**: `EnqueueCandidate` MUST deduplicate by `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` across active/completed/rework states, append to the persistent FIFO queue, and trigger non-blocking dispatch (`tryDispatchNext`) with `SlotRetryReminder` fallback when no worker slot is available.
- **FR-004**: `MergeActor` MUST enforce at most one `IN_FLIGHT` ticket at a time; register `!SharedTargetBranch` targets as `git.KindMergeBranch` via `RunMergeNotifier.RegisterManagedBranch` prior to dispatch (never registering explicit shared targets); resolve credentials via `ConfigResolver` into `RunMergeCandidateRequest.ResolvedEnv` + `Auth`; and advance the FIFO queue immediately after `ReportCandidateResult` regardless of `MergeStatusSucceeded`, `MergeStatusReworkSource`, or infrastructure error.
- **FR-005**: `OnWorkerDead` MUST reset the affected `IN_FLIGHT` ticket to the front of `Queue` as `PENDING` (clearing old lease fields) and immediately re-dispatch; `CancelByRun` MUST remove matching `PENDING` tickets, abort + release any matching `IN_FLIGHT` ticket, and advance remaining tickets; `GetState` MUST return a defensive snapshot of queue state and `LatestTargetSHA`.
- **FR-006**: `daemon.RunMergeCandidateRequest` MUST include backward-compatible `ResolvedEnv map[string]string`, and `WorkerDaemon.runMergeTask` MUST bind `makeIsolatedLookupEnv(req.ResolvedEnv)` onto its default `git.CLIAdapter` so merge tasks never read host `os.LookupEnv`.

## Success Criteria

### Measurable Outcomes

- **SC-001**: `go test -v ./internal/actor/merge/... ./internal/worker/daemon/... -count=1` and `go test ./... -count=1` pass offline with zero external processes.
- **SC-002**: `go test -v -tags=integration ./internal/actor/merge/... ./internal/worker/daemon/... -count=1` passes end-to-end against real `daprhost.Manager` (`scheduler` + `daprd`), bare Git repo, and Podman `alpine:latest` `gatekeeper` containers with zero leftover processes or containers.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly.
