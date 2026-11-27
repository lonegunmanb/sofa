# Implementation Plan: S3 / MinIO Compatible Streaming `BlobStore` (`021-s3-blobstore`)

**Branch**: `021-s3-blobstore` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 3 Slice `021`: implement `internal/blob/s3` (`Config`, `NewStore`, `Store` implementing `blob.Store`) backed by `github.com/aws/aws-sdk-go-v2/service/s3` and the non-deprecated `github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager` for streaming multipart uploads, end-to-end SHA-256 verification via `x-amz-meta-sofa-sha256`, `KeyPrefix` isolation, and paginated slash-boundary `DeletePrefix`. Verify with deterministic Tier 1 unit tests via an injected narrow `s3API` interface and Tier 2 Podman `ministack:latest` integration tests.

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Primary Dependencies**:
  - `github.com/aws/aws-sdk-go-v2`
  - `github.com/aws/aws-sdk-go-v2/config`
  - `github.com/aws/aws-sdk-go-v2/credentials`
  - `github.com/aws/aws-sdk-go-v2/service/s3`
  - `github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager` (replaces deprecated `feature/s3/manager`)
  - `github.com/aws/smithy-go`
  - `golang.org/x/sys/windows` (Windows protected DACL on temp staging file when `expectedSHA256 == ""`)
- **Existing Dependencies Consumed**: `internal/blob` (`Store`, `ObjectMeta`, `ValidateKey`, `ValidatePrefix`, `IsValidSHA256Hex`, `ErrNotFound`, `ErrDigestMismatch`, `ErrInvalidKey`)
- **Testing**:
  - Tier 1: `internal/blob/s3/store_test.go` (`go test ./... -count=1`)
  - Tier 2: `internal/blob/s3/store_integration_test.go` (`//go:build integration`, Podman `docker.io/ministackorg/ministack:latest`)

## Constitution Check

- **Principle I (Spec-First)**: All Spec Kit artifacts (`spec.md`, `plan.md`, `research.md`, `data-model.md`, `quickstart.md`, `contracts/s3-blobstore.md`, `tasks.md`, `checklists/requirements.md`) authored before code.
- **Principle II (Strict TDD)**: Failing Red tests in `internal/blob/s3/store_test.go` are written and confirmed failing before implementing `store.go`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `Store` depends on narrow `s3API` (embedding `transfermanager.S3APIClient` + `DeleteObject` + `DeleteObjects`), enabling fast offline Tier 1 tests and real S3 container Tier 2 tests against `ministack`.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Key/prefix validation, hex digest validation, rollback deletion on mismatch, and EOF stream hashing are enforced in Go runtime code.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `internal/blob/{store.go,local.go,store_test.go}` and `aws-sdk-go-v2` sources in `GOMODCACHE` (`feature/s3/manager` deprecation notice, `feature/s3/transfermanager@v0.4.1`, `service/s3@v1.114.5`). Scoped strictly to Slice `021` (`internal/blob/s3`).

## Project Structure

```text
specs/021-s3-blobstore/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── s3-blobstore.md
├── checklists/
│   └── requirements.md
└── tasks.md

internal/blob/s3/
├── store.go                  # Config, s3API, NewStore, Store implementing blob.Store
├── permissions_unix.go       # 0600 temp file restriction on Unix
├── permissions_windows.go    # Current-user protected DACL on Windows
├── store_test.go             # Tier 1 offline unit tests with in-memory fake s3API
└── store_integration_test.go # //go:build integration Podman ministack tests
```

## Complexity Tracking

No Constitution violations.
