package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestInitRejectsMissingAndDynamicVariableTypes(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		want        string
	}{
		{
			name:        "missing",
			declaration: `variable "value" { default = "x" }`,
			want:        "concrete type",
		},
		{
			name:        "dynamic",
			declaration: "variable \"value\" {\n  type    = any\n  default = \"x\"\n}\n",
			want:        "dynamic type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.declaration)
			loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
			require.NoError(t, err)
			require.False(t, diags.HasErrors(), diags.Error())

			cfg := newTestPlanner(t, dir, nil)
			err = cfg.Init(loaded.Blocks)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Contains(t, err.Error(), "value")
		})
	}
}

func TestVariableValuePrecedence(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "env_value" {
  type    = string
  default = "default"
}
variable "default_file" {
  type    = string
  default = "default"
}
variable "json_file" {
  type    = string
  default = "default"
}
variable "auto_value" {
  type    = string
  default = "default"
}
variable "cli_value" {
  type    = string
  default = "default"
}
`)
	writePlannerFile(t, filepath.Join(dir, "sofa.sofavars"), `
default_file = "hcl-file"
json_file    = "hcl-file"
`)
	writePlannerFile(t, filepath.Join(dir, "sofa.sofavars.json"), `{
  "json_file": "json-file"
}`)
	writePlannerFile(t, filepath.Join(dir, "a.auto.sofavars"), `auto_value = "a-auto"`)
	writePlannerFile(t, filepath.Join(dir, "z.auto.sofavars"), `auto_value = "z-auto"`)
	t.Setenv("SOFA_VAR_env_value", "environment")
	t.Setenv("SOFA_VAR_default_file", "environment")

	cfg := newTestPlanner(t, dir, []golden.CliFlagAssignedVariables{
		golden.NewCliFlagAssignedVariable("cli_value", `"cli"`),
	})
	initPlanner(t, cfg, dir)

	varValues := variableValues(t, cfg)
	require.Equal(t, "environment", varValues["env_value"].AsString())
	require.Equal(t, "hcl-file", varValues["default_file"].AsString())
	require.Equal(t, "json-file", varValues["json_file"].AsString())
	require.Equal(t, "z-auto", varValues["auto_value"].AsString())
	require.Equal(t, "cli", varValues["cli_value"].AsString())
}

func TestVariableValidationFailureIncludesMessage(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "name" {
  type    = string
  default = "x"

  validation {
    condition     = length(var.name) > 2
    error_message = "name is too short"
  }
}
`)

	cfg := newTestPlanner(t, dir, nil)
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	require.Error(t, err)
	require.Contains(t, err.Error(), "name is too short")
}

func TestInitRejectsUndeclaredExplicitVariable(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "known" {
  type    = string
  default = "value"
}
`)

	cfg := newTestPlanner(t, dir, []golden.CliFlagAssignedVariables{
		golden.NewCliFlagAssignedVariable("unknown", `"value"`),
	})
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	err = cfg.Init(loaded.Blocks)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown")
	require.Contains(t, err.Error(), "cannot find a variable")
}

func TestSensitiveVariablePropagatesThroughLocals(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "secret" {
  type      = string
  default   = "hidden-value"
  sensitive = true
}

locals {
  nested = {
    value = var.secret
  }
  derived = local.nested.value
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	ctx := cfg.EvalContext()
	require.True(t, specpkg.IsSensitive(ctx.Variables["var"].GetAttr("secret")))
	require.True(t, specpkg.IsSensitive(ctx.Variables["local"].GetAttr("derived")))
}

func TestSensitiveVariablePropagatesThroughOutput(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "secret" {
  type      = string
  default   = "hidden-value"
  sensitive = true
}

locals {
  derived = "value=${var.secret}"
}

output "derived" {
  value = local.derived
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	outputs := cfg.Outputs()
	require.Len(t, outputs, 1)
	require.True(t, specpkg.IsSensitive(outputs[0].Value))
	require.True(t, outputs[0].Sensitive)
	require.Equal(t, "<sensitive>", golden.CtyValueToString(outputs[0].Value))
}

func TestOnePreservesSensitiveSourcesInOutputs(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
variable "secrets" {
  type      = list(string)
  default   = ["hidden-list"]
  sensitive = true
}

variable "secret" {
  type      = string
  default   = "hidden-element"
  sensitive = true
}

output "from_sensitive_list" {
  value = one(var.secrets)
}

output "from_sensitive_element" {
  value = one([var.secret])
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	outputs := cfg.Outputs()
	require.Len(t, outputs, 2)
	for _, output := range outputs {
		require.True(t, specpkg.IsSensitive(output.Value), output.Name)
		require.True(t, output.Sensitive, output.Name)
		require.Equal(t, "<sensitive>", golden.CtyValueToString(output.Value), output.Name)
	}
}

func newTestPlanner(t *testing.T, dir string, cliVariables []golden.CliFlagAssignedVariables) *SofaConfig {
	t.Helper()
	return NewSofaConfig(ConfigArgs{
		Context:                  context.Background(),
		Directory:                dir,
		CliFlagAssignedVariables: cliVariables,
	})
}

func initPlanner(t *testing.T, cfg *SofaConfig, dir string) {
	t.Helper()
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	require.NoError(t, cfg.Init(loaded.Blocks))
}

func variableValues(t *testing.T, cfg *SofaConfig) map[string]cty.Value {
	t.Helper()
	value, ok := cfg.EvalContext().Variables["var"]
	require.True(t, ok)
	return value.AsValueMap()
}
