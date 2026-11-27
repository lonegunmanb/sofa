# Contract: `internal/blob/s3`

## 1. Public API

```go
package s3

type Config struct {
	Bucket          string
	Region          string // defaults to "us-east-1"
	Endpoint        string // optional custom S3 endpoint
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UsePathStyle    bool
	KeyPrefix       string // optional bucket root prefix
	PartSizeBytes   int64  // defaults to 8 MiB
}

func NewStore(ctx context.Context, cfg Config) (*Store, error)
```

`*Store` implements `blob.Store`:
- `Put(ctx context.Context, key string, r io.Reader, expectedSHA256 string) (blob.ObjectMeta, error)`
- `Get(ctx context.Context, key string, expectedSHA256 string) (io.ReadCloser, blob.ObjectMeta, error)`
- `Stat(ctx context.Context, key string) (blob.ObjectMeta, error)`
- `Delete(ctx context.Context, key string) error`
- `DeletePrefix(ctx context.Context, prefix string) error`

## 2. Behavioral Invariants

1. **Key & Digest Validation**:
   - `Put`, `Get`, `Stat`, `Delete` validate `key` via `blob.ValidateKey(key)` -> `blob.ErrInvalidKey`.
   - `DeletePrefix` validates `prefix` via `blob.ValidatePrefix(prefix)` -> `blob.ErrInvalidKey`.
   - Non-empty `expectedSHA256` on `Put` and `Get` MUST pass `blob.IsValidSHA256Hex(expectedSHA256)` -> `blob.ErrDigestMismatch` before any S3 call.
2. **Streaming `Put` & Rollback**:
   - `expectedSHA256 != ""`: stream upload via `transfermanager.UploadObject` with `Metadata: {"sofa-sha256": expectedSHA256}` while hashing and counting bytes; on actual SHA-256 mismatch at EOF, immediately call `DeleteObject` with `context.WithoutCancel(ctx)` and return `blob.ErrDigestMismatch`.
   - `expectedSHA256 == ""`: stream `r` to a restricted local temp file (`0600` / Windows protected DACL) computing actual SHA-256 and byte length, rewind (`Seek(0, io.SeekStart)`), stream-upload with `Metadata: {"sofa-sha256": actualSHA256}`, and close + remove the temp file in `defer`.
3. **`Get`, `Stat`, `Delete` & Error Mapping**:
   - `NoSuchKey`, `NotFound`, and HTTP `404` map to `blob.ErrNotFound` on `Get`/`Stat`, and `nil` on `Delete`.
   - `Get` and `Stat` validate that stored `Metadata["sofa-sha256"]` (case-insensitive key match) is a valid 64-character lowercase hex digest (`blob.IsValidSHA256Hex`), returning `blob.ErrDigestMismatch` (and closing `Body` on `Get`) if missing or malformed. `Get` also checks stored digest upfront against non-empty `expectedSHA256`, and wraps `Body` so reading to `io.EOF` verifies actual SHA-256 (and `ContentLength` when provided) against stored metadata and `expectedSHA256`. Early `Close()` does not fail with `ErrDigestMismatch`.
4. **Boundary-Safe Paginated `DeletePrefix`**:
   - Non-empty `prefix` is normalized with trailing `'/'` (`"runs/r1"` -> `"runs/r1/"`), scoped under `KeyPrefix/` if set.
   - Empty `prefix == ""` lists `KeyPrefix + "/"` when `KeyPrefix != ""`, or `""` when `KeyPrefix == ""`.
   - Uses `s3.NewListObjectsV2Paginator` and calls `DeleteObjects` (`Quiet: true`) in chunks of up to 1000 keys, failing if `DeleteObjectsOutput.Errors` is non-empty.
