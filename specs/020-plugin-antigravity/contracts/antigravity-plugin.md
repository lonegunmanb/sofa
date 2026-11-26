# Contract: `sofa-harness-antigravity` gRPC Plugin (`AgentHarness` v1)

## 1. Module & Packaging Isolation Boundary
- `plugins/sofa-harness-antigravity/pyproject.toml` defines standalone Python package `sofa-harness-antigravity`.
- Root `github.com/lonegunmanb/sofa` (`go.mod`) MUST NOT be modified.
- `bin/sofa-harness-antigravity` uses pure LF (`\n`), mode `0755`, preferring `$SELF_DIR/python/bin/python3 -m sofa_harness_antigravity "$@"` with fallback to `python3 -m sofa_harness_antigravity "$@"`.

## 2. `hashicorp/go-plugin` AutoMTLS Handshake Contract
- Required env: `SOFA_HARNESS_PLUGIN == "sofa-harness-v1"`, non-empty `PLUGIN_CLIENT_CERT`.
- Immediately remove `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` keys from `os.environ`.
- Ephemeral ECDSA P-256 (`SECP256R1`) server certificate with SAN `DNS:localhost`, `IP:127.0.0.1`, `IP:0.0.0.0`, 24h validity; `require_client_auth=True` against `PLUGIN_CLIENT_CERT`.
- Register `grpc_health.v1.HealthServicer` with `"plugin" -> SERVING` and `"" -> SERVING`.
- Bind `unix://{PLUGIN_UNIX_SOCKET_DIR}/harness.sock` if `PLUGIN_UNIX_SOCKET_DIR` is non-empty, else `0.0.0.0:{PLUGIN_MIN_PORT}` (default `19500`).
- Write `1|1|<unix|tcp>|<addr>|grpc|<unpadded_der_b64>\n` to `sys.stdout` and flush; write all logs only to `sys.stderr`.

## 3. 7-RPC `AgentHarness` Contract
- **`GetCapabilities`**: `harness_name="antigravity"`, `protocol_version=1`, `supported_provider_types=[]`, `supports_session_resume=True`, `supports_mcp_servers=True`, `supports_mcp_resources=False`, `supports_skills=True`.
- **`OpenSession`**:
  - Non-empty external `provider` (`req.HasField("provider")` and `req.provider.type.strip() != ""`) -> `OpenSessionResponse(error=ErrorDetail(category=ERROR_CATEGORY_UNSUPPORTED_CAPABILITY, http_status_code=400, ...))`.
  - Non-empty `resume_session_id` not found -> `OpenSessionResponse(error=ErrorDetail(category=ERROR_CATEGORY_SESSION_NOT_FOUND, http_status_code=404, ...))` without creating a new session.
  - `harness_env` delivered solely to SDK `LocalAgentConfig` (`api_key` / `env`), NEVER written to `os.environ`.
  - `permission_mode == "read_only"` disables write tools (`create_file`, `edit_file`, `run_command`); `"approve_all"` enables autonomous execution (`policy.allow_all()`).
  - `host_tools` tracked in `host_tool_names` and wrapped via `ToolWithSchema`.
- **`RunTurn`**:
  - First frame MUST be `StartTurnRequest`.
  - Streams `ASSISTANT_REASONING_DELTA`, `ASSISTANT_MESSAGE_DELTA`, `ASSISTANT_USAGE`, `TOOL_EXECUTION_*`, `SESSION_IDLE`, and terminal `TurnResult`.
  - Host tools (`tool_name in host_tool_names`) emit ONLY `InvokeHostToolRequest` (never `PreToolUseRequest`/`PostToolUseRequest`).
  - Non-host built-in/MCP tools emit `PreToolUseRequest` -> await `PreToolUseDecision` -> on `allow=True`, run tool and emit `PostToolUseRequest(succeeded=...)` -> await `PostToolUseAck`; on `allow=False`, deny tool with `deny_reason`.
- **`AbortTurn`**: Cancels active turn matching `session_id` (and `turn_id` when non-empty) without waiting on `turn_lock`; aborted turns emit `TurnResult(error=ErrorDetail(category=ERROR_CATEGORY_INTERNAL, ...))`.
- **`ResumeSession` / `ReadMCPResource` / `CloseSession`**: Clean delegation and error normalization (`429` -> `RATE_LIMIT`, `401` -> `AUTHENTICATION`, `403` -> `AUTHORIZATION`, quota -> `QUOTA`, context/token limit -> `CONTEXT_LIMIT`, `5xx`/timeout -> `TRANSIENT`, else `INTERNAL`).
