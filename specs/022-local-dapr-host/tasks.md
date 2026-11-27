---
description: "Stage 3 Slice 022 TDD tasks: Local Dapr Host & primaryEncryptionKey transparent encryption base (internal/daprhost)"
---

# Tasks

- [X] T001 Add `github.com/dapr/go-sdk` `v1.16.0` to `go.mod` (`go get github.com/dapr/go-sdk@v1.16.0`).
- [X] T002 [Red] Write failing Tier 1 unit tests in `internal/daprhost/components_test.go` covering:
  - `EnsureMasterKey` first creation (valid JSON, 64 lowercase hex chars, 32 decoded bytes, restricted permissions `0600` on Unix / protected current-user DACL on Windows);
  - `EnsureMasterKey` idempotent preservation on second call and explicit error on corrupt existing key file;
  - `WriteComponents` YAML schema validation for `local-secrets.yaml` (`secretstores.local.file`), `statestore.yaml` (`state.sqlite`, `actorStateStore: "true"`, `primaryEncryptionKey` `secretKeyRef`, top-level `auth.secretStore`, Windows path compatibility), and `pubsub.yaml` (`pubsub.in-memory`, name `"sofa-pubsub"`);
  - `NewManager` validation (empty `StateDir`, empty `AppID`, out-of-range `AppPort`, invalid explicit binary override), random `AppAPIToken` / `DaprAPIToken` generation, and binary discovery precedence (`Options` > `~/.dapr/bin` > `LookPath`).
- [X] T003 [Green] Implement `internal/daprhost/components.go`, `internal/daprhost/permissions_unix.go`, `internal/daprhost/permissions_windows.go`, `internal/daprhost/manager.go`, `internal/daprhost/process_unix.go`, and `internal/daprhost/process_windows.go`, and verify `go test ./internal/daprhost/... -count=1` passes.
- [X] T004 [Red -> Green] Write Tier 2 integration test `internal/daprhost/manager_integration_test.go` (`//go:build integration`) against real `scheduler` + `daprd`:
  - Start internal `127.0.0.1:<appPort>` `dapr/go-sdk/service/http` server verifying `dapr-api-token == m.AppAPIToken()` on callback routes and registering a `ServerImplBaseCtx` + `ReminderCallee` actor;
  - Call `m.Start(ctx)`, verify parent `os.Environ()` is unpolluted, write `"SUPER_SECRET_CANARY_VALUE_9918"` via Actor invocation, scan `<stateDir>/actor-state.db` (and `-wal` if present) raw bytes asserting plaintext absence;
  - Register a short-period persistent Actor Reminder and assert callback delivery within window;
  - Call `m.Stop(ctx)` and verify processes exit cleanly, then re-create and `Start(ctx)` a new `Manager` on the same `StateDir`, asserting `"SUPER_SECRET_CANARY_VALUE_9918"` is decrypted back and the persistent Reminder resumes firing.
  - Run `go test -v -tags=integration ./internal/daprhost/... -count=1`.
- [X] T005 [Refactor & Verify] Review code for cleanliness and zero duplication, run `go mod tidy`, `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/daprhost/... -count=1`, confirm zero lingering `scheduler`/`daprd` processes, and mark all tasks `[X]`.
