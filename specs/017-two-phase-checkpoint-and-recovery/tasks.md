---
description: "Stage 2 Slice 017 TDD tasks: git.Adapter.HeadSHA, NodeStateStore epoch fencing, SaveCheckpoint, RecoverLatestAligned, and Tier 1/Tier 2 verification"
---

# Tasks

- [X] T001 [Git Boundary] Add `HeadSHA(ctx context.Context, workspaceDir string) (string, error)` to `internal/git/adapter.go`, implement it on `*CLIAdapter` in `internal/git/cli.go`, and add an assertion in `internal/git/cli_integration_test.go`.
- [X] T002 [Red] Write failing Tier 1 tests in `internal/checkpoint/checkpoint_test.go` covering:
  - Clean workspace (`committed == false`, `finalSHA == initialSHA`, `AppendFinalSHA` skipped) and dirty workspace (`committed == true`, `finalSHA != initialSHA`, `AppendFinalSHA` called and persisted);
  - Pre-check blocking unauthorized `writable_paths` modifications before `BeginCheckpoint` or `CommitAllAndPush`;
  - Fault injection Case A (`Seq=1` `COMPLETE`, `Seq=2` `PENDING` after `CommitAllAndPush` crash) -> `RecoverLatestAligned` falls back to `Seq=1` code, artifacts, and diary only (excluding `Seq=2` diary);
  - Fault injection Case B (`Seq=2` `COMPLETE` in store, remote branch missing `Seq=2.FinalSHA`) -> falls back to `Seq=1`;
  - Fault injection Case C (`Seq=2` `COMPLETE` in store, `ArtifactsSnapshot` missing or digest-corrupted in `BlobStore`) -> falls back to `Seq=1`;
  - Zombie process epoch fencing: advancing authoritative epoch from `1` to `2` mid-`CommitAllAndPush` rejects old epoch `1` `AppendFinalSHA`/`CompleteCheckpoint` with `ErrStaleEpoch` and leaves store unpolluted;
  - Multi-checkpoint same-epoch idempotent branch registration, `ErrNoAlignedCheckpoint` on empty/unaligned stores, and `LocalNodeStateStore` JSON file persistence.
- [X] T003 [Red] Write failing Tier 2 integration test in `internal/checkpoint/checkpoint_integration_test.go` (`//go:build integration`) against real `git init --bare`, `git.CLIAdapter`, `blob.LocalStore`, `artifact.Service`, and file-backed `LocalNodeStateStore`: Machine A completes Checkpoint 1 (code + `/.sofa/artifacts/draft.md`), begins Checkpoint 2 edits + partial `StatusPending` record, `os.RemoveAll(machineADir)`, then Machine B advances to `epoch = 2`, runs `RecoverLatestAligned`, verifies 100% restored workspace/artifacts/diary, and saves a new Checkpoint on `e2`.
- [X] T004 [Green] Implement `internal/checkpoint/model.go`, `internal/checkpoint/local_store.go`, and `internal/checkpoint/manager.go` so Tier 1 and Tier 2 tests pass.
- [X] T005 [Verify] Run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/git/... ./internal/checkpoint/... -count=1`, resolve any issues, and mark all tasks `[X]`.
