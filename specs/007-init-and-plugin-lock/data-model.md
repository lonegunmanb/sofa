# Data Model: Workspace Init, Recursive Module Installation, and Harness Plugin Locking

## `.sofa/` Directory Layout

```text
<state_dir>/
├── .initializing          # Transaction lock present during Init; blocks Open on interrupted init
├── state.json             # format_version = 1 project metadata & applied outputs (0600)
├── plugins.lock.json      # format_version = 1 deterministic plugin SHA-256 lockfile (0600)
├── config/                # Snapshot of root configuration (skips .sofa, .git, *.sofaplan)
│   └── .initialized.json  # format_version = 2 snapshot consistency marker
└── modules/               # Recursively installed module snapshots (<label>/<nested>...)
```

## `internal/project.State` (`state.json`)

| Field | Type | Rules |
|---|---|---|
| `format_version` | `int` | `1` (`StateFormatVersion`) |
| `configuration.source` | `string` | Local slash path or redacted remote source (no userinfo, fragment, or non-`ref` query params) |
| `configuration.source_key` | `string` | SHA-256 hex digest of canonical source |
| `configuration.directory` | `string` | `<state_dir>/config` |
| `configuration.initialized_at` | `time.Time` | UTC timestamp matching `.initialized.json` |
| `outputs` | `*StateOutputs` | Optional applied JSON object outputs (`run_directory`, `applied_at`, `values`) |

## `internal/project.Project`

| Field | Type | Description |
|---|---|---|
| `StateDirectory` | `string` | Clean absolute path to `<state_dir>` |
| `ConfigDirectory` | `string` | `<state_dir>/config` |
| `ModulesDirectory` | `string` | `<state_dir>/modules` |
| `LockFilePath` | `string` | `<state_dir>/plugins.lock.json` |
| `State` | `State` | Validated `state.json` contents |

## `internal/harness.PluginLockFile` (`plugins.lock.json`)

| Field | Type | Rules |
|---|---|---|
| `format_version` | `int` | `1` (`LockFormatVersion`) |
| `plugins` | `[]PluginLockEntry` | Sorted lexicographically by `name`; non-nil `[]` when empty |

### `PluginLockEntry`

| Field | Type | Rules |
|---|---|---|
| `name` | `string` | Harness block label |
| `source` | `string` | `<host>/<owner>/<repo>` |
| `version` | `string` | Exact semver without leading `v` |
| `verified_signature` | `string` | `"none"` \| `"pgp"` \| `"attestation"` |
| `signing_key_fingerprint` | `string` (omitempty) | Uppercase primary OpenPGP key fingerprint hex when `verified_signature == "pgp"` |
| `assets` | `map[string]PluginAsset` | Keyed by `<os>_<arch>`, must include `linux_amd64` or `linux_arm64` |

### `PluginAsset`

| Field | Type | Rules |
|---|---|---|
| `filename` | `string` | `sofa-harness-<name>_<os>_<arch>.zip` |
| `sha256` | `string` | Lowercase 64-character hex digest from `checksums.txt` |
