"""The engine sidecar: the analysis engine behind the Go host (ADR-0023 H2).

The Go sub-application host serves the SPI, merges parameters, checks the
clone destination, composes the result and uploads it. What it cannot carry
is the engine — the copied tool layer and its dependency closure — so that
stays here, in this process, listening on a Unix socket next to the host:

    POST /engine/invoke                  {invocation_id, tool, arguments}
      → application/x-ndjson: {"thinking": …} and {"token": …} interleaved,
        then {"result": …} | {"error": …}
    POST /engine/invocations/{id}/stop   a cooperative stop
    GET  /engine/health

``thinking`` is progress text and ``token`` is a fragment of the ANSWER; they
share one queue so their order is the order the engine produced them in, and
they are separate keys so that neither hop below has to guess which one a
line is (issue #701, ``conformance/provider/fixtures/deepwiki/stream/
token_events.json``).

``arguments`` is the legacy keyword set the host already derived
(``ArgumentsFor``); the engine call is exactly the one ``LegacyToolRunner``
makes. The runner is the one ``ELITEA_DEEPWIKI_RUNNER`` names — ``legacy``
for the engine, ``fixture`` for a stack that proves the hop without it.
Publishing the generated index (``_publish``) happens here, engine-side,
where the storage code lives.

No SPI here, no descriptor, no identity headers: the socket is reachable
only from the host's pod (a shared emptyDir, or a shared compose volume).
"""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any, AsyncIterator

from fastapi import FastAPI, HTTPException
from fastapi.responses import StreamingResponse

from .errors import InvocationCancelled, classify

logger = logging.getLogger(__name__)

#: What the sidecar will run. ``resolve_wiki`` joined the three when the
#: wiki_query family was ported (ADR-0022 parity): the family is served by
#: the Go host over the artifact bucket, and only the step that needs a
#: MODEL — which wiki a free-text question is about — crosses this socket.
#: See :mod:`elitea_deepwiki.wiki_query`.
ENGINE_TOOLS = ("generate_wiki", "ask", "deep_research", "resolve_wiki")


class SidecarContext:
    """The hooks the tool layer needs, for one sidecar invocation.

    ``thinking`` feeds the progress stream and ``token`` feeds the answer
    stream; ``checkpoint`` raises once the host has asked for a stop, after
    terminating the tracked worker processes — exactly what the legacy
    ``InvocationContext`` did, without an invocation store behind it.
    """

    def __init__(self, invocation_id: str, tool_name: str, queue: asyncio.Queue) -> None:
        self._invocation_id = invocation_id
        self._tool_name = tool_name
        self._queue = queue
        self.stop_requested = False
        self._processes: list[Any] = []

    @property
    def invocation_id(self) -> str:
        return self._invocation_id

    @property
    def toolkit_name(self) -> str:
        return "Wikis"

    @property
    def tool_name(self) -> str:
        return self._tool_name

    async def thinking(self, message: str) -> None:
        await self._queue.put({"thinking": message})

    async def token(self, text: str) -> None:
        """One fragment of the answer, on the same queue as the progress.

        An empty fragment is DROPPED here rather than downstream: the poll
        drain discards an event with an empty message, so letting one
        through would spend a read-once event slot on nothing.
        """
        if not text:
            return
        await self._queue.put({"token": text})

    async def checkpoint(self) -> None:
        if not self.stop_requested:
            return
        self._terminate_processes()
        raise InvocationCancelled(self._invocation_id)

    def process_add(self, process) -> None:
        self._processes.append(process)

    def process_remove(self, process) -> None:
        try:
            self._processes.remove(process)
        except ValueError:
            pass

    def _terminate_processes(self) -> None:
        for process in list(self._processes):
            try:
                if process.poll() is not None:
                    continue
                process.terminate()
                try:
                    process.wait(timeout=3)
                except Exception:  # noqa: BLE001 - fall through to kill
                    process.kill()
            except Exception:  # noqa: BLE001 - a dead handle is not an error
                logger.debug("failed to terminate a tracked process", exc_info=True)


def _error_line(exc: BaseException) -> dict[str, Any]:
    return {
        "error": {
            "message": str(exc) or exc.__class__.__name__,
            "error_type": exc.__class__.__name__,
            "error_category": classify(exc),
        }
    }


def create_sidecar(settings, runner=None) -> FastAPI:
    """The sidecar app over ``runner`` (default: the one the settings name)."""
    from .toolrunner import build_runner  # noqa: PLC0415

    if runner is None:
        runner = build_runner(settings)
    app = FastAPI(title="elitea-deepwiki engine sidecar", docs_url=None, redoc_url=None)
    contexts: dict[str, SidecarContext] = {}

    @app.get("/engine/health")
    async def health() -> dict[str, Any]:
        return {"status": "UP", "runner": getattr(runner, "name", "unknown"), "active": len(contexts)}

    @app.post("/engine/invocations/{invocation_id}/stop", status_code=202)
    async def stop(invocation_id: str) -> dict[str, Any]:
        context = contexts.get(invocation_id)
        if context is None:
            return {"stopped": False}
        context.stop_requested = True
        return {"stopped": True}

    @app.post("/engine/invoke")
    async def invoke(request: dict[str, Any]) -> StreamingResponse:
        invocation_id = request.get("invocation_id")
        tool = request.get("tool")
        arguments = request.get("arguments")
        if not isinstance(invocation_id, str) or not invocation_id:
            raise HTTPException(status_code=400, detail="invocation_id is required")
        if tool not in ENGINE_TOOLS:
            raise HTTPException(status_code=400, detail=f"Unknown tool: {tool}")
        if not isinstance(arguments, dict):
            raise HTTPException(status_code=400, detail="arguments must be an object")
        if invocation_id in contexts:
            raise HTTPException(status_code=409, detail=f"{invocation_id} is already running")

        queue: asyncio.Queue = asyncio.Queue()
        context = SidecarContext(invocation_id, tool, queue)
        contexts[invocation_id] = context

        async def run_tool() -> None:
            try:
                result = await runner.run_engine_tool(tool, arguments, context)
                if tool == "generate_wiki" and isinstance(result, dict) and result.get("success"):
                    await runner.publish(result, context)
                await queue.put({"result": result})
            except InvocationCancelled:
                await queue.put({"error": {"message": "Invocation cancelled", "error_type": "RuntimeError", "error_category": "runtime_error"}})
            except BaseException as exc:  # noqa: BLE001 - reported on the stream
                logger.exception("engine tool %s failed", tool)
                await queue.put(_error_line(exc))
            finally:
                await queue.put(None)

        task = asyncio.create_task(run_tool())

        async def lines() -> AsyncIterator[bytes]:
            try:
                while True:
                    item = await queue.get()
                    if item is None:
                        break
                    yield (json.dumps(item) + "\n").encode("utf-8")
            finally:
                # A client that went away is a stop: nothing will read the result.
                if not task.done():
                    context.stop_requested = True
                contexts.pop(invocation_id, None)

        return StreamingResponse(lines(), media_type="application/x-ndjson")

    return app


def main() -> None:
    import uvicorn  # noqa: PLC0415

    from .config import Settings  # noqa: PLC0415

    import os  # noqa: PLC0415

    settings = Settings.from_env()
    # The socket is shared with the host's container, which runs as another
    # (non-root) user: connecting needs write permission on the socket file,
    # so it is created world-writable. The directory is the only thing that
    # scopes who can reach it — a shared emptyDir or compose volume.
    os.makedirs(os.path.dirname(settings.engine_socket) or ".", exist_ok=True)
    os.umask(0)
    uvicorn.run(create_sidecar(settings), uds=settings.engine_socket, log_level="info")


if __name__ == "__main__":
    main()
