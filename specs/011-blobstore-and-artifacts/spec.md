# Feature Specification: Streaming `BlobStore` & Cross-Machine Immutable `Artifact` Packaging (`011-blobstore-and-artifacts`)

**Feature Branch**: `011-blobstore-and-artifacts`

**Created**: 2026-11-26

**Status**: Approved

**Input**: Stage 2 Slice `011` blueprint (`docs/stage2/011-blobstore-and-artifacts.md`), `docs/stage2/README.md`, `internal/worker/spec/artifact.go`, `internal/plan/archive.go`, and `r42/internal/artifact/validator.go`.

## User Scenarios & Testing

### User Story 1 - Streaming `BlobStore` Interface & Atomic `LocalStore` (`internal/blob`) (Priority: P1)

Host workers and future distributed controllers need a streaming `blob.Store` abstraction (`Put`, `Get`, `Stat`, `Delete`, `DeletePrefix`) backed in Stage 2 by an atomic local filesystem `LocalStore` under `<state_dir>/blobs/`. Every key is strictly validated (`ValidateKey`), writes stream into `.tmp/upload-*.tmp` while hashing SHA-256 and counting bytes, reject mismatched `expectedSHA256` without leaving temp files, close all Windows handles before restricting permissions (`0600` / current-user protected DACL) and atomically renaming `.meta.json` + data files, and verify SHA-256 on `Get` reads up to `io.EOF` (returning `ErrDigestMismatch` if underlying content was tampered with).

**Independent Test**: Unit-test `ValidateKey`, `Put` with valid and mismatched `expectedSHA256` (confirming no leftover `.tmp` files), `Get` with tampered stored data (confirming `ErrDigestMismatch` on reading to `EOF`), early `Close()` before `EOF` without handle leaks, `Stat`, idempotent `Delete`, `DeletePrefix` isolating `"runs/run-1/"` from `"runs/run-2/"`, and concurrent `Put`/`Get`/`DeletePrefix` (`internal/blob/store_test.go`).

### User Story 2 - Host `artifactsDir` Validation (`internal/artifact`) (Priority: P1)

Before publishing worker outputs or gating work completion, the host validates declared `workerspec.Artifact` entries against the host `artifactsDir` (mounted as `/.sofa/artifacts/`). `Validate(artifactsDir, decls)` confines traversal with `os.OpenRoot`, rejects any symlink (`fs.ModeSymlink`) with an error, and produces structured `Issue` entries (`artifact_missing`, `artifact_path_blocked`, `artifact_type_mismatch`, `artifact_empty`) while skipping missing optional (`Required == false`) artifacts from `valid`.

**Independent Test**: Unit-test `Validate` matrix covering: valid file & directory declarations, missing required artifact (`artifact_missing`), missing optional artifact (no issue, omitted from `valid`), non-directory ancestor blocking required path (`artifact_path_blocked`), file-vs-directory type mismatch (`artifact_type_mismatch`, even when `Required == false`), `NonEmpty == true` on 0-byte file or directory without regular files (`artifact_empty`), and symlink inside declared path/directory returning an error (`internal/artifact/artifact_test.go`).

### User Story 3 - Deterministic Artifact Publishing, Cross-Machine `StageImports` Caching & Staging Snapshots (`internal/artifact`) (Priority: P1)

After validation, `Service.Publish` packs each valid artifact into a deterministic ZIP (`"payload"` for `file`, sorted `"tree/<rel>"` with `time.Unix(0, 0).UTC()` for `directory`), enforces symlink/regular-file and size/entry limits, and uploads to `runs/<runID>/artifacts/<art_uuid>.zip` via `blob.Store`. On any downstream machine, `Service.StageImports` resolves each source via optional local `CacheDir` (`<cacheDir>/<sha256>.zip`, re-verifying actual SHA-256 so tampered cache files are evicted and re-fetched) or `store.Get`, enforces Zip-Slip/symlink/zip-bomb checks via `os.OpenRoot(importsDir)`, and unpacks single-source (`file` -> `<importsDir>/<RelPath>`, `directory` -> `<importsDir>/<RelPath>/**`) and multi-source (`<importsDir>/<RelPath>/<000_name>`) layouts. `SnapshotStagingDir` and `RestoreStagingDir` preserve and restore in-progress `artifactsDir` trees across machines for checkpoints.

**Independent Test**: Unit-test simulated cross-machine round-trips (delete `machineA` after `Publish`, unpack in `machineB` for single file, single directory, and `len(Sources) == 2` multi-source `for_each`, verify second `StageImports` hits `CacheDir` even with store object deleted, verify corrupted `CacheDir` entry is detected and re-fetched, verify Zip-Slip / symlink / limit rejections, and verify `SnapshotStagingDir` -> delete `machineA` -> `RestoreStagingDir` in `machineC`, including empty dir returning `(nil, nil)`) (`internal/artifact/artifact_test.go`).

### Edge Cases

- `ValidateKey` MUST reject empty keys, leading/trailing `/`, empty segments (`//`), `\`, `.`, `..`, spaces, and control characters, allowing only `[A-Za-z0-9._-]` per slash-separated segment. `DeletePrefix` MUST allow `""` or slash-separated valid segments with an optional trailing `/`.
- `Put` and `Get` MUST validate non-empty `expectedSHA256` as lowercase 64-char hex, returning `ErrDigestMismatch` (and leaving zero `.tmp` artifacts) on invalid format or digest mismatch.
- `Get` early `Close()` before reaching `io.EOF` MUST close the underlying OS file handle cleanly without returning `ErrDigestMismatch` or leaking handles.
- On Windows, all temp and target file handles MUST be closed before `os.Rename`, `os.Remove`, or protected DACL application.
- `Validate` MUST check for symlinks inside declared `directory` artifacts even when `NonEmpty == false`.
- `StageImports` with tampered `CacheDir/<sha256>.zip` MUST NOT trust the filename; it MUST verify actual file SHA-256, discard the corrupted cache file, and re-fetch from `blob.Store`.
- `SnapshotStagingDir` on a missing or completely empty `artifactsDir` MUST return `(nil, nil)` without writing a blob object.

## Requirements

- **FR-001**: `internal/blob` MUST define `ObjectMeta`, `Store`, sentinel errors (`ErrNotFound`, `ErrDigestMismatch`, `ErrInvalidKey`), and `ValidateKey` / prefix validation.
- **FR-002**: `internal/blob` `LocalStore` MUST store data at `<rootDir>/objects/<key>` and metadata at `<rootDir>/meta/<key>.json` (avoiding sidecar collisions with keys ending in `.meta.json`), use `<rootDir>/.tmp/upload-*.tmp` streaming writes with SHA-256 verification, restrict file permissions (`0600` / Windows current-user protected DACL), atomically replace targets, verify full-stream SHA-256 on `Get` up to `io.EOF`, and support `Stat`, idempotent `Delete`, and `DeletePrefix`.
- **FR-003**: `internal/artifact` MUST define/alias `Issue = spec.Issue` and implement `Validate(artifactsDir string, decls []workerspec.Artifact)` using `os.OpenRoot`, symlink rejection, and `r42`-aligned issue codes (`artifact_missing`, `artifact_path_blocked`, `artifact_type_mismatch`, `artifact_empty`).
- **FR-004**: `internal/artifact` MUST define `PublishedArtifact`, `ImportSource`, `ImportRequest`, `ServiceOptions`, and `NewService(store, opts)` with deterministic ZIP packing (`Publish`), local SHA-256-verified caching and safe single/multi-source unpacking (`StageImports`), and checkpoint staging snapshot helpers (`SnapshotStagingDir`, `RestoreStagingDir`).
- **FR-005**: Archive packing and unpacking in `internal/artifact` MUST enforce `os.OpenRoot` confinement, symlink/non-regular file rejection, Zip-Slip path checks, and configurable decompression/entry limits (`DefaultMaxSingleFileBytes`, `DefaultMaxTotalBytes`, `DefaultMaxEntries`).

## Success Criteria

- **SC-001**: All Tier 1 unit tests in `internal/blob` and `internal/artifact` pass deterministically offline on Windows/Unix.
- **SC-002**: `gofmt -l .` produces empty output, and `go vet ./...` and `go test ./... -count=1` pass with zero errors.
