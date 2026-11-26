# Implementation Plan: Epoch-Scoped Two-Phase Checkpoint & Cross-Machine Aligned Recovery (`017-two-phase-checkpoint-and-recovery`)

**Branch**: `017-two-phase-checkpoint-and-recovery` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `017`:
1. Extend `internal/git/adapter.go` and `internal/git/cli.go` with `HeadSHA(ctx context.Context, workspaceDir string) (string, error)` so `internal/checkpoint` stays 100% free of `os/exec`.
2. Implement `internal/checkpoint/model.go` (`Status`, `CheckpointRecord`, `NodeStateStore`, `ErrStaleEpoch`, `ErrNoAlignedCheckpoint`).
3. Implement `internal/checkpoint/local_store.go` (`LocalNodeStateStore` with epoch fencing, `AdvanceEpoch`/`CurrentEpoch`, optional `*git.Ledger` registration, and optional atomic JSON persistence).
4. Implement `internal/checkpoint/manager.go` (`Manager`, `SaveCheckpoint`, `RecoverLatestAligned`, `FormatDiaryPromptContext`).
5. Deliver Tier 1 fault-injection/fencing tests in `internal/checkpoint/checkpoint_test.go` and Tier 2 cross-directory real Git + `blob.LocalStore` integration tests in `internal/checkpoint/checkpoint_integration_test.go`.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **New Direct Dependencies**: None (uses Go stdlib + `internal/blob`, `internal/artifact`, `internal/git`, `internal/session`, `github.com/stretchr/testify/require`).
- **Existing Packages Reused**:
  - `internal/blob` (`Store`, `LocalStore`, `ObjectMeta`, `ValidateKey`, `ErrNotFound`, `ErrDigestMismatch`)
  - `internal/artifact` (`Service.SnapshotStagingDir`, `Service.RestoreStagingDir`)
  - `internal/git` (`Adapter`, `CLIAdapter`, `Ledger`, `CheckpointBranchName`, `KindCheckpointBranch`, `FindUnauthorizedPaths`, `IsValidCommitSHA`, `NormalizeBranchName`, `NormalizeRepoURL`)
  - `internal/session` (`DiaryInput`)
- **Testing**:
  - Tier 1 offline unit tests: `go test ./internal/checkpoint/... -count=1` and `go test ./... -count=1`
  - Tier 2 real-dependency integration tests: `go test -tags=integration ./internal/git/... ./internal/checkpoint/... -count=1`

## Constitution Check

- **Principle I (Spec-First)**: Spec Kit artifacts authored in `specs/017-two-phase-checkpoint-and-recovery/` before production Go code.
- **Principle II (Strict TDD)**: Failing Tier 1 (`checkpoint_test.go`) and Tier 2 (`checkpoint_integration_test.go`) tests authored and verified Red prior to implementing `internal/checkpoint/`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `internal/checkpoint` contains zero `os/exec`, container CLI, or Dapr transport imports; interacts exclusively via `git.Adapter`, `NodeStateStore`, `blob.Store`, and `*artifact.Service`, with real-dependency verification against `git init --bare` and `blob.LocalStore`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Unauthorized `writable_paths` files are rejected via `git.Adapter.StatusPaths` + `git.FindUnauthorizedPaths` before any checkpoint commit, and zombie writes are fenced by monotonic `epoch` checks before every state mutation.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `internal/blob`, `internal/artifact`, `internal/git`, `internal/session`, `docs/stage2/017`, and `docs/stage2/018` first; scoped strictly to Slice `017`.

## Files

```text
internal/git/adapter.go
internal/git/cli.go
internal/git/cli_integration_test.go
internal/checkpoint/model.go
internal/checkpoint/local_store.go
internal/checkpoint/manager.go
internal/checkpoint/checkpoint_test.go
internal/checkpoint/checkpoint_integration_test.go
specs/017-two-phase-checkpoint-and-recovery/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
