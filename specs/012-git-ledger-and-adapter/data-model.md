# Data Model: `internal/git`

## Pure Domain Types (`ledger.go`, `pathmatch.go`)

### `BranchKind`
- `KindWorkBranch = "work_branch"`
- `KindCheckpointBranch = "checkpoint_branch"`
- `KindMergeBranch = "merge_branch"`

### `SourceRef` & `PreserveRef` & `BranchTarget`
- `SourceRef`: `{URL string, Branch string}` (JSON: `url`, `branch`)
- `PreserveRef`: `{URL string, Branch string}` (JSON: `url`, `branch`)
- `BranchTarget`: `{RepoURL string, Branch string}` (JSON: `repo_url`, `branch`)

### `BranchEntry` & `LedgerSnapshot`
- `BranchEntry`: `{RepoURL string, Branch string, BaseSHA string, Kind BranchKind, OwnerNode string, Epoch uint64, Registered bool}`
- `LedgerSnapshot`: `{ProtectedSources []SourceRef, Entries []BranchEntry}`

### Sentinel Errors
- `ErrUnregisteredBranch`
- `ErrProtectedSourceBranch`
- `ErrBranchOwnershipConflict`
- `ErrInvalidBaseSHA`

### `ChangedPath` (`pathmatch.go`)
- `{Path string, OldPath string, Untracked bool}`

## Adapter Types (`adapter.go`, `cli.go`)

### `AuthConfig`
- `{TokenRef *string, SSHKeyRef *string}`

### `RebaseOutcome`
- `{Conflicted bool, TargetSHA string, RebasedSHA string, ConflictFiles []string, Details string}`

### `Adapter` Interface
- `ResolveBranchSHA(ctx, repoURL, branch, auth) (string, error)`
- `PrepareWorkspace(ctx, repoURL, fetchBranch, pinSHA, destDir, auth) error`
- `StatusPaths(ctx, workspaceDir) ([]ChangedPath, error)`
- `RevertPaths(ctx, workspaceDir, paths) error`
- `CommitAllAndPush(ctx, workspaceDir, repoURL, remoteBranch, message, auth) (string, bool, error)`
- `BranchContainsSHA(ctx, repoURL, remoteBranch, targetSHA, auth) (string, bool, error)`
- `DeleteRemoteBranch(ctx, repoURL, remoteBranch, auth) error`
- `RebaseCandidate(ctx, repoURL, targetBranch, candidateBranch, workDir, auth) (RebaseOutcome, error)`
