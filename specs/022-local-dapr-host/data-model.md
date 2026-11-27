# Data Model: Local Dapr Host (`022-local-dapr-host`)

## 1. Configuration & Runtime Entities

| Entity | Description | Key Fields / Invariants |
| :--- | :--- | :--- |
| `Options` | Caller configuration for `NewManager` | `StateDir` (non-empty), `AppID` (non-empty), `AppPort` (`0..65535`), `AppAPIToken` (optional; 32-byte random hex if empty), `DaprAPIToken` (optional; 32-byte random hex if empty), `SchedulerBin` (optional override), `DaprdBin` (optional override), `ExternalSchedulerAddr` (optional external scheduler host:port) |
| `masterKeyFile` | `<stateDir>/keys/master-key.json` | JSON `{"masterEncryptionKey": "<64 lowercase hex chars>"}`; atomic temp-file write + `0600` (Unix) / protected current-user DACL (Windows); preserved verbatim if already valid |
| `local-secrets.yaml` | `<stateDir>/components/local-secrets.yaml` | `Component` `sofa-secrets` (`secretstores.local.file`, `secretsFile` -> `master-key.json`) |
| `statestore.yaml` | `<stateDir>/components/statestore.yaml` | `Component` `sofa-statestore` (`state.sqlite`, `connectionString: file:<slashStateDir>/actor-state.db`, `actorStateStore: "true"`, `primaryEncryptionKey` `secretKeyRef: {name: masterEncryptionKey, key: masterEncryptionKey}`, `auth.secretStore: sofa-secrets`) |
| `pubsub.yaml` | `<stateDir>/components/pubsub.yaml` | `Component` `sofa-pubsub` (`pubsub.in-memory`, `v1`) |
| `Manager` | Lifecycle controller for `scheduler` + `daprd` + `daprclient.Client` | Owns resolved binary paths, dynamic loopback ports, per-instance tokens, child `exec.Cmd` handles, and active gRPC client |

## 2. Lifecycle State Transitions

1. **`Created` (`NewManager`)**: Validates `Options`, resolves `StateDir` to absolute path, resolves `scheduler`/`daprd` binaries, generates random `AppAPIToken` and `DaprAPIToken` if omitted.
2. **`Starting` (`Start(ctx)`)**:
   - Ensures `<stateDir>/keys/master-key.json` and `<stateDir>/components/*.yaml`.
   - Reserves dynamic `127.0.0.1` ports.
   - Starts `scheduler` (unless `ExternalSchedulerAddr` is set) and waits for `GET http://127.0.0.1:<healthzPort>/healthz` == `200 OK`.
   - Starts `daprd` with isolated `cmd.Env` (`APP_API_TOKEN`, `DAPR_API_TOKEN`), waits for `GET /v1.0/healthz/outbound` (`200`/`204`), connects `daprclient.Client` with `WithAuthToken(m.DaprAPIToken())`, and waits for `GetMetadata` `ActorRuntime_RUNNING` (plus `HostReady` & `placement: connected` when `AppPort > 0`).
   - Rolls back and tears down partial resources on any error.
3. **`Running`**: Serves `GRPCAddress()`, `HTTPAddress()`, `AppAPIToken()`, `DaprAPIToken()`, and active `daprclient.Client`.
4. **`Stopped` (`Stop(ctx)`)**: Calls `client.Shutdown` + `client.Close()`, waits for `daprd` exit (with kill fallback), terminates `scheduler` and waits for exit, releasing SQLite and `etcd` file locks.
