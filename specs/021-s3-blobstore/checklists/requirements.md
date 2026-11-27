# Specification Quality Checklist: S3 / MinIO Compatible Streaming `BlobStore`

- [x] User stories and acceptance scenarios cover `Config`, `NewStore`, `Put` (known and unknown `expectedSHA256`), `Get`, `Stat`, `Delete`, and `DeletePrefix`
- [x] `LocalStore` parity invariants (`ValidateKey`, `ValidatePrefix`, `IsValidSHA256Hex`, `ErrNotFound`, `ErrDigestMismatch`, `ErrInvalidKey`, slash-boundary prefix safety) are specified
- [x] Streaming multipart upload via non-deprecated `feature/s3/transfermanager` (`sofa-sha256` metadata, rollback deletion via `context.WithoutCancel`, restricted temp file cleanup) is documented
- [x] Narrow `s3API` boundary for deterministic Tier 1 unit tests and Podman `ministack` Tier 2 integration tests are defined
- [x] Out-of-scope Stage 3 slices (`022`–`032`) remain excluded
