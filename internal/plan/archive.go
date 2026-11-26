package plan

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/zclconf/go-cty/cty"
)

const (
	archiveFormatVersion = 1
	archiveSofaDSL       = "sofa"

	manifestEntryName   = "manifest.json"
	planEntryName       = "plan.json"
	pluginLockEntryName = "plugins.lock.json"

	// DefaultMaxSingleFileBytes limits each decompressed ZIP entry to 32 MiB.
	DefaultMaxSingleFileBytes int64 = 32 << 20
	// DefaultMaxTotalBytes limits total decompressed archive bytes to 256 MiB.
	DefaultMaxTotalBytes int64 = 256 << 20
	// DefaultMaxEntries limits total ZIP entries in an archive.
	DefaultMaxEntries = 10000
)

// Manifest records integrity digests for a self-contained .sofaplan archive.
type Manifest struct {
	FormatVersion     int    `json:"format_version"`
	SofaDSL           string `json:"sofa_dsl"`
	PlanSHA256        string `json:"plan_sha256"`
	PluginsLockSHA256 string `json:"plugins_lock_sha256,omitempty"`
	BundleSHA256      string `json:"bundle_sha256"`
}

// ArchiveSource provides the static bundle directories and optional lockfile for SaveArchive.
type ArchiveSource struct {
	ConfigDir  string
	ModulesDir string
	PluginLock *harness.PluginLockFile
}

// LoadArchiveOptions configures resource limits when unpacking a .sofaplan archive.
type LoadArchiveOptions struct {
	MaxSingleFileBytes int64
	MaxTotalBytes      int64
	MaxEntries         int
}

// LoadedArchive contains the verified Plan, Manifest, PluginLock, and extracted paths.
type LoadedArchive struct {
	Manifest   Manifest
	Plan       *Plan
	PluginLock *harness.PluginLockFile
	ExtractDir string
	ConfigDir  string
	ModulesDir string
}

type archiveEntry struct {
	name  string
	isDir bool
	data  []byte
}

// SaveArchive packages planned, optional plugins.lock.json, and static config/module bundles
// into a deterministic, 0600-restricted .sofaplan ZIP archive.
func SaveArchive(archivePath string, planned *Plan, source ArchiveSource) error {
	if planned == nil {
		return fmt.Errorf("plan is nil")
	}
	if strings.TrimSpace(archivePath) == "" {
		return fmt.Errorf("archive path is required")
	}
	if strings.TrimSpace(source.ConfigDir) == "" {
		return fmt.Errorf("config directory is required")
	}

	bundleEntries, bundleDirs, err := collectBundleEntries(planned, source)
	if err != nil {
		return err
	}
	if err := validateSkillDirectoriesInBundle(planned, bundleDirs); err != nil {
		return err
	}

	planBytes, err := Marshal(planned)
	if err != nil {
		return err
	}

	lock := source.PluginLock
	if lock == nil {
		lock = planned.PluginLock()
	}

	var lockBytes []byte
	var lockSHA string
	if lock != nil && len(lock.Plugins) > 0 {
		sortedLock := clonePluginLock(lock)
		sort.Slice(sortedLock.Plugins, func(i, j int) bool {
			return sortedLock.Plugins[i].Name < sortedLock.Plugins[j].Name
		})
		lockBytes, err = json.MarshalIndent(sortedLock, "", "  ")
		if err != nil {
			return fmt.Errorf("encode plugins lockfile: %w", err)
		}
		lockBytes = append(lockBytes, '\n')
		lockSHA = sha256Hex(lockBytes)
	}

	manifest := Manifest{
		FormatVersion:     archiveFormatVersion,
		SofaDSL:           archiveSofaDSL,
		PlanSHA256:        sha256Hex(planBytes),
		PluginsLockSHA256: lockSHA,
		BundleSHA256:      computeBundleSHA256(bundleEntries),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	allEntries := make([]archiveEntry, 0, len(bundleEntries)+3)
	allEntries = append(allEntries,
		archiveEntry{name: manifestEntryName, data: manifestBytes},
		archiveEntry{name: planEntryName, data: planBytes},
	)
	if len(lockBytes) > 0 {
		allEntries = append(allEntries, archiveEntry{name: pluginLockEntryName, data: lockBytes})
	}
	allEntries = append(allEntries, bundleEntries...)
	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].name < allEntries[j].name
	})

	absArchive, err := filepath.Abs(archivePath)
	if err != nil {
		return fmt.Errorf("resolve archive path: %w", err)
	}
	parentDir := filepath.Dir(absArchive)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}

	tmpPath, err := writeTempZip(parentDir, allEntries)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpPath) }()

	if err := restrictPlanFile(tmpPath, nil); err != nil {
		return fmt.Errorf("restrict temporary plan archive permissions: %w", err)
	}
	if err := os.Rename(tmpPath, absArchive); err != nil {
		return fmt.Errorf("activate plan archive: %w", err)
	}
	if err := restrictPlanFile(absArchive, nil); err != nil {
		return fmt.Errorf("restrict plan archive permissions: %w", err)
	}
	return nil
}

// LoadArchive verifies integrity, security invariants, and byte limits of archivePath,
// extracts bundle/ and optional plugins.lock.json into extractDir, and returns the decoded Plan.
func LoadArchive(archivePath, extractDir string, opts LoadArchiveOptions) (*LoadedArchive, error) {
	if strings.TrimSpace(archivePath) == "" {
		return nil, fmt.Errorf("archive path is required")
	}
	if strings.TrimSpace(extractDir) == "" {
		return nil, fmt.Errorf("extract directory is required")
	}
	if opts.MaxSingleFileBytes <= 0 {
		opts.MaxSingleFileBytes = DefaultMaxSingleFileBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}

	entries, err := readValidatedArchiveEntries(archivePath, opts)
	if err != nil {
		return nil, err
	}

	var manifestBytes []byte
	var planBytes []byte
	var lockBytes []byte
	var hasManifest, hasPlan, hasLock bool
	bundleEntries := make([]archiveEntry, 0, len(entries))

	for _, entry := range entries {
		switch entry.name {
		case manifestEntryName:
			hasManifest = true
			manifestBytes = entry.data
		case planEntryName:
			hasPlan = true
			planBytes = entry.data
		case pluginLockEntryName:
			hasLock = true
			lockBytes = entry.data
		default:
			bundleEntries = append(bundleEntries, entry)
		}
	}

	if !hasManifest {
		return nil, fmt.Errorf("plan archive missing %s", manifestEntryName)
	}
	if !hasPlan {
		return nil, fmt.Errorf("plan archive missing %s", planEntryName)
	}

	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode %s: %w", manifestEntryName, err)
	}
	if manifest.FormatVersion != archiveFormatVersion {
		return nil, fmt.Errorf("unsupported archive format_version %d", manifest.FormatVersion)
	}
	if manifest.SofaDSL != archiveSofaDSL {
		return nil, fmt.Errorf("unsupported archive sofa_dsl %q", manifest.SofaDSL)
	}
	if manifest.PlanSHA256 == "" || manifest.BundleSHA256 == "" {
		return nil, fmt.Errorf("%s is missing required sha256 digests", manifestEntryName)
	}
	if sha256Hex(planBytes) != manifest.PlanSHA256 {
		return nil, fmt.Errorf("plan_sha256 mismatch: %s failed integrity check", planEntryName)
	}

	var pluginLock *harness.PluginLockFile
	switch {
	case manifest.PluginsLockSHA256 != "" && !hasLock:
		return nil, fmt.Errorf("plan archive missing %s declared in %s", pluginLockEntryName, manifestEntryName)
	case manifest.PluginsLockSHA256 == "" && hasLock:
		return nil, fmt.Errorf("plan archive contains unlisted %s", pluginLockEntryName)
	case hasLock:
		if sha256Hex(lockBytes) != manifest.PluginsLockSHA256 {
			return nil, fmt.Errorf("plugins_lock_sha256 mismatch: %s failed integrity check", pluginLockEntryName)
		}
		var decodedLock harness.PluginLockFile
		if err := json.Unmarshal(lockBytes, &decodedLock); err != nil {
			return nil, fmt.Errorf("decode %s: %w", pluginLockEntryName, err)
		}
		if err := harness.ValidateLockFile(decodedLock); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", pluginLockEntryName, err)
		}
		pluginLock = &decodedLock
	}

	sort.Slice(bundleEntries, func(i, j int) bool {
		return bundleEntries[i].name < bundleEntries[j].name
	})
	if computeBundleSHA256(bundleEntries) != manifest.BundleSHA256 {
		return nil, fmt.Errorf("bundle_sha256 mismatch: archive bundle failed integrity check")
	}

	planned, err := Unmarshal(planBytes)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", planEntryName, err)
	}
	if err := validateArchiveHarnessLocks(planned, pluginLock); err != nil {
		return nil, err
	}

	absExtract, err := filepath.Abs(extractDir)
	if err != nil {
		return nil, fmt.Errorf("resolve extract directory: %w", err)
	}
	absExtract = filepath.Clean(absExtract)
	if err := extractBundleEntries(absExtract, bundleEntries, lockBytes, hasLock); err != nil {
		return nil, err
	}

	return &LoadedArchive{
		Manifest:   manifest,
		Plan:       planned,
		PluginLock: pluginLock,
		ExtractDir: absExtract,
		ConfigDir:  filepath.Join(absExtract, "config"),
		ModulesDir: filepath.Join(absExtract, "modules"),
	}, nil
}

func readValidatedArchiveEntries(archivePath string, opts LoadArchiveOptions) ([]archiveEntry, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open plan archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) == 0 {
		return nil, fmt.Errorf("plan archive is empty")
	}
	if len(reader.File) > opts.MaxEntries {
		return nil, fmt.Errorf("plan archive entry count %d exceeds maximum %d", len(reader.File), opts.MaxEntries)
	}

	seen := make(map[string]struct{}, len(reader.File))
	entries := make([]archiveEntry, 0, len(reader.File))
	var totalBytes int64

	for _, f := range reader.File {
		name, isDir, err := validateZipEntry(f)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("duplicate archive entry %q", name)
		}
		seen[name] = struct{}{}

		if isDir {
			if name == manifestEntryName || name == planEntryName || name == pluginLockEntryName {
				return nil, fmt.Errorf("archive entry %q must be a regular file", name)
			}
			entries = append(entries, archiveEntry{name: name, isDir: true})
			continue
		}

		data, err := readZipEntryBounded(f, opts.MaxSingleFileBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += int64(len(data))
		if totalBytes > opts.MaxTotalBytes {
			return nil, fmt.Errorf("archive total uncompressed size exceeds maximum %d bytes", opts.MaxTotalBytes)
		}
		entries = append(entries, archiveEntry{name: name, data: data})
	}
	return entries, nil
}

func validateZipEntry(f *zip.File) (string, bool, error) {
	rawName := f.Name
	if rawName == "" {
		return "", false, fmt.Errorf("archive contains an entry with an empty name")
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

	switch trimmed {
	case manifestEntryName, planEntryName, pluginLockEntryName:
		return trimmed, isDir, nil
	default:
		if trimmed == "bundle/config" || strings.HasPrefix(trimmed, "bundle/config/") ||
			trimmed == "bundle/modules" || strings.HasPrefix(trimmed, "bundle/modules/") {
			if isDir {
				return trimmed + "/", true, nil
			}
			return trimmed, false, nil
		}
		return "", false, fmt.Errorf("archive entry %q has unexpected path outside manifest/plan/plugins.lock/bundle", rawName)
	}
}

func readZipEntryBounded(f *zip.File, maxBytes int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open archive entry %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read archive entry %q: %w", f.Name, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("archive entry %q exceeds maximum single file size %d bytes", f.Name, maxBytes)
	}
	return data, nil
}

func extractBundleEntries(extractDir string, bundleEntries []archiveEntry, lockBytes []byte, hasLock bool) error {
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return fmt.Errorf("create extract directory: %w", err)
	}
	root, err := os.OpenRoot(extractDir)
	if err != nil {
		return fmt.Errorf("open extract root: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, base := range []string{"config", "modules"} {
		if err := root.MkdirAll(base, 0o700); err != nil {
			return fmt.Errorf("create extract %s directory: %w", base, err)
		}
	}

	for _, entry := range bundleEntries {
		rel := strings.TrimPrefix(entry.name, "bundle/")
		if entry.isDir {
			dirRel := strings.TrimSuffix(rel, "/")
			if dirRel != "" {
				if err := root.MkdirAll(dirRel, 0o700); err != nil {
					return fmt.Errorf("create directory %q: %w", dirRel, err)
				}
			}
			continue
		}
		if parent := path.Dir(rel); parent != "." && parent != "" {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return fmt.Errorf("create parent directory %q: %w", parent, err)
			}
		}
		if err := writeRootFile(root, rel, entry.data); err != nil {
			return err
		}
	}

	if hasLock {
		if err := writeRootFile(root, pluginLockEntryName, lockBytes); err != nil {
			return err
		}
	}
	return nil
}

func writeRootFile(root *os.Root, rel string, data []byte) error {
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create extracted file %q: %w", rel, err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write extracted file %q: %w", rel, err)
	}
	return nil
}

func writeTempZip(dir string, entries []archiveEntry) (string, error) {
	tmpFile, err := os.CreateTemp(dir, ".sofaplan-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary archive: %w", err)
	}
	tmpPath := tmpFile.Name()

	zw := zip.NewWriter(tmpFile)
	epoch := time.Unix(0, 0).UTC()
	var writeErr error
	for _, entry := range entries {
		hdr := &zip.FileHeader{
			Name:     entry.name,
			Method:   zip.Deflate,
			Modified: epoch,
		}
		if entry.isDir {
			hdr.Method = zip.Store
			hdr.SetMode(os.ModeDir | 0o755)
		} else {
			hdr.SetMode(0o644)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			writeErr = fmt.Errorf("write archive entry header %q: %w", entry.name, err)
			break
		}
		if !entry.isDir && len(entry.data) > 0 {
			if _, err := w.Write(entry.data); err != nil {
				writeErr = fmt.Errorf("write archive entry %q: %w", entry.name, err)
				break
			}
		}
	}
	zwCloseErr := zw.Close()
	fileCloseErr := tmpFile.Close()
	if err := errors.Join(writeErr, zwCloseErr, fileCloseErr); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func collectBundleEntries(planned *Plan, source ArchiveSource) ([]archiveEntry, map[string]bool, error) {
	bundleDirs := map[string]bool{
		"bundle/config": true,
	}
	entries := make([]archiveEntry, 0)

	cfgEntries, cfgDirs, err := walkCleanBundleDir(source.ConfigDir, "bundle/config", true, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("collect config bundle: %w", err)
	}
	entries = append(entries, cfgEntries...)
	for d := range cfgDirs {
		bundleDirs[d] = true
	}

	activePrefixes := collectActiveModuleRelDirs(planned)
	if len(activePrefixes) > 0 && strings.TrimSpace(source.ModulesDir) != "" {
		info, statErr := os.Stat(source.ModulesDir)
		switch {
		case statErr == nil && !info.IsDir():
			return nil, nil, fmt.Errorf("modules path %q is not a directory", source.ModulesDir)
		case statErr == nil && info.IsDir():
			modEntries, modDirs, err := walkCleanBundleDir(source.ModulesDir, "bundle/modules", false, activePrefixes)
			if err != nil {
				return nil, nil, fmt.Errorf("collect modules bundle: %w", err)
			}
			if len(modEntries) > 0 {
				bundleDirs["bundle/modules"] = true
			}
			entries = append(entries, modEntries...)
			for d := range modDirs {
				bundleDirs[d] = true
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return nil, nil, fmt.Errorf("inspect modules directory %q: %w", source.ModulesDir, statErr)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})
	return entries, bundleDirs, nil
}

func collectActiveModuleRelDirs(planned *Plan) map[string]struct{} {
	if planned == nil {
		return nil
	}
	active := make(map[string]struct{})
	var walk func(*Plan)
	walk = func(p *Plan) {
		if p == nil {
			return
		}
		for _, node := range p.nodes {
			if node.Kind == "module" && node.Module != nil {
				bp := path.Clean(filepath.ToSlash(node.Module.BundlePath))
				if strings.HasPrefix(bp, "modules/") {
					rel := strings.TrimPrefix(bp, "modules/")
					if rel != "" && rel != "." {
						active[rel] = struct{}{}
					}
				}
				walk(node.Module.Plan)
			}
		}
	}
	walk(planned)
	return active
}

func validateArchiveHarnessLocks(planned *Plan, pluginLock *harness.PluginLockFile) error {
	effectiveLock := harness.PluginLockFile{FormatVersion: harness.LockFormatVersion}
	if pluginLock != nil {
		effectiveLock = *pluginLock
	}
	var walk func(*Plan) error
	walk = func(p *Plan) error {
		if p == nil {
			return nil
		}
		for _, node := range p.nodes {
			switch node.Kind {
			case "harness":
				cfg, isRemote, err := harnessConfigFromNode(node)
				if err != nil {
					return err
				}
				if isRemote {
					if err := cfg.Validate(); err != nil {
						return fmt.Errorf("harness %q: %w", cfg.Name, err)
					}
					if err := harness.CanSatisfyHarnessLock(cfg, effectiveLock); err != nil {
						return err
					}
				}
			case "module":
				if node.Module != nil {
					if err := walk(node.Module.Plan); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(planned)
}

func harnessConfigFromNode(node NodeSpec) (harnessspec.HarnessConfig, bool, error) {
	if node.Config == cty.NilVal {
		return harnessspec.HarnessConfig{}, false, nil
	}
	unmarked, _ := node.Config.UnmarkDeep()
	if !unmarked.IsKnown() {
		return harnessspec.HarnessConfig{}, false, fmt.Errorf("harness %q config must not be unknown", node.Address)
	}
	if unmarked.IsNull() {
		return harnessspec.HarnessConfig{}, false, nil
	}
	if !unmarked.Type().IsObjectType() {
		return harnessspec.HarnessConfig{}, false, fmt.Errorf("harness %q config must be an object", node.Address)
	}
	readStr := func(attr string) (string, error) {
		if !unmarked.Type().HasAttribute(attr) {
			return "", nil
		}
		val := unmarked.GetAttr(attr)
		if !val.IsKnown() {
			return "", fmt.Errorf("harness %q %s must not be unknown", node.Address, attr)
		}
		if val.IsNull() {
			return "", nil
		}
		if !val.Type().Equals(cty.String) {
			return "", fmt.Errorf("harness %q %s must be a string", node.Address, attr)
		}
		return val.AsString(), nil
	}
	name, err := readStr("name")
	if err != nil {
		return harnessspec.HarnessConfig{}, false, err
	}
	source, err := readStr("source")
	if err != nil {
		return harnessspec.HarnessConfig{}, false, err
	}
	version, err := readStr("version")
	if err != nil {
		return harnessspec.HarnessConfig{}, false, err
	}
	signature, err := readStr("signature")
	if err != nil {
		return harnessspec.HarnessConfig{}, false, err
	}
	signingKey, err := readStr("signing_key")
	if err != nil {
		return harnessspec.HarnessConfig{}, false, err
	}
	if strings.TrimSpace(source) == "" {
		return harnessspec.HarnessConfig{}, false, nil
	}
	if name == "" {
		name = strings.TrimPrefix(node.Address, "harness.")
	}
	return harnessspec.HarnessConfig{
		Name:       name,
		Source:     source,
		Version:    version,
		Signature:  harnessspec.SignatureMode(signature),
		SigningKey: signingKey,
	}, true, nil
}

func walkCleanBundleDir(
	dir string,
	bundlePrefix string,
	skipInitializedMarker bool,
	activePrefixes map[string]struct{},
) ([]archiveEntry, map[string]bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%q is not a directory", dir)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()

	dirs := make(map[string]bool)
	entries := make([]archiveEntry, 0)

	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if rel == "." {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		linfo, lerr := root.Lstat(rel)
		if lerr != nil {
			return fmt.Errorf("inspect %q: %w", slashRel, lerr)
		}
		if linfo.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is a symlink, which is not allowed in archive bundles", slashRel)
		}
		if linfo.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".sofa" {
				return fs.SkipDir
			}
			if activePrefixes != nil && !matchesActiveModuleDir(slashRel, activePrefixes) {
				return fs.SkipDir
			}
			bundleDir := bundlePrefix + "/" + slashRel
			dirs[bundleDir] = true
			entries = append(entries, archiveEntry{name: bundleDir + "/", isDir: true})
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", slashRel)
		}
		name := d.Name()
		if name == ".git" || strings.HasSuffix(name, ".sofaplan") || (skipInitializedMarker && name == ".initialized.json") {
			return nil
		}
		if activePrefixes != nil && !matchesActiveModuleFile(slashRel, activePrefixes) {
			return nil
		}
		data, err := readRootRegularFile(root, rel)
		if err != nil {
			return err
		}
		entries = append(entries, archiveEntry{name: bundlePrefix + "/" + slashRel, data: data})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return entries, dirs, nil
}

func matchesActiveModuleDir(slashRel string, activePrefixes map[string]struct{}) bool {
	for active := range activePrefixes {
		if slashRel == active || strings.HasPrefix(active, slashRel+"/") || strings.HasPrefix(slashRel, active+"/") {
			return true
		}
	}
	return false
}

func matchesActiveModuleFile(slashRel string, activePrefixes map[string]struct{}) bool {
	for active := range activePrefixes {
		if strings.HasPrefix(slashRel, active+"/") {
			return true
		}
	}
	return false
}

func readRootRegularFile(root *os.Root, rel string) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", rel, err)
	}
	return data, nil
}

func validateSkillDirectoriesInBundle(planned *Plan, bundleDirs map[string]bool) error {
	var walk func(*Plan, string) error
	walk = func(p *Plan, defaultBundleBase string) error {
		if p == nil {
			return nil
		}
		base := p.bundlePath
		if base == "" {
			base = defaultBundleBase
		}
		for _, node := range p.nodes {
			switch node.Kind {
			case "worker":
				if err := checkConfigSkillDirs(node, node.Config, base, bundleDirs); err != nil {
					return err
				}
			case "dynamic_worker":
				unmarked, _ := node.Config.UnmarkDeep()
				if unmarked != cty.NilVal && unmarked.IsKnown() && !unmarked.IsNull() && unmarked.Type().IsObjectType() && unmarked.Type().HasAttribute("content") {
					if err := checkConfigSkillDirs(node, unmarked.GetAttr("content"), base, bundleDirs); err != nil {
						return err
					}
				}
			case "module":
				if node.Module != nil {
					childBase := node.Module.BundlePath
					if childBase == "" {
						childBase = node.Module.Plan.BundlePath()
					}
					if err := walk(node.Module.Plan, childBase); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(planned, "config")
}

func checkConfigSkillDirs(node NodeSpec, cfg cty.Value, moduleBundlePath string, bundleDirs map[string]bool) error {
	unmarked, _ := cfg.UnmarkDeep()
	if unmarked == cty.NilVal || !unmarked.IsKnown() || unmarked.IsNull() || !unmarked.Type().IsObjectType() {
		return nil
	}
	if err := checkSkillDirListAttr(node, unmarked, "skill_directories", moduleBundlePath, bundleDirs); err != nil {
		return err
	}
	if unmarked.Type().HasAttribute("qc") {
		qc := unmarked.GetAttr("qc")
		if qc.IsKnown() && !qc.IsNull() && qc.Type().IsObjectType() {
			if err := checkSkillDirListAttr(node, qc, "skill_directories", moduleBundlePath, bundleDirs); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkSkillDirListAttr(
	node NodeSpec,
	obj cty.Value,
	attrName string,
	moduleBundlePath string,
	bundleDirs map[string]bool,
) error {
	if !obj.Type().HasAttribute(attrName) {
		return nil
	}
	list := obj.GetAttr(attrName)
	if !list.IsKnown() || list.IsNull() {
		return nil
	}
	for it := list.ElementIterator(); it.Next(); {
		_, item := it.Element()
		if !item.IsKnown() || item.IsNull() || !item.Type().Equals(cty.String) {
			continue
		}
		raw := item.AsString()
		bundlePath, isBundle, err := resolveBundleSkillDir(raw, moduleBundlePath)
		if err != nil {
			addr := node.CanonicalAddress
			if addr == "" {
				addr = node.Address
			}
			return fmt.Errorf("%s skill_directories entry %q is invalid: %w", addr, raw, err)
		}
		if !isBundle {
			continue
		}
		if !bundleDirs[bundlePath] {
			addr := node.CanonicalAddress
			if addr == "" {
				addr = node.Address
			}
			return fmt.Errorf("%s references missing skill directory %q (expected %q in archive bundle)", addr, raw, bundlePath)
		}
	}
	return nil
}

func resolveBundleSkillDir(raw, moduleBundlePath string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, fmt.Errorf("must not be empty")
	}
	slash := filepath.ToSlash(trimmed)
	if strings.HasPrefix(slash, "/") || (len(slash) >= 2 && isASCIIAlpha(slash[0]) && slash[1] == ':') {
		return "", false, nil
	}
	cleaned := path.Clean(slash)
	switch {
	case cleaned == "config" || strings.HasPrefix(cleaned, "config/") ||
		cleaned == "modules" || strings.HasPrefix(cleaned, "modules/"):
		return "bundle/" + cleaned, true, nil
	case cleaned == ".." || strings.HasPrefix(cleaned, "../"):
		return "", false, fmt.Errorf("escapes bundle directory")
	default:
		base := moduleBundlePath
		if base == "" {
			base = "config"
		}
		joined := path.Clean(path.Join(base, cleaned))
		if joined == ".." || strings.HasPrefix(joined, "../") {
			return "", false, fmt.Errorf("escapes bundle directory")
		}
		if joined == "config" || strings.HasPrefix(joined, "config/") ||
			joined == "modules" || strings.HasPrefix(joined, "modules/") {
			return "bundle/" + joined, true, nil
		}
		return "", false, nil
	}
}

func computeBundleSHA256(entries []archiveEntry) string {
	sorted := make([]archiveEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].name < sorted[j].name
	})

	h := sha256.New()
	for _, entry := range sorted {
		h.Write([]byte(entry.name))
		h.Write([]byte{0})
		if entry.isDir {
			h.Write([]byte{'D', 0})
			continue
		}
		h.Write([]byte{'F', 0})
		h.Write([]byte(fmt.Sprintf("%d", len(entry.data))))
		h.Write([]byte{0})
		h.Write(entry.data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
