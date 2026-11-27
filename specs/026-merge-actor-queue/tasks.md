---
description: "Stage 3 Slice 026 TDD tasks: MergeActor persistent target-branch queue and source-responsible serial integration (internal/actor/merge + daemon isolation)"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/worker/daemon/daemon_test.go` and `internal/actor/merge/merge_test.go` covering:
  - `daemon.RunMergeCandidateRequest.ResolvedEnv` credential isolation: when `Options.GitAdapter == nil`, `runMergeTask` binds an isolated `LookupEnv` reading exclusively from `req.ResolvedEnv` and never falling back to host `os.LookupEnv`;
  - `EffectiveTargetBranch` (`nil` -> `git.WorkBranchName` + `shared=false`; explicit `target_branch` -> normalized branch + `shared=true`) and `MergeActorID` (deterministic 64-char lowercase hex across URL variants);
  - Concurrent/batch enqueue of `W1, W2, W3`: strictly 1 `IN_FLIGHT` (`W1`) at a time; when `W1` returns `MergeStatusReworkSource` (conflict / gatekeeper failure), `RunMergeNotifier.OnMergeCandidateResult` receives accurate rework report + `LatestTargetSHA`, and `W2` immediately dispatches followed serially by `W3`;
  - Idempotent `EnqueueCandidate` on `(RunID, MergeNodeAddr, SourceNodeAddr, SourceEpoch)` (no duplicate tickets), while `SourceEpoch + 1` after source repair enqueues and executes normally;
  - `SharedTargetBranch` behavior: explicit `target_branch != null` sets `MergedIntoSharedBranch: true` on success without registering `KindMergeBranch`; default branch sets `MergedIntoSharedBranch: false` and registers `KindMergeBranch` via `RegisterManagedBranch` before `RunMergeCandidate`;
  - `ClaimSlot` saturation / `RunMergeCandidate` failure -> `ReleaseSlot` + `ReportSuspect` + `PENDING` + `SlotRetryReminder` -> `ReminderCall` explicit persistence & dispatch;
  - `OnWorkerDead` during `IN_FLIGHT` resets head ticket to `PENDING`, clears old lease, re-claims slot and re-dispatches on healthy worker while ignoring late stale reports;
  - `CancelByRun` (Run-wide and filtered by `MergeNodeAddr`/`SourceNodeAddr`) purges matching `PENDING` tickets, aborts + releases matching `IN_FLIGHT` ticket, and immediately advances queued tickets from other Runs.
- [X] T002 [Green] Update `internal/worker/daemon/daemon.go` (`RunMergeCandidateRequest.ResolvedEnv` + isolated default `git.CLIAdapter` in `runMergeTask`) and implement `internal/actor/merge/types.go` and `internal/actor/merge/actor.go`; verify `go test -v ./internal/actor/merge/... ./internal/worker/daemon/... -count=1` passes.
- [X] T003 [Red -> Green] Write Tier 2 real-Dapr + Git + Podman integration test `internal/actor/merge/merge_integration_test.go` (`//go:build integration`) using `daprhost.Manager` (`scheduler` + `daprd`), `ConfigActor`, `ProcessRegistryActor`, `MergeActor`, `WorkerDaemon`, real local bare Git repo, and real Podman `alpine:latest` `gatekeeper` containers:
  - Concurrently submit 3 candidate branches (`W1` + `W2` non-conflicting and passing Podman `gatekeeper`, `W3` conflicting or failing Podman `gatekeeper`);
  - Assert 2 linear rebase merges succeed on target branch, 1 fails with `MergeStatusReworkSource` receiving `LatestTargetSHA` equal to the exact tip SHA after the first 2 merges, and zero orphan `scheduler`/`daprd` processes or Podman containers remain.
- [X] T004 [Refactor & Verify] Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/actor/merge/... ./internal/worker/daemon/... -count=1`, verify zero stray processes/containers, and mark tasks `[X]`.
