//go:build windows

package daprhost

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func assertRestrictedKeyPermissions(t *testing.T, path string) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	require.NoError(t, err)

	ctrl, _, err := sd.Control()
	require.NoError(t, err)
	require.NotZero(t, ctrl&windows.SE_DACL_PROTECTED, "expected protected DACL on %s", path)

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)

	actualStr := sd.String()
	expectedACE := "(A;;FA;;;" + user.User.Sid.String() + ")"
	require.True(t, strings.HasSuffix(actualStr, expectedACE) && strings.Count(actualStr, "(") == 1,
		"expected single current-user full-access protected DACL %s, got %s", expectedACE, actualStr)
}
