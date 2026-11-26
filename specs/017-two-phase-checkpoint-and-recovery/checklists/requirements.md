# Specification Quality Checklist: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery

- [x] `git.Adapter.HeadSHA` and `CLIAdapter.HeadSHA` boundary without `os/exec` in `internal/checkpoint` is specified
- [x] `CheckpointRecord`, `Status`, `NodeStateStore`, `ErrStaleEpoch`, and `ErrNoAlignedCheckpoint` match `docs/stage2/017` §3.1 verbatim
- [x] `LocalNodeStateStore` strict epoch fencing on every mutating method, `AdvanceEpoch`/`CurrentEpoch`, `*git.Ledger` integration, atomic JSON persistence, and descending `ListCheckpoints` are specified
- [x] `SaveCheckpoint` pre-check (`writable_paths`), idempotent epoch branch registration, Phase 1/2/3 protocol, conditional `AppendFinalSHA`, deterministic `BlobStore` staging snapshot, and three-way alignment verification are specified
- [x] `RecoverLatestAligned` newest-to-oldest scan, fault fallback (pending, unaligned Git branch, missing/mismatched `BlobStore` snapshot), `e<newEpoch>` registration, workspace/artifacts restore, and `FormatDiaryPromptContext` are specified
- [x] Tier 1 fault-injection/fencing tests and Tier 2 real Git + `BlobStore` cross-directory integration tests are specified
