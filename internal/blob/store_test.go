package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"file.zip",
		"runs/run-1/artifacts/art_123.zip",
		"A_B-C.01/sub_dir/item-2.json",
	}
	for _, k := range valid {
		assert.NoError(t, blob.ValidateKey(k), "key %q should be valid", k)
	}

	invalid := []string{
		"",
		"/",
		"/runs/a",
		"runs/a/",
		"runs//a",
		`runs\a`,
		".",
		"..",
		"runs/./a",
		"runs/../a",
		"runs/a/..",
		"runs/a b",
		"runs/a\x00b",
		"runs/a\nb",
		"C:/runs/a",
	}
	for _, k := range invalid {
		err := blob.ValidateKey(k)
		require.Error(t, err, "key %q should be invalid", k)
		assert.ErrorIs(t, err, blob.ErrInvalidKey, "key %q should return ErrInvalidKey", k)
	}
}

func TestLocalStorePutGetIntegrityAndCleanup(t *testing.T) {
	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "blobs")
	store, err := blob.NewLocalStore(rootDir)
	require.NoError(t, err)

	payload := []byte("hello sofa blobstore payload")
	digest := sha256Of(payload)

	// Invalid keys rejected.
	_, err = store.Put(ctx, "../evil", bytes.NewReader(payload), "")
	assert.ErrorIs(t, err, blob.ErrInvalidKey)
	_, _, err = store.Get(ctx, `a\b`, "")
	assert.ErrorIs(t, err, blob.ErrInvalidKey)
	_, err = store.Stat(ctx, "")
	assert.ErrorIs(t, err, blob.ErrInvalidKey)
	err = store.Delete(ctx, "/leading")
	assert.ErrorIs(t, err, blob.ErrInvalidKey)

	// Mismatched or malformed expectedSHA256 rejected with zero leftover temp files.
	wrongDigest := strings.Repeat("0", 64)
	_, err = store.Put(ctx, "runs/run-1/bad.bin", bytes.NewReader(payload), wrongDigest)
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)
	_, err = store.Put(ctx, "runs/run-1/bad.bin", bytes.NewReader(payload), strings.ToUpper(digest))
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)
	_, err = store.Stat(ctx, "runs/run-1/bad.bin")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	tmpEntries, err := os.ReadDir(filepath.Join(rootDir, ".tmp"))
	if err == nil {
		assert.Empty(t, tmpEntries, "failed Put must not leave temp files")
	}

	// Valid Put creates data file + meta.json with expected metadata.
	key := "runs/run-1/artifacts/art_001.zip"
	meta, err := store.Put(ctx, key, bytes.NewReader(payload), digest)
	require.NoError(t, err)
	assert.Equal(t, key, meta.Key)
	assert.Equal(t, digest, meta.SHA256)
	assert.Equal(t, int64(len(payload)), meta.SizeBytes)

	objPath := filepath.Join(rootDir, "objects", filepath.FromSlash(key))
	metaPath := filepath.Join(rootDir, "meta", filepath.FromSlash(key)+".json")
	assert.FileExists(t, objPath)
	assert.FileExists(t, metaPath)

	rawMeta, err := os.ReadFile(metaPath)
	require.NoError(t, err)
	var diskMeta map[string]any
	require.NoError(t, json.Unmarshal(rawMeta, &diskMeta))
	assert.Equal(t, digest, diskMeta["sha256"])
	assert.Equal(t, float64(len(payload)), diskMeta["size_bytes"])

	// Stat returns matching metadata.
	st, err := store.Stat(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, meta, st)

	// Get with valid expectedSHA256 succeeds and reads exact bytes.
	rc, gotMeta, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	assert.Equal(t, meta, gotMeta)
	gotBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, payload, gotBytes)

	// Early Close before EOF releases handle cleanly without ErrDigestMismatch.
	rcEarly, _, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	oneByte := make([]byte, 1)
	_, err = rcEarly.Read(oneByte)
	require.NoError(t, err)
	require.NoError(t, rcEarly.Close())

	// Tampering stored bytes causes Get to return ErrDigestMismatch on reading to EOF.
	tampered := append([]byte(nil), payload...)
	tampered[0] ^= 0xff
	require.NoError(t, os.WriteFile(objPath, tampered, 0o600))

	rcTampered, _, err := store.Get(ctx, key, digest)
	require.NoError(t, err)
	_, err = io.ReadAll(rcTampered)
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)
	require.NoError(t, rcTampered.Close())

	// Even with empty expectedSHA256, meta.json digest mismatch is caught on read.
	rcTamperedNoExpected, _, err := store.Get(ctx, key, "")
	require.NoError(t, err)
	_, err = io.ReadAll(rcTamperedNoExpected)
	assert.ErrorIs(t, err, blob.ErrDigestMismatch)
	require.NoError(t, rcTamperedNoExpected.Close())

	// Idempotent Delete.
	require.NoError(t, store.Delete(ctx, key))
	require.NoError(t, store.Delete(ctx, key))
	_, err = store.Stat(ctx, key)
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, _, err = store.Get(ctx, key, "")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestLocalStoreDeletePrefix(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	put := func(key, content string) {
		t.Helper()
		_, err := store.Put(ctx, key, strings.NewReader(content), "")
		require.NoError(t, err)
	}

	put("runs/run-1/a.txt", "1a")
	put("runs/run-1/sub/b.txt", "1b")
	put("runs/run-10/c.txt", "10c")
	put("runs/run-2/d.txt", "2d")

	err = store.DeletePrefix(ctx, "../evil")
	assert.ErrorIs(t, err, blob.ErrInvalidKey)

	require.NoError(t, store.DeletePrefix(ctx, "runs/run-1/"))

	_, err = store.Stat(ctx, "runs/run-1/a.txt")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = store.Stat(ctx, "runs/run-1/sub/b.txt")
	assert.ErrorIs(t, err, blob.ErrNotFound)

	// Sibling prefixes remain intact.
	st10, err := store.Stat(ctx, "runs/run-10/c.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(3), st10.SizeBytes)

	st2, err := store.Stat(ctx, "runs/run-2/d.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(2), st2.SizeBytes)

	// Empty prefix deletes all remaining objects.
	require.NoError(t, store.DeletePrefix(ctx, ""))
	_, err = store.Stat(ctx, "runs/run-2/d.txt")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = store.Stat(ctx, "runs/run-10/c.txt")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestLocalStoreMetaSidecarCollision(t *testing.T) {
	ctx := context.Background()
	rootDir := filepath.Join(t.TempDir(), "blobs")
	store, err := blob.NewLocalStore(rootDir)
	require.NoError(t, err)

	itemPayload := []byte("primary-item-bytes")
	sidecarNamedPayload := []byte("distinct-object-whose-key-ends-with-meta-json")

	metaItem, err := store.Put(ctx, "dir/item", bytes.NewReader(itemPayload), sha256Of(itemPayload))
	require.NoError(t, err)
	metaSidecarKey, err := store.Put(ctx, "dir/item.meta.json", bytes.NewReader(sidecarNamedPayload), sha256Of(sidecarNamedPayload))
	require.NoError(t, err)

	// Re-stat and re-read both keys; neither may corrupt or shadow the other.
	gotStatItem, err := store.Stat(ctx, "dir/item")
	require.NoError(t, err)
	assert.Equal(t, metaItem, gotStatItem)

	gotStatSidecar, err := store.Stat(ctx, "dir/item.meta.json")
	require.NoError(t, err)
	assert.Equal(t, metaSidecarKey, gotStatSidecar)

	rc1, _, err := store.Get(ctx, "dir/item", sha256Of(itemPayload))
	require.NoError(t, err)
	b1, err := io.ReadAll(rc1)
	require.NoError(t, err)
	require.NoError(t, rc1.Close())
	assert.Equal(t, itemPayload, b1)

	rc2, _, err := store.Get(ctx, "dir/item.meta.json", sha256Of(sidecarNamedPayload))
	require.NoError(t, err)
	b2, err := io.ReadAll(rc2)
	require.NoError(t, err)
	require.NoError(t, rc2.Close())
	assert.Equal(t, sidecarNamedPayload, b2)

	// DeletePrefix must remove both objects and their metadata cleanly.
	require.NoError(t, store.DeletePrefix(ctx, "dir/"))
	_, err = store.Stat(ctx, "dir/item")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	_, err = store.Stat(ctx, "dir/item.meta.json")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestLocalStoreConcurrency(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	const workers = 8
	const iters = 15
	var wg sync.WaitGroup
	errs := make(chan error, workers*iters)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("runs/concurrent-%d/item-%d.bin", id, i)
				data := []byte(fmt.Sprintf("payload-%d-%d", id, i))
				digest := sha256Of(data)

				if _, err := store.Put(ctx, key, bytes.NewReader(data), digest); err != nil {
					errs <- err
					return
				}
				rc, _, err := store.Get(ctx, key, digest)
				if err != nil {
					errs <- err
					return
				}
				got, readErr := io.ReadAll(rc)
				closeErr := rc.Close()
				if readErr != nil || closeErr != nil {
					errs <- errors.Join(readErr, closeErr)
					return
				}
				if !bytes.Equal(got, data) {
					errs <- fmt.Errorf("data mismatch for %s", key)
					return
				}
			}
			if err := store.DeletePrefix(ctx, fmt.Sprintf("runs/concurrent-%d/", id)); err != nil {
				errs <- err
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
