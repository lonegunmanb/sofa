# Contract: Schema & Mount Alignment (`010-schema-mount-alignment`)

## 1. `sandbox` Block Contract
- `entrypoint` is forbidden on `sandbox` blocks. Specifying `entrypoint` (with any value, including `null`) MUST fail during `golden.Decode` as an unsupported argument.
- Exported `sandbox` `NodeSpec.Config` MUST NOT contain an `"entrypoint"` attribute.

## 2. `worker.qc` / `dynamic_worker.*.qc` Contract
- `qc.sandbox`: optional `sandbox.<name>` block reference or `null`.
  - Omitted or `null` -> `QCConfig.Sandbox = cty.NullVal(cty.EmptyObject)` (inherits parent worker `sandbox`).
  - Non-null non-sandbox reference (e.g., `harness.local`) -> Plan error (`validateBlockReference`).
  - Valid `sandbox.<name>` -> creates DAG dependency edge from `worker.<w>` / `dynamic_worker.<dw>` to `sandbox.<name>`.
- `qc.profile`: optional non-blank string or `null`.
  - Omitted or `null` -> `QCConfig.Profile = ""`.
  - Blank string -> Plan error.
- Exported `worker.qc` / `dynamic_worker.content.qc` MUST contain `"sandbox"` and `"profile"`.

## 3. `artifact` & `import_artifact` Path Contract
- HCL `artifact.path` (required) and `import_artifact.path` (optional) MUST be clean relative sub-paths validated by `validateWorkspaceRelativePath(..., allowDot=false)`.
- `artifact("<name>").path` and `worker.<w>.artifact.<name>.path` MUST equal `"/.sofa/artifacts/<rel_path>"`; `.rel_path` MUST equal `"<rel_path>"`.
- `import_artifact.<name>.path` (and `DeferredExpression` `import_artifact.<name>.path`) MUST equal `"/.sofa/imports/<rel_or_name>"` (defaulting `<rel_or_name>` to `<name>` when `path` is omitted).
- Intra-`artifact` overlapping paths (`a == b`, `a` parent of `b`, or `b` parent of `a`) MUST fail; intra-`import_artifact` overlapping paths MUST fail; identical relative paths across `artifact` and `import_artifact` MUST succeed.
- Exported `worker.artifacts[name]` and `worker.imports[name]` MUST include both `"path"` (relative) and `"container_path"` (container-absolute under `/.sofa/{artifacts,imports}`).
