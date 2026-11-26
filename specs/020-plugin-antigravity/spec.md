# Feature Specification: Isolated In-Repo `sofa-harness-antigravity` Plugin (`020-plugin-antigravity`)

**Feature Branch**: `020-plugin-antigravity`

**Created**: 2026-11-27

**Status**: Approved

**Input**: Stage 2 Slice `020` blueprint (`docs/stage2/020-plugin-antigravity.md`), `docs/harness-plugin-contract.md`, `proto/sofa/harness/v1/harness.proto`, `pkg/harnessplugin/plugin.go`, `internal/harness/installer/installer.go`, `internal/worker/runner/worker_runner.go`, and `google-antigravity` Python SDK (`0.1.24`).

## User Scenarios & Testing

### User Story 1 - `hashicorp/go-plugin` AutoMTLS Handshake & Isolated Python Project (`plugins/sofa-harness-antigravity/`) (Priority: P1)

`plugins/sofa-harness-antigravity` incubates the official Python Google Antigravity AgentHarness plugin as an independent Python package (`pyproject.toml`, zero changes to root `go.mod`, zero imports of Go `internal/` packages):
- `bin/sofa-harness-antigravity` is a pure-LF (`\n`) POSIX `/bin/sh` launcher preferring `$SELF_DIR/python/bin/python3 -m sofa_harness_antigravity "$@"` and falling back to `python3 -m sofa_harness_antigravity "$@"`. `.gitattributes` enforces `text eol=lf` for `plugins/sofa-harness-antigravity/bin/*` and `*.sh`.
- `handshake.py` implements the exact 5-step `hashicorp/go-plugin` AutoMTLS handshake aligned with `pkg/harnessplugin/plugin.go`:
  1. Verifies `os.environ.get("SOFA_HARNESS_PLUGIN") == "sofa-harness-v1"` (else prints an error to `sys.stderr` and exits `1`) and requires non-empty `PLUGIN_CLIENT_CERT`.
  2. Reads `PLUGIN_CLIENT_CERT`, `PLUGIN_UNIX_SOCKET_DIR`, `PLUGIN_MIN_PORT` (default `19500`), `PLUGIN_MAX_PORT`, and `PLUGIN_PROTOCOL_VERSIONS`, and immediately scrubs `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` keys from `os.environ`.
  3. Generates an ephemeral ECDSA P-256 (`SECP256R1`) server key and 24-hour self-signed X.509 certificate with SAN `DNSName("localhost")`, `IPAddress(127.0.0.1)`, and `IPAddress(0.0.0.0)`, configures `grpc.ssl_server_credentials(..., root_certificates=client_cert_pem, require_client_auth=True)`, and encodes DER bytes with **unpadded** `base64.b64encode(der).decode("ascii").rstrip("=")` (matching `base64.RawStdEncoding` in `hashicorp/go-plugin`).
  4. Registers `grpc_health.v1.HealthServicer` with `"plugin" -> SERVING` (and `"" -> SERVING`), binds `unix://{unix_dir}/harness.sock` if `unix_dir` is non-empty else `0.0.0.0:{port}`, writes the single-line handshake `1|1|<net>|<addr>|grpc|<cert_b64>\n` to `sys.stdout`, flushes immediately, and directs all subsequent logging strictly to `sys.stderr`.

**Independent Test**: Offline pytest unit tests in `plugins/sofa-harness-antigravity/tests/test_handshake.py` verify cookie validation, `PLUGIN_*` / `SOFA_HARNESS_PLUGIN` environment scrubbing, single-line handshake format with unpadded DER base64 certificate, mTLS verification by a client holding the matching client cert/key, and mTLS rejection when a rogue client connects without the trusted client key.

**Acceptance Scenarios**:

1. **Given** `SOFA_HARNESS_PLUGIN="sofa-harness-v1"` and a valid `PLUGIN_CLIENT_CERT`, **When** the handshake helper starts the gRPC server, **Then** `os.environ` no longer contains `SOFA_HARNESS_PLUGIN` or any `PLUGIN_*` variable, stdout contains a single `1|1|...|grpc|<unpadded_b64>` line, and `Health("plugin")` returns `SERVING` over mTLS.
2. **Given** an mTLS channel without the trusted client certificate/key, **When** a rogue gRPC client calls `GetCapabilities` or `Health`, **Then** the TLS handshake fails and the RPC is rejected.
3. **Given** missing or mismatched `SOFA_HARNESS_PLUGIN` or empty `PLUGIN_CLIENT_CERT`, **When** startup runs, **Then** a diagnostic message is written to `stderr` and the process exits with code `1`.

---

### User Story 2 - 7-RPC `AgentHarnessServicer`, `google-antigravity` SDK Mapping & Bidirectional `RunTurn` (Priority: P1)

`servicer.py` and `backend.py` implement the full async 7-RPC `AgentHarnessServicer` over `grpc.aio`:
1. `GetCapabilities` returns `harness_name="antigravity"`, `protocol_version=1`, `supported_provider_types=[]`, `supports_session_resume=True`, `supports_mcp_servers=True`, `supports_mcp_resources=False` (or `True` when supported by backend), and `supports_skills=True`.
2. `OpenSession` rejects any non-empty external `provider` (`req.HasField("provider")` with non-empty `type`) with `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY` (`http_status_code=400`). It maps `permission_mode` (`"read_only"` vs `"approve_all"`), `system_prompt`, `working_directory`, `session_store_directory`, `skills`/`skill_directories`/`disabled_skills`, `available_tools`/`excluded_tools` (`ToolFilter.configured`), `mcp_servers`, `harness_env`, and `host_tools` (`hostToolNames` set) into `GoogleAntigravityBackend` (`LocalAgentConfig`, `CapabilitiesConfig`, `ToolWithSchema`, `PreToolCallDecideHook`, `PostToolCallHook`, `OnToolErrorHook`) without mutating `os.environ` with `harness_env`. If `resume_session_id` is not found, `OpenSession` returns `ERROR_CATEGORY_SESSION_NOT_FOUND` (`http_status_code=404`) without silent new-session fallback. Duplicate `session_id` closes any displaced session cleanly.
3. `RunTurn` bridges a single turn concurrently via an `asyncio.Queue` plus `pending_tool_futures` and `pending_hook_futures`:
   - Streams `ASSISTANT_REASONING_DELTA` (thoughts), `ASSISTANT_MESSAGE_DELTA` (text), `ASSISTANT_USAGE`, `TOOL_EXECUTION_*`, and `SESSION_IDLE`.
   - Host tools (`tool_name in host_tool_names`) emit `InvokeHostToolRequest` and await `InvokeHostToolResponse` — and NEVER emit `PreToolUseRequest` or `PostToolUseRequest`.
   - Non-host built-in/MCP tools emit `PreToolUseRequest` (enforcing `allow == False` -> deny with `deny_reason`, `modified_arguments_json`, and `additional_context`) followed by `PostToolUseRequest(succeeded=True/False)` with matching `tool_call_id`.
4. `AbortTurn` cancels the active turn task (honoring non-empty `turn_id` matching so stale `AbortTurn` calls do not cancel a newer turn, and classifying explicit cancellation as `ERROR_CATEGORY_INTERNAL`), `ResumeSession` reconnects or returns `ERROR_CATEGORY_SESSION_NOT_FOUND`, `ReadMCPResource` delegates cleanly, `CloseSession` closes and cleans up session state, and SDK/HTTP errors map deterministically (`429` -> `RATE_LIMIT`, `401` -> `AUTHENTICATION`, `403` -> `AUTHORIZATION`, quota -> `QUOTA`, context/token limit -> `CONTEXT_LIMIT`, `5xx`/timeout -> `TRANSIENT`, else `INTERNAL`).

**Independent Test**: Offline pytest unit tests in `plugins/sofa-harness-antigravity/tests/test_servicer.py` using `ScriptedFakeBackend` (and testing `GoogleAntigravityBackend` config mapping) verify all 7 RPCs, `openai` provider rejection, `RunTurn` concurrent thoughts + text + built-in `PreToolUse` allow/deny + `PostToolUse` + `InvokeHostTool` without double interception, `AbortTurn` with `turn_id` guard, and error classification.

**Acceptance Scenarios**:

1. **Given** `OpenSessionRequest` with `provider.type = "openai"`, **When** `OpenSession` runs, **Then** it returns `OpenSessionResponse` with `error.category == ERROR_CATEGORY_UNSUPPORTED_CAPABILITY` and `error.http_status_code == 400`.
2. **Given** an active `RunTurn` with `host_tools = ["sofa_checkpoint"]` where the backend invokes both `bash` (built-in) and `sofa_checkpoint` (host tool), **When** events and hooks execute, **Then** `bash` emits `PreToolUseRequest` -> `PostToolUseRequest(succeeded=...)`, whereas `sofa_checkpoint` emits only `InvokeHostToolRequest` and zero `PreToolUseRequest`/`PostToolUseRequest` frames.
3. **Given** a pending `RunTurn` blocked on a tool or response, **When** `AbortTurn` arrives with matching `turn_id` (or empty `turn_id`), **Then** the turn cancels promptly and emits `TurnResult` with `error.category == ERROR_CATEGORY_INTERNAL`; a mismatched `turn_id` does not cancel an active newer turn.

---

### User Story 3 - Self-Contained Linux Bundle & Tier 2 Podman `golang:1.27` End-to-End Integration (Priority: P1)

- `plugins/sofa-harness-antigravity/scripts/build_linux_bundle.py` builds a relocatable, zero-symlink Linux bundle `<out>/sofa-harness-antigravity/{sofa-harness-antigravity, python/...}` using `uv` standalone Linux CPython + manylinux wheels (`grpcio`, `grpcio-health-checking`, `protobuf`, `cryptography`, and optionally `google-antigravity`) plus the `sofa_harness_antigravity` package, so a `golang:1.27` container executes `--entrypoint /.sofa/harness/sofa-harness-antigravity` directly with zero pre-installed Python packages.
- `internal/worker/runner/antigravity_plugin_integration_test.go` (`//go:build integration`) runs `build_linux_bundle.py`, stages the bundle into `<plugin_dir>/sofa-harness-antigravity/linux_<arch>/`, configures `harness "antigravity"` (zero hardcoded paths, `model_provider` unset/null, `options` enabling the built-in `fake_scripted` backend, `env_refs` delivering `GEMINI_API_KEY` solely in-memory), and executes `018` `WorkerRunner.Run` inside a real Podman `golang:1.27` container through Work (`PreToolUse`/`PostToolUse` + `sofa_checkpoint` + `sofa_complete_work`) -> Physical Freeze -> Gatekeeper -> Read-Only QC -> `work_branch` + `BlobStore` artifact publication.

**Independent Test**: `go test -tags=integration ./internal/worker/runner/... -count=1` passes against real Podman `golang:1.27` and local Git bare repositories with zero external internet API keys required during test execution and zero leftover containers.

## Requirements

- **FR-001**: `plugins/sofa-harness-antigravity/pyproject.toml` and `.gitignore` MUST keep the Python plugin cleanly isolated; root `go.mod` MUST NOT be modified.
- **FR-002**: `plugins/sofa-harness-antigravity/bin/sofa-harness-antigravity` and bundled entrypoints MUST preserve pure LF (`\n`) line endings enforced by `.gitattributes` and `0755` permissions.
- **FR-003**: `handshake.py` MUST enforce `SOFA_HARNESS_PLUGIN == "sofa-harness-v1"`, require `PLUGIN_CLIENT_CERT`, scrub `SOFA_HARNESS_PLUGIN` and all `PLUGIN_*` env vars before serving, encode server DER cert with unpadded standard base64, register `HealthServicer("plugin")`, and flush the single-line handshake to stdout while directing logs to stderr.
- **FR-004**: `backend.py` MUST define the async `AgentBackend`/`AgentSession` protocol, `GoogleAntigravityBackend` mapping `OpenSessionRequest` to `google.antigravity` (`LocalAgentConfig`, `CapabilitiesConfig`, `ToolWithSchema`, `PreToolCallDecideHook`, `PostToolCallHook`, `OnToolErrorHook`, `harness_env` isolated from `os.environ`), and `ScriptedFakeBackend` selectable via `harness_options_json` (`{"backend": "fake_scripted"}`).
- **FR-005**: `servicer.py` MUST implement all 7 `AgentHarnessServicer` async RPCs with anti-double-interception hook filtering (`host_tool_names`), `ERROR_CATEGORY_UNSUPPORTED_CAPABILITY` for external `provider`, `ERROR_CATEGORY_SESSION_NOT_FOUND` for missing `resume_session_id`, `turn_id`-guarded `AbortTurn`, and deterministic error classification.
- **FR-006**: `scripts/build_linux_bundle.py` MUST produce a symlink-free, relocatable Linux bundle runnable in `golang:1.27`.
- **FR-007**: `internal/worker/runner/antigravity_plugin_integration_test.go` (`//go:build integration`) MUST exercise full `WorkerRunner.Run` inside Podman `golang:1.27` using `013` local plugin discovery and in-memory `GEMINI_API_KEY` delivery.

## Success Criteria

- **SC-001**: `git diff go.mod` in `d:\project\sofa` is empty and no `.venv`, `__pycache__`, `.pytest_cache`, or bundle build artifacts appear in `git status -s`.
- **SC-002**: `pytest` on `plugins/sofa-harness-antigravity` passes 100% of handshake and servicer unit tests.
- **SC-003**: `gofmt -l .`, `go vet ./...`, `go test ./... -count=1`, and `go test -tags=integration ./internal/worker/runner/... -count=1` pass with zero leftover Podman containers.
