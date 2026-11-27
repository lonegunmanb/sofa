# Feature Specification: `NodeActor` Authoritative State Machine, Interjections/Decision Cards & `NodeControlPlane` Bridge (`025-node-actor-and-bridge`)

**Feature Branch**: `025-node-actor-and-bridge`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `025` blueprint (`docs/stage3/025-node-actor-and-bridge.md`), `docs/stage3/028-operator-and-run-actor.md`, `docs/stage3/029-rest-ws-api-server.md`, `docs/stage3/README.md`, `docs/architecture-contract.md`, `internal/checkpoint/{model.go,local_store.go,manager.go}`, `internal/worker/runner/{types.go,worker_runner.go}`, `internal/session/{diary.go,gates.go,protocol_tools.go}`, `internal/git/{adapter.go,ledger.go,cli.go}`, `internal/actor/config/`, `internal/actor/registry/`, and `internal/worker/daemon/daemon.go`.

## User Scenarios & Testing

### User Story 1 - URL-Safe Bidirectional Addressing & Non-Blocking Slot Dispatch with Failover (Priority: P1)

`RunActor` (`028`) dispatches each static `worker` and each expanded `dynamic_worker` instance to an independent Dapr `NodeActor` (`ActorType = "NodeActor"`). Because `canonicalNodeAddr` may contain slashes, brackets, and quotes (e.g. `dynamic_worker.x["a/b"]`), Actor IDs MUST be encoded losslessly and URL-safely via `EncodeNodeActorID(runID, canonicalNodeAddr)` / `DecodeNodeActorID(actorID)` using only `[A-Za-z0-9_-]`.

Upon `StartOrResume(ctx, req)`:
- `NodeActor` persists its `DispatchSpec` and resolves the node's minimal credential set (`RequiredEnvKeys` plus any Git `TokenRef`/`SSHKeyRef`) via `ConfigResolver.ResolveForNode`, materializing resolved values into `daemon.StartNodeRequest.ResolvedEnv` and `daemon.StartNodeRequest.Auth` (`git.AuthConfig`) so worker `GitAdapter`s never read worker host `os.Environ()`;
- `NodeActor` claims an execution slot via `SlotManager.ClaimSlot`:
  - If `Granted == false`, `NodeActor` transitions to `QUEUED_SLOT`, registers a short-period Dapr reminder (`DefaultSlotRetryInterval = 1s`, configurable in tests), and returns immediately without blocking `RunActor`;
  - On `ReminderCall`, `NodeActor` explicitly saves state (`Set` + `Save`) and retries dispatch outside its mutex;
  - Once a slot is granted, `NodeActor` persists `(CurrentEpoch, LeaseID, LeaseKey, WorkerProcessID, WorkerAppID)`, transitions to `RUNNING`, and invokes `WorkerDispatcher.StartNode`; if `StartNode` fails, `NodeActor` releases the slot, calls `SlotManager.ReportSuspect`, transitions back to `QUEUED_SLOT`, and schedules the retry reminder;
- Upon `OnWorkerDead` matching the active `(LeaseID, WorkerProcessID)` while `RUNNING`, `NodeActor` monotonically increments `CurrentEpoch++`, clears the dead lease, and re-enters slot dispatch to resume from the latest aligned checkpoint.

**Independent Test**: Tier 1 unit tests in `internal/actor/node/node_test.go` verifying lossless `EncodeNodeActorID`/`DecodeNodeActorID` round-trip with special characters, `QUEUED_SLOT` + reminder retry dispatch + explicit state persistence in `ReminderCall`, `StartNode` failure fallback (`ReleaseSlot` + `ReportSuspect` + `QUEUED_SLOT`), Git credential materialization into `ResolvedEnv` and `Auth`, and `OnWorkerDead` `epoch++` failover.

### User Story 2 - Monotonic Execution Epoch Fencing & Lossless `RemoteNodeControlPlane` Bridge (Priority: P1)

`NodeActor` is the single authoritative control plane for all `runner.NodeControlPlane` operations across machines, and `RemoteNodeControlPlane` (`internal/actor/node/control_plane_client.go`) implements `runner.NodeControlPlane` over `NodeActor` `ClientStub` so `WorkerRunner` runs distributedly without changing Stage 2 code.

Because `dapr/go-sdk@v1.16.0` converts any non-nil Go `error` returned by an Actor method into a generic `ErrActorInvokeFailed` (losing the original error text/sentinel), `NodeActor` epoch-fenced RPCs MUST return `nil` Go error with structured `StaleEpoch: true` (plus structured `ErrorCode`/`ErrorMessage` for domain validation errors) in the response struct when `req.Epoch != CurrentEpoch` or a validation invariant fails. `RemoteNodeControlPlane` MUST translate `resp.StaleEpoch == true` back into `checkpoint.ErrStaleEpoch` and preserve `git.ErrInvalidBaseSHA`, `git.ErrProtectedSourceBranch`, `git.ErrBranchOwnershipConflict`, `blob.ErrDigestMismatch`, and `blob.ErrInvalidKey`.

`RegisterCheckpointBranch` and `RegisterWorkBranch` MUST verify `epoch == CurrentEpoch` and synchronously invoke `RunNotifier.RegisterManagedBranch` (recording in the Run branch ledger) before returning to allow any remote Git push. `PauseNode("immediate")`, `AdvanceForRework`, and `ResetForRerun` MUST monotonically increment `CurrentEpoch++`, abort any active worker (`abort_node` + `ReleaseSlot`), update `BaseSHA`/`BasedSource` when provided, clear stale `PendingPublication`/`ApprovalRecord`, and inject rework reasons as high-priority interjections.

**Independent Test**: Tier 1 unit tests verifying that a zombie worker at `epoch=1` calling `RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`, `SaveQCLedgerSnapshot`, `SavePendingPublication`, `RegisterWorkBranch`, `DrainInterjections`, or `ReportNodeOutcome` after `NodeActor` has advanced to `epoch=2` receives `checkpoint.ErrStaleEpoch` through `RemoteNodeControlPlane` and never mutates `epoch=2` state.

### User Story 3 - Human Interjections, Decision Cards & Version-Bound Approval Invalidation (Priority: P1)

`NodeActor` manages:
- Structured checkpoints (`CheckpointRecord` with `DiaryInput`), FIFO human interjections (`EnqueueInterjection` returning ID and confirmation status, `DrainInterjections`, `HasPendingInterjections`), and `QC` ledger snapshots;
- Automatic `DecisionCard` creation when `ReportNodeOutcome` reports `StatusWaitingHuman` across all three pause shapes: `DecisionRequest` (`sofa_request_decision`), `PauseReason` (`sofa_pause_for_human`), and `PendingApproval` (`human_approval == "required"`).
- Strict `(CommitSHA, ArtifactIDs)` version binding for `human_approval == "required"`:
  - If a subsequent run after interjection/rework produces a new `(CommitSHA', ArtifactIDs')` that differs from an existing `ApprovalRecord`, `NodeActor` automatically invalidates the stale approval and creates a new `PENDING` `human_approval` `DecisionCard` (`WAITING_HUMAN`);
  - Calling `SubmitDecision` with a non-matching `CommitSHA` or `ArtifactIDs`, or against an answered/invalidated card, is explicitly rejected;
- Safe manual pause handling: when the node (`ManualPaused == true` / `StatusPaused`) or parent Run (`RunPaused == true`) is manually paused, `SubmitDecision` persists the answer/approval and updates the `DecisionCard` to `ANSWERED`, NEVER launching a worker container until resumed; when not manually paused, `SubmitDecision` increments `CurrentEpoch++` and automatically dispatches resumption from the latest checkpoint.

**Independent Test**: Tier 1 unit tests for all three `DecisionCard` kinds, `(CommitSHA, ArtifactIDs)` mismatch rejection and automatic invalidation on new commit, and manual pause protection in `SubmitDecision`; plus Tier 2 real-Dapr integration test `internal/actor/node/node_integration_test.go` (`//go:build integration`) driving `StartOrResume` (`epoch=1`) -> `WorkerRunner` checkpoint + `WAITING_HUMAN` (container closed & slot released) -> `NodeActor.SubmitDecision` -> automatic `epoch=2` checkpoint recovery -> `SUCCEEDED`.

### Edge Cases

- `ReportNodeOutcome` with `Aborted: true` or stale `Epoch` is safely ignored (returns `StaleEpoch`/`IgnoredAborted`) without overwriting `PAUSED` or `epoch+1` state with `FAILED`.
- `RegisterCheckpointBranch` and `RegisterWorkBranch` re-check `CurrentEpoch` after `RunNotifier.RegisterManagedBranch` returns before committing `NodeActor` state, preventing concurrent epoch races.
- `SubmitDecision` when `human_approval` is rejected (`Approved: false, ReturnedUpstream: false`) records the rejection and injects feedback so subsequent resumption does not loop blindly on the same commit.
- Empty `ConfigResolver` / `RunNotifier` in isolated unit tests behave gracefully when no keys or external ledger callbacks are configured, while missing required keys returned by `ConfigResolver` fail dispatch cleanly without launching a worker.

## Requirements

### Functional Requirements

- **FR-001**: `internal/actor/node` MUST define `ActorType = "NodeActor"`, `EncodeNodeActorID(runID, canonicalNodeAddr) (string, error)` / `DecodeNodeActorID(actorID) (string, string, error)` restricted to `[A-Za-z0-9_-]`, `NodeStatus` (`PENDING`, `QUEUED_SLOT`, `RUNNING`, `WAITING_HUMAN`, `SUCCEEDED`, `FALLBACK_TRIGGERED`, `FAILED`, `PAUSED`), `DecisionCard` (`request_decision`, `pause_for_human`, `human_approval` with `PENDING`/`ANSWERED`/`INVALIDATED`), and injectable boundaries (`ConfigResolver`, `SlotManager`, `WorkerDispatcher`, `RunNotifier`, `ReminderScheduler`).
- **FR-002**: `NodeActor` MUST implement `actor.ServerContext` and `actor.ReminderCallee`, persisting a single `"node_actor_v1"` document explicitly via `Set` + `Save` in RPCs and `ReminderCall`, and executing all external RPCs (`ResolveForNode`, `ClaimSlot`, `ReleaseSlot`, `ReportSuspect`, `StartNode`, `AbortNode`, `RegisterManagedBranch`, `NotifyNodeEvent`, reminder registration) outside `NodeActor`'s mutex.
- **FR-003**: `NodeActor` MUST enforce monotonic `CurrentEpoch` fencing across `RegisterCheckpointBranch`, `RegisterWorkBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`, `SaveQCLedgerSnapshot`, `SavePendingPublication`, `DrainInterjections` (when epoch > 0), and `ReportNodeOutcome`, returning `StaleEpoch: true` with `nil` Go error so `RemoteNodeControlPlane` restores `checkpoint.ErrStaleEpoch` losslessly over `dapr/go-sdk`.
- **FR-004**: `StartOrResume`, `ReminderCall`, `OnWorkerDead`, `PauseNode`, `AdvanceForRework`, and `ResetForRerun` MUST implement non-blocking `QUEUED_SLOT` reminder retry, `DEAD` worker takeover (`epoch++`), `planned`/`immediate` pause semantics, Git credential materialization (`ResolvedEnv` + `AuthConfig`), and rework/rerun state resets with high-priority interjection injection.
- **FR-005**: `SubmitDecision` and `ReportNodeOutcome` MUST enforce exact `(CommitSHA, ArtifactIDs)` version binding for `human_approval == "required"` (auto-invalidating stale approvals on commit/artifact changes and rejecting mismatched submissions) and MUST NEVER launch a container when the node or Run is manually `PAUSED`.
- **FR-006**: `RemoteNodeControlPlane` in `internal/actor/node/control_plane_client.go` MUST compile-time assert and implement `runner.NodeControlPlane` over `*ClientStub`.

## Success Criteria

### Measurable Outcomes

- **SC-001**: `go test ./internal/actor/node/... -count=1` and `go test ./... -count=1` pass offline with zero external processes.
- **SC-002**: `go test -v -tags=integration ./internal/actor/node/... -count=1` passes end-to-end against real `daprhost.Manager` (`scheduler` + `daprd`), real local bare Git repository, `blob.LocalStore`, `ConfigActor`, `ProcessRegistryActor`, `WorkerDaemon`, and `WorkerRunner` with zero orphan `scheduler`/`daprd` processes.
- **SC-003**: `gofmt -l .` is empty and `go vet ./...` passes cleanly.
