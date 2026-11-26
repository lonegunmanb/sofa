# Implementation Plan: Recursive `module` Planning, Unknown Input Relay, and Orthogonal Singleton Inheritance

**Branch**: `008-module-recursive-planning` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Implement `ModuleBlock`, `Plan`, and `ModulePlan` in `internal/module/spec`, wire recursive child module planning, `variable` binding/type-checking/deferred validation, orthogonal `sandbox`/`harness`/`model_provider` inheritance, `CanonicalAddress`, `BundlePath`-based `path.module`, `for_each` module namespace assembly, and cross-module `SourceURL` lineage in `internal/planner`, `internal/worker/spec`, and `internal/merge/spec`.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Existing Dependencies Consumed**: `github.com/lonegunmanb/golden`, `github.com/hashicorp/hcl/v2`, `github.com/hashicorp/hcl/v2/ext/typeexpr`, `github.com/zclconf/go-cty/cty`, `internal/module` (`Directory`, `IsLocalSource`), `internal/config` (`LoadDirectoryContext`), `internal/spec` (`Condition`, `ValidateType`, `MarkSensitive`, `IsSensitive`), `internal/worker/spec`, `internal/merge/spec`
- **Testing**: 100% offline Tier 1 unit tests (`internal/module/spec/module_test.go`, `internal/planner/module_test.go`, `internal/planner/cold_start_test.go`)

## Constitution Check

- **Principle I (Spec-First)**: Full Spec Kit artifacts authored under `specs/008-module-recursive-planning/` prior to implementation.
- **Principle II (Strict TDD)**: Failing Red tests written in `internal/module/spec/module_test.go`, `internal/planner/module_test.go`, and `internal/planner/cold_start_test.go` before production code.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `ModuleBlock` depends on `ModulePlanner` interface defined in `internal/module/spec`; `worker/spec` and `merge/spec` depend on `ModuleRegistry` interfaces; all planning remains side-effect-free against real temp directories.
- **Principle IV (Runtime Enforcement)**: Cross-module `SourceURL` repository lineage, `variable` types/`nullable`/validations, and `sandbox`/`harness` singleton requirements are enforced deterministically during `Plan`.
- **Principle VI (Research-First & Golden Reuse)**: `golden` `VariableBlock`, `OutputBlock`, `ExpandDynamicBlocks`, `MetaAttributeNames`, `MetaNestedBlockNames`, and DAG scheduling are reused directly. Inspected `golden@v0.0.0-20261125162047-059cc8cb0d1d` and `hcl/v2@v2.25.0/ext/typeexpr/defaults.go` prior to planning.

## Design Decisions

1. **`ModuleBlock` & `Plan` / `ModulePlan` (`internal/module/spec`)**:
   - `ModuleBlock` implements `golden.CustomDecode`, `golden.PlanBlock`, and `golden.SingleValueBlock`.
   - `Decode` validates single-segment safe labels (`workerspec.ValidateSafeLabel(..., true)` + control-char check), filters `golden.MetaAttributeNames` and `golden.MetaNestedBlockNames` (allowing only `precondition` and `dynamic "precondition"`, expanding `dynamic "precondition"` into `preconditions` checked by `PreConditionCheck`), validates static literal `source` (`len(Variables()) == 0`), optional positive `parallelism`, optional positive Go duration `timeout`, and evaluates all remaining attributes into `Inputs` + `InputExprs` + `inputAttrs` allowing typed unknowns and preserving marks.
2. **Child `variable` Binding & Deferred Validations (`internal/planner/module.go`)**:
   - Pass a guaranteed non-existent `VarConfigDir` (`filepath.Join(childDir, ".sofa-disabled-child-varfiles")`) and populate `CliFlagAssignedVariables` for every declared child `variable` so host `SOFA_VAR_*` and child `.sofavars` never leak.
   - Inspect child `variable` blocks (`type`, `nullable` defaulting to `true`, `sensitive` defaulting to `false`, `default`, `validation`), enforce undeclared/missing/nullability rules, apply `typeexpr.Defaults` + `convert.Convert` cleanly on both known and partially-unknown inputs, preserve sensitivity via `childConfig.sensitiveVariables`, and when `!converted.IsWhollyKnown()` statically type-check `validation` blocks, record `DeferredValidations`, and strip `validation` blocks from the cloned `variable` `HclBlock` passed to `golden.InitConfig`.
3. **Orthogonal Singleton Inheritance (`internal/planner/defaults.go`, `module.go`)**:
   - Track per-dimension `InheritedSingleton` (`SingletonNone`, `SingletonUnique`, `SingletonAmbiguous`) with canonicalized `"address"` for `sandbox`, `harness`, and `model_provider`.
   - In parent modules, inject unique local singletons into `module` `depends_on` (unless that singleton itself references `module.<name>`) so parent singletons plan before child modules even in parallel DAG runs.
   - In child modules, dimensions with local declarations `>= 1` resolve strictly locally and override outgoing inheritance; dimensions with `0` local declarations bind to unique ancestor singletons (populated into child `EvalContext()`), allow `model_provider = null`, or fail with canonical consumer errors on missing/ambiguous dimensions.
4. **Canonical Addressing, `BundlePath`, `for_each`, and `SourceURL` Lineage**:
   - `SofaConfig.CanonicalAddress` prepends `addressPrefix + "."` inside child modules; `for_each` keys are stripped when resolving `module.Directory` and computing `BundlePath` (`"modules/<labels...>"`).
   - `SofaConfig.EvalContext()` assembles `ctx.Variables["module"]` per-key objects for `for_each` modules (and `cty.EmptyObjectVal` for empty `for_each`).
   - Extend `workerspec.ModuleRegistry` and `mergespec.ModuleRegistry` with `LookupVariableSourceURLs` and `LookupModuleOutputSourceURLs`; trace `SourceURL`s into child `variable`s and out of child `output`s.

## Files

```text
internal/module/spec/{output.go,plan.go,module.go,module_test.go}
internal/worker/spec/schema.go
internal/merge/spec/schema.go
internal/planner/{config.go,defaults.go,module.go,module_test.go,cold_start_test.go}
specs/008-module-recursive-planning/{spec,plan,research,data-model,quickstart,tasks}.md
specs/008-module-recursive-planning/contracts/module-recursive-planning.md
specs/008-module-recursive-planning/checklists/requirements.md
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
