# Data Model: `embeddedcli` & `cmd/bundler` Adoption (`033`)

## 1. `sofa-harness-copilot` CLI Path & Runtime Home Resolution

### `HarnessOptionsJson` Fields
| Field | Type | Meaning |
| :--- | :--- | :--- |
| `cli_path` | `string` | Highest-precedence explicit Copilot CLI binary path (trimmed; ignored when whitespace-only) |
| `use_path_cli` | `bool` | Opt-in flag enabling `LookPath("copilot")` fallback when neither `cli_path` nor `<execDir>/bin/copilot` exists |

### Resolution Order (`resolveCLIPath`)
1. `options.cli_path` (trimmed, non-empty) → returns `(cli_path, nil)`
2. `<execDir>/bin/copilot` (regular non-directory file) → returns `(bundled, nil)`
3. `options.use_path_cli == true` → invokes `s.lookPath("copilot")`; returns `(found, nil)` or error if missing
4. Default → returns `("", nil)` (delegates to `copilot-sdk/go` `embeddedcli`)

### `COPILOT_HOME` / `BaseDirectory` State
| Context | Source Priority | Directory Mode |
| :--- | :--- | :--- |
| Plugin host process (`embeddedcli` extraction) | Existing `os.Getenv("COPILOT_HOME")`, else `filepath.Join(os.TempDir(), "sofa-copilot")` | `0o700` via `os.MkdirAll` |
| Child `copilot-runtime` process (`COPILOT_HOME` + `sdk.ClientOptions.BaseDirectory`) | `HarnessEnv["COPILOT_HOME"]` → `OpenSessionRequest.SessionStoreDirectory` → host `COPILOT_HOME` | Inherited / mounted writable |

---

## 2. `build-bundle.go` Flags & Output Layouts

### Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `-out` | `./dist` | Output parent directory (`<out>/sofa-harness-copilot`) |
| `-arch` | `amd64` | Target Linux `GOARCH` (`amd64` or `arm64`) |
| `-cache-dir` | `""` | Optional cache directory storing `copilot-bundler-linux-<arch>/zcopilot*` |
| `-cli-version` | `""` | Optional `--cli-version` passed to `cmd/bundler` |
| `-copilotbin` | `""` | Override: prebuilt Linux `copilot` binary copied to `bin/copilot` |
| `-copilotsrc` | `""` | Override: Go package cross-compiled to `bin/copilot` |

### Output Layouts
- **Default `bundler` mode**: `<out>/sofa-harness-copilot/sofa-harness-copilot` (single static Linux binary; no `bin/` directory).
- **Override mode (`-copilotbin` / `-copilotsrc`)**: `<out>/sofa-harness-copilot/{sofa-harness-copilot, bin/copilot}`.

### Cache Directory Layout (`-cache-dir` in Default Mode)
`<cache-dir>/copilot-bundler-linux-<arch>/zcopilot_linux_<arch>.go` (+ any companion `zcopilot*` files produced by `cmd/bundler`).
