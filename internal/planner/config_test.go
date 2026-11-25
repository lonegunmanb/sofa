package planner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/stretchr/testify/require"
)

func TestInitRejectsUnregisteredBlock(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `source "project" {}`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := NewSofaConfig(ConfigArgs{
		Context:   context.Background(),
		Directory: dir,
	})
	err = cfg.Init(loaded.Blocks)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unregistered")
	require.Contains(t, err.Error(), "source")
	require.Contains(t, err.Error(), "project")
}

func TestInitRejectsExtraneousBlockLabels(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `variable "first" "second" { type = string }`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := NewSofaConfig(ConfigArgs{
		Context:   context.Background(),
		Directory: dir,
	})
	err = cfg.Init(loaded.Blocks)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Extraneous label")
	require.Contains(t, err.Error(), "variable")
}

func TestOutputsPublishKnownAndSensitiveValues(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
output "artifact_path" {
  value       = ".sofa/report.json"
  description = "generated path"
}

output "secret" {
  value     = "hidden-value"
  sensitive = true
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	outputs := cfg.Outputs()
	require.Len(t, outputs, 2)
	require.Equal(t, "artifact_path", outputs[0].Name)
	require.Equal(t, ".sofa/report.json", outputs[0].Value.AsString())
	require.Equal(t, "secret", outputs[1].Name)
	require.True(t, outputs[1].Value.HasMarkDeep(golden.SensitiveMark))
	require.Equal(t, "<sensitive>", golden.CtyValueToString(outputs[1].Value))
}

func TestOutputSchemaErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown attribute",
			body: `output "result" {
  value = 1
  unexpected = true
}`,
			want: "unexpected",
		},
		{
			name: "unknown nested block",
			body: `output "result" {
  value = 1
  unsupported {
    value = true
  }
}`,
			want: "unsupported",
		},
		{
			name: "invalid description",
			body: `output "result" {
  value       = 1
  description = 42
}`,
			want: "description",
		},
		{
			name: "null description",
			body: `output "result" {
  value       = 1
  description = null
}`,
			want: "description",
		},
		{
			name: "invalid sensitive",
			body: `output "result" {
  value     = 1
  sensitive = "yes"
}`,
			want: "sensitive",
		},
		{
			name: "missing value",
			body: `output "result" {}`,
			want: "value",
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
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestInitPreservesMissingVariableNameDiagnostic(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `variable {}`)

	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	cfg := NewSofaConfig(ConfigArgs{
		Context:   context.Background(),
		Directory: dir,
	})
	err = cfg.Init(loaded.Blocks)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Missing variable block name")
	require.NotContains(t, err.Error(), "MUST declare a concrete type")
}

func writePlannerFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
