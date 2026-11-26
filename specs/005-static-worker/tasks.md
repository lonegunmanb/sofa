---
description: "Static worker remediation and validation tasks"
---

# Tasks

- [X] T001 Write initial artifact/import, gatekeeper, QC, fallback, and planner tests (prior baseline).
- [X] T002 Write failing Red tests in `internal/worker/spec/worker_test.go` for P0-1 (`evalAttr` error propagation), P0-2 (strict scalar/nullable/known/whole-int validation), P0-3 (missing attributes, `retry`, `tool_call_quota`, `allowed_tools`/`disallowed_tools`, strict nested whitelists), P0-4 (`fallback.artifacts = [artifact(...)]`), P0-5 (`sandbox`/`harness`/`model_provider` `kind` and knownness), P0-6 (`import_artifact` duplicates, `sources` types, self-artifact prohibition, `import_artifact.<name>.path` interpolation), P1-1 (`DeferredExpression` completeness and parseability), P1-4 (safe labels, unique `gatekeeper`/`tool_use`, path edges, blank `command[0]`), and P2 (`WorkerConfig()` deep-copy isolation).
- [X] T003 Write failing Red tests in `internal/planner/worker_test.go` and `internal/planner/cold_start_test.go` for P1-2 (`ExecuteDuringPlan()` tool/MCP registration, `tool_use.input` constraint checks, `based_source` `SourceURL` resolution), P1-3 (`fallback` with `locals`, `depends_on`, and `for_each` instances), P1-4 (`dynamic "gatekeeper"`), and the 4-stage `specify -> plan -> tasks -> implement` integration chain + `TestColdStartLoadWorkerBeforeNewSofaConfig`.
- [X] T004 [P0-1, P0-2, P0-5] Remove `evalAttr` and implement strict `cty` scalar, whole-integer, nullability, knownness, and `validateBlockReference` helpers in `internal/worker/spec/schema.go`.
- [X] T005 [P0-3, P0-4, P0-6, P1-4] Complete `Config`, `WorkerConfig() Config` (`Clone()`), `retry`, `tool_call_quota`, `allowed_tools`/`disallowed_tools`, `qc` (with `ModelProviderSet` and nested `retry`), `fallback.artifacts`, `import_artifact`, label/path validation, and native `dynamic` expansion across `internal/worker/spec/{artifact,gatekeeper,qc,fallback,schema,errors}.go`, removing dead structs `QCBlock`/`FallbackBlock`.
- [X] T006 [P1-1] Implement `internal/worker/spec/deferred.go` for complete, parseable HCL `DeferredExpression` serialization across all Apply-time unknown fields.
- [X] T007 [P1-2, P1-3] Implement `ModuleRegistry` on `SofaConfig` in `internal/planner/config.go`, fix `internal/planner/worker_fallback.go` (`locals`, `depends_on`, `for_each` instances), wire `WorkerBlock.ExecuteDuringPlan()` registration/input/`SourceURL`/fallback checks, and update `TestWorkerResultOnlyExposedWithTerminatingTool`.
- [X] T008 Verify all unit, planner, and cold-start tests pass Green, then run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`.
