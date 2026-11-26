//go:build integration

package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness/runtime"
	"github.com/lonegunmanb/sofa/internal/provider"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const integrationContainerImage = "alpine:latest"

func crossCompileLinuxLoopback(t *testing.T, arch string) string {
	t.Helper()
	harnessDir := t.TempDir()
	outBin := filepath.Join(harnessDir, "sofa-harness-loopback")

	cmd := exec.Command("go", "build", "-trimpath", "-o", outBin, "./testdata/loopbackplugin")
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "cross-compile loopbackplugin for linux/%s: %s", arch, string(out))
	require.NoError(t, os.Chmod(outBin, 0o755))
	return harnessDir
}

func TestPodmanContainerRunnerAutoMTLSAndZeroSecretEnv(t *testing.T) {
	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(t.Context())
	require.NoError(t, err)
	require.NoError(t, platform.Validate())

	harnessDir := crossCompileLinuxLoopback(t, platform.Arch)
	workspaceDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, ".git"), 0o755))
	sessionStoreDir := t.TempDir()

	t.Setenv("SOFA_HOST_SECRET", "host-secret-must-not-enter-container")

	layout := sandboxruntime.SessionMountLayout{
		Sandbox: sandboxspec.SandboxConfig{
			Runtime: "podman",
			Image:   integrationContainerImage,
			Network: "bridge",
		},
		Entrypoint:       "/.sofa/harness/sofa-harness-loopback",
		WorkingDir:       ".",
		HostWorkspaceDir: workspaceDir,
		HarnessDir:       harnessDir,
		SessionStoreDir:  sessionStoreDir,
	}

	opener, err := runtime.NewContainerOpener(layout, runtime.OpenerOptions{
		StartTimeout: 30 * time.Second,
		RuntimeBin:   "podman",
	})
	require.NoError(t, err)

	cid := opener.ContainerID()
	require.NotEmpty(t, cid)
	cleanedUp := false
	t.Cleanup(func() {
		if !cleanedUp {
			_ = opener.Close()
		}
		_ = exec.Command("podman", "rm", "-f", "-t", "1", cid).Run()
	})

	// 1. Capabilities over AutoMTLS inside real Podman container.
	caps, err := opener.Capabilities(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "loopback", caps.HarnessName)
	assert.Equal(t, uint32(1), caps.ProtocolVersion)

	// 2. OpenSession delivering business secrets purely in-memory over AutoMTLS.
	const providerSecret = "sk-live-container-secret"
	const harnessSecret = "harness-container-secret"
	apiKeyRef := "CONTAINER_PROVIDER_KEY"
	lookupEnv := func(key string) (string, bool) {
		switch key {
		case "CONTAINER_PROVIDER_KEY":
			return providerSecret, true
		case "CONTAINER_HARNESS_TOKEN":
			return harnessSecret, true
		default:
			return "", false
		}
	}

	matProvider, err := runtime.MaterializeProviderConfig(provider.Config{
		Type:      provider.TypeOpenAI,
		Endpoint:  "https://api.example.invalid/v1",
		APIKeyRef: &apiKeyRef,
	}, "gpt-5", "gpt-5", lookupEnv)
	require.NoError(t, err)

	matHarnessEnv, err := runtime.MaterializeHarnessEnv(map[string]string{
		"GITHUB_TOKEN": "CONTAINER_HARNESS_TOKEN",
	}, lookupEnv)
	require.NoError(t, err)

	sess, err := opener.Open(t.Context(), runtime.SessionConfig{
		SessionID:             "ctr-sess-1",
		Model:                 "gpt-5",
		WorkingDirectory:      "/workspace",
		SessionStoreDirectory: "/.sofa/session-store",
		PermissionMode:        "approve_all",
		Provider:              &matProvider,
		HarnessEnv:            matHarnessEnv,
		HostTools: []runtime.HostToolSpec{
			{Name: "echo_host", Description: "echo", ParametersJSONSchema: []byte(`{"type":"object"}`)},
		},
		ToolHandlers: map[string]runtime.ToolHandler{
			"echo_host": func(_ context.Context, call runtime.ToolInvocation) (runtime.ToolOutput, error) {
				return runtime.ToolOutput{
					ResultType:       runtime.HostToolResultSuccess,
					TextResultForLLM: "ctr-tool:" + string(call.ArgumentsJSON),
				}, nil
			},
		},
	})
	require.NoError(t, err)

	// 3. Verify `podman inspect` contains ZERO business API keys / host secrets.
	inspectOut, err := exec.CommandContext(t.Context(), "podman", "inspect", cid, "--format", "{{json .Config.Env}}").CombinedOutput()
	require.NoError(t, err, "podman inspect failed: %s", string(inspectOut))
	var containerEnv []string
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(inspectOut))), &containerEnv))
	for _, entry := range containerEnv {
		assert.NotContains(t, entry, providerSecret)
		assert.NotContains(t, entry, harnessSecret)
		assert.NotContains(t, entry, "host-secret-must-not-enter-container")
		assert.False(t, strings.HasPrefix(entry, "GITHUB_TOKEN="))
		assert.False(t, strings.HasPrefix(entry, "OPENAI_API_KEY="))
	}

	// 4. Verify secrets arrived over mTLS RPC and PLUGIN_* / SOFA_HARNESS_PLUGIN were scrubbed inside plugin.
	sessRes, err := sess.ReadMCPResource(t.Context(), "loopback", "loopback://session")
	require.NoError(t, err)
	require.Len(t, sessRes, 1)
	assert.Contains(t, sessRes[0].Text, providerSecret)
	assert.Contains(t, sessRes[0].Text, harnessSecret)

	envRes, err := sess.ReadMCPResource(t.Context(), "loopback", "loopback://env")
	require.NoError(t, err)
	require.Len(t, envRes, 1)
	var pluginEnv []string
	require.NoError(t, json.Unmarshal([]byte(envRes[0].Text), &pluginEnv))
	for _, entry := range pluginEnv {
		key, _, _ := strings.Cut(entry, "=")
		assert.False(t, strings.HasPrefix(key, "PLUGIN_"), "PLUGIN_* var %q must be scrubbed", key)
		assert.NotEqual(t, "SOFA_HARNESS_PLUGIN", key, "SOFA_HARNESS_PLUGIN must be scrubbed")
	}

	// 5. RunTurn roundtrip including streaming events + host tool callback inside Podman container.
	var events []runtime.Event
	summary, err := sess.RunTurn(t.Context(), "hello-podman", func(ev runtime.Event) {
		events = append(events, ev)
	})
	require.NoError(t, err)
	assert.Contains(t, summary.FinalContent, "done:ctr-tool:")
	assert.NotEmpty(t, events)

	// 6. Close session + opener and assert container is completely removed.
	require.NoError(t, sess.Close(t.Context()))
	require.NoError(t, opener.Close())
	cleanedUp = true

	postInspect, postErr := exec.CommandContext(t.Context(), "podman", "inspect", cid).CombinedOutput()
	require.Error(t, postErr, "container %q must be removed after Close (inspect output: %s)", cid, string(postInspect))
}
