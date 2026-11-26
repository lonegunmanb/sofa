# Research: Recursive `module` Planning, Unknown Input Relay, and Orthogonal Singleton Inheritance

## 1. `golden` `SingleValueBlock` + `for_each` Behavior

- **Inspected**: `golden@v0.0.0-20261125162047-059cc8cb0d1d/base_config.go` (`EvalContext`, lines 75–86) and `block.go` (`SingleValues`, lines 200–209).
- **Finding**: `BaseConfig.EvalContext()` checks `if _, ok := sample.(SingleValueBlock); ok` and calls `SingleValues(bs)` which keys only by `b.Name()`. When a `ModuleBlock` uses `for_each`, expanded instances share `b.Name()` with distinct `b.Address()` (`module.audit[east]`), so `SingleValues` overwrites earlier instances and returns `cty.EmptyObjectVal` for empty `for_each` instead of `{ audit = {} }`.
- **Decision**: Track `for_each` module names during `SofaConfig.Init` and rebuild `ctx.Variables["module"]` for any `for_each` modules in `SofaConfig.EvalContext()` using the instance key from `mb.Address()`, defaulting empty expansions to `cty.EmptyObjectVal`.

## 2. `golden` `VariableBlock` + `typeexpr.Defaults` with `cty.UnknownVal`

- **Inspected**: `golden/variable.go` (`ExecuteBeforePlan`, `validationCheck`) and `hcl/v2@v2.25.0/ext/typeexpr/defaults.go` (`Defaults.Apply`).
- **Finding**:
  1. `typeexpr.Defaults.Apply(v)` checks `if !v.IsKnown() || v.IsNull() { return v }` at the top level and walks known object/map/slice containers without panicking on unknown leaf fields, applying missing optional attributes and refining non-null defaults safely.
  2. `golden.VariableBlock.validationCheck()` uses `gohcl.DecodeBody(nb.Body, ctx, &vb)` into `Condition bool`, which errors whenever `condition` evaluates to `cty.UnknownVal(cty.Bool)`.
- **Decision**: Perform full child variable binding, `nullable` handling, `Defaults.Apply`, and `convert.Convert` in `PlanChildModule`. When `!converted.IsWhollyKnown()` and the `variable` declares `validation` blocks, statically validate `error_message` and type-check `condition` against `var.<v> = converted`, fail fast on known `false`, record `corespec.Condition` in `Plan.DeferredValidations` when `condition` is unknown, and strip `validation` blocks from the cloned `variable` block handed to `golden.InitConfig`.

## 3. `golden` `depends_on` Injection & `PreConditionCheck`

- **Inspected**: `golden/block.go` (`verifyDependsOn`, lines 151–171) and `golden/base_block.go` (`PreConditionCheck`, lines 146–160).
- **Finding**:
  1. `verifyDependsOn` parses `dependsOn.ExprString()` (from `hclwrite` tokens) as `[addr1, addr2]`, while `dagWalker` walks `hclsyntax.TupleConsExpr` traversals. Both syntax and write bodies must be kept in sync when injecting parent singleton dependencies onto `module` blocks.
  2. `BaseBlock.setMetaNestedBlock()` collects static `precondition` blocks at wrap time before `CustomDecode` runs; `ModuleBlock` overrides `PreConditionCheck` when `dynamic "precondition"` blocks are present so dynamically expanded preconditions run via `golden`'s native `dagPlan` check.

## 4. `golden` `dagWalker` on Inherited Singletons

- **Inspected**: `golden/dag_walker.go` (lines 35–38).
- **Finding**: When `applyModuleDefaults` injects `sandbox = sandbox.<name>` onto a child `worker` whose parent provided the inherited singleton, `dagWalker` looks up `d.dag.GetChildren("sandbox.<name>")`, gets a not-found error because `sandbox.<name>` is not in the child DAG, and skips adding an edge (`continue`). Exposing `ctx.Variables["sandbox"] = { <name>: inheritedValue }` in `SofaConfig.EvalContext()` then resolves the reference cleanly during `WorkerBlock.Decode`.
