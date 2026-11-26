package planner

import (
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/golden"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestHarnessLocalModeZeroSpecialCase(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
harness "copilot" {}

harness "custom" {
  source  = null
  version = null
}

output "copilot_addr" {
  value = harness.copilot.address
}

output "copilot_source" {
  value = harness.copilot.source
}

output "custom_addr" {
  value = harness.custom.address
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	outputs := make(map[string]cty.Value, len(cfg.Outputs()))
	for _, out := range cfg.Outputs() {
		outputs[out.Name] = out.Value
	}
	require.Equal(t, "harness.copilot", outputs["copilot_addr"].AsString())
	require.True(t, outputs["copilot_source"].IsNull())
	require.Equal(t, cty.String, outputs["copilot_source"].Type())
	require.Equal(t, "harness.custom", outputs["custom_addr"].AsString())
}

func TestHarnessGitHubReleasesModeAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "api_secret" {
  type      = string
  sensitive = true
  default   = "super-secret-value"
}

harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "auto"
  signing_key = null

  env_refs = {
    GEMINI_API_KEY = "HOST_GEMINI_API_KEY"
  }

  options = {
    location = "us-central1"
    token    = var.api_secret
  }
}

output "info" {
  value = {
    address = harness.antigravity.address
    kind    = harness.antigravity.kind
    name    = harness.antigravity.name
    source  = harness.antigravity.source
    version = harness.antigravity.version
  }
}
`)
	t.Setenv("HOST_GEMINI_API_KEY", "")

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	info := cfg.Outputs()[0].Value
	require.Equal(t, "harness.antigravity", info.GetAttr("address").AsString())
	require.Equal(t, "harness", info.GetAttr("kind").AsString())
	require.Equal(t, "antigravity", info.GetAttr("name").AsString())
	require.Equal(t, "github.com/lonegunmanb/sofa-harness-antigravity", info.GetAttr("source").AsString())
	require.Equal(t, "0.1.0", info.GetAttr("version").AsString())

	var block *harnessspec.HarnessBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*harnessspec.HarnessBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	snap := block.HarnessConfig()
	require.Equal(t, "github.com", snap.SourceHost)
	require.Equal(t, "lonegunmanb", snap.SourceOwner)
	require.Equal(t, "sofa-harness-antigravity", snap.SourceRepo)
	require.Equal(t, harnessspec.SignatureModeAuto, snap.Signature)
	require.Equal(t, map[string]string{"GEMINI_API_KEY": "HOST_GEMINI_API_KEY"}, snap.EnvRefs)
	require.True(t, snap.Options.GetAttr("token").HasMark(golden.SensitiveMark))
	require.Equal(t, "<sensitive>", golden.CtyValueToString(snap.Options.GetAttr("token")))

	snap.EnvRefs["GEMINI_API_KEY"] = "MUTATED"
	require.Equal(t, "HOST_GEMINI_API_KEY", block.HarnessConfig().EnvRefs["GEMINI_API_KEY"])
}

func TestHarnessPreservesSensitiveSourceAndVersionInValue(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "private_repo" {
  type      = string
  sensitive = true
  default   = "ghe.example.com/org/sofa-harness-private"
}

harness "private" {
  source  = var.private_repo
  version = "1.0.0"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	val := cfg.EvalContext().Variables["harness"].GetAttr("private")
	require.True(t, val.GetAttr("source").HasMark(golden.SensitiveMark))
	require.Equal(t, "<sensitive>", golden.CtyValueToString(val.GetAttr("source")))
	require.False(t, val.GetAttr("version").HasMark(golden.SensitiveMark))
}

func TestHarnessSupportsGoldenNativeMetaConstructs(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
harness "alpha" {
  for_each = toset(["one", "two"])
  source   = "github.com/org/sofa-harness-${each.key}"
  version  = "0.1.0"

  precondition {
    condition     = each.key != ""
    error_message = "must not be empty"
  }
}

output "after_alpha" {
  depends_on = [harness.alpha]
  value      = "ready"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	got := make(map[string]string)
	for _, vertex := range cfg.GetVertices() {
		if block, ok := vertex.(*harnessspec.HarnessBlock); ok {
			got[block.Address()] = block.HarnessConfig().Source
		}
	}
	require.Equal(t, map[string]string{
		"harness.alpha[one]": "github.com/org/sofa-harness-one",
		"harness.alpha[two]": "github.com/org/sofa-harness-two",
	}, got)
	require.Equal(t, "ready", cfg.Outputs()[0].Value.AsString())
}

func TestHarnessRejectsInvalidConfigurations(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "rejects embedded model_provider",
			body: `harness "copilot" {
  model_provider = "openai"
}`,
			want: "model_provider",
		},
		{
			name: "source without version",
			body: `harness "agy" {
  source = "github.com/org/sofa-harness-agy"
}`,
			want: "together",
		},
		{
			name: "version without source",
			body: `harness "agy" {
  version = "0.1.0"
}`,
			want: "together",
		},
		{
			name: "version with leading v",
			body: `harness "agy" {
  source  = "github.com/org/sofa-harness-agy"
  version = "v0.1.0"
}`,
			want: `omit the leading "v"`,
		},
		{
			name: "version with range",
			body: `harness "agy" {
  source  = "github.com/org/sofa-harness-agy"
  version = ">= 0.1.0"
}`,
			want: "range",
		},
		{
			name: "source with https scheme",
			body: `harness "agy" {
  source  = "https://github.com/org/sofa-harness-agy"
  version = "0.1.0"
}`,
			want: "scheme",
		},
		{
			name: "pgp without signing_key",
			body: `harness "agy" {
  source    = "github.com/org/sofa-harness-agy"
  version   = "0.1.0"
  signature = "pgp"
}`,
			want: "signing_key",
		},
		{
			name: "local mode with non-default signature",
			body: `harness "copilot" {
  signature = "none"
}`,
			want: "local",
		},
		{
			name: "invalid env_refs key",
			body: `harness "copilot" {
  env_refs = {
    "1INVALID" = "VALUE"
  }
}`,
			want: "env_refs",
		},
		{
			name: "env_refs boolean value rejected without string coercion",
			body: `harness "copilot" {
  env_refs = {
    FLAG = true
  }
}`,
			want: "string",
		},
		{
			name: "env_refs null element rejected",
			body: `harness "copilot" {
  env_refs = {
    TOKEN = null
  }
}`,
			want: "null",
		},
		{
			name: "options unknown rejected",
			body: `source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

harness "agy" {
  options = {
    sha = source.project.commit_sha
  }
}`,
			want: "known",
		},
		{
			name: "numeric source rejected instead of coerced",
			body: `harness "agy" {
  source  = 123
  version = "0.1.0"
}`,
			want: "string",
		},
		{
			name: "empty source string rejected",
			body: `harness "agy" {
  source  = ""
  version = ""
}`,
			want: "empty",
		},
		{
			name: "empty harness label rejected",
			body: `harness "" {}`,
			want: "name",
		},
		{
			name: "path traversal harness label rejected",
			body: `harness "../evil" {}`,
			want: "name",
		},
		{
			name: "leading dash harness label rejected",
			body: `harness "-bad" {}`,
			want: "name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.body)
			cfg := newTestPlanner(t, dir, nil)
			initPlanner(t, cfg, dir)
			err := cfg.RunPlan()
			require.Error(t, err)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestHarnessNullEnvRefsAndOptionsAllowed(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
harness "copilot" {
  signature = null
  env_refs  = null
  options   = null
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var block *harnessspec.HarnessBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*harnessspec.HarnessBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	snap := block.HarnessConfig()
	require.Equal(t, harnessspec.SignatureModeAuto, snap.Signature)
	require.Empty(t, snap.EnvRefs)
	require.Equal(t, cty.NilVal, snap.Options)
}

func TestHarnessOptionsAllowsUntypedNullAndEmptyTuple(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
harness "agy" {
  options = {
    reasoning_effort = "high"
    custom_endpoint  = null
    extra_args       = []
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var block *harnessspec.HarnessBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*harnessspec.HarnessBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	opts := block.HarnessConfig().Options
	require.Equal(t, "high", opts.GetAttr("reasoning_effort").AsString())
	require.True(t, opts.GetAttr("custom_endpoint").IsNull())
	require.Equal(t, 0, opts.GetAttr("extra_args").LengthInt())
}

func TestHarnessSensitivePGPSigningKeyPlansCleanly(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "pgp_key" {
  type      = string
  sensitive = true
  default   = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
}

harness "agy" {
  source      = "ghe.example.com:8443/org/sofa-harness-agy"
  version     = "0.1.0"
  signature   = "pgp"
  signing_key = var.pgp_key
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	var block *harnessspec.HarnessBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*harnessspec.HarnessBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	snap := block.HarnessConfig()
	require.Equal(t, "ghe.example.com:8443", snap.SourceHost)
	require.Equal(t, harnessspec.SignatureModePGP, snap.Signature)
	require.Equal(t, "-----BEGIN PGP PUBLIC KEY BLOCK-----", snap.SigningKey)
	require.NotContains(t, golden.BlockToString(block), "BEGIN PGP PUBLIC KEY BLOCK")
}
