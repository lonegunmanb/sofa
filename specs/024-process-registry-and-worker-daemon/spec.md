# Feature Specification: `ProcessRegistryActor` Slot Scheduling, Three-State Liveness Probing & `WorkerDaemon` Host (`024-process-registry-and-worker-daemon`)

**Feature Branch**: `024-process-registry-and-worker-daemon`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `024` blueprint (`docs/stage3/024-process-registry-and-worker-daemon.md`), `docs/stage3/025-node-actor-and-bridge.md`, `docs/stage3/026-merge-actor-queue.md`, `docs/stage3/README.md`, `docs/architecture-contract.md`, `internal/daprhost/manager.go`, `internal/actor/config/actor.go`, `internal/worker/runner/{types.go,worker_runner.go,merge_primitive.go}`, `internal/blob/store.go`, and upstream inspection of `github.com/dapr/go-sdk@v1.16.0` and `github.com/dapr/dapr@v1.19.0`.

## User Scenarios & Testing

### User Story 1 - Lease-Based Least-Load Slot Scheduling with Architecture Matching & Idempotency (Priority: P1)

`RunActor` / `NodeActor` (`025`) and `MergeActor` (`026`) allocate execution slots on host workers through `ProcessRegistryActor` (`ActorType = "ProcessRegistryActor"`, singleton `GlobalActorID = "global"`). Each worker registers with `ProcessID`, `AppID`, `MaxConcurrency` (default `10` for standalone workers, `2` for `--dev` co-located mode), and target container platform `Arch` (e.g. `linux/amd64`, `linux/arm64`). `ClaimSlot(ctx, req)` MUST:
- Return the existing `(LeaseID, ProcessID, AppID)` idempotently when `LeaseKey` already holds an active lease on a non-`DEAD` worker, preventing double-counting on retries;
- Otherwise consider ONLY workers with `Status == StatusHealthy`, `ActiveSlots < MaxConcurrency`, and matching `Arch` (when `RequiredArch != ""`);
- Select the worker with the lowest utilization ratio (`ActiveSlots / MaxConcurrency`, i.e., highest free-slot ratio) and assign a new `LeaseID`;
- Return `Granted: false` (without error) when all matching workers are full or unhealthy so callers can queue non-blockingly;
- Support idempotent `ReleaseSlot` by `LeaseID` or `LeaseKey`, and `ListProcesses` snapshots for `/api/v1/workers`.

**Independent Test**: Tier 1 unit tests in `internal/actor/registry/registry_test.go` using in-memory `actor.StateManagerContext` verifying least-load selection across equal and unequal `MaxConcurrency` capacities, architecture filtering, `Granted: false` on saturation, `LeaseKey` idempotency, and idempotent `ReleaseSlot`.

### User Story 2 - Three-State Liveness State Machine (`HEALTHY` -> `SUSPECT` -> `DEAD`), Active Probing & Dead-Worker Notifications (Priority: P1)

Workers periodically send `Heartbeat(ProcessID)`. If a worker's heartbeat exceeds `SuspectTimeout` (default `15s`) or a control-plane dispatch calls `ReportSuspect(ProcessID)`, the worker transitions from `HEALTHY` to `SUSPECT`: it stops receiving new `ClaimSlot` allocations while preserving its in-flight leases. On each `ReminderCall` tick (explicitly persisting state via `Set` + `Save` because `dapr/go-sdk` `InvokeReminder` does not auto-save), `ProcessRegistryActor` actively probes every `SUSPECT` worker via its Dapr Service Invocation `ping` endpoint (checking `ProcessID` match):
- A successful `ping` immediately restores `HEALTHY` and resets consecutive failure count;
- `DeadThreshold` (default `3`) consecutive probe failures transition the worker to `DEAD`, purge all of its active `SlotLease`s (resetting `ActiveSlots = 0`), and invoke `OnWorkerDead` on every affected lease's `(OwnerActorType, OwnerActorID)` with `{LeaseID, LeaseKey, ProcessID, AppID}`;
- A subsequent `Heartbeat` from an unknown or `DEAD` `ProcessID` MUST return `{Status: StatusDead, FenceRequired: true}` and NEVER silently resurrect the dead `ProcessID`.

**Independent Test**: Tier 1 unit tests in `internal/actor/registry/registry_test.go` verifying `HEALTHY -> SUSPECT -> HEALTHY` recovery, `HEALTHY -> SUSPECT -> DEAD` transition on threshold failures, slot purging, exact per-lease `OnWorkerDead` callbacks, and `FenceRequired: true` on `Heartbeat` for `DEAD`/unknown processes.

### User Story 3 - Worker Dapr Service Host, Env Isolation, Dual-Pipeline JSONL Logging & Self-Fencing (Priority: P1)

`internal/worker/daemon` (`WorkerDaemon`) mounts Dapr Service Invocation routes (`ping`, `start_node`, `abort_node`, `run_merge_candidate`) onto a `common.Service`:
- `ping` returns current `ProcessID` and rejects requests whose expected `ProcessID` does not match the active `ProcessID`;
- `start_node` registers `(RunID, NodeAddr, Epoch)` in an in-flight task table, launches background `NodeExecutor` (defaulting to `runner.WorkerRunner`), and returns accepted status within milliseconds; `LookupEnv` passed to `NodeExecutionRequest` MUST read strictly from the request's `ResolvedEnv` map and NEVER fall back to host `os.LookupEnv`;
- `abort_node` idempotently cancels the in-flight `(RunID, NodeAddr, Epoch)` context;
- `run_merge_candidate` asynchronously executes `MergeExecutor` (defaulting to `runner.ExecuteMergeCandidate`), reports completion, and idempotently calls `ReleaseSlot`;
- Node events are streamed as structured JSONL lines to both Dapr PubSub (`sofa-pubsub` / `sofa-events`) and a bounded local temp file, uploaded to `BlobStore` (`runs/<run_id>/logs/<node_key>/e<epoch>.jsonl`, validated by `blob.ValidateKey`) on checkpoint events and unconditionally on node exit;
- The background heartbeat loop immediately cancels all in-flight node and merge tasks, invokes local cleanup callbacks, rotates to a fresh `ProcessID` (and updates identity in standalone mode while preserving fixed `AppID` in `--dev` mode), and re-registers with `ProcessRegistryActor` whenever `Heartbeat` returns `StatusDead` / `FenceRequired`.

**Independent Test**: Tier 1 unit tests in `internal/worker/daemon/daemon_test.go` and Tier 2 integration tests in `internal/worker/daemon/daemon_integration_test.go` (`//go:build integration`) on real multi-`daprd` `daprhost.Manager` instances (using SQLite name resolution on Windows loopback) verifying registration, least-load `ClaimSlot`, async `start_node` + PubSub + `BlobStore` JSONL archival, and real Dapr Reminder-driven `HEALTHY -> SUSPECT -> DEAD` failover with `OnWorkerDead` delivery.

### Edge Cases

- Passing `nil` request pointers or empty `ProcessID`/`AppID`/`LeaseKey` returns explicit validation errors without panicking.
- Calling `ClaimSlot` when an existing `LeaseKey`'s worker transitioned to `DEAD` purges the stale lease and allocates a new healthy slot instead of returning the dead worker.
- `ReminderCall` executes outside lock during network `ping` and `OnWorkerDead` calls, re-checking whether a concurrent `Heartbeat` restored health before recording a probe failure.
- `WorkerRunner` receiving empty `ResolvedEnv` receives a non-nil `LookupEnv` returning `("", false)` so `worker_runner.go` never falls back to `os.LookupEnv`.
- Multi-`daprd` local Service Invocation on Windows loopback uses Dapr `nameresolution` `sqlite` (`daprhost.Options.NameResolutionDBPath`) so `daprd` peers discover each other reliably without mDNS firewall dependence.

## Requirements

### Functional Requirements

- **FR-001**: `internal/actor/registry` MUST export `ActorType = "ProcessRegistryActor"`, `GlobalActorID = "global"`, `DefaultWorkerMaxConcurrency = 10`, `DefaultDevMaxConcurrency = 2`, `ProcessStatus` (`StatusHealthy`, `StatusSuspect`, `StatusDead`), and single-pointer RPC methods `RegisterProcess`, `Heartbeat`, `ReportSuspect`, `ClaimSlot`, `ReleaseSlot`, `ListProcesses` plus `ClientStub` / `NewClientStub`.
- **FR-002**: `ProcessRegistryActor` MUST implement `actor.ServerContext` and `actor.ReminderCallee`, persisting state explicitly (`Set` + `Save`) during both RPC mutations and `ReminderCall`.
- **FR-003**: `ClaimSlot` MUST enforce `LeaseKey` idempotency (for non-`DEAD` workers), `StatusHealthy` + `Arch` + capacity filtering, lowest `ActiveSlots/MaxConcurrency` selection, and non-blocking `Granted: false` when saturated.
- **FR-004**: `ReminderCall` MUST transition expired `HEALTHY` workers to `SUSPECT`, probe `SUSPECT` workers via `ping`, restore `HEALTHY` on success, and transition to `DEAD` on `DeadThreshold` consecutive failures while purging leases and sending `OnWorkerDead` to every affected `(OwnerActorType, OwnerActorID)`.
- **FR-005**: `internal/worker/daemon` MUST implement `WorkerDaemon` mounting `ping`, `start_node`, `abort_node`, and `run_merge_candidate` onto `common.Service`, enforcing strict `ResolvedEnv`-only `LookupEnv`, dual-pipeline PubSub + `BlobStore` JSONL logging, idempotent slot release, and heartbeat-driven self-fencing with `ProcessID` rotation.
- **FR-006**: `internal/daprhost` MUST support optional `NameResolutionDBPath` in `Options` (backward-compatible when empty) generating Dapr `Configuration` (`nameresolution` `sqlite`) passed via `--config` so multiple local `daprd` instances resolve each other reliably on Windows and Unix.

## Success Criteria

### Measurable Outcomes

- **SC-001**: `go test ./internal/actor/registry/... ./internal/worker/daemon/... ./internal/daprhost/... -count=1` and `go test ./... -count=1` pass offline with zero external processes.
- **SC-002**: `go test -v -tags=integration ./internal/actor/... ./internal/worker/daemon/... ./internal/daprhost/... -count=1` passes against real `scheduler` and `daprd` binaries with zero orphan processes.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly.
