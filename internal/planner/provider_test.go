package planner

import (
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/golden"
	providerpkg "github.com/lonegunmanb/sofa/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestModelProviderPlansAndPublishesRetry(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
model_provider "primary" {
  type      = "openai"
  endpoint  = "https://api.example.com/v1"
  wire_api  = "responses"
  transport = "websockets"
  headers = {
    "X-Test" = "yes"
  }
  api_key = "secret"

  retry {
    lifecycle_retries    = 3
    model_call_retries   = 2
    interval_seconds     = 2
    max_interval_seconds = 20
    error_message_regex  = ["timeout"]
  }
}

output "retry" {
  value = model_provider.primary.retry
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	providerValue := cfg.EvalContext().Variables["model_provider"].GetAttr("primary")
	require.Equal(t, "model_provider.primary", providerValue.GetAttr("address").AsString())
	lifecycleRetries, _ := providerValue.GetAttr("retry").GetAttr("lifecycle_retries").AsBigFloat().Int64()
	require.Equal(t, int64(3), lifecycleRetries)
	require.Len(t, cfg.Outputs(), 1)

	var block *providerpkg.ModelProviderBlock
	for _, vertex := range cfg.GetVertices() {
		if candidate, ok := vertex.(*providerpkg.ModelProviderBlock); ok {
			block = candidate
		}
	}
	require.NotNil(t, block)
	require.True(t, block.APIKeyValue().HasMarkDeep(golden.SensitiveMark))
	require.Equal(t, "<sensitive>", golden.CtyValueToString(block.APIKeyValue()))
}

func TestModelProviderRejectsAuthenticationCombination(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
model_provider "primary" {
  type       = "openai"
  endpoint   = "https://api.example.com/v1"
  api_key    = "secret"
  api_key_ref = "TOKEN"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	err := cfg.RunPlan()
	require.ErrorContains(t, err, "at most one")
}

func TestModelProviderRefDoesNotReadEnvironment(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
model_provider "primary" {
  type        = "openai"
  endpoint    = "https://api.example.com/v1"
  api_key_ref = "MISSING_PROVIDER_KEY"
}
`)
	t.Setenv("MISSING_PROVIDER_KEY", "")

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())
}
