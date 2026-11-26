//go:build !windows

package plan

import "os"

func restrictPlanFile(path string, file *os.File) error {
	if file != nil {
		return file.Chmod(0o600)
	}
	return os.Chmod(path, 0o600)
}
