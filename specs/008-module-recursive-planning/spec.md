# Feature Specification: Recursive `module` Planning, Unknown Input Relay, and Orthogonal Singleton Inheritance

**Feature Branch**: `008-module-recursive-planning`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Implementation slice 008 from `docs/stage1/008-module-recursive-planning.md`, `docs/hcl-contract.md`, and `docs/architecture-contract.md`.

## User Scenarios & Testing

### User Story 1 - `ModuleBlock` decoding, metadata constraints, and typed unknown input relay (`internal/module/spec`) (Priority: P1)

A workflow author declares `module "<name>"` blocks referencing child modules via static literal `source` strings, optional `parallelism` / `timeout`, `golden` meta-attributes (`for_each`, `depends_on`), and `precondition` / `dynamic "precondition"` blocks. Business input attributes on `module` accept both wholly-known values and typed `cty.UnknownVal` values (including composite objects/lists/maps containing unknowns such as `worker.<w>.work_branch`, `worker.<w>.result`, `worker.<w>.artifact.<name>`, `dynamic_worker.<dw>.work_branches`, and `merge.<m>.work_branch`) while preserving `golden.SensitiveMark` and recording `Inputs` and `InputExprs`. During `ExecuteDuringPlan()`, `ModuleBlock` delegates recursive planning via the `ModulePlanner` consumer interface and publishes the child module's planned outputs via `Value()` (`cty.DynamicVal` before planning; `cty.EmptyObjectVal` if zero outputs; `cty.ObjectVal` otherwise).

**Independent Test**: Unit-test `ModuleBlock.Decode`, `PreConditionCheck`, `ExecuteDuringPlan`, `Value`, `PlannedModule`, `Plan.Clone`, and `ModulePlan.Clone` in `internal/module/spec/module_test.go`, covering literal `source` enforcement, `parallelism` (`> 0` or `null`) and `timeout` (`time.ParseDuration > 0` or `null`) validation, nested block filtering (`precondition` and `dynamic "precondition"` allowed, others rejected), typed unknown/marked input retention, and deep-clone isolation.

### User Story 2 - Recursive child module planning, variable binding, and deferred validation (`internal/planner`) (Priority: P1)

`SofaConfig.PlanChildModule` resolves the child module directory from `ModulesDirectory` (`module.Directory`, stripping `for_each` `[key]` suffixes) or from local relative/absolute paths when `ModulesDirectory` is unset (enforcing cycle detection across local directory stacks and requiring `sofa init` for uninitialized or remote sources), binds every declared child `variable` strictly from caller inputs and variable defaults without leaking host `SOFA_VAR_*` env vars or child `.sofavars` files, enforces `nullable` and `typeexpr.Defaults` + `convert.Convert` compatibility for both known and unknown inputs, propagates `golden.SensitiveMark`, evaluates known `variable` `validation` blocks natively via `golden`, and statically type-checks unknown `variable` `validation` blocks while recording `DeferredValidations` for runtime evaluation.

**Independent Test**: End-to-end `RunPlan()` tests in `internal/planner/module_test.go` covering upstream `worker` -> `module` (with unknown `work_branch` + `artifact` + known parameters) -> child `worker` / `dynamic_worker` / `merge` -> child `output` -> root `preserve`, plus full matrix of variable binding errors (undeclared input, missing required input, `nullable = false` rejecting `null`, `nullable = true` + `default` substituting default, unknown object with `optional(..., default)`, incompatible unknown type, sensitive propagation, known validation failure, unknown validation deferral).

### User Story 3 - Orthogonal singleton inheritance, `CanonicalAddress`, `BundlePath`, and cross-module `SourceURL` lineage (`internal/planner`, `internal/worker/spec`, `internal/merge/spec`) (Priority: P1)

Parent and child modules resolve `sandbox`, `harness`, and `model_provider` independently per dimension: when a child declares `>= 1` blocks in a dimension it resolves strictly locally and exports its own status to grandchildren; when a child declares `0` blocks in a dimension, consumers inherit the unique ancestor singleton (exposing it in child `EvalContext()` with canonical `"address"`), allow `model_provider = null` opt-out, and fail with clear canonical consumer diagnostics on missing (`sandbox` / `harness`) or ambiguous (`>= 2`) inherited dimensions. `SofaConfig.CanonicalAddress` namespaces child blocks (including `for_each` instances) so `go_tool`, `external_tool`, and `mcp_server` deterministic IDs never collide across modules, `BundlePath` stabilizes `${path.module}` (`"config"` at root, `"modules/<labels...>"` in children), and `SourceURL` lineage propagates across `var.*` and `module.<name>[*].<output>` references.

**Independent Test**: Unit tests in `internal/planner/module_test.go` and `internal/planner/cold_start_test.go` covering 3-dimension orthogonal inheritance and override across 1- and 2-level module nesting, `model_provider = null` opt-out, cross-module ambiguity errors, multi-source `SourceURL` propagation & conflict rejection across module boundaries, cross-module tool/MCP deterministic ID isolation and `tool_name()`, `BundlePath` `"config"` / `"modules/..."`, `module` with `for_each` + `depends_on` + `precondition`, and cold-start subprocess planning.

### Edge Cases

- A `module` block with `for_each` (including `for_each = toset([])`) cannot rely solely on `golden`'s `SingleValues`, which keys by `b.Name()` only; `SofaConfig.EvalContext()` must assemble `ctx.Variables["module"][name]` as an object keyed by `for_each` instance keys (or `cty.EmptyObjectVal` when empty).
- Parent unique local singletons (`sandbox`, `harness`, `model_provider`) must execute before `module` blocks in the parent DAG so their planned `cty.Value` is ready when `ModuleBlock.ExecuteDuringPlan()` runs, while avoiding spurious cycle edges if a singleton block itself references a module.
- `golden.VariableBlock.validationCheck()` uses `gohcl.DecodeBody` into `Condition bool`, which rejects `cty.UnknownVal(cty.Bool)`. Cloned child `variable` blocks whose resolved input is `!IsWhollyKnown()` must have `validation` blocks statically checked and deferred in `SofaConfig`, then stripped from the clone passed to `golden.InitConfig` so `RunPrePlan()` succeeds.
- Host `SOFA_VAR_*` environment variables and child-directory `.sofavars` / `*.auto.sofavars` files must never override or leak into child module variable bindings.
- Cross-module `SourceURL` lineage must work even when root has 2+ `source` blocks and intermediate `local` / `var` / `module` outputs pass branches through nested modules.

## Requirements

- **FR-001**: `internal/module/spec` MUST define `Plan`, `ModulePlan`, `Output` (with `SourceURLs` and `Clone`), `ModulePlanRequest`, `ModulePlanner`, and `ModuleBlock` implementing `golden.CustomDecode`, `golden.PlanBlock`, and `golden.SingleValueBlock`.
- **FR-002**: `ModuleBlock.Decode` MUST validate single-segment safe module labels, enforce literal non-empty `source` without variable references, validate optional positive integer `parallelism` (`null` = unset) and positive Go duration `timeout` (`null` = unset), delegate meta-attribute and meta-nested-block filtering to `golden.MetaAttributeNames` / `golden.MetaNestedBlockNames` (allowing only `precondition` and `dynamic "precondition"`), and capture typed known/unknown/sensitive inputs and raw expressions.
- **FR-003**: `planner.RegisterBlocks()` MUST register `&modulespec.ModuleBlock{}`. `ConfigArgs` MUST support `ModulesDirectory` and `BundlePath`. `SofaConfig` MUST implement `modulespec.ModulePlanner`, `CanonicalAddress(string) string`, `PlannedModules() map[string]modulespec.ModulePlan`, and `BundlePath`-aware `path.module` + `for_each`-aware `module` values in `EvalContext()`.
- **FR-004**: `SofaConfig.PlanChildModule` MUST resolve child directories safely (`ModulesDirectory` via `module.Directory` with static `[key]`-stripped address, or local-only relative/abs paths with cycle detection), isolate child variable resolution from host env vars and child varfiles, validate `type`, `nullable`, `sensitive`, `default`, `typeexpr.Defaults`, `convert.Convert`, and statically check/defer `validation` blocks on unknown inputs.
- **FR-005**: `applyModuleDefaults` and `SofaConfig` MUST implement orthogonal per-dimension inheritance (`sandbox`, `harness`, `model_provider`) across arbitrary module depths, enforce parent singleton -> `module` DAG ordering, and expose inherited singletons in child `EvalContext()` with canonical `"address"`.
- **FR-006**: `workerspec.ModuleRegistry`, `mergespec.ModuleRegistry`, and `SofaConfig` MUST propagate `SourceURL` lineage through `var.<name>` and `module.<name>[*].<output>` expressions so cross-module multi-repository merges are validated accurately.

## Success Criteria

- **SC-001**: All unit tests in `internal/module/spec/module_test.go`, `internal/planner/module_test.go`, and `internal/planner/cold_start_test.go` pass deterministically offline.
- **SC-002**: `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` pass with zero warnings or errors.
