# Contracts: Workspace Init, Recursive Module Installation, and Harness Plugin Locking

## `internal/module` Public API

```go
type FetchFunc func(ctx context.Context, source, destination, workingDirectory string) error

type InitOptions struct {
	ModulesDirectory string
	Upgrade          bool
	Fetch            FetchFunc
}

func Init(ctx context.Context, directory string, options InitOptions) error
func Directory(modulesDirectory, canonicalAddress string) (string, error)
func IsLocalSource(source string) bool
func Fetch(ctx context.Context, source, destination, workingDirectory string) error
func CopyCleanDirectory(ctx context.Context, source, destination string) error
```

## `internal/harness` Public API

```go
const (
	LockFileName      = "plugins.lock.json"
	LockFormatVersion = 1
)

var ErrAttestationNotFound = errors.New("release attestation not found")

type PluginAsset struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
}

type PluginLockEntry struct {
	Name                  string                 `json:"name"`
	Source                string                 `json:"source"`
	Version               string                 `json:"version"`
	VerifiedSignature     string                 `json:"verified_signature"`
	SigningKeyFingerprint string                 `json:"signing_key_fingerprint,omitempty"`
	Assets                map[string]PluginAsset `json:"assets"`
}

type PluginLockFile struct {
	FormatVersion int               `json:"format_version"`
	Plugins       []PluginLockEntry `json:"plugins"`
}

type ReleaseFetcher interface {
	FetchReleaseAsset(ctx context.Context, host, owner, repo, tag, filename, token string) ([]byte, error)
}

type AttestationVerifier interface {
	VerifyAttestation(ctx context.Context, host, owner, repo, tag string, checksums []byte, token string) error
}

type LockOptions struct {
	Upgrade             bool
	ReleaseFetcher      ReleaseFetcher
	AttestationVerifier AttestationVerifier
	LookupEnv           func(string) (string, bool)
}

func LockPlugins(ctx context.Context, stateDir string, opts LockOptions) (PluginLockFile, error)
func ReadLockFile(stateDir string) (PluginLockFile, error)
func ResolveGitHubToken(host string, lookupEnv func(string) (string, bool)) string
```

## `internal/project` Public API

```go
const (
	StateFileName      = "state.json"
	StateFormatVersion = 1
)

type InitOptions struct {
	Upgrade             bool
	Fetch               module.FetchFunc
	ReleaseFetcher      harness.ReleaseFetcher
	AttestationVerifier harness.AttestationVerifier
	LookupEnv           func(string) (string, bool)
}

type Project struct {
	StateDirectory   string
	ConfigDirectory  string
	ModulesDirectory string
	LockFilePath     string
	State            State
}

type State struct { /* format_version, configuration, outputs */ }
type StateConfiguration struct { /* source, source_key, directory, initialized_at */ }
type StateOutputs struct { /* run_directory, applied_at, values */ }

func Init(ctx context.Context, sourceLocation, stateDirectory string, options InitOptions) error
func Open(stateDirectory string) (string, string, error)
func OpenProject(stateDirectory string) (Project, error)
func SaveOutputs(stateDirectory, runDirectory string, outputs []byte) error
func ReadOutputs(stateDirectory string) ([]byte, error)
```
