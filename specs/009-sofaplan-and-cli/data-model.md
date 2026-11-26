# Data Model: `internal/plan`, `internal/variableschema`, `internal/planner`, `internal/cli`

## `internal/plan`

### `Plan` (`model.go`)

Immutable root or child plan snapshot with defensive-cloning getters:

| Field | Type | Getter |
|---|---|---|
| `directory` | `string` | `Directory() string` |
| `bundlePath` | `string` | `BundlePath() string` |
| `nodes` | `[]NodeSpec` | `Nodes() []NodeSpec` |
| `outputs` | `map[string]OutputSpec` | `Outputs() map[string]OutputSpec` |
| `context` | `map[string]cty.Value` | `Context() map[string]cty.Value` |
| `localExpressions` | `map[string]string` | `LocalExpressions() map[string]string` |
| `deferredValidations` | `map[string][]corespec.Condition` | `DeferredValidations() map[string][]corespec.Condition` |
| `tools` | `map[string]ToolSpec` | `Tools() map[string]ToolSpec` |
| `pluginLock` | `*harness.PluginLockFile` | `PluginLock() *harness.PluginLockFile` |

### `NodeSpec`, `ModuleSpec`, `ToolSpec`, `OutputSpec`, `Origin` (`model.go`)

- `NodeSpec`: `Address`, `CanonicalAddress`, `Kind` (one of the 12 valid `sofa` block kinds), `Dependencies []string`, `Config cty.Value`, `Module *ModuleSpec`, `Origin Origin`.
- `ModuleSpec`: `Plan *Plan`, `Source string`, `CanonicalAddress string`, `BundlePath string`, `Parallelism *int`, `Timeout *time.Duration`, `TimeoutRaw string`, `Inputs map[string]cty.Value`, `InputExprs map[string]string`, `DeferredValidations map[string][]corespec.Condition`.
- `ToolSpec`: `ID`, `Name`, `Address`, `CanonicalAddress`, `Kind` (`"go"` | `"external"`), `Description`, `Source`, `Dependencies []string`, `Program []string`, `WorkingDir`, `InputSchema json.RawMessage`, `InputType json.RawMessage`, `OutputType json.RawMessage`, `QuotePaths [][]string`, `Postconditions []corespec.Condition`, `Origin Origin`.
- `OutputSpec`: `Value cty.Value`, `Description *string`, `Sensitive bool`, `Expression string`, `SourceURLs []string`.
- `Origin`: `Filename`, `StartLine`, `StartColumn`, `EndLine`, `EndColumn`, `Source`.

### `Manifest`, `ArchiveSource`, `LoadArchiveOptions`, `LoadedArchive` (`archive.go`)

- `Manifest`: `FormatVersion int` (`1`), `SofaDSL string` (`"sofa"`), `PlanSHA256 string`, `PluginsLockSHA256 string` (`omitempty`), `BundleSHA256 string`.
- `ArchiveSource`: `ConfigDir string`, `ModulesDir string`, `PluginLock *harness.PluginLockFile`.
- `LoadArchiveOptions`: `MaxSingleFileBytes int64`, `MaxTotalBytes int64`, `MaxEntries int`.
- `LoadedArchive`: `Manifest Manifest`, `Plan *Plan`, `PluginLock *harness.PluginLockFile`, `ExtractDir string`, `ConfigDir string`, `ModulesDir string`.

## `internal/variableschema`

- `Document`: `SchemaVersion int` (`1`), `Variables []Variable`.
- `Variable`: `Name`, `Description *string`, `Type string`, `Required bool`, `Nullable bool`, `Sensitive bool`, `HasDefault bool`, `Default json.RawMessage`, `DefaultRedacted bool`.

## `internal/cli`

- `Options`: `WorkingDir string`, `Fetch module.FetchFunc`, `ReleaseFetcher harness.ReleaseFetcher`, `AttestationVerifier harness.AttestationVerifier`, `LookupEnv func(string) (string, bool)`.
- Exit codes: `ExitSuccess = 0`, `ExitFailure = 1`, `ExitUsage = 2` (`130` on `context.Canceled` interrupt in `cmd/sofa`).
