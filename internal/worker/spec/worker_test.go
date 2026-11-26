package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkspaceRelativePathValidation(t *testing.T) {
	for _, value := range []string{"docs/spec.md", ".sofa/imports/design"} {
		require.NoError(t, validateWorkspaceRelativePath(value, "path"))
	}
	for _, value := range []string{"", "/absolute", "../outside", "a/../../outside"} {
		require.Error(t, validateWorkspaceRelativePath(value, "path"))
	}
}

func TestGatekeeperDefaultsAndValidation(t *testing.T) {
	config := GatekeeperBlock{
		Name:    "test",
		Command: []string{"go", "test", "./..."},
	}.Config()
	require.Equal(t, ".", config.WorkingDir)
	require.Equal(t, "10m", config.Timeout)
	require.True(t, config.RequireCleanTree)
	require.NoError(t, validateGatekeeper(config))

	config.Command = nil
	require.ErrorContains(t, validateGatekeeper(config), "command")
	config.Command = []string{"go"}
	config.WorkingDir = "../outside"
	require.ErrorContains(t, validateGatekeeper(config), "workspace-relative")
}
