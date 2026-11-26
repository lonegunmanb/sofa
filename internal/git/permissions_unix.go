//go:build !windows

package git

import "os"

func restrictPrivateKeyFile(path string) error {
	return os.Chmod(path, 0o600)
}
