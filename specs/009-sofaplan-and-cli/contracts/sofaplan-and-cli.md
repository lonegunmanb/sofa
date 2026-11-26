# Contract: `.sofaplan` Format, `ExportPlan`, Variable Schema, and Offline CLI

## 1. `wirePlan` & `.sofaplan` ZIP Layout

- `plan.Marshal` emits JSON with `format_version = 1`. Every `cty.Value` (`NodeSpec.Config`, `OutputSpec.Value`, `Context[*]`, `ModuleSpec.Inputs[*]`) is encoded as `wireValue{Type, Value, SensitivePaths}` using `ctyjson.MarshalType` and `ctymsgpack.Marshal` on `UnmarkDeepWithPaths()`.
- `Display` renders any value bearing `golden.SensitiveMark` as `"<sensitive>"` and any unknown value as `"<known after apply>"`.
- `.sofaplan` archive entries (sorted lexicographically, `Modified = time.Unix(0, 0).UTC()`, permissions `0600` / protected current-user Windows DACL):
  - `manifest.json`
  - `plan.json`
  - `plugins.lock.json` (when `PluginLock` has `>= 1` entries)
  - `bundle/config/...`
  - `bundle/modules/...`
- `SaveArchive` rejects missing `bundle/` directories referenced by `skill_directories` on `worker`, `worker.qc`, or `dynamic_worker`.
- `LoadArchive` enforces `MaxSingleFileBytes`, `MaxTotalBytes`, `MaxEntries`, path hygiene, symlink rejection, exact `PlanSHA256`, `PluginsLockSHA256` presence/digest match, and `BundleSHA256`.

## 2. `SofaConfig.ExportPlan(ExportOptions)`

- Exports all 12 node kinds ordered deterministically by `(topologicalDepth, address)`.
- Resolves transitive node dependencies across `locals`/`var` via DAG ancestors filtered to exported node addresses.
- Preserves `golden.SensitiveMark` on `model_provider` (`api_key`, `bearer_token`, `headers`), `mcp_server` (`http.bearer_token`, `http.headers`, `stdio.env`), `harness` (`signing_key`, `options`, `source`, `version`), `worker`/`dynamic_worker`, `output`, and `var` context.
- When `RequirePluginLock` is true, verifies every remote `harness` across root and child modules via `harness.CanSatisfyHarnessLock`.

## 3. CLI Commands

- `sofa init [source] [--state-dir .sofa] [--upgrade]`
- `sofa schema [--state-dir .sofa] [--json]`
- `sofa plan [-var 'k=v'] [-var-file path] [--state-dir .sofa] [--out path.sofaplan]`
  - Interleaved `-var` / `--var` and `-var-file` / `--var-file` flags apply left-to-right.
  - Malformed `-var` (missing `=` or empty name) returns `ExitUsage` (`2`).
  - Uninitialized state directory returns `ExitFailure` (`1`) guiding the user to run `sofa init`.
  - When `--out` is set and `ContainsSensitiveValues(planned)` is true, writes an unencrypted-sensitive warning to `stderr`.
