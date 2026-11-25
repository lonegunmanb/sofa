package planner

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestSourcePlansOfflineAndExposesUnknownCommit(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

output "revision" {
  value = source.project.commit_sha
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	sourceValue := cfg.EvalContext().Variables["source"].GetAttr("project")
	require.Equal(t, "source.project", sourceValue.GetAttr("address").AsString())
	require.Equal(t, "source", sourceValue.GetAttr("kind").AsString())
	require.Equal(t, "project", sourceValue.GetAttr("name").AsString())
	require.Equal(t, "https://github.com/example/repo.git", sourceValue.GetAttr("url").AsString())
	require.Equal(t, "main", sourceValue.GetAttr("branch").AsString())
	require.False(t, sourceValue.GetAttr("commit_sha").IsKnown())
	require.True(t, sourceValue.GetAttr("commit_sha").Type().Equals(cty.String))

	outputs := cfg.Outputs()
	require.Len(t, outputs, 1)
	require.False(t, outputs[0].Value.IsKnown())
	require.True(t, outputs[0].Value.Type().Equals(cty.String))
}

func TestSourceRejectsCredentialReferenceCombination(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url         = "https://github.com/example/repo.git"
  branch      = "main"
  token_ref   = "GITHUB_TOKEN"
  ssh_key_ref = "DEPLOY_KEY"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	err := cfg.RunPlan()
	require.ErrorContains(t, err, "mutually exclusive")
}

func TestSourceDoesNotReadCredentialEnvironment(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url       = "git@github.com:example/repo.git"
  branch    = "main"
  token_ref = "SOFA_TEST_UNSET_TOKEN"
}
`)
	t.Setenv("SOFA_TEST_UNSET_TOKEN", "")

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())
}

func TestSourceRejectsImplicitStringConversion(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = 123
  branch = "main"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	err := cfg.RunPlan()
	require.ErrorContains(t, err, "known non-null string")
}

func TestSourceRejectsUnsupportedSchema(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown attribute",
			body: `source "project" {
  url      = "https://github.com/example/repo.git"
  branch   = "main"
  unexpected = true
}`,
			want: "unexpected",
		},
		{
			name: "unsupported nested block",
			body: `source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
  unsupported {}
}`,
			want: "unsupported",
		},
		{
			name: "for_each",
			body: `source "project" {
  for_each = toset(["project"])
  url      = "https://github.com/example/repo.git"
  branch   = "main"
}`,
			want: "for_each",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), tt.body)
			cfg := newTestPlanner(t, dir, nil)
			initPlanner(t, cfg, dir)
			err := cfg.RunPlan()
			require.ErrorContains(t, err, tt.want)
		})
	}
}
