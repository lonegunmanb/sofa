# Implementation Plan: Apply-Time Deferred Evaluator (`027-apply-deferred-evaluator`)

**Branch**: `027-apply-deferred-evaluator` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement the pure core evaluation package `internal/applyeval` (plus Plan-time `each` snapshot preservation in `internal/planner/export.go`) used by `RunActor` (`028`) to evaluate deferred HCL expressions during Apply strictly from frozen `*plan.Plan` snapshots and runtime completion structs:
- `internal/planner/export.go`: Preserve Plan-time `each` (`{ key, value }`, including sensitivity marks) on `for_each`-expanded `worker`, `merge`, and `module` `node.Config` snapshots (`cty.NullVal(cty.EmptyObject)` when unset).
- `internal/applyeval/context.go`: `RuntimeState`, runtime completion structs (`SourceRuntimeState`, `ArtifactRuntimeState`, `WorkerRuntimeResult`, `DynamicWorkerRuntimeResult`, `MergeRuntimeResult`, `ModuleRuntimeResult`), public `cty.Value` builders, `EvalOption` (`WithBaseDir`, `WithPlan`, `WithParentPlans`), static block value reconstruction matching Plan-time schemas (including `for_each`-indexed blocks and inherited parent singletons), topological `Plan.LocalExpressions()` evaluation, and `BuildEvalContext`.
- `internal/applyeval/eval.go`: `EvalReadyWorker`, `EvalDynamicWorkerKeys`, `EvalDynamicWorkerCollection`, `ProjectDynamicInstance`, `EvalMergeBase`, `EvalReadyMerge`, `MaterializedMerge` (with `SourcesKnown`), `EvalModuleInputsAndValidate`, `BoundModule`, and `EvalPlanOutputs`.
- `internal/applyeval/eval_test.go`: Tier 1 TDD unit tests covering real `planner` + `.sofaplan` archive round-trip with deleted source directory, `dynamic_worker` empty/multi-element & custom/default iterator expansion, child `module` deferred `variable.validation` & nested outputs, and chained `locals` + `for_each` static blocks.

## Technical Context

**Language/Version**: Go 1.27 (`module github.com/lonegunmanb/sofa`)  
**Primary Dependencies**: `github.com/hashicorp/hcl/v2`, `github.com/hashicorp/hcl/v2/hclsyntax`, `github.com/lonegunmanb/golden`, `github.com/zclconf/go-cty/cty`, `internal/config`, `internal/plan`, `internal/worker/spec`, `internal/merge/spec`, `internal/provider`, `internal/spec`, `internal/artifact`.  
**Storage**: None (pure stateless computation over immutable `*plan.Plan` + in-memory `RuntimeState`).  
**Testing**: `go test -v ./internal/applyeval/... -count=1` (Tier 1 pure unit tests using real `planner` + `plan.SaveArchive` / `plan.LoadArchive` + deleted source directory).  
**Constraints**: PowerShell 5.1 (no `&&`); zero Dapr, zero network, zero `os/exec`, zero re-reading of host `.sofa.hcl` files; reuse `config.Functions` and `corespec` directly; preserve all `golden.SensitiveMark` paths; do NOT run `git commit` or `git push`.

## Constitution Check

- **I. Spec-First Delivery**: Spec Kit artifacts under `specs/027-apply-deferred-evaluator/` created prior to tests and implementation. PASS.
- **II. Strict TDD**: `tasks.md` mandates writing failing tests (`eval_test.go`) first, observing red failure, then implementing `context.go` and `eval.go` to green. PASS.
- **III. Pure Core & Real-Dependency Boundaries**: `internal/applyeval` is 100% pure deterministic logic with zero Dapr, container, Git CLI, or filesystem `.sofa.hcl` dependencies. PASS.
- **IV. Runtime Enforcement Over Prompt Trust**: Validates branch names, block references, `for_each` collection types, `import_artifact` sources, and deferred `variable.validation` rules deterministically in Go. PASS.
- **V. Single-Node & Distributed Parity**: Stateless evaluation over `plan.Plan` produces identical results across standalone and distributed `RunActor` modes. PASS.
- **VI. Research-First & Surgical Task Sizing**: Informed by direct inspection of `internal/config/functions.go`, `internal/plan/`, `internal/planner/`, `internal/worker/spec/`, `internal/merge/spec/`, `internal/source/`, `internal/sandbox/spec/`, `internal/harness/spec/`, `internal/provider/`, `internal/mcp/spec/`, `internal/tool/spec/`, `internal/module/spec/`, and `golden`; scoped solely to `internal/applyeval`. PASS.

## Project Structure

```text
specs/027-apply-deferred-evaluator/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/applyeval.md
└── tasks.md

internal/applyeval/
├── context.go    # RuntimeState, cty.Value runtime builders, static/inherited block values, topological locals, BuildEvalContext
├── eval.go       # EvalReadyWorker, EvalDynamicWorkerKeys, ProjectDynamicInstance, EvalReadyMerge, EvalModuleInputsAndValidate, EvalPlanOutputs
└── eval_test.go  # Tier 1 pure unit tests (end-to-end .sofaplan round-trip, empty/multi dynamic_worker, child module validation, chained locals)
```
