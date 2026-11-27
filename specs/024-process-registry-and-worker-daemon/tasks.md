---
description: "Stage 3 Slice 024 TDD tasks: ProcessRegistryActor & WorkerDaemon (internal/actor/registry, internal/worker/daemon, internal/daprhost)"
---

# Tasks

- [X] T001 [Red -> Green] Extend `internal/daprhost` (`Options.NameResolutionDBPath`) with backward-compatible Dapr `Configuration` generation (`nameresolution` `sqlite`) and `--config` wiring:
  - Write failing unit test in `internal/daprhost/components_test.go` verifying `WriteNameResolutionConfig` and `NewManager` validation;
  - Implement in `internal/daprhost/components.go` and `internal/daprhost/manager.go` and verify `go test ./internal/daprhost/... -count=1` passes.
- [X] T002 [Red] Write failing Tier 1 unit tests in `internal/actor/registry/registry_test.go` covering:
  - Least-load slot selection (`max=10, active=5` vs `max=10, active=1` and heterogeneous `MaxConcurrency` ratios), `Arch` filtering, `Granted: false` when full/unhealthy, `LeaseKey` idempotency, and idempotent `ReleaseSlot`;
  - `Heartbeat` timeout -> `SUSPECT` (excluded from new `ClaimSlot`, existing leases preserved) -> `ping` recovery to `HEALTHY`;
  - Consecutive `ping` failures reaching `DeadThreshold` -> `DEAD`, purging active leases, and invoking `OnWorkerDead` for each lease owner `(OwnerActorType, OwnerActorID)`;
  - `Heartbeat` on unknown or `DEAD` `ProcessID` returning `StatusDead` + `FenceRequired: true` without resurrecting;
  - Explicit state persistence inside `ReminderCall` (verified with an in-memory `StateManagerContext` that does not auto-save on `ReminderCall`).
- [X] T003 [Green] Implement `internal/actor/registry/types.go` and `internal/actor/registry/actor.go` and verify `go test ./internal/actor/registry/... -count=1` passes.
- [X] T004 [Red] Write failing Tier 1 unit tests in `internal/worker/daemon/daemon_test.go` covering:
  - `ping` returning active `ProcessID` and rejecting mismatched `ProcessID`;
  - `start_node` non-blocking async acceptance, idempotent duplicate `(RunID, NodeAddr, Epoch)`, and strict `LookupEnv` isolation (setting a host env var via `t.Setenv` and verifying `WorkerRunner` / `NodeExecutionRequest.LookupEnv` CANNOT read it);
  - `abort_node` immediate cancellation of in-flight node context and idempotency;
  - `run_merge_candidate` async execution, result reporting, and idempotent `ReleaseSlot`;
  - Dual-pipeline JSONL logging to Dapr PubSub (`sofa-pubsub`/`sofa-events`) and `BlobStore` (`runs/<run_id>/logs/<node_key>/e<epoch>.jsonl`) on checkpoints and node exit;
  - Heartbeat receiving `StatusDead` / `FenceRequired: true` immediately cancelling all active tasks, invoking cleanup, rotating to a new `ProcessID`, and re-registering.
- [X] T005 [Green] Implement `internal/worker/daemon/daemon.go` and verify `go test ./internal/worker/daemon/... -count=1` passes.
- [X] T006 [Red -> Green] Write Tier 2 real-Dapr integration test `internal/worker/daemon/daemon_integration_test.go` (`//go:build integration`):
  - Launch control-plane `daprhost.Manager` (hosting `ProcessRegistryActor` and a stub `NodeActor` receiver for `OnWorkerDead`) and two worker `daprhost.Manager` + `WorkerDaemon` instances sharing `ExternalSchedulerAddr` and `NameResolutionDBPath` (SQLite name resolution);
  - Verify cross-`daprd` registration, least-load `ClaimSlot`, `start_node` async execution, PubSub event delivery, and `BlobStore` JSONL archival;
  - Block/stop one worker's `ping` response and verify real Dapr `ReminderCall` drives `HEALTHY -> SUSPECT -> DEAD` within a short sub-second/few-second window and delivers `OnWorkerDead` to the owning `NodeActor`.
  - Run `go test -v -tags=integration ./internal/actor/... ./internal/worker/daemon/... ./internal/daprhost/... -count=1`.
- [X] T007 [Refactor & Verify] Review code, run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/actor/... ./internal/worker/daemon/... ./internal/daprhost/... -count=1`, ensure zero stray `scheduler`/`daprd` processes, and mark tasks `[X]`.
