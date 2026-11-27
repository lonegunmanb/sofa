---
description: "Stage 3 Slice 021 TDD tasks: S3 / MinIO compatible streaming BlobStore (internal/blob/s3)"
---

# Tasks

- [X] T001 Add `github.com/aws/aws-sdk-go-v2`, `github.com/aws/aws-sdk-go-v2/config`, `github.com/aws/aws-sdk-go-v2/credentials`, `github.com/aws/aws-sdk-go-v2/service/s3`, `github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager`, and `github.com/aws/smithy-go` to `go.mod`.
- [X] T002 [Red] Write failing Tier 1 unit tests in `internal/blob/s3/store_test.go` covering:
  - `NewStore` config validation and defaults (`Region`, `PartSizeBytes`, `KeyPrefix`);
  - `ValidateKey` / `ValidatePrefix` (`blob.ErrInvalidKey`), malformed `expectedSHA256` (`blob.ErrDigestMismatch` before S3 calls), and `404` / `NoSuchKey` / `NotFound` -> `blob.ErrNotFound`;
  - `Put` with `expectedSHA256` mismatch triggering rollback `DeleteObject` (even under canceled context after read) and returning `blob.ErrDigestMismatch`, plus multipart upload path above `PartSizeBytes`;
  - `Put` with empty `expectedSHA256` computing digest via temp file and storing `sofa-sha256` metadata;
  - `Get` upfront metadata mismatch, mid-stream corruption caught at `io.EOF` (`blob.ErrDigestMismatch`) both with and without `expectedSHA256`, and clean early `Close()`;
  - `Stat`, idempotent `Delete`, `KeyPrefix` isolation, and `DeletePrefix("runs/run-1")` deleting >1000 keys across multiple pages while preserving `"runs/run-10/..."` and handling `DeleteObjectsOutput.Errors`.
- [X] T003 [Green] Implement `internal/blob/s3/store.go`, `internal/blob/s3/permissions_unix.go`, and `internal/blob/s3/permissions_windows.go`, and verify `go test ./internal/blob/s3/... -count=1` passes.
- [X] T004 [Red -> Green] Write Tier 2 integration test `internal/blob/s3/store_integration_test.go` (`//go:build integration`) using Podman `docker.io/ministackorg/ministack:latest` verifying >12 MiB multipart streaming `Put` (with and without `expectedSHA256`), `Stat`, streaming `Get` SHA-256 verification, `ErrDigestMismatch`, `KeyPrefix` isolation, `DeletePrefix`, and zero leftover containers; run `go test -v -tags=integration ./internal/blob/... -count=1`.
- [X] T005 [Refactor & Verify] Review code for cleanliness/duplication, run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/blob/... -count=1`, verify zero leftover Podman containers, and mark all tasks `[X]`.
