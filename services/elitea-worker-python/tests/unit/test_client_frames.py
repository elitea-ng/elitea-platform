"""The client frame catalogue's golden frames, captured from THIS worker.

Client contract 1.1 documents the `execution.node_event` frames a native
client renders tool activity and pauses from (`x-elitea-client-frames` in
services/elitea-main/api/openapi/v2.yaml). This test emits each of them through
the real callback and compares the result with
testdata/client-frames/python.json; elitea-main's
TestClientFrameCatalogueAcceptsWorkerFrames validates that file against the
catalogue. A change to what this worker emits therefore fails HERE until the
file is regenerated, and the regenerated file fails THERE if it no longer
satisfies the contract.

Regenerate with ELITEA_UPDATE_CLIENT_FRAMES=1.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any

from elitea_worker.handlers.agent_events import (
    TOOL_OUTPUT_CHUNK_EVENT,
    CurrentAgentNodeEventCallback,
    CurrentAgentNodeEventContext,
)
from elitea_worker.protocol.node_event import encode_current_node_event_json

GOLDEN = Path(__file__).resolve().parents[4] / "testdata" / "client-frames" / "python.json"

# Wall-clock values the callback stamps; everything else must be stable.
_VOLATILE_KEYS = frozenset({"created_at", "timestamp_start", "timestamp_finish"})


def _callback():
    events: list[Any] = []
    callback = CurrentAgentNodeEventCallback(
        CurrentAgentNodeEventContext(
            execution_id="execution-1",
            stream_id="conversation-1",
            message_id="message-1",
            execution_generation="generation-1",
            sio_event="chat_predict",
            thread_id="thread-1",
            project_id=7,
            chat_project_id=7,
        ),
        events.append,
    )
    return callback, events


def _payload():
    return SimpleNamespace(
        application={"id": 11, "version_id": 22},
        should_continue=False,
        hitl_resume=False,
        parallel_reconcile=None,
        invoked_skills=[],
    )


def _stable(value: Any) -> Any:
    if isinstance(value, dict):
        return {
            key: ("<timestamp>" if key in _VOLATILE_KEYS and value[key] is not None else _stable(item))
            for key, item in value.items()
        }
    if isinstance(value, list):
        return [_stable(item) for item in value]
    return value


def _frames(events: list[Any]) -> list[dict[str, Any]]:
    return [_stable(json.loads(encode_current_node_event_json(event))) for event in events]


def _first(frames: list[dict[str, Any]], frame_type: str) -> dict[str, Any]:
    return next(frame for frame in frames if frame["type"] == frame_type)


def _capture() -> dict[str, dict[str, Any]]:
    captured: dict[str, dict[str, Any]] = {}

    # A tool call by a sub-agent, start to end.
    callback, events = _callback()
    callback.on_tool_start(
        {"name": "list_issues", "metadata": {"display_name": "List issues"}},
        "ignored",
        run_id="run-1",
        metadata={
            "toolkit_name": "github",
            "toolkit_type": "github",
            "parent_agent_name": "researcher",
            "parent_agent_call_id": "call-parent",
        },
        inputs={"state": "open"},
    )
    callback.on_tool_end({"items": 2}, run_id="run-1")
    frames = _frames(events)
    captured["tool_start"] = _first(frames, "agent_tool_start")
    captured["tool_end"] = _first(frames, "agent_tool_end")

    # A failed tool call.
    callback, events = _callback()
    callback.on_tool_start(
        {"name": "read_file", "metadata": {"display_name": "Read file"}},
        "ignored",
        run_id="run-2",
        metadata={"toolkit_name": "artifact"},
        inputs={"filename": "missing.txt"},
    )
    callback.on_tool_error(RuntimeError("file not found"), run_id="run-2")
    captured["tool_error"] = _first(_frames(events), "agent_tool_error")

    # A result too large for one frame: its first chunk and its end.
    callback, events = _callback()
    callback.on_tool_start(
        {"name": "read_file", "metadata": {"display_name": "Read file"}},
        "ignored",
        run_id="run-3",
        metadata={"toolkit_name": "artifact"},
        inputs={"filename": "big.txt"},
    )
    callback.on_tool_end("golden chunked output line\n" * 4_000, run_id="run-3")
    frames = _frames(events)
    chunk = _first(frames, TOOL_OUTPUT_CHUNK_EVENT)
    chunk["content"] = "<slice>"
    captured["tool_output_chunk"] = chunk
    captured["tool_end_chunked"] = _first(frames, "agent_tool_end")

    # A single HITL pause (terminal).
    callback, _ = _callback()
    interrupt = {
        "interrupt_id": "interrupt-1",
        "tool_call_id": "call-1",
        "node_name": "sensitive_tool",
        "message": "Approve?",
        "available_actions": ["approve", "reject", "edit"],
        "guardrail_type": "sensitive_tool",
        "tool_name": "delete_branch",
        "toolkit_name": "github",
        "tool_args": {"branch": "old"},
        "routes": [{"tool_call_id": "call-1"}],
    }
    terminal = callback.emit_terminal(
        {
            "thread_id": "thread-1",
            "paused": True,
            "pause_type": "hitl",
            "hitl_interrupt": interrupt,
            "hitl_interrupts": [interrupt],
        },
        _payload(),
    )
    captured["hitl_interrupt"] = _frames([terminal])[0]

    # A toolkit that needs sign-in: the progress frame and the terminal one.
    class McpAuthorizationRequired(RuntimeError):
        server_url = "https://mcp.example.test/mcp"
        resource_metadata_url = "https://mcp.example.test/.well-known/oauth-protected-resource"
        resource_metadata = {"resource_name": "Example"}
        authorization_servers = ["https://login.example.test"]
        tool_name = "search"

    callback, events = _callback()
    callback.on_tool_start(
        {
            "name": "search",
            "metadata": {"display_name": "Search", "toolkit_name": "example", "toolkit_type": "mcp"},
        },
        "ignored",
        run_id="auth-1",
        metadata={},
        inputs={},
    )
    callback.on_tool_error(McpAuthorizationRequired("Example needs sign-in."), run_id="auth-1")
    terminal = callback.emit_terminal(callback.authorization_pause_result(), _payload())
    captured["mcp_authorization_progress"] = _first(_frames(events), "mcp_authorization_required")
    captured["mcp_authorization_terminal"] = _frames([terminal])[0]
    return captured


def test_client_frames_match_the_golden_file() -> None:
    captured = _capture()
    rendered = json.dumps(captured, indent=2, sort_keys=True, ensure_ascii=False) + "\n"
    if os.environ.get("ELITEA_UPDATE_CLIENT_FRAMES") == "1":
        GOLDEN.parent.mkdir(parents=True, exist_ok=True)
        GOLDEN.write_text(rendered, encoding="utf-8")
    assert GOLDEN.exists(), f"{GOLDEN} is missing; regenerate with ELITEA_UPDATE_CLIENT_FRAMES=1"
    golden = json.loads(GOLDEN.read_text(encoding="utf-8"))
    assert golden == captured, (
        "a client-rendered frame changed; regenerate testdata/client-frames/python.json with "
        "ELITEA_UPDATE_CLIENT_FRAMES=1 and make sure elitea-main's "
        "TestClientFrameCatalogueAcceptsWorkerFrames still passes"
    )


def test_the_terminal_rules_hold_on_the_captured_frames() -> None:
    captured = _capture()
    # Only the authorization frame that carries the requests is terminal.
    assert "authorization_requests" not in captured["mcp_authorization_progress"]["response_metadata"]
    assert captured["mcp_authorization_terminal"]["response_metadata"]["authorization_requests"]
    # A chunk names its call, and the end names the chunk count and digest.
    position = captured["tool_output_chunk"]["response_metadata"]["tool_output_chunk"]
    assert position["tool_call_id"] == "run-3" and position["index"] == 0
    assert captured["tool_end_chunked"]["response_metadata"]["tool_output_chunks"]["total"] == position["total"]
