# Data Model: `020-plugin-antigravity`

## Entities

### 1. `HandshakeConfig` (`handshake.py`)
- `unix_socket_dir`: `str | None` (`PLUGIN_UNIX_SOCKET_DIR`)
- `min_port`: `int` (`PLUGIN_MIN_PORT`, default `19500`)
- `max_port`: `int` (`PLUGIN_MAX_PORT`, default `min_port`)
- `client_cert_pem`: `bytes` (`PLUGIN_CLIENT_CERT`, required)
- `negotiated_protocol_version`: `int` (negotiated against `PLUGIN_PROTOCOL_VERSIONS`, default `1`)
- `server_credentials`: `grpc.ServerCredentials` (ECDSA P-256 ephemeral cert + `require_client_auth=True`)
- `server_cert_der_b64`: `str` (unpadded standard Base64 DER)

### 2. `ManagedSession` (`servicer.py`)
- `session_id`: `str`
- `backend_session`: `AgentSession`
- `host_tool_names`: `set[str]` (exact names from `OpenSessionRequest.host_tools` for anti-double-interception)
- `turn_lock`: `asyncio.Lock` (enforces at most one active `RunTurn` per `session_id`)
- `active_turn`: `ActiveTurn | None`

### 3. `ActiveTurn` (`servicer.py`)
- `turn_id`: `str`
- `outbound_queue`: `asyncio.Queue[PluginTurnFrame | None]`
- `pending_hooks`: `dict[str, asyncio.Future[HostTurnFrame]]` (keyed by `hook_call_id`)
- `pending_tools`: `dict[str, asyncio.Future[InvokeHostToolResponse]]` (keyed by `tool_call_id`)
- `turn_task`: `asyncio.Task | None`
- `aborted`: `bool`

### 4. `BackendEvent` / `AgentSession` / `AgentBackend` (`backend.py`)
- `TurnCallbacks`: async callbacks provided by `RunTurn` to `AgentSession.run_turn`:
  - `emit_event(SessionEvent)`
  - `invoke_host_tool(tool_call_id, tool_name, arguments_json) -> InvokeHostToolResponse`
  - `pre_tool_use(tool_call_id, tool_name, arguments_json) -> PreToolUseDecision`
  - `post_tool_use(tool_call_id, tool_name, succeeded, error_message) -> None`
- `GoogleAntigravityBackend`: production adapter creating `Agent(LocalAgentConfig(...))` with `CapabilitiesConfig`, `ToolWithSchema` host tools, `PreToolCallDecideHook` / `PostToolCallHook` / `OnToolErrorHook`, and `harness_env` isolated from `os.environ`.
- `ScriptedFakeBackend`: deterministic offline backend enabled via `harness_options_json` (`{"backend": "fake_scripted"}`) or injected in unit tests, driving built-in tool `PreToolUse`/`PostToolUse` + `sofa_checkpoint` + `sofa_complete_work` (Work) and read-only verification + `sofa_qc_submit_verdict` (QC) inside containers without external Gemini API keys.
