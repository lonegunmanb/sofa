# Research: Immutable `Plan` Serialization, `.sofaplan` Self-Contained Archive, and Offline CLI

## 1. `r42/internal/plan` vs `sofa` Requirements

- **Inspected**: `d:\project\r42\internal\plan\{model.go,codec.go,display.go,file.go,permissions_windows.go,permissions_unix.go}`.
- **Findings**:
  - `r42` uses `ctyjson.MarshalType` + `ctymsgpack.Marshal` + `SensitivePaths` for lossless `cty.Value` encoding (including nested `cty.UnknownVal` and `golden.SensitiveMark`).
  - `r42/internal/plan/display.go` renders unknown values as `"<unknown>"`, whereas `docs/stage1/009-sofaplan-and-cli.md` explicitly requires `"<known after apply>"` for `sofa`.
  - `r42` saves raw JSON plans (`Save`/`Load`), whereas `sofa` packages a deterministic `.sofaplan` ZIP archive (`SaveArchive`/`LoadArchive`) containing `manifest.json`, `plan.json`, optional `plugins.lock.json`, and `bundle/{config,modules}/**`, with `skill_directories` validation and Zip-Slip/symlink/zip-bomb/SHA-256 verification.
  - On Windows, `restrictPlanFile(path, _)` uses `windows.SetNamedSecurityInfo` with `DACL_SECURITY_INFORMATION|PROTECTED_DACL_SECURITY_INFORMATION` and current-user SID. In `SaveArchive`, the temp file must be closed and renamed before calling `restrictPlanFile(archivePath, nil)` so no open handle blocks rename or ACL application.

## 2. Sensitivity Marks on `mcp_server`, `harness`, and `model_provider`

- **Inspected**: `internal/provider/schema.go`, `internal/mcp/spec/schema.go`, `internal/harness/spec/schema.go`.
- **Findings**:
  - `ModelProviderBlock` already exposes `APIKeyValue()` and `BearerTokenValue()` marked with `golden.SensitiveMark`, and `provider.NormalizeHeaders` transfers marks from object attributes to `cty.Map(cty.String)` keys.
  - `mcp/spec/ServerBlock` converts `HTTPBlocks[0].Headers` and `StdioBlocks[0].Env` to plain `map[string]string` via `stringMap` (which unmarks) and stores `BearerToken` as `*string`. To preserve sensitivity on `mcp_server` node configs without altering `mcppkg.Config`, `ServerBlock` still retains `HTTPBlocks[0].Headers` (`cty.Value`) and `StdioBlocks[0].Env` (`cty.Value`), and `BearerToken` can be explicitly marked with `specpkg.MarkSensitive` during export.
  - `HarnessBlock` stores `Options` as `cty.Value` (preserving marks) and `sourceVal`/`versionVal` in `BaseDecode`, while `SigningKey` is stored as `*string`. Capturing `signingKeyVal` in `HarnessBlock.BaseDecode` (with a `SigningKeyValue()` accessor) guarantees marks from sensitive variables on `signing_key` are preserved as well.

## 3. `golden` CLI Variable Precedence & Transitive DAG Dependencies

- **Inspected**: `golden/cli_flag_assigned_variables.go` and `golden/base_config.go` (`readCliAssignedVariables`, `GetAncestors`).
- **Findings**:
  - `golden` evaluates `CliFlagAssignedVariables` entries sequentially in slice order and merges them right-over-left. Using a single shared `pflag.Value` backing an ordered slice for both `--var` and `--var-file` ensures interleaved `-var` and `-var-file` flags preserve exact left-to-right CLI override order.
  - `CliFlagAssignedVariableFile` checks `afero.Exists(configFs, v.varFileName)` and falls back to `filepath.Join(c.variableConfigFilesDir(), v.varFileName)`. Pre-resolving relative `-var-file` paths against `WorkingDir` (when present on disk) before falling back to `proj.ConfigDirectory` matches the CLI contract cleanly.
  - `SofaConfig.GetAncestors(addr)` returns all transitive DAG ancestors, allowing `ExportPlan` to collect direct + `locals`-routed node dependencies cleanly by filtering ancestors to exported node addresses.
