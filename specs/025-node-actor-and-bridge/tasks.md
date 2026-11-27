---
description: "Stage 3 Slice 025 TDD tasks: NodeActor authoritative state machine, decision cards, and RemoteNodeControlPlane bridge (internal/actor/node)"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/actor/node/node_test.go` covering:
  - `EncodeNodeActorID` / `DecodeNodeActorID` lossless URL-safe round-trip on `dynamic_worker` addresses containing `/`, `"`, `[`, `]`, spaces, and `%` (asserting strictly `[A-Za-z0-9_-]`);
  - Monotonic `epoch` fencing: zombie worker at `epoch=1` calling `RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`, `SaveQCLedgerSnapshot`, `SavePendingPublication`, `RegisterWorkBranch`, `DrainInterjections`, and `ReportNodeOutcome` after `NodeActor` advances to `epoch=2` MUST receive `checkpoint.ErrStaleEpoch` through `RemoteNodeControlPlane` and leave `epoch=2` state unpolluted;
  - `StartOrResume` + `ClaimSlot` saturation -> `QUEUED_SLOT` + reminder registration -> `ReminderCall` explicit persistence (`Set` + `Save`) & dispatch on slot availability; `StartNode` failure -> `ReleaseSlot` + `ReportSuspect` + `QUEUED_SLOT`;
  - `OnWorkerDead` matching active lease -> `CurrentEpoch++`, clear old lease, auto re-dispatch; stale `OnWorkerDead` safely ignored;
  - Git credential materialization: `RequiredEnvKeys` + `TokenRef`/`SSHKeyRef` resolved via `ConfigResolver` into both `StartNodeRequest.ResolvedEnv` and `StartNodeRequest.Auth`;
  - `human_approval == "required"` exact `(CommitSHA, ArtifactIDs)` binding: stale approval auto-invalidated when a new commit/artifact set arrives; mismatched `CommitSHA` / `ArtifactIDs` submission rejected;
  - `SubmitDecision` across `request_decision`, `pause_for_human`, and `human_approval`: manual `PAUSED` node/run records decision without starting a container; `WAITING_HUMAN` auto-resumes with `CurrentEpoch++`;
  - `PauseNode("immediate")`, `AdvanceForRework` (updating `BaseSHA`, clearing `PendingPublication`/`Approval`, injecting high-priority interjection), and `ResetForRerun`;
  - `RegisterCheckpointBranch` & `RegisterWorkBranch` invoking `RunNotifier.RegisterManagedBranch` synchronously before returning.
- [X] T002 [Green] Implement `internal/actor/node/types.go`, `internal/actor/node/actor.go`, and `internal/actor/node/control_plane_client.go`, and verify `go test -v ./internal/actor/node/... -count=1` passes.
- [X] T003 [Red -> Green] Write Tier 2 real-Dapr integration test `internal/actor/node/node_integration_test.go` (`//go:build integration`) using `daprhost.Manager` (`scheduler` + `daprd`), local bare Git repo, `blob.LocalStore`, `ConfigActor`, `ProcessRegistryActor`, `WorkerDaemon`, `WorkerRunner` (with deterministic fake `SessionLauncher`), and `RemoteNodeControlPlane`:
  - Execute `StartOrResume` (`epoch=1`) -> `WorkerRunner` saves checkpoint and pauses at `WAITING_HUMAN` (container closed & slot released) -> `NodeActor.SubmitDecision` -> automatic `epoch=2` checkpoint recovery -> `SUCCEEDED` with `work_branch` pushed to bare Git remote;
  - Verify `go test -v -tags=integration ./internal/actor/node/... -count=1` passes with zero leftover `scheduler`/`daprd` processes.
- [X] T004 [Refactor & Verify] Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/actor/node/... -count=1`, verify zero stray processes, and mark tasks `[X]`.
