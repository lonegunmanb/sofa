//go:build integration

package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessruntime "github.com/lonegunmanb/sofa/internal/harness/runtime"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const antigravityIntegrationImage = "docker.io/library/golang:1.27"

func buildAntigravityLinuxPluginBundle(t *testing.T, arch string) string {
	t.Helper()

	pluginRoot := t.TempDir()
	bundleStaging := t.TempDir()

	pluginDir, err := filepath.Abs(filepath.Join("..", "..", "..", "plugins", "sofa-harness-antigravity"))
	require.NoError(t, err)

	cmd := exec.Command(
		"uv", "run", "--python", "3.12",
		"./scripts/build_linux_bundle.py",
		"--out", bundleStaging,
		"--arch", arch,
	)
	cmd.Dir = pluginDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build antigravity linux bundle: %s", string(out))

	// Stage into <plugin_dir>/sofa-harness-antigravity/linux_<arch>/ so local 013 installer
	// platform-specific discovery finds {sofa-harness-antigravity, python/...} with zero hardcoded paths.
	builtDir := filepath.Join(bundleStaging, "sofa-harness-antigravity")
	platformDir := filepath.Join(pluginRoot, "sofa-harness-antigravity", "linux_"+arch)
	require.NoError(t, os.MkdirAll(filepath.Dir(platformDir), 0o755))
	require.NoError(t, os.Rename(builtDir, platformDir))
	return pluginRoot
}

func TestWorkerRunnerWithExternalAntigravityPluginPodmanIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, _, baseSHA := initBareRepoForRunner(t)

	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(ctx)
	require.NoError(t, err)
	pluginRoot := buildAntigravityLinuxPluginBundle(t, platform.Arch)

	t.Setenv("SOFA_TEST_GEMINI_API_KEY", "gemini-in-mem-secret")

	sharedRoot := t.TempDir()
	tempRoot := filepath.Join(sharedRoot, "runner-tmp")
	blobStore, err := blob.NewLocalStore(filepath.Join(sharedRoot, "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})

	runID := "run-int-antigravity"
	nodeAddr := "worker.antigravity"
	ledger := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	gitAdapter := git.NewCLIAdapter(git.CLIAdapterOptions{Ledger: ledger})
	cp, err := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
		InitialEpoch:        1,
		Ledger:              ledger,
		OwnerNode:           nodeAddr,
		CheckpointStatePath: filepath.Join(sharedRoot, "cp-ckpt.json"),
		StatePath:           filepath.Join(sharedRoot, "cp-state.json"),
	})
	require.NoError(t, err)

	r, err := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
		ControlPlane:  cp,
		GitAdapter:    gitAdapter,
		BlobStore:     blobStore,
		ArtifactSvc:   artSvc,
		SandboxEngine: eng,
		SessionLauncher: &runner.ContainerSessionLauncher{
			Installer:    harnessinstaller.NewInstaller(),
			ToolCacheDir: filepath.Join(sharedRoot, "tool-cache"),
			StartTimeout: 45 * time.Second,
		},
	})
	require.NoError(t, err)

	psBefore, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err)

	var (
		evMu   sync.Mutex
		events []harnessruntime.Event
	)

	outcome, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		Epoch:       1,
		RepoURL:     bareDir,
		BasedSource: "main",
		TempRoot:    tempRoot,
		Sandboxes: map[string]sandboxspec.SandboxConfig{
			"sandbox.golang": {Runtime: "podman", Image: antigravityIntegrationImage, Network: "bridge"},
		},
		Harnesses: map[string]harnessspec.HarnessConfig{
			"harness.antigravity": {
				Name: "antigravity",
				EnvRefs: map[string]string{
					"GEMINI_API_KEY": "SOFA_TEST_GEMINI_API_KEY",
				},
				Options: cty.ObjectVal(map[string]cty.Value{
					"backend": cty.StringVal("fake_scripted"),
				}),
			},
		},
		PluginResolveOpts: harnessinstaller.ResolveOptions{
			PluginDir: pluginRoot,
		},
		OnEvent: func(ev harnessruntime.Event) {
			evMu.Lock()
			events = append(events, ev)
			evMu.Unlock()
		},
		Worker: workerspec.Config{
			Sandbox:       baseSandboxRef("golang"),
			Harness:       baseHarnessRef("antigravity"),
			Model:         "gemini-2.5-pro",
			SystemPrompt:  cty.StringVal("work"),
			Prompt:        cty.StringVal("Implement feature"),
			WritablePaths: []string{"allowed/**"},
			HumanApproval: "optional",
			Artifacts: map[string]workerspec.Artifact{
				"report": {Name: "report", Type: "file", Path: "report.json", Description: "report", Required: true, NonEmpty: true},
			},
			QC: &workerspec.QCConfig{
				Criteria: cty.MapVal(map[string]cty.Value{
					"read_only_isolation": cty.StringVal("QC must not mutate workspace or artifacts"),
					"feature_content":     cty.StringVal("feature.txt and report.json must match antigravity output"),
				}),
				Model:       "gemini-2.5-flash",
				MaxQCRounds: 2,
				Gatekeepers: []workerspec.GatekeeperConfig{
					{
						Name:             "verify_feature",
						Command:          []string{"sh", "-c", "test \"$(cat allowed/feature.txt)\" = \"feature-v1\""},
						WorkingDir:       ".",
						Timeout:          "1m",
						ExpectExitCode:   0,
						RequireCleanTree: true,
					},
				},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, outcome.Status, "failure_reason=%s", outcome.FailureReason)

	expectedBranch := git.WorkBranchName(runID, nodeAddr)
	require.Equal(t, expectedBranch, outcome.WorkBranch)
	require.NotEqual(t, baseSHA, outcome.CommitSHA)
	require.Len(t, outcome.Artifacts, 1)
	require.Equal(t, "report", outcome.Artifacts[0].Name)

	// Verify streaming events arrived from Python plugin over AutoMTLS gRPC.
	evMu.Lock()
	var sawReasoning, sawDelta, sawUsage, sawIdle bool
	for _, ev := range events {
		switch ev.Kind {
		case harnessruntime.EventAssistantReasoningDelta:
			sawReasoning = true
		case harnessruntime.EventAssistantMessageDelta:
			sawDelta = true
		case harnessruntime.EventAssistantUsage:
			sawUsage = true
		case harnessruntime.EventSessionIdle:
			sawIdle = true
		}
	}
	evMu.Unlock()
	assert.True(t, sawReasoning, "expected EventAssistantReasoningDelta from Python plugin")
	assert.True(t, sawDelta, "expected EventAssistantMessageDelta from Python plugin")
	assert.True(t, sawUsage, "expected EventAssistantUsage from Python plugin")
	assert.True(t, sawIdle, "expected EventSessionIdle from Python plugin")

	// Verify work_branch exists on bare remote and contains feature.txt.
	verifyWs := filepath.Join(t.TempDir(), "verify-ws")
	require.NoError(t, gitAdapter.PrepareWorkspace(ctx, bareDir, expectedBranch, outcome.CommitSHA, verifyWs, git.AuthConfig{}))
	featBytes, err := os.ReadFile(filepath.Join(verifyWs, "allowed", "feature.txt"))
	require.NoError(t, err)
	require.Equal(t, "feature-v1\n", strings.ReplaceAll(string(featBytes), "\r\n", "\n"))

	// Verify report.json is downloadable and digest-verified from BlobStore.
	verifyImports := filepath.Join(t.TempDir(), "verify-imports")
	require.NoError(t, artSvc.StageImports(ctx, verifyImports, []artifact.ImportRequest{{
		Name:    "report",
		RelPath: "report.json",
		Sources: []artifact.ImportSource{{Artifact: outcome.Artifacts[0]}},
	}}))
	repBytes, err := os.ReadFile(filepath.Join(verifyImports, "report.json"))
	require.NoError(t, err)
	require.Contains(t, string(repBytes), `"harness":"antigravity"`)

	// Verify zero leftover Podman containers.
	psAfter, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(psBefore)), strings.TrimSpace(string(psAfter)))
}
