# Research: Dapr `1.19.0` CLI, `go-sdk@v1.16.0`, and Encryption (`022-local-dapr-host`)

## 1. Local Dapr `1.19.0` CLI Flags (`scheduler.exe --help`, `daprd.exe --help`)

- **`scheduler` (v1.19.0)** supports standalone embedded `etcd` + integrated Actor Placement:
  - `--mode=standalone`
  - `--id=dapr-scheduler-server-0`
  - `--etcd-embed=true`
  - `--etcd-data-dir=<absStateDir>/scheduler`
  - `--etcd-client-listen-address=127.0.0.1` and `--etcd-client-port=<port>`
  - `--etcd-initial-cluster=dapr-scheduler-server-0=http://127.0.0.1:<peerPort>`
  - `--placement-enabled=true`
  - `--listen-address=127.0.0.1` and `--port=<grpcPort>`
  - `--healthz-listen-address=127.0.0.1` and `--healthz-port=<healthzPort>` (`GET /healthz` returns `200 OK` once ready)
  - `--metrics-listen-address=127.0.0.1`, `--metrics-port=<metricsPort>`, `--enable-metrics=false`
- **`daprd` (v1.19.0)**:
  - `--mode=standalone`
  - `--app-id=<appID>`
  - `--app-port=<appPort>`, `--app-channel-address=127.0.0.1`, `--app-protocol=http` (when `AppPort > 0`)
  - `--dapr-listen-addresses=127.0.0.1`
  - `--dapr-http-port=<httpPort>`, `--dapr-grpc-port=<grpcPort>`
  - `--dapr-internal-grpc-listen-address=127.0.0.1`, `--dapr-internal-grpc-port=<internalGRPCPort>`
  - `--dapr-public-listen-address=127.0.0.1`, `--dapr-public-port=<publicPort>`
  - `--metrics-listen-address=127.0.0.1`, `--metrics-port=<metricsPort>`, `--enable-metrics=false`
  - `--scheduler-host-address=127.0.0.1:<schedulerPort>` (note: in `dapr@v1.19.0/pkg/runtime/config.go` `SchedulerPlacementEnabled()`, standalone mode automatically enables scheduler-based actor placement when `--scheduler-host-address` is set, without requiring `--placement-host-address`)
  - `--resources-path=<componentsDir>`
  - `--disable-builtin-k8s-secret-store=true`
  - `--enable-mtls=false`
  - `--dapr-graceful-shutdown-seconds=2`

## 2. `github.com/dapr/dapr@v1.19.0` Internals (`GOMODCACHE`)

- **Encryption (`pkg/encryption/encryption.go`, `pkg/runtime/processor/state/state.go`, `tests/integration/suite/daprd/state/http/encryption.go`)**:
  - `createCipher` calls `hex.DecodeString(key.Key)` and `aes.NewCipher(keyBytes)` with `AES-GCM`. Thus 32 random bytes encoded as 64 lowercase hex characters produce a valid AES-256-GCM key.
  - `Component` YAML requires top-level `auth: secretStore: sofa-secrets` and metadata entry:
    ```yaml
    - name: primaryEncryptionKey
      secretKeyRef:
        name: masterEncryptionKey
        key: masterEncryptionKey
    ```
    matching JSON `{"masterEncryptionKey":"<64-hex>"}` loaded by `secretstores.local.file` (`secretsFile`).
  - `state.sqlite` uses `connectionString: "file:<toSlash(stateDir)>/actor-state.db"` and `actorStateStore: "true"`.
- **Health & Actor Placement Readiness (`pkg/api/http/healthz.go`, `pkg/actors/actors.go`, `pkg/api/http/metadata.go`)**:
  - `GET /v1.0/healthz/outbound` is marked `AlwaysAllowed: true` (does not require `dapr-api-token`) and returns `204 No Content` once outbound components initialize.
  - `GetMetadata` on gRPC (`pb.DaprClient.GetMetadata`, authenticated with `dapr-api-token`) returns `ActorRuntime` with `RuntimeStatus == pb.ActorRuntime_RUNNING`, `HostReady == true`, and `Placement == "placement: connected"` once placement stream registration succeeds.
- **Process Shutdown (`tests/integration/framework/process/exec/kill`)**:
  - Calling `daprclient.Shutdown(ctx)` triggers `daprd`'s clean shutdown path so SQLite closes connections and checkpoints WAL cleanly before fallback process termination.
  - On Windows, `taskkill /T /F /PID` cleanly tears down child process trees; on Unix, `SIGTERM` followed by `SIGKILL` after grace period cleans up embedded `etcd` and releases all locks.

## 3. `github.com/dapr/go-sdk@v1.16.0` Source Inspection (`GOMODCACHE`)

- **Non-deprecated Actor API (`actor/actor.go`)**:
  - `actor.ServerContext` + embedded `actor.ServerImplBaseCtx` + `actor.ReminderCallee` (`ReminderCall(reminderName string, data []byte, dueTime, period string)`).
  - Registered via `s.RegisterActorImplFactoryContext(factory)`.
- **Per-Client Auth Token (`client/client.go`)**:
  - `NewClientWithAddressContext(ctx, addr)` installs `authTokenUnaryInterceptor` and `authTokenStreamInterceptor` that read `c.authToken`. Calling `c.WithAuthToken(token)` injects `dapr-api-token` outgoing metadata on that client instance alone without touching `os.Setenv`.
- **HTTP Callback Server (`service/http/service.go`, `service/http/invoke.go`, `service/http/topic.go`)**:
  - `daprhttp.NewServiceWithMux(addr, router)` allows attaching a `chi.Mux` middleware that enforces `r.Header.Get(common.APITokenKey) == expectedAppAPIToken` across `/dapr/config`, `/actors/...`, and all Dapr callback routes, proving `daprd` sends `APP_API_TOKEN` in `dapr-api-token`.
