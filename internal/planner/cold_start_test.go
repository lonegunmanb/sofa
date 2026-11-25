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
		require.Len(t, loaded.Blocks, 1)
		require.Equal(t, []string{"", "cold_start"}, loaded.Blocks[0].Labels)

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
