# Research: Slice `024` (`ProcessRegistryActor` & `WorkerDaemon`)

## 1. Upstream `dapr/go-sdk@v1.16.0` Inspection

- **Reminder State Persistence Trap (`actor/manager/manager.go:210-215`)**:
  - `DefaultActorManagerContext.InvokeMethod` automatically calls `actorContainer.GetActor().SaveState(ctx)` (line 179), whereas `InvokeReminder` directly calls `targetActor.ReminderCall(reminderName, reminderParams.Data, reminderParams.DueTime, reminderParams.Period)` and returns `actorErr.Success` **without calling `SaveState`**.
  - **Decision**: `ProcessRegistryActor.ReminderCall` MUST explicitly call `saveStore(ctx, store)` (`sm.Set` + `sm.Save`) whenever any process status or lease state changes.
- **StateManager Single-Document Pattern (`internal/actor/config/actor.go`)**:
  - `daprhost.NewActorStateManager(a.Type(), a.ID(), c)` binds `StateManagerContext` to the dynamic-port `daprclient.Client`.
  - Using `sm.Contains(ctx, stateKey)` before `sm.Get(ctx, stateKey, &doc)` avoids `StateManager` tracker caching pitfalls on uninitialized actors; persisting a single JSON state document via `sm.Set` + `sm.Save` guarantees atomic updates across processes and leases.
- **Actor Method Signatures (`actor/manager/manager.go:285+`, `client/actor.go:296+`)**:
  - Both server reflection and `ImplActorClientStub` expect exported methods with signature `func(context.Context, *Req) (*Resp, error)`.
- **Service Invocation (`service/http/invoke.go`, `client/invoke.go`)**:
  - `AddServiceInvocationHandler(route, fn)` registers chi HTTP routes on `daprhttp.NewServiceWithMux`; `client.InvokeMethodWithContent(ctx, appID, method, "POST", &DataContent{...})` invokes them across `daprd` sidecars.
  - Note: `daprhttp.NewServiceWithMux` reads `APP_API_TOKEN` from `os.Getenv` in `newServer`, whereas `daprhost` tests/runtime enforce `dapr-api-token` via chi middleware on the callback router using `mgr.AppAPIToken()` so multiple `daprhost.Manager` instances run concurrently in one process without mutating process-global env vars.

## 2. Upstream `dapr/dapr@v1.19.0` Multi-`daprd` Local Name Resolution

- **Observation (`pkg/runtime/runtime.go:1384-1455`, `cmd/daprd/components/nameresolution_sqlite.go`)**:
  - In `modes.StandaloneMode`, when `Configuration.Spec.NameResolutionSpec` is unset, `daprd` defaults to `"mdns"`. On Windows loopback (`127.0.0.1`) with local firewall rules, UDP multicast mDNS between multiple local `daprd` processes can fail or stall.
  - `daprd` v1.19.0 compiles in `nameresolution.sqlite` (`nrLoader.DefaultRegistry.RegisterComponent(sqlite.NewResolver, "sqlite")`), configured via a Dapr `Configuration` YAML (`spec.nameResolution.component: "sqlite"`, `version: "v1"`, `configuration.connectionString: "<path>"`) passed to `daprd` via `--config=<path>`.
  - **Decision**: Add optional `NameResolutionDBPath string` to `daprhost.Options` (preserving 100% backward compatibility when empty). When set, `daprhost` writes `<stateDir>/dapr-config.yaml` and passes `--config=<path>` to `daprd`, allowing multiple local `daprd` instances sharing the same SQLite path (and `ExternalSchedulerAddr`) to perform fast, deterministic cross-sidecar `InvokeMethod` and Actor calls on Windows and Unix.

## 3. Integration with Existing `runner` & `blob` Packages

- **`NodeExecutionRequest.LookupEnv` (`internal/worker/runner/worker_runner.go:118-121`)**:
  - `WorkerRunner.Run` falls back to `os.LookupEnv` ONLY when `req.LookupEnv == nil`.
  - **Decision**: `WorkerDaemon` MUST always set `req.LookupEnv` to a closure backed exclusively by a defensive copy of `StartNodeRequest.ResolvedEnv` (never nil, never `os.LookupEnv`).
- **Checkpoint & Node Exit Log Key (`internal/blob/store.go`, `internal/git/ledger.go`)**:
  - `blob.ValidateKey` requires relative slash-separated segments matching `^[A-Za-z0-9._-]+$`.
  - Canonical node addresses like `dynamic_worker.dyn["check/1"]` contain brackets, quotes, and slashes, so `WorkerDaemon` sanitizes `run_id` and `node_addr` into `<node_key>` (matching `git.sanitizeNodeKey` style: sanitized prefix + `_` + 8-hex SHA-256) so `runs/<run_id>/logs/<node_key>/e<epoch>.jsonl` always satisfies `blob.ValidateKey`.
- **Downstream Slice Alignment (`025` & `026`)**:
  - `OnWorkerDead` Actor method payload: `WorkerDeadNotification{LeaseID, LeaseKey, ProcessID, AppID, Reason}`.
  - `WorkerDaemon` delegates node execution via `NodeExecutor` + `NodeResultReporter` (for `025` `NodeActor.ReportOutcome`) and merge execution via `MergeExecutor` + `MergeResultReporter` (for `026` `MergeActor.ReportCandidateResult`).
