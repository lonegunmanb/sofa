package module_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lonegunmanb/sofa/internal/module"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModuleDirectoryUsesCanonicalModuleAddress(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "modules")
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{name: "root child", address: "module.a", want: filepath.Join(root, "a")},
		{name: "nested child", address: "module.a.module.b.module.c", want: filepath.Join(root, "a", "b", "c")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := module.Directory(root, tt.address)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestModuleDirectoryRejectsInvalidAddresses(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"", "sofa.a", "module", "module.a.module", "module...module.b", "module.a/b"} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			_, err := module.Directory(t.TempDir(), address)
			require.Error(t, err)
			assert.ErrorContains(t, err, "module address")
		})
	}
}

func TestInitCopiesNestedLocalModulesAndFiltersIgnoredFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	child := filepath.Join(root, "modules", "a")
	grandchild := filepath.Join(child, "b")
	require.NoError(t, os.MkdirAll(filepath.Join(child, ".git"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(child, ".sofa"), 0o700))
	require.NoError(t, os.MkdirAll(grandchild, 0o700))

	writeConfig(t, root, `module "a" { source = "./modules/a" }`)
	writeConfig(t, child, `module "b" { source = "./b" }`)
	require.NoError(t, os.WriteFile(filepath.Join(child, ".git", "config"), []byte("ignored"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(child, ".sofa", "state.json"), []byte("ignored"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(child, "cached.sofaplan"), []byte("ignored"), 0o600))
	writeConfig(t, grandchild, `output "answer" { value = "42" }`)
	// Simulate a git worktree/submodule .git regular file inside grandchild.
	require.NoError(t, os.WriteFile(filepath.Join(grandchild, ".git"), []byte("gitdir: ../../.git/modules/b\n"), 0o600))

	err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(modules, "a", "main.sofa.hcl"))
	assert.FileExists(t, filepath.Join(modules, "a", "b", "main.sofa.hcl"))
	assert.NoDirExists(t, filepath.Join(modules, "a", ".git"))
	assert.NoFileExists(t, filepath.Join(modules, "a", "b", ".git"))
	assert.NoDirExists(t, filepath.Join(modules, "a", ".sofa"))
	assert.NoFileExists(t, filepath.Join(modules, "a", "cached.sofaplan"))
}

func TestCopyCleanDirectorySkipsGitWorktreeFile(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: /path/to/worktree\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.sofa.hcl"), []byte(`output "ok" { value = "1" }`+"\n"), 0o600))

	require.NoError(t, module.CopyCleanDirectory(t.Context(), src, dst))
	assert.FileExists(t, filepath.Join(dst, "main.sofa.hcl"))
	assert.NoFileExists(t, filepath.Join(dst, ".git"))
}

func TestInitResolvesNestedLocalSourcesFromOriginalModuleDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	parent := filepath.Join(root, "parent")
	shared := filepath.Join(root, "shared")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, os.Mkdir(shared, 0o700))
	writeConfig(t, root, `module "parent" { source = "./parent" }`)
	writeConfig(t, parent, `module "nested" { source = "../shared" }`)
	writeConfig(t, shared, `output "answer" { value = "42" }`)

	err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(modules, "parent", "nested", "main.sofa.hcl"))
}

func TestInitReplacesCopiedDirectoryThatConflictsWithNestedModuleLabel(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	parent := filepath.Join(root, "parent")
	conflicting := filepath.Join(parent, "nested")
	shared := filepath.Join(root, "shared")
	require.NoError(t, os.MkdirAll(conflicting, 0o700))
	require.NoError(t, os.Mkdir(shared, 0o700))
	writeConfig(t, root, `module "parent" { source = "./parent" }`)
	writeConfig(t, parent, `module "nested" { source = "../shared" }`)
	writeConfig(t, conflicting, `output "origin" { value = "copied parent directory" }`)
	writeConfig(t, shared, `output "origin" { value = "declared module source" }`)

	err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

	require.NoError(t, err)
	installed, err := os.ReadFile(filepath.Join(modules, "parent", "nested", "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "output \"origin\" { value = \"declared module source\" }\n", string(installed))
}

func TestInitRejectsNestedLocalSourceCycles(t *testing.T) {
	t.Parallel()

	t.Run("a -> b -> a", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		modules := filepath.Join(t.TempDir(), "modules")
		dirA := filepath.Join(root, "a")
		dirB := filepath.Join(root, "b")
		require.NoError(t, os.Mkdir(dirA, 0o700))
		require.NoError(t, os.Mkdir(dirB, 0o700))
		writeConfig(t, root, `module "a" { source = "./a" }`)
		writeConfig(t, dirA, `module "b" { source = "../b" }`)
		writeConfig(t, dirB, `module "a" { source = "../a" }`)

		err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

		require.Error(t, err)
		assert.ErrorContains(t, err, "module source cycle at module.a.module.b.module.a")
	})

	t.Run("self cycle a -> a", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		modules := filepath.Join(t.TempDir(), "modules")
		dirA := filepath.Join(root, "a")
		require.NoError(t, os.Mkdir(dirA, 0o700))
		writeConfig(t, root, `module "a" { source = "./a" }`)
		writeConfig(t, dirA, `module "a" { source = "." }`)

		err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

		require.Error(t, err)
		assert.ErrorContains(t, err, "module source cycle at module.a.module.a")
	})
}

func TestInitRejectsSymlinksInLocalModule(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	writeConfig(t, root, `module "child" { source = "./child" }`)
	writeConfig(t, child, `output "answer" { value = "42" }`)

	target := filepath.Join(root, "outside.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	linkPath := filepath.Join(child, "link.txt")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	err := module.Init(t.Context(), root, module.InitOptions{
		ModulesDirectory: filepath.Join(t.TempDir(), "modules"),
	})

	require.Error(t, err)
}

func TestInitUsesFetcherForNonLocalSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	writeConfig(t, root, `module "remote" { source = "example.com/acme/sofa-mod" }`)
	var gotSource string
	var gotWorkingDirectory string
	fetch := func(_ context.Context, source, destination, workingDirectory string) error {
		gotSource = source
		gotWorkingDirectory = workingDirectory
		return os.WriteFile(filepath.Join(destination, "main.sofa.hcl"), []byte(`output "answer" { value = "remote" }`), 0o600)
	}

	err := module.Init(t.Context(), root, module.InitOptions{
		ModulesDirectory: modules,
		Fetch:            fetch,
	})

	require.NoError(t, err)
	assert.Equal(t, "example.com/acme/sofa-mod", gotSource)
	assert.Equal(t, root, gotWorkingDirectory)
	assert.FileExists(t, filepath.Join(modules, "remote", "main.sofa.hcl"))
}

func TestInitDownloadsRemoteModuleWithGoGetter(t *testing.T) {
	t.Parallel()

	archive := newModuleArchive(t, `output "answer" { value = "remote" }`)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/zip")
		_, _ = response.Write(archive)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	writeConfig(t, root, `module "remote" { source = "`+server.URL+`/module.zip" }`)

	err := module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules})

	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(modules, "remote", "main.sofa.hcl"))
}

func TestInitReusesCachedModulesWhenUpgradeFalseAndStillRecursesIntoChildren(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	parent := filepath.Join(root, "parent")
	require.NoError(t, os.Mkdir(parent, 0o700))
	writeConfig(t, root, `module "parent" { source = "./parent" }`)
	writeConfig(t, parent, `output "version" { value = "one" }`)
	require.NoError(t, module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules}))

	// Update source parent; with Upgrade=false, cached parent is kept without re-copying.
	writeConfig(t, parent, `output "version" { value = "two" }`)
	require.NoError(t, module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules}))
	installed, err := os.ReadFile(filepath.Join(modules, "parent", "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Contains(t, string(installed), `value = "one"`)

	// Simulate cached parent already existing on disk while its remote child is missing.
	writeConfig(t, filepath.Join(modules, "parent"), `module "nested" { source = "example.com/acme/nested" }`)
	fetchCount := 0
	require.NoError(t, module.Init(t.Context(), root, module.InitOptions{
		ModulesDirectory: modules,
		Upgrade:          false,
		Fetch: func(_ context.Context, _, destination, _ string) error {
			fetchCount++
			return os.WriteFile(filepath.Join(destination, "main.sofa.hcl"), []byte(`output "ok" { value = "1" }`), 0o600)
		},
	}))
	assert.Equal(t, 1, fetchCount)
	assert.FileExists(t, filepath.Join(modules, "parent", "nested", "main.sofa.hcl"))

	// Upgrade=true refreshes from source.
	require.NoError(t, module.Init(t.Context(), root, module.InitOptions{
		ModulesDirectory: modules,
		Upgrade:          true,
	}))
	installed, err = os.ReadFile(filepath.Join(modules, "parent", "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Contains(t, string(installed), `value = "two"`)
	assert.NoFileExists(t, filepath.Join(modules, "parent", "nested", "main.sofa.hcl"))
}

func TestInitPreservesInstalledTreeWhenUpgradeFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modules := filepath.Join(t.TempDir(), "modules")
	parent := filepath.Join(root, "parent")
	require.NoError(t, os.Mkdir(parent, 0o700))
	writeConfig(t, root, `module "parent" { source = "./parent" }`)
	writeConfig(t, parent, `output "version" { value = "existing" }`)
	require.NoError(t, module.Init(t.Context(), root, module.InitOptions{ModulesDirectory: modules}))

	writeConfig(t, parent, `
module "nested" { source = "example.com/acme/sofa-mod" }
output "version" { value = "replacement" }
`)
	err := module.Init(t.Context(), root, module.InitOptions{
		ModulesDirectory: modules,
		Upgrade:          true,
		Fetch: func(context.Context, string, string, string) error {
			return errors.New("nested download failed")
		},
	})

	require.Error(t, err)
	require.ErrorContains(t, err, "nested download failed")
	installedSource, readErr := os.ReadFile(filepath.Join(modules, "parent", "main.sofa.hcl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(installedSource), "existing")
	assert.NotContains(t, string(installedSource), "replacement")
}

func TestInitRejectsInvalidModuleDeclarations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		source    string
		wantError string
	}{
		{name: "missing source", source: `module "child" {}`, wantError: "module source must be a literal string"},
		{name: "wrong source type", source: `module "child" { source = 42 }`, wantError: "module source must be a literal string"},
		{name: "variable source", source: "variable \"mod\" { type = string }\nmodule \"child\" { source = var.mod }", wantError: "module source must be a literal string"},
		{name: "computed local source", source: "locals { child = \"./c\" }\nmodule \"child\" { source = local.child }", wantError: "module source must be a literal string"},
		{name: "empty source", source: `module "child" { source = "" }`, wantError: "module source must not be empty"},
		{name: "whitespace source", source: `module "child" { source = "   " }`, wantError: "module source must not be empty"},
		{name: "unsafe label dotdot", source: `module "../evil" { source = "./child" }`, wantError: "module label is invalid"},
		{name: "unsafe label empty", source: `module "" { source = "./child" }`, wantError: "module label is invalid"},
		{name: "unsafe label slash", source: `module "a/b" { source = "./child" }`, wantError: "module label is invalid"},
		{name: "unsafe label backslash", source: `module "a\\b" { source = "./child" }`, wantError: "module label is invalid"},
		{name: "multiple labels", source: `module "a" "b" { source = "./child" }`, wantError: "module label is invalid"},
		{
			name: "duplicate label",
			source: `
module "child" { source = "./first" }
module "child" { source = "./second" }
`,
			wantError: `module "child" is declared more than once`,
		},
		{
			name: "case-insensitive label collision",
			source: `
module "Audit" { source = "./first" }
module "audit" { source = "./second" }
`,
			wantError: `module "audit" conflicts with module "Audit"`,
		},
		{name: "self cycle to root", source: `module "self" { source = "." }`, wantError: "module source cycle"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeConfig(t, root, tt.source)
			err := module.Init(t.Context(), root, module.InitOptions{
				ModulesDirectory: filepath.Join(t.TempDir(), "modules"),
			})

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestInitRedactsCredentialsFragmentsAndQueryFromFetchErrors(t *testing.T) {
	t.Parallel()

	t.Run("default Fetch redacts URL secrets", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeConfig(t, root, `
module "remote" {
  source = "unknown::https://user:secret-password@example.test/module?ref=v1&token=secret-query#secret-frag"
}
`)

		err := module.Init(t.Context(), root, module.InitOptions{
			ModulesDirectory: filepath.Join(t.TempDir(), "modules"),
		})

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret-password")
		assert.NotContains(t, err.Error(), "secret-query")
		assert.NotContains(t, err.Error(), "secret-frag")
	})

	t.Run("custom FetchFunc error redacts percent-encoded credentials and query secrets", func(t *testing.T) {
		t.Parallel()

		const remoteSource = "https://user%40corp:p%40ssword@example.test/module.zip?ref=v1&token=tok%3D123#frag%20sec"
		root := t.TempDir()
		writeConfig(t, root, `module "remote" { source = "`+remoteSource+`" }`)

		err := module.Init(t.Context(), root, module.InitOptions{
			ModulesDirectory: filepath.Join(t.TempDir(), "modules"),
			Fetch: func(context.Context, string, string, string) error {
				return errors.New("custom fetch failed for " + remoteSource)
			},
		})

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "user%40corp")
		assert.NotContains(t, err.Error(), "p%40ssword")
		assert.NotContains(t, err.Error(), "p@ssword")
		assert.NotContains(t, err.Error(), "tok%3D123")
		assert.NotContains(t, err.Error(), "frag%20sec")
	})
}

func TestInitRemoteModuleAppliesCleanDirectoryHygiene(t *testing.T) {
	t.Parallel()

	t.Run("strips .git, .sofa, and *.sofaplan from fetched remote module", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		modules := filepath.Join(t.TempDir(), "modules")
		writeConfig(t, root, `module "remote" { source = "example.com/acme/mod" }`)

		err := module.Init(t.Context(), root, module.InitOptions{
			ModulesDirectory: modules,
			Fetch: func(_ context.Context, _, destination, _ string) error {
				require.NoError(t, os.MkdirAll(filepath.Join(destination, ".git"), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Join(destination, ".sofa"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(destination, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(destination, ".sofa", "state.json"), []byte("{}"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(destination, "stale.sofaplan"), []byte("plan"), 0o600))
				return os.WriteFile(filepath.Join(destination, "main.sofa.hcl"), []byte(`output "ok" { value = "1" }`), 0o600)
			},
		})

		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(modules, "remote", "main.sofa.hcl"))
		assert.NoDirExists(t, filepath.Join(modules, "remote", ".git"))
		assert.NoDirExists(t, filepath.Join(modules, "remote", ".sofa"))
		assert.NoFileExists(t, filepath.Join(modules, "remote", "stale.sofaplan"))
	})

	t.Run("rejects symlink created by custom FetchFunc", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		modules := filepath.Join(t.TempDir(), "modules")
		outside := filepath.Join(t.TempDir(), "outside.txt")
		require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
		writeConfig(t, root, `module "remote" { source = "example.com/acme/mod" }`)

		var symlinkUnsupported bool
		err := module.Init(t.Context(), root, module.InitOptions{
			ModulesDirectory: modules,
			Fetch: func(_ context.Context, _, destination, _ string) error {
				if symErr := os.Symlink(outside, filepath.Join(destination, "bad.txt")); symErr != nil {
					symlinkUnsupported = true
					return symErr
				}
				return os.WriteFile(filepath.Join(destination, "main.sofa.hcl"), []byte(`output "ok" { value = "1" }`), 0o600)
			},
		})
		if symlinkUnsupported {
			t.Skip("symlinks unavailable in this environment")
		}

		require.Error(t, err)
		assert.NoDirExists(t, filepath.Join(modules, "remote"))
	})
}

func writeConfig(t *testing.T, directory, source string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(directory, "main.sofa.hcl"),
		[]byte(source+"\n"),
		0o600,
	))
}

func newModuleArchive(t *testing.T, source string) []byte {
	t.Helper()

	var encoded bytes.Buffer
	archive := zip.NewWriter(&encoded)
	file, err := archive.Create("main.sofa.hcl")
	require.NoError(t, err)
	_, err = file.Write([]byte(source + "\n"))
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	return encoded.Bytes()
}
