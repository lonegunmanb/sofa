//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/cli"
	"github.com/lonegunmanb/sofa/internal/git"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reserveFreeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func runGitCLIForIT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoForCLIIT(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")

	runGitCLIForIT(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runGitCLIForIT(t, seedDir, "init")
	runGitCLIForIT(t, seedDir, "config", "user.name", "seed")
	runGitCLIForIT(t, seedDir, "config", "user.email", "seed@sofa.local")
	runGitCLIForIT(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, "allowed"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "allowed", "base.txt"), []byte("base\n"), 0o644))
	runGitCLIForIT(t, seedDir, "add", "-A")
	runGitCLIForIT(t, seedDir, "commit", "-m", "initial")
	runGitCLIForIT(t, seedDir, "branch", "-M", "main")
	runGitCLIForIT(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	return bareDir
}

func crossCompileScriptedPluginForCLIIT(t *testing.T, arch string) string {
	t.Helper()
	pluginRoot := t.TempDir()
	binPath := filepath.Join(pluginRoot, "sofa-harness-scripted", "linux_"+arch, "sofa-harness-scripted")
	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))

	cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "../worker/runner/testdata/scriptedplugin")
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "cross-compile scriptedplugin for linux/%s: %s", arch, string(out))
	require.NoError(t, os.Chmod(binPath, 0o755))
	return pluginRoot
}

func buildSofaBinaryForIT(t *testing.T) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	binPath := filepath.Join(t.TempDir(), "sofa"+ext)
	cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "../../cmd/sofa")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build cmd/sofa: %s", string(out))
	return binPath
}

func killProcessTree(cmd *exec.Cmd, done <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func TestSofaServerDevAndCLIEndToEndIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(ctx)
	require.NoError(t, err)

	pluginDir := crossCompileScriptedPluginForCLIIT(t, platform.Arch)
	sofaBin := buildSofaBinaryForIT(t)
	bareDir := initBareRepoForCLIIT(t)

	stateDir := filepath.Join(t.TempDir(), "dev-state")
	listenAddr := reserveFreeLoopbackAddr(t)
	serverURL := "http://" + listenAddr

	// Launch real `sofa server --dev` subprocess.
	srvCmd := exec.Command(sofaBin,
		"server", "--dev",
		"--listen", listenAddr,
		"--state-dir", stateDir,
		"--plugin-dir", pluginDir,
	)
	var srvOut bytes.Buffer
	srvCmd.Stdout = &srvOut
	srvCmd.Stderr = &srvOut
	require.NoError(t, srvCmd.Start())
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- srvCmd.Wait()
	}()
	t.Cleanup(func() {
		killProcessTree(srvCmd, srvDone)
	})

	// Poll /healthz until 200 OK.
	require.Eventually(t, func() bool {
		select {
		case exitErr := <-srvDone:
			t.Fatalf("sofa server --dev exited prematurely (%v):\n%s", exitErr, srvOut.String())
		default:
		}
		resp, err := http.Get(serverURL + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 45*time.Second, 200*time.Millisecond, "sofa server --dev logs:\n%s", srvOut.String())

	// Author HCL workflow in a fresh config dir (without running sofa init manually first).
	hclDir := filepath.Join(t.TempDir(), "workflow")
	require.NoError(t, os.MkdirAll(hclDir, 0o755))
	writeCLIFile(t, filepath.Join(hclDir, "main.sofa.hcl"), fmt.Sprintf(`
source "project" {
  url    = %q
  branch = "main"
}

sandbox "alpine" {
  runtime = "podman"
  image   = "alpine:latest"
}

model_provider "openai" {
  type        = "openai"
  endpoint    = "https://api.openai.com/v1"
  api_key_ref = "TEST_API_KEY"
}

harness "scripted" {}

worker "build" {
  based_source   = source.project.branch
  model          = "work-model"
  system_prompt  = "Implement feature"
  writable_paths = ["allowed/**"]
  artifact "report" {
    type        = "file"
    path        = "report.json"
    description = "Report output"
  }
}

output "work_branch" {
  value = worker.build.work_branch
}
`, bareDir))

	// 1. Apply BEFORE setting TEST_API_KEY -> 422 preflight failure with sofa secret set guidance.
	var outBuf, errBuf bytes.Buffer
	optsMissing := cli.Options{
		WorkingDir: hclDir,
		LookupEnv: mapLookupEnv(map[string]string{
			"SOFA_SERVER_ADDR": serverURL,
		}),
	}
	code := cli.Run(ctx, []string{"apply", "."}, strings.NewReader(""), &outBuf, &errBuf, optsMissing)
	assert.Equal(t, cli.ExitFailure, code)
	assert.Contains(t, errBuf.String(), "TEST_API_KEY")
	assert.Contains(t, errBuf.String(), "sofa secret set --from-env")

	// 2. Set TEST_API_KEY via `sofa secret set --from-env TEST_API_KEY`.
	outBuf.Reset()
	errBuf.Reset()
	optsWithSecret := cli.Options{
		WorkingDir: hclDir,
		LookupEnv: mapLookupEnv(map[string]string{
			"SOFA_SERVER_ADDR": serverURL,
			"TEST_API_KEY":     "secret-test-key-value",
		}),
	}
	code = cli.Run(ctx, []string{"secret", "set", "--from-env", "TEST_API_KEY"}, strings.NewReader(""), &outBuf, &errBuf, optsWithSecret)
	require.Equal(t, cli.ExitSuccess, code, "stderr: %s", errBuf.String())
	assert.NotContains(t, outBuf.String(), "secret-test-key-value")

	// 3. Apply in foreground and follow until SUCCEEDED.
	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(ctx, []string{"apply", hclDir}, strings.NewReader(""), &outBuf, &errBuf, optsWithSecret)
	require.Equal(t, cli.ExitSuccess, code, "stdout:\n%s\nstderr:\n%s\nserver:\n%s", outBuf.String(), errBuf.String(), srvOut.String())

	m := regexp.MustCompile(`Run created:\s+(\S+)`).FindStringSubmatch(outBuf.String())
	require.Len(t, m, 2, "expected run_id in stdout: %s", outBuf.String())
	runID := m[1]

	// 4. Verify `sofa runs get <run_id>` shows SUCCEEDED and work_branch.
	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(ctx, []string{"runs", "get", runID}, strings.NewReader(""), &outBuf, &errBuf, optsWithSecret)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())
	assert.Contains(t, outBuf.String(), "Status: SUCCEEDED")
	expectedWorkBranch := git.WorkBranchName(runID, "worker.build")
	assert.Contains(t, outBuf.String(), expectedWorkBranch)

	// Verify work_branch exists in bare remote.git and local blob dir exists.
	workSHA := runGitCLIForIT(t, bareDir, "rev-parse", "--verify", "refs/heads/"+expectedWorkBranch)
	assert.NotEmpty(t, workSHA)
	runBlobDir := filepath.Join(stateDir, "blobs", "objects", "runs", runID)
	_, statErr := os.Stat(runBlobDir)
	require.NoError(t, statErr, "expected blob directory %s to exist before delete", runBlobDir)

	// 5. Run `sofa delete <run_id>` and verify remote ckpt/work branches and local blobs are deleted while main remains.
	outBuf.Reset()
	errBuf.Reset()
	code = cli.Run(ctx, []string{"delete", runID}, strings.NewReader(""), &outBuf, &errBuf, optsWithSecret)
	require.Equal(t, cli.ExitSuccess, code, errBuf.String())

	mainSHA := runGitCLIForIT(t, bareDir, "rev-parse", "--verify", "refs/heads/main")
	assert.NotEmpty(t, mainSHA)

	refsOut := runGitCLIForIT(t, bareDir, "for-each-ref", "--format=%(refname)", "refs/heads")
	assert.NotContains(t, refsOut, expectedWorkBranch)
	assert.NotContains(t, refsOut, "sofa/ckpt/"+runID)

	_, statAfterErr := os.Stat(runBlobDir)
	assert.ErrorIs(t, statAfterErr, os.ErrNotExist, "expected blob directory %s to be deleted", runBlobDir)
}
