"""AgentBackend protocol, GoogleAntigravityBackend SDK adapter, and ScriptedFakeBackend."""

from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import tempfile
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from sofa.harness.v1 import harness_pb2


@dataclass(slots=True)
class SessionOpenParams:
    """Normalized parameters extracted from OpenSessionRequest."""

    session_id: str
    resume_session_id: str
    model: str
    profile: str
    reasoning_effort: str
    system_prompt: str
    working_directory: str
    session_store_directory: str
    permission_mode: str
    harness_env: dict[str, str]
    harness_options: dict[str, Any]
    host_tools: list[harness_pb2.HostToolSpec]
    available_tools_configured: bool
    available_tools: list[str]
    excluded_tools_configured: bool
    excluded_tools: list[str]
    skill_directories: list[str]
    skills: list[str]
    disabled_skills: list[str]
    mcp_servers: list[harness_pb2.MCPServerConfig]


@dataclass(slots=True)
class TurnOutcome:
    """Result returned by an AgentSession turn execution."""

    assistant_message_id: str = ""
    final_content: str = ""


class TurnCallbacks(Protocol):
    """Stream callbacks provided by RunTurn to an active AgentSession."""

    async def emit_event(self, event: harness_pb2.SessionEvent) -> None: ...

    async def invoke_host_tool(
        self, tool_call_id: str, tool_name: str, arguments_json: bytes
    ) -> harness_pb2.InvokeHostToolResponse: ...

    async def pre_tool_use(
        self, tool_call_id: str, tool_name: str, arguments_json: bytes
    ) -> harness_pb2.PreToolUseDecision: ...

    async def post_tool_use(
        self, tool_call_id: str, tool_name: str, succeeded: bool, error_message: str
    ) -> None: ...


class AgentSession(Protocol):
    """Per-session backend lifecycle interface."""

    async def run_turn(self, turn_id: str, prompt: str, callbacks: TurnCallbacks) -> TurnOutcome: ...

    async def resume(self) -> None: ...

    async def read_mcp_resource(
        self, server_name: str, uri: str
    ) -> list[harness_pb2.MCPResourceContent]: ...

    async def close(self) -> None: ...


class AgentBackend(Protocol):
    """Factory protocol for opening or resuming AgentSessions."""

    async def open_session(self, params: SessionOpenParams) -> tuple[str, bool, AgentSession]: ...


def classify_exception(exc: BaseException, *, aborted: bool = False) -> harness_pb2.ErrorDetail:
    """Map Python/SDK/HTTP exceptions into deterministic harness ErrorDetail."""
    msg = str(exc) or type(exc).__name__
    lower = msg.lower()

    if aborted or isinstance(exc, asyncio.CancelledError):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_INTERNAL,
            http_status_code=0,
            message=msg or "turn aborted",
        )

    status_code = 0
    for attr in ("status_code", "http_status_code", "code"):
        val = getattr(exc, attr, None)
        if isinstance(val, int) and val > 0:
            status_code = val
            break

    if "session not found" in lower or isinstance(exc, KeyError):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND,
            http_status_code=status_code or 404,
            message=msg,
        )

    if status_code == 429 or any(
        needle in lower for needle in ("rate_limit", "rate limit", "too many requests", "resource_exhausted", "http 429", "status 429")
    ):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_RATE_LIMIT,
            http_status_code=status_code or 429,
            message=msg,
        )

    if status_code == 401 or any(
        needle in lower for needle in ("authentication", "unauthorized", "invalid api key", "api key not valid", "401")
    ):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_AUTHENTICATION,
            http_status_code=status_code or 401,
            message=msg,
        )

    if status_code == 403 or any(
        needle in lower for needle in ("authorization", "forbidden", "permission denied", "403")
    ):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_AUTHORIZATION,
            http_status_code=status_code or 403,
            message=msg,
        )

    if any(needle in lower for needle in ("quota", "insufficient_quota", "billing")):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_QUOTA,
            http_status_code=status_code,
            message=msg,
        )

    if any(
        needle in lower
        for needle in ("context_limit", "context length", "context window", "maximum context", "token limit", "too many tokens")
    ):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_CONTEXT_LIMIT,
            http_status_code=status_code,
            message=msg,
        )

    if "unsupported" in lower:
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_UNSUPPORTED_CAPABILITY,
            http_status_code=status_code or 400,
            message=msg,
        )

    if status_code in (408, 500, 502, 503, 504) or status_code >= 500 or any(
        needle in lower
        for needle in (
            "connection reset",
            "connection refused",
            "broken pipe",
            "unexpected eof",
            "timeout",
            "timed out",
            "temporarily unavailable",
            "service unavailable",
            "bad gateway",
            "gateway timeout",
            "deadline exceeded",
        )
    ):
        return harness_pb2.ErrorDetail(
            category=harness_pb2.ERROR_CATEGORY_TRANSIENT,
            http_status_code=status_code,
            message=msg,
        )

    return harness_pb2.ErrorDetail(
        category=harness_pb2.ERROR_CATEGORY_INTERNAL,
        http_status_code=status_code,
        message=msg,
    )


def _now_unix_nano() -> int:
    return time.time_ns()


def _new_event_id() -> str:
    return f"ev-{uuid.uuid4().hex[:12]}"


def _parse_builtin_tools(names: list[str]) -> list[Any]:
    from google.antigravity import types as ag_types

    parsed: list[Any] = []
    for raw in names:
        try:
            parsed.append(ag_types.BuiltinTools(raw))
        except ValueError:
            continue
    return parsed


def _parse_skill_frontmatter_name(skill_md_text: str, fallback_dir_name: str) -> str:
    """Extract top-level frontmatter `name:` from SKILL.md or fall back to directory name."""
    normalized = skill_md_text.replace("\r\n", "\n").replace("\r", "\n")
    if not normalized.startswith("---\n"):
        return fallback_dir_name
    rest = normalized[len("---\n") :]
    end_idx = rest.find("\n---")
    if end_idx < 0:
        return fallback_dir_name
    after = rest[end_idx + len("\n---") :]
    if after and not after.startswith("\n"):
        return fallback_dir_name
    block = rest[:end_idx]
    for line in block.split("\n"):
        if not line.startswith("name:"):
            continue
        val = line[len("name:") :].strip()
        if len(val) >= 2 and val[0] == val[-1] and val[0] in ('"', "'"):
            val = val[1:-1]
        val = val.strip()
        if val:
            return val
    return fallback_dir_name


def _copy_clean_skill_subdir(src_dir: Path, dst_dir: Path) -> None:
    """Copy a skill directory without symlinks, preserving executable modes."""
    if src_dir.is_symlink() or not src_dir.is_dir():
        return
    dst_dir.mkdir(parents=True, exist_ok=True)
    for entry in os.scandir(src_dir):
        if entry.is_symlink():
            continue
        src_path = Path(entry.path)
        dst_path = dst_dir / entry.name
        if entry.is_dir(follow_symlinks=False):
            _copy_clean_skill_subdir(src_path, dst_path)
        elif entry.is_file(follow_symlinks=False):
            shutil.copy2(src_path, dst_path)


def _prepare_skills(
    params: SessionOpenParams,
) -> tuple[list[str], list[tuple[str, Path, str]], list[str], str | None]:
    """Filter disabled skills onto clean per-session dirs and resolve lenient eager skills."""
    disabled = {s.strip() for s in params.disabled_skills if s and s.strip()}
    effective_skills_paths: list[str] = []
    temp_stage_root: str | None = None
    stage_base: Path | None = None

    # Track discovered enabled skills: maps lookup name -> (effective_name, subdir_path, skill_md_text)
    by_effective: dict[str, tuple[str, Path, str]] = {}
    by_entry: dict[str, tuple[str, Path, str]] = {}

    for idx, raw_dir in enumerate(params.skill_directories):
        dir_str = (raw_dir or "").strip()
        if not dir_str:
            continue
        dir_path = Path(dir_str)
        if not dir_path.is_dir() or dir_path.is_symlink():
            # Lenient mode: keep unmapped/missing directories unchanged.
            effective_skills_paths.append(dir_str)
            continue

        enabled_items: list[tuple[str, str, Path, str]] = []
        excluded_any = False
        for entry in sorted(os.scandir(dir_path), key=lambda e: e.name):
            if entry.is_symlink() or not entry.is_dir(follow_symlinks=False):
                continue
            skill_md = Path(entry.path) / "SKILL.md"
            if skill_md.is_symlink() or not skill_md.is_file():
                continue
            md_text = skill_md.read_text(encoding="utf-8")
            eff_name = _parse_skill_frontmatter_name(md_text, entry.name)
            if eff_name in disabled or entry.name in disabled:
                excluded_any = True
                continue
            enabled_items.append((eff_name, entry.name, Path(entry.path), md_text))

        if disabled and excluded_any:
            if stage_base is None:
                if params.session_store_directory:
                    stage_base = Path(params.session_store_directory) / ".sofa-filtered-skills"
                else:
                    temp_stage_root = tempfile.mkdtemp(prefix="sofa-ag-skills-")
                    stage_base = Path(temp_stage_root) / ".sofa-filtered-skills"
            staged_dir = stage_base / f"dir-{idx}"
            if staged_dir.exists():
                shutil.rmtree(staged_dir, ignore_errors=True)
            staged_dir.mkdir(parents=True, exist_ok=True)
            for eff_name, entry_name, src_subdir, md_text in enabled_items:
                dst_subdir = staged_dir / entry_name
                _copy_clean_skill_subdir(src_subdir, dst_subdir)
                by_effective.setdefault(eff_name, (eff_name, dst_subdir, md_text))
                by_entry.setdefault(entry_name, (eff_name, dst_subdir, md_text))
            effective_skills_paths.append(str(staged_dir))
        else:
            for eff_name, entry_name, src_subdir, md_text in enabled_items:
                by_effective.setdefault(eff_name, (eff_name, src_subdir, md_text))
                by_entry.setdefault(entry_name, (eff_name, src_subdir, md_text))
            effective_skills_paths.append(dir_str)

    active_entries: list[tuple[str, Path, str]] = []
    effective_skills: list[str] = []
    seen_skills: set[str] = set()
    for raw_skill in params.skills:
        req_name = (raw_skill or "").strip()
        if not req_name or req_name in disabled:
            continue
        matched = by_effective.get(req_name) or by_entry.get(req_name)
        if matched is None:
            # Lenient mode: silently skip missing requested skills.
            continue
        eff_name, _, _ = matched
        if eff_name in disabled or eff_name in seen_skills:
            continue
        seen_skills.add(eff_name)
        active_entries.append(matched)
        effective_skills.append(eff_name)

    return effective_skills_paths, active_entries, effective_skills, temp_stage_root


def _build_system_instructions_with_skills(
    base_prompt: str, active_entries: list[tuple[str, Path, str]]
) -> str:
    parts: list[str] = []
    if base_prompt:
        parts.append(base_prompt)
    for eff_name, skill_dir_path, md_text in active_entries:
        parts.append(
            f"Active Skill ({eff_name}) [directory: {skill_dir_path}]:\n{md_text.strip()}"
        )
    return "\n\n".join(parts)


class GoogleAntigravitySession:
    """Live session wrapping google.antigravity.Agent."""

    def __init__(
        self,
        backend: "GoogleAntigravityBackend",
        params: SessionOpenParams,
    ) -> None:
        self._backend = backend
        self._params = params
        self._host_tool_names = {spec.name for spec in params.host_tools}
        self._callbacks: TurnCallbacks | None = None
        self._agent: Any = None
        self._entered = False
        self._tool_call_names: dict[str, str] = {}
        self._temp_stage_root: str | None = None

    async def start(self) -> None:
        from google.antigravity import Agent

        config, temp_stage_root = self._backend._build_agent_config_with_stage_root(
            self._params,
            tool_callables=self._build_host_tool_callables(),
            hooks=self._build_hooks(),
        )
        self._temp_stage_root = temp_stage_root
        self._agent = Agent(config)
        await self._agent.__aenter__()
        self._entered = True

    def _make_host_tool_callable(self, spec: harness_pb2.HostToolSpec) -> Any:
        from google.antigravity import ToolExecutionError
        from google.antigravity.tools.tool_runner import ToolWithSchema

        schema: dict[str, Any] = {"type": "object", "properties": {}}
        if spec.parameters_json_schema:
            try:
                loaded = json.loads(spec.parameters_json_schema.decode("utf-8"))
                if isinstance(loaded, dict):
                    schema = loaded
            except Exception:
                pass

        tool_name = spec.name
        tool_desc = spec.description or f"Host tool {tool_name}"

        async def _host_tool_handler(**kwargs: Any) -> str:
            cb = self._callbacks
            if cb is None:
                raise ToolExecutionError(f"no active TurnCallbacks for host tool {tool_name}")
            call_id = f"tc-{uuid.uuid4().hex[:12]}"
            args_bytes = json.dumps(kwargs).encode("utf-8")
            resp = await cb.invoke_host_tool(call_id, tool_name, args_bytes)
            if resp.result_type == harness_pb2.HOST_TOOL_RESULT_TYPE_FAILURE:
                err_text = resp.handler_error or resp.text_result_for_llm or f"host tool {tool_name} failed"
                raise ToolExecutionError(err_text)
            return resp.text_result_for_llm

        _host_tool_handler.__name__ = tool_name
        _host_tool_handler.__doc__ = tool_desc
        return ToolWithSchema(_host_tool_handler, schema)

    def _build_host_tool_callables(self) -> list[Any]:
        return [self._make_host_tool_callable(spec) for spec in self._params.host_tools]

    def _build_hooks(self) -> list[Any]:
        from google.antigravity import hooks as ag_hooks
        from google.antigravity import types as ag_types

        @ag_hooks.pre_tool_call_decide
        async def _pre_tool_decide(call: ag_types.ToolCall) -> ag_types.HookResult:
            name = call.name.value if hasattr(call.name, "value") else str(call.name)
            # Anti-double-interception: host tools bypass PreToolUse/PostToolUse entirely.
            if name in self._host_tool_names:
                return ag_types.HookResult(allow=True)
            cb = self._callbacks
            if cb is None:
                return ag_types.HookResult(allow=True)
            call_id = call.id or f"tc-{uuid.uuid4().hex[:12]}"
            self._tool_call_names[call_id] = name
            args_bytes = json.dumps(call.args or {}).encode("utf-8")
            decision = await cb.pre_tool_use(call_id, name, args_bytes)
            if not decision.allow:
                return ag_types.HookResult(
                    allow=False,
                    message=decision.deny_reason or "denied by sofa host gate",
                )
            modified_args = None
            if decision.modified_arguments_json:
                try:
                    parsed = json.loads(decision.modified_arguments_json.decode("utf-8"))
                    if isinstance(parsed, dict):
                        modified_args = parsed
                except Exception:
                    modified_args = None
            return ag_types.HookResult(
                allow=True,
                message=decision.additional_context or "",
                modified_args=modified_args,
            )

        @ag_hooks.post_tool_call
        async def _post_tool(result: ag_types.ToolResult) -> None:
            name = result.name.value if hasattr(result.name, "value") else str(result.name)
            if name in self._host_tool_names:
                return
            cb = self._callbacks
            if cb is None:
                return
            call_id = result.id or f"tc-{uuid.uuid4().hex[:12]}"
            err_msg = result.error or (str(result.exception) if result.exception else "")
            succeeded = result.error is None and result.exception is None
            await cb.post_tool_use(call_id, name, succeeded, err_msg)

        @ag_hooks.on_tool_error
        async def _on_tool_error(exc: Exception) -> str | None:
            return str(exc)

        return [_pre_tool_decide, _post_tool, _on_tool_error]

    async def run_turn(self, turn_id: str, prompt: str, callbacks: TurnCallbacks) -> TurnOutcome:
        from google.antigravity import types as ag_types

        if self._agent is None:
            await self.start()

        self._callbacks = callbacks
        message_id = f"msg-{turn_id}"
        text_parts: list[str] = []
        response = None
        try:
            response = await self._agent.chat(prompt)
            async for chunk in response.chunks:
                if isinstance(chunk, ag_types.Thought):
                    if chunk.text:
                        await callbacks.emit_event(
                            harness_pb2.SessionEvent(
                                event_id=_new_event_id(),
                                timestamp_unix_nano=_now_unix_nano(),
                                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA,
                                message_id=message_id,
                                text=chunk.text,
                            )
                        )
                elif isinstance(chunk, ag_types.Text):
                    if chunk.text:
                        text_parts.append(chunk.text)
                        await callbacks.emit_event(
                            harness_pb2.SessionEvent(
                                event_id=_new_event_id(),
                                timestamp_unix_nano=_now_unix_nano(),
                                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA,
                                message_id=message_id,
                                text=chunk.text,
                            )
                        )
                elif isinstance(chunk, ag_types.ToolCall):
                    tname = chunk.name.value if hasattr(chunk.name, "value") else str(chunk.name)
                    await callbacks.emit_event(
                        harness_pb2.SessionEvent(
                            event_id=_new_event_id(),
                            timestamp_unix_nano=_now_unix_nano(),
                            kind=harness_pb2.SESSION_EVENT_KIND_TOOL_EXECUTION_START,
                            tool_call_id=chunk.id or "",
                            tool_name=tname,
                            arguments_json=json.dumps(chunk.args or {}).encode("utf-8"),
                        )
                    )
                elif isinstance(chunk, ag_types.ToolResult):
                    tname = chunk.name.value if hasattr(chunk.name, "value") else str(chunk.name)
                    await callbacks.emit_event(
                        harness_pb2.SessionEvent(
                            event_id=_new_event_id(),
                            timestamp_unix_nano=_now_unix_nano(),
                            kind=harness_pb2.SESSION_EVENT_KIND_TOOL_EXECUTION_COMPLETE,
                            tool_call_id=chunk.id or "",
                            tool_name=tname,
                            error_message=chunk.error or "",
                        )
                    )

            usage = response.usage_metadata
            if usage is not None:
                await callbacks.emit_event(
                    harness_pb2.SessionEvent(
                        event_id=_new_event_id(),
                        timestamp_unix_nano=_now_unix_nano(),
                        kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_USAGE,
                        usage=harness_pb2.TokenUsage(
                            model=self._params.model,
                            input_tokens=int(usage.prompt_token_count or 0),
                            output_tokens=int(usage.candidates_token_count or 0),
                            reasoning_tokens=int(usage.thoughts_token_count or 0),
                            cache_read_tokens=int(usage.cached_content_token_count or 0),
                        ),
                    )
                )

            await callbacks.emit_event(
                harness_pb2.SessionEvent(
                    event_id=_new_event_id(),
                    timestamp_unix_nano=_now_unix_nano(),
                    kind=harness_pb2.SESSION_EVENT_KIND_SESSION_IDLE,
                )
            )
            return TurnOutcome(
                assistant_message_id=message_id,
                final_content="".join(text_parts),
            )
        except asyncio.CancelledError:
            if response is not None:
                try:
                    await response.cancel()
                except Exception:
                    pass
            raise
        finally:
            self._callbacks = None

    async def resume(self) -> None:
        if self._agent is None:
            await self.start()

    async def read_mcp_resource(
        self, server_name: str, uri: str
    ) -> list[harness_pb2.MCPResourceContent]:
        raise NotImplementedError("MCP resource reading is not supported by google-antigravity SDK")

    async def close(self) -> None:
        try:
            if self._entered and self._agent is not None:
                try:
                    await self._agent.__aexit__(None, None, None)
                finally:
                    self._entered = False
                    self._agent = None
        finally:
            if self._temp_stage_root:
                shutil.rmtree(self._temp_stage_root, ignore_errors=True)
                self._temp_stage_root = None
            for key, sess in list(self._backend._sessions.items()):
                if sess is self:
                    self._backend._sessions.pop(key, None)


class GoogleAntigravityBackend:
    """Production AgentBackend backed by google-antigravity LocalAgentConfig."""

    def __init__(self) -> None:
        self._sessions: dict[str, GoogleAntigravitySession] = {}

    def build_agent_config(
        self,
        params: SessionOpenParams,
        tool_callables: list[Any],
        hooks: list[Any],
    ) -> Any:
        config, _ = self._build_agent_config_with_stage_root(params, tool_callables, hooks)
        return config

    def _build_agent_config_with_stage_root(
        self,
        params: SessionOpenParams,
        tool_callables: list[Any],
        hooks: list[Any],
    ) -> tuple[Any, str | None]:
        from google.antigravity import CapabilitiesConfig, LocalAgentConfig, types as ag_types
        from google.antigravity.hooks import policy

        disabled: list[ag_types.BuiltinTools] = []
        enabled: list[ag_types.BuiltinTools] | None = None

        if params.available_tools_configured:
            enabled = _parse_builtin_tools(params.available_tools)
        if params.excluded_tools_configured:
            disabled.extend(_parse_builtin_tools(params.excluded_tools))

        if params.permission_mode == "read_only":
            for mut_tool in (
                ag_types.BuiltinTools.CREATE_FILE,
                ag_types.BuiltinTools.EDIT_FILE,
                ag_types.BuiltinTools.RUN_COMMAND,
                ag_types.BuiltinTools.GENERATE_IMAGE,
            ):
                if mut_tool not in disabled:
                    disabled.append(mut_tool)
            policies: list[Any] | None = None
        else:
            policies = [policy.allow_all()]

        caps = CapabilitiesConfig(
            agent_behavior=ag_types.AgentBehavior.AUTONOMOUS,
            enabled_tools=enabled,
            disabled_tools=disabled or None,
        )

        mcp_configs: list[Any] = []
        for srv in params.mcp_servers:
            which = srv.WhichOneof("transport")
            if which == "stdio":
                mcp_configs.append(
                    ag_types.McpStdioServer(
                        name=srv.name,
                        command=srv.stdio.command,
                        args=list(srv.stdio.args),
                        env=dict(srv.stdio.env) or None,
                    )
                )
            elif which == "http":
                timeout_sec = (srv.timeout_ms / 1000.0) if srv.timeout_ms > 0 else 30.0
                mcp_configs.append(
                    ag_types.McpStreamableHttpServer(
                        name=srv.name,
                        url=srv.http.url,
                        headers=dict(srv.http.headers) or None,
                        timeout=timeout_sec,
                    )
                )

        # Keep GEMINI_API_KEY / GOOGLE_API_KEY in SDK api_key config rather than shell env.
        api_key = params.harness_env.get("GEMINI_API_KEY") or params.harness_env.get("GOOGLE_API_KEY")
        sdk_env = {
            k: v
            for k, v in params.harness_env.items()
            if k not in ("GEMINI_API_KEY", "GOOGLE_API_KEY")
        }

        effective_model = params.profile.strip() or params.model.strip() or None
        workspaces = [params.working_directory] if params.working_directory else None
        save_dir = params.session_store_directory or None
        raw_conv_id = params.resume_session_id or params.session_id
        conv_id: str | None = None
        if raw_conv_id:
            if len(raw_conv_id) >= 32 and re.fullmatch(r"[a-zA-Z0-9-]+", raw_conv_id):
                conv_id = raw_conv_id
            else:
                conv_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"sofa:antigravity:{raw_conv_id}"))

        effective_skills_paths, active_entries, _, temp_stage_root = _prepare_skills(params)
        combined_instructions = _build_system_instructions_with_skills(
            params.system_prompt or "", active_entries
        )

        kwargs: dict[str, Any] = {
            "system_instructions": combined_instructions or None,
            "capabilities": caps,
            "tools": tool_callables or None,
            "hooks": hooks or None,
            "mcp_servers": mcp_configs or None,
            "workspaces": workspaces,
            "env": sdk_env or None,
            "conversation_id": conv_id,
            "save_dir": save_dir,
            "skills_paths": effective_skills_paths or None,
            "model": effective_model,
            "api_key": api_key,
        }
        if policies is not None:
            kwargs["policies"] = policies
        if params.resume_session_id:
            kwargs["session_continuation_mode"] = ag_types.SessionContinuationMode.RESUME_OR_FAIL

        return LocalAgentConfig(**kwargs), temp_stage_root

    async def open_session(self, params: SessionOpenParams) -> tuple[str, bool, AgentSession]:
        if params.resume_session_id:
            existing = self._sessions.get(params.resume_session_id)
            if existing is not None:
                return params.resume_session_id, True, existing
            if not params.session_store_directory:
                raise KeyError(f"session not found: {params.resume_session_id}")
            session = GoogleAntigravitySession(self, params)
            await session.start()
            self._sessions[params.resume_session_id] = session
            return params.resume_session_id, True, session

        sess_id = params.session_id or f"sess-ag-{uuid.uuid4().hex[:10]}"
        session = GoogleAntigravitySession(self, params)
        await session.start()
        self._sessions[sess_id] = session
        return sess_id, False, session


class ScriptedFakeSession:
    """Deterministic offline session used in unit tests and Podman integration tests."""

    def __init__(self, params: SessionOpenParams) -> None:
        self.params = params
        self._host_tool_names = {spec.name for spec in params.host_tools}
        eff_paths, active_entries, eff_skills, temp_stage_root = _prepare_skills(params)
        self._temp_stage_root = temp_stage_root
        self.effective_skill_directories = eff_paths
        self.effective_skills = eff_skills
        self.effective_system_instructions = _build_system_instructions_with_skills(
            params.system_prompt or "", active_entries
        )

    async def run_turn(self, turn_id: str, prompt: str, callbacks: TurnCallbacks) -> TurnOutcome:
        message_id = f"msg-{turn_id}"

        # Verify harness_env secrets did NOT leak into os.environ.
        for key in self.params.harness_env:
            if key in os.environ:
                raise RuntimeError(f"harness_env secret {key} leaked into os.environ")

        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                event_id=_new_event_id(),
                timestamp_unix_nano=_now_unix_nano(),
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA,
                message_id=message_id,
                text="Planning antigravity execution step",
            )
        )
        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                event_id=_new_event_id(),
                timestamp_unix_nano=_now_unix_nano(),
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA,
                message_id=message_id,
                text="Running scripted antigravity turn",
            )
        )
        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                event_id=_new_event_id(),
                timestamp_unix_nano=_now_unix_nano(),
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_USAGE,
                usage=harness_pb2.TokenUsage(
                    model=self.params.model,
                    input_tokens=32,
                    output_tokens=16,
                    reasoning_tokens=8,
                ),
            )
        )

        is_qc = self.params.permission_mode == "read_only" or "sofa_qc_submit_verdict" in self._host_tool_names
        if is_qc:
            final_text = await self._run_qc(callbacks)
        else:
            final_text = await self._run_work(callbacks)

        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                event_id=_new_event_id(),
                timestamp_unix_nano=_now_unix_nano(),
                kind=harness_pb2.SESSION_EVENT_KIND_SESSION_IDLE,
            )
        )
        return TurnOutcome(assistant_message_id=message_id, final_content=final_text)

    async def _run_work(self, callbacks: TurnCallbacks) -> str:
        ws = Path(self.params.working_directory or "/workspace")
        artifacts_dir = Path("/.sofa/artifacts")

        args_bytes = json.dumps({"path": "allowed/feature.txt", "content": "feature-v1\n"}).encode("utf-8")
        dec = await callbacks.pre_tool_use("tc-ag-edit-1", "edit_file", args_bytes)
        if not dec.allow:
            raise RuntimeError(f"built-in edit_file denied: {dec.deny_reason}")

        try:
            allowed_dir = ws / "allowed"
            allowed_dir.mkdir(parents=True, exist_ok=True)
            (allowed_dir / "feature.txt").write_text("feature-v1\n", encoding="utf-8")
            artifacts_dir.mkdir(parents=True, exist_ok=True)
            (artifacts_dir / "report.json").write_text('{"harness":"antigravity","status":"ok"}\n', encoding="utf-8")
        except Exception as exc:
            await callbacks.post_tool_use("tc-ag-edit-1", "edit_file", False, str(exc))
            raise
        await callbacks.post_tool_use("tc-ag-edit-1", "edit_file", True, "")

        if "sofa_checkpoint" in self._host_tool_names:
            await callbacks.invoke_host_tool(
                "tc-ag-ckpt-1",
                "sofa_checkpoint",
                json.dumps(
                    {
                        "completed": ["implemented feature.txt and report.json"],
                        "next_steps": ["complete work"],
                    }
                ).encode("utf-8"),
            )
        if "sofa_complete_work" in self._host_tool_names:
            await callbacks.invoke_host_tool(
                "tc-ag-complete-1",
                "sofa_complete_work",
                json.dumps({"summary": "feature.txt and report.json ready"}).encode("utf-8"),
            )
        return "work completed"

    async def _run_qc(self, callbacks: TurnCallbacks) -> str:
        ws = Path(self.params.working_directory or "/workspace")
        artifacts_dir = Path("/.sofa/artifacts")
        issues: list[dict[str, str]] = []

        # 1. Verify workspace is physically read-only.
        try:
            (ws / "allowed" / "qc_tamper.txt").write_text("bad", encoding="utf-8")
            issues.append(
                {
                    "criterion": "read_only_isolation",
                    "code": "workspace_writable",
                    "message": "QC container was unexpectedly able to write to /workspace",
                }
            )
        except OSError:
            pass

        # 2. Verify /.sofa/artifacts/report.json is physically read-only.
        try:
            (artifacts_dir / "report.json").write_text("tampered", encoding="utf-8")
            issues.append(
                {
                    "criterion": "read_only_isolation",
                    "code": "artifacts_writable",
                    "message": "QC container was unexpectedly able to write to /.sofa/artifacts/report.json",
                }
            )
        except OSError:
            pass

        # 3. Verify frozen workspace and artifact contents match Work output.
        feat_path = ws / "allowed" / "feature.txt"
        if not feat_path.exists() or feat_path.read_text(encoding="utf-8").strip() != "feature-v1":
            issues.append(
                {
                    "criterion": "feature_content",
                    "code": "missing_feature",
                    "message": f"unexpected feature.txt at {feat_path}",
                }
            )

        rep_path = artifacts_dir / "report.json"
        if not rep_path.exists() or '"harness":"antigravity"' not in rep_path.read_text(encoding="utf-8"):
            issues.append(
                {
                    "criterion": "feature_content",
                    "code": "missing_report",
                    "message": f"unexpected report.json at {rep_path}",
                }
            )

        if issues and "sofa_qc_open_issues" in self._host_tool_names:
            await callbacks.invoke_host_tool(
                "tc-ag-qc-issues",
                "sofa_qc_open_issues",
                json.dumps({"issues": issues}).encode("utf-8"),
            )
            if "sofa_qc_submit_verdict" in self._host_tool_names:
                await callbacks.invoke_host_tool(
                    "tc-ag-qc-verdict",
                    "sofa_qc_submit_verdict",
                    json.dumps({"decision": "revise", "summary": "qc verification failed"}).encode("utf-8"),
                )
        elif "sofa_qc_submit_verdict" in self._host_tool_names:
            await callbacks.invoke_host_tool(
                "tc-ag-qc-verdict",
                "sofa_qc_submit_verdict",
                json.dumps(
                    {
                        "decision": "pass",
                        "summary": "verified read-only workspace and antigravity report.json",
                    }
                ).encode("utf-8"),
            )
        return "qc completed"

    async def resume(self) -> None:
        return None

    async def read_mcp_resource(
        self, server_name: str, uri: str
    ) -> list[harness_pb2.MCPResourceContent]:
        return []

    async def close(self) -> None:
        if self._temp_stage_root:
            shutil.rmtree(self._temp_stage_root, ignore_errors=True)
            self._temp_stage_root = None
        return None


class ScriptedFakeBackend:
    """Offline AgentBackend returning ScriptedFakeSession instances."""

    def __init__(self) -> None:
        self._sessions: dict[str, ScriptedFakeSession] = {}

    async def open_session(self, params: SessionOpenParams) -> tuple[str, bool, AgentSession]:
        if params.resume_session_id:
            existing = self._sessions.get(params.resume_session_id)
            if existing is not None:
                return params.resume_session_id, True, existing
            if params.session_store_directory:
                marker = Path(params.session_store_directory) / f"{params.resume_session_id}.marker"
                if marker.is_file():
                    session = ScriptedFakeSession(params)
                    self._sessions[params.resume_session_id] = session
                    return params.resume_session_id, True, session
            raise KeyError(f"session not found: {params.resume_session_id}")

        sess_id = params.session_id or f"sess-ag-{uuid.uuid4().hex[:10]}"
        if params.session_store_directory:
            store_dir = Path(params.session_store_directory)
            store_dir.mkdir(parents=True, exist_ok=True)
            (store_dir / f"{sess_id}.marker").write_text("ok\n", encoding="utf-8")

        session = ScriptedFakeSession(params)
        self._sessions[sess_id] = session
        return sess_id, False, session


class HybridBackend:
    """Selects ScriptedFakeBackend when options['backend'] == 'fake_scripted', else GoogleAntigravityBackend."""

    def __init__(self) -> None:
        self._real = GoogleAntigravityBackend()
        self._fake = ScriptedFakeBackend()

    async def open_session(self, params: SessionOpenParams) -> tuple[str, bool, AgentSession]:
        if params.harness_options.get("backend") == "fake_scripted":
            return await self._fake.open_session(params)
        return await self._real.open_session(params)
