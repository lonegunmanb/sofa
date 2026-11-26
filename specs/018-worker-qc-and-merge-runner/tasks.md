---
description: "Stage 2 Slice 018 TDD tasks: git.PushCommit, WorkerRunner state machine, ExecuteMergeCandidate, and Tier 1 + Tier 2 real Git/Podman verification"
---

# Tasks

- [X] T001 [Git Boundary] Add `PushCommit` to `internal/git/adapter.go` and `*CLIAdapter` in `internal/git/cli.go`, update `fakeGitAdapter` in `internal/checkpoint/checkpoint_test.go`, and add lease-protected push tests in `internal/git/cli_integration_test.go`.
- [X] T002 [Types] Create `internal/worker/runner/types.go` defining `OutcomeStatus`, `NodeOutcome`, `ApprovalTarget`, `ApprovalRecord`, `PendingPublication`, `NodeControlPlane`, `LocalNodeControlPlane`, `SessionLauncher`, `ContainerSessionLauncher`, `NodeExecutionRequest`, `MergeCandidateRequest`, and `MergeCandidateResult`.
- [X] T003 [Red] Write failing Tier 1 tests in `internal/worker/runner/runner_test.go` covering:
  - Full rework loop (Round 1 gatekeeper fail -> Work fix -> Round 1 QC opens `QC-001` & `revise` -> Work fix -> Round 2 QC resolves `QC-001` & `pass` -> publish `work_branch` + `Artifact`);
  - Strict ordering & mount invariants (`workHandle.Close()` before `RunGatekeepers` and `QC`, QC `ReadOnlyWorkspace == true`, `ReadOnlyArtifacts == true`, independent QC `Sandbox`/`Harness`/`Model`/`model_provider = null`);
  - Unauthorized path exhaustion -> `RevertPaths` hard-rollback before failure;
  - `human_approval = "required"` (pending approval -> `StatusWaitingHuman`, stale `CommitSHA` rejected without LLM re-run, matching `(CommitSHA, ArtifactIDs)` publishes without LLM re-run, `ReturnedUpstream` fallback);
  - `qc_exhausted`, `gatekeeper_exhausted`, `agent_escalated` with declared `fallback` -> `StatusFallbackTriggered`, undeclared -> `StatusFailed`;
  - `WaitingHuman` on `sofa_request_decision` saves checkpoint, closes container, and cleans host temp root;
  - `ExecuteMergeCandidate` clean rebase + gatekeeper + lease-push, conflict rework, and gatekeeper rework.
- [X] T004 [Red] Write failing Tier 2 integration tests in `internal/worker/runner/runner_integration_test.go` (`//go:build integration`) against real local Git bare repo, real Podman `alpine:latest`, cross-compiled scripted Linux harness plugin, and `blob.LocalStore` covering end-to-end `WorkerRunner` (Work -> Freeze -> Gatekeeper -> Read-Only QC -> `work_branch` + `BlobStore`) and `ExecuteMergeCandidate` (2 clean merges + 3rd conflicting/gatekeeper-failing candidate).
- [X] T005 [Green] Implement `internal/worker/runner/worker_runner.go` and `internal/worker/runner/merge_primitive.go` so all Tier 1 and Tier 2 tests pass.
- [X] T006 [Verify] Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/git/... ./internal/checkpoint/... ./internal/worker/runner/... -count=1`, leaving working tree uncommitted for review.
