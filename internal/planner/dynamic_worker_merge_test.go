package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	mergespec "github.com/lonegunmanb/sofa/internal/merge/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/require"
)

func TestDynamicWorkerAndMergeEndToEndPipeline(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "model" {
  type    = string
  default = "gpt-5"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

model_provider "openai" {
  type     = "openai"
  endpoint = "https://api.openai.com/v1"
}

go_tool "discover_checks" {
  description = "Discover notification checks"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output struct {
      Checks map[string]struct{ Rule string `+"`json:\"rule\"`"+` } `+"`json:\"checks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

worker "discover" {
  based_source      = source.project.branch
  model             = var.model
  system_prompt     = "Discover checks."
  terminate_tool_id = go_tool.discover_checks.id
}

dynamic_worker "verify_notifications" {
  parallelism = 4
  serial      = false

  dynamic "worker" {
    for_each = jsondecodewithtype(
      worker.discover.result,
      object({ checks = map(object({ rule = string })) })
    ).checks
    iterator = check

    content {
      based_source  = worker.discover.work_branch
      model         = var.model
      system_prompt = "Verify single notification check."
      prompt        = "Verify ${check.key}: ${check.value.rule}"

      qc {
        gatekeeper "test" {
          command = ["go", "test", "./..."]
        }
      }
    }
  }
}

merge "integrate" {
  based_source = worker.discover.work_branch
  sources      = dynamic_worker.verify_notifications.work_branches

  gatekeeper "regression" {
    command = ["go", "test", "./...", "-count=1"]
  }
}

worker "post_merge" {
  based_source  = merge.integrate.work_branch
  model         = var.model
  system_prompt = "Validate integrated branch."

  fallback "rework_discover" {
    on     = ["gatekeeper_exhausted"]
    target = worker.discover
  }
}

preserve "final" {
  branch = worker.post_merge.work_branch
}

output "merged_branch" {
  value = merge.integrate.work_branch
}

output "final_branch" {
  value = worker.post_merge.work_branch
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	// DAG edges: discover -> verify_notifications -> integrate -> post_merge -> preserve.final
	dynAncestors, err := cfg.GetAncestors("dynamic_worker.verify_notifications")
	require.NoError(t, err)
	require.Contains(t, dynAncestors, "worker.discover")
	require.Contains(t, dynAncestors, "sandbox.go_env")
	require.Contains(t, dynAncestors, "harness.local")
	require.Contains(t, dynAncestors, "model_provider.openai")

	mergeAncestors, err := cfg.GetAncestors("merge.integrate")
	require.NoError(t, err)
	require.Contains(t, mergeAncestors, "dynamic_worker.verify_notifications")
	require.Contains(t, mergeAncestors, "worker.discover")
	require.Contains(t, mergeAncestors, "sandbox.go_env")

	postAncestors, err := cfg.GetAncestors("worker.post_merge")
	require.NoError(t, err)
	require.Contains(t, postAncestors, "merge.integrate")
	require.Contains(t, postAncestors, "dynamic_worker.verify_notifications")
	require.Contains(t, postAncestors, "worker.discover")

	preserveAncestors, err := cfg.GetAncestors("preserve.final")
	require.NoError(t, err)
	require.Contains(t, preserveAncestors, "worker.post_merge")

	// SourceURL propagation and ContentTemplate / DeferredExpression.
	var dynBlock *workerspec.DynamicWorkerBlock
	var mergeBlock *mergespec.MergeBlock
	var postBlock *workerspec.WorkerBlock
	for _, v := range cfg.GetVertices() {
		switch b := v.(type) {
		case *workerspec.DynamicWorkerBlock:
			if b.Name() == "verify_notifications" {
				dynBlock = b
			}
		case *mergespec.MergeBlock:
			if b.Name() == "integrate" {
				mergeBlock = b
			}
		case *workerspec.WorkerBlock:
			if b.Name() == "post_merge" {
				postBlock = b
			}
		}
	}
	require.NotNil(t, dynBlock)
	require.NotNil(t, mergeBlock)
	require.NotNil(t, postBlock)

	dynCfg := dynBlock.DynamicWorkerConfig()
	require.Equal(t, "https://github.com/example/sofa.git", dynCfg.Content.SourceURL)
	require.NotEmpty(t, dynCfg.ContentTemplate)

	mergeCfg := mergeBlock.MergeConfig()
	require.Equal(t, "https://github.com/example/sofa.git", mergeCfg.SourceURL)
	require.Equal(t, "sandbox.go_env", mergeCfg.Sandbox.GetAttr("address").AsString())

	postCfg := postBlock.WorkerConfig()
	require.Equal(t, "https://github.com/example/sofa.git", postCfg.SourceURL)
	require.NotEmpty(t, postCfg.DeferredExpression)
}

func TestDynamicWorkerOrthogonalRootDefaults(t *testing.T) {
	baseDynamic := `
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check
    content {
      based_source  = "main"
      model         = "gpt-5"
      system_prompt = "Verify ${check.key}"
    }
  }
}
`
	// 0 sandboxes fails.
	dir0 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir0, "workflow.sofa.hcl"), `
harness "local" {}
`+baseDynamic)
	cfg0 := newTestPlanner(t, dir0, nil)
	loaded0, diags0, err := config.LoadDirectoryContext(context.Background(), dir0)
	require.NoError(t, err)
	require.False(t, diags0.HasErrors())
	require.Error(t, cfg0.Init(loaded0.Blocks))

	// 2 sandboxes without explicit sandbox fails.
	dir2 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir2, "workflow.sofa.hcl"), `
sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
sandbox "b" {
  runtime = "podman"
  image   = "img:2"
}
harness "local" {}
`+baseDynamic)
	cfg2 := newTestPlanner(t, dir2, nil)
	loaded2, diags2, err := config.LoadDirectoryContext(context.Background(), dir2)
	require.NoError(t, err)
	require.False(t, diags2.HasErrors(), diags2.Error())
	require.Error(t, cfg2.Init(loaded2.Blocks))

	// 0 harnesses fails.
	dirNoHarness := t.TempDir()
	writePlannerFile(t, filepath.Join(dirNoHarness, "workflow.sofa.hcl"), `
sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
`+baseDynamic)
	cfgNH := newTestPlanner(t, dirNoHarness, nil)
	loadedNH, diagsNH, err := config.LoadDirectoryContext(context.Background(), dirNoHarness)
	require.NoError(t, err)
	require.False(t, diagsNH.HasErrors(), diagsNH.Error())
	require.Error(t, cfgNH.Init(loadedNH.Blocks))

	// 2 model_providers without explicit model_provider fails.
	dir2MP := t.TempDir()
	writePlannerFile(t, filepath.Join(dir2MP, "workflow.sofa.hcl"), `
sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
harness "local" {}
model_provider "p1" {
  type     = "openai"
  endpoint = "https://a.example/v1"
}
model_provider "p2" {
  type     = "openai"
  endpoint = "https://b.example/v1"
}
`+baseDynamic)
	cfg2MP := newTestPlanner(t, dir2MP, nil)
	loaded2MP, diags2MP, err := config.LoadDirectoryContext(context.Background(), dir2MP)
	require.NoError(t, err)
	require.False(t, diags2MP.HasErrors(), diags2MP.Error())
	require.Error(t, cfg2MP.Init(loaded2MP.Blocks))

	// Explicit model_provider = null inside content exempts singleton model_provider injection.
	dirNullMP := t.TempDir()
	writePlannerFile(t, filepath.Join(dirNullMP, "workflow.sofa.hcl"), `
sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
harness "local" {}
model_provider "openai" {
  type     = "openai"
  endpoint = "https://a.example/v1"
}

dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check
    content {
      based_source   = "main"
      model_provider = null
      model          = "gpt-5"
      system_prompt  = "Verify ${check.key}"
    }
  }
}
`)
	cfgNull := newTestPlanner(t, dirNullMP, nil)
	initPlanner(t, cfgNull, dirNullMP)
	require.NoError(t, cfgNull.RunPlan())
	for _, b := range golden.Blocks[*workerspec.DynamicWorkerBlock](cfgNull) {
		require.True(t, b.DynamicWorkerConfig().Content.ModelProvider.IsNull())
		require.Equal(t, "sandbox.a", b.DynamicWorkerConfig().Content.Sandbox.GetAttr("address").AsString())
		require.Equal(t, "harness.local", b.DynamicWorkerConfig().Content.Harness.GetAttr("address").AsString())
	}
}

func TestMergeConditionalRootSandboxDefault(t *testing.T) {
	// With gatekeeper: 0 sandboxes fails, 2 sandboxes fails, 1 sandbox binds.
	withGK := `
merge "integrate" {
  based_source = "main"
  sources      = ["feat/a"]
  gatekeeper "test" {
    command = ["go", "test", "./..."]
  }
}
`
	dir0 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir0, "workflow.sofa.hcl"), withGK)
	cfg0 := newTestPlanner(t, dir0, nil)
	loaded0, diags0, err := config.LoadDirectoryContext(context.Background(), dir0)
	require.NoError(t, err)
	require.False(t, diags0.HasErrors(), diags0.Error())
	require.Error(t, cfg0.Init(loaded0.Blocks))

	dir2 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir2, "workflow.sofa.hcl"), `
sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
sandbox "b" {
  runtime = "podman"
  image   = "img:2"
}
`+withGK)
	cfg2 := newTestPlanner(t, dir2, nil)
	loaded2, diags2, err := config.LoadDirectoryContext(context.Background(), dir2)
	require.NoError(t, err)
	require.False(t, diags2.HasErrors(), diags2.Error())
	require.Error(t, cfg2.Init(loaded2.Blocks))

	dir1 := t.TempDir()
	writePlannerFile(t, filepath.Join(dir1, "workflow.sofa.hcl"), `
sandbox "only" {
  runtime = "podman"
  image   = "img:1"
}
`+withGK)
	cfg1 := newTestPlanner(t, dir1, nil)
	initPlanner(t, cfg1, dir1)
	require.NoError(t, cfg1.RunPlan())
	for _, mb := range golden.Blocks[*mergespec.MergeBlock](cfg1) {
		require.Equal(t, "sandbox.only", mb.MergeConfig().Sandbox.GetAttr("address").AsString())
	}

	// Without gatekeeper: 0 or 2 sandboxes both pass without binding sandbox.
	withoutGK := `
merge "integrate" {
  based_source = "main"
  sources      = ["feat/a"]
}
`
	for _, prefix := range []string{
		"",
		`sandbox "a" {
  runtime = "podman"
  image   = "img:1"
}
sandbox "b" {
  runtime = "podman"
  image   = "img:2"
}`,
	} {
		dir := t.TempDir()
		writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), prefix+"\n"+withoutGK)
		cfg := newTestPlanner(t, dir, nil)
		initPlanner(t, cfg, dir)
		require.NoError(t, cfg.RunPlan())
		for _, mb := range golden.Blocks[*mergespec.MergeBlock](cfg) {
			require.True(t, mb.MergeConfig().Sandbox.IsNull())
		}
	}
}

func TestDynamicWorkerContentFallbackAndMergeTargetBranchPreserveGuard(t *testing.T) {
	// Dynamic worker with fallback targeting positive ancestor passes; targeting unrelated worker fails.
	goodDir := t.TempDir()
	writePlannerFile(t, filepath.Join(goodDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
sandbox "go_env" {
  runtime = "podman"
  image   = "img:1"
}
harness "local" {}

worker "discover" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "discover"
}

worker "unrelated" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "unrelated"
}

dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check
    content {
      based_source  = worker.discover.work_branch
      model         = "gpt-5"
      system_prompt = "verify ${check.key}"

      qc {
        dynamic "gatekeeper" {
          for_each = toset(["unit"])
          content "unit" {
            command = ["go", "test", "./..."]
          }
        }
      }

      fallback "rework" {
        on     = ["gatekeeper_exhausted"]
        target = worker.discover
      }
    }
  }
}

merge "integrate" {
  based_source  = worker.discover.work_branch
  target_branch = "main"
  sources       = dynamic_worker.verify.work_branches
  dynamic "gatekeeper" {
    for_each = toset(["reg"])
    content "reg" {
      command = ["go", "test", "./..."]
    }
  }
}

preserve "keep_main" {
  branch = "main"
}
`)
	goodCfg := newTestPlanner(t, goodDir, nil)
	initPlanner(t, goodCfg, goodDir)
	require.NoError(t, goodCfg.RunPlan())

	// Removing preserve "keep_main" causes merge.target_branch = "main" to fail Plan.
	badMergeDir := t.TempDir()
	writePlannerFile(t, filepath.Join(badMergeDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}
merge "integrate" {
  based_source  = "dev"
  target_branch = "main"
  sources       = ["feat/a"]
}
`)
	badMergeCfg := newTestPlanner(t, badMergeDir, nil)
	initPlanner(t, badMergeCfg, badMergeDir)
	err := badMergeCfg.RunPlan()
	require.Error(t, err)
	require.Contains(t, err.Error(), "preserve")

	// Dynamic worker fallback targeting unrelated worker fails Init.
	badFBDir := t.TempDir()
	writePlannerFile(t, filepath.Join(badFBDir, "workflow.sofa.hcl"), `
sandbox "go_env" {
  runtime = "podman"
  image   = "img:1"
}
harness "local" {}
worker "discover" {
  based_source  = "main"
  model         = "gpt-5"
  system_prompt = "discover"
}
worker "unrelated" {
  based_source  = "main"
  model         = "gpt-5"
  system_prompt = "unrelated"
}
dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check
    content {
      based_source  = worker.discover.work_branch
      model         = "gpt-5"
      system_prompt = "verify ${check.key}"
      fallback "rework" {
        on     = ["gatekeeper_exhausted"]
        target = worker.unrelated
      }
    }
  }
}
`)
	badFBCfg := newTestPlanner(t, badFBDir, nil)
	loadedFB, diagsFB, err := config.LoadDirectoryContext(context.Background(), badFBDir)
	require.NoError(t, err)
	require.False(t, diagsFB.HasErrors(), diagsFB.Error())
	require.Error(t, badFBCfg.Init(loadedFB.Blocks))
}

func TestMergeTargetBranchDependsOnPreserveAndSameNameDynamicImport(t *testing.T) {
	// preserve "keep_main" runs AFTER merge "integrate" via depends_on, yet merge targets "main": must pass!
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "img:1"
}

harness "local" {}

worker "verify" {
  based_source  = source.project.branch
  model         = "gpt-5"
  system_prompt = "produce spec"

  artifact "spec" {
    type        = "file"
    path        = "docs/spec.md"
    description = "spec"
  }
}

dynamic_worker "verify" {
  dynamic "worker" {
    for_each = toset(["a"])
    iterator = check

    content {
      based_source  = worker.verify.work_branch
      model         = "gpt-5"
      system_prompt = "verify ${check.key}"
      prompt        = "Read ${import_artifact.spec.path}"

      import_artifact "spec" {
        desc    = "spec from static worker with same name"
        sources = [worker.verify.artifact.spec]
      }
    }
  }
}

merge "integrate" {
  based_source  = worker.verify.work_branch
  target_branch = "main"
  sources       = dynamic_worker.verify.work_branches
}

preserve "keep_main" {
  depends_on = [merge.integrate]
  branch     = "main"
}
`)
	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.Equal(t, []mergespec.PreservedBranchInfo{
		{URL: "https://github.com/example/sofa.git", Branch: "main"},
	}, cfg.PreservedKnownBranches())
	require.NoError(t, cfg.RunPlan())

	mergeAncestors, err := cfg.GetAncestors("preserve.keep_main")
	require.NoError(t, err)
	require.Contains(t, mergeAncestors, "merge.integrate")

	// Still fails when preserve (with depends_on = [merge.integrate]) preserves a different branch.
	mismatchDir := t.TempDir()
	writePlannerFile(t, filepath.Join(mismatchDir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

merge "integrate" {
  based_source  = "dev"
  target_branch = "main"
  sources       = ["feat/a"]
}

preserve "keep_other" {
  depends_on = [merge.integrate]
  branch     = "other"
}
`)
	mismatchCfg := newTestPlanner(t, mismatchDir, nil)
	initPlanner(t, mismatchCfg, mismatchDir)
	err = mismatchCfg.RunPlan()
	require.Error(t, err)
	require.Contains(t, err.Error(), "preserve")
}
