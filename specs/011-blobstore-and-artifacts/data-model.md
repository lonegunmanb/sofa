# Data Model: `internal/blob` & `internal/artifact`

## `internal/blob` (`store.go`, `local.go`)

### `ObjectMeta`
| Field | Type | JSON Tag | Notes |
|---|---|---|---|
| `Key` | `string` | `json:"key"` | Validated relative slash-separated object key |
| `SHA256` | `string` | `json:"sha256"` | Lowercase 64-char hex digest of stored bytes |
| `SizeBytes` | `int64` | `json:"size_bytes"` | Exact byte length of stored object |

### `Store` Interface
- `Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (ObjectMeta, error)`
- `Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, ObjectMeta, error)`
- `Stat(ctx context.Context, key string) (ObjectMeta, error)`
- `Delete(ctx context.Context, key string) error`
- `DeletePrefix(ctx context.Context, prefix string) error`

### Sentinel Errors
- `ErrNotFound`
- `ErrDigestMismatch`
- `ErrInvalidKey`

## `internal/artifact` (`validate.go`, `service.go`)

### `Issue`
- Type alias `type Issue = corespec.Issue` (`ID`, `Code`, `Message`, `Path`, `RepairHint`).

### `PublishedArtifact`
| Field | Type | JSON Tag | Notes |
|---|---|---|---|
| `ID` | `string` | `json:"id"` | `"art_<uuid>"` |
| `Name` | `string` | `json:"name"` | HCL `artifact` block label |
| `Type` | `string` | `json:"type"` | `"file"` or `"directory"` |
| `RelPath` | `string` | `json:"rel_path"` | Clean relative path under `artifactsDir` |
| `ContainerPath` | `string` | `json:"container_path"` | `"/.sofa/artifacts/<rel_path>"` |
| `BlobKey` | `string` | `json:"blob_key"` | `"runs/<run_id>/artifacts/art_<uuid>.zip"` |
| `SHA256` | `string` | `json:"sha256"` | Lowercase 64-char hex digest of ZIP archive |
| `SizeBytes` | `int64` | `json:"size_bytes"` | Byte size of ZIP archive |

### `ImportSource` & `ImportRequest`
- `ImportSource`: `Artifact PublishedArtifact`
- `ImportRequest`: `Name string`, `RelPath string`, `Sources []ImportSource`

### `ServiceOptions`
- `CacheDir string`
- `MaxSingleFileBytes int64` (default `32 << 20`)
- `MaxTotalBytes int64` (default `256 << 20`)
- `MaxEntries int` (default `10000`)
