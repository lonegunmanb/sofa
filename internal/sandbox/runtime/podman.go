package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/git"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

// CommandRunner abstracts container CLI process execution for deterministic Tier 1 tests.
type CommandRunner interface {
	Run(ctx context.Context, bin string, args []string, stdin io.Reader) (stdout string, stderr string, exitCode int, err error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, bin string, args []string, stdin io.Reader) (string, string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", "", -1, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = stdin
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	if runErr == nil {
		return stdoutStr, stderrStr, 0, nil
	}
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return stdoutStr, stderrStr, exitCode, runErr
}

// PodmanEngineOptions configures a PodmanEngine.
type PodmanEngineOptions struct {
	RuntimeBin string
	GitAdapter git.Adapter
	Runner     CommandRunner
}

// PodmanEngine implements Engine against podman, docker, or nerdctl CLIs.
type PodmanEngine struct {
	RuntimeBin string
	GitAdapter git.Adapter
	runner     CommandRunner
}

// NewPodmanEngine constructs a PodmanEngine with defaults for RuntimeBin ("podman"),
// GitAdapter (CLIAdapter), and Runner (os/exec).
func NewPodmanEngine(opts PodmanEngineOptions) *PodmanEngine {
	bin := strings.TrimSpace(opts.RuntimeBin)
	if bin == "" {
		bin = "podman"
	}
	ga := opts.GitAdapter
	if ga == nil {
		ga = git.NewCLIAdapter(git.CLIAdapterOptions{})
	}
	runner := opts.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &PodmanEngine{
		RuntimeBin: bin,
		GitAdapter: ga,
		runner:     runner,
	}
}

var _ Engine = (*PodmanEngine)(nil)

func (e *PodmanEngine) runtimeBin() string {
	if e != nil && strings.TrimSpace(e.RuntimeBin) != "" {
		return strings.TrimSpace(e.RuntimeBin)
	}
	return "podman"
}

func (e *PodmanEngine) cmdRunner() CommandRunner {
	if e != nil && e.runner != nil {
		return e.runner
	}
	return execCommandRunner{}
}

func (e *PodmanEngine) gitAdapter() git.Adapter {
	if e != nil && e.GitAdapter != nil {
		return e.GitAdapter
	}
	return git.NewCLIAdapter(git.CLIAdapterOptions{})
}

// DetectPlatform queries the container engine and normalizes its target Linux OS/architecture.
func (e *PodmanEngine) DetectPlatform(ctx context.Context) (Platform, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	formats := []string{
		"{{.Version.OsArch}}",
		"{{.Host.OS}}/{{.Host.Arch}}",
		"{{.OSType}}/{{.Architecture}}",
	}
	var errs []string
	for _, tmpl := range formats {
		stdout, stderr, _, err := e.cmdRunner().Run(ctx, e.runtimeBin(), []string{"info", "--format", tmpl}, nil)
		if err != nil {
			if ctx.Err() != nil {
				return Platform{}, ctx.Err()
			}
			errs = append(errs, fmt.Sprintf("%s: %v (%s)", tmpl, err, strings.TrimSpace(stderr)))
			continue
		}
		if p, ok := parseOSArch(stdout); ok {
			if err := p.Validate(); err != nil {
				return Platform{}, err
			}
			return p, nil
		}
		errs = append(errs, fmt.Sprintf("%s: unparseable output %q", tmpl, strings.TrimSpace(stdout)))
	}
	return Platform{}, fmt.Errorf("detect container engine platform via %s: %s", e.runtimeBin(), strings.Join(errs, "; "))
}

func parseOSArch(raw string) (Platform, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return Platform{}, false
	}
	osPart := strings.TrimSpace(parts[0])
	archPart := strings.TrimSpace(parts[1])
	switch archPart {
	case "x86_64":
		archPart = "amd64"
	case "aarch64":
		archPart = "arm64"
	}
	if osPart == "" || archPart == "" {
		return Platform{}, false
	}
	return Platform{OS: osPart, Arch: archPart}, true
}

// Exec runs command inside an already-running container via `<runtimeBin> exec -i`.
// Non-zero command exit codes are returned in ExecResult.ExitCode with err == nil;
// errors are returned only on context cancellation/timeout or CLI spawn failures.
func (e *PodmanEngine) Exec(ctx context.Context, containerName, workDir string, command []string, env map[string]string, stdin io.Reader) (ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(containerName) == "" {
		return ExecResult{ExitCode: -1}, errors.New("container name is required for exec")
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return ExecResult{ExitCode: -1}, errors.New("exec command must not be empty")
	}
	resolvedWorkDir, err := ResolveContainerWorkDir(workDir)
	if err != nil {
		return ExecResult{ExitCode: -1}, err
	}

	args := []string{"exec", "-i", "-w", resolvedWorkDir}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+env[k])
		}
	}
	args = append(args, containerName)
	args = append(args, command...)

	stdout, stderr, exitCode, runErr := e.cmdRunner().Run(ctx, e.runtimeBin(), args, stdin)
	if runErr == nil {
		return ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
	}
	if ctx.Err() != nil {
		return ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) || exitCode >= 0 {
		return ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
	}
	return ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: -1}, fmt.Errorf("container exec %q: %w", containerName, runErr)
}

// StartDetached launches a detached container with a unique name and returns that container name.
func (e *PodmanEngine) StartDetached(ctx context.Context, layout SessionMountLayout) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	layoutCopy := layout
	layoutCopy.Detached = true
	if strings.TrimSpace(layoutCopy.ContainerName) == "" {
		layoutCopy.ContainerName = "sofa-" + uuid.NewString()[:12]
	}
	runArgs, err := BuildRunArgs(layoutCopy)
	if err != nil {
		return "", err
	}
	_, stderr, _, err := e.cmdRunner().Run(ctx, e.runtimeBin(), runArgs, nil)
	if err != nil {
		return "", fmt.Errorf("start detached container %q: %w (%s)", layoutCopy.ContainerName, err, strings.TrimSpace(stderr))
	}
	return layoutCopy.ContainerName, nil
}

// RemoveContainer force-removes a container if it still exists.
func (e *PodmanEngine) RemoveContainer(ctx context.Context, containerName string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(containerName) == "" {
		return nil
	}
	_, stderr, _, err := e.cmdRunner().Run(ctx, e.runtimeBin(), []string{"rm", "-f", "-t", "2", containerName}, nil)
	if err != nil {
		return fmt.Errorf("remove container %q: %w (%s)", containerName, err, strings.TrimSpace(stderr))
	}
	return nil
}

// Port queries the published host address (e.g. "127.0.0.1:49152") for containerPort.
func (e *PodmanEngine) Port(ctx context.Context, containerName string, containerPort int) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stdout, stderr, _, err := e.cmdRunner().Run(ctx, e.runtimeBin(), []string{"port", containerName, fmt.Sprintf("%d", containerPort)}, nil)
	if err != nil {
		return "", fmt.Errorf("inspect port %d on container %q: %w (%s)", containerPort, containerName, err, strings.TrimSpace(stderr))
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(stdout), "\n")[0])
	if line == "" {
		return "", fmt.Errorf("container %q returned empty port mapping for %d", containerName, containerPort)
	}
	return line, nil
}

// RunGatekeepers sequentially executes each gatekeeper in an isolated short-lived container.
// When RequireCleanTree is true, it snapshots pre-gatekeeper workspace state (preserving
// pre-existing uncommitted Worker edits) and rejects + restores any additions, deletions,
// or mutations introduced by the gatekeeper.
func (e *PodmanEngine) RunGatekeepers(ctx context.Context, spec GatekeeperRunSpec, gatekeepers []workerspec.GatekeeperConfig) (*GatekeeperFailure, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(gatekeepers) == 0 {
		return nil, nil
	}
	if err := spec.Sandbox.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.HostWorkspaceDir) == "" {
		return nil, errors.New("gatekeeper host workspace directory is required")
	}

	ga := e.gitAdapter()
	for _, gk := range gatekeepers {
		if len(gk.Command) == 0 || strings.TrimSpace(gk.Command[0]) == "" {
			return nil, fmt.Errorf("gatekeeper %q command must not be empty", gk.Name)
		}
		timeout, err := time.ParseDuration(gk.Timeout)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("gatekeeper %q timeout %q must be a positive duration", gk.Name, gk.Timeout)
		}

		cmd := RelocateBundleCommandArgs(gk.Command, spec.BundleDir, spec.HostWorkspaceDir)
		containerName := "sofa-gk-" + uuid.NewString()[:12]
		runArgs, err := BuildRunArgs(SessionMountLayout{
			Sandbox:           spec.Sandbox,
			HostOS:            spec.HostOS,
			ContainerName:     containerName,
			ForAgentSession:   false,
			Entrypoint:        cmd[0],
			WorkingDir:        gk.WorkingDir,
			Args:              cmd[1:],
			HostWorkspaceDir:  spec.HostWorkspaceDir,
			ReadOnlyWorkspace: false,
			WritablePaths:     nil,
			BundleDir:         spec.BundleDir,
			ImportsDir:        spec.ImportsDir,
			ArtifactsDir:      spec.ArtifactsDir,
			ReadOnlyArtifacts: true,
		})
		if err != nil {
			return nil, err
		}

		var snap *workspaceSnapshot
		if gk.RequireCleanTree {
			snap, err = captureWorkspaceSnapshot(ctx, ga, spec.HostWorkspaceDir)
			if err != nil {
				return nil, fmt.Errorf("snapshot pre-gatekeeper workspace for %q: %w", gk.Name, err)
			}
		}

		gkCtx, cancel := context.WithTimeout(ctx, timeout)
		stdout, stderr, exitCode, runErr := e.cmdRunner().Run(gkCtx, e.runtimeBin(), runArgs, nil)
		timedOut := errors.Is(gkCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()

		if timedOut || (runErr != nil && exitCode < 0) {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = e.RemoveContainer(cleanupCtx, containerName)
			cleanupCancel()
		}

		if ctx.Err() != nil {
			if snap != nil {
				_, _ = snap.reconcileAndRestore(context.Background(), ga)
				snap.cleanup()
			}
			return nil, ctx.Err()
		}

		if runErr != nil && !timedOut {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) && exitCode < 0 {
				if snap != nil {
					_, _ = snap.reconcileAndRestore(ctx, ga)
					snap.cleanup()
				}
				return nil, fmt.Errorf("run gatekeeper %q: %w", gk.Name, runErr)
			}
		}

		var dirtyFiles []string
		if snap != nil {
			var snapErr error
			dirtyFiles, snapErr = snap.reconcileAndRestore(ctx, ga)
			snap.cleanup()
			if snapErr != nil {
				return nil, fmt.Errorf("verify/restore clean worktree after gatekeeper %q: %w", gk.Name, snapErr)
			}
		}

		if timedOut || exitCode != gk.ExpectExitCode || len(dirtyFiles) > 0 {
			return &GatekeeperFailure{
				Name:             gk.Name,
				Command:          slices.Clone(cmd),
				ExitCode:         exitCode,
				ExpectedExitCode: gk.ExpectExitCode,
				TimedOut:         timedOut,
				DirtyFiles:       dirtyFiles,
				Stdout:           stdout,
				Stderr:           stderr,
			}, nil
		}
	}

	return nil, nil
}

type capturedEntry struct {
	exists     bool
	isRegular  bool
	mode       os.FileMode
	sha256Hex  string
	backupFile string
}

type workspaceSnapshot struct {
	workspaceDir string
	backupDir    string
	entries      map[string]capturedEntry
}

func captureWorkspaceSnapshot(ctx context.Context, ga git.Adapter, workspaceDir string) (*workspaceSnapshot, error) {
	changed, err := ga.StatusPaths(ctx, workspaceDir)
	if err != nil {
		return nil, err
	}

	backupDir, err := os.MkdirTemp("", "sofa-gk-snap-*")
	if err != nil {
		return nil, fmt.Errorf("create gatekeeper snapshot directory: %w", err)
	}

	snap := &workspaceSnapshot{
		workspaceDir: workspaceDir,
		backupDir:    backupDir,
		entries:      make(map[string]capturedEntry),
	}

	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		snap.cleanup()
		return nil, fmt.Errorf("open workspace root %q: %w", workspaceDir, err)
	}
	defer func() { _ = root.Close() }()

	capturePath := func(rel string) error {
		cleanRel := normalizeSlashPath(rel)
		if cleanRel == "" || cleanRel == ".git" || strings.HasPrefix(cleanRel, ".git/") {
			return nil
		}
		if _, seen := snap.entries[cleanRel]; seen {
			return nil
		}

		info, lerr := root.Lstat(cleanRel)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				snap.entries[cleanRel] = capturedEntry{exists: false}
				return nil
			}
			return fmt.Errorf("stat pre-gatekeeper path %q: %w", cleanRel, lerr)
		}

		entry := capturedEntry{
			exists:    true,
			isRegular: info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular(),
			mode:      info.Mode().Perm(),
		}
		if entry.isRegular {
			digest, backupPath, copyErr := backupRootFile(root, cleanRel, backupDir)
			if copyErr != nil {
				return copyErr
			}
			entry.sha256Hex = digest
			entry.backupFile = backupPath
		}
		snap.entries[cleanRel] = entry
		return nil
	}

	for _, c := range changed {
		if err := capturePath(c.Path); err != nil {
			snap.cleanup()
			return nil, err
		}
		if c.OldPath != "" {
			if err := capturePath(c.OldPath); err != nil {
				snap.cleanup()
				return nil, err
			}
		}
	}
	return snap, nil
}

func (s *workspaceSnapshot) cleanup() {
	if s != nil && s.backupDir != "" {
		_ = os.RemoveAll(s.backupDir)
	}
}

func (s *workspaceSnapshot) reconcileAndRestore(ctx context.Context, ga git.Adapter) ([]string, error) {
	postChanged, err := ga.StatusPaths(ctx, s.workspaceDir)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(s.workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %q: %w", s.workspaceDir, err)
	}
	defer func() { _ = root.Close() }()

	dirtySet := make(map[string]struct{})
	var newDirty []string
	var preRestore []string

	// 1. Check all pre-existing dirty/untracked/deleted paths against post-gatekeeper disk state.
	for rel, pre := range s.entries {
		changed, err := hasPathStateChanged(root, rel, pre)
		if err != nil {
			return nil, err
		}
		if changed {
			dirtySet[rel] = struct{}{}
			preRestore = append(preRestore, rel)
		}
	}

	// 2. Check any newly dirty paths reported by git status that were not in the pre-snapshot.
	for _, c := range postChanged {
		for _, candidate := range []string{c.Path, c.OldPath} {
			rel := normalizeSlashPath(candidate)
			if rel == "" || rel == ".git" || strings.HasPrefix(rel, ".git/") {
				continue
			}
			if _, existed := s.entries[rel]; existed {
				continue
			}
			if _, seen := dirtySet[rel]; seen {
				continue
			}
			dirtySet[rel] = struct{}{}
			newDirty = append(newDirty, rel)
		}
	}

	if len(dirtySet) == 0 {
		return nil, nil
	}

	// Close root before invoking git CLI RevertPaths, then reopen if pre-existing entries need byte restore.
	_ = root.Close()

	if len(newDirty) > 0 {
		sort.Strings(newDirty)
		if err := ga.RevertPaths(ctx, s.workspaceDir, newDirty); err != nil {
			return nil, err
		}
	}

	if len(preRestore) > 0 {
		sort.Strings(preRestore)
		restoreRoot, err := os.OpenRoot(s.workspaceDir)
		if err != nil {
			return nil, fmt.Errorf("reopen workspace root %q: %w", s.workspaceDir, err)
		}
		defer func() { _ = restoreRoot.Close() }()

		for _, rel := range preRestore {
			if err := restoreCapturedEntry(restoreRoot, rel, s.entries[rel]); err != nil {
				return nil, err
			}
		}
	}

	dirty := make([]string, 0, len(dirtySet))
	for rel := range dirtySet {
		dirty = append(dirty, rel)
	}
	sort.Strings(dirty)
	return dirty, nil
}

func hasPathStateChanged(root *os.Root, rel string, pre capturedEntry) (bool, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return pre.exists, nil
		}
		return false, fmt.Errorf("inspect post-gatekeeper path %q: %w", rel, err)
	}
	if !pre.exists {
		return true, nil
	}
	isReg := info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
	if isReg != pre.isRegular {
		return true, nil
	}
	if !pre.isRegular {
		return false, nil
	}
	digest, err := sha256RootFile(root, rel)
	if err != nil {
		return false, err
	}
	return digest != pre.sha256Hex, nil
}

func restoreCapturedEntry(root *os.Root, rel string, pre capturedEntry) error {
	if !pre.exists {
		if err := removePathInRoot(root, rel); err != nil {
			return fmt.Errorf("remove gatekeeper-recreated path %q: %w", rel, err)
		}
		pruneEmptyParentsInRoot(root, path.Dir(rel))
		return nil
	}
	if !pre.isRegular {
		return nil
	}

	if err := removePathInRoot(root, rel); err != nil {
		return fmt.Errorf("clear mutated path %q before restore: %w", rel, err)
	}
	if parent := path.Dir(rel); parent != "." && parent != "" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("recreate parent directory %q: %w", parent, err)
		}
	}

	src, err := os.Open(pre.backupFile)
	if err != nil {
		return fmt.Errorf("open snapshot backup for %q: %w", rel, err)
	}
	defer func() { _ = src.Close() }()

	mode := pre.mode
	if mode == 0 {
		mode = 0o644
	}
	dst, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("recreate pre-gatekeeper file %q: %w", rel, err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("restore pre-gatekeeper file %q: %w", rel, err)
	}
	_ = root.Chmod(rel, mode)
	return nil
}

func backupRootFile(root *os.Root, rel, backupDir string) (string, string, error) {
	src, err := root.Open(rel)
	if err != nil {
		return "", "", fmt.Errorf("open pre-gatekeeper file %q: %w", rel, err)
	}
	defer func() { _ = src.Close() }()

	dstFile, err := os.CreateTemp(backupDir, "entry-*")
	if err != nil {
		return "", "", fmt.Errorf("create snapshot backup file: %w", err)
	}
	backupPath := dstFile.Name()

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(dstFile, h), src)
	closeErr := dstFile.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(backupPath)
		return "", "", fmt.Errorf("backup pre-gatekeeper file %q: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), backupPath, nil
}

func sha256RootFile(root *os.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", fmt.Errorf("open post-gatekeeper file %q: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash post-gatekeeper file %q: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
		return err
	}
	return nil
}

func pruneEmptyParentsInRoot(root *os.Root, dir string) {
	for dir != "." && dir != "" && dir != ".git" && !strings.HasPrefix(dir, ".git/") {
		if err := root.Remove(dir); err != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

func normalizeSlashPath(raw string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if cleaned == "" {
		return ""
	}
	cleaned = path.Clean(cleaned)
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." {
		return ""
	}
	return cleaned
}
