# Requirements Quality Checklist: `027-apply-deferred-evaluator`

- [x] Zero Dapr, zero network, zero `os/exec`, and zero re-reading of host `.sofa.hcl` files enforced.
- [x] `config.Functions(baseDir)` reused directly without re-implementing `golden` or `hclfuncs` functions.
- [x] Exact Plan-time `cty.Value` shapes matched for static blocks, `for_each` blocks, inherited parent singletons, and runtime node results (`source`, `worker`, `dynamic_worker`, `merge`, `module`).
- [x] Topological `Plan.LocalExpressions()` evaluation and `golden.SensitiveMark` propagation specified.
- [x] `EvalReadyWorker`, `EvalDynamicWorkerKeys`, `ProjectDynamicInstance`, `EvalReadyMerge`, `EvalModuleInputsAndValidate`, and `EvalPlanOutputs` contracts specified.
- [x] Edge cases (`dynamic_worker` 0-element `for_each`, `null` rejection, child `module` deferred `variable.validation` error messages, sensitive outputs) covered.
- [x] Strict TDD task ordering (Red -> Green -> Refactor) documented in `tasks.md`.
