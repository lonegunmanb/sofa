package installer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lonegunmanb/sofa/internal/harness"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
)

const (
	// DefaultMaxSingleFileBytes limits each unpacked plugin ZIP entry to 128 MiB.
	DefaultMaxSingleFileBytes int64 = 128 << 20
	// DefaultMaxTotalBytes limits total uncompressed plugin ZIP bytes to 512 MiB.
	DefaultMaxTotalBytes int64 = 512 << 20
	// DefaultMaxEntries limits total entries in a plugin ZIP archive.
	DefaultMaxEntries = 20000

	verifiedMarkerFile = ".verified.sha256"
)

// Platform identifies a target Linux container OS/architecture pair.
type Platform struct {
	OS   string
	Arch string
}

// Key returns the underscore-delimited asset key such as "linux_amd64".
func (p Platform) Key() string {
	return p.OS + "_" + p.Arch
}

// Validate enforces that the target container platform is Linux on amd64 or arm64.
func (p Platform) Validate() error {
	if p.OS != "linux" {
		return fmt.Errorf("harness target platform OS %q must be \"linux\"", p.OS)
	}
	switch p.Arch {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("harness target platform arch %q must be \"amd64\" or \"arm64\"", p.Arch)
	}
}

// PreparedPlugin describes a verified host harness plugin directory ready to mount into a container.
type PreparedPlugin struct {
	Name          string
	HostDir       string
	ContainerPath string
}

// ResolveOptions configures local discovery, remote lock verification, and cache extraction limits.
type ResolveOptions struct {
	PluginDir          string
	WorkingDir         string
	UserHomeDir        string
	CacheRoot          string
	LockFile           harness.PluginLockFile
	ReleaseFetcher     harness.ReleaseFetcher
	LookupEnv          func(string) (string, bool)
	MaxSingleFileBytes int64
	MaxTotalBytes      int64
	MaxEntries         int
}

// Installer resolves local development harness plugins and caches/verifies locked remote plugins.
type Installer struct {
	mu         sync.Mutex
	locks      map[string]*sync.Mutex
	fetcher    harness.ReleaseFetcher
	maxSingle  int64
	maxTotal   int64
	maxEntries int
}

// NewInstaller creates an Installer with safe default archive bounds and HTTP release fetcher.
func NewInstaller() *Installer {
	return &Installer{
		locks:      make(map[string]*sync.Mutex),
		fetcher:    harness.NewDefaultReleaseFetcher(),
		maxSingle:  DefaultMaxSingleFileBytes,
		maxTotal:   DefaultMaxTotalBytes,
		maxEntries: DefaultMaxEntries,
	}
}

// Prepare resolves and verifies the harness plugin for cfg and platform, returning the host directory
// to mount read-only at /.sofa/harness/ and the container entrypoint path.
func (i *Installer) Prepare(ctx context.Context, cfg harnessspec.HarnessConfig, platform Platform, opts ResolveOptions) (PreparedPlugin, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PreparedPlugin{}, err
	}
	if err := platform.Validate(); err != nil {
		return PreparedPlugin{}, err
	}
	cfgCopy := cfg.Clone()
	if err := cfgCopy.Validate(); err != nil {
		return PreparedPlugin{}, err
	}

	if cfgCopy.Source == "" {
		return prepareLocalPlugin(cfgCopy, platform, opts)
	}
	return i.prepareRemotePlugin(ctx, cfgCopy, platform, opts)
}

func prepareLocalPlugin(cfg harnessspec.HarnessConfig, platform Platform, opts ResolveOptions) (PreparedPlugin, error) {
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	var bases []string
	if dir := strings.TrimSpace(opts.PluginDir); dir != "" {
		bases = append(bases, dir)
	}
	if envDir, ok := lookupEnv("SOFA_PLUGIN_DIR"); ok && strings.TrimSpace(envDir) != "" {
		bases = append(bases, strings.TrimSpace(envDir))
	}
	if workDir := strings.TrimSpace(opts.WorkingDir); workDir != "" {
		bases = append(bases, filepath.Join(workDir, ".sofa", "plugins"))
	}
	if homeDir := resolveHomeDir(opts.UserHomeDir); homeDir != "" {
		bases = append(bases, filepath.Join(homeDir, ".sofa", "plugins"))
	}

	exeName := "sofa-harness-" + cfg.Name
	platformKey := platform.Key()
	searched := make([]string, 0, len(bases)*2)

	for _, base := range bases {
		candidates := []string{
			filepath.Join(base, exeName, platformKey, exeName),
			filepath.Join(base, exeName, exeName),
		}
		for _, candidate := range candidates {
			searched = append(searched, candidate)
			info, err := os.Lstat(candidate)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return PreparedPlugin{}, fmt.Errorf("inspect local harness %q candidate %q: %w", cfg.Name, candidate, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return PreparedPlugin{}, fmt.Errorf("harness %q entrypoint %q must be a regular file (not a directory or symlink)", cfg.Name, candidate)
			}
			_ = os.Chmod(candidate, 0o755)
			absHostDir, err := filepath.Abs(filepath.Dir(candidate))
			if err != nil {
				return PreparedPlugin{}, fmt.Errorf("resolve harness %q directory %q: %w", cfg.Name, filepath.Dir(candidate), err)
			}
			return PreparedPlugin{
				Name:          cfg.Name,
				HostDir:       filepath.Clean(absHostDir),
				ContainerPath: "/.sofa/harness/" + exeName,
			}, nil
		}
	}

	return PreparedPlugin{}, fmt.Errorf(
		"local harness plugin %q for %s not found; searched: %s",
		cfg.Name, platformKey, strings.Join(searched, ", "),
	)
}

func (i *Installer) prepareRemotePlugin(ctx context.Context, cfg harnessspec.HarnessConfig, platform Platform, opts ResolveOptions) (PreparedPlugin, error) {
	if opts.LockFile.FormatVersion != 0 || len(opts.LockFile.Plugins) > 0 {
		if err := harness.ValidateLockFile(opts.LockFile); err != nil {
			return PreparedPlugin{}, err
		}
	}
	if err := harness.CanSatisfyHarnessLock(cfg, opts.LockFile); err != nil {
		return PreparedPlugin{}, err
	}

	var lockedEntry *harness.PluginLockEntry
	for idx := range opts.LockFile.Plugins {
		if opts.LockFile.Plugins[idx].Name == cfg.Name {
			lockedEntry = &opts.LockFile.Plugins[idx]
			break
		}
	}
	if lockedEntry == nil {
		return PreparedPlugin{}, fmt.Errorf("harness %q is missing from plugins.lock.json", cfg.Name)
	}

	platformKey := platform.Key()
	asset, ok := lockedEntry.Assets[platformKey]
	if !ok {
		return PreparedPlugin{}, fmt.Errorf("harness %q lockfile entry has no asset for platform %q", cfg.Name, platformKey)
	}

	cacheRoot := strings.TrimSpace(opts.CacheRoot)
	if cacheRoot == "" {
		homeDir := resolveHomeDir(opts.UserHomeDir)
		if homeDir == "" {
			return PreparedPlugin{}, errors.New("harness cache root could not be resolved (UserHomeDir unavailable)")
		}
		cacheRoot = filepath.Join(homeDir, ".sofa", "cache", "plugins")
	}
	absCacheRoot, err := filepath.Abs(cacheRoot)
	if err != nil {
		return PreparedPlugin{}, fmt.Errorf("resolve harness cache root %q: %w", cacheRoot, err)
	}
	absCacheRoot = filepath.Clean(absCacheRoot)

	hostSegment := sanitizeHostDirSegment(cfg.SourceHost)
	for _, seg := range []string{hostSegment, cfg.SourceOwner, cfg.SourceRepo, cfg.Version, platformKey} {
		if err := validateCacheSegment(seg); err != nil {
			return PreparedPlugin{}, fmt.Errorf("harness %q cache path: %w", cfg.Name, err)
		}
	}

	cacheDir := filepath.Join(absCacheRoot, hostSegment, cfg.SourceOwner, cfg.SourceRepo, cfg.Version, platformKey)
	exeName := "sofa-harness-" + cfg.Name
	containerPath := "/.sofa/harness/" + exeName

	unlock := i.lockTarget(cacheDir)
	defer unlock()

	if isVerifiedCacheHit(cacheDir, exeName, asset.SHA256) {
		_ = os.Chmod(filepath.Join(cacheDir, exeName), 0o755)
		return PreparedPlugin{
			Name:          cfg.Name,
			HostDir:       cacheDir,
			ContainerPath: containerPath,
		}, nil
	}

	// Missing or corrupt cache marker: purge any stale target directory before fetching.
	if err := os.RemoveAll(cacheDir); err != nil {
		return PreparedPlugin{}, fmt.Errorf("purge stale plugin cache %q: %w", cacheDir, err)
	}

	fetcher := opts.ReleaseFetcher
	if fetcher == nil && i != nil {
		fetcher = i.fetcher
	}
	if fetcher == nil {
		fetcher = harness.NewDefaultReleaseFetcher()
	}

	token := harness.ResolveGitHubToken(cfg.SourceHost, opts.LookupEnv)
	tag := "v" + cfg.Version
	zipBytes, err := fetcher.FetchReleaseAsset(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, asset.Filename, token)
	if err != nil {
		return PreparedPlugin{}, fmt.Errorf("download harness %q asset %q: %w", cfg.Name, asset.Filename, redactSecret(err, token))
	}

	sum := sha256.Sum256(zipBytes)
	actualSHA := hex.EncodeToString(sum[:])
	if actualSHA != asset.SHA256 {
		return PreparedPlugin{}, fmt.Errorf(
			"harness %q asset %q sha256 mismatch: expected %s, got %s",
			cfg.Name, asset.Filename, asset.SHA256, actualSHA,
		)
	}

	parent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return PreparedPlugin{}, fmt.Errorf("create harness cache parent %q: %w", parent, err)
	}
	stagingDir, err := os.MkdirTemp(parent, ".tmp-*")
	if err != nil {
		return PreparedPlugin{}, fmt.Errorf("create harness staging directory: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return PreparedPlugin{}, fmt.Errorf("open harness %q archive %q: %w", cfg.Name, asset.Filename, err)
	}

	maxSingle, maxTotal, maxEntries := i.archiveLimits(opts)
	if err := extractPluginZipToDir(stagingDir, zr, maxSingle, maxTotal, maxEntries); err != nil {
		return PreparedPlugin{}, fmt.Errorf("extract harness %q archive %q: %w", cfg.Name, asset.Filename, err)
	}

	entrypointHostPath := filepath.Join(stagingDir, exeName)
	entryInfo, err := os.Lstat(entrypointHostPath)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
		return PreparedPlugin{}, fmt.Errorf("harness %q archive %q did not produce a regular file %q at bundle root", cfg.Name, asset.Filename, exeName)
	}
	_ = os.Chmod(entrypointHostPath, 0o755)

	markerPath := filepath.Join(stagingDir, verifiedMarkerFile)
	if err := os.WriteFile(markerPath, []byte(asset.SHA256+"\n"), 0o644); err != nil {
		return PreparedPlugin{}, fmt.Errorf("write harness cache verification marker: %w", err)
	}

	if err := os.RemoveAll(cacheDir); err != nil {
		return PreparedPlugin{}, fmt.Errorf("clear stale harness cache %q: %w", cacheDir, err)
	}
	if err := os.Rename(stagingDir, cacheDir); err != nil {
		return PreparedPlugin{}, fmt.Errorf("activate harness cache %q: %w", cacheDir, err)
	}
	cleanupStaging = false

	return PreparedPlugin{
		Name:          cfg.Name,
		HostDir:       cacheDir,
		ContainerPath: containerPath,
	}, nil
}

func (i *Installer) lockTarget(key string) func() {
	if i == nil {
		return func() {}
	}
	i.mu.Lock()
	if i.locks == nil {
		i.locks = make(map[string]*sync.Mutex)
	}
	mu, ok := i.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		i.locks[key] = mu
	}
	i.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

func (i *Installer) archiveLimits(opts ResolveOptions) (int64, int64, int) {
	maxSingle := DefaultMaxSingleFileBytes
	maxTotal := DefaultMaxTotalBytes
	maxEntries := DefaultMaxEntries
	if i != nil {
		if i.maxSingle > 0 {
			maxSingle = i.maxSingle
		}
		if i.maxTotal > 0 {
			maxTotal = i.maxTotal
		}
		if i.maxEntries > 0 {
			maxEntries = i.maxEntries
		}
	}
	if opts.MaxSingleFileBytes > 0 {
		maxSingle = opts.MaxSingleFileBytes
	}
	if opts.MaxTotalBytes > 0 {
		maxTotal = opts.MaxTotalBytes
	}
	if opts.MaxEntries > 0 {
		maxEntries = opts.MaxEntries
	}
	return maxSingle, maxTotal, maxEntries
}

func isVerifiedCacheHit(cacheDir, exeName, expectedSHA string) bool {
	markerBytes, err := os.ReadFile(filepath.Join(cacheDir, verifiedMarkerFile))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(markerBytes)) != expectedSHA {
		return false
	}
	info, err := os.Lstat(filepath.Join(cacheDir, exeName))
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func extractPluginZipToDir(stagingDir string, zr *zip.Reader, maxSingle, maxTotal int64, maxEntries int) error {
	if len(zr.File) > maxEntries {
		return fmt.Errorf("archive entry count %d exceeds maximum %d", len(zr.File), maxEntries)
	}

	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return fmt.Errorf("open staging root %q: %w", stagingDir, err)
	}
	defer func() { _ = root.Close() }()

	seen := make(map[string]struct{}, len(zr.File))
	var totalBytes int64

	for _, f := range zr.File {
		relName, isDir, err := validateZipEntry(f)
		if err != nil {
			return err
		}
		if _, dup := seen[relName]; dup {
			return fmt.Errorf("duplicate archive entry %q", relName)
		}
		seen[relName] = struct{}{}

		if isDir {
			dirRel := strings.TrimSuffix(relName, "/")
			if err := root.MkdirAll(dirRel, 0o755); err != nil {
				return fmt.Errorf("create extracted directory %q: %w", dirRel, err)
			}
			continue
		}

		if parent := path.Dir(relName); parent != "." && parent != "" {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("create extracted parent %q: %w", parent, err)
			}
		}

		perm := os.FileMode(0o644)
		if f.Mode().Perm()&0o111 != 0 {
			perm = 0o755
		}

		written, err := extractZipEntryFile(root, f, relName, perm, maxSingle)
		if err != nil {
			return err
		}
		totalBytes += written
		if totalBytes > maxTotal {
			return fmt.Errorf("archive total uncompressed size exceeds maximum %d bytes", maxTotal)
		}
	}
	return nil
}

func extractZipEntryFile(root *os.Root, f *zip.File, destRel string, perm os.FileMode, maxSingle int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("open archive entry %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	out, err := root.OpenFile(destRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return 0, fmt.Errorf("create extracted file %q: %w", destRel, err)
	}
	written, copyErr := io.Copy(out, io.LimitReader(rc, maxSingle+1))
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return 0, fmt.Errorf("write extracted file %q: %w", destRel, err)
	}
	if written > maxSingle {
		_ = root.Remove(destRel)
		return 0, fmt.Errorf("archive entry %q exceeds maximum single file size %d bytes", f.Name, maxSingle)
	}
	_ = root.Chmod(destRel, perm)
	return written, nil
}

func validateZipEntry(f *zip.File) (string, bool, error) {
	rawName := f.Name
	if rawName == "" {
		return "", false, fmt.Errorf("archive entry name must not be empty")
	}
	if strings.Contains(rawName, `\`) {
		return "", false, fmt.Errorf("archive entry %q must not contain backslashes", rawName)
	}
	if strings.HasPrefix(rawName, "/") {
		return "", false, fmt.Errorf("archive entry %q must not be absolute", rawName)
	}
	if len(rawName) >= 2 && isASCIIAlpha(rawName[0]) && rawName[1] == ':' {
		return "", false, fmt.Errorf("archive entry %q must not contain a drive prefix", rawName)
	}

	mode := f.Mode()
	if mode&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("archive entry %q is a symlink, which is forbidden", rawName)
	}
	isDir := strings.HasSuffix(rawName, "/") || mode.IsDir()
	if mode&os.ModeType != 0 && mode&os.ModeType != os.ModeDir {
		return "", false, fmt.Errorf("archive entry %q has unsupported non-regular file mode %v", rawName, mode)
	}

	trimmed := strings.TrimSuffix(rawName, "/")
	if trimmed == "" {
		return "", false, fmt.Errorf("archive entry %q has an empty path", rawName)
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false, fmt.Errorf("archive entry %q contains unsafe path segment %q", rawName, seg)
		}
	}
	if isDir {
		return trimmed + "/", true, nil
	}
	return trimmed, false, nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func sanitizeHostDirSegment(host string) string {
	return strings.ReplaceAll(host, ":", "_")
}

func validateCacheSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, `/\:`) {
		return fmt.Errorf("invalid cache path segment %q", seg)
	}
	return nil
}

func resolveHomeDir(override string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(home)
}

func redactSecret(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	if !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "<redacted>"))
}
