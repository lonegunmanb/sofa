# Contract: `internal/applyeval` Pure Core Evaluator (`027-apply-deferred-evaluator`)

## 1. Evaluation Context & Runtime Value Builders (`context.go`)

- `BuildEvalContext(planned *plan.Plan, state RuntimeState, opts ...EvalOption) (*hcl.EvalContext, error)`:
  - Constructs `*hcl.EvalContext` strictly from `planned` and `state` (zero host `.sofa.hcl` reads).
  - Reuses `config.Functions(baseDir)` directly.
  - Projects static referenceable blocks (`source`, `preserve`, `sandbox`, `harness`, `model_provider`, `mcp_server`, `go_tool`, `external_tool`), inherited parent singletons (`WithParentPlans`), and runtime node states (`worker`, `dynamic_worker`, `merge`, `module`), supporting `for_each`-indexed addresses (`kind.name[key]`).
  - Topologically evaluates `planned.LocalExpressions()` over `local.<dep>` edges, preserving `golden.SensitiveMark` paths and reporting errors on cycles or runtime evaluation errors once upstreams are completed.
- Public `cty.Value` builders:
  - `SourceRuntimeValue(node plan.NodeSpec, state SourceRuntimeState) cty.Value`
  - `WorkerRuntimeValue(node plan.NodeSpec, res WorkerRuntimeResult) cty.Value`
  - `DynamicWorkerRuntimeValue(node plan.NodeSpec, res DynamicWorkerRuntimeResult) cty.Value`
  - `MergeRuntimeValue(node plan.NodeSpec, res MergeRuntimeResult) cty.Value`
  - `ModuleRuntimeValue(outputs map[string]cty.Value) cty.Value`

## 2. Node, Module & Output Materialization (`eval.go`)

- `EvalReadyWorker(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (workerspec.Config, error)`:
  - Binds `each` from `node.Config.GetAttr("each")` when non-null and materializes `worker` node into a wholly-known `workerspec.Config` (evaluating `DeferredExpression` with worker-local `artifact(name)` and `import_artifact`, validating branch names and block references, resolving `SourceURL` when empty at Plan time, and preserving `golden.SensitiveMark`).
- `EvalDynamicWorkerKeys(ctx *hcl.EvalContext, node plan.NodeSpec) ([]string, error)`:
  - Evaluates `ForEachExpr` (or known `ForEach`), rejects `null`, non-wholly-known, null `set(string)` elements, or non-map/object/string-set values, and returns deterministically sorted `[]string{}` (`len == 0` when empty, nil error).
- `EvalDynamicWorkerCollection(ctx *hcl.EvalContext, node plan.NodeSpec) ([]string, map[string]cty.Value, error)`:
  - Returns sorted keys plus per-key `cty.Value` elements preserving `SensitiveMark` paths.
- `ProjectDynamicInstance(ctx *hcl.EvalContext, node plan.NodeSpec, key string, elemVal cty.Value, opts ...EvalOption) (workerspec.Config, error)`:
  - Binds `IteratorName` (`"worker"` merging `key` & `value` without clobbering static workers, or custom `iterator`), attaches worker-local `artifact(name)` and `import_artifact`, evaluates `ContentTemplate`, resolves `SourceURL`, and returns a wholly-known `workerspec.Config`.
- `EvalMergeBase(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (MaterializedMerge, error)`:
  - Binds `each` from `node.Config.GetAttr("each")` when non-null, evaluates `BasedSourceExpr` (requiring wholly known valid branch), `SourceURL`, `TargetBranch`, `Strategy`, `MaxReworks`, `HumanApproval`, `Sandbox`, `Gatekeepers`, and inspects `SourcesExpr`/`Sources`: sets `SourcesKnown = true` and validates `Sources` when wholly known, or `SourcesKnown = false` and `Sources = nil` when `sources` still contains unknowns (supporting streaming `dynamic_worker -> merge`).
- `EvalReadyMerge(ctx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (MaterializedMerge, error)`:
  - Calls `EvalMergeBase` and enforces `SourcesKnown == true` (returning `"sources must be wholly known during apply"` otherwise).
- `EvalModuleInputsAndValidate(parentCtx *hcl.EvalContext, node plan.NodeSpec, opts ...EvalOption) (BoundModule, error)`:
  - Binds `each` from `node.Config.GetAttr("each")` when non-null, evaluates deferred `ModuleSpec.InputExprs` (preserving already wholly-known `for_each` inputs), binds into child `var` map with defaults and `sensitive` marks, executes `ModuleSpec.DeferredValidations` & `childPlan.DeferredValidations()` (returning exact `ErrorMessage` on failure), and returns updated immutable child `*plan.Plan`.
- `EvalPlanOutputs(planned *plan.Plan, ctx *hcl.EvalContext) (map[string]cty.Value, error)`:
  - Evaluates `planned.Outputs()` (preserving wholly-known Plan values, evaluating deferred `Expression`), re-applying `corespec.MarkSensitive` when `OutputSpec.Sensitive` is `true` while retaining expression-propagated `SensitiveMark` paths.
