package planner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/stretchr/testify/require"
)

const coldStartChildEnv = "SOFA_COLD_START_CHILD"

func TestColdStartLoadOutputBeforeNewSofaConfig(t *testing.T) {
	if os.Getenv(coldStartChildEnv) == "1" {
		dir := os.Getenv("SOFA_COLD_START_DIR")
		loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
		require.NoError(t, err)
		require.False(t, diags.HasErrors(), diags.Error())
		require.Len(t, loaded.Blocks, 2)
		require.Equal(t, "source", loaded.Blocks[0].Type)
		require.Equal(t, []string{"", "project"}, loaded.Blocks[0].Labels)
		require.Equal(t, "output", loaded.Blocks[1].Type)
		require.Equal(t, []string{"", "cold_start"}, loaded.Blocks[1].Labels)

		cfg := NewSofaConfig(ConfigArgs{
			Context:   context.Background(),
			Directory: dir,
		})
		require.NoError(t, cfg.Init(loaded.Blocks))
		require.NoError(t, cfg.RunPlan())
		require.Equal(t, "ready", cfg.Outputs()[0].Value.AsString())
		return
	}

	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

output "cold_start" {
  value = "ready"
}
`)

	cmd := exec.Command(os.Args[0], "-test.run=^TestColdStartLoadOutputBeforeNewSofaConfig$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		coldStartChildEnv+"=1",
		"SOFA_COLD_START_DIR="+dir,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

const coldStartToolsChildEnv = "SOFA_COLD_START_TOOLS_CHILD"

func TestColdStartLoadTypedToolsBeforeNewSofaConfig(t *testing.T) {
	if os.Getenv(coldStartToolsChildEnv) == "1" {
		dir := os.Getenv("SOFA_COLD_START_DIR")
		loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
		require.NoError(t, err)
		require.False(t, diags.HasErrors(), diags.Error())
		require.Len(t, loaded.Blocks, 3)

		cfg := NewSofaConfig(ConfigArgs{
			Context:   context.Background(),
			Directory: dir,
		})
		require.NoError(t, cfg.Init(loaded.Blocks))
		require.NoError(t, cfg.RunPlan())
		require.Len(t, cfg.Outputs(), 1)
		require.Contains(t, cfg.Outputs()[0].Value.AsString(), "tool_go_tool_finish_")
		return
	}

	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
go_tool "finish" {
  description = "Finish"
  source = <<-GO
    import "context"
    type Input struct{}
    type Output string
    func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{Accepted: true}, nil }
  GO
}

external_tool "lookup" {
  description = "Lookup"
  program     = ["lookup"]
  input_type  = object({ query = string })
  output_type = string
}

output "tool" {
  value = tool_name(go_tool.finish)
}
`)

	cmd := exec.Command(os.Args[0], "-test.run=^TestColdStartLoadTypedToolsBeforeNewSofaConfig$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		coldStartToolsChildEnv+"=1",
		"SOFA_COLD_START_DIR="+dir,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

const coldStartHarnessChildEnv = "SOFA_COLD_START_HARNESS_CHILD"

func TestColdStartLoadHarnessBeforeNewSofaConfig(t *testing.T) {
	if os.Getenv(coldStartHarnessChildEnv) == "1" {
		dir := os.Getenv("SOFA_COLD_START_DIR")
		loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
		require.NoError(t, err)
		require.False(t, diags.HasErrors(), diags.Error())
		require.Len(t, loaded.Blocks, 2)

		cfg := NewSofaConfig(ConfigArgs{
			Context:   context.Background(),
			Directory: dir,
		})
		require.NoError(t, cfg.Init(loaded.Blocks))
		require.NoError(t, cfg.RunPlan())
		require.Len(t, cfg.Outputs(), 1)
		require.Equal(t, "github.com/lonegunmanb/sofa-harness-antigravity", cfg.Outputs()[0].Value.AsString())
		return
	}

	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.1.0"
}

output "harness_source" {
  value = harness.antigravity.source
}
`)

	cmd := exec.Command(os.Args[0], "-test.run=^TestColdStartLoadHarnessBeforeNewSofaConfig$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		coldStartHarnessChildEnv+"=1",
		"SOFA_COLD_START_DIR="+dir,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
