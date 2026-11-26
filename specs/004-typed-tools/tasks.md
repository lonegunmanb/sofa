---
description: "TDD task list for Stage 1 Slice 004: Typed Tools (go_tool and external_tool)"
---

# Tasks: Typed Tools (`go_tool` and `external_tool`)

**Input**: `specs/004-typed-tools/` (`spec.md`, `plan.md`, `data-model.md`, `contracts/typed-tools.md`)

**Tests**: Mandatory and written before production code for every component (Red → Green → Refactor).

## Phase 1: Setup

- [X] T001 Create package directory skeletons `internal/tool/gotool/` and `internal/tool/spec/`.

## Phase 2: Foundational Primitives (`internal/spec` and `Constraint`)

- [X] T002 [P] Write failing unit tests for `Condition.Evaluate` (known pass/fail, unknown deferral, undeclared variable root rejection, non-bool rejection, null/blank validation) in `internal/spec/condition_test.go`, covering `data-model.md` `Condition.Expression` and `Condition.ErrorMessage`.
- [X] T003 [P] Write failing unit tests for `Issue.Validate` and `ToolResponse[T].Validate` in `internal/spec/response_test.go`, covering `data-model.md` `Issue.Code`, `Issue.Message`, and `ToolResponse.Accepted`.
- [X] T004 [P] Implement `Condition.Evaluate` in `internal/spec/condition.go` (`Condition.Expression`, `Condition.ErrorMessage` per `data-model.md`) to turn T002 green.
- [X] T005 [P] Implement `Issue` and `ToolResponse[T]` in `internal/spec/response.go` (`Issue.Code`, `Issue.Message`, `Issue.ID`, `Issue.Path`, `Issue.RepairHint`, `ToolResponse.Accepted` per `data-model.md`) to turn T003 green.
- [X] T006 Write failing unit tests for `Constraint.Apply`, `Constraint.JSONSchema`, and `JSONSchema` (primitives, collections, maps, tuples, objects with `optional(...)` defaults, unknown/null/undeclared attribute rejection, unsupported dynamic/capsule rejection) in `internal/tool/spec/constraint_test.go`.
- [X] T007 Implement `Constraint` and `JSONSchema` in `internal/tool/spec/constraint.go` to turn T006 green.

## Phase 3: US1 — `go_tool` Offline AST Analysis & Block Schema (Priority: P1)

- [X] T008 [US1] Write failing AST analyzer tests in `internal/tool/gotool/analyze_test.go` covering `data-model.md` `GoToolBlock.Source` and `GoToolBlock.Dependencies`: valid nested `struct` + pointer optional fields + JSON tags, offline declared `module@version` dependencies without `go get`, syntax error line numbers, `package` clause / `func main` rejection, non-stdlib undeclared imports, missing/non-struct `Input`, missing `Output`, wrong `Invoke` signature, `chan`/`func`/`[]byte`/non-string-map-key/embedded/unexported/duplicate/recursive/imported-named/custom-JSON-method rejections, and invalid `dependencies` (`"-modfile=x"`, relative paths, whitespace, missing `@version`).
- [X] T009 [US1] Implement pure offline AST analyzer in `internal/tool/gotool/analyze.go` (`Analyze`, `AnalyzeWithDependencies`) to turn T008 green without `go get` or external compilation.
- [X] T010 [US1] Write failing `GoToolBlock` decode, plan, `dependencies = null`, always-analyze-without-postcondition, `postcondition` static pre-check (`input.typo`, non-bool, undeclared root), and `golden` meta-syntax (`for_each`, `depends_on`, `precondition`, `dynamic`) tests in `internal/tool/spec/schema_test.go`, covering `data-model.md` `GoToolBlock.Description`, `GoToolBlock.Source`, `GoToolBlock.Dependencies`, and `GoToolBlock.Postconditions`.
- [X] T011 [US1] Implement `GoToolBlock` (and shared `postcondition` decoding) in `internal/tool/spec/schema.go` to turn T010 green.

## Phase 4: US2 — `external_tool` Schema & Type Constraints (Priority: P1)

- [X] T012 [US2] Write failing `ExternalToolBlock` tests in `internal/tool/spec/schema_test.go` covering `data-model.md` `ExternalToolBlock.Description`, `ExternalToolBlock.Program`, `ExternalToolBlock.WorkingDir`, `ExternalToolBlock.InputConstraint`, `ExternalToolBlock.OutputConstraint`, and `ExternalToolBlock.Postconditions`: valid `object({...})` with `optional(...)`, non-object `input_type = string` rejection, `any` rejection, `working_dir = null` handling, nonexistent local `program` acceptance, blank/null/missing `program` rejection, `postcondition` static checks, and `golden` meta-syntax preservation.
- [X] T013 [US2] Implement `ExternalToolBlock` in `internal/tool/spec/schema.go` to turn T012 green.

## Phase 5: US3 — Deterministic Tool IDs, `tool_name()`, and Planner Integration (Priority: P1)

- [X] T014 [P] [US3] Write failing deterministic tool ID tests (`sofa/tool-id/v1`, `<= 64` char cap on long names, canonical-address differentiation, `Id()` + `BaseValues()["id"]` + `Value()["id"]` alignment) in `internal/tool/spec/identity_test.go`, covering `data-model.md` `GoToolBlock.Id` and `ExternalToolBlock.Id`.
- [X] T015 [P] [US3] Write failing `tool_name()` unit tests (valid `go`/`external`/`builtin` objects, outer mark preservation, unknown/null/invalid object/non-tool rejection) in `internal/config/functions_test.go`.
- [X] T016 [US3] Implement `internal/tool/spec/identity.go` (`sofa/tool-id/v1` SHA-256 + UUID formatting + name sanitization/truncation + `BaseValues` override) and wire into `GoToolBlock` / `ExternalToolBlock` in `internal/tool/spec/schema.go` to turn T014 green.
- [X] T017 [US3] Implement `tool_name()` in `internal/config/functions.go` to turn T015 green without importing `internal/tool`.
- [X] T018 [US3] Write failing planner integration tests (deterministic IDs across repeated plans, `tool_name(go_tool.x)` / `tool_name(external_tool.y)` in HCL outputs, invalid `go_tool` / `external_tool` / `postcondition` diagnostics, and cold-start subprocess test) in `internal/planner/tools_test.go` and `internal/planner/cold_start_test.go`.
- [X] T019 [US3] Register `&toolspec.GoToolBlock{}` and `&toolspec.ExternalToolBlock{}` in `internal/planner/config.go` (`RegisterBlocks`) to turn T018 green.

## Phase 6: Polish & Verification

- [X] T020 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, and verify `internal/config` has zero imports of `internal/tool` or `internal/planner` and zero `go get` / `os/exec` calls exist in `internal/tool/`.
- [X] T021 Verify all tasks in `specs/004-typed-tools/tasks.md` are marked `[X]` and leave working tree clean of temporary/untracked scratch files for Lead Reviewer inspection (do not commit/push).
