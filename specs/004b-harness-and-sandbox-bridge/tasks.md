---

description: "Tasks for 004b harness plugin block and sandbox bridge default"
---

# Tasks: Harness Plugin Block and Sandbox Bridge Default

**Input**: `specs/004b-harness-and-sandbox-bridge/` (`spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/`)

**Tests**: Mandatory and MUST be written first and observed failing (Red) before implementing production code (Green).

## Phase 1: Setup

- [X] T001 Create `internal/harness/spec` package directory and empty placeholder if needed.

## Phase 2: User Story 1 - Default Sandbox Network to Bridge (Priority: P1)

**Goal**: Change default `sandbox.network` from `"none"` to `"bridge"` when omitted or `null`, preserving `"none"` and `"host"`.

**Independent Test**: Run `go test ./internal/sandbox/spec ./internal/planner -run TestSandbox -count=1`.

### Tests for User Story 1 (Red First)

- [X] T002 [P] [US1] Add failing unit tests for omitted/`null` `Sandbox.network` defaulting to `"bridge"` and explicit `"none"`/`"host"` in `internal/sandbox/spec/schema_test.go`.
- [X] T003 [P] [US1] Update `TestSandboxPlansWithSafeDefaults` and add `network = null` and explicit `network = "none"` assertions in `internal/planner/sandbox_test.go`.

### Implementation for User Story 1 (Green)

- [X] T004 [US1] Update `Sandbox.network` default from `"none"` to `"bridge"` in `internal/sandbox/spec/schema.go` (per `data-model.md` `Sandbox.network`).

**Checkpoint**: US1 sandbox tests pass.

---

## Phase 3: User Story 2 - External Harness Block Schema & Planner Integration (Priority: P1)

**Goal**: Implement `HarnessBlock`, `HarnessConfig`, `SignatureMode`, strict validation, `Value()`, deep-copied `HarnessConfig()`, and register `HarnessBlock` in `planner`.

**Independent Test**: Run `go test ./internal/harness/spec ./internal/planner -run "TestHarness|TestColdStartLoadHarness" -count=1`.

### Tests for User Story 2 (Red First)

- [X] T005 [P] [US2] Write failing unit tests in `internal/harness/spec/schema_test.go` covering:
  - Local mode (both `source` and `version` omitted or `null`) for `"copilot"` and arbitrary custom names with zero special cases;
  - GitHub Releases mode (`source` + `version` paired, `SourceHost`/`SourceOwner`/`SourceRepo` parsing, valid SemVer pre-release/build);
  - Paired `source`/`version` mismatches, invalid `source` (schemes, whitespace, `.git`, `..`, wrong segment count, invalid host), invalid `version` (leading `"v"`/`"V"`, range operators, non-SemVer);
  - `signature` (`auto`/`attestation`/`pgp`/`none`, `null` -> `auto`) and `signing_key` (`pgp` requires non-blank key; local mode rejects non-default `signature` or non-null `signing_key`; `attestation`/`none` reject non-null `signing_key`);
  - `env_refs` (`null` -> empty, unknown/null elements, invalid key/value names, boolean/number rejection, zero host env reads);
  - `options` (`null` -> `cty.NilVal`, unknown rejection, non-object/non-map rejection, dynamic/capsule rejection, `SensitiveMark` preservation);
  - `Value()` pre-plan `cty.DynamicVal` and post-plan object (with `SensitiveMark` propagation) and `HarnessConfig()` deep-copy isolation.
- [X] T006 [P] [US2] Write failing planner integration tests in `internal/planner/harness_test.go` covering HCL planning for local mode (`"copilot"` and `"custom"`), GitHub Releases mode, `output` referencing `harness.<name>.source`/`version`, `model_provider` rejected as unknown attribute, native `for_each`/`depends_on`/`precondition`/`dynamic` support, sensitive variable propagation, and validation errors.
- [X] T007 [P] [US2] Add failing cold-start subprocess test `TestColdStartLoadHarnessBeforeNewSofaConfig` in `internal/planner/cold_start_test.go`.

### Implementation for User Story 2 (Green)

- [X] T008 [US2] Implement `SignatureMode`, `HarnessConfig` (`Validate()`, `Clone()`), and `HarnessBlock` (`Type`, `BlockType`, `AddressLength`, `CanExecutePrePlan`, `BaseDecode`, `ExecuteDuringPlan`, `Value`, `HarnessConfig`) in `internal/harness/spec/schema.go`, enforcing all constraints in `data-model.md` for `HarnessConfig.Name`, `HarnessConfig.Source`, `HarnessConfig.SourceHost`, `HarnessConfig.SourceOwner`, `HarnessConfig.SourceRepo`, `HarnessConfig.Version`, `HarnessConfig.Signature`, `HarnessConfig.SigningKey`, `HarnessConfig.EnvRefs`, and `HarnessConfig.Options`.
- [X] T009 [US2] Register `&harnessspec.HarnessBlock{}` in `RegisterBlocks()` in `internal/planner/config.go`.

**Checkpoint**: US2 harness unit, planner, and cold-start tests pass.

---

## Phase 4: User Story 3 - RootDefaults Harness Resolution (Priority: P1)

**Goal**: Collect root `harness` declarations in `RootDefaults.Harnesses` and implement `ResolveDefaultHarness(consumerAddr string) (string, error)`.

**Independent Test**: Run `go test ./internal/planner -run TestRootDefaults -count=1`.

### Tests for User Story 3 (Red First)

- [X] T010 [P] [US3] Add failing tests for `RootDefaults.Harnesses` inspection and `ResolveDefaultHarness` (`0` missing error, `1` unique name, `2` ambiguity error listing all candidates) in `internal/planner/defaults_test.go`.

### Implementation for User Story 3 (Green)

- [X] T011 [US3] Add `Harnesses []string` (`data-model.md` `RootDefaults.Harnesses`), `case "harness"` in `InspectRootDefaults`, and `ResolveDefaultHarness(consumerAddr string) (string, error)` in `internal/planner/defaults.go`.

**Checkpoint**: US3 root defaults tests pass.

---

## Phase 5: Polish & Verification

- [X] T012 Verify zero `"copilot"` special cases in `internal/harness` and `internal/planner` production code, and zero host env / network / exec calls.
- [X] T013 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` and confirm clean pass; mark completed tasks `[X]` in `specs/004b-harness-and-sandbox-bridge/tasks.md` (do NOT commit or push).

## Phase 6: Review Remediation (Red → Green → Refactor)

- [X] T014 [P] Add failing tests for `HarnessConfig.Name` validation, `BaseDecode` field reset on re-decode, `options` with untyped `null`/empty tuple and capsule rejection, `:port` source hosts, and sensitive `signing_key` with `signature = "pgp"` in `internal/harness/spec/schema_test.go` and `internal/planner/harness_test.go`.
- [X] T015 Enforce `HarnessConfig.Name` pattern (`data-model.md` `HarnessConfig.Name`), reset all fields in `HarnessBlock.BaseDecode`, support untyped `null` while rejecting capsules in `HarnessConfig.Options` (`data-model.md` `HarnessConfig.Options`), and allow valid `:port` on `source` host (`data-model.md` `HarnessConfig.Source`) in `internal/harness/spec/schema.go`.
- [X] T016 Re-run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1` and mark Phase 6 complete.

## Dependencies & Execution Order

- Phase 1 (T001) → Phase 2, Phase 3, Phase 4 tests can be authored in parallel, then each story's implementation makes its Red tests pass.
- T009 (`internal/planner/config.go`) depends on T008 (`internal/harness/spec/schema.go`).
- Phase 5 runs after all implementation tasks pass.
