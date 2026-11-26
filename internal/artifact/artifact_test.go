package artifact_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMatrix(t *testing.T) {
	t.Run("valid file and directory declarations", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "plan.md"), []byte("# Plan\n"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "bundle", "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bundle", "sub", "item.txt"), []byte("ok"), 0o644))

		decls := []workerspec.Artifact{
			{Name: "plan", Type: "file", Path: "docs/plan.md", Required: true, NonEmpty: true},
			{Name: "bundle", Type: "directory", Path: "bundle", Required: true, NonEmpty: true},
			{Name: "opt_missing", Type: "file", Path: "optional.txt", Required: false, NonEmpty: true},
		}

		valid, issues, err := artifact.Validate(dir, decls)
		require.NoError(t, err)
		assert.Empty(t, issues)
		require.Len(t, valid, 2)
		assert.Equal(t, "plan", valid[0].Name)
		assert.Equal(t, "bundle", valid[1].Name)
	})

	t.Run("missing required, blocked path, type mismatch, and empty issues", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blocker"), []byte("not a dir"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "as_dir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "as_file"), []byte("regular"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "zero.txt"), nil, 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty_dir", "nested_empty"), 0o755))

		decls := []workerspec.Artifact{
			{Name: "missing_req", Type: "file", Path: "missing.md", Required: true},
			{Name: "missing_opt", Type: "file", Path: "opt.md", Required: false},
			{Name: "blocked_req", Type: "file", Path: "blocker/out.md", Required: true},
			{Name: "blocked_opt", Type: "file", Path: "blocker/opt.md", Required: false},
			{Name: "mismatch_file", Type: "file", Path: "as_dir", Required: true},
			{Name: "mismatch_dir_opt", Type: "directory", Path: "as_file", Required: false},
			{Name: "empty_file", Type: "file", Path: "zero.txt", Required: true, NonEmpty: true},
			{Name: "empty_dir", Type: "directory", Path: "empty_dir", Required: true, NonEmpty: true},
		}

		valid, issues, err := artifact.Validate(dir, decls)
		require.NoError(t, err)
		assert.Empty(t, valid)

		codes := make([]string, 0, len(issues))
		for _, iss := range issues {
			codes = append(codes, iss.Code)
			assert.NoError(t, iss.Validate())
		}
		assert.Equal(t, []string{
			"artifact_missing",
			"artifact_path_blocked",
			"artifact_type_mismatch",
			"artifact_type_mismatch",
			"artifact_empty",
			"artifact_empty",
		}, codes)
	})

	t.Run("missing artifactsDir still reports required missing and skips optional", func(t *testing.T) {
		nonExistent := filepath.Join(t.TempDir(), "not-created-yet")
		decls := []workerspec.Artifact{
			{Name: "req", Type: "file", Path: "out.txt", Required: true},
			{Name: "opt", Type: "directory", Path: "optdir", Required: false},
		}

		valid, issues, err := artifact.Validate(nonExistent, decls)
		require.NoError(t, err)
		assert.Empty(t, valid)
		require.Len(t, issues, 1)
		assert.Equal(t, "artifact_missing", issues[0].Code)
	})

	t.Run("rejects symlink at artifact path or inside declared directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "secret.txt")
		require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))

		linkFile := filepath.Join(dir, "link.txt")
		if err := os.Symlink(outside, linkFile); err != nil {
			t.Skipf("symlinks unavailable in this environment: %v", err)
		}

		_, _, err := artifact.Validate(dir, []workerspec.Artifact{
			{Name: "linked", Type: "file", Path: "link.txt", Required: true},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "symlink")

		subDir := filepath.Join(dir, "tree")
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "ok.txt"), []byte("ok"), 0o644))
		require.NoError(t, os.Symlink(outside, filepath.Join(subDir, "bad_link.txt")))

		_, _, err = artifact.Validate(dir, []workerspec.Artifact{
			{Name: "dir_with_link", Type: "directory", Path: "tree", Required: true, NonEmpty: false},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "symlink")
	})
}

func TestPublishAndStageImportsCrossMachineRoundTrip(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "shared-blobs")
	store, err := blob.NewLocalStore(storeDir)
	require.NoError(t, err)

	// Machine A produces a single-file artifact and a nested directory artifact.
	machineA := filepath.Join(t.TempDir(), "machineA")
	artDirA := filepath.Join(machineA, "artifacts")
	require.NoError(t, os.MkdirAll(filepath.Join(artDirA, "docs"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(artDirA, "dist", "sub"), 0o755))

	planBytes := []byte("# Architecture Plan\nDeterministic bytes.\n")
	fileA := []byte("alpha-content")
	fileB := []byte{0x00, 0x01, 0x02, 0xfe, 0xff}
	require.NoError(t, os.WriteFile(filepath.Join(artDirA, "docs", "plan.md"), planBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA, "dist", "a.txt"), fileA, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA, "dist", "sub", "b.bin"), fileB, 0o644))

	declsA := []workerspec.Artifact{
		{Name: "plan", Type: "file", Path: "docs/plan.md", Required: true, NonEmpty: true},
		{Name: "dist", Type: "directory", Path: "dist", Required: true, NonEmpty: true},
	}
	validA, issues, err := artifact.Validate(artDirA, declsA)
	require.NoError(t, err)
	require.Empty(t, issues)

	svcA := artifact.NewService(store, artifact.ServiceOptions{})
	published1, err := svcA.Publish(ctx, "run-42", artDirA, validA)
	require.NoError(t, err)
	require.Len(t, published1, 2)

	// Publish a second time to verify deterministic ZIP digests.
	published2, err := svcA.Publish(ctx, "run-42", artDirA, validA)
	require.NoError(t, err)
	assert.Equal(t, published1[0].SHA256, published2[0].SHA256)
	assert.Equal(t, published1[0].SizeBytes, published2[0].SizeBytes)
	assert.Equal(t, published1[1].SHA256, published2[1].SHA256)
	assert.Equal(t, published1[1].SizeBytes, published2[1].SizeBytes)

	pubPlan := published1[0]
	pubDist := published1[1]
	assert.True(t, strings.HasPrefix(pubPlan.ID, "art_"))
	assert.Equal(t, "/.sofa/artifacts/docs/plan.md", pubPlan.ContainerPath)
	assert.Equal(t, "runs/run-42/artifacts/"+pubPlan.ID+".zip", pubPlan.BlobKey)
	assert.Equal(t, "/.sofa/artifacts/dist", pubDist.ContainerPath)

	// Verify ZIP internal layout ("payload" and sorted "tree/<rel>" at Unix epoch).
	verifyZipLayout(t, store, pubPlan.BlobKey, []string{"payload"})
	verifyZipLayout(t, store, pubDist.BlobKey, []string{"tree/a.txt", "tree/sub/", "tree/sub/b.bin"})

	// Publish two same-named file and directory artifacts from separate simulated for_each workers.
	artDirA2 := filepath.Join(machineA, "artifacts-shard2")
	require.NoError(t, os.MkdirAll(filepath.Join(artDirA2, "sub_tree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA2, "report.md"), []byte("shard-2"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA2, "sub_tree", "item.txt"), []byte("dir-2"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(artDirA, "sub_tree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA, "report.md"), []byte("shard-1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artDirA, "sub_tree", "item.txt"), []byte("dir-1"), 0o644))

	pubShard1, err := svcA.Publish(ctx, "run-42", artDirA, []workerspec.Artifact{
		{Name: "report", Type: "file", Path: "report.md", Required: true, NonEmpty: true},
		{Name: "dir_shard", Type: "directory", Path: "sub_tree", Required: true, NonEmpty: true},
	})
	require.NoError(t, err)
	pubShard2, err := svcA.Publish(ctx, "run-42", artDirA2, []workerspec.Artifact{
		{Name: "report", Type: "file", Path: "report.md", Required: true, NonEmpty: true},
		{Name: "dir_shard", Type: "directory", Path: "sub_tree", Required: true, NonEmpty: true},
	})
	require.NoError(t, err)

	// Completely delete Machine A before Machine B stages imports.
	require.NoError(t, os.RemoveAll(machineA))

	cacheDirB := filepath.Join(t.TempDir(), "machineB-cache")
	svcB := artifact.NewService(store, artifact.ServiceOptions{CacheDir: cacheDirB})
	importsB := filepath.Join(t.TempDir(), "machineB-imports")

	requests := []artifact.ImportRequest{
		{
			Name:    "design_plan",
			RelPath: "inputs/plan.md",
			Sources: []artifact.ImportSource{{Artifact: pubPlan}},
		},
		{
			Name:    "bundle",
			RelPath: "design",
			Sources: []artifact.ImportSource{{Artifact: pubDist}},
		},
		{
			Name:    "reports",
			RelPath: "reports",
			Sources: []artifact.ImportSource{
				{Artifact: pubShard1[0]},
				{Artifact: pubShard2[0]},
			},
		},
		{
			Name:    "dir_shards",
			RelPath: "dir_shards",
			Sources: []artifact.ImportSource{
				{Artifact: pubShard1[1]},
				{Artifact: pubShard2[1]},
			},
		},
	}

	require.NoError(t, svcB.StageImports(ctx, importsB, requests))

	gotPlan, err := os.ReadFile(filepath.Join(importsB, "inputs", "plan.md"))
	require.NoError(t, err)
	assert.Equal(t, planBytes, gotPlan)

	gotA, err := os.ReadFile(filepath.Join(importsB, "design", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, fileA, gotA)

	gotB, err := os.ReadFile(filepath.Join(importsB, "design", "sub", "b.bin"))
	require.NoError(t, err)
	assert.Equal(t, fileB, gotB)

	gotShard1, err := os.ReadFile(filepath.Join(importsB, "reports", "000_report"))
	require.NoError(t, err)
	assert.Equal(t, []byte("shard-1"), gotShard1)

	gotShard2, err := os.ReadFile(filepath.Join(importsB, "reports", "001_report"))
	require.NoError(t, err)
	assert.Equal(t, []byte("shard-2"), gotShard2)

	gotDirShard1, err := os.ReadFile(filepath.Join(importsB, "dir_shards", "000_dir_shard", "item.txt"))
	require.NoError(t, err)
	assert.Equal(t, []byte("dir-1"), gotDirShard1)

	gotDirShard2, err := os.ReadFile(filepath.Join(importsB, "dir_shards", "001_dir_shard", "item.txt"))
	require.NoError(t, err)
	assert.Equal(t, []byte("dir-2"), gotDirShard2)

	// Verify cache files exist and serve a second StageImports even if BlobStore objects are removed.
	assert.FileExists(t, filepath.Join(cacheDirB, pubPlan.SHA256+".zip"))
	assert.FileExists(t, filepath.Join(cacheDirB, pubDist.SHA256+".zip"))

	// Delete pubDist from store, then stage it again from cacheDirB -> must succeed via cache hit!
	require.NoError(t, store.Delete(ctx, pubDist.BlobKey))
	importsB2 := filepath.Join(t.TempDir(), "machineB-imports-cached")
	require.NoError(t, svcB.StageImports(ctx, importsB2, []artifact.ImportRequest{
		{Name: "bundle", RelPath: "design", Sources: []artifact.ImportSource{{Artifact: pubDist}}},
	}))
	gotCachedB, err := os.ReadFile(filepath.Join(importsB2, "design", "sub", "b.bin"))
	require.NoError(t, err)
	assert.Equal(t, fileB, gotCachedB)

	// Corrupt cache file for pubPlan while store still holds pubPlan -> StageImports must detect
	// tampered cache SHA-256, re-fetch from store, heal cache, and unpack valid bytes.
	planCachePath := filepath.Join(cacheDirB, pubPlan.SHA256+".zip")
	require.NoError(t, os.WriteFile(planCachePath, []byte("corrupted-zip"), 0o600))

	importsB3 := filepath.Join(t.TempDir(), "machineB-imports-healed")
	require.NoError(t, svcB.StageImports(ctx, importsB3, []artifact.ImportRequest{
		{Name: "design_plan", RelPath: "plan.md", Sources: []artifact.ImportSource{{Artifact: pubPlan}}},
	}))
	gotHealedPlan, err := os.ReadFile(filepath.Join(importsB3, "plan.md"))
	require.NoError(t, err)
	assert.Equal(t, planBytes, gotHealedPlan)

	healedCacheBytes, err := os.ReadFile(planCachePath)
	require.NoError(t, err)
	sum := sha256.Sum256(healedCacheBytes)
	assert.Equal(t, pubPlan.SHA256, hex.EncodeToString(sum[:]))
}

func TestStageImportsSecurityGuards(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	putZip := func(key string, entries []struct {
		name string
		mode fs.FileMode
		data []byte
	}) artifact.PublishedArtifact {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, e := range entries {
			hdr := &zip.FileHeader{
				Name:     e.name,
				Method:   zip.Deflate,
				Modified: time.Unix(0, 0).UTC(),
			}
			hdr.SetMode(e.mode)
			w, err := zw.CreateHeader(hdr)
			require.NoError(t, err)
			_, err = w.Write(e.data)
			require.NoError(t, err)
		}
		require.NoError(t, zw.Close())
		meta, err := store.Put(ctx, key, bytes.NewReader(buf.Bytes()), "")
		require.NoError(t, err)
		return artifact.PublishedArtifact{
			ID:        "art_test",
			Name:      "evil",
			Type:      "directory",
			RelPath:   "out",
			BlobKey:   key,
			SHA256:    meta.SHA256,
			SizeBytes: meta.SizeBytes,
		}
	}

	t.Run("rejects zip-slip entry", func(t *testing.T) {
		pub := putZip("runs/r/evil-slip.zip", []struct {
			name string
			mode fs.FileMode
			data []byte
		}{
			{name: "tree/../../escape.txt", mode: 0o644, data: []byte("boom")},
		})
		svc := artifact.NewService(store, artifact.ServiceOptions{})
		err := svc.StageImports(ctx, t.TempDir(), []artifact.ImportRequest{
			{Name: "imp", RelPath: "safe", Sources: []artifact.ImportSource{{Artifact: pub}}},
		})
		require.Error(t, err)
	})

	t.Run("rejects symlink zip entry", func(t *testing.T) {
		pub := putZip("runs/r/evil-symlink.zip", []struct {
			name string
			mode fs.FileMode
			data []byte
		}{
			{name: "tree/link", mode: os.ModeSymlink | 0o777, data: []byte("/etc/passwd")},
		})
		svc := artifact.NewService(store, artifact.ServiceOptions{})
		err := svc.StageImports(ctx, t.TempDir(), []artifact.ImportRequest{
			{Name: "imp", RelPath: "safe", Sources: []artifact.ImportSource{{Artifact: pub}}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "symlink")
	})

	t.Run("enforces single file and total byte limits", func(t *testing.T) {
		pub := putZip("runs/r/big.zip", []struct {
			name string
			mode fs.FileMode
			data []byte
		}{
			{name: "tree/a.txt", mode: 0o644, data: bytes.Repeat([]byte("a"), 20)},
			{name: "tree/b.txt", mode: 0o644, data: bytes.Repeat([]byte("b"), 20)},
		})
		svcSmallFile := artifact.NewService(store, artifact.ServiceOptions{MaxSingleFileBytes: 10})
		err := svcSmallFile.StageImports(ctx, t.TempDir(), []artifact.ImportRequest{
			{Name: "imp", RelPath: "safe", Sources: []artifact.ImportSource{{Artifact: pub}}},
		})
		require.Error(t, err)

		svcSmallTotal := artifact.NewService(store, artifact.ServiceOptions{MaxSingleFileBytes: 25, MaxTotalBytes: 30})
		err = svcSmallTotal.StageImports(ctx, t.TempDir(), []artifact.ImportRequest{
			{Name: "imp", RelPath: "safe", Sources: []artifact.ImportSource{{Artifact: pub}}},
		})
		require.Error(t, err)

		svcSmallEntries := artifact.NewService(store, artifact.ServiceOptions{MaxEntries: 1})
		err = svcSmallEntries.StageImports(ctx, t.TempDir(), []artifact.ImportRequest{
			{Name: "imp", RelPath: "safe", Sources: []artifact.ImportSource{{Artifact: pub}}},
		})
		require.Error(t, err)
	})
}

func TestSnapshotAndRestoreStagingDir(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	svc := artifact.NewService(store, artifact.ServiceOptions{})

	// Empty or missing staging directory returns (nil, nil) without writing to store.
	emptyDir := filepath.Join(t.TempDir(), "empty-staging")
	require.NoError(t, os.MkdirAll(emptyDir, 0o755))
	metaNil, err := svc.SnapshotStagingDir(ctx, "runs/r1/ckpt/empty.zip", emptyDir)
	require.NoError(t, err)
	assert.Nil(t, metaNil)

	metaMissing, err := svc.SnapshotStagingDir(ctx, "runs/r1/ckpt/missing.zip", filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	assert.Nil(t, metaMissing)

	// Machine A writes partial staging artifacts -> SnapshotStagingDir -> delete Machine A -> RestoreStagingDir on Machine C.
	machineA := filepath.Join(t.TempDir(), "machineA-staging")
	require.NoError(t, os.MkdirAll(filepath.Join(machineA, "drafts", "nested"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(machineA, "empty_sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(machineA, "drafts", "wip.md"), []byte("work in progress"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(machineA, "drafts", "nested", "data.json"), []byte(`{"step":2}`), 0o644))

	meta, err := svc.SnapshotStagingDir(ctx, "runs/r1/ckpt/node-1-e1.zip", machineA)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, "runs/r1/ckpt/node-1-e1.zip", meta.Key)

	require.NoError(t, os.RemoveAll(machineA))

	machineC := filepath.Join(t.TempDir(), "machineC-staging")
	require.NoError(t, svc.RestoreStagingDir(ctx, *meta, machineC))

	gotWIP, err := os.ReadFile(filepath.Join(machineC, "drafts", "wip.md"))
	require.NoError(t, err)
	assert.Equal(t, []byte("work in progress"), gotWIP)

	gotJSON, err := os.ReadFile(filepath.Join(machineC, "drafts", "nested", "data.json"))
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"step":2}`), gotJSON)

	info, err := os.Stat(filepath.Join(machineC, "empty_sub"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestStageImportsRejectsOverlappingRelPaths(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a"), 0o644))
	svc := artifact.NewService(store, artifact.ServiceOptions{})
	pubs, err := svc.Publish(ctx, "run-overlap", srcDir, []workerspec.Artifact{
		{Name: "a", Type: "file", Path: "a.txt", Required: true, NonEmpty: true},
	})
	require.NoError(t, err)

	cases := []struct {
		name string
		p1   string
		p2   string
	}{
		{name: "exact duplicate", p1: "shared/spec.md", p2: "shared/spec.md"},
		{name: "parent then child", p1: "shared", p2: "shared/sub"},
		{name: "child then parent", p1: "shared/sub/file.txt", p2: "shared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			importsDir := filepath.Join(t.TempDir(), "imports")
			err := svc.StageImports(ctx, importsDir, []artifact.ImportRequest{
				{Name: "first", RelPath: tc.p1, Sources: []artifact.ImportSource{{Artifact: pubs[0]}}},
				{Name: "second", RelPath: tc.p2, Sources: []artifact.ImportSource{{Artifact: pubs[0]}}},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "overlap")
			// Fails upfront before creating importsDir or writing any files.
			assert.NoDirExists(t, importsDir)
		})
	}

	// Sibling prefixes must NOT conflict.
	importsOK := filepath.Join(t.TempDir(), "imports-ok")
	require.NoError(t, svc.StageImports(ctx, importsOK, []artifact.ImportRequest{
		{Name: "one", RelPath: "shared", Sources: []artifact.ImportSource{{Artifact: pubs[0]}}},
		{Name: "two", RelPath: "shared2/sub", Sources: []artifact.ImportSource{{Artifact: pubs[0]}}},
	}))
	assert.FileExists(t, filepath.Join(importsOK, "shared"))
	assert.FileExists(t, filepath.Join(importsOK, "shared2", "sub"))
}

func TestStageImportsConcurrentSameUncachedArtifact(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocalStore(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)

	srcDir := t.TempDir()
	payload := bytes.Repeat([]byte("concurrent-cache-payload-"), 512)
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "big.bin"), payload, 0o644))

	pubSvc := artifact.NewService(store, artifact.ServiceOptions{})
	pubs, err := pubSvc.Publish(ctx, "run-conc", srcDir, []workerspec.Artifact{
		{Name: "big", Type: "file", Path: "big.bin", Required: true, NonEmpty: true},
	})
	require.NoError(t, err)

	cacheDir := filepath.Join(t.TempDir(), "shared-cache")
	svc := artifact.NewService(store, artifact.ServiceOptions{CacheDir: cacheDir})

	const goroutines = 12
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dest := filepath.Join(t.TempDir(), "imports")
			if err := svc.StageImports(ctx, dest, []artifact.ImportRequest{
				{Name: "big", RelPath: "big.bin", Sources: []artifact.ImportSource{{Artifact: pubs[0]}}},
			}); err != nil {
				errs <- err
				return
			}
			got, readErr := os.ReadFile(filepath.Join(dest, "big.bin"))
			if readErr != nil {
				errs <- readErr
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- os.ErrInvalid
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func verifyZipLayout(t *testing.T, store blob.Store, key string, expectedNames []string) {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key, "")
	require.NoError(t, err)
	data, err := os.ReadFile(func() string {
		p := filepath.Join(t.TempDir(), "check.zip")
		b, readErr := readAllAndClose(rc)
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}())
	require.NoError(t, err)

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
		assert.Equal(t, time.Unix(0, 0).UTC(), f.Modified.UTC())
	}
	assert.Equal(t, expectedNames, names)
}

func readAllAndClose(rc interface {
	Read([]byte) (int, error)
	Close() error
}) ([]byte, error) {
	var buf bytes.Buffer
	_, readErr := buf.ReadFrom(rc)
	closeErr := rc.Close()
	if readErr != nil {
		return nil, readErr
	}
	return buf.Bytes(), closeErr
}
