# Feature Specification: S3 / MinIO Compatible Streaming `BlobStore` (`021-s3-blobstore`)

**Feature Branch**: `021-s3-blobstore`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `021` blueprint (`docs/stage3/021-s3-blobstore.md`), `docs/stage3/README.md`, `internal/blob/store.go`, `internal/blob/local.go`, and `internal/blob/store_test.go`.

## User Scenarios & Testing

### User Story 1 - Streaming S3 Upload with End-to-End SHA-256 Enforcement (`Put`) (Priority: P1)

Distributed `sofa server` and `sofa worker` nodes need an S3/MinIO/OSS-compatible `blob.Store` (`internal/blob/s3`) that streams large artifacts without buffering whole payloads in RAM and guarantees end-to-end SHA-256 integrity. When `expectedSHA256` is provided, `Put` streams directly via `transfermanager.Client.UploadObject` (multipart above `PartSizeBytes`) while computing actual SHA-256 and byte count, attaching `"sofa-sha256": expectedSHA256` in S3 object user metadata; if the stream's actual digest at EOF differs from `expectedSHA256`, `Put` immediately rolls back the dirty object (`DeleteObject` using `context.WithoutCancel(ctx)`) and returns `blob.ErrDigestMismatch`. When `expectedSHA256` is empty, `Put` streams to a restricted local temp file (`0600` / Windows current-user protected DACL) to compute actual SHA-256 and size, rewinds, uploads with `"sofa-sha256": actualSHA256`, and deletes the temp file in `defer`.

**Independent Test**: Unit-test `Put` with valid/invalid keys (`ErrInvalidKey`), malformed `expectedSHA256` rejected before S3 calls (`ErrDigestMismatch`), `expectedSHA256` mismatch triggering rollback `DeleteObject` and returning `ErrDigestMismatch`, unknown-digest streaming `Put` persisting `sofa-sha256` metadata without leaving temp files, and multipart upload triggered above `PartSizeBytes` (`internal/blob/s3/store_test.go`).

### User Story 2 - Streaming Integrity-Verified Download, `Stat`, and Idempotent `Delete` (Priority: P1)

Callers retrieving artifacts or checking existence need `Get`, `Stat`, and `Delete` with 100% behavioral parity with `blob.LocalStore`. S3 `NoSuchKey`, `NotFound`, and HTTP `404` responses map to `blob.ErrNotFound`. `Get` validates key and optional `expectedSHA256`, rejects mismatches between stored `"sofa-sha256"` metadata and `expectedSHA256` immediately (closing the response body), and wraps `GetObjectOutput.Body` in a verifying `io.ReadCloser` that checks actual SHA-256 (and `ContentLength` when present) against stored metadata and optional `expectedSHA256` at `io.EOF`. Early `Close()` before `EOF` releases the HTTP stream cleanly without `ErrDigestMismatch`. `Delete` removes the object idempotently (`404`/`NoSuchKey` returns `nil`).

**Independent Test**: Unit-test `Get` valid reads, `404`/`NoSuchKey` -> `ErrNotFound`, upfront metadata mismatch -> `ErrDigestMismatch` with body closed, mid-stream byte corruption caught at `io.EOF` (`ErrDigestMismatch`) both with and without `expectedSHA256`, early `Close()`, `Stat` metadata mapping, and idempotent `Delete` (`internal/blob/s3/store_test.go`).

### User Story 3 - `KeyPrefix` Isolation & Paginated Boundary-Safe `DeletePrefix` (Priority: P1)

Multi-cluster deployments sharing a single S3 bucket require optional `KeyPrefix` isolation and safe run-level cleanup (`DeletePrefix`). All operations (`Put`, `Get`, `Stat`, `Delete`, `DeletePrefix`) operate exclusively inside `KeyPrefix/` when configured while returning logical keys in `ObjectMeta.Key`. `DeletePrefix` validates `blob.ValidatePrefix(prefix)`, normalizes non-empty prefixes with a trailing `'/'` so deleting `"runs/run-1"` never touches `"runs/run-10/..."`, paginates via `ListObjectsV2Paginator`, deletes batches of at most 1000 keys per `DeleteObjects` call (`Quiet: true`), and reports partial `DeleteObjectsOutput.Errors`.

**Independent Test**: Unit-test `KeyPrefix` scoping on all 5 `blob.Store` methods, `DeletePrefix("runs/run-1")` deleting >1000 keys across multiple pages while preserving `"runs/run-10/..."` and objects outside `KeyPrefix/`, `DeletePrefix("")` clearing only `KeyPrefix/` (or bucket root when `KeyPrefix` is empty), and partial `DeleteObjectsOutput.Errors` propagation (`internal/blob/s3/store_test.go`), plus Tier 2 integration verification against a real Podman `ministack` S3 container (`internal/blob/s3/store_integration_test.go`).

### Edge Cases

- `NewStore` MUST reject empty `Bucket` or negative `PartSizeBytes` or invalid `KeyPrefix`, and default `Region` to `"us-east-1"` and `PartSizeBytes` to `8 MiB` (`8 * 1024 * 1024`).
- `Put`, `Get`, `Stat`, and `Delete` MUST reject invalid keys using `blob.ValidateKey(key)` (`blob.ErrInvalidKey`) before issuing S3 requests.
- `Put` and `Get` MUST validate non-empty `expectedSHA256` with `blob.IsValidSHA256Hex`, returning `blob.ErrDigestMismatch` before issuing S3 requests.
- `Put` with `expectedSHA256 != ""` when `ctx` is canceled right as digest mismatch is detected MUST still execute rollback `DeleteObject` with `context.WithoutCancel(ctx)`.
- `Get` reading a stream whose `io.Read` returns `(n > 0, io.EOF)` MUST hash those `n` bytes before checking SHA-256 and return `0, ErrDigestMismatch` on mismatch or `n, io.EOF` on match.
- `DeletePrefix` on an empty prefix matches zero objects idempotently (`nil`) and MUST NEVER delete sibling keys outside the slash boundary (`"runs/r1"` -> `"runs/r1/"`).

## Requirements

### Functional Requirements

- **FR-001**: `internal/blob/s3` MUST define `Config` (`Bucket`, `Region`, `Endpoint`, `AccessKeyID`, `SecretAccessKey`, `SessionToken`, `UsePathStyle`, `KeyPrefix`, `PartSizeBytes`) and `NewStore(ctx context.Context, cfg Config) (*Store, error)` implementing `blob.Store`.
- **FR-002**: `Store` MUST depend on a narrow internal `s3API` interface (embedding `transfermanager.S3APIClient` plus `DeleteObject` and `DeleteObjects`) so Tier 1 unit tests run deterministically offline without network/containers.
- **FR-003**: `Put` MUST stream uploads via `aws-sdk-go-v2/feature/s3/transfermanager` (`PartSizeBytes` and `MultipartUploadThreshold` aligned to configured `PartSizeBytes`), persist `"sofa-sha256"` user metadata, roll back dirty objects on `expectedSHA256` mismatch, and use a restricted (`0600` / Windows protected DACL) temp file removed in `defer` when `expectedSHA256 == ""`.
- **FR-004**: `Get`, `Stat`, and `Delete` MUST map S3 `NoSuchKey`, `NotFound`, and HTTP `404` to `blob.ErrNotFound` (or `nil` for idempotent `Delete`), and `Get` MUST verify full-stream SHA-256 at `io.EOF` against both stored `"sofa-sha256"` metadata and optional `expectedSHA256`.
- **FR-005**: `DeletePrefix` MUST enforce `blob.ValidatePrefix`, slash-boundary normalization (`"runs/r1"` -> `"runs/r1/"`), `KeyPrefix` scoping, `ListObjectsV2Paginator` pagination, `<= 1000`-key `DeleteObjects` (`Quiet: true`) batching, and `DeleteObjectsOutput.Errors` error reporting.

## Success Criteria

### Measurable Outcomes

- **SC-001**: All Tier 1 unit tests in `internal/blob/s3/store_test.go` pass offline with `go test ./internal/blob/... -count=1`.
- **SC-002**: Tier 2 integration tests in `internal/blob/s3/store_integration_test.go` (`//go:build integration`) pass against Podman `docker.io/ministackorg/ministack:latest` covering >12 MiB multipart streaming `Put` (with and without `expectedSHA256`), `Stat`, `Get` integrity verification, `ErrDigestMismatch`, `KeyPrefix` isolation, and `DeletePrefix`, leaving zero Podman containers behind.
- **SC-003**: `gofmt -l .` produces empty output and `go vet ./...` and `go test ./... -count=1` pass cleanly.
