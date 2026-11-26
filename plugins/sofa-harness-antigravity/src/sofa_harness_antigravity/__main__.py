"""CLI entrypoint for sofa-harness-antigravity gRPC plugin."""

from __future__ import annotations

import asyncio
import signal
import sys

from sofa_harness_antigravity.handshake import (
    HandshakeError,
    configure_stderr_logging,
    prepare_handshake,
    start_grpc_server,
)
from sofa_harness_antigravity.servicer import AgentHarnessServicer


async def _serve() -> int:
    configure_stderr_logging()
    try:
        prepared = prepare_handshake()
    except HandshakeError as exc:
        sys.stderr.write(f"{exc}\n")
        sys.stderr.flush()
        return 1

    servicer = AgentHarnessServicer()
    server, _, _ = await start_grpc_server(servicer=servicer, prepared=prepared)

    stop_event = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig_name in ("SIGINT", "SIGTERM"):
        sig = getattr(signal, sig_name, None)
        if sig is not None:
            try:
                loop.add_signal_handler(sig, stop_event.set)
            except NotImplementedError:
                pass

    wait_task = asyncio.create_task(server.wait_for_termination())
    stop_task = asyncio.create_task(stop_event.wait())
    done, pending = await asyncio.wait(
        {wait_task, stop_task},
        return_when=asyncio.FIRST_COMPLETED,
    )
    if stop_task in done:
        await server.stop(grace=2.0)
    for task in pending:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
    return 0


def main() -> None:
    raise SystemExit(asyncio.run(_serve()))


if __name__ == "__main__":
    main()
