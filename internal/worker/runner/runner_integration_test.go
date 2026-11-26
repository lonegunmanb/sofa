//go:build integration

package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/git"
	harnessinstaller "github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
	"github.com/lonegunmanb/sofa/internal/worker/runner"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const integrationImage = "alpine:latest"

func runGitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initBareRepoForRunner(t *testing.T) (bareDir, seedDir, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	seedDir = filepath.Join(root, "seed")

	runGitCLI(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runGitCLI(t, seedDir, "init")
	runGitCLI(t, seedDir, "config", "user.name", "seed")
	runGitCLI(t, seedDir, "config", "user.email", "seed@sofa.local")
	runGitCLI(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, "allowed"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "allowed", "base.txt"), []byte("base\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-base\nline2-base\n"), 0o644))

	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "initial")
	runGitCLI(t, seedDir, "branch", "-M", "main")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	baseSHA = runGitCLI(t, seedDir, "rev-parse", "HEAD")
	return bareDir, seedDir, baseSHA
}

func crossCompileScriptedPlugin(t *testing.T, arch string) string {
	t.Helper()
	pluginRoot := t.TempDir()
	binPath := filepath.Join(pluginRoot, "sofa-harness-scripted", "linux_"+arch, "sofa-harness-scripted")
	require.NoError(t, os.MkdirAll(filepath.Dir(binPath), 0o755))

	cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "./testdata/scriptedplugin")
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

func TestWorkerRunnerEndToEndGitPodmanAndBlobStoreIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, _, baseSHA := initBareRepoForRunner(t)

	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(ctx)
	require.NoError(t, err)
	pluginRoot := crossCompileScriptedPlugin(t, platform.Arch)

	sharedRoot := t.TempDir()
	tempRoot := filepath.Join(sharedRoot, "runner-tmp")
	blobStore, err := blob.NewLocalStore(filepath.Join(sharedRoot, "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})

	runID := "run-int-worker"
	nodeAddr := "worker.impl"
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
			StartTimeout: 30 * time.Second,
		},
	})
	require.NoError(t, err)

	psBefore, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err)

	outcome, err := r.Run(ctx, runner.NodeExecutionRequest{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		Epoch:       1,
		RepoURL:     bareDir,
		BasedSource: "main",
		TempRoot:    tempRoot,
		Sandboxes: map[string]sandboxspec.SandboxConfig{
			"sandbox.alpine": {Runtime: "podman", Image: integrationImage, Network: "bridge"},
		},
		Harnesses: map[string]harnessspec.HarnessConfig{
			"harness.scripted": {Name: "scripted"},
		},
		PluginResolveOpts: harnessinstaller.ResolveOptions{
			PluginDir: pluginRoot,
		},
		Worker: workerspec.Config{
			Sandbox:       baseSandboxRef("alpine"),
			Harness:       baseHarnessRef("scripted"),
			Model:         "work-model",
			SystemPrompt:  cty.StringVal("work"),
			Prompt:        cty.StringVal("Implement feature"),
			WritablePaths: []string{"allowed/**"},
			HumanApproval: "optional",
			Artifacts: map[string]workerspec.Artifact{
				"report": {Name: "report", Type: "file", Path: "report.json", Description: "report", Required: true, NonEmpty: true},
			},
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
			QC: &workerspec.QCConfig{
				Criteria: cty.MapVal(map[string]cty.Value{
					"read_only_isolation": cty.StringVal("QC must not be able to mutate workspace or artifacts"),
					"feature_content":     cty.StringVal("feature.txt and report.json must be valid"),
				}),
				Model:       "qc-model",
				MaxQCRounds: 2,
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, runner.StatusSucceeded, outcome.Status)
	expectedBranch := git.WorkBranchName(runID, nodeAddr)
	require.Equal(t, expectedBranch, outcome.WorkBranch)
	require.NotEqual(t, baseSHA, outcome.CommitSHA)
	require.Len(t, outcome.Artifacts, 1)
	require.Equal(t, "report", outcome.Artifacts[0].Name)

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
	require.Contains(t, string(repBytes), `"status":"ok"`)

	// Verify zero leftover host temp directories in tempRoot.
	entries, err := os.ReadDir(tempRoot)
	if err == nil {
		for _, e := range entries {
			sub, _ := os.ReadDir(filepath.Join(tempRoot, e.Name()))
			assert.Empty(t, sub, "expected zero leftover temp dirs under %s/%s", tempRoot, e.Name())
		}
	}

	// Verify zero leftover containers created by this test.
	psAfter, err := exec.Command("podman", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(psBefore)), strings.TrimSpace(string(psAfter)))
}

func TestWorkerRunnerCrossEpochDivergedWorkBranchRepublishGitIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, seedDir, baseSHA := initBareRepoForRunner(t)

	// Advance main to M_1 after Epoch 1 branches from baseSHA (M_0), so Epoch 2 produces a diverged history.
	runGitCLI(t, seedDir, "checkout", "main")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "upstream.txt"), []byte("from-M1\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "advance main to M1")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	m1SHA := runGitCLI(t, seedDir, "rev-parse", "HEAD")

	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{})
	platform, err := eng.DetectPlatform(ctx)
	require.NoError(t, err)
	pluginRoot := crossCompileScriptedPlugin(t, platform.Arch)

	sharedRoot := t.TempDir()
	blobStore, err := blob.NewLocalStore(filepath.Join(sharedRoot, "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})

	runID := "run-int-repub"
	nodeAddr := "worker.impl"
	ledger := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	gitAdapter := git.NewCLIAdapter(git.CLIAdapterOptions{Ledger: ledger})

	runEpoch := func(epoch uint64, base string) runner.NodeOutcome {
		cp, cpErr := runner.NewLocalNodeControlPlane(runner.LocalNodeControlPlaneOptions{
			InitialEpoch: epoch,
			Ledger:       ledger,
			OwnerNode:    nodeAddr,
		})
		require.NoError(t, cpErr)
		r, rErr := runner.NewWorkerRunner(runner.WorkerRunnerDeps{
			ControlPlane:  cp,
			GitAdapter:    gitAdapter,
			BlobStore:     blobStore,
			ArtifactSvc:   artSvc,
			SandboxEngine: eng,
			SessionLauncher: &runner.ContainerSessionLauncher{
				Installer:    harnessinstaller.NewInstaller(),
				ToolCacheDir: filepath.Join(sharedRoot, "tool-cache"),
				StartTimeout: 30 * time.Second,
			},
		})
		require.NoError(t, rErr)
		out, runErr := r.Run(ctx, runner.NodeExecutionRequest{
			RunID: runID, NodeAddr: nodeAddr, Epoch: epoch,
			RepoURL: bareDir, BasedSource: "main", BaseSHA: base,
			Sandboxes: map[string]sandboxspec.SandboxConfig{
				"sandbox.alpine": {Runtime: "podman", Image: integrationImage, Network: "bridge"},
			},
			Harnesses:         map[string]harnessspec.HarnessConfig{"harness.scripted": {Name: "scripted"}},
			PluginResolveOpts: harnessinstaller.ResolveOptions{PluginDir: pluginRoot},
			Worker: workerspec.Config{
				Sandbox: baseSandboxRef("alpine"), Harness: baseHarnessRef("scripted"),
				Model: "work-model", SystemPrompt: cty.StringVal("work"), Prompt: cty.StringVal("Implement feature"),
				WritablePaths: []string{"allowed/**"},
			},
		})
		require.NoError(t, runErr)
		require.Equal(t, runner.StatusSucceeded, out.Status)
		return out
	}

	out1 := runEpoch(1, baseSHA)
	out2 := runEpoch(2, m1SHA)
	require.Equal(t, out1.WorkBranch, out2.WorkBranch)
	require.NotEqual(t, out1.CommitSHA, out2.CommitSHA)

	// Verify real remote branch now points to out2.CommitSHA (descendant of m1SHA, diverged from out1.CommitSHA).
	remoteTip, containsM1, err := gitAdapter.BranchContainsSHA(ctx, bareDir, out2.WorkBranch, m1SHA, git.AuthConfig{})
	require.NoError(t, err)
	require.True(t, containsM1)
	require.Equal(t, out2.CommitSHA, remoteTip)
	_, containsW1, err := gitAdapter.BranchContainsSHA(ctx, bareDir, out2.WorkBranch, out1.CommitSHA, git.AuthConfig{})
	require.NoError(t, err)
	require.False(t, containsW1)
}

func TestExecuteMergeCandidateEndToEndGitAndPodmanIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, seedDir, baseSHA := initBareRepoForRunner(t)

	// Candidate 1: adds feat1.txt (passes gatekeeper).
	runGitCLI(t, seedDir, "checkout", "-b", "cand1")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "feat1.txt"), []byte("feat1\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "feat1")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand1")

	// Candidate 2: adds feat2.txt and updates shared.txt cleanly (passes gatekeeper).
	runGitCLI(t, seedDir, "checkout", "main")
	runGitCLI(t, seedDir, "checkout", "-b", "cand2")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "feat2.txt"), []byte("feat2\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-cand2\nline2-base\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "feat2")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand2")

	// Candidate 3a: conflicts with cand2 on shared.txt line 1.
	runGitCLI(t, seedDir, "checkout", "main")
	runGitCLI(t, seedDir, "checkout", "-b", "cand3_conflict")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "shared.txt"), []byte("line1-cand3\nline2-base\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "cand3 conflict")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand3_conflict")

	// Candidate 3b: rebases cleanly onto cand2, but creates fail_gatekeeper.flag which breaks real Podman gatekeeper!
	runGitCLI(t, seedDir, "checkout", "main")
	runGitCLI(t, seedDir, "checkout", "-b", "cand3_gk_fail")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "fail_gatekeeper.flag"), []byte("bad\n"), 0o644))
	runGitCLI(t, seedDir, "add", "-A")
	runGitCLI(t, seedDir, "commit", "-m", "cand3 breaks gatekeeper")
	runGitCLI(t, seedDir, "push", bareDir, "HEAD:refs/heads/cand3_gk_fail")

	ledger := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	gitAdapter := git.NewCLIAdapter(git.CLIAdapterOptions{Ledger: ledger})
	eng := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{GitAdapter: gitAdapter})
	sb := sandboxspec.SandboxConfig{Runtime: "podman", Image: integrationImage, Network: "none"}
	gks := []workerspec.GatekeeperConfig{{
		Name:             "no_fail_flag",
		Command:          []string{"sh", "-c", "test ! -f fail_gatekeeper.flag"},
		WorkingDir:       ".",
		Timeout:          "1m",
		ExpectExitCode:   0,
		RequireCleanTree: true,
	}}

	// 1. Merge candidate 1.
	res1, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
		RunID: "run-merge-int", MergeNodeAddr: "merge.integrate", Epoch: 1,
		RepoURL: bareDir, BasedSource: "main", BaseSHA: baseSHA,
		CandidateBranch: "cand1", Sandbox: sb, Gatekeepers: gks,
		GitAdapter: gitAdapter, Ledger: ledger, SandboxEngine: eng,
	})
	require.NoError(t, err)
	require.Equal(t, runner.MergeStatusSucceeded, res1.Status)
	targetBranch := res1.TargetBranch

	// 2. Merge candidate 2 -> linear history on top of candidate 1.
	res2, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
		RunID: "run-merge-int", MergeNodeAddr: "merge.integrate", Epoch: 1,
		RepoURL: bareDir, BasedSource: "main", BaseSHA: baseSHA,
		CandidateBranch: "cand2", Sandbox: sb, Gatekeepers: gks,
		GitAdapter: gitAdapter, Ledger: ledger, SandboxEngine: eng,
	})
	require.NoError(t, err)
	require.Equal(t, runner.MergeStatusSucceeded, res2.Status)
	cand2MergedSHA := res2.MergedCommitSHA

	_, contains1, err := gitAdapter.BranchContainsSHA(ctx, bareDir, targetBranch, res1.MergedCommitSHA, git.AuthConfig{})
	require.NoError(t, err)
	require.True(t, contains1)

	// 3a. Conflicting candidate 3a -> MergeStatusReworkSource, targetBranch stays at cand2MergedSHA.
	res3a, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
		RunID: "run-merge-int", MergeNodeAddr: "merge.integrate", Epoch: 1,
		RepoURL: bareDir, BasedSource: "main", BaseSHA: baseSHA,
		CandidateBranch: "cand3_conflict", Sandbox: sb, Gatekeepers: gks,
		GitAdapter: gitAdapter, Ledger: ledger, SandboxEngine: eng,
	})
	require.NoError(t, err)
	require.Equal(t, runner.MergeStatusReworkSource, res3a.Status)
	require.Equal(t, cand2MergedSHA, res3a.LatestTargetSHA)
	require.Equal(t, []string{"shared.txt"}, res3a.ConflictFiles)

	tipAfter3a, err := gitAdapter.ResolveBranchSHA(ctx, bareDir, targetBranch, git.AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, cand2MergedSHA, tipAfter3a)

	// 3b. Gatekeeper-failing candidate 3b -> MergeStatusReworkSource, targetBranch stays at cand2MergedSHA.
	res3b, err := runner.ExecuteMergeCandidate(ctx, runner.MergeCandidateRequest{
		RunID: "run-merge-int", MergeNodeAddr: "merge.integrate", Epoch: 1,
		RepoURL: bareDir, BasedSource: "main", BaseSHA: baseSHA,
		CandidateBranch: "cand3_gk_fail", Sandbox: sb, Gatekeepers: gks,
		GitAdapter: gitAdapter, Ledger: ledger, SandboxEngine: eng,
	})
	require.NoError(t, err)
	require.Equal(t, runner.MergeStatusReworkSource, res3b.Status)
	require.Equal(t, cand2MergedSHA, res3b.LatestTargetSHA)
	require.NotNil(t, res3b.GatekeeperFailure)
	require.Equal(t, 1, res3b.GatekeeperFailure.ExitCode)

	tipAfter3b, err := gitAdapter.ResolveBranchSHA(ctx, bareDir, targetBranch, git.AuthConfig{})
	require.NoError(t, err)
	require.Equal(t, cand2MergedSHA, tipAfter3b)
}
