# Feature Specification: Local Dapr Host & Transparent Encryption Base (`022-local-dapr-host`)

**Feature Branch**: `022-local-dapr-host`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `022` blueprint (`docs/stage3/022-local-dapr-host.md`), `docs/stage3/README.md`, `docs/architecture-contract.md`, `internal/blob/permissions_{windows,unix}.go`, local Dapr `1.19.0` CLI flags (`scheduler`, `daprd`), and `github.com/dapr/go-sdk@v1.16.0` / `github.com/dapr/dapr@v1.19.0` source inspection.

## User Scenarios & Testing

### User Story 1 - Idempotent Master Key Generation & Dapr Component Rendering (Priority: P1)

`sofa server --dev` and Tier 2 Actor integration tests require local Dapr configuration artifacts generated deterministically inside a caller-supplied `StateDir` without custom encryption code. On preparation, `internal/daprhost` creates `<stateDir>/keys/master-key.json` atomically with a random 32-byte (64 lowercase hex characters) key (`masterEncryptionKey`) and restricts permissions to the current user (`0600` on Unix; Protected DACL for the current user SID on Windows). If `master-key.json` already exists and contains a valid 64-character hex key, it is preserved verbatim across restarts so encrypted state remains decryptable. It also renders three Dapr Component manifests in `<stateDir>/components/`: `local-secrets.yaml` (`secretstores.local.file`), `statestore.yaml` (`state.sqlite` with `actorStateStore: "true"`, `primaryEncryptionKey` `secretKeyRef`, and `auth.secretStore`), and `pubsub.yaml` (`pubsub.in-memory`, component name `"sofa-pubsub"`).

**Independent Test**: Unit-test master key creation (64 lowercase hex chars, valid 32-byte AES-256 key, restricted permissions), idempotent reuse on subsequent calls without overwriting existing keys, corrupt key rejection, YAML schema accuracy (`actorStateStore: "true"`, `primaryEncryptionKey` `secretKeyRef`, `auth.secretStore`, `"sofa-pubsub"`), Windows path escaping/URI compatibility, binary discovery precedence, and invalid `Options` validation (`internal/daprhost/components_test.go`).

### User Story 2 - Managed Dual-Process Runtime (`scheduler` + `daprd`) with Dynamic Loopback Ports & Mutual API Tokens (Priority: P1)

To run multiple tests or single-node stacks concurrently with zero port collisions and without exposing Dapr control endpoints or unauthenticated callbacks to other local processes, `Manager` resolves `scheduler` and `daprd` binaries in priority order (explicit `Options.{SchedulerBin,DaprdBin}` > `~/.dapr/bin/{scheduler,daprd}` with Windows `.exe` support > `PATH`), dynamically reserves free `127.0.0.1` loopback ports for all `scheduler` (`port`, `healthz`, `metrics`, `etcd-client`, `etcd-initial-cluster` peer) and `daprd` (`http`, `grpc`, `internal-grpc`, `public`, `metrics`) listeners, generates isolated per-instance random `APP_API_TOKEN` (when omitted) and `DAPR_API_TOKEN`, injects them exclusively into child process environments (never mutating parent `os.Setenv`), waits for `scheduler /healthz`, `daprd /v1.0/healthz/outbound`, and Actor Runtime placement readiness (`ActorRuntime_RUNNING` / `HostReady`), and returns a `daprclient.Client` pre-configured with `WithAuthToken(m.DaprAPIToken())`.

**Independent Test**: Unit-test binary resolution priority (explicit override > `~/.dapr/bin` > `LookPath`), invalid `Options` rejection, token generation, and non-mutation of parent environment; integration-test full `Start(ctx)` / `Stop(ctx)` lifecycle against real `scheduler` and `daprd` binaries (`internal/daprhost/manager_integration_test.go`).

### User Story 3 - Verified At-Rest Encryption & Cross-Restart Actor State / Reminder Recovery (Priority: P1)

State persisted by Dapr Actors through `sofa-statestore` MUST be transparently encrypted by `daprd` using `primaryEncryptionKey` before hitting `<stateDir>/actor-state.db` and MUST survive full `Manager.Stop` + re-`Start` in the same `StateDir` (including clean release of SQLite and embedded `etcd` locks on Windows and Unix).

**Independent Test**: Tier 2 integration test (`//go:build integration` in `internal/daprhost/manager_integration_test.go`) starting an internal `127.0.0.1:<AppPort>` HTTP callback service (verifying incoming `dapr-api-token` equals `m.AppAPIToken()`), registering a `ServerImplBaseCtx` + `ReminderCallee` test actor, calling `m.Start(ctx)`, writing `"SUPER_SECRET_CANARY_VALUE_9918"`, asserting raw bytes of `actor-state.db` (and `-wal` if present) do NOT contain the plaintext canary, asserting a short-period persistent Actor Reminder fires, calling `m.Stop(ctx)`, creating a fresh `Manager` on the same `StateDir`, calling `Start(ctx)`, and asserting `"SUPER_SECRET_CANARY_VALUE_9918"` is decrypted back and the persistent Reminder resumes firing.

### Edge Cases

- `NewManager` MUST reject empty/whitespace `StateDir`, empty/whitespace `AppID`, negative or out-of-range `AppPort` (`< 0 || > 65535`), or missing/non-regular explicit `SchedulerBin` / `DaprdBin` paths.
- If `master-key.json` exists with invalid JSON, missing `masterEncryptionKey`, or non-64-char hex content, preparation MUST fail with an explicit error rather than silently overwriting the key and destroying access to historical encrypted state.
- If `Start(ctx)` fails partway (e.g., `daprd` fails to start or `ctx` cancels while waiting for readiness), `Manager` MUST stop any already-launched child processes and close any client connection before returning the error.
- `Stop(ctx)` MUST be idempotent and MUST wait for both `daprd` (preferring graceful gRPC `Shutdown` first so SQLite flushes cleanly) and `scheduler` processes to fully exit before returning so the same `StateDir` can be immediately reopened without file lock errors on Windows or Unix.

## Requirements

### Functional Requirements

- **FR-001**: `internal/daprhost` MUST export `Options`, `Manager`, `NewManager(opts Options) (*Manager, error)`, `AppAPIToken() string`, `DaprAPIToken() string`, `GRPCAddress() string`, `Client() daprclient.Client`, `Start(ctx context.Context) (daprclient.Client, error)`, `Stop(ctx context.Context) error`, and `NewActorStateManager(actorType, actorID string, client daprclient.Client) actor.StateManagerContext` matching `docs/stage3/022-local-dapr-host.md` §2 & §3.1 (plus optional non-breaking read-only getters / `ExternalSchedulerAddr`).
- **FR-002**: Binary discovery MUST follow `Options.{SchedulerBin,DaprdBin}` > `~/.dapr/bin/{scheduler,daprd}` (with `.exe` on Windows) > `exec.LookPath`.
- **FR-003**: `<stateDir>/keys/master-key.json` MUST be created atomically on first run with a random 32-byte (64 lowercase hex) `masterEncryptionKey` and restricted permissions (`0600` on Unix; protected current-user DACL on Windows), and preserved untouched if already present and valid.
- **FR-004**: `<stateDir>/components/` MUST render valid Dapr `v1alpha1` YAML manifests for `local-secrets.yaml` (`secretstores.local.file`), `statestore.yaml` (`state.sqlite`, `actorStateStore: "true"`, `primaryEncryptionKey` `secretKeyRef`, `auth.secretStore`), and `pubsub.yaml` (`pubsub.in-memory` named `"sofa-pubsub"`).
- **FR-005**: All `scheduler` and `daprd` network ports MUST be dynamically allocated on `127.0.0.1` and bound exclusively to `127.0.0.1`.
- **FR-006**: `APP_API_TOKEN` and `DAPR_API_TOKEN` MUST be injected only into child process environments (`cmd.Env`) without modifying parent `os.Environ`, and the returned `daprclient.Client` MUST be configured via `WithAuthToken`.
- **FR-007**: `Start(ctx)` MUST enforce startup readiness (`scheduler /healthz`, `daprd /v1.0/healthz/outbound`, and `GetMetadata` Actor Runtime readiness) and `Stop(ctx)` MUST cleanly shut down and reap both processes, releasing SQLite and `etcd` locks.

## Success Criteria

### Measurable Outcomes

- **SC-001**: Tier 1 unit tests in `internal/daprhost/components_test.go` pass deterministically offline via `go test ./... -count=1`.
- **SC-002**: Tier 2 integration tests in `internal/daprhost/manager_integration_test.go` (`//go:build integration`) pass via `go test -v -tags=integration ./internal/daprhost/... -count=1` against local `scheduler.exe` and `daprd.exe`, proving `dapr-api-token` callback enforcement, at-rest encryption of `"SUPER_SECRET_CANARY_VALUE_9918"`, Reminder execution, and full state + Reminder recovery after `Stop` + `Start` on the same `StateDir`, with zero orphan `scheduler`/`daprd` processes remaining.
- **SC-003**: `gofmt -l .` produces empty output and `go vet ./...` passes cleanly.
