//go:build integration

package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(sanitizedGitBaseEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoWithBaseCommit(t *testing.T) (bareDir, seedDir, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	seedDir = filepath.Join(root, "seed")

	runTestGit(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runTestGit(t, seedDir, "init")
	runTestGit(t, seedDir, "config", "user.name", "seed")
	runTestGit(t, seedDir, "config", "user.email", "seed@sofa.local")
	runTestGit(t, seedDir, "config", "commit.gpgsign", "false")

	writeSeedFile := func(rel, content string) {
		full := filepath.Join(seedDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	writeSeedFile("go.mod", "module example.com/test\n\ngo 1.24\n")
	writeSeedFile("cmd/app.go", "package main\nfunc main() {}\n")
	writeSeedFile("internal/keep.go", "package internal\n")
	writeSeedFile("README.md", "original readme\n")
	writeSeedFile("docs/old.txt", "old doc\n")
	writeSeedFile("shared.txt", "line1-base\nline2-base\n")

	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "initial")
	runTestGit(t, seedDir, "branch", "-M", "main")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	baseSHA = runTestGit(t, seedDir, "rev-parse", "HEAD")
	return bareDir, seedDir, baseSHA
}

func TestCLIAdapterWorkspaceRevertPushContainsAndDeleteIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, seedDir, baseSHA := initBareRepoWithBaseCommit(t)

	// Create a divergent commit on branch "other" for non-ancestor verification.
	runTestGit(t, seedDir, "checkout", "-b", "other")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "other.txt"), []byte("other\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "other commit")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/other")
	otherSHA := runTestGit(t, seedDir, "rev-parse", "HEAD")
	runTestGit(t, seedDir, "checkout", "main")

	ledger := NewLedger(SourceRef{URL: bareDir, Branch: "main"})
	adapter := NewCLIAdapter(CLIAdapterOptions{Ledger: ledger})

	resolvedSHA, err := adapter.ResolveBranchSHA(ctx, bareDir, "main", AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, baseSHA, resolvedSHA)

	// Prepare isolated workspace from pinned SHA.
	wsDir := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, adapter.PrepareWorkspace(ctx, bareDir, "main", baseSHA, wsDir, AuthConfig{}))

	headSHA0, err := adapter.HeadSHA(ctx, wsDir)
	require.NoError(t, err)
	require.Equal(t, baseSHA, headSHA0)

	// .git/config must contain zero remote URLs or credentials.
	gitConfigBytes, err := os.ReadFile(filepath.Join(wsDir, ".git", "config"))
	require.NoError(t, err)
	gitConfigText := string(gitConfigBytes)
	require.NotContains(t, gitConfigText, "[remote ")
	require.NotContains(t, gitConfigText, "extraHeader")
	require.NotContains(t, gitConfigText, "Authorization")

	// Reject pinSHA that does not belong to fetchBranch.
	badWsDir := filepath.Join(t.TempDir(), "bad-ws")
	err = adapter.PrepareWorkspace(ctx, bareDir, "main", otherSHA, badWsDir, AuthConfig{})
	require.Error(t, err)

	// Create mixed authorized & unauthorized modifications:
	// 1) Authorized tracked edit
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "cmd", "app.go"), []byte("package main\nfunc main() { println(1) }\n"), 0o644))
	// 2) Unauthorized tracked edit
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("tampered readme\n"), 0o644))
	// 3) Unauthorized staged new file
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "secret"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "secret", "staged_new.txt"), []byte("secret\n"), 0o644))
	runTestGit(t, wsDir, "add", "secret/staged_new.txt")
	// 4) Unauthorized untracked nested file
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "forbidden", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "forbidden", "sub", "untracked.txt"), []byte("bad\n"), 0o644))
	// 4b) Unauthorized untracked nested git repository (with a committed file so .git/objects exists)
	subRepoDir := filepath.Join(wsDir, "forbidden_subrepo")
	require.NoError(t, os.MkdirAll(subRepoDir, 0o755))
	runTestGit(t, subRepoDir, "init")
	runTestGit(t, subRepoDir, "config", "user.name", "nested")
	runTestGit(t, subRepoDir, "config", "user.email", "nested@sofa.local")
	runTestGit(t, subRepoDir, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(subRepoDir, "nested.txt"), []byte("nested\n"), 0o644))
	runTestGit(t, subRepoDir, "add", "nested.txt")
	runTestGit(t, subRepoDir, "commit", "-m", "nested init")
	// 5) Rename where destination is unauthorized (forbidden_mv/moved.go)
	require.NoError(t, os.MkdirAll(filepath.Join(wsDir, "forbidden_mv"), 0o755))
	runTestGit(t, wsDir, "mv", "internal/keep.go", "forbidden_mv/moved.go")
	// 6) Rename where source is unauthorized (docs/old.txt -> cmd/from_docs.txt)
	runTestGit(t, wsDir, "mv", "docs/old.txt", "cmd/from_docs.txt")

	changed, err := adapter.StatusPaths(ctx, wsDir)
	require.NoError(t, err)
	unauth := FindUnauthorizedPaths(changed, []string{"cmd/**", "internal/**/*.go"})
	require.Equal(t, []string{
		"README.md",
		"docs/old.txt",
		"forbidden/sub/untracked.txt",
		"forbidden_mv/moved.go",
		"forbidden_subrepo",
		"secret/staged_new.txt",
	}, unauth)

	require.NoError(t, adapter.RevertPaths(ctx, wsDir, unauth))

	// Authorized cmd/app.go modification must remain intact.
	appBytes, err := os.ReadFile(filepath.Join(wsDir, "cmd", "app.go"))
	require.NoError(t, err)
	require.Contains(t, string(appBytes), "println(1)")

	// Unauthorized changes must be reverted/removed and empty dirs pruned.
	readmeBytes, err := os.ReadFile(filepath.Join(wsDir, "README.md"))
	require.NoError(t, err)
	require.Equal(t, "original readme\n", strings.ReplaceAll(string(readmeBytes), "\r\n", "\n"))

	oldDocBytes, err := os.ReadFile(filepath.Join(wsDir, "docs", "old.txt"))
	require.NoError(t, err)
	require.Equal(t, "old doc\n", strings.ReplaceAll(string(oldDocBytes), "\r\n", "\n"))

	_, err = os.Stat(filepath.Join(wsDir, "secret"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(wsDir, "forbidden"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(wsDir, "forbidden_mv"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(wsDir, "forbidden_subrepo"))
	require.ErrorIs(t, err, os.ErrNotExist)

	// Ledger guard blocks pushing unregistered branch.
	ckpt1 := CheckpointBranchName("run1", "worker.impl", 1)
	workBranch := WorkBranchName("run1", "worker.impl")
	_, _, err = adapter.CommitAllAndPush(ctx, wsDir, bareDir, ckpt1, "unregistered", AuthConfig{})
	require.ErrorIs(t, err, ErrUnregisteredBranch)

	// Register and push ckpt1 (has changes -> committed == true).
	require.NoError(t, ledger.Register(bareDir, ckpt1, baseSHA, KindCheckpointBranch, "worker.impl", 1))
	ckptSHA, committed, err := adapter.CommitAllAndPush(ctx, wsDir, bareDir, ckpt1, "epoch 1 ckpt", AuthConfig{})
	require.NoError(t, err)
	require.True(t, committed)
	require.True(t, IsValidCommitSHA(ckptSHA))
	require.NotEqual(t, baseSHA, ckptSHA)

	// Register and push workBranch on clean tree (committed == false, still pushes remote branch).
	require.NoError(t, ledger.Register(bareDir, workBranch, baseSHA, KindWorkBranch, "worker.impl", 1))
	workSHA, committed, err := adapter.CommitAllAndPush(ctx, wsDir, bareDir, workBranch, "publish work", AuthConfig{})
	require.NoError(t, err)
	require.False(t, committed)
	require.Equal(t, ckptSHA, workSHA)

	// BranchContainsSHA: ancestor, equal, non-ancestor, and missing branch.
	headSHA, contains, err := adapter.BranchContainsSHA(ctx, bareDir, workBranch, baseSHA, AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, ckptSHA, headSHA)
	require.True(t, contains)

	headSHA, contains, err = adapter.BranchContainsSHA(ctx, bareDir, workBranch, ckptSHA, AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, ckptSHA, headSHA)
	require.True(t, contains)

	headSHA, contains, err = adapter.BranchContainsSHA(ctx, bareDir, workBranch, otherSHA, AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, ckptSHA, headSHA)
	require.False(t, contains)

	headSHA, contains, err = adapter.BranchContainsSHA(ctx, bareDir, "sofa/work/run1/missing", baseSHA, AuthConfig{})
	require.NoError(t, err)
	require.Empty(t, headSHA)
	require.False(t, contains)

	// DeleteRemoteBranch: ledger guard rejects unregistered and protected source (even with KindMergeBranch),
	// deletes registered non-source branch, and is idempotent.
	err = adapter.DeleteRemoteBranch(ctx, bareDir, "other", AuthConfig{})
	require.ErrorIs(t, err, ErrUnregisteredBranch)

	require.NoError(t, ledger.Register(bareDir, "main", baseSHA, KindMergeBranch, "merge.release", 1))
	err = adapter.DeleteRemoteBranch(ctx, bareDir, "main", AuthConfig{})
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = adapter.DeleteRemoteBranch(ctx, bareDir, "refs/heads/main", AuthConfig{})
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	mainStillSHA, err := adapter.ResolveBranchSHA(ctx, bareDir, "main", AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, baseSHA, mainStillSHA)

	require.NoError(t, adapter.DeleteRemoteBranch(ctx, bareDir, ckpt1, AuthConfig{}))
	headSHA, contains, err = adapter.BranchContainsSHA(ctx, bareDir, ckpt1, ckptSHA, AuthConfig{})
	require.NoError(t, err)
	require.Empty(t, headSHA)
	require.False(t, contains)

	// Idempotent repeat delete.
	require.NoError(t, adapter.DeleteRemoteBranch(ctx, bareDir, ckpt1, AuthConfig{}))
}

func TestCLIAdapterRebaseCandidateIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, seedDir, _ := initBareRepoWithBaseCommit(t)

	// Create clean candidate branch modifying clean.txt.
	runTestGit(t, seedDir, "checkout", "-b", "feat_clean")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "clean.txt"), []byte("clean change\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "clean candidate")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/feat_clean")

	// Create conflicting candidate branch modifying line 1 of shared.txt.
	runTestGit(t, seedDir, "checkout", "main")
	runTestGit(t, seedDir, "checkout", "-b", "feat_conflict")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-candidate\nline2-base\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "conflicting candidate")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/feat_conflict")

	// Advance main with both a non-conflicting file and a conflicting edit on line 1 of shared.txt.
	runTestGit(t, seedDir, "checkout", "main")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "main_only.txt"), []byte("from main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-main\nline2-base\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "advance main")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	targetSHA := runTestGit(t, seedDir, "rev-parse", "HEAD")

	adapter := NewCLIAdapter(CLIAdapterOptions{})

	// 1) Clean rebase leaves workDir checked out at RebasedSHA.
	cleanWorkDir := filepath.Join(t.TempDir(), "rebase-clean")
	cleanOutcome, err := adapter.RebaseCandidate(ctx, bareDir, "main", "feat_clean", cleanWorkDir, AuthConfig{})
	require.NoError(t, err)
	require.False(t, cleanOutcome.Conflicted)
	require.Equal(t, targetSHA, cleanOutcome.TargetSHA)
	require.True(t, IsValidCommitSHA(cleanOutcome.RebasedSHA))
	require.NotEqual(t, targetSHA, cleanOutcome.RebasedSHA)

	headAtWorkDir := runTestGit(t, cleanWorkDir, "rev-parse", "HEAD")
	require.Equal(t, cleanOutcome.RebasedSHA, headAtWorkDir)
	_, err = os.Stat(filepath.Join(cleanWorkDir, "main_only.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(cleanWorkDir, "clean.txt"))
	require.NoError(t, err)

	// 2) Conflicting rebase reports Conflicted=true, ConflictFiles, TargetSHA, and aborts cleanly.
	conflictWorkDir := filepath.Join(t.TempDir(), "rebase-conflict")
	conflictOutcome, err := adapter.RebaseCandidate(ctx, bareDir, "main", "feat_conflict", conflictWorkDir, AuthConfig{})
	require.NoError(t, err)
	require.True(t, conflictOutcome.Conflicted)
	require.Equal(t, targetSHA, conflictOutcome.TargetSHA)
	require.Empty(t, conflictOutcome.RebasedSHA)
	require.Equal(t, []string{"shared.txt"}, conflictOutcome.ConflictFiles)
	require.NotEmpty(t, conflictOutcome.Details)

	_, rebaseMergeErr := os.Stat(filepath.Join(conflictWorkDir, ".git", "rebase-merge"))
	require.ErrorIs(t, rebaseMergeErr, os.ErrNotExist)
	_, rebaseApplyErr := os.Stat(filepath.Join(conflictWorkDir, ".git", "rebase-apply"))
	require.ErrorIs(t, rebaseApplyErr, os.ErrNotExist)
}

func TestCLIAdapterPushCommitAndForceWithLeaseIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, seedDir, baseSHA := initBareRepoWithBaseCommit(t)

	ledger := NewLedger(SourceRef{URL: bareDir, Branch: "main"})
	adapter := NewCLIAdapter(CLIAdapterOptions{Ledger: ledger})

	// Create two candidate commits locally in separate branches.
	runTestGit(t, seedDir, "checkout", "-b", "cand1")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "c1.txt"), []byte("c1\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "c1")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand1")

	runTestGit(t, seedDir, "checkout", "main")
	runTestGit(t, seedDir, "checkout", "-b", "cand2")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "c2.txt"), []byte("c2\n"), 0o644))
	runTestGit(t, seedDir, "add", "-A")
	runTestGit(t, seedDir, "commit", "-m", "c2")
	runTestGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand2")

	mergeBranch := WorkBranchName("run-push", "merge.integrate")
	// Unregistered branch must be rejected by Ledger.
	err := adapter.PushCommit(ctx, seedDir, bareDir, mergeBranch, baseSHA, "", AuthConfig{})
	require.ErrorIs(t, err, ErrUnregisteredBranch)

	require.NoError(t, ledger.Register(bareDir, mergeBranch, baseSHA, KindMergeBranch, "merge.integrate", 1))
	require.NoError(t, adapter.PushCommit(ctx, seedDir, bareDir, mergeBranch, baseSHA, "", AuthConfig{}))

	// Rebase cand1 onto mergeBranch and advance mergeBranch with exact expectedOldRemoteSHA.
	rebaseDir1 := filepath.Join(t.TempDir(), "rebase1")
	out1, err := adapter.RebaseCandidate(ctx, bareDir, mergeBranch, "cand1", rebaseDir1, AuthConfig{})
	require.NoError(t, err)
	require.False(t, out1.Conflicted)
	require.Equal(t, baseSHA, out1.TargetSHA)
	require.NoError(t, adapter.PushCommit(ctx, rebaseDir1, bareDir, mergeBranch, out1.RebasedSHA, out1.TargetSHA, AuthConfig{}))

	// Stale lease (baseSHA instead of out1.RebasedSHA) must fail safely without overwriting.
	rebaseDir2 := filepath.Join(t.TempDir(), "rebase2")
	out2, err := adapter.RebaseCandidate(ctx, bareDir, mergeBranch, "cand2", rebaseDir2, AuthConfig{})
	require.NoError(t, err)
	require.False(t, out2.Conflicted)
	require.Equal(t, out1.RebasedSHA, out2.TargetSHA)

	err = adapter.PushCommit(ctx, rebaseDir2, bareDir, mergeBranch, out2.RebasedSHA, baseSHA, AuthConfig{})
	require.Error(t, err)

	tipAfterStale, err := adapter.ResolveBranchSHA(ctx, bareDir, mergeBranch, AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, out1.RebasedSHA, tipAfterStale)

	// Exact lease succeeds.
	require.NoError(t, adapter.PushCommit(ctx, rebaseDir2, bareDir, mergeBranch, out2.RebasedSHA, out2.TargetSHA, AuthConfig{}))
	tipAfterValid, err := adapter.ResolveBranchSHA(ctx, bareDir, mergeBranch, AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, out2.RebasedSHA, tipAfterValid)
}
