# Implementation Plan: Three-Tier Managed Env / Secret ConfigActor & `.sofaplan` Preflight Scanner (`023-config-actor-env-secrets`)

**Branch**: `023-config-actor-env-secrets` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 3 Slice `023`: implement `internal/actor/config` providing:
1. Three-tier (`Run > Namespace > Global`) managed `Env` and `Secret` domain types with key validation (`^[A-Za-z_][A-Za-z0-9_]*$`), namespace normalization (`"" -> "default"`), same-scope `Env`/`Secret` mutual exclusion, and strictly write-only `SecretMeta` (`types.go`).
2. Pure recursive `.sofaplan` host environment variable scanner (`ExtractPlanEnvRefs` and `ExtractNodeEnvRefs`) supporting nested `Module.Plan` hierarchies, `source`, `model_provider`, `harness`, `mcp_server` (`http` + `stdio`), `external_tool`, and minimal per-node dependency resolution (`plan_refs.go`).
3. `ConfigActor` (`ActorType = "ConfigActor"`, `GlobalActorID = "global"`) embedding `actor.ServerImplBaseCtx` with optional `daprhost.NewActorStateManager` dynamic client binding and `ClientStub` (`ImplActorClientStub`) wrapper (`actor.go`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **Primary Dependencies**:
  - `github.com/dapr/go-sdk` `v1.16.0` (`actor`, `client`)
  - `github.com/zclconf/go-cty/cty` (inspecting `plan.NodeSpec.Config` safely across `UnmarkDeep`, `Null`, `Unknown`, and `DynamicVal`)
  - `github.com/lonegunmanb/sofa/internal/daprhost` (Slice `022` `Manager` and `NewActorStateManager`)
  - `github.com/lonegunmanb/sofa/internal/plan` (immutable `Plan`, `NodeSpec`, `ModuleSpec`, `ToolSpec`)
- **Testing**:
  - Tier 1: `internal/actor/config/config_test.go` (in-memory `actor.StateManagerContext` + real `internal/planner` / `plan.Plan` extraction tests)
  - Tier 2: `internal/actor/config/config_integration_test.go` (`//go:build integration`, real `daprhost.Manager` + `ImplActorClientStub` + SQLite encryption & restart recovery verification)

## Constitution Check

- **Principle I (Spec-First)**: Full Spec Kit artifacts (`spec.md`, `plan.md`, `research.md`, `data-model.md`, `quickstart.md`, `contracts/config-actor.md`, `checklists/requirements.md`, `tasks.md`) created before tests and production code.
- **Principle II (Strict TDD)**: Failing Red tests in `config_test.go` and `config_integration_test.go` written and confirmed failing before implementing `types.go`, `plan_refs.go`, and `actor.go`.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: Pure `plan_refs.go` scanning and `ConfigActor` state logic tested offline via in-memory `StateManagerContext` in Tier 1; real `daprd` + encrypted `state.sqlite` + `ImplActorClientStub` transport verified in Tier 2.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Secrets never leave `ConfigActor` via `ListMetadata` or `PreflightCheck`; `ResolveForNode` strictly filters to requested `Keys`; runtime does not fall back to worker `os.Environ()`.
- **Principle V (Single-Node & Distributed Parity)**: `ConfigActor("global")` uses standard `dapr/go-sdk/actor` state semantics compatible with both local `daprhost` SQLite and distributed Dapr state stores.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `internal/planner/export.go`, `internal/source/schema.go`, `internal/provider/schema.go`, `internal/harness/spec/schema.go`, `internal/mcp/spec/schema.go`, `internal/tool/spec/schema.go`, `internal/harness/runtime/client.go`, `internal/tool/runtime/executor.go`, `internal/daprhost/manager.go`, and `github.com/dapr/go-sdk@v1.16.0` (`actor/manager/container.go`, `actor/manager/manager.go`, `actor/state/state_manager.go`, `actor/state/state_async_provider.go`, `client/actor.go`).

## Project Structure

```text
specs/023-config-actor-env-secrets/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── config-actor.md
├── checklists/
│   └── requirements.md
└── tasks.md

internal/actor/config/
├── types.go                   # Scopes, key validation, EnvEntry, SecretMeta, RPC Req/Resp types
├── plan_refs.go               # ExtractPlanEnvRefs & ExtractNodeEnvRefs recursive .sofaplan scanners
├── actor.go                   # ConfigActor ("ConfigActor"/"global"), state helpers, ClientStub
├── config_test.go             # Tier 1 pure unit tests (Red -> Green)
└── config_integration_test.go # Tier 2 real Dapr encrypted persistence & restart tests
```

## Complexity Tracking

No Constitution violations.
