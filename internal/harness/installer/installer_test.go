package installer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/harness/installer"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeReleaseFetcher struct {
	mu     sync.Mutex
	assets map[string][]byte
	err    error
	calls  int
	tokens []string
}

func (f *fakeReleaseFetcher) FetchReleaseAsset(_ context.Context, host, owner, repo, tag, filename, token string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return nil, f.err
	}
	key := fmt.Sprintf("%s/%s/%s@%s/%s", host, owner, repo, tag, filename)
	data, ok := f.assets[key]
	if !ok {
		return nil, fmt.Errorf("asset %q not found", key)
	}
	return data, nil
}

func TestPlatformKeyAndValidate(t *testing.T) {
	t.Parallel()

	p := installer.Platform{OS: "linux", Arch: "amd64"}
	assert.Equal(t, "linux_amd64", p.Key())
	require.NoError(t, p.Validate())
	require.NoError(t, (installer.Platform{OS: "linux", Arch: "arm64"}).Validate())

	for _, bad := range []installer.Platform{
		{OS: "windows", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
		{OS: "linux", Arch: ""},
		{OS: "linux", Arch: "386"},
		{OS: "", Arch: "amd64"},
	} {
		assert.Error(t, bad.Validate(), "expected error for %+v", bad)
	}
}

func TestLocalPluginDiscoveryPriorityAndLayout(t *testing.T) {
	t.Parallel()

	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	cfg := harnessspec.HarnessConfig{Name: "copilot"}

	cliDir := filepath.Join(t.TempDir(), "cli")
	envDir := filepath.Join(t.TempDir(), "env")
	workDir := filepath.Join(t.TempDir(), "work")
	homeDir := filepath.Join(t.TempDir(), "home")

	writeLocalPlugin := func(base string, subPlatform bool, content string) string {
		var exePath string
		if subPlatform {
			exePath = filepath.Join(base, "sofa-harness-copilot", platform.Key(), "sofa-harness-copilot")
		} else {
			exePath = filepath.Join(base, "sofa-harness-copilot", "sofa-harness-copilot")
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(exePath), 0o755))
		require.NoError(t, os.WriteFile(exePath, []byte(content), 0o644))
		return exePath
	}

	// Populate all four tiers to prove strict priority order:
	// 1. PluginDir > 2. SOFA_PLUGIN_DIR > 3. WorkingDir/.sofa/plugins > 4. UserHomeDir/.sofa/plugins
	cliExe := writeLocalPlugin(cliDir, true, "cli")
	envExe := writeLocalPlugin(envDir, false, "env")
	workExe := writeLocalPlugin(filepath.Join(workDir, ".sofa", "plugins"), false, "work")
	homeExe := writeLocalPlugin(filepath.Join(homeDir, ".sofa", "plugins"), false, "home")

	inst := installer.NewInstaller()
	lookupEnv := func(k string) (string, bool) {
		if k == "SOFA_PLUGIN_DIR" {
			return envDir, true
		}
		return "", false
	}

	// Tier 1: --plugin-dir wins.
	prep, err := inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		PluginDir:   cliDir,
		WorkingDir:  workDir,
		UserHomeDir: homeDir,
		LookupEnv:   lookupEnv,
	})
	require.NoError(t, err)
	assert.Equal(t, "copilot", prep.Name)
	assert.Equal(t, filepath.Dir(cliExe), prep.HostDir)
	assert.Equal(t, "/.sofa/harness/sofa-harness-copilot", prep.ContainerPath)

	// Tier 2: SOFA_PLUGIN_DIR wins when PluginDir is empty.
	prep, err = inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		WorkingDir:  workDir,
		UserHomeDir: homeDir,
		LookupEnv:   lookupEnv,
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(envExe), prep.HostDir)

	// Tier 3: WorkingDir/.sofa/plugins wins when SOFA_PLUGIN_DIR is empty.
	prep, err = inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		WorkingDir:  workDir,
		UserHomeDir: homeDir,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(workExe), prep.HostDir)

	// Tier 4: UserHomeDir/.sofa/plugins used when higher tiers are absent.
	prep, err = inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		UserHomeDir: homeDir,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(homeExe), prep.HostDir)

	// Within same base dir, <platform.Key()>/sofa-harness-<name> beats flat sofa-harness-<name>.
	flatAndArchDir := filepath.Join(t.TempDir(), "both")
	writeLocalPlugin(flatAndArchDir, false, "flat")
	archExe := writeLocalPlugin(flatAndArchDir, true, "arch")
	prep, err = inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		PluginDir: flatAndArchDir,
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(archExe), prep.HostDir)

	// Missing everywhere lists searched candidate paths.
	_, err = inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		PluginDir:   filepath.Join(t.TempDir(), "missing1"),
		WorkingDir:  filepath.Join(t.TempDir(), "missing2"),
		UserHomeDir: filepath.Join(t.TempDir(), "missing3"),
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing1")
	assert.Contains(t, err.Error(), "missing2")
	assert.Contains(t, err.Error(), "missing3")
}

func TestLocalPluginRejectsSymlinkAndDirectoryEntrypoint(t *testing.T) {
	t.Parallel()

	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	cfg := harnessspec.HarnessConfig{Name: "copilot"}
	inst := installer.NewInstaller()

	t.Run("directory entrypoint rejected", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dirEntry := filepath.Join(base, "sofa-harness-copilot", "sofa-harness-copilot")
		require.NoError(t, os.MkdirAll(dirEntry, 0o755))

		_, err := inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
			PluginDir: base,
			LookupEnv: func(string) (string, bool) { return "", false },
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})

	t.Run("symlink entrypoint rejected", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		target := filepath.Join(t.TempDir(), "real-binary")
		require.NoError(t, os.WriteFile(target, []byte("bin"), 0o755))

		linkPath := filepath.Join(base, "sofa-harness-copilot", "sofa-harness-copilot")
		require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0o755))
		if err := os.Symlink(target, linkPath); err != nil {
			t.Skipf("symlink creation not permitted on this host: %v", err)
		}

		_, err := inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
			PluginDir: base,
			LookupEnv: func(string) (string, bool) { return "", false },
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})
}

func TestRemotePluginCacheVerificationHitMissAndCorruption(t *testing.T) {
	t.Parallel()

	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	cfg := harnessspec.HarnessConfig{
		Name:    "antigravity",
		Source:  "github.com/org/sofa-harness-antigravity",
		Version: "0.2.0",
	}

	zipBytes := buildTestZip(t, []zipEntrySpec{
		{name: "sofa-harness-antigravity", data: []byte("#!/bin/sh\necho ok\n"), mode: 0o644},
		{name: "lib/helper.py", data: []byte("print('hi')\n"), mode: 0o644},
		{name: "bin/python", data: []byte("ELF"), mode: 0o755},
	})
	sum := sha256.Sum256(zipBytes)
	digest := hex.EncodeToString(sum[:])

	lockFile := harness.PluginLockFile{
		FormatVersion: harness.LockFormatVersion,
		Plugins: []harness.PluginLockEntry{
			{
				Name:              "antigravity",
				Source:            cfg.Source,
				Version:           cfg.Version,
				VerifiedSignature: "attestation",
				Assets: map[string]harness.PluginAsset{
					"linux_amd64": {
						Filename: "sofa-harness-antigravity_linux_amd64.zip",
						SHA256:   digest,
					},
				},
			},
		},
	}

	cacheRoot := t.TempDir()
	fetcher := &fakeReleaseFetcher{
		assets: map[string][]byte{
			"github.com/org/sofa-harness-antigravity@v0.2.0/sofa-harness-antigravity_linux_amd64.zip": zipBytes,
		},
	}
	inst := installer.NewInstaller()
	opts := installer.ResolveOptions{
		CacheRoot:      cacheRoot,
		LockFile:       lockFile,
		ReleaseFetcher: fetcher,
		LookupEnv: func(k string) (string, bool) {
			if k == "GITHUB_TOKEN" {
				return "tok-123", true
			}
			return "", false
		},
	}

	// 1. Cache miss downloads, verifies SHA-256, extracts, writes .verified.sha256.
	prep, err := inst.Prepare(t.Context(), cfg, platform, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, fetcher.calls)
	assert.Equal(t, []string{"tok-123"}, fetcher.tokens)
	assert.Equal(t, "/.sofa/harness/sofa-harness-antigravity", prep.ContainerPath)

	expectedCacheDir := filepath.Join(cacheRoot, "github.com", "org", "sofa-harness-antigravity", "0.2.0", "linux_amd64")
	assert.Equal(t, expectedCacheDir, prep.HostDir)

	marker, err := os.ReadFile(filepath.Join(prep.HostDir, ".verified.sha256"))
	require.NoError(t, err)
	assert.Equal(t, digest, string(bytes.TrimSpace(marker)))

	content, err := os.ReadFile(filepath.Join(prep.HostDir, "lib", "helper.py"))
	require.NoError(t, err)
	assert.Equal(t, "print('hi')\n", string(content))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(prep.HostDir, "sofa-harness-antigravity"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		binInfo, err := os.Stat(filepath.Join(prep.HostDir, "bin", "python"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), binInfo.Mode().Perm())
	}

	// 2. Second call hits local cache without invoking ReleaseFetcher.
	prep2, err := inst.Prepare(t.Context(), cfg, platform, installer.ResolveOptions{
		CacheRoot:      cacheRoot,
		LockFile:       lockFile,
		ReleaseFetcher: &fakeReleaseFetcher{err: errors.New("network should not be called on cache hit")},
		LookupEnv:      opts.LookupEnv,
	})
	require.NoError(t, err)
	assert.Equal(t, prep, prep2)

	// 3. Corrupt .verified.sha256 triggers purge and re-fetch.
	require.NoError(t, os.WriteFile(filepath.Join(prep.HostDir, ".verified.sha256"), []byte("bad"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(prep.HostDir, "stale-extra.txt"), []byte("stale"), 0o644))
	prep3, err := inst.Prepare(t.Context(), cfg, platform, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, fetcher.calls)
	assert.Equal(t, prep.HostDir, prep3.HostDir)
	_, err = os.Stat(filepath.Join(prep3.HostDir, "stale-extra.txt"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRemotePluginRejectsDigestMismatchAndUnsafeArchives(t *testing.T) {
	t.Parallel()

	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	cfg := harnessspec.HarnessConfig{
		Name:      "copilot",
		Source:    "github.com/org/sofa-harness-copilot",
		Version:   "1.0.0",
		Signature: harnessspec.SignatureModeNone,
	}

	makeOpts := func(t *testing.T, zipBytes []byte, lockedDigest string) (installer.ResolveOptions, string) {
		t.Helper()
		if lockedDigest == "" {
			sum := sha256.Sum256(zipBytes)
			lockedDigest = hex.EncodeToString(sum[:])
		}
		cacheRoot := t.TempDir()
		return installer.ResolveOptions{
			CacheRoot: cacheRoot,
			LockFile: harness.PluginLockFile{
				FormatVersion: harness.LockFormatVersion,
				Plugins: []harness.PluginLockEntry{
					{
						Name:              "copilot",
						Source:            cfg.Source,
						Version:           cfg.Version,
						VerifiedSignature: "none",
						Assets: map[string]harness.PluginAsset{
							"linux_amd64": {
								Filename: "sofa-harness-copilot_linux_amd64.zip",
								SHA256:   lockedDigest,
							},
						},
					},
				},
			},
			ReleaseFetcher: &fakeReleaseFetcher{
				assets: map[string][]byte{
					"github.com/org/sofa-harness-copilot@v1.0.0/sofa-harness-copilot_linux_amd64.zip": zipBytes,
				},
			},
		}, cacheRoot
	}

	inst := installer.NewInstaller()

	t.Run("tampered zip rejected before extraction", func(t *testing.T) {
		t.Parallel()
		opts, cacheRoot := makeOpts(t, []byte("tampered-bytes"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		_, err := inst.Prepare(t.Context(), cfg, platform, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sha256")
		entries, readErr := os.ReadDir(cacheRoot)
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	})

	t.Run("missing platform asset in lockfile rejected", func(t *testing.T) {
		t.Parallel()
		opts := installer.ResolveOptions{
			CacheRoot: t.TempDir(),
			LockFile: harness.PluginLockFile{
				FormatVersion: harness.LockFormatVersion,
				Plugins: []harness.PluginLockEntry{
					{
						Name:              "copilot",
						Source:            cfg.Source,
						Version:           cfg.Version,
						VerifiedSignature: "none",
						Assets: map[string]harness.PluginAsset{
							"linux_arm64": {
								Filename: "sofa-harness-copilot_linux_arm64.zip",
								SHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
							},
						},
					},
				},
			},
			ReleaseFetcher: &fakeReleaseFetcher{err: errors.New("should not fetch")},
		}
		_, err := inst.Prepare(t.Context(), cfg, platform, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "linux_amd64")
	})

	unsafeZipCases := []struct {
		name    string
		entries []zipEntrySpec
	}{
		{
			name: "zip slip dotdot",
			entries: []zipEntrySpec{
				{name: "sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
				{name: "../../evil.txt", data: []byte("bad"), mode: 0o644},
			},
		},
		{
			name: "backslash path",
			entries: []zipEntrySpec{
				{name: "sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
				{name: `sub\evil.txt`, data: []byte("bad"), mode: 0o644},
			},
		},
		{
			name: "absolute path",
			entries: []zipEntrySpec{
				{name: "sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
				{name: "/etc/passwd", data: []byte("bad"), mode: 0o644},
			},
		},
		{
			name: "drive prefix",
			entries: []zipEntrySpec{
				{name: "sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
				{name: "C:/evil.txt", data: []byte("bad"), mode: 0o644},
			},
		},
		{
			name: "symlink entry",
			entries: []zipEntrySpec{
				{name: "sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
				{name: "bad-link", data: []byte("/etc/passwd"), mode: os.ModeSymlink | 0o777},
			},
		},
		{
			name: "missing root entrypoint",
			entries: []zipEntrySpec{
				{name: "nested/sofa-harness-copilot", data: []byte("ok"), mode: 0o755},
			},
		},
	}

	for _, tc := range unsafeZipCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			zb := buildTestZip(t, tc.entries)
			opts, _ := makeOpts(t, zb, "")
			_, err := inst.Prepare(t.Context(), cfg, platform, opts)
			require.Error(t, err)
		})
	}

	t.Run("zip bomb single file limit rejected", func(t *testing.T) {
		t.Parallel()
		zb := buildTestZip(t, []zipEntrySpec{
			{name: "sofa-harness-copilot", data: bytes.Repeat([]byte("A"), 2048), mode: 0o755},
		})
		opts, _ := makeOpts(t, zb, "")
		opts.MaxSingleFileBytes = 1024
		_, err := inst.Prepare(t.Context(), cfg, platform, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})

	t.Run("zip bomb total size limit rejected", func(t *testing.T) {
		t.Parallel()
		zb := buildTestZip(t, []zipEntrySpec{
			{name: "sofa-harness-copilot", data: bytes.Repeat([]byte("A"), 600), mode: 0o755},
			{name: "extra.bin", data: bytes.Repeat([]byte("B"), 600), mode: 0o644},
		})
		opts, _ := makeOpts(t, zb, "")
		opts.MaxTotalBytes = 1000
		_, err := inst.Prepare(t.Context(), cfg, platform, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})

	t.Run("zip bomb entry count limit rejected", func(t *testing.T) {
		t.Parallel()
		zb := buildTestZip(t, []zipEntrySpec{
			{name: "sofa-harness-copilot", data: []byte("a"), mode: 0o755},
			{name: "f1", data: []byte("b"), mode: 0o644},
			{name: "f2", data: []byte("c"), mode: 0o644},
		})
		opts, _ := makeOpts(t, zb, "")
		opts.MaxEntries = 2
		_, err := inst.Prepare(t.Context(), cfg, platform, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "entry count")
	})
}

func TestRemotePluginConcurrentInstallsSerializeCleanly(t *testing.T) {
	t.Parallel()

	platform := installer.Platform{OS: "linux", Arch: "amd64"}
	cfg := harnessspec.HarnessConfig{
		Name:      "copilot",
		Source:    "127.0.0.1:18080/org/sofa-harness-copilot",
		Version:   "0.3.0",
		Signature: harnessspec.SignatureModeNone,
	}
	zb := buildTestZip(t, []zipEntrySpec{
		{name: "sofa-harness-copilot", data: []byte("elf"), mode: 0o755},
	})
	sum := sha256.Sum256(zb)
	digest := hex.EncodeToString(sum[:])

	var fetchCount atomic.Int32
	fetcher := &countingFetcher{
		data:  zb,
		count: &fetchCount,
	}
	inst := installer.NewInstaller()
	opts := installer.ResolveOptions{
		CacheRoot: t.TempDir(),
		LockFile: harness.PluginLockFile{
			FormatVersion: harness.LockFormatVersion,
			Plugins: []harness.PluginLockEntry{
				{
					Name:              "copilot",
					Source:            cfg.Source,
					Version:           cfg.Version,
					VerifiedSignature: "none",
					Assets: map[string]harness.PluginAsset{
						"linux_amd64": {Filename: "sofa-harness-copilot_linux_amd64.zip", SHA256: digest},
					},
				},
			},
		},
		ReleaseFetcher: fetcher,
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	dirs := make(chan string, workers)
	for range workers {
		go func() {
			defer wg.Done()
			prep, err := inst.Prepare(t.Context(), cfg, platform, opts)
			if err != nil {
				errs <- err
				return
			}
			dirs <- prep.HostDir
		}()
	}
	wg.Wait()
	close(errs)
	close(dirs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), fetchCount.Load())

	var firstDir string
	for dir := range dirs {
		if firstDir == "" {
			firstDir = dir
		}
		assert.Equal(t, firstDir, dir)
	}
}

type countingFetcher struct {
	data  []byte
	count *atomic.Int32
}

func (f *countingFetcher) FetchReleaseAsset(_ context.Context, _, _, _, _, _, _ string) ([]byte, error) {
	f.count.Add(1)
	return f.data, nil
}

type zipEntrySpec struct {
	name string
	data []byte
	mode os.FileMode
}

func buildTestZip(t *testing.T, entries []zipEntrySpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		hdr := &zip.FileHeader{
			Name:   entry.name,
			Method: zip.Deflate,
		}
		hdr.SetMode(entry.mode)
		w, err := zw.CreateHeader(hdr)
		require.NoError(t, err)
		_, err = w.Write(entry.data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
