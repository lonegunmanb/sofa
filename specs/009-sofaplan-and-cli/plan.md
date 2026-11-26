# Implementation Plan: Immutable `Plan` Serialization, `.sofaplan` Self-Contained Archive, and Offline CLI

**Branch**: `009-sofaplan-and-cli` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 1 closure in Slice `009`: immutable `Plan` model + lossless `cty.Value` codec + redacted `Display` + deterministic/hardened `.sofaplan` ZIP archiving (`internal/plan`), recursive `SofaConfig.ExportPlan` with full sensitivity preservation and remote harness lockfile verification (`internal/planner/export.go`, `internal/harness/lock.go`), static root `variable` schema extraction (`internal/variableschema`), and offline Cobra CLI (`internal/cli`, `cmd/sofa`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **New Dependency**: `github.com/spf13/cobra` (bringing `github.com/spf13/pflag` for ordered `-var` / `-var-file` parsing)
- **Existing Dependencies Consumed**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty/cty` (`ctyjson`, `ctymsgpack`), `golang.org/x/sys/windows` (Windows ACLs), `internal/project`, `internal/harness`, `internal/planner`, `internal/module`, `internal/config`, `internal/spec`, `internal/source`, `internal/sandbox/spec`, `internal/provider`, `internal/mcp/spec`, `internal/tool/spec`, `internal/worker/spec`, `internal/merge/spec`
- **Testing**: 100% offline Tier 1 unit and end-to-end tests across `internal/plan`, `internal/planner`, `internal/variableschema`, `internal/cli`, and `cmd/sofa`

## Constitution Check

- **Principle I (Spec-First)**: Full Spec Kit artifacts created under `specs/009-sofaplan-and-cli/` before code changes.
- **Principle II (Strict TDD)**: Failing Red tests written for every package before production code (`tasks.md` enforces Red -> Green -> Refactor).
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `internal/plan` and `internal/variableschema` remain pure deterministic packages; CLI injects `Fetch`, `ReleaseFetcher`, `AttestationVerifier`, `LookupEnv`, `WorkingDir`, and IO streams via `cli.Options` for offline testing.
- **Principle IV (Runtime Enforcement)**: `LoadArchive` enforces SHA-256 verification, entry limits, and `os.OpenRoot` path safety; `ExportPlan` enforces `RequirePluginLock` against `plugins.lock.json`; `SaveArchive` enforces missing bundle `skill_directories` validation and `0600` / protected Windows DACL permissions.
- **Principle VI (Research-First & Golden Reuse)**: Inspected `r42/internal/{plan,variableschema,cli}` and `golden` (`readCliAssignedVariables`, `CliFlagAssignedVariable`, `CliFlagAssignedVariableFile`, `LocalBlock`, `GetAncestors`). Reuses `golden` variable precedence and DAG directly.

## Design Decisions

1. **Immutable `Plan`, `wireValue` Codec, and `Display` (`internal/plan`)**:
   - `Plan` owns unexported fields (`directory`, `bundlePath`, `nodes`, `outputs`, `context`, `localExpressions`, `deferredValidations`, `tools`, `pluginLock`) with deep-cloning constructors and getters.
   - `wireValue` uses `UnmarkDeepWithPaths()` + `ctyjson.MarshalType` + `ctymsgpack.Marshal` + `SensitivePaths` (filtering paths whose marks include `golden.SensitiveMark`). `Unmarshal` verifies every `SensitivePath` via `path.Apply(value)` before `MarkWithPaths` and re-runs `Plan` structural validation.
   - `Display` checks marks before recursing into composite values, rendering marked values as `"<sensitive>"` and unknown values as `"<known after apply>"`.
2. **Deterministic `.sofaplan` Archive & Hardened Unpack (`internal/plan/archive.go`)**:
   - Collects regular files/dirs from `ConfigDir` (`bundle/config/...`) and `ModulesDir` (`bundle/modules/...`), excluding `.initialized.json`, `.git`, `.sofa`, and `*.sofaplan` while rejecting symlinks/non-regular files via `os.OpenRoot`.
   - Validates all `skill_directories` referencing bundle paths across `planned` and nested `Module.Plan`s before writing the archive.
   - Writes entries sorted lexicographically with fixed `time.Unix(0, 0).UTC()`, writes atomically via temp file + close + rename + `restrictPlanFile`.
   - `LoadArchive` validates archive structure, entry limits, stream byte limits (`io.LimitReader`), SHA-256 of `plan.json`, `plugins.lock.json` (both presence/absence parity and digest), and `bundle_sha256` before returning.
3. **Recursive `ExportPlan` (`internal/planner/export.go`) & Lock Helper (`internal/harness/lock.go`)**:
   - Export `CanSatisfyHarnessLock(cfg harnessspec.HarnessConfig, lockFile PluginLockFile) error` in `internal/harness` reusing `canReuseCachedEntry` so `ExportPlan` checks root and child remote harnesses identically to `LockPlugins`.
   - Preserve sensitivity marks on `model_provider` (`api_key`, `bearer_token`, `headers`), `mcp_server` (`http.bearer_token`, `http.headers`, `stdio.env`), `harness` (`signing_key`, `options`, `source`, `version`), `worker`/`dynamic_worker`, `output`, and `var` context.
   - Compute transitive node dependencies by walking `c.GetAncestors(addr)` and filtering to exported node addresses; sort nodes by `(topologicalDepth, address)` and dependencies lexicographically.
4. **Root Variable Schema & Cobra CLI (`internal/variableschema`, `internal/cli`, `cmd/sofa`)**:
   - `variableschema.Inspect` parses root `variable` blocks using `typeexpr.TypeConstraintWithDefaults`, `specpkg.ValidateType`, and `convert.Convert`, formatting `optional(...)` types and redacting `sensitive = true` defaults.
   - `cli` normalizes `-var`/`-var-file` before `--`, parses interleaved `--var`/`--var-file` through a shared ordered `pflag.Value`, resolves relative `-var-file` paths against `WorkingDir` before falling back to `proj.ConfigDirectory`, and wires `init`, `schema`, and `plan` cleanly.

## Files

```text
go.mod / go.sum
internal/harness/lock.go
internal/harness/spec/schema.go
internal/mcp/spec/schema.go
internal/plan/{model.go,codec.go,display.go,archive.go,permissions_windows.go,permissions_unix.go,plan_test.go}
internal/planner/{export.go,export_test.go}
internal/variableschema/{schema.go,schema_test.go}
internal/cli/{command.go,command_test.go}
cmd/sofa/{main.go,main_test.go}
specs/009-sofaplan-and-cli/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
