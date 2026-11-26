package git

import "context"

// AuthConfig configures transient host-memory authentication for remote Git commands.
type AuthConfig struct {
	TokenRef  *string
	SSHKeyRef *string
}

// RebaseOutcome describes the result of rebasing candidateBranch onto targetBranch.
type RebaseOutcome struct {
	Conflicted    bool
	TargetSHA     string
	RebasedSHA    string
	ConflictFiles []string
	Details       string
}

// Adapter defines host Git operations without persistent credentials or shared bare caches.
type Adapter interface {
	ResolveBranchSHA(ctx context.Context, repoURL, branch string, auth AuthConfig) (string, error)
	PrepareWorkspace(ctx context.Context, repoURL, fetchBranch, pinSHA, destDir string, auth AuthConfig) error
	HeadSHA(ctx context.Context, workspaceDir string) (string, error)
	StatusPaths(ctx context.Context, workspaceDir string) ([]ChangedPath, error)
	RevertPaths(ctx context.Context, workspaceDir string, paths []string) error
	CommitAllAndPush(ctx context.Context, workspaceDir, repoURL, remoteBranch, message string, auth AuthConfig) (newSHA string, committed bool, err error)
	PushCommit(ctx context.Context, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA string, auth AuthConfig) error
	BranchContainsSHA(ctx context.Context, repoURL, remoteBranch, targetSHA string, auth AuthConfig) (headSHA string, contains bool, err error)
	DeleteRemoteBranch(ctx context.Context, repoURL, remoteBranch string, auth AuthConfig) error
	RebaseCandidate(ctx context.Context, repoURL, targetBranch, candidateBranch, workDir string, auth AuthConfig) (RebaseOutcome, error)
}
