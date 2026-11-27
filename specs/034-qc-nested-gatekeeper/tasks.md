# Tasks: Move Worker/Dynamic-Worker `gatekeeper` Into `qc` (`034-qc-nested-gatekeeper`)

## Phase 1: Pre-Task Investigation & Spec Kit Artifacts
- [x] T001 Inspect `docs/hcl-contract.md`, `internal/worker/spec/{schema,qc,gatekeeper,deferred,dynamic,json_codec,fallback}.go`, `internal/merge/spec/schema.go`, `internal/applyeval/eval.go`, `internal/planner/export.go`, `internal/worker/runner/{types,worker_runner}.go`, `internal/sandbox/runtime/podman.go`, `internal/actor/node/actor.go`, `examples/copilot-agy-self-host/`, and `golden` `ExpandDynamicBlocks`
- [x] T002 Author `specs/034-qc-nested-gatekeeper/{spec,plan,data-model,tasks}.md` and `contracts/qc-nested-gatekeeper.md`

## Phase 2: Red Tests First
- [x] T003 Add failing spec tests in `internal/worker/spec/worker_test.go` and `internal/worker/spec/dynamic_test.go` covering:
  - `qc` with gatekeepers only (no `criteria`) succeeds
  - `qc` with neither `gatekeeper` nor `criteria` fails
  - worker-level `gatekeeper` and `dynamic "gatekeeper"` fail with migration error pointing to `qc`
  - duplicate `gatekeeper` names in `qc` fail
  - `dynamic "gatekeeper"` inside `qc` expands via `golden`
  - `max_gatekeeper_reworks` default `3`, explicit `0`, and negative/non-integer rejection
  - `dynamic_worker` `qc.gatekeeper` deferred evaluation at Apply time
- [x] T004 Add failing planner export + `.sofaplan` round-trip + `applyeval` tests in `internal/planner/worker_test.go` and `internal/applyeval/eval_test.go` verifying `qc.gatekeepers` and `qc.max_gatekeeper_reworks` preservation and `dynamic_worker` per-instance `qc.gatekeeper` evaluation
- [x] T005 Add failing runner tests in `internal/worker/runner/runner_test.go` covering:
  - gatekeepers execute using `qc.sandbox` (and fall back to worker `sandbox`)
  - LLM QC is not launched when gatekeeper fails
  - gatekeeper-only `qc` completes without any QC LLM session or QC ledger
  - `max_gatekeeper_reworks` (including per-node `0` / override of `req.MaxGatekeeperReworks`) is counted independently from `max_qc_rounds`
  - after QC `revise`, next loop iteration reruns gatekeepers before next LLM QC round
- [x] T006 Run `go test ./internal/worker/spec/... ./internal/planner/... ./internal/applyeval/... ./internal/worker/runner/...` to confirm Red failures before implementation

## Phase 3: Implementation (Green)
- [x] T007 Update `internal/worker/spec/{schema.go,qc.go,gatekeeper.go,deferred.go}` and `internal/actor/node/actor.go` to remove worker-level `gatekeeper`, add `Gatekeepers`, `MaxGatekeeperReworks`, `MaxGatekeeperReworksSet`, optional `criteria`, `dynamic "gatekeeper"` support, and deferred `qc` expression encoding
- [x] T008 Update `internal/planner/export.go` and `internal/applyeval/eval.go` to encode/decode `qc.gatekeepers` and `qc.max_gatekeeper_reworks` across Plan, `.sofaplan`, and deferred evaluation
- [x] T009 Update `internal/worker/runner/{types.go,worker_runner.go}` to run `qc.gatekeepers` in `qcSandbox`, skip QC LLM/ledger when `criteria` is absent, honor per-node `max_gatekeeper_reworks`, and rerun gatekeepers after QC `revise`
- [x] T010 Update existing tests and Tier 2 integration test fixtures (`internal/worker/spec/*_test.go`, `internal/planner/*_test.go`, `internal/applyeval/eval_test.go`, `internal/worker/runner/*_test.go`) to use `qc { gatekeeper ... }` while keeping all `merge` gatekeeper tests intact
- [x] T011 Update `docs/hcl-contract.md`, `examples/copilot-agy-self-host/main.sofa.hcl`, `examples/copilot-agy-self-host/README.md`, and `examples/examples_test.go`

## Phase 4: Verification Gates
- [x] T012 Run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`
- [x] T013 Run `go test -tags=integration ./internal/worker/runner/... -count=1`
- [x] T014 Verify `git status` shows only Slice `034` changes (uncommitted) and mark all tasks `[x]`
