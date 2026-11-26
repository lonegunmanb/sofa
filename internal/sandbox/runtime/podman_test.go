package runtime_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeWorkspaceMounts(t *testing.T) {
	t.Parallel()

	t.Run("qc mode mounts single read-only workspace", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		mounts, err := runtime.ComputeWorkspaceMounts(ws, true, []string{"docs/**"})
		require.NoError(t, err)
		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: true},
		}, mounts)
	})

	t.Run("work mode with nil writable_paths mounts rw workspace and trailing ro .git", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		mounts, err := runtime.ComputeWorkspaceMounts(ws, false, nil)
		require.NoError(t, err)
		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: false},
			{Source: filepath.Join(ws, ".git"), Target: "/workspace/.git", ReadOnly: true},
		}, mounts)
	})

	t.Run("work mode with empty slice mounts single ro workspace", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		mounts, err := runtime.ComputeWorkspaceMounts(ws, false, []string{})
		require.NoError(t, err)
		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: true},
		}, mounts)
	})

	t.Run("work mode with multi-segment paths auto-creates missing dirs and deduplicates", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		mounts, err := runtime.ComputeWorkspaceMounts(ws, false, []string{"docs/spec.md", "internal/**/*.go", "internal/other.go"})
		require.NoError(t, err)

		docsInfo, err := os.Stat(filepath.Join(ws, "docs"))
		require.NoError(t, err)
		assert.True(t, docsInfo.IsDir())
		internalInfo, err := os.Stat(filepath.Join(ws, "internal"))
		require.NoError(t, err)
		assert.True(t, internalInfo.IsDir())

		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: true},
			{Source: filepath.Join(ws, "docs"), Target: "/workspace/docs", ReadOnly: false},
			{Source: filepath.Join(ws, "internal"), Target: "/workspace/internal", ReadOnly: false},
			{Source: filepath.Join(ws, ".git"), Target: "/workspace/.git", ReadOnly: true},
		}, mounts)
	})

	t.Run("single-segment literal root file mounts rw when present and falls back when absent", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		goMod := filepath.Join(ws, "go.mod")
		require.NoError(t, os.WriteFile(goMod, []byte("module test\n"), 0o644))

		mounts, err := runtime.ComputeWorkspaceMounts(ws, false, []string{"go.mod", "docs/**"})
		require.NoError(t, err)
		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: true},
			{Source: goMod, Target: "/workspace/go.mod", ReadOnly: false},
			{Source: filepath.Join(ws, "docs"), Target: "/workspace/docs", ReadOnly: false},
			{Source: filepath.Join(ws, ".git"), Target: "/workspace/.git", ReadOnly: true},
		}, mounts)

		// Missing root file falls back to /workspace:rw + /.git:ro.
		mountsFallback, err := runtime.ComputeWorkspaceMounts(ws, false, []string{"new_root_file.txt", "docs/**"})
		require.NoError(t, err)
		assert.Equal(t, []runtime.BindMount{
			{Source: ws, Target: "/workspace", ReadOnly: false},
			{Source: filepath.Join(ws, ".git"), Target: "/workspace/.git", ReadOnly: true},
		}, mountsFallback)
	})

	t.Run("root-level wildcards fall back to rw workspace plus ro .git", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		for _, pat := range []string{"*.md", "*", "**", ".", "v?.txt"} {
			mounts, err := runtime.ComputeWorkspaceMounts(ws, false, []string{pat})
			require.NoError(t, err, pat)
			assert.Equal(t, []runtime.BindMount{
				{Source: ws, Target: "/workspace", ReadOnly: false},
				{Source: filepath.Join(ws, ".git"), Target: "/workspace/.git", ReadOnly: true},
			}, mounts, pat)
		}
	})

	t.Run("rejects .git and escaping writable_paths", func(t *testing.T) {
		t.Parallel()
		ws := newFakeGitDir(t)
		for _, bad := range []string{".git", ".git/**", ".git/HEAD", "../outside/**", "/etc/passwd", `docs\spec.md`, ""} {
			_, err := runtime.ComputeWorkspaceMounts(ws, false, []string{bad})
			require.Error(t, err, "expected error for %q", bad)
		}
	})
}

func TestBuildRunArgsMatrixAndGuards(t *testing.T) {
	t.Parallel()

	ws := newFakeGitDir(t)
	userMountDir := t.TempDir()
	bundleDir := t.TempDir()
	importsDir := t.TempDir()
	artifactsDir := t.TempDir()
	toolsDir := t.TempDir()
	harnessDir := t.TempDir()
	sessionStoreDir := t.TempDir()
	pluginSockDir := t.TempDir()

	cfg := sandboxspec.SandboxConfig{
		Runtime: "podman",
		Image:   "alpine:latest",
		Network: "bridge",
		Mounts: []sandboxspec.MountSpec{
			{Source: userMountDir, Target: "/opt/cache", ReadOnly: true},
		},
		Resources: &sandboxspec.ResourcesSpec{
			CPUs:   "2",
			Memory: "4g",
		},
	}

	args, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:           cfg,
		HostOS:            "windows",
		ContainerName:     "sofa-worker-1",
		Detached:          true,
		PublishPluginPort: true,
		ForAgentSession:   true,
		Entrypoint:        "/.sofa/harness/sofa-harness-copilot",
		WorkingDir:        ".",
		Args:              []string{"--stdio"},
		HostWorkspaceDir:  ws,
		ReadOnlyWorkspace: false,
		WritablePaths:     []string{"docs/spec.md", "internal/**"},
		BundleDir:         bundleDir,
		ImportsDir:        importsDir,
		ArtifactsDir:      artifactsDir,
		ToolsDir:          toolsDir,
		HarnessDir:        harnessDir,
		SessionStoreDir:   sessionStoreDir,
		PluginSocketDir:   pluginSockDir,
	})
	require.NoError(t, err)

	// Verify base flags & zero secret env vars.
	assert.Equal(t, "run", args[0])
	assert.Contains(t, args, "--rm")
	assert.Contains(t, args, "--init")
	assert.Contains(t, args, "-d")

	envVars := collectFlagValues(args, "-e")
	assert.Equal(t, []string{
		"HOME=/tmp/sofa-home",
		"TMPDIR=/tmp",
		"GIT_OPTIONAL_LOCKS=0",
	}, envVars)

	assertFlagPair(t, args, "-w", "/workspace")
	assertFlagPair(t, args, "--network", "bridge")
	assertFlagPair(t, args, "--cpus", "2")
	assertFlagPair(t, args, "--memory", "4g")
	assertFlagPair(t, args, "--name", "sofa-worker-1")
	assertFlagPair(t, args, "-p", "127.0.0.1::19500")
	assertFlagPair(t, args, "--entrypoint", "/.sofa/harness/sofa-harness-copilot")

	mounts := collectFlagValues(args, "-v")
	require.NotEmpty(t, mounts)
	assert.Equal(t, filepath.Join(ws, ".git")+":/workspace/.git:ro", mounts[len(mounts)-1], "/workspace/.git:ro must be final mount")
	assert.Contains(t, mounts, userMountDir+":/opt/cache:ro")
	assert.Contains(t, mounts, bundleDir+":/.sofa/bundle:ro")
	assert.Contains(t, mounts, importsDir+":/.sofa/imports:ro")
	assert.Contains(t, mounts, artifactsDir+":/.sofa/artifacts:rw")
	assert.Contains(t, mounts, toolsDir+":/.sofa/tools:ro")
	assert.Contains(t, mounts, harnessDir+":/.sofa/harness:ro")
	assert.Contains(t, mounts, sessionStoreDir+":/.sofa/session-store:rw")
	assert.Contains(t, mounts, pluginSockDir+":/.sofa/plugin:rw")
	assert.Contains(t, mounts, ws+":/workspace:ro")
	assert.Contains(t, mounts, filepath.Join(ws, "docs")+":/workspace/docs:rw")
	assert.Contains(t, mounts, filepath.Join(ws, "internal")+":/workspace/internal:rw")

	// Image + trailing Args at end.
	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, []string{"alpine:latest", "--stdio"}, args[len(args)-2:])

	// ReadOnlyArtifacts mounts artifacts :ro even when workspace is :rw (gatekeeper mode).
	gkArgs, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:           sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
		HostOS:            "windows",
		ForAgentSession:   false,
		HostWorkspaceDir:  ws,
		ArtifactsDir:      artifactsDir,
		ReadOnlyArtifacts: true,
	})
	require.NoError(t, err)
	assert.Contains(t, collectFlagValues(gkArgs, "-v"), artifactsDir+":/.sofa/artifacts:ro")

	// Cross-platform Agent network guard: Windows & macOS reject network = "none" for Agent sessions.
	for _, hostOS := range []string{"windows", "darwin"} {
		_, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
			Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
			HostOS:           hostOS,
			ForAgentSession:  true,
			HostWorkspaceDir: ws,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "none")
	}

	// Linux Agent session allows network = "none" (UDS over /.sofa/plugin).
	_, err = runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
		HostOS:           "linux",
		ForAgentSession:  true,
		HostWorkspaceDir: ws,
	})
	require.NoError(t, err)

	// HandshakeEnv allows only SOFA_HARNESS_PLUGIN and PLUGIN_* keys.
	hsArgs, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "bridge"},
		HostOS:           "windows",
		ForAgentSession:  true,
		HostWorkspaceDir: ws,
		HandshakeEnv: []string{
			"SOFA_HARNESS_PLUGIN=sofa-harness-v1",
			"PLUGIN_MIN_PORT=19500",
			"PLUGIN_MAX_PORT=19500",
			"PLUGIN_CLIENT_CERT=-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n",
		},
	})
	require.NoError(t, err)
	hsEnv := collectFlagValues(hsArgs, "-e")
	assert.Contains(t, hsEnv, "SOFA_HARNESS_PLUGIN=sofa-harness-v1")
	assert.Contains(t, hsEnv, "PLUGIN_MIN_PORT=19500")
	assert.Contains(t, hsEnv, "PLUGIN_MAX_PORT=19500")

	for _, forbidden := range []string{
		"OPENAI_API_KEY=sk-secret",
		"GITHUB_TOKEN=ghp-secret",
		"SOFA_HARNESS_PLUGIN_EXTRA=nope",
		"NO_EQUALS",
		"=empty-key",
	} {
		_, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
			Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "bridge"},
			HostOS:           "windows",
			ForAgentSession:  true,
			HostWorkspaceDir: ws,
			HandshakeEnv:     []string{forbidden},
		})
		require.Error(t, err, "expected HandshakeEnv %q to be rejected", forbidden)
	}
}

type recordedCmd struct {
	args  []string
	stdin string
}

type scriptedCmdRunner struct {
	recorded []recordedCmd
	handler  func(ctx context.Context, bin string, args []string, stdin io.Reader) (string, string, int, error)
}

func (r *scriptedCmdRunner) Run(ctx context.Context, bin string, args []string, stdin io.Reader) (string, string, int, error) {
	var stdinText string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		stdinText = string(b)
	}
	r.recorded = append(r.recorded, recordedCmd{args: append([]string(nil), args...), stdin: stdinText})
	if r.handler != nil {
		return r.handler(ctx, bin, args, strings.NewReader(stdinText))
	}
	return "", "", 0, nil
}

func TestPodmanEngineDetectPlatformAndExec(t *testing.T) {
	t.Parallel()

	t.Run("detects linux/amd64 and docker fallback x86_64/aarch64", func(t *testing.T) {
		t.Parallel()

		runner := &scriptedCmdRunner{
			handler: func(_ context.Context, _ string, args []string, _ io.Reader) (string, string, int, error) {
				if len(args) >= 3 && args[0] == "info" && args[2] == "{{.Version.OsArch}}" {
					return "linux/arm64\n", "", 0, nil
				}
				return "", "unexpected", 1, errors.New("unexpected")
			},
		}
		eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{Runner: runner})
		p, err := eng.DetectPlatform(t.Context())
		require.NoError(t, err)
		assert.Equal(t, runtime.Platform{OS: "linux", Arch: "arm64"}, p)

		dockerFallback := &scriptedCmdRunner{
			handler: func(_ context.Context, _ string, args []string, _ io.Reader) (string, string, int, error) {
				if len(args) >= 3 && args[0] == "info" {
					switch args[2] {
					case "{{.OSType}}/{{.Architecture}}":
						return "linux/x86_64\n", "", 0, nil
					default:
						return "", "template error", 1, &exec.ExitError{}
					}
				}
				return "", "", 1, errors.New("unexpected")
			},
		}
		eng2 := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{RuntimeBin: "docker", Runner: dockerFallback})
		p2, err := eng2.DetectPlatform(t.Context())
		require.NoError(t, err)
		assert.Equal(t, runtime.Platform{OS: "linux", Arch: "amd64"}, p2)
	})

	t.Run("exec returns non-zero exit code without error and respects context cancellation", func(t *testing.T) {
		t.Parallel()

		runner := &scriptedCmdRunner{
			handler: func(ctx context.Context, _ string, args []string, stdin io.Reader) (string, string, int, error) {
				if ctx.Err() != nil {
					return "partial", "canceled", -1, ctx.Err()
				}
				return "out", "err", 2, nil
			},
		}
		eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{Runner: runner})
		res, err := eng.Exec(t.Context(), "ctr-1", "/workspace/sub", []string{"/bin/sh", "-c", "cat"}, map[string]string{"B": "2", "A": "1"}, strings.NewReader(`{"ok":true}`))
		require.NoError(t, err)
		assert.Equal(t, 2, res.ExitCode)
		assert.Equal(t, "out", res.Stdout)
		assert.Equal(t, "err", res.Stderr)
		require.Len(t, runner.recorded, 1)
		assert.Equal(t, []string{"exec", "-i", "-w", "/workspace/sub", "-e", "A=1", "-e", "B=2", "ctr-1", "/bin/sh", "-c", "cat"}, runner.recorded[0].args)
		assert.Equal(t, `{"ok":true}`, runner.recorded[0].stdin)

		canceledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = eng.Exec(canceledCtx, "ctr-1", "", []string{"echo"}, nil, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestPodmanEngineRunGatekeepersSnapshotAndRestore(t *testing.T) {
	t.Parallel()

	// Initialize a real temporary git repository so StatusPaths & RevertPaths exercise real git semantics.
	ws := t.TempDir()
	runGitCmd(t, ws, "init")
	runGitCmd(t, ws, "config", "user.name", "tester")
	runGitCmd(t, ws, "config", "user.email", "tester@example.invalid")
	runGitCmd(t, ws, "config", "commit.gpgsign", "false")
	runGitCmd(t, ws, "config", "core.autocrlf", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(ws, "allowed"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "clean.txt"), []byte("clean-head\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "deleted.txt"), []byte("to-delete\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "allowed", "worker_edit.txt"), []byte("head-v1\n"), 0o644))
	runGitCmd(t, ws, "add", "-A")
	runGitCmd(t, ws, "commit", "-m", "init")

	// Pre-gatekeeper Worker uncommitted edits:
	// 1. Modified tracked file
	const workerEditContent = "worker-modified-content\n"
	require.NoError(t, os.WriteFile(filepath.Join(ws, "allowed", "worker_edit.txt"), []byte(workerEditContent), 0o644))
	// 2. Untracked worker file
	const workerUntrackedContent = "worker-untracked\n"
	require.NoError(t, os.WriteFile(filepath.Join(ws, "allowed", "new_worker.txt"), []byte(workerUntrackedContent), 0o644))
	// 3. Deleted tracked file
	require.NoError(t, os.Remove(filepath.Join(ws, "deleted.txt")))

	runner := &scriptedCmdRunner{
		handler: func(_ context.Context, _ string, args []string, _ io.Reader) (string, string, int, error) {
			if len(args) > 0 && args[0] == "run" {
				// Simulate a misbehaving gatekeeper mutating workspace during execution:
				// - creates untracked garbage.txt
				// - overwrites allowed/worker_edit.txt
				// - reverts clean.txt? actually mutates clean.txt
				// - deletes allowed/new_worker.txt
				// - recreates deleted.txt
				_ = os.WriteFile(filepath.Join(ws, "garbage.txt"), []byte("junk"), 0o644)
				_ = os.WriteFile(filepath.Join(ws, "allowed", "worker_edit.txt"), []byte("corrupted-by-gk\n"), 0o644)
				_ = os.WriteFile(filepath.Join(ws, "clean.txt"), []byte("dirty-clean\n"), 0o644)
				_ = os.Remove(filepath.Join(ws, "allowed", "new_worker.txt"))
				_ = os.WriteFile(filepath.Join(ws, "deleted.txt"), []byte("zombie"), 0o644)
				return "gk stdout", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{
		Runner:     runner,
		GitAdapter: git.NewCLIAdapter(git.CLIAdapterOptions{}),
	})

	gks := []workerspec.GatekeeperConfig{
		{
			Name:             "lint",
			Command:          []string{"sh", "-c", "echo ok"},
			WorkingDir:       ".",
			Timeout:          "30s",
			ExpectExitCode:   0,
			RequireCleanTree: true,
		},
		{
			Name:             "second-should-not-run",
			Command:          []string{"echo", "never"},
			WorkingDir:       ".",
			Timeout:          "30s",
			ExpectExitCode:   0,
			RequireCleanTree: true,
		},
	}

	failure, err := eng.RunGatekeepers(t.Context(), runtime.GatekeeperRunSpec{
		Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: "alpine:latest", Network: "none"},
		HostWorkspaceDir: ws,
		HostOS:           "windows",
	}, gks)
	require.NoError(t, err)
	require.NotNil(t, failure)
	assert.Equal(t, "lint", failure.Name)
	assert.Len(t, runner.recorded, 1, "subsequent gatekeeper must not execute after failure")
	assert.Equal(t, []string{
		"allowed/new_worker.txt",
		"allowed/worker_edit.txt",
		"clean.txt",
		"deleted.txt",
		"garbage.txt",
	}, failure.DirtyFiles)

	// Verify exact restoration of pre-gatekeeper workspace state!
	_, statGarbage := os.Stat(filepath.Join(ws, "garbage.txt"))
	assert.ErrorIs(t, statGarbage, os.ErrNotExist)
	_, statDeleted := os.Stat(filepath.Join(ws, "deleted.txt"))
	assert.ErrorIs(t, statDeleted, os.ErrNotExist)

	gotEdit, err := os.ReadFile(filepath.Join(ws, "allowed", "worker_edit.txt"))
	require.NoError(t, err)
	assert.Equal(t, workerEditContent, string(gotEdit))

	gotUntracked, err := os.ReadFile(filepath.Join(ws, "allowed", "new_worker.txt"))
	require.NoError(t, err)
	assert.Equal(t, workerUntrackedContent, string(gotUntracked))

	gotClean, err := os.ReadFile(filepath.Join(ws, "clean.txt"))
	require.NoError(t, err)
	assert.Equal(t, "clean-head\n", string(gotClean))
}

func newFakeGitDir(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))
	return ws
}

func collectFlagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func assertFlagPair(t *testing.T, args []string, flag, expected string) {
	t.Helper()
	vals := collectFlagValues(args, flag)
	assert.Contains(t, vals, expected, "expected %s %s in %v", flag, expected, args)
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
}
