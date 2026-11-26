# Contract: AgentHarness Protocol, Public Plugin SDK & Containerized Go-Plugin Bridge (`014`)

## 1. Protocol & Codegen (`proto/sofa/harness/v1/harness.proto`, `buf.yaml`, `buf.gen.yaml`)

- Defines `package sofa.harness.v1;` with `option go_package = "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1;harnessv1";`.
- Service `AgentHarness` exposes exactly 7 RPCs: `GetCapabilities`, `OpenSession`, `RunTurn(stream HostTurnFrame) returns (stream PluginTurnFrame)`, `AbortTurn`, `ResumeSession`, `ReadMCPResource`, `CloseSession`.
- `buf.gen.yaml` emits checked-in Go stubs under `pkg/harnessplugin/v1/` and Python stubs under `plugins/sofa-harness-antigravity/src/sofa_harness_antigravity/proto/`.

## 2. Public Go Plugin SDK (`pkg/harnessplugin`)

- Zero imports of `internal/...`.
- `Handshake`: `ProtocolVersion: 1`, `MagicCookieKey: "SOFA_HARNESS_PLUGIN"`, `MagicCookieValue: "sofa-harness-v1"`.
- `PluginName = "agent_harness"`.
- `ScrubPluginEnv()`: removes `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` env vars from `os.Environ()`.
- `Serve(impl harnessv1.AgentHarnessServer)`:
  - Standard mode (`PLUGIN_UNIX_SOCKET_DIR != ""` or `PLUGIN_MIN_PORT != PLUGIN_MAX_PORT`): delegates to `plugin.Serve` with `ScrubPluginEnv()` invoked inside `GRPCServer` factory.
  - Container TCP mode (`PLUGIN_UNIX_SOCKET_DIR == ""` and `PLUGIN_MIN_PORT == PLUGIN_MAX_PORT > 0`): validates magic cookie, reads `PLUGIN_CLIENT_CERT` and `PLUGIN_MIN_PORT`, immediately invokes `ScrubPluginEnv()`, binds `0.0.0.0:<port>` with mutual TLS (`ClientAuth: tls.RequireAndVerifyClientCert`, `ServerName: "localhost"`), registers `grpc_health_v1` (`"plugin" -> SERVING`), reflection, and `AgentHarnessServer`, prints `1|1|tcp|<addr>|grpc|<rawStdBase64DER>\n`, and handles `SIGINT`/`SIGTERM`.

## 3. Sandbox `HandshakeEnv` Guard (`internal/sandbox/runtime`)

- `SessionMountLayout.HandshakeEnv []string` permits only `SOFA_HARNESS_PLUGIN` and `PLUGIN_*` keys; any other key or malformed entry fails `BuildRunArgs`.

## 4. Host Domain & Container Bridge (`internal/harness/runtime`)

- `types.go` imports zero `harnessv1` types; defines `Opener`, `Session`, `Capabilities`, `SessionConfig`, `Event`, `TurnSummary`, `ToolHandler`, `PreToolUseHook`, `PostToolUseHook`, `*HarnessError`, etc.
- `container_runner.go` configures `plugin.ClientConfig` with `SkipHostEnv: true`, `AutoMTLS: true`, `AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC}`, `Detached: false`, `ForAgentSession: true`:
  - `HostOS == "linux"`: mounts `tmpDir` at `/.sofa/plugin:rw`, rewrites `PLUGIN_UNIX_SOCKET_DIR=/.sofa/plugin`, translates `("unix", "/.sofa/plugin/<sock>") -> ("unix", filepath.Join(tmpDir, "<sock>"))`.
  - `HostOS != "linux"`: strips `PLUGIN_UNIX_SOCKET_DIR`, pins `PLUGIN_MIN_PORT=19500`, `PLUGIN_MAX_PORT=19500`, sets `PublishPluginPort: true` (`-p 127.0.0.1::19500`), sets unique `--name`, translates `("tcp", pluginAddr)` via `<runtimeBin> port <containerName> 19500`.
  - `Kill(ctx)` removes the container via `<runtimeBin> rm -f -t 2 <containerName>`.
- `client.go`:
  - Materializes `ProviderConfig`, `MCPServerConfig`, `HarnessEnv`, and `HarnessOptionsJSON` in memory via injectable `LookupEnv`.
  - Serializes `RunTurn` per session while `Abort(ctx)` runs unary `AbortTurn` without blocking on the turn mutex.
  - Processes `SessionEvent` synchronously via `onEvent`; runs hooks and `InvokeHostTool` callbacks concurrently with `sendMu` guarding `stream.Send` and `sync.WaitGroup` drained prior to `CloseSend()` and return.
