# Research: `dapr/go-sdk@v1.16.0` Actor Semantics & `plan.Plan` Reference Layout (`023-config-actor-env-secrets`)

## 1. `dapr/go-sdk@v1.16.0` Actor Reflection, `ImplActorClientStub`, and `StateManagerContext`

Inspected `C:\Users\hezijie\go\pkg\mod\github.com\dapr\go-sdk@v1.16.0\`:

1. **Method Signatures & Single-Param Pointer Unmarshaling (`actor/manager/container.go` L104-122, `actor/manager/manager.go` L285-335, `client/actor.go` L340-407)**:
   - `DefaultActorContainerContext.Invoke` accepts zero or one non-context argument (`typ := methodType.argsType[0]`), allocates `reflect.New(typ)`, JSON-unmarshals `param` into `paramValue.Interface()`, and passes `Elem()` (which is `*Req` when the method declares `*Req`) to the actor method.
   - Return signature MUST be `(*Resp, error)` (or `error`), matching `func(context.Context, *Req) (*Resp, error)`.
   - `ImplActorClientStub` binds exported struct fields of type `func(context.Context, *Req) (*Resp, error)` on a struct implementing `actor.Client` (`Type() string` and `ID() string`), JSON-marshaling `*Req` and JSON-unmarshaling `*Resp`.
   - **Important**: In `actor/manager/manager.go` L172-174, when an actor method returns a non-nil `error`, `InvokeMethod` discards `replyv` and returns `actorErr.ErrActorInvokeFailed`. Therefore `PreflightCheck` and `ResolveForNode` return structured `MissingKeys` in their response struct with `nil` error (reserving non-nil `error` for validation failures, state errors, and same-scope `Env`/`Secret` conflicts).

2. **`StateManagerContext` Pitfalls (`actor/state/state_manager.go` L108-250 & `state_async_provider.go` L35-112)**:
   - `Get` when a key does not yet exist in the backing store returns an error (`"get actor state result empty..."`) **and still stores an empty entry in `stateChangeTracker`**. Calling `Contains(ctx, key)` BEFORE `Get(ctx, key, &doc)` avoids that trap because `ContainsContext` returns `(false, nil)` when `len(result.Data) == 0`.
   - `Remove(ctx, key)` in `v1.16.0` L200 has an upstream bug (`if exist, err := ContainsContext(...); err != nil && exist`) when the key has not been loaded into `stateChangeTracker` in the current activation. Storing `ConfigActor`'s three-tier state inside a single versioned state document (`"config_store_v1"`) via `Set` + `Save` avoids `Remove` completely, makes `DeleteRunScope` atomic and restart-safe, and reduces `PreflightCheck`/`ResolveForNode` to a single state read.
   - `NewDefaultActorContainerContext` initially constructs a `StateManagerContext` backed by `dapr.NewClient()` (global default). By overriding `SetStateManager` on `ConfigActor` with an optional `clientProvider func() daprclient.Client` (matching `internal/daprhost/manager_integration_test.go`), Tier 2 tests and `sofa server` bind `daprhost.NewActorStateManager(a.Type(), a.ID(), client)`, while Tier 1 tests pass `clientProvider == nil` and inject an in-memory `actor.StateManagerContext` directly via `SetStateManager`.

## 2. Verified `plan.Plan` & `NodeSpec.Config` Schemas (`internal/planner/export.go` & Resource Packages)

Inspected `internal/plan/model.go`, `internal/planner/export.go`, `internal/planner/defaults.go`, `internal/planner/module.go`, `internal/source/schema.go`, `internal/provider/schema.go`, `internal/harness/spec/schema.go`, `internal/mcp/spec/schema.go`, `internal/tool/spec/schema.go`, `internal/harness/runtime/client.go`, `internal/tool/runtime/executor.go`, and `internal/worker/runner/worker_runner.go`:

1. **`source` (`internal/planner/export.go` L442-449)**:
   - `Config` object has `url` (`string`), `branch` (`string`), `token_ref` (`string` or `null`), `ssh_key_ref` (`string` or `null`).
2. **`model_provider` (`export.go` L521-557)**:
   - `Config` object has `api_key_ref` (`string` or `null`) and `bearer_token_ref` (`string` or `null`).
3. **`harness` (`export.go` L490-519, `internal/harness/runtime/client.go` L697)**:
   - `Config` object has `env_refs` (`map(string)`).
   - `MaterializeHarnessEnv` iterates `for dstKey, srcEnv := range envRefs { val, ok := lookupEnv(srcEnv) }`: **map values (`srcEnv`) are the host-side reference names**, while map keys (`dstKey`) are the target container env variable names.
4. **`mcp_server` (`export.go` L559-607, `internal/harness/runtime/client.go` L647-673)**:
   - `Config` object has `transport` (`"http"` | `"stdio"`), `http` (object with `bearer_token_ref` `string` or `null`, or `http == null`), and `stdio` (object with `env_refs` `map(string)`, or `stdio == null`).
   - `MaterializeMCPServerConfig` reads `*cfg.HTTP.BearerTokenRef` for HTTP and `srcEnv` (map values) of `cfg.Stdio.EnvRefs` for Stdio.
5. **`external_tool` (`export.go` L622-633, `internal/tool/runtime/executor.go` L546-555)**:
   - Current `ExternalToolBlock` does not declare an `env_refs` HCL attribute yet, while `toolruntime.ToolBinding.EnvRefs` uses the same `dstKey -> srcEnv` convention (`lookupEnv(srcEnv)`). `plan_refs.go` checks `NodeSpec.Config` for an `"env_refs"` map attribute when present on `external_tool` nodes so both current compiled plans and hand-constructed/future plans with `external_tool` `env_refs` work identically.
6. **Per-Node Dependency & Singleton Resolution (`worker`, `dynamic_worker`, `merge`, `module`)**:
   - `worker.Dependencies` includes transitive ancestors in its module (so a downstream `worker.after_local` lists `mcp_server` used only by upstream `worker.plan_step`). Therefore `ExtractNodeEnvRefs` inspects the target node's actual `Config` references (`harness.address`, `model_provider.address`, `qc.harness.address`, `qc.model_provider.address`, `source_url` matched against `source` nodes, `mcp_tool_ids` / `mcp_resource_ids` matched against `mcp_server` deterministic IDs, and `tool_ids` / `terminate_tool_id` / `tool_uses` matched via `Plan.Tools()` / node addresses), falling back to direct resource `Dependencies` only when `Config` is empty/unpopulated (e.g. minimal test plans).
   - Child module `worker` blocks that inherit a root singleton `harness` or `model_provider` carry its root canonical address in `config.harness.address` / `config.model_provider.address` even though that singleton node lives in an ancestor `Plan`. Indexing all nodes across the `Plan` tree by canonical address ensures cross-module singleton refs are always found.
