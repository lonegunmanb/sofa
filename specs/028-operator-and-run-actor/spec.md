# Feature Specification: `OperatorActor` & `RunActor` Non-Blocking DAG Orchestration, Rework Cascades, and Deferred Cleanup (`028-operator-and-run-actor`)

**Feature Branch**: `028-operator-and-run-actor`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `028` blueprint (`docs/stage3/028-operator-and-run-actor.md`), `docs/architecture-contract.md`, `docs/hcl-contract.md`, `internal/applyeval/{context,eval}.go`, `internal/actor/node/{types,actor}.go`, `internal/actor/merge/{types,actor}.go`, `internal/actor/config/{types,actor,plan_refs}.go`, `internal/actor/registry/{types,actor}.go`, `internal/git/{adapter,ledger}.go`, `internal/plan/{model,codec,archive}.go`, `internal/blob/store.go`, `internal/artifact/service.go`, and `internal/daprhost/{manager,components}.go`.

## User Scenarios & Testing

### User Story 1 - Global Run Index & Cold-Start Active Run Recovery (`OperatorActor("global")`) (Priority: P1)

`OperatorActor` (`ActorType = "OperatorActor"`, singleton ID `"global"`) maintains the persistent global index of `RunSummary` records under state key `"operator_actor_v1"` (explicit `Contains` + `Get`, `Set` + `Save`, never `Remove`).
- `UpsertRun`, `GetRunSummary`, `ListRuns` (supporting optional `Namespace` and `Status` filters, deterministically sorted newest-first), and `RemoveRun` manage the persistent `RunSummary` map.
- `RecoverActiveRuns` scans all indexed Runs whose status is `RUNNING` or `PAUSING` and wakes each corresponding `RunActor(run_id)` via `Reconcile(Reason: "operator_recover")`, driving automatic recovery across host restarts without touching `PAUSED`, `SUCCEEDED`, `FAILED`, or `DELETED` runs.

**Independent Test**: Tier 1 unit tests in `internal/actor/run/run_test.go` verifying `UpsertRun`, `GetRunSummary`, filtered `ListRuns`, `RemoveRun`, and `RecoverActiveRuns` waking exclusively `RUNNING` and `PAUSING` runs; plus Tier 2 integration test `internal/actor/run/run_integration_test.go` (`//go:build integration`) stopping/restarting `daprhost.Manager` mid-run and asserting `RecoverActiveRuns` drives the interrupted Run to `SUCCEEDED`.

### User Story 2 - Non-Blocking DAG Progression, Module Parallelism, One-Time `dynamic_worker` Expansion & Streaming `merge` (Priority: P1)

`RunActor` (`ActorType = "RunActor"`, Actor ID = `<run_id>`) drives the full `.sofaplan` DAG in millisecond-scale non-blocking turns without ever holding Actor locks across container execution:
- **Archive & Cold Re-Hydration**: On `StartRun`, `RunActor` loads `runs/<run_id>/plan/plan.sofaplan` from `BlobStore` via `plan.LoadArchive` (extracting bundle directories and `PluginLock`), resolves all `source` branch SHAs via `git.Adapter` (with scoped `ConfigActor.ResolveForNode` credentials when `token_ref`/`ssh_key_ref` are set), initializes and persists `git.LedgerSnapshot` with protected `SourceRef`s, records `RunSummary` in `OperatorActor`, registers a persistent reconciliation Reminder, and dispatches initial ready nodes. If woken cold after host restart (`Reconcile`, `OnNodeEvent`, `OnMergeCandidateResult`, `ReminderCall`, etc.), `RunActor` lazily re-hydrates its cached `*plan.Plan` and extracted bundle from `BlobStore`.
- **Nested `module` & Parallelism Enforcement**: Supports root plan plus nested `module` plans (`applyeval.EvalModuleInputsAndValidate` on module readiness, failing the Run if deferred variable validation fails), enforcing `module.parallelism` on active concurrent nodes within each module scope as well as `dynamic_worker.parallelism` and `dynamic_worker.serial`.
- **One-Time Persisted `dynamic_worker` Expansion**: Evaluates `applyeval.EvalDynamicWorkerCollection` exactly once when upstream dependencies complete, persists sorted `DynamicKeys` and serialized element values (preserving `golden.SensitiveMark` paths!), and never re-evaluates on reconcile or restart unless cascaded by upstream `fallback`/`rerun`. Empty `Keys` (`len == 0`) immediately marks the `dynamic_worker` `SUCCEEDED`.
- **Streaming `dynamic_worker -> merge`**: Uses `applyeval.EvalMergeBase` so as soon as `merge.based_source` is known and ANY upstream candidate (`worker` or `dynamic_worker[key]`) finishes `SUCCEEDED`, `MergeActor.EnqueueCandidate` is called immediately without waiting for slower sibling instances. Handles 0-candidate `merge` cleanly (initializing target branch from `based_source` when Run-exclusive), tracks merged branches (`WorkBranch` + `CommitSHA`), and unblocks downstream nodes and `applyeval.EvalPlanOutputs` (preserving `OutputSpec.Sensitive` and `cty` sensitivity marks for Slice 029).
- **Resource & Import Materialization**: Extracts `Sandboxes`, `Harnesses`, `ModelProviders`, `MCPServers`, `MCPResources`, `Tools`, `ToolEnvs`, `RequiredEnvKeys` (`config.ExtractNodeEnvRefs`), and maps published upstream artifacts into `artifact.ImportRequest`s on `node.StartOrResumeReq` and `merge.EnqueueCandidateReq`.

**Independent Test**: Tier 1 unit test executing a root DAG with static `worker` -> `dynamic_worker` (3 instances, `module.parallelism = 2`) -> streaming `merge` (asserting `W1` enqueues into `MergeActor` before `W3` completes!) -> top-level `EvalPlanOutputs` (verifying `Sensitive` output metadata + value); plus nested `module` input validation & output propagation.

### User Story 3 - Three Rework / Rerun `epoch` Cascade Protocols (Priority: P1)

1. **`FallbackTriggered`**: Enforces `FallbackConfig.MaxReworks` (`max_cycles`); if exceeded, marks node and Run `FAILED`. Otherwise increments `epoch` (`AdvanceForRework` with `Reason`, `FallbackArtifacts`, and pinned `BaseSHA`) on `ancestorAddr`, cancels matching `MergeActor` tickets and increments `MergeEpoch` on affected `merge` nodes, resets affected downstream nodes (`ResetForRerun`, clearing persisted `dynamic_worker` keys and child module state), and re-dispatches `ancestorAddr`.
2. **`MergeReworkSource`**: When `MergeActor` notifies `MergeStatusReworkSource`, enforces `merge.max_reworks`; if exceeded, marks `merge` and Run `FAILED`. Otherwise advances **ONLY** the conflicting/failing source (`worker` or `dynamic_worker[key]`) via `NodeActor.AdvanceForRework(BaseSHA: LatestTargetSHA, BasedSource: TargetBranch, Reason: FailureReport)` and re-dispatches ONLY that source while keeping all sibling instances untouched.
3. **`RerunNode(nodeAddr)`**: Checks whether `nodeAddr` or any transitive downstream node in this Run has `MergedIntoSharedBranch == true`. If so, rejects with `ErrAlreadyMergedSharedBranch` (`AlreadyMergedSharedBranch: true`, `ErrorCode: "ALREADY_MERGED_SHARED_BRANCH"` -> HTTP `409 Conflict` in Slice 029) without mutating any state. Otherwise cancels matching `MergeActor` tickets, resets `nodeAddr` and all transitive downstream nodes (`NodeActor.ResetForRerun`, incrementing `MergeEpoch` on Run-exclusive downstream `merge` nodes, clearing downstream `dynamic_worker` keys), and re-dispatches.

**Independent Test**: Tier 1 unit tests covering `fallback` cascade + `max_cycles` -> `FAILED`, `MergeReworkSource` advancing only the conflicting `dynamic_worker[key]` to `epoch=2` with `BaseSHA = LatestTargetSHA` without touching siblings, and `RerunNode` rejection (`errors.Is(err, ErrAlreadyMergedSharedBranch)`) after shared `target_branch` merge vs successful cascade on Run-exclusive `merge` / `worker`.

### User Story 4 - `planned` / `immediate` Pause, Resume, and Deferred `DeleteRun` Cleanup (Priority: P1)

- `PauseRun("planned")`: Sets `ManualPaused = true`, dispatches no new nodes, lets currently `RUNNING` nodes finish (`PAUSING` -> `PAUSED` once idle).
- `PauseRun("immediate")`: Immediately cancels queued/in-flight `MergeActor` tickets via `CancelByRun`, calls `NodeActor.PauseNode(Mode: "immediate")` (aborting active containers and incrementing `epoch++`), and transitions Run to `PAUSED`.
- `ResumeRun`: Clears `ManualPaused`, resets paused/failed nodes for resumption from their latest checkpoints, re-enqueues unmerged candidates if needed, and reconciles.
- **Deferred Cleanup Iron Law**: `SUCCEEDED`, `FAILED`, and `PAUSED` runs delete **ZERO** Git branches and **ZERO** `BlobStore` objects. Only `DeleteRun` fences/aborts active `NodeActor`s and `MergeActor` tickets, computes `git.Ledger.PlanCleanup(sources, preserves)` (preserving all `source` branches and evaluated `preserve` branches including dynamic `branch_expr`s), deletes only intermediate `sofa/ckpt/*`, `sofa/work/*`, and Run-exclusive merge branches via `git.Adapter.DeleteRemoteBranch`, calls `BlobStore.DeletePrefix("runs/<run_id>/")`, calls `ConfigActor.DeleteRunScope(runID)`, removes the Run from `OperatorActor`, unregisters reminders, and marks the Run `DELETED`.

**Independent Test**: Tier 1 unit tests verifying `Pause("planned")`, `Pause("immediate")`, `Resume`, zero deletions on `SUCCEEDED`/`FAILED`/`PAUSED`, and complete verified cleanup on `DeleteRun` preserving `main` and `preserve` branches.

### Edge Cases

- Calling `RerunNode` or `GetRunState` over Dapr RPC preserves domain errors (`ErrAlreadyMergedSharedBranch`, `ErrRunNotFound`, `ErrNodeNotFound`, `ErrRunConflict`) losslessly via structured fields on response structs and exported client/error-decoding helpers.
- A `dynamic_worker` whose `for_each` evaluates to an empty collection (`len == 0`) marks the `dynamic_worker` `SUCCEEDED` immediately; a downstream `merge` with 0 candidates resolves `WorkBranch`/`CommitSHA` cleanly without stalling.
- Calling `RegisterManagedBranch` from `NodeActor` or `MergeActor` enforces `git.Ledger` protected-source and ownership invariants, persisting the updated `LedgerSnapshot` in `RunActor` state and returning structured error codes on violation.
- `ReminderCall` explicitly saves state via `Set` + `Save` whenever mutating state, and automatically unregisters the reconcile reminder once the Run reaches a terminal state (`SUCCEEDED`, `FAILED`, `DELETED`) or `PAUSED` (re-registering on `ResumeRun` / `RerunNode`).

## Requirements

### Functional Requirements

- **FR-001**: `internal/actor/run` MUST define `OperatorActorType = "OperatorActor"`, `GlobalOperatorActorID = "global"`, `RunActorType = "RunActor"`, sentinel errors (`ErrAlreadyMergedSharedBranch`, `ErrRunNotFound`, `ErrNodeNotFound`, `ErrRunConflict`), `RunStatus` constants, `RunSummary`, `NodeRunSummary`, lossless RPC request/response structs, and injectable consumer interfaces (`NodeController`, `MergeController`, `OperatorIndex`, `ConfigManager`, `EventPublisher`, `RunReconciler`, `ReminderScheduler`).
- **FR-002**: `OperatorActor` MUST persist its `RunSummary` index under `"operator_actor_v1"` using explicit `Contains` + `Get` and `Set` + `Save` (never `Remove`), support `UpsertRun`, `GetRunSummary`, `ListRuns`, `RemoveRun`, and wake all `RUNNING` and `PAUSING` Runs in `RecoverActiveRuns`.
- **FR-003**: `RunActor` MUST lazily load/re-hydrate `.sofaplan` archives from `BlobStore`, pin `source` branch SHAs via `git.Adapter` + scoped `ConfigActor` credentials, maintain and persist `git.LedgerSnapshot` via `RegisterManagedBranch`, advance root and nested `module` DAGs via `applyeval` respecting `module.parallelism` and `dynamic_worker` concurrency, persist `dynamic_worker` keys once, stream completed candidates into `MergeActor.EnqueueCandidate` via `EvalMergeBase`, materialize `artifact.ImportRequest`s and resource catalogs, and evaluate `EvalPlanOutputs` preserving `Sensitive` metadata.
- **FR-004**: `RunActor` MUST implement the three epoch-cascade protocols (`FallbackTriggered` with `max_reworks`/`max_cycles`, `MergeReworkSource` with `merge.max_reworks` advancing only the failing source with `BaseSHA = LatestTargetSHA`, and `RerunNode` rejecting `MergedIntoSharedBranch` with `ErrAlreadyMergedSharedBranch` and otherwise cascading downstream resets).
- **FR-005**: `RunActor` MUST implement `PauseRun("planned" | "immediate")`, `ResumeRun`, `GetRunState`, `ListDecisions`, and deferred `DeleteRun` cleanup (zero branch/blob deletion on `SUCCEEDED`/`FAILED`/`PAUSED`; complete fenced cleanup on `DeleteRun` preserving `source` and `preserve` branches).

## Success Criteria

### Measurable Outcomes

- **SC-001**: `go test -v ./internal/actor/run/... -count=1` and `go test ./... -count=1` pass offline in seconds with zero external processes or containers.
- **SC-002**: `go test -v -tags=integration ./internal/actor/run/... -count=1` passes end-to-end against real `daprhost.Manager`, `OperatorActor`, `ConfigActor`, `ProcessRegistryActor`, `RunActor`, `NodeActor`, `WorkerDaemon`, and local bare Git repo, recovering mid-run host stop via `OperatorActor.RecoverActiveRuns` to `SUCCEEDED` with `epoch >= 2` and zero leftover `scheduler`/`daprd` processes.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly.
