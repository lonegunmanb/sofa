---
description: "Stage 3 Slice 027 TDD tasks: Apply-time deferred evaluator, dynamic_worker expansion, and child module validation (internal/applyeval)"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/applyeval/eval_test.go` covering:
  - End-to-end real `planner` compilation -> `plan.SaveArchive` / `plan.LoadArchive` `.sofaplan` round-trip -> **deleting the original `.sofa.hcl` directory** -> stepping through `BuildEvalContext`, `EvalReadyWorker` (with terminating tool, `artifact(...)`, `import_artifact`, `SensitiveMark`), chained `locals` (`jsondecodewithtype`), `EvalDynamicWorkerKeys`, `ProjectDynamicInstance`, `EvalReadyMerge` (consuming `dynamic_worker.work_branches`), and `EvalPlanOutputs` (including `sensitive = true` and sensitive `var` propagation);
  - `dynamic_worker` empty `for_each` (`{}` / `toset([])`) returning `[]string{}` with nil error, empty `workers` & `work_branches`, clean downstream `merge` + `output`; plus `null` collection, `null` `set(string)` element, and invalid type rejection;
  - Child `module` deferred `variable.validation` depending on parent `worker` unknown output: violating runtime input fails `EvalModuleInputsAndValidate` with the exact declared `error_message`, while valid runtime input materializes child `worker` and propagates child `module.<name>` outputs (preserving `sensitive` marks) into parent outputs;
  - Chained `locals` topological evaluation, `for_each` static `worker`/`module` blocks (including mixed `each.key`/`each.value` + Apply-time unknown expressions), `EvalMergeBase` streaming `dynamic_worker -> merge` readiness (`SourcesKnown == false` vs `true`), default `worker` iterator vs custom `iterator` name, and inherited parent singleton `sandbox`/`harness`/`model_provider` in child modules.
- [X] T002 [Green] Update `internal/planner/export.go` (`each` snapshot preservation) and implement `internal/applyeval/context.go` and `internal/applyeval/eval.go` (runtime `cty.Value` builders, `BuildEvalContext`, topological `locals`, `EvalReadyWorker`, `EvalDynamicWorkerKeys`, `EvalDynamicWorkerCollection`, `ProjectDynamicInstance`, `EvalMergeBase`, `EvalReadyMerge`, `EvalModuleInputsAndValidate`, `EvalPlanOutputs`), and verify `go test -v ./internal/applyeval/... -count=1` passes.
- [X] T003 [Refactor & Verify] Refactor any duplicated helpers in `internal/applyeval`, run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, confirm 100% pass, and mark tasks `[X]`.
