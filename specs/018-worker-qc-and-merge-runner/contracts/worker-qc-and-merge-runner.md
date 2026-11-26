# Contract: Worker + QC Runner & Merge Primitive (`018-worker-qc-and-merge-runner`)

## 1. Git Adapter Contract (`internal/git`)

- `PushCommit(ctx context.Context, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA string, auth AuthConfig) error`
  - Validates `workspaceDir`, `repoURL`, `remoteBranch`, `commitSHA`, and optional `expectedOldRemoteSHA`.
  - When `Ledger != nil`, enforces `Ledger.EnsureRegistered(repoURL, remoteBranch)`.
  - When `expectedOldRemoteSHA != ""`, runs `git push --force-with-lease=refs/heads/<remoteBranch>:<expectedOldRemoteSHA> <repoURL> <commitSHA>:refs/heads/<remoteBranch>`.
  - When `expectedOldRemoteSHA == ""`, runs `git push <repoURL> <commitSHA>:refs/heads/<remoteBranch>`.

## 2. `WorkerRunner` State Machine (`internal/worker/runner/worker_runner.go`)

1. **Isolation & Cleanup**: Every execution creates a dedicated temp root containing `workspace/`, `imports/`, `artifacts/`, `work-session-store/`, `qc-session-store/`, `tools/` and removes it via `defer os.RemoveAll`.
2. **Physical Freeze Invariant**: `workHandle.Close()` MUST complete before any `artifact.Validate`, `sandboxEngine.RunGatekeepers`, or QC `SessionLauncher.Launch` executes.
3. **Read-Only QC Invariant**: QC container launch MUST set `ReadOnlyWorkspace: true`, `ReadOnlyArtifacts: true`, `WritablePaths: nil`, and inherit `sandbox`/`harness`/`model_provider`/`model`/`profile`/`reasoning_effort`/`retry` from `worker` unless overridden on `qc` (respecting `ModelProviderSet` when `qc` sets `model_provider = null`).
4. **Hard Rollback Invariant**: On Work failure with unauthorized workspace modifications outside `writable_paths`, `git.Adapter.RevertPaths` MUST revert all unauthorized paths before returning failure.
5. **Version-Bound Approval Invariant**: `human_approval = "required"` only proceeds when `ApprovalRecord.Approved == true && ApprovalRecord.CommitSHA == finalCommitSHA && slices.Equal(ApprovalRecord.ArtifactIDs, artifactIDs)`. Queued human interjections invalidate any `PendingPublication` short-circuit and force re-entry into the Work loop; successful publication or fallback clears `PendingPublication`. On `ReturnedUpstream == true`, routes to `"human_returned_upstream"` fallback or `StatusFailed`.
6. **Epoch Fencing & Lease Re-Publication Invariant**: `controlPlane.RegisterWorkBranch(ctx, epoch, repoURL, workBranch, baseSHA)` MUST be called and pass before `gitAdapter.PushCommit`, using `BranchContainsSHA` tip as `expectedOldRemoteSHA` so diverged cross-epoch rework pushes safely via `--force-with-lease`.

## 3. `ExecuteMergeCandidate` Contract (`internal/worker/runner/merge_primitive.go`)

1. Resolves `targetBranch` (`req.TargetBranch` or `git.WorkBranchName(req.RunID, req.MergeNodeAddr)`), ensures `KindMergeBranch` registration in `Ledger`, and initializes remote `targetBranch` at `baseSHA` if absent on remote.
2. Runs `RebaseCandidate` in an isolated temp directory (`defer os.RemoveAll`).
3. On rebase conflict -> returns `MergeStatusReworkSource` with `LatestTargetSHA`, `ConflictFiles`, `FailureReport`.
4. Runs `RunGatekeepers` if configured -> on failure returns `MergeStatusReworkSource` with `LatestTargetSHA`, `GatekeeperFailure`, `FailureReport`.
5. Pushes `RebasedSHA` with `expectedOldRemoteSHA = rebaseOut.TargetSHA` via `PushCommit` and returns `MergeStatusSucceeded`.
