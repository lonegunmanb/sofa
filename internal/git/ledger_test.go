package git

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/require"
)

func TestBranchNamesDeterministicAndCollisionFree(t *testing.T) {
	wb1 := WorkBranchName("run.1/prod", `worker.a.b`)
	wb2 := WorkBranchName("run.1/prod", `worker.a_b`)
	wb3 := WorkBranchName("run.1/prod", `worker.impl["alpha"]`)
	wb4 := WorkBranchName("run.1/prod", `worker.impl["beta"]`)

	require.NotEqual(t, wb1, wb2)
	require.NotEqual(t, wb3, wb4)
	require.True(t, strings.HasPrefix(wb1, "sofa/work/"))
	require.NoError(t, corespec.ValidateBranchName(wb1))
	require.NoError(t, corespec.ValidateBranchName(wb2))
	require.NoError(t, corespec.ValidateBranchName(wb3))
	require.NoError(t, corespec.ValidateBranchName(wb4))

	// Deterministic across calls.
	require.Equal(t, wb1, WorkBranchName("run.1/prod", `worker.a.b`))

	ck1 := CheckpointBranchName("run-1", `worker.a.b`, 1)
	ck2 := CheckpointBranchName("run-1", `worker.a_b`, 1)
	ck3 := CheckpointBranchName("run-1", `worker.a.b`, 2)
	require.NotEqual(t, ck1, ck2)
	require.NotEqual(t, ck1, ck3)
	require.True(t, strings.HasPrefix(ck1, "sofa/ckpt/run-1/"))
	require.True(t, strings.HasSuffix(ck1, "/e1"))
	require.True(t, strings.HasSuffix(ck3, "/e2"))
	require.NoError(t, corespec.ValidateBranchName(ck1))
	require.NoError(t, corespec.ValidateBranchName(ck2))
	require.NoError(t, corespec.ValidateBranchName(ck3))
}

func TestNormalizeRepoURL(t *testing.T) {
	require.Equal(t,
		NormalizeRepoURL("HTTPS://GitHub.COM/Org/Repo.git/"),
		NormalizeRepoURL("https://github.com/Org/Repo"),
	)
	require.Equal(t,
		NormalizeRepoURL("ssh://Git@GitHub.COM/Org/Repo.git/"),
		NormalizeRepoURL("ssh://Git@github.com/Org/Repo"),
	)
	require.Equal(t,
		NormalizeRepoURL("git@GitHub.com:Org/Repo.git/"),
		NormalizeRepoURL("git@github.com:Org/Repo"),
	)
	require.Equal(t,
		NormalizeRepoURL("file:///tmp/remote.git/"),
		NormalizeRepoURL("file:///tmp/remote"),
	)
	require.Equal(t,
		NormalizeRepoURL("/tmp/remote.git/"),
		NormalizeRepoURL("/tmp/remote"),
	)
}

func TestLedgerRegisterEnsureCleanupAndSnapshot(t *testing.T) {
	const (
		sha1 = "0123456789abcdef0123456789abcdef01234567"
		sha2 = "fedcba9876543210fedcba9876543210fedcba98"
	)

	ledger := NewLedger(SourceRef{
		URL:    "https://github.com/acme/repo.git/",
		Branch: "main",
	})

	// Unregistered branch must fail EnsureRegistered.
	err := ledger.EnsureRegistered("https://github.com/acme/repo", "sofa/work/run1/w1")
	require.ErrorIs(t, err, ErrUnregisteredBranch)

	// Invalid baseSHA must be rejected.
	err = ledger.Register("https://github.com/acme/repo", "sofa/work/run1/w1", "not-a-sha", KindWorkBranch, "worker.w1", 1)
	require.ErrorIs(t, err, ErrInvalidBaseSHA)
	err = ledger.Register("https://github.com/acme/repo", "sofa/work/run1/w1", strings.ToUpper(sha1), KindWorkBranch, "worker.w1", 1)
	require.ErrorIs(t, err, ErrInvalidBaseSHA)

	// Protected source branch cannot be registered as work or checkpoint branch (even with normalized URL variant).
	err = ledger.Register("https://GitHub.com/acme/repo", "main", sha1, KindWorkBranch, "worker.w1", 1)
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = ledger.Register("https://github.com/acme/repo.git", "main", sha1, KindCheckpointBranch, "worker.w1", 1)
	require.ErrorIs(t, err, ErrProtectedSourceBranch)

	// KindMergeBranch is explicitly permitted to target a source branch (e.g. merge.target_branch).
	require.NoError(t, ledger.Register("https://github.com/acme/repo.git", "main", sha1, KindMergeBranch, "merge.release", 1))

	// Register checkpoint e1, e2 and two work branches.
	ckpt1 := CheckpointBranchName("run1", "worker.impl", 1)
	ckpt2 := CheckpointBranchName("run1", "worker.impl", 2)
	work1 := WorkBranchName("run1", "worker.impl")
	work2 := WorkBranchName("run1", "worker.preserved")

	require.NoError(t, ledger.Register("https://github.com/acme/repo.git", ckpt1, sha1, KindCheckpointBranch, "worker.impl", 1))
	// Idempotent re-registration by same owner/kind/epoch/sha succeeds.
	require.NoError(t, ledger.Register("https://github.com/acme/repo", ckpt1, sha1, KindCheckpointBranch, "worker.impl", 1))

	// Conflicting owner or kind fails.
	err = ledger.Register("https://github.com/acme/repo", ckpt1, sha1, KindCheckpointBranch, "worker.other", 1)
	require.ErrorIs(t, err, ErrBranchOwnershipConflict)
	err = ledger.Register("https://github.com/acme/repo", ck1OrWork(ckpt1), sha1, KindWorkBranch, "worker.impl", 1)
	require.ErrorIs(t, err, ErrBranchOwnershipConflict)

	require.NoError(t, ledger.Register("https://github.com/acme/repo", ckpt2, sha2, KindCheckpointBranch, "worker.impl", 2))
	require.NoError(t, ledger.Register("https://github.com/acme/repo", work1, sha1, KindWorkBranch, "worker.impl", 1))
	require.NoError(t, ledger.Register("https://github.com/acme/repo", work2, sha1, KindWorkBranch, "worker.preserved", 1))

	// Advancing epoch on same owner/kind updates entry; stale epoch fails.
	require.NoError(t, ledger.Register("https://github.com/acme/repo", work1, sha2, KindWorkBranch, "worker.impl", 2))
	require.Error(t, ledger.Register("https://github.com/acme/repo", work1, sha1, KindWorkBranch, "worker.impl", 1))

	// EnsureRegistered matches normalized URL.
	require.NoError(t, ledger.EnsureRegistered("https://GitHub.com/acme/repo.git/", work1))

	// Snapshot + JSON round-trip + Restore.
	rawSnap, err := json.Marshal(ledger.Snapshot())
	require.NoError(t, err)
	var decoded LedgerSnapshot
	require.NoError(t, json.Unmarshal(rawSnap, &decoded))

	restored := NewLedger()
	require.NoError(t, restored.Restore(decoded))
	require.NoError(t, restored.EnsureRegistered("https://github.com/acme/repo", work1))

	// PlanCleanup excludes sources (main) and preserves (work2) with .git URL normalization,
	// and never returns unregistered branches.
	cleanup := restored.PlanCleanup(
		[]SourceRef{{URL: "https://github.com/acme/repo", Branch: "main"}},
		[]PreserveRef{{URL: "https://github.com/acme/repo.git/", Branch: work2}},
	)
	gotBranches := make([]string, len(cleanup))
	for i, target := range cleanup {
		gotBranches[i] = target.Branch
	}
	require.ElementsMatch(t, []string{ckpt1, ckpt2, work1}, gotBranches)
	require.NotContains(t, gotBranches, "main")
	require.NotContains(t, gotBranches, work2)
}

func ck1OrWork(s string) string { return s }

func TestLedgerConcurrency(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	ledger := NewLedger()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			node := "worker.n" + string(rune('a'+idx))
			branch := WorkBranchName("run1", node)
			require.NoError(t, ledger.Register("https://github.com/acme/repo", branch, sha, KindWorkBranch, node, 1))
			require.NoError(t, ledger.EnsureRegistered("https://github.com/acme/repo.git", branch))
			_ = ledger.Entries()
		}(i)
	}
	wg.Wait()
	require.Len(t, ledger.Entries(), 16)
}

func TestFindUnauthorizedPaths(t *testing.T) {
	changed := []ChangedPath{
		{Path: "cmd/sofa/main.go"},
		{Path: "internal/app.go"},
		{Path: "internal/git/ledger.go"},
		{Path: "internal/git/README.md"},
		{Path: "go.mod"},
		{Path: "README.md", Untracked: true},
	}

	// nil allows everything.
	require.Empty(t, FindUnauthorizedPaths(changed, nil))

	// Non-nil empty slice forbids ALL modifications.
	forbidAll := FindUnauthorizedPaths(changed, []string{})
	require.Equal(t, []string{
		"README.md",
		"cmd/sofa/main.go",
		"go.mod",
		"internal/app.go",
		"internal/git/README.md",
		"internal/git/ledger.go",
	}, forbidAll)

	// Glob matching including ** (0+ segments), *, and ?.
	globs := []string{"cmd/**", "internal/**/*.go", "go.mod", "docs/v?.md"}
	unauth := FindUnauthorizedPaths(append(changed,
		ChangedPath{Path: "docs/v1.md"},
		ChangedPath{Path: "docs/v10.md"},
		ChangedPath{Path: "cmd_extra/x.go"},
	), globs)
	require.Equal(t, []string{
		"README.md",
		"cmd_extra/x.go",
		"docs/v10.md",
		"internal/git/README.md",
	}, unauth)

	// Rename checks BOTH OldPath and Path.
	renameTests := []ChangedPath{
		{Path: "cmd/moved.go", OldPath: "secret/orig.go"},
		{Path: "secret/moved.go", OldPath: "cmd/orig.go"},
		{Path: "cmd/ok_dst.go", OldPath: "cmd/ok_src.go"},
	}
	require.Equal(t, []string{"secret/moved.go", "secret/orig.go"}, FindUnauthorizedPaths(renameTests, []string{"cmd/**"}))

	// "." allows workspace paths.
	require.Empty(t, FindUnauthorizedPaths(changed, []string{"."}))
}

func TestBuildAuthEnvAndSecretRedaction(t *testing.T) {
	tokenRef := "SOFA_TEST_GIT_TOKEN"
	sshRef := "SOFA_TEST_GIT_SSH_KEY"
	secretToken := "ghp_super_secret_token_12345"
	expectedBasic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secretToken))

	adapter := NewCLIAdapter(CLIAdapterOptions{
		LookupEnv: func(k string) (string, bool) {
			switch k {
			case tokenRef:
				return secretToken, true
			default:
				return "", false
			}
		},
	})

	// Mutual exclusion.
	_, _, _, err := adapter.buildAuthEnv(AuthConfig{TokenRef: &tokenRef, SSHKeyRef: &sshRef})
	require.Error(t, err)

	// Missing env var fails clearly.
	missingRef := "MISSING_VAR"
	_, _, _, err = adapter.buildAuthEnv(AuthConfig{TokenRef: &missingRef})
	require.Error(t, err)
	require.Contains(t, err.Error(), missingRef)

	// TokenRef injects GIT_TERMINAL_PROMPT=0 and GIT_CONFIG_* without leaking raw token in env var name.
	env, secrets, cleanup, err := adapter.buildAuthEnv(AuthConfig{TokenRef: &tokenRef})
	require.NoError(t, err)
	defer cleanup()

	envMap := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}
	require.Equal(t, "0", envMap["GIT_TERMINAL_PROMPT"])
	require.Equal(t, "1", envMap["GIT_CONFIG_COUNT"])
	require.Equal(t, "http.extraHeader", envMap["GIT_CONFIG_KEY_0"])
	require.Equal(t, "Authorization: Basic "+expectedBasic, envMap["GIT_CONFIG_VALUE_0"])

	// Redact both raw token and encoded header value.
	rawErr := "fatal: header 'Authorization: Basic " + expectedBasic + "' failed for token " + secretToken
	redacted := redactSecrets(rawErr, secrets)
	require.NotContains(t, redacted, secretToken)
	require.NotContains(t, redacted, expectedBasic)
	require.Contains(t, redacted, "<redacted>")

	// Inline SSH key writes temp file with strict permissions and deletes on cleanup.
	inlineKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nTESTKEYMATERIAL\n-----END OPENSSH PRIVATE KEY-----"
	sshAdapter := NewCLIAdapter(CLIAdapterOptions{
		LookupEnv: func(k string) (string, bool) {
			if k == sshRef {
				return inlineKey, true
			}
			return "", false
		},
	})
	sshEnv, sshSecrets, sshCleanup, err := sshAdapter.buildAuthEnv(AuthConfig{SSHKeyRef: &sshRef})
	require.NoError(t, err)

	var sshCmd string
	for _, kv := range sshEnv {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			sshCmd = strings.TrimPrefix(kv, "GIT_SSH_COMMAND=")
		}
	}
	require.NotEmpty(t, sshCmd)
	require.Contains(t, sshCmd, "-o IdentitiesOnly=yes")
	require.Contains(t, sshCmd, "-o BatchMode=yes")
	require.Contains(t, sshCmd, "-o StrictHostKeyChecking=accept-new")

	// Extract quoted key path from ssh -i "<path>" ...
	start := strings.Index(sshCmd, `ssh -i "`)
	require.GreaterOrEqual(t, start, 0)
	rest := sshCmd[start+len(`ssh -i "`):]
	end := strings.Index(rest, `"`)
	require.Greater(t, end, 0)
	keyPath := filepath.FromSlash(rest[:end])

	info, statErr := os.Stat(keyPath)
	require.NoError(t, statErr)
	require.False(t, info.IsDir())

	require.Contains(t, redactSecrets("err "+inlineKey, sshSecrets), "<redacted>")
	sshCleanup()
	_, statErr = os.Stat(keyPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestParseStatusPorcelainZ(t *testing.T) {
	raw := []byte(
		" M internal/git/cli.go\x00" +
			"?? untracked dir/with spaces.txt\x00" +
			"D  removed.txt\x00" +
			"R  new name.go\x00old name.go\x00" +
			"A  staged.go\x00",
	)
	got, err := parseStatusPorcelainZ(raw)
	require.NoError(t, err)
	require.Equal(t, []ChangedPath{
		{Path: "internal/git/cli.go"},
		{Path: "untracked dir/with spaces.txt", Untracked: true},
		{Path: "removed.txt"},
		{Path: "new name.go", OldPath: "old name.go"},
		{Path: "staged.go"},
	}, got)
}

func TestNormalizeBranchNameAndPrefixBypassPrevention(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	got, err := NormalizeBranchName("main")
	require.NoError(t, err)
	require.Equal(t, "main", got)

	got, err = NormalizeBranchName("  refs/heads/main  ")
	require.NoError(t, err)
	require.Equal(t, "main", got)

	got, err = NormalizeBranchName("refs/heads/feature/sub-1")
	require.NoError(t, err)
	require.Equal(t, "feature/sub-1", got)

	for _, invalid := range []string{"", "   ", "HEAD", "refs/heads/", "refs/tags/v1.0", "refs/remotes/origin/main", "-bad", "a..b"} {
		_, err := NormalizeBranchName(invalid)
		require.Error(t, err, "expected invalid branch name %q to fail", invalid)
	}

	repo := "https://github.com/acme/repo.git"
	ledger := NewLedger(SourceRef{URL: repo, Branch: "refs/heads/main"})

	// refs/heads/main must NOT bypass protected source check.
	err = ledger.Register(repo, "refs/heads/main", sha, KindWorkBranch, "worker.x", 1)
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = ledger.Register(repo, "main", sha, KindCheckpointBranch, "worker.x", 1)
	require.ErrorIs(t, err, ErrProtectedSourceBranch)

	// Invalid/non-head refs must be rejected by Register.
	require.Error(t, ledger.Register(repo, "HEAD", sha, KindWorkBranch, "worker.x", 1))
	require.Error(t, ledger.Register(repo, "refs/tags/v1", sha, KindWorkBranch, "worker.x", 1))

	// Registering with refs/heads/ prefix canonicalizes so EnsureRegistered matches bare branch and vice versa.
	work1 := WorkBranchName("run1", "worker.a")
	work2 := WorkBranchName("run1", "worker.b")
	require.NoError(t, ledger.Register(repo, "refs/heads/"+work1, sha, KindWorkBranch, "worker.a", 1))
	require.NoError(t, ledger.EnsureRegistered(repo, work1))
	require.NoError(t, ledger.EnsureRegistered(repo, "refs/heads/"+work1))
	require.NoError(t, ledger.EnsureDeletable(repo, work1))
	require.NoError(t, ledger.EnsureDeletable(repo, "refs/heads/"+work1))

	require.NoError(t, ledger.Register(repo, work2, sha, KindWorkBranch, "worker.b", 1))

	// KindMergeBranch can target main, but EnsureDeletable and DeleteRemoteBranch MUST refuse to delete main.
	require.NoError(t, ledger.Register(repo, "refs/heads/main", sha, KindMergeBranch, "merge.rel", 1))
	require.NoError(t, ledger.EnsureRegistered(repo, "main"))
	err = ledger.EnsureDeletable(repo, "main")
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = ledger.EnsureDeletable(repo, "refs/heads/main")
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = ledger.EnsureDeletable(repo, "sofa/work/run1/missing")
	require.ErrorIs(t, err, ErrUnregisteredBranch)

	adapter := NewCLIAdapter(CLIAdapterOptions{Ledger: ledger})
	err = adapter.DeleteRemoteBranch(nil, repo, "main", AuthConfig{})
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	err = adapter.DeleteRemoteBranch(nil, repo, "refs/heads/main", AuthConfig{})
	require.ErrorIs(t, err, ErrProtectedSourceBranch)
	_, _, err = adapter.CommitAllAndPush(nil, t.TempDir(), repo, "HEAD", "msg", AuthConfig{})
	require.Error(t, err)
	_, _, err = adapter.CommitAllAndPush(nil, t.TempDir(), repo, "refs/tags/v1", "msg", AuthConfig{})
	require.Error(t, err)

	// PlanCleanup preserves refs/heads/main and refs/heads/<work2>.
	cleanup := ledger.PlanCleanup(
		[]SourceRef{{URL: repo, Branch: "refs/heads/main"}},
		[]PreserveRef{{URL: repo, Branch: "refs/heads/" + work2}},
	)
	require.Equal(t, []BranchTarget{{RepoURL: repo, Branch: work1}}, cleanup)
}

func TestIsolatedRepoInitCommandsDisableAutoCRLF(t *testing.T) {
	t.Parallel()
	cmds := isolatedRepoInitCommands()
	require.Contains(t, cmds, []string{"config", "core.autocrlf", "false"})
}
