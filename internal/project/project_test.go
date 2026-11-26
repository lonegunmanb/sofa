package project_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubReleaseFetcher struct {
	assets map[string][]byte
	err    error
	calls  int
	tokens []string
}

func (f *stubReleaseFetcher) FetchReleaseAsset(_ context.Context, _, _, _, _, filename, token string) ([]byte, error) {
	f.calls++
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.assets[filename]
	if !ok {
		return nil, errors.New("asset not found")
	}
	return data, nil
}

type stubAttestationVerifier struct {
	err   error
	calls int
}

func (v *stubAttestationVerifier) VerifyAttestation(_ context.Context, _, _, _, _ string, _ []byte, _ string) error {
	v.calls++
	return v.err
}

func TestInitCopiesConfigInstallsModulesLocksPluginsAndSkipsIgnoredEntries(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	state := filepath.Join(t.TempDir(), ".sofa")
	writeProjectFile(t, source, "main.sofa.hcl", `
module "child" { source = "./child" }
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
output "answer" { value = module.child.answer }
`)
	writeProjectFile(t, source, "child/main.sofa.hcl", `
harness "copilot" {
  source    = "ghe.example.com/org/sofa-harness-copilot"
  version   = "0.2.0"
  signature = "none"
}
output "answer" { value = "42" }
`)
	writeProjectFile(t, source, "skills/check/SKILL.md", "# Check\n")
	writeProjectFile(t, source, ".git/config", "ignored")
	writeProjectFile(t, source, ".sofa/runs/old/events.jsonl", "ignored")
	writeProjectFile(t, source, "stale.sofaplan", "ignored")
	// Worktree .git regular file in child module should also be skipped.
	writeProjectFile(t, source, "child/.git", "gitdir: ../.git/modules/child")

	fetcher := &stubReleaseFetcher{assets: map[string][]byte{
		"checksums.txt": []byte(
			strings.Repeat("a", 64) + "  sofa-harness-antigravity_linux_amd64.zip\n" +
				strings.Repeat("b", 64) + "  sofa-harness-copilot_linux_arm64.zip\n",
		),
	}}
	verifier := &stubAttestationVerifier{}

	err := project.Init(t.Context(), source, state, project.InitOptions{
		ReleaseFetcher:      fetcher,
		AttestationVerifier: verifier,
		LookupEnv: func(k string) (string, bool) {
			if k == "GITHUB_TOKEN_GHE_EXAMPLE_COM" {
				return "ghe-secret-token", true
			}
			return "", false
		},
	})

	require.NoError(t, err)
	configDir, modulesDir, err := project.Open(state)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(configDir, "main.sofa.hcl"))
	assert.FileExists(t, filepath.Join(configDir, ".initialized.json"))
	assert.FileExists(t, filepath.Join(configDir, "skills", "check", "SKILL.md"))
	assert.NoDirExists(t, filepath.Join(configDir, ".git"))
	assert.NoDirExists(t, filepath.Join(configDir, ".sofa"))
	assert.NoFileExists(t, filepath.Join(configDir, "stale.sofaplan"))
	assert.FileExists(t, filepath.Join(modulesDir, "child", "main.sofa.hcl"))
	assert.NoFileExists(t, filepath.Join(modulesDir, "child", ".git"))
	assert.NoFileExists(t, filepath.Join(state, ".initializing"))

	proj, err := project.OpenProject(state)
	require.NoError(t, err)
	assert.Equal(t, configDir, proj.ConfigDirectory)
	assert.Equal(t, modulesDir, proj.ModulesDirectory)
	assert.Equal(t, filepath.Join(state, harness.LockFileName), proj.LockFilePath)

	lockFile, err := harness.ReadLockFile(state)
	require.NoError(t, err)
	require.Len(t, lockFile.Plugins, 2)
	assert.Equal(t, "antigravity", lockFile.Plugins[0].Name)
	assert.Equal(t, "attestation", lockFile.Plugins[0].VerifiedSignature)
	assert.Equal(t, "copilot", lockFile.Plugins[1].Name)
	assert.Equal(t, "none", lockFile.Plugins[1].VerifiedSignature)
	assert.Contains(t, fetcher.tokens, "ghe-secret-token")
}

func TestInitSnapshotIsolationAfterSourceEdits(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	state := filepath.Join(t.TempDir(), ".sofa")
	writeProjectFile(t, source, "main.sofa.hcl", `
module "child" { source = "./child" }
output "v" { value = "v1" }
`)
	writeProjectFile(t, source, "child/main.sofa.hcl", `output "c" { value = "c1" }`)
	require.NoError(t, project.Init(t.Context(), source, state, project.InitOptions{}))

	// Mutate original source files without running Init again.
	writeProjectFile(t, source, "main.sofa.hcl", `output "v" { value = "v2-mutated" }`)
	writeProjectFile(t, source, "child/main.sofa.hcl", `output "c" { value = "c2-mutated" }`)

	configDir, modulesDir, err := project.Open(state)
	require.NoError(t, err)
	rootContent, err := os.ReadFile(filepath.Join(configDir, "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Contains(t, string(rootContent), "v1")
	assert.NotContains(t, string(rootContent), "v2-mutated")

	childContent, err := os.ReadFile(filepath.Join(modulesDir, "child", "main.sofa.hcl"))
	require.NoError(t, err)
	assert.Contains(t, string(childContent), "c1")
	assert.NotContains(t, string(childContent), "c2-mutated")
}

func TestInitRejectsSymlinksAndSourceInsideStateDirectory(t *testing.T) {
	t.Parallel()

	t.Run("source inside state directory", func(t *testing.T) {
		t.Parallel()

		state := filepath.Join(t.TempDir(), ".sofa")
		source := filepath.Join(state, "source")
		writeProjectFile(t, source, "main.sofa.hcl", `output "answer" { value = "42" }`)

		err := project.Init(t.Context(), source, state, project.InitOptions{})

		require.Error(t, err)
		assert.ErrorContains(t, err, "must be outside")
	})

	t.Run("symlink inside source directory fails fast before fetching remote child modules", func(t *testing.T) {
		t.Parallel()

		source := t.TempDir()
		state := filepath.Join(t.TempDir(), ".sofa")
		writeProjectFile(t, source, "main.sofa.hcl", `
module "remote" { source = "example.com/acme/remote" }
output "answer" { value = "42" }
`)
		outside := filepath.Join(t.TempDir(), "outside.txt")
		require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
		if err := os.Symlink(outside, filepath.Join(source, "leak.txt")); err != nil {
			t.Skipf("symlinks unavailable in this environment: %v", err)
		}

		remoteFetchCalled := false
		err := project.Init(t.Context(), source, state, project.InitOptions{
			Fetch: func(context.Context, string, string, string) error {
				remoteFetchCalled = true
				return nil
			},
		})

		require.Error(t, err)
		assert.False(t, remoteFetchCalled, "root symlink validation should fail before fetching remote modules")
	})
}

func TestInitWritesRedactedRemoteSourceToStateAndErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{
			name:     "URL credentials, fragment, and non-ref query are removed",
			source:   "git::https://user:secret@example.invalid/workflow.git//sofa?ref=v1&token=sensitive#frag-secret",
			expected: "git::https://example.invalid/workflow.git//sofa?ref=v1",
		},
		{
			name:     "GitHub shorthand preserves ref only",
			source:   "github.com/acme/workflow//sofa?ref=v2&token=sensitive",
			expected: "github.com/acme/workflow//sofa?ref=v2",
		},
		{
			name:     "SSH shorthand identity is redacted",
			source:   "git@github.com:acme/workflow.git",
			expected: "<redacted-remote-source>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stateDir := filepath.Join(t.TempDir(), ".sofa")
			fetch := func(_ context.Context, _, destination, _ string) error {
				writeProjectFile(t, destination, "main.sofa.hcl", `output "answer" { value = "42" }`)
				return nil
			}

			require.NoError(t, project.Init(t.Context(), tt.source, stateDir, project.InitOptions{Fetch: fetch}))

			encoded, err := os.ReadFile(filepath.Join(stateDir, project.StateFileName))
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "secret")
			assert.NotContains(t, string(encoded), "sensitive")
			assert.NotContains(t, string(encoded), "frag-secret")
			var state project.State
			require.NoError(t, json.Unmarshal(encoded, &state))
			assert.Equal(t, project.StateFormatVersion, state.FormatVersion)
			assert.Equal(t, tt.expected, state.Configuration.Source)
		})
	}

	t.Run("remote fetch error redacts credentials and preserves prior state", func(t *testing.T) {
		t.Parallel()

		localSource := t.TempDir()
		state := filepath.Join(t.TempDir(), ".sofa")
		writeProjectFile(t, localSource, "main.sofa.hcl", `output "version" { value = "one" }`)
		require.NoError(t, project.Init(t.Context(), localSource, state, project.InitOptions{}))

		const remoteSource = "https://user:secret-pass@example.invalid/workflow.tar.gz?token=secret-tok#secret-frag"
		err := project.Init(t.Context(), remoteSource, state, project.InitOptions{
			Fetch: func(context.Context, string, string, string) error {
				return errors.New("download failed for " + remoteSource)
			},
		})

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret-pass")
		assert.NotContains(t, err.Error(), "secret-tok")
		assert.NotContains(t, err.Error(), "secret-frag")

		configDir, _, openErr := project.Open(state)
		require.NoError(t, openErr)
		content, readErr := os.ReadFile(filepath.Join(configDir, "main.sofa.hcl"))
		require.NoError(t, readErr)
		assert.Contains(t, string(content), `value = "one"`)
	})
}

func TestInitDownloadsRemoteConfigurationWithGoGetter(t *testing.T) {
	t.Parallel()

	archive := newProjectArchive(t, `output "answer" { value = "remote" }`)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/zip")
		_, _ = response.Write(archive)
	}))
	t.Cleanup(server.Close)
	state := filepath.Join(t.TempDir(), ".sofa")

	err := project.Init(t.Context(), server.URL+"/workflow.zip", state, project.InitOptions{})

	require.NoError(t, err)
	configDir, _, err := project.Open(state)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(configDir, "main.sofa.hcl"))
}

func TestProjectOutputsLifecycleAndValidation(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), ".sofa")
	runDir := filepath.Join(stateDir, "runs", "run-test")
	writeProjectFile(t, source, "main.sofa.hcl", `output "answer" { value = "42" }`)
	require.NoError(t, project.Init(t.Context(), source, stateDir, project.InitOptions{}))

	require.NoError(t, project.SaveOutputs(stateDir, runDir, []byte(`{"answer":"42"}`)))
	outputs, err := project.ReadOutputs(stateDir)
	require.NoError(t, err)
	assert.JSONEq(t, `{"answer":"42"}`, string(outputs))

	for _, bad := range []string{`[]`, `null`, `{`} {
		err := project.SaveOutputs(stateDir, runDir, []byte(bad))
		require.Error(t, err)
		assert.ErrorContains(t, err, "JSON object")
	}

	// Next Init resets saved outputs.
	require.NoError(t, project.Init(t.Context(), source, stateDir, project.InitOptions{}))
	_, err = project.ReadOutputs(stateDir)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no saved outputs")
}

func TestOpenRejectsMismatchedIncompleteAndInterruptedStateAndInitRecovers(t *testing.T) {
	t.Parallel()

	t.Run("incomplete directory", func(t *testing.T) {
		t.Parallel()

		state := filepath.Join(t.TempDir(), ".sofa")
		require.NoError(t, os.MkdirAll(filepath.Join(state, "config"), 0o700))
		require.NoError(t, os.MkdirAll(filepath.Join(state, "modules"), 0o700))

		_, _, err := project.Open(state)
		require.Error(t, err)
		assert.ErrorContains(t, err, "run sofa init")
	})

	t.Run("mismatched state timestamp", func(t *testing.T) {
		t.Parallel()

		source := t.TempDir()
		stateDir := filepath.Join(t.TempDir(), ".sofa")
		writeProjectFile(t, source, "main.sofa.hcl", `output "answer" { value = "42" }`)
		require.NoError(t, project.Init(t.Context(), source, stateDir, project.InitOptions{}))

		raw, err := os.ReadFile(filepath.Join(stateDir, project.StateFileName))
		require.NoError(t, err)
		var state project.State
		require.NoError(t, json.Unmarshal(raw, &state))
		state.Configuration.InitializedAt = state.Configuration.InitializedAt.Add(time.Second)
		updated, err := json.Marshal(state)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(stateDir, project.StateFileName), updated, 0o600))

		_, _, err = project.Open(stateDir)
		require.Error(t, err)
		assert.ErrorContains(t, err, "does not match initialized configuration")
	})

	t.Run("module.Init failure keeps .initializing and blocks Open until recovered", func(t *testing.T) {
		t.Parallel()

		source := t.TempDir()
		state := filepath.Join(t.TempDir(), ".sofa")
		writeProjectFile(t, source, "main.sofa.hcl", `output "v" { value = "one" }`)
		require.NoError(t, project.Init(t.Context(), source, state, project.InitOptions{}))

		writeProjectFile(t, source, "main.sofa.hcl", `module "broken" { source = "./missing" }`)
		err := project.Init(t.Context(), source, state, project.InitOptions{})
		require.Error(t, err)
		assert.FileExists(t, filepath.Join(state, ".initializing"))

		_, _, openErr := project.Open(state)
		require.Error(t, openErr)
		assert.ErrorContains(t, openErr, "initialization was interrupted")
		assert.ErrorContains(t, openErr, "run sofa init")

		// Fix source and re-run Init to recover cleanly.
		writeProjectFile(t, source, "main.sofa.hcl", `module "ok" { source = "./ok" }`)
		writeProjectFile(t, source, "ok/main.sofa.hcl", `output "v" { value = "recovered" }`)
		require.NoError(t, project.Init(t.Context(), source, state, project.InitOptions{}))
		assert.NoFileExists(t, filepath.Join(state, ".initializing"))
		_, modulesDir, openErr := project.Open(state)
		require.NoError(t, openErr)
		assert.FileExists(t, filepath.Join(modulesDir, "ok", "main.sofa.hcl"))
	})

	t.Run("harness.LockPlugins failure keeps .initializing and blocks Open until recovered", func(t *testing.T) {
		t.Parallel()

		source := t.TempDir()
		state := filepath.Join(t.TempDir(), ".sofa")
		writeProjectFile(t, source, "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
}
`)
		err := project.Init(t.Context(), source, state, project.InitOptions{
			ReleaseFetcher: &stubReleaseFetcher{err: errors.New("release server unavailable")},
		})
		require.Error(t, err)
		assert.FileExists(t, filepath.Join(state, ".initializing"))

		_, _, openErr := project.Open(state)
		require.Error(t, openErr)
		assert.ErrorContains(t, openErr, "initialization was interrupted")

		require.NoError(t, project.Init(t.Context(), source, state, project.InitOptions{
			ReleaseFetcher: &stubReleaseFetcher{assets: map[string][]byte{
				"checksums.txt": []byte(strings.Repeat("a", 64) + "  sofa-harness-antigravity_linux_amd64.zip\n"),
			}},
		}))
		assert.NoFileExists(t, filepath.Join(state, ".initializing"))
		_, _, openErr = project.Open(state)
		require.NoError(t, openErr)
	})
}

func TestInitFromWorkingDirectoryDoesNotCopyStateDirectory(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	state := filepath.Join(source, ".sofa")
	writeProjectFile(t, source, "main.sofa.hcl", `output "answer" { value = "42" }`)
	writeProjectFile(t, source, ".sofa/old.txt", "old state")

	require.NoError(t, project.Init(t.Context(), source, state, project.InitOptions{}))

	configDir, _, err := project.Open(state)
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(configDir, ".sofa"))
}

func writeProjectFile(t *testing.T, root, relativePath, content string) {
	t.Helper()

	path := filepath.Join(root, filepath.FromSlash(relativePath))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o600))
}

func newProjectArchive(t *testing.T, source string) []byte {
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
