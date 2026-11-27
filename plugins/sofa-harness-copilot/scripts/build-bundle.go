// Command build-bundle packages a self-contained Linux sofa-harness-copilot bundle.
// By default it invokes github.com/github/copilot-sdk/go/cmd/bundler inside an
// isolated temporary staging copy so embeddedcli archives are compiled directly
// into sofa-harness-copilot without polluting the repository working tree.
// When -copilotbin or -copilotsrc is provided, it populates bin/copilot instead.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	outDir := flag.String("out", "./dist", "output parent directory (creates <out>/sofa-harness-copilot)")
	arch := flag.String("arch", "amd64", "target Linux GOARCH (amd64 or arm64)")
	cacheDir := flag.String("cache-dir", "", "optional cache directory for copilot-sdk bundler artifacts")
	cliVersion := flag.String("cli-version", "", "optional CLI version passed to copilot-sdk/go cmd/bundler")
	copilotBin := flag.String("copilotbin", "", "path to prebuilt Linux copilot binary to copy into bin/copilot (skips cmd/bundler)")
	copilotSrc := flag.String("copilotsrc", "", "path to Go package to cross-compile into bin/copilot (skips cmd/bundler)")
	flag.Parse()

	if *arch != "amd64" && *arch != "arm64" {
		fatalf("unsupported -arch %q: expected amd64 or arm64", *arch)
	}
	binFlag := strings.TrimSpace(*copilotBin)
	srcFlag := strings.TrimSpace(*copilotSrc)
	if binFlag != "" && srcFlag != "" {
		fatalf("-copilotbin and -copilotsrc are mutually exclusive")
	}

	bundleRoot := filepath.Join(*outDir, "sofa-harness-copilot")
	if binFlag != "" || srcFlag != "" {
		if err := buildOverrideBundle(bundleRoot, *arch, binFlag, srcFlag); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("Built self-contained Linux bundle at %s\n", bundleRoot)
		return
	}

	if err := buildEmbeddedBundlerBundle(bundleRoot, *arch, strings.TrimSpace(*cacheDir), strings.TrimSpace(*cliVersion)); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("Built self-contained embeddedcli Linux bundle at %s\n", bundleRoot)
}

func buildOverrideBundle(bundleRoot, arch, copilotBin, copilotSrc string) error {
	binDir := filepath.Join(bundleRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create bundle directory %q: %w", binDir, err)
	}

	pluginDest := filepath.Join(bundleRoot, "sofa-harness-copilot")
	if err := buildLinuxBinary(".", "./cmd/sofa-harness-copilot", pluginDest, arch, os.Environ()); err != nil {
		return fmt.Errorf("build sofa-harness-copilot: %w", err)
	}

	copilotDest := filepath.Join(binDir, "copilot")
	if copilotSrc != "" {
		if err := buildLinuxBinary(".", copilotSrc, copilotDest, arch, os.Environ()); err != nil {
			return fmt.Errorf("build bin/copilot from %q: %w", copilotSrc, err)
		}
	} else {
		if err := copyFile(copilotBin, copilotDest, 0o755); err != nil {
			return fmt.Errorf("copy bin/copilot from %q: %w", copilotBin, err)
		}
	}
	return nil
}

func buildEmbeddedBundlerBundle(bundleRoot, arch, cacheDir, cliVersion string) error {
	pluginDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	pluginDir = filepath.Clean(pluginDir)
	repoRoot, err := findRepoRootFromPlugin(pluginDir)
	if err != nil {
		return err
	}

	absBundleRoot, err := filepath.Abs(bundleRoot)
	if err != nil {
		return fmt.Errorf("resolve output bundle dir %q: %w", bundleRoot, err)
	}
	if err := os.MkdirAll(absBundleRoot, 0o755); err != nil {
		return fmt.Errorf("create bundle directory %q: %w", absBundleRoot, err)
	}

	stageDir, err := os.MkdirTemp("", "sofa-copilot-bundler-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	if err := stageIsolatedPluginModule(pluginDir, repoRoot, stageDir); err != nil {
		return fmt.Errorf("stage isolated module copy: %w", err)
	}

	stagedCmdDir := filepath.Join(stageDir, "cmd", "sofa-harness-copilot")
	childEnv := stripGoModFlags(os.Environ())

	var bundlerCacheDir string
	cacheHit := false
	if cacheDir != "" {
		absCacheDir, err := filepath.Abs(cacheDir)
		if err != nil {
			return fmt.Errorf("resolve cache dir %q: %w", cacheDir, err)
		}
		bundlerCacheDir = filepath.Join(absCacheDir, "copilot-bundler-linux-"+arch)
		cachedEntry := filepath.Join(bundlerCacheDir, "zcopilot_linux_"+arch+".go")
		if isBundlerCacheValid(cachedEntry, cliVersion) {
			if err := copyMatchingFiles(bundlerCacheDir, stagedCmdDir, "zcopilot"); err != nil {
				return fmt.Errorf("restore cached bundler artifacts from %q: %w", bundlerCacheDir, err)
			}
			cacheHit = true
			fmt.Printf("Using cached copilot-sdk bundler artifacts from %s\n", bundlerCacheDir)
		}
	}

	if !cacheHit {
		args := []string{
			"run", "github.com/github/copilot-sdk/go/cmd/bundler",
			"--platform", "linux/" + arch,
			"--output", stagedCmdDir,
		}
		if cliVersion != "" {
			args = append(args, "--cli-version", cliVersion)
		}
		cmd := exec.Command("go", args...)
		cmd.Dir = stageDir
		cmd.Env = childEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("run copilot-sdk cmd/bundler: %w: %s", err, strings.TrimSpace(string(out)))
		}

		if bundlerCacheDir != "" {
			if err := os.MkdirAll(bundlerCacheDir, 0o755); err != nil {
				return fmt.Errorf("create bundler cache dir %q: %w", bundlerCacheDir, err)
			}
			if err := copyMatchingFiles(stagedCmdDir, bundlerCacheDir, "zcopilot"); err != nil {
				return fmt.Errorf("save bundler cache to %q: %w", bundlerCacheDir, err)
			}
		}
	}

	pluginDest := filepath.Join(absBundleRoot, "sofa-harness-copilot")
	if err := buildLinuxBinary(stageDir, "./cmd/sofa-harness-copilot", pluginDest, arch, childEnv); err != nil {
		return fmt.Errorf("build sofa-harness-copilot: %w", err)
	}
	return nil
}

func findRepoRootFromPlugin(pluginDir string) (string, error) {
	candidate := filepath.Clean(filepath.Join(pluginDir, "..", ".."))
	if fileExists(filepath.Join(candidate, "go.mod")) {
		return candidate, nil
	}
	dir := pluginDir
	for {
		if fileExists(filepath.Join(dir, "go.mod")) && fileExists(filepath.Join(dir, "plugins", "sofa-harness-copilot", "go.mod")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate sofa repository root from %q", pluginDir)
		}
		dir = parent
	}
}

func stageIsolatedPluginModule(pluginDir, repoRoot, stageDir string) error {
	modBytes, err := os.ReadFile(filepath.Join(pluginDir, "go.mod"))
	if err != nil {
		return err
	}
	replaced := strings.ReplaceAll(string(modBytes), "=> ../..", "=> "+filepath.ToSlash(repoRoot))
	if err := os.WriteFile(filepath.Join(stageDir, "go.mod"), []byte(replaced), 0o644); err != nil {
		return err
	}

	seen := make(map[string]struct{})
	var merged strings.Builder
	for _, sumPath := range []string{
		filepath.Join(pluginDir, "go.sum"),
		filepath.Join(repoRoot, "go.sum"),
	} {
		data, readErr := os.ReadFile(sumPath)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return readErr
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			merged.WriteString(trimmed)
			merged.WriteByte('\n')
		}
	}
	if err := os.WriteFile(filepath.Join(stageDir, "go.sum"), []byte(merged.String()), 0o644); err != nil {
		return err
	}

	for _, sub := range []string{"cmd", "internal"} {
		srcSub := filepath.Join(pluginDir, sub)
		dstSub := filepath.Join(stageDir, sub)
		if err := copySourceTree(srcSub, dstSub); err != nil {
			return err
		}
	}
	return nil
}

func copySourceTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, srcPath)
		if err != nil {
			return err
		}
		dstPath := dstRoot
		if rel != "." {
			dstPath = filepath.Join(dstRoot, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		if strings.HasPrefix(d.Name(), "zcopilot") {
			return nil
		}
		return copyFile(srcPath, dstPath, 0o644)
	})
}

func isBundlerCacheValid(cachedZGo, cliVersion string) bool {
	info, err := os.Lstat(cachedZGo)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	if cliVersion == "" {
		return true
	}
	data, err := os.ReadFile(cachedZGo)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), fmt.Sprintf("Version: %q", cliVersion))
}

func copyMatchingFiles(srcDir, dstDir, prefix string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	copied := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		srcPath := filepath.Join(srcDir, entry.Name())
		info, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		dstPath := filepath.Join(dstDir, entry.Name())
		if err := copyFile(srcPath, dstPath, 0o644); err != nil {
			return err
		}
		copied++
	}
	if copied == 0 {
		return fmt.Errorf("no %q files found in %q", prefix+"*", srcDir)
	}
	return nil
}

func stripGoModFlags(baseEnv []string) []string {
	env := make([]string, 0, len(baseEnv))
	for _, item := range baseEnv {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(k, "GOFLAGS") {
			fields := strings.Fields(v)
			kept := make([]string, 0, len(fields))
			for i := 0; i < len(fields); i++ {
				f := fields[i]
				if f == "-modfile" || f == "-mod" {
					i++
					continue
				}
				if strings.HasPrefix(f, "-modfile=") || strings.HasPrefix(f, "-mod=") {
					continue
				}
				kept = append(kept, f)
			}
			if len(kept) > 0 {
				env = append(env, "GOFLAGS="+strings.Join(kept, " "))
			}
			continue
		}
		env = append(env, item)
	}
	return env
}

func buildLinuxBinary(workDir, pkgPath, outPath, arch string, baseEnv []string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-o", outPath, pkgPath)
	cmd.Dir = workDir
	cmd.Env = append(append([]string{}, baseEnv...),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Chmod(outPath, 0o755)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
