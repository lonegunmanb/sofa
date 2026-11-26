package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
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
	originalBlocks := loadDefaultTestBlocks(t, dir)
	require.NoError(t, cfg.Init(originalBlocks))
	for _, block := range originalBlocks {
		if block.Type == "preserve" {
			require.NotContains(t, block.Body.Attributes, "url")
		}
	}
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

	blocks := loadDefaultTestBlocks(t, dir)
	cfg := newTestPlanner(t, dir, nil)
	err := cfg.Init(blocks)
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

	blocks := loadDefaultTestBlocks(t, dir)
	cfg := newTestPlanner(t, dir, nil)
	err := cfg.Init(blocks)
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

func loadDefaultTestBlocks(t *testing.T, dir string) []*golden.HclBlock {
	t.Helper()
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	return loaded.Blocks
}

func TestRootDefaultsInspectionAndResolution(t *testing.T) {
	blocks := []*golden.HclBlock{
		testRootBlock("source", "source_a"),
		testRootBlock("source", "source_b"),
		testRootBlock("sandbox", "go_env"),
		testRootBlock("harness", "antigravity"),
		testRootBlock("model_provider", "primary"),
	}
	defaults := InspectRootDefaults(blocks)
	require.Equal(t, []string{"source_a", "source_b"}, defaults.Sources)
	require.Equal(t, []string{"go_env"}, defaults.Sandboxes)
	require.Equal(t, []string{"antigravity"}, defaults.Harnesses)
	require.Equal(t, []string{"primary"}, defaults.ModelProviders)

	sandbox, err := defaults.ResolveDefaultSandbox("worker.implement")
	require.NoError(t, err)
	require.Equal(t, "go_env", sandbox)
	harness, err := defaults.ResolveDefaultHarness("worker.implement")
	require.NoError(t, err)
	require.Equal(t, "antigravity", harness)
	provider, err := defaults.ResolveDefaultModelProvider("worker.implement")
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, "primary", *provider)
}

func TestRootDefaultsZeroAndAmbiguous(t *testing.T) {
	empty := InspectRootDefaults(nil)
	_, err := empty.ResolveDefaultSandbox("worker.implement")
	require.ErrorContains(t, err, "no sandbox")
	_, err = empty.ResolveDefaultHarness("worker.implement")
	require.ErrorContains(t, err, "no harness")
	provider, err := empty.ResolveDefaultModelProvider("worker.implement")
	require.NoError(t, err)
	require.Nil(t, provider)

	ambiguous := InspectRootDefaults([]*golden.HclBlock{
		testRootBlock("sandbox", "first"),
		testRootBlock("sandbox", "second"),
		testRootBlock("harness", "copilot"),
		testRootBlock("harness", "antigravity"),
		testRootBlock("model_provider", "primary"),
		testRootBlock("model_provider", "backup"),
	})
	_, err = ambiguous.ResolveDefaultSandbox("worker.implement")
	require.ErrorContains(t, err, "multiple sandbox")
	require.ErrorContains(t, err, "first")
	require.ErrorContains(t, err, "second")
	_, err = ambiguous.ResolveDefaultHarness("worker.implement")
	require.ErrorContains(t, err, "multiple harness")
	require.ErrorContains(t, err, "copilot")
	require.ErrorContains(t, err, "antigravity")
	_, err = ambiguous.ResolveDefaultModelProvider("worker.implement")
	require.ErrorContains(t, err, "multiple model_provider")
	require.ErrorContains(t, err, "primary")
	require.ErrorContains(t, err, "backup")
}

func testRootBlock(blockType, name string) *golden.HclBlock {
	return &golden.HclBlock{
		Block: &hclsyntax.Block{
			Type:   blockType,
			Labels: []string{"", name},
		},
	}
}
