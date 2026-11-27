//go:build ignore

// Command install-local-plugins cross-builds and atomically installs local Linux
// sofa-harness-copilot and sofa-harness-antigravity plugin bundles into
// <plugin-dir>/sofa-harness-<name>/linux_<arch>/ so sofa server --dev and
// internal/harness/installer discover them directly in local plugin mode.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "install-local-plugins: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaultPluginDir := filepath.Join(".sofa", "plugins")
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		defaultPluginDir = filepath.Join(home, ".sofa", "plugins")
	}

	pluginDirFlag := flag.String("plugin-dir", defaultPluginDir, "destination plugin root directory")
	archFlag := flag.String("arch", "", "target Linux container architecture (amd64 or arm64; auto-detected when empty)")
	runtimeBinFlag := flag.String("runtime-bin", "podman", "container runtime binary for platform detection and fallback npm pack")
	copilotBinFlag := flag.String("copilot-linux-bin", "", "path to a pre-downloaded Linux copilot binary (skips npm pack)")
	copilotSrcFlag := flag.String("copilot-src", "", "Go package or local stub path to cross-compile as bin/copilot")
	cacheDirFlag := flag.String("cache-dir", filepath.Join(".sofa", "cache"), "cache directory for extracted Linux @github/copilot binary")
	skipAntigravitySDKFlag := flag.Bool("skip-antigravity-sdk", false, "pass --skip-sdk to build_linux_bundle.py")
	onlyFlag := flag.String("only", "", "optional filter: copilot or antigravity (empty installs both)")
	flag.Parse()

	if strings.TrimSpace(*copilotBinFlag) != "" && strings.TrimSpace(*copilotSrcFlag) != "" {
		return errors.New("-copilot-linux-bin and -copilot-src are mutually exclusive")
	}

	only := strings.TrimSpace(strings.ToLower(*onlyFlag))
	if only != "" && only != "copilot" && only != "antigravity" {
		return fmt.Errorf("unsupported -only %q: expected copilot, antigravity, or empty", *onlyFlag)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}

	arch, err := resolveTargetArch(strings.TrimSpace(*archFlag), strings.TrimSpace(*runtimeBinFlag))
	if err != nil {
		return err
	}

	pluginDir, err := filepath.Abs(strings.TrimSpace(*pluginDirFlag))
	if err != nil {
		return fmt.Errorf("resolve -plugin-dir: %w", err)
	}
	pluginDir = filepath.Clean(pluginDir)

	cacheDir := strings.TrimSpace(*cacheDirFlag)
	if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(repoRoot, cacheDir)
	}
	cacheDir = filepath.Clean(cacheDir)

	fmt.Printf("Target platform: linux/%s\nPlugin dir: %s\n", arch, pluginDir)

	if only == "" || only == "copilot" {
		if err := buildAndInstallCopilot(repoRoot, pluginDir, cacheDir, arch, strings.TrimSpace(*runtimeBinFlag), strings.TrimSpace(*copilotBinFlag), strings.TrimSpace(*copilotSrcFlag)); err != nil {
			return fmt.Errorf("install sofa-harness-copilot: %w", err)
		}
	}
	if only == "" || only == "antigravity" {
		if err := buildAndInstallAntigravity(repoRoot, pluginDir, arch, *skipAntigravitySDKFlag); err != nil {
			return fmt.Errorf("install sofa-harness-antigravity: %w", err)
		}
	}
	return nil
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	dir := filepath.Clean(wd)
	for {
		copilotDir := filepath.Join(dir, "plugins", "sofa-harness-copilot", "scripts", "build-bundle.go")
		agDir := filepath.Join(dir, "plugins", "sofa-harness-antigravity", "scripts", "build_linux_bundle.py")
		if fileExists(copilotDir) && fileExists(agDir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("must run inside sofa repository root (could not locate plugins/sofa-harness-{copilot,antigravity} from %q)", wd)
		}
		dir = parent
	}
}

func resolveTargetArch(explicit, runtimeBin string) (string, error) {
	if explicit != "" {
		switch explicit {
		case "amd64", "arm64":
			return explicit, nil
		default:
			return "", fmt.Errorf("unsupported -arch %q: expected amd64 or arm64", explicit)
		}
	}
	if runtimeBin == "" {
		runtimeBin = "podman"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	engine := sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{RuntimeBin: runtimeBin})
	if platform, err := engine.DetectPlatform(ctx); err == nil && platform.OS == "linux" {
		if platform.Arch == "amd64" || platform.Arch == "arm64" {
			return platform.Arch, nil
		}
	}
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		return runtime.GOARCH, nil
	}
	return "amd64", nil
}

func buildAndInstallCopilot(repoRoot, pluginDir, cacheDir, arch, runtimeBin, copilotBin, copilotSrc string) error {
	const exeName = "sofa-harness-copilot"
	pluginBase := filepath.Join(pluginDir, exeName)
	if err := os.MkdirAll(pluginBase, 0o755); err != nil {
		return fmt.Errorf("create plugin base dir %q: %w", pluginBase, err)
	}

	stageParent, err := os.MkdirTemp(pluginBase, ".stage-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = removeAllSafe(stageParent) }()

	buildArgs := []string{
		"run", "-mod=readonly", "./scripts/build-bundle.go",
		"-out", stageParent,
		"-arch", arch,
	}

	if copilotSrc != "" {
		compiledBin, cleanup, handledLocal, err := tryCompileLocalCopilotStub(repoRoot, stageParent, copilotSrc, arch)
		if err != nil {
			return err
		}
		defer cleanup()
		if handledLocal {
			buildArgs = append(buildArgs, "-copilotbin", compiledBin)
		} else {
			buildArgs = append(buildArgs, "-copilotsrc", copilotSrc)
		}
	} else {
		resolvedBin, err := resolveCopilotLinuxBinary(cacheDir, arch, runtimeBin, copilotBin)
		if err != nil {
			return err
		}
		buildArgs = append(buildArgs, "-copilotbin", resolvedBin)
	}

	copilotModuleDir := filepath.Join(repoRoot, "plugins", exeName)
	tmpModFile, err := prepareIsolatedCopilotModFiles(repoRoot, copilotModuleDir, stageParent)
	if err != nil {
		return fmt.Errorf("prepare isolated go.mod/go.sum: %w", err)
	}

	cmd := exec.Command("go", buildArgs...)
	cmd.Dir = copilotModuleDir
	cmd.Env = withGoFlags(os.Environ(), "-modfile="+tmpModFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(buildArgs, " "), err)
	}

	stagedBundle := filepath.Join(stageParent, exeName)
	destDir := filepath.Join(pluginBase, "linux_"+arch)
	if err := atomicReplaceDir(stagedBundle, destDir); err != nil {
		return err
	}
	if err := verifyRegularEntrypoint(destDir, exeName); err != nil {
		return err
	}
	if err := verifyRegularEntrypoint(filepath.Join(destDir, "bin"), "copilot"); err != nil {
		return err
	}
	fmt.Printf("Installed %s -> %s\n", exeName, destDir)
	return nil
}

func tryCompileLocalCopilotStub(repoRoot, stageParent, copilotSrc, arch string) (string, func(), bool, error) {
	noop := func() {}
	candidate := copilotSrc
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(repoRoot, candidate)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", noop, false, nil
	}

	outBin := filepath.Join(stageParent, "copilot-stub-linux-"+arch)
	var cmd *exec.Cmd
	if info.IsDir() {
		if fileExists(filepath.Join(candidate, "go.mod")) {
			cmd = exec.Command("go", "build", "-trimpath", "-o", outBin, ".")
			cmd.Dir = candidate
		} else if isWithin(repoRoot, candidate) && fileExists(filepath.Join(repoRoot, "go.mod")) {
			rel, relErr := filepath.Rel(repoRoot, candidate)
			if relErr != nil {
				return "", noop, false, relErr
			}
			cmd = exec.Command("go", "build", "-mod=readonly", "-trimpath", "-o", outBin, "./"+filepath.ToSlash(rel))
			cmd.Dir = repoRoot
		} else {
			cmd = exec.Command("go", "build", "-trimpath", "-o", outBin, ".")
			cmd.Dir = candidate
			cmd.Env = append(os.Environ(), "GO111MODULE=off")
		}
	} else {
		cmd = exec.Command("go", "build", "-trimpath", "-o", outBin, candidate)
		cmd.Dir = filepath.Dir(candidate)
		if !fileExists(filepath.Join(filepath.Dir(candidate), "go.mod")) {
			cmd.Env = append(os.Environ(), "GO111MODULE=off")
		}
	}
	if len(cmd.Env) == 0 {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	out, buildErr := cmd.CombinedOutput()
	if buildErr != nil {
		return "", noop, true, fmt.Errorf("cross-compile -copilot-src %q: %w: %s", copilotSrc, buildErr, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(outBin, 0o755); err != nil {
		return "", noop, true, err
	}
	return outBin, func() { _ = os.Remove(outBin) }, true, nil
}

func resolveCopilotLinuxBinary(cacheDir, arch, runtimeBin, explicitBin string) (string, error) {
	if explicitBin != "" {
		abs, err := filepath.Abs(explicitBin)
		if err != nil {
			return "", fmt.Errorf("resolve -copilot-linux-bin %q: %w", explicitBin, err)
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return "", fmt.Errorf("inspect -copilot-linux-bin %q: %w", abs, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
			return "", fmt.Errorf("-copilot-linux-bin %q must be a non-empty regular file", abs)
		}
		return abs, nil
	}

	cachedPath := filepath.Join(cacheDir, "copilot-linux-"+arch, "copilot")
	if info, err := os.Lstat(cachedPath); err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Size() > 0 {
		fmt.Printf("Using cached Linux copilot binary at %s\n", cachedPath)
		return cachedPath, nil
	}

	npmPkg := "@github/copilot-linux-x64"
	if arch == "arm64" {
		npmPkg = "@github/copilot-linux-arm64"
	}

	tmpDir, err := os.MkdirTemp("", "sofa-copilot-pack-*")
	if err != nil {
		return "", fmt.Errorf("create temp pack dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := fetchNpmTarball(tmpDir, npmPkg, runtimeBin); err != nil {
		return "", err
	}

	matches, err := filepath.Glob(filepath.Join(tmpDir, "*.tgz"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("npm pack for %s did not produce a .tgz archive in %s", npmPkg, tmpDir)
	}

	cacheSubDir := filepath.Dir(cachedPath)
	if err := os.MkdirAll(cacheSubDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %q: %w", cacheSubDir, err)
	}
	tmpOut := cachedPath + ".tmp"
	if err := extractCopilotFromTgz(matches[0], tmpOut); err != nil {
		_ = os.Remove(tmpOut)
		return "", err
	}
	if err := os.Chmod(tmpOut, 0o755); err != nil {
		_ = os.Remove(tmpOut)
		return "", err
	}
	if err := os.Rename(tmpOut, cachedPath); err != nil {
		_ = os.Remove(tmpOut)
		return "", fmt.Errorf("cache extracted copilot binary: %w", err)
	}
	fmt.Printf("Cached Linux copilot binary -> %s\n", cachedPath)
	return cachedPath, nil
}

func fetchNpmTarball(destDir, npmPkg, runtimeBin string) error {
	npmBin := "npm"
	if runtime.GOOS == "windows" {
		npmBin = "npm.cmd"
	}
	if _, err := exec.LookPath(npmBin); err == nil {
		cmd := exec.Command(npmBin, "pack", npmPkg)
		cmd.Dir = destDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s pack %s: %w", npmBin, npmPkg, err)
		}
		return nil
	}

	if runtimeBin == "" {
		runtimeBin = "podman"
	}
	if _, err := exec.LookPath(runtimeBin); err != nil {
		return fmt.Errorf("neither %s nor container runtime %q is available to fetch %s", npmBin, runtimeBin, npmPkg)
	}
	mountSpec := destDir + ":/out"
	cmd := exec.Command(runtimeBin, "run", "--rm", "-v", mountSpec, "-w", "/out", "docker.io/library/node:22-alpine", "npm", "pack", npmPkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s npm pack %s: %w", runtimeBin, npmPkg, err)
	}
	return nil
}

func extractCopilotFromTgz(tgzPath, destPath string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return fmt.Errorf("open tarball %q: %w", tgzPath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip stream %q: %w", tgzPath, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry from %q: %w", tgzPath, err)
		}
		cleanName := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		if hdr.Typeflag == tar.TypeReg && (cleanName == "package/copilot" || strings.HasSuffix(cleanName, "/copilot")) {
			out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return fmt.Errorf("create extracted binary %q: %w", destPath, err)
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return fmt.Errorf("write extracted binary %q: %w", destPath, err)
			}
			return nil
		}
	}
	return fmt.Errorf("tarball %q did not contain package/copilot regular file", tgzPath)
}

func buildAndInstallAntigravity(repoRoot, pluginDir, arch string, skipSDK bool) error {
	const exeName = "sofa-harness-antigravity"
	pluginBase := filepath.Join(pluginDir, exeName)
	if err := os.MkdirAll(pluginBase, 0o755); err != nil {
		return fmt.Errorf("create plugin base dir %q: %w", pluginBase, err)
	}

	stageParent, err := os.MkdirTemp(pluginBase, ".stage-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = removeAllSafe(stageParent) }()

	agDir := filepath.Join(repoRoot, "plugins", exeName)
	scriptArgs := []string{"scripts/build_linux_bundle.py", "--out", stageParent, "--arch", arch}
	if skipSDK {
		scriptArgs = append(scriptArgs, "--skip-sdk")
	}

	var cmd *exec.Cmd
	if _, err := exec.LookPath("uv"); err == nil {
		uvArgs := append([]string{"run", "python"}, scriptArgs...)
		cmd = exec.Command("uv", uvArgs...)
	} else if _, err := exec.LookPath("python3"); err == nil {
		cmd = exec.Command("python3", scriptArgs...)
	} else if _, err := exec.LookPath("python"); err == nil {
		cmd = exec.Command("python", scriptArgs...)
	} else {
		return errors.New("neither uv, python3, nor python was found on PATH")
	}
	cmd.Dir = agDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build_linux_bundle.py failed: %w", err)
	}

	stagedBundle := filepath.Join(stageParent, exeName)
	destDir := filepath.Join(pluginBase, "linux_"+arch)
	if err := atomicReplaceDir(stagedBundle, destDir); err != nil {
		return err
	}
	if err := verifyRegularEntrypoint(destDir, exeName); err != nil {
		return err
	}
	fmt.Printf("Installed %s -> %s\n", exeName, destDir)
	return nil
}

func atomicReplaceDir(stagedDir, destDir string) error {
	if _, err := os.Stat(stagedDir); err != nil {
		return fmt.Errorf("staged bundle %q missing: %w", stagedDir, err)
	}
	parent := filepath.Dir(destDir)
	backupDir := filepath.Join(parent, ".backup-"+filepath.Base(destDir))
	_ = removeAllSafe(backupDir)

	hadExisting := false
	if _, err := os.Lstat(destDir); err == nil {
		hadExisting = true
		if err := os.Rename(winLongPath(destDir), winLongPath(backupDir)); err != nil {
			return fmt.Errorf("backup existing bundle %q: %w", destDir, err)
		}
	}

	if err := os.Rename(winLongPath(stagedDir), winLongPath(destDir)); err != nil {
		// Fallback to long-path-safe copy if rename fails across volumes or Windows handles.
		if copyErr := copyTreeSafe(stagedDir, destDir); copyErr != nil {
			if hadExisting {
				_ = os.Rename(winLongPath(backupDir), winLongPath(destDir))
			}
			return fmt.Errorf("activate bundle %q: rename=%v copy=%w", destDir, err, copyErr)
		}
	}
	if hadExisting {
		_ = removeAllSafe(backupDir)
	}
	return nil
}

func verifyRegularEntrypoint(dir, name string) error {
	p := filepath.Join(dir, name)
	info, err := os.Lstat(winLongPath(p))
	if err != nil {
		return fmt.Errorf("verify entrypoint %q: %w", p, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("entrypoint %q must be a regular non-symlink file", p)
	}
	return nil
}

func copyTreeSafe(srcRoot, dstRoot string) error {
	return filepath.WalkDir(winLongPath(srcRoot), func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(winLongPath(srcRoot), srcPath)
		if err != nil {
			return err
		}
		dstPath := dstRoot
		if rel != "." {
			dstPath = filepath.Join(dstRoot, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(winLongPath(dstPath), 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %q in installed plugin bundle", srcPath)
		}
		if err := os.MkdirAll(winLongPath(filepath.Dir(dstPath)), 0o755); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(winLongPath(srcPath))
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(winLongPath(dstPath), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		return errors.Join(copyErr, closeErr)
	})
}

func removeAllSafe(path string) error {
	return os.RemoveAll(winLongPath(path))
}

func winLongPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if strings.HasPrefix(abs, `\\?\`) || len(abs) < 240 {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func prepareIsolatedCopilotModFiles(repoRoot, copilotModuleDir, stageParent string) (string, error) {
	modBytes, err := os.ReadFile(filepath.Join(copilotModuleDir, "go.mod"))
	if err != nil {
		return "", err
	}
	replaced := strings.ReplaceAll(string(modBytes), "=> ../..", "=> "+filepath.ToSlash(repoRoot))
	tmpMod := filepath.Join(stageParent, "copilot.mod")
	if err := os.WriteFile(tmpMod, []byte(replaced), 0o644); err != nil {
		return "", err
	}

	seen := make(map[string]struct{})
	var merged strings.Builder
	for _, sumPath := range []string{
		filepath.Join(copilotModuleDir, "go.sum"),
		filepath.Join(repoRoot, "go.sum"),
	} {
		data, readErr := os.ReadFile(sumPath)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return "", readErr
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

	tmpSum := filepath.Join(stageParent, "copilot.sum")
	if err := os.WriteFile(tmpSum, []byte(merged.String()), 0o644); err != nil {
		return "", err
	}
	return tmpMod, nil
}

func withGoFlags(baseEnv []string, extraFlag string) []string {
	env := make([]string, 0, len(baseEnv)+1)
	existing := ""
	for _, item := range baseEnv {
		if k, v, ok := strings.Cut(item, "="); ok && strings.EqualFold(k, "GOFLAGS") {
			existing = strings.TrimSpace(v)
			continue
		}
		env = append(env, item)
	}
	combined := strings.TrimSpace(existing + " " + extraFlag)
	return append(env, "GOFLAGS="+combined)
}
