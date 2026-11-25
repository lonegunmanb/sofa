---

description: "Task list for Configuration Foundation and Outputs"
---

# Tasks: Configuration Foundation and Outputs

**Input**: Design documents from `specs/001-golden-config-output/`

**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`

**Tests**: Mandatory. The repository constitution overrides the general template's optional-test language. Every story starts with failing tests.

**Organization**: Tasks are grouped by user story so each story can be independently verified.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel because it uses a different file and has no incomplete dependency
- **[Story]**: Maps to the user story in `spec.md`
- Every task includes an exact repository-relative file path

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Pin dependencies and establish the package tree without production behavior.

- [X] T001 Add and pin the dependencies listed in `plan.md` by updating `go.mod` and `go.sum`; run `go mod tidy` and confirm module resolution succeeds.
- [X] T002 Create empty package directories for `internal/spec`, `internal/config`, `internal/module/spec`, and `internal/planner` using package declaration files where Go requires them; do not add production behavior.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Shared cty and base-configuration primitives required by every story.

**Critical**: Complete this phase before story implementation.

### Tests for Foundational primitives

- [X] T003 [P] Write failing tests for concrete-type validation, recursive sensitive-mark detection, and marking with the upstream sensitivity mark in `internal/spec/cty_test.go`; cover Nil, dynamic, capsule, nested collection/object, and unknown values.
- [X] T004 [P] Write failing tests for base-configuration construction, `sofa` DSL names, standard function availability, and helper-function overrides in `internal/config/base_test.go`.

### Implementation for Foundational primitives

- [X] T005 Implement `MarkSensitive`, `IsSensitive`, and recursive `ValidateType` in `internal/spec/cty.go` against `data-model.md` entities `Variable.type`, `Output.sensitive`, and `PlanningContext.path.module`, following `contracts/planning-functions.md`.
- [X] T006 Implement `NewBaseConfig` and the one-time `go-defaults` initialization guard in `internal/config/base.go`, using the fields and initialization order in `plan.md`.

**Checkpoint**: Shared primitives are green and no user story behavior has been written ahead of its tests.

---

## Phase 3: User Story 1 - Load and validate one configuration directory (Priority: P1) 🎯 MVP

**Goal**: Deterministically load all direct `*.sofa.hcl` files into one planning graph and return actionable errors for invalid directories and declarations.

**Independent Test**: Run the loader and planner over temporary directories containing ordered files, ignored files, empty input, top-level attributes, unsupported blocks, and malformed labels.

### Tests for User Story 1

- [X] T007 [P] [US1] Write failing tests for exact suffix filtering, ignored subdirectories, lexical order, original source retention, missing paths, non-directories, and empty directories in `internal/config/loader_test.go`, covering `data-model.md` entities `ConfigurationDirectory.path` and `ConfigurationFile.path`.
- [X] T008 [P] [US1] Write failing tests for top-level bare attributes, malformed HCL, block-order stability, and parse diagnostics in `internal/config/loader_test.go`, following `contracts/configuration-loading.md`.
- [X] T009 [US1] Write failing planner tests for an unregistered block and malformed label diagnostics in `internal/planner/config_test.go`, covering `data-model.md` entity `ConfigurationDeclaration.type`.

### Implementation for User Story 1

- [X] T010 [US1] Implement `SourceFile`, `Loaded`, and `LoadDirectoryContext` in `internal/config/loader.go` with deterministic dual parsing, suffix filtering, top-level attribute rejection, and source-preserving diagnostics.
- [X] T011 [US1] Implement the minimal `SofaConfig`, registration guard, `Init`, and `RunPlan` flow in `internal/planner/config.go`, delegating block wrapping, DAG construction, and traversal to `golden`.
- [X] T012 [US1] Refine loader and planner diagnostics until the red tests pass without swallowing syntax errors or panicking on malformed labels.

**Checkpoint**: US1 works independently and `go test ./internal/config ./internal/planner -count=1` passes.

---

## Phase 4: User Story 2 - Use native variables and locals with enforced types (Priority: P1)

**Goal**: Enforce concrete variable types, preserve established variable precedence, run native validation, reject orphan assignments, and propagate sensitivity through locals to outputs.

**Independent Test**: Plan temporary configurations with defaults, environment and variable files, explicit assignments, invalid types, failed validation, orphan variables, and sensitive locals.

### Tests for User Story 2

- [X] T013 [P] [US2] Write failing tests for missing `type`, `any`, capsule/nil rejection, typed defaults, and sensitive-variable discovery in `internal/planner/variable_test.go`, covering `data-model.md` entities `Variable.type`, `Variable.default`, and `Variable.sensitive`.
- [X] T014 [P] [US2] Write failing tests for environment, default variable files, automatic variable files, explicit assignment precedence, validation messages, and orphan assignments in `internal/planner/variable_test.go`, covering `Variable.validations`; do not use `t.Parallel()` in environment tests.
- [X] T015 [US2] Write a failing end-to-end sensitive propagation test from a sensitive variable through a local to an output in `internal/planner/variable_test.go`.

### Implementation for User Story 2

- [X] T016 [US2] Implement the pre-init variable scan in `internal/planner/config.go`, recording sensitive names and rejecting missing, dynamic, Nil, and capsule types with source-aware errors.
- [X] T017 [US2] Override `SofaConfig.EvalContext` in `internal/planner/config.go` to mark sensitive variable values without replacing `golden` value-source or validation behavior.
- [X] T018 [US2] Run the focused variable tests, capture their failure reasons before T016/T017 where applicable, and make the minimal fixes required in `internal/planner/config.go`.

**Checkpoint**: US2 passes independently, including no interactive variable prompt and no `t.Parallel()` in environment-mutating tests.

---

## Phase 5: User Story 3 - Evaluate standard planning functions and module path (Priority: P2)

**Goal**: Provide `cwd`, `one`, `local_timestamp`, `jsondecodewithtype`, and normalized `path.module` in all planning contexts.

**Independent Test**: Evaluate function expressions directly and through planned locals, including known and unknown JSON inputs and a Windows-style module path.

### Tests for User Story 3

- [X] T019 [P] [US3] Write failing tests for `cwd`, `one` empty/singleton/multiple/null/non-collection/unknown cases, RFC3339Nano timestamps, and exact typed JSON decoding in `internal/config/functions_test.go`, following `contracts/planning-functions.md`.
- [X] T020 [P] [US3] Write failing integration tests for these functions and normalized `path.module` through `SofaConfig.EvalContext` in `internal/planner/functions_integration_test.go`, covering `data-model.md` entity `PlanningContext.path.module`.

### Implementation for User Story 3

- [X] T021 [US3] Implement the complete helper function map in `internal/config/functions.go`, preserving upstream functions and using a type-constraint argument for `jsondecodewithtype`.
- [X] T022 [US3] Inject `path.module` and the helper overrides in `internal/planner/config.go`, including normalization to `/` on Windows.
- [X] T023 [US3] Verify that unknown JSON input yields an unknown result with the exact declared object type and fix the implementation if cty inference differs.

**Checkpoint**: US3 passes independently and `jsondecodewithtype(unknown, type)` has exact static type.

---

## Phase 6: User Story 4 - Publish typed output declarations (Priority: P2)

**Goal**: Register sensitive, described, known, null, and unknown named outputs and expose stable snapshots while rejecting invalid schemas.

**Independent Test**: Plan configurations containing each valid output form and each invalid schema, then inspect names, values, types, descriptions, marks, and diagnostics.

### Tests for User Story 4

- [X] T024 [P] [US4] Write failing block tests for valid schema, defaults, unknown values, null values, sensitive marking, unknown attributes, unsupported nested blocks, and invalid description/sensitivity types in `internal/module/spec/output_test.go`, covering `data-model.md` entities `Output.value`, `Output.description`, and `Output.sensitive`.
- [X] T025 [P] [US4] Write failing end-to-end output snapshot tests and a locals/dependency cycle test in `internal/planner/config_test.go` and `internal/planner/dag_test.go`, following `contracts/output-block.md`.

### Implementation for User Story 4

- [X] T026 [US4] Implement `OutputBlock` and `Output` in `internal/module/spec/output.go` with single-label contract, `cty.Value` storage, schema validation, sensitivity marking, and stable snapshot fields.
- [X] T027 [US4] Register `OutputBlock` exactly once from `internal/planner` package initialization and ensure the registration order satisfies `go-defaults` initialization.
- [X] T028 [US4] Expose a deterministic `SofaConfig.Outputs()` snapshot in `internal/planner/config.go` and verify unknown output types are retained.

**Checkpoint**: All four stories pass independently; output values remain inspectable while formatted sensitive display is redacted.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Final validation, regression prevention, and reproducible handoff.

- [X] T029 Run `gofmt` over changed Go files and ensure no generated or temporary files are required by tests.
- [X] T030 Run `go vet ./...`, `go test ./... -count=1`, and the focused quickstart command; resolve all failures without weakening assertions.
- [X] T031 Run a repeated-load determinism check and inspect test logs to confirm no sensitive test value appears in formatted diagnostics.
- [X] T032 Mark every completed task `[X]` in `specs/001-golden-config-output/tasks.md`, confirm all requirements map to at least one test, and record the 001 completion gate in the final handoff.

---

## Phase 8: Review Remediation

**Purpose**: Close blocking defects found during post-implementation review without expanding Stage 1 scope.

- [X] T033 Add a subprocess cold-start regression test proving `LoadDirectoryContext` can adapt an `output` block before `NewSofaConfig` is called in a fresh process.
- [X] T034 Keep idempotent registration in `internal/planner/config.go`, initialize defaults first, and register all blocks from planner package initialization before callers load HCL.
- [X] T035 Add direct and end-to-end tests proving `one()` preserves collection-level and element-level sensitivity marks.
- [X] T036 Return `cty.DynamicVal` from an output before plan execution and report derived sensitivity in output snapshots.
- [X] T037 Leave malformed variable labels to `golden` diagnostics and add the missing output `value` schema test.
- [X] T038 Replace the misleading known-JSON test name and add an HCL expression test using an actual unknown string.
- [X] T039 Re-run formatting, vet, cold-start, focused sensitive-value tests, and the complete fast suite.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup**: Starts immediately.
- **Foundational**: Depends on Setup and blocks all stories.
- **US1**: Depends on Foundational.
- **US2**: Depends on US1 because it uses the complete loader/planner path; it remains independently testable through variable-focused fixtures.
- **US3**: Depends on US2 for the shared planner context and sensitive-variable behavior.
- **US4**: Depends on US2 and US3 for context and helper functions.
- **Polish**: Depends on all stories.

### Within Each Story

1. Write the listed tests.
2. Run them and record the expected failure.
3. Implement the minimum needed.
4. Re-run focused tests, then the complete suite.

### Parallel Opportunities

- T003 and T004 use different files and may run in parallel.
- T007 and T008 are initially parallel authoring tasks; because both edit `loader_test.go`, coordinate the final write.
- T013, T014, and T019 use different files and may run in parallel.
- T024 and T025 use different files and may run in parallel.
- No destructive or external-dependency operations are required.

## Implementation Strategy

### MVP

Complete Setup, Foundational, and US1. This yields deterministic offline loading and a minimal planning flow that rejects unsupported configuration.

### Incremental Delivery

1. US1: load and diagnose configuration.
2. US2: typed values and sensitivity.
3. US3: complete expression primitives.
4. US4: inspectable outputs and DAG cycles.

### Completion Gate

001 is complete only when `go vet ./...` and `go test ./... -count=1` pass, all 001 tasks are checked, and no later Stage 1 block family has been added prematurely.
