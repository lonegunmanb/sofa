# Implementation Plan: `OperatorActor` & `RunActor` Non-Blocking DAG Orchestration (`028-operator-and-run-actor`)

**Branch**: `028-operator-and-run-actor` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement `internal/actor/run` tying together Slices `021`–`027`:
- `internal/actor/run/types.go`: Domain statuses (`RunStatus`, `NodeRunSummary`, `RunSummary`, `EvaluatedOutput`), sentinel errors (`ErrAlreadyMergedSharedBranch`, `ErrRunNotFound`, `ErrNodeNotFound`, `ErrRunConflict`), lossless Dapr RPC request/response structs, consumer boundary interfaces (`NodeController`, `MergeController`, `OperatorIndex`, `ConfigManager`, `EventPublisher`, `RunReconciler`, `ReminderScheduler`), `RunClient` abstraction (in-process direct & Dapr RPC with lossless error translation), and `ClientStub`s.
- `internal/actor/run/operator_actor.go`: `OperatorActor("global")` maintaining persistent `RunSummary` map under `"operator_actor_v1"` and `RecoverActiveRuns` waking `RUNNING` / `PAUSING` `RunActor`s.
- `internal/actor/run/run_actor.go`: `RunActor(<run_id>)` loading & cold-rehydrating `.sofaplan` from `BlobStore`, pinning `source` commit SHAs, managing `git.LedgerSnapshot` (`RegisterManagedBranch`), advancing root + nested `module` DAGs via `applyeval` (respecting `module.parallelism`, `dynamic_worker.parallelism`/`serial`), persisting `dynamic_worker` keys once, streaming completed candidates into `MergeActor` via `EvalMergeBase`, handling `FallbackTriggered` / `MergeReworkSource` / `RerunNode` epoch cascades, `PauseRun` (`planned`/`immediate`), `ResumeRun`, `ListDecisions`, `GetRunState` (with `EvaluatedOutput` sensitivity metadata), and deferred `DeleteRun` cleanup.
- `internal/actor/run/run_test.go` & `internal/actor/run/run_integration_test.go`: Tier 1 deterministic unit tests and Tier 2 real `daprhost` + Git + `WorkerDaemon` mid-execution host-restart recovery integration test (`//go:build integration`).

## Technical Context

**Language/Version**: Go 1.27 (`module github.com/lonegunmanb/sofa`)  
**Primary Dependencies**: `github.com/dapr/go-sdk@v1.16.0`, `github.com/hashicorp/hcl/v2`, `github.com/lonegunmanb/golden`, `github.com/zclconf/go-cty/cty`, `internal/applyeval`, `internal/actor/{node,merge,config,registry}`, `internal/plan`, `internal/blob`, `internal/git`, `internal/artifact`, `internal/daprhost`, `internal/worker/{daemon,runner,spec}`.  
**Storage**: Dapr Actor State Manager (`"operator_actor_v1"` and `"run_actor_v1"` single versioned JSON documents via explicit `Contains`+`Get` and `Set`+`Save`; never `Remove`) + `BlobStore` (`runs/<run_id>/plan/plan.sofaplan`, artifact zips).  
**Testing**: `go test -v ./internal/actor/run/... -count=1` (Tier 1 in-memory) and `go test -v -tags=integration ./internal/actor/run/... -count=1` (Tier 2 real `daprhost.Manager` + local bare Git repo + `WorkerDaemon`).  
**Constraints**: PowerShell 5.1 (no `&&`); `SetStateManager` override using `daprhost.NewActorStateManager`; explicit `Set`+`Save` in `ReminderCall`; lossless error flags/codes on `*Resp` + error translation helpers; worker-only callback servers return `{"entities":[]}` on `GET /dapr/config`; no `git commit` or `git push`.

## Constitution Check

- **I. Spec-First Delivery**: Spec Kit artifacts under `specs/028-operator-and-run-actor/` authored prior to tests and production code. PASS.
- **II. Strict TDD**: `tasks.md` strictly orders failing Tier 1 and Tier 2 tests before production implementation. PASS.
- **III. Pure Core & Real-Dependency Boundaries**: `RunActor` and `OperatorActor` isolate all Dapr RPC, `NodeActor`, `MergeActor`, `ConfigActor`, `BlobStore`, `GitAdapter`, PubSub, and Reminder boundaries behind clean Go interfaces; Tier 1 tests run in-memory and Tier 2 runs against real `daprhost` + Git + `WorkerDaemon`. PASS.
- **IV. Runtime Enforcement Over Prompt Trust**: `git.Ledger` branch ownership/preservation, `ErrAlreadyMergedSharedBranch` shared-target rerun fencing, `max_reworks` / `max_cycles` enforcement, and epoch-fenced cascades are enforced deterministically in Go code. PASS.
- **V. Single-Node & Distributed Parity**: `OperatorActor` and `RunActor` run identically over `dapr/go-sdk/actor` and `daprhost.Manager` with full persistent audit state and cold-start recovery. PASS.
- **VI. Research-First & Surgical Task Sizing**: Based on direct inspection of `applyeval`, `node`, `merge`, `config`, `registry`, `plan`, `blob`, `git`, `artifact`, `daprhost`, and `worker/runner`; scoped strictly to `internal/actor/run/`. PASS.

## Project Structure

```text
specs/028-operator-and-run-actor/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/run-actor.md
└── tasks.md

internal/actor/run/
├── types.go
├── operator_actor.go
├── run_actor.go
├── run_test.go
└── run_integration_test.go
```
