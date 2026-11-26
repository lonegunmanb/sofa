package planner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lonegunmanb/golden"
	mergespec "github.com/lonegunmanb/sofa/internal/merge/spec"
	"github.com/lonegunmanb/sofa/internal/module"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestModuleCrossModuleUnknownAndArtifactRelay(t *testing.T) {
	rootDir := t.TempDir()
	auditDir := filepath.Join(rootDir, "modules", "audit")
	require.NoError(t, os.MkdirAll(auditDir, 0o755))

	writePlannerFile(t, filepath.Join(auditDir, "audit.sofa.hcl"), `
variable "branch" {
  type = string
}

variable "spec_artifact" {
  type = object({
    id          = string
    name        = string
    kind        = string
    type        = string
    path        = string
    description = string
    required    = bool
    non_empty   = bool
  })
}

variable "summary_json" {
  type = string
}

variable "label" {
  type = string
}

go_tool "discover_checks" {
  description = "Discover checks"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

worker "review" {
  based_source      = var.branch
  model             = "test-model"
  system_prompt     = "Review ${var.label} using ${import_artifact.spec.path}"
  prompt            = var.summary_json
  terminate_tool_id = go_tool.discover_checks.id

  import_artifact "spec" {
    desc    = "Specification from parent"
    sources = [var.spec_artifact]
  }
}

dynamic_worker "checks" {
  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.review.result,
      object({ checks = map(string) })
    ).checks
    iterator = check

    content {
      based_source  = worker.review.work_branch
      model         = "test-model"
      system_prompt = "Run check ${check.key}"
      prompt        = check.value
    }
  }
}

merge "integrate" {
  based_source = worker.review.work_branch
  sources      = dynamic_worker.checks.work_branches

  gatekeeper "unit" {
    command = ["go", "test", "./..."]
  }
}

output "verified_branch" {
  value = merge.integrate.work_branch
}
`)

	writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

go_tool "emit_spec" {
  description = "Emit spec"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

worker "specify" {
  based_source      = source.project.branch
  model             = "test-model"
  system_prompt     = "Specify"
  terminate_tool_id = go_tool.emit_spec.id

  artifact "spec" {
    type        = "file"
    path        = "docs/spec.md"
    description = "Spec"
  }
}

module "audit" {
  source      = "./modules/audit"
  parallelism = 2
  timeout     = "30m"

  branch        = worker.specify.work_branch
  spec_artifact = worker.specify.artifact.spec
  summary_json  = worker.specify.result
  label         = "security"
}

preserve "final" {
  branch = module.audit.verified_branch
}

output "final_branch" {
  value = module.audit.verified_branch
}
`)

	cfg := newTestPlanner(t, rootDir, nil)
	initPlanner(t, cfg, rootDir)
	require.NoError(t, cfg.RunPlan())

	// Verify DAG edges: worker.specify -> module.audit -> preserve.final
	auditAncestors, err := cfg.GetAncestors("module.audit")
	require.NoError(t, err)
	require.Contains(t, auditAncestors, "worker.specify")

	preserveAncestors, err := cfg.GetAncestors("preserve.final")
	require.NoError(t, err)
	require.Contains(t, preserveAncestors, "module.audit")

	outputs := cfg.Outputs()
	require.Len(t, outputs, 1)
	require.Equal(t, "final_branch", outputs[0].Name)
	require.False(t, outputs[0].Value.IsKnown())
	require.True(t, outputs[0].Value.Type().Equals(cty.String))

	plannedModules := cfg.PlannedModules()
	require.Contains(t, plannedModules, "module.audit")
	mp := plannedModules["module.audit"]
	require.Equal(t, "module.audit", mp.Address)
	require.Equal(t, "audit", mp.Name)
	require.Equal(t, "module.audit", mp.CanonicalAddress)
	require.Equal(t, "./modules/audit", mp.Source)
	require.NotNil(t, mp.Parallelism)
	require.Equal(t, 2, *mp.Parallelism)
	require.NotNil(t, mp.Timeout)
	require.Equal(t, 30*time.Minute, *mp.Timeout)
	require.Equal(t, "30m", mp.TimeoutRaw)
	require.Equal(t, "worker.specify.work_branch", mp.InputExprs["branch"])
	require.NotNil(t, mp.ChildConfig)

	childCfg, ok := mp.ChildConfig.(*SofaConfig)
	require.True(t, ok)
	childMerge := golden.Blocks[*mergespec.MergeBlock](childCfg)[0]
	require.Equal(t, "https://github.com/example/repo.git", childMerge.MergeConfig().SourceURL)
}

func TestModuleChildVariableValidationMatrix(t *testing.T) {
	t.Run("defaults_nullable_sensitivity_isolation_and_deferred_validation", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		// Host env var & child varfile must NOT leak into child module.
		t.Setenv("SOFA_VAR_from_default", "leaked-from-env")
		writePlannerFile(t, filepath.Join(childDir, "sofa.sofavars"), `from_default = "leaked-from-varfile"`)
		writePlannerFile(t, filepath.Join(childDir, "extra.auto.sofavars"), `from_default = "leaked-from-auto"`)

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
variable "from_default" {
  type    = string
  default = "child-default"
}

variable "nullable_with_default" {
  type     = string
  default  = "fallback"
  nullable = true
}

variable "nullable_no_default" {
  type     = string
  nullable = true
}

variable "sensitive_var" {
  type      = string
  sensitive = true
}

variable "caller_marked" {
  type = string
}

variable "unknown_branch" {
  type = string

  validation {
    condition     = length(var.unknown_branch) > 3
    error_message = "branch name too short"
  }
}

variable "unknown_cfg" {
  type = object({
    branch  = string
    retries = optional(number, 3)
  })

  validation {
    condition     = var.unknown_cfg.retries >= 1 && var.unknown_cfg.branch != ""
    error_message = "invalid unknown_cfg"
  }
}

output "out_default" {
  value = var.from_default
}

output "out_nullable_default" {
  value = var.nullable_with_default
}

output "out_nullable_null" {
  value = var.nullable_no_default
}

output "out_sensitive" {
  value = "prefix-${var.sensitive_var}"
}

output "out_caller_marked" {
  value = "caller-${var.caller_marked}"
}

output "out_retries" {
  value = var.unknown_cfg.retries
}
`)

		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

variable "root_secret" {
  type      = string
  default   = "s3cr3t"
  sensitive = true
}

worker "prep" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "prep"
}

module "child" {
  source                = "./child"
  nullable_with_default = null
  nullable_no_default   = null
  sensitive_var         = "plain-value"
  caller_marked         = var.root_secret
  unknown_branch        = worker.prep.work_branch
  unknown_cfg = {
    branch = worker.prep.work_branch
  }
}

output "all" {
  value = {
    def      = module.child.out_default
    nul_def  = module.child.out_nullable_default
    nul_null = module.child.out_nullable_null
    retries  = module.child.out_retries
  }
}

output "sec1" {
  value = module.child.out_sensitive
}

output "sec2" {
  value = module.child.out_caller_marked
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())

		outputs := map[string]cty.Value{}
		for _, o := range cfg.Outputs() {
			outputs[o.Name] = o.Value
		}
		all := outputs["all"]
		require.Equal(t, "child-default", all.GetAttr("def").AsString())
		require.Equal(t, "fallback", all.GetAttr("nul_def").AsString())
		require.True(t, all.GetAttr("nul_null").IsNull())
		retries, _ := all.GetAttr("retries").AsBigFloat().Int64()
		require.Equal(t, int64(3), retries)

		require.True(t, specpkg.IsSensitive(outputs["sec1"]))
		require.True(t, specpkg.IsSensitive(outputs["sec2"]))

		mp := cfg.PlannedModules()["module.child"]
		require.Contains(t, mp.DeferredValidations, "unknown_branch")
		require.Len(t, mp.DeferredValidations["unknown_branch"], 1)
		require.Equal(t, "branch name too short", mp.DeferredValidations["unknown_branch"][0].ErrorMessage)
		require.Contains(t, mp.DeferredValidations, "unknown_cfg")
		require.Len(t, mp.DeferredValidations["unknown_cfg"], 1)
	})

	tests := []struct {
		name     string
		childHCL string
		rootHCL  string
		wantErr  string
	}{
		{
			name: "undeclared input rejected",
			childHCL: `
variable "expected" {
  type = string
}
`,
			rootHCL: `
module "child" {
  source   = "./child"
  expected = "ok"
  extra    = "nope"
}
`,
			wantErr: "extra",
		},
		{
			name: "missing required variable rejected",
			childHCL: `
variable "required_input" {
  type = string
}
`,
			rootHCL: `
module "child" {
  source = "./child"
}
`,
			wantErr: "required_input",
		},
		{
			name: "nullable false rejects null",
			childHCL: `
variable "strict" {
  type     = string
  nullable = false
}
`,
			rootHCL: `
module "child" {
  source = "./child"
  strict = null
}
`,
			wantErr: "null",
		},
		{
			name: "incompatible unknown type rejected",
			childHCL: `
variable "count_num" {
  type = number
}
`,
			rootHCL: `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}
harness "local" {}
worker "w" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
dynamic_worker "dw" {
  dynamic "worker" {
    for_each = toset(["a"])
    content {
      based_source  = worker.w.work_branch
      model         = "m"
      system_prompt = "s"
    }
  }
}
module "child" {
  source    = "./child"
  count_num = dynamic_worker.dw.work_branches
}
`,
			wantErr: "count_num",
		},
		{
			name: "known validation failure fails during plan",
			childHCL: `
variable "name" {
  type = string
  validation {
    condition     = length(var.name) > 5
    error_message = "name must exceed 5 chars"
  }
}
`,
			rootHCL: `
module "child" {
  source = "./child"
  name   = "abc"
}
`,
			wantErr: "name must exceed 5 chars",
		},
		{
			name: "unknown input with known false validation condition fails immediately",
			childHCL: `
variable "cfg" {
  type = object({
    branch  = string
    retries = optional(number, 0)
  })
  validation {
    condition     = var.cfg.retries >= 1
    error_message = "retries must be >= 1"
  }
}
`,
			rootHCL: `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}
harness "local" {}
worker "w" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
module "child" {
  source = "./child"
  cfg = {
    branch = worker.w.work_branch
  }
}
`,
			wantErr: "retries must be >= 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootDir := t.TempDir()
			childDir := filepath.Join(rootDir, "child")
			require.NoError(t, os.MkdirAll(childDir, 0o755))
			writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), tt.childHCL)
			writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), tt.rootHCL)

			cfg := newTestPlanner(t, rootDir, nil)
			initPlanner(t, cfg, rootDir)
			err := cfg.RunPlan()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestModuleOrthogonalSingletonInheritanceMatrix(t *testing.T) {
	t.Run("inherit_all_three_and_model_provider_null_opt_out", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
worker "impl" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "impl"
}

worker "native" {
  based_source   = "main"
  model          = "test-model"
  model_provider = null
  system_prompt  = "native"
}

dynamic_worker "checks" {
  dynamic "worker" {
    for_each = toset(["a"])
    content {
      based_source  = worker.impl.work_branch
      model         = "test-model"
      system_prompt = "check"
    }
  }
}

merge "integrate" {
  based_source = worker.impl.work_branch
  sources      = dynamic_worker.checks.work_branches

  gatekeeper "unit" {
    command = ["go", "test", "./..."]
  }
}

output "done" {
  value = merge.integrate.work_branch
}
`)

		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

model_provider "openrouter" {
  type        = "openai"
  endpoint    = "https://openrouter.ai/api/v1"
  api_key_ref = "OPENROUTER_API_KEY"
}

module "child" {
  source = "./child"
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())

		childCfg := cfg.PlannedModules()["module.child"].ChildConfig.(*SofaConfig)
		workers := map[string]*workerspec.WorkerBlock{}
		for _, wb := range golden.Blocks[*workerspec.WorkerBlock](childCfg) {
			workers[wb.Name()] = wb
		}
		impl := workers["impl"].WorkerConfig()
		require.Equal(t, "sandbox.go_env", impl.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "harness.copilot", impl.Harness.GetAttr("address").AsString())
		require.Equal(t, "model_provider.openrouter", impl.ModelProvider.GetAttr("address").AsString())

		native := workers["native"].WorkerConfig()
		require.Equal(t, "sandbox.go_env", native.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "harness.copilot", native.Harness.GetAttr("address").AsString())
		require.True(t, native.ModelProvider.IsNull())

		dw := golden.Blocks[*workerspec.DynamicWorkerBlock](childCfg)[0].DynamicWorkerConfig()
		require.Equal(t, "sandbox.go_env", dw.Content.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "harness.copilot", dw.Content.Harness.GetAttr("address").AsString())
		require.Equal(t, "model_provider.openrouter", dw.Content.ModelProvider.GetAttr("address").AsString())

		mg := golden.Blocks[*mergespec.MergeBlock](childCfg)[0].MergeConfig()
		require.Equal(t, "sandbox.go_env", mg.Sandbox.GetAttr("address").AsString())
	})

	t.Run("nested_override_harness_orthogonally_inherits_root_sandbox", func(t *testing.T) {
		rootDir := t.TempDir()
		outerDir := filepath.Join(rootDir, "outer")
		innerDir := filepath.Join(outerDir, "inner")
		require.NoError(t, os.MkdirAll(innerDir, 0o755))

		writePlannerFile(t, filepath.Join(innerDir, "inner.sofa.hcl"), `
worker "leaf" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "leaf"
}
`)
		writePlannerFile(t, filepath.Join(outerDir, "outer.sofa.hcl"), `
harness "alt" {}

worker "mid" {
  based_source  = "main"
  model         = "test-model"
  system_prompt = "mid"
}

module "inner" {
  source = "./inner"
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "root_harness" {}

module "outer" {
  source = "./outer"
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())

		outerCfg := cfg.PlannedModules()["module.outer"].ChildConfig.(*SofaConfig)
		midWorker := golden.Blocks[*workerspec.WorkerBlock](outerCfg)[0].WorkerConfig()
		require.Equal(t, "sandbox.go_env", midWorker.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "module.outer.harness.alt", midWorker.Harness.GetAttr("address").AsString())
		require.True(t, midWorker.ModelProvider.IsNull())

		innerCfg := outerCfg.PlannedModules()["module.inner"].ChildConfig.(*SofaConfig)
		leafWorker := golden.Blocks[*workerspec.WorkerBlock](innerCfg)[0].WorkerConfig()
		require.Equal(t, "sandbox.go_env", leafWorker.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "module.outer.harness.alt", leafWorker.Harness.GetAttr("address").AsString())
		require.True(t, leafWorker.ModelProvider.IsNull())
	})

	t.Run("ambiguity_and_missing_diagnostics", func(t *testing.T) {
		tests := []struct {
			name     string
			rootHCL  string
			childHCL string
			wantErrs []string
		}{
			{
				name: "ambiguous sandbox in root",
				rootHCL: `
sandbox "s1" {
  runtime = "podman"
  image   = "img:1"
}
sandbox "s2" {
  runtime = "podman"
  image   = "img:2"
}
harness "h1" {}
module "audit" { source = "./child" }
`,
				childHCL: `
worker "check" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
`,
				wantErrs: []string{"module.audit.worker.check", "multiple sandbox", "s1", "s2"},
			},
			{
				name: "ambiguous harness in root",
				rootHCL: `
sandbox "s1" {
  runtime = "podman"
  image   = "img:1"
}
harness "h1" {}
harness "h2" {}
module "audit" { source = "./child" }
`,
				childHCL: `
worker "check" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
`,
				wantErrs: []string{"module.audit.worker.check", "multiple harness", "h1", "h2"},
			},
			{
				name: "ambiguous model_provider in root",
				rootHCL: `
sandbox "s1" {
  runtime = "podman"
  image   = "img:1"
}
harness "h1" {}
model_provider "p1" {
  type        = "openai"
  endpoint    = "https://a.example/v1"
  api_key_ref = "K1"
}
model_provider "p2" {
  type        = "openai"
  endpoint    = "https://b.example/v1"
  api_key_ref = "K2"
}
module "audit" { source = "./child" }
`,
				childHCL: `
worker "check" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
`,
				wantErrs: []string{"module.audit.worker.check", "multiple model_provider", "p1", "p2"},
			},
			{
				name: "missing sandbox across modules",
				rootHCL: `
harness "h1" {}
module "audit" { source = "./child" }
`,
				childHCL: `
worker "check" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
`,
				wantErrs: []string{"module.audit.worker.check", "sandbox"},
			},
			{
				name: "missing harness across modules",
				rootHCL: `
sandbox "s1" {
  runtime = "podman"
  image   = "img:1"
}
module "audit" { source = "./child" }
`,
				childHCL: `
worker "check" {
  based_source  = "main"
  model         = "m"
  system_prompt = "s"
}
`,
				wantErrs: []string{"module.audit.worker.check", "harness"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rootDir := t.TempDir()
				childDir := filepath.Join(rootDir, "child")
				require.NoError(t, os.MkdirAll(childDir, 0o755))
				writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), tt.childHCL)
				writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), tt.rootHCL)

				cfg := newTestPlanner(t, rootDir, nil)
				initPlanner(t, cfg, rootDir)
				err := cfg.RunPlan()
				require.Error(t, err)
				for _, want := range tt.wantErrs {
					require.Contains(t, err.Error(), want)
				}
			})
		}
	})

	t.Run("ambiguous_root_model_provider_allows_explicit_null_opt_out_in_child", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))
		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
worker "check" {
  based_source   = "main"
  model          = "m"
  model_provider = null
  system_prompt  = "s"
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "s1" {
  runtime = "podman"
  image   = "img:1"
}
harness "h1" {}
model_provider "p1" {
  type        = "openai"
  endpoint    = "https://a.example/v1"
  api_key_ref = "K1"
}
model_provider "p2" {
  type        = "openai"
  endpoint    = "https://b.example/v1"
  api_key_ref = "K2"
}
module "audit" { source = "./child" }
`)
		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())
	})
}

func TestModuleCrossModuleSourceURLLineage(t *testing.T) {
	t.Run("same_repo_across_module_succeeds", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
variable "in_branch" {
  type = string
}

worker "review" {
  based_source  = var.in_branch
  model         = "m"
  system_prompt = "review"
}

output "verified_branch" {
  value = worker.review.work_branch
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "repo_a" {
  url    = "https://github.com/example/repo-a.git"
  branch = "main"
}

source "repo_b" {
  url    = "https://github.com/example/repo-b.git"
  branch = "main"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
harness "h" {}

worker "a" {
  based_source  = source.repo_a.branch
  model         = "m"
  system_prompt = "a"
}

module "audit" {
  source    = "./child"
  in_branch = worker.a.work_branch
}

merge "final" {
  based_source = source.repo_a.branch
  sources      = [module.audit.verified_branch]
}
`)
		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())
		mg := golden.Blocks[*mergespec.MergeBlock](cfg)[0].MergeConfig()
		require.Equal(t, "https://github.com/example/repo-a.git", mg.SourceURL)
	})

	t.Run("mixed_repos_across_module_rejected", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
variable "in_branch" {
  type = string
}

worker "review" {
  based_source  = var.in_branch
  model         = "m"
  system_prompt = "review"
}

output "verified_branch" {
  value = worker.review.work_branch
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "repo_a" {
  url    = "https://github.com/example/repo-a.git"
  branch = "main"
}

source "repo_b" {
  url    = "https://github.com/example/repo-b.git"
  branch = "main"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
harness "h" {}

worker "a" {
  based_source  = source.repo_a.branch
  model         = "m"
  system_prompt = "a"
}

module "audit" {
  source    = "./child"
  in_branch = worker.a.work_branch
}

merge "final" {
  based_source = source.repo_b.branch
  sources      = [module.audit.verified_branch]
}
`)
		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		err := cfg.RunPlan()
		require.ErrorContains(t, err, "multiple distinct source repositories")
	})
}

func TestModuleCanonicalAddressToolAndMCPCollisionPrevention(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "shared")
	require.NoError(t, os.MkdirAll(childDir, 0o755))

	writePlannerFile(t, filepath.Join(childDir, "shared.sofa.hcl"), `
go_tool "check" {
  description = "Check"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "lint" {
  description = "Lint"
  program     = ["lint"]
  input_type  = object({})
  output_type = string
}

mcp_server "docs" {
  tools     = ["search"]
  resources = ["docs://index"]
  http {
    url = "https://mcp.example.com"
  }
}

output "go_id" {
  value = go_tool.check.id
}

output "go_name" {
  value = tool_name(go_tool.check)
}

output "ext_id" {
  value = external_tool.lint.id
}

output "mcp_tool_id" {
  value = mcp_server.docs.tool_ids["search"]
}

output "mcp_res_id" {
  value = mcp_server.docs.resource_ids["docs://index"]
}
`)

	writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
module "a" {
  source = "./shared"
}

module "b" {
  source = "./shared"
}

output "a_ids" {
  value = {
    go_id       = module.a.go_id
    go_name     = module.a.go_name
    ext_id      = module.a.ext_id
    mcp_tool_id = module.a.mcp_tool_id
    mcp_res_id  = module.a.mcp_res_id
  }
}

output "b_ids" {
  value = {
    go_id       = module.b.go_id
    go_name     = module.b.go_name
    ext_id      = module.b.ext_id
    mcp_tool_id = module.b.mcp_tool_id
    mcp_res_id  = module.b.mcp_res_id
  }
}
`)

	cfg := newTestPlanner(t, rootDir, nil)
	initPlanner(t, cfg, rootDir)
	require.NoError(t, cfg.RunPlan())

	outputs := map[string]cty.Value{}
	for _, o := range cfg.Outputs() {
		outputs[o.Name] = o.Value
	}
	a := outputs["a_ids"]
	b := outputs["b_ids"]

	require.Equal(t, a.GetAttr("go_id").AsString(), a.GetAttr("go_name").AsString())
	require.Equal(t, b.GetAttr("go_id").AsString(), b.GetAttr("go_name").AsString())
	require.NotEqual(t, a.GetAttr("go_id").AsString(), b.GetAttr("go_id").AsString())
	require.NotEqual(t, a.GetAttr("ext_id").AsString(), b.GetAttr("ext_id").AsString())
	require.NotEqual(t, a.GetAttr("mcp_tool_id").AsString(), b.GetAttr("mcp_tool_id").AsString())
	require.NotEqual(t, a.GetAttr("mcp_res_id").AsString(), b.GetAttr("mcp_res_id").AsString())
}

func TestModuleBundlePathAndModulesDirectory(t *testing.T) {
	t.Run("initialized_modules_directory_and_bundle_path", func(t *testing.T) {
		srcDir := t.TempDir()
		auditSrc := filepath.Join(srcDir, "audit")
		nestedSrc := filepath.Join(auditSrc, "nested")
		require.NoError(t, os.MkdirAll(nestedSrc, 0o755))

		writePlannerFile(t, filepath.Join(nestedSrc, "nested.sofa.hcl"), `
output "skills_dir" {
  value = "${path.module}/skills"
}
`)
		writePlannerFile(t, filepath.Join(auditSrc, "audit.sofa.hcl"), `
module "nested" {
  source = "./nested"
}

output "skills_dir" {
  value = "${path.module}/skills"
}

output "nested_skills_dir" {
  value = module.nested.skills_dir
}
`)
		writePlannerFile(t, filepath.Join(srcDir, "workflow.sofa.hcl"), `
module "audit" {
  source = "./audit"
}

output "root_skills" {
  value = "${path.module}/skills"
}

output "audit_skills" {
  value = module.audit.skills_dir
}

output "nested_skills" {
  value = module.audit.nested_skills_dir
}
`)

		modulesDir := t.TempDir()
		require.NoError(t, module.Init(context.Background(), srcDir, module.InitOptions{
			ModulesDirectory: modulesDir,
		}))

		cfg := NewSofaConfig(ConfigArgs{
			Context:          context.Background(),
			Directory:        srcDir,
			ModulesDirectory: modulesDir,
			BundlePath:       "config",
		})
		initPlanner(t, cfg, srcDir)
		require.NoError(t, cfg.RunPlan())

		outputs := map[string]string{}
		for _, o := range cfg.Outputs() {
			outputs[o.Name] = o.Value.AsString()
		}
		require.Equal(t, "config/skills", outputs["root_skills"])
		require.Equal(t, "modules/audit/skills", outputs["audit_skills"])
		require.Equal(t, "modules/audit/nested/skills", outputs["nested_skills"])
	})

	t.Run("missing_uninitialized_module_error", func(t *testing.T) {
		rootDir := t.TempDir()
		modulesDir := t.TempDir()
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
module "audit" {
  source = "./audit"
}
`)
		cfg := NewSofaConfig(ConfigArgs{
			Context:          context.Background(),
			Directory:        rootDir,
			ModulesDirectory: modulesDir,
		})
		initPlanner(t, cfg, rootDir)
		err := cfg.RunPlan()
		require.ErrorContains(t, err, "sofa init")
		require.ErrorContains(t, err, "module.audit")
	})

	t.Run("remote_source_without_modules_dir_requires_init", func(t *testing.T) {
		rootDir := t.TempDir()
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
module "audit" {
  source = "github.com/example/audit"
}
`)
		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		err := cfg.RunPlan()
		require.ErrorContains(t, err, "sofa init")
	})

	t.Run("local_directory_cycle_detected", func(t *testing.T) {
		rootDir := t.TempDir()
		aDir := filepath.Join(rootDir, "a")
		bDir := filepath.Join(rootDir, "b")
		require.NoError(t, os.MkdirAll(aDir, 0o755))
		require.NoError(t, os.MkdirAll(bDir, 0o755))

		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
module "a" {
  source = "./a"
}
`)
		writePlannerFile(t, filepath.Join(aDir, "a.sofa.hcl"), `
module "b" {
  source = "../b"
}
`)
		writePlannerFile(t, filepath.Join(bDir, "b.sofa.hcl"), `
module "a" {
  source = "../a"
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		err := cfg.RunPlan()
		require.ErrorContains(t, err, "cycle")
	})
}

func TestModuleForEachDependsOnAndPrecondition(t *testing.T) {
	rootDir := t.TempDir()
	regionDir := filepath.Join(rootDir, "region")
	require.NoError(t, os.MkdirAll(regionDir, 0o755))

	writePlannerFile(t, filepath.Join(regionDir, "region.sofa.hcl"), `
variable "region_name" {
  type = string
}

go_tool "check" {
  description = "Check region"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

worker "run" {
  based_source  = "main"
  model         = "m"
  system_prompt = "run ${var.region_name}"
  tool_ids      = [go_tool.check.id]
}

output "verified_branch" {
  value = worker.run.work_branch
}

output "tool_id" {
  value = go_tool.check.id
}
`)

	writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

worker "prep" {
  based_source  = "main"
  model         = "m"
  system_prompt = "prep"
}

module "region" {
  for_each   = toset(["east", "west"])
  depends_on = [worker.prep]
  source     = "./region"

  region_name = each.key

  precondition {
    condition     = each.key != ""
    error_message = "region key required"
  }

  dynamic "precondition" {
    for_each = ["dynamic check"]
    content {
      condition     = length(each.key) > 1
      error_message = precondition.value
    }
  }
}

module "skipped" {
  for_each = toset([])
  source   = "./region"

  region_name = each.key
}

output "east_tool" {
  value = module.region["east"].tool_id
}

output "west_tool" {
  value = module.region["west"].tool_id
}

output "east_branch" {
  value = module.region["east"].verified_branch
}

output "skipped_count" {
  value = length(keys(module.skipped))
}
`)

	cfg := newTestPlanner(t, rootDir, nil)
	initPlanner(t, cfg, rootDir)
	require.NoError(t, cfg.RunPlan())

	outputs := map[string]cty.Value{}
	for _, o := range cfg.Outputs() {
		outputs[o.Name] = o.Value
	}
	require.NotEqual(t, outputs["east_tool"].AsString(), outputs["west_tool"].AsString())
	require.False(t, outputs["east_branch"].IsKnown())
	cnt, _ := outputs["skipped_count"].AsBigFloat().Int64()
	require.Equal(t, int64(0), cnt)

	planned := cfg.PlannedModules()
	require.Contains(t, planned, "module.region[east]")
	require.Contains(t, planned, "module.region[west]")
	require.Equal(t, "module.region[east]", planned["module.region[east]"].CanonicalAddress)
	require.Equal(t, "module.region[west]", planned["module.region[west]"].CanonicalAddress)

	ancestors, err := cfg.GetAncestors("module.region[east]")
	require.NoError(t, err)
	require.Contains(t, ancestors, "worker.prep")
}

func TestModuleInheritsRootSourceContextAndBranchProtection(t *testing.T) {
	t.Run("single_root_source_inherited_by_child_literal_based_source", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
worker "review" {
  based_source  = "main"
  model         = "m"
  system_prompt = "review"
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}

harness "h" {}

module "audit" {
  source = "./child"
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		require.NoError(t, cfg.RunPlan())

		childCfg := cfg.PlannedModules()["module.audit"].ChildConfig.(*SofaConfig)
		childWorker := golden.Blocks[*workerspec.WorkerBlock](childCfg)[0].WorkerConfig()
		require.Equal(t, "https://github.com/example/repo.git", childWorker.SourceURL)
	})

	t.Run("multiple_root_sources_reject_ambiguous_literal_based_source_in_child", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
worker "review" {
  based_source  = "main"
  model         = "m"
  system_prompt = "review"
}
`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "repo_a" {
  url    = "https://github.com/example/repo-a.git"
  branch = "main"
}

source "repo_b" {
  url    = "https://github.com/example/repo-b.git"
  branch = "main"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}

harness "h" {}

module "audit" {
  source = "./child"
}
`)

		cfg := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfg, rootDir)
		err := cfg.RunPlan()
		require.ErrorContains(t, err, "source blocks exist")
	})

	t.Run("child_merge_target_branch_enforces_root_source_protection", func(t *testing.T) {
		build := func(preserveMain bool) error {
			rootDir := t.TempDir()
			childDir := filepath.Join(rootDir, "child")
			require.NoError(t, os.MkdirAll(childDir, 0o755))

			writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
variable "in_branch" {
  type = string
}

worker "review" {
  based_source  = var.in_branch
  model         = "m"
  system_prompt = "review"
}

merge "integrate" {
  based_source  = var.in_branch
  target_branch = "main"
  sources       = [worker.review.work_branch]
}
`)
			preserveBlock := ""
			if preserveMain {
				preserveBlock = `
preserve "keep_main" {
  url    = source.project.url
  branch = "main"
}
`
			}
			writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}

harness "h" {}

worker "prep" {
  based_source  = source.project.branch
  model         = "m"
  system_prompt = "prep"
}

module "audit" {
  source    = "./child"
  in_branch = worker.prep.work_branch
}
`+preserveBlock)
			cfg := newTestPlanner(t, rootDir, nil)
			initPlanner(t, cfg, rootDir)
			return cfg.RunPlan()
		}

		err := build(false)
		require.ErrorContains(t, err, "without a matching preserve block")
		require.NoError(t, build(true))
	})
}

func TestModuleChildSandboxAndModelProviderCanonicalAddresses(t *testing.T) {
	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))

	writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
sandbox "child_env" {
  runtime = "podman"
  image   = "img:child"
}

harness "child_harness" {}

model_provider "child_llm" {
  type        = "openai"
  endpoint    = "https://child.example/v1"
  api_key_ref = "CHILD_KEY"
}

worker "run" {
  based_source  = "main"
  model         = "m"
  system_prompt = "run"
}
`)
	writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "root_env" {
  runtime = "podman"
  image   = "img:root"
}

harness "root_harness" {}

model_provider "root_llm" {
  type        = "openai"
  endpoint    = "https://root.example/v1"
  api_key_ref = "ROOT_KEY"
}

module "child" {
  source = "./child"
}
`)

	cfg := newTestPlanner(t, rootDir, nil)
	initPlanner(t, cfg, rootDir)
	require.NoError(t, cfg.RunPlan())

	childCfg := cfg.PlannedModules()["module.child"].ChildConfig.(*SofaConfig)
	w := golden.Blocks[*workerspec.WorkerBlock](childCfg)[0].WorkerConfig()
	require.Equal(t, "module.child.sandbox.child_env", w.Sandbox.GetAttr("address").AsString())
	require.Equal(t, "module.child.harness.child_harness", w.Harness.GetAttr("address").AsString())
	require.Equal(t, "module.child.model_provider.child_llm", w.ModelProvider.GetAttr("address").AsString())
}

func TestModuleRejectsUntypedDynamicUnknownAndSupportsWholeUnknownObjectDefaults(t *testing.T) {
	t.Run("untyped_jsondecode_rejected_while_jsondecodewithtype_succeeds", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))

		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `
variable "payload" {
  type = object({
    branch  = string
    retries = optional(number, 3)
  })
}

output "branch" {
  value = var.payload.branch
}
`)

		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
harness "h" {}
go_tool "emit" {
  description = "Emit"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}
worker "w" {
  based_source      = "main"
  model             = "m"
  system_prompt     = "w"
  terminate_tool_id = go_tool.emit.id
}
module "bad" {
  source  = "./child"
  payload = jsondecode(worker.w.result)
}
`)

		cfgBad := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfgBad, rootDir)
		err := cfgBad.RunPlan()
		require.ErrorContains(t, err, "jsondecodewithtype")

		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
harness "h" {}
go_tool "emit" {
  description = "Emit"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}
worker "w" {
  based_source      = "main"
  model             = "m"
  system_prompt     = "w"
  terminate_tool_id = go_tool.emit.id
}
module "good" {
  source  = "./child"
  payload = jsondecodewithtype(worker.w.result, object({ branch = string }))
}
output "branch" {
  value = module.good.branch
}
`)

		cfgGood := newTestPlanner(t, rootDir, nil)
		initPlanner(t, cfgGood, rootDir)
		require.NoError(t, cfgGood.RunPlan())
		require.Len(t, cfgGood.Outputs(), 1)
		require.False(t, cfgGood.Outputs()[0].Value.IsKnown())
		require.True(t, cfgGood.Outputs()[0].Value.Type().Equals(cty.String))
	})

	t.Run("case_insensitive_duplicate_module_labels_rejected", func(t *testing.T) {
		rootDir := t.TempDir()
		childDir := filepath.Join(rootDir, "child")
		require.NoError(t, os.MkdirAll(childDir, 0o755))
		writePlannerFile(t, filepath.Join(childDir, "child.sofa.hcl"), `output "ok" { value = "ok" }`)
		writePlannerFile(t, filepath.Join(rootDir, "workflow.sofa.hcl"), `
module "Audit" {
  source = "./child"
}
module "audit" {
  source = "./child"
}
`)
		loaded := loadDefaultTestBlocks(t, rootDir)
		cfg := newTestPlanner(t, rootDir, nil)
		err := cfg.Init(loaded)
		require.ErrorContains(t, err, "conflicts with module")
	})
}
