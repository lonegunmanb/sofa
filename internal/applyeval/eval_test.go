package applyeval_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/applyeval"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func compileTestPlan(t *testing.T, rootDir string) *plan.Plan {
	t.Helper()
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{
		Context:    context.Background(),
		Directory:  rootDir,
		BundlePath: "config",
	})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	exported, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)
	return exported
}

func nodeByAddress(t *testing.T, p *plan.Plan, addr string) plan.NodeSpec {
	t.Helper()
	for _, n := range p.Nodes() {
		if n.Address == addr {
			return n
		}
	}
	t.Fatalf("node %q not found in plan", addr)
	return plan.NodeSpec{}
}

func writeHCLFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600))
}

func TestEndToEndPlanArchiveRoundTripDeletedSourceAndSensitivity(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	archiveDir := t.TempDir()
	extractDir := t.TempDir()

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
variable "api_secret" {
  type      = string
  sensitive = true
  default   = "sk-top-secret-token"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

model_provider "main" {
  type     = "openai"
  endpoint = "https://api.example.com/v1"
  api_key  = var.api_secret
}

go_tool "submit" {
  description = "Submit design JSON"
  source = <<-GO
    import "context"
    type Input struct {
      Plan string `+"`json:\"plan\"`"+`
    }
    type Output struct {
      Tasks map[string]string `+"`json:\"tasks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "design" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Design carefully"
  prompt            = "Write ${artifact("spec_doc").path} with ${var.api_secret}"
  terminate_tool_id = go_tool.submit.id

  artifact "spec_doc" {
    type        = "file"
    path        = "docs/spec.md"
    description = "Specification document"
    required    = true
    non_empty   = true
  }
}

locals {
  design_payload = jsondecodewithtype(
    worker.design.result,
    object({
      summary = string
      tasks   = map(string)
    })
  )
  design_summary = local.design_payload.summary
}

worker "implement" {
  based_source  = worker.design.work_branch
  model         = "gpt-5"
  system_prompt = "Implement ${local.design_summary}"
  prompt        = "Read ${import_artifact.spec.path}, write ${artifact("impl_log").path}, secret=${var.api_secret}"

  import_artifact "spec" {
    desc    = "Approved specification"
    path    = "spec.md"
    sources = [worker.design.artifact.spec_doc]
  }

  artifact "impl_log" {
    type        = "file"
    path        = "logs/impl.txt"
    description = "Implementation log"
  }

  tool_use "submit_step" {
    tool_id = go_tool.submit.id
    input = {
      plan = "${local.design_summary}:${var.api_secret}"
    }
  }

  qc {
    criteria = {
      auth = "Ensure ${var.api_secret} is never hardcoded"
    }
  }
}

dynamic_worker "coding" {
  parallelism = 2
  dynamic "worker" {
    for_each = local.design_payload.tasks
    iterator = task

    content {
      based_source  = worker.design.work_branch
      model         = "gpt-5"
      system_prompt = "Code task ${task.key}"
      prompt        = "Task ${task.key}: ${task.value} using ${import_artifact.spec.path} and write ${artifact("task_report").path}"

      import_artifact "spec" {
        desc    = "Spec for coding"
        path    = "spec.md"
        sources = [worker.design.artifact.spec_doc]
      }

      artifact "task_report" {
        type        = "file"
        path        = "reports/task.json"
        description = "Task report"
      }
    }
  }
}

merge "int" {
  based_source = worker.design.work_branch
  sources      = concat([worker.implement.work_branch], dynamic_worker.coding.work_branches)

  gatekeeper "unit" {
    command = ["go", "test", "./...", "-count=1"]
  }
}

output "final_branch" {
  value = merge.int.work_branch
}

output "secret_summary" {
  value     = "${local.design_summary}:${merge.int.work_branch}"
  sensitive = true
}

output "propagated_secret" {
  value = {
    public = merge.int.work_branch
    token  = var.api_secret
  }
}
`)

	exported := compileTestPlan(t, rootDir)
	archivePath := filepath.Join(archiveDir, "run.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, exported, plan.ArchiveSource{ConfigDir: rootDir}))
	loadedArchive, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)

	// Delete the original .sofa.hcl directory completely to prove zero host source re-reading.
	require.NoError(t, os.RemoveAll(rootDir))
	// Also remove extracted .sofa.hcl files to verify Apply evaluation relies strictly on Plan.
	require.NoError(t, os.Remove(filepath.Join(loadedArchive.ConfigDir, "main.sofa.hcl")))

	planned := loadedArchive.Plan
	state := applyeval.RuntimeState{
		Sources: map[string]applyeval.SourceRuntimeState{
			"source.project": {
				CommitSHA: "1111111111111111111111111111111111111111",
			},
		},
		Workers:        map[string]applyeval.WorkerRuntimeResult{},
		DynamicWorkers: map[string]applyeval.DynamicWorkerRuntimeResult{},
		Merges:         map[string]applyeval.MergeRuntimeResult{},
		Modules:        map[string]applyeval.ModuleRuntimeResult{},
	}

	// Step 1: Materialize worker.design
	ctx0, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)
	designCfg, err := applyeval.EvalReadyWorker(ctx0, nodeByAddress(t, planned, "worker.design"), applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.Equal(t, "main", designCfg.BasedSource.AsString())
	assert.Equal(t, "https://github.com/example/sofa.git", designCfg.SourceURL)
	assert.Equal(t, "sandbox.go_env", designCfg.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, "harness.copilot", designCfg.Harness.GetAttr("address").AsString())
	assert.Equal(t, "model_provider.main", designCfg.ModelProvider.GetAttr("address").AsString())
	assert.True(t, designCfg.Prompt.HasMark(golden.SensitiveMark))
	unmarkedDesignPrompt, _ := designCfg.Prompt.UnmarkDeep()
	assert.Contains(t, unmarkedDesignPrompt.AsString(), "/.sofa/artifacts/docs/spec.md")

	// Record worker.design completion
	designResultJSON := `{"summary":"Auth overhaul","tasks":{"api":"Add JWT","ui":"Add login form"}}`
	state.Workers["worker.design"] = applyeval.WorkerRuntimeResult{
		WorkBranch: "sofa/run-1/worker.design",
		CommitSHA:  "2222222222222222222222222222222222222222",
		Result:     &designResultJSON,
		Artifacts: map[string]applyeval.ArtifactRuntimeState{
			"spec_doc": {
				ID:            "art-spec-doc-001",
				Name:          "spec_doc",
				Type:          "file",
				RelPath:       "docs/spec.md",
				ContainerPath: "/.sofa/artifacts/docs/spec.md",
				Description:   "Specification document",
				Required:      true,
				NonEmpty:      true,
			},
		},
	}

	// Step 2: Materialize worker.implement (consumes local.design_summary, artifact(), import_artifact, sensitive var)
	ctx1, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)
	implCfg, err := applyeval.EvalReadyWorker(ctx1, nodeByAddress(t, planned, "worker.implement"), applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/worker.design", implCfg.BasedSource.AsString())
	assert.Equal(t, "https://github.com/example/sofa.git", implCfg.SourceURL)
	assert.Equal(t, "Implement Auth overhaul", implCfg.SystemPrompt.AsString())
	assert.True(t, implCfg.Prompt.HasMark(golden.SensitiveMark))
	unmarkedImplPrompt, _ := implCfg.Prompt.UnmarkDeep()
	assert.Contains(t, unmarkedImplPrompt.AsString(), "/.sofa/imports/spec.md")
	assert.Contains(t, unmarkedImplPrompt.AsString(), "/.sofa/artifacts/logs/impl.txt")
	require.Contains(t, implCfg.Imports, "spec")
	assert.Equal(t, "spec.md", implCfg.Imports["spec"].Path)
	assert.Equal(t, "/.sofa/imports/spec.md", implCfg.Imports["spec"].ContainerPath())
	require.True(t, implCfg.Imports["spec"].Sources.IsWhollyKnown())
	assert.Equal(t, "art-spec-doc-001", implCfg.Imports["spec"].Sources.Index(cty.NumberIntVal(0)).GetAttr("id").AsString())
	require.Len(t, implCfg.ToolUses, 1)
	assert.True(t, implCfg.ToolUses[0].Input.GetAttr("plan").HasMark(golden.SensitiveMark))
	require.NotNil(t, implCfg.QC)
	assert.True(t, implCfg.QC.Criteria.GetAttr("auth").HasMark(golden.SensitiveMark))

	// Step 3: Expand dynamic_worker.coding and project instances
	dynNode := nodeByAddress(t, planned, "dynamic_worker.coding")
	keys, elemMap, err := applyeval.EvalDynamicWorkerCollection(ctx1, dynNode)
	require.NoError(t, err)
	assert.Equal(t, []string{"api", "ui"}, keys)

	apiCfg, err := applyeval.ProjectDynamicInstance(ctx1, dynNode, "api", elemMap["api"], applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/worker.design", apiCfg.BasedSource.AsString())
	assert.Equal(t, "https://github.com/example/sofa.git", apiCfg.SourceURL)
	assert.Equal(t, "Code task api", apiCfg.SystemPrompt.AsString())
	assert.Equal(t, "Task api: Add JWT using /.sofa/imports/spec.md and write /.sofa/artifacts/reports/task.json", apiCfg.Prompt.AsString())
	assert.Equal(t, "spec.md", apiCfg.Imports["spec"].Path)
	assert.Equal(t, "art-spec-doc-001", apiCfg.Imports["spec"].Sources.Index(cty.NumberIntVal(0)).GetAttr("id").AsString())

	// Step 3b: Streaming merge readiness via EvalMergeBase before dynamic_worker.coding finishes.
	stateBeforeDyn := applyeval.RuntimeState{
		Sources: state.Sources,
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.design": state.Workers["worker.design"],
			"worker.implement": {
				WorkBranch: "sofa/run-1/worker.implement",
				CommitSHA:  "3333333333333333333333333333333333333333",
			},
		},
	}
	ctxBeforeDyn, err := applyeval.BuildEvalContext(planned, stateBeforeDyn, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)
	mergeNode := nodeByAddress(t, planned, "merge.int")
	streamingMerge, err := applyeval.EvalMergeBase(ctxBeforeDyn, mergeNode, applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.False(t, streamingMerge.SourcesKnown)
	assert.Nil(t, streamingMerge.Sources)
	assert.Equal(t, "sofa/run-1/worker.design", streamingMerge.BasedSource)
	assert.Equal(t, "https://github.com/example/sofa.git", streamingMerge.SourceURL)
	_, err = applyeval.EvalReadyMerge(ctxBeforeDyn, mergeNode, applyeval.WithPlan(planned))
	require.ErrorContains(t, err, "sources must be wholly known")

	// Record worker.implement & dynamic_worker.coding completions
	state.Workers["worker.implement"] = applyeval.WorkerRuntimeResult{
		WorkBranch: "sofa/run-1/worker.implement",
		CommitSHA:  "3333333333333333333333333333333333333333",
	}
	state.DynamicWorkers["dynamic_worker.coding"] = applyeval.DynamicWorkerRuntimeResult{
		Keys: []string{"api", "ui"},
		Instances: map[string]applyeval.WorkerRuntimeResult{
			"api": {WorkBranch: "sofa/run-1/dyn.api", CommitSHA: "4444444444444444444444444444444444444444"},
			"ui":  {WorkBranch: "sofa/run-1/dyn.ui", CommitSHA: "5555555555555555555555555555555555555555"},
		},
	}

	// Step 4: Materialize merge.int
	ctx2, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)
	merged, err := applyeval.EvalReadyMerge(ctx2, mergeNode, applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.True(t, merged.SourcesKnown)
	assert.Equal(t, "sofa/run-1/worker.design", merged.BasedSource)
	assert.Equal(t, "https://github.com/example/sofa.git", merged.SourceURL)
	assert.Equal(t, []string{"sofa/run-1/worker.implement", "sofa/run-1/dyn.api", "sofa/run-1/dyn.ui"}, merged.Sources)
	require.Len(t, merged.Gatekeepers, 1)
	assert.Equal(t, "unit", merged.Gatekeepers[0].Name)

	state.Merges["merge.int"] = applyeval.MergeRuntimeResult{
		WorkBranch: "sofa/run-1/merge.int",
		CommitSHA:  "6666666666666666666666666666666666666666",
	}

	// Step 5: Evaluate Plan outputs + sensitive marks
	ctx3, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)
	outputs, err := applyeval.EvalPlanOutputs(planned, ctx3)
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/merge.int", outputs["final_branch"].AsString())
	assert.False(t, corespec.IsSensitive(outputs["final_branch"]))

	assert.True(t, outputs["secret_summary"].HasMark(golden.SensitiveMark))
	unmarkedSecretSummary, _ := outputs["secret_summary"].UnmarkDeep()
	assert.Equal(t, "Auth overhaul:sofa/run-1/merge.int", unmarkedSecretSummary.AsString())

	propOut := outputs["propagated_secret"]
	assert.False(t, propOut.GetAttr("public").HasMark(golden.SensitiveMark))
	assert.True(t, propOut.GetAttr("token").HasMark(golden.SensitiveMark))
	unmarkedToken, _ := propOut.GetAttr("token").UnmarkDeep()
	assert.Equal(t, "sk-top-secret-token", unmarkedToken.AsString())
}

func TestDynamicWorkerEmptyAndInvalidForEach(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

go_tool "discover" {
  description = "Discover"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Checks map[string]string `+"`json:\"checks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "discover" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Discover"
  terminate_tool_id = go_tool.discover.id
}

dynamic_worker "checks" {
  dynamic "worker" {
    for_each = jsondecodewithtype(worker.discover.result, object({ checks = map(string) })).checks
    content {
      based_source  = worker.discover.work_branch
      model         = "gpt-5"
      system_prompt = "Check ${worker.key}: ${worker.value}"
    }
  }
}

merge "integrate" {
  based_source = worker.discover.work_branch
  sources      = dynamic_worker.checks.work_branches
}

output "branches" {
  value = dynamic_worker.checks.work_branches
}

output "worker_map_count" {
  value = length(keys(dynamic_worker.checks.workers))
}
`)

	planned := compileTestPlan(t, rootDir)
	emptyResult := `{"checks":{}}`
	state := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.discover": {
				WorkBranch: "sofa/run-1/worker.discover",
				CommitSHA:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Result:     &emptyResult,
			},
		},
	}

	ctx, err := applyeval.BuildEvalContext(planned, state)
	require.NoError(t, err)

	dynNode := nodeByAddress(t, planned, "dynamic_worker.checks")
	keys, err := applyeval.EvalDynamicWorkerKeys(ctx, dynNode)
	require.NoError(t, err)
	assert.Empty(t, keys)
	assert.NotNil(t, keys)

	// Record empty dynamic_worker completion and verify downstream merge & outputs succeed cleanly.
	state.DynamicWorkers = map[string]applyeval.DynamicWorkerRuntimeResult{
		"dynamic_worker.checks": {
			Keys:      []string{},
			Instances: map[string]applyeval.WorkerRuntimeResult{},
		},
	}
	ctx2, err := applyeval.BuildEvalContext(planned, state)
	require.NoError(t, err)

	merged, err := applyeval.EvalReadyMerge(ctx2, nodeByAddress(t, planned, "merge.integrate"))
	require.NoError(t, err)
	assert.Empty(t, merged.Sources)

	outputs, err := applyeval.EvalPlanOutputs(planned, ctx2)
	require.NoError(t, err)
	assert.Equal(t, 0, outputs["branches"].LengthInt())
	cnt, _ := outputs["worker_map_count"].AsBigFloat().Int64()
	assert.Equal(t, int64(0), cnt)

	// Null collection rejection
	nullNode := plan.NodeSpec{
		Address: "dynamic_worker.bad_null",
		Kind:    "dynamic_worker",
		Config: cty.ObjectVal(map[string]cty.Value{
			"name":          cty.StringVal("bad_null"),
			"for_each_expr": cty.StringVal("null"),
			"for_each":      cty.NullVal(cty.Map(cty.String)),
		}),
	}
	_, err = applyeval.EvalDynamicWorkerKeys(ctx, nullNode)
	require.ErrorContains(t, err, "null")

	// Null set element rejection
	nullSetElemNode := plan.NodeSpec{
		Address: "dynamic_worker.bad_set",
		Kind:    "dynamic_worker",
		Config: cty.ObjectVal(map[string]cty.Value{
			"name":          cty.StringVal("bad_set"),
			"for_each_expr": cty.StringVal(`toset(["a", null])`),
			"for_each":      cty.UnknownVal(cty.Set(cty.String)),
		}),
	}
	_, err = applyeval.EvalDynamicWorkerKeys(ctx, nullSetElemNode)
	require.ErrorContains(t, err, "null")

	// Invalid type (list) rejection
	listNode := plan.NodeSpec{
		Address: "dynamic_worker.bad_list",
		Kind:    "dynamic_worker",
		Config: cty.ObjectVal(map[string]cty.Value{
			"name":          cty.StringVal("bad_list"),
			"for_each_expr": cty.StringVal(`["a", "b"]`),
			"for_each":      cty.UnknownVal(cty.List(cty.String)),
		}),
	}
	_, err = applyeval.EvalDynamicWorkerKeys(ctx, listNode)
	require.Error(t, err)
}

func TestChildModuleDeferredValidationAndOutputs(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "audit")

	writeHCLFile(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "branch_in" {
  type = string
  validation {
    condition     = startswith(var.branch_in, "sofa/")
    error_message = "branch_in must start with sofa/"
  }
}

variable "secret_note" {
  type      = string
  sensitive = true
}

worker "review" {
  based_source  = var.branch_in
  model         = "gpt-5"
  system_prompt = "Review branch"
  prompt        = "Note: ${var.secret_note}"
}

output "reviewed_branch" {
  value = worker.review.work_branch
}

output "audit_secret" {
  value = var.secret_note
}
`)

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "prep" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Prep"
}

module "audit" {
  source      = "./modules/audit"
  branch_in   = worker.prep.work_branch
  secret_note = "child-secret-payload"
}

output "final_reviewed" {
  value = module.audit.reviewed_branch
}

output "final_secret" {
  value = module.audit.audit_secret
}
`)

	planned := compileTestPlan(t, rootDir)
	modNode := nodeByAddress(t, planned, "module.audit")

	// Case 1: Invalid runtime input violates child variable.validation -> returns exact error_message.
	badState := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.prep": {
				WorkBranch: "feature/not-sofa-prefixed",
				CommitSHA:  "1111111111111111111111111111111111111111",
			},
		},
	}
	badCtx, err := applyeval.BuildEvalContext(planned, badState)
	require.NoError(t, err)
	_, err = applyeval.EvalModuleInputsAndValidate(badCtx, modNode)
	require.ErrorContains(t, err, "branch_in must start with sofa/")

	// Case 2: Valid runtime input passes validation, binds child Plan, materializes child worker inheriting parent sandbox/harness, and propagates child outputs.
	goodState := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.prep": {
				WorkBranch: "sofa/run-1/worker.prep",
				CommitSHA:  "2222222222222222222222222222222222222222",
			},
		},
	}
	parentCtx, err := applyeval.BuildEvalContext(planned, goodState)
	require.NoError(t, err)
	bound, err := applyeval.EvalModuleInputsAndValidate(parentCtx, modNode)
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/worker.prep", bound.Inputs["branch_in"].AsString())
	assert.True(t, bound.Inputs["secret_note"].HasMark(golden.SensitiveMark))

	childState := applyeval.RuntimeState{}
	childCtx, err := applyeval.BuildEvalContext(bound.Plan, childState, applyeval.WithParentPlans(planned))
	require.NoError(t, err)
	childWorkerCfg, err := applyeval.EvalReadyWorker(
		childCtx,
		nodeByAddress(t, bound.Plan, "worker.review"),
		applyeval.WithPlan(bound.Plan),
		applyeval.WithParentPlans(planned),
	)
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/worker.prep", childWorkerCfg.BasedSource.AsString())
	assert.Equal(t, "https://github.com/example/sofa.git", childWorkerCfg.SourceURL)
	assert.Equal(t, "sandbox.go_env", childWorkerCfg.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, "harness.copilot", childWorkerCfg.Harness.GetAttr("address").AsString())
	assert.True(t, childWorkerCfg.Prompt.HasMark(golden.SensitiveMark))

	childState.Workers = map[string]applyeval.WorkerRuntimeResult{
		"worker.review": {
			WorkBranch: "sofa/run-1/module.audit.worker.review",
			CommitSHA:  "3333333333333333333333333333333333333333",
		},
	}
	childDoneCtx, err := applyeval.BuildEvalContext(bound.Plan, childState, applyeval.WithParentPlans(planned))
	require.NoError(t, err)
	childOutputs, err := applyeval.EvalPlanOutputs(bound.Plan, childDoneCtx)
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/module.audit.worker.review", childOutputs["reviewed_branch"].AsString())
	assert.True(t, childOutputs["audit_secret"].HasMark(golden.SensitiveMark))

	goodState.Modules = map[string]applyeval.ModuleRuntimeResult{
		"module.audit": {Outputs: childOutputs},
	}
	parentDoneCtx, err := applyeval.BuildEvalContext(planned, goodState)
	require.NoError(t, err)
	parentOutputs, err := applyeval.EvalPlanOutputs(planned, parentDoneCtx)
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/module.audit.worker.review", parentOutputs["final_reviewed"].AsString())
	assert.True(t, parentOutputs["final_secret"].HasMark(golden.SensitiveMark))
	unmarkedFinalSecret, _ := parentOutputs["final_secret"].UnmarkDeep()
	assert.Equal(t, "child-secret-payload", unmarkedFinalSecret.AsString())
}

func TestChainedLocalsStaticForEachAndDefaultWorkerIterator(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "matrix" {
  for_each      = toset(["linux", "darwin"])
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Build ${each.key}"
}

locals {
  step1 = worker.matrix["linux"].work_branch
  step2 = "${local.step1}-step2"
  step3 = upper(local.step2)
  items = toset(["alpha", "beta"])
}

dynamic_worker "fanout" {
  dynamic "worker" {
    for_each = local.items
    content {
      based_source  = local.step1
      model         = "gpt-5"
      system_prompt = "Item ${worker.key}=${worker.value} from ${worker.matrix["darwin"].work_branch}"
    }
  }
}

output "chained" {
  value = local.step3
}
`)

	planned := compileTestPlan(t, rootDir)
	state := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.matrix[linux]": {
				WorkBranch: "sofa/run-1/linux",
				CommitSHA:  "1111111111111111111111111111111111111111",
			},
			"worker.matrix[darwin]": {
				WorkBranch: "sofa/run-1/darwin",
				CommitSHA:  "2222222222222222222222222222222222222222",
			},
		},
	}

	ctx, err := applyeval.BuildEvalContext(planned, state)
	require.NoError(t, err)

	dynNode := nodeByAddress(t, planned, "dynamic_worker.fanout")
	keys, elemMap, err := applyeval.EvalDynamicWorkerCollection(ctx, dynNode)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, keys)

	alphaCfg, err := applyeval.ProjectDynamicInstance(ctx, dynNode, "alpha", elemMap["alpha"], applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/linux", alphaCfg.BasedSource.AsString())
	assert.Equal(t, "Item alpha=alpha from sofa/run-1/darwin", alphaCfg.SystemPrompt.AsString())

	outputs, err := applyeval.EvalPlanOutputs(planned, ctx)
	require.NoError(t, err)
	assert.Equal(t, "SOFA/RUN-1/LINUX-STEP2", outputs["chained"].AsString())
}

func TestDynamicWorkerSensitiveElementsBadLocalJSONAndForEachModule(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "region")
	writeHCLFile(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "region_name" {
  type = string
}

variable "branch_in" {
  type = string
}

output "tag" {
  value = "${var.region_name}:${var.branch_in}"
}
`)

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
variable "secret_val" {
  type      = string
  sensitive = true
  default   = "dyn-secret-xyz"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

go_tool "emit" {
  description = "Emit JSON"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Key string `+"`json:\"key\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "prep" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Prep"
  terminate_tool_id = go_tool.emit.id
}

locals {
  decoded = jsondecodewithtype(worker.prep.result, object({ key = string }))
  dyn_map = {
    one = {
      msg = var.secret_val
    }
  }
}

dynamic_worker "secret_fanout" {
  dynamic "worker" {
    for_each = local.dyn_map
    iterator = item
    content {
      based_source  = worker.prep.work_branch
      model         = "gpt-5"
      system_prompt = "Handle ${item.key}"
      prompt        = "Secret: ${item.value.msg}"
    }
  }
}

module "region" {
  for_each    = toset(["east", "west"])
  source      = "./modules/region"
  region_name = each.key
  branch_in   = worker.prep.work_branch
}

output "east_tag" {
  value = module.region["east"].tag
}
`)

	planned := compileTestPlan(t, rootDir)

	// Invalid JSON from completed worker.prep must fail BuildEvalContext.
	badJSON := "{invalid-json"
	_, err := applyeval.BuildEvalContext(planned, applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.prep": {
				WorkBranch: "sofa/run-1/prep",
				CommitSHA:  "1111111111111111111111111111111111111111",
				Result:     &badJSON,
			},
		},
	})
	require.Error(t, err)

	// Valid JSON + sensitive dynamic_worker element + for_each module evaluation.
	goodJSON := `{"key":"ok"}`
	state := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.prep": {
				WorkBranch: "sofa/run-1/prep",
				CommitSHA:  "1111111111111111111111111111111111111111",
				Result:     &goodJSON,
			},
		},
	}
	ctx, err := applyeval.BuildEvalContext(planned, state)
	require.NoError(t, err)

	dynNode := nodeByAddress(t, planned, "dynamic_worker.secret_fanout")
	keys, elems, err := applyeval.EvalDynamicWorkerCollection(ctx, dynNode)
	require.NoError(t, err)
	assert.Equal(t, []string{"one"}, keys)
	assert.True(t, corespec.IsSensitive(elems["one"]))

	instCfg, err := applyeval.ProjectDynamicInstance(ctx, dynNode, "one", elems["one"], applyeval.WithPlan(planned))
	require.NoError(t, err)
	assert.True(t, instCfg.Prompt.HasMark(golden.SensitiveMark))
	unmarkedPrompt, _ := instCfg.Prompt.UnmarkDeep()
	assert.Equal(t, "Secret: dyn-secret-xyz", unmarkedPrompt.AsString())

	// Bind and evaluate for_each module.region[east]
	eastNode := nodeByAddress(t, planned, "module.region[east]")
	boundEast, err := applyeval.EvalModuleInputsAndValidate(ctx, eastNode)
	require.NoError(t, err)
	assert.Equal(t, "east", boundEast.Inputs["region_name"].AsString())
	assert.Equal(t, "sofa/run-1/prep", boundEast.Inputs["branch_in"].AsString())

	eastCtx, err := applyeval.BuildEvalContext(boundEast.Plan, applyeval.RuntimeState{}, applyeval.WithParentPlans(planned))
	require.NoError(t, err)
	eastOut, err := applyeval.EvalPlanOutputs(boundEast.Plan, eastCtx)
	require.NoError(t, err)
	assert.Equal(t, "east:sofa/run-1/prep", eastOut["tag"].AsString())

	state.Modules = map[string]applyeval.ModuleRuntimeResult{
		"module.region[east]": {Outputs: eastOut},
	}
	parentDoneCtx, err := applyeval.BuildEvalContext(planned, state)
	require.NoError(t, err)
	parentOut, err := applyeval.EvalPlanOutputs(planned, parentDoneCtx)
	require.NoError(t, err)
	assert.Equal(t, "east:sofa/run-1/prep", parentOut["east_tag"].AsString())
}

func TestStaticForEachMixedWithUnknownInWorkerAndModule(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "region")
	writeHCLFile(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "tag" {
  type = string
}

output "tag" {
  value = var.tag
}
`)

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
variable "region_secret" {
  type      = string
  sensitive = true
  default   = "us-east-secret"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "copilot" {}

worker "prep" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "Prep"
}

worker "matrix" {
  for_each      = { linux = "amd64" }
  based_source  = worker.prep.work_branch
  model         = "gpt-5"
  system_prompt = "Build ${each.key}"
  prompt        = "Build ${each.key}/${each.value} on ${worker.prep.work_branch} (${var.region_secret})"
}

module "region" {
  for_each = { east = "us-east-1" }
  source   = "./modules/region"
  tag      = "${each.key}-${each.value}-${worker.prep.work_branch}-${var.region_secret}"
}

output "east_tag" {
  value = module.region["east"].tag
}
`)

	exported := compileTestPlan(t, rootDir)
	encoded, err := plan.Marshal(exported)
	require.NoError(t, err)
	restored, err := plan.Unmarshal(encoded)
	require.NoError(t, err)

	state := applyeval.RuntimeState{
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.prep": {
				WorkBranch: "sofa/run-1/prep",
				CommitSHA:  "1111111111111111111111111111111111111111",
			},
		},
	}
	ctx, err := applyeval.BuildEvalContext(restored, state)
	require.NoError(t, err)

	matrixCfg, err := applyeval.EvalReadyWorker(ctx, nodeByAddress(t, restored, "worker.matrix[linux]"), applyeval.WithPlan(restored))
	require.NoError(t, err)
	assert.Equal(t, "sofa/run-1/prep", matrixCfg.BasedSource.AsString())
	assert.True(t, matrixCfg.Prompt.HasMark(golden.SensitiveMark))
	unmarkedPrompt, _ := matrixCfg.Prompt.UnmarkDeep()
	assert.Equal(t, "Build linux/amd64 on sofa/run-1/prep (us-east-secret)", unmarkedPrompt.AsString())

	boundEast, err := applyeval.EvalModuleInputsAndValidate(ctx, nodeByAddress(t, restored, "module.region[east]"))
	require.NoError(t, err)
	assert.True(t, boundEast.Inputs["tag"].HasMark(golden.SensitiveMark))
	unmarkedTag, _ := boundEast.Inputs["tag"].UnmarkDeep()
	assert.Equal(t, "east-us-east-1-sofa/run-1/prep-us-east-secret", unmarkedTag.AsString())

	eastCtx, err := applyeval.BuildEvalContext(boundEast.Plan, applyeval.RuntimeState{}, applyeval.WithParentPlans(restored))
	require.NoError(t, err)
	eastOut, err := applyeval.EvalPlanOutputs(boundEast.Plan, eastCtx)
	require.NoError(t, err)
	assert.True(t, eastOut["tag"].HasMark(golden.SensitiveMark))

	// Also verify sensitive each.value mark survives Marshal/Unmarshal and propagates into EvalReadyWorker and EvalModuleInputsAndValidate.
	sensNodes := restored.Nodes()
	for i := range sensNodes {
		switch sensNodes[i].Address {
		case "worker.matrix[linux]":
			attrs := sensNodes[i].Config.AsValueMap()
			attrs["each"] = cty.ObjectVal(map[string]cty.Value{
				"key":   cty.StringVal("linux"),
				"value": corespec.MarkSensitive(cty.StringVal("secret-arch")),
			})
			attrs["deferred_expression"] = cty.StringVal(`{
  based_source = worker.prep.work_branch
  sandbox = sandbox.go_env
  harness = harness.copilot
  model = "gpt-5"
  system_prompt = "Build ${each.key}"
  prompt = "Arch=${each.value}"
}`)
			sensNodes[i].Config = cty.ObjectVal(attrs)
		case "module.region[east]":
			attrs := sensNodes[i].Config.AsValueMap()
			attrs["each"] = cty.ObjectVal(map[string]cty.Value{
				"key":   cty.StringVal("east"),
				"value": corespec.MarkSensitive(cty.StringVal("secret-az")),
			})
			sensNodes[i].Config = cty.ObjectVal(attrs)
			sensNodes[i].Module.InputExprs["tag"] = `"${each.key}-${each.value}-${worker.prep.work_branch}"`
		}
	}
	sensPlan, err := plan.New(plan.Args{
		Directory:        restored.Directory(),
		BundlePath:       restored.BundlePath(),
		Nodes:            sensNodes,
		Outputs:          restored.Outputs(),
		Context:          restored.Context(),
		LocalExpressions: restored.LocalExpressions(),
		Tools:            restored.Tools(),
	})
	require.NoError(t, err)
	sensBytes, err := plan.Marshal(sensPlan)
	require.NoError(t, err)
	sensRestored, err := plan.Unmarshal(sensBytes)
	require.NoError(t, err)

	sensCtx, err := applyeval.BuildEvalContext(sensRestored, state)
	require.NoError(t, err)
	sensWorkerCfg, err := applyeval.EvalReadyWorker(sensCtx, nodeByAddress(t, sensRestored, "worker.matrix[linux]"), applyeval.WithPlan(sensRestored))
	require.NoError(t, err)
	assert.True(t, sensWorkerCfg.Prompt.HasMark(golden.SensitiveMark))
	unmarkedSensPrompt, _ := sensWorkerCfg.Prompt.UnmarkDeep()
	assert.Equal(t, "Arch=secret-arch", unmarkedSensPrompt.AsString())

	sensModBound, err := applyeval.EvalModuleInputsAndValidate(sensCtx, nodeByAddress(t, sensRestored, "module.region[east]"))
	require.NoError(t, err)
	assert.True(t, sensModBound.Inputs["tag"].HasMark(golden.SensitiveMark))
	unmarkedSensTag, _ := sensModBound.Inputs["tag"].UnmarkDeep()
	assert.Equal(t, "east-secret-az-sofa/run-1/prep", unmarkedSensTag.AsString())
}

func TestQCNestedGatekeeperPlanRoundTripAndDynamicApplyEval(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	archiveDir := t.TempDir()
	extractDir := t.TempDir()

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "golang:1.27"
}

sandbox "qc_env" {
  runtime = "podman"
  image   = "alpine:latest"
}

harness "copilot" {}

go_tool "discover_checks" {
  description = "Discover checks"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Checks map[string]string `+"`json:\"checks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "discover" {
  based_source      = source.project.branch
  sandbox           = sandbox.go_env
  harness           = harness.copilot
  model             = "gpt-5"
  system_prompt     = "Discover checks"
  terminate_tool_id = go_tool.discover_checks.id
  qc {
    sandbox                = sandbox.qc_env
    max_gatekeeper_reworks = 0
    gatekeeper "lint" {
      command = ["go", "vet", "./..."]
    }
  }
}

dynamic_worker "verify" {
  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(string) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      sandbox       = sandbox.go_env
      harness       = harness.copilot
      model         = "gpt-5"
      system_prompt = "Verify ${check.key}"

      qc {
        sandbox                = sandbox.qc_env
        max_gatekeeper_reworks = 4
        gatekeeper "unit" {
          command = ["go", "test", "-run", check.value]
        }
      }
    }
  }
}
`)

	exported := compileTestPlan(t, rootDir)
	archivePath := filepath.Join(archiveDir, "qc-gk.sofaplan")
	require.NoError(t, plan.SaveArchive(archivePath, exported, plan.ArchiveSource{ConfigDir: rootDir}))
	loaded, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)

	planned := loaded.Plan
	discoverResult := `{"checks":{"auth":"TestAuth"}}`
	state := applyeval.RuntimeState{
		Sources: map[string]applyeval.SourceRuntimeState{
			"source.project": {CommitSHA: "1111111111111111111111111111111111111111"},
		},
		Workers: map[string]applyeval.WorkerRuntimeResult{
			"worker.discover": {
				WorkBranch: "sofa/run-1/worker.discover",
				CommitSHA:  "2222222222222222222222222222222222222222",
				Result:     &discoverResult,
			},
		},
	}

	ctx, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)

	discoverCfg, err := applyeval.EvalReadyWorker(ctx, nodeByAddress(t, planned, "worker.discover"), applyeval.WithPlan(planned))
	require.NoError(t, err)
	require.NotNil(t, discoverCfg.QC)
	assert.Equal(t, "sandbox.qc_env", discoverCfg.QC.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, 0, discoverCfg.QC.MaxGatekeeperReworks)
	assert.True(t, discoverCfg.QC.MaxGatekeeperReworksSet)
	assert.Equal(t, 0, discoverCfg.QC.Criteria.LengthInt())
	require.Len(t, discoverCfg.QC.Gatekeepers, 1)
	assert.Equal(t, "lint", discoverCfg.QC.Gatekeepers[0].Name)
	assert.Equal(t, []string{"go", "vet", "./..."}, discoverCfg.QC.Gatekeepers[0].Command)

	dynNode := nodeByAddress(t, planned, "dynamic_worker.verify")
	keys, elems, err := applyeval.EvalDynamicWorkerCollection(ctx, dynNode)
	require.NoError(t, err)
	assert.Equal(t, []string{"auth"}, keys)

	authCfg, err := applyeval.ProjectDynamicInstance(ctx, dynNode, "auth", elems["auth"], applyeval.WithPlan(planned))
	require.NoError(t, err)
	require.NotNil(t, authCfg.QC)
	assert.Equal(t, "sandbox.qc_env", authCfg.QC.Sandbox.GetAttr("address").AsString())
	assert.Equal(t, 4, authCfg.QC.MaxGatekeeperReworks)
	assert.True(t, authCfg.QC.MaxGatekeeperReworksSet)
	require.Len(t, authCfg.QC.Gatekeepers, 1)
	assert.Equal(t, "unit", authCfg.QC.Gatekeepers[0].Name)
	assert.Equal(t, []string{"go", "test", "-run", "TestAuth"}, authCfg.QC.Gatekeepers[0].Command)
}

func TestApplyEvalPreservesNormalizedExternalSkillDirectoriesAcrossDeferredEval(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	rootDir := filepath.Join(workspace, "wf")
	extDir := filepath.Join(workspace, "ext-skills")
	extractDir := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "ext.sofaplan")

	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "skills", "local"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "skills", "local", "SKILL.md"), []byte("# Local\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(extDir, "ext"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(extDir, "ext", "SKILL.md"), []byte("# Ext\n"), 0o644))

	writeHCLFile(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
sandbox "go_env" {
  runtime = "podman"
  image   = "alpine:latest"
}
harness "copilot" {}
worker "implement" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Implement"
  skill_directories = ["./skills", "../ext-skills", "/workspace/.agents/skills"]
  qc {
    skill_directories = ["../ext-skills"]
    criteria = {
      ok = "check"
    }
  }
}
`)

	exported := compileTestPlan(t, rootDir)
	require.NoError(t, plan.SaveArchive(archivePath, exported, plan.ArchiveSource{ConfigDir: rootDir}))
	loaded, err := plan.LoadArchive(archivePath, extractDir, plan.LoadArchiveOptions{})
	require.NoError(t, err)

	planned := loaded.Plan
	state := applyeval.RuntimeState{
		Sources: map[string]applyeval.SourceRuntimeState{
			"source.project": {CommitSHA: "1111111111111111111111111111111111111111"},
		},
	}
	ctx, err := applyeval.BuildEvalContext(planned, state, applyeval.WithBaseDir(extractDir))
	require.NoError(t, err)

	cfg, err := applyeval.EvalReadyWorker(ctx, nodeByAddress(t, planned, "worker.implement"), applyeval.WithPlan(planned))
	require.NoError(t, err)
	require.Len(t, cfg.SkillDirectories, 3)
	assert.Equal(t, "config/skills", cfg.SkillDirectories[0])
	assert.True(t, strings.HasPrefix(cfg.SkillDirectories[1], "external_skills/"))
	assert.Equal(t, "/workspace/.agents/skills", cfg.SkillDirectories[2])
	require.NotNil(t, cfg.QC)
	require.Len(t, cfg.QC.SkillDirectories, 1)
	assert.Equal(t, cfg.SkillDirectories[1], cfg.QC.SkillDirectories[0])
}
