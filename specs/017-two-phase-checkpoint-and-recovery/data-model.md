# Data Model: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery (`017-two-phase-checkpoint-and-recovery`)

## `internal/checkpoint/model.go`

| Entity / Field | Rules & Invariants |
|---|---|
| `Status` | `StatusPending = "PENDING"`, `StatusComplete = "COMPLETE"` |
| `CheckpointRecord` | `Seq` (`uint64`, monotonic 1-based), `Epoch` (`uint64`), `Branch` (`string`), `InitialSHA` (40-char hex), `FinalSHA` (empty until `AppendFinalSHA` or `CompleteCheckpoint`), `ArtifactsSnapshot` (`*blob.ObjectMeta`, optional), `Diary` (`session.DiaryInput`), `Status` (`Status`) |
| `NodeStateStore` | `RegisterCheckpointBranch`, `BeginCheckpoint`, `AppendFinalSHA`, `CompleteCheckpoint`, `ListCheckpoints` |
| `ErrStaleEpoch` | Returned immediately by every mutating `NodeStateStore` method when `reqEpoch != currentEpoch` |
| `ErrNoAlignedCheckpoint` | Returned by `RecoverLatestAligned` when no record is complete and three-way aligned |

## `internal/checkpoint/local_store.go`

| Entity / Field | Rules & Invariants |
|---|---|
| `LocalNodeStateStoreOptions` | `InitialEpoch` (defaults to `1` if `0` when initializing fresh state), `Ledger` (optional `*git.Ledger`), `OwnerNode` (required when `Ledger != nil`), `StatePath` (optional JSON file path) |
| `LocalNodeStateStore` | Thread-safe (`sync.RWMutex`); `AdvanceEpoch(nextEpoch)` requires `nextEpoch > currentEpoch`; `ListCheckpoints` returns deep-copied records sorted `Seq` descending |

## `internal/checkpoint/manager.go`

| Entity / Field | Rules & Invariants |
|---|---|
| `ManagerConfig` | `RunID`, `NodeAddr`, `RepoURL`, `Auth`, `StateStore`, `GitAdapter`, `BlobStore`, optional `ArtifactSvc` (defaults to `artifact.NewService(BlobStore, ServiceOptions{})`) |
| `RecoveryResult` | `Record` (aligned `CheckpointRecord`), `NewCheckpointBranch` (`e<newEpoch>` branch), `ChronologicalDiaries` (`StatusComplete` records with `Seq <= Record.Seq` sorted `Seq` ascending), `DiaryPromptContext` (formatted via `FormatDiaryPromptContext`) |
