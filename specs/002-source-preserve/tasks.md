---

description: "Task list for Source Repositories and Preserved Branches"
---

# Tasks: Source Repositories and Preserved Branches

**Input**: Design documents from `specs/002-source-preserve/`

**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/`, `quickstart.md`

**Tests**: Mandatory per the repository constitution. Every story begins with failing tests.

**Organization**: Tasks are grouped by user story and follow the required Red → Green → Refactor flow.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel when using a different file with no incomplete dependency
- **[Story]**: Maps to the corresponding user story
- Every task includes an exact repository-relative file path

## Phase 1: Setup (Shared Infrastructure)

- [X] T001 Create the `internal/source` package skeleton and test directories required by `plan.md`; do not add production behavior.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Establish the shared branch-reference primitive used by both blocks.

### Tests for BranchRef

- [X] T002 [P] Write failing tests for known and unknown string branches, null/non-string rejection, expression retention, and branch-name edge cases in `internal/spec/branch_ref_test.go`, covering `data-model.md` entities `BranchRef.value` and `BranchRef.expression`.

### Implementation for BranchRef

- [X] T003 Implement `BranchRef`, branch-name validation, and expression capture in `internal/spec/branch_ref.go` against `data-model.md` fields `BranchRef.value` and `BranchRef.expression`.

**Checkpoint**: Shared branch semantics are green.

---

## Phase 3: User Story 1 - Declare a valid Git source (Priority: P1) 🎯 MVP

**Goal**: Accept and validate supported repository declarations and publish a stable source value with a deferred commit revision.

**Independent Test**: Plan each supported URL form and every invalid URL, branch, credential-reference, and mutual-exclusion case.

### Tests for User Story 1

- [X] T004 [P] [US1] Write failing table tests for supported and rejected repository addresses, source branches, credential-reference names, and mutual exclusion in `internal/source/schema_test.go`, covering `data-model.md` fields `SourceConfig.url`, `SourceConfig.branch`, `SourceConfig.token_ref`, and `SourceConfig.ssh_key_ref`.
- [X] T005 [P] [US1] Write failing planner tests for offline acceptance, unknown `commit_sha`, strict schema errors, and downstream readability in `internal/planner/source_test.go`, covering `data-model.md` fields `Source.address`, `Source.kind`, `Source.name`, `Source.url`, `Source.branch`, and `Source.commit_sha`.

### Implementation for User Story 1

- [X] T006 [US1] Implement `SourceConfig.Validate`, `SourceBlock`, strict BaseDecode checks, and stable `Value()` publication in `internal/source/schema.go` against the source contract in `contracts/source-block.md`.
- [X] T007 [US1] Register `SourceBlock` from `internal/planner/config.go` without introducing dependencies from `internal/config` to source packages.
- [X] T008 [US1] Run focused source tests, correct diagnostics, and verify that planning never reads referenced credential environment variables or launches Git.

**Checkpoint**: US1 passes independently.

---

## Phase 4: User Story 2 - Preserve a known or deferred branch (Priority: P1)

**Goal**: Decode literal and deferred string branches, preserve expressions, and publish a stable preserve value.

**Independent Test**: Plan known valid/invalid branches, unknown string references, and unknown non-string references.

### Tests for User Story 2

- [X] T009 [P] [US2] Write failing custom-decode tests for known/invalid/unknown branches, exact type rules, expression retention, unknown attributes, nested blocks, and required URL/branch fields in `internal/source/schema_test.go`, covering `data-model.md` fields `Preserve.address`, `Preserve.kind`, `Preserve.name`, `Preserve.url`, `Preserve.branch`, and `Preserve.branch_expr`.
- [X] T010 [P] [US2] Write failing planner tests proving an unknown string branch plans successfully and creates a dependency while an unknown number branch fails in `internal/planner/preserve_test.go`, following `contracts/preserve-block.md`.

### Implementation for User Story 2

- [X] T011 [US2] Implement `PreserveBlock` with `golden.CustomDecode`, strict URL decoding, `BranchRef` decoding, schema rejection, and stable `Value()` publication in `internal/source/schema.go`.
- [X] T012 [US2] Register `PreserveBlock` from `internal/planner/config.go` and run the focused preserve tests.
- [X] T013 [US2] Verify DAG edges for known and unknown branch references without replacing golden's graph traversal.

**Checkpoint**: US1 and US2 pass independently and together.

---

## Phase 5: User Story 3 - Infer the single root source URL (Priority: P1)

**Goal**: Resolve omitted preserve URLs with the 0/1/≥2 source matrix and inject a traversal into both HCL ASTs before adaptation.

**Independent Test**: Plan omitted URL cases with zero, one, and two sources, plus explicit URL with two sources, and inspect dependency edges.

### Tests for User Story 3

- [X] T014 [P] [US3] Write failing loader tests proving Root defaults are not applied during directory loading and real syntax diagnostics remain visible in `internal/config/loader_test.go`.
- [X] T015 [P] [US3] Write failing root-default matrix tests and dependency assertions in `internal/planner/defaults_test.go`, covering `data-model.md` entity `RootSourceSet.sources`.

### Implementation for User Story 3

- [X] T016 [US3] Keep `internal/config/loader.go` free of Root singleton behavior and business block dependencies.
- [X] T017 [US3] Implement `applyRootDefaults` in `internal/planner/defaults.go`, rebuilding affected blocks with `golden.CloneHclSyntaxBlock`, `golden.NewHclBlock`, and both syntax/write traversals.
- [X] T018 [US3] Make `SofaConfig.Init` apply Root defaults to a fresh block slice before `golden.InitConfig`, leaving `Loaded.Blocks` unchanged.
- [X] T019 [US3] Run the 0/1/≥2 matrix, explicit-URL, dependency-edge, and repeated-plan tests; fix only implementation defects.

**Checkpoint**: All three stories pass, including the singleton graph edge.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T020 Run `gofmt` and `go vet ./...`; resolve all formatting and static-analysis findings.
- [X] T021 Run `go test ./... -count=1`, all 001 regressions, and the 002 quickstart scenarios.
- [X] T022 Confirm no network, Git subprocess, or credential environment access occurs during focused source/preserve tests.
- [X] T023 Verify `internal/config` still has no dependency on any sofa business block package.
- [X] T024 Mark all completed tasks `[X]`, confirm every FR/SC is covered, and verify a clean worktree after the slice commit.
- [X] T025 Commit all 002 specification, source, test, and modified shared files and push them to `origin/main` before starting 003.

---

## Dependencies & Execution Order

- **Setup** starts immediately.
- **Foundational BranchRef** blocks both block implementations.
- **US1 Source** depends on BranchRef.
- **US2 Preserve** depends on BranchRef and may use Source only in integration tests.
- **US3 Root inference** depends on Source and Preserve registration plus both AST representations from the loader.
- **Polish and commit/push gate** depends on all stories.

## Parallel Opportunities

- T002 is the only foundational test file.
- T004 and T005 use different files and can be authored in parallel.
- T009 and T010 use different files and can be authored in parallel.
- T014 and T015 use different files and can be authored in parallel.

## Implementation Strategy

1. Complete BranchRef.
2. Deliver Source as the MVP.
3. Add Preserve and deferred branch behavior.
4. Add singleton URL inference and graph dependency.
5. Run full gates, commit, push, and verify the remote before starting slice 003.
