# Feature Specification: AgentHarness Protocol, Buf Stubs, Public Plugin SDK & Containerized Go-Plugin Bridge (`014-harness-proto-and-goplugin-bridge`)

**Feature Branch**: `014-harness-proto-and-goplugin-bridge`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `014` blueprint (`docs/stage2/014-harness-proto-and-goplugin-bridge.md`), `docs/harness-plugin-contract.md`, `docs/stage2/README.md`, `docs/stage2/019-plugin-copilot.md`, `docs/stage2/020-plugin-antigravity.md`, `internal/sandbox/runtime/spec.go`, `internal/provider/config.go`, `internal/mcp/config.go`, `internal/harness/spec/schema.go`.

## User Scenarios & Testing

### User Story 1 - Canonical `harness.proto` & Multi-Language `buf` Codegen (`proto/`, `buf.yaml`, `buf.gen.yaml`, `pkg/harnessplugin/v1`) (Priority: P1)

`proto/sofa/harness/v1/harness.proto` defines the single canonical 7-RPC `AgentHarness` gRPC service (`GetCapabilities`, `OpenSession`, `RunTurn`, `AbortTurn`, `ResumeSession`, `ReadMCPResource`, `CloseSession`) with native bidirectional `RunTurn(stream HostTurnFrame) returns (stream PluginTurnFrame)` and zero HashiCorp-proprietary `GRPCBroker` sub-stream dependencies. Declarative `buf.yaml` and `buf.gen.yaml` generate and check in:
- Go protobuf and gRPC stubs under `pkg/harnessplugin/v1/` (`harness.pb.go`, `harness_grpc.pb.go`, `go_package = "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1;harnessv1"`);
- Python protobuf and gRPC stubs (`harness_pb2.py`, `harness_pb2_grpc.py`, and `__init__.py` package markers) under `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto/` for downstream Slice `020`.

**Independent Test**: `go build ./pkg/harnessplugin/...` and `go test ./... -count=1` succeed offline with zero external `buf` / `protoc` runtime dependencies.

### User Story 2 - Public Zero-Internal Go Plugin SDK (`pkg/harnessplugin/plugin.go`) (Priority: P1)

External Go harness plugins (such as `019` `sofa-harness-copilot` and Tier 1/2 `loopbackplugin`) depend exclusively on `pkg/harnessplugin` (zero imports of `internal/...`):
- Exports `Handshake` (`ProtocolVersion: 1`, `MagicCookieKey: "SOFA_HARNESS_PLUGIN"`, `MagicCookieValue: "sofa-harness-v1"`), `PluginName = "agent_harness"`, `GRPCPlugin`, `ScrubPluginEnv()`, and `Serve(impl harnessv1.AgentHarnessServer)`.
- `ScrubPluginEnv()` removes `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` environment variables from the plugin process immediately after handshake initialization so child processes (e.g., `bash`) never inherit plugin control variables or client certificates.
- `Serve` operates transparently across all three execution environments:
  1. Local host subprocess mode (Windows TCP `127.0.0.1` or Linux `/tmp` UDS);
  2. Linux container UDS mode (`PLUGIN_UNIX_SOCKET_DIR=/.sofa/plugin`);
  3. Non-Linux host container TCP mode (Linux container where `PLUGIN_UNIX_SOCKET_DIR` is unset and `PLUGIN_MIN_PORT == PLUGIN_MAX_PORT`, binding `0.0.0.0:<port>` with `AutoMTLS` client-cert verification, `grpc.health.v1.Health` `"plugin" -> SERVING`, and `base64.RawStdEncoding` DER server cert handshake line).

**Independent Test**: Tier 1 local subprocess `AutoMTLS` tests verify `SkipHostEnv: true` isolation, `PLUGIN_*`/`SOFA_HARNESS_PLUGIN` scrubbing inside RPC handlers, and rejection of unauthenticated/rogue TLS connections; Tier 2 Podman container tests verify `0.0.0.0:19500` port-mapped `AutoMTLS` serving inside `alpine:latest`.

### User Story 3 - Host Domain Abstractions, In-Memory Credential Materialization & Bidirectional `RunTurn` Client (`internal/harness/runtime/types.go`, `client.go`) (Priority: P1)

Upper orchestration packages (`015`–`018`) program exclusively against pure Go domain interfaces (`Opener`, `Session`) and domain structs in `internal/harness/runtime/types.go` with zero direct dependency on `harnessv1` protobuf types:
- In-memory credential materialization helpers resolve `provider.Config` (`api_key_ref`, `bearer_token_ref`, sensitive `headers`), `mcp.Config` (`http.bearer_token_ref`, `stdio.env_refs`), and `harness.HarnessConfig` (`env_refs`, `options`) via injectable `LookupEnv`, delivering secrets exclusively over `AutoMTLS` `OpenSession` RPC.
- `GRPCClientOpener` manages `plugin.Client` lifecycle (`Capabilities`, `Open`, `Close`), converting domain types to/from `harnessv1` and mapping `ErrorDetail` to `*HarnessError`.
- `GRPCSession` serializes active `RunTurn` streams per session via a dedicated turn mutex while allowing `Abort(ctx)` to invoke unary `AbortTurn` concurrently without blocking on the turn mutex; `RunTurn` processes `SessionEvent` synchronously via `onEvent`, handles `PreToolUseRequest` / `PostToolUseRequest` hooks, and dispatches `InvokeHostToolRequest` callbacks in `sync.WaitGroup`-tracked goroutines with a `sendMu` guarding `stream.Send`, supporting parallel host tool invocations in a single turn.

**Independent Test**: Tier 1 tests in `internal/harness/runtime/bridge_test.go` launch `testdata/loopbackplugin` as a local `AutoMTLS` subprocess (`SkipHostEnv: true`) and exercise all 7 RPCs, concurrent `InvokeHostTool` calls via barrier synchronization, `PreToolUse` allow/deny + modified args + additional context, `PostToolUse` failure notification, `AbortTurn` preemption, `*HarnessError` propagation, env scrubbing, and rogue TLS rejection.

### User Story 4 - Container `RunnerFunc` & `AddrTranslator` (`internal/harness/runtime/container_runner.go`, `internal/sandbox/runtime/spec.go`) (Priority: P1)

`ContainerRunner` implements `github.com/hashicorp/go-plugin/runner.Runner` (`Start`, `Wait`, `Kill`, `ID`, `Name`, `Diagnose`, `Stdout`, `Stderr`, `PluginToHost`, `HostToPlugin`) backed by `internal/sandbox/runtime.BuildRunArgs`:
- `SessionMountLayout` gains a strictly validated `HandshakeEnv []string` field that permits ONLY `SOFA_HARNESS_PLUGIN` and `PLUGIN_*` keys (rejecting any business secret keys such as `OPENAI_API_KEY` or `GITHUB_TOKEN` in `BuildRunArgs`).
- `NewClientConfig` sets `SkipHostEnv: true`, `AutoMTLS: true`, and `AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC}`.
- On Linux hosts (`HostOS == "linux"`), mounts `go-plugin`'s `0700` `tmpDir` at `/.sofa/plugin:rw`, rewrites `PLUGIN_UNIX_SOCKET_DIR=/.sofa/plugin`, and translates `("unix", "/.sofa/plugin/<sock>")` to `("unix", filepath.Join(tmpDir, "<sock>"))`.
- On non-Linux hosts (`HostOS != "linux"`), strips `PLUGIN_UNIX_SOCKET_DIR`, pins `PLUGIN_MIN_PORT=19500` & `PLUGIN_MAX_PORT=19500`, sets `PublishPluginPort: true` (`-p 127.0.0.1::19500`), assigns a unique `--name`, and translates `("tcp", pluginAddr)` via `podman port <containerName> 19500`.
- `ID()` returns the unique container name for `PodmanEngine.Exec` (`015`), and `Kill(ctx)` runs `podman rm -f -t 2 <containerName>` so zero orphan containers remain.

**Independent Test**: Tier 1 tests validate `HandshakeEnv` key enforcement and Linux/Windows arg translation; Tier 2 real Podman integration tests (`bridge_integration_test.go`, `//go:build integration`) cross-compile `testdata/loopbackplugin` (`CGO_ENABLED=0 GOOS=linux`), mount it `:ro` into `alpine:latest` via `ContainerRunner`, verify `podman inspect` contains zero business secrets, run `Capabilities`/`Open`/`RunTurn` (with host tool callback), and assert the container is removed after `Close`.

### Edge Cases

- `plugin.Serve` in `hashicorp/go-plugin@v1.8.0` ignores `PLUGIN_MIN_PORT`/`PLUGIN_MAX_PORT` on `runtime.GOOS == "linux"` and always binds a Unix socket in `/tmp` when `PLUGIN_UNIX_SOCKET_DIR` is empty; `pkg/harnessplugin.Serve` MUST explicitly handle container TCP mode (`PLUGIN_UNIX_SOCKET_DIR == ""` and `PLUGIN_MIN_PORT == PLUGIN_MAX_PORT > 0`) by binding `0.0.0.0:<port>` with `AutoMTLS` and emitting unpadded `base64.RawStdEncoding` DER server cert bytes.
- `go-plugin`'s `Client.Start()` cancels `startCtx` immediately after `runner.Start(startCtx)` returns; `ContainerRunner.Start` MUST NOT bind the long-lived container CLI process to `startCtx` via `exec.CommandContext`, and instead manages container termination via `Kill(ctx)` (`podman rm -f -t 2`).
- `ScrubPluginEnv()` inside `pkg/harnessplugin.Serve` MUST run only AFTER `go-plugin` reads `SOFA_HARNESS_PLUGIN`, `PLUGIN_CLIENT_CERT`, `PLUGIN_PROTOCOL_VERSIONS`, and listener env vars, and BEFORE any RPC handler executes.
- `RunTurn` must `wg.Wait()` on all in-flight `InvokeHostTool` / hook goroutines before closing the send stream or returning, and must never deadlock if `stream.Recv()` fails due to `AbortTurn`.

## Requirements

- **FR-001**: `proto/sofa/harness/v1/harness.proto` MUST match `docs/harness-plugin-contract.md` §3 verbatim, with `buf.yaml` and `buf.gen.yaml` generating checked-in Go stubs in `pkg/harnessplugin/v1/` and Python stubs in `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto/`.
- **FR-002**: `pkg/harnessplugin/plugin.go` MUST import zero `internal/...` packages and export `Handshake`, `PluginName`, `GRPCPlugin`, `ScrubPluginEnv`, and `Serve`.
- **FR-003**: `internal/sandbox/runtime/spec.go` MUST preserve the zero-secret-env invariant while supporting `HandshakeEnv []string` validated strictly to `SOFA_HARNESS_PLUGIN` and `PLUGIN_*`.
- **FR-004**: `internal/harness/runtime/types.go` MUST define `Opener`, `Session`, `Capabilities`, `SessionConfig`, `Event`, `TurnSummary`, `ToolInvocation`, `ToolOutput`, `ToolHandler`, `PreToolUseInput`, `PreToolUseOutput`, `PreToolUseHook`, `PostToolUseInput`, `PostToolUseHook`, `MCPResourceContent`, `ErrorCategory`, and `*HarnessError` with zero imports of `harnessv1`.
- **FR-005**: `internal/harness/runtime/container_runner.go` MUST implement `runner.Runner` and `runner.AddrTranslator` for both Linux UDS and non-Linux `-p 127.0.0.1::19500` modes with `SkipHostEnv: true`, `AutoMTLS: true`, unique container naming, and guaranteed `podman rm -f -t 2` cleanup.
- **FR-006**: `internal/harness/runtime/client.go` MUST provide in-memory credential materialization helpers, `GRPCClientOpener`, non-blocking unary `Abort`, and concurrent `RunTurn` stream dispatching with `sendMu` and `sync.WaitGroup`.

## Success Criteria

- **SC-001**: `gofmt -l .` produces empty output.
- **SC-002**: `go vet ./...` passes with zero diagnostics.
- **SC-003**: `go test ./... -count=1` passes offline with zero external daemon dependencies.
- **SC-004**: `go test -tags=integration ./internal/harness/runtime/... -count=1` passes against real local Podman (`alpine:latest`) with cross-compiled Linux `loopbackplugin`.
