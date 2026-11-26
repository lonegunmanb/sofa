//go:build !windows

package blob

import "os"

func restrictBlobFile(path string, file *os.File) error {
	if file != nil {
		return file.Chmod(0o600)
	}
	return os.Chmod(path, 0o600)
}
