# Feature Specification: Three-Tier Managed Env / Secret ConfigActor & `.sofaplan` Preflight Scanner (`023-config-actor-env-secrets`)

**Feature Branch**: `023-config-actor-env-secrets`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 3 Slice `023` blueprint (`docs/stage3/023-config-actor-env-secrets.md`), `docs/stage3/README.md`, `docs/architecture-contract.md`, `docs/hcl-contract.md`, `internal/daprhost`, `internal/plan`, `internal/planner`, `internal/harness/runtime`, and `github.com/dapr/go-sdk@v1.16.0` source inspection.

## User Scenarios & Testing

### User Story 1 - Three-Tier Managed `Env` / `Secret` Store with Same-Scope Mutual Exclusion & Write-Only Secrets (Priority: P1)

Control-plane operators and automated runs need a single authoritative store (`ConfigActor` with type `"ConfigActor"` and singleton ID `"global"`) managing environment variables and secrets across three strictly ordered inheritance tiers: **`Run` (`<run_id>`) > `Namespace` (default `"default"`, no pre-creation required) > `Global`**. Within any single scope, a key name (`^[A-Za-z_][A-Za-z0-9_]*$`) CANNOT exist as both an `Env` and a `Secret`: attempting to overwrite an existing `Secret` with an `Env` (or vice versa) in the same scope MUST fail with an explicit conflict error to prevent accidental secret exposure into readable `Env` listings, while same-kind overwrites and cross-scope overrides are permitted. `Secret` values are strictly Write-Only across all query/listing APIs (`ListMetadata` returns `EnvEntry` with `Value` and `SecretMeta` with zero value field). `DeleteEntry` removes a single key from a scope, and `DeleteRunScope` cleans up all `Run`-scoped entries when `sofa delete <run_id>` runs.

**Independent Test**: Tier 1 unit tests in `internal/actor/config/config_test.go` using an in-memory `actor.StateManagerContext` verifying:
- `Run > Namespace("proj-a") > Global` override precedence and complete isolation between `Namespace("proj-a")` and `Namespace("proj-b")` (and default `"default"` normalization for empty namespace strings);
- Invalid key name rejection (`^[A-Za-z_][A-Za-z0-9_]*$`), invalid/missing scope parameters (`ScopeRun` without `RunID`), and same-batch duplicate/conflicting entries;
- Bidirectional same-scope `Env` vs `Secret` mutual exclusion conflict errors, successful same-kind overwrite updates, successful deletion followed by re-insertion with the other kind, and `DeleteRunScope` cleanup;
- `ListMetadata` returning sorted `EnvEntry` (with `Value`) and `SecretMeta` (structurally incapable of holding plaintext values).

### User Story 2 - Static `.sofaplan` Reference Extraction & Zero-Second Preflight Verification (Priority: P1)

When `sofa apply` receives a `.sofaplan`, the control plane MUST fail at second 0 before launching any container or Run if any referenced host variable is missing from the merged `RunOverrides > Run > Namespace > Global` view, and when dispatching a specific node to `sofa-worker`, it MUST resolve ONLY the minimal host variable map required by that node (`ResolveForNode`). Pure scanner functions `ExtractPlanEnvRefs(p *plan.Plan)` and `ExtractNodeEnvRefs(p *plan.Plan, nodeAddr string)` recursively scan the root `Plan` and all nested `Module.Plan` instances for:
- `source`: `token_ref`, `ssh_key_ref`;
- `model_provider`: `api_key_ref`, `bearer_token_ref`;
- `harness`: `env_refs` **values** (container env var keys are map keys and MUST NOT be treated as host ref names);
- `mcp_server`: `http.bearer_token_ref` and `stdio.env_refs` **values**;
- `external_tool`: `env_refs` **values** (when present on `NodeSpec.Config`).

**Independent Test**: Tier 1 unit tests in `internal/actor/config/config_test.go` compiling a real HCL workflow via `internal/planner` (plus covering `external_tool` `env_refs` and nested child modules, including root singleton inheritance and `for_each` / `cty.NullVal` / `cty.UnknownVal` / `golden.SensitiveMark` edge cases) to verify `ExtractPlanEnvRefs` and `ExtractNodeEnvRefs` extract 100% of host ref names in sorted deduplicated order, never confuse `env_refs` map keys for host refs, and never over-include unrelated nodes' secrets. `PreflightCheck` returns sorted deduplicated `MissingKeys` (never values) and `ResolveForNode` returns only the requested `Keys` merged across `RunOverrides > Run > Namespace > Global`.

### User Story 3 - Type-Safe Dapr Client Stub & Encrypted At-Rest Cross-Restart Persistence (Priority: P1)

Remote control-plane callers (`028`/`029`/`030`) interact with `ConfigActor("global")` via `NewClientStub(client)` backed by `dapr/go-sdk`'s `ImplActorClientStub`, while `ConfigActor` persists encrypted state through `daprhost.NewActorStateManager` into `state.sqlite` with `primaryEncryptionKey`.

**Independent Test**: Tier 2 integration test (`//go:build integration` in `internal/actor/config/config_integration_test.go`) starting a real `scheduler` + `daprd` via `daprhost.Manager` and an authenticated loopback callback server, invoking `UpsertEntries`, `ListMetadata`, `PreflightCheck`, `ResolveForNode`, and `DeleteRunScope` via `NewClientStub`, stopping `Manager`, inspecting raw `actor-state.db` (and `-wal`) bytes to confirm `"ULTRA_SECRET_TOKEN_778899"` does not appear in plaintext, restarting `Manager` on the same `StateDir`, and asserting all three tiers of `Env` and `Secret` state recover identically.

### Edge Cases

- Calling `ExtractPlanEnvRefs(nil)` returns an empty slice without panicking; calling `ExtractNodeEnvRefs(nil, ...)` or querying a non-existent `nodeAddr` returns a descriptive error.
- Empty `Namespace` strings (`""` or whitespace-only) on `ScopeNamespace`, `PreflightCheckReq`, and `ResolveForNodeReq` normalize to `"default"`.
- `ScopeRun` operations with empty/whitespace `RunID` return a validation error (`DeleteRunScope` on a non-existent `RunID` is idempotent with `DeletedCount: 0`).
- Passing `nil` request pointers to any `ConfigActor` method returns a clear error instead of panicking.
- `ConfigActor` handles first-activation empty state cleanly without falling prey to `dapr/go-sdk` `StateManagerContext.Get` tracker caching on missing keys or `Remove` skipping uncached keys.

## Requirements

### Functional Requirements

- **FR-001**: `internal/actor/config` MUST define `ActorType = "ConfigActor"`, `GlobalActorID = "global"`, `DefaultNamespace = "default"`, `Scope` (`ScopeGlobal`, `ScopeNamespace`, `ScopeRun`), `ValidateKeyName`, `NormalizeNamespace`, `EntryInput`, `EnvEntry`, `SecretMeta` (strictly without any value field), and single-parameter pointer request/response types for all 6 actor methods.
- **FR-002**: `ConfigActor` MUST embed `actor.ServerImplBaseCtx`, support optional `func() daprclient.Client` injection via `NewConfigActor` / `SetStateManager` (using `daprhost.NewActorStateManager` when a client is available while preserving direct in-memory `StateManagerContext` injection in Tier 1 unit tests), and implement `UpsertEntries`, `DeleteEntry`, `DeleteRunScope`, `ListMetadata`, `PreflightCheck`, and `ResolveForNode`.
- **FR-003**: Resolution precedence MUST be strictly `RunOverrides` (when provided to `PreflightCheck`/`ResolveForNode`) > `ScopeRun(RunID)` > `ScopeNamespace(normalizedNamespace)` > `ScopeGlobal`, with complete isolation across distinct namespaces and runs.
- **FR-004**: Same-scope writes MUST reject `Env` vs `Secret` kind conflicts without partial mutation, allow same-kind updates, and allow cross-scope overrides.
- **FR-005**: `ListMetadata` MUST never expose secret values; `PreflightCheck` MUST never return values (only `OK` and sorted deduplicated `MissingKeys`); `ResolveForNode` MUST only resolve and return values for the explicitly requested `Keys`.
- **FR-006**: `ExtractPlanEnvRefs` and `ExtractNodeEnvRefs` MUST recursively inspect root and nested child `Module.Plan` nodes (`source`, `model_provider`, `harness`, `mcp_server`, `external_tool`), extract host ref names (specifically values—never keys—of `env_refs`), safely handle `cty.NilVal`/`Null`/`Unknown`/sensitive marks, and return sorted deduplicated slices.
- **FR-007**: `NewClientStub(daprclient.Client) *ClientStub` MUST wire all 6 methods through `ImplActorClientStub` targeting `ConfigActor` / `"global"`.

## Success Criteria

### Measurable Outcomes

- **SC-001**: Tier 1 unit tests (`go test ./internal/actor/config/... -count=1`) and the entire workspace suite (`go test ./... -count=1`) pass offline with zero external daemons.
- **SC-002**: Tier 2 integration tests (`go test -v -tags=integration ./internal/actor/config/... -count=1`) pass against real local `scheduler` and `daprd` binaries, proving `ImplActorClientStub` RPC round-trips, encrypted `actor-state.db` persistence without plaintext secrets, and full 3-tier recovery across restarts with zero orphan processes.
- **SC-003**: `gofmt -l .` produces empty output and `go vet ./...` passes cleanly.
