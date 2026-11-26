package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lonegunmanb/sofa/internal/harness/installer"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const (
	// DefaultContainerWorkingDir is the container workspace mount and default working directory.
	DefaultContainerWorkingDir = "/workspace"
	// ContainerPluginPort is the fixed TCP port listened on inside containers on non-Linux hosts.
	ContainerPluginPort = 19500
)

// Platform identifies the container engine target Linux OS/architecture pair.
type Platform = installer.Platform

// BindMount describes a host-to-container bind mount specification.
type BindMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// FlagValue formats the mount for `-v` / `--volume`.
func (m BindMount) FlagValue() string {
	mode := "rw"
	if m.ReadOnly {
		mode = "ro"
	}
	return m.Source + ":" + m.Target + ":" + mode
}

// SessionMountLayout describes all non-secret container runtime flags and bind mounts.
// By design it exposes no arbitrary environment map so API keys, MCP headers, and Git tokens
// can never be injected via container environment variables.
type SessionMountLayout struct {
	Sandbox           sandboxspec.SandboxConfig
	HostOS            string
	ContainerName     string
	Detached          bool
	PublishPluginPort bool
	ForAgentSession   bool
	Entrypoint        string
	WorkingDir        string
	Args              []string
	HostWorkspaceDir  string
	ReadOnlyWorkspace bool
	WritablePaths     []string
	BundleDir         string
	ImportsDir        string
	ArtifactsDir      string
	ReadOnlyArtifacts bool
	ToolsDir          string
	HarnessDir        string
	SessionStoreDir   string
	PluginSocketDir   string
	HandshakeEnv      []string
}

// ExecResult captures the stdout, stderr, and exit code of a container exec invocation.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// GatekeeperRunSpec configures the container environment and mounts for RunGatekeepers.
type GatekeeperRunSpec struct {
	Sandbox          sandboxspec.SandboxConfig
	HostWorkspaceDir string
	BundleDir        string
	ImportsDir       string
	ArtifactsDir     string
	HostOS           string
}

// GatekeeperFailure captures deterministic evidence when a gatekeeper command fails, times out,
// or leaves workspace modifications under require_clean_worktree = true.
type GatekeeperFailure struct {
	Name             string
	Command          []string
	ExitCode         int
	ExpectedExitCode int
	TimedOut         bool
	DirtyFiles       []string
	Stdout           string
	Stderr           string
}

// Engine abstracts container platform detection, in-container exec, and deterministic gatekeepers.
type Engine interface {
	DetectPlatform(ctx context.Context) (Platform, error)
	Exec(ctx context.Context, containerName, workDir string, command []string, env map[string]string, stdin io.Reader) (ExecResult, error)
	RunGatekeepers(ctx context.Context, spec GatekeeperRunSpec, gatekeepers []workerspec.GatekeeperConfig) (*GatekeeperFailure, error)
}

// ComputeWorkspaceMounts calculates the /workspace and /workspace/.git bind mounts according to
// Stage 2 isolation rules:
//   - ReadOnlyWorkspace == true: single /workspace:ro mount.
//   - WritablePaths == nil: /workspace:rw + /workspace/.git:ro (if .git exists).
//   - len(WritablePaths) == 0: single /workspace:ro mount (forbids all writes).
//   - Non-empty WritablePaths: mounts /workspace:ro plus deduplicated :rw submounts for literal
//     top-level directories (auto-creating missing directories safely) and existing root files;
//     falls back to /workspace:rw + /workspace/.git:ro for root-level wildcards or missing root files.
func ComputeWorkspaceMounts(hostWorkspace string, readOnlyWorkspace bool, writablePaths []string) ([]BindMount, error) {
	if strings.TrimSpace(hostWorkspace) == "" {
		return nil, errors.New("host workspace directory is required")
	}
	absWorkspace, err := filepath.Abs(hostWorkspace)
	if err != nil {
		return nil, fmt.Errorf("resolve host workspace %q: %w", hostWorkspace, err)
	}
	absWorkspace = filepath.Clean(absWorkspace)

	normalizedGlobs, err := validateAndNormalizeWritablePaths(writablePaths)
	if err != nil {
		return nil, err
	}

	if readOnlyWorkspace {
		return []BindMount{{
			Source:   absWorkspace,
			Target:   DefaultContainerWorkingDir,
			ReadOnly: true,
		}}, nil
	}

	gitPath := filepath.Join(absWorkspace, ".git")
	gitExists := false
	if _, err := os.Lstat(gitPath); err == nil {
		gitExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect workspace .git %q: %w", gitPath, err)
	}

	rwFullWithGitRO := func() []BindMount {
		mounts := []BindMount{{
			Source:   absWorkspace,
			Target:   DefaultContainerWorkingDir,
			ReadOnly: false,
		}}
		if gitExists {
			mounts = append(mounts, BindMount{
				Source:   gitPath,
				Target:   DefaultContainerWorkingDir + "/.git",
				ReadOnly: true,
			})
		}
		return mounts
	}

	if writablePaths == nil {
		return rwFullWithGitRO(), nil
	}
	if len(normalizedGlobs) == 0 {
		return []BindMount{{
			Source:   absWorkspace,
			Target:   DefaultContainerWorkingDir,
			ReadOnly: true,
		}}, nil
	}

	root, err := os.OpenRoot(absWorkspace)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %q: %w", absWorkspace, err)
	}
	defer func() { _ = root.Close() }()

	seenTargets := make(map[string]struct{})
	var subMounts []BindMount
	addSubMount := func(hostRel, containerTarget string) {
		if _, dup := seenTargets[containerTarget]; dup {
			return
		}
		seenTargets[containerTarget] = struct{}{}
		subMounts = append(subMounts, BindMount{
			Source:   filepath.Join(absWorkspace, filepath.FromSlash(hostRel)),
			Target:   containerTarget,
			ReadOnly: false,
		})
	}

	for _, pat := range normalizedGlobs {
		segs := strings.Split(pat, "/")
		first := segs[0]
		if first == "." || hasGlobMeta(first) {
			return rwFullWithGitRO(), nil
		}

		if len(segs) == 1 {
			info, err := root.Lstat(first)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return rwFullWithGitRO(), nil
				}
				return nil, fmt.Errorf("inspect writable root path %q: %w", first, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("writable_paths entry %q must not be a symlink", first)
			}
			addSubMount(first, DefaultContainerWorkingDir+"/"+first)
			continue
		}

		info, err := root.Lstat(first)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("inspect writable directory %q: %w", first, err)
			}
			if mkErr := root.MkdirAll(first, 0o755); mkErr != nil {
				return nil, fmt.Errorf("create writable directory %q: %w", first, mkErr)
			}
		} else {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("writable_paths directory %q must not be a symlink", first)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("writable_paths prefix %q exists as a non-directory", first)
			}
		}
		addSubMount(first, DefaultContainerWorkingDir+"/"+first)
	}

	mounts := make([]BindMount, 0, 2+len(subMounts))
	mounts = append(mounts, BindMount{
		Source:   absWorkspace,
		Target:   DefaultContainerWorkingDir,
		ReadOnly: true,
	})
	mounts = append(mounts, subMounts...)
	if gitExists {
		mounts = append(mounts, BindMount{
			Source:   gitPath,
			Target:   DefaultContainerWorkingDir + "/.git",
			ReadOnly: true,
		})
	}
	return mounts, nil
}

// BuildRunArgs builds the container CLI `run` argument list while enforcing the zero-secret-env
// invariant, cross-platform Agent network guard, and trailing /workspace/.git:ro mount protection.
func BuildRunArgs(layout SessionMountLayout) ([]string, error) {
	if err := layout.Sandbox.Validate(); err != nil {
		return nil, err
	}

	hostOS := strings.TrimSpace(layout.HostOS)
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if layout.ForAgentSession && hostOS != "linux" && layout.Sandbox.Network == "none" {
		return nil, fmt.Errorf(
			"agent session on host OS %q cannot use sandbox network = \"none\" (non-Linux hosts require 127.0.0.1 loopback port forwarding to reach harness plugins)",
			hostOS,
		)
	}

	workDir, err := ResolveContainerWorkDir(layout.WorkingDir)
	if err != nil {
		return nil, err
	}

	workspaceMounts, err := ComputeWorkspaceMounts(layout.HostWorkspaceDir, layout.ReadOnlyWorkspace, layout.WritablePaths)
	if err != nil {
		return nil, err
	}

	args := []string{
		"run",
		"--rm",
		"--init",
		"-w", workDir,
		"--network", layout.Sandbox.Network,
		"-e", "HOME=/tmp/sofa-home",
		"-e", "TMPDIR=/tmp",
		"-e", "GIT_OPTIONAL_LOCKS=0",
	}

	for _, entry := range layout.HandshakeEnv {
		if err := validateHandshakeEnvEntry(entry); err != nil {
			return nil, err
		}
		args = append(args, "-e", entry)
	}

	if layout.Sandbox.Resources != nil {
		if layout.Sandbox.Resources.CPUs != "" {
			args = append(args, "--cpus", layout.Sandbox.Resources.CPUs)
		}
		if layout.Sandbox.Resources.Memory != "" {
			args = append(args, "--memory", layout.Sandbox.Resources.Memory)
		}
	}

	if name := strings.TrimSpace(layout.ContainerName); name != "" {
		args = append(args, "--name", name)
	}
	if layout.Detached {
		args = append(args, "-d")
	}
	if layout.PublishPluginPort {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", ContainerPluginPort))
	}
	if ep := strings.TrimSpace(layout.Entrypoint); ep != "" {
		args = append(args, "--entrypoint", ep)
	}

	for _, m := range layout.Sandbox.Mounts {
		absSource, err := filepath.Abs(m.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve sandbox mount source %q: %w", m.Source, err)
		}
		args = append(args, "-v", BindMount{
			Source:   filepath.Clean(absSource),
			Target:   path.Clean(m.Target),
			ReadOnly: true,
		}.FlagValue())
	}

	appendSystemMount := func(hostDir, target string, readOnly bool) error {
		trimmed := strings.TrimSpace(hostDir)
		if trimmed == "" {
			return nil
		}
		absDir, err := filepath.Abs(trimmed)
		if err != nil {
			return fmt.Errorf("resolve mount %q for %s: %w", trimmed, target, err)
		}
		args = append(args, "-v", BindMount{
			Source:   filepath.Clean(absDir),
			Target:   target,
			ReadOnly: readOnly,
		}.FlagValue())
		return nil
	}

	artifactsRO := layout.ReadOnlyWorkspace || layout.ReadOnlyArtifacts
	systemMounts := []struct {
		hostDir  string
		target   string
		readOnly bool
	}{
		{layout.BundleDir, "/.sofa/bundle", true},
		{layout.ImportsDir, "/.sofa/imports", true},
		{layout.ArtifactsDir, "/.sofa/artifacts", artifactsRO},
		{layout.ToolsDir, "/.sofa/tools", true},
		{layout.HarnessDir, "/.sofa/harness", true},
		{layout.SessionStoreDir, "/.sofa/session-store", false},
		{layout.PluginSocketDir, "/.sofa/plugin", false},
	}
	for _, sm := range systemMounts {
		if err := appendSystemMount(sm.hostDir, sm.target, sm.readOnly); err != nil {
			return nil, err
		}
	}

	// Workspace mounts go last so /workspace/.git:ro is always the final protective mount.
	for _, wm := range workspaceMounts {
		args = append(args, "-v", wm.FlagValue())
	}

	args = append(args, layout.Sandbox.Image)
	args = append(args, layout.Args...)
	return args, nil
}

// ResolveContainerWorkDir converts a workspace-relative path (or "" / ".") into a clean
// container path anchored under /workspace.
func ResolveContainerWorkDir(relOrAbs string) (string, error) {
	trimmed := strings.TrimSpace(relOrAbs)
	if trimmed == "" || trimmed == "." {
		return DefaultContainerWorkingDir, nil
	}
	if strings.Contains(trimmed, `\`) {
		return "", fmt.Errorf("container working_dir %q must use '/' separators", relOrAbs)
	}
	if strings.HasPrefix(trimmed, "/") {
		cleaned := path.Clean(trimmed)
		if cleaned != DefaultContainerWorkingDir && !strings.HasPrefix(cleaned, DefaultContainerWorkingDir+"/") {
			return "", fmt.Errorf("container working_dir %q must remain under %s", relOrAbs, DefaultContainerWorkingDir)
		}
		return cleaned, nil
	}
	if len(trimmed) >= 2 && isASCIIAlpha(trimmed[0]) && trimmed[1] == ':' {
		return "", fmt.Errorf("container working_dir %q must not contain a drive prefix", relOrAbs)
	}
	cleaned := path.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("container working_dir %q escapes workspace", relOrAbs)
	}
	if cleaned == "." {
		return DefaultContainerWorkingDir, nil
	}
	return path.Join(DefaultContainerWorkingDir, cleaned), nil
}

func validateAndNormalizeWritablePaths(writablePaths []string) ([]string, error) {
	if writablePaths == nil {
		return nil, nil
	}
	out := make([]string, 0, len(writablePaths))
	for _, raw := range writablePaths {
		if strings.TrimSpace(raw) == "" {
			return nil, errors.New("writable_paths entry must not be empty")
		}
		if strings.Contains(raw, `\`) {
			return nil, fmt.Errorf("writable_paths entry %q must use '/' separators", raw)
		}
		if strings.HasPrefix(raw, "/") || (len(raw) >= 2 && isASCIIAlpha(raw[0]) && raw[1] == ':') {
			return nil, fmt.Errorf("writable_paths entry %q must be relative to workspace", raw)
		}
		trimmed := strings.TrimSpace(raw)
		if strings.HasSuffix(trimmed, "/") {
			trimmed += "**"
		}
		cleaned := path.Clean(trimmed)
		cleaned = strings.TrimPrefix(cleaned, "./")
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, fmt.Errorf("writable_paths entry %q escapes workspace", raw)
		}
		if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
			return nil, fmt.Errorf("writable_paths entry %q must not target .git", raw)
		}
		out = append(out, cleaned)
	}
	return out, nil
}

func hasGlobMeta(seg string) bool {
	return strings.ContainsAny(seg, "*?[")
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func validateHandshakeEnvEntry(entry string) error {
	key, _, ok := strings.Cut(entry, "=")
	if !ok || key == "" {
		return fmt.Errorf("handshake env entry %q must be in KEY=VALUE format", entry)
	}
	if key != "SOFA_HARNESS_PLUGIN" && !strings.HasPrefix(key, "PLUGIN_") {
		return fmt.Errorf("handshake env key %q is forbidden: only SOFA_HARNESS_PLUGIN and PLUGIN_* are permitted", key)
	}
	return nil
}
