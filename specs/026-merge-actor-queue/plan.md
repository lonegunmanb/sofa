# Implementation Plan: `MergeActor` Persistent Target-Branch Queue & Source-Responsible Serial Integration (`026-merge-actor-queue`)

**Branch**: `026-merge-actor-queue` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement `internal/actor/merge` and minimal credential-isolation alignment in `internal/worker/daemon`:
- `internal/worker/daemon/daemon.go` & `daemon_test.go`: Add backward-compatible `ResolvedEnv map[string]string` to `RunMergeCandidateRequest` and bind `makeIsolatedLookupEnv(req.ResolvedEnv)` to default `git.CLIAdapter` in `runMergeTask` when `Options.GitAdapter == nil`.
- `internal/actor/merge/types.go`: Define `ActorType = "MergeActor"`, deterministic `EffectiveTargetBranch` and `MergeActorID` (64-char lowercase SHA-256 hex), `TicketStatus` / `CandidateTicket`, `MergeCandidateNotification`, `RegisterManagedBranchReq`, injectable boundary interfaces (`ConfigResolver`, `SlotManager`, `MergeWorkerDispatcher`, `RunMergeNotifier`, `ReminderScheduler`), and all RPC request/response structs.
- `internal/actor/merge/actor.go`: Implement `MergeActor` (`actor.ServerImplBaseCtx` + `actor.ReminderCallee`), single-document `"merge_actor_v1"` explicit `Set`+`Save` persistence, non-blocking single-in-flight FIFO dispatcher with `SlotRetryReminder`, `EnqueueCandidate` idempotency, `ReportCandidateResult` (`MergeStatusSucceeded` / `MergeStatusReworkSource` / error / aborted handling + immediate queue advance), `CancelByRun`, `OnWorkerDead`, `GetState`, Dapr production adapters, and `ClientStub` / `NewClientStub` / `NewClientStubForTarget`.
- `internal/actor/merge/merge_test.go` & `internal/actor/merge/merge_integration_test.go`: Tier 1 deterministic unit tests and Tier 2 real `daprhost.Manager` + Git bare repo + Podman `alpine:latest` `gatekeeper` integration test (`//go:build integration`).

## Technical Context

**Language/Version**: Go 1.27 (`module github.com/lonegunmanb/sofa`)  
**Primary Dependencies**: `github.com/dapr/go-sdk@v1.16.0` (`actor`, `client`), `github.com/google/uuid`, `internal/actor/config`, `internal/actor/registry`, `internal/worker/daemon`, `internal/worker/runner`, `internal/worker/spec`, `internal/sandbox/spec`, `internal/git`, `internal/daprhost`.  
**Storage**: Dapr Actor State Manager (`merge_actor_v1` JSON document, explicitly persisted via `Set` + `Save`).  
**Testing**: `go test -v ./internal/actor/merge/... ./internal/worker/daemon/... -count=1` (Tier 1 in-memory `actor.StateManagerContext`), `go test -v -tags=integration ./internal/actor/merge/... ./internal/worker/daemon/... -count=1` (Tier 2 real `daprhost.Manager`, local bare Git repo, `ConfigActor`, `ProcessRegistryActor`, `WorkerDaemon`, real Podman `alpine:latest` `gatekeeper`).  
**Constraints**: PowerShell 5.1 (no `&&`); no holding `MergeActor.mu` across external RPCs; explicit `Set`+`Save` in `ReminderCall`; strictly single `IN_FLIGHT` ticket; never roll back merged commits; never block subsequent queue items on conflict/gatekeeper failure; no `git commit` or `git push`.

## Constitution Check

- **I. Spec-First Delivery**: Complete Spec Kit artifacts under `specs/026-merge-actor-queue/` created prior to tests and production code. PASS.
- **II. Strict TDD**: `tasks.md` orders failing Tier 1 and Tier 2 test tasks strictly before implementation tasks. PASS.
- **III. Pure Core & Real-Dependency Boundaries**: `MergeActor` isolates Dapr RPC, config resolution, slot leasing, worker dispatch, run ledger notification, and reminders behind explicit Go interfaces; Tier 1 tests run in-memory and Tier 2 exercises real `daprd` + Git + `WorkerDaemon` + Podman. PASS.
- **IV. Runtime Enforcement Over Prompt Trust**: Single in-flight lock, `KindMergeBranch` registration before remote push, `--force-with-lease` via `ExecuteMergeCandidate`, isolated `makeIsolatedLookupEnv`, and deterministic `gatekeeper` exit codes are enforced in Go runtime code. PASS.
- **V. Single-Node & Distributed Parity**: `MergeActor` runs identically over `dapr/go-sdk/actor` and `daprhost.Manager` in standalone `--dev` or distributed mode, persisting auditable queue/history state. PASS.
- **VI. Research-First & Surgical Task Sizing**: Based on direct inspection of `internal/worker/runner/merge_primitive.go`, `internal/git/{adapter,cli,ledger}.go`, `internal/actor/{config,registry,node}/`, `internal/worker/daemon/daemon.go`, and `docs/stage3/{026,028}-*.md`; scoped strictly to `internal/actor/merge/` and the minimal `ResolvedEnv` addition in `internal/worker/daemon/`. PASS.

## Project Structure

```text
specs/026-merge-actor-queue/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/merge-actor.md
└── tasks.md

internal/worker/daemon/
├── daemon.go                      # Add ResolvedEnv to RunMergeCandidateRequest + isolated LookupEnv on default git.CLIAdapter
└── daemon_test.go                 # Add unit test for RunMergeCandidateRequest.ResolvedEnv isolation

internal/actor/merge/
├── types.go
├── actor.go
├── merge_test.go
└── merge_integration_test.go
```
