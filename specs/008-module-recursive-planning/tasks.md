---
description: "Recursive module planning, unknown input relay, and orthogonal singleton inheritance TDD tasks"
---

# Tasks

- [X] T001 [Red] Write failing unit tests in `internal/module/spec/module_test.go` covering `ModuleBlock.Decode` (required literal `source`, rejection of variable refs / empty / missing `source`, optional positive `parallelism` / `null`, optional positive duration `timeout` / `null`, safe label validation, `precondition` and `dynamic "precondition"` allowed vs unexpected nested blocks rejected, typed `cty.UnknownVal` and sensitive input preservation + `InputExprs`), `PreConditionCheck`, `ExecuteDuringPlan`, `Value()` (`cty.DynamicVal` pre-plan, `cty.EmptyObjectVal` for 0 outputs, `cty.ObjectVal` for outputs), and `Plan`/`ModulePlan`/`Output` deep `Clone()` isolation.
- [X] T002 [Green] Implement `internal/module/spec/output.go` (`SourceURLs`, `Clone`), `internal/module/spec/plan.go` (`Plan`, `ModulePlan`, `Clone`), and `internal/module/spec/module.go` (`ModuleBlock`, `ModulePlanRequest`, `ModulePlanner`) passing all `internal/module/spec` tests.
- [X] T003 [Red] Write failing unit tests in `internal/planner/module_test.go` and `internal/planner/cold_start_test.go` covering:
  - End-to-end cross-module unknown & artifact relay (`source` -> `worker` -> `module` -> child `worker`/`dynamic_worker`/`merge` -> `output` -> root `preserve`, verifying DAG edges and `PlannedModules()`);
  - Child `variable` binding matrix (undeclared input, missing required input, `nullable = false` vs `nullable = true` + `default`, host env/varfile isolation, `typeexpr.Defaults` on known and partially-unknown objects, incompatible unknown type, sensitive variable/input propagation, known `validation` failure, unknown `validation` static check + `DeferredValidations`);
  - Orthogonal singleton inheritance matrix (1/0/2 across `sandbox`, `harness`, `model_provider`, `model_provider = null` opt-out, child override + orthogonal parent inheritance, 2-level nesting `root -> outer -> inner`, cross-module missing/ambiguity diagnostics);
  - Multi-source cross-module `SourceURL` lineage & conflict detection;
  - Cross-module `CanonicalAddress` tool & MCP ID isolation and child `tool_name()`;
  - `ModulesDirectory` + `BundlePath` (`config`, `modules/audit`, `modules/audit/nested`), uninitialized/remote module errors, and local directory cycle detection;
  - `module` with `for_each`, `depends_on`, `precondition`, and `dynamic "precondition"`;
  - `TestColdStartLoadModuleBeforeNewSofaConfig` subprocess test.
- [X] T004 [Green] Extend `workerspec.ModuleRegistry` / `mergespec.ModuleRegistry` (`LookupVariableSourceURLs`, `LookupModuleOutputSourceURLs`) and implement recursive module planning, variable binding/deferred validation, orthogonal singleton inheritance, `CanonicalAddress`, `BundlePath`, and `for_each` namespace handling in `internal/planner/{config.go,defaults.go,module.go}` passing all `internal/planner` tests.
- [X] T005 [Refactor & Verify] Review diffs for duplication/edge cases, run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, and update `tasks.md` checkboxes `[X]`.
