package harness_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFetcher struct {
	assets map[string][]byte
	err    error
	calls  []string
	tokens []string
}

func (f *fakeFetcher) FetchReleaseAsset(_ context.Context, host, owner, repo, tag, filename, token string) ([]byte, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%s@%s:%s", host, owner, repo, tag, filename))
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.assets[filename]
	if !ok {
		return nil, fmt.Errorf("asset %q not found", filename)
	}
	return data, nil
}

type fakeAttestationVerifier struct {
	err    error
	calls  int
	tokens []string
}

func (v *fakeAttestationVerifier) VerifyAttestation(_ context.Context, _, _, _, _ string, _ []byte, token string) error {
	v.calls++
	v.tokens = append(v.tokens, token)
	return v.err
}

func TestLockPluginsLocalModeMakesZeroNetworkCalls(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "copilot" {}

harness "custom" {
  source  = null
  version = null
}
`)
	fetcher := &fakeFetcher{err: errors.New("network should not be called")}
	verifier := &fakeAttestationVerifier{err: errors.New("network should not be called")}

	lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		ReleaseFetcher:      fetcher,
		AttestationVerifier: verifier,
	})

	require.NoError(t, err)
	assert.Equal(t, harness.LockFormatVersion, lockFile.FormatVersion)
	assert.Empty(t, lockFile.Plugins)
	assert.Empty(t, fetcher.calls)
	assert.Zero(t, verifier.calls)

	persisted, err := harness.ReadLockFile(stateDir)
	require.NoError(t, err)
	assert.Equal(t, lockFile, persisted)
}

func TestLockPluginsAllowsPlanVariablesInOptionsAndEnvRefsButRejectsInMetadata(t *testing.T) {
	t.Parallel()

	t.Run("allows var and local in options and env_refs", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
variable "model_loc" {
  type    = string
  default = "us-central1"
}

locals {
  secret_ref = "GEMINI_API_KEY"
}

harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
  env_refs  = { GEMINI_API_KEY = local.secret_ref }
  options   = { location = var.model_loc }
}
`)
		checksums := sampleChecksums("antigravity")
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": checksums}}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Equal(t, "none", lockFile.Plugins[0].VerifiedSignature)
	})

	for _, attr := range []string{"source", "version", "signature", "signing_key"} {
		t.Run("rejects variable in "+attr, func(t *testing.T) {
			t.Parallel()

			stateDir := t.TempDir()
			body := `
variable "v" { type = string }
harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.1.0"
`
			switch attr {
			case "source":
				body = "variable \"v\" { type = string }\nharness \"antigravity\" {\n  source  = var.v\n  version = \"0.1.0\"\n}"
			case "version":
				body = "variable \"v\" { type = string }\nharness \"antigravity\" {\n  source  = \"github.com/lonegunmanb/sofa-harness-antigravity\"\n  version = var.v\n}"
			case "signature":
				body += `  signature = var.v` + "\n}"
			case "signing_key":
				body += `  signature = "pgp"` + "\n" + `  signing_key = var.v` + "\n}"
			}
			writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", body)

			_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
				ReleaseFetcher: &fakeFetcher{},
			})

			require.Error(t, err)
			assert.ErrorContains(t, err, "static literal")
		})
	}
}

func TestLockPluginsSignatureModesAndPGPVerification(t *testing.T) {
	t.Parallel()

	pubArmor, armoredSig, binarySig, fingerprint, checksums := generateSignedChecksums(t, "antigravity")

	t.Run("auto succeeds with attestation", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.1.0"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": checksums}}
		verifier := &fakeAttestationVerifier{err: nil}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Equal(t, "attestation", lockFile.Plugins[0].VerifiedSignature)
		assert.Empty(t, lockFile.Plugins[0].SigningKeyFingerprint)
	})

	t.Run("auto falls back to PGP on ErrAttestationNotFound", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "auto"
  signing_key = %q
}
`, pubArmor))
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt":     checksums,
			"checksums.txt.sig": armoredSig,
		}}
		verifier := &fakeAttestationVerifier{err: harness.ErrAttestationNotFound}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Equal(t, "pgp", lockFile.Plugins[0].VerifiedSignature)
		assert.Equal(t, fingerprint, lockFile.Plugins[0].SigningKeyFingerprint)
	})

	t.Run("auto fails without fallback when attestation verification fails", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "auto"
  signing_key = %q
}
`, pubArmor))
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt":     checksums,
			"checksums.txt.sig": armoredSig,
		}}
		verifier := &fakeAttestationVerifier{err: errors.New("forged attestation bundle")}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.Error(t, err)
		assert.ErrorContains(t, err, "forged attestation bundle")
	})

	t.Run("auto fails when ErrAttestationNotFound and no signing_key", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.1.0"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": checksums}}
		verifier := &fakeAttestationVerifier{err: harness.ErrAttestationNotFound}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, harness.ErrAttestationNotFound)
	})

	t.Run("attestation mode rejects ErrAttestationNotFound", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": checksums}}
		verifier := &fakeAttestationVerifier{err: harness.ErrAttestationNotFound}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, harness.ErrAttestationNotFound)
	})

	t.Run("pgp accepts binary detached signature", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "pgp"
  signing_key = %q
}
`, pubArmor))
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt":     checksums,
			"checksums.txt.sig": binarySig,
		}}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Equal(t, "pgp", lockFile.Plugins[0].VerifiedSignature)
		assert.Equal(t, fingerprint, lockFile.Plugins[0].SigningKeyFingerprint)
	})

	t.Run("pgp rejects tampered checksums.txt", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "pgp"
  signing_key = %q
}
`, pubArmor))
		tampered := bytes.ReplaceAll(checksums, []byte("aaaa"), []byte("bbbb"))
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt":     tampered,
			"checksums.txt.sig": armoredSig,
		}}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.Error(t, err)
		assert.ErrorContains(t, err, "PGP")
	})

	t.Run("auto rejects invalid signing_key upfront even if attestation succeeds", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "auto"
  signing_key = "not-a-valid-openpgp-key"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": checksums}}
		verifier := &fakeAttestationVerifier{err: nil}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.Error(t, err)
		assert.ErrorContains(t, err, "signing_key")
		assert.Empty(t, fetcher.calls)
		assert.Zero(t, verifier.calls)
	})
}

func TestLockPluginsCrossModuleConflictsAndPolicyMerging(t *testing.T) {
	t.Parallel()

	t.Run("merges compatible declarations keeping stricter policy", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "child" { source = "./child" }

harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`)
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child"), "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}}
		verifier := &fakeAttestationVerifier{err: harness.ErrAttestationNotFound}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher:      fetcher,
			AttestationVerifier: verifier,
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, harness.ErrAttestationNotFound)
	})

	t.Run("rejects version conflict", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "child" { source = "./child" }

harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.1.0"
}
`)
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child"), "main.sofa.hcl", `
harness "antigravity" {
  source  = "github.com/lonegunmanb/sofa-harness-antigravity"
  version = "0.2.0"
}
`)

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})

		require.Error(t, err)
		assert.ErrorContains(t, err, "conflict")
	})

	t.Run("rejects local vs remote conflict", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "child" { source = "./child" }

harness "copilot" {}
`)
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child"), "main.sofa.hcl", `
harness "copilot" {
  source  = "github.com/lonegunmanb/sofa-harness-copilot"
  version = "0.1.0"
}
`)

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})

		require.Error(t, err)
		assert.ErrorContains(t, err, "conflict")
	})

	t.Run("rejects incompatible attestation vs pgp", func(t *testing.T) {
		t.Parallel()

		pubArmor, _, _, _, _ := generateSignedChecksums(t, "antigravity")
		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "child" { source = "./child" }

harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`)
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source      = "github.com/lonegunmanb/sofa-harness-antigravity"
  version     = "0.1.0"
  signature   = "pgp"
  signing_key = %q
}
`, pubArmor))

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})

		require.Error(t, err)
		assert.ErrorContains(t, err, "incompatible")
	})

	t.Run("ignores orphaned cached module directories and unreferenced subdirectories", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "child" { source = "./child" }

harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
}
`)
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child"), "main.sofa.hcl", `output "ok" { value = "1" }`)
		// Unreferenced subdirectory inside active module (e.g. examples/).
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "child", "examples"), "demo.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "9.9.9"
  signature = "none"
}
`)
		// Orphaned module directory left in cache after removal from config.
		writeHarnessFile(t, filepath.Join(stateDir, "modules", "removed_mod"), "main.sofa.hcl", `
harness "removed" {
  source    = "github.com/lonegunmanb/sofa-harness-removed"
  version   = "1.0.0"
  signature = "none"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Equal(t, "antigravity", lockFile.Plugins[0].Name)
		assert.Equal(t, "0.1.0", lockFile.Plugins[0].Version)
	})

	t.Run("fails when config directory or declared child module directory is missing", func(t *testing.T) {
		t.Parallel()

		emptyState := t.TempDir()
		_, err := harness.LockPlugins(t.Context(), emptyState, harness.LockOptions{})
		require.Error(t, err)

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
module "missing" { source = "./missing" }
`)
		_, err = harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "missing")
	})
}

func TestResolveGitHubTokenAndNoPersistence(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"GITHUB_TOKEN":                      "fallback-token",
		"GITHUB_TOKEN_GHE_EXAMPLE_COM_8443": "ghe-host-token",
		"GITHUB_TOKEN_EMPTY_EXAMPLE_COM":    "",
		"GITHUB_TOKEN_GITHUB_COM":           "github-specific-token",
	}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}

	assert.Equal(t, "ghe-host-token", harness.ResolveGitHubToken("ghe.example.com:8443", lookup))
	assert.Equal(t, "github-specific-token", harness.ResolveGitHubToken("github.com", lookup))
	assert.Equal(t, "fallback-token", harness.ResolveGitHubToken("empty.example.com", lookup))
	assert.Equal(t, "fallback-token", harness.ResolveGitHubToken("other.example.org", lookup))

	stateDir := t.TempDir()
	writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "copilot" {
  source    = "ghe.example.com:8443/org/sofa-harness-copilot"
  version   = "0.1.0"
  signature = "none"
}
`)
	fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("copilot")}}
	_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		ReleaseFetcher: fetcher,
		LookupEnv:      lookup,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"ghe-host-token"}, fetcher.tokens)

	rawLock, err := os.ReadFile(filepath.Join(stateDir, harness.LockFileName))
	require.NoError(t, err)
	assert.NotContains(t, string(rawLock), "ghe-host-token")
	assert.NotContains(t, string(rawLock), "fallback-token")
}

func TestLockPluginsChecksumsValidationAndSorting(t *testing.T) {
	t.Parallel()

	t.Run("supports underscore and hyphen harness names and sorts deterministically", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "z_custom-one" {
  source    = "github.com/org/sofa-harness-z"
  version   = "1.0.0"
  signature = "none"
}

harness "a_first" {
  source    = "github.com/org/sofa-harness-a"
  version   = "1.0.0"
  signature = "none"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt": []byte(
				strings.Repeat("a", 64) + "  sofa-harness-z_custom-one_linux_arm64.zip\n" +
					strings.Repeat("b", 64) + " *sofa-harness-z_custom-one_windows_amd64.zip\n" +
					strings.Repeat("c", 64) + "  sofa-harness-a_first_linux_amd64.zip\n",
			),
		}}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 2)
		assert.Equal(t, "a_first", lockFile.Plugins[0].Name)
		assert.Equal(t, "z_custom-one", lockFile.Plugins[1].Name)
		assert.Contains(t, lockFile.Plugins[1].Assets, "linux_arm64")
		assert.Contains(t, lockFile.Plugins[1].Assets, "windows_amd64")
	})

	t.Run("ignores sibling archives and non-platform suffixes in shared checksums.txt", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "foo" {
  source    = "github.com/org/sofa-harnesses"
  version   = "1.0.0"
  signature = "none"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt": []byte(
				strings.Repeat("a", 64) + "  sofa-harness-foo_linux_amd64.zip\n" +
					strings.Repeat("b", 64) + "  sofa-harness-foo_bar_linux_amd64.zip\n" +
					strings.Repeat("c", 64) + "  sofa-harness-foo_linux_amd64_sbom.zip\n",
			),
		}}

		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.NoError(t, err)
		require.Len(t, lockFile.Plugins, 1)
		assert.Len(t, lockFile.Plugins[0].Assets, 1)
		assert.Contains(t, lockFile.Plugins[0].Assets, "linux_amd64")
	})

	t.Run("rejects missing linux_amd64 and linux_arm64", func(t *testing.T) {
		t.Parallel()

		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "copilot" {
  source    = "github.com/org/sofa-harness-copilot"
  version   = "1.0.0"
  signature = "none"
}
`)
		fetcher := &fakeFetcher{assets: map[string][]byte{
			"checksums.txt": []byte(strings.Repeat("a", 64) + "  sofa-harness-copilot_darwin_arm64.zip\n"),
		}}

		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			ReleaseFetcher: fetcher,
		})

		require.Error(t, err)
		assert.ErrorContains(t, err, "linux_amd64")
	})

	t.Run("rejects malformed lines and duplicate filenames", func(t *testing.T) {
		t.Parallel()

		for _, content := range []string{
			"not-a-sha  sofa-harness-copilot_linux_amd64.zip\n",
			strings.Repeat("a", 64) + "  sofa-harness-copilot_linux_amd64.zip\n" + strings.Repeat("b", 64) + "  sofa-harness-copilot_linux_amd64.zip\n",
		} {
			stateDir := t.TempDir()
			writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "copilot" {
  source    = "github.com/org/sofa-harness-copilot"
  version   = "1.0.0"
  signature = "none"
}
`)
			fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": []byte(content)}}
			_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{ReleaseFetcher: fetcher})
			require.Error(t, err)
		}
	})
}

func TestLockPluginsReusesCacheWhenUpgradeFalse(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`)
	fetcher := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}}
	verifier := &fakeAttestationVerifier{err: nil}

	_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		ReleaseFetcher:      fetcher,
		AttestationVerifier: verifier,
	})
	require.NoError(t, err)
	assert.Len(t, fetcher.calls, 1)
	assert.Equal(t, 1, verifier.calls)

	// Upgrade=false reuses matching lockfile entry with zero network calls.
	_, err = harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		Upgrade:             false,
		ReleaseFetcher:      &fakeFetcher{err: errors.New("should not fetch")},
		AttestationVerifier: &fakeAttestationVerifier{err: errors.New("should not verify")},
	})
	require.NoError(t, err)

	// Upgrade=true re-fetches and re-verifies.
	fetcher2 := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}}
	verifier2 := &fakeAttestationVerifier{err: nil}
	_, err = harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		Upgrade:             true,
		ReleaseFetcher:      fetcher2,
		AttestationVerifier: verifier2,
	})
	require.NoError(t, err)
	assert.Len(t, fetcher2.calls, 1)
	assert.Equal(t, 1, verifier2.calls)

	// Downgraded lock ("none") is NOT reused when policy becomes stricter ("attestation").
	writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "none"
}
`)
	_, err = harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		Upgrade:        true,
		ReleaseFetcher: &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}},
	})
	require.NoError(t, err)

	writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", `
harness "antigravity" {
  source    = "github.com/lonegunmanb/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`)
	fetcher3 := &fakeFetcher{assets: map[string][]byte{"checksums.txt": sampleChecksums("antigravity")}}
	verifier3 := &fakeAttestationVerifier{err: nil}
	lockFile3, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
		Upgrade:             false,
		ReleaseFetcher:      fetcher3,
		AttestationVerifier: verifier3,
	})
	require.NoError(t, err)
	assert.Len(t, fetcher3.calls, 1)
	assert.Equal(t, 1, verifier3.calls)
	assert.Equal(t, "attestation", lockFile3.Plugins[0].VerifiedSignature)
}

func TestLockPluginsDefaultHTTPClientsAgainstHTTPTestServer(t *testing.T) {
	t.Parallel()

	checksums := sampleChecksums("antigravity")
	sum := sha256.Sum256(checksums)
	digestHex := hex.EncodeToString(sum[:])

	var serverURL string
	var authHeader string
	var mode string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/org/sofa-harness-antigravity/releases/download/v0.1.0/checksums.txt":
			_, _ = w.Write(checksums)
		case "/repos/org/sofa-harness-antigravity/attestations/sha256:" + digestHex:
			switch mode {
			case "ok":
				payload, _ := json.Marshal(map[string]any{
					"subject": []map[string]any{
						{"name": "checksums.txt", "digest": map[string]string{"sha256": digestHex}},
					},
				})
				_ = json.NewEncoder(w).Encode(map[string]any{
					"attestations": []map[string]any{
						{"bundle": map[string]any{"dsseEnvelope": map[string]any{
							"payloadType": "application/vnd.in-toto+json",
							"payload":     base64.StdEncoding.EncodeToString(payload),
							"signatures":  []map[string]string{{"sig": "dGVzdC1zaWc="}},
						}}},
					},
				})
			case "404":
				w.WriteHeader(http.StatusNotFound)
			case "bad-digest":
				payload, _ := json.Marshal(map[string]any{
					"subject": []map[string]any{
						{"name": "checksums.txt", "digest": map[string]string{"sha256": strings.Repeat("0", 64)}},
					},
				})
				_ = json.NewEncoder(w).Encode(map[string]any{
					"attestations": []map[string]any{
						{"bundle": map[string]any{"dsseEnvelope": map[string]any{
							"payloadType": "application/vnd.in-toto+json",
							"payload":     base64.StdEncoding.EncodeToString(payload),
							"signatures":  []map[string]string{{"sig": "dGVzdC1zaWc="}},
						}}},
					},
				})
			case "bad-payload-type":
				payload, _ := json.Marshal(map[string]any{
					"subject": []map[string]any{
						{"name": "checksums.txt", "digest": map[string]string{"sha256": digestHex}},
					},
				})
				_ = json.NewEncoder(w).Encode(map[string]any{
					"attestations": []map[string]any{
						{"bundle": map[string]any{"dsseEnvelope": map[string]any{
							"payloadType": "application/octet-stream",
							"payload":     base64.StdEncoding.EncodeToString(payload),
							"signatures":  []map[string]string{{"sig": "dGVzdC1zaWc="}},
						}}},
					},
				})
			case "no-signatures":
				payload, _ := json.Marshal(map[string]any{
					"subject": []map[string]any{
						{"name": "checksums.txt", "digest": map[string]string{"sha256": digestHex}},
					},
				})
				_ = json.NewEncoder(w).Encode(map[string]any{
					"attestations": []map[string]any{
						{"bundle": map[string]any{"dsseEnvelope": map[string]any{
							"payloadType": "application/vnd.in-toto+json",
							"payload":     base64.StdEncoding.EncodeToString(payload),
							"signatures":  []map[string]string{},
						}}},
					},
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	serverURL = strings.TrimPrefix(server.URL, "http://")

	t.Run("default HTTP attestation succeeds with Bearer token", func(t *testing.T) {
		mode = "ok"
		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source    = "%s/org/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`, serverURL))
		lockFile, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{
			LookupEnv: func(k string) (string, bool) {
				if k == "GITHUB_TOKEN" {
					return "secret-http-token", true
				}
				return "", false
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "Bearer secret-http-token", authHeader)
		assert.Equal(t, "attestation", lockFile.Plugins[0].VerifiedSignature)
	})

	t.Run("default HTTP attestation 404 returns ErrAttestationNotFound", func(t *testing.T) {
		mode = "404"
		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source    = "%s/org/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`, serverURL))
		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})
		require.Error(t, err)
		assert.ErrorIs(t, err, harness.ErrAttestationNotFound)
	})

	t.Run("default HTTP attestation digest mismatch fails without ErrAttestationNotFound", func(t *testing.T) {
		mode = "bad-digest"
		stateDir := t.TempDir()
		writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source    = "%s/org/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`, serverURL))
		_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})
		require.Error(t, err)
		assert.NotErrorIs(t, err, harness.ErrAttestationNotFound)
	})

	for _, invalidMode := range []string{"bad-payload-type", "no-signatures"} {
		t.Run("default HTTP attestation rejects "+invalidMode, func(t *testing.T) {
			mode = invalidMode
			stateDir := t.TempDir()
			writeHarnessFile(t, filepath.Join(stateDir, "config"), "main.sofa.hcl", fmt.Sprintf(`
harness "antigravity" {
  source    = "%s/org/sofa-harness-antigravity"
  version   = "0.1.0"
  signature = "attestation"
}
`, serverURL))
			_, err := harness.LockPlugins(t.Context(), stateDir, harness.LockOptions{})
			require.Error(t, err)
			assert.NotErrorIs(t, err, harness.ErrAttestationNotFound)
		})
	}
}

func TestReadLockFileRejectsCorruptAndInvalidEntries(t *testing.T) {
	t.Parallel()

	validAsset := map[string]harness.PluginAsset{
		"linux_amd64": {
			Filename: "sofa-harness-antigravity_linux_amd64.zip",
			SHA256:   strings.Repeat("a", 64),
		},
	}

	tests := []struct {
		name string
		lock harness.PluginLockFile
	}{
		{
			name: "duplicate plugin names",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "none", Assets: validAsset},
					{Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "none", Assets: validAsset},
				},
			},
		},
		{
			name: "empty source",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{Name: "antigravity", Source: "", Version: "0.1.0", VerifiedSignature: "none", Assets: validAsset},
				},
			},
		},
		{
			name: "invalid verified_signature",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "auto", Assets: validAsset},
				},
			},
		},
		{
			name: "pgp without fingerprint",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "pgp", Assets: validAsset},
				},
			},
		},
		{
			name: "mismatched asset filename",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{
						Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "none",
						Assets: map[string]harness.PluginAsset{
							"linux_amd64": {Filename: "wrong.zip", SHA256: strings.Repeat("a", 64)},
						},
					},
				},
			},
		},
		{
			name: "uppercase sha256",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{
						Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "none",
						Assets: map[string]harness.PluginAsset{
							"linux_amd64": {Filename: "sofa-harness-antigravity_linux_amd64.zip", SHA256: strings.Repeat("A", 64)},
						},
					},
				},
			},
		},
		{
			name: "missing linux_amd64 and linux_arm64",
			lock: harness.PluginLockFile{
				FormatVersion: 1,
				Plugins: []harness.PluginLockEntry{
					{
						Name: "antigravity", Source: "github.com/org/a", Version: "0.1.0", VerifiedSignature: "none",
						Assets: map[string]harness.PluginAsset{
							"darwin_arm64": {Filename: "sofa-harness-antigravity_darwin_arm64.zip", SHA256: strings.Repeat("a", 64)},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stateDir := t.TempDir()
			data, err := json.Marshal(tt.lock)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(stateDir, harness.LockFileName), data, 0o600))

			_, err = harness.ReadLockFile(stateDir)
			require.Error(t, err)
		})
	}
}

func writeHarnessFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimSpace(content)+"\n"), 0o600))
}

func sampleChecksums(name string) []byte {
	return []byte(
		strings.Repeat("a", 64) + fmt.Sprintf("  sofa-harness-%s_linux_amd64.zip\n", name) +
			strings.Repeat("b", 64) + fmt.Sprintf("  sofa-harness-%s_linux_arm64.zip\n", name),
	)
}

func generateSignedChecksums(t *testing.T, name string) (pubArmor string, armoredSig []byte, binarySig []byte, fingerprint string, checksums []byte) {
	t.Helper()

	entity, err := openpgp.NewEntity("Sofa Test", "release", "test@example.invalid", nil)
	require.NoError(t, err)

	var pubBuf bytes.Buffer
	w, err := armor.Encode(&pubBuf, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(w))
	require.NoError(t, w.Close())
	pubArmor = pubBuf.String()
	fingerprint = strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))

	checksums = sampleChecksums(name)

	var armSigBuf bytes.Buffer
	require.NoError(t, openpgp.ArmoredDetachSign(&armSigBuf, entity, bytes.NewReader(checksums), nil))
	armoredSig = armSigBuf.Bytes()

	var binSigBuf bytes.Buffer
	require.NoError(t, openpgp.DetachSign(&binSigBuf, entity, bytes.NewReader(checksums), nil))
	binarySig = binSigBuf.Bytes()
	return pubArmor, armoredSig, binarySig, fingerprint, checksums
}
