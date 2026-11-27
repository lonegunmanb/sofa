# Feature Specification: Apply-Time Deferred Evaluator, `dynamic_worker` Expansion & Child `module` Validation (`027-apply-deferred-evaluator`)

**Feature Branch**: `027-apply-deferred-evaluator`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `027` blueprint (`docs/stage3/027-apply-deferred-evaluator.md`), `docs/hcl-contract.md`, `internal/config/functions.go`, `internal/plan/`, `internal/planner/`, `internal/worker/spec/`, `internal/merge/spec/`, `internal/source/`, `internal/sandbox/spec/`, `internal/harness/spec/`, `internal/provider/`, `internal/mcp/spec/`, `internal/tool/spec/`, `internal/module/spec/`, and `internal/spec/`.

## User Scenarios & Testing

### User Story 1 - Offline Apply-Time `EvalContext` Construction & Chained `locals` Resolution (Priority: P1)

`RunActor` (`028`) executes an immutable `*plan.Plan` (typically unpacked from a `.sofaplan` archive after original host `.sofa.hcl` files are no longer present) and needs a deterministic `*hcl.EvalContext` populated with frozen Plan variables, `config.Functions(basedir)`, Plan-time static block reference objects, completed runtime node outputs, and topologically evaluated `Plan.LocalExpressions()`—preserving all `golden.SensitiveMark` sensitivity paths.

- `BuildEvalContext(planned, runtimeState, opts...)` builds `*hcl.EvalContext` strictly from `*plan.Plan` and `RuntimeState` without ever reading `.sofa.hcl` files from disk:
  - Seeds `var`, initial `local`, and `path` from `planned.Context()`, plus `config.Functions(baseDir)` (reusing `sofa`'s exact `hclfuncs` + `cwd`, `local_timestamp`, `one`, `jsondecodewithtype`, `tool_name` function table).
  - Projects static referenceable blocks in `planned.Nodes()` (`source`, `preserve`, `sandbox`, `harness`, `model_provider`, `mcp_server`, `go_tool`, `external_tool`) and any inherited parent-module singletons (`WithParentPlans(...)`) into their exact Plan-time `cty.Object` shapes (supporting `for_each`-indexed block addresses such as `worker.foo["k"]`, `merge.bar["k"]`, `module.sub["k"]`, etc.).
  - Injects completed runtime values (and Plan-time unknown placeholders for uncompleted nodes so independent branches remain evaluable):
    - `source.<name>`: `{ address, kind = "source", name, url, branch, commit_sha }`
    - `worker.<name>`: `{ address, kind = "worker", name, work_branch, commit_sha, artifact = object({ <art_name> = artifactValue }), [result = string (when worker declares a terminating tool)] }`
    - `dynamic_worker.<name>`: `{ address, kind = "dynamic_worker", name, workers = map(workerItemType), work_branches = list(string) }` ordered by sorted expansion keys `keys []string` (empty `[]string{}` yields `cty.MapValEmpty(workerItemType)` and `cty.ListValEmpty(cty.String)`)
    - `merge.<name>`: `{ address, kind = "merge", name, work_branch, commit_sha }`
    - `module.<name>`: `cty.ObjectVal` of child module evaluated outputs (preserving `golden.SensitiveMark`)
  - Evaluates `planned.LocalExpressions()` in topological `local.<dep>` dependency order, preserving `SensitiveMark` paths and returning clear errors on cyclic or invalid runtime expressions (e.g., malformed JSON in `jsondecodewithtype(worker.design.result, ...)` once upstream has completed).

**Independent Test**: Tier 1 unit tests in `internal/applyeval/eval_test.go` compiling real `.sofa.hcl` configs via `planner`, round-tripping through `.sofaplan` (`plan.SaveArchive` / `plan.LoadArchive`), deleting the original `.sofa.hcl` directory, and verifying `BuildEvalContext` + chained `locals` + `for_each` static blocks + `SensitiveMark` propagation.

### User Story 2 - Materializing `worker`, `dynamic_worker`, `merge` & `output` Nodes (Priority: P1)

When DAG dependencies satisfy readiness for a `worker`, `dynamic_worker`, `merge`, or top-level/child `output`, `RunActor` calls `internal/applyeval` to materialize Plan-time unknown expressions into wholly-known runtime execution configs:

- `EvalReadyWorker(ctx, node, opts...)` materializes a `worker` `plan.NodeSpec` into a wholly-known `workerspec.Config`:
  - Binds `each` (`each.key` / `each.value`, preserving sensitivity marks) from `node.Config.GetAttr("each")` when `node` is a `for_each` instance, so expressions mixing `each.*` with Apply-time unknowns evaluate identically to Plan time.
  - Evaluates `DeferredExpression` (when non-empty) inside a child `*hcl.EvalContext` equipped with worker-local `artifact("<name>")` function and `import_artifact` variable (matching `WorkerBlock.addWorkerLocals`).
  - Validates `based_source` (`ValidateBranchName`), `sandbox`, `harness`, optional `model_provider`, non-blank `system_prompt`, `import_artifact` sources, and resolves `SourceURL` from upstream/source nodes if empty at Plan time.
  - Preserves `golden.SensitiveMark` on `Prompt`, `SystemPrompt`, `ToolUses[i].Input`, `QC.Criteria`, `Sandbox`, `Harness`, `ModelProvider`, etc.
- `EvalDynamicWorkerKeys(ctx, node)` (and `EvalDynamicWorkerCollection(ctx, node)`) evaluates `ForEachExpr` (or known `ForEach`), validates that the collection is non-null, wholly known, and of type `map`, `object`, or `set(string)` (rejecting `null` collections, `null` set elements, or unsupported types), and returns deterministically sorted `[]string{}` (empty slice `[]string{}` with `nil` error when 0 elements).
- `ProjectDynamicInstance(ctx, node, key, elemVal, opts...)` binds `IteratorName` (`"worker"` merging `key` & `value` into `Variables["worker"]` without clobbering static workers and rejecting `"key"`/`"value"` static worker collisions, or custom `iterator` binding `Variables[iteratorName] = { key, value }`), preserves element sensitivity marks, attaches worker-local `artifact(...)` and `import_artifact`, and materializes `ContentTemplate` into a wholly-known `workerspec.Config`.
- `EvalMergeBase(ctx, node, opts...)` and `EvalReadyMerge(ctx, node, opts...)` bind `each` when `node` is a `for_each` instance and evaluate `BasedSourceExpr` and `SourcesExpr` into `MaterializedMerge` (`BasedSource`, `SourceURL`, flattened/validated candidate branch slice `[]string`, `SourcesKnown`, `TargetBranch`, `Strategy`, `MaxReworks`, `HumanApproval`, `Sandbox`, `Gatekeepers`):
  - `EvalMergeBase` supports streaming `dynamic_worker -> merge` dispatch in `RunActor` (`028`): when `based_source` is wholly known while `sources` still contains `cty.UnknownVal` (e.g. pending sibling `dynamic_worker` instances), it returns `SourcesKnown = false` and `Sources = nil` without error; when `sources` is wholly known, it populates `Sources` and sets `SourcesKnown = true`.
  - `EvalReadyMerge` delegates to `EvalMergeBase` and returns an error when `!SourcesKnown`.
- `EvalPlanOutputs(planned, ctx)` evaluates `planned.Outputs()` (keeping already wholly-known `OutputSpec.Value`, otherwise parsing & evaluating `OutputSpec.Expression`), applying `corespec.MarkSensitive` when `OutputSpec.Sensitive` is true and preserving expression-propagated `SensitiveMark` paths.

**Independent Test**: Tier 1 unit tests stepping through `worker.design` (with terminating tool + artifact) -> chained `locals` -> `dynamic_worker.coding` (both multi-element and 0-element `for_each`, custom & default iterator, `import_artifact`, `artifact(...)`) -> `merge.int` -> `output` (sensitive & non-sensitive) from an unpacked `.sofaplan` after source deletion.

### User Story 3 - Child `module` Deferred Inputs & `variable.validation` Execution (Priority: P1)

When a `module "<name>"` node's upstream dependencies complete, `EvalModuleInputsAndValidate(parentCtx, node, opts...)` materializes all deferred `ModuleSpec.InputExprs`, merges them with child defaults and `sensitive` variable declarations, and executes `ModuleSpec.DeferredValidations` (and `childPlan.DeferredValidations()`):

- Preserves already wholly-known `ModuleSpec.Inputs` (including `for_each` `each.key`/`each.value` inputs expanded at Plan time).
- Evaluates deferred `InputExprs` against `parentCtx`, preserves upstream `SensitiveMark`s AND enforces child `sensitive = true` variable markings.
- Executes every deferred `corespec.Condition` against `{ var = boundVarObj }` with `config.Functions(baseDir)`, returning an error containing the exact declared `ErrorMessage` if any condition evaluates to `false` (or fails to resolve to a known boolean).
- Returns an updated immutable child `*plan.Plan` (with bound `Context["var"]`) and `BoundInputs` so child nodes (`EvalReadyWorker`, `EvalDynamicWorkerKeys`, `EvalReadyMerge`) and child `EvalPlanOutputs` execute cleanly and feed `module.<name>` outputs back into the parent `*hcl.EvalContext`.

**Independent Test**: Tier 1 unit tests compiling parent + child module with `validation` blocks referencing parent `worker` unknown outputs (`based_source` / `result`), verifying invalid runtime inputs fail with the exact declared `error_message` while valid inputs materialize child nodes and propagate child `module.<name>` outputs (preserving `sensitive` marks) into parent outputs.

### Edge Cases

- Empty `dynamic_worker` `for_each` (`{}` or `toset([])`): `EvalDynamicWorkerKeys` returns `[]string{}` and `nil` error; `DynamicWorkerRuntimeValue` produces `workers = cty.MapValEmpty(workerItemType)` and `work_branches = cty.ListValEmpty(cty.String)`; downstream `merge` with `sources = dynamic_worker.<name>.work_branches` (or `concat([...], dynamic_worker.<name>.work_branches)`) evaluates cleanly without error.
- Null `for_each`, null element inside `set(string)`, or non-map/object/string-set `for_each` returns a clear descriptive error.
- Static `worker` / `merge` / `module` blocks with `for_each` (e.g. `worker.os_matrix[linux]`, `module.region[east]`) project cleanly under `worker.<name>[<key>]` / `merge.<name>[<key>]` / `module.<name>[<key>]`.
- Child modules inheriting root/parent singleton `sandbox`, `harness`, or `model_provider` resolve those block references via `WithParentPlans(...)` even when the child `Plan.Nodes()` declares none locally.
- Cyclic or invalid `LocalExpressions` (once upstream values are known) fail deterministically with clear errors.

## Requirements

### Functional Requirements

- **FR-001**: `internal/applyeval` MUST be a pure, side-effect-free evaluation package (zero Dapr, zero network, zero `os/exec`, zero reading of host `.sofa.hcl` files) reusing `config.Functions` and `corespec` directly.
- **FR-002**: `BuildEvalContext` MUST construct an `*hcl.EvalContext` matching Plan-time `cty.Value` shapes for `var`, `path`, `local` (evaluated in topological order from `Plan.LocalExpressions()`), static referenceable blocks (`source`, `preserve`, `sandbox`, `harness`, `model_provider`, `mcp_server`, `go_tool`, `external_tool`), `for_each`-indexed blocks, inherited parent singletons (`WithParentPlans`), and completed runtime values (`source`, `worker`, `dynamic_worker`, `merge`, `module`), preserving all `golden.SensitiveMark` paths.
- **FR-003**: `SourceRuntimeValue`, `WorkerRuntimeValue`, `DynamicWorkerRuntimeValue`, `MergeRuntimeValue`, and `ModuleRuntimeValue` helpers MUST construct runtime `cty.Value` objects matching `docs/hcl-contract.md` and Plan-time schemas (including optional `result` on `worker` only when a terminating tool exists, `artifact` objects matching `artifactValueType`, and `workers`/`work_branches` on `dynamic_worker` ordered by sorted `keys`).
- **FR-004**: `EvalReadyWorker` MUST bind `each` when present on `for_each`-expanded `worker` nodes and materialize `worker` `plan.NodeSpec` (with or without `DeferredExpression`, including worker-local `artifact(name)` and `import_artifact` context, `SourceURL` resolution, and `SensitiveMark` preservation) into a wholly-known `workerspec.Config`.
- **FR-005**: `EvalDynamicWorkerKeys` MUST validate and deterministically sort `for_each` keys (returning `[]string{}` on empty collections, rejecting `null`/invalid types/null set elements); `ProjectDynamicInstance` MUST bind `"worker"` (without clobbering static workers) or custom `iterator` and materialize `ContentTemplate` into a wholly-known `workerspec.Config`.
- **FR-006**: `EvalMergeBase` MUST bind `each` when present on `for_each`-expanded `merge` nodes, resolve `BasedSource` and `SourceURL`, and evaluate `SourcesExpr`/`Sources` (setting `SourcesKnown = false` when `sources` still contains unknowns for streaming `dynamic_worker -> merge`, or validating `Sources` with `SourcesKnown = true` including empty `work_branches`); `EvalReadyMerge` MUST enforce `SourcesKnown == true`.
- **FR-007**: `EvalModuleInputsAndValidate` MUST bind `each` when present on `for_each`-expanded `module` nodes, evaluate deferred child `ModuleSpec.InputExprs`, preserve Plan-time wholly-known `for_each` inputs and child defaults/sensitivity marks, execute `ModuleSpec.DeferredValidations` + `childPlan.DeferredValidations()`, return the exact declared `ErrorMessage` on failure, and return an updated child `*plan.Plan` bound with the materialized `var` context.
- **FR-008**: `EvalPlanOutputs` MUST evaluate `Plan.Outputs()` (preserving wholly-known Plan values and evaluating deferred `OutputSpec.Expression` strings), preserving expression-propagated `SensitiveMark` paths and applying `corespec.MarkSensitive` whenever `OutputSpec.Sensitive` is `true`.

## Success Criteria

### Measurable Outcomes

- **SC-001**: `go test -v ./internal/applyeval/... -count=1` and `go test ./... -count=1` pass 100% offline with zero external processes or host `.sofa.hcl` reads.
- **SC-002**: End-to-end `.sofaplan` archive round-trip test deletes the original `.sofa.hcl` directory and evaluates `worker -> locals -> dynamic_worker -> merge -> module -> output` with exact field and `SensitiveMark` fidelity.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly.
