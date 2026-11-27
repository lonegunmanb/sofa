# Implementation Plan: `NodeActor` Authoritative State Machine, Interjections/Decision Cards & `NodeControlPlane` Bridge (`025-node-actor-and-bridge`)

**Branch**: `025-node-actor-and-bridge` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement `internal/actor/node` containing:
- `types.go`: URL-safe bidirectional Actor ID encoding (`EncodeNodeActorID`/`DecodeNodeActorID`), `NodeStatus` state machine enum, `DecisionCard` and `InterjectionRecord` models, injectable boundary interfaces (`ConfigResolver`, `SlotManager`, `WorkerDispatcher`, `RunNotifier`, `ReminderScheduler`), and all single-pointer Actor RPC request/response types.
- `actor.go`: `NodeActor` (`actor.ServerImplBaseCtx` + `actor.ReminderCallee`), single-document `"node_actor_v1"` explicit `Set`+`Save` persistence, non-blocking `QUEUED_SLOT` reminder retry loop, `OnWorkerDead` takeover, `PauseNode` (`planned`/`immediate`), `AdvanceForRework`/`ResetForRerun`, Git credential materialization (`ResolvedEnv` + `Auth`), epoch-fenced control-plane RPCs, `ReportNodeOutcome`, `SubmitDecision` (version-bound approval & manual-pause guard), `GetState` query, Dapr production boundary adapters, and `ClientStub` / `NewClientStub` / `NewClientStubForNode`.
- `control_plane_client.go`: `RemoteNodeControlPlane` implementing `runner.NodeControlPlane` over `*ClientStub`, accurately translating `StaleEpoch: true` and structured error codes back to sentinel Go errors (`checkpoint.ErrStaleEpoch`, `git.ErrInvalidBaseSHA`, etc.).
- `node_test.go` & `node_integration_test.go`: Tier 1 deterministic unit tests and Tier 2 real-Dapr multi-component integration tests (`//go:build integration`).

## Technical Context

**Language/Version**: Go 1.27 (`module github.com/lonegunmanb/sofa`)  
**Primary Dependencies**: `github.com/dapr/go-sdk@v1.16.0` (`actor`, `client`), `github.com/google/uuid`, `internal/actor/config`, `internal/actor/registry`, `internal/worker/daemon`, `internal/worker/runner`, `internal/checkpoint`, `internal/session`, `internal/git`, `internal/blob`, `internal/artifact`, `internal/daprhost`.  
**Storage**: Dapr Actor State Manager (`node_actor_v1` JSON document, explicitly persisted via `Set` + `Save`), `BlobStore` (indirectly via `WorkerRunner` / `WorkerDaemon`).  
**Testing**: `go test ./internal/actor/node/... -count=1` (Tier 1 in-memory `actor.StateManagerContext`), `go test -v -tags=integration ./internal/actor/node/... -count=1` (Tier 2 real `daprhost.Manager`, local bare Git repo, `ConfigActor`, `ProcessRegistryActor`, `WorkerDaemon`, `WorkerRunner`).  
**Constraints**: No `ErrStaleEpoch` lost over `dapr/go-sdk` `InvokeActor` (always return `nil` Go error + `StaleEpoch: true`); no holding `NodeActor.mu` across external RPCs; no launching containers when manually paused; no `git commit` or `git push` until review.

## Constitution Check

- **I. Spec-First Delivery**: Complete Spec Kit artifacts under `specs/025-node-actor-and-bridge/` created prior to tests and production code. PASS.
- **II. Strict TDD**: `tasks.md` orders failing Tier 1 and Tier 2 test tasks strictly before implementation tasks. PASS.
- **III. Pure Core & Real-Dependency Boundaries**: `NodeActor` isolates Dapr RPC, config resolution, slot leasing, worker dispatch, run ledger notification, and reminders behind explicit Go interfaces; Tier 1 tests run in-memory and Tier 2 exercises real `daprd` + Git + `WorkerDaemon` + `WorkerRunner`. PASS.
- **IV. Runtime Enforcement Over Prompt Trust**: Monotonic `CurrentEpoch` fencing, synchronous `RegisterManagedBranch` before remote Git push, exact `(CommitSHA, ArtifactIDs)` binding, and manual-pause guard are all enforced in Go control-plane state. PASS.
- **V. Single-Node & Distributed Parity**: `RemoteNodeControlPlane` implements the exact `runner.NodeControlPlane` interface used by `LocalNodeControlPlane`, enabling zero-change `WorkerRunner` execution in distributed Dapr mode. PASS.
- **VI. Research-First & Surgical Task Sizing**: Based on direct inspection of `dapr/go-sdk@v1.16.0`, `internal/checkpoint`, `internal/worker/runner`, `internal/actor/config`, `internal/actor/registry`, and `internal/worker/daemon`; scoped strictly to `internal/actor/node/`. PASS.

## Project Structure

```text
specs/025-node-actor-and-bridge/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/node-actor.md
└── tasks.md

internal/actor/node/
├── types.go
├── actor.go
├── control_plane_client.go
├── node_test.go
└── node_integration_test.go
```
