# Implementation Plan: Isolated In-Repo `sofa-harness-copilot` Plugin (`019-plugin-copilot`)

**Branch**: `019-plugin-copilot` | **Date**: 2026-11-27 | **Spec**: [spec.md](file:///d:/project/sofa/specs/019-plugin-copilot/spec.md)

## Summary

Implement `sofa-harness-copilot` inside an isolated Go sub-module `plugins/sofa-harness-copilot/` (`github.com/lonegunmanb/sofa-harness-copilot`) wrapping `github.com/github/copilot-sdk/go v1.0.19` behind the canonical 7-RPC `harnessv1.AgentHarnessServer` contract (`pkg/harnessplugin`). Add a self-contained Linux bundle script (`scripts/build-bundle.go`) and a Tier 2 Podman container end-to-end integration test (`internal/worker/runner/copilot_plugin_integration_test.go`) driving the real `copilot-sdk/go` client against a minimal JSON-RPC stdio `bin/copilot` stub through `018` `WorkerRunner.Run`.

## Technical Context

**Language/Version**: Go 1.26+ / 1.27 (`plugins/sofa-harness-copilot/go.mod`, root `go.mod` untouched)
**Primary Dependencies**: `github.com/github/copilot-sdk/go v1.0.19`, `github.com/lonegunmanb/sofa/pkg/harnessplugin` (+ `v1`), `github.com/google/uuid`
**Testing**: Tier 1 offline unit tests (`plugins/sofa-harness-copilot/internal/server/server_test.go`) with injected `sdkClient`/`sdkSession` mocks; Tier 2 real Podman + Git integration test (`internal/worker/runner/copilot_plugin_integration_test.go`, `//go:build integration`)
**Target Platform**: Linux container (`linux/amd64`, `linux/arm64`) cross-compiled from Windows/macOS/Linux host

## Constitution Check

*GATE: Verified against `.specify/memory/constitution.md` v1.5.1:*

- **Principle I (Spec-First Delivery)**: Complete `specs/019-plugin-copilot/` artifacts authored before implementation.
- **Principle II (Strict TDD)**: Failing tests in `server_test.go` and `copilot_plugin_integration_test.go` authored before production implementations (`Red -> Green -> Refactor`).
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `sdkClient`/`sdkSession` interfaces isolate `copilot-sdk/go` for fast Tier 1 tests; Tier 2 verifies real `AutoMTLS` `go-plugin` inside Podman `alpine:latest` and real Git bare repos.
- **Principle IV (Runtime Enforcement)**: `hostToolNames` first-line hook check prevents double quota accounting; `HarnessEnv` stays scoped to `StdioConnection.Env`; `PLUGIN_*` env scrubbed by `harnessplugin.Serve`; `WorkerRunner` enforces `writable_paths`, Gatekeeper, and read-only QC.
- **Principle VI (Research-First & Surgical Sizing)**: Inspected `copilot-sdk/go@v1.0.19` (`types.go`, `client.go`, `session.go`, `rpc/zsession_events.go`, `rpc/zrpc.go`, `internal/jsonrpc2/frame.go`), `r42/internal/copilot`, `pkg/harnessplugin/plugin.go`, `internal/harness/installer/installer.go`, and `internal/worker/runner/` before writing spec or code. Scope is strictly `019-plugin-copilot` only.

## Project Structure

```text
specs/019-plugin-copilot/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/copilot-plugin.md
├── checklists/requirements.md
└── tasks.md

plugins/sofa-harness-copilot/
├── go.mod
├── go.sum
├── cmd/sofa-harness-copilot/
│   └── main.go
├── internal/server/
│   ├── sdk_iface.go
│   ├── errors.go
│   ├── server.go
│   └── server_test.go
└── scripts/
    └── build-bundle.go

internal/worker/runner/
├── copilot_plugin_integration_test.go   # //go:build integration
└── testdata/fakecopilot/
    └── main.go                          # Stdlib-only JSON-RPC 2.0 stdio stub for copilot CLI
```
