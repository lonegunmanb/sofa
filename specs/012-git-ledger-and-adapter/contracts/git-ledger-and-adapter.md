# Contract: `internal/git`

## 1. Branch Naming & Ledger (`ledger.go`)

- `WorkBranchName(runID, canonicalNodeAddr string) string`: returns `"sofa/work/" + sanitizeRefSegment(runID) + "/" + sanitizeNodeKey(canonicalNodeAddr)`.
- `CheckpointBranchName(runID, canonicalNodeAddr string, epoch uint64) string`: returns `"sofa/ckpt/" + sanitizeRefSegment(runID) + "/" + sanitizeNodeKey(canonicalNodeAddr) + "/e" + strconv.FormatUint(epoch, 10)`.
- `sanitizeNodeKey` appends `_` + 8-char lowercase SHA-256 hex of raw `canonicalNodeAddr` so distinct addresses never collide after punctuation sanitization.
- `NormalizeBranchName`: strips optional `"refs/heads/"` prefix, rejects `"HEAD"`, non-head `"refs/*"` namespaces, and invalid branch names (`corespec.ValidateBranchName`).
- `NormalizeRepoURL`: trims trailing `/`, strips trailing `.git`, lowercases scheme/host for URLs and SCP hostnames, normalizes `file://` and local path separators.
- `Ledger.Register`: canonicalizes branch names (`NormalizeBranchName`); requires valid 40-char lowercase hex `baseSHA`; rejects protected `source` branches unless `kind == KindMergeBranch`; rejects conflicting `OwnerNode`/`Kind`; supports idempotent re-registration and monotonic epoch updates; supports `Snapshot()` / `Restore()`.
- `Ledger.EnsureRegistered`: canonicalizes branch names and returns `ErrUnregisteredBranch` when `(repoURL, branch)` is unregistered.
- `Ledger.EnsureDeletable`: verifies `EnsureRegistered` and rejects any protected `source` branch with `ErrProtectedSourceBranch` (even if registered as `KindMergeBranch`).
- `Ledger.PlanCleanup`: returns sorted `[]BranchTarget` for all registered entries excluding any entry matching `sources` or `preserves` (with normalized URL and canonical branch matching).

## 2. `writable_paths` Matcher (`pathmatch.go`)

- `FindUnauthorizedPaths(changed []ChangedPath, writableGlobs []string) []string`:
  - `writableGlobs == nil`: returns `nil` (all allowed).
  - `writableGlobs != nil && len(writableGlobs) == 0`: forbids all changed paths (returns sorted unique paths).
  - Supports POSIX `/` globs `*`, `?`, `**` (0+ directory segments), and `"."`.
  - Checks both `Path` and non-empty `OldPath` (Git renames/copies).

## 3. Host `git` CLI Adapter (`adapter.go`, `cli.go`)

- `CLIAdapter` injects `LookupEnv` (default `os.LookupEnv`) and optional `*Ledger`.
- When `Ledger != nil`, `CommitAllAndPush` calls `Ledger.EnsureRegistered(repoURL, remoteBranch)` and `DeleteRemoteBranch` calls `Ledger.EnsureDeletable(repoURL, remoteBranch)` before contacting the remote.
- `RevertPaths` restores `HEAD`-backed paths via `git checkout HEAD --`, unstages newly added paths via `git reset HEAD --`, and removes untracked files and untracked nested directories (including nested `.git` directories) safely inside `os.OpenRoot(workspaceDir)` without following symlinks while pruning empty parent directories.
- Zero credentials written to `<workspace>/.git/config`; `TokenRef` uses `GIT_CONFIG_COUNT=1` / `http.extraHeader` and redacts token + base64 credentials in errors; `SSHKeyRef` inline keys use `0600` / Windows protected DACL temp files deleted immediately on return.
- `CommitAllAndPush` stages all changes, commits if staged diff against `HEAD` is non-empty (`committed = true`) or keeps `HEAD` (`committed = false`), and pushes `HEAD:refs/heads/<remoteBranch>`.
- `BranchContainsSHA` returns `("", false, nil)` when the remote branch is absent; otherwise verifies `targetSHA` is equal to or an ancestor of remote tip `headSHA`.
- `DeleteRemoteBranch` deletes the remote branch idempotently.
- `RebaseCandidate` fetches `targetBranch` (recording `TargetSHA`) and `candidateBranch`, rebases candidate onto `TargetSHA`, aborts and returns `Conflicted: true` with `ConflictFiles` on conflict, or leaves `workDir` checked out at `RebasedSHA`.
