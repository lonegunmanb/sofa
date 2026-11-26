# Implementation Plan: AgentHarness Protocol, Buf Stubs, Public Plugin SDK & Containerized Go-Plugin Bridge (`014-harness-proto-and-goplugin-bridge`)

**Branch**: `014-harness-proto-and-goplugin-bridge` | **Date**: 2026-11-27 | **Spec**: [spec.md](./spec.md)

## Summary

Deliver Stage 2 Slice `014`:
1. Author `proto/sofa/harness/v1/harness.proto` (matching `docs/harness-plugin-contract.md` §3 verbatim), `buf.yaml`, and `buf.gen.yaml`, and run `buf generate` to emit and commit Go stubs (`pkg/harnessplugin/v1/`) and Python stubs (`plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto/`).
2. Implement public Go SDK `pkg/harnessplugin/plugin.go` (`Handshake`, `PluginName`, `GRPCPlugin`, `ScrubPluginEnv()`, `Serve`) with zero `internal/` imports, supporting local subprocess mode, Linux container UDS mode, and non-Linux container TCP `0.0.0.0:19500` `AutoMTLS` mode.
3. Extend `internal/sandbox/runtime.SessionMountLayout` with strictly validated `HandshakeEnv []string` (`SOFA_HARNESS_PLUGIN` and `PLUGIN_*` only).
4. Implement `internal/harness/runtime` (`types.go`, `container_runner.go`, `client.go`) providing pure Go domain interfaces (`Opener`, `Session`), `ContainerRunner` (`runner.Runner` + `runner.AddrTranslator`), in-memory credential materialization, `GRPCClientOpener`, and concurrent `RunTurn` stream dispatch.
5. Verify via Red -> Green TDD with Tier 1 local subprocess `loopbackplugin` tests (`bridge_test.go`) and Tier 2 real Podman `alpine:latest` integration tests (`bridge_integration_test.go`).

## Technical Context

- **Language/Version**: Go 1.27.3 (`github.com/lonegunmanb/sofa`)
- **New Direct Dependencies**:
  - `github.com/hashicorp/go-plugin` (`v1.8.0`)
  - `google.golang.org/grpc` (`v1.85.0`)
  - `google.golang.org/protobuf` (`v1.36.12`)
- **Existing Packages Reused**:
  - `internal/sandbox/runtime` (`SessionMountLayout`, `BuildRunArgs`, `ContainerPluginPort`)
  - `internal/provider` (`Config`, `Type`, `WireAPI`, `Transport`)
  - `internal/mcp` (`Config`, `HTTPConfig`, `StdioConfig`, `Transport`)
  - `internal/harness/spec` (`HarnessConfig`)
  - `github.com/google/uuid`, `github.com/hashicorp/go-hclog`, `github.com/zclconf/go-cty`
- **Codegen Tooling**: `go run github.com/bufbuild/buf/cmd/buf@latest generate` (generated files checked into repo so `go test ./...` requires zero codegen tools).
- **Testing**: Tier 1 offline unit/subprocess tests (`go test ./... -count=1`) + Tier 2 real Podman container tests (`go test -tags=integration ./internal/harness/runtime/... -count=1`).

## Constitution Check

- **Principle I (Spec-First)**: Complete Spec Kit artifacts under `specs/014-harness-proto-and-goplugin-bridge/` authored prior to Go/proto production code.
- **Principle II (Strict TDD)**: Failing Tier 1 (`bridge_test.go`, `podman_test.go` extension) and Tier 2 (`bridge_integration_test.go`) tests written and verified Red before production implementation.
- **Principle III (Pure Core & Real-Dependency Boundaries)**: `internal/harness/runtime/types.go` defines pure Go `Opener` and `Session` interfaces free of gRPC/Protobuf types; `ContainerRunner` and `pkg/harnessplugin` are verified against real `hashicorp/go-plugin` subprocesses and real Podman `alpine:latest` containers.
- **Principle IV (Runtime Enforcement Over Prompt Trust)**: Enforces `SkipHostEnv: true`, `AutoMTLS: true` mutual TLS client+server cert authentication, `HandshakeEnv` key allowlist (`SOFA_HARNESS_PLUGIN` / `PLUGIN_*` only), immediate `ScrubPluginEnv()` inside plugin processes, and in-memory secret delivery via `OpenSessionRequest`.
- **Principle VI (Research-First & Surgical Task Sizing)**: Inspected `github.com/hashicorp/go-plugin@v1.8.0` source (`client.go`, `server.go`, `mtls.go`, `grpc_server.go`, `grpc_client.go`, `runner/runner.go`), uncovering Linux `serverListener` UDS behavior, `startCtx` lifetime, and `base64.RawStdEncoding` unpadded DER requirement before designing. Scoped strictly to Slice `014`.

## Files

```text
buf.yaml
buf.gen.yaml
proto/sofa/harness/v1/harness.proto
pkg/harnessplugin/v1/harness.pb.go
pkg/harnessplugin/v1/harness_grpc.pb.go
pkg/harnessplugin/plugin.go
plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto/...
internal/sandbox/runtime/spec.go
internal/sandbox/runtime/podman_test.go
internal/harness/runtime/types.go
internal/harness/runtime/container_runner.go
internal/harness/runtime/client.go
internal/harness/runtime/testdata/loopbackplugin/main.go
internal/harness/runtime/bridge_test.go
internal/harness/runtime/bridge_integration_test.go
specs/014-harness-proto-and-goplugin-bridge/...
```

## Post-Design Check

All Constitution I–VI checks pass without exceptions.
