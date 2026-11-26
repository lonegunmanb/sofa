---

description: "Static worker implementation tasks"
---

# Tasks

- [X] T001 Write failing artifact/import context and path-safety tests in `internal/worker/spec/worker_test.go` and planner integration tests.
- [X] T002 Implement artifact/import models, defaults, collision checks, `artifact()` function, and import context.
- [X] T003 Write failing gatekeeper/QC/fallback schema tests in `internal/worker/spec/worker_test.go` and planner integration tests.
- [X] T004 Implement gatekeeper, QC, fallback, and tool-use validation models.
- [X] T005 Write failing WorkerBlock decode/value tests covering deferred values, conditional result, schema, and `for_each` in `internal/planner/worker_test.go`.
- [X] T006 Implement WorkerBlock custom decode and `Values()`, including deferred expression retention.
- [X] T007 Add planner tests for root defaults, explicit null preservation, worker namespace unwrapping, and dependency edges.
- [X] T008 Extend planner default injection for sandbox/harness/model provider and worker fallback ancestry validation.
- [X] T009 Register WorkerBlock and unwrap `worker` in SofaConfig EvalContext.
- [X] T010 Run full output/worker/for_each/fallback/prior-slice regressions and fix defects.
- [X] T011 Run gofmt, vet, full tests, dependency-boundary checks.
- [X] T012 Commit and push 005.
