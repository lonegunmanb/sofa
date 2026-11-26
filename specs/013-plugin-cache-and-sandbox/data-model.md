# Data Model: Plugin Cache Verification & Podman Sandbox Runtime (`013`)

## `internal/harness/installer`

### `Platform`
- `{OS string, Arch string}`
- `Key() string`: returns `OS + "_" + Arch` (e.g., `"linux_amd64"`).
- `Validate() error`: requires `OS == "linux"` and `Arch` in `{"amd64", "arm64"}`.

### `PreparedPlugin`
- `{Name string, HostDir string, ContainerPath string}`
- `ContainerPath` is fixed to `"/.sofa/harness/sofa-harness-" + Name`.

### `ResolveOptions`
- `PluginDir string` (`--plugin-dir`)
- `WorkingDir string`
- `UserHomeDir string`
- `CacheRoot string`
- `LockFile harness.PluginLockFile`
- `ReleaseFetcher harness.ReleaseFetcher`
- `LookupEnv func(string) (string, bool)`
- `MaxSingleFileBytes int64` (optional override; default `128 MiB`)
- `MaxTotalBytes int64` (optional override; default `512 MiB`)
- `MaxEntries int` (optional override; default `20000`)

## `internal/sandbox/runtime`

### `Platform`
- Type alias `type Platform = installer.Platform`

### `BindMount`
- `{Source string, Target string, ReadOnly bool}`

### `SessionMountLayout`
- `Sandbox sandboxspec.SandboxConfig`
- `HostOS string` (defaults to `runtime.GOOS`)
- `ContainerName string`
- `Detached bool`
- `PublishPluginPort bool` (`-p 127.0.0.1::19500`)
- `ForAgentSession bool`
- `Entrypoint string`
- `WorkingDir string` (defaults to `"/workspace"`)
- `Args []string`
- `HostWorkspaceDir string`
- `ReadOnlyWorkspace bool`
- `WritablePaths []string`
- `BundleDir string`
- `ImportsDir string`
- `ArtifactsDir string`
- `ReadOnlyArtifacts bool`
- `ToolsDir string`
- `HarnessDir string`
- `SessionStoreDir string`
- `PluginSocketDir string`

### `ExecResult`
- `{Stdout string, Stderr string, ExitCode int}`

### `GatekeeperRunSpec`
- `{Sandbox sandboxspec.SandboxConfig, HostWorkspaceDir string, BundleDir string, ImportsDir string, ArtifactsDir string, HostOS string}`

### `GatekeeperFailure`
- `{Name string, Command []string, ExitCode int, ExpectedExitCode int, TimedOut bool, DirtyFiles []string, Stdout string, Stderr string}`

### `Engine` Interface
- `DetectPlatform(ctx context.Context) (Platform, error)`
- `Exec(ctx context.Context, containerName, workDir string, command []string, env map[string]string, stdin io.Reader) (ExecResult, error)`
- `RunGatekeepers(ctx context.Context, spec GatekeeperRunSpec, gatekeepers []workerspec.GatekeeperConfig) (*GatekeeperFailure, error)`
