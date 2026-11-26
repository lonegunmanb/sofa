//go:build windows

package artifact

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func restrictArtifactFile(path string) error {
	return applyCurrentUserDACL(path, false)
}

func restrictArtifactDir(path string) error {
	return applyCurrentUserDACL(path, true)
}

func applyCurrentUserDACL(path string, inherit bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	aceFlags := ""
	if inherit {
		aceFlags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("D:P(A;%s;FA;;;%s)", aceFlags, user.User.Sid.String()),
	)
	if err != nil {
		return fmt.Errorf("build current-user security descriptor: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read current-user dacl: %w", err)
	}
	if err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("set current-user dacl: %w", err)
	}
	return nil
}
