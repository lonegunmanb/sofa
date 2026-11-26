# Feature Specification: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery (`017-two-phase-checkpoint-and-recovery`)

**Feature Branch**: `017-two-phase-checkpoint-and-recovery`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `017` blueprint (`docs/stage2/017-two-phase-checkpoint-and-recovery.md`), `README.md` ("检查点与恢复"), `docs/architecture-contract.md` (§5.2), `docs/stage2/018-worker-qc-and-merge-runner.md`, `internal/blob/`, `internal/artifact/`, `internal/git/`, and `internal/session/diary.go`.

## User Scenarios & Testing

### User Story 1 - Control-Plane State Port & Strict Epoch Fencing (`internal/checkpoint/model.go`, `local_store.go`) (Priority: P1)

`NodeStateStore` abstracts the control-plane operations required for epoch-scoped checkpointing (`RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`, `ListCheckpoints`) with `LocalNodeStateStore` providing a thread-safe in-memory and optional atomic JSON-file-backed implementation (plus optional `*git.Ledger` registration) that enforces strict monotonic `epoch` fencing on every mutating call.

**Independent Test**: Tier 1 unit tests in `internal/checkpoint/checkpoint_test.go` verify that advancing `LocalNodeStateStore` from `epoch = 1` to `epoch = 2` causes any subsequent `RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, or `CompleteCheckpoint` call with `reqEpoch = 1` to fail immediately with `ErrStaleEpoch` without mutating state or persisted JSON, and that `ListCheckpoints` returns deep-copied records ordered newest-to-oldest (`Seq` descending).

**Acceptance Scenarios**:

1. **Given** a `LocalNodeStateStore` at authoritative `epoch = 1`, **When** `AdvanceEpoch(2)` is called and a stale caller invokes `RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, or `CompleteCheckpoint` with `reqEpoch = 1`, **Then** every call returns `ErrStaleEpoch` immediately and leaves stored records and branch registrations untouched.
2. **Given** a `LocalNodeStateStore` configured with a `StatePath`, **When** checkpoints are begun, updated, completed, or epoch is advanced, **Then** state is written atomically via temp-file + rename and reloads cleanly in a new `LocalNodeStateStore` instance without downgrading the persisted epoch.
3. **Given** a `LocalNodeStateStore` wired to a `*git.Ledger` and `OwnerNode`, **When** `RegisterCheckpointBranch` succeeds, **Then** the checkpoint branch is registered in `*git.Ledger` as `git.KindCheckpointBranch` with the expected `baseSHA` and `epoch`.

---

### User Story 2 - Epoch-Isolated Two-Phase Checkpoint Save (`internal/checkpoint/manager.go`) (Priority: P1)

`Manager.SaveCheckpoint` enforces pre-checkpoint `writable_paths` boundaries, reads workspace `HEAD` via `git.Adapter.HeadSHA`, idempotently registers the epoch branch `sofa/ckpt/<run_id>/<node_key>/e<epoch>`, executes Phase 1 (`BeginCheckpoint` pending diary + `initialSHA`), Phase 2 (`CommitAllAndPush` + conditional `AppendFinalSHA` when `committed && finalSHA != initialSHA` + `artifact.Service.SnapshotStagingDir`), and Phase 3 (three-way verification via `BranchContainsSHA` and `blob.Store.Stat` before `CompleteCheckpoint`).

**Independent Test**: Tier 1 unit tests exercise clean workspaces (`committed == false`, `finalSHA == initialSHA`, `AppendFinalSHA` skipped while remote branch push and `CompleteCheckpoint` succeed), dirty workspaces (`committed == true`, `finalSHA != initialSHA`, `AppendFinalSHA` called and persisted), unauthorized `writable_paths` rejection before `BeginCheckpoint`, and mid-`CommitAllAndPush` epoch advancement causing immediate `ErrStaleEpoch` fencing.

**Acceptance Scenarios**:

1. **Given** a workspace containing files modified outside `writable_paths`, **When** `SaveCheckpoint` runs, **Then** it fails immediately with an unauthorized-paths error without calling `RegisterCheckpointBranch`, `BeginCheckpoint`, or `CommitAllAndPush`.
2. **Given** a clean workspace (`committed == false`, `finalSHA == initialSHA`), **When** `SaveCheckpoint` runs, **Then** `CommitAllAndPush` still ensures the remote `e<epoch>` branch points to `initialSHA`, `AppendFinalSHA` is NOT called, and `CompleteCheckpoint` records `FinalSHA == initialSHA` and `StatusComplete`.
3. **Given** a dirty workspace (`committed == true`, `finalSHA != initialSHA`) and non-empty `artifactsDir`, **When** `SaveCheckpoint` runs, **Then** `AppendFinalSHA` records `finalSHA`, `SnapshotStagingDir` uploads a deterministic ZIP snapshot, three-way alignment (`BranchContainsSHA` + `blob.Store.Stat`) passes, and `CompleteCheckpoint` marks the record `StatusComplete`.
4. **Given** authoritative `epoch` advancing from `1` to `2` during `CommitAllAndPush`, **When** `SaveCheckpoint` for `epoch = 1` proceeds to `AppendFinalSHA` or `CompleteCheckpoint`, **Then** it immediately fails with `ErrStaleEpoch` and never pollutes `epoch = 2` state.

---

### User Story 3 - Cross-Machine Newest-to-Oldest Aligned Recovery (`internal/checkpoint/manager.go`) (Priority: P1)

`Manager.RecoverLatestAligned` scans historical checkpoints from newest to oldest (`Seq` descending), discarding incomplete (`StatusPending`) records, records whose `FinalSHA` is absent from the remote checkpoint branch, or records whose `ArtifactsSnapshot` is missing or digest-mismatched in `BlobStore`, and recovers the first three-way-aligned checkpoint into a fresh machine workspace and artifacts directory with chronological diary context.

**Independent Test**: Tier 1 fault-injection tests (Cases A, B, C) verify fallback from `Seq=2` to `Seq=1` when `Seq=2` is `StatusPending`, unaligned on remote Git branch, or missing/corrupted in `BlobStore` (ensuring `Seq=2`'s incomplete diary is excluded from recovered diary context); Tier 2 integration tests in `internal/checkpoint/checkpoint_integration_test.go` verify full Machine A crash + `os.RemoveAll` -> Machine B recovery on `epoch=2` -> subsequent `SaveCheckpoint` on `e2` branch against real `git init --bare`, `git.CLIAdapter`, `blob.LocalStore`, `artifact.Service`, and file-backed `LocalNodeStateStore`.

**Acceptance Scenarios**:

1. **Given** `Seq=1` (`StatusComplete`) and `Seq=2` (`StatusPending` after crash between `CommitAllAndPush` and `CompleteCheckpoint`), **When** `RecoverLatestAligned` runs with `newEpoch = 2`, **Then** it skips `Seq=2`, registers `sofa/ckpt/.../e2` at `Seq=1.FinalSHA`, checks out `Seq=1.FinalSHA`, restores `Seq=1` artifacts, and formats only `Seq=1` in `ChronologicalDiaries` and `DiaryPromptContext`.
2. **Given** `Seq=2` marked `StatusComplete` in store whose `FinalSHA` is NOT contained in `rec.Branch` on the remote, **When** `RecoverLatestAligned` runs, **Then** it discards `Seq=2` and falls back to `Seq=1`.
3. **Given** `Seq=2` marked `StatusComplete` in store whose `ArtifactsSnapshot` is missing (`ErrNotFound`) or SHA-256 mismatched in `BlobStore`, **When** `RecoverLatestAligned` runs, **Then** it discards `Seq=2` and falls back to `Seq=1`.
4. **Given** zero aligned checkpoints (empty store or all candidates unaligned/pending), **When** `RecoverLatestAligned` runs, **Then** it returns `ErrNoAlignedCheckpoint`.

### Edge Cases

- `git.Adapter.HeadSHA` validates a 40-character lowercase hex commit SHA via `git rev-parse HEAD` inside `CLIAdapter` so `internal/checkpoint` never imports `os/exec`.
- Deterministic checkpoint artifact blob keys sanitize `runID` and `nodeAddr` (including `dynamic_worker` bracketed addresses) with a SHA-256 suffix so every key passes `blob.ValidateKey` without collisions.
- Multiple checkpoints within the same `epoch` only register the `e<epoch>` branch in `*git.Ledger` once so `Ledger.Register` never rejects subsequent checkpoints whose `initialSHA` advances beyond the original `baseSHA`.

## Requirements

- **FR-001**: `internal/git/adapter.go` and `internal/git/cli.go` MUST expose `HeadSHA(ctx context.Context, workspaceDir string) (string, error)` returning a validated 40-char lowercase hex commit SHA.
- **FR-002**: `internal/checkpoint/model.go` MUST define `StatusPending`, `StatusComplete`, `CheckpointRecord`, `NodeStateStore`, `ErrStaleEpoch`, and `ErrNoAlignedCheckpoint` matching `docs/stage2/017-two-phase-checkpoint-and-recovery.md` §3.1 verbatim.
- **FR-003**: `internal/checkpoint/local_store.go` MUST implement thread-safe `LocalNodeStateStore` with configurable initial epoch, `AdvanceEpoch`/`CurrentEpoch`, optional `*git.Ledger` registration, optional atomic JSON persistence, strict `reqEpoch == currentEpoch` fencing on every mutating method, and descending `Seq` deep-copied `ListCheckpoints`.
- **FR-004**: `internal/checkpoint/manager.go` MUST implement `Manager`, `SaveCheckpoint`, `RecoverLatestAligned`, and `FormatDiaryPromptContext` with zero `os/exec` imports.

## Success Criteria

- **SC-001**: `gofmt -l .` produces empty output.
- **SC-002**: `go vet ./...` passes with zero diagnostics.
- **SC-003**: `go test ./... -count=1` passes offline with all Tier 1 unit tests in `internal/checkpoint/checkpoint_test.go` green.
- **SC-004**: `go test -tags=integration ./internal/git/... ./internal/checkpoint/... -count=1` passes against real local Git bare repositories and `blob.LocalStore`.
