# Research: Streaming `BlobStore` & Cross-Machine Immutable `Artifact` Packaging (`011`)

## 1. Existing `sofa` & `r42` Patterns

- **Inspected**:
  - `internal/spec/response.go`: already defines `Issue{ID, Code, Message, Path, RepairHint}` matching `r42/internal/spec`. `internal/artifact` can type-alias `type Issue = corespec.Issue` without introducing a duplicate struct.
  - `internal/worker/spec/artifact.go`: defines `Artifact{Name, Type, Path, Description, Required, NonEmpty}`, `Artifact.ContainerPath()`, `EffectiveImportRelPath`, and `ValidateSafeLabel`.
  - `internal/plan/archive.go` & `internal/plan/permissions_{windows,unix}.go`: provides the reference pattern for deterministic ZIP writing (`time.Unix(0, 0).UTC()`, lexicographical entry sorting), `os.OpenRoot` confinement, Zip-Slip/symlink/mode/size/entry limits (`32 MiB` / `256 MiB` / `10000`), and Windows `SetNamedSecurityInfo` (`PROTECTED_DACL_SECURITY_INFORMATION`) with explicit handle closing before `os.Rename`.
  - `d:\project\r42\internal\artifact\validator.go`: confirms the exact validation issue codes (`artifact_missing`, `artifact_path_blocked`, `artifact_type_mismatch`, `artifact_empty`) and recursive `directoryContainsRegularFile` rule for `NonEmpty` directory artifacts.

## 2. `LocalStore` Atomicity & Windows Handle Semantics

- On Windows, `os.Rename` and `os.Remove` fail if the target or temp file still has an open handle, and `SetNamedSecurityInfo` requires the file to exist on disk. Closing temp files before applying `restrictBlobFile` and performing `os.Rename` avoids sharing violations.
- In `Get`, `io.Reader.Read` may return `(n > 0, io.EOF)` on the final read or `(0, io.EOF)` on a subsequent read. The `verifyingReadCloser` wrapper must hash `p[:n]` whenever `n > 0` before checking `errors.Is(err, io.EOF)` so final-chunk digests are never skipped or miscomputed.
- Synchronizing `Put` commit renames, `Get` open, `Stat`, `Delete`, and `DeletePrefix` with a `sync.RWMutex` prevents torn reads between data files and `.meta.json` sidecars under concurrent access.

## 3. Cross-Machine `StageImports` & Cache Integrity

- Local `CacheDir` entries keyed by `<sha256>.zip` must not rely solely on filename presence: `StageImports` recomputes the cached file's SHA-256 on hit, discards any corrupted/tampered cache file, and re-fetches from `blob.Store` atomically through a temp file in `CacheDir`.
- Multi-source imports (`len(Sources) > 1`, e.g., from `dynamic_worker` / `for_each`) extract each source `i` into `<importsDir>/<RelPath>/<fmt.Sprintf("%03d_%s", i, src.Artifact.Name)>` so identically named artifacts from multiple upstream workers never overwrite each other.
