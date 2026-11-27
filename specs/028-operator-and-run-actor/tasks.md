---
description: "Stage 3 Slice 028 TDD tasks: OperatorActor and RunActor non-blocking DAG orchestration, rework cascades, and deferred cleanup"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/actor/run/run_test.go` covering:
  - `OperatorActor("global")` index operations (`UpsertRun`, `GetRunSummary`, `ListRuns`, `RemoveRun`) and `RecoverActiveRuns` waking exclusively `RUNNING` and `PAUSING` runs;
  - Static `worker` -> `dynamic_worker` (3 instances, `module.parallelism = 2`) streaming into `merge` (verifying `W1` enqueues into `MergeActor` before `W3` completes, `dynamic_worker` keys persisted once across reconcile/cold re-hydration, empty `dynamic_worker` handling, and top-level `EvalPlanOutputs` preserving `Sensitive` metadata);
  - Nested `module` input binding/validation (`EvalModuleInputsAndValidate`), child DAG execution, and module output propagation;
  - `FallbackTriggered` returning to ancestor cascading `epoch++` across affected downstream chain and exceeding `max_reworks`/`max_cycles` transitioning Run to `FAILED`;
  - `MergeReworkSource` advancing ONLY the conflicting `dynamic_worker[key]` to `epoch = 2` with `BaseSHA = LatestTargetSHA` while keeping sibling instances untouched, plus `merge.max_reworks` exhaustion transitioning Run to `FAILED`;
  - `RerunNode` on a node already merged into explicit `merge.target_branch` rejected with `errors.Is(err, ErrAlreadyMergedSharedBranch)` (both in-process and via `RunClient`/lossless error translation), vs `RerunNode` on Run-exclusive `merge` / `worker` cascading `epoch++` and succeeding;
  - `PauseRun("planned")` vs `PauseRun("immediate")` + `ResumeRun`;
  - Deferred cleanup: `SUCCEEDED`/`FAILED`/`PAUSED` delete 0 branches and 0 blobs; `DeleteRun` fences active nodes/merges, preserves `source` + evaluated `preserve` branches, and deletes all `sofa/ckpt/*`, `sofa/work/*`, Run-exclusive merge branches, `runs/<run_id>/` blobs, `ConfigActor` Run scope, and `OperatorActor` entry.
- [X] T002 [Green] Implement `internal/actor/run/types.go`, `internal/actor/run/operator_actor.go`, and `internal/actor/run/run_actor.go`; verify `go test -v ./internal/actor/run/... -count=1` passes.
- [X] T003 [Red -> Green] Write Tier 2 real-Dapr + Git + WorkerDaemon integration test `internal/actor/run/run_integration_test.go` (`//go:build integration`):
  - Submit a 2-node `.sofaplan` (`worker.step1 -> worker.step2`) on real `daprhost.Manager` (`scheduler` + `daprd` + SQLite + `OperatorActor` + `ConfigActor` + `ProcessRegistryActor` + `RunActor` + `NodeActor` + `WorkerDaemon` + real local bare Git repo);
  - Stop/kill the host while `worker.step2` is mid-execution (`RUNNING` with a persisted `e1` checkpoint), restart `daprhost` + `WorkerDaemon` on the same state directory, call `OperatorActor.RecoverActiveRuns`, and assert `worker.step2` automatically advances to `epoch >= 2` and drives the entire Run to `SUCCEEDED` with zero orphan `scheduler`/`daprd` processes.
- [X] T004 [Refactor & Verify] Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/actor/run/... -count=1`, verify zero orphan `scheduler`/`daprd` processes remain, and mark tasks `[X]`.
