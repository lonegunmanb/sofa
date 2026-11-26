package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/config"
	harnessspec "github.com/lonegunmanb/sofa/internal/harness/spec"
	"github.com/lonegunmanb/sofa/internal/module"
	"github.com/zclconf/go-cty/cty"
)

const (
	// LockFileName is the deterministic harness plugin lockfile written inside the state directory.
	LockFileName = "plugins.lock.json"
	// LockFormatVersion is the schema version of plugins.lock.json.
	LockFormatVersion = 1
)

// ErrAttestationNotFound indicates a release has no GitHub artifact attestation
// (distinct from an invalid/forged attestation or network failure).
var ErrAttestationNotFound = errors.New("release attestation not found")

var (
	sha256HexPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexUpperOrLower   = regexp.MustCompile(`^[0-9A-Fa-f]+$`)
	assetTokenPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

// PluginAsset describes a single platform release archive entry locked by SHA-256.
type PluginAsset struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
}

// PluginLockEntry records the verified release metadata and per-platform digests for a remote harness.
type PluginLockEntry struct {
	Name                  string                 `json:"name"`
	Source                string                 `json:"source"`
	Version               string                 `json:"version"`
	VerifiedSignature     string                 `json:"verified_signature"`
	SigningKeyFingerprint string                 `json:"signing_key_fingerprint,omitempty"`
	Assets                map[string]PluginAsset `json:"assets"`
}

// PluginLockFile is the persisted JSON structure of plugins.lock.json.
type PluginLockFile struct {
	FormatVersion int               `json:"format_version"`
	Plugins       []PluginLockEntry `json:"plugins"`
}

// ReleaseFetcher downloads named assets from a harness GitHub Release tag.
type ReleaseFetcher interface {
	FetchReleaseAsset(ctx context.Context, host, owner, repo, tag, filename, token string) ([]byte, error)
}

// AttestationVerifier verifies GitHub OIDC artifact attestations for checksums.txt.
type AttestationVerifier interface {
	VerifyAttestation(ctx context.Context, host, owner, repo, tag string, checksums []byte, token string) error
}

// LockOptions configures harness plugin scanning, verification, and lockfile caching.
type LockOptions struct {
	Upgrade             bool
	ReleaseFetcher      ReleaseFetcher
	AttestationVerifier AttestationVerifier
	LookupEnv           func(string) (string, bool)
}

// LockPlugins scans <stateDir>/config and <stateDir>/modules/** for harness declarations,
// verifies remote releases according to their signature policy, and atomically writes plugins.lock.json.
func LockPlugins(ctx context.Context, stateDir string, opts LockOptions) (PluginLockFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return PluginLockFile{}, fmt.Errorf("resolve state directory: %w", err)
	}
	absState = filepath.Clean(absState)

	configs, err := collectHarnessConfigs(ctx, absState)
	if err != nil {
		return PluginLockFile{}, err
	}

	var existingCache map[string]PluginLockEntry
	if !opts.Upgrade {
		cached, cacheErr := ReadLockFile(absState)
		if cacheErr == nil {
			existingCache = make(map[string]PluginLockEntry, len(cached.Plugins))
			for _, entry := range cached.Plugins {
				existingCache[entry.Name] = entry
			}
		} else if !errors.Is(cacheErr, os.ErrNotExist) {
			return PluginLockFile{}, cacheErr
		}
	}

	fetcher := opts.ReleaseFetcher
	if fetcher == nil {
		fetcher = &httpReleaseFetcher{client: &http.Client{Timeout: 30 * time.Second}}
	}
	verifier := opts.AttestationVerifier
	if verifier == nil {
		verifier = &httpAttestationVerifier{client: &http.Client{Timeout: 30 * time.Second}}
	}

	lockFile := PluginLockFile{
		FormatVersion: LockFormatVersion,
		Plugins:       []PluginLockEntry{},
	}

	names := make([]string, 0, len(configs))
	for name, cfg := range configs {
		if cfg.Source == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cfg := configs[name]
		if !opts.Upgrade {
			if cached, ok := existingCache[name]; ok && canReuseCachedEntry(cfg, cached) {
				lockFile.Plugins = append(lockFile.Plugins, cloneLockEntry(cached))
				continue
			}
		}
		entry, err := resolveAndVerifyPlugin(ctx, cfg, fetcher, verifier, opts.LookupEnv)
		if err != nil {
			return PluginLockFile{}, err
		}
		lockFile.Plugins = append(lockFile.Plugins, entry)
	}

	if err := writeLockFile(filepath.Join(absState, LockFileName), lockFile); err != nil {
		return PluginLockFile{}, err
	}
	return lockFile, nil
}

// ReadLockFile reads and validates <stateDir>/plugins.lock.json.
func ReadLockFile(stateDir string) (PluginLockFile, error) {
	lockPath := filepath.Join(stateDir, LockFileName)
	encoded, err := os.ReadFile(lockPath)
	if err != nil {
		return PluginLockFile{}, fmt.Errorf("read plugin lockfile %q: %w", lockPath, err)
	}
	var lockFile PluginLockFile
	if err := json.Unmarshal(encoded, &lockFile); err != nil {
		return PluginLockFile{}, fmt.Errorf("decode plugin lockfile %q: %w", lockPath, err)
	}
	if err := ValidateLockFile(lockFile); err != nil {
		return PluginLockFile{}, fmt.Errorf("invalid plugin lockfile %q: %w", lockPath, err)
	}
	if lockFile.Plugins == nil {
		lockFile.Plugins = []PluginLockEntry{}
	}
	return lockFile, nil
}

// ValidateLockFile verifies the schema version, entry invariants, and uniqueness of lockFile.
func ValidateLockFile(lockFile PluginLockFile) error {
	return validateLockFile(lockFile)
}

func validateLockFile(lockFile PluginLockFile) error {
	if lockFile.FormatVersion != LockFormatVersion {
		return fmt.Errorf("unsupported format_version %d", lockFile.FormatVersion)
	}
	seen := make(map[string]struct{}, len(lockFile.Plugins))
	for _, entry := range lockFile.Plugins {
		if err := validateLockEntry(entry); err != nil {
			return err
		}
		if _, dup := seen[entry.Name]; dup {
			return fmt.Errorf("duplicate plugin entry %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}
	}
	return nil
}

func validateLockEntry(entry PluginLockEntry) error {
	cfg := harnessspec.HarnessConfig{
		Name:    entry.Name,
		Source:  entry.Source,
		Version: entry.Version,
	}
	if strings.TrimSpace(entry.Source) == "" || strings.TrimSpace(entry.Version) == "" {
		return fmt.Errorf("plugin %q must have non-empty source and version", entry.Name)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	switch entry.VerifiedSignature {
	case string(harnessspec.SignatureModeNone), string(harnessspec.SignatureModeAttestation):
	case string(harnessspec.SignatureModePGP):
		fp := strings.TrimSpace(entry.SigningKeyFingerprint)
		if fp == "" || !hexUpperOrLower.MatchString(fp) {
			return fmt.Errorf("plugin %q with pgp signature must have a valid signing_key_fingerprint", entry.Name)
		}
	default:
		return fmt.Errorf("plugin %q has invalid verified_signature %q", entry.Name, entry.VerifiedSignature)
	}
	if err := validateLockedAssets(entry.Name, entry.Assets); err != nil {
		return fmt.Errorf("plugin %q: %w", entry.Name, err)
	}
	return nil
}

func validateLockedAssets(pluginName string, assets map[string]PluginAsset) error {
	if len(assets) == 0 {
		return fmt.Errorf("assets must not be empty")
	}
	for key, asset := range assets {
		parts := strings.Split(key, "_")
		if len(parts) != 2 || !assetTokenPattern.MatchString(parts[0]) || !assetTokenPattern.MatchString(parts[1]) {
			return fmt.Errorf("invalid asset platform key %q", key)
		}
		expectedFilename := fmt.Sprintf("sofa-harness-%s_%s.zip", pluginName, key)
		if asset.Filename != expectedFilename {
			return fmt.Errorf("asset %q filename %q does not match expected %q", key, asset.Filename, expectedFilename)
		}
		if !sha256HexPattern.MatchString(asset.SHA256) {
			return fmt.Errorf("asset %q has invalid lowercase SHA-256 digest %q", key, asset.SHA256)
		}
	}
	_, hasAMD64 := assets["linux_amd64"]
	_, hasARM64 := assets["linux_arm64"]
	if !hasAMD64 && !hasARM64 {
		return fmt.Errorf("assets must include at least linux_amd64 or linux_arm64")
	}
	return nil
}

// ResolveGitHubToken resolves a GitHub token from GITHUB_TOKEN_<NORMALIZED_HOST> then GITHUB_TOKEN.
func ResolveGitHubToken(host string, lookupEnv func(string) (string, bool)) string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if normalized := normalizeHostEnvSuffix(host); normalized != "" {
		if token, ok := lookupEnv("GITHUB_TOKEN_" + normalized); ok && token != "" {
			return token
		}
	}
	if token, ok := lookupEnv("GITHUB_TOKEN"); ok && token != "" {
		return token
	}
	return ""
}

func normalizeHostEnvSuffix(host string) string {
	var b strings.Builder
	b.Grow(len(host))
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - ('a' - 'A'))
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// CanSatisfyHarnessLock verifies whether cfg (when referencing a remote release) is satisfied
// by an entry in lockFile under the exact same signature rules as LockPlugins.
func CanSatisfyHarnessLock(cfg harnessspec.HarnessConfig, lockFile PluginLockFile) error {
	if cfg.Source == "" {
		return nil
	}
	for _, entry := range lockFile.Plugins {
		if entry.Name != cfg.Name {
			continue
		}
		if entry.Source != cfg.Source || entry.Version != cfg.Version {
			return fmt.Errorf(
				"harness %q lockfile mismatch (locked %s@%s, configured %s@%s); run sofa init --upgrade",
				cfg.Name, entry.Source, entry.Version, cfg.Source, cfg.Version,
			)
		}
		if !canReuseCachedEntry(cfg, entry) {
			return fmt.Errorf(
				"harness %q lockfile entry does not satisfy signature policy %q; run sofa init --upgrade",
				cfg.Name, cfg.Signature,
			)
		}
		return nil
	}
	return fmt.Errorf("harness %q (%s@%s) is not locked in plugins.lock.json; run sofa init --upgrade", cfg.Name, cfg.Source, cfg.Version)
}

func collectHarnessConfigs(ctx context.Context, stateDir string) (map[string]harnessspec.HarnessConfig, error) {
	configDir := filepath.Join(stateDir, "config")
	modulesDir := filepath.Join(stateDir, "modules")
	merged := make(map[string]harnessspec.HarnessConfig)
	visited := make(map[string]struct{})
	if err := walkActiveModuleConfigs(ctx, configDir, modulesDir, "", visited, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func walkActiveModuleConfigs(
	ctx context.Context,
	scanDir string,
	modulesDir string,
	addressPrefix string,
	visited map[string]struct{},
	merged map[string]harnessspec.HarnessConfig,
) error {
	cleanDir := filepath.Clean(scanDir)
	if _, seen := visited[cleanDir]; seen {
		return nil
	}
	visited[cleanDir] = struct{}{}

	loaded, diags, err := config.LoadDirectoryContext(ctx, scanDir)
	if err != nil {
		return fmt.Errorf("load configuration %q for harness locking: %w", scanDir, err)
	}
	if diags.HasErrors() {
		return diags
	}

	childLabels := make([]string, 0)
	for _, block := range loaded.Blocks {
		switch block.Type {
		case "harness":
			cfg, err := parseStaticHarnessConfig(block)
			if err != nil {
				return err
			}
			if existing, exists := merged[cfg.Name]; exists {
				combined, err := mergeHarnessConfigs(existing, cfg)
				if err != nil {
					return err
				}
				merged[cfg.Name] = combined
			} else {
				merged[cfg.Name] = cfg
			}
		case "module":
			label, ok := module.SingleLabel(block.Labels)
			if !ok {
				return fmt.Errorf("module block in %q must have exactly 1 label", scanDir)
			}
			childLabels = append(childLabels, label)
		}
	}

	sort.Strings(childLabels)
	for _, label := range childLabels {
		address := "module." + label
		if addressPrefix != "" {
			address = addressPrefix + "." + address
		}
		childDir, err := module.Directory(modulesDir, address)
		if err != nil {
			return err
		}
		if err := walkActiveModuleConfigs(ctx, childDir, modulesDir, address, visited, merged); err != nil {
			return err
		}
	}
	return nil
}

func parseStaticHarnessConfig(block *golden.HclBlock) (harnessspec.HarnessConfig, error) {
	name, ok := module.SingleLabel(block.Labels)
	if !ok {
		return harnessspec.HarnessConfig{}, fmt.Errorf("harness block must have exactly 1 label")
	}

	attrs := block.Attributes()
	values := make(map[string]string, 4)
	for _, attrName := range []string{"source", "version", "signature", "signing_key"} {
		attr, exists := attrs[attrName]
		if !exists {
			continue
		}
		if len(attr.Expr.Variables()) != 0 {
			return harnessspec.HarnessConfig{}, fmt.Errorf(
				"harness %q %s must be a static literal without variable references during init",
				name, attrName,
			)
		}
		val, diags := attr.Expr.Value(&hcl.EvalContext{})
		if diags.HasErrors() {
			return harnessspec.HarnessConfig{}, diags
		}
		unmarked, _ := val.UnmarkDeep()
		if unmarked.IsNull() {
			continue
		}
		if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
			return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q %s must be a known string or null", name, attrName)
		}
		s := unmarked.AsString()
		if s == "" || (attrName == "signing_key" && strings.TrimSpace(s) == "") {
			return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q %s must not be empty", name, attrName)
		}
		values[attrName] = s
	}

	cfg := harnessspec.HarnessConfig{
		Name:       name,
		Source:     values["source"],
		Version:    values["version"],
		Signature:  harnessspec.SignatureMode(values["signature"]),
		SigningKey: values["signing_key"],
	}
	if err := cfg.Validate(); err != nil {
		return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q: %w", name, err)
	}
	if strings.TrimSpace(cfg.SigningKey) != "" {
		if _, err := primaryKeyFingerprint(cfg.SigningKey); err != nil {
			return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q signing_key: %w", name, err)
		}
	}
	return cfg, nil
}

func mergeHarnessConfigs(existing, candidate harnessspec.HarnessConfig) (harnessspec.HarnessConfig, error) {
	if (existing.Source == "") != (candidate.Source == "") {
		return harnessspec.HarnessConfig{}, fmt.Errorf(
			"harness %q has conflicting local and remote declarations",
			existing.Name,
		)
	}
	if existing.Source == "" {
		return existing, nil
	}
	if existing.Source != candidate.Source || existing.Version != candidate.Version {
		return harnessspec.HarnessConfig{}, fmt.Errorf(
			"harness %q has conflicting source/version declarations (%s@%s vs %s@%s)",
			existing.Name, existing.Source, existing.Version, candidate.Source, candidate.Version,
		)
	}

	keyA := strings.TrimSpace(existing.SigningKey)
	keyB := strings.TrimSpace(candidate.SigningKey)
	mergedKey := keyA
	if keyA != "" && keyB != "" && !signingKeysMatch(keyA, keyB) {
		return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q has conflicting signing_key declarations", existing.Name)
	}
	if mergedKey == "" {
		mergedKey = keyB
	}

	sig, err := mergeSignatureModes(existing.Name, existing.Signature, candidate.Signature)
	if err != nil {
		return harnessspec.HarnessConfig{}, err
	}
	result := existing.Clone()
	result.Signature = sig
	if sig == harnessspec.SignatureModeAttestation || sig == harnessspec.SignatureModeNone {
		result.SigningKey = ""
	} else {
		result.SigningKey = mergedKey
	}
	if err := result.Validate(); err != nil {
		return harnessspec.HarnessConfig{}, fmt.Errorf("harness %q: %w", existing.Name, err)
	}
	return result, nil
}

func signingKeysMatch(a, b string) bool {
	if a == b {
		return true
	}
	fpA, errA := primaryKeyFingerprint(a)
	fpB, errB := primaryKeyFingerprint(b)
	return errA == nil && errB == nil && fpA == fpB
}

func mergeSignatureModes(name string, a, b harnessspec.SignatureMode) (harnessspec.SignatureMode, error) {
	if a == b {
		return a, nil
	}
	if (a == harnessspec.SignatureModeAttestation && b == harnessspec.SignatureModePGP) ||
		(a == harnessspec.SignatureModePGP && b == harnessspec.SignatureModeAttestation) {
		return "", fmt.Errorf("harness %q has incompatible signature policies %q and %q", name, a, b)
	}
	rank := func(m harnessspec.SignatureMode) int {
		switch m {
		case harnessspec.SignatureModeAttestation, harnessspec.SignatureModePGP:
			return 3
		case harnessspec.SignatureModeAuto:
			return 2
		case harnessspec.SignatureModeNone:
			return 1
		default:
			return 0
		}
	}
	if rank(a) >= rank(b) {
		return a, nil
	}
	return b, nil
}

func canReuseCachedEntry(cfg harnessspec.HarnessConfig, entry PluginLockEntry) bool {
	if entry.Name != cfg.Name || entry.Source != cfg.Source || entry.Version != cfg.Version {
		return false
	}
	if validateLockEntry(entry) != nil {
		return false
	}
	switch cfg.Signature {
	case harnessspec.SignatureModeNone:
		return entry.VerifiedSignature == string(harnessspec.SignatureModeNone) ||
			entry.VerifiedSignature == string(harnessspec.SignatureModePGP) ||
			entry.VerifiedSignature == string(harnessspec.SignatureModeAttestation)
	case harnessspec.SignatureModeAttestation:
		return entry.VerifiedSignature == string(harnessspec.SignatureModeAttestation)
	case harnessspec.SignatureModePGP:
		if entry.VerifiedSignature != string(harnessspec.SignatureModePGP) {
			return false
		}
		fp, err := primaryKeyFingerprint(cfg.SigningKey)
		return err == nil && strings.EqualFold(entry.SigningKeyFingerprint, fp)
	case harnessspec.SignatureModeAuto:
		if entry.VerifiedSignature == string(harnessspec.SignatureModeAttestation) {
			return true
		}
		if entry.VerifiedSignature == string(harnessspec.SignatureModePGP) && strings.TrimSpace(cfg.SigningKey) != "" {
			fp, err := primaryKeyFingerprint(cfg.SigningKey)
			return err == nil && strings.EqualFold(entry.SigningKeyFingerprint, fp)
		}
		return false
	default:
		return false
	}
}

func cloneLockEntry(entry PluginLockEntry) PluginLockEntry {
	cloned := entry
	cloned.Assets = maps.Clone(entry.Assets)
	return cloned
}

func resolveAndVerifyPlugin(
	ctx context.Context,
	cfg harnessspec.HarnessConfig,
	fetcher ReleaseFetcher,
	verifier AttestationVerifier,
	lookupEnv func(string) (string, bool),
) (PluginLockEntry, error) {
	tag := "v" + cfg.Version
	token := ResolveGitHubToken(cfg.SourceHost, lookupEnv)

	checksumsBytes, err := fetcher.FetchReleaseAsset(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, "checksums.txt", token)
	if err != nil {
		return PluginLockEntry{}, fmt.Errorf("harness %q fetch checksums.txt: %w", cfg.Name, redactTokenErr(err, token))
	}

	assets, err := parseChecksums(cfg.Name, checksumsBytes)
	if err != nil {
		return PluginLockEntry{}, fmt.Errorf("harness %q: %w", cfg.Name, err)
	}

	verifiedSig := ""
	fingerprint := ""

	switch cfg.Signature {
	case harnessspec.SignatureModeNone:
		verifiedSig = string(harnessspec.SignatureModeNone)
	case harnessspec.SignatureModeAttestation:
		if err := verifier.VerifyAttestation(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, checksumsBytes, token); err != nil {
			return PluginLockEntry{}, fmt.Errorf("harness %q verify attestation: %w", cfg.Name, redactTokenErr(err, token))
		}
		verifiedSig = string(harnessspec.SignatureModeAttestation)
	case harnessspec.SignatureModePGP:
		sigBytes, err := fetcher.FetchReleaseAsset(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, "checksums.txt.sig", token)
		if err != nil {
			return PluginLockEntry{}, fmt.Errorf("harness %q fetch checksums.txt.sig: %w", cfg.Name, redactTokenErr(err, token))
		}
		fp, err := verifyPGPSignature(cfg.SigningKey, checksumsBytes, sigBytes)
		if err != nil {
			return PluginLockEntry{}, fmt.Errorf("harness %q verify PGP signature: %w", cfg.Name, err)
		}
		verifiedSig = string(harnessspec.SignatureModePGP)
		fingerprint = fp
	case harnessspec.SignatureModeAuto:
		err := verifier.VerifyAttestation(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, checksumsBytes, token)
		if err == nil {
			verifiedSig = string(harnessspec.SignatureModeAttestation)
			break
		}
		if errors.Is(err, ErrAttestationNotFound) && strings.TrimSpace(cfg.SigningKey) != "" {
			sigBytes, fetchErr := fetcher.FetchReleaseAsset(ctx, cfg.SourceHost, cfg.SourceOwner, cfg.SourceRepo, tag, "checksums.txt.sig", token)
			if fetchErr != nil {
				return PluginLockEntry{}, fmt.Errorf("harness %q fetch checksums.txt.sig fallback: %w", cfg.Name, redactTokenErr(fetchErr, token))
			}
			fp, pgpErr := verifyPGPSignature(cfg.SigningKey, checksumsBytes, sigBytes)
			if pgpErr != nil {
				return PluginLockEntry{}, fmt.Errorf("harness %q verify PGP fallback signature: %w", cfg.Name, pgpErr)
			}
			verifiedSig = string(harnessspec.SignatureModePGP)
			fingerprint = fp
			break
		}
		return PluginLockEntry{}, fmt.Errorf("harness %q verify attestation: %w", cfg.Name, redactTokenErr(err, token))
	default:
		return PluginLockEntry{}, fmt.Errorf("harness %q has unsupported signature mode %q", cfg.Name, cfg.Signature)
	}

	return PluginLockEntry{
		Name:                  cfg.Name,
		Source:                cfg.Source,
		Version:               cfg.Version,
		VerifiedSignature:     verifiedSig,
		SigningKeyFingerprint: fingerprint,
		Assets:                assets,
	}, nil
}

func parseChecksums(harnessName string, data []byte) (map[string]PluginAsset, error) {
	prefix := "sofa-harness-" + harnessName + "_"
	const suffix = ".zip"

	seenFilenames := make(map[string]struct{})
	assets := make(map[string]PluginAsset)

	lines := strings.Split(string(data), "\n")
	for lineIdx, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums.txt line %d is malformed", lineIdx+1)
		}
		digest := strings.ToLower(fields[0])
		if !sha256HexPattern.MatchString(digest) {
			return nil, fmt.Errorf("checksums.txt line %d has invalid SHA-256 digest %q", lineIdx+1, fields[0])
		}
		filename := strings.TrimPrefix(fields[1], "*")
		if filename == "" {
			return nil, fmt.Errorf("checksums.txt line %d has empty filename", lineIdx+1)
		}
		if _, dup := seenFilenames[filename]; dup {
			return nil, fmt.Errorf("checksums.txt contains duplicate filename %q", filename)
		}
		seenFilenames[filename] = struct{}{}

		if !strings.HasPrefix(filename, prefix) || !strings.HasSuffix(filename, suffix) {
			continue
		}
		platform := strings.TrimSuffix(strings.TrimPrefix(filename, prefix), suffix)
		parts := strings.Split(platform, "_")
		if len(parts) != 2 || !assetTokenPattern.MatchString(parts[0]) || !assetTokenPattern.MatchString(parts[1]) {
			continue
		}
		key := parts[0] + "_" + parts[1]
		assets[key] = PluginAsset{
			Filename: filename,
			SHA256:   digest,
		}
	}

	if len(assets) == 0 {
		return nil, fmt.Errorf("checksums.txt contains no release archives matching %s<os>_<arch>%s", prefix, suffix)
	}
	_, hasAMD64 := assets["linux_amd64"]
	_, hasARM64 := assets["linux_arm64"]
	if !hasAMD64 && !hasARM64 {
		return nil, fmt.Errorf("checksums.txt must include at least linux_amd64 or linux_arm64 archive")
	}
	return assets, nil
}

func primaryKeyFingerprint(armoredKey string) (string, error) {
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		return "", fmt.Errorf("parse OpenPGP signing_key: %w", err)
	}
	if len(keyring) == 0 || keyring[0].PrimaryKey == nil {
		return "", fmt.Errorf("OpenPGP signing_key contains no primary key")
	}
	return strings.ToUpper(hex.EncodeToString(keyring[0].PrimaryKey.Fingerprint)), nil
}

func verifyPGPSignature(armoredKey string, checksums, signature []byte) (string, error) {
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		return "", fmt.Errorf("parse OpenPGP signing_key: %w", err)
	}
	if len(keyring) == 0 {
		return "", fmt.Errorf("OpenPGP signing_key keyring is empty")
	}

	var signer *openpgp.Entity
	if _, armorErr := armor.Decode(bytes.NewReader(signature)); armorErr == nil {
		signer, err = openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(checksums), bytes.NewReader(signature), nil)
	} else {
		signer, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(checksums), bytes.NewReader(signature), nil)
	}
	if err != nil {
		return "", fmt.Errorf("detached PGP verification failed: %w", err)
	}
	if signer == nil || signer.PrimaryKey == nil {
		return "", fmt.Errorf("detached PGP verification produced no signer key")
	}
	return strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint)), nil
}

func writeLockFile(path string, lockFile PluginLockFile) error {
	if lockFile.Plugins == nil {
		lockFile.Plugins = []PluginLockEntry{}
	}
	sort.Slice(lockFile.Plugins, func(i, j int) bool {
		return lockFile.Plugins[i].Name < lockFile.Plugins[j].Name
	})

	encoded, err := json.MarshalIndent(lockFile, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin lockfile: %w", err)
	}
	encoded = append(encoded, '\n')

	temporary, err := os.CreateTemp(filepath.Dir(path), ".sofa-lock-")
	if err != nil {
		return fmt.Errorf("create plugin lockfile temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write plugin lockfile temporary file: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("activate plugin lockfile: %w", err)
	}
	return nil
}

type httpReleaseFetcher struct {
	client *http.Client
}

func (f *httpReleaseFetcher) FetchReleaseAsset(
	ctx context.Context,
	host, owner, repo, tag, filename, token string,
) ([]byte, error) {
	urlStr := fmt.Sprintf("%s://%s/%s/%s/releases/download/%s/%s",
		schemeForHost(host), host, owner, repo, tag, filename,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, redactTokenErr(err, token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, redactTokenErr(err, token)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d downloading %s", resp.StatusCode, filename)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, redactTokenErr(err, token)
	}
	return body, nil
}

type httpAttestationVerifier struct {
	client *http.Client
}

func (v *httpAttestationVerifier) VerifyAttestation(
	ctx context.Context,
	host, owner, repo, _ string,
	checksums []byte,
	token string,
) error {
	sum := sha256.Sum256(checksums)
	digestHex := hex.EncodeToString(sum[:])

	var urlStr string
	switch {
	case host == "github.com":
		urlStr = fmt.Sprintf("https://api.github.com/repos/%s/%s/attestations/sha256:%s", owner, repo, digestHex)
	case isLoopbackHost(host):
		urlStr = fmt.Sprintf("http://%s/repos/%s/%s/attestations/sha256:%s", host, owner, repo, digestHex)
	default:
		urlStr = fmt.Sprintf("https://%s/api/v3/repos/%s/%s/attestations/sha256:%s", host, owner, repo, digestHex)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return redactTokenErr(err, token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return redactTokenErr(err, token)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrAttestationNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP %d querying attestations", resp.StatusCode)
	}

	var payload struct {
		Attestations []struct {
			Bundle struct {
				DSSEEnvelope struct {
					PayloadType string `json:"payloadType"`
					Payload     string `json:"payload"`
					Signatures  []struct {
						Sig string `json:"sig"`
					} `json:"signatures"`
				} `json:"dsseEnvelope"`
			} `json:"bundle"`
		} `json:"attestations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("decode attestation response: %w", err)
	}
	if len(payload.Attestations) == 0 {
		return ErrAttestationNotFound
	}

	for _, att := range payload.Attestations {
		env := att.Bundle.DSSEEnvelope
		if env.PayloadType != "application/vnd.in-toto+json" || env.Payload == "" {
			continue
		}
		hasSig := false
		for _, s := range env.Signatures {
			if strings.TrimSpace(s.Sig) != "" {
				hasSig = true
				break
			}
		}
		if !hasSig {
			continue
		}
		rawStatement, err := base64.StdEncoding.DecodeString(env.Payload)
		if err != nil {
			continue
		}
		var statement struct {
			Subject []struct {
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		}
		if err := json.Unmarshal(rawStatement, &statement); err != nil {
			continue
		}
		for _, subj := range statement.Subject {
			if strings.EqualFold(subj.Digest["sha256"], digestHex) {
				return nil
			}
		}
	}
	return fmt.Errorf("no attestation bundle matched checksums.txt sha256:%s", digestHex)
}

func isLoopbackHost(host string) bool {
	hostname := host
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		hostname = host[:colon]
	}
	return hostname == "localhost" || hostname == "127.0.0.1"
}

func schemeForHost(host string) string {
	if isLoopbackHost(host) {
		return "http"
	}
	return "https"
}

func redactTokenErr(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	if !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}
