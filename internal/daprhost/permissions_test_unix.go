//go:build !windows

package daprhost

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func assertRestrictedKeyPermissions(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "expected 0600 permissions on %s", path)
}
