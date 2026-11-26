# Implementation Plan: Stage 2 Schema & Mount Alignment (`010-schema-mount-alignment`)

**Branch**: `010-schema-mount-alignment` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `010`: align the HCL schema, DAG references, and exported `Plan` representations with the Stage 2 container execution contract before building Stage 2 runtime packages (`011`–`020`). Specifically: remove `sandbox.entrypoint`, add optional `sandbox` and `profile` overrides to `worker.qc`, and align `artifact` / `import_artifact` `.path` values to container-external `/.sofa/artifacts/...` and `/.sofa/imports/...` mounts while isolating intra-set path overlap checks.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Existing Dependencies Consumed**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty/cty`, `internal/sandbox/spec`, `internal/worker/spec`, `internal/planner`
- **Testing**: 100% offline Tier 1 unit tests (`go test ./... -count=1`)

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts created under `specs/010-schema-mount-alignment/` prior to editing Go code.
- **Principle II (Strict TDD)**: Red failing tests are added/updated first for each requirement (`sandbox.entrypoint` rejection, `qc.sandbox`/`qc.profile` overrides & DAG edges, `/.sofa/{artifacts,imports}` interpolation, path overlap rules, and `ExportPlan` fields), verified failing, then satisfied with minimal Green edits.
- **Principle III (Pure Core)**: All changes reside in pure schema/planner packages (`internal/sandbox/spec`, `internal/worker/spec`, `internal/planner`) with zero container/Git side effects.
- **Principle VI (Research-First & Golden Reuse)**: Inspected `internal/sandbox/spec/schema.go`, `internal/worker/spec/{qc.go,artifact.go,schema.go,deferred.go,dynamic.go}`, `internal/planner/{export.go,defaults.go,module_test.go}`, and `docs/stage2/010-schema-mount-alignment.md`. `golden.Decode` natively rejects removed `entrypoint` without any manual attribute blacklist, and `golden` natively builds DAG edges for `qc.sandbox`.

## Design Decisions

1. **Remove `sandbox.entrypoint` (`internal/sandbox/spec/schema.go`, `internal/planner/export.go`)**:
   - Delete `Entrypoint` from `SandboxConfig` and `SandboxBlock`, remove `Entrypoint` validation in `Validate()` / `ExecuteDuringPlan()`, remove `Entrypoint` copy logic in `SandboxConfig()` / `cloneSandboxConfig()`, and remove `"entrypoint"` from `encodeSandboxConfig`.
   - `golden.Decode` rejects unknown attribute `entrypoint` automatically.
2. **Independent `qc.sandbox` and `qc.profile` (`internal/worker/spec/qc.go`, `internal/planner/export.go`)**:
   - Add `"sandbox"` and `"profile"` to `qcAllowedAttrs`, and `Sandbox cty.Value` + `Profile string` to `QCConfig`.
   - Decode `qc.sandbox` identically to `qc.harness` (default `cty.NullVal(cty.EmptyObject)`, non-null validated with `validateBlockReference(val, "sandbox", "qc sandbox")`).
   - Decode `qc.profile` via `optionalKnownString` rejecting blank strings.
   - `writeDeferredQC` in `internal/worker/spec/deferred.go` already serializes all attributes on `qc`, preserving `sandbox` and `profile`.
   - Export `"sandbox"` (normalizing `cty.NilVal` to `cty.NullVal(cty.EmptyObject)`) and `"profile"` in `encodeWorkerConfig`.
3. **Container-External Artifact & Import Paths (`internal/worker/spec/{artifact.go,schema.go,deferred.go}`, `internal/planner/export.go`)**:
   - Export `ContainerArtifactsRoot = "/.sofa/artifacts"` and `ContainerImportsRoot = "/.sofa/imports"`.
   - Keep HCL `artifact.path` and `import_artifact.path` as clean relative sub-paths validated by `validateWorkspaceRelativePath`.
   - Implement `(a Artifact) ContainerPath() string`, `(i ImportArtifact) ContainerPath() string`, and `EffectiveImportRelPath(imported ImportArtifact) string`.
   - Update `artifactValueType` and `artifactValue` to expose `.path = ContainerPath()` and `.rel_path = Artifact.Path`.
   - Update `WorkerBlock.decodeNestedDeclarations` to set `imported.Path = effectiveImportRelPath(imported)`, expose `import_artifact.<name>.path = imported.ContainerPath()`, and enforce separate intra-set overlap checks (`exact` or `parent/child`) for `artifacts` and `imports`.
   - Update `buildDeferredExpression` (`internal/worker/spec/deferred.go`) so `import_artifact.<name>.path` emits `ContainerPath()`.
   - Update `encodeWorkerConfig` (`internal/planner/export.go`) to include `"container_path"` on both `artifacts` and `imports`.

## Files

```text
internal/sandbox/spec/{schema.go,schema_test.go}
internal/worker/spec/{qc.go,artifact.go,fallback.go,schema.go,deferred.go,worker_test.go,dynamic_test.go}
internal/planner/{export.go,export_test.go,sandbox_test.go,worker_test.go,module_test.go}
docs/hcl-contract.md
specs/010-schema-mount-alignment/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
