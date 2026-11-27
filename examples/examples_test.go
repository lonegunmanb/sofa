package examples_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lonegunmanb/golden"
	actorcfg "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/applyeval"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/lonegunmanb/sofa/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func repoRootDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Clean(filepath.Join(wd, ".."))
}

func findPlanNode(t *testing.T, p *plan.Plan, addr string) plan.NodeSpec {
	t.Helper()
	for _, n := range p.Nodes() {
		if n.Address == addr {
			return n
		}
	}
	t.Fatalf("expected node %q in compiled plan", addr)
	return plan.NodeSpec{}
}

func TestCopilotAgySelfHostExampleCompilesAndRoundTripsPlan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	exampleDir := filepath.Join(repoRootDir(t), "examples", "copilot-agy-self-host")
	stateDir := filepath.Join(t.TempDir(), ".sofa")
	archivePath := filepath.Join(t.TempDir(), "self-host.sofaplan")
	extractDir := filepath.Join(t.TempDir(), "extracted")

	require.NoError(t, project.Init(ctx, exampleDir, stateDir, project.InitOptions{}))
	proj, err := project.OpenProject(stateDir)
	require.NoError(t, err)

	loadedDir, diags, err := config.LoadDirectoryContext(ctx, proj.ConfigDirectory)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:          ctx,
		Directory:        proj.ConfigDirectory,
		ModulesDirectory: proj.ModulesDirectory,
		BundlePath:       "config",
		CliFlagAssignedVariables: []golden.CliFlagAssignedVariables{
			golden.NewCliFlagAssignedVariable("repo_url", strconv.Quote("https://github.com/lonegunmanb/sofa.git")),
			golden.NewCliFlagAssignedVariable("slice_prompt", strconv.Quote("Implement Slice 033 with strict TDD.")),
		},
	})
	require.NoError(t, cfg.Init(loadedDir.Blocks))
	require.NoError(t, cfg.RunPlan())

	exported, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	require.NoError(t, plan.SaveArchive(archivePath, exported, plan.ArchiveSource{
		ConfigDir:  proj.ConfigDirectory,
		ModulesDir: proj.ModulesDirectory,
	}))
	loadedArchive, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)

	planned := loadedArchive.Plan
	workerNode := findPlanNode(t, planned, "worker.implement_slice")

	evalCtx, err := applyeval.BuildEvalContext(planned, applyeval.RuntimeState{
		Sources: map[string]applyeval.SourceRuntimeState{
			"source.repo": {
				URL:       "https://github.com/lonegunmanb/sofa.git",
				Branch:    "main",
				CommitSHA: "1111111111111111111111111111111111111111",
			},
		},
	}, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)

	workerCfg, err := applyeval.EvalReadyWorker(evalCtx, workerNode, applyeval.WithPlan(planned))
	require.NoError(t, err)

	assert.Equal(t, "main", workerCfg.BasedSource.AsString())
	assert.Equal(t, "sandbox.go_dev", workerCfg.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, "harness.copilot", workerCfg.Harness.GetAttr("address").AsString())
	assert.True(t, workerCfg.ModelProvider == cty.NilVal || workerCfg.ModelProvider.IsNull())
	assert.Equal(t, "required", workerCfg.HumanApproval)

	summaryArt, ok := workerCfg.Artifacts["summary"]
	require.True(t, ok, "artifact summary must be declared")
	assert.Equal(t, "file", summaryArt.Type)
	assert.Equal(t, "summary.md", summaryArt.Path)
	assert.True(t, summaryArt.Required)
	assert.True(t, summaryArt.NonEmpty)

	require.NotNil(t, workerCfg.QC, "worker.implement_slice must configure qc")
	assert.Equal(t, "sandbox.qc_audit", workerCfg.QC.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, "harness.antigravity", workerCfg.QC.Harness.GetAttr("address").AsString())
	assert.True(t, workerCfg.QC.ModelProviderSet)
	assert.True(t, workerCfg.QC.ModelProvider == cty.NilVal || workerCfg.QC.ModelProvider.IsNull())
	assert.Greater(t, workerCfg.QC.Criteria.LengthInt(), 0)

	require.Len(t, workerCfg.QC.Gatekeepers, 1)
	gk := workerCfg.QC.Gatekeepers[0]
	assert.Equal(t, "go_quality", gk.Name)
	assert.True(t, gk.RequireCleanTree)
	joinedCmd := strings.Join(gk.Command, " ")
	assert.Contains(t, joinedCmd, "gofmt")
	assert.Contains(t, joinedCmd, "go vet ./...")
	assert.Contains(t, joinedCmd, "go test ./... -count=1")

	outputs := planned.Outputs()
	assert.Contains(t, outputs, "work_branch")
	assert.Contains(t, outputs, "commit_sha")

	envRefs := actorcfg.ExtractPlanEnvRefs(planned)
	assert.Equal(t, []string{"COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY", "GITHUB_TOKEN"}, envRefs)
}

func TestInstallLocalPluginsOutputLayoutMatchesHarnessInstaller(t *testing.T) {
	t.Parallel()

	root := repoRootDir(t)
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	stubDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stubDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))

	cmd := exec.Command(
		"go", "run", "./scripts/install-local-plugins.go",
		"-only", "copilot",
		"-arch", "amd64",
		"-plugin-dir", pluginDir,
		"-copilot-src", stubDir,
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "install-local-plugins.go failed: %s", string(out))

	// Stage deterministic offline layout for antigravity matching build_linux_bundle.py output.
	agDir := filepath.Join(pluginDir, "sofa-harness-antigravity", "linux_amd64")
	require.NoError(t, os.MkdirAll(filepath.Join(agDir, "python", "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agDir, "sofa-harness-antigravity"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agDir, "python", "bin", "python3"), []byte("#!/bin/sh\nexit 0\n"), 0o755))

	inst := installer.NewInstaller()
	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	opts := installer.ResolveOptions{PluginDir: pluginDir}

	copilotPrepared, err := inst.Prepare(context.Background(), harnessspec.HarnessConfig{Name: "copilot"}, platform, opts)
	require.NoError(t, err)
	assert.Equal(t, "/.sofa/harness/sofa-harness-copilot", copilotPrepared.ContainerPath)

	copilotEntryInfo, err := os.Lstat(filepath.Join(copilotPrepared.HostDir, "sofa-harness-copilot"))
	require.NoError(t, err)
	assert.True(t, copilotEntryInfo.Mode().IsRegular())
	assert.Zero(t, copilotEntryInfo.Mode()&os.ModeSymlink)

	copilotCLIBinInfo, err := os.Lstat(filepath.Join(copilotPrepared.HostDir, "bin", "copilot"))
	require.NoError(t, err)
	assert.True(t, copilotCLIBinInfo.Mode().IsRegular())
	assert.Zero(t, copilotCLIBinInfo.Mode()&os.ModeSymlink)

	// Re-run with -copilot-linux-bin into the existing pluginDir to exercise atomic replacement.
	fakeLinuxBin := filepath.Join(t.TempDir(), "copilot-linux-amd64")
	require.NoError(t, os.WriteFile(fakeLinuxBin, []byte("#!/bin/sh\necho fake-copilot\n"), 0o755))
	cmd2 := exec.Command(
		"go", "run", "./scripts/install-local-plugins.go",
		"-only", "copilot",
		"-arch", "amd64",
		"-plugin-dir", pluginDir,
		"-copilot-linux-bin", fakeLinuxBin,
	)
	cmd2.Dir = root
	out2, err := cmd2.CombinedOutput()
	require.NoError(t, err, "re-install with -copilot-linux-bin failed: %s", string(out2))

	replacedBytes, err := os.ReadFile(filepath.Join(copilotPrepared.HostDir, "bin", "copilot"))
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho fake-copilot\n", string(replacedBytes))

	agPrepared, err := inst.Prepare(context.Background(), harnessspec.HarnessConfig{Name: "antigravity"}, platform, opts)
	require.NoError(t, err)
	assert.Equal(t, "/.sofa/harness/sofa-harness-antigravity", agPrepared.ContainerPath)

	agEntryInfo, err := os.Lstat(filepath.Join(agPrepared.HostDir, "sofa-harness-antigravity"))
	require.NoError(t, err)
	assert.True(t, agEntryInfo.Mode().IsRegular())
}

func TestInstallLocalPluginsDefaultBundlerModeWithSeededCache(t *testing.T) {
	t.Parallel()

	root := repoRootDir(t)
	pluginDir := filepath.Join(t.TempDir(), "plugins")
	cacheDir := filepath.Join(t.TempDir(), "cache")
	bundlerCacheDir := filepath.Join(cacheDir, "copilot-bundler-linux-amd64")
	require.NoError(t, os.MkdirAll(bundlerCacheDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(bundlerCacheDir, "zcopilot_linux_amd64.go"),
		[]byte("package main\n\nimport (\n\t_ \"github.com/github/copilot-sdk/go/embeddedcli\"\n\t_ \"github.com/klauspost/compress/zstd\"\n)\n\nfunc init() {}\n"),
		0o644,
	))

	cmd := exec.Command(
		"go", "run", "./scripts/install-local-plugins.go",
		"-only", "copilot",
		"-arch", "amd64",
		"-plugin-dir", pluginDir,
		"-cache-dir", cacheDir,
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "install-local-plugins.go default bundler mode failed: %s", string(out))

	inst := installer.NewInstaller()
	prepared, err := inst.Prepare(
		context.Background(),
		harnessspec.HarnessConfig{Name: "copilot"},
		installer.Platform{OS: "linux", Arch: "amd64"},
		installer.ResolveOptions{PluginDir: pluginDir},
	)
	require.NoError(t, err)
	assert.Equal(t, "/.sofa/harness/sofa-harness-copilot", prepared.ContainerPath)

	entryInfo, err := os.Lstat(filepath.Join(prepared.HostDir, "sofa-harness-copilot"))
	require.NoError(t, err)
	assert.True(t, entryInfo.Mode().IsRegular())
	assert.Zero(t, entryInfo.Mode()&os.ModeSymlink)

	_, binErr := os.Lstat(filepath.Join(prepared.HostDir, "bin"))
	assert.True(t, os.IsNotExist(binErr), "default bundler mode must not create bin/copilot sidecar directory")

	_, workingTreeZcopilotErr := os.Lstat(filepath.Join(root, "plugins", "sofa-harness-copilot", "cmd", "sofa-harness-copilot", "zcopilot_linux_amd64.go"))
	assert.True(t, os.IsNotExist(workingTreeZcopilotErr), "build-bundle.go must never pollute working tree with zcopilot_* files")
}
