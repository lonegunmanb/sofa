# Contract: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery (`017`)

## 1. `internal/git` Additions (`adapter.go`, `cli.go`)

- `Adapter.HeadSHA(ctx context.Context, workspaceDir string) (string, error)`: returns the current 40-char lowercase hex `HEAD` commit SHA of `workspaceDir`.

## 2. `internal/checkpoint/model.go`

- `Status`: `StatusPending = "PENDING"`, `StatusComplete = "COMPLETE"`.
- `CheckpointRecord`: `Seq`, `Epoch`, `Branch`, `InitialSHA`, `FinalSHA`, `ArtifactsSnapshot`, `Diary`, `Status`.
- `ErrStaleEpoch`, `ErrNoAlignedCheckpoint`.
- `NodeStateStore`:
  - `RegisterCheckpointBranch(ctx context.Context, epoch uint64, repoURL, branch, baseSHA string) error`
  - `BeginCheckpoint(ctx context.Context, reqEpoch uint64, branch, initialSHA string, diary session.DiaryInput) (uint64, error)`
  - `AppendFinalSHA(ctx context.Context, reqEpoch, seq uint64, finalSHA string) error`
  - `CompleteCheckpoint(ctx context.Context, reqEpoch, seq uint64, finalSHA string, artifactsSnap *blob.ObjectMeta) error`
  - `ListCheckpoints(ctx context.Context) ([]CheckpointRecord, error)`

## 3. `internal/checkpoint/local_store.go`

- `NewLocalNodeStateStore(opts LocalNodeStateStoreOptions) (*LocalNodeStateStore, error)`
- `(*LocalNodeStateStore).CurrentEpoch() uint64`
- `(*LocalNodeStateStore).AdvanceEpoch(nextEpoch uint64) error`
- Implements `NodeStateStore` with strict `reqEpoch == currentEpoch` fencing on all mutations, optional `*git.Ledger` registration, and optional atomic temp-file + rename JSON persistence.

## 4. `internal/checkpoint/manager.go`

- `NewManager(cfg ManagerConfig) (*Manager, error)`
- `(*Manager).SaveCheckpoint(ctx context.Context, epoch uint64, workspaceDir, artifactsDir string, writablePaths []string, diary session.DiaryInput) (CheckpointRecord, error)`
- `(*Manager).RecoverLatestAligned(ctx context.Context, newEpoch uint64, destWorkspaceDir, destArtifactsDir string) (RecoveryResult, error)`
- `FormatDiaryPromptContext(records []CheckpointRecord) string`
