# Feature Specification: Move Worker/Dynamic-Worker `gatekeeper` Into `qc` (`034-qc-nested-gatekeeper`)

**Feature Branch**: `034-qc-nested-gatekeeper`

**Created**: 2026-11-27

**Status**: Approved

**Input**: `docs/hcl-contract.md`, `internal/worker/spec/{schema.go,qc.go,gatekeeper.go,deferred.go,dynamic.go,json_codec.go,fallback.go}`, `internal/merge/spec/schema.go`, `internal/planner/export.go`, `internal/applyeval/eval.go`, `internal/worker/runner/{types.go,worker_runner.go}`, `internal/actor/node/actor.go`, `internal/sandbox/runtime/podman.go`, and `examples/copilot-agy-self-host/`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Nest `gatekeeper` and `max_gatekeeper_reworks` Inside `qc` (Priority: P1)

Workflow authors declare deterministic `gatekeeper "<name>" { ... }` blocks (and `dynamic "gatekeeper"`) inside `worker.<name>.qc` (and `dynamic_worker.<name>.dynamic.content.qc`) instead of directly on `worker`. Declaring `gatekeeper` or `dynamic "gatekeeper"` at the `worker` level fails Plan with a clear migration error directing the author to move it into `qc { gatekeeper "..." {} }`. `qc` accepts an optional `max_gatekeeper_reworks` (whole integer >= 0, default `3`). `qc.criteria` is optional; a `qc` block is valid as long as it declares at least one `gatekeeper` or a non-empty `criteria`, and errors if it declares neither.

**Independent Test**: Unit tests in `internal/worker/spec/worker_test.go` and `internal/worker/spec/dynamic_test.go` verify:
- `qc` with gatekeepers only (no `criteria`) succeeds.
- `qc` with neither `gatekeeper` nor `criteria` fails.
- Worker-level `gatekeeper` and `dynamic "gatekeeper"` fail with a migration error mentioning `qc`.
- Duplicate `gatekeeper` names inside `qc` fail.
- `dynamic "gatekeeper"` inside `qc` expands via `golden.HclBlock.ExpandDynamicBlocks`.
- `max_gatekeeper_reworks` defaults to `3`, accepts `0`, and rejects negative/fractional/non-integer values.

---

### User Story 2 - Plan Export, `.sofaplan` Round-Trip, and Apply-Time Deferred Evaluation (Priority: P1)

Moving `Gatekeepers []GatekeeperConfig`, `MaxGatekeeperReworks int`, and `MaxGatekeeperReworksSet bool` from `workerspec.Config` into `workerspec.QCConfig` preserves full Plan serialization, `.sofaplan` round-trip, JSON codec for Dapr actors/daemons, and Apply-time deferred expression evaluation for both static `worker` and `dynamic_worker` instances (including dynamic expressions referencing `dynamic_worker` iterators inside `qc.gatekeeper` or `qc.criteria`).

**Independent Test**: Tests in `internal/planner/worker_test.go`, `internal/worker/spec/worker_test.go`, `internal/worker/spec/dynamic_test.go`, and `internal/applyeval/eval_test.go` verify deferred expressions place `gatekeeper` and `max_gatekeeper_reworks` inside `qc`, `.sofaplan` archive round-trips preserve `QC.Gatekeepers` and `QC.MaxGatekeeperReworks`, and `ProjectDynamicInstance` evaluates per-instance `qc.gatekeeper` at Apply time.

---

### User Story 3 - Execute Gatekeepers in QC Sandbox Before Optional LLM QC (Priority: P1)

`WorkerRunner` executes `qc.gatekeepers` using the resolved QC sandbox (`qc.sandbox`, falling back to worker `sandbox`) before launching any QC LLM session:
- When `qc.gatekeepers` fail, failure output feeds back to the Work session for rework up to `max_gatekeeper_reworks` (with per-node `qc.max_gatekeeper_reworks` overriding `req.MaxGatekeeperReworks` when explicitly set), without launching the QC LLM session; on exhaustion, triggers `gatekeeper_exhausted` fallback or fails.
- When `qc.criteria` is absent, `WorkerRunner` runs only gatekeepers (zero QC LLM sessions, zero QC ledger) and completes once gatekeepers pass.
- When `qc.criteria` is present, LLM QC runs only after all gatekeepers pass; after a QC `revise` verdict, the next loop iteration reruns gatekeepers before the next LLM QC round.
- Gatekeeper rework counts remain independent from `max_qc_rounds`, preserving both `gatekeeper_exhausted` and `qc_exhausted` fallback triggers.
- `merge` block semantics and `merge.gatekeeper` remain completely unchanged.

**Independent Test**: Tier 1 runner tests in `internal/worker/runner/runner_test.go` with fake `SandboxEngine` and scripted `SessionLauncher` verify QC sandbox selection, no QC LLM launch on gatekeeper failure, gatekeeper-only completion without QC session/ledger, independent rework budgets (`max_gatekeeper_reworks` vs `max_qc_rounds`), per-node `max_gatekeeper_reworks` override (including `0`), and gatekeeper rerun after QC `revise`.

---

### User Story 4 - Update Contract Docs and Self-Hosting Example (Priority: P1)

`docs/hcl-contract.md`, `examples/copilot-agy-self-host/main.sofa.hcl`, `examples/copilot-agy-self-host/README.md`, and `examples/examples_test.go` reflect `qc`-nested `gatekeeper` syntax and read gatekeepers from `workerCfg.QC.Gatekeepers`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `internal/worker/spec/schema.go` MUST remove `Gatekeepers` from `Config` (and `Config.Clone()`) and reject `gatekeeper` and `dynamic "gatekeeper"` at top-level `worker` (and `dynamic_worker` `content`) with an error instructing the author to move them into `qc { gatekeeper "..." {} }`.
- **FR-002**: `internal/worker/spec/qc.go` MUST add `Gatekeepers []GatekeeperConfig`, `MaxGatekeeperReworks int` (default `DefaultMaxGatekeeperReworks = 3`), and `MaxGatekeeperReworksSet bool` to `QCConfig`, decode repeatable `gatekeeper "<name>"` (and `dynamic "gatekeeper"` via `golden`'s `ExpandDynamicBlocks`), reject duplicate gatekeeper names inside `qc`, validate `max_gatekeeper_reworks >= 0`, make `criteria` optional (treating omitted or `null` as absent while rejecting explicit empty maps `{}`), and require at least one `gatekeeper` or non-empty `criteria`.
- **FR-003**: `internal/worker/spec/deferred.go`, `internal/planner/export.go`, `internal/applyeval/eval.go`, `internal/worker/spec/json_codec.go`, and `internal/actor/node/actor.go` MUST relocate gatekeeper encoding/decoding and defaults (`working_dir = "."`, `timeout = "10m"`, `expect_exit_code = 0`, `require_clean_worktree = true`, `max_gatekeeper_reworks = 3`) inside `qc`, supporting both static `worker` and `dynamic_worker` Apply-time evaluation.
- **FR-004**: `internal/worker/runner/worker_runner.go` MUST run `req.Worker.QC.Gatekeepers` in the resolved `qcSandbox` (`qc.sandbox` falling back to `workSandbox`) before any QC LLM round, skip QC ledger/session when `qc.criteria` is absent, skip LLM QC when gatekeepers fail, rerun gatekeepers after QC `revise`, count `gatekeeperReworks` independently from `qcRound`, and honor per-node `qc.max_gatekeeper_reworks` (when `MaxGatekeeperReworksSet` is true) over `req.MaxGatekeeperReworks`.
- **FR-005**: `internal/merge/spec/schema.go` and `merge` runtime MUST remain unchanged, sharing `workerspec.DecodeGatekeeper`.
- **FR-006**: `docs/hcl-contract.md`, `examples/copilot-agy-self-host/{main.sofa.hcl,README.md}`, `examples/examples_test.go`, and all existing tests/integration fixtures declaring worker-level gatekeepers MUST be updated to `qc`-nested gatekeepers.

## Success Criteria *(mandatory)*

- **SC-001**: `gofmt -l .` returns empty.
- **SC-002**: `go vet ./...` passes cleanly across all packages.
- **SC-003**: `go test ./... -count=1` passes 100% including `./examples/...`.
- **SC-004**: `go test -tags=integration ./internal/worker/runner/... -count=1` passes cleanly (or reports environment skip/status accurately if Podman unavailable).
- **SC-005**: `git status` shows only Slice `034` changes (uncommitted for parent review).
