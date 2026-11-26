package planner_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/lonegunmanb/sofa/internal/planner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestExportPlanAllBlockKindsSensitivityLocalsDepsAndRoundTrip(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "audit")
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "skills", "tdd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "skills", "tdd", "SKILL.md"), []byte("# TDD\n"), 0o600))
	require.NoError(t, os.MkdirAll(childDir, 0o700))

	writeExportHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "branch_in" {
  type = string
  validation {
    condition     = length(var.branch_in) > 0
    error_message = "branch_in must not be empty"
  }
}

variable "secret_note" {
  type      = string
  sensitive = true
}

worker "review" {
  based_source  = var.branch_in
  model         = "gpt-5"
  system_prompt = "Review branch with note: ${var.secret_note}"
}

output "reviewed_branch" {
  value = worker.review.work_branch
}
`)

	writeExportHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
variable "api_secret" {
  type      = string
  sensitive = true
  default   = "sk-root-api-secret"
}

variable "mcp_secret" {
  type      = string
  sensitive = true
  default   = "mcp-hdr-secret"
}

variable "stdio_secret" {
  type      = string
  sensitive = true
  default   = "stdio-env-secret"
}

variable "signing_secret" {
  type      = string
  sensitive = true
  default   = "signing-key-secret"
}

source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "auto"
  signing_key = var.signing_secret
  options = {
    location = "us-central1"
    secret   = var.api_secret
  }
}

model_provider "openai" {
  type     = "openai"
  endpoint = "https://api.openai.com/v1"
  api_key  = "sk-literal-provider-secret"
  headers = {
    "X-Custom" = "ok"
    "X-Secret" = var.api_secret
  }
}

mcp_server "docs_http" {
  tools = ["search"]
  http {
    url          = "https://mcp.example.com/http"
    bearer_token = "mcp-literal-bearer-secret"
    headers = {
      "X-MCP-Secret" = var.mcp_secret
    }
  }
}

mcp_server "docs_stdio" {
  tools = ["lint"]
  stdio {
    command = "mcp-lint"
    env = {
      SECRET_ENV = var.stdio_secret
    }
  }
}

go_tool "discover" {
  description = "Discover checks"
  source = <<-GO
    import "context"
    type Input struct {
      Note string `+"`json:\"note\"`"+`
    }
    type Output struct {
      Checks map[string]string `+"`json:\"checks\"`"+`
    }
    func Invoke(context.Context, Input) (ToolResponse[Output], error) {
      return ToolResponse[Output]{Accepted: true}, nil
    }
  GO
}

external_tool "linter" {
  description = "External linter"
  program     = ["golangci-lint", "run"]
  input_type  = object({ path = string })
  output_type = object({ ok = bool })
}

worker "plan_step" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Plan changes."
  prompt            = "Use ${var.api_secret} and write ${artifact("plan_doc").path}"
  skill_directories = ["${path.module}/skills"]
  mcp_tool_ids      = [mcp_server.docs_http.tool_ids["search"], mcp_server.docs_stdio.tool_ids["lint"]]

  artifact "plan_doc" {
    type        = "file"
    path        = "docs/plan.md"
    description = "plan doc"
  }

  tool_use "run_lint" {
    tool_id = external_tool.linter.id
    input = {
      path = var.api_secret
    }
  }

  tool_use "finish" {
    tool_id   = go_tool.discover.id
    terminate = true
  }

  qc {
    profile = "opus-audit"
    criteria = {
      check = "Verify using ${var.api_secret}"
    }
  }
}

locals {
  plan_branch = worker.plan_step.work_branch
}

worker "after_local" {
  based_source  = local.plan_branch
  model         = "gpt-5"
  system_prompt = "Follow local branch."

  import_artifact "plan_doc" {
    desc    = "plan doc"
    path    = "inputs/plan.md"
    sources = [worker.plan_step.artifact.plan_doc]
  }
}

dynamic_worker "verify" {
  parallelism = 2
  dynamic "worker" {
    for_each = jsondecodewithtype(worker.plan_step.result, object({ checks = map(string) })).checks
    iterator = item
    content {
      based_source  = worker.plan_step.work_branch
      model         = "gpt-5"
      system_prompt = "Verify ${item.key}: ${item.value}"
    }
  }
}

merge "integrate" {
  based_source = worker.plan_step.work_branch
  sources      = dynamic_worker.verify.work_branches
  gatekeeper "unit" {
    command = ["go", "test", "./..."]
  }
}

module "audit" {
  source      = "./modules/audit"
  branch_in   = merge.integrate.work_branch
  secret_note = var.api_secret
}

preserve "final" {
  branch = module.audit.reviewed_branch
}

output "final_branch" {
  value = module.audit.reviewed_branch
}

output "secret_out" {
  value     = var.api_secret
  sensitive = true
}
`)

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

	lockFile := &harness.PluginLockFile{
		FormatVersion: harness.LockFormatVersion,
		Plugins: []harness.PluginLockEntry{
			{
				Name:              "antigravity",
				Source:            "github.com/lonegunmanb/sofa-harness-antigravity",
				Version:           "0.1.0",
				VerifiedSignature: "attestation",
				Assets: map[string]harness.PluginAsset{
					"linux_amd64": {
						Filename: "sofa-harness-antigravity_linux_amd64.zip",
						SHA256:   strings.Repeat("a", 64),
					},
				},
			},
		},
	}

	// Missing / mismatched lockfile must fail when RequirePluginLock is true.
	_, err = cfg.ExportPlan(planner.ExportOptions{RequirePluginLock: true})
	require.ErrorContains(t, err, "antigravity")
	require.ErrorContains(t, err, "sofa init")

	exported, err := cfg.ExportPlan(planner.ExportOptions{
		PluginLock:        lockFile,
		RequirePluginLock: true,
	})
	require.NoError(t, err)

	nodes := exported.Nodes()
	byAddr := make(map[string]plan.NodeSpec, len(nodes))
	kindSeen := make(map[string]bool)
	for _, n := range nodes {
		byAddr[n.Address] = n
		kindSeen[n.Kind] = true
	}
	for _, wantKind := range []string{
		"source", "preserve", "sandbox", "harness", "model_provider",
		"mcp_server", "go_tool", "external_tool", "worker", "dynamic_worker",
		"merge", "module",
	} {
		assert.True(t, kindSeen[wantKind], "expected exported node kind %q", wantKind)
	}

	// Transitive dependency routed through local.plan_branch -> worker.after_local
	assert.Contains(t, byAddr["worker.after_local"].Dependencies, "worker.plan_step")

	// Deterministic topological ordering: dependencies always appear earlier in Nodes().
	indexByAddr := make(map[string]int, len(nodes))
	for i, n := range nodes {
		indexByAddr[n.Address] = i
	}
	for _, n := range nodes {
		for _, dep := range n.Dependencies {
			assert.Less(t, indexByAddr[dep], indexByAddr[n.Address], "dependency %s should precede %s", dep, n.Address)
		}
	}

	// Sensitivity marks preserved on exported nodes
	assert.False(t, byAddr["sandbox.go_env"].Config.Type().HasAttribute("entrypoint"))
	assert.True(t, byAddr["model_provider.openai"].Config.GetAttr("api_key").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["model_provider.openai"].Config.GetAttr("headers").Index(cty.StringVal("X-Secret")).HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["mcp_server.docs_http"].Config.GetAttr("http").GetAttr("bearer_token").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["mcp_server.docs_http"].Config.GetAttr("http").GetAttr("headers").Index(cty.StringVal("X-MCP-Secret")).HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["mcp_server.docs_stdio"].Config.GetAttr("stdio").GetAttr("env").Index(cty.StringVal("SECRET_ENV")).HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["harness.antigravity"].Config.GetAttr("signing_key").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["harness.antigravity"].Config.GetAttr("options").GetAttr("secret").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["worker.plan_step"].Config.GetAttr("prompt").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["worker.plan_step"].Config.GetAttr("qc").GetAttr("criteria").GetAttr("check").HasMark(golden.SensitiveMark))
	assert.True(t, byAddr["worker.plan_step"].Config.GetAttr("qc").GetAttr("sandbox").IsNull())
	assert.Equal(t, "opus-audit", byAddr["worker.plan_step"].Config.GetAttr("qc").GetAttr("profile").AsString())
	assert.Equal(t, "docs/plan.md", byAddr["worker.plan_step"].Config.GetAttr("artifacts").GetAttr("plan_doc").GetAttr("path").AsString())
	assert.Equal(t, "/.sofa/artifacts/docs/plan.md", byAddr["worker.plan_step"].Config.GetAttr("artifacts").GetAttr("plan_doc").GetAttr("container_path").AsString())
	assert.Equal(t, "inputs/plan.md", byAddr["worker.after_local"].Config.GetAttr("imports").GetAttr("plan_doc").GetAttr("path").AsString())
	assert.Equal(t, "/.sofa/imports/inputs/plan.md", byAddr["worker.after_local"].Config.GetAttr("imports").GetAttr("plan_doc").GetAttr("container_path").AsString())

	// Child module plan & deferred validations
	modNode := byAddr["module.audit"]
	require.NotNil(t, modNode.Module)
	assert.NotEmpty(t, modNode.Module.Plan.DeferredValidations()["branch_in"])
	assert.True(t, modNode.Module.Inputs["secret_note"].HasMark(golden.SensitiveMark))

	// Tools metadata collected with JSON Schema
	require.Len(t, exported.Tools(), 2)
	for _, tool := range exported.Tools() {
		assert.NotEmpty(t, tool.InputSchema)
		assert.NotEmpty(t, tool.InputType)
		assert.NotEmpty(t, tool.OutputType)
		assert.NotEmpty(t, tool.Origin.Filename)
	}

	// Origin captured
	assert.Equal(t, "main.sofa.hcl", byAddr["source.project"].Origin.Filename)
	assert.Contains(t, byAddr["source.project"].Origin.Source, `source "project"`)

	// Marshal -> Unmarshal round-trip & Display zero secret leakage
	encoded, err := plan.Marshal(exported)
	require.NoError(t, err)
	restored, err := plan.Unmarshal(encoded)
	require.NoError(t, err)

	displayText, err := plan.Display(restored)
	require.NoError(t, err)
	assert.False(t, restored.Nodes()[0].Config.Type().HasAttribute("entrypoint"))
	for _, n := range restored.Nodes() {
		if n.Kind == "sandbox" {
			assert.False(t, n.Config.Type().HasAttribute("entrypoint"))
		}
	}
	for _, secret := range []string{
		"sk-root-api-secret",
		"mcp-hdr-secret",
		"stdio-env-secret",
		"signing-key-secret",
		"sk-literal-provider-secret",
		"mcp-literal-bearer-secret",
	} {
		assert.NotContains(t, displayText, secret)
	}
	assert.Contains(t, displayText, `"<sensitive>"`)
	assert.Contains(t, displayText, `"<known after apply>"`)
}

func TestExportPlanRejectsUnlockedChildModuleRemoteHarness(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "child")
	require.NoError(t, os.MkdirAll(childDir, 0o700))

	writeExportHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
harness "child_remote" {
  source    = "github.com/lonegunmanb/sofa-harness-child"
  version   = "0.2.0"
  signature = "none"
}
`)
	writeExportHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
module "child" {
  source = "./modules/child"
}
`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{Context: context.Background(), Directory: rootDir})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	_, err = cfg.ExportPlan(planner.ExportOptions{
		PluginLock:        &harness.PluginLockFile{FormatVersion: harness.LockFormatVersion},
		RequirePluginLock: true,
	})
	require.ErrorContains(t, err, "child_remote")
	require.ErrorContains(t, err, "sofa init")
}

func TestExportPlanAndDisplayRedactChildSensitiveLiteralInputsAndInlineSecrets(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	childDir := filepath.Join(rootDir, "modules", "child")
	require.NoError(t, os.MkdirAll(childDir, 0o700))

	writeExportHCL(t, filepath.Join(childDir, "main.sofa.hcl"), `
variable "secret_note" {
  type      = string
  sensitive = true
}

output "ok" {
  value = "public-child-output"
}
`)

	writeExportHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/sofa.git"
  branch = "main"
}

sandbox "go_env" {
  runtime = "podman"
  image   = "ghcr.io/example/go:1.27"
}

harness "local" {}

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

worker "upstream" {
  based_source      = source.project.branch
  model             = "gpt-5"
  system_prompt     = "Discover"
  prompt            = sensitive("inline-worker-secret")
  terminate_tool_id = go_tool.discover.id
}

worker "deferred_worker" {
  based_source  = worker.upstream.work_branch
  model         = "gpt-5"
  system_prompt = "Run downstream"
  prompt        = sensitive("inline-deferred-worker-secret")
}

dynamic_worker "sweep" {
  dynamic "worker" {
    for_each = jsondecodewithtype(worker.upstream.result, object({ checks = map(string) })).checks
    iterator = item
    content {
      based_source  = worker.upstream.work_branch
      model         = "gpt-5"
      system_prompt = "Check ${item.key}"
      prompt        = sensitive("inline-dynamic-secret")
    }
  }
}

module "child" {
  source      = "./modules/child"
  secret_note = "literal-child-secret"
}
`)

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
	assert.True(t, plan.ContainsSensitiveValues(exported))

	byAddr := make(map[string]plan.NodeSpec)
	for _, n := range exported.Nodes() {
		byAddr[n.Address] = n
	}
	modNode := byAddr["module.child"]
	require.NotNil(t, modNode.Module)
	assert.True(t, modNode.Module.Inputs["secret_note"].HasMark(golden.SensitiveMark))
	assert.True(t, modNode.Config.GetAttr("inputs").GetAttr("secret_note").HasMark(golden.SensitiveMark))

	displayText, err := plan.Display(exported)
	require.NoError(t, err)
	for _, secret := range []string{
		"literal-child-secret",
		"inline-worker-secret",
		"inline-deferred-worker-secret",
		"inline-dynamic-secret",
	} {
		assert.NotContains(t, displayText, secret)
	}
	assert.Contains(t, displayText, `"<sensitive>"`)
}

func TestExportPlanQCSandboxOverrideRoundTrip(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	writeExportHCL(t, filepath.Join(rootDir, "main.sofa.hcl"), `
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
  system_prompt = "Implement"

  qc {
    sandbox  = sandbox.sec_audit
    profile  = "opus-audit"
    criteria = { audit = "ok" }
  }
}
`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), rootDir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := planner.NewSofaConfig(planner.ConfigArgs{Context: context.Background(), Directory: rootDir})
	require.NoError(t, cfg.Init(loaded.Blocks))
	require.NoError(t, cfg.RunPlan())

	exported, err := cfg.ExportPlan(planner.ExportOptions{})
	require.NoError(t, err)

	encoded, err := plan.Marshal(exported)
	require.NoError(t, err)
	restored, err := plan.Unmarshal(encoded)
	require.NoError(t, err)

	byAddr := make(map[string]plan.NodeSpec)
	for _, n := range restored.Nodes() {
		byAddr[n.Address] = n
	}
	impl := byAddr["worker.implement"]
	assert.Contains(t, impl.Dependencies, "sandbox.work_env")
	assert.Contains(t, impl.Dependencies, "sandbox.sec_audit")
	assert.Equal(t, "sandbox.sec_audit", impl.Config.GetAttr("qc").GetAttr("sandbox").GetAttr("address").AsString())
	assert.Equal(t, "opus-audit", impl.Config.GetAttr("qc").GetAttr("profile").AsString())
}

func writeExportHCL(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600))
}
