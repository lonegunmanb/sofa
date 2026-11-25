package planner

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreservePublishesKnownBranch(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = "release/v1"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	value := cfg.EvalContext().Variables["preserve"].GetAttr("delivery")
	require.Equal(t, "preserve.delivery", value.GetAttr("address").AsString())
	require.Equal(t, "release/v1", value.GetAttr("branch").AsString())
	require.Equal(t, `"release/v1"`, value.GetAttr("branch_expr").AsString())
}

func TestPreserveAcceptsUnknownStringBranchAndCreatesEdge(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

output "branch" {
  value = source.project.commit_sha
}

preserve "delivery" {
  url    = source.project.url
  branch = output.branch
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	value := cfg.EvalContext().Variables["preserve"].GetAttr("delivery")
	require.False(t, value.GetAttr("branch").IsKnown())
	require.Equal(t, "output.branch", value.GetAttr("branch_expr").AsString())

	ancestors, err := cfg.GetAncestors("preserve.delivery")
	require.NoError(t, err)
	require.Contains(t, ancestors, "output.branch")
	require.Contains(t, ancestors, "source.project")
}

func TestPreserveRejectsKnownInvalidBranch(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
preserve "delivery" {
  url    = "https://github.com/example/repo.git"
  branch = "bad..branch"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	err := cfg.RunPlan()
	require.ErrorContains(t, err, "branch")
}
