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
	require.Equal(t, "/.sofa/artifacts/docs/design.md",
		planValue.GetAttr("artifact").GetAttr("design").GetAttr("path").AsString())
	require.Equal(t, "docs/design.md",
		planValue.GetAttr("artifact").GetAttr("design").GetAttr("rel_path").AsString())
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
	require.Equal(t, "design", specifyBlock.WorkerConfig().Imports["design"].Path)
	require.Equal(t, "/.sofa/imports/design", specifyBlock.WorkerConfig().Imports["design"].ContainerPath())

	// Same relative path in artifact and import_artifact must NOT conflict (disjoint container mount roots).
	disjointDir := t.TempDir()
	writePlannerFile(t, filepath.Join(disjointDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  artifact "spec" {
    type        = "file"
    path        = "spec.md"
    description = "spec"
  }
  import_artifact "doc" {
    desc    = "doc"
    path    = "spec.md"
    sources = []
  }
}
`)
	cfg = newTestPlanner(t, disjointDir, nil)
	initPlanner(t, cfg, disjointDir)
	require.NoError(t, cfg.RunPlan())

	// Intra-artifact and intra-import_artifact overlapping paths MUST fail.
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "duplicate artifact paths",
			body: workerInfrastructure + `
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  artifact "a" {
    type        = "file"
    path        = "spec.md"
    description = "a"
  }
  artifact "b" {
    type        = "file"
    path        = "spec.md"
    description = "b"
  }
}`,
		},
		{
			name: "nested artifact dir and file overlap",
			body: workerInfrastructure + `
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  artifact "dir" {
    type        = "directory"
    path        = "out"
    description = "dir"
  }
  artifact "nested" {
    type        = "file"
    path        = "out/a.txt"
    description = "nested"
  }
}`,
		},
		{
			name: "nested import_artifact dir and file overlap",
			body: workerInfrastructure + `
worker "specify" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "specify"

  import_artifact "dir" {
    desc    = "dir"
    path    = "inputs"
    sources = []
  }
  import_artifact "nested" {
    desc    = "nested"
    path    = "inputs/spec.md"
    sources = []
  }
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conflictDir := t.TempDir()
			writePlannerFile(t, filepath.Join(conflictDir, "workflow.sofa.hcl"), tc.body)
			cfg := newTestPlanner(t, conflictDir, nil)
			loaded, diags, err := config.LoadDirectoryContext(context.Background(), conflictDir)
			require.NoError(t, err)
			require.False(t, diags.HasErrors(), diags.Error())
			err = cfg.Init(loaded.Blocks)
			require.NoError(t, err)
			err = cfg.RunPlan()
			require.ErrorContains(t, err, "conflicts")
		})
	}
}

func TestWorkerQCIndependentSandboxAndProfileDAG(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
sandbox "work_env" {
  runtime = "podman"
  image   = "ghcr.io/example/work:1.27"
}

sandbox "sec_audit" {
  runtime = "podman"
  image   = "ghcr.io/example/audit:1.27"
}

harness "local" {}

worker "implement" {
  based_source  = "main"
  sandbox       = sandbox.work_env
  model         = "gpt-5"
  profile       = "dev-fast"
  system_prompt = "implement"

  qc {
    sandbox  = sandbox.sec_audit
    profile  = "opus-audit"
    criteria = { security = "must pass audit" }
  }
}

dynamic_worker "sweep" {
  dynamic "worker" {
    for_each = toset(["a"])
    content {
      based_source  = worker.implement.work_branch
      sandbox       = sandbox.work_env
      model         = "gpt-5"
      system_prompt = "sweep ${worker.key}"

      qc {
        sandbox  = sandbox.sec_audit
        profile  = "opus-audit"
        criteria = { audit = "ok" }
      }
    }
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	ancestors, err := cfg.GetAncestors("worker.implement")
	require.NoError(t, err)
	require.Contains(t, ancestors, "sandbox.work_env")
	require.Contains(t, ancestors, "sandbox.sec_audit")

	dynAncestors, err := cfg.GetAncestors("dynamic_worker.sweep")
	require.NoError(t, err)
	require.Contains(t, dynAncestors, "sandbox.sec_audit")
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
go_tool "finish" {
  description = "Finish specification"
  source = <<-GO
    import "context"
    type Input struct {
      Summary string `+"`json:\"summary\"`"+`
    }
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      out := Output("done")
      return ToolResponse[Output]{Accepted: true, Output: &out}, nil
    }
  GO
}

worker "specify" {
  based_source      = "main"
  model             = "test-model"
  system_prompt     = "specify"
  terminate_tool_id = go_tool.finish.id
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
			return block.WorkerConfig().DeferredExpression
		}
	}
	return ""
}

func TestWorkerToolAndMCPRegistrationChecks(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
	}{
		{
			name: "unregistered tool_ids",
			hcl: workerInfrastructure + `
worker "w" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "sys"
  tool_ids      = ["tool_go_tool_missing_123"]
}`,
		},
		{
			name: "unregistered terminate_tool_id",
			hcl: workerInfrastructure + `
worker "w" {
  based_source      = "main"
  model             = "test-model"
  system_prompt     = "sys"
  terminate_tool_id = "tool_go_tool_missing_123"
}`,
		},
		{
			name: "unregistered tool_use tool_id",
			hcl: workerInfrastructure + `
worker "w" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "sys"
  tool_use "u" {
    tool_id = "tool_go_tool_missing_123"
  }
}`,
		},
		{
			name: "unregistered qc tool_ids",
			hcl: workerInfrastructure + `
worker "w" {
  based_source  = "main"
  model             = "test-model"
  system_prompt = "sys"
  qc {
    criteria = { a = "b" }
    tool_ids = ["tool_go_tool_missing_123"]
  }
}`,
		},
		{
			name: "unregistered mcp_tool_ids",
			hcl: workerInfrastructure + `
worker "w" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "sys"
  mcp_tool_ids  = ["mcp_tool_missing_123"]
}`,
		},
		{
			name: "unregistered mcp_resource_ids",
			hcl: workerInfrastructure + `
worker "w" {
  based_source     = "main"
  model            = "test-model"
  system_prompt    = "sys"
  mcp_resource_ids = ["mcp_resource_missing_123"]
}`,
		},
		{
			name: "tool_use input unknown field fails InputConstraint",
			hcl: workerInfrastructure + `
external_tool "check" {
  description = "check"
  program     = ["check"]
  input_type  = object({ target = string })
  output_type = string
}

worker "w" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "sys"
  tool_use "u" {
    tool_id = external_tool.check.id
    input = {
      typo = "x"
    }
  }
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.hcl)
			cfg := newTestPlanner(t, dir, nil)
			initPlanner(t, cfg, dir)
			require.Error(t, cfg.RunPlan())
		})
	}
}

func TestWorkerSourceURLResolutionAndMultiSourceAmbiguity(t *testing.T) {
	multiDir := t.TempDir()
	writePlannerFile(t, filepath.Join(multiDir, "workflow.sofa.hcl"), workerInfrastructure+`
source "one" {
  url    = "https://github.com/example/one.git"
  branch = "main"
}

source "two" {
  url    = "https://github.com/example/two.git"
  branch = "main"
}

worker "ambiguous" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "sys"
}
`)
	cfg := newTestPlanner(t, multiDir, nil)
	initPlanner(t, cfg, multiDir)
	require.ErrorContains(t, cfg.RunPlan(), "source")

	chainDir := t.TempDir()
	writePlannerFile(t, filepath.Join(chainDir, "workflow.sofa.hcl"), workerInfrastructure+`
source "one" {
  url    = "https://github.com/example/one.git"
  branch = "main"
}

source "two" {
  url    = "https://github.com/example/two.git"
  branch = "main"
}

locals {
  base_branch = source.two.branch
  next_branch = worker.specify.work_branch
}

worker "specify" {
  based_source  = local.base_branch
  model         = "test-model"
  system_prompt = "specify"
}

worker "plan" {
  based_source  = local.next_branch
  model         = "test-model"
  system_prompt = "plan"
}
`)
	cfg = newTestPlanner(t, chainDir, nil)
	initPlanner(t, cfg, chainDir)
	require.NoError(t, cfg.RunPlan())
	for _, vertex := range cfg.GetVertices() {
		if wb, ok := vertex.(*workerspec.WorkerBlock); ok {
			require.Equal(t, "https://github.com/example/two.git", wb.WorkerConfig().SourceURL)
		}
	}
}

func TestWorkerFallbackLocalsTransitivityAndForEachInstances(t *testing.T) {
	// Locals transitive positive ancestry must succeed.
	localsDir := t.TempDir()
	writePlannerFile(t, filepath.Join(localsDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "tasks" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "tasks"
}

locals {
  tasks_branch = worker.tasks.work_branch
}

worker "implement" {
  based_source  = local.tasks_branch
  model         = "test-model"
  system_prompt = "implement"

  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.tasks
  }
}
`)
	cfg := newTestPlanner(t, localsDir, nil)
	initPlanner(t, cfg, localsDir)
	require.NoError(t, cfg.RunPlan())

	// Target attribute suffix must fail.
	attrSuffixDir := t.TempDir()
	writePlannerFile(t, filepath.Join(attrSuffixDir, "workflow.sofa.hcl"), workerInfrastructure+`
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
    on     = ["qc_exhausted"]
    target = worker.tasks.work_branch
  }
}
`)
	cfg = newTestPlanner(t, attrSuffixDir, nil)
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), attrSuffixDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	if err == nil {
		err = cfg.RunPlan()
	}
	require.Error(t, err)

	// for_each instance fallback: linux is ancestor, windows is NOT.
	forEachOKDir := t.TempDir()
	writePlannerFile(t, filepath.Join(forEachOKDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "matrix" {
  for_each      = toset(["linux", "windows"])
  based_source  = "main"
  model         = "test-model"
  system_prompt = "matrix ${each.key}"
}

worker "implement" {
  based_source  = worker.matrix["linux"].work_branch
  model         = "test-model"
  system_prompt = "implement"

  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.matrix["linux"]
  }
}
`)
	cfg = newTestPlanner(t, forEachOKDir, nil)
	initPlanner(t, cfg, forEachOKDir)
	require.NoError(t, cfg.RunPlan())

	// depends_on positive ancestry must succeed.
	dependsOnDir := t.TempDir()
	writePlannerFile(t, filepath.Join(dependsOnDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "tasks" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "tasks"
}

worker "implement" {
  depends_on    = [worker.tasks]
  based_source  = "main"
  model         = "test-model"
  system_prompt = "implement"

  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.tasks
  }
}
`)
	cfg = newTestPlanner(t, dependsOnDir, nil)
	initPlanner(t, cfg, dependsOnDir)
	require.NoError(t, cfg.RunPlan())

	// Non-worker intermediate block transitive ancestry (worker.tasks -> preserve.checkpoint -> worker.implement) must succeed.
	nonWorkerTransitiveDir := t.TempDir()
	writePlannerFile(t, filepath.Join(nonWorkerTransitiveDir, "workflow.sofa.hcl"), workerInfrastructure+`
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

worker "tasks" {
  based_source  = source.project.branch
  model         = "test-model"
  system_prompt = "tasks"
}

preserve "checkpoint" {
  branch = worker.tasks.work_branch
}

worker "implement" {
  depends_on    = [preserve.checkpoint]
  based_source  = source.project.branch
  model         = "test-model"
  system_prompt = "implement"

  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.tasks
  }
}
`)
	cfg = newTestPlanner(t, nonWorkerTransitiveDir, nil)
	initPlanner(t, cfg, nonWorkerTransitiveDir)
	require.NoError(t, cfg.RunPlan())

	// for_each instance fallback: linux is ancestor, windows is NOT.
	forEachBadDir := t.TempDir()
	writePlannerFile(t, filepath.Join(forEachBadDir, "workflow.sofa.hcl"), workerInfrastructure+`
worker "matrix" {
  for_each      = toset(["linux", "windows"])
  based_source  = "main"
  model         = "test-model"
  system_prompt = "matrix ${each.key}"
}

worker "implement" {
  based_source  = worker.matrix["linux"].work_branch
  model         = "test-model"
  system_prompt = "implement"

  fallback "rework" {
    on     = ["qc_exhausted"]
    target = worker.matrix["windows"]
  }
}
`)
	cfg = newTestPlanner(t, forEachBadDir, nil)
	loaded, diags, err = config.LoadDirectoryContext(context.Background(), forEachBadDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	if err == nil {
		err = cfg.RunPlan()
	}
	require.Error(t, err)
}

func TestWorkerDynamicGatekeeperAndFourStageEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}
harness "antigravity" {}

model_provider "openai" {
  type     = "openai"
  endpoint = "https://api.example.com/v1"
}

mcp_server "docs" {
  tools     = ["search"]
  resources = ["docs://readme"]
  http {
    url = "https://mcp.example.com/mcp"
  }
}

go_tool "verify_done" {
  description = "Verify completion"
  source = <<-GO
    import "context"
    type Input struct {
      Note string `+"`json:\"note\"`"+`
    }
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      out := Output("ok")
      return ToolResponse[Output]{Accepted: true, Output: &out}, nil
    }
  GO
}

worker "specify" {
  based_source  = source.project.branch
  harness       = harness.copilot
  model         = "gpt-5"
  system_prompt = "Write spec"

  artifact "spec" {
    type        = "file"
    path        = "docs/spec.md"
    description = "spec"
  }
}

worker "plan" {
  based_source  = worker.specify.work_branch
  harness       = harness.copilot
  model         = "gpt-5"
  system_prompt = "Write plan"
  prompt        = "Read ${import_artifact.spec.path} and write ${artifact("design").path}"

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

worker "tasks" {
  based_source  = worker.plan.work_branch
  harness       = harness.copilot
  model         = "gpt-5"
  system_prompt = "Write tasks"
  prompt        = "Read ${import_artifact.design.path}"

  import_artifact "design" {
    desc    = "approved design"
    sources = [worker.plan.artifact.design]
  }
}

worker "implement" {
  based_source            = worker.tasks.work_branch
  harness                 = harness.antigravity
  model_provider          = null
  model                   = "gemini-2.5-pro"
  system_prompt           = "Implement tasks"
  prompt                  = "Read ${import_artifact.design.path}"
  mcp_tool_ids            = [mcp_server.docs.tool_ids["search"]]
  mcp_resource_ids        = [mcp_server.docs.resource_ids["docs://readme"]]
  tool_call_quota         = { bash = 50 }
  checkpoint_every_rounds = 5
  max_protocol_attempts   = 10

  tool_use "done" {
    tool_id   = go_tool.verify_done.id
    terminate = true
    input = {
      note = "final"
    }
  }

  import_artifact "design" {
    desc    = "design"
    sources = [worker.plan.artifact.design]
  }

  artifact "test_report" {
    type        = "file"
    path        = "reports/test.json"
    description = "test summary"
  }

  dynamic "gatekeeper" {
    for_each = toset(["unit"])
    content "unit" {
      command = ["go", "test", "./..."]
    }
  }

  retry {
    lifecycle_retries  = 3
    model_call_retries = 2
    interval_seconds   = 5
  }

  qc {
    harness        = harness.copilot
    model_provider = model_provider.openai
    model          = "gpt-5"
    criteria       = { tests = "all tests pass" }
  }

  fallback "rework_tasks" {
    on        = ["qc_exhausted", "gatekeeper_exhausted"]
    target    = worker.tasks
    artifacts = [artifact("test_report")]
  }
}

preserve "final_impl" {
  branch = worker.implement.work_branch
}

output "final_result" {
  value = worker.implement.result
}
`)
	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var impl *workerspec.WorkerBlock
	for _, vertex := range cfg.GetVertices() {
		if wb, ok := vertex.(*workerspec.WorkerBlock); ok && wb.Name() == "implement" {
			impl = wb
		}
	}
	require.NotNil(t, impl)
	wc := impl.WorkerConfig()
	require.Equal(t, "https://github.com/example/sofa.git", wc.SourceURL)
	require.Len(t, wc.Gatekeepers, 1)
	require.Equal(t, "unit", wc.Gatekeepers[0].Name)
	require.NotNil(t, wc.QC)
	require.True(t, wc.QC.ModelProviderSet)
	require.Len(t, wc.Fallbacks, 1)
	require.Equal(t, []string{"test_report"}, wc.Fallbacks[0].Artifacts)
	require.NotEmpty(t, wc.DeferredExpression)
}
