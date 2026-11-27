# Implementation Plan: Local Dapr Host & Transparent Encryption Base (`022-local-dapr-host`)

**Branch**: `022-local-dapr-host` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 3 Slice `022`: implement `internal/daprhost` to manage local Dapr `scheduler` (standalone embedded `etcd` + `--placement-enabled`) and `daprd` processes bound exclusively to dynamic `127.0.0.1` loopback ports with isolated per-instance `APP_API_TOKEN` / `DAPR_API_TOKEN`, atomic restricted `master-key.json` generation/reuse, and Dapr component YAML rendering (`local-secrets.yaml`, encrypted `statestore.yaml` using `state.sqlite` + `primaryEncryptionKey`, and `pubsub.yaml` `pubsub.in-memory`). Verify via offline Tier 1 unit tests and real-binary Tier 2 integration tests (`//go:build integration`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Primary Dependencies**:
  - `github.com/dapr/go-sdk` `v1.16.0` (`client`, `actor`, `service/http`, `service/common`)
  - `gopkg.in/yaml.v3` (already in `go.mod`, for safe YAML rendering across Windows/Unix paths)
  - `golang.org/x/sys/windows` (Windows current-user protected DACL on `master-key.json`)
- **Local Runtime Binaries (Tier 2)**: Dapr `1.19.0` `scheduler` and `daprd` (`~/.dapr/bin/`)
- **Testing**:
  - Tier 1: `internal/daprhost/components_test.go` (`go test ./... -count=1`)
  - Tier 2: `internal/daprhost/manager_integration_test.go` (`//go:build integration`)

## Constitution Check

- **Principle I (Spec-First)**: All Spec Kit artifacts (`spec.md`, `plan.md`, `research.md`, `data-model.md`, `quickstart.md`, `contracts/daprhost.md`, `checklists/requirements.md`, `tasks.md`) authored before tests and implementation.
- **Principle II (Strict TDD)**: Failing Red tests in `components_test.go` and `manager_integration_test.go` are written and verified failing before implementing production code.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: Pure key/YAML/path resolution logic lives in `components.go` tested offline in Tier 1; real `scheduler` + `daprd` process orchestration and encryption/reminder persistence are verified in Tier 2 against actual Dapr binaries.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: State encryption uses Dapr's native AES-GCM `primaryEncryptionKey` mechanism (zero custom crypto), tokens are cryptographically random per instance and enforced at HTTP/gRPC boundaries without mutating process-global env vars.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `scheduler.exe --help`, `daprd.exe --help` (v1.19.0), `github.com/dapr/go-sdk@v1.16.0`, `github.com/dapr/dapr@v1.19.0`, and `internal/blob/permissions_{windows,unix}.go`. Scoped strictly to Slice `022` (`internal/daprhost`).

## Project Structure

```text
specs/022-local-dapr-host/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── daprhost.md
├── checklists/
│   └── requirements.md
└── tasks.md

internal/daprhost/
├── components.go               # Master key creation/reuse & Dapr component YAML rendering
├── permissions_unix.go         # 0600 key file permissions on Unix
├── permissions_windows.go      # Current-user protected DACL on Windows
├── manager.go                  # Options, Manager, binary lookup, dynamic ports, Start/Stop
├── process_unix.go             # Graceful SIGTERM -> SIGKILL process teardown on Unix
├── process_windows.go          # Process tree termination (taskkill /T /F /PID) on Windows
├── components_test.go          # Tier 1 unit tests
└── manager_integration_test.go # Tier 2 real scheduler + daprd integration tests
```

## Complexity Tracking

No Constitution violations.
