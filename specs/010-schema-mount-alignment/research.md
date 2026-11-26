# Research: Stage 2 Schema & Mount Alignment (`010-schema-mount-alignment`)

## 1. Existing `sandbox.entrypoint` Implementation & `golden.Decode` Rejection

- **Inspected**: `internal/sandbox/spec/schema.go`, `internal/planner/sandbox_test.go`, `internal/planner/export.go`.
- **Findings**:
  - `SandboxBlock` uses `golden` struct-tag decoding (`hcl:"...,optional"`). Removing `Entrypoint []string` from `SandboxBlock` causes `golden.Decode` to reject `entrypoint` as an unsupported argument automatically, with zero custom attribute checks.
  - `SandboxConfig.Entrypoint`, `Validate()`, `ExecuteDuringPlan()`, `SandboxBlock.SandboxConfig()`, `cloneSandboxConfig()`, and `encodeSandboxConfig` all reference `Entrypoint` and must be cleaned up together. `SandboxBlock.Value()` already omits `entrypoint`.

## 2. `worker.qc` Decoding, `dynamic_worker`, Deferred Serialization & DAG Edges

- **Inspected**: `internal/worker/spec/{qc.go,schema.go,deferred.go,dynamic.go}`, `internal/planner/{defaults.go,export.go}`.
- **Findings**:
  - `decodeQC` validates allowed attributes via `qcAllowedAttrs` and nested blocks via `qcAllowedNested`. `qc.harness` uses `validateBlockReference(val, "harness", "qc harness")` when non-null, defaulting to `cty.NullVal(cty.EmptyObject)`. `qc.sandbox` follows the exact same pattern.
  - `qc.model` uses `optionalKnownString(block, ctx, "model", "qc")` plus `strings.TrimSpace(...) == ""` rejection; `qc.profile` matches this pattern.
  - `DynamicWorkerBlock.Decode` delegates to a virtual `WorkerBlock.Decode`, so `dynamic_worker` inherits `qc.sandbox` and `qc.profile` validation automatically.
  - `writeDeferredQC` in `internal/worker/spec/deferred.go` writes all attributes on the `qc` block via `writeExprAttributes(builder, block.Attributes(), nil, nil)`, so `sandbox` and `profile` are automatically preserved in `DeferredExpression` and `ContentTemplate`.
  - `defaults.go` only injects `sandbox`/`harness`/`model_provider` on `worker`/`dynamic_worker.content` top-level attributes; `qc` omits them to mean runtime inheritance (`null`). When `qc.sandbox = sandbox.<name>` is present, `golden` scans nested block expressions during graph construction and creates the DAG edge to `sandbox.<name>`.

## 3. `artifact` & `import_artifact` Path Representation, Overlap Validation & Consumers

- **Inspected**: `internal/worker/spec/{artifact.go,schema.go,deferred.go,dynamic.go}`, `internal/planner/{export.go,worker_test.go,module_test.go}`.
- **Findings**:
  - Currently `effectiveImportPath` returns `".sofa/imports/" + imported.Name` when `path` is omitted, and `decodeNestedDeclarations` checks exact map equality across a single shared `paths` map for both `artifact` and `import_artifact`.
  - Stage 2 mounts `artifact` at `/.sofa/artifacts/<rel>` and `import_artifact` at `/.sofa/imports/<rel_or_name>`. Thus:
    - Relative paths are stored in `Artifact.Path` and `ImportArtifact.Path` (via `effectiveImportRelPath`), and container paths come from `ContainerPath()`.
    - Intra-set overlap checks must use separate maps for `artifact` and `import_artifact` and reject both exact matches (`a == b`) and parent-child prefix overlaps (`strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")`).
    - Adding `"rel_path": cty.String` to `artifactValueType` affects `internal/planner/module_test.go` (`variable "spec_artifact"` object constraint), which must declare `rel_path = string` to match `worker.specify.artifact.spec`.
    - `buildDeferredExpression` in `internal/worker/spec/deferred.go` currently defaults `import_artifact.<name>.path` to `".sofa/imports/" + name` and reads `resolved.Path`; it must be updated to use `ContainerPath()` (`"/.sofa/imports/..."`).
