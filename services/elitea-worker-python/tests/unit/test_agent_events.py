from __future__ import annotations

from types import SimpleNamespace

import hashlib

from elitea.runtime.v1 import (
    agent_pb2,
    command_pb2,
    common_pb2,
    control_pb2,
    envelope_pb2,
)

from elitea_worker.execution.delivery import _validate_pending_output
from elitea_worker.handlers.agent_events import (
    CurrentAgentNodeEventCallback,
    CurrentAgentNodeEventContext,
    nested_skill_registry_from_payload,
)
from elitea_worker.protocol.codec import (
    VerifiedWorkerCommand,
    build_node_event_output_frame,
)
from elitea_worker.protocol.node_event import encode_current_node_event_json


def _callback():
    events = []
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


def _request_payload():
    return SimpleNamespace(
        application={"id": 11, "version_id": 22},
        should_continue=False,
        hitl_resume=False,
        parallel_reconcile=None,
        invoked_skills=[],
    )


def _json(event):
    import json

    return json.loads(encode_current_node_event_json(event))


def test_tool_lifecycle_emits_live_events_and_existing_trace_deltas() -> None:
    callback, events = _callback()
    metadata = {
        "parent_agent_name": "researcher",
        "parent_agent_call_id": "call-parent",
        "langgraph_node": "tools",
        "toolkit_name": "github",
    }

    callback.on_tool_start(
        {"name": "search", "metadata": {"display_name": "GitHub"}},
        "ignored",
        run_id="run-1",
        metadata=metadata,
        inputs={"query": "typed contracts"},
    )
    callback.on_tool_end({"items": 2}, run_id="run-1")

    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "agent_tool_start",
        "partial_message",
        "agent_tool_end",
        "partial_message",
    ]
    delta = decoded[-1]
    assert delta["execution_generation"] == "generation-1"
    tool = delta["response_metadata"]["tool_calls"]["run-1"]
    assert tool["tool_name"] == "search"
    assert tool["tool_output"] == '{"items":2}'
    assert tool["metadata"]["parent_agent_name"] == "researcher"
    assert tool["finish_reason"] == "stop"
    assert decoded[2]["content"] is None


def test_large_tool_result_fits_one_data_plane_frame_without_duplicate_content() -> None:
    callback, events = _callback()
    callback.on_tool_start(
        {"name": "list_initiatives", "metadata": {"display_name": "Aha"}},
        "ignored",
        run_id="run-large",
        metadata={"toolkit_name": "aha"},
        inputs={"max_records": 100},
    )
    # Production evidence for the failed Aha turn was 51,979 bytes. Keep this
    # fixture at that exact UTF-8 size while including JSON quoting overhead.
    # The stored current output required 57,366 bytes when encoded as a JSON
    # string. Reproduce the same 5,387-byte escaping overhead (including the
    # outer quotes), not only a friendly ASCII payload.
    escaped_quote_count = 5_385
    large_output = ('"' * escaped_quote_count) + (
        "x" * (51_979 - escaped_quote_count)
    )
    assert len(large_output.encode("utf-8")) == 51_979

    callback.on_tool_end(large_output, run_id="run-large")

    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "agent_tool_start",
        "partial_message",
        "agent_tool_end",
        "partial_message",
    ]
    assert decoded[2]["content"] is None
    assert decoded[2]["response_metadata"]["tool_output"] == large_output

    fence = common_pb2.ExecutionFenceV1(
        workload_session_id="worker-session",
        producer_id="worker-1",
        claim_attempt=1,
        lease_epoch=1,
        fence_token=b"f" * 32,
    )
    command = command_pb2.WorkerCommandV1(
        command_id="command-large",
        execution_id="execution-large",
        generation=1,
        tenant_id="tenant-1",
        resource_project_id="7",
        projection_project_id="7",
        agent_execution=agent_pb2.AgentExecutionCommandV1(
            request_entry_id="agent-request",
            client_stream_id="conversation-1",
            client_message_id="message-1",
            sio_event="chat_predict",
        ),
    )
    verified = VerifiedWorkerCommand(
        envelope=envelope_pb2.WorkerExecutionEnvelopeV1(fence=fence),
        command=command,
    )
    for sequence, event in enumerate(events[2:], start=1):
        frame = build_node_event_output_frame(
            verified,
            event,
            sequence=sequence,
            occurred_at_unix_millis=1_700_000_000_000,
        )
        assert len(frame.SerializeToString(deterministic=True)) <= 64 * 1024


class _Generation:
    def model_dump(self):
        return {
            "type": "ChatGeneration",
            "text": "answer",
            "message": {
                "content": [
                    {"type": "thinking", "thinking": "reasoning"},
                    {"type": "text", "text": "answer"},
                ],
                "additional_kwargs": {},
            },
        }


class _ToolCallGeneration:
    def model_dump(self):
        return {
            "type": "ChatGeneration",
            "text": "",
            "message": {
                "content": "",
                "additional_kwargs": {
                    "tool_calls": [
                        {
                            "function": {
                                "name": "list_products",
                                "arguments": '{"private":"must-not-be-copied"}',
                            }
                        }
                    ]
                },
            },
        }


def test_llm_callbacks_emit_stream_and_thinking_step_delta() -> None:
    callback, events = _callback()
    callback.on_chat_model_start(
        {"name": "ChatModel"},
        [[]],
        run_id="llm-1",
        metadata={"ls_model_name": "model-1", "parent_agent_name": "planner"},
    )
    callback.on_llm_new_token("hel", run_id="llm-1")
    callback.on_llm_new_token("hello", run_id="llm-1")
    callback.on_llm_end(
        SimpleNamespace(generations=[[_Generation()]]),
        run_id="llm-1",
    )

    decoded = [_json(event) for event in events]
    start = decoded[0]
    assert start["type"] == "agent_llm_start"
    assert start["response_metadata"]["tool_name"] == "Thinking step"
    assert start["response_metadata"]["metadata"]["ls_model_name"] == "model-1"
    assert [event["content"] for event in decoded if event["type"] == "agent_llm_chunk"] == [
        "hel",
        "lo",
    ]
    partial = decoded[-1]
    step = partial["response_metadata"]["thinking_steps"][0]
    assert step["text"] == "answer"
    assert step["thinking"] == "reasoning"
    assert step["message"]["response_metadata"]["model_name"] == "model-1"
    assert step["parent_agent_name"] == "planner"


def test_tool_call_only_llm_turn_keeps_model_activity_without_copying_arguments() -> None:
    callback, events = _callback()
    callback.on_chat_model_start(
        {"name": "ChatModel"},
        [[]],
        run_id="llm-tool-call",
        metadata={"ls_model_name": "model-1"},
    )
    callback.on_llm_end(
        SimpleNamespace(generations=[[_ToolCallGeneration()]]),
        run_id="llm-tool-call",
    )

    decoded = [_json(event) for event in events]
    step = decoded[-1]["response_metadata"]["thinking_steps"][0]
    assert step["text"] == "Planned to call tool 'list_products'"
    assert "must-not-be-copied" not in str(step)


def test_hitl_terminal_keeps_current_shape_and_is_not_a_full_message() -> None:
    callback, events = _callback()
    payload = _request_payload()
    interrupt = {
        "interrupt_id": "interrupt-1",
        "node_name": "sensitive_tool",
        "message": "Approve?",
        "available_actions": ["approve", "reject"],
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
        payload,
    )

    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == ["agent_hitl_interrupt"]
    event = _json(terminal)
    assert event["stream_id"] == "conversation-1"
    assert event["sio_event"] == "chat_predict"
    assert event["content"] == "Approve?"
    assert event["response_metadata"]["hitl_interrupt"] == interrupt
    assert event["response_metadata"]["hitl_interrupts"] == [interrupt]
    assert event["response_metadata"]["available_actions"] == [
        "approve",
        "reject",
    ]


def test_next_input_suggestion_follows_primary_response_and_precedes_terminal() -> None:
    callback, events = _callback()

    response = callback.emit_completed_response(
        {"thread_id": "thread-1", "output": "Primary answer"}
    )
    suggestion = callback.emit_next_input_suggestion("Continue with tests.")
    terminal = callback.emit_terminal(
        {"thread_id": "thread-1", "output": "Primary answer"},
        _request_payload(),
        response_emitted=True,
    )

    decoded = [_json(event) for event in events]
    assert terminal.type == "full_message"
    assert response.type == "agent_response"
    assert suggestion is not None
    assert [event["type"] for event in decoded] == [
        "agent_response",
        "next_input_suggestion_ready",
        "full_message",
    ]
    assert decoded[1]["stream_id"] == "conversation-1"
    assert decoded[1]["message_id"] == "message-1"
    assert decoded[1]["execution_generation"] == "generation-1"
    assert decoded[1]["response_metadata"] == {
        "suggestion": "Continue with tests."
    }


def test_completed_response_content_excludes_hitl_and_authorization_pauses() -> None:
    callback, _ = _callback()

    assert callback.completed_response_content({"output": "Primary answer"}) == (
        "Primary answer"
    )
    assert (
        callback.completed_response_content(
            {
                "paused": True,
                "pause_type": "mcp_auth",
                "error": "Authorization required.",
            }
        )
        is None
    )
    assert (
        callback.completed_response_content(
            {
                "execution_finished": False,
                "hitl_interrupt": {
                    "interrupt_id": "interrupt-1",
                    "message": "Awaiting review.",
                },
            }
        )
        is None
    )


def test_completed_response_content_keeps_only_current_visible_text_blocks() -> None:
    callback, _ = _callback()

    assert callback.completed_response_content(
        {
            "output": [
                {"type": "text", "text": "First"},
                {"type": "output_text", "text": " second"},
                {"type": "reasoning", "text": "must remain private"},
                {"type": "tool_use", "name": "search"},
                {},
                17,
            ]
        }
    ) == "First second"


def test_next_input_suggestion_rejects_empty_and_oversized_text() -> None:
    callback, events = _callback()

    assert callback.emit_next_input_suggestion("") is None
    assert callback.emit_next_input_suggestion("x" * 2049) is None
    assert events == []


def test_delegated_authorization_terminal_preserves_exact_requests() -> None:
    callback, events = _callback()
    payload = _request_payload()
    callback.on_custom_event(
        "mcp_authorization_required",
        {
            "server_url": "https://tool.example.test/mcp",
            "authorization_servers": ["https://auth.example.test"],
            "tool_run_id": "auth-call-1",
            "tool_name": "list_sites",
            "toolkit_name": "SharePoint",
            "toolkit_type": "sharepoint",
        },
        run_id="callback-run-1",
        metadata={
            "parent_agent_name": "researcher",
            "parent_agent_call_id": "agent-call-1",
        },
    )

    terminal = callback.emit_terminal(
        {
            "thread_id": "thread-1",
            "paused": True,
            "pause_type": "mcp_auth",
            "error": "SharePoint authorization is required.",
        },
        payload,
    )

    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "mcp_authorization_required",
        "mcp_authorization_required",
    ]
    event = _json(terminal)
    assert event["content"] == "SharePoint authorization is required."
    assert event["response_metadata"]["tool_run_id"] == "auth-call-1"
    assert event["response_metadata"]["thread_id"] == "thread-1"
    assert event["response_metadata"]["authorization_requests"] == [
        decoded[0]["response_metadata"]
    ]


def test_sdk_authorization_tool_error_becomes_an_exact_pause() -> None:
    callback, events = _callback()

    class McpAuthorizationRequired(RuntimeError):
        server_url = "https://tenant.example.test/sites/team"
        resource_metadata_url = "https://login.example.test/discovery"
        resource_metadata = {
            "resource_name": "SharePoint",
            "provided_settings": {
                "mcp_client_id": "must-not-cross-output",
                "mcp_client_secret": "must-not-cross-output",
            },
        }
        authorization_servers = ["https://login.example.test"]
        tool_name = "get_lists"

    callback.on_tool_start(
        {
            "name": "mcp_authorize_sharepoint",
            "metadata": {
                "display_name": "SharePoint",
                "toolkit_name": "sharepoint",
                "toolkit_type": "sharepoint",
            },
        },
        "ignored",
        run_id="auth-call-1",
        metadata={
            "parent_agent_name": "researcher",
            "parent_agent_call_id": "agent-call-1",
            "parent_agent_path": [
                {"name": "researcher", "call_id": "agent-call-1"}
            ],
            "child_thread_id": "thread-1:researcher",
            "checkpoint_ns": "agent:researcher",
        },
        inputs={},
    )
    callback.on_tool_error(
        McpAuthorizationRequired("SharePoint authorization is required."),
        run_id="auth-call-1",
    )

    assert callback.authorization_pause_result() == {
        "thread_id": "thread-1",
        "error": "SharePoint authorization is required.",
        "paused": True,
        "pause_type": "mcp_auth",
    }
    terminal = callback.emit_terminal(
        callback.authorization_pause_result(),
        _request_payload(),
    )
    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "agent_tool_start",
        "partial_message",
        "mcp_authorization_required",
        "mcp_authorization_required",
    ]
    authorization = _json(terminal)["response_metadata"]
    assert authorization["tool_run_id"] == "auth-call-1"
    assert authorization["tool_name"] == "get_lists"
    assert authorization["toolkit_name"] == "sharepoint"
    assert authorization["toolkit_type"] == "sharepoint"
    assert authorization["parent_agent_name"] == "researcher"
    assert authorization["parent_agent_call_id"] == "agent-call-1"
    assert authorization["parent_agent_path"] == [
        {"name": "researcher", "call_id": "agent-call-1"}
    ]
    assert authorization["child_thread_id"] == "thread-1:researcher"
    assert authorization["checkpoint_ns"] == "agent:researcher"
    assert "provided_settings" not in authorization
    assert "provided_settings" not in authorization["resource_metadata"]
    assert "provided_settings" not in authorization["authorization_requests"][0]["resource_metadata"]


def test_saved_mcp_authorization_pause_remains_at_the_root_scope() -> None:
    callback, events = _callback()

    class McpAuthorizationRequired(RuntimeError):
        server_url = "https://mcp.example.test/events"
        resource_metadata_url = "https://login.example.test/discovery"
        resource_metadata = {"resource_name": "Documentation MCP"}
        authorization_servers = ["https://login.example.test"]
        tool_name = "search_docs"

    callback.on_tool_start(
        {
            "name": "mcp_authorize_documentation",
            "metadata": {
                "display_name": "Documentation MCP",
                "toolkit_name": "documentation-mcp",
                "toolkit_type": "mcp",
            },
        },
        "ignored",
        run_id="root-mcp-auth-call",
        metadata={},
        inputs={},
    )
    callback.on_tool_error(
        McpAuthorizationRequired("Documentation MCP authorization is required."),
        run_id="root-mcp-auth-call",
    )

    terminal = callback.emit_terminal(
        callback.authorization_pause_result(),
        _request_payload(),
    )
    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "agent_tool_start",
        "partial_message",
        "mcp_authorization_required",
        "mcp_authorization_required",
    ]
    authorization = _json(terminal)["response_metadata"]
    assert authorization["tool_run_id"] == "root-mcp-auth-call"
    assert authorization["tool_name"] == "search_docs"
    assert authorization["toolkit_name"] == "documentation-mcp"
    assert authorization["toolkit_type"] == "mcp"
    assert "parent_agent_name" not in authorization
    assert "parent_agent_call_id" not in authorization
    assert "parent_agent_path" not in authorization
    assert "child_thread_id" not in authorization
    assert "checkpoint_ns" not in authorization


def test_materialization_authorization_uses_a_deterministic_guardrail_identity() -> None:
    callback, events = _callback()

    class McpAuthorizationRequired(RuntimeError):
        server_url = "https://mcp.example.test/events"
        resource_metadata_url = "https://login.example.test/discovery"
        resource_metadata = {
            "resource_name": "Documentation MCP",
            "provided_settings": {"client_secret": "must-not-cross-output"},
        }
        authorization_servers = ["https://login.example.test"]
        tool_name = "search_docs"
        toolkit_name = "documentation-mcp"
        toolkit_type = "mcp"

    assert callback.capture_materialization_authorization(
        McpAuthorizationRequired("Documentation MCP authorization is required.")
    )
    pause = callback.authorization_pause_result()
    assert pause is not None
    terminal = callback.emit_terminal(pause, _request_payload())

    decoded = [_json(event) for event in events]
    assert [event["type"] for event in decoded] == [
        "mcp_authorization_required",
        "mcp_authorization_required",
    ]
    authorization = _json(terminal)["response_metadata"]
    assert authorization["tool_run_id"] == (
        "mcp-auth-materialization:execution-1"
    )
    assert authorization["tool_name"] == "search_docs"
    assert authorization["toolkit_name"] == "documentation-mcp"
    assert authorization["toolkit_type"] == "mcp"
    assert "provided_settings" not in authorization["resource_metadata"]


def test_delegated_authorization_terminal_requires_callback_identity() -> None:
    callback, _ = _callback()

    try:
        callback.emit_terminal(
            {"paused": True, "pause_type": "mcp_auth"},
            _request_payload(),
        )
    except Exception as error:  # noqa: BLE001 - assert the worker contract error
        assert "authorization request identity" in str(error)
    else:
        raise AssertionError("missing delegated authorization identity was accepted")


def test_pipeline_hitl_terminal_uses_execution_identity_when_sdk_omits_one() -> None:
    callback, events = _callback()
    payload = _request_payload()
    interrupt = {
        "type": "hitl",
        "node_name": "review",
        "message": "Review the generated answer.",
        "available_actions": ["approve", "reject", "edit"],
        "routes": {
            "approve": "publish",
            "reject": "END",
            "edit": "revise",
        },
        "edit_state_key": "answer",
    }

    terminal = callback.emit_terminal(
        {
            "thread_id": "thread-1",
            "execution_finished": False,
            "hitl_interrupt": interrupt,
            "hitl_interrupts": [interrupt],
        },
        payload,
    )

    event = _json(terminal)
    persisted = event["response_metadata"]["hitl_interrupt"]
    assert event["type"] == "agent_hitl_interrupt"
    assert persisted["interrupt_id"] == "execution-1"
    assert event["response_metadata"]["hitl_interrupts"] == [persisted]
    assert "interrupt_id" not in interrupt


def test_raw_sdk_hitl_custom_event_is_not_a_second_terminal_owner() -> None:
    callback, events = _callback()
    callback.on_custom_event(
        "hitl_interrupt",
        {
            "node_name": "sensitive_tool",
            "message": "Approve?",
            "available_actions": ["approve", "reject"],
            "routes": [{"tool_call_id": "call-1"}],
        },
        run_id="hitl-1",
        metadata={"checkpoint_ns": "agent:child"},
    )

    assert events == []


def test_large_transition_omits_only_duplicate_transcript_state() -> None:
    callback, events = _callback()
    large_tool_transcript = "x" * 90_000

    callback.on_custom_event(
        "on_transitional_edge",
        {
            "next_step": "agent",
            "state": {
                "messages": [
                    {"role": "tool", "content": large_tool_transcript},
                ],
                "chat_history": large_tool_transcript,
                "business_state": {"preserved": True},
            },
        },
        run_id="transition-large",
        metadata={"langgraph_node": "tools"},
    )

    event = _json(events[0])
    response_metadata = event["response_metadata"]
    assert event["type"] == "agent_on_transitional_edge"
    assert response_metadata["next_step"] == "agent"
    assert response_metadata["state"] == {
        "business_state": {"preserved": True},
    }
    assert response_metadata["state_projection"] == {
        "omitted_duplicate_fields": ["messages", "chat_history"],
    }
    assert large_tool_transcript not in str(event)
    assert len(encode_current_node_event_json(events[0])) <= 60 * 1024


def test_small_transition_also_omits_duplicate_transcript_state() -> None:
    callback, events = _callback()

    callback.on_custom_event(
        "on_transitional_edge",
        {
            "next_step": "agent",
            "state": {
                "messages": ["private skill instructions"],
                "chat_history": "private skill instructions",
                "business_state": {"preserved": True},
            },
        },
        run_id="transition-small",
    )

    event = _json(events[0])
    assert event["response_metadata"]["state"] == {
        "business_state": {"preserved": True}
    }
    assert "private skill instructions" not in str(event)


def test_agent_event_frame_uses_durable_execution_fence_and_sequence() -> None:
    callback, events = _callback()
    callback.emit_agent_start(invoked_skills=[{"name": "review"}])
    fence = common_pb2.ExecutionFenceV1(
        workload_session_id="worker-session",
        producer_id="worker-1",
        claim_attempt=2,
        lease_epoch=3,
        fence_token=b"f" * 32,
    )
    command = command_pb2.WorkerCommandV1(
        command_id="command-1",
        execution_id="execution-1",
        generation=4,
        tenant_id="tenant-1",
        resource_project_id="7",
        projection_project_id="7",
        agent_execution=agent_pb2.AgentExecutionCommandV1(
            request_entry_id="agent-request",
            client_stream_id="conversation-1",
            client_message_id="message-1",
            sio_event="chat_predict",
        ),
    )
    verified = VerifiedWorkerCommand(
        envelope=envelope_pb2.WorkerExecutionEnvelopeV1(fence=fence),
        command=command,
    )

    frame = build_node_event_output_frame(
        verified,
        events[0],
        sequence=8,
        occurred_at_unix_millis=1_700_000_000_000,
        claim_handoff_watermark=7,
    )

    payload = events[0].SerializeToString(deterministic=True)
    assert frame.stream_id == "execution-1:4"
    assert frame.logical_output_id == "node-event:execution-1:8"
    assert frame.event_id == "command-1:8"
    assert frame.fence == fence
    assert frame.claim_handoff_watermark == 7
    assert bytes(frame.payload_digest.value) == hashlib.sha256(payload).digest()
    assert frame.WhichOneof("payload") == "node_event"
    assert not frame.terminal

    receipt = control_pb2.ClaimReceiptV1(
        identity=common_pb2.ExecutionIdentityV1(
            tenant_id=command.tenant_id,
            resource_project_id=command.resource_project_id,
            projection_project_id=command.projection_project_id,
            command_id=command.command_id,
            execution_id=command.execution_id,
            generation=command.generation,
        )
    )
    assert _validate_pending_output(
        frame,
        command=command,
        receipt=receipt,
    ) is False


def test_load_skill_projects_cumulative_compact_applied_skills() -> None:
    callback, events = _callback()
    callback.configure_skills(
        applied_skills=[
            {"skill_id": 1, "name": "Review", "icon_meta": {"icon": "review"}}
        ],
        attached_skills=[
            {
                "skill_id": 2,
                "name": "Deploy",
                "description": "Deployment rules",
                "icon_meta": {"icon": "deploy"},
                "instructions": "must not enter browser events",
            }
        ],
    )

    callback.on_tool_start(
        {"name": "load_skill", "metadata": {"display_name": "Skills"}},
        "ignored",
        run_id="load-deploy",
        metadata={"toolkit_name": "skills"},
        inputs={"skill": "deploy"},
    )
    callback.on_tool_end(
        'Skill "Deploy" is now active\n\nmust not enter browser events',
        run_id="load-deploy",
    )
    callback.emit_terminal(
        {"response": "done", "thread_id": "thread-1"},
        _request_payload(),
    )

    decoded = [_json(event) for event in events]
    start = next(event for event in decoded if event["type"] == "agent_tool_start")
    assert start["response_metadata"]["tool_meta"]["loaded_skill"] == "Deploy"
    assert start["response_metadata"]["tool_meta"]["icon_meta"] == {
        "icon": "deploy"
    }
    expected = [
        {"skill_id": 1, "name": "Review", "icon_meta": {"icon": "review"}},
        {"skill_id": 2, "name": "Deploy", "icon_meta": {"icon": "deploy"}},
    ]
    partial = [event for event in decoded if event["type"] == "partial_message"][-1]
    terminal = decoded[-1]
    assert partial["response_metadata"]["invoked_skills"] == expected
    assert terminal["type"] == "full_message"
    assert terminal["response_metadata"]["invoked_skills"] == expected
    assert "must not enter browser events" not in str(
        partial["response_metadata"]["invoked_skills"]
    )
    assert "must not enter browser events" not in str(
        terminal["response_metadata"]["invoked_skills"]
    )
    assert decoded[2]["response_metadata"]["tool_output"] == (
        'Skill "Deploy" is active.'
    )
    assert "must not enter browser events" not in str(decoded)


def test_nested_load_skill_uses_admission_frozen_child_identity() -> None:
    callback, events = _callback()
    callback.configure_skills(
        applied_skills=[],
        attached_skills=[],
        nested_skill_registry=[
            {
                "application_id": 31,
                "application_version_id": 41,
                "application_name": "child-agent",
                "skills": [
                    {
                        "skill_id": 7,
                        "name": "Deploy",
                        "icon_meta": {"icon": "deploy"},
                    }
                ],
            }
        ],
    )

    callback.on_tool_start(
        {"name": "load_skill", "metadata": {"display_name": "Skills"}},
        "ignored",
        run_id="nested-load-deploy",
        metadata={
            "parent_agent_name": "child-agent",
            "parent_agent_path": [{"name": "child-agent", "call_id": "call-1"}],
        },
        inputs={"skill": "deploy"},
    )
    callback.on_tool_end(
        'Skill "Deploy" is now active\n\nchild instructions must not enter events',
        run_id="nested-load-deploy",
    )

    decoded = [_json(event) for event in events]
    start = decoded[0]
    assert start["response_metadata"]["tool_meta"]["loaded_skill"] == "Deploy"
    assert start["response_metadata"]["tool_meta"]["icon_meta"] == {
        "icon": "deploy"
    }
    partial = decoded[-1]
    assert partial["response_metadata"]["invoked_skills"] == [
        {"skill_id": 7, "name": "Deploy", "icon_meta": {"icon": "deploy"}}
    ]
    assert "child instructions" not in str(decoded)


def test_nested_skill_registry_is_collected_from_adhoc_and_saved_roots() -> None:
    registry = [
        {
            "application_id": 31,
            "application_version_id": 41,
            "application_name": "child-agent",
            "skills": [{"skill_id": 7, "name": "Deploy", "icon_meta": None}],
        }
    ]
    payload = SimpleNamespace(
        tools=[
            {
                "type": "application",
                "nested_skill_registry": registry,
            }
        ],
        application={
            "version_details": {
                "tools": [
                    {
                        "type": "application",
                        "nested_skill_registry": registry,
                    }
                ]
            }
        },
    )

    assert nested_skill_registry_from_payload(payload) == registry + registry


def test_terminal_application_details_omit_skill_instruction_bodies() -> None:
    callback, events = _callback()
    payload = _request_payload()
    payload.application = {
        "id": 11,
        "version_id": 22,
        "version_details": {
            "skills": [
                {
                    "skill_id": 2,
                    "name": "Deploy",
                    "description": "Deployment rules",
                    "instructions": "must not enter browser events",
                }
            ]
        },
    }

    callback.emit_terminal(
        {"response": "done", "thread_id": "thread-1"},
        payload,
    )

    event = _json(events[-1])
    skill = event["response_metadata"]["application_details"][
        "version_details"
    ]["skills"][0]
    assert skill["name"] == "Deploy"
    assert skill["description"] == "Deployment rules"
    assert "instructions" not in skill
    assert "must not enter browser events" not in str(event)


def test_load_skill_already_active_is_deduplicated() -> None:
    callback, events = _callback()
    callback.configure_skills(
        applied_skills=[{"skill_id": 2, "name": "Deploy", "icon_meta": None}],
        attached_skills=[{"skill_id": 2, "name": "Deploy", "icon_meta": None}],
    )
    callback.on_tool_start(
        {"name": "load_skill"},
        "ignored",
        run_id="load-deploy",
        inputs={"skill": "Deploy"},
    )
    callback.on_tool_end(
        'Skill "Deploy" is already active for this conversation.',
        run_id="load-deploy",
    )

    partial = [_json(event) for event in events if event.type == "partial_message"][-1]
    assert partial["response_metadata"]["invoked_skills"] == [
        {"skill_id": 2, "name": "Deploy", "icon_meta": None}
    ]


def test_project_context_snapshot_is_not_in_public_application_details():
    from elitea_worker.handlers.agent_events import _public_application_details

    for skills in (None, []):
        version = {"project_context": {"content": "PRIVATE_PROJECT_INSTRUCTIONS"}}
        if skills is not None:
            version["skills"] = skills
        source = {"version_details": version}
        assert "PRIVATE_PROJECT_INSTRUCTIONS" not in str(_public_application_details(source))
        assert source["version_details"]["project_context"]["content"] == "PRIVATE_PROJECT_INSTRUCTIONS"


def test_project_context_tool_trace_withholds_instruction_body():
    callback, events = _callback()
    callback.on_tool_start({"name": "read_project_context"}, "{}", run_id="project-context")
    callback.on_tool_end("PRIVATE_PROJECT_INSTRUCTIONS", run_id="project-context")
    assert "PRIVATE_PROJECT_INSTRUCTIONS" not in str([_json(event) for event in events])
    assert "Project Context is active." in str([_json(event) for event in events])


def test_oversized_tool_result_is_chunked_rather_than_refused() -> None:
    """CHUNKED TOOL OUTPUT (#956), the emit half.

    An 80,000-character tool result — well inside the SDK artifact toolkit's
    own 200,000-character agent-path cap — used to raise
    ``RESOURCE_EXHAUSTED: The agent event exceeds its output limit`` here, and
    the user saw a failed tool call for a file that had been read perfectly
    well. It must now ride as an ordered sequence of frame-sized chunk events
    that reassemble BYTE FOR BYTE, with the completed call naming the count and
    the digest instead of text it could not carry.
    """
    import json

    from elitea_worker.handlers.agent_events import TOOL_OUTPUT_CHUNK_EVENT
    from elitea_worker.protocol.node_event import MAX_CURRENT_NODE_EVENT_JSON_BYTES

    callback, events = _callback()
    callback.on_tool_start(
        {"name": "read_file", "metadata": {"display_name": "Artifact"}},
        "ignored",
        run_id="run-big",
        metadata={"toolkit_name": "artifact"},
        inputs={"filename": "big.txt"},
    )
    payload = (
        "AUTOTESTMED the quick brown fox jumps over the lazy dog 0123456789\n" * 1_213
    )
    assert len(payload) > 80_000
    callback.on_tool_end(payload, run_id="run-big")

    decoded = [_json(event) for event in events]
    types = [event["type"] for event in decoded]
    assert types[0] == "agent_tool_start"
    chunks = [event for event in decoded if event["type"] == TOOL_OUTPUT_CHUNK_EVENT]
    assert len(chunks) > 1, "an 80k result must span frames"

    # Every chunk is ONE output frame. This is the property the whole mechanism
    # exists for, so it is asserted on the encoded event rather than the text.
    for chunk in chunks:
        encoded = json.dumps(chunk, ensure_ascii=False, separators=(",", ":")).encode(
            "utf-8"
        )
        assert len(encoded) <= MAX_CURRENT_NODE_EVENT_JSON_BYTES

    # The chunks precede the completed call, so main holds the whole value by
    # the time it sees the entry that names its digest.
    assert types.index(TOOL_OUTPUT_CHUNK_EVENT) < types.index("agent_tool_end")

    reassembled = ""
    digest = chunks[0]["response_metadata"]["tool_output_chunk"]["tool_output_sha256"]
    for index, chunk in enumerate(chunks):
        position = chunk["response_metadata"]["tool_output_chunk"]
        assert position["tool_call_id"] == "run-big"
        assert position["index"] == index
        assert position["total"] == len(chunks)
        assert position["tool_output_sha256"] == digest
        reassembled += chunk["content"]
    assert reassembled == payload
    assert digest == hashlib.sha256(payload.encode("utf-8")).hexdigest()

    completed = decoded[types.index("agent_tool_end")]["response_metadata"]
    assert completed["tool_output"] == ""
    assert completed["tool_output_chunks"] == {
        "total": len(chunks),
        "tool_output_sha256": digest,
    }
    # …and the stored entry is the chunked one, so the partial message that
    # follows does not try to carry the same oversized value again.
    trailing = decoded[-1]["response_metadata"]["tool_calls"]["run-big"]
    assert trailing["tool_output"] == ""
    assert trailing["tool_output_chunks"]["total"] == len(chunks)


def test_a_tool_result_that_fits_is_not_chunked() -> None:
    """Chunking a small result would change every consumer's shape for nothing."""
    from elitea_worker.handlers.agent_events import TOOL_OUTPUT_CHUNK_EVENT

    callback, events = _callback()
    callback.on_tool_start(
        {"name": "read_file", "metadata": {"display_name": "Artifact"}},
        "ignored",
        run_id="run-small",
        metadata={"toolkit_name": "artifact"},
        inputs={"filename": "small.txt"},
    )
    callback.on_tool_end("a short result", run_id="run-small")

    decoded = [_json(event) for event in events]
    assert all(event["type"] != TOOL_OUTPUT_CHUNK_EVENT for event in decoded)
    completed = decoded[2]["response_metadata"]
    assert completed["tool_output"] == "a short result"
    assert "tool_output_chunks" not in completed
