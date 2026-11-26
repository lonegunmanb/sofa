# Implementation Plan: Streaming `BlobStore` & Cross-Machine Immutable `Artifact` Packaging (`011-blobstore-and-artifacts`)

**Branch**: `011-blobstore-and-artifacts` | **Date**: 2026-11-26 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `011`: build the streaming `blob.Store` abstraction and atomic local filesystem `LocalStore` under `internal/blob`, and build host artifact validation (`Validate`), deterministic ZIP publishing (`Publish`), SHA-256-cached cross-machine staging (`StageImports`), and checkpoint staging snapshots (`SnapshotStagingDir`, `RestoreStagingDir`) under `internal/artifact`.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Existing Dependencies Consumed**: `internal/spec` (`Issue`), `internal/worker/spec` (`Artifact`, `ContainerPath`, `ValidateSafeLabel`), `github.com/google/uuid`, `golang.org/x/sys/windows` (Windows DACL), Go stdlib `archive/zip`, `crypto/sha256`, `os.OpenRoot`
- **Testing**: 100% offline Tier 1 unit tests (`go test ./... -count=1`)

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts created under `specs/011-blobstore-and-artifacts/` prior to editing Go code.
- **Principle II (Strict TDD)**: Failing Red unit tests in `internal/blob/store_test.go` and `internal/artifact/artifact_test.go` are written and verified failing before implementing `store.go`, `local.go`, `validate.go`, and `service.go`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `blob.Store` defines an explicit streaming interface (`internal/blob/store.go`) injected into `artifact.Service` so Stage 3 `S3Store` drops in without changing `internal/artifact` or `internal/checkpoint`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Artifact presence, types, non-empty invariants, symlink/Zip-Slip defenses, and SHA-256 integrity are deterministically enforced in Go runtime code using `os.OpenRoot` and streaming digests.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `internal/worker/spec/artifact.go`, `internal/spec/response.go`, `internal/plan/{archive.go,permissions_windows.go,permissions_unix.go}`, `d:\project\r42\internal\artifact\validator.go`, and `docs/stage2/011-blobstore-and-artifacts.md` prior to planning. Reuses `corespec.Issue`, `workerspec.Artifact`, and the exact Windows `SetNamedSecurityInfo` protected DACL pattern from `internal/plan`.

## Design Decisions

1. **`internal/blob` (`store.go`, `local.go`, `permissions_windows.go`, `permissions_unix.go`)**:
   - Define `ObjectMeta`, `Store`, `ErrNotFound`, `ErrDigestMismatch`, `ErrInvalidKey`, and `ValidateKey` / `ValidatePrefix`.
   - `LocalStore` stores object bytes at `<rootDir>/objects/<key>` and metadata JSON at `<rootDir>/meta/<key>.json` (separate directory tree preventing `.meta.json` key collisions).
   - `Put` streams into `<rootDir>/.tmp/upload-*.tmp` with a `sha256` hash + byte counter, validates `expectedSHA256` (lowercase 64-hex), closes temp files before calling `restrictBlobFile` (`0600` on Unix, current-user protected DACL on Windows), and atomically renames data file then `.meta.json` under an internal `sync.RWMutex` write lock.
   - `Get` verifies key & optional `expectedSHA256` format, reads `.meta.json` and opens the data file under `RLock`, and returns a `verifyingReadCloser` that computes SHA-256 and byte count on `Read`, validating against both `meta.SHA256` (`meta.SizeBytes`) and `expectedSHA256` at `io.EOF` (even when `n > 0` accompanies `io.EOF`). Early `Close()` closes the file handle cleanly.
   - `DeletePrefix` validates prefix (`""` or valid segments with optional trailing `/`), removes matching object + metadata files under write lock, and prunes empty subdirectories under `objects/`.
2. **`internal/artifact/validate.go`**:
   - Alias `type Issue = corespec.Issue` so structured issues align with `internal/spec` and `r42`.
   - `Validate(artifactsDir string, decls []workerspec.Artifact)` confines traversal via `os.OpenRoot(artifactsDir)` (when `artifactsDir` exists as a directory; missing `artifactsDir` yields `artifact_missing` for required declarations and skips optional ones after verifying `artifactsDir` is not a symlink or regular file).
   - Inspects ancestor segments for symlinks (`fs.ModeSymlink` -> error) and non-directory blockers (`artifact_path_blocked` when `Required == true`).
   - Checks existence (`artifact_missing` when `Required`, skip when optional), type match (`artifact_type_mismatch`), recursively walks `directory` artifacts to reject any embedded symlinks or non-regular files, and checks `NonEmpty` (`Size() > 0` for `file`; at least one regular file recursively for `directory`, returning `artifact_empty`).
3. **`internal/artifact/service.go`**:
   - `Service` holds `blob.Store` and `ServiceOptions` (`CacheDir`, `MaxSingleFileBytes`, `MaxTotalBytes`, `MaxEntries` with `32 MiB`, `256 MiB`, `10000` defaults).
   - `Publish`: packs `"payload"` (`file`) or lexicographically sorted `"tree/<rel>"` (`directory`) with `Modified = time.Unix(0, 0).UTC()` into a temp deterministic ZIP while computing `SHA256`, then streams to `runs/<runID>/artifacts/<art_uuid>.zip` via `store.Put`.
   - `StageImports`: resolves each source ZIP via `CacheDir/<sha256>.zip` (always verifying actual SHA-256 on hit and evicting/re-fetching if tampered) or `store.Get`, validates entries against Zip-Slip/symlink/type/size/count limits, and extracts via `os.OpenRoot(importsDir)` (`len(Sources) == 1`: single `file` -> `<importsDir>/<RelPath>`, single `directory` -> `<importsDir>/<RelPath>/**`; `len(Sources) > 1`: `<importsDir>/<RelPath>/<000_name>`).
   - `SnapshotStagingDir` / `RestoreStagingDir`: snapshots non-empty `artifactsDir` trees into deterministic ZIPs (returning `(nil, nil)` when empty/missing) and restores them safely via `os.OpenRoot`.

## Files

```text
internal/blob/{store.go,local.go,permissions_windows.go,permissions_unix.go,store_test.go}
internal/artifact/{validate.go,service.go,artifact_test.go}
go.mod
specs/011-blobstore-and-artifacts/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
