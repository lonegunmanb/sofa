//go:build ignore

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "build-webui: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	webDir := filepath.Join(cwd, "web")
	if _, err := os.Stat(filepath.Join(webDir, "package.json")); err != nil {
		return fmt.Errorf("must be executed from repo root containing web/package.json: %w", err)
	}

	npmBin := "npm"
	if runtime.GOOS == "windows" {
		npmBin = "npm.cmd"
	}

	cmd := exec.Command(npmBin, "--prefix", "web", "run", "build")
	cmd.Dir = cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s --prefix web run build: %w", npmBin, err)
	}

	srcDist := filepath.Join(webDir, "dist")
	if _, err := os.Stat(filepath.Join(srcDist, "index.html")); err != nil {
		return fmt.Errorf("missing built index.html in %s: %w", srcDist, err)
	}

	dstDist := filepath.Join(cwd, "internal", "webui", "dist")
	stageDir := filepath.Join(cwd, "internal", "webui", ".dist-stage")
	backupDir := filepath.Join(cwd, "internal", "webui", ".dist-backup")

	_ = os.RemoveAll(stageDir)
	_ = os.RemoveAll(backupDir)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	// Preserve .keep inside internal/webui/dist so go:embed all:dist always tracks the folder.
	keepSrc := filepath.Join(dstDist, ".keep")
	keepDst := filepath.Join(stageDir, ".keep")
	if keepData, err := os.ReadFile(keepSrc); err == nil {
		if err := os.WriteFile(keepDst, keepData, 0o644); err != nil {
			return fmt.Errorf("preserve .keep: %w", err)
		}
	} else {
		const defaultKeep = "# Keeps internal/webui/dist tracked so //go:embed all:dist always compiles even before frontend build.\n"
		if err := os.WriteFile(keepDst, []byte(defaultKeep), 0o644); err != nil {
			return fmt.Errorf("write .keep: %w", err)
		}
	}

	if err := copyTree(srcDist, stageDir); err != nil {
		return fmt.Errorf("copy web/dist to stage: %w", err)
	}

	if _, err := os.Stat(dstDist); err == nil {
		if err := os.Rename(dstDist, backupDir); err != nil {
			return fmt.Errorf("backup existing dist: %w", err)
		}
	}
	if err := os.Rename(stageDir, dstDist); err != nil {
		if _, bErr := os.Stat(backupDir); bErr == nil {
			_ = os.Rename(backupDir, dstDist)
		}
		return fmt.Errorf("swap staged dist: %w", err)
	}
	cleanupStage = false
	_ = os.RemoveAll(backupDir)

	fmt.Printf("Synced %s -> %s\n", srcDist, dstDist)
	return nil
}

func copyTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, srcPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		return copyFile(srcPath, dstPath)
	})
}

func copyFile(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
