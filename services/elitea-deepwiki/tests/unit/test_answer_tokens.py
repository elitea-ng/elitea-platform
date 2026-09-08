"""The answer token channel, engine side (issue #701).

The SPI had ONE event channel and it carried progress text, so the wiki chat
showed a spinner until the whole answer landed. The sidecar now has two keys
on its NDJSON stream — ``thinking`` for progress and ``token`` for a fragment
of the answer — and this pins what the Go host reads off that stream.

The golden fixture the three hops answer to is
``conformance/provider/fixtures/deepwiki/stream/token_events.json``: this
file covers the ENGINE hop of it, the Go host's
``internal/apps/deepwiki/run/engine_test.go`` covers the host hop, and the
browser's ``framesFromChatPoll.test.ts`` covers the last one.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from httpx import ASGITransport, AsyncClient

from elitea_deepwiki.config import Settings
from elitea_deepwiki.fixture_runner import (
    STREAMED_ANSWER_KEY,
    answer_fragments,
    FixtureToolRunner,
)
from elitea_deepwiki.legacy_runner import ToolHost
from elitea_deepwiki.sidecar import SidecarContext, create_sidecar

SERVICE_ROOT = Path(__file__).resolve().parents[2]
GOLDEN = json.loads(
    (
        SERVICE_ROOT.parents[1]
        / "conformance"
        / "provider"
        / "fixtures"
        / "deepwiki"
        / "stream"
        / "token_events.json"
    ).read_text(encoding="utf-8")
)


def settings(**overrides: Any) -> Settings:
    values: dict[str, Any] = {
        "git_allowlist": "github.com",
        "fixture_step_seconds": 0.0,
        "runner": "fixture",
    }
    values.update(overrides)
    return Settings(**values)


async def stream(app, body: dict[str, Any]) -> list[dict[str, Any]]:
    lines: list[dict[str, Any]] = []
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://engine") as client:
        async with client.stream("POST", "/engine/invoke", json=body) as response:
            assert response.status_code == 200, await response.aread()
            async for line in response.aiter_lines():
                if line.strip():
                    lines.append(json.loads(line))
    return lines


async def test_the_answer_arrives_as_token_lines_after_the_progress_lines():
    """One `ask` run, as the Go host reads it off the socket."""
    runner = FixtureToolRunner(settings())
    lines = await stream(
        create_sidecar(settings(), runner),
        {
            "invocation_id": "invocation_1",
            "tool": "ask",
            "arguments": {"question": "Where do the wiki pages live?"},
        },
    )

    # The two channels are separate KEYS, so neither hop downstream has to
    # parse a message to find out which one it is reading.
    assert [line["thinking"] for line in lines if "thinking" in line] == [
        "Searching the wiki index",
        "Composing the answer",
    ]

    tokens = [line["token"] for line in lines if "token" in line]
    assert len(tokens) > 1, "one fragment cannot tell a stream from a whole answer"

    # The fragments JOIN BACK to the answer the same run returns, with no
    # separator. Asserted against the result rather than against a copy of
    # the text, so the two cannot drift.
    answer = lines[-1]["result"]["answer"]
    assert "".join(tokens) == answer

    # Order: every progress line comes before every token, and the result is
    # last. A reader that sorted the channels apart would lose the ordering
    # that makes an answer around a tool call readable.
    kinds = [next(iter(line)) for line in lines]
    assert kinds == ["thinking", "thinking"] + ["token"] * len(tokens) + ["result"]


async def test_a_generation_streams_no_tokens_because_it_produces_no_answer():
    runner = FixtureToolRunner(settings())
    lines = await stream(
        create_sidecar(settings(), runner),
        {
            "invocation_id": "invocation_2",
            "tool": "generate_wiki",
            "arguments": {
                "query": "GO",
                "repo_config": {"repository": "acme/e2e-service"},
                "active_branch": "main",
            },
        },
    )
    assert not any("token" in line for line in lines)
    assert "generate_wiki" not in STREAMED_ANSWER_KEY


async def test_deep_research_streams_its_report():
    runner = FixtureToolRunner(settings())
    lines = await stream(
        create_sidecar(settings(), runner),
        {
            "invocation_id": "invocation_3",
            "tool": "deep_research",
            "arguments": {"question": "How is storage laid out?"},
        },
    )
    tokens = [line["token"] for line in lines if "token" in line]
    assert "".join(tokens) == lines[-1]["result"]["report"]


async def test_an_empty_fragment_never_reaches_the_wire():
    """The poll drain discards an event with an empty message.

    A fragment that carries nothing would be spent as one of the read-once
    events and deliver no text, so it is dropped at the source instead.
    """
    queue: Any

    import asyncio  # noqa: PLC0415

    queue = asyncio.Queue()
    context = SidecarContext("invocation_4", "ask", queue)
    await context.token("")
    await context.token("real")
    assert queue.qsize() == 1
    assert await queue.get() == {"token": "real"}


def test_the_fragments_of_an_answer_join_back_to_it():
    answer = "The wiki pages live in the wiki-artifacts bucket."
    fragments = answer_fragments(answer)
    assert len(fragments) > 1
    assert "".join(fragments) == answer
    # An answer that is shorter than the fragment count still streams, and
    # still joins.
    assert "".join(answer_fragments("ab")) == "ab"
    assert answer_fragments("") == ()


async def test_the_tool_layer_hook_reaches_the_token_channel():
    """`invocation_token` is the sixth host hook the engine calls back into.

    The tool layer is synchronous and runs OFF the event loop, so the hook is
    called from a worker thread — which is why the host is built on the loop
    and the call is made from a thread here. What is pinned is that a
    fragment marshals back and lands on the token channel, not the thinking
    one.
    """
    import asyncio  # noqa: PLC0415

    seen: list[dict[str, Any]] = []

    class RecordingContext:
        async def thinking(self, message: str) -> None:
            seen.append({"thinking": message})

        async def token(self, text: str) -> None:
            seen.append({"token": text})

    host = ToolHost(settings(), RecordingContext())

    def worker() -> None:
        host.invocation_thinking("Processing your question...")
        host.invocation_token("half ")
        host.invocation_token("an answer")

    await asyncio.to_thread(worker)
    assert seen == [
        {"thinking": "Processing your question..."},
        {"token": "half "},
        {"token": "an answer"},
    ]


async def test_a_context_without_a_token_channel_still_runs():
    """An older context has no `token`, and the answer still arrives whole.

    The hook is ours, not the legacy Pylon module's, so a host that predates
    it must degrade to progress-only rather than fail the tool.
    """
    import asyncio  # noqa: PLC0415

    calls: list[str] = []

    class ProgressOnlyContext:
        async def thinking(self, message: str) -> None:
            calls.append(message)

    host = ToolHost(settings(), ProgressOnlyContext())

    def worker() -> None:
        host.invocation_token("dropped")
        host.invocation_thinking("still running")

    await asyncio.to_thread(worker)
    assert calls == ["still running"]


def test_the_engine_hop_of_the_golden_fixture_is_the_shape_this_service_writes():
    """The keys, not the words: the fixture names the wire, this pins it."""
    hop = GOLDEN["hops"]["engine_socket"]
    assert list(hop["progress_line"]) == ["thinking"]
    assert list(hop["token_line"]) == ["token"]
    assert [next(iter(line)) for line in GOLDEN["golden_sequence"]["engine_lines"]] == [
        "thinking",
        "token",
        "token",
        "thinking",
        "token",
        "result",
    ]
    streamed = "".join(
        line["token"] for line in GOLDEN["golden_sequence"]["engine_lines"] if "token" in line
    )
    assert streamed == GOLDEN["golden_sequence"]["streaming_text"]


def test_the_worker_prefix_and_the_tool_layer_reader_agree():
    """The two halves of the engine's own token path, which are declared in
    two different substitutions of two different frozen files.

    The worker prints `[LLM_CHUNK] {json}` and the tool layer strips a fixed
    number of characters before parsing. A prefix and an offset that
    disagreed by one would make `json.loads` raise, the reader swallow the
    exception, and every fragment of every answer disappear with nothing
    logged — which is exactly the failure the channel exists to end.
    """
    prefix = "[LLM_CHUNK] "
    line = f"{prefix}{json.dumps({'text': 'the answer'})}"
    assert len(prefix) == 12
    assert line.startswith(prefix)
    assert json.loads(line[12:])["text"] == "the answer"

    tools_source = (SERVICE_ROOT / "src" / "elitea_deepwiki" / "tool_operations.py").read_text(
        encoding="utf-8"
    )
    assert 'line.startswith("[LLM_CHUNK] ")' in tools_source
    assert "self.invocation_token(fragment)" in tools_source

    worker_source = (
        SERVICE_ROOT / "src" / "elitea_deepwiki" / "engine" / "ask_subprocess_worker.py"
    ).read_text(encoding="utf-8")
    assert '_print(f"[LLM_CHUNK] {json.dumps({\'text\': fragment})}")' in worker_source
    # Streaming is ON by default; a deployment turns it off through the
    # settings rather than by editing a frozen copy.
    assert "streaming=_answer_streaming(llm_settings)" in worker_source
    assert 'llm_settings.get("streaming", True)' in worker_source

    engine_source = (
        SERVICE_ROOT / "src" / "elitea_deepwiki" / "engine" / "ask_engine.py"
    ).read_text(encoding="utf-8")
    assert '"event_type": "llm_chunk"' in engine_source
    # The fragments are JOINED. The assignment this replaced kept the last
    # one, which with streaming on is the end of the answer and nothing else.
    assert 'self.final_answer = "".join(answer_fragments)' in engine_source
    # And ONE card per tool call. Streaming can bring the same call round the
    # loop once per chunk of its arguments, and the thinking log would fill
    # with repeats of it — a regression the token channel would have caused
    # rather than fixed.
    assert "announced_tool_calls = set()" in engine_source
    assert "if announced in announced_tool_calls:" in engine_source
