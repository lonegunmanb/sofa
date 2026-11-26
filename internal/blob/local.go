package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const metaSuffix = ".json"

// LocalStore is an atomic, integrity-verified local filesystem implementation of Store.
type LocalStore struct {
	mu         sync.RWMutex
	rootDir    string
	objectsDir string
	metaDir    string
	tmpDir     string
}

// NewLocalStore initializes a LocalStore rooted at rootDir with 0700 directory permissions.
func NewLocalStore(rootDir string) (*LocalStore, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("blob root directory is required")
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve blob root directory: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	objectsDir := filepath.Join(absRoot, "objects")
	metaDir := filepath.Join(absRoot, "meta")
	tmpDir := filepath.Join(absRoot, ".tmp")

	for _, dir := range []string{absRoot, objectsDir, metaDir, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create blob directory %q: %w", dir, err)
		}
	}
	return &LocalStore{
		rootDir:    absRoot,
		objectsDir: objectsDir,
		metaDir:    metaDir,
		tmpDir:     tmpDir,
	}, nil
}

// Put streams r to a staging file, verifies optional expectedSHA256, restricts file permissions,
// and atomically commits both the object payload and its .meta.json sidecar.
func (s *LocalStore) Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return ObjectMeta{}, err
	}
	if err := ValidateKey(key); err != nil {
		return ObjectMeta{}, err
	}
	if r == nil {
		return ObjectMeta{}, fmt.Errorf("blob reader is nil")
	}
	if expectedSHA256 != "" && !IsValidSHA256Hex(expectedSHA256) {
		return ObjectMeta{}, fmt.Errorf("%w: invalid expected sha256 %q", ErrDigestMismatch, expectedSHA256)
	}

	tmpDataFile, err := os.CreateTemp(s.tmpDir, "upload-*.tmp")
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("create staging blob file: %w", err)
	}
	tmpDataPath := tmpDataFile.Name()
	cleanupData := true
	defer func() {
		if cleanupData {
			_ = os.Remove(tmpDataPath)
		}
	}()

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmpDataFile, hasher), &contextReader{ctx: ctx, r: r})
	closeErr := tmpDataFile.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return ObjectMeta{}, err
	}

	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && actualSHA256 != expectedSHA256 {
		return ObjectMeta{}, fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, expectedSHA256, actualSHA256)
	}
	if err := restrictBlobFile(tmpDataPath, nil); err != nil {
		return ObjectMeta{}, fmt.Errorf("restrict staging blob permissions: %w", err)
	}

	meta := ObjectMeta{
		Key:       key,
		SHA256:    actualSHA256,
		SizeBytes: written,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("encode blob metadata: %w", err)
	}
	metaBytes = append(metaBytes, '\n')

	tmpMetaFile, err := os.CreateTemp(s.tmpDir, "meta-*.tmp")
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("create staging metadata file: %w", err)
	}
	tmpMetaPath := tmpMetaFile.Name()
	cleanupMeta := true
	defer func() {
		if cleanupMeta {
			_ = os.Remove(tmpMetaPath)
		}
	}()

	_, metaWriteErr := tmpMetaFile.Write(metaBytes)
	metaCloseErr := tmpMetaFile.Close()
	if err := errors.Join(metaWriteErr, metaCloseErr); err != nil {
		return ObjectMeta{}, fmt.Errorf("write staging metadata: %w", err)
	}
	if err := restrictBlobFile(tmpMetaPath, nil); err != nil {
		return ObjectMeta{}, fmt.Errorf("restrict staging metadata permissions: %w", err)
	}

	objPath, metaPath := s.objectPaths(key)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(objPath), 0o700); err != nil {
		return ObjectMeta{}, fmt.Errorf("create object parent directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		return ObjectMeta{}, fmt.Errorf("create metadata parent directory: %w", err)
	}
	if err := os.Rename(tmpDataPath, objPath); err != nil {
		return ObjectMeta{}, fmt.Errorf("commit blob object %q: %w", key, err)
	}
	cleanupData = false
	if err := restrictBlobFile(objPath, nil); err != nil {
		return ObjectMeta{}, fmt.Errorf("restrict blob object permissions: %w", err)
	}

	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		return ObjectMeta{}, fmt.Errorf("commit blob metadata %q: %w", key, err)
	}
	cleanupMeta = false
	if err := restrictBlobFile(metaPath, nil); err != nil {
		return ObjectMeta{}, fmt.Errorf("restrict blob metadata permissions: %w", err)
	}

	return meta, nil
}

// Get opens a stream to key and wraps it with EOF digest verification against stored metadata
// and optional expectedSHA256.
func (s *LocalStore) Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return nil, ObjectMeta{}, err
	}
	if err := ValidateKey(key); err != nil {
		return nil, ObjectMeta{}, err
	}
	if expectedSHA256 != "" && !IsValidSHA256Hex(expectedSHA256) {
		return nil, ObjectMeta{}, fmt.Errorf("%w: invalid expected sha256 %q", ErrDigestMismatch, expectedSHA256)
	}

	objPath, metaPath := s.objectPaths(key)

	s.mu.RLock()
	meta, err := readMetaFile(key, metaPath)
	if err != nil {
		s.mu.RUnlock()
		return nil, ObjectMeta{}, err
	}
	f, err := os.Open(objPath)
	s.mu.RUnlock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ObjectMeta{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, ObjectMeta{}, fmt.Errorf("open blob %q: %w", key, err)
	}

	if expectedSHA256 != "" && meta.SHA256 != expectedSHA256 {
		_ = f.Close()
		return nil, ObjectMeta{}, fmt.Errorf("%w: metadata digest %s does not match expected %s", ErrDigestMismatch, meta.SHA256, expectedSHA256)
	}

	return &verifyingReadCloser{
		file:           f,
		hasher:         sha256.New(),
		expectedMeta:   meta,
		expectedSHA256: expectedSHA256,
	}, meta, nil
}

// Stat returns the stored ObjectMeta for key without reading object bytes.
func (s *LocalStore) Stat(ctx context.Context, key string) (ObjectMeta, error) {
	if err := checkContext(ctx); err != nil {
		return ObjectMeta{}, err
	}
	if err := ValidateKey(key); err != nil {
		return ObjectMeta{}, err
	}

	objPath, metaPath := s.objectPaths(key)

	s.mu.RLock()
	defer s.mu.RUnlock()

	meta, err := readMetaFile(key, metaPath)
	if err != nil {
		return ObjectMeta{}, err
	}
	info, err := os.Stat(objPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectMeta{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return ObjectMeta{}, fmt.Errorf("stat blob %q: %w", key, err)
	}
	if info.IsDir() {
		return ObjectMeta{}, fmt.Errorf("%w: %s is a directory", ErrNotFound, key)
	}
	return meta, nil
}

// Delete removes key and its metadata idempotently.
func (s *LocalStore) Delete(ctx context.Context, key string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return err
	}

	objPath, metaPath := s.objectPaths(key)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := removeIfExists(metaPath); err != nil {
		return fmt.Errorf("remove blob metadata %q: %w", key, err)
	}
	if err := removeIfExists(objPath); err != nil {
		return fmt.Errorf("remove blob object %q: %w", key, err)
	}
	pruneEmptyParents(s.metaDir, filepath.Dir(metaPath))
	pruneEmptyParents(s.objectsDir, filepath.Dir(objPath))
	return nil
}

// DeletePrefix deletes all objects whose keys begin with prefix.
func (s *LocalStore) DeletePrefix(ctx context.Context, prefix string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := ValidatePrefix(prefix); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := deletePrefixInTree(s.objectsDir, "", prefix); err != nil {
		return fmt.Errorf("delete object prefix %q: %w", prefix, err)
	}
	if err := deletePrefixInTree(s.metaDir, metaSuffix, prefix); err != nil {
		return fmt.Errorf("delete metadata prefix %q: %w", prefix, err)
	}
	return nil
}

func deletePrefixInTree(baseDir, suffix, prefix string) error {
	var toRemove []string
	var dirs []string
	err := filepath.WalkDir(baseDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if p == baseDir {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		rel, err := filepath.Rel(baseDir, p)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if suffix != "" {
			if !strings.HasSuffix(slashRel, suffix) {
				return nil
			}
			slashRel = strings.TrimSuffix(slashRel, suffix)
		}
		if prefix == "" || strings.HasPrefix(slashRel, prefix) {
			toRemove = append(toRemove, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range toRemove {
		if err := removeIfExists(p); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only removes if empty
	}
	return nil
}

func (s *LocalStore) objectPaths(key string) (string, string) {
	rel := filepath.FromSlash(key)
	return filepath.Join(s.objectsDir, rel), filepath.Join(s.metaDir, rel+metaSuffix)
}

func pruneEmptyParents(baseDir, dir string) {
	for dir != baseDir && strings.HasPrefix(dir, baseDir+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func readMetaFile(key, metaPath string) (ObjectMeta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectMeta{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return ObjectMeta{}, fmt.Errorf("read blob metadata %q: %w", key, err)
	}
	var meta ObjectMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return ObjectMeta{}, fmt.Errorf("decode blob metadata %q: %w", key, err)
	}
	meta.Key = key
	return meta, nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := checkContext(c.ctx); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type verifyingReadCloser struct {
	mu             sync.Mutex
	file           *os.File
	hasher         hash.Hash
	bytesRead      int64
	expectedMeta   ObjectMeta
	expectedSHA256 string
	verified       bool
	verifyErr      error
	closed         bool
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return 0, os.ErrClosed
	}
	if v.verified {
		if v.verifyErr != nil {
			return 0, v.verifyErr
		}
		return 0, io.EOF
	}

	n, err := v.file.Read(p)
	if n > 0 {
		v.hasher.Write(p[:n])
		v.bytesRead += int64(n)
	}
	if errors.Is(err, io.EOF) {
		v.verified = true
		actual := hex.EncodeToString(v.hasher.Sum(nil))
		if v.bytesRead != v.expectedMeta.SizeBytes || actual != v.expectedMeta.SHA256 {
			v.verifyErr = fmt.Errorf("%w: stored metadata mismatch (expected sha256=%s size=%d, got sha256=%s size=%d)",
				ErrDigestMismatch, v.expectedMeta.SHA256, v.expectedMeta.SizeBytes, actual, v.bytesRead)
			return 0, v.verifyErr
		}
		if v.expectedSHA256 != "" && actual != v.expectedSHA256 {
			v.verifyErr = fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, v.expectedSHA256, actual)
			return 0, v.verifyErr
		}
		return n, io.EOF
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return nil
	}
	v.closed = true
	return v.file.Close()
}
