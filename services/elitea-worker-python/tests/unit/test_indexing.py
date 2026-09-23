from __future__ import annotations

import asyncio
import hashlib
import json
from pathlib import Path
import threading
from pathlib import Path
from typing import Any
from uuid import UUID

import pytest
from elitea_sdk.runtime.exceptions import BudgetExceededError

from elitea.runtime.v1 import (
    command_pb2,
    common_pb2,
    envelope_pb2,
    indexing_pb2,
    output_pb2,
)

from elitea_worker.agents.sdk_adapter import (
    EliteaSdkIndexingAdapter,
    SdkBudgetExceeded,
)
from elitea_worker.constants import MAX_WORKER_COMMAND_BYTES
from elitea_worker.execution.errors import InternalFailure, InvalidInput, ResourceExhausted
from elitea_worker.execution.supervisor import ExecutionSupervisor
from elitea_worker.handlers.indexing import (
    CurrentIndexNodeEventCallback,
    CurrentIndexNodeEventContext,
    CurrentIndexTerminalStatus,
    IndexIngestHandler,
    IndexIngestInputBinding,
    IndexIngestResult,
    ResolvedIndexIngestInput,
)
from elitea_worker.protocol.codec import (
    VerifiedWorkerCommand,
    build_node_event_output_frame,
)
from elitea_worker.protocol.indexing import (
    INDEX_INGEST_FAILURE_SAFE_MESSAGE,
    bind_result_artifact,
    bind_result_summary,
    request_from,
    resolve_embedding_binding,
)
from elitea_worker.protocol.node_event import encode_current_node_event_json


class _ClientStub:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def test_toolkit_tool(self, **kwargs: Any) -> dict[str, Any]:
        self.calls.append(kwargs)
        kwargs["toolkit_config"]["settings"]["space"] = "MUTATED"
        kwargs["tool_params"]["filters"]["state"] = "MUTATED"
        kwargs["llm_config"]["sampling"]["temperature"] = 1
        return {"success": True, "result": "indexed"}


class _SdkStub:
    def __init__(self, result: dict[str, Any]) -> None:
        self.result = result
        self.calls: list[dict[str, Any]] = []
        self.thread_names: list[str] = []

    def ingest(self, **kwargs: Any) -> dict[str, Any]:
        self.calls.append(kwargs)
        self.thread_names.append(threading.current_thread().name)
        return self.result


def test_adapter_invokes_current_sdk_method_once_with_exact_arguments() -> None:
    client = _ClientStub()
    adapter = object.__new__(EliteaSdkIndexingAdapter)
    adapter._client = client
    toolkit_config = {"type": "confluence", "settings": {"space": "ENG"}}
    tool_params = {
        "index_name": "knowledge",
        "clean_index": False,
        "filters": {"state": "current"},
    }
    runtime_config = {"callbacks": [object()]}
    llm_config = {"sampling": {"temperature": 0}}
    mcp_tokens = {"server": "token"}

    result = adapter.ingest(
        toolkit_config=toolkit_config,
        tool_params=tool_params,
        runtime_config=runtime_config,
        llm_model="gpt-test",
        llm_config=llm_config,
        mcp_tokens=mcp_tokens,
    )

    assert result == {"success": True, "result": "indexed"}
    assert len(client.calls) == 1
    call = client.calls[0]
    assert list(call) == [
        "toolkit_config",
        "tool_name",
        "tool_params",
        "runtime_config",
        "llm_model",
        "llm_config",
        "mcp_tokens",
    ]
    assert call["tool_name"] == "index_data"
    assert call["toolkit_config"] is not toolkit_config
    assert call["tool_params"] is not tool_params
    assert call["llm_config"] is not llm_config
    assert call["runtime_config"] is runtime_config
    assert call["mcp_tokens"] is mcp_tokens
    assert toolkit_config["settings"]["space"] == "ENG"
    assert tool_params["filters"]["state"] == "current"
    assert llm_config["sampling"]["temperature"] == 0


def test_adapter_preserves_current_proxy_embedding_model_on_invocation_copy() -> None:
    client = _ClientStub()
    adapter = object.__new__(EliteaSdkIndexingAdapter)
    adapter._client = client
    toolkit_config = {
        "type": "confluence",
        "settings": {
            "space": "ENG",
            "embedding_model": "embedding-current",
        },
    }

    adapter.ingest(
        toolkit_config=toolkit_config,
        tool_params={"filters": {"state": "current"}},
        runtime_config={},
        llm_model=None,
        llm_config={"sampling": {"temperature": 0}},
        mcp_tokens=None,
    )

    assert (
        client.calls[0]["toolkit_config"]["settings"]["embedding_model"]
        == "embedding-current"
    )
    assert toolkit_config["settings"]["embedding_model"] == "embedding-current"


@pytest.mark.parametrize(
    "scope",
    ["project_budget_exceeded", "member_budget_exceeded"],
)
def test_adapter_neutralizes_typed_sdk_budget_error(scope: str) -> None:
    canary = f"BUDGET_SECRET_CANARY_{scope}"

    class _BudgetBlockedClient:
        def test_toolkit_tool(self, **kwargs: Any) -> dict[str, Any]:
            raise BudgetExceededError(canary, scope)

    adapter = object.__new__(EliteaSdkIndexingAdapter)
    adapter._client = _BudgetBlockedClient()

    with pytest.raises(SdkBudgetExceeded) as caught:
        adapter.ingest(
            toolkit_config={"type": "github", "settings": {}},
            tool_params={},
            runtime_config={},
            llm_model=None,
            llm_config={},
            mcp_tokens=None,
        )

    assert str(caught.value) == ""
    assert canary not in str(caught.value)


def test_embedding_binding_is_required_only_by_frozen_toolkit_contract() -> None:
    toolkit = _resolved(
        "toolkit-config",
        {"type": "github", "settings": {}},
        b"t" * 32,
    )
    assert resolve_embedding_binding(toolkit, None) is None

    toolkit.value["settings"]["embedding_model"] = "embedding-current"
    with pytest.raises(InvalidInput, match="required embedding binding"):
        resolve_embedding_binding(toolkit, None)

    binding = _resolved(
        "embedding-binding",
        {
            "schema_version": "elitea.index.embedding-binding.v2",
            "model_name": "embedding-current",
            "resolved_model_group": "42_embedding-current",
            "route": "project",
            "model_project_id": 42,
            "configuration_project_id": 42,
            "configuration_uuid": "00000000-0000-4000-8000-000000000107",
            "configuration_digest": (
                f"sha256:{hashlib.sha256(b'configuration').hexdigest()}"
            ),
        },
        b"e" * 32,
    )
    resolved = resolve_embedding_binding(toolkit, binding)
    assert resolved is not None
    assert resolved.resolved_model_group == "42_embedding-current"
    assert resolved.input is binding

    binding.value["model_name"] = "other"
    with pytest.raises(InvalidInput, match="does not match"):
        resolve_embedding_binding(toolkit, binding)


def test_embedding_binding_validation_shared_go_python_fixture() -> None:
    fixture_path = (
        Path(__file__).resolve().parents[4]
        / "services"
        / "elitea-main"
        / "internal"
        / "application"
        / "indexing"
        / "testdata"
        / "embedding_binding_validation_v2.json"
    )
    cases = json.loads(fixture_path.read_text())
    for case in cases:
        document = case["document"]
        toolkit = _resolved(
            "toolkit-config",
            {
                "type": "github",
                "settings": {"embedding_model": document["model_name"]},
            },
            b"t" * 32,
        )
        binding = _resolved("embedding-binding", document, b"e" * 32)
        if case["valid"]:
            assert resolve_embedding_binding(toolkit, binding) is not None, case["name"]
        else:
            with pytest.raises(InvalidInput):
                resolve_embedding_binding(toolkit, binding)


@pytest.mark.parametrize(
    ("selected_tools", "expected"),
    [
        (
            ["index_data", "list_collections", "search_index"],
            ["index_data", "search_index", "list_indexes"],
        ),
        (
            ["index_data", "list_collections", "list_indexes"],
            ["index_data", "list_indexes"],
        ),
        (
            ["index_data", "unknown_tool"],
            ["index_data", "unknown_tool"],
        ),
        ("list_collections", "list_collections"),
    ],
)
def test_adapter_applies_only_current_index_tool_name_compatibility(
    selected_tools: object,
    expected: object,
) -> None:
    client = _ClientStub()
    adapter = object.__new__(EliteaSdkIndexingAdapter)
    adapter._client = client
    toolkit_config = {
        "type": "github",
        "settings": {
            "repository": "owner/repository",
            "selected_tools": selected_tools,
        },
    }

    adapter.ingest(
        toolkit_config=toolkit_config,
        tool_params={"filters": {"state": "current"}},
        runtime_config={},
        llm_model=None,
        llm_config={"sampling": {"temperature": 0}},
        mcp_tokens=None,
    )

    assert toolkit_config["settings"]["selected_tools"] == selected_tools
    assert client.calls[0]["toolkit_config"]["settings"]["selected_tools"] == expected
    assert client.calls[0]["toolkit_config"]["settings"]["repository"] == "owner/repository"


def test_current_index_callback_maps_state_shape_without_redeemed_configuration() -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            message_id="message-1",
            sio_event="chat_predict",
            display_name="configurations",
        ),
        events.append,
    )
    callback.on_custom_event(
        "index_data_status",
        {
            "id": "meta-1",
            "index_name": "knowledge",
            "state": "completed",
            "error": None,
            "reindex": True,
            "indexed": 11,
            "updated": 2,
            "created_at": 1_700_000_000.0,
            "updated_on": 1_700_000_100.0,
            "toolkit_id": 9,
            "toolkit_config": {"settings": {"token": "redeemed-secret"}},
        },
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
        metadata={"source": "current-sdk"},
    )
    callback.raise_if_failed()

    assert len(events) == 1
    current = json.loads(encode_current_node_event_json(events[0]))
    assert set(current) == {
        "type",
        "stream_id",
        "message_id",
        "question_id",
        "content",
        "thinking",
        "response_metadata",
        "references",
        "sio_event",
        "created_at",
        "parent_message_id",
        "agent_name",
        "execution_generation",
    }
    assert current["type"] == "agent_index_data_status"
    assert current["stream_id"] == "execution-1"
    assert current["content"] is None
    metadata = current["response_metadata"]
    assert metadata["name"] == "index_data_status"
    assert metadata["run_id"] == "00000000-0000-0000-0000-000000000001"
    assert metadata["tool_run_id"] == metadata["run_id"]
    assert metadata["task_id"] == "execution-1"
    assert metadata["initiator"] == "user"
    assert metadata["project_id"] == 42
    assert metadata["user_id"] == 7
    assert metadata["toolkit_id"] == 9
    assert metadata["state"] == "completed"
    assert "toolkit_config" not in metadata
    assert "redeemed-secret" not in json.dumps(current)


@pytest.mark.parametrize(
    ("event_name", "event_type", "data", "expected_fields"),
    [
        (
            "thinking_step",
            "agent_thinking_step",
            {
                "message": "20 files processed",
                "tool_name": "loader",
                "toolkit": "EliteaGitHubAPIWrapper",
                "toolkit_config": {
                    "settings": {"private_token": "redeemed-secret"}
                },
                "tool_params": {"api_key": "redeemed-secret"},
            },
            {
                "message": "20 files processed",
                "tool_name": "loader",
                "toolkit": "EliteaGitHubAPIWrapper",
            },
        ),
        (
            "thinking_step_update",
            "agent_thinking_step_update",
            {
                "message": "20 files processed",
                "tool_name": "loader",
                "toolkit": "EliteaGitHubAPIWrapper",
                "markdown": True,
                "toolkit_config": {
                    "settings": {"private_token": "redeemed-secret"}
                },
                "tool_params": {"api_key": "redeemed-secret"},
            },
            {
                "message": "20 files processed",
                "tool_name": "loader",
                "toolkit": "EliteaGitHubAPIWrapper",
                "markdown": True,
            },
        ),
    ],
)
def test_current_index_callback_maps_current_thinking_shape_without_enrichment(
    event_name: str,
    event_type: str,
    data: dict[str, Any],
    expected_fields: dict[str, Any],
) -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            message_id="message-1",
            sio_event="chat_predict",
            display_name="configurations",
        ),
        events.append,
    )
    callback.on_custom_event(
        event_name,
        data,
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
        metadata={
            "initiator": "user",
            "tool_name": "index_data",
            "display_name": "configurations",
        },
    )
    callback.raise_if_failed()

    assert len(events) == 1
    current = json.loads(encode_current_node_event_json(events[0]))
    assert set(current) == {
        "type",
        "stream_id",
        "message_id",
        "question_id",
        "content",
        "thinking",
        "response_metadata",
        "references",
        "sio_event",
        "created_at",
        "parent_message_id",
        "agent_name",
        "execution_generation",
    }
    assert current["type"] == event_type
    assert current["stream_id"] == "execution-1"
    assert current["message_id"] == "message-1"
    assert current["sio_event"] == "chat_predict"
    assert current["content"] is None
    assert current["references"] == []
    metadata = current["response_metadata"]
    assert metadata == {
        "name": event_name,
        "run_id": "00000000-0000-0000-0000-000000000001",
        "tool_run_id": "00000000-0000-0000-0000-000000000001",
        "metadata": {
            "initiator": "user",
            "tool_name": "index_data",
            "display_name": "configurations",
        },
        "datetime": metadata["datetime"],
        **expected_fields,
    }
    assert "project_id" not in metadata
    assert "user_id" not in metadata
    assert "toolkit_id" not in metadata
    assert "task_id" not in metadata
    assert "redeemed-secret" not in json.dumps(current)


def test_current_index_thinking_wire_matches_cross_language_fixture() -> None:
    fixture = json.loads(
        (
            Path(__file__).resolve().parents[1]
            / "fixtures"
            / "current_index_thinking_step.json"
        ).read_text(encoding="utf-8")
    )
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id=fixture["stream_id"],
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            message_id=fixture["message_id"],
            sio_event=fixture["sio_event"],
            display_name="configurations",
        ),
        events.append,
    )
    callback.on_custom_event(
        "thinking_step",
        {
            "message": "10 files processed",
            "tool_name": "loader",
            "toolkit": "EliteaGitHubAPIWrapper",
        },
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
        metadata={"ignored_by_the_standalone_mapper": True},
    )
    callback.raise_if_failed()

    assert len(events) == 1
    current = json.loads(encode_current_node_event_json(events[0]))
    # The callback owns wall-clock capture. Normalize only those two values;
    # every identity, metadata, and business field must match the fixture that
    # the Go projection integration test consumes.
    current["created_at"] = fixture["created_at"]
    current["response_metadata"]["datetime"] = fixture["response_metadata"][
        "datetime"
    ]
    assert current == fixture
    assert set(current["response_metadata"]) == {
        "name",
        "run_id",
        "tool_run_id",
        "metadata",
        "datetime",
        "message",
        "tool_name",
        "toolkit",
    }


def test_current_index_tool_lifecycle_is_correlated_and_credential_free() -> None:
    events = []
    canary = "REDEEMED_TOOL_LIFECYCLE_CANARY"
    run_id = UUID("00000000-0000-0000-0000-000000000001")
    parent_run_id = UUID("00000000-0000-0000-0000-000000000099")
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="conversation-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            message_id="message-1",
            sio_event="chat_predict",
            display_name="configurations",
        ),
        events.append,
    )

    callback.on_tool_start(
        {"name": "index_data", "description": canary},
        canary,
        run_id=run_id,
        parent_run_id=parent_run_id,
        metadata={"credential": canary},
        inputs={"token": canary},
    )
    callback.on_custom_event(
        "thinking_step",
        {
            "message": "10 files processed",
            "tool_name": "loader",
            "toolkit": "EliteaGitHubAPIWrapper",
            "credential": canary,
        },
        run_id=UUID("00000000-0000-0000-0000-000000000003"),
        metadata={"credential": canary},
    )
    callback.on_tool_end(
        {"token": canary},
        run_id=run_id,
        parent_run_id=parent_run_id,
    )
    events.append(callback.finish_tool(success=True))
    callback.raise_if_failed()

    current = [
        json.loads(encode_current_node_event_json(event)) for event in events
    ]
    assert [event["type"] for event in current] == [
        "agent_tool_start",
        "agent_thinking_step",
        "agent_tool_end",
    ]
    assert {
        (event["stream_id"], event["message_id"], event["sio_event"])
        for event in current
    } == {("conversation-1", "message-1", "chat_predict")}
    assert {
        event["response_metadata"]["tool_run_id"] for event in current
    } == {str(run_id)}
    assert current[0]["response_metadata"] == {
        "tool_name": "index_data",
        "tool_run_id": str(run_id),
        "timestamp_start": current[0]["response_metadata"]["timestamp_start"],
        "metadata": {
            "initiator": "user",
            "tool_name": "index_data",
            "display_name": "configurations",
        },
    }
    assert current[-1]["content"] is None
    assert current[-1]["response_metadata"]["finish_reason"] == "stop"
    assert canary not in json.dumps(current)


def test_current_index_tool_error_omits_exception_detail() -> None:
    events = []
    canary = "RAW_EXCEPTION_CANARY"
    run_id = UUID("00000000-0000-0000-0000-000000000002")
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="conversation-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            message_id="message-1",
            sio_event="chat_predict",
        ),
        events.append,
    )

    callback.on_tool_start({}, canary, run_id=run_id)
    callback.on_tool_error(RuntimeError(canary), run_id=run_id)
    events.append(callback.finish_tool(success=False))

    current = [
        json.loads(encode_current_node_event_json(event)) for event in events
    ]
    assert [event["type"] for event in current] == [
        "agent_tool_start",
        "agent_tool_error",
    ]
    assert current[-1]["response_metadata"]["finish_reason"] == "error"
    assert current[-1]["response_metadata"]["tool_run_id"] == str(run_id)
    assert canary not in json.dumps(current)


def test_current_index_failure_status_is_safe_and_emitted_once() -> None:
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="conversation-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            index_name="knowledge",
            message_id="message-1",
            sio_event="chat_predict",
        ),
        lambda event: None,
    )

    event = callback.finish_index_status_on_failure()

    assert event is not None
    assert callback.finish_index_status_on_failure() is None
    current = json.loads(encode_current_node_event_json(event))
    assert current["type"] == "agent_index_data_status"
    assert current["response_metadata"] == {
        "task_id": "execution-1",
        "index_name": "knowledge",
        "state": "failed",
        "error": "Indexing reported an error.",
        "indexed": 0,
        "updated": 0,
        "toolkit_id": 9,
        "initiator": "user",
        "project_id": 42,
        "user_id": 7,
    }
    assert len(encode_current_node_event_json(event)) < 1024


def test_current_index_terminal_status_suppresses_failure_fallback() -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="conversation-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
            index_name="knowledge",
        ),
        events.append,
    )

    callback.on_custom_event(
        "index_data_status",
        {"index_name": "knowledge", "state": "partly_indexed"},
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )

    assert callback.finish_index_status_on_failure() is None
    assert len(events) == 1


def test_current_index_callback_filters_only_scheduled_transient_events() -> None:
    def emitted_types(initiator: str) -> tuple[list[str], list[dict[str, Any]]]:
        events = []
        callback = CurrentIndexNodeEventCallback(
            CurrentIndexNodeEventContext(
                stream_id="conversation-1" if initiator != "schedule" else "",
                task_id="execution-1",
                initiator=initiator,
                project_id=42,
                user_id=7,
                toolkit_id=9,
                message_id="message-1" if initiator != "schedule" else None,
                sio_event="chat_predict" if initiator != "schedule" else None,
            ),
            events.append,
        )
        run_id = UUID("00000000-0000-0000-0000-000000000004")
        callback.on_tool_start({}, "", run_id=run_id)
        callback.on_custom_event(
            "thinking_step",
            {"message": "10 files processed"},
            run_id=run_id,
        )
        callback.on_custom_event(
            "thinking_step_update",
            {"message": "20 files processed"},
            run_id=run_id,
        )
        callback.on_custom_event(
            "index_data_status",
            {"index_name": "knowledge", "state": "completed"},
            run_id=run_id,
        )
        callback.on_custom_event(
            "index_data_removed",
            {"index_name": "obsolete"},
            run_id=run_id,
        )
        callback.on_tool_end({}, run_id=run_id)
        terminal = callback.finish_tool(success=True)
        if terminal is not None:
            events.append(terminal)
        callback.raise_if_failed()
        current = [
            json.loads(encode_current_node_event_json(event)) for event in events
        ]
        return [event["type"] for event in current], current

    manual_types, manual = emitted_types("user")
    scheduled_types, scheduled = emitted_types("schedule")

    assert manual_types == [
        "agent_tool_start",
        "agent_thinking_step",
        "agent_thinking_step_update",
        "agent_index_data_status",
        "agent_index_data_removed",
        "agent_tool_end",
    ]
    assert scheduled_types == [
        "agent_index_data_status",
        "agent_index_data_removed",
    ]
    assert all(event["stream_id"] is None for event in scheduled)
    assert all(event["message_id"] is None for event in scheduled)
    assert all(event["sio_event"] is None for event in scheduled)
    assert all(
        event["response_metadata"]["metadata"]["initiator"] == "schedule"
        for event in scheduled
    )
    assert manual[0]["stream_id"] == "conversation-1"


def test_current_index_callback_projects_an_oversized_status_error() -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
        ),
        events.append,
    )

    callback.on_custom_event(
        "index_data_status",
        {
            "index_name": "knowledge",
            "state": "failed",
            "error": "x" * (48 * 1024),
        },
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )
    callback.raise_if_failed()

    current = json.loads(encode_current_node_event_json(events[0]))
    assert current["response_metadata"]["error"] == "Indexing reported an error."
    assert len(encode_current_node_event_json(events[0])) < 48 * 1024


@pytest.mark.parametrize(
    "message",
    [
        "Loading the documents to index...{'chunking_config': {'prompt': 'LEAK_CANARY'}}",
        "access_token=LEAK_CANARY",
        "https://user:LEAK_CANARY@example.test/private",
        "<ProviderClient object at 0xDEADBEEF> LEAK_CANARY",
        "{'nested': {'settings': {'api_key': 'LEAK_CANARY'}}}",
        "/private/LEAK_CANARY/indexing.log",
        "LEAK_CANARY" * (48 * 1024),
    ],
)
def test_current_index_callback_projects_unsafe_progress_message(message: str) -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
        ),
        events.append,
    )

    callback.on_custom_event(
        "thinking_step",
        {
            "message": message,
            "tool_name": "loader",
            "toolkit": "EliteaGitHubAPIWrapper",
        },
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )
    callback.raise_if_failed()

    current = json.loads(encode_current_node_event_json(events[0]))
    metadata = current["response_metadata"]
    assert "LEAK_CANARY" not in json.dumps(current)
    assert metadata["message"] in {
        "Loading the documents to index.",
        "Indexing progress updated.",
    }
    assert len(encode_current_node_event_json(events[0])) < 48 * 1024


def test_current_index_callback_preserves_safe_progress_message() -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
        ),
        events.append,
    )
    message = "10 files processed"

    callback.on_custom_event(
        "thinking_step",
        {
            "message": message,
            "tool_name": "loader",
            "toolkit": "EliteaGitHubAPIWrapper",
        },
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )

    current = json.loads(encode_current_node_event_json(events[0]))
    assert current["response_metadata"]["message"] == message


def test_current_index_callback_maps_index_removed_shape() -> None:
    events = []
    callback = CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
        ),
        events.append,
    )

    callback.on_custom_event(
        "index_data_removed",
        {"index_name": "knowledge", "toolkit_id": 9, "project_id": 42},
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )

    current = json.loads(encode_current_node_event_json(events[0]))
    assert current["type"] == "agent_index_data_removed"
    assert current["response_metadata"]["index_name"] == "knowledge"
    assert current["response_metadata"]["toolkit_id"] == 9
    assert current["response_metadata"]["project_id"] == 42


def test_node_event_frame_reuses_claim_identity_digest_and_handoff_sequence() -> None:
    command = command_pb2.WorkerCommandV1(
        command_id="command-1",
        execution_id="execution-1",
        generation=2,
        tenant_id="tenant-1",
        resource_project_id="42",
        projection_project_id="42",
        index_ingest=indexing_pb2.IndexIngestCommandV1(
            toolkit_configuration_entry_id="toolkit",
            tool_parameters_entry_id="parameters",
        ),
    )
    fence = common_pb2.ExecutionFenceV1(
        workload_session_id="worker-session",
        producer_id="worker-1",
        claim_attempt=3,
        lease_epoch=4,
        fence_token=b"f" * 32,
    )
    verified = VerifiedWorkerCommand(
        envelope=envelope_pb2.WorkerExecutionEnvelopeV1(fence=fence),
        command=command,
    )
    event = []
    CurrentIndexNodeEventCallback(
        CurrentIndexNodeEventContext(
            stream_id="execution-1",
            task_id="execution-1",
            initiator="user",
            project_id=42,
            user_id=7,
            toolkit_id=9,
        ),
        event.append,
    ).on_custom_event(
        "index_data_status",
        {"index_name": "knowledge", "state": "in_progress", "toolkit_id": 9},
        run_id=UUID("00000000-0000-0000-0000-000000000001"),
    )

    frame = build_node_event_output_frame(
        verified,
        event[0],
        sequence=4,
        occurred_at_unix_millis=1_700_000_000_000,
        claim_handoff_watermark=3,
    )

    payload = event[0].SerializeToString(deterministic=True)
    assert frame.sequence == 4
    assert frame.event_id == "command-1:4"
    assert frame.logical_output_id == "node-event:execution-1:4"
    assert frame.stream_id == "execution-1:2"
    assert frame.identity.command_id == command.command_id
    assert frame.fence == fence
    assert frame.claim_handoff_watermark == 3
    assert bytes(frame.payload_digest.value) == hashlib.sha256(payload).digest()
    assert not frame.terminal
    assert not frame.HasField("settlement_proposal")


def test_kernel_uses_bounded_sync_executor_and_keeps_bulk_off_wire() -> None:
    async def run() -> None:
        canary = "TEST_ONLY_INDEX_INGEST_SECRET_CANARY_NOT_A_SECRET"
        sdk = _SdkStub({"success": True, "result": canary + ("x" * 1_000_000)})
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        handler = IndexIngestHandler(sdk, supervisor)
        command = indexing_pb2.IndexIngestCommandV1(
            toolkit_configuration_entry_id="toolkit-config",
            tool_parameters_entry_id="tool-params",
            llm_model_entry_id="llm-model",
            llm_configuration_entry_id="llm-config",
        )
        toolkit = _resolved(
            "toolkit-config",
            {"settings": {"password": canary}},
            b"t" * 32,
        )
        params = _resolved("tool-params", {"index_name": "knowledge"}, b"p" * 32)
        model = _resolved("llm-model", "gpt-test", b"m" * 32)
        llm = _resolved("llm-config", {"api_key": canary}, b"l" * 32)
        request = request_from(
            command,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            toolkit_configuration=toolkit,
            tool_parameters=params,
            llm_model=model,
            llm_configuration=llm,
            mcp_tokens=None,
            runtime_config={"callbacks": []},
        )

        wire_command = command_pb2.WorkerCommandV1(
            protocol_revision="elitea.runtime.v1",
            command_id="command-1",
            idempotency_key="idempotency-1",
            command_type=command_pb2.WORKER_COMMAND_TYPE_V1_INDEX_INGEST,
            execution_id="execution-1",
            generation=1,
            dispatch_ordinal=1,
            tenant_id="tenant-1",
            resource_project_id="project-1",
            projection_project_id="project-1",
            principal_ref="principal-1",
            capability_id="index.ingest.v1",
            capability_version="2",
            resource_class="indexing",
            isolation_class="shared",
            priority=1,
            deadline_unix_millis=1,
            limits_revision="elitea.runtime.limits.conformance.v2",
            index_ingest=command,
        )
        command_bytes = wire_command.SerializeToString(deterministic=True)
        assert len(command_bytes) < MAX_WORKER_COMMAND_BYTES
        assert canary.encode() not in command_bytes
        assert b"index_name" not in command_bytes

        result = await handler.execute(request)
        assert len(sdk.calls) == 1
        assert sdk.thread_names[0].startswith("elitea-sdk-sync")
        assert sdk.thread_names[0] != threading.current_thread().name
        assert result.sdk_result["result"].startswith(canary)

        reference = bind_result_artifact(
            result,
            artifact_id="artifact-1",
            immutable_version="1",
            byte_length=1_000_128,
            digest=b"r" * 32,
        )
        frame = output_pb2.ExecutionOutputFrameV1(
            output_schema_revision="elitea.runtime.execution-output.v1",
            stream_id="execution-1:1",
            event_type=output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT,
            index_ingest=reference,
        )
        frame_bytes = frame.SerializeToString(deterministic=True)
        assert len(frame_bytes) < 1024
        assert canary.encode() not in frame_bytes
        assert frame.WhichOneof("payload") == "index_ingest"
        assert not reference.HasField("mcp_tokens")
        assert reference.result_artifact.byte_length == 1_000_128
        assert reference.result_artifact.classification == "tenant-confidential"
        assert (
            reference.result_artifact.media_type
            == "application/vnd.elitea.index-ingest-result.v1+json"
        )
        await supervisor.shutdown()

    asyncio.run(run())


def test_optional_reference_and_resolved_value_must_match() -> None:
    command = indexing_pb2.IndexIngestCommandV1(
        toolkit_configuration_entry_id="toolkit-config",
        tool_parameters_entry_id="tool-params",
        mcp_tokens_entry_id="mcp-tokens",
    )

    with pytest.raises(InvalidInput):
        request_from(
            command,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            toolkit_configuration=_resolved("toolkit-config", {}, b"t" * 32),
            tool_parameters=_resolved("tool-params", {}, b"p" * 32),
            llm_model=None,
            llm_configuration=None,
            mcp_tokens=None,
            runtime_config={},
        )


def test_inline_summary_projects_only_the_nested_allowlist() -> None:
    canary = "REDEEMED_CREDENTIAL_CANARY"
    result = _index_result(
        {
            "success": True,
            "result": {"status": "partly_indexed", "message": "Indexed with gaps"},
            "toolkit_config": {"settings": {"token": canary}},
            "llm_model": canary,
            "events_dispatched": [{"secret": canary}],
        }
    )

    bound = bind_result_summary(result)
    encoded = bound.SerializeToString(deterministic=True)

    assert bound.result_summary.status == indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED
    assert bound.result_summary.message == "Indexed with gaps"
    assert not bound.HasField("result_artifact")
    assert canary.encode() not in encoded


def test_inline_summary_carries_only_typed_terminal_notification_fields() -> None:
    bound = bind_result_summary(
        _index_result(
            {
                "success": True,
                "result": {"status": "ok", "message": "Indexed safely"},
            }
        ),
        CurrentIndexTerminalStatus(
            state="scheduled_reindex",
            indexed=17,
            updated=4,
            reindex=True,
        ),
    )

    summary = bound.result_summary
    assert (
        summary.terminal_state
        == indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX
    )
    assert summary.indexed == 17
    assert summary.updated == 4
    assert summary.HasField("reindex")
    assert summary.reindex is True


@pytest.mark.parametrize(
    ("message", "expected_status", "expected_message"),
    [
        (
            "Successfully indexed 20 documents (0 chunks).\n"
            "Skipped items (21 total):\n"
            "  - Documents with errors (20): first, second\n"
            "  - Runtime skipped (errors) (1): test_case.md",
            indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR,
            INDEX_INGEST_FAILURE_SAFE_MESSAGE,
        ),
        (
            "Successfully indexed 8 documents (12 chunks). "
            "\nSkipped items (2 total):"
            "\n  - Documents with errors (2): first, second",
            indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED,
            None,
        ),
        (
            "Successfully indexed 61 documents (0 chunks).\n"
            "Skipped items (5 total):\n"
            "  - Files with empty content (2): one, two\n"
            "  - Files with unsupported extension (3): a, b, c",
            indexing_pb2.INDEX_INGEST_STATUS_V1_OK,
            None,
        ),
    ],
)
def test_inline_summary_corrects_generic_parser_failures(
    message: str,
    expected_status: int,
    expected_message: str | None,
) -> None:
    bound = bind_result_summary(
        _index_result(
            {
                "success": True,
                "result": {"status": "ok", "message": message},
            }
        )
    )

    assert bound.result_summary.status == expected_status
    assert bound.result_summary.message == (expected_message or message)


def test_outer_sdk_failure_becomes_fixed_safe_error_summary() -> None:
    canary = "raw endpoint and credential-adjacent detail"
    bound = bind_result_summary(
        _index_result(
            {
                "success": False,
                "error": canary,
                "toolkit_config": {"token": "secret"},
            }
        )
    )

    assert bound.result_summary.status == indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
    assert bound.result_summary.message == INDEX_INGEST_FAILURE_SAFE_MESSAGE
    assert canary.encode() not in bound.SerializeToString(deterministic=True)


@pytest.mark.parametrize("error", [None, "", 7, {"message": "failure"}])
def test_outer_sdk_failure_rejects_malformed_error_shape(error: object) -> None:
    with pytest.raises(InternalFailure):
        bind_result_summary(_index_result({"success": False, "error": error}))


def test_inline_summary_rejects_unbounded_sdk_message() -> None:
    with pytest.raises(ResourceExhausted):
        bind_result_summary(
            _index_result(
                {
                    "success": True,
                    "result": {"status": "ok", "message": "x" * (48 * 1024 + 1)},
                }
            )
        )


def test_kernel_preserves_current_wrapper_llm_defaults_and_fallback() -> None:
    async def run() -> None:
        sdk = _SdkStub({"success": True})
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        handler = IndexIngestHandler(sdk, supervisor)
        command = indexing_pb2.IndexIngestCommandV1(
            toolkit_configuration_entry_id="toolkit-config",
            tool_parameters_entry_id="tool-params",
            llm_configuration_entry_id="llm-config",
        )
        request = request_from(
            command,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            toolkit_configuration=_resolved(
                "toolkit-config", {"type": "confluence"}, b"t" * 32
            ),
            tool_parameters=_resolved("tool-params", {}, b"p" * 32),
            llm_model=None,
            llm_configuration=_resolved(
                "llm-config", {"model_name": "fallback-model"}, b"l" * 32
            ),
            mcp_tokens=None,
            runtime_config={},
        )

        await handler.execute(request)

        assert len(sdk.calls) == 1
        assert sdk.calls[0]["llm_model"] == "fallback-model"
        assert sdk.calls[0]["llm_config"] == {"model_name": "fallback-model"}
        assert sdk.calls[0]["mcp_tokens"] is None
        await supervisor.shutdown()

    asyncio.run(run())


def _resolved(entry_id: str, value: Any, digest: bytes) -> ResolvedIndexIngestInput:
    return ResolvedIndexIngestInput(
        binding=IndexIngestInputBinding(
            entry_id=entry_id,
            immutable_version="1",
            content_digest=digest,
        ),
        value=value,
    )


def _index_result(sdk_result: dict[str, Any]) -> IndexIngestResult:
    return IndexIngestResult(
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        toolkit_configuration=IndexIngestInputBinding(
            entry_id="toolkit-config",
            immutable_version="1",
            content_digest=b"t" * 32,
        ),
        tool_parameters=IndexIngestInputBinding(
            entry_id="tool-params",
            immutable_version="1",
            content_digest=b"p" * 32,
        ),
        llm_model=None,
        llm_configuration=None,
        mcp_tokens=None,
        sdk_result=sdk_result,
    )
