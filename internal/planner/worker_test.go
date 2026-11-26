package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/sofa/internal/config"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/require"
)

const workerInfrastructure = `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}
`

func TestWorkerChainWithArtifactsAndImports(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  artifact "spec" {
    type        = "file"
    path        = "docs/spec.md"
    description = "approved spec"
  }
}

worker "plan" {
  based_source  = worker.specify.work_branch
  model         = "test-model"
  system_prompt = "plan"
  prompt        = "Read ${worker.specify.artifact.spec.path} and write ${artifact("design").path}"

  import_artifact "spec" {
    desc    = "approved spec"
    sources = [worker.specify.artifact.spec]
  }

  artifact "design" {
    type        = "file"
    path        = "docs/design.md"
    description = "design"
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	planValue := cfg.EvalContext().Variables["worker"].GetAttr("plan")
	require.True(t, planValue.Type().HasAttribute("work_branch"))
	require.True(t, planValue.Type().HasAttribute("artifact"))
	require.Equal(t, "docs/design.md",
		planValue.GetAttr("artifact").GetAttr("design").GetAttr("path").AsString())
	ancestors, err := cfg.GetAncestors("worker.plan")
	require.NoError(t, err)
	require.Contains(t, ancestors, "worker.specify")
	require.NotEmpty(t, planBlockDeferredExpression(t, cfg, "plan"))
}

func TestWorkerArtifactImportPathDefaultsAndConflicts(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  import_artifact "design" {
    desc    = "design"
    sources = []
  }
}
`)
	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var specifyBlock *workerspec.WorkerBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*workerspec.WorkerBlock); ok && candidate.Name() == "specify" {
			specifyBlock = candidate
		}
	}
	require.NotNil(t, specifyBlock)
	require.Equal(t, ".sofa/imports/design", specifyBlock.WorkerConfig.Imports["design"].Path)

	conflictDir := t.TempDir()
	writePlannerFile(t, filepath.Join(conflictDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  artifact "spec" {
    type        = "file"
    path        = ".sofa/imports/design"
    description = "spec"
  }
  import_artifact "design" {
    desc    = "design"
    sources = []
  }
}
`)
	cfg = newTestPlanner(t, conflictDir, nil)
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), conflictDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	require.NoError(t, err)
	err = cfg.RunPlan()
	require.ErrorContains(t, err, "conflicts")
}

func TestWorkerStaticForEachUsesGoldenExpansion(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "os_matrix" {
  for_each      = toset(["linux", "windows"])
  based_source  = "main"
  model         = "test-model"
  system_prompt = "test ${each.key}"
}

output "linux_branch" {
  value = worker.os_matrix["linux"].work_branch
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())
	matrix := cfg.EvalContext().Variables["worker"].GetAttr("os_matrix")
	require.True(t, matrix.Type().HasAttribute("linux"))
	require.True(t, matrix.Type().HasAttribute("windows"))
	require.False(t, matrix.GetAttr("linux").GetAttr("work_branch").IsKnown())
}

func TestWorkerRootDefaultsAndExplicitNullProvider(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), workerInfrastructure+`
model_provider "openai" {
  type     = "openai"
  endpoint = "https://api.example.com/v1"
}

harness "alternative" {}

worker "specify" {
  based_source   = "main"
  model          = "test-model"
  system_prompt  = "specify"
  harness        = harness.alternative
  model_provider = null
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	ancestors, err := cfg.GetAncestors("worker.specify")
	require.NoError(t, err)
	require.Contains(t, ancestors, "sandbox.go_env")
	require.Contains(t, ancestors, "harness.alternative")
	require.NotContains(t, ancestors, "model_provider.openai")
}

func TestWorkerRootDefaultErrors(t *testing.T) {
	tests := []struct {
		name  string
		infra string
		want  string
	}{
		{
			name:  "missing sandbox and harness",
			infra: ``,
			want:  "sandbox",
		},
		{
			name: "ambiguous sandbox",
			infra: `
sandbox "one" {
  runtime = "podman"
  image   = "image"
}
sandbox "two" {
  runtime = "docker"
  image   = "image"
}
harness "local" {}
`,
			want: "multiple sandbox",
		},
		{
			name: "ambiguous provider",
			infra: workerInfrastructure + `
model_provider "one" {
  type     = "openai"
  endpoint = "https://one.example.com"
}
model_provider "two" {
  type     = "openai"
  endpoint = "https://two.example.com"
}
`,
			want: "multiple model_provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.infra+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"
}
`)
			loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
			require.NoError(t, err)
			require.False(t, diags.HasErrors(), diags.Error())
			cfg := newTestPlanner(t, dir, nil)
			require.ErrorContains(t, cfg.Init(loaded.Blocks), tt.want)
		})
	}
}

func TestWorkerResultOnlyExposedWithTerminatingTool(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"
}

output "result" {
  value = worker.specify.result
}
`)
	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.ErrorContains(t, cfg.RunPlan(), "result")

	withToolDir := t.TempDir()
	writePlannerFile(t, filepath.Join(withToolDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source      = "main"
  model             = "test-model"
  system_prompt     = "specify"
  terminate_tool_id = "tool_go_tool_finish_12345678-1234-8234-9234-123456789abc"
}

output "result" {
  value = worker.specify.result
}
`)
	cfg = newTestPlanner(t, withToolDir, nil)
	initPlanner(t, cfg, withToolDir)
	require.NoError(t, cfg.RunPlan())
	require.False(t, cfg.Outputs()[0].Value.IsKnown())
}

func TestWorkerGatekeeperAndFallbackValidation(t *testing.T) {
	invalidGatekeeperDir := t.TempDir()
	writePlannerFile(t, filepath.Join(invalidGatekeeperDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"
  gatekeeper "test" {
    command     = ["go", "test", "./..."]
    working_dir = "../outside"
  }
}
`)
	cfg := newTestPlanner(t, invalidGatekeeperDir, nil)
	initPlanner(t, cfg, invalidGatekeeperDir)
	require.ErrorContains(t, cfg.RunPlan(), "workspace-relative")

	validFallbackDir := t.TempDir()
	writePlannerFile(t, filepath.Join(validFallbackDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "tasks" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "tasks"
}

worker "implement" {
  based_source  = worker.tasks.work_branch
  model         = "test-model"
  system_prompt = "implement"
  fallback "rework" {
    on          = ["qc_exhausted"]
    target      = worker.tasks
    max_reworks = 2
  }
}
`)
	cfg = newTestPlanner(t, validFallbackDir, nil)
	initPlanner(t, cfg, validFallbackDir)
	require.NoError(t, cfg.RunPlan())

	unrelatedDir := t.TempDir()
	writePlannerFile(t, filepath.Join(unrelatedDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "tasks" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "tasks"
}

worker "unrelated" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "unrelated"
}

worker "implement" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "implement"
  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.unrelated
  }
}
`)
	cfg = newTestPlanner(t, unrelatedDir, nil)
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), unrelatedDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	require.ErrorContains(t, err, "positive ancestor")
}

func planBlockDeferredExpression(t *testing.T, cfg *SofaConfig, name string) string {
	t.Helper()
	for _, vertex := range cfg.GetVertices() {
		if block, ok := vertex.(*workerspec.WorkerBlock); ok && block.Name() == name {
			return block.WorkerConfig.DeferredExpression
		}
	}
	return ""
}
