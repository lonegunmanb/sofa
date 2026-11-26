package artifact

import (
	"archive/zip"
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lonegunmanb/sofa/internal/blob"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

const (
	// DefaultMaxSingleFileBytes limits each packed or unpacked ZIP entry to 32 MiB.
	DefaultMaxSingleFileBytes int64 = 32 << 20
	// DefaultMaxTotalBytes limits total uncompressed ZIP bytes to 256 MiB.
	DefaultMaxTotalBytes int64 = 256 << 20
	// DefaultMaxEntries limits total entries in an artifact or staging ZIP.
	DefaultMaxEntries = 10000
)

// PublishedArtifact describes an immutable, content-addressed artifact stored in BlobStore.
type PublishedArtifact struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	RelPath       string `json:"rel_path"`
	ContainerPath string `json:"container_path"`
	BlobKey       string `json:"blob_key"`
	SHA256        string `json:"sha256"`
	SizeBytes     int64  `json:"size_bytes"`
}

// ImportSource identifies one published upstream artifact supplying an import_artifact block.
type ImportSource struct {
	Artifact PublishedArtifact
}

// ImportRequest describes an import_artifact staging target and its 1..N upstream sources.
type ImportRequest struct {
	Name    string
	RelPath string
	Sources []ImportSource
}

// ServiceOptions configures local caching and archive size/entry safety limits for Service.
type ServiceOptions struct {
	CacheDir           string
	MaxSingleFileBytes int64
	MaxTotalBytes      int64
	MaxEntries         int
}

// Service packages, publishes, stages, and snapshots worker artifacts using a blob.Store.
type Service struct {
	store              blob.Store
	cacheDir           string
	maxSingleFileBytes int64
	maxTotalBytes      int64
	maxEntries         int
	cacheMu            sync.Mutex
	cacheLocks         map[string]*sync.Mutex
}

// NewService creates an artifact Service backed by store and optional cache/size limits.
func NewService(store blob.Store, opts ServiceOptions) *Service {
	if opts.MaxSingleFileBytes <= 0 {
		opts.MaxSingleFileBytes = DefaultMaxSingleFileBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	return &Service{
		store:              store,
		cacheDir:           strings.TrimSpace(opts.CacheDir),
		maxSingleFileBytes: opts.MaxSingleFileBytes,
		maxTotalBytes:      opts.MaxTotalBytes,
		maxEntries:         opts.MaxEntries,
		cacheLocks:         make(map[string]*sync.Mutex),
	}
}

// Publish deterministically archives each valid artifact from artifactsDir and uploads it to BlobStore.
func (s *Service) Publish(ctx context.Context, runID, artifactsDir string, validDecls []workerspec.Artifact) ([]PublishedArtifact, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("artifact service store is required")
	}
	if err := blob.ValidateKey(fmt.Sprintf("runs/%s/artifacts/probe.zip", runID)); err != nil {
		return nil, fmt.Errorf("invalid runID %q: %w", runID, err)
	}
	if strings.TrimSpace(artifactsDir) == "" {
		return nil, fmt.Errorf("artifact directory is required")
	}
	if len(validDecls) == 0 {
		return []PublishedArtifact{}, nil
	}

	root, err := os.OpenRoot(artifactsDir)
	if err != nil {
		return nil, fmt.Errorf("open artifact root %q: %w", artifactsDir, err)
	}
	defer func() { _ = root.Close() }()

	published := make([]PublishedArtifact, 0, len(validDecls))
	for _, decl := range validDecls {
		if err := workerspec.ValidateSafeLabel(decl.Name, "artifact", true); err != nil {
			return nil, err
		}
		if decl.Type != "file" && decl.Type != "directory" {
			return nil, fmt.Errorf("artifact %q type must be file or directory", decl.Name)
		}
		rel, err := cleanRelativePath(decl.Path, fmt.Sprintf("artifact %q path", decl.Name))
		if err != nil {
			return nil, err
		}
		decl.Path = rel

		entries, err := s.collectArtifactEntries(root, decl)
		if err != nil {
			return nil, err
		}
		id := "art_" + uuid.NewString()
		blobKey := fmt.Sprintf("runs/%s/artifacts/%s.zip", runID, id)

		meta, err := s.packAndPut(ctx, root, blobKey, entries)
		if err != nil {
			return nil, fmt.Errorf("publish artifact %q: %w", decl.Name, err)
		}
		published = append(published, PublishedArtifact{
			ID:            id,
			Name:          decl.Name,
			Type:          decl.Type,
			RelPath:       decl.Path,
			ContainerPath: decl.ContainerPath(),
			BlobKey:       blobKey,
			SHA256:        meta.SHA256,
			SizeBytes:     meta.SizeBytes,
		})
	}
	return published, nil
}

// StageImports fetches, verifies, and safely extracts upstream artifacts into importsDir.
func (s *Service) StageImports(ctx context.Context, importsDir string, requests []ImportRequest) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("artifact service store is required")
	}
	if strings.TrimSpace(importsDir) == "" {
		return fmt.Errorf("imports directory is required")
	}

	targetRels := make([]string, len(requests))
	for i, req := range requests {
		targetRel, err := cleanRelativePath(req.RelPath, fmt.Sprintf("import %q rel_path", req.Name))
		if err != nil {
			return err
		}
		if len(req.Sources) == 0 {
			return fmt.Errorf("import %q must have at least one source", req.Name)
		}
		targetRels[i] = targetRel
	}
	if err := checkImportPathOverlaps(targetRels); err != nil {
		return err
	}

	if err := os.MkdirAll(importsDir, 0o700); err != nil {
		return fmt.Errorf("create imports directory %q: %w", importsDir, err)
	}
	root, err := os.OpenRoot(importsDir)
	if err != nil {
		return fmt.Errorf("open imports root %q: %w", importsDir, err)
	}
	defer func() { _ = root.Close() }()

	for i, req := range requests {
		targetRel := targetRels[i]
		if len(req.Sources) == 1 {
			src := req.Sources[0]
			if err := s.stageSingleSource(ctx, root, targetRel, src.Artifact); err != nil {
				return fmt.Errorf("stage import %q: %w", req.Name, err)
			}
			continue
		}

		if err := root.MkdirAll(targetRel, 0o755); err != nil {
			return fmt.Errorf("create import directory %q: %w", targetRel, err)
		}
		for idx, src := range req.Sources {
			if err := workerspec.ValidateSafeLabel(src.Artifact.Name, "import source artifact", true); err != nil {
				return err
			}
			subRel := path.Join(targetRel, fmt.Sprintf("%03d_%s", idx, src.Artifact.Name))
			if err := s.stageSingleSource(ctx, root, subRel, src.Artifact); err != nil {
				return fmt.Errorf("stage import %q source %d (%s): %w", req.Name, idx, src.Artifact.Name, err)
			}
		}
	}
	return nil
}

func checkImportPathOverlaps(relPaths []string) error {
	if len(relPaths) <= 1 {
		return nil
	}
	sorted := make([]string, len(relPaths))
	copy(sorted, relPaths)
	sort.Strings(sorted)
	for i := 0; i < len(sorted)-1; i++ {
		a := sorted[i]
		for j := i + 1; j < len(sorted); j++ {
			b := sorted[j]
			if a == b || strings.HasPrefix(b, a+"/") {
				return fmt.Errorf("import rel_paths %q and %q overlap", a, b)
			}
			if !strings.HasPrefix(b, a) {
				break
			}
		}
	}
	return nil
}

// SnapshotStagingDir packs a non-empty staging artifactsDir into a deterministic ZIP and uploads
// it to blobKey. If artifactsDir does not exist or has no entries, it returns (nil, nil).
func (s *Service) SnapshotStagingDir(ctx context.Context, blobKey, artifactsDir string) (*blob.ObjectMeta, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("artifact service store is required")
	}
	if err := blob.ValidateKey(blobKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(artifactsDir) == "" {
		return nil, fmt.Errorf("artifact directory is required")
	}

	dirInfo, err := os.Lstat(artifactsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect staging directory %q: %w", artifactsDir, err)
	}
	if dirInfo.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("staging directory %q is a symlink, which is forbidden", artifactsDir)
	}
	if !dirInfo.IsDir() {
		return nil, fmt.Errorf("staging directory %q is not a directory", artifactsDir)
	}

	root, err := os.OpenRoot(artifactsDir)
	if err != nil {
		return nil, fmt.Errorf("open staging root %q: %w", artifactsDir, err)
	}
	defer func() { _ = root.Close() }()

	entries, err := collectTreeEntries(root, ".", "")
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	meta, err := s.packAndPut(ctx, root, blobKey, entries)
	if err != nil {
		return nil, fmt.Errorf("snapshot staging directory: %w", err)
	}
	return &meta, nil
}

// RestoreStagingDir fetches a staging snapshot from BlobStore, verifies its SHA-256 digest,
// and restores its contents safely into destArtifactsDir.
func (s *Service) RestoreStagingDir(ctx context.Context, meta blob.ObjectMeta, destArtifactsDir string) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("artifact service store is required")
	}
	if err := blob.ValidateKey(meta.Key); err != nil {
		return err
	}
	if !blob.IsValidSHA256Hex(meta.SHA256) {
		return fmt.Errorf("%w: invalid snapshot sha256 %q", blob.ErrDigestMismatch, meta.SHA256)
	}
	if strings.TrimSpace(destArtifactsDir) == "" {
		return fmt.Errorf("destination artifacts directory is required")
	}

	zipPath, cleanup, err := s.resolveVerifiedZip(ctx, meta.Key, meta.SHA256)
	if err != nil {
		return fmt.Errorf("fetch staging snapshot: %w", err)
	}
	defer cleanup()

	if info, lerr := os.Lstat(destArtifactsDir); lerr == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("destination artifacts directory %q is a symlink, which is forbidden", destArtifactsDir)
		}
		if !info.IsDir() {
			return fmt.Errorf("destination artifacts directory %q is not a directory", destArtifactsDir)
		}
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return fmt.Errorf("inspect destination artifacts directory %q: %w", destArtifactsDir, lerr)
	}

	if err := os.MkdirAll(destArtifactsDir, 0o700); err != nil {
		return fmt.Errorf("create destination artifacts directory %q: %w", destArtifactsDir, err)
	}
	root, err := os.OpenRoot(destArtifactsDir)
	if err != nil {
		return fmt.Errorf("open destination artifacts root %q: %w", destArtifactsDir, err)
	}
	defer func() { _ = root.Close() }()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open snapshot archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	if err := s.extractTreeEntries(root, zr.File, "", ""); err != nil {
		return fmt.Errorf("restore staging snapshot: %w", err)
	}
	return nil
}

type zipPackEntry struct {
	zipName string
	hostRel string
	isDir   bool
}

func (s *Service) collectArtifactEntries(root *os.Root, decl workerspec.Artifact) ([]zipPackEntry, error) {
	info, err := root.Lstat(decl.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact %q (%s): %w", decl.Name, decl.Path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("artifact %q path %q is a symlink, which is forbidden", decl.Name, decl.Path)
	}
	if decl.Type == "file" {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact %q (%s) is not a regular file", decl.Name, decl.Path)
		}
		return []zipPackEntry{{zipName: "payload", hostRel: decl.Path, isDir: false}}, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %q (%s) is not a directory", decl.Name, decl.Path)
	}
	return collectTreeEntries(root, decl.Path, "tree/")
}

func collectTreeEntries(root *os.Root, baseRel, zipPrefix string) ([]zipPackEntry, error) {
	entries := make([]zipPackEntry, 0)
	err := fs.WalkDir(root.FS(), baseRel, func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		linfo, lerr := root.Lstat(rel)
		if lerr != nil {
			return lerr
		}
		if linfo.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is a symlink, which is forbidden", rel)
		}
		if rel == baseRel {
			return nil
		}
		subRel := rel
		if baseRel != "." {
			subRel = strings.TrimPrefix(rel, baseRel+"/")
		}
		if linfo.IsDir() {
			entries = append(entries, zipPackEntry{
				zipName: zipPrefix + subRel + "/",
				hostRel: rel,
				isDir:   true,
			})
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", rel)
		}
		entries = append(entries, zipPackEntry{
			zipName: zipPrefix + subRel,
			hostRel: rel,
			isDir:   false,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].zipName < entries[j].zipName
	})
	return entries, nil
}

func (s *Service) packAndPut(ctx context.Context, root *os.Root, blobKey string, entries []zipPackEntry) (blob.ObjectMeta, error) {
	if len(entries) > s.maxEntries {
		return blob.ObjectMeta{}, fmt.Errorf("archive entry count %d exceeds maximum %d", len(entries), s.maxEntries)
	}

	tmpZip, err := os.CreateTemp("", "sofa-art-*.zip")
	if err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("create temporary archive: %w", err)
	}
	tmpPath := tmpZip.Name()
	defer func() {
		_ = tmpZip.Close()
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(tmpZip, hasher))
	epoch := time.Unix(0, 0).UTC()
	var totalBytes int64

	for _, entry := range entries {
		hdr := &zip.FileHeader{
			Name:     entry.zipName,
			Method:   zip.Deflate,
			Modified: epoch,
		}
		if entry.isDir {
			hdr.Method = zip.Store
			hdr.SetMode(os.ModeDir | 0o755)
			if _, err := zw.CreateHeader(hdr); err != nil {
				return blob.ObjectMeta{}, fmt.Errorf("write zip directory %q: %w", entry.zipName, err)
			}
			continue
		}

		hdr.SetMode(0o644)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return blob.ObjectMeta{}, fmt.Errorf("write zip entry %q: %w", entry.zipName, err)
		}
		written, err := copyRootFileBounded(root, entry.hostRel, w, s.maxSingleFileBytes)
		if err != nil {
			return blob.ObjectMeta{}, err
		}
		totalBytes += written
		if totalBytes > s.maxTotalBytes {
			return blob.ObjectMeta{}, fmt.Errorf("archive total uncompressed size exceeds maximum %d bytes", s.maxTotalBytes)
		}
	}

	if err := zw.Close(); err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("finalize zip archive: %w", err)
	}
	if _, err := tmpZip.Seek(0, io.SeekStart); err != nil {
		return blob.ObjectMeta{}, fmt.Errorf("rewind temporary zip archive: %w", err)
	}
	expectedSHA256 := hex.EncodeToString(hasher.Sum(nil))
	return s.store.Put(ctx, blobKey, tmpZip, expectedSHA256)
}

func copyRootFileBounded(root *os.Root, hostRel string, dst io.Writer, maxSingle int64) (int64, error) {
	f, err := root.Open(hostRel)
	if err != nil {
		return 0, fmt.Errorf("open %q: %w", hostRel, err)
	}
	defer func() { _ = f.Close() }()

	written, err := io.Copy(dst, io.LimitReader(f, maxSingle+1))
	if err != nil {
		return 0, fmt.Errorf("copy %q: %w", hostRel, err)
	}
	if written > maxSingle {
		return 0, fmt.Errorf("file %q exceeds maximum single file size %d bytes", hostRel, maxSingle)
	}
	return written, nil
}

func (s *Service) stageSingleSource(ctx context.Context, root *os.Root, targetRel string, art PublishedArtifact) error {
	if art.Type != "file" && art.Type != "directory" {
		return fmt.Errorf("artifact %q has invalid type %q", art.Name, art.Type)
	}
	zipPath, cleanup, err := s.resolveVerifiedZip(ctx, art.BlobKey, art.SHA256)
	if err != nil {
		return err
	}
	defer cleanup()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open artifact archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	if art.Type == "file" {
		return s.extractSinglePayload(root, zr.File, targetRel)
	}
	return s.extractTreeEntries(root, zr.File, "tree/", targetRel)
}

func (s *Service) resolveVerifiedZip(ctx context.Context, blobKey, expectedSHA256 string) (string, func(), error) {
	if err := blob.ValidateKey(blobKey); err != nil {
		return "", nil, err
	}
	if !blob.IsValidSHA256Hex(expectedSHA256) {
		return "", nil, fmt.Errorf("%w: invalid sha256 %q", blob.ErrDigestMismatch, expectedSHA256)
	}

	if s.cacheDir != "" {
		unlock := s.lockForDigest(expectedSHA256)
		defer unlock()

		if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
			return "", nil, fmt.Errorf("create artifact cache directory %q: %w", s.cacheDir, err)
		}
		if err := restrictArtifactDir(s.cacheDir); err != nil {
			return "", nil, fmt.Errorf("restrict artifact cache directory %q: %w", s.cacheDir, err)
		}
		cachePath := filepath.Join(s.cacheDir, expectedSHA256+".zip")
		if info, err := os.Lstat(cachePath); err == nil {
			if info.Mode().IsRegular() {
				if actual, hashErr := sha256File(cachePath); hashErr == nil && actual == expectedSHA256 {
					return cachePath, func() {}, nil
				}
			}
			_ = os.Remove(cachePath)
		}

		rc, _, err := s.store.Get(ctx, blobKey, expectedSHA256)
		if err != nil {
			return "", nil, err
		}
		tmpFile, err := os.CreateTemp(s.cacheDir, "cache-*.tmp")
		if err != nil {
			_ = rc.Close()
			return "", nil, fmt.Errorf("create cache temp file: %w", err)
		}
		tmpPath := tmpFile.Name()
		_, copyErr := io.Copy(tmpFile, rc)
		rcCloseErr := rc.Close()
		tmpCloseErr := tmpFile.Close()
		if err := errors.Join(copyErr, rcCloseErr, tmpCloseErr); err != nil {
			_ = os.Remove(tmpPath)
			return "", nil, err
		}
		if err := restrictArtifactFile(tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return "", nil, fmt.Errorf("restrict cache temp file: %w", err)
		}
		if err := os.Rename(tmpPath, cachePath); err != nil {
			_ = os.Remove(tmpPath)
			return "", nil, fmt.Errorf("commit cache archive: %w", err)
		}
		if err := restrictArtifactFile(cachePath); err != nil {
			return "", nil, fmt.Errorf("restrict cache archive: %w", err)
		}
		return cachePath, func() {}, nil
	}

	rc, _, err := s.store.Get(ctx, blobKey, expectedSHA256)
	if err != nil {
		return "", nil, err
	}
	tmpFile, err := os.CreateTemp("", "sofa-stage-*.zip")
	if err != nil {
		_ = rc.Close()
		return "", nil, fmt.Errorf("create temp stage file: %w", err)
	}
	tmpPath := tmpFile.Name()
	_, copyErr := io.Copy(tmpFile, rc)
	rcCloseErr := rc.Close()
	tmpCloseErr := tmpFile.Close()
	if err := errors.Join(copyErr, rcCloseErr, tmpCloseErr); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, err
	}
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

func (s *Service) extractSinglePayload(root *os.Root, files []*zip.File, targetRel string) error {
	if len(files) == 0 {
		return fmt.Errorf("file artifact archive is empty")
	}
	if len(files) > s.maxEntries || len(files) != 1 {
		return fmt.Errorf("file artifact archive must contain exactly 1 entry (%q), got %d", "payload", len(files))
	}
	f := files[0]
	name, isDir, err := validateArchiveEntry(f)
	if err != nil {
		return err
	}
	if isDir || name != "payload" {
		return fmt.Errorf("file artifact archive entry %q must be regular file %q", f.Name, "payload")
	}
	if parent := path.Dir(targetRel); parent != "." && parent != "" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("create parent directory %q: %w", parent, err)
		}
	}
	_, err = extractZipFileToRoot(root, f, targetRel, s.maxSingleFileBytes)
	return err
}

func (s *Service) extractTreeEntries(root *os.Root, files []*zip.File, requiredPrefix, targetBase string) error {
	if len(files) > s.maxEntries {
		return fmt.Errorf("archive entry count %d exceeds maximum %d", len(files), s.maxEntries)
	}

	seen := make(map[string]struct{}, len(files))
	type validatedEntry struct {
		file    *zip.File
		destRel string
		isDir   bool
	}
	validated := make([]validatedEntry, 0, len(files))

	for _, f := range files {
		name, isDir, err := validateArchiveEntry(f)
		if err != nil {
			return err
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		seen[name] = struct{}{}

		relWithin := name
		if requiredPrefix != "" {
			if !strings.HasPrefix(name, requiredPrefix) {
				return fmt.Errorf("archive entry %q outside expected prefix %q", name, requiredPrefix)
			}
			relWithin = strings.TrimPrefix(name, requiredPrefix)
		}
		relWithin = strings.TrimSuffix(relWithin, "/")
		if relWithin == "" {
			if isDir {
				continue
			}
			return fmt.Errorf("archive entry %q has empty relative path", name)
		}

		destRel := relWithin
		if targetBase != "" {
			destRel = path.Join(targetBase, relWithin)
		}
		validated = append(validated, validatedEntry{
			file:    f,
			destRel: destRel,
			isDir:   isDir,
		})
	}

	if targetBase != "" {
		if err := root.MkdirAll(targetBase, 0o755); err != nil {
			return fmt.Errorf("create target directory %q: %w", targetBase, err)
		}
	}

	var totalBytes int64
	for _, entry := range validated {
		if entry.isDir {
			if err := root.MkdirAll(entry.destRel, 0o755); err != nil {
				return fmt.Errorf("create extracted directory %q: %w", entry.destRel, err)
			}
			continue
		}
		if parent := path.Dir(entry.destRel); parent != "." && parent != "" {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("create extracted parent %q: %w", parent, err)
			}
		}
		written, err := extractZipFileToRoot(root, entry.file, entry.destRel, s.maxSingleFileBytes)
		if err != nil {
			return err
		}
		totalBytes += written
		if totalBytes > s.maxTotalBytes {
			return fmt.Errorf("archive total uncompressed size exceeds maximum %d bytes", s.maxTotalBytes)
		}
	}
	return nil
}

func validateArchiveEntry(f *zip.File) (string, bool, error) {
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

func extractZipFileToRoot(root *os.Root, f *zip.File, destRel string, maxSingle int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("open archive entry %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	out, err := root.OpenFile(destRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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
	return written, nil
}

func (s *Service) lockForDigest(digest string) func() {
	s.cacheMu.Lock()
	if s.cacheLocks == nil {
		s.cacheLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := s.cacheLocks[digest]
	if !ok {
		mu = &sync.Mutex{}
		s.cacheLocks[digest] = mu
	}
	s.cacheMu.Unlock()

	mu.Lock()
	return mu.Unlock
}

func sha256File(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
