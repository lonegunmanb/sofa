//go:build !windows

package daemon

import "os"

func restrictLogFile(path string, file *os.File) error {
	if file != nil {
		return file.Chmod(0o600)
	}
	return os.Chmod(path, 0o600)
}
