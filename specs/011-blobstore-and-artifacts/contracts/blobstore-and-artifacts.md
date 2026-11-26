# Contract: `internal/blob` & `internal/artifact`

## 1. `internal/blob` Contract

- **Key Rules (`ValidateKey`)**:
  - Slash-separated relative path segments matching `[A-Za-z0-9._-]+`.
  - Rejects `""`, leading/trailing `/`, `//`, `\`, `.`, `..`, spaces, and control characters with `ErrInvalidKey`.
  - `DeletePrefix` permits `""` or valid slash-separated segments with an optional trailing `/`.
- **On-Disk Layout (`LocalStore`)**:
  - Root directory `<rootDir>` (`0700`), temporary staging `<rootDir>/.tmp/upload-*.tmp`, object payload `<rootDir>/objects/<key>`, and sidecar metadata `<rootDir>/meta/<key>.json`.
  - `Put` streams `r` into `.tmp`, computes SHA-256 and byte count, validates optional lowercase 64-hex `expectedSHA256` (removing `.tmp` and returning `ErrDigestMismatch` on mismatch), closes file handles, restricts permissions (`0600` / Windows current-user protected DACL), and atomically renames data + `.meta.json`.
  - `Get` returns `ErrNotFound` when absent and wraps the returned `io.ReadCloser` so reading to `io.EOF` validates actual SHA-256 and size against stored `.meta.json` and optional `expectedSHA256`, returning `ErrDigestMismatch` on mismatch. Early `Close()` releases the handle cleanly.

## 2. `internal/artifact` Contract

- **`Validate(artifactsDir, decls)`**:
  - Confines access via `os.OpenRoot(artifactsDir)` and rejects any symlink (`fs.ModeSymlink`) with an error.
  - Missing required -> `artifact_missing`; missing optional -> omitted from `valid` with no issue; non-directory ancestor blocking required path -> `artifact_path_blocked`; type mismatch -> `artifact_type_mismatch`; `NonEmpty == true` with 0-byte file or directory lacking any regular file -> `artifact_empty`.
- **`Publish(ctx, runID, artifactsDir, validDecls)`**:
  - Assigns `id = "art_" + uuid4`, builds a deterministic ZIP (`Modified = time.Unix(0, 0).UTC()`; `"payload"` for `file`, sorted `"tree/<rel>"` for `directory`), and uploads via `store.Put(ctx, "runs/"+runID+"/artifacts/"+id+".zip", r, zipSHA256)`.
- **`StageImports(ctx, importsDir, requests)`**:
  - Rejects duplicate or ancestor/descendant overlapping `RelPath` values across `requests` upfront before modifying `importsDir`.
  - Uses local `<CacheDir>/<sha256>.zip` (guarded by per-digest synchronization and `0600`/Windows protected DACL permissions) if configured and actual SHA-256 matches, otherwise fetches via `store.Get(ctx, blobKey, sha256)` and atomically populates cache.
  - Enforces `os.OpenRoot(importsDir)`, Zip-Slip/symlink/non-regular file rejection, and `MaxSingleFileBytes` / `MaxTotalBytes` / `MaxEntries` limits.
  - `len(Sources) == 1`: single `file` -> `<importsDir>/<RelPath>`; single `directory` -> `<importsDir>/<RelPath>/**`.
  - `len(Sources) > 1`: source `i` -> `<importsDir>/<RelPath>/<000_name>`.
- **`SnapshotStagingDir` / `RestoreStagingDir`**:
  - Empty/missing `artifactsDir` returns `(nil, nil)`; non-empty `artifactsDir` packs deterministically and restores safely via `os.OpenRoot`.
