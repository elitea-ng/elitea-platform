"""A failed graph node must settle FAILED, not SUCCEEDED with no output.

The pinned SDK does not always raise for a failed node. ``LLMNode.invoke`` in
``elitea_sdk/runtime/tools/llm.py`` catches every non-budget exception from
``_invoke_llm_internal`` and replaces the node result with one ``AIMessage``
whose content is ``f"Error: {e}"``. LangGraph records no ``__error__`` write,
the graph reaches ``END``, and the worker receives an ordinary object.

``AgentExecutionResultV1`` has no failed terminal state and
``build_output_frame`` maps every one of them to
``EXECUTION_OUTCOME_V1_SUCCEEDED``, so a returned failure settled SUCCEEDED and
the conversation showed nothing. These tests cover the classification, the
event ordering it protects, and the terminal frame the classified failure
produces.
"""

from __future__ import annotations

import ast
import inspect

import pytest
from elitea.runtime.v1 import (
    agent_pb2,
    command_pb2,
    common_pb2,
    envelope_pb2,
    output_pb2,
)
from langchain_core.messages import AIMessage, HumanMessage
from langchain_core.outputs import ChatGeneration, LLMResult

from elitea_worker.execution import delivery
from elitea_worker.execution.errors import InternalFailure
from elitea_worker.handlers.agent_events import (
    CurrentAgentNodeEventCallback,
    CurrentAgentNodeEventContext,
    agent_terminal_failure,
)
from elitea_worker.protocol.codec import VerifiedWorkerCommand, build_output_frame


_NODE_ERROR = "Error: LLMNode requires 'messages' in state for chat-based interaction"


def _callback() -> tuple[CurrentAgentNodeEventCallback, list]:
    published: list = []
    callback = CurrentAgentNodeEventCallback(
        CurrentAgentNodeEventContext(
            execution_id="execution-1",
            stream_id="stream-1",
            message_id="message-1",
            execution_generation="1",
            sio_event="chat",
            thread_id="thread-1",
            project_id=1,
            chat_project_id=1,
        ),
        published.append,
    )
    return callback, published


class TestClassification:
    def test_a_swallowed_node_error_is_a_failure(self) -> None:
        result = {
            "output": _NODE_ERROR,
            "execution_finished": True,
            "messages": [HumanMessage(content="hi"), AIMessage(content=_NODE_ERROR)],
        }

        assert agent_terminal_failure(result, model_responded=False) == "node_error"

    def test_a_finished_turn_with_no_content_is_a_failure(self) -> None:
        result = {"output": "", "execution_finished": True, "messages": []}

        assert (
            agent_terminal_failure(result, model_responded=False)
            == "empty_terminal_output"
        )

    def test_the_flattened_sdk_failure_envelope_is_a_failure(self) -> None:
        result = {
            "success": False,
            "error": "Failed to instantiate toolkit 'github'",
            "output": "Failed to instantiate toolkit 'github'",
            "execution_finished": True,
        }

        assert (
            agent_terminal_failure(result, model_responded=False)
            == "sdk_reported_failure"
        )

    def test_a_completed_turn_is_not_a_failure(self) -> None:
        result = {
            "output": "The pipeline produced a summary.",
            "execution_finished": True,
            "messages": [AIMessage(content="The pipeline produced a summary.")],
        }

        assert agent_terminal_failure(result, model_responded=True) is None

    def test_a_model_answer_that_starts_with_the_marker_is_kept(self) -> None:
        """The marker arm must not claim a real model answer.

        A model asked to explain an error message answers with one. The gate is
        that the swallow happens INSTEAD of the model call, so a turn in which a
        model responded keeps its answer.
        """

        result = {
            "output": _NODE_ERROR,
            "execution_finished": True,
            "messages": [AIMessage(content=_NODE_ERROR)],
        }

        assert agent_terminal_failure(result, model_responded=True) is None

    @pytest.mark.parametrize(
        "pause",
        [
            {"hitl_interrupt": {"message": "Approve?", "interrupt_id": "i-1"}},
            {"paused": True, "pause_type": "mcp_auth", "error": "authorize"},
            {"parallel_parked": True, "execution_finished": False},
            {"execution_finished": False},
        ],
        ids=["hitl", "mcp-auth", "parked", "unfinished"],
    )
    def test_an_intentional_pause_is_not_a_failure(self, pause: dict) -> None:
        """Every pause reaches this worker with no assistant content.

        Without this arm the empty-content rule would turn each one into a
        failed turn, which is the opposite defect.
        """

        result = {"output": "", "messages": [], **pause}

        assert agent_terminal_failure(result, model_responded=False) is None

    def test_a_toolkit_result_in_a_named_variable_is_not_a_node_error(self) -> None:
        """The marker arm reads the messages channel, not state variables.

        A value-producing node writes to a named variable, so its text cannot
        be mistaken for the SDK's message-shaped swallow.
        """

        result = {
            "output": "Error: file not found",
            "execution_finished": True,
            "messages": [],
            "tool_output": "Error: file not found",
        }

        assert agent_terminal_failure(result, model_responded=False) is None


class TestCallbackGate:
    def test_the_callback_reports_a_completed_model_call(self) -> None:
        callback, _ = _callback()
        assert not callback.model_responded

        callback.on_chat_model_start({}, [], run_id="run-1", metadata={})
        callback.on_llm_end(
            LLMResult(generations=[[ChatGeneration(message=AIMessage(content="hi"))]]),
            run_id="run-1",
        )

        assert callback.model_responded
        assert callback.terminal_failure_stage(
            {
                "output": _NODE_ERROR,
                "execution_finished": True,
                "messages": [AIMessage(content=_NODE_ERROR)],
            }
        ) is None

    def test_a_turn_with_no_model_call_classifies_the_swallowed_error(self) -> None:
        callback, published = _callback()

        stage = callback.terminal_failure_stage(
            {
                "output": _NODE_ERROR,
                "execution_finished": True,
                "messages": [AIMessage(content=_NODE_ERROR)],
            }
        )

        assert stage == "node_error"
        # Classification publishes nothing. The caller raises on it, so no
        # assistant turn is written for a failed run.
        assert published == []


class TestTerminalFrame:
    def test_a_classified_failure_settles_failed_with_an_error_event(self) -> None:
        """The classified failure produces the malformed-YAML frame shape."""

        command = command_pb2.WorkerCommandV1(
            tenant_id="tenant-1",
            resource_project_id="1",
            projection_project_id="1",
            command_id="command-1",
            execution_id="execution-1",
            generation=1,
            agent_execution=agent_pb2.AgentExecutionCommandV1(),
        )
        verified = VerifiedWorkerCommand(
            envelope=envelope_pb2.WorkerExecutionEnvelopeV1(
                fence=common_pb2.ExecutionFenceV1(
                    workload_session_id="session-1",
                    producer_id="producer-1",
                    fence_token=b"0" * 32,
                    claim_attempt=1,
                    lease_epoch=1,
                )
            ),
            command=command,
        )

        frame = build_output_frame(
            verified,
            InternalFailure(),
            occurred_at_unix_millis=1,
        )

        assert (
            frame.event_type
            == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR
        )
        assert (
            frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )
        assert frame.runtime_error.safe_message == "The runtime operation failed."
        assert not frame.runtime_error.retryable
        assert not frame.HasField("agent_execution")


class TestCallSite:
    """Order gate for the arm the harness above cannot reach.

    Driving ``_execute_resolved`` needs the whole claim, control, output and
    spool harness AND the real SDK, because the adapter is built from it. What
    can break here without any assertion above noticing is ORDER: classify
    after ``emit_completed_response`` and the failed turn still publishes an
    assistant response event before it fails, which is the half of the defect
    the user sees.
    """

    def test_the_classification_runs_before_any_response_event(self) -> None:
        source = inspect.getsource(
            delivery.AgentExecutionDeliveryProcessor._execute_resolved
        )
        classify_at = source.find("terminal_failure_stage")
        completed_at = source.find("completed_response_content")
        emit_at = source.find("emit_completed_response")
        terminal_at = source.find("emit_terminal")

        assert classify_at != -1, "the agent execute arm no longer classifies the result"
        assert completed_at != -1 and emit_at != -1 and terminal_at != -1
        assert classify_at < completed_at < emit_at < terminal_at
        assert "raise _AgentTerminalFailure(failure_stage)" in source

    def test_the_terminal_failure_arm_maps_to_a_non_retryable_worker_error(
        self,
    ) -> None:
        source = inspect.getsource(
            delivery.AgentExecutionDeliveryProcessor._execute_resolved
        )
        tree = ast.parse(inspect.cleandoc(source))
        handlers = [
            handler
            for node in ast.walk(tree)
            if isinstance(node, ast.Try)
            for handler in node.handlers
            if isinstance(handler.type, ast.Name)
            and handler.type.id == "_AgentTerminalFailure"
        ]

        assert len(handlers) == 1, "the returned-failure arm is missing"
        raised = [
            node.exc.func.id
            for node in ast.walk(handlers[0])
            if isinstance(node, ast.Raise)
            and isinstance(node.exc, ast.Call)
            and isinstance(node.exc.func, ast.Name)
        ]
        assert raised == ["InternalFailure"]
