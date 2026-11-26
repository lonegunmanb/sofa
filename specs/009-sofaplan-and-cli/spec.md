# Feature Specification: Immutable `Plan` Serialization, `.sofaplan` Self-Contained Archive, and Offline CLI

**Feature Branch**: `009-sofaplan-and-cli`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 009 from `docs/stage1/009-sofaplan-and-cli.md`, `docs/hcl-contract.md`, and `docs/architecture-contract.md`.

## User Scenarios & Testing

### User Story 1 - Immutable `Plan` Model, Lossless `cty.Value` Codec, and Redacted `Display` (`internal/plan`) (Priority: P1)

An operator or downstream runner serializes a planned `sofa` DAG into an immutable `*plan.Plan` containing node topologies, node configs, child module `ModuleSpec` plans, tool metadata (`Tools`), local expressions, deferred validations, plugin lockfile snapshot, and outputs. `Marshal` and `Unmarshal` round-trip known values, typed `cty.UnknownVal` values (primitive and nested), and `golden.SensitiveMark` paths losslessly via `ctyjson.MarshalType`, `ctymsgpack.Marshal`, and `SensitivePaths`. `Display` formats a redacted JSON review view where every sensitive leaf or marked container renders as `"<sensitive>"` and every unknown value renders as `"<known after apply>"`. `ContainsSensitiveValues` accurately detects sensitivity across nodes, outputs, context, module inputs, and nested child plans.

**Independent Test**: Unit-test `New`, getters, validation invariants (unique addresses, valid node kinds, child plan requirement on `module` nodes, known dependency addresses, acyclicity, valid `ToolSpec` IDs), `Marshal`/`Unmarshal` round-trip, `Display`/`DisplayValues` redaction + unknown rendering, and `ContainsSensitiveValues` in `internal/plan/plan_test.go`.

### User Story 2 - Deterministic `.sofaplan` ZIP Archive Packaging, `skill_directories` Validation, and Hardened Unpacking (`internal/plan/archive.go`) (Priority: P1)

`SaveArchive` packages `manifest.json`, `plan.json`, optional `plugins.lock.json`, and clean snapshots of `ConfigDir` (`bundle/config/...`) and `ModulesDir` (`bundle/modules/...`) into a byte-reproducible `.sofaplan` ZIP archive with restricted `0600` (or Windows current-user-only protected DACL) permissions. Before writing the archive, `SaveArchive` walks the root and all nested child plans to verify that every `skill_directories` entry pointing inside `bundle/` (for `worker`, `worker.qc`, and `dynamic_worker` content/`qc`) exists as a directory in the collected bundle. `LoadArchive` enforces strict security limits (`MaxSingleFileBytes`, `MaxTotalBytes`, `MaxEntries`), rejects path traversal/Zip-Slip/symlinks/unexpected prefixes/duplicate entries, verifies SHA-256 checksums for `plan.json`, `plugins.lock.json`, and `bundle/**` against `manifest.json`, and safely extracts via `os.OpenRoot`.

**Independent Test**: Unit-test `SaveArchive` byte-for-byte determinism, cross-directory `LoadArchive` restoration, missing `skill_directories` rejection, and adversarial archive rejection (tampered `plan.json`, tampered `bundle/`, missing/unlisted `plugins.lock.json`, `../../evil`, backslash paths, symlink entries, and oversized decompression streams) in `internal/plan/plan_test.go`.

### User Story 3 - Recursive `*SofaConfig` Plan Export with Sensitivity & Lockfile Verification (`internal/planner/export.go`) (Priority: P1)

`(c *SofaConfig) ExportPlan(opts ExportOptions) (*plan.Plan, error)` exports a `RunPlan()`-completed configuration (root and all nested child modules) into `*plan.Plan`, covering all 12 block kinds (`source`, `preserve`, `sandbox`, `harness`, `model_provider`, `mcp_server`, `go_tool`, `external_tool`, `worker`, `dynamic_worker`, `merge`, `module`), `Origin` metadata, transitive DAG dependencies routed through `locals`/`var`, `Tools` metadata (JSON Schema, `ctyjson.MarshalType`, `QuotePaths`, `Postconditions`), and `golden.SensitiveMark` preservation on secrets (`model_provider` `api_key`/`bearer_token`/`headers`, `mcp_server` `http.bearer_token`/`http.headers`/`stdio.env`, `harness` `signing_key`/`options`, `worker`/`dynamic_worker` prompts/inputs/imports/criteria, `output`, and `var` context). When `RequirePluginLock` is true, every remote `harness` across root and child modules is verified against `PluginLock` using `harness.CanSatisfyHarnessLock`.

**Independent Test**: Unit-test exporting a multi-module workflow containing all 12 block kinds, sensitive fields, `locals`-routed transitive dependencies, `Marshal -> Unmarshal` round-trip, `Display` zero-secret leakage, and `RequirePluginLock` rejection of unlocked/mismatched remote harnesses in `internal/planner/export_test.go`.

### User Story 4 - Static Root Variable Schema Inspection & Offline Cobra CLI (`internal/variableschema`, `internal/cli`, `cmd/sofa`) (Priority: P1)

`variableschema.Inspect` extracts root `variable` declarations (`name`, `description`, `type`, `required`, `nullable`, `sensitive`, `has_default`, `default`, `default_redacted`) without requiring values for required variables, formatting `optional(...)` object types deterministically and redacting `sensitive = true` defaults to `null` + `default_redacted = true` in JSON and `<sensitive>` in `FormatText`. The Cobra CLI exposes `sofa init [source] [--state-dir .sofa] [--upgrade]`, `sofa schema [--state-dir .sofa] [--json]`, and `sofa plan [-var 'k=v'] [-var-file path] [--state-dir .sofa] [--out path.sofaplan]` with single-dash `-var`/`-var-file` normalization, strict left-to-right interleaved `-var`/`-var-file` precedence, redacted `Display` JSON on `stdout`, unencrypted-sensitive warning on `stderr` when writing `--out`, and clean exit codes (`0`, `1`, `2`, `130`).

**Independent Test**: Unit-test `variableschema` in `internal/variableschema/schema_test.go` and end-to-end CLI pipelines (`init` -> `schema` -> `plan --out` -> `LoadArchive`, uninitialized project error, invalid flag/var usage errors, and `cmd/sofa` interrupt/diagnostic formatting) in `internal/cli/command_test.go` and `cmd/sofa/main_test.go`.

### Edge Cases

- `Display` must check value marks *before* descending into composite children so a marked container collapses to `"<sensitive>"` while an unmarked container redacts only its marked leaves; unknown values must render strictly as `"<known after apply>"`.
- On Windows, all `*os.File`, `*zip.ReadCloser`, and `*os.Root` handles must be closed inside helper scopes before `os.Rename`, `os.RemoveAll`, or `restrictPlanFile`.
- `mcp_server` `http.bearer_token` must be marked sensitive and marks on `http.headers` / `stdio.env` preserved during export even though `mcppkg.Config` stores un-marked `string`/`map[string]string`; similarly `harness` `signing_key` must preserve marks from sensitive variables.
- Transitive DAG edges passing through `local.*` or `var.*` vertices must map to concrete exported node dependencies in `NodeSpec.Dependencies` without dangling references to non-node vertices.
- Interleaved `-var` and `-var-file` flags on `sofa plan` must populate a single ordered `[]golden.CliFlagAssignedVariables` slice via a shared `pflag.Value` so left-to-right CLI precedence holds across mixed flags.
- `skill_directories` validation must check both root (`config/...`) and nested module (`modules/<labels...>/...`) relative or bundle-prefixed paths while skipping host-absolute container paths not rooted inside `bundle/`.

## Requirements

- **FR-001**: `internal/plan` MUST define immutable `Plan`, `NodeSpec`, `ModuleSpec`, `ToolSpec`, `OutputSpec`, and `Origin` with deep-cloning constructors/getters and full structural validation (unique addresses, valid node kinds, non-nil child `Module.Plan`, valid intra-plan dependencies, acyclicity, and valid `ToolSpec` IDs).
- **FR-002**: `internal/plan` MUST provide `Marshal` / `Unmarshal` (`format_version = 1`) preserving `cty.UnknownVal`, `golden.SensitiveMark` paths, `ModuleSpec.Inputs`, `DeferredValidations`, `Tools`, and `PluginLock`.
- **FR-003**: `internal/plan` MUST provide `Display`, `DisplayValues`, and `ContainsSensitiveValues`, rendering marked values as `"<sensitive>"` and unknown values as `"<known after apply>"`.
- **FR-004**: `internal/plan` MUST provide deterministic `SaveArchive` (`0600` / Windows current-user protected DACL, `skill_directories` bundle validation) and security-hardened `LoadArchive` (SHA-256 verification, Zip-Slip/symlink/zip-bomb defense, `os.OpenRoot` extraction).
- **FR-005**: `internal/planner` MUST provide `ExportPlan(ExportOptions)` exporting all 12 HCL block kinds, preserving sensitivity marks across all blocks/outputs/context, resolving transitive node dependencies across `locals`, collecting `Tools`, and verifying remote harnesses against `PluginLock` when `RequirePluginLock` is true.
- **FR-006**: `internal/variableschema` MUST inspect root `variable` declarations without requiring required variable values, support `optional(...)` types and `nullable = false` validation, redact sensitive defaults in JSON (`null` + `default_redacted: true`) and `FormatText` (`<sensitive>`), and reject unknown attributes/duplicates.
- **FR-007**: `internal/cli` and `cmd/sofa` MUST implement `sofa init`, `sofa schema`, and `sofa plan` with single-dash `-var`/`-var-file` normalization, strict left-to-right interleaved `-var`/`-var-file` evaluation, `Display` stdout output, sensitive `--out` stderr warning, `hcl.Diagnostics` error formatting, and exit codes `0`/`1`/`2`/`130`.

## Success Criteria

- **SC-001**: All unit tests in `internal/plan`, `internal/planner`, `internal/variableschema`, `internal/cli`, and `cmd/sofa` pass 100% offline.
- **SC-002**: `gofmt -l .` produces empty output, and `go vet ./...` and `go test ./... -count=1` pass with zero warnings or errors.
