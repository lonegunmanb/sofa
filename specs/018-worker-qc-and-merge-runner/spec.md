# Feature Specification: Single-Node Work + Frozen QC Runner & Deterministic Merge Primitive (`018-worker-qc-and-merge-runner`)

**Feature Branch**: `018-worker-qc-and-merge-runner`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `018` blueprint (`docs/stage2/018-worker-qc-and-merge-runner.md`), `docs/stage2/README.md`, `docs/hcl-contract.md`, `docs/architecture-contract.md`, and upstream Stage 2 packages (`011`–`017`).

## User Scenarios & Testing

### User Story 1 - Single-Node Work -> Physical Freeze -> Gatekeeper -> Independent Read-Only QC -> Approval -> Publish (`internal/worker/runner/worker_runner.go`) (Priority: P1)

`WorkerRunner.Run` executes a single static or dynamically-expanded `worker` node through its full Stage 2 closed-loop state machine:
1. Creates an isolated host temporary root (`workspace/`, `imports/`, `artifacts/`, `work-session-store/`, `qc-session-store/`, `tools/`) cleaned via `defer os.RemoveAll`.
2. Recovers from the latest three-way-aligned checkpoint (`checkpoint.Manager.RecoverLatestAligned`) or checks out a pinned `based_source` SHA (`git.Adapter.ResolveBranchSHA` + `PrepareWorkspace`), and stages `import_artifact` inputs (`artifact.Service.StageImports`). If a recovered checkpoint already has a verified `PendingPublication` at `FinalSHA`, jumps directly to `human_approval` evaluation without re-running LLM sessions.
3. Drives the rework loop:
   - **Work Session**: Launches the Work container (`ReadOnlyWorkspace: false`, `ReadOnlyArtifacts: false`, `WritablePaths: worker.WritablePaths`), runs `session.RunWorkPhase` with `ControlHooks` (`DrainInterjections`, `ValidateWorkspaceForHandoff`, `OnCheckpointRequested` -> `ckptMgr.SaveCheckpoint`), and **unconditionally closes and destroys the Work container immediately after `RunWorkPhase` returns** (`workHandle.Close()`). On `WaitingHuman`, saves checkpoint + QC ledger snapshot and returns `StatusWaitingHuman`; on `AgentEscalated`, publishes valid declared fallback artifacts, saves checkpoint, and returns `StatusFallbackTriggered`; on `ErrProtocolAttemptsExhausted` (or any error) with unauthorized paths, hard-rolls back unauthorized paths via `git.Adapter.RevertPaths`.
   - **Post-Freeze Verification**: Re-checks `FindUnauthorizedPaths` and `artifact.Validate(artifactsDir, artifactDecls)`, looping back to Work with formatted issues if rework budget remains.
   - **Deterministic Gatekeepers**: Runs `sandbox.Engine.RunGatekeepers` (`ReadOnlyArtifacts: true`); on failure loops back to Work up to `maxGatekeeperReworks` (default 3) or evaluates `"gatekeeper_exhausted"` fallback / `StatusFailed`.
   - **Independent Read-Only QC**: Resolves orthogonal QC overrides (`sandbox`, `harness`, `model_provider` including explicit `null` via `ModelProviderSet`, `model`, `profile`, `reasoning_effort`, `retry`), launches a separate QC container (`ReadOnlyWorkspace: true`, `ReadOnlyArtifacts: true`, `qc-session-store`), runs `session.RunQCPhase` with the cross-round `QCLedger`, and immediately destroys the QC container (`qcHandle.Close()`). On `"revise"`, persists `QCLedger` snapshot and loops back to Work if `qcRound < MaxQCRounds`, otherwise evaluates `"qc_exhausted"` fallback / `StatusFailed`.
4. Saves the final verified checkpoint (`finalCommitSHA`), publishes valid declared artifacts (`artifact.Service.Publish`), enforces version-bound `human_approval == "required"` against `(finalCommitSHA, artifactIDs)` (including `"human_returned_upstream"` fallback), fences & registers `work_branch` via `controlPlane.RegisterWorkBranch` (`ErrStaleEpoch` protection), and pushes `finalCommitSHA` via `git.Adapter.PushCommit`.

**Independent Test**: Tier 1 unit tests in `internal/worker/runner/runner_test.go` and Tier 2 real Git + Podman integration tests in `internal/worker/runner/runner_integration_test.go`.

**Acceptance Scenarios**:

1. **Given** a worker with a failing Round 1 `gatekeeper` and a `QC` block (`max_qc_rounds = 2`), **When** Round 1 `gatekeeper` fails, Work repairs, Round 1 `QC` opens `QC-001` (`revise`), Work repairs, and Round 2 `QC` resolves `QC-001` (`pass`), **Then** `workHandle.Close()` executes strictly before `RunGatekeepers` and before `QC`, `QC` receives `ReadOnlyWorkspace == true` and `ReadOnlyArtifacts == true` with its overridden `Sandbox`/`Harness`/`Model`, and the node publishes `work_branch` + `Artifact` returning `StatusSucceeded`.
2. **Given** a Work session exhausting `max_protocol_attempts` with unauthorized workspace modifications, **When** `RunWorkPhase` returns an error, **Then** `WorkerRunner` invokes `git.Adapter.RevertPaths` on all unauthorized paths before returning `StatusFailed`.
3. **Given** `human_approval = "required"`, **When** no approval exists -> returns `StatusWaitingHuman` with `PendingApproval` (`CommitSHA`, `ArtifactIDs`); **When** resumed with an approval carrying a stale `CommitSHA` -> stays `StatusWaitingHuman` without re-running Work/QC; **When** resumed with matching `(CommitSHA, ArtifactIDs)` -> publishes `work_branch` without re-running LLM sessions.
4. **Given** `qc_exhausted`, `gatekeeper_exhausted`, `agent_escalated`, or `human_returned_upstream`, **When** a matching `fallback` is declared -> returns `StatusFallbackTriggered` with `FallbackName`, `FallbackTarget`, `FallbackReason`, and valid `FallbackArtifacts`; **When** undeclared -> returns `StatusFailed`.
5. **Given** `sofa_request_decision`, **When** `RunWorkPhase` returns `OutcomeWaitingHuman`, **Then** `WorkerRunner` saves checkpoint, closes the Work container, removes the host temp root, and returns `StatusWaitingHuman` with `DecisionRequest`.

---

### User Story 2 - Deterministic Single-Candidate Merge Primitive (`internal/worker/runner/merge_primitive.go`) (Priority: P1)

`ExecuteMergeCandidate` executes the strict source-responsibility merge verification step:
1. Resolves `targetBranch` (`req.TargetBranch` or `git.WorkBranchName(req.RunID, req.MergeNodeAddr)`), registers `KindMergeBranch` in `git.Ledger` when configured, and initializes remote `targetBranch` at `baseSHA` if it does not yet exist on remote.
2. Runs `git.Adapter.RebaseCandidate` into an isolated temp directory (`defer os.RemoveAll`).
3. If `Conflicted == true`, returns `MergeStatusReworkSource` with `LatestTargetSHA`, `ConflictFiles`, and `FailureReport`.
4. If `len(req.Gatekeepers) > 0`, runs `sandbox.Engine.RunGatekeepers`; on failure returns `MergeStatusReworkSource` with `LatestTargetSHA` and `GatekeeperFailure`.
5. On pass, pushes `RebasedSHA` to `targetBranch` using `--force-with-lease=refs/heads/<branch>:<TargetSHA>` via `git.Adapter.PushCommit` and returns `MergeStatusSucceeded`.

**Independent Test**: Tier 1 unit tests in `runner_test.go` and Tier 2 sequential merge + conflict/gatekeeper rejection integration tests against real local Git bare repo and real Podman `alpine:latest` in `runner_integration_test.go`.

**Acceptance Scenarios**:

1. **Given** two non-conflicting candidate branches, **When** `ExecuteMergeCandidate` runs sequentially, **Then** both merge cleanly with linear rebased history on `targetBranch`.
2. **Given** a 3rd candidate branch with a merge conflict or failing Podman `gatekeeper`, **When** `ExecuteMergeCandidate` runs, **Then** `targetBranch` remains untouched at candidate 2's SHA and `ExecuteMergeCandidate` returns `MergeStatusReworkSource` with candidate 2's `LatestTargetSHA`.

---

### User Story 3 - Lease-Protected `git.Adapter.PushCommit` (`internal/git/adapter.go`, `cli.go`) (Priority: P1)

`git.Adapter` exposes `PushCommit(ctx, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA, auth)` so `internal/worker/runner` remains 100% free of `os/exec`.

**Acceptance Scenarios**:

1. **Given** a `CLIAdapter` with a `Ledger`, **When** `PushCommit` targets an unregistered branch, **Then** it fails with `git.ErrUnregisteredBranch`.
2. **Given** `expectedOldRemoteSHA != ""`, **When** remote branch tip matches `expectedOldRemoteSHA`, **Then** `git push --force-with-lease=refs/heads/<branch>:<expectedOldRemoteSHA>` succeeds; **When** remote branch concurrently advanced, **Then** `PushCommit` fails safely without overwriting.

## Requirements

- **FR-001**: `internal/git/adapter.go` and `internal/git/cli.go` MUST implement `PushCommit` with `Ledger.EnsureRegistered` enforcement and `--force-with-lease=refs/heads/<branch>:<expectedOldRemoteSHA>` support.
- **FR-002**: `internal/worker/runner/types.go` MUST define `OutcomeStatus`, `NodeOutcome`, `ApprovalTarget`, `ApprovalRecord`, `PendingPublication`, `NodeControlPlane`, `LocalNodeControlPlane`, `SessionLauncher`, `SessionHandle`, `ContainerSessionLauncher`, and `NodeExecutionRequest`.
- **FR-003**: `internal/worker/runner/worker_runner.go` MUST implement `WorkerRunner.Run` with physical Work container close before Gatekeepers/QC, orthogonal QC overrides (including `ModelProviderSet`), `writable_paths` hard rollback, epoch-fenced `RegisterWorkBranch`, version-bound `human_approval`, and deterministic fallback routing.
- **FR-004**: `internal/worker/runner/merge_primitive.go` MUST implement `ExecuteMergeCandidate` with zero `os/exec` imports in `internal/worker/runner`.

## Success Criteria

- **SC-001**: `gofmt -l .` returns empty output.
- **SC-002**: `go vet ./...` passes cleanly.
- **SC-003**: `go test ./... -count=1` passes all Tier 1 tests.
- **SC-004**: `go test -tags=integration ./internal/git/... ./internal/checkpoint/... ./internal/worker/runner/... -count=1` passes against real Git bare repos, real Podman `alpine:latest`, cross-compiled Linux harness plugin, and `blob.LocalStore`.
