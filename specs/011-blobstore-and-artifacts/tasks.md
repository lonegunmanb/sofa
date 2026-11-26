---
description: "Stage 2 Slice 011 TDD tasks: streaming BlobStore + LocalStore and cross-machine Artifact Validate/Publish/StageImports/Snapshot"
---

# Tasks

- [X] T001 [Red] Write failing unit tests for `internal/blob` in `internal/blob/store_test.go`:
  - `ValidateKey` accepts valid relative slash-separated keys and rejects empty, leading/trailing `/`, `//`, `\`, `.`, `..`, spaces, and control characters with `ErrInvalidKey`;
  - `LocalStore.Put` rejects invalid or mismatched `expectedSHA256` (`ErrDigestMismatch`) leaving zero `.tmp` files;
  - `LocalStore.Get` reads valid objects cleanly, returns `ErrNotFound` when absent, returns `ErrDigestMismatch` at `io.EOF` when stored data is tampered with, and supports early `Close()` before `EOF` without error or handle leak;
  - `LocalStore.Stat`, idempotent `Delete`, `DeletePrefix("runs/run-1/")` isolating `"runs/run-2/"`, file permissions (`0600` / Windows protected DACL), and concurrent `Put`/`Get`/`DeletePrefix`.
- [X] T002 [Green] Implement `internal/blob/store.go`, `internal/blob/local.go`, `internal/blob/permissions_windows.go`, and `internal/blob/permissions_unix.go` and verify `go test ./internal/blob/... -count=1` passes.
- [X] T003 [Red] Write failing unit tests for `internal/artifact` in `internal/artifact/artifact_test.go`:
  - `Validate` full matrix: valid file/directory, missing required (`artifact_missing`), missing optional (no issue, omitted from `valid`), non-directory parent blocking required path (`artifact_path_blocked`), file/directory type mismatch (`artifact_type_mismatch`), `NonEmpty` on 0-byte file and empty/subdir-only directory (`artifact_empty`), and symlink inside path or directory returning an error;
  - Simulated cross-machine `Publish` -> delete `machineA` -> `StageImports` in `machineB` covering single `file`, single `directory`, and multi-source `len(Sources) == 2` (`000_<name>`, `001_<name>`), deterministic ZIP digest, `CacheDir` hit when store object is deleted, and `CacheDir` tamper detection forcing re-fetch from `blob.Store`;
  - Security limits in `StageImports`: Zip-Slip (`..`), symlink entry, entry count/single-file/total-byte limits;
  - `SnapshotStagingDir` (empty dir returns `(nil, nil)`, non-empty snapshots deterministically) -> delete `machineA` -> `RestoreStagingDir` in `machineC` restoring exact files and directory structure.
- [X] T004 [Green] Implement `internal/artifact/validate.go` and `internal/artifact/service.go` (and `go mod tidy` for `github.com/google/uuid` direct require) and verify `go test ./internal/artifact/... -count=1` passes.
- [X] T005 [Refactor & Verify] Review code for duplication/clarity, run `gofmt -l .`, `go vet ./...`, and `go test ./... -count=1`, and mark all tasks `[X]`.
