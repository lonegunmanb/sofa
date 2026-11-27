---
description: "Stage 3 Slice 023 TDD tasks: Three-tier managed Env/Secret ConfigActor & .sofaplan preflight scanner (internal/actor/config)"
---

# Tasks

- [X] T001 [Red] Write failing Tier 1 unit tests in `internal/actor/config/config_test.go` covering:
  - `Run > Namespace("proj-a") > Global` inheritance/override order, default `"default"` namespace normalization, and strict isolation from `Namespace("proj-b")`;
  - Key name regex (`^[A-Za-z_][A-Za-z0-9_]*$`) rejection, invalid scope/run_id rejection, bidirectional same-scope `Env` vs `Secret` conflict rejection, same-kind overwrite success, `DeleteEntry` re-typing, `DeleteRunScope` cleanup, and `ListMetadata` zero-secret-plaintext guarantee;
  - `PreflightCheck` (with `RunOverrides`, persisted `Run`, `Namespace`, `Global`) returning sorted deduplicated `MissingKeys`, and `ResolveForNode` returning only requested `Keys`;
  - `ExtractPlanEnvRefs` & `ExtractNodeEnvRefs` over a real `planner`-compiled `plan.Plan` (plus `external_tool` `env_refs` and nested child `module`s) verifying 100% accurate extraction of `source`, `model_provider`, `harness`, `mcp_server` (`http` + `stdio`), and `external_tool` host refs without confusing `env_refs` container keys as host refs or leaking unrelated node refs.
- [X] T002 [Green] Implement `internal/actor/config/types.go`, `internal/actor/config/plan_refs.go`, and `internal/actor/config/actor.go`, and verify `go test ./internal/actor/config/... -count=1` passes.
- [X] T003 [Red -> Green] Write Tier 2 real-Dapr integration test `internal/actor/config/config_integration_test.go` (`//go:build integration`) using `daprhost.Manager`:
  - Register `ConfigActor` on an authenticated loopback callback server, start `Manager`, invoke `UpsertEntries` across `Global`, `Namespace`, and `Run` (including `"ULTRA_SECRET_TOKEN_778899"`), `ListMetadata`, `PreflightCheck`, `ResolveForNode`, and `DeleteRunScope` via `NewClientStub` (`ImplActorClientStub`);
  - Stop `Manager` and assert raw `<stateDir>/actor-state.db` (and `-wal` if present) bytes do NOT contain `"ULTRA_SECRET_TOKEN_778899"`;
  - Restart `Manager` on the same `StateDir` and assert all three tiers of `Env` and `Secret` state recover identically via `NewClientStub`.
  - Run `go test -v -tags=integration ./internal/actor/config/... -count=1`.
- [X] T004 [Refactor & Verify] Review code for cleanliness and zero duplication, run `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -v -tags=integration ./internal/actor/config/... -count=1`, verify zero orphan `scheduler`/`daprd` processes, and mark all tasks `[X]`.
