# Research: Apply-Time Deferred Evaluator (`027-apply-deferred-evaluator`)

## Findings from Upstream Codebase Inspection

1. **Function Table (`internal/config/functions.go`)**:
   - `config.Functions(basedir)` returns `hclfuncs.Functions(basedir)` augmented with `cwd`, `local_timestamp`, `one`, `jsondecodewithtype`, and `tool_name`. `internal/applyeval` reuses `config.Functions(baseDir)` directly with zero re-implementation.
2. **Plan Persistence & Encoded Schemas (`internal/plan/`, `internal/planner/export.go`)**:
   - `Plan.Context()` persists ONLY `"var"`, `"local"`, and `"path"` (`export.go` lines 162–169), preserving `golden.SensitiveMark` paths via `plan/codec.go`.
   - `Plan.LocalExpressions()` preserves raw HCL expressions for every `local.<name>`, and `Plan.Outputs()` preserves `OutputSpec{Value, Description, Sensitive, Expression, SourceURLs}`.
   - `Plan.Nodes()` stores `node.Config` with slightly different schemas than `Block.Value()` for static referenceable blocks:
     - `sandbox` `node.Config` stores `{ runtime, image, network, mounts, resources }`, whereas `SandboxBlock.Value()` exposes `{ address (canonical), kind = "sandbox", name, runtime, image, network }`.
     - `model_provider` `node.Config` stores `RetryOverride` (nanoseconds, nullable), whereas `ModelProviderBlock.Value()` exposes `{ address (canonical), kind = "model_provider", name, retry (merged RetryPolicy in seconds) }`.
     - `mcp_server` `node.Config` stores `{ name, runtime_name, transport, tools, resources, ... }`, whereas `ServerBlock.Value()` exposes `{ address, kind = "mcp_server", name, tool_ids, resource_ids }` derived deterministically via `sha256("sofa/mcp-tool-id/v1\x00" + runtime_name + "\x00" + tool)`.
     - `go_tool` / `external_tool` `node.Config` omit `kind`, whereas `GoToolBlock.Value()` / `ExternalToolBlock.Value()` expose `kind = "go"` / `"external"`, `id`, `address`, plus tool fields.
     - `harness`, `source`, `preserve` map cleanly from `node.Config` + `node.Address` / `node.CanonicalAddress`.
3. **Worker `DeferredExpression` vs `node.Config` (`internal/worker/spec/deferred.go`, `schema.go`, `artifact.go`, `dynamic.go`)**:
   - `buildDeferredExpression` serializes an HCL object expression containing worker attributes and nested blocks:
     - `retry` in `DeferredExpression` uses `interval_seconds` and `max_interval_seconds` (integer seconds), whereas `encodeWorkerConfig` in `export.go` stores `interval_nanoseconds` and `max_interval_nanoseconds`.
     - `import_artifact.<name>.path` in `DeferredExpression` is written as `ContainerPath()` (`/.sofa/imports/...`), whereas `workerspec.ImportArtifact.Path` expects the relative path (stripped of `/.sofa/imports/` or read from `node.Config.GetAttr("imports")`) so `ImportArtifact.ContainerPath()` does not double-prefix `/.sofa/imports/`.
     - Worker-local evaluation context (`addWorkerLocals`) injects function `artifact(name)` returning `artifactValue` (`{ id, name, kind = "artifact", type, path = "/.sofa/artifacts/...", rel_path, description, required, non_empty }`) and variable `import_artifact` mapping `<name> -> { path = "/.sofa/imports/...", desc }`.
     - `WorkerBlock.Values()` exposes `work_branch`, `commit_sha`, `artifact`, and `result` (only when `hasTerminatingTool()` is true).
   - `DynamicWorkerBlock` (`dynamic.go`) sets `IteratorName` (`"worker"` by default, merging `key` & `value` into `Variables["worker"]` without clobbering static workers and rejecting static workers named `"key"` or `"value"`, or custom `iterator` binding `Variables[iteratorName]`), and produces `dynamic_worker.<name> = { address, kind = "dynamic_worker", name, workers = map(workerItemType), work_branches = list(string) }`.
4. **Child `module` Inheritance, `for_each`, & Deferred Validations (`internal/planner/module.go`, `internal/module/spec/`)**:
   - Child modules inheriting root/parent singleton `sandbox`, `harness`, or `model_provider` have injected traversals (e.g., `sandbox = sandbox.go_env`) in `DeferredExpression` without a local block in child `Plan.Nodes()`. Passing parent plans (`WithParentPlans(...)`) allows `BuildEvalContext` to propagate inherited singletons identically to `outgoingInheritedSingletons`.
   - `module` blocks using `for_each` have `each.key` / `each.value` inputs already wholly known in `ModuleSpec.Inputs`, while `ModuleSpec.InputExprs` still holds raw `"each.key"` text; keeping already wholly-known `ModuleSpec.Inputs` avoids false failures on `each`.
   - Child `variable` `validation` blocks deferred at Plan time reside in `ModuleSpec.DeferredValidations` and `childPlan.DeferredValidations()`, evaluated via `corespec.Condition.Evaluate(map[string]cty.Value{"var": boundVarObj}, fns)`.
5. **Merge (`internal/merge/spec/schema.go`)**:
   - `MergeBlock` persists `based_source_expr` and `sources_expr` alongside `target_branch`, `strategy`, `max_reworks`, `human_approval`, `sandbox`, and `gatekeepers`. At Apply time, `sources` may evaluate to `[]string{}` when a `dynamic_worker` expands to `0` elements.
