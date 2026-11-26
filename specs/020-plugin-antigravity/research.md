# Research: `020-plugin-antigravity`

## 1. `hashicorp/go-plugin` AutoMTLS Handshake Alignment (`pkg/harnessplugin/plugin.go`)
- `pkg/harnessplugin/plugin.go` checks `SOFA_HARNESS_PLUGIN == "sofa-harness-v1"` and requires `PLUGIN_CLIENT_CERT`.
- In container TCP mode (Windows/macOS Podman port forwarding), `ContainerRunner` strips `PLUGIN_UNIX_SOCKET_DIR` and sets `PLUGIN_MIN_PORT=19500`, `PLUGIN_MAX_PORT=19500`, listening on `0.0.0.0:19500`. In Linux mode, `PLUGIN_UNIX_SOCKET_DIR=/.sofa/plugin` is set and the plugin listens on `unix:///.sofa/plugin/harness.sock`.
- `go-plugin` parses the server DER certificate in `1|1|<net>|<addr>|grpc|<b64>` using `base64.RawStdEncoding` (**unpadded** standard Base64). Python's `base64.b64encode(der).decode("ascii").rstrip("=")` matches `RawStdEncoding`.
- `go-plugin` requires `grpc.health.v1.Health` to report `SERVING` for service `"plugin"` (plus `""` for general health probes).
- `PLUGIN_*` and `SOFA_HARNESS_PLUGIN` must be scrubbed from `os.environ` immediately after reading so spawned tools/subprocesses never see handshake material.
- **P-521 AutoMTLS Interop**: `hashicorp/go-plugin` (`mtls.go`) generates `PLUGIN_CLIENT_CERT` using ECDSA P-521 (`elliptic.P521()`), while `grpcio`'s bundled BoringSSL rejects P-521 signature algorithms (`NO_COMMON_SIGNATURE_ALGORITHMS`). `handshake.py` terminates TLS 1.2/1.3 ALPN `h2` mTLS via Python's stdlib OpenSSL (`ssl.SSLContext`, which natively supports P-521, P-384, P-256, and RSA) and forwards loopback frames over an ephemeral ECDSA P-256 mTLS connection to `grpc.aio.server()`, ensuring strict AutoMTLS compatibility with unmodified `hashicorp/go-plugin` clients and leaving zero plaintext ports inside the container.

## 2. Generated Python Protobuf Stubs (`proto/sofa/harness/v1`)
- `harness_pb2.py` was generated for Protobuf `7.36.2` (`protobuf>=7.36.2` or compatible runtime matching `ValidateProtobufRuntimeVersion(PUBLIC, 7, 36, 2, ...)`).
- `harness_pb2_grpc.py` executes `from sofa.harness.v1 import harness_pb2`. Adding `Path(__file__).resolve().parent / "proto"` to `sys.path` in `sofa_harness_antigravity/__init__.py` (idempotently) allows `from sofa.harness.v1 import harness_pb2, harness_pb2_grpc` cleanly regardless of working directory.

## 3. `google-antigravity` Python SDK (`0.1.24`) Inspection
- `Agent(config)` is an async context manager (`async with Agent(config) as agent:`).
- `LocalAgentConfig` accepts: `system_instructions`, `capabilities=CapabilitiesConfig(...)`, `tools=[ToolWithSchema(fn, schema), ...]`, `policies=[policy.allow_all()]`, `hooks=[PreToolCallDecideHook(...), PostToolCallHook(...), OnToolErrorHook(...)]`, `mcp_servers=[McpStdioServer(...), McpStreamableHttpServer(...)]`, `workspaces=[working_directory]`, `env=...`, `conversation_id=...`, `session_continuation_mode=...`, `save_dir=...`, `skills_paths=...`, `model=...`, `api_key=...`.
- `CapabilitiesConfig` accepts `enabled_tools: list[BuiltinTools] | None`, `disabled_tools: list[BuiltinTools] | None`, `agent_behavior=AgentBehavior.AUTONOMOUS`. When `permission_mode == "read_only"`, `BuiltinTools.read_only()` (plus safe discovery tools `LIST_DIR`, `SEARCH_DIR`, `FIND_FILE` if not excluded) or excluding `CREATE_FILE`, `EDIT_FILE`, `RUN_COMMAND` prevents workspace mutation.
- `agent.chat(prompt)` returns `ChatResponse` with independent async cursors (`response.chunks` yielding `Text`, `Thought`, `ToolCall`, `ToolResult`, plus `response.usage_metadata` and `response.cancel()`).
- `ToolWithSchema(fn, input_schema)` wraps an async callable with an explicit JSON Schema dict (`HostToolSpec.parameters_json_schema`).
- `PreToolCallDecideHook` receives `types.ToolCall` (`id`, `name`, `args`) and returns `HookResult(allow=True/False, message=..., modified_args=...)`. `PostToolCallHook` receives `types.ToolResult` (`id`, `name`, `result`, `error`, `exception`).

## 4. Relocatable Linux Bundle Strategy on Windows + Podman
- `uv python install cpython-3.12.14-linux-x86_64-gnu --no-bin --no-registry --install-dir ...` downloads standalone Linux CPython directly on Windows/macOS/Linux.
- Copying `bin/python3.12` to `bin/python3` as a regular file avoids Windows NTFS symlink issues across Podman virtiofs mounts and satisfies `installer.go`'s no-symlink ZIP invariant.
- `uv pip install --python-platform x86_64-manylinux_2_28 --python-version 3.12 --target <py>/lib/python3.12/site-packages` installs prebuilt Linux manylinux wheels (`grpcio`, `grpcio-health-checking`, `protobuf`, `cryptography`, and optionally `google-antigravity`) directly into the bundle, verified working in `docker.io/library/golang:1.27`.
- Note on Windows `crlf`: `.gitattributes` must pin `plugins/sofa-harness-antigravity/bin/* text eol=lf` and `*.sh text eol=lf`, and `build_linux_bundle.py` must write the launcher script with explicit `newline="\n"` and `0o755` permissions.
