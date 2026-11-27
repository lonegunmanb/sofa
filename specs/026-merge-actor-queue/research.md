# Research: `MergeActor` Persistent Queue & Managed Git Credential Isolation (`026-merge-actor-queue`)

## Findings from Codebase Inspection

1. **`ExecuteMergeCandidate` Behavior (`internal/worker/runner/merge_primitive.go`, `types.go`)**:
   - Computes `targetBranch = git.WorkBranchName(req.RunID, req.MergeNodeAddr)` when `req.TargetBranch == nil` (or blank), and `strings.TrimSpace(*req.TargetBranch)` when provided.
   - Probes remote branch tip via `GitAdapter.BranchContainsSHA(..., strings.Repeat("0", 40), ...)`; initializes missing target branch from `BaseSHA` / `BasedSource` using `PrepareWorkspace` + `PushCommit`.
   - Rebases `CandidateBranch` onto `targetBranch` via `GitAdapter.RebaseCandidate`. On conflict, aborts rebase and returns `MergeStatusReworkSource` with `LatestTargetSHA = rebaseOut.TargetSHA`, `ConflictFiles`, and `FailureReport` without touching remote `targetBranch`.
   - Runs `req.Gatekeepers` inside `req.Sandbox` via `SandboxEngine.RunGatekeepers` (defaulting to `PodmanEngine` with `req.GitAdapter`). On gatekeeper failure, returns `MergeStatusReworkSource` with `LatestTargetSHA = rebaseOut.TargetSHA`, `GatekeeperFailure`, and `FailureReport` without advancing remote `targetBranch`.
   - Advances remote `targetBranch` with `PushCommit(..., rebaseOut.RebasedSHA, rebaseOut.TargetSHA, req.Auth)` (`--force-with-lease`) and returns `MergeStatusSucceeded` with `MergedCommitSHA = rebaseOut.RebasedSHA` and `LatestTargetSHA = rebaseOut.RebasedSHA`.
   - Requires `req.Epoch > 0` and `req.GitAdapter != nil`.

2. **Git Branch Ledger & Protected Branches (`internal/git/ledger.go`, `cli.go`)**:
   - `git.NormalizeRepoURL` canonicalizes local paths, `file://`, `ssh`/`scp`, and `https` URLs (stripping trailing `/` and `.git`).
   - `git.NormalizeBranchName` strips optional `refs/heads/`, rejects `HEAD` and non-branch refs (`refs/tags/...`), and validates branch syntax.
   - `git.Ledger.Register(..., git.KindMergeBranch, ...)` allows registering even branches matching `protectedSources` (for `EnsureRegistered` during `PushCommit`), while `EnsureDeletable` protects `protectedSources` and `PlanCleanup` preserves `sources` and `preserves`.
   - For default Run-exclusive merge branches (`!SharedTargetBranch`), `MergeActor` MUST notify `RunActor.RegisterManagedBranch(..., Kind: git.KindMergeBranch)` before dispatching `RunMergeCandidate` so `DeleteRun` cleans up the intermediate merge branch. For explicit shared `target_branch != null` (`SharedTargetBranch == true`), `MergeActor` MUST NOT register it as a Run-owned branch, and returns `MergedIntoSharedBranch: true` upon success so `RunActor.Rerun` (`028`) can enforce `409 Conflict` (`ErrAlreadyMergedSharedBranch`).

3. **Worker Daemon Git Credential Isolation (`internal/worker/daemon/daemon.go`)**:
   - `StartNodeRequest` carries `ResolvedEnv map[string]string` and `runNodeTask` passes `makeIsolatedLookupEnv(req.ResolvedEnv)` to `WorkerRunner`.
   - Currently, `RunMergeCandidateRequest` omits `ResolvedEnv` and `runMergeTask` passes `d.opts.GitAdapter` directly (which causes `ExecuteMergeCandidate` to fail when `d.opts.GitAdapter == nil` in production, or fall back to host `os.LookupEnv` if a default `CLIAdapter` without `LookupEnv` was used).
   - **Decision**: Add `ResolvedEnv map[string]string` (`json:"resolved_env,omitempty"`) to `daemon.RunMergeCandidateRequest`, and in `runMergeTask`, when `d.opts.GitAdapter == nil`, construct `git.NewCLIAdapter(git.CLIAdapterOptions{LookupEnv: makeIsolatedLookupEnv(req.ResolvedEnv)})` so merge tasks read Git `TokenRef`/`SSHKeyRef` exclusively from control-plane-resolved `ResolvedEnv` and never fall back to host `os.LookupEnv`.

4. **Actor Patterns & Dapr Concurrency (`internal/actor/node/`, `registry/`, `config/`)**:
   - Persist a single JSON document (`"merge_actor_v1"`) via `Contains` on load and `Set` + `Save` on write (including inside `ReminderCall`, which `dapr/go-sdk` does not auto-save).
   - Never hold `MergeActor.mu` across external RPCs (`ResolveForNode`, `RegisterManagedBranch`, `ClaimSlot`, `ReleaseSlot`, `ReportSuspect`, `RunMergeCandidate`, `AbortMergeCandidate`, `OnMergeCandidateResult`, reminder registration).
   - Use an explicit `dispatching`/`dispatchWake` loop flag under `mu` so concurrent callers (`EnqueueCandidate`, `ReminderCall`, `ReportCandidateResult`, `CancelByRun`, `OnWorkerDead`) serialize `tryDispatchNext` without holding `mu` during RPCs and without blocking `EnqueueCandidate` on slow state operations.
   - Commit `state.InFlight` (and `LeaseID`) BEFORE calling `MergeWorkerDispatcher.RunMergeCandidate` so immediate `ReportCandidateResult` callbacks match `state.InFlight` cleanly.
