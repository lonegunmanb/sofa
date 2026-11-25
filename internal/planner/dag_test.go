package planner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLocalsCycleFailsPlanning(t *testing.T) {
	dir := t.TempDir()
	writePlannerFile(t, filepath.Join(dir, "workflow.sofa.hcl"), `
locals {
  a = local.b
  b = local.a
}
`)

	cfg := newTestPlanner(t, dir, nil)
	loaded, diags, err := config.LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	err = cfg.Init(loaded.Blocks)
	if err == nil {
		err = cfg.RunPlan()
	}
	require.Error(t, err)
}
