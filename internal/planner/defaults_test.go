package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPreserveURLOmittedWithSingleSource(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

preserve "delivery" {
  branch = "release/v1"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	value := cfg.EvalContext().Variables["preserve"].GetAttr("delivery")
	require.Equal(t, "https://github.com/example/repo.git", value.GetAttr("url").AsString())
	ancestors, err := cfg.GetAncestors("preserve.delivery")
	require.NoError(t, err)
	require.Contains(t, ancestors, "source.project")
}

func TestPreserveURLOmittedWithoutSourceFails(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
preserve "delivery" {
  branch = "release/v1"
}
`)

	_, _, err := config.LoadDirectoryContext(context.Background(), dir)
	require.ErrorContains(t, err, `preserve "delivery" omits url`)
	require.ErrorContains(t, err, "no source block")
}

func TestPreserveURLOmittedWithMultipleSourcesFails(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "first" {
  url    = "https://github.com/example/first.git"
  branch = "main"
}

source "second" {
  url    = "https://github.com/example/second.git"
  branch = "main"
}

preserve "delivery" {
  branch = "release/v1"
}
`)

	_, _, err := config.LoadDirectoryContext(context.Background(), dir)
	require.ErrorContains(t, err, `preserve "delivery" omits url`)
	require.ErrorContains(t, err, "multiple source blocks")
	require.ErrorContains(t, err, "first")
	require.ErrorContains(t, err, "second")
}

func TestPreserveExplicitURLWithMultipleSources(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "first" {
  url    = "https://github.com/example/first.git"
  branch = "main"
}

source "second" {
  url    = "https://github.com/example/second.git"
  branch = "main"
}

preserve "delivery" {
  url    = source.second.url
  branch = "release/v1"
}
`)

	cfg := newTestPlanner(t, dir, nil)
	initPlanner(t, cfg, dir)
	require.NoError(t, cfg.RunPlan())

	value := cfg.EvalContext().Variables["preserve"].GetAttr("delivery")
	require.Equal(t, "https://github.com/example/second.git", value.GetAttr("url").AsString())
	ancestors, err := cfg.GetAncestors("preserve.delivery")
	require.NoError(t, err)
	require.Contains(t, ancestors, "source.second")
	require.NotContains(t, ancestors, "source.first")
}
