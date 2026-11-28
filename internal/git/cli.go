package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// CLIAdapterOptions configures a CLIAdapter.
type CLIAdapterOptions struct {
	GitBin    string
	LookupEnv func(string) (string, bool)
	Ledger    *Ledger
}

// CLIAdapter implements Adapter by invoking the host git CLI without persisting credentials.
type CLIAdapter struct {
	GitBin    string
	LookupEnv func(string) (string, bool)
	Ledger    *Ledger
}

// NewCLIAdapter creates a CLIAdapter with default git binary and os.LookupEnv when omitted.
func NewCLIAdapter(opts CLIAdapterOptions) *CLIAdapter {
	gitBin := strings.TrimSpace(opts.GitBin)
	if gitBin == "" {
		gitBin = "git"
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &CLIAdapter{
		GitBin:    gitBin,
		LookupEnv: lookup,
		Ledger:    opts.Ledger,
	}
}

var _ Adapter = (*CLIAdapter)(nil)

func (a *CLIAdapter) gitBin() string {
	if a != nil && strings.TrimSpace(a.GitBin) != "" {
		return a.GitBin
	}
	return "git"
}

func (a *CLIAdapter) lookupEnvFunc() func(string) (string, bool) {
	if a != nil && a.LookupEnv != nil {
		return a.LookupEnv
	}
	return os.LookupEnv
}

func (a *CLIAdapter) buildAuthEnv(auth AuthConfig) ([]string, []string, func(), error) {
	noop := func() {}
	if auth.TokenRef != nil && auth.SSHKeyRef != nil {
		return nil, nil, noop, errors.New("token_ref and ssh_key_ref are mutually exclusive")
	}

	env := sanitizedGitBaseEnv(os.Environ())
	env = append(env, "GIT_TERMINAL_PROMPT=0")

	var secrets []string
	cleanup := noop
	lookup := a.lookupEnvFunc()

	if auth.TokenRef != nil {
		refName := strings.TrimSpace(*auth.TokenRef)
		if refName == "" {
			return nil, nil, noop, errors.New("token_ref environment variable name must not be empty")
		}
		token, ok := lookup(refName)
		if !ok || strings.TrimSpace(token) == "" {
			return nil, nil, noop, fmt.Errorf("git token environment variable %q is not set or empty", refName)
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		header := "Authorization: Basic " + basic
		secrets = append(secrets, token, basic, header)
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+header,
		)
	}

	if auth.SSHKeyRef != nil {
		refName := strings.TrimSpace(*auth.SSHKeyRef)
		if refName == "" {
			return nil, nil, noop, errors.New("ssh_key_ref environment variable name must not be empty")
		}
		val, ok := lookup(refName)
		if !ok || strings.TrimSpace(val) == "" {
			return nil, nil, noop, fmt.Errorf("git ssh key environment variable %q is not set or empty", refName)
		}

		keyPath := strings.TrimSpace(val)
		if strings.HasPrefix(keyPath, "-----BEGIN") {
			secrets = append(secrets, val, keyPath)
			tmpDir, err := os.MkdirTemp("", "sofa-ssh-*")
			if err != nil {
				return nil, nil, noop, fmt.Errorf("create temp ssh key directory: %w", err)
			}
			keyFilePath := filepath.Join(tmpDir, "id_key")
			f, err := os.OpenFile(keyFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				_ = os.RemoveAll(tmpDir)
				return nil, nil, noop, fmt.Errorf("create temp ssh key file: %w", err)
			}
			content := val
			if !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			_, writeErr := f.WriteString(content)
			closeErr := f.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				_ = os.RemoveAll(tmpDir)
				return nil, nil, noop, fmt.Errorf("write temp ssh key file: %w", err)
			}
			if err := restrictPrivateKeyFile(keyFilePath); err != nil {
				_ = os.RemoveAll(tmpDir)
				return nil, nil, noop, fmt.Errorf("restrict temp ssh key permissions: %w", err)
			}
			keyPath = keyFilePath
			cleanup = func() {
				_ = os.Remove(keyFilePath)
				_ = os.RemoveAll(tmpDir)
			}
		}

		slashKeyPath := strings.ReplaceAll(filepath.ToSlash(keyPath), `"`, `\"`)
		sshCmd := fmt.Sprintf(`ssh -i "%s" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new`, slashKeyPath)
		env = append(env, "GIT_SSH_COMMAND="+sshCmd)
	}

	return env, secrets, cleanup, nil
}

func sanitizedGitBaseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "GIT_TERMINAL_PROMPT",
			k == "GIT_CONFIG_COUNT",
			strings.HasPrefix(k, "GIT_CONFIG_KEY_"),
			strings.HasPrefix(k, "GIT_CONFIG_VALUE_"),
			k == "GIT_SSH_COMMAND",
			k == "GIT_DIR",
			k == "GIT_WORK_TREE",
			k == "GIT_INDEX_FILE":
			continue
		default:
			out = append(out, kv)
		}
	}
	return out
}

func redactSecrets(text string, secrets []string) string {
	out := text
	for _, s := range secrets {
		if s != "" {
			out = strings.ReplaceAll(out, s, "<redacted>")
		}
	}
	return out
}

func parseStatusPorcelainZ(raw []byte) ([]ChangedPath, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	tokens := bytes.Split(raw, []byte{0})
	var out []ChangedPath
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) == 0 {
			continue
		}
		if len(tok) < 4 || tok[2] != ' ' {
			return nil, fmt.Errorf("unexpected git status entry %q", string(tok))
		}
		x, y := tok[0], tok[1]
		entryPath := filepath.ToSlash(string(tok[3:]))
		cp := ChangedPath{
			Path:      entryPath,
			Untracked: x == '?' && y == '?',
		}
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			if i+1 >= len(tokens) {
				return nil, fmt.Errorf("missing origin path for rename/copy entry %q", entryPath)
			}
			i++
			cp.OldPath = filepath.ToSlash(string(tokens[i]))
		}
		out = append(out, cp)
	}
	return out, nil
}

func validateRepoURLForCLI(repoURL string) (string, error) {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return "", errors.New("repository url is required")
	}
	if strings.HasPrefix(trimmed, "-") {
		return "", fmt.Errorf("repository url %q must not start with '-'", trimmed)
	}
	if strings.IndexFunc(trimmed, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", errors.New("repository url must not contain whitespace or control characters")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("invalid repository url: %w", err)
		}
		if parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return "", errors.New("repository url must not embed a password; use token_ref instead")
			}
		}
	}
	return trimmed, nil
}

func validateBranchForCLI(branch string) (string, error) {
	return NormalizeBranchName(branch)
}

func fullBranchRef(branch string) string {
	return "refs/heads/" + branch
}

func (a *CLIAdapter) runGit(ctx context.Context, workDir string, env []string, secrets []string, args ...string) (string, string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", "", -1, err
	}
	if env == nil {
		env = append(sanitizedGitBaseEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	}
	cmd := exec.CommandContext(ctx, a.gitBin(), args...)
	cmd.Dir = workDir
	cmd.Env = env

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	stdoutStr := redactSecrets(stdoutBuf.String(), secrets)
	stderrStr := redactSecrets(stderrBuf.String(), secrets)
	if runErr == nil {
		return stdoutStr, stderrStr, 0, nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	errMsg := redactSecrets(runErr.Error(), secrets)
	if detail := strings.TrimSpace(stderrStr); detail != "" {
		return stdoutStr, stderrStr, exitCode, fmt.Errorf("git %s: %s (%s)", args[0], errMsg, detail)
	}
	return stdoutStr, stderrStr, exitCode, fmt.Errorf("git %s: %s", args[0], errMsg)
}

func isolatedRepoInitCommands() [][]string {
	return [][]string{
		{"init"},
		{"config", "user.name", "sofa-worker"},
		{"config", "user.email", "sofa-worker@sofa.local"},
		{"config", "commit.gpgsign", "false"},
		{"config", "core.autocrlf", "false"},
	}
}

func (a *CLIAdapter) initIsolatedRepo(ctx context.Context, dir string, env []string, secrets []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create git directory %q: %w", dir, err)
	}
	for _, args := range isolatedRepoInitCommands() {
		if _, _, _, err := a.runGit(ctx, dir, env, secrets, args...); err != nil {
			return err
		}
	}
	return nil
}

func (a *CLIAdapter) lsRemoteBranchSHA(ctx context.Context, repoURL, branch string, env []string, secrets []string) (string, error) {
	fullRef := fullBranchRef(branch)
	stdout, _, _, err := a.runGit(ctx, "", env, secrets, "ls-remote", repoURL, fullRef)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == fullRef {
			sha := strings.ToLower(fields[0])
			if !IsValidCommitSHA(sha) {
				return "", fmt.Errorf("unexpected commit sha %q for ref %q", fields[0], fullRef)
			}
			return sha, nil
		}
	}
	return "", nil
}

// ResolveBranchSHA resolves the remote tip commit SHA for branch using transient auth env vars.
func (a *CLIAdapter) ResolveBranchSHA(ctx context.Context, repoURL, branch string, auth AuthConfig) (string, error) {
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return "", err
	}
	cleanBranch, err := validateBranchForCLI(branch)
	if err != nil {
		return "", err
	}
	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return "", err
	}
	defer cleanup()

	sha, err := a.lsRemoteBranchSHA(ctx, cleanURL, cleanBranch, env, secrets)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "", fmt.Errorf("branch %q not found on repository", cleanBranch)
	}
	return sha, nil
}

// PrepareWorkspace initializes an isolated repository in destDir, fetches fetchBranch without
// adding a persistent remote, verifies pinSHA belongs to fetchBranch, and checks out work at pinSHA.
func (a *CLIAdapter) PrepareWorkspace(ctx context.Context, repoURL, fetchBranch, pinSHA, destDir string, auth AuthConfig) error {
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return err
	}
	cleanBranch, err := validateBranchForCLI(fetchBranch)
	if err != nil {
		return err
	}
	if !IsValidCommitSHA(pinSHA) {
		return fmt.Errorf("%w: %q", ErrInvalidBaseSHA, pinSHA)
	}
	if strings.TrimSpace(destDir) == "" {
		return errors.New("destination workspace directory is required")
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := a.initIsolatedRepo(ctx, destDir, env, secrets); err != nil {
		return err
	}

	refspec := "+" + fullBranchRef(cleanBranch) + ":refs/remotes/sofa/base"
	if _, _, _, err := a.runGit(ctx, destDir, env, secrets, "fetch", "--no-tags", cleanURL, refspec); err != nil {
		return err
	}

	objType, _, _, err := a.runGit(ctx, destDir, env, secrets, "cat-file", "-t", pinSHA)
	if err != nil || strings.TrimSpace(objType) != "commit" {
		return fmt.Errorf("pinned sha %s is not a valid commit in fetched branch %q", pinSHA, cleanBranch)
	}

	if _, _, code, err := a.runGit(ctx, destDir, env, secrets, "merge-base", "--is-ancestor", pinSHA, "refs/remotes/sofa/base"); err != nil {
		if code == 1 {
			return fmt.Errorf("pinned sha %s is not an ancestor of branch %q", pinSHA, cleanBranch)
		}
		return err
	}

	if _, _, _, err := a.runGit(ctx, destDir, env, secrets, "checkout", "-B", "work", pinSHA); err != nil {
		return err
	}
	return nil
}

// HeadSHA resolves and validates the 40-character lowercase hex HEAD commit SHA of workspaceDir.
func (a *CLIAdapter) HeadSHA(ctx context.Context, workspaceDir string) (string, error) {
	if strings.TrimSpace(workspaceDir) == "" {
		return "", errors.New("workspace directory is required")
	}
	stdout, _, _, err := a.runGit(ctx, workspaceDir, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.ToLower(strings.TrimSpace(stdout))
	if !IsValidCommitSHA(sha) {
		return "", fmt.Errorf("unexpected HEAD sha %q", strings.TrimSpace(stdout))
	}
	return sha, nil
}

// StatusPaths inspects workspaceDir using NUL-delimited porcelain status output.
func (a *CLIAdapter) StatusPaths(ctx context.Context, workspaceDir string) ([]ChangedPath, error) {
	if strings.TrimSpace(workspaceDir) == "" {
		return nil, errors.New("workspace directory is required")
	}
	stdout, _, _, err := a.runGit(ctx, workspaceDir, nil, nil, "-c", "core.quotepath=false", "status", "--porcelain=v1", "-z", "-uall")
	if err != nil {
		return nil, err
	}
	return parseStatusPorcelainZ([]byte(stdout))
}

// RevertPaths restores unauthorized tracked paths to HEAD and safely removes unauthorized
// newly staged or untracked files inside workspaceDir while preserving all other changes.
func (a *CLIAdapter) RevertPaths(ctx context.Context, workspaceDir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if strings.TrimSpace(workspaceDir) == "" {
		return errors.New("workspace directory is required")
	}

	seen := make(map[string]struct{}, len(paths))
	cleanPaths := make([]string, 0, len(paths))
	for _, raw := range paths {
		rel, err := validateWorkspaceRelPathForRevert(raw)
		if err != nil {
			return err
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		cleanPaths = append(cleanPaths, rel)
	}
	sort.Strings(cleanPaths)

	var headBacked []string
	var stagedNew []string
	var removeFiles []string

	for _, rel := range cleanPaths {
		_, _, headCode, headErr := a.runGit(ctx, workspaceDir, nil, nil, "cat-file", "-e", "HEAD:"+rel)
		if headErr == nil && headCode == 0 {
			headBacked = append(headBacked, rel)
			continue
		}
		if headCode != 128 && headCode != 1 && headErr != nil {
			// Check whether HEAD itself exists; if git failed for context/exec reasons, propagate.
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		_, _, idxCode, _ := a.runGit(ctx, workspaceDir, nil, nil, "ls-files", "--error-unmatch", "--", rel)
		if idxCode == 0 {
			stagedNew = append(stagedNew, rel)
		}
		removeFiles = append(removeFiles, rel)
	}

	if len(stagedNew) > 0 {
		args := append([]string{"reset", "HEAD", "--"}, stagedNew...)
		if _, _, _, err := a.runGit(ctx, workspaceDir, nil, nil, args...); err != nil {
			return err
		}
	}
	if len(headBacked) > 0 {
		args := append([]string{"checkout", "HEAD", "--"}, headBacked...)
		if _, _, _, err := a.runGit(ctx, workspaceDir, nil, nil, args...); err != nil {
			return err
		}
	}

	if len(removeFiles) > 0 {
		root, err := os.OpenRoot(workspaceDir)
		if err != nil {
			return fmt.Errorf("open workspace root %q: %w", workspaceDir, err)
		}
		defer func() { _ = root.Close() }()

		for _, rel := range removeFiles {
			if err := removePathInRoot(root, rel); err != nil {
				return fmt.Errorf("remove unauthorized path %q: %w", rel, err)
			}
			pruneEmptyParentsInRoot(root, path.Dir(rel))
		}
	}
	return nil
}

func removePathInRoot(root *os.Root, rel string) error {
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if info.Mode()&fs.ModeSymlink == 0 {
				_ = root.Chmod(rel, 0o600)
				if retryErr := root.Remove(rel); retryErr == nil || errors.Is(retryErr, fs.ErrNotExist) {
					return nil
				}
			}
			return err
		}
		return nil
	}

	dirFile, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	names, readErr := dirFile.Readdirnames(-1)
	closeErr := dirFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, name := range names {
		if err := removePathInRoot(root, path.Join(rel, name)); err != nil {
			return err
		}
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = root.Chmod(rel, 0o700)
		if retryErr := root.Remove(rel); retryErr == nil || errors.Is(retryErr, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func validateWorkspaceRelPathForRevert(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("revert path must not be empty")
	}
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("revert path %q must use '/' separators", raw)
	}
	if strings.HasPrefix(raw, "/") || (len(raw) >= 2 && isASCIIAlpha(raw[0]) && raw[1] == ':') {
		return "", fmt.Errorf("revert path %q must be relative", raw)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("revert path %q escapes workspace", raw)
	}
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return "", fmt.Errorf("revert path %q must not target .git", raw)
	}
	return cleaned, nil
}

func pruneEmptyParentsInRoot(root *os.Root, dir string) {
	for dir != "." && dir != "" && dir != ".git" && !strings.HasPrefix(dir, ".git/") {
		if err := root.Remove(dir); err != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

// CommitAllAndPush stages all workspace changes, commits if non-empty against HEAD, and pushes
// HEAD to remoteBranch on repoURL. When Ledger is configured, remoteBranch must already be registered.
func (a *CLIAdapter) CommitAllAndPush(ctx context.Context, workspaceDir, repoURL, remoteBranch, message string, auth AuthConfig) (string, bool, error) {
	if strings.TrimSpace(workspaceDir) == "" {
		return "", false, errors.New("workspace directory is required")
	}
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return "", false, err
	}
	cleanBranch, err := validateBranchForCLI(remoteBranch)
	if err != nil {
		return "", false, err
	}
	if a.Ledger != nil {
		if err := a.Ledger.EnsureRegistered(cleanURL, cleanBranch); err != nil {
			return "", false, err
		}
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return "", false, err
	}
	defer cleanup()

	if _, _, _, err := a.runGit(ctx, workspaceDir, env, secrets, "add", "-A"); err != nil {
		return "", false, err
	}

	_, _, diffCode, diffErr := a.runGit(ctx, workspaceDir, env, secrets, "diff", "--cached", "--quiet")
	committed := false
	switch diffCode {
	case 0:
		// No staged changes against HEAD; push existing HEAD to ensure remote branch exists.
	case 1:
		commitMsg := strings.TrimSpace(message)
		if commitMsg == "" {
			commitMsg = "sofa checkpoint"
		}
		if _, _, _, err := a.runGit(ctx, workspaceDir, env, secrets, "commit", "-m", commitMsg); err != nil {
			return "", false, err
		}
		committed = true
	default:
		return "", false, diffErr
	}

	headOut, _, _, err := a.runGit(ctx, workspaceDir, env, secrets, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	newSHA := strings.TrimSpace(headOut)
	if !IsValidCommitSHA(newSHA) {
		return "", false, fmt.Errorf("unexpected HEAD sha %q", newSHA)
	}

	refspec := "HEAD:" + fullBranchRef(cleanBranch)
	if _, _, _, err := a.runGit(ctx, workspaceDir, env, secrets, "push", cleanURL, refspec); err != nil {
		return "", false, err
	}
	return newSHA, committed, nil
}

// PushCommit pushes commitSHA from workspaceDir to remoteBranch on repoURL.
// When Ledger is configured, remoteBranch must already be registered.
// When expectedOldRemoteSHA is non-empty, --force-with-lease protects against concurrent remote advances.
func (a *CLIAdapter) PushCommit(ctx context.Context, workspaceDir, repoURL, remoteBranch, commitSHA, expectedOldRemoteSHA string, auth AuthConfig) error {
	if strings.TrimSpace(workspaceDir) == "" {
		return errors.New("workspace directory is required")
	}
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return err
	}
	cleanBranch, err := validateBranchForCLI(remoteBranch)
	if err != nil {
		return err
	}
	if !IsValidCommitSHA(commitSHA) {
		return fmt.Errorf("%w: commit_sha %q", ErrInvalidBaseSHA, commitSHA)
	}
	if expectedOldRemoteSHA != "" && !IsValidCommitSHA(expectedOldRemoteSHA) {
		return fmt.Errorf("%w: expected_old_remote_sha %q", ErrInvalidBaseSHA, expectedOldRemoteSHA)
	}
	if a.Ledger != nil {
		if err := a.Ledger.EnsureRegistered(cleanURL, cleanBranch); err != nil {
			return err
		}
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return err
	}
	defer cleanup()

	fullRef := fullBranchRef(cleanBranch)
	refspec := commitSHA + ":" + fullRef
	args := []string{"push"}
	if expectedOldRemoteSHA != "" {
		args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", fullRef, expectedOldRemoteSHA))
	}
	args = append(args, cleanURL, refspec)
	_, _, _, err = a.runGit(ctx, workspaceDir, env, secrets, args...)
	return err
}

// BranchContainsSHA reports whether remoteBranch exists on repoURL, its tip headSHA, and whether
// targetSHA equals or is an ancestor of headSHA. Missing remote branches return ("", false, nil).
func (a *CLIAdapter) BranchContainsSHA(ctx context.Context, repoURL, remoteBranch, targetSHA string, auth AuthConfig) (string, bool, error) {
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return "", false, err
	}
	cleanBranch, err := validateBranchForCLI(remoteBranch)
	if err != nil {
		return "", false, err
	}
	if !IsValidCommitSHA(targetSHA) {
		return "", false, fmt.Errorf("%w: %q", ErrInvalidBaseSHA, targetSHA)
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return "", false, err
	}
	defer cleanup()

	headSHA, err := a.lsRemoteBranchSHA(ctx, cleanURL, cleanBranch, env, secrets)
	if err != nil {
		return "", false, err
	}
	if headSHA == "" {
		return "", false, nil
	}
	if headSHA == targetSHA {
		return headSHA, true, nil
	}

	tmpDir, err := os.MkdirTemp("", "sofa-git-contains-*")
	if err != nil {
		return "", false, fmt.Errorf("create temp verification repo: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := a.initIsolatedRepo(ctx, tmpDir, env, secrets); err != nil {
		return "", false, err
	}

	refspec := "+" + fullBranchRef(cleanBranch) + ":refs/remotes/sofa/check"
	if _, _, _, err := a.runGit(ctx, tmpDir, env, secrets, "fetch", "--no-tags", cleanURL, refspec); err != nil {
		return "", false, err
	}
	fetchedHeadOut, _, _, err := a.runGit(ctx, tmpDir, env, secrets, "rev-parse", "refs/remotes/sofa/check")
	if err != nil {
		return "", false, err
	}
	fetchedHead := strings.TrimSpace(fetchedHeadOut)
	if fetchedHead == targetSHA {
		return fetchedHead, true, nil
	}

	objType, _, catCode, catErr := a.runGit(ctx, tmpDir, env, secrets, "cat-file", "-t", targetSHA)
	if catErr != nil || catCode != 0 || strings.TrimSpace(objType) != "commit" {
		if ctx != nil && ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return fetchedHead, false, nil
	}

	_, _, ancCode, ancErr := a.runGit(ctx, tmpDir, env, secrets, "merge-base", "--is-ancestor", targetSHA, "refs/remotes/sofa/check")
	switch ancCode {
	case 0:
		return fetchedHead, true, nil
	case 1:
		return fetchedHead, false, nil
	default:
		return "", false, ancErr
	}
}

// DeleteRemoteBranch idempotently deletes remoteBranch from repoURL.
// When Ledger is configured, remoteBranch must already be registered.
func (a *CLIAdapter) DeleteRemoteBranch(ctx context.Context, repoURL, remoteBranch string, auth AuthConfig) error {
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return err
	}
	cleanBranch, err := validateBranchForCLI(remoteBranch)
	if err != nil {
		return err
	}
	if a.Ledger != nil {
		if err := a.Ledger.EnsureDeletable(cleanURL, cleanBranch); err != nil {
			return err
		}
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return err
	}
	defer cleanup()

	existingSHA, err := a.lsRemoteBranchSHA(ctx, cleanURL, cleanBranch, env, secrets)
	if err != nil {
		return err
	}
	if existingSHA == "" {
		return nil
	}

	refspec := ":" + fullBranchRef(cleanBranch)
	_, _, _, err = a.runGit(ctx, "", env, secrets, "push", cleanURL, refspec)
	return err
}

// RebaseCandidate fetches targetBranch and candidateBranch into workDir, rebases candidate onto
// targetBranch's tip, and either aborts returning conflict details or leaves workDir at RebasedSHA.
func (a *CLIAdapter) RebaseCandidate(ctx context.Context, repoURL, targetBranch, candidateBranch, workDir string, auth AuthConfig) (RebaseOutcome, error) {
	cleanURL, err := validateRepoURLForCLI(repoURL)
	if err != nil {
		return RebaseOutcome{}, err
	}
	cleanTarget, err := validateBranchForCLI(targetBranch)
	if err != nil {
		return RebaseOutcome{}, err
	}
	cleanCandidate, err := validateBranchForCLI(candidateBranch)
	if err != nil {
		return RebaseOutcome{}, err
	}
	if strings.TrimSpace(workDir) == "" {
		return RebaseOutcome{}, errors.New("rebase work directory is required")
	}

	env, secrets, cleanup, err := a.buildAuthEnv(auth)
	if err != nil {
		return RebaseOutcome{}, err
	}
	defer cleanup()

	if err := a.initIsolatedRepo(ctx, workDir, env, secrets); err != nil {
		return RebaseOutcome{}, err
	}

	targetRefspec := "+" + fullBranchRef(cleanTarget) + ":refs/remotes/sofa/target"
	if _, _, _, err := a.runGit(ctx, workDir, env, secrets, "fetch", "--no-tags", cleanURL, targetRefspec); err != nil {
		return RebaseOutcome{}, err
	}
	targetOut, _, _, err := a.runGit(ctx, workDir, env, secrets, "rev-parse", "refs/remotes/sofa/target")
	if err != nil {
		return RebaseOutcome{}, err
	}
	targetSHA := strings.TrimSpace(targetOut)

	candRefspec := "+" + fullBranchRef(cleanCandidate) + ":refs/remotes/sofa/candidate"
	if _, _, _, err := a.runGit(ctx, workDir, env, secrets, "fetch", "--no-tags", cleanURL, candRefspec); err != nil {
		return RebaseOutcome{}, err
	}
	if _, _, _, err := a.runGit(ctx, workDir, env, secrets, "checkout", "-B", "candidate", "refs/remotes/sofa/candidate"); err != nil {
		return RebaseOutcome{}, err
	}

	rebaseStdout, rebaseStderr, _, rebaseErr := a.runGit(ctx, workDir, env, secrets, "rebase", targetSHA)
	if rebaseErr == nil {
		headOut, _, _, err := a.runGit(ctx, workDir, env, secrets, "rev-parse", "HEAD")
		if err != nil {
			return RebaseOutcome{}, err
		}
		return RebaseOutcome{
			Conflicted: false,
			TargetSHA:  targetSHA,
			RebasedSHA: strings.TrimSpace(headOut),
		}, nil
	}

	conflictOut, _, _, _ := a.runGit(ctx, workDir, env, secrets, "-c", "core.quotepath=false", "diff", "--name-only", "--diff-filter=U", "-z")
	conflictFiles := parseNulSeparatedPaths(conflictOut)
	if len(conflictFiles) > 0 || isRebaseInProgress(workDir) {
		if _, _, _, abortErr := a.runGit(ctx, workDir, env, secrets, "rebase", "--abort"); abortErr != nil {
			return RebaseOutcome{}, fmt.Errorf("rebase conflict abort failed: %w (original: %v)", abortErr, rebaseErr)
		}
		details := strings.TrimSpace(strings.Join(nonEmptyStrings(rebaseStderr, rebaseStdout, rebaseErr.Error()), "\n"))
		return RebaseOutcome{
			Conflicted:    true,
			TargetSHA:     targetSHA,
			ConflictFiles: conflictFiles,
			Details:       details,
		}, nil
	}
	return RebaseOutcome{}, rebaseErr
}

func parseNulSeparatedPaths(raw string) []string {
	if raw == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, seg := range strings.Split(raw, "\x00") {
		if seg == "" {
			continue
		}
		p := filepath.ToSlash(seg)
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func isRebaseInProgress(workDir string) bool {
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(workDir, ".git", sub)); err == nil {
			return true
		}
	}
	return false
}

func nonEmptyStrings(items ...string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			out = append(out, strings.TrimSpace(item))
		}
	}
	return out
}
