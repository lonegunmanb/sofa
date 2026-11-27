# Implementation Plan: `ProcessRegistryActor` Slot Scheduling, Three-State Liveness Probing & `WorkerDaemon` Host (`024-process-registry-and-worker-daemon`)

**Branch**: `024-process-registry-and-worker-daemon` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 3 Slice `024`:
1. Extend `internal/daprhost` (`Options.NameResolutionDBPath`) with backward-compatible Dapr `Configuration` generation (`nameresolution` `sqlite`) so multiple local `daprd` sidecars invoke each other deterministically on Windows loopback without mDNS multicast flakiness.
2. Implement `internal/actor/registry` (`ProcessRegistryActor` `"global"`): three-state health machine (`HEALTHY` -> `SUSPECT` -> `DEAD`), `LeaseKey`-idempotent least-load slot allocation, `ReminderCall` active probing with explicit `Set`+`Save` persistence, `OnWorkerDead` owner callbacks, `Heartbeat` dead-process fencing, and `ClientStub`.
3. Implement `internal/worker/daemon` (`WorkerDaemon`): Dapr Service Invocation handlers (`ping`, `start_node`, `abort_node`, `run_merge_candidate`), strict `ResolvedEnv`-only `LookupEnv` isolation, dual-pipeline JSONL PubSub (`sofa-pubsub`/`sofa-events`) + `BlobStore` archival (`runs/<run_id>/logs/<node_key>/e<epoch>.jsonl`), and lost-contact self-fencing + `ProcessID` rotation.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Primary Dependencies**:
  - `github.com/dapr/go-sdk` `v1.16.0` (`actor`, `client`, `service/common`, `service/http`)
  - `github.com/google/uuid` (lease IDs and rotated `ProcessID`s)
  - `github.com/lonegunmanb/sofa/internal/daprhost` (`NewActorStateManager`, `Manager`, `PubSubComponentName`)
  - `github.com/lonegunmanb/sofa/internal/worker/runner` (`WorkerRunner`, `ExecuteMergeCandidate`, `NodeExecutionRequest`, `MergeCandidateRequest`)
  - `github.com/lonegunmanb/sofa/internal/blob` (`Store`, `ValidateKey`)
- **Testing**:
  - Tier 1: `internal/daprhost/components_test.go`, `internal/actor/registry/registry_test.go`, `internal/worker/daemon/daemon_test.go`
  - Tier 2: `internal/worker/daemon/daemon_integration_test.go` (`//go:build integration`, real `daprhost.Manager` multi-`daprd` + SQLite name resolution + Dapr Reminder failover)

## Constitution Check

- **Principle I (Spec-First)**: Full Spec Kit directory created before any tests or production code.
- **Principle II (Strict TDD)**: Red tests written and confirmed failing before implementing each package.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `ProcessRegistryActor` and `WorkerDaemon` inject `Prober`, `DeadNotifier`, `ReminderScheduler`, `NodeExecutor`, `MergeExecutor`, `PubSubPublisher`, `BlobStore`, and `RegistryClient` interfaces for deterministic Tier 1 unit tests while exercising real `daprd` + `scheduler` + `Reminder` + `InvokeMethod` + `LocalStore` in Tier 2.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: `LookupEnv` strictly bound to `ResolvedEnv` map (zero `os.LookupEnv` fallback); `DEAD` `ProcessID` cannot be resurrected by `Heartbeat`; `ping` rejects mismatched `ProcessID`; self-fencing cancels all active tasks immediately.
- **Principle V (Single-Node & Distributed Parity)**: `WorkerDaemon` supports both standalone (`sofa-worker-<process_id>`) and co-located `--dev` mode over standard `dapr/go-sdk` Actor & Service Invocation contracts.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `dapr/go-sdk@v1.16.0` (`actor/manager/manager.go`, `actor/actor.go`, `client/actor.go`, `client/invoke.go`, `client/pubsub.go`, `service/http/{invoke,service}.go`), `dapr/dapr@v1.19.0` (`pkg/runtime/runtime.go`, `pkg/config/configuration.go`, `cmd/daprd/components/nameresolution_sqlite.go`, `pkg/messaging/direct_messaging.go`), `internal/actor/config/actor.go`, `internal/daprhost/manager.go`, `internal/worker/runner/{types,worker_runner,merge_primitive}.go`, `internal/blob/{store,local}.go`, and `internal/git/ledger.go`.

## Project Structure

```text
specs/024-process-registry-and-worker-daemon/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── registry-and-daemon.md
├── checklists/
│   └── requirements.md
└── tasks.md

internal/daprhost/
├── components.go              # Optional nameresolution.sqlite Configuration YAML writer
├── manager.go                 # Optional Options.NameResolutionDBPath -> --config flag
└── components_test.go         # Tier 1 unit tests for nameresolution config generation

internal/actor/registry/
├── types.go                   # Constants, ProcessStatus, SlotLease, WorkerDeadNotification, RPC Req/Resp
├── actor.go                   # ProcessRegistryActor, Prober/DeadNotifier/ReminderScheduler, ClientStub
└── registry_test.go           # Tier 1 unit tests (Red -> Green)

internal/worker/daemon/
├── daemon.go                  # WorkerDaemon handlers, isolated LookupEnv, JSONL PubSub+BlobStore, self-fence loop
├── daemon_test.go             # Tier 1 unit tests (Red -> Green)
└── daemon_integration_test.go # Tier 2 real multi-daprd + Reminder failover integration tests
```

## Complexity Tracking

No Constitution violations.
