"""The engine sidecar: the Go host's view of the Python engine (ADR-0023 H2).

Driven over ASGI with the fixture runner, so what is pinned is the wire the
Go host reads — one NDJSON line per thinking event, then the result or an
error carrying the legacy type and category — and the two behaviours the
host relies on: a stop that reaches the running tool, and a client that
goes away counting as one.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

import pytest
from httpx import ASGITransport, AsyncClient

from elitea_deepwiki.config import Settings
from elitea_deepwiki.fixture_runner import FixtureToolRunner
from elitea_deepwiki.legacy_runner import LegacyToolRunner
from elitea_deepwiki.sidecar import ENGINE_TOOLS, create_sidecar


def settings(**overrides: Any) -> Settings:
    values: dict[str, Any] = {"git_allowlist": "github.com", "fixture_step_seconds": 0.0, "runner": "fixture"}
    values.update(overrides)
    return Settings(**values)


def client_for(app) -> AsyncClient:
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://engine")


ARGUMENTS = {
    "query": "GO",
    "llm_settings": {},
    "embedding_model": None,
    "repo_config": {"provider_type": "github", "provider_config": {"repository": "acme/e2e-service"}, "repository": None, "branch": "main", "project": None, "is_cloud": None},
    "active_branch": "main",
    "force_rebuild_index": True,
    "indexing_method": "filesystem",
    "planner_mode": None,
    "exclude_tests": None,
    "run_in_subprocess": True,
}


async def stream(client: AsyncClient, body: dict[str, Any]) -> list[dict[str, Any]]:
    lines: list[dict[str, Any]] = []
    async with client.stream("POST", "/engine/invoke", json=body) as response:
        assert response.status_code == 200, await response.aread()
        assert response.headers["content-type"].startswith("application/x-ndjson")
        async for line in response.aiter_lines():
            if line.strip():
                lines.append(json.loads(line))
    return lines


async def test_the_fixture_engine_streams_progress_then_the_result():
    runner = FixtureToolRunner(settings())
    async with client_for(create_sidecar(settings(), runner)) as client:
        lines = await stream(client, {"invocation_id": "invocation_1", "tool": "generate_wiki", "arguments": ARGUMENTS})
    thinking = [line["thinking"] for line in lines if "thinking" in line]
    assert thinking == [
        "Cloning the repository",
        "Indexing 12 files",
        "Planning the wiki structure",
        "Writing 3 pages",
        "Assembling the manifest",
    ]
    result = lines[-1]["result"]
    assert result["success"] is True
    assert result["wiki_id"] == "acme--e2e-service--main"
    # The sidecar hands the ENGINE result back untouched: composition and
    # upload are the host's, so no result objects and no artifact upload here.
    assert "artifacts" in result and all("_uploaded_directly" not in a for a in result["artifacts"])


async def test_an_engine_exception_becomes_an_error_line_with_type_and_category():
    def boom(**_kwargs):
        raise FileNotFoundError("Wiki not found for repository")

    runner = LegacyToolRunner(settings=settings(), tools={"ask": boom})
    async with client_for(create_sidecar(settings(), runner)) as client:
        lines = await stream(client, {"invocation_id": "invocation_2", "tool": "ask", "arguments": {"question": "?"}})
    assert lines == [
        {"error": {"message": "Wiki not found for repository", "error_type": "FileNotFoundError", "error_category": "resource_not_found"}}
    ]


async def test_a_failed_engine_result_is_passed_through_for_the_host_to_classify():
    runner = LegacyToolRunner(settings=settings(), tools={"ask": lambda **_k: {"success": False, "error": "[SERVICE_BUSY]"}})
    async with client_for(create_sidecar(settings(), runner)) as client:
        lines = await stream(client, {"invocation_id": "invocation_3", "tool": "ask", "arguments": {"question": "?"}})
    assert lines == [{"result": {"success": False, "error": "[SERVICE_BUSY]"}}]


async def test_a_stop_reaches_the_running_tool(tmp_path):
    """Over a REAL Unix socket: httpx's ASGI transport buffers a response
    whole, so a stop sent mid-stream through it would land after the tool
    had finished and prove nothing. This is the transport the Go host uses.
    """
    import uvicorn  # noqa: PLC0415
    from httpx import AsyncHTTPTransport  # noqa: PLC0415

    socket_path = str(tmp_path / "e.sock")
    if len(socket_path) > 100:  # macOS caps a socket path at 104 bytes
        import tempfile  # noqa: PLC0415

        socket_path = tempfile.mkdtemp(prefix="eng", dir="/tmp") + "/e.sock"
    runner = FixtureToolRunner(settings(fixture_step_seconds=0.05))
    server = uvicorn.Server(uvicorn.Config(create_sidecar(settings(), runner), uds=socket_path, log_level="warning", lifespan="off"))
    serving = asyncio.create_task(server.serve())
    try:
        for _ in range(100):
            if server.started:
                break
            await asyncio.sleep(0.02)
        assert server.started
        async with AsyncClient(transport=AsyncHTTPTransport(uds=socket_path), base_url="http://engine") as client:
            lines: list[dict[str, Any]] = []
            async with client.stream("POST", "/engine/invoke", json={"invocation_id": "invocation_4", "tool": "generate_wiki", "arguments": ARGUMENTS}) as response:
                async for line in response.aiter_lines():
                    if not line.strip():
                        continue
                    lines.append(json.loads(line))
                    if lines[-1].get("thinking") == "Cloning the repository":
                        stop = await client.post("/engine/invocations/invocation_4/stop")
                        assert stop.status_code == 202 and stop.json() == {"stopped": True}
            assert "result" not in lines[-1], lines
            assert lines[-1]["error"]["message"] == "Invocation cancelled"
            assert not any(line.get("thinking") == "Assembling the manifest" for line in lines)
            # Unknown invocation: not an error, just nothing to stop.
            assert (await client.post("/engine/invocations/nope/stop")).json() == {"stopped": False}
    finally:
        server.should_exit = True
        await serving


async def test_the_door_refuses_what_the_host_would_never_send():
    app = create_sidecar(settings(), FixtureToolRunner(settings()))
    async with client_for(app) as client:
        for body, detail in [
            ({"tool": "ask", "arguments": {}}, "invocation_id is required"),
            # list_wikis is a wiki_query TOOL and is served by the Go host
            # over the artifact bucket; the sidecar never sees it, and a
            # request carrying it is a host that lost track of the split.
            ({"invocation_id": "i", "tool": "list_wikis", "arguments": {}}, "Unknown tool: list_wikis"),
            ({"invocation_id": "i", "tool": "ask", "arguments": []}, "arguments must be an object"),
        ]:
            response = await client.post("/engine/invoke", json=body)
            assert response.status_code == 400 and response.json()["detail"] == detail, body
        health = await client.get("/engine/health")
        assert health.json() == {"status": "UP", "runner": "fixture", "active": 0}
    # resolve_wiki is the wiki_query family's one model-backed step; the rest
    # of that family is the Go host's, over the artifact bucket.
    assert ENGINE_TOOLS == ("generate_wiki", "ask", "deep_research", "resolve_wiki")


async def test_a_second_invoke_for_a_running_id_is_a_conflict():
    started = asyncio.Event()
    release = asyncio.Event()

    def slow(**_kwargs):
        started.set()
        while not release.is_set():
            import time  # noqa: PLC0415

            time.sleep(0.01)
        return {"success": True, "answer": "done"}

    runner = LegacyToolRunner(settings=settings(), tools={"ask": slow})
    app = create_sidecar(settings(), runner)
    async with client_for(app) as client:
        first = asyncio.create_task(stream(client, {"invocation_id": "dup", "tool": "ask", "arguments": {"question": "?"}}))
        await asyncio.wait_for(started.wait(), 5)
        again = await client.post("/engine/invoke", json={"invocation_id": "dup", "tool": "ask", "arguments": {}})
        assert again.status_code == 409
        release.set()
        lines = await first
    assert lines[-1]["result"]["answer"] == "done"
