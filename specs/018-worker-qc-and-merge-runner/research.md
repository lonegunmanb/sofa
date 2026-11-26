# Research Findings: `018-worker-qc-and-merge-runner`

## Upstream Package Verification (`011`–`017`)

1. **`internal/git` (`012`)**:
   - `git.Adapter` currently exposes `ResolveBranchSHA`, `PrepareWorkspace`, `HeadSHA`, `StatusPaths`, `RevertPaths`, `CommitAllAndPush`, `BranchContainsSHA`, `DeleteRemoteBranch`, `RebaseCandidate`.
   - `BranchContainsSHA` returns `("", false, nil)` when a remote branch does not exist, avoiding fragile CLI error-string parsing to detect missing `target_branch` in `ExecuteMergeCandidate`.
   - Extending `git.Adapter` with `PushCommit(ctx, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA, auth)` keeps `internal/worker/runner` completely free of `os/exec`, enforces `Ledger.EnsureRegistered`, and uses `--force-with-lease=refs/heads/<branch>:<expectedOldRemoteSHA>` for lease-protected merge advancement.
   - Only `internal/checkpoint/checkpoint_test.go` (`fakeGitAdapter`) mocks `git.Adapter` in the repository and must be updated alongside `adapter.go`/`cli.go`.

2. **`internal/sandbox/runtime` (`013`) & `internal/harness/runtime` (`014`)**:
   - `sandboxruntime.BuildRunArgs` automatically omits mounts whose host dir is `""`, enforces `/workspace/.git:ro` last, and sets `artifactsRO := layout.ReadOnlyWorkspace || layout.ReadOnlyArtifacts`.
   - `harnessruntime.NewContainerOpener` creates a `*GRPCClientOpener` exposing `ContainerID()`, `Capabilities()`, `Open()`, and `Close()`. `GRPCClientOpener.Close()` kills the plugin client and triggers `ContainerRunner.Kill` (`podman rm -f -t 2`).

3. **`internal/tool/runtime` (`015`) & `internal/session` (`016`)**:
   - `session.RunWorkPhase` and `session.RunQCPhase` open and close the logical gRPC session (`sess.Close`), while `WorkHandle.Close()` / `QCHandle.Close()` destroys the underlying `GRPCClientOpener` and container.
   - `session.QCLedger` supports `Snapshot()` / `Restore(data)` for checkpoint persistence across `WAITING_HUMAN` pauses and epochs.

4. **`internal/checkpoint` (`017`)**:
   - `checkpoint.LocalNodeStateStore` exposes `CurrentEpoch()` and `AdvanceEpoch()` with atomic JSON persistence and `ErrStaleEpoch` fencing.
   - `checkpoint.Manager.RecoverLatestAligned` restores workspace code, `artifacts/`, and `DiaryPromptContext` from the newest three-way-aligned checkpoint.

5. **`internal/worker/spec` (`005` / `010`) & `internal/merge/spec` (`006`)**:
   - `workerspec.QCConfig.ModelProviderSet` distinguishes omitted `model_provider` (inherit from `worker`) from explicit `model_provider = null` (clear provider so QC harness uses native auth).
   - `mergespec.Config` uses `TargetBranch *string` (`nil` -> `git.WorkBranchName(runID, mergeNodeAddr)`).
