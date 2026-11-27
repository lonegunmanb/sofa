# Implementation Plan: Dual-Port REST/WebSocket API Server & CSP Preview Gateway (`029-rest-ws-api-server`)

**Branch**: `029-rest-ws-api-server` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Implement `internal/server` to expose the unified Control Plane REST JSON + WebSocket management API and zero-trust artifact preview gateway:
- `internal/server/auth.go`: Startup non-loopback/tokenless refusal (`New`), constant-time `Authorization: Bearer` middleware, dual-mode WebSocket auth (`Authorization: Bearer` + `Sec-WebSocket-Protocol: sofa.v1, sofa.bearer.<b64url>`), always-on strict anti-CSRF `Origin` validation (rejecting `Origin: null` and host mismatches with `403` even in `--dev` mode), and short-lived HMAC-SHA256 preview tickets scoped to `(run_id, artifact_id)`.
- `internal/server/ws_hub.go`: Thread-safe `WSHub` with bounded non-blocking per-client channels, global and `run_id`-filtered subscriptions, and Dapr PubSub topic handlers for `"sofa-events"` (`daemon.EventsTopic`) and `"sofa.run.events"` (`run.DefaultRunEventsTopic`) on `"sofa-pubsub"` (`daprhost.PubSubComponentName`).
- `internal/server/handlers.go` & `internal/server/server.go`: Dual-port physical isolation (internal `daprhttp.NewServiceWithMux` with `APP_API_TOKEN` middleware vs public `chi.Router` with zero `/actors/*` or `/dapr/*` routes), clean injectable backend interfaces + Dapr-backed default implementation, all `/api/v1/*` routes, second-0 `ConfigActor.PreflightCheck` (`422` + `missing_keys`), recursive `<sensitive>` output masking (`?reveal_sensitive=true`), write-only `/api/v1/secrets`, `409 Conflict` mapping, and forced CSP + `X-Content-Type-Options: nosniff` headers on artifact preview.
- `internal/server/server_test.go` & `internal/server/server_integration_test.go`: Tier 1 TDD unit tests and Tier 2 real `daprhost.Manager` + `WorkerDaemon` + bare Git repo integration test (`//go:build integration`).

## Technical Context

**Language/Version**: Go 1.27 (`module github.com/lonegunmanb/sofa`)  
**Primary Dependencies**: `github.com/go-chi/chi/v5`, `github.com/coder/websocket@v1.8.14`, `github.com/dapr/go-sdk@v1.16.0`, `github.com/lonegunmanb/golden`, `github.com/zclconf/go-cty`, `internal/actor/{run,node,config,registry,merge}`, `internal/worker/daemon`, `internal/daprhost`, `internal/plan`, `internal/blob`, `internal/artifact`, `internal/spec`.  
**Storage**: Delegated to Control Plane Actors (Dapr Actor State) and `blob.Store`.  
**Testing**: `go test -v ./internal/server/... -count=1` (Tier 1) and `go test -v -tags=integration ./internal/server/... -count=1` (Tier 2).  
**Constraints**: PowerShell 5.1 (no `&&`); zero `/actors/*` or `/dapr/*` routes on public port; strict `Origin` guard active even in `--dev` tokenless loopback mode; no `git commit` or `git push`.

## Constitution Check

- **I. Spec-First Delivery**: Full Spec Kit suite created under `specs/029-rest-ws-api-server/` prior to test/production code. PASS.
- **II. Strict TDD**: `tasks.md` mandates failing Tier 1 tests in `server_test.go` before implementing `auth.go`, `ws_hub.go`, `handlers.go`, and `server.go`. PASS.
- **III. Pure Core & Real-Dependency Boundaries**: Handlers depend on clean Go backend interfaces (`Backend` / `OperatorBackend`, `RunBackend`, `NodeBackend`, `ConfigBackend`) so Tier 1 runs in-memory without `daprd`, while `DaprBackend` wires real `daprclient.Client` stubs and `server_integration_test.go` exercises real `daprhost.Manager`. PASS.
- **IV. Runtime Enforcement Over Prompt Trust**: Enforces startup non-loopback refusal, constant-time token checks, strict `Origin` anti-CSRF checks, second-0 preflight gate, write-only secrets, recursive `<sensitive>` masking, and sandboxed CSP artifact headers deterministically in Go middleware/handlers. PASS.
- **V. Single-Node & Distributed Parity**: Public REST + WebSocket API is identical across `--dev` all-in-one and distributed deployments. PASS.
- **VI. Research-First & Surgical Task Sizing**: Based on direct inspection of all existing Stage 3 actor contracts, `daemon.go`, `daprhost`, `artifact/service.go`, and `plan/archive.go`; scoped strictly to `internal/server/`. PASS.

## Project Structure

```text
specs/029-rest-ws-api-server/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── checklists/requirements.md
├── contracts/rest-ws-api.md
└── tasks.md

internal/server/
├── auth.go
├── ws_hub.go
├── handlers.go
├── server.go
├── server_test.go
└── server_integration_test.go
```
