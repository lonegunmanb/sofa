//go:build !windows

package artifact

import "os"

func restrictArtifactFile(path string) error {
	return os.Chmod(path, 0o600)
}

func restrictArtifactDir(path string) error {
	return os.Chmod(path, 0o700)
}
