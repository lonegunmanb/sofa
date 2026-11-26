//go:build integration

package runtime_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const integrationAlpineImage = "alpine:latest"

func TestPodmanIntegrationDetectPlatform(t *testing.T) {
	eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "linux", platform.OS)
	assert.Contains(t, []string{"amd64", "arm64"}, platform.Arch)
	require.NoError(t, platform.Validate())
}

func TestPodmanIntegrationWritablePathsMountIsolation(t *testing.T) {
	ws := initIntegrationGitRepo(t)

	// 1. Writing inside /workspace/allowed/ok.txt succeeds on host.
	okArgs, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:           sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationAlpineImage, Network: "none"},
		HostOS:            "linux",
		ForAgentSession:   false,
		Entrypoint:        "sh",
		Args:              []string{"-c", "printf 'allowed-ok' > /workspace/allowed/ok.txt"},
		HostWorkspaceDir:  ws,
		ReadOnlyWorkspace: false,
		WritablePaths:     []string{"allowed/**"},
	})
	require.NoError(t, err)
	out, runErr := runPodmanCLI(t.Context(), okArgs...)
	require.NoError(t, runErr, "expected allowed write to succeed, output: %s", out)

	gotOK, err := os.ReadFile(filepath.Join(ws, "allowed", "ok.txt"))
	require.NoError(t, err)
	assert.Equal(t, "allowed-ok", string(gotOK))

	// 2. Writing /workspace/forbidden.txt fails with Read-only file system.
	forbiddenArgs, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:           sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationAlpineImage, Network: "none"},
		HostOS:            "linux",
		ForAgentSession:   false,
		Entrypoint:        "sh",
		Args:              []string{"-c", "echo bad > /workspace/forbidden.txt"},
		HostWorkspaceDir:  ws,
		ReadOnlyWorkspace: false,
		WritablePaths:     []string{"allowed/**"},
	})
	require.NoError(t, err)
	out, runErr = runPodmanCLI(t.Context(), forbiddenArgs...)
	require.Error(t, runErr)
	assert.Contains(t, out, "Read-only file system")
	_, statErr := os.Stat(filepath.Join(ws, "forbidden.txt"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	// 3. Writing /workspace/.git/HEAD fails with Read-only file system even when workspace is unrestricted.
	gitWriteArgs, err := runtime.BuildRunArgs(runtime.SessionMountLayout{
		Sandbox:           sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationAlpineImage, Network: "none"},
		HostOS:            "linux",
		ForAgentSession:   false,
		Entrypoint:        "sh",
		Args:              []string{"-c", "echo hijack > /workspace/.git/HEAD"},
		HostWorkspaceDir:  ws,
		ReadOnlyWorkspace: false,
		WritablePaths:     nil,
	})
	require.NoError(t, err)
	out, runErr = runPodmanCLI(t.Context(), gitWriteArgs...)
	require.Error(t, runErr)
	assert.Contains(t, out, "Read-only file system")
	headBytes, err := os.ReadFile(filepath.Join(ws, ".git", "HEAD"))
	require.NoError(t, err)
	assert.NotContains(t, string(headBytes), "hijack")
}

func TestPodmanIntegrationDetachedExecStdinAndExitCodes(t *testing.T) {
	ws := initIntegrationGitRepo(t)
	eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{})

	cid, err := eng.StartDetached(t.Context(), runtime.SessionMountLayout{
		Sandbox:          sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationAlpineImage, Network: "none"},
		HostOS:           "linux",
		Entrypoint:       "sleep",
		Args:             []string{"30"},
		HostWorkspaceDir: ws,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = eng.RemoveContainer(cleanupCtx, cid)
	})

	// 1. Exec with JSON via stdin, env var, stdout & stderr.
	res, err := eng.Exec(
		t.Context(),
		cid,
		".",
		[]string{"sh", "-c", `cat; printf '|env=%s' "$TEST_VAR"; echo err-msg >&2`},
		map[string]string{"TEST_VAR": "ok"},
		strings.NewReader(`{"hello":"sofa"}`),
	)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, `{"hello":"sofa"}|env=ok`, res.Stdout)
	assert.Contains(t, res.Stderr, "err-msg")

	// 2. Non-zero exit code returned in ExecResult with err == nil.
	resFail, err := eng.Exec(
		t.Context(),
		cid,
		".",
		[]string{"sh", "-c", "echo fail-out; echo fail-err >&2; exit 42"},
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 42, resFail.ExitCode)
	assert.Contains(t, resFail.Stdout, "fail-out")
	assert.Contains(t, resFail.Stderr, "fail-err")

	// 3. Timeout returns non-nil context error.
	timeoutCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err = eng.Exec(timeoutCtx, cid, ".", []string{"sleep", "10"}, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPodmanIntegrationRunGatekeepers(t *testing.T) {
	eng := runtime.NewPodmanEngine(runtime.PodmanEngineOptions{
		GitAdapter: git.NewCLIAdapter(git.CLIAdapterOptions{}),
	})
	sbCfg := sandboxspec.SandboxConfig{
		Runtime: "podman",
		Image:   integrationAlpineImage,
		Network: "none",
	}

	t.Run("passing gatekeeper preserves pre-existing worker edit", func(t *testing.T) {
		ws := initIntegrationGitRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(ws, "allowed"), 0o755))
		workerFile := filepath.Join(ws, "allowed", "worker_edit.txt")
		require.NoError(t, os.WriteFile(workerFile, []byte("worker-v1\n"), 0o644))

		failure, err := eng.RunGatekeepers(t.Context(), runtime.GatekeeperRunSpec{
			Sandbox:          sbCfg,
			HostWorkspaceDir: ws,
			HostOS:           "linux",
		}, []workerspec.GatekeeperConfig{
			{
				Name:             "verify-worker-file",
				Command:          []string{"sh", "-c", "grep -q worker-v1 /workspace/allowed/worker_edit.txt"},
				WorkingDir:       ".",
				Timeout:          "20s",
				ExpectExitCode:   0,
				RequireCleanTree: true,
			},
		})
		require.NoError(t, err)
		assert.Nil(t, failure)

		got, err := os.ReadFile(workerFile)
		require.NoError(t, err)
		assert.Equal(t, "worker-v1\n", string(got))
	})

	t.Run("failing exit code stops pipeline and reports details", func(t *testing.T) {
		ws := initIntegrationGitRepo(t)
		failure, err := eng.RunGatekeepers(t.Context(), runtime.GatekeeperRunSpec{
			Sandbox:          sbCfg,
			HostWorkspaceDir: ws,
			HostOS:           "linux",
		}, []workerspec.GatekeeperConfig{
			{
				Name:             "unit-tests",
				Command:          []string{"sh", "-c", "echo test-out; echo test-err >&2; exit 7"},
				WorkingDir:       ".",
				Timeout:          "20s",
				ExpectExitCode:   0,
				RequireCleanTree: true,
			},
		})
		require.NoError(t, err)
		require.NotNil(t, failure)
		assert.Equal(t, "unit-tests", failure.Name)
		assert.Equal(t, 7, failure.ExitCode)
		assert.Equal(t, 0, failure.ExpectedExitCode)
		assert.False(t, failure.TimedOut)
		assert.Contains(t, failure.Stdout, "test-out")
		assert.Contains(t, failure.Stderr, "test-err")
	})

	t.Run("timeout cancels gatekeeper and reports TimedOut", func(t *testing.T) {
		ws := initIntegrationGitRepo(t)
		failure, err := eng.RunGatekeepers(t.Context(), runtime.GatekeeperRunSpec{
			Sandbox:          sbCfg,
			HostWorkspaceDir: ws,
			HostOS:           "linux",
		}, []workerspec.GatekeeperConfig{
			{
				Name:             "slow-check",
				Command:          []string{"sleep", "15"},
				WorkingDir:       ".",
				Timeout:          "1s",
				ExpectExitCode:   0,
				RequireCleanTree: true,
			},
		})
		require.NoError(t, err)
		require.NotNil(t, failure)
		assert.Equal(t, "slow-check", failure.Name)
		assert.True(t, failure.TimedOut)
	})

	t.Run("require_clean_worktree catches garbage and restores mutated pre-existing worker edit", func(t *testing.T) {
		ws := initIntegrationGitRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(ws, "allowed"), 0o755))
		workerFile := filepath.Join(ws, "allowed", "worker_edit.txt")
		const expectedWorkerContent = "exact-worker-content\n"
		require.NoError(t, os.WriteFile(workerFile, []byte(expectedWorkerContent), 0o644))

		failure, err := eng.RunGatekeepers(t.Context(), runtime.GatekeeperRunSpec{
			Sandbox:          sbCfg,
			HostWorkspaceDir: ws,
			HostOS:           "linux",
		}, []workerspec.GatekeeperConfig{
			{
				Name:             "sneaky-gatekeeper",
				Command:          []string{"sh", "-c", "echo junk > /workspace/garbage.txt && echo mutated > /workspace/allowed/worker_edit.txt"},
				WorkingDir:       ".",
				Timeout:          "20s",
				ExpectExitCode:   0,
				RequireCleanTree: true,
			},
		})
		require.NoError(t, err)
		require.NotNil(t, failure)
		assert.Equal(t, "sneaky-gatekeeper", failure.Name)
		assert.Equal(t, []string{"allowed/worker_edit.txt", "garbage.txt"}, failure.DirtyFiles)

		_, garbageErr := os.Stat(filepath.Join(ws, "garbage.txt"))
		assert.ErrorIs(t, garbageErr, os.ErrNotExist)

		gotWorkerContent, err := os.ReadFile(workerFile)
		require.NoError(t, err)
		assert.Equal(t, expectedWorkerContent, string(gotWorkerContent))
	})
}

func initIntegrationGitRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "tester"},
		{"config", "user.email", "tester@example.invalid"},
		{"config", "commit.gpgsign", "false"},
		{"config", "core.autocrlf", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	require.NoError(t, os.WriteFile(filepath.Join(ws, "README.md"), []byte("hello\n"), 0o644))
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	return ws
}

func runPodmanCLI(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
