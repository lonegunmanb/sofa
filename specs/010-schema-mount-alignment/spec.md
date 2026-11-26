# Feature Specification: Stage 2 Schema & Mount Alignment (`010-schema-mount-alignment`)

**Feature Branch**: `010-schema-mount-alignment`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Stage 2 Slice `010` blueprint (`docs/stage2/010-schema-mount-alignment.md`), `docs/stage2/README.md`, and `docs/hcl-contract.md`.

## User Scenarios & Testing

### User Story 1 - Forbid `sandbox.entrypoint` (`internal/sandbox/spec`, `internal/planner/export.go`) (Priority: P1)

Because Stage 2 executes `sofa-harness-<name>` directly as the container entrypoint inside Agent sessions and `command[0]` inside `gatekeeper` containers, user-configured `sandbox.entrypoint` values are removed from the `sandbox` HCL schema. Any HCL `sandbox` block declaring `entrypoint` (even `null` or `[]`) is rejected natively by `golden.Decode` as an unsupported attribute, and `SandboxConfig`, `SandboxBlock`, and exported `Plan` node configs contain no `entrypoint` field.

**Independent Test**: Unit-test that `sandbox` blocks with `entrypoint = ["/bin/sh"]` or `entrypoint = null` fail during `golden.Decode`, `SandboxConfig.Validate()` / `SandboxBlock.SandboxConfig()` expose no `Entrypoint` field, and `ExportPlan` -> `Marshal` -> `Unmarshal` -> `Display` outputs contain no `"entrypoint"` key on `sandbox` nodes (`internal/sandbox/spec/schema_test.go`, `internal/planner/sandbox_test.go`, `internal/planner/export_test.go`).

### User Story 2 - Independent `sandbox` and `profile` Overrides on `worker.qc` (`internal/worker/spec`, `internal/planner/export.go`) (Priority: P1)

Workflow authors can configure `qc` nested blocks inside `worker` and `dynamic_worker` with independent `sandbox = sandbox.<name>` and `profile = "<name>"` attributes so QC sessions can run in dedicated audit containers with distinct runtime profiles while keeping `(sandbox, harness, model_provider, model/profile)` orthogonal. When `qc.sandbox` is omitted or `null`, `QCConfig.Sandbox` defaults to `cty.NullVal(cty.EmptyObject)` (inheriting the parent worker's `sandbox` at runtime). When `qc.sandbox` references a `sandbox.<name>` block, `decodeQC` validates the block reference via `validateBlockReference(val, "sandbox", "qc sandbox")` (rejecting wrong block kinds such as `harness.<name>` or arbitrary objects), `golden` automatically wires the DAG dependency edge to `sandbox.<name>`, and `ExportPlan` exports both `qc.sandbox` and `qc.profile`.

**Independent Test**: Unit-test `qc` decoding (omitted/`null`, valid `sandbox = sandbox.sec_audit` + `profile = "opus-audit"`, blank `profile` rejection, wrong-kind `sandbox = harness.local` rejection) for both `worker` and `dynamic_worker`, verify DAG ancestor edges to `sandbox.<name>`, verify `DeferredExpression` preservation, and verify `ExportPlan` round-trip (`internal/worker/spec/worker_test.go`, `internal/worker/spec/dynamic_test.go`, `internal/planner/worker_test.go`, `internal/planner/export_test.go`).

### User Story 3 - Container-External `/.sofa/artifacts` & `/.sofa/imports` Path Alignment (`internal/worker/spec`, `internal/planner/export.go`) (Priority: P1)

Non-Git workflow artifacts and imports live outside `/workspace` inside containers under `/.sofa/artifacts` and `/.sofa/imports`. HCL inputs on `artifact.path` and `import_artifact.path` remain clean relative sub-paths validated by `validateWorkspaceRelativePath` (rejecting absolute paths, backslashes, drive letters, `.`, and `..` traversal). `Artifact.ContainerPath()` returns `"/.sofa/artifacts/<rel_path>"` and `artifactValue` sets `.path` to `Artifact.ContainerPath()` while preserving the relative path in `.rel_path`. `ImportArtifact.ContainerPath()` returns `"/.sofa/imports/<rel_path_or_name>"` (using `effectiveImportRelPath`, which defaults to `imported.Name` when `path` is omitted), and `import_artifact.<name>.path` evaluates to `ImportArtifact.ContainerPath()`. Intra-`artifact` and intra-`import_artifact` overlapping/parent-child paths remain rejected, while `artifact` and `import_artifact` may use identical relative sub-paths (such as `"spec.md"`) without conflict because they mount into disjoint container trees. `ExportPlan` exports `"container_path"` alongside `"path"` on both `artifacts` and `imports`.

**Independent Test**: Unit-test `artifact("<name>").path`, `artifact("<name>").rel_path`, `worker.<w>.artifact.<name>.path`, `worker.<w>.artifact.<name>.rel_path`, `import_artifact.<name>.path` (both default and explicit `path`), deferred expression `import_artifact` paths, rejection of absolute/escaping `artifact.path` and `import_artifact.path`, intra-set overlap rejection (`"spec.md"`/`"spec.md"` and `"dir"`/`"dir/a.txt"`), cross-set non-conflict (`artifact` and `import_artifact` both using `"spec.md"`), and `ExportPlan` `container_path` export (`internal/worker/spec/worker_test.go`, `internal/planner/worker_test.go`, `internal/planner/module_test.go`, `internal/planner/export_test.go`).

### Edge Cases

- `sandbox` blocks declaring `entrypoint` (even `entrypoint = null`) MUST be rejected natively by `golden.Decode` without custom attribute-name blacklists.
- `qc.sandbox = null` or omitted MUST normalize to `cty.NullVal(cty.EmptyObject)` in both `QCConfig` and `ExportPlan` (`cty.NilVal` normalized cleanly).
- `qc.profile = null` or omitted leaves `QCConfig.Profile == ""`, while an explicit blank string (`"   "`) or non-string MUST fail during plan decoding.
- `dynamic_worker` virtual worker decoding MUST enforce the same `qc.sandbox` and `qc.profile` validation and preserve `qc` attributes in `ContentTemplate` / `DeferredExpression`.
- Path overlap detection within `artifact` or within `import_artifact` MUST reject both exact duplicates (`"spec.md"` vs `"spec.md"`) and parent/child nesting (`"dir"` vs `"dir/a.txt"`), without false positives on sibling prefixes (`"dir"` vs `"dir2/a.txt"`).
- Child module `variable` objects typed after `worker.<w>.artifact.<name>` MUST include `rel_path = string` when matching the updated `artifactValueType`.

## Requirements

- **FR-001**: `internal/sandbox/spec` and `internal/planner/export.go` MUST remove `Entrypoint` from `SandboxConfig`, `SandboxBlock`, `cloneSandboxConfig`, and `encodeSandboxConfig`, relying natively on `golden.Decode` to reject `entrypoint`.
- **FR-002**: `internal/worker/spec/qc.go` MUST support optional `sandbox` (default `cty.NullVal(cty.EmptyObject)`, validated via `validateBlockReference(..., "sandbox", "qc sandbox")`) and optional non-blank `profile` on `QCConfig` and `decodeQC`, preserving DAG dependency edges and `DeferredExpression` serialization for both `worker` and `dynamic_worker`.
- **FR-003**: `internal/planner/export.go` MUST export `qc.sandbox` (normalizing `cty.NilVal` to `cty.NullVal(cty.EmptyObject)`) and `qc.profile` in `encodeWorkerConfig`.
- **FR-004**: `internal/worker/spec` MUST define `ContainerArtifactsRoot = "/.sofa/artifacts"` and `ContainerImportsRoot = "/.sofa/imports"`, `Artifact.ContainerPath()`, `EffectiveImportRelPath`, and `ImportArtifact.ContainerPath()`.
- **FR-005**: `artifactValue` MUST return `.path` as `Artifact.ContainerPath()` and `.rel_path` as `Artifact.Path`; `WorkerBlock` `import_artifact.<name>.path` and `buildDeferredExpression` MUST evaluate to `ImportArtifact.ContainerPath()`.
- **FR-006**: `WorkerBlock.decodeNestedDeclarations` MUST enforce intra-`artifact` and intra-`import_artifact` exact and parent-child path overlap checks while permitting identical relative paths across `artifact` and `import_artifact`.
- **FR-007**: `internal/planner/export.go` MUST include `"container_path"` alongside relative `"path"` on exported `artifacts` and `imports` entries.

## Success Criteria

- **SC-001**: All Tier 1 unit tests across `internal/sandbox/spec`, `internal/worker/spec`, `internal/planner`, and the full repository pass offline.
- **SC-002**: `gofmt -l .` produces empty output, and `go vet ./...` and `go test ./... -count=1` pass with zero errors.
