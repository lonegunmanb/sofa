# Research: AgentHarness Protocol, Buf Stubs & Containerized Go-Plugin Bridge (`014`)

## 1. Upstream `github.com/hashicorp/go-plugin@v1.8.0` Source Inspection

Inspected `client.go`, `server.go`, `mtls.go`, `grpc_server.go`, `grpc_client.go`, `grpc_stdio.go`, `grpc_controller.go`, `constants.go`, and `runner/runner.go` in Go module cache:

1. **Linux Container TCP Listener Trap (`server.go` lines 528–576)**:
   - `go-plugin`'s `serverListener` checks `if runtime.GOOS == "windows" { return serverListener_tcp() }` and otherwise **unconditionally** calls `serverListener_unix(unixSocketCfg)`.
   - Inside a Linux container launched from a Windows or macOS host (`PLUGIN_UNIX_SOCKET_DIR` stripped, `PLUGIN_MIN_PORT=19500`, `PLUGIN_MAX_PORT=19500`), `runtime.GOOS` is `"linux"`, so calling `plugin.Serve` directly would bind a Unix socket in `/tmp` instead of TCP!
   - Furthermore, `serverListener_tcp()` binds `127.0.0.1:<port>`, which inside a container is unreachable via Podman `-p 127.0.0.1::19500` port forwarding (Podman forwards to the container's external interface / `0.0.0.0:19500`).
   - **Resolution**: `pkg/harnessplugin.Serve` detects container TCP mode (`os.Getenv("PLUGIN_UNIX_SOCKET_DIR") == ""` and `PLUGIN_MIN_PORT == PLUGIN_MAX_PORT` > 0) and runs a self-contained `AutoMTLS` gRPC server bound to `0.0.0.0:<port>` with `grpc.health.v1.Health` (`"plugin"` -> `SERVING`), `reflection`, and `AgentHarnessServer`. For local subprocess tests (`PLUGIN_MIN_PORT != PLUGIN_MAX_PORT`) and Linux container UDS mode (`PLUGIN_UNIX_SOCKET_DIR != ""`), it delegates directly to `plugin.Serve`.

2. **Server Cert DER Base64 Encoding (`client.go` line 955 & `server.go` line 339)**:
   - `go-plugin` decodes the 6th handshake segment using `base64.RawStdEncoding.DecodeString(cert)` (unpadded Base64 of raw leaf DER bytes).
   - `Client.Start()` configures `TLSConfig` with `ServerName: "localhost"`, so the server's ephemeral self-signed certificate MUST include `"localhost"` in `DNSNames` (plus `127.0.0.1` in `IPAddresses` for completeness) and `ExtKeyUsageServerAuth | ExtKeyUsageClientAuth`.

3. **`AutoMTLS` + `SkipHostEnv` + Env Scrubbing Order (`client.go` lines 638–723, `server.go` lines 259–400)**:
   - With `SkipHostEnv: true`, `Client.Start()` populates `cmd.Env` exclusively with `MagicCookieKey=MagicCookieValue`, `PLUGIN_MIN_PORT`, `PLUGIN_MAX_PORT`, `PLUGIN_PROTOCOL_VERSIONS`, `PLUGIN_CLIENT_CERT` (when `AutoMTLS: true`), and `PLUGIN_UNIX_SOCKET_DIR` (when `RunnerFunc != nil`).
   - Calling `ScrubPluginEnv()` inside `ServeConfig.GRPCServer` (or immediately after reading env vars in container TCP mode) ensures `go-plugin` reads all handshake/mTLS variables first while guaranteeing all `PLUGIN_*` and `SOFA_HARNESS_PLUGIN` keys are unset before any RPC handler executes.

4. **`RunnerFunc` `startCtx` Lifetime (`client.go` lines 733–738)**:
   - `Client.Start()` creates `startCtx` with `StartTimeout` and cancels it via `defer startCtxCancel()` as soon as `runner.Start(startCtx)` returns.
   - **Resolution**: `ContainerRunner.Start(startCtx)` must start `podman run` without `exec.CommandContext(startCtx, ...)` (so cancelling `startCtx` after start does not kill the running container); lifecycle cleanup happens via `Kill(ctx)` executing `podman rm -f -t 2 <containerName>`.

5. **Non-Go Plugin Compatibility (`grpc_client.go` lines 76–133)**:
   - `newGRPCStdioClient` ignores `codes.Unavailable` / `codes.Unimplemented`; `GRPCClient.Close()` ignores `controller.Shutdown` errors; `Ping()` requires `grpc.health.v1.Health` service `"plugin"` to report `SERVING`.

## 2. Existing Repository Packages Inspected

- **`internal/sandbox/runtime/spec.go` & `podman.go`**:
  - `SessionMountLayout` and `BuildRunArgs` enforce zero secret env vars (`HOME`, `TMPDIR`, `GIT_OPTIONAL_LOCKS` only), `ContainerPluginPort = 19500`, `-p 127.0.0.1::19500` when `PublishPluginPort == true`, `--name`, `--entrypoint`, and `/.sofa/{harness,plugin,tools,session-store,...}` mounts.
  - `BuildRunArgs` already rejects `ForAgentSession && HostOS != "linux" && Sandbox.Network == "none"`.
- **`internal/provider/config.go`**:
  - `Config` exposes `Type`, `Endpoint`, `WireAPI`, `Transport`, `Headers` (`cty.Value` potentially marked sensitive), `APIKey`, `APIKeyRef`, `BearerToken`, `BearerTokenRef`.
- **`internal/mcp/config.go`**:
  - `Config` exposes `Name`, `RuntimeName`, `Transport`, `Tools`, `Timeout`, `HTTP` (`URL`, `Headers`, `BearerToken`, `BearerTokenRef`), `Stdio` (`Command`, `Args`, `Env`, `EnvRefs`, `WorkingDirectory`).
- **`internal/harness/spec/schema.go`**:
  - `HarnessConfig` exposes `Name`, `EnvRefs map[string]string`, and `Options cty.Value`.
