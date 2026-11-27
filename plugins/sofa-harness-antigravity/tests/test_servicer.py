"""Offline unit tests for 7-RPC AgentHarnessServicer and GoogleAntigravityBackend mapping."""

from __future__ import annotations

import asyncio
import json
import os
from typing import Any

import grpc
import pytest

from sofa.harness.v1 import harness_pb2, harness_pb2_grpc
from sofa_harness_antigravity.backend import (
    AgentBackend,
    AgentSession,
    GoogleAntigravityBackend,
    GoogleAntigravitySession,
    ScriptedFakeBackend,
    SessionOpenParams,
    TurnCallbacks,
    TurnOutcome,
    classify_exception,
)
from sofa_harness_antigravity.servicer import AgentHarnessServicer


class _DummyContext:
    def __init__(self) -> None:
        self.code = None
        self.details = None

    def set_code(self, code: Any) -> None:
        self.code = code

    def set_details(self, details: str) -> None:
        self.details = details


async def _start_inproc_servicer(backend: AgentBackend | None = None):
    server = grpc.aio.server()
    servicer = AgentHarnessServicer(backend=backend or ScriptedFakeBackend())
    harness_pb2_grpc.add_AgentHarnessServicer_to_server(servicer, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
    stub = harness_pb2_grpc.AgentHarnessStub(channel)
    return server, channel, stub


async def test_get_capabilities() -> None:
    servicer = AgentHarnessServicer()
    resp = await servicer.GetCapabilities(harness_pb2.GetCapabilitiesRequest(), _DummyContext())
    assert resp.harness_name == "antigravity"
    assert resp.protocol_version == 1
    assert list(resp.supported_provider_types) == []
    assert resp.supports_session_resume is True
    assert resp.supports_mcp_servers is True
    assert resp.supports_skills is True


async def test_open_session_rejects_external_provider_and_missing_resume() -> None:
    servicer = AgentHarnessServicer()

    # 1. External provider (e.g. openai) -> ERROR_CATEGORY_UNSUPPORTED_CAPABILITY
    bad_provider_req = harness_pb2.OpenSessionRequest(
        session_id="sess-ext",
        model="gpt-5",
        provider=harness_pb2.ProviderConfig(type="openai", api_key="sk-test"),
    )
    resp = await servicer.OpenSession(bad_provider_req, _DummyContext())
    assert resp.error.category == harness_pb2.ERROR_CATEGORY_UNSUPPORTED_CAPABILITY
    assert resp.error.http_status_code == 400

    # 2. Missing resume_session_id -> ERROR_CATEGORY_SESSION_NOT_FOUND (no silent fallback)
    missing_resume_req = harness_pb2.OpenSessionRequest(
        session_id="sess-new",
        resume_session_id="does-not-exist",
        model="gemini-2.5-pro",
    )
    resp2 = await servicer.OpenSession(missing_resume_req, _DummyContext())
    assert resp2.error.category == harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND
    assert resp2.error.http_status_code == 404


async def test_open_session_does_not_leak_harness_env_to_os_environ() -> None:
    servicer = AgentHarnessServicer()
    secret_key = "SOFA_UNIT_SECRET_GEMINI_KEY"
    os.environ.pop(secret_key, None)

    req = harness_pb2.OpenSessionRequest(
        session_id="sess-env",
        model="gemini-2.5-pro",
        harness_env={secret_key: "top-secret-in-mem", "GEMINI_API_KEY": "gm-secret"},
        harness_options_json=b'{"backend":"fake_scripted"}',
        available_tools=harness_pb2.ToolFilter(configured=True, names=[]),
        excluded_tools=harness_pb2.ToolFilter(configured=False),
        host_tools=[
            harness_pb2.HostToolSpec(
                name="sofa_checkpoint",
                description="checkpoint",
                parameters_json_schema=b'{"type":"object"}',
            )
        ],
    )
    resp = await servicer.OpenSession(req, _DummyContext())
    assert resp.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED
    assert resp.session_id == "sess-env"
    assert secret_key not in os.environ
    assert "GEMINI_API_KEY" not in os.environ


class _MockTurnSession(AgentSession):
    def __init__(self, params: SessionOpenParams) -> None:
        self.params = params
        self.hook_outcomes: list[tuple[str, bool, str, str]] = []
        self.host_tool_responses: list[harness_pb2.InvokeHostToolResponse] = []
        self.resumed_count = 0
        self.closed = False

    async def run_turn(self, turn_id: str, prompt: str, callbacks: TurnCallbacks) -> TurnOutcome:
        # 1. Stream reasoning thoughts + text delta + usage
        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA,
                text="thinking step 1",
            )
        )
        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA,
                text="hello ",
            )
        )
        await callbacks.emit_event(
            harness_pb2.SessionEvent(
                kind=harness_pb2.SESSION_EVENT_KIND_ASSISTANT_USAGE,
                usage=harness_pb2.TokenUsage(
                    model=self.params.model,
                    input_tokens=10,
                    output_tokens=5,
                    reasoning_tokens=3,
                ),
            )
        )

        # 2. Built-in non-host tool (allowed with modified args + additional context)
        dec1 = await callbacks.pre_tool_use("tc-bash-1", "bash", b'{"command":"ls"}')
        self.hook_outcomes.append(("bash-1", dec1.allow, dec1.modified_arguments_json.decode(), dec1.additional_context))
        if dec1.allow:
            await callbacks.post_tool_use("tc-bash-1", "bash", True, "")

        # 3. Built-in non-host tool (denied) -> must NOT invoke post_tool_use
        dec2 = await callbacks.pre_tool_use("tc-bash-2", "bash", b'{"command":"rm -rf"}')
        self.hook_outcomes.append(("bash-2", dec2.allow, dec2.deny_reason, ""))

        # 4. Built-in non-host tool failure -> post_tool_use(succeeded=False)
        dec3 = await callbacks.pre_tool_use("tc-edit-1", "edit_file", b'{"path":"x"}')
        if dec3.allow:
            await callbacks.post_tool_use("tc-edit-1", "edit_file", False, "permission denied")

        # 5. Host tool -> ONLY InvokeHostToolRequest, zero PreToolUse/PostToolUse
        host_resp = await callbacks.invoke_host_tool("tc-ckpt-1", "sofa_checkpoint", b'{"completed":["done"]}')
        self.host_tool_responses.append(host_resp)

        await callbacks.emit_event(harness_pb2.SessionEvent(kind=harness_pb2.SESSION_EVENT_KIND_SESSION_IDLE))
        return TurnOutcome(assistant_message_id="msg-1", final_content="all done")

    async def resume(self) -> None:
        self.resumed_count += 1

    async def read_mcp_resource(self, server_name: str, uri: str):
        return [harness_pb2.MCPResourceContent(uri=uri, mime_type="text/plain", text=f"from:{server_name}")]

    async def close(self) -> None:
        self.closed = True


class _MockBackend(AgentBackend):
    def __init__(self, session_cls=None) -> None:
        self.sessions: dict[str, Any] = {}
        self.session_cls = session_cls or _MockTurnSession

    async def open_session(self, params: SessionOpenParams) -> tuple[str, bool, AgentSession]:
        if params.resume_session_id:
            if params.resume_session_id not in self.sessions:
                raise KeyError(f"session not found: {params.resume_session_id}")
            return params.resume_session_id, True, self.sessions[params.resume_session_id]
        sess = self.session_cls(params)
        self.sessions[params.session_id] = sess
        return params.session_id, False, sess


async def test_run_turn_streaming_hooks_and_anti_double_interception() -> None:
    backend = _MockBackend()
    server, channel, stub = await _start_inproc_servicer(backend)
    try:
        open_resp = await stub.OpenSession(
            harness_pb2.OpenSessionRequest(
                session_id="sess-turn",
                model="gemini-2.5-pro",
                host_tools=[harness_pb2.HostToolSpec(name="sofa_checkpoint", description="ckpt")],
            )
        )
        assert open_resp.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED

        pre_tool_names: list[str] = []
        post_tool_events: list[tuple[str, bool, str]] = []
        host_tool_names: list[str] = []
        event_kinds: list[int] = []
        final_result = None

        stream = stub.RunTurn()
        await stream.write(
            harness_pb2.HostTurnFrame(
                start_turn=harness_pb2.StartTurnRequest(
                    session_id="sess-turn",
                    turn_id="turn-1",
                    prompt="do work",
                )
            )
        )

        async for frame in stream:
            which = frame.WhichOneof("payload")
            if which == "event":
                event_kinds.append(frame.event.kind)
            elif which == "pre_tool_use":
                pre = frame.pre_tool_use
                pre_tool_names.append(pre.tool_name)
                if pre.tool_call_id == "tc-bash-1":
                    await stream.write(
                        harness_pb2.HostTurnFrame(
                            pre_tool_use_decision=harness_pb2.PreToolUseDecision(
                                hook_call_id=pre.hook_call_id,
                                allow=True,
                                modified_arguments_json=b'{"command":"ls -la"}',
                                additional_context="use safe flags",
                            )
                        )
                    )
                elif pre.tool_call_id == "tc-bash-2":
                    await stream.write(
                        harness_pb2.HostTurnFrame(
                            pre_tool_use_decision=harness_pb2.PreToolUseDecision(
                                hook_call_id=pre.hook_call_id,
                                allow=False,
                                deny_reason="destructive command denied",
                            )
                        )
                    )
                else:
                    await stream.write(
                        harness_pb2.HostTurnFrame(
                            pre_tool_use_decision=harness_pb2.PreToolUseDecision(
                                hook_call_id=pre.hook_call_id,
                                allow=True,
                            )
                        )
                    )
            elif which == "post_tool_use":
                post = frame.post_tool_use
                post_tool_events.append((post.tool_name, post.succeeded, post.error_message))
                await stream.write(
                    harness_pb2.HostTurnFrame(
                        post_tool_use_ack=harness_pb2.PostToolUseAck(hook_call_id=post.hook_call_id)
                    )
                )
            elif which == "invoke_host_tool":
                inv = frame.invoke_host_tool
                host_tool_names.append(inv.tool_name)
                await stream.write(
                    harness_pb2.HostTurnFrame(
                        invoke_host_tool_response=harness_pb2.InvokeHostToolResponse(
                            tool_call_id=inv.tool_call_id,
                            result_type=harness_pb2.HOST_TOOL_RESULT_TYPE_SUCCESS,
                            text_result_for_llm='{"accepted":true}',
                        )
                    )
                )
            elif which == "turn_result":
                final_result = frame.turn_result
                break

        await stream.done_writing()

        assert final_result is not None
        assert final_result.turn_id == "turn-1"
        assert final_result.final_content == "all done"
        assert final_result.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED

        assert harness_pb2.SESSION_EVENT_KIND_ASSISTANT_REASONING_DELTA in event_kinds
        assert harness_pb2.SESSION_EVENT_KIND_ASSISTANT_MESSAGE_DELTA in event_kinds
        assert harness_pb2.SESSION_EVENT_KIND_ASSISTANT_USAGE in event_kinds
        assert harness_pb2.SESSION_EVENT_KIND_SESSION_IDLE in event_kinds

        # Anti-double-interception: sofa_checkpoint MUST NOT appear in PreToolUse or PostToolUse.
        assert pre_tool_names == ["bash", "bash", "edit_file"]
        assert post_tool_events == [
            ("bash", True, ""),
            ("edit_file", False, "permission denied"),
        ]
        assert host_tool_names == ["sofa_checkpoint"]

        sess = backend.sessions["sess-turn"]
        assert sess.hook_outcomes[0] == ("bash-1", True, '{"command":"ls -la"}', "use safe flags")
        assert sess.hook_outcomes[1] == ("bash-2", False, "destructive command denied", "")
        assert sess.host_tool_responses[0].text_result_for_llm == '{"accepted":true}'

        # ResumeSession, ReadMCPResource, CloseSession
        res_resp = await stub.ResumeSession(harness_pb2.ResumeSessionRequest(session_id="sess-turn"))
        assert res_resp.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED
        assert sess.resumed_count == 1

        mcp_resp = await stub.ReadMCPResource(
            harness_pb2.ReadMCPResourceRequest(session_id="sess-turn", server_name="docs", uri="docs://readme")
        )
        assert len(mcp_resp.contents) == 1
        assert mcp_resp.contents[0].text == "from:docs"

        close_resp = await stub.CloseSession(harness_pb2.CloseSessionRequest(session_id="sess-turn"))
        assert close_resp.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED
        assert sess.closed is True
    finally:
        await channel.close()
        await server.stop(grace=0.5)


class _BlockingTurnSession(AgentSession):
    def __init__(self, params: SessionOpenParams) -> None:
        self.started = asyncio.Event()
        self.cancelled = False

    async def run_turn(self, turn_id: str, prompt: str, callbacks: TurnCallbacks) -> TurnOutcome:
        self.started.set()
        try:
            await asyncio.sleep(30)
            return TurnOutcome(final_content="unexpected")
        except asyncio.CancelledError:
            self.cancelled = True
            raise

    async def resume(self) -> None:
        return None

    async def read_mcp_resource(self, server_name: str, uri: str):
        return []

    async def close(self) -> None:
        return None


async def test_abort_turn_respects_turn_id_guard_and_cancels_active_task() -> None:
    backend = _MockBackend(session_cls=_BlockingTurnSession)
    server, channel, stub = await _start_inproc_servicer(backend)
    try:
        await stub.OpenSession(harness_pb2.OpenSessionRequest(session_id="sess-abort", model="gemini-2.5-pro"))
        sess: _BlockingTurnSession = backend.sessions["sess-abort"]

        stream = stub.RunTurn()
        await stream.write(
            harness_pb2.HostTurnFrame(
                start_turn=harness_pb2.StartTurnRequest(
                    session_id="sess-abort",
                    turn_id="turn-current",
                    prompt="block",
                )
            )
        )
        await asyncio.wait_for(sess.started.wait(), timeout=2.0)

        # Stale turn_id must NOT cancel active turn.
        stale_abort = await stub.AbortTurn(
            harness_pb2.AbortTurnRequest(session_id="sess-abort", turn_id="turn-stale")
        )
        assert stale_abort.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED
        assert not sess.cancelled

        # Matching turn_id cancels immediately and classifies as INTERNAL (not TRANSIENT).
        real_abort = await stub.AbortTurn(
            harness_pb2.AbortTurnRequest(session_id="sess-abort", turn_id="turn-current")
        )
        assert real_abort.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED

        final_result = None
        async for frame in stream:
            if frame.WhichOneof("payload") == "turn_result":
                final_result = frame.turn_result
                break
        await stream.done_writing()

        assert sess.cancelled is True
        assert final_result is not None
        assert final_result.error.category == harness_pb2.ERROR_CATEGORY_INTERNAL
    finally:
        await channel.close()
        await server.stop(grace=0.5)


def test_error_classification_mapping() -> None:
    class _StatusErr(Exception):
        def __init__(self, status_code: int, message: str) -> None:
            super().__init__(message)
            self.status_code = status_code

    err429 = classify_exception(_StatusErr(429, "Too Many Requests"))
    assert err429.category == harness_pb2.ERROR_CATEGORY_RATE_LIMIT
    assert err429.http_status_code == 429

    err401 = classify_exception(_StatusErr(401, "Invalid API key"))
    assert err401.category == harness_pb2.ERROR_CATEGORY_AUTHENTICATION
    assert err401.http_status_code == 401

    err403 = classify_exception(_StatusErr(403, "Permission denied"))
    assert err403.category == harness_pb2.ERROR_CATEGORY_AUTHORIZATION
    assert err403.http_status_code == 403

    err_quota = classify_exception(RuntimeError("insufficient quota exceeded"))
    assert err_quota.category == harness_pb2.ERROR_CATEGORY_QUOTA

    err_ctx = classify_exception(RuntimeError("context window token limit exceeded"))
    assert err_ctx.category == harness_pb2.ERROR_CATEGORY_CONTEXT_LIMIT

    err_503 = classify_exception(_StatusErr(503, "Service Unavailable"))
    assert err_503.category == harness_pb2.ERROR_CATEGORY_TRANSIENT
    assert err_503.http_status_code == 503


def test_google_antigravity_backend_config_mapping_and_env_isolation() -> None:
    from google.antigravity import types as ag_types

    backend = GoogleAntigravityBackend()
    os.environ.pop("GEMINI_API_KEY", None)

    params_ro = SessionOpenParams(
        session_id="s-ro",
        resume_session_id="",
        model="gemini-2.5-pro",
        profile="gemini-2.5-pro",
        reasoning_effort="high",
        system_prompt="Be careful",
        working_directory="/workspace",
        session_store_directory="/.sofa/session-store",
        permission_mode="read_only",
        harness_env={"GEMINI_API_KEY": "secret-gemini-key", "EXTRA_FLAG": "1"},
        harness_options={},
        host_tools=[
            harness_pb2.HostToolSpec(
                name="sofa_qc_submit_verdict",
                description="submit verdict",
                parameters_json_schema=b'{"type":"object","properties":{"decision":{"type":"string"}}}',
            )
        ],
        available_tools_configured=False,
        available_tools=[],
        excluded_tools_configured=False,
        excluded_tools=[],
        skill_directories=["/.sofa/bundle/skills"],
        skills=[],
        disabled_skills=[],
        mcp_servers=[],
    )

    cfg_ro = backend.build_agent_config(
        params_ro,
        tool_callables=[],
        hooks=[],
    )
    assert "GEMINI_API_KEY" not in os.environ
    assert cfg_ro.api_key == "secret-gemini-key"
    assert cfg_ro.conversation_id is not None and len(cfg_ro.conversation_id) >= 32
    assert os.path.normcase(cfg_ro.workspaces[0]) == os.path.normcase(os.path.abspath("/workspace"))
    assert cfg_ro.capabilities.disabled_tools is not None
    assert ag_types.BuiltinTools.EDIT_FILE in cfg_ro.capabilities.disabled_tools
    assert ag_types.BuiltinTools.CREATE_FILE in cfg_ro.capabilities.disabled_tools
    assert ag_types.BuiltinTools.RUN_COMMAND in cfg_ro.capabilities.disabled_tools

    params_rw = SessionOpenParams(
        session_id="s-rw",
        resume_session_id="",
        model="gemini-2.5-pro",
        profile="gemini-2.5-pro",
        reasoning_effort="medium",
        system_prompt="Implement",
        working_directory="/workspace",
        session_store_directory="/.sofa/session-store",
        permission_mode="approve_all",
        harness_env={"GEMINI_API_KEY": "secret-gemini-key"},
        harness_options={},
        host_tools=[],
        available_tools_configured=False,
        available_tools=[],
        excluded_tools_configured=False,
        excluded_tools=[],
        skill_directories=[],
        skills=[],
        disabled_skills=[],
        mcp_servers=[],
    )
    cfg_rw = backend.build_agent_config(params_rw, tool_callables=[], hooks=[])
    assert any(getattr(p, "name", "") == "allow_all" for p in cfg_rw.policies)
    assert cfg_rw.capabilities.disabled_tools is None or ag_types.BuiltinTools.EDIT_FILE not in cfg_rw.capabilities.disabled_tools


async def test_google_antigravity_host_tool_callables_avoid_late_binding_closure() -> None:
    params = SessionOpenParams(
        session_id="sess-closure",
        resume_session_id="",
        model="gemini-2.5-pro",
        profile="gemini-2.5-pro",
        reasoning_effort="",
        system_prompt="",
        working_directory="/workspace",
        session_store_directory="",
        permission_mode="approve_all",
        harness_env={},
        harness_options={},
        host_tools=[
            harness_pb2.HostToolSpec(
                name="sofa_checkpoint",
                description="checkpoint tool",
                parameters_json_schema=b'{"type":"object"}',
            ),
            harness_pb2.HostToolSpec(
                name="sofa_complete_work",
                description="complete tool",
                parameters_json_schema=b'{"type":"object"}',
            ),
        ],
        available_tools_configured=False,
        available_tools=[],
        excluded_tools_configured=False,
        excluded_tools=[],
        skill_directories=[],
        skills=[],
        disabled_skills=[],
        mcp_servers=[],
    )

    recorded_calls: list[tuple[str, bytes]] = []

    class _RecordingCallbacks:
        async def emit_event(self, event: harness_pb2.SessionEvent) -> None:
            return None

        async def invoke_host_tool(
            self, tool_call_id: str, tool_name: str, arguments_json: bytes
        ) -> harness_pb2.InvokeHostToolResponse:
            recorded_calls.append((tool_name, arguments_json))
            return harness_pb2.InvokeHostToolResponse(
                tool_call_id=tool_call_id,
                result_type=harness_pb2.HOST_TOOL_RESULT_TYPE_SUCCESS,
                text_result_for_llm=f"ok:{tool_name}",
            )

        async def pre_tool_use(self, tool_call_id: str, tool_name: str, arguments_json: bytes):
            raise AssertionError("host tools must not call pre_tool_use")

        async def post_tool_use(self, tool_call_id: str, tool_name: str, succeeded: bool, error_message: str):
            raise AssertionError("host tools must not call post_tool_use")

    session = GoogleAntigravitySession(GoogleAntigravityBackend(), params)
    session._callbacks = _RecordingCallbacks()
    callables = session._build_host_tool_callables()
    assert len(callables) == 2

    res1 = await callables[0](summary="step-1")
    res2 = await callables[1](summary="done")

    assert res1 == "ok:sofa_checkpoint"
    assert res2 == "ok:sofa_complete_work"
    assert [name for name, _ in recorded_calls] == ["sofa_checkpoint", "sofa_complete_work"]


async def test_cross_container_resume_via_session_store_directory(tmp_path, monkeypatch: pytest.MonkeyPatch) -> None:
    store_dir = str(tmp_path / "session-store")

    # 1. ScriptedFakeBackend: fresh instance resumes via .marker file in session_store_directory.
    servicer1 = AgentHarnessServicer(backend=ScriptedFakeBackend())
    open1 = await servicer1.OpenSession(
        harness_pb2.OpenSessionRequest(
            session_id="sess-rework-1",
            model="gemini-2.5-pro",
            session_store_directory=store_dir,
        ),
        _DummyContext(),
    )
    assert open1.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED

    servicer2 = AgentHarnessServicer(backend=ScriptedFakeBackend())
    resumed_ok = await servicer2.OpenSession(
        harness_pb2.OpenSessionRequest(
            session_id="sess-rework-1",
            resume_session_id="sess-rework-1",
            model="gemini-2.5-pro",
            session_store_directory=store_dir,
        ),
        _DummyContext(),
    )
    assert resumed_ok.error.category == harness_pb2.ERROR_CATEGORY_UNSPECIFIED
    assert resumed_ok.resumed is True
    assert resumed_ok.session_id == "sess-rework-1"

    missing_res = await servicer2.OpenSession(
        harness_pb2.OpenSessionRequest(
            session_id="sess-new",
            resume_session_id="absent-session",
            model="gemini-2.5-pro",
            session_store_directory=store_dir,
        ),
        _DummyContext(),
    )
    assert missing_res.error.category == harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND

    # 2. GoogleAntigravityBackend: cross-process resume requires session_store_directory and removes on close().
    started_ids: list[str] = []

    async def _fake_start(self: GoogleAntigravitySession) -> None:
        started_ids.append(self._params.resume_session_id or self._params.session_id)
        self._entered = False

    monkeypatch.setattr(GoogleAntigravitySession, "start", _fake_start)

    ag_backend = GoogleAntigravityBackend()
    with pytest.raises(KeyError):
        await ag_backend.open_session(
            SessionOpenParams(
                session_id="s-no-dir",
                resume_session_id="s-no-dir",
                model="gemini-2.5-pro",
                profile="",
                reasoning_effort="",
                system_prompt="",
                working_directory="/workspace",
                session_store_directory="",
                permission_mode="approve_all",
                harness_env={},
                harness_options={},
                host_tools=[],
                available_tools_configured=False,
                available_tools=[],
                excluded_tools_configured=False,
                excluded_tools=[],
                skill_directories=[],
                skills=[],
                disabled_skills=[],
                mcp_servers=[],
            )
        )

    sid, is_resumed, sess_obj = await ag_backend.open_session(
        SessionOpenParams(
            session_id="s-with-dir",
            resume_session_id="s-with-dir",
            model="gemini-2.5-pro",
            profile="",
            reasoning_effort="",
            system_prompt="",
            working_directory="/workspace",
            session_store_directory=store_dir,
            permission_mode="approve_all",
            harness_env={},
            harness_options={},
            host_tools=[],
            available_tools_configured=False,
            available_tools=[],
            excluded_tools_configured=False,
            excluded_tools=[],
            skill_directories=[],
            skills=[],
            disabled_skills=[],
            mcp_servers=[],
        )
    )
    assert sid == "s-with-dir"
    assert is_resumed is True
    assert "s-with-dir" in ag_backend._sessions
    await sess_obj.close()
    assert "s-with-dir" not in ag_backend._sessions
    assert started_ids == ["s-with-dir"]


def test_google_antigravity_skills_lenient_eager_loading_and_disabled_isolation(tmp_path) -> None:
    skills_dir = tmp_path / "bundle_skills"
    alpha_dir = skills_dir / "alpha"
    alpha_dir.mkdir(parents=True)
    (alpha_dir / "SKILL.md").write_text("---\nname: \"alpha-custom\"\n---\nAlways follow alpha rules.\n", encoding="utf-8")
    scripts_dir = alpha_dir / "scripts"
    scripts_dir.mkdir()
    helper_sh = scripts_dir / "helper.sh"
    helper_sh.write_text("#!/bin/sh\necho helper\n", encoding="utf-8")
    os.chmod(helper_sh, 0o755)

    disabled_dir = skills_dir / "bad_skill"
    disabled_dir.mkdir(parents=True)
    (disabled_dir / "SKILL.md").write_text("---\nname: bad_skill\n---\nNever load me.\n", encoding="utf-8")

    store_dir = tmp_path / "session-store"
    backend = GoogleAntigravityBackend()
    params = SessionOpenParams(
        session_id="s-skills",
        resume_session_id="",
        model="gemini-2.5-pro",
        profile="gemini-2.5-pro",
        reasoning_effort="medium",
        system_prompt="Base system prompt.",
        working_directory="/workspace",
        session_store_directory=str(store_dir),
        permission_mode="approve_all",
        harness_env={},
        harness_options={},
        host_tools=[],
        available_tools_configured=False,
        available_tools=[],
        excluded_tools_configured=False,
        excluded_tools=[],
        skill_directories=[str(skills_dir), str(tmp_path / "missing_optional_dir")],
        skills=["alpha", "bad_skill", "non_existent_skill"],
        disabled_skills=["bad_skill"],
        mcp_servers=[],
    )

    cfg = backend.build_agent_config(params, tool_callables=[], hooks=[])

    # LocalHarness forbids combining inline_skills and skills_paths.
    assert not cfg.inline_skills
    assert cfg.skills_paths is not None and len(cfg.skills_paths) == 2

    staged_dir = cfg.skills_paths[0]
    assert staged_dir != str(skills_dir)
    assert ".sofa-filtered-skills" in staged_dir
    assert (os.path.isdir(os.path.join(staged_dir, "alpha")))
    assert not os.path.exists(os.path.join(staged_dir, "bad_skill"))
    assert cfg.skills_paths[1] == str(tmp_path / "missing_optional_dir")

    staged_helper = os.path.join(staged_dir, "alpha", "scripts", "helper.sh")
    assert os.path.isfile(staged_helper)
    if os.name != "nt":
        assert os.stat(staged_helper).st_mode & 0o111

    sys_instr = cfg.system_instructions
    assert isinstance(sys_instr, str)
    assert sys_instr.startswith("Base system prompt.")
    assert "Active Skill (alpha-custom)" in sys_instr
    assert "Always follow alpha rules." in sys_instr
    assert os.path.join(staged_dir, "alpha") in sys_instr
    assert "Never load me." not in sys_instr
    assert "bad_skill" not in sys_instr
    assert "non_existent_skill" not in sys_instr


async def test_google_antigravity_fallback_temp_stage_root_cleaned_up_on_close(tmp_path) -> None:
    skills_dir = tmp_path / "bundle_skills"
    (skills_dir / "keep").mkdir(parents=True)
    (skills_dir / "keep" / "SKILL.md").write_text("# Keep\n", encoding="utf-8")
    (skills_dir / "drop").mkdir(parents=True)
    (skills_dir / "drop" / "SKILL.md").write_text("# Drop\n", encoding="utf-8")

    fake_backend = ScriptedFakeBackend()
    _, _, fake_sess = await fake_backend.open_session(
        SessionOpenParams(
            session_id="s-fake-cleanup",
            resume_session_id="",
            model="gemini-2.5-pro",
            profile="gemini-2.5-pro",
            reasoning_effort="",
            system_prompt="",
            working_directory="/workspace",
            session_store_directory="",
            permission_mode="approve_all",
            harness_env={},
            harness_options={},
            host_tools=[],
            available_tools_configured=False,
            available_tools=[],
            excluded_tools_configured=False,
            excluded_tools=[],
            skill_directories=[str(skills_dir)],
            skills=["keep", "drop"],
            disabled_skills=["drop"],
            mcp_servers=[],
        )
    )
    temp_root = getattr(fake_sess, "_temp_stage_root", None)
    assert temp_root is not None and os.path.isdir(temp_root)
    await fake_sess.close()
    assert not os.path.exists(temp_root)

    ag_backend = GoogleAntigravityBackend()
    ag_sess = GoogleAntigravitySession(
        ag_backend,
        SessionOpenParams(
            session_id="s-ag-cleanup",
            resume_session_id="",
            model="gemini-2.5-pro",
            profile="gemini-2.5-pro",
            reasoning_effort="",
            system_prompt="",
            working_directory="/workspace",
            session_store_directory="",
            permission_mode="approve_all",
            harness_env={},
            harness_options={},
            host_tools=[],
            available_tools_configured=False,
            available_tools=[],
            excluded_tools_configured=False,
            excluded_tools=[],
            skill_directories=[str(skills_dir)],
            skills=["keep", "drop"],
            disabled_skills=["drop"],
            mcp_servers=[],
        ),
    )
    _, ag_temp_root = ag_backend._build_agent_config_with_stage_root(ag_sess._params, tool_callables=[], hooks=[])
    ag_sess._temp_stage_root = ag_temp_root
    assert ag_temp_root is not None and os.path.isdir(ag_temp_root)
    await ag_sess.close()
    assert not os.path.exists(ag_temp_root)
