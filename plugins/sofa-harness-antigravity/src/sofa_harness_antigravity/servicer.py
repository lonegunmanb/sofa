"""Async 7-RPC AgentHarnessServicer implementation over grpc.aio."""

from __future__ import annotations

import asyncio
import json
import uuid
from dataclasses import dataclass, field
from typing import Any, AsyncIterator

from sofa.harness.v1 import harness_pb2, harness_pb2_grpc
from sofa_harness_antigravity import __version__
from sofa_harness_antigravity.backend import (
    AgentBackend,
    AgentSession,
    HybridBackend,
    SessionOpenParams,
    TurnOutcome,
    classify_exception,
)


@dataclass
class _ActiveTurn:
    turn_id: str
    outbound_queue: asyncio.Queue[harness_pb2.PluginTurnFrame | None] = field(default_factory=asyncio.Queue)
    pending_hooks: dict[str, asyncio.Future[harness_pb2.HostTurnFrame]] = field(default_factory=dict)
    pending_tools: dict[str, asyncio.Future[harness_pb2.InvokeHostToolResponse]] = field(default_factory=dict)
    turn_task: asyncio.Task[None] | None = None
    aborted: bool = False


@dataclass
class _ManagedSession:
    session_id: str
    backend_session: AgentSession
    host_tool_names: set[str]
    turn_lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    active_turn: _ActiveTurn | None = None


class _StreamTurnCallbacks:
    """TurnCallbacks bridging backend events, hooks, and host tools onto RunTurn queue."""

    def __init__(self, turn: _ActiveTurn, loop: asyncio.AbstractEventLoop) -> None:
        self._turn = turn
        self._loop = loop

    async def emit_event(self, event: harness_pb2.SessionEvent) -> None:
        await self._turn.outbound_queue.put(harness_pb2.PluginTurnFrame(event=event))

    async def invoke_host_tool(
        self, tool_call_id: str, tool_name: str, arguments_json: bytes
    ) -> harness_pb2.InvokeHostToolResponse:
        call_id = tool_call_id or f"tc-{uuid.uuid4().hex[:12]}"
        fut: asyncio.Future[harness_pb2.InvokeHostToolResponse] = self._loop.create_future()
        self._turn.pending_tools[call_id] = fut
        try:
            await self._turn.outbound_queue.put(
                harness_pb2.PluginTurnFrame(
                    invoke_host_tool=harness_pb2.InvokeHostToolRequest(
                        tool_call_id=call_id,
                        tool_name=tool_name,
                        arguments_json=arguments_json,
                    )
                )
            )
            return await fut
        finally:
            self._turn.pending_tools.pop(call_id, None)

    async def pre_tool_use(
        self, tool_call_id: str, tool_name: str, arguments_json: bytes
    ) -> harness_pb2.PreToolUseDecision:
        hook_id = f"hk-pre-{uuid.uuid4().hex[:12]}"
        fut: asyncio.Future[harness_pb2.HostTurnFrame] = self._loop.create_future()
        self._turn.pending_hooks[hook_id] = fut
        try:
            await self._turn.outbound_queue.put(
                harness_pb2.PluginTurnFrame(
                    pre_tool_use=harness_pb2.PreToolUseRequest(
                        hook_call_id=hook_id,
                        tool_call_id=tool_call_id,
                        tool_name=tool_name,
                        arguments_json=arguments_json,
                    )
                )
            )
            frame = await fut
            return frame.pre_tool_use_decision
        finally:
            self._turn.pending_hooks.pop(hook_id, None)

    async def post_tool_use(
        self, tool_call_id: str, tool_name: str, succeeded: bool, error_message: str
    ) -> None:
        hook_id = f"hk-post-{uuid.uuid4().hex[:12]}"
        fut: asyncio.Future[harness_pb2.HostTurnFrame] = self._loop.create_future()
        self._turn.pending_hooks[hook_id] = fut
        try:
            await self._turn.outbound_queue.put(
                harness_pb2.PluginTurnFrame(
                    post_tool_use=harness_pb2.PostToolUseRequest(
                        hook_call_id=hook_id,
                        tool_call_id=tool_call_id,
                        tool_name=tool_name,
                        succeeded=succeeded,
                        error_message=error_message,
                    )
                )
            )
            await fut
        finally:
            self._turn.pending_hooks.pop(hook_id, None)


class AgentHarnessServicer(harness_pb2_grpc.AgentHarnessServicer):
    """Implements all 7 RPCs of sofa.harness.v1.AgentHarness."""

    def __init__(self, backend: AgentBackend | None = None, version: str = __version__) -> None:
        self._backend: AgentBackend = backend or HybridBackend()
        self._version = version
        self._sessions: dict[str, _ManagedSession] = {}

    async def GetCapabilities(
        self, request: harness_pb2.GetCapabilitiesRequest, context: Any
    ) -> harness_pb2.GetCapabilitiesResponse:
        return harness_pb2.GetCapabilitiesResponse(
            harness_name="antigravity",
            harness_version=self._version,
            protocol_version=1,
            supported_provider_types=[],
            supports_session_resume=True,
            supports_mcp_servers=True,
            supports_mcp_resources=False,
            supports_skills=True,
        )

    async def OpenSession(
        self, request: harness_pb2.OpenSessionRequest, context: Any
    ) -> harness_pb2.OpenSessionResponse:
        if request.HasField("provider") and request.provider.type.strip():
            return harness_pb2.OpenSessionResponse(
                error=harness_pb2.ErrorDetail(
                    category=harness_pb2.ERROR_CATEGORY_UNSUPPORTED_CAPABILITY,
                    http_status_code=400,
                    message=f"antigravity harness does not support external provider {request.provider.type!r}",
                )
            )

        harness_options: dict[str, Any] = {}
        if request.harness_options_json:
            try:
                loaded = json.loads(request.harness_options_json.decode("utf-8"))
                if isinstance(loaded, dict):
                    harness_options = loaded
            except Exception as exc:
                return harness_pb2.OpenSessionResponse(
                    error=harness_pb2.ErrorDetail(
                        category=harness_pb2.ERROR_CATEGORY_INTERNAL,
                        message=f"invalid harness_options_json: {exc}",
                    )
                )

        params = SessionOpenParams(
            session_id=request.session_id,
            resume_session_id=request.resume_session_id,
            model=request.model,
            profile=request.profile,
            reasoning_effort=request.reasoning_effort,
            system_prompt=request.system_prompt,
            working_directory=request.working_directory,
            session_store_directory=request.session_store_directory,
            permission_mode=request.permission_mode,
            harness_env=dict(request.harness_env),
            harness_options=harness_options,
            host_tools=list(request.host_tools),
            available_tools_configured=request.HasField("available_tools") and request.available_tools.configured,
            available_tools=list(request.available_tools.names) if request.HasField("available_tools") else [],
            excluded_tools_configured=request.HasField("excluded_tools") and request.excluded_tools.configured,
            excluded_tools=list(request.excluded_tools.names) if request.HasField("excluded_tools") else [],
            skill_directories=list(request.skill_directories),
            skills=list(request.skills),
            disabled_skills=list(request.disabled_skills),
            mcp_servers=list(request.mcp_servers),
        )

        try:
            sess_id, resumed, backend_session = await self._backend.open_session(params)
        except Exception as exc:
            return harness_pb2.OpenSessionResponse(error=classify_exception(exc))

        displaced = self._sessions.get(sess_id)
        if displaced is not None and displaced.backend_session is not backend_session:
            try:
                await displaced.backend_session.close()
            except Exception:
                pass

        self._sessions[sess_id] = _ManagedSession(
            session_id=sess_id,
            backend_session=backend_session,
            host_tool_names={spec.name for spec in params.host_tools},
        )
        return harness_pb2.OpenSessionResponse(session_id=sess_id, resumed=resumed)

    async def RunTurn(
        self,
        request_iterator: AsyncIterator[harness_pb2.HostTurnFrame],
        context: Any,
    ) -> AsyncIterator[harness_pb2.PluginTurnFrame]:
        req_iter = aiter(request_iterator)
        try:
            first_frame = await anext(req_iter)
        except StopAsyncIteration:
            yield harness_pb2.PluginTurnFrame(
                turn_result=harness_pb2.TurnResult(
                    error=harness_pb2.ErrorDetail(
                        category=harness_pb2.ERROR_CATEGORY_INTERNAL,
                        message="RunTurn stream closed before StartTurnRequest",
                    )
                )
            )
            return

        if first_frame.WhichOneof("payload") != "start_turn":
            yield harness_pb2.PluginTurnFrame(
                turn_result=harness_pb2.TurnResult(
                    error=harness_pb2.ErrorDetail(
                        category=harness_pb2.ERROR_CATEGORY_INTERNAL,
                        message="first RunTurn frame must be StartTurnRequest",
                    )
                )
            )
            return

        start = first_frame.start_turn
        session = self._sessions.get(start.session_id)
        if session is None:
            yield harness_pb2.PluginTurnFrame(
                turn_result=harness_pb2.TurnResult(
                    turn_id=start.turn_id,
                    error=harness_pb2.ErrorDetail(
                        category=harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND,
                        http_status_code=404,
                        message=f"session not found: {start.session_id}",
                    ),
                )
            )
            return

        async with session.turn_lock:
            loop = asyncio.get_running_loop()
            turn = _ActiveTurn(turn_id=start.turn_id)
            session.active_turn = turn
            callbacks = _StreamTurnCallbacks(turn, loop)

            async def _read_host_frames() -> None:
                try:
                    async for frame in req_iter:
                        which = frame.WhichOneof("payload")
                        if which == "pre_tool_use_decision":
                            hc_id = frame.pre_tool_use_decision.hook_call_id
                            fut = turn.pending_hooks.get(hc_id)
                            if fut is not None and not fut.done():
                                fut.set_result(frame)
                        elif which == "post_tool_use_ack":
                            hc_id = frame.post_tool_use_ack.hook_call_id
                            fut = turn.pending_hooks.get(hc_id)
                            if fut is not None and not fut.done():
                                fut.set_result(frame)
                        elif which == "invoke_host_tool_response":
                            resp = frame.invoke_host_tool_response
                            fut = turn.pending_tools.get(resp.tool_call_id)
                            if fut is not None and not fut.done():
                                fut.set_result(resp)
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    for fut in list(turn.pending_hooks.values()):
                        if not fut.done():
                            fut.set_exception(exc)
                    for fut in list(turn.pending_tools.values()):
                        if not fut.done():
                            fut.set_exception(exc)

            async def _execute_turn() -> None:
                outcome: TurnOutcome | None = None
                err_detail: harness_pb2.ErrorDetail | None = None
                try:
                    outcome = await session.backend_session.run_turn(start.turn_id, start.prompt, callbacks)
                except asyncio.CancelledError as exc:
                    err_detail = classify_exception(exc, aborted=True)
                except Exception as exc:
                    err_detail = classify_exception(exc, aborted=turn.aborted)
                finally:
                    for fut in list(turn.pending_hooks.values()):
                        if not fut.done():
                            fut.cancel()
                    for fut in list(turn.pending_tools.values()):
                        if not fut.done():
                            fut.cancel()

                    result_msg = harness_pb2.TurnResult(
                        turn_id=start.turn_id,
                        assistant_message_id=outcome.assistant_message_id if outcome else "",
                        final_content=outcome.final_content if outcome else "",
                        error=err_detail,
                    )
                    await turn.outbound_queue.put(harness_pb2.PluginTurnFrame(turn_result=result_msg))
                    await turn.outbound_queue.put(None)

            reader_task = asyncio.create_task(_read_host_frames())
            turn_task = asyncio.create_task(_execute_turn())
            turn.turn_task = turn_task

            try:
                while True:
                    frame = await turn.outbound_queue.get()
                    if frame is None:
                        break
                    yield frame
            finally:
                if not turn_task.done():
                    turn_task.cancel()
                    await asyncio.gather(turn_task, return_exceptions=True)
                if not reader_task.done():
                    reader_task.cancel()
                    await asyncio.gather(reader_task, return_exceptions=True)
                if session.active_turn is turn:
                    session.active_turn = None

    async def AbortTurn(
        self, request: harness_pb2.AbortTurnRequest, context: Any
    ) -> harness_pb2.AbortTurnResponse:
        session = self._sessions.get(request.session_id)
        if session is None:
            return harness_pb2.AbortTurnResponse(
                error=harness_pb2.ErrorDetail(
                    category=harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND,
                    http_status_code=404,
                    message=f"session not found: {request.session_id}",
                )
            )
        turn = session.active_turn
        if turn is not None and (not request.turn_id or turn.turn_id == request.turn_id):
            turn.aborted = True
            if turn.turn_task is not None and not turn.turn_task.done():
                turn.turn_task.cancel()
        return harness_pb2.AbortTurnResponse()

    async def ResumeSession(
        self, request: harness_pb2.ResumeSessionRequest, context: Any
    ) -> harness_pb2.ResumeSessionResponse:
        session = self._sessions.get(request.session_id)
        if session is None:
            return harness_pb2.ResumeSessionResponse(
                error=harness_pb2.ErrorDetail(
                    category=harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND,
                    http_status_code=404,
                    message=f"session not found: {request.session_id}",
                )
            )
        try:
            await session.backend_session.resume()
        except Exception as exc:
            return harness_pb2.ResumeSessionResponse(error=classify_exception(exc))
        return harness_pb2.ResumeSessionResponse()

    async def ReadMCPResource(
        self, request: harness_pb2.ReadMCPResourceRequest, context: Any
    ) -> harness_pb2.ReadMCPResourceResponse:
        session = self._sessions.get(request.session_id)
        if session is None:
            return harness_pb2.ReadMCPResourceResponse(
                error=harness_pb2.ErrorDetail(
                    category=harness_pb2.ERROR_CATEGORY_SESSION_NOT_FOUND,
                    http_status_code=404,
                    message=f"session not found: {request.session_id}",
                )
            )
        try:
            contents = await session.backend_session.read_mcp_resource(request.server_name, request.uri)
            return harness_pb2.ReadMCPResourceResponse(contents=contents)
        except Exception as exc:
            return harness_pb2.ReadMCPResourceResponse(error=classify_exception(exc))

    async def CloseSession(
        self, request: harness_pb2.CloseSessionRequest, context: Any
    ) -> harness_pb2.CloseSessionResponse:
        session = self._sessions.pop(request.session_id, None)
        if session is None:
            return harness_pb2.CloseSessionResponse()
        try:
            await session.backend_session.close()
        except Exception as exc:
            return harness_pb2.CloseSessionResponse(error=classify_exception(exc))
        return harness_pb2.CloseSessionResponse()
