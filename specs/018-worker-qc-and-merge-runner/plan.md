# Implementation Plan: Single-Node Work + Frozen QC Runner & Deterministic Merge Primitive (`018-worker-qc-and-merge-runner`)

**Branch**: `018-worker-qc-and-merge-runner` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `018`:
1. Extend `internal/git/adapter.go` and `internal/git/cli.go` with `PushCommit(ctx, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA, auth)` (plus update `internal/checkpoint/checkpoint_test.go` mock and add integration coverage in `internal/git/cli_integration_test.go`).
2. Implement `internal/worker/runner/types.go` (`OutcomeStatus`, `NodeOutcome`, `ApprovalTarget`, `ApprovalRecord`, `PendingPublication`, `NodeControlPlane`, `LocalNodeControlPlane`, `SessionLauncher`, `ContainerSessionLauncher`, `NodeExecutionRequest`, `MergeCandidateRequest`, `MergeCandidateResult`).
3. Implement `internal/worker/runner/worker_runner.go` (`WorkerRunner` orchestrating Work -> Freeze -> Gatekeepers -> Read-Only QC -> Final Checkpoint -> Version-Bound Approval -> Epoch-Fenced Work Branch Publish).
4. Implement `internal/worker/runner/merge_primitive.go` (`ExecuteMergeCandidate`).
5. Deliver comprehensive Tier 1 unit tests in `internal/worker/runner/runner_test.go` and Tier 2 real Git + Podman + `BlobStore` + scripted Linux plugin integration tests in `internal/worker/runner/runner_integration_test.go`.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Existing Packages Reused**:
  - `internal/blob`, `internal/artifact`
  - `internal/git`
  - `internal/harness/installer`, `internal/harness/runtime`, `internal/harness/spec`
  - `internal/sandbox/runtime`, `internal/sandbox/spec`
  - `internal/tool/runtime`, `internal/plan`
  - `internal/provider`, `internal/mcp`
  - `internal/session`
  - `internal/checkpoint`
  - `internal/worker/spec`, `internal/merge/spec`
- **Testing**:
  - Tier 1 offline unit tests: `go test ./... -count=1`
  - Tier 2 real-dependency integration tests: `go test -tags=integration ./internal/git/... ./internal/checkpoint/... ./internal/worker/runner/... -count=1`

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts created under `specs/018-worker-qc-and-merge-runner/` prior to production code.
- **Principle II (Strict TDD)**: Failing Tier 1 (`runner_test.go`) and Tier 2 (`runner_integration_test.go`) tests authored and verified Red before implementing `worker_runner.go` / `merge_primitive.go`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `internal/worker/runner` contains ZERO `os/exec` imports; all Git CLI, container runtime, and plugin launching sit behind `git.Adapter`, `sandboxruntime.Engine`, `SessionLauncher`, and `NodeControlPlane`, verified end-to-end against real Git and Podman.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Work container is unconditionally closed before Gatekeepers and QC; QC mounts workspace and artifacts `:ro`; unauthorized `writable_paths` are rolled back via `git.Adapter.RevertPaths`; approvals are strictly bound to `(CommitSHA, ArtifactIDs)`; `work_branch` registration is fenced by `epoch`.
- **Principle VI (Research-First & Surgical Task Sizing)**: All upstream packages `011`–`017` and `worker/spec` + `merge/spec` inspected in full; scoped strictly to Slice `018`.

## Files

```text
internal/git/adapter.go
internal/git/cli.go
internal/git/cli_integration_test.go
internal/checkpoint/checkpoint_test.go
internal/worker/runner/types.go
internal/worker/runner/worker_runner.go
internal/worker/runner/merge_primitive.go
internal/worker/runner/runner_test.go
internal/worker/runner/runner_integration_test.go
specs/018-worker-qc-and-merge-runner/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
