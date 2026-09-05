"""Current browser-event projection for SDK-owned agent execution.

This callback contains no Pylon transport or database code. It emits the same
``NodeEvent`` shapes used by the current UI; elitea-main persists
``partial_message`` deltas into the existing tenant ``chat_message_trace_step``
table and forwards the live events over SSE.
"""

from __future__ import annotations

import json
import re
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Callable

from elitea.runtime.v1 import node_event_pb2
from langchain_core.callbacks import BaseCallbackHandler

from elitea_worker.execution.errors import InvalidInput, ResourceExhausted
from elitea_worker.handlers.agent import AgentExecutionPayload
from elitea_worker.protocol.node_event import (
    MAX_CURRENT_NODE_EVENT_JSON_BYTES,
    InvalidCurrentNodeEvent,
    decode_current_node_event_json,
)


_HIERARCHY_KEYS = (
    "parent_agent_name",
    "parent_agent_call_id",
    "parent_agent_path",
    "sibling_ordinal",
    "child_thread_id",
    "thread_id",
    "checkpoint_ns",
    "langgraph_node",
)
_CUSTOM_EVENTS: dict[str, tuple[str, frozenset[str]]] = {
    "on_tool_node": ("agent_on_tool_node", frozenset({"state", "input_variables", "tool_result"})),
    "on_function_tool_node": ("agent_on_function_tool_node", frozenset({"state", "input_variables", "input_mapping", "tool_result"})),
    "on_loop_tool_node": ("agent_on_loop_tool_node", frozenset({"state", "input_variables", "tool_result"})),
    "on_loop_node": ("agent_on_loop_node", frozenset({"state", "input_variables", "accumulated_response"})),
    "on_conditional_edge": ("agent_on_conditional_edge", frozenset({"state", "condition"})),
    "on_decision_edge": ("agent_on_decision_edge", frozenset({"state", "decisional_inputs"})),
    "on_transitional_edge": ("agent_on_transitional_edge", frozenset({"state", "next_step"})),
    "thinking_step": ("agent_thinking_step", frozenset({"message", "tool_name", "toolkit"})),
    "thinking_step_update": ("agent_thinking_step_update", frozenset({"message", "tool_name", "toolkit", "markdown"})),
    "file_modified": ("agent_file_modified", frozenset({"message", "filepath", "tool_name", "toolkit", "operation_type", "meta", "media_type"})),
    "index_data_status": ("agent_index_data_status", frozenset({"id", "index_name", "state", "error", "reindex", "indexed", "updated", "created_at", "updated_on", "toolkit_id"})),
    "index_data_removed": ("agent_index_data_removed", frozenset({"index_name", "toolkit_id", "project_id"})),
    "mcp_authorization_required": ("mcp_authorization_required", frozenset({"server_url", "resource_metadata_url", "www_authenticate", "resource_metadata", "authorization_servers", "tool_run_id", "tool_name", "toolkit_name", "toolkit_type"})),
    "swarm_agent_start": ("agent_swarm_agent_start", frozenset({"agent_name", "is_parent", "message_count"})),
    "swarm_agent_response": ("agent_swarm_agent_response", frozenset({"agent_name", "is_parent", "content", "has_tool_calls", "tool_calls"})),
    "swarm_handoff": ("agent_swarm_handoff", frozenset({"from_agent", "to_agent"})),
}
_MAX_TRACE_TEXT_BYTES = 128 * 1024
_MAX_AUTHORIZATION_REQUESTS = 16
# The exact literal the pinned SDK writes when it converts a node exception
# into message content (elitea_sdk/runtime/tools/llm.py, LLMNode.invoke).
_NODE_ERROR_PREFIX = "Error: "
_LOADED_SKILL_PREFIX_RE = re.compile(r'^Skill "([^"]+)" is now active')
_LOAD_SKILL_ALREADY_ACTIVE_RE = re.compile(
    r'^Skill "([^"]+)" is already (?:loaded|active)'
)
_CUSTOM_STATE_TRANSCRIPT_FIELDS = ("messages", "chat_history")
_AUTHORIZATION_PRIVATE_KEYS = frozenset(
    {
        "access_token",
        "authorization",
        "client_secret",
        "mcp_client_secret",
        "mcp_tokens",
        "provided_settings",
        "refresh_token",
    }
)


def _normalize_hitl_pause(
    result: dict[str, Any],
    *,
    execution_id: str,
) -> tuple[dict[str, Any] | None, list[dict[str, Any]]]:
    """Preserve the current singular/plural HITL result contract.

    Sensitive-tool interrupts already carry the SDK-owned interrupt identity.
    A pipeline ``HITLNode`` exposes the same pause contract without an
    ``interrupt_id``.  One worker execution can settle at only one sequential
    terminal pause, so its durable execution identity is the stable fallback:
    command redelivery keeps the same card identity while a later continuation
    execution receives a new one.
    """

    raw_singular = result.get("hitl_interrupt")
    if raw_singular is not None and not isinstance(raw_singular, dict):
        raise InvalidInput("The agent HITL interrupt is malformed.")
    singular = dict(raw_singular) if raw_singular is not None else None
    raw_plural = result.get("hitl_interrupts")
    if raw_plural is None:
        plural: list[dict[str, Any]] = []
    elif isinstance(raw_plural, list) and all(
        isinstance(item, dict) for item in raw_plural
    ):
        plural = [dict(item) for item in raw_plural]
    else:
        raise InvalidInput("The agent HITL interrupt list is malformed.")
    if not plural and singular is not None:
        plural = [singular]
    if singular is None and plural:
        singular = dict(plural[0])

    if singular is not None and len(plural) == 1:
        interrupt_id = singular.get("interrupt_id") or plural[0].get(
            "interrupt_id"
        )
        if not isinstance(interrupt_id, str) or not interrupt_id:
            if not execution_id:
                raise InvalidInput(
                    "The agent HITL interrupt identity is unavailable."
                )
            interrupt_id = execution_id
        singular["interrupt_id"] = interrupt_id
        plural[0]["interrupt_id"] = interrupt_id
    return singular, plural


def agent_terminal_failure(
    result: Any,
    *,
    model_responded: bool,
) -> str | None:
    """Classify a terminal SDK result that is not a completed turn.

    The SDK does not always raise when a graph node fails. Two shapes reach
    this worker as an ordinary returned object:

    * The flattened failure envelope, ``{"success": False, "error": ...}``.
      ``_sdk_failure_category`` in the delivery module already trusts this
      shape on the index path.
    * The node-error-to-message conversion in the pinned SDK's
      ``elitea_sdk/runtime/tools/llm.py``. ``LLMNode.invoke`` catches every
      non-budget exception from ``_invoke_llm_internal`` and replaces the whole
      node result with one ``AIMessage`` whose content is ``f"Error: {e}"``.
      LangGraph therefore records no ``__error__`` write, the graph reaches
      ``END``, and the worker receives a normal result.

    ``AgentExecutionResultV1`` has no failed terminal state, and
    ``build_output_frame`` maps every one of them to
    ``EXECUTION_OUTCOME_V1_SUCCEEDED``. A failure must therefore leave this
    worker as a ``RuntimeErrorV1`` frame instead, which is what the caller does
    with the value returned here.

    ``model_responded`` states whether any model call completed in this turn.
    It gates the message-marker arm so that a model that answers with the word
    ``Error:`` keeps its answer. The gate makes the arm conservative: a node
    that fails AFTER a successful model call in the same turn is not reported.

    Returns a short stage name for the worker log, or None for a completed turn
    and for every intentional pause.
    """

    if not isinstance(result, dict):
        return "malformed_result"
    # Intentional pauses own their own terminal events. HITL, delegated
    # toolkit authorization and a parked parallel fan-out are all incomplete
    # ON PURPOSE, and none of them is a failure.
    if result.get("hitl_interrupt") is not None or result.get("hitl_interrupts"):
        return None
    if result.get("paused") is True or result.get("parallel_parked") is True:
        return None
    if result.get("execution_finished") is False:
        return None
    if result.get("success") is False:
        return "sdk_reported_failure"
    content = _extract_response_content(result).strip()
    if not content:
        # A finished turn with no assistant content cannot be shown to anybody.
        # The SDK's own "output is None" sentinel is non-empty, so an empty
        # result means the extraction chain found nothing at all.
        return "empty_terminal_output"
    if not model_responded and _is_node_error_message(result, content):
        return "node_error"
    return None


def _is_node_error_message(result: dict[str, Any], content: str) -> bool:
    """Match the SDK's swallowed node error, and only that.

    Require both halves: the terminal content is the whole marker string, and
    the last entry of the ``messages`` channel is the assistant message that
    carries it. A node that writes its result to a named state variable never
    reaches the ``messages`` channel, so it cannot match here.
    """

    if not content.startswith(_NODE_ERROR_PREFIX):
        return False
    messages = result.get("messages")
    if not isinstance(messages, list) or not messages:
        return False
    last = messages[-1]
    if isinstance(last, dict):
        role = last.get("type") or last.get("role")
        raw = last.get("content")
    else:
        role = getattr(last, "type", None)
        raw = getattr(last, "content", None)
    if role is not None and str(role).lower() in {"human", "user", "system"}:
        return False
    return isinstance(raw, str) and raw.strip() == content


@dataclass(frozen=True, slots=True)
class CurrentAgentNodeEventContext:
    execution_id: str
    stream_id: str
    message_id: str
    execution_generation: str
    sio_event: str
    thread_id: str
    project_id: int | str
    chat_project_id: int | str


class CurrentAgentNodeEventCallback(BaseCallbackHandler):
    """Project LangChain callbacks to bounded current UI and trace deltas."""

    def __init__(
        self,
        context: CurrentAgentNodeEventContext,
        publish: Callable[[node_event_pb2.NodeEventV1], None],
    ) -> None:
        super().__init__()
        self.raise_error = True
        self._context = context
        self._publish = publish
        self._failure: Exception | None = None
        self._lock = threading.Lock()
        self._tools: dict[str, dict[str, Any]] = {}
        self._llm: dict[str, dict[str, Any]] = {}
        self._last_content: dict[str, str] = {}
        self._last_thinking: dict[str, str] = {}
        self._authorization_requests: dict[str, dict[str, Any]] = {}
        self._authorization_pause_message: str | None = None
        self._applied_skills: list[dict[str, Any]] = []
        self._skills_by_name: dict[str, dict[str, Any]] = {}
        self._nested_skills_by_parent: dict[
            tuple[str, str], dict[str, Any] | None
        ] = {}
        self._nested_skills_by_name: dict[str, dict[str, Any] | None] = {}
        self._tool_skill_identities: dict[str, dict[str, Any]] = {}
        # Counts completed model calls in this turn. `agent_terminal_failure`
        # reads it to keep a model answer that starts with "Error: ".
        self._model_responses = 0

    @property
    def model_responded(self) -> bool:
        """Report whether any model call completed in this turn."""

        with self._lock:
            return self._model_responses > 0

    def terminal_failure_stage(self, result: Any) -> str | None:
        """Classify the SDK result this callback observed being produced."""

        return agent_terminal_failure(result, model_responded=self.model_responded)

    def configure_skills(
        self,
        *,
        applied_skills: list[Any] | None,
        attached_skills: list[Any] | None,
        nested_skill_registry: list[Any] | None = None,
    ) -> None:
        """Install the current turn-scoped skill projection.

        The callback owns only compact UI/persistence metadata. Instruction
        bodies continue to cross exclusively through the SDK configurable
        channel and are never copied into partial or terminal browser events.
        """

        applied: list[dict[str, Any]] = []
        seen: set[str] = set()
        for raw in applied_skills or []:
            if not isinstance(raw, dict):
                continue
            name = raw.get("name")
            if not isinstance(name, str) or not name.strip():
                continue
            key = name.strip().lower()
            if key in seen:
                continue
            seen.add(key)
            applied.append(_compact_skill(raw, name=name))
        registry: dict[str, dict[str, Any]] = {}
        for raw in attached_skills or []:
            if not isinstance(raw, dict):
                continue
            name = raw.get("name")
            if not isinstance(name, str) or not name.strip():
                continue
            registry.setdefault(name.strip().lower(), dict(raw))
        nested_by_parent: dict[tuple[str, str], dict[str, Any] | None] = {}
        nested_by_name: dict[str, dict[str, Any] | None] = {}
        for raw_registry in nested_skill_registry or []:
            if not isinstance(raw_registry, dict):
                continue
            parent = raw_registry.get("application_name")
            skills = raw_registry.get("skills")
            if not isinstance(parent, str) or not parent.strip() or not isinstance(skills, list):
                continue
            parent_key = parent.strip().lower()
            for raw in skills:
                identity = _valid_compact_skill(raw)
                if identity is None:
                    continue
                skill_key = identity["name"].strip().lower()
                _register_unique_skill(
                    nested_by_parent,
                    (parent_key, skill_key),
                    identity,
                )
                _register_unique_skill(nested_by_name, skill_key, identity)
        with self._lock:
            self._applied_skills = applied
            self._skills_by_name = registry
            self._nested_skills_by_parent = nested_by_parent
            self._nested_skills_by_name = nested_by_name
            self._tool_skill_identities = {}

    def emit_agent_start(self, *, invoked_skills: list[Any] | None = None) -> None:
        self._emit("agent_start", response_metadata={"invoked_skills": invoked_skills or []})

    def authorization_pause_result(self) -> dict[str, Any] | None:
        """Return the current SDK-compatible pause marker after a tool callback.

        The SDK reports delegated toolkit authorization as a typed tool error,
        not as the graph result.  The current indexer converts that callback
        into an intentional pause after ``invoke`` returns; keep the same
        ownership here so the SDK remains unchanged.
        """

        with self._lock:
            if not self._authorization_requests:
                return None
            message = self._authorization_pause_message
        return {
            "thread_id": self._context.thread_id,
            "error": message or "Toolkit authorization is required.",
            "paused": True,
            "pause_type": "mcp_auth",
        }

    def capture_materialization_authorization(self, error: BaseException) -> bool:
        """Project an SDK authorization raised before a tool callback exists.

        Saved or nested MCP toolkits can discover delegated authorization while
        the SDK is constructing the agent graph. LangChain therefore has no
        tool invocation on which to deliver ``on_tool_error``. Preserve the
        established authorization terminal contract with a deterministic
        execution-scoped invocation identity.
        """

        if not _is_mcp_authorization_required(error):
            return False
        supplied_run_id = getattr(error, "tool_run_id", None)
        if (
            not isinstance(supplied_run_id, str)
            or not supplied_run_id
            or len(supplied_run_id.encode("utf-8")) > 512
            or any(character in supplied_run_id for character in "\x00\r\n")
        ):
            supplied_run_id = (
                f"mcp-auth-materialization:{self._context.execution_id}"
            )
        metadata = {
            key: getattr(error, key)
            for key in _HIERARCHY_KEYS
            if getattr(error, key, None) is not None
        }
        callback_name = (
            getattr(error, "tool_name", None)
            or getattr(error, "toolkit_name", None)
            or "mcp_authorization"
        )
        self._guard(
            self._pause_for_authorization,
            supplied_run_id,
            error,
            metadata,
            callback_name,
        )
        return True

    def emit_terminal(
        self,
        result: dict[str, Any],
        payload: AgentExecutionPayload,
        *,
        response_emitted: bool = False,
    ) -> node_event_pb2.NodeEventV1:
        """Emit exactly one current terminal outcome for a completed or HITL run."""

        hitl_interrupt, hitl_interrupts = _normalize_hitl_pause(
            result,
            execution_id=self._context.execution_id,
        )
        if hitl_interrupt is not None:
            thread_id = result.get("thread_id")
            if not isinstance(thread_id, str) or not thread_id:
                thread_id = self._context.thread_id
            message = hitl_interrupt.get("message")
            if not isinstance(message, str) or not message:
                message = "Awaiting human review..."
            return self._emit(
                "agent_hitl_interrupt",
                content=message,
                response_metadata={
                    "thread_id": thread_id,
                    "chat_project_id": self._context.chat_project_id,
                    "message": message,
                    "hitl_interrupt": _json_value(hitl_interrupt),
                    "hitl_interrupts": _json_value(hitl_interrupts),
                    "node_name": _json_value(hitl_interrupt.get("node_name")),
                    "available_actions": _json_value(
                        hitl_interrupt.get("available_actions", [])
                    ),
                    "routes": _json_value(hitl_interrupt.get("routes", {})),
                    "edit_state_key": _json_value(
                        hitl_interrupt.get("edit_state_key")
                    ),
                },
            )

        if result.get("paused") is True and result.get("pause_type") == "mcp_auth":
            with self._lock:
                authorization_requests = [
                    dict(request)
                    for request in self._authorization_requests.values()
                ]
            if not authorization_requests:
                raise InvalidInput(
                    "The delegated authorization request identity is unavailable."
                )
            primary = authorization_requests[-1]
            thread_id = result.get("thread_id")
            if not isinstance(thread_id, str) or not thread_id:
                thread_id = self._context.thread_id
            message = result.get("error")
            if not isinstance(message, str) or not message:
                message = "Toolkit authorization is required."
            return self._emit(
                "mcp_authorization_required",
                content=message,
                response_metadata={
                    **primary,
                    "thread_id": thread_id,
                    "chat_project_id": self._context.chat_project_id,
                    "authorization_requests": authorization_requests,
                },
            )

        if result.get("paused") is True:
            raise InvalidInput("The paused agent result has no supported interrupt.")

        content = _extract_response_content(result)
        thread_id = result.get("thread_id")
        if not isinstance(thread_id, str) or not thread_id:
            thread_id = self._context.thread_id
        if not response_emitted:
            self.emit_completed_response(result)
        return self._emit(
            "full_message",
            content=content,
            response_metadata={
                "project_id": self._context.project_id,
                "chat_project_id": self._context.chat_project_id,
                "application_details": _public_application_details(
                    payload.application
                ),
                "thread_id": thread_id,
                "llm_start_timestamp": None,
                "additional_response_meta": {},
                "files_modified": [],
                "image_thumbnails": {},
                "index_statuses": {},
                "chat_history_tokens_input": 0,
                "llm_response_tokens_output": 0,
                "should_continue": payload.should_continue,
                "hitl_resume": payload.hitl_resume,
                "parallel_reconcile": bool(payload.parallel_reconcile),
                "context_info": _json_value(result.get("context_info")),
                "invoked_skills": self._applied_skills_snapshot(),
            },
        )

    def emit_completed_response(
        self,
        result: dict[str, Any],
    ) -> node_event_pb2.NodeEventV1:
        """Publish completed content before optional post-turn enrichment."""

        content = self.completed_response_content(result)
        if content is None:
            raise InvalidInput("The agent execution did not complete.")
        thread_id = result.get("thread_id")
        if not isinstance(thread_id, str) or not thread_id:
            thread_id = self._context.thread_id
        if result.get("execution_finished") is True:
            self._emit(
                "pipeline_finish",
                content=content,
                response_metadata={
                    "finish_reason": "finished",
                    "next_step": "END",
                    "thread_id": thread_id,
                },
            )
        return self._emit(
            "agent_response",
            content=content,
            response_metadata={"finish_reason": "stop", "thread_id": thread_id},
        )

    def emit_completion(
        self,
        result: dict[str, Any],
        payload: AgentExecutionPayload,
    ) -> node_event_pb2.NodeEventV1:
        """Compatibility alias for callers that still expect a completed result."""

        terminal = self.emit_terminal(result, payload)
        if terminal.type != "full_message":
            raise InvalidInput("The agent execution did not complete.")
        return terminal

    def completed_response_content(self, result: dict[str, Any]) -> str | None:
        """Return completed output, excluding every intentional pause outcome."""

        hitl_interrupt, _ = _normalize_hitl_pause(
            result,
            execution_id=self._context.execution_id,
        )
        if hitl_interrupt is not None or result.get("paused") is True:
            return None
        return _extract_response_content(result)

    def emit_next_input_suggestion(
        self,
        suggestion: str,
    ) -> node_event_pb2.NodeEventV1 | None:
        """Emit one ephemeral suggestion before the durable terminal event."""

        text = _bounded_text(suggestion, "")
        if not text:
            return None
        return self._emit(
            "next_input_suggestion_ready",
            response_metadata={"suggestion": text},
        )

    def on_tool_start(
        self,
        serialized: dict[str, Any],
        input_str: str,
        *,
        run_id: Any,
        metadata: dict[str, Any] | None = None,
        inputs: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        _ = input_str, kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._tool_start, serialized, run_id, metadata, inputs)

    def _tool_start(self, serialized, run_id, metadata, inputs) -> None:
        selected = _run_id(run_id)
        now = _now()
        tool_name = _bounded_text(serialized.get("name"), "tool")
        hierarchy = _hierarchy(metadata)
        tool_meta = {
            "name": tool_name,
            "metadata": {
                **_tool_display_metadata(serialized, metadata),
                **hierarchy,
            },
        }
        entry = {
            "tool_name": tool_name,
            "tool_run_id": selected,
            "run_id": selected,
            "tool_meta": tool_meta,
            "tool_inputs": _json_value(inputs),
            "metadata": {**_tool_display_metadata(serialized, metadata), **hierarchy},
            "timestamp_start": now,
            "timestamp_finish": None,
            "finish_reason": None,
            "tool_output": None,
            "error": None,
        }
        if tool_name == "load_skill":
            requested = inputs.get("skill") if isinstance(inputs, dict) else None
            if isinstance(requested, str) and requested.strip():
                with self._lock:
                    registered = self._resolve_skill_identity_locked(
                        requested,
                        hierarchy,
                    )
                    if registered is not None:
                        self._tool_skill_identities[selected] = dict(registered)
                registered = registered or {}
                loaded_name = registered.get("name") or requested.strip()
                tool_meta["loaded_skill"] = loaded_name
                icon_meta = registered.get("icon_meta")
                if icon_meta:
                    entry["metadata"]["icon_meta"] = _json_value(icon_meta)
                    tool_meta["icon_meta"] = _json_value(icon_meta)
        with self._lock:
            self._tools[selected] = entry
        self._emit("agent_tool_start", response_metadata=entry)
        self._emit_partial(tool_calls={selected: entry})

    def on_tool_end(self, output: Any, *, run_id: Any, **kwargs: Any) -> None:
        _ = kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._tool_finish, run_id, output, False)

    def on_tool_error(self, error: BaseException, *, run_id: Any, **kwargs: Any) -> None:
        if self.authorization_pause_result() is not None:
            return
        if _is_mcp_authorization_required(error):
            self._guard(
                self._pause_for_authorization,
                run_id,
                error,
                kwargs.get("metadata"),
                kwargs.get("name"),
            )
            return
        self._guard(self._tool_finish, run_id, error, True)

    def _pause_for_authorization(
        self,
        run_id: Any,
        error: BaseException,
        metadata: Any,
        callback_name: Any,
    ) -> None:
        selected = _run_id(run_id)
        message = _bounded_text(
            error.args[0] if error.args else str(error),
            "Toolkit authorization is required.",
        )
        with self._lock:
            previous = self._tools.get(selected)
            entry = dict(previous) if previous is not None else {
                "tool_name": _bounded_text(callback_name, "tool"),
                "tool_run_id": selected,
                "run_id": selected,
                "tool_meta": {},
                "tool_inputs": None,
                "metadata": {},
                "timestamp_start": _now(),
            }
            entry["timestamp_finish"] = _now()
            entry["finish_reason"] = "action_required"
            entry["tool_output"] = None
            entry["error"] = message
            self._tools[selected] = entry

        display = _tool_display_metadata(
            entry.get("tool_meta") if isinstance(entry.get("tool_meta"), dict) else {},
            metadata,
        )
        stored_metadata = entry.get("metadata")
        if isinstance(stored_metadata, dict):
            display = {**stored_metadata, **display}
        payload = {
            key: _public_authorization_value(getattr(error, key))
            for key in (
                "server_url",
                "resource_metadata_url",
                "www_authenticate",
                "resource_metadata",
                "authorization_servers",
            )
            if getattr(error, key, None) is not None
        }
        payload.update(
            {
                "tool_run_id": selected,
                "tool_name": _bounded_text(
                    getattr(error, "tool_name", None) or entry.get("tool_name"),
                    "tool",
                ),
            }
        )
        for key in ("toolkit_name", "toolkit_type"):
            value = getattr(error, key, None) or display.get(key)
            if value is not None:
                payload[key] = _json_value(value)
        # LangChain's tool-error callback does not consistently repeat the
        # hierarchy supplied to tool-start.  The start event is the
        # invocation-authoritative source, so retain its child ownership and
        # let any error-event metadata override only the keys it actually
        # carries.  Losing this data makes a nested authorization pause look
        # root-owned to Go and the UI.
        hierarchy = _hierarchy(stored_metadata)
        tool_meta = entry.get("tool_meta")
        if isinstance(tool_meta, dict):
            hierarchy.update(_hierarchy(tool_meta.get("metadata")))
        hierarchy.update(_hierarchy(metadata))
        payload.update(hierarchy)
        self._record_authorization_request(payload)
        with self._lock:
            self._authorization_pause_message = message
        self._emit(
            "mcp_authorization_required",
            content=message,
            response_metadata=payload,
        )

    def _tool_finish(self, run_id: Any, value: Any, failed: bool) -> None:
        selected = _run_id(run_id)
        with self._lock:
            previous = self._tools.get(selected)
            if previous is None:
                return
            entry = dict(previous)
            entry["timestamp_finish"] = _now()
            entry["finish_reason"] = "error" if failed else "stop"
            public_output = None if failed else _trace_text(value)
            if not failed and entry.get("tool_name") == "load_skill":
                self._record_loaded_skill_locked(
                    public_output or "",
                    self._tool_skill_identities.pop(selected, None),
                )
                public_output = _public_load_skill_output(public_output)
            else:
                self._tool_skill_identities.pop(selected, None)
            entry["tool_output"] = public_output
            entry["error"] = _trace_text(value) if failed else None
            self._tools[selected] = entry
        self._emit(
            "agent_tool_error" if failed else "agent_tool_end",
            # Successful tool output already has one established owner in
            # response_metadata.tool_output. Duplicating the same potentially
            # large value into content made a 51 KiB current Aha result exceed
            # the 64 KiB data-plane frame even though the control plane held
            # only references. Errors retain content for the existing error UI.
            content=entry["error"] if failed else None,
            response_metadata=entry,
        )
        self._emit_partial(tool_calls={selected: entry})

    def on_llm_start(
        self,
        serialized: dict[str, Any],
        prompts: list[str],
        *,
        run_id: Any,
        metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        _ = prompts, kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._llm_start, serialized, run_id, metadata)

    def on_chat_model_start(self, serialized, messages, *, run_id, metadata=None, **kwargs):
        _ = messages, kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._llm_start, serialized, run_id, metadata)

    def _llm_start(self, serialized, run_id, metadata) -> None:
        selected = _run_id(run_id)
        now = _now()
        model = _model_name(serialized, metadata)
        hierarchy = _hierarchy(metadata)
        state = {"timestamp_start": now, "model_name": model, **hierarchy}
        with self._lock:
            self._llm[selected] = state
        self._emit(
            "agent_llm_start",
            response_metadata={
                "tool_name": hierarchy.get("langgraph_node") or "Thinking step",
                "tool_run_id": selected,
                "metadata": {"ls_model_name": model, **hierarchy},
                **state,
            },
        )

    def on_llm_new_token(self, token: str, *, run_id: Any, chunk: Any = None, **kwargs: Any) -> None:
        _ = kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._llm_chunk, token, run_id, chunk)

    def _llm_chunk(self, token: str, run_id: Any, chunk: Any) -> None:
        selected = _run_id(run_id)
        content, thinking = _chunk_values(token, chunk)
        content = _delta(content, self._last_content.get(selected, ""))
        thinking = _delta(thinking, self._last_thinking.get(selected, ""))
        if content:
            self._last_content[selected] = self._last_content.get(selected, "") + content
        if thinking:
            self._last_thinking[selected] = self._last_thinking.get(selected, "") + thinking
        if not content and not thinking:
            return
        self._emit(
            "agent_llm_chunk",
            content=content or None,
            thinking=thinking or None,
            response_metadata={"tool_run_id": selected},
        )

    def on_llm_end(self, response: Any, *, run_id: Any, **kwargs: Any) -> None:
        _ = kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._llm_end, response, run_id)

    def _llm_end(self, response: Any, run_id: Any) -> None:
        selected = _run_id(run_id)
        with self._lock:
            state = dict(self._llm.pop(selected, {}))
            self._model_responses += 1
        step = _thinking_step(response, selected, state)
        self._emit(
            "agent_llm_end",
            response_metadata={"tool_run_id": selected, "thinking_steps": [step]},
        )
        self._emit_partial(thinking_steps=[step])

    def on_custom_event(
        self,
        name: str,
        data: Any,
        *,
        run_id: Any,
        metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        _ = kwargs
        if self.authorization_pause_result() is not None:
            return
        self._guard(self._custom_event, name, data, run_id, metadata)

    def _custom_event(self, name, data, run_id, metadata) -> None:
        selected = _CUSTOM_EVENTS.get(name)
        if selected is None:
            return
        if not isinstance(data, dict):
            raise InvalidInput("The agent custom event is malformed.")
        event_type, fields = selected
        payload = {
            "name": name,
            "run_id": _run_id(run_id),
            "tool_run_id": _run_id(run_id),
            "metadata": _hierarchy(metadata),
            "datetime": _now(),
            **{key: _json_value(data[key]) for key in sorted(fields) if key in data},
        }
        if event_type == "agent_swarm_agent_response":
            payload["chat_project_id"] = self._context.chat_project_id
        payload = self._bounded_custom_event_payload(event_type, payload)
        if event_type == "mcp_authorization_required":
            self._record_authorization_request(payload)
        self._emit(event_type, response_metadata=payload)

    def _record_authorization_request(self, payload: dict[str, Any]) -> None:
        request_id = payload.get("tool_run_id")
        if not isinstance(request_id, str) or not request_id:
            raise InvalidInput(
                "The delegated authorization request identity is unavailable."
            )
        with self._lock:
            if (
                request_id not in self._authorization_requests
                and len(self._authorization_requests) >= _MAX_AUTHORIZATION_REQUESTS
            ):
                raise ResourceExhausted(
                    "The agent produced too many delegated authorization requests."
                )
            self._authorization_requests[request_id] = dict(payload)

    def _bounded_custom_event_payload(
        self,
        event_type: str,
        payload: dict[str, Any],
    ) -> dict[str, Any]:
        """Drop only redundant transcript copies from an oversized SDK event.

        The current SDK includes its accumulated ``messages`` state in graph
        transition events. Tool output already has an authoritative owner in
        ``agent_tool_end``/``partial_message``; copying the entire transcript
        again makes later turns grow without bound and defers a callback
        failure until otherwise-successful agent finalization. Preserve the
        remaining pipeline state byte-for-byte and mark the omitted duplicate
        fields so consumers can distinguish a bounded projection.
        """

        state = payload.get("state")
        if not isinstance(state, dict):
            return payload
        projected_state = dict(state)
        omitted = [
            field
            for field in _CUSTOM_STATE_TRANSCRIPT_FIELDS
            if field in projected_state
        ]
        if not omitted:
            return payload
        for field in omitted:
            projected_state.pop(field, None)
        projected = dict(payload)
        projected["state"] = projected_state
        projected["state_projection"] = {
            "omitted_duplicate_fields": omitted,
        }
        return projected

    def _emit_partial(
        self,
        *,
        tool_calls: dict[str, Any] | None = None,
        thinking_steps: list[Any] | None = None,
    ) -> None:
        self._emit(
            "partial_message",
            response_metadata={
                "project_id": self._context.project_id,
                "chat_project_id": self._context.chat_project_id,
                "thread_id": self._context.thread_id,
                "thinking_steps": thinking_steps or [],
                "tool_calls": tool_calls or {},
                "additional_response_meta": {},
                "invoked_skills": self._applied_skills_snapshot(),
            },
        )

    def _record_loaded_skill_locked(
        self,
        output: str,
        registered: dict[str, Any] | None,
    ) -> None:
        match = _LOADED_SKILL_PREFIX_RE.match(output) or (
            _LOAD_SKILL_ALREADY_ACTIVE_RE.match(output)
        )
        if match is None:
            return
        name = match.group(1)
        key = name.strip().lower()
        if any(
            (skill.get("name") or "").strip().lower() == key
            for skill in self._applied_skills
        ):
            return
        if registered is None:
            registered = self._skills_by_name.get(key)
        identity = _valid_compact_skill(registered)
        if identity is None or identity["name"].strip().lower() != key:
            raise InvalidInput("The loaded skill identity is unavailable.")
        self._applied_skills.append(_compact_skill(identity, name=identity["name"]))

    def _resolve_skill_identity_locked(
        self,
        requested: str,
        hierarchy: dict[str, Any],
    ) -> dict[str, Any] | None:
        skill_key = requested.strip().lower()
        parent = _immediate_parent_name(hierarchy)
        if parent is None:
            registered = self._skills_by_name.get(skill_key)
            return dict(registered) if registered is not None else None
        registered = self._nested_skills_by_parent.get((parent, skill_key))
        if registered is None:
            registered = self._nested_skills_by_name.get(skill_key)
        return dict(registered) if registered is not None else None

    def _applied_skills_snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(skill) for skill in self._applied_skills]

    def _emit(
        self,
        event_type: str,
        *,
        content: Any = None,
        thinking: str | None = None,
        response_metadata: dict[str, Any] | None = None,
    ) -> node_event_pb2.NodeEventV1:
        raw = self._event_json(
            event_type,
            content=content,
            thinking=thinking,
            response_metadata=response_metadata,
        )
        if len(raw) > MAX_CURRENT_NODE_EVENT_JSON_BYTES:
            raise ResourceExhausted("The agent event exceeds its output limit.")
        try:
            event = decode_current_node_event_json(raw)
        except InvalidCurrentNodeEvent as exc:
            raise InvalidInput("The agent event is malformed.") from exc
        self._publish(event)
        return event

    def _event_json(
        self,
        event_type: str,
        *,
        content: Any = None,
        thinking: str | None = None,
        response_metadata: dict[str, Any] | None = None,
    ) -> bytes:
        return json.dumps(
            {
                "type": event_type,
                "stream_id": self._context.stream_id,
                "message_id": self._context.message_id,
                "content": content,
                "thinking": thinking,
                "response_metadata": response_metadata or {},
                "references": [],
                "sio_event": self._context.sio_event,
                "created_at": _now(),
                "execution_generation": self._context.execution_generation,
            },
            ensure_ascii=False,
            allow_nan=False,
            separators=(",", ":"),
        ).encode("utf-8")

    def _guard(self, function, *args) -> None:
        try:
            function(*args)
        except Exception as exc:
            with self._lock:
                if self._failure is None:
                    self._failure = exc
            raise

    def raise_if_failed(self) -> None:
        with self._lock:
            failure = self._failure
        if failure is not None:
            raise failure


def nested_skill_registry_from_payload(
    payload: AgentExecutionPayload,
) -> list[Any]:
    """Collect admission-frozen compact child skill registries.

    Main attaches each registry to the application reference whose subtree it
    describes. The SDK still fetches child instruction bodies itself; only the
    compact identity required by UI and persistence projection crosses here.
    """

    groups: list[Any] = [payload.tools]
    version_details = payload.application.get("version_details")
    if isinstance(version_details, dict):
        groups.append(version_details.get("tools"))
    registries: list[Any] = []
    for group in groups:
        if not isinstance(group, list):
            continue
        for tool in group:
            if not isinstance(tool, dict) or tool.get("type") != "application":
                continue
            registry = tool.get("nested_skill_registry")
            if isinstance(registry, list):
                registries.extend(registry)
    return registries


def _thinking_step(response: Any, run_id: str, state: dict[str, Any]) -> dict[str, Any]:
    generation = None
    generations = getattr(response, "generations", None)
    if isinstance(generations, list) and generations and isinstance(generations[0], list) and generations[0]:
        generation = generations[0][-1]
    dumped = generation.model_dump() if generation is not None and callable(getattr(generation, "model_dump", None)) else {}
    message = dumped.get("message") if isinstance(dumped.get("message"), dict) else {}
    text, thinking = _message_values(message, dumped.get("text"))
    if not text:
        # Current agent callbacks keep tool-call-only model turns visible as an
        # LLM activity chip. Preserve that outcome without duplicating tool
        # arguments into the thinking-step/output stream; agent_tool_start owns
        # the typed inputs and their independent size/security boundary.
        text = _tool_call_decision_text(message)
    hierarchy = {key: state[key] for key in _HIERARCHY_KEYS if key in state}
    response_metadata = {
        "model_name": state.get("model_name"),
        "tool_name": hierarchy.get("langgraph_node"),
        "metadata": hierarchy,
    }
    return {
        "tool_run_id": run_id,
        "type": dumped.get("type") or "ChatGeneration",
        "text": _trace_text(text),
        "thinking": _trace_text(thinking),
        "timestamp_start": state.get("timestamp_start"),
        "timestamp_finish": _now(),
        "message": {"response_metadata": response_metadata},
        **hierarchy,
    }


def _extract_response_content(response: dict[str, Any]) -> str:
    """Preserve the current worker's standardized-output then messages fallback."""

    content = response.get("output", "")
    if not content:
        messages = response.get("messages")
        if isinstance(messages, list) and messages:
            last = messages[-1]
            if isinstance(last, dict):
                content = last.get("content", "")
            else:
                content = getattr(last, "content", str(last))
    return _normalize_response_content(content)


def _normalize_response_content(content: Any) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        stripped = content.strip()
        if stripped.startswith("[") and "tool_use" in stripped:
            try:
                parsed = json.loads(stripped)
            except json.JSONDecodeError:
                return content
            if isinstance(parsed, list):
                return _normalize_response_content(parsed)
        return content
    if isinstance(content, list):
        text_parts: list[str] = []
        for block in content:
            if isinstance(block, dict):
                if block.get("type") in {"text", "output_text"}:
                    text = block.get("text")
                    if isinstance(text, str) and text:
                        text_parts.append(text)
                elif "text" in block and "type" not in block:
                    text = block.get("text")
                    if isinstance(text, str) and text:
                        text_parts.append(text)
                elif block.get("type") in {
                    "tool_use",
                    "tool_result",
                    "thinking",
                    "reasoning",
                }:
                    continue
            elif isinstance(block, str):
                if block:
                    text_parts.append(block)
        return "".join(text_parts)
    return json.dumps(content, ensure_ascii=False)


def _message_values(message: dict[str, Any], fallback: Any = None) -> tuple[str, str]:
    text_parts: list[str] = []
    thinking_parts: list[str] = []
    content = message.get("content", fallback)
    if isinstance(content, str):
        text_parts.append(content)
    elif isinstance(content, list):
        for item in content:
            if not isinstance(item, dict):
                continue
            if item.get("type") == "text" and isinstance(item.get("text"), str):
                text_parts.append(item["text"])
            if item.get("type") in {"thinking", "reasoning"}:
                value = item.get("thinking") or item.get("reasoning")
                if isinstance(value, str):
                    thinking_parts.append(value)
    additional = message.get("additional_kwargs")
    if isinstance(additional, dict) and isinstance(additional.get("thinking"), str):
        thinking_parts.append(additional["thinking"])
    return "\n".join(text_parts), "\n".join(thinking_parts)


def _tool_call_decision_text(message: dict[str, Any]) -> str:
    calls: Any = None
    additional = message.get("additional_kwargs")
    if isinstance(additional, dict):
        calls = additional.get("tool_calls")
    if not isinstance(calls, list):
        calls = message.get("tool_calls")
    if not isinstance(calls, list):
        return ""

    decisions: list[str] = []
    for call in calls:
        if not isinstance(call, dict):
            continue
        function = call.get("function")
        name = function.get("name") if isinstance(function, dict) else call.get("name")
        if isinstance(name, str) and name:
            decisions.append(f"Planned to call tool '{_bounded_text(name, 'tool')}'")
    return "\n".join(decisions)


def _chunk_values(token: Any, chunk: Any) -> tuple[str, str]:
    content = token if isinstance(token, str) else ""
    thinking = ""
    if chunk is not None:
        text = getattr(chunk, "text", None)
        if isinstance(text, str) and text:
            content = text
        message = getattr(chunk, "message", None)
        additional = getattr(message, "additional_kwargs", None)
        if isinstance(additional, dict) and isinstance(additional.get("thinking"), str):
            thinking = additional["thinking"]
    return content, thinking


def _delta(value: str, previous: str) -> str:
    if not value:
        return ""
    return value[len(previous) :] if previous and value.startswith(previous) else value


def _hierarchy(metadata: Any) -> dict[str, Any]:
    if not isinstance(metadata, dict):
        return {}
    return {key: _json_value(metadata[key]) for key in _HIERARCHY_KEYS if key in metadata}


def _public_authorization_value(value: Any, *, depth: int = 0) -> Any:
    """Project only non-secret authorization discovery data to Go and the UI."""

    if depth > 8:
        return None
    if isinstance(value, dict):
        return {
            str(key): _public_authorization_value(item, depth=depth + 1)
            for key, item in list(value.items())[:256]
            if str(key).lower() not in _AUTHORIZATION_PRIVATE_KEYS
        }
    if isinstance(value, (list, tuple)):
        return [
            _public_authorization_value(item, depth=depth + 1)
            for item in value[:256]
        ]
    return _json_value(value, depth=depth)


def _tool_display_metadata(serialized: Any, metadata: Any) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for source in (serialized.get("metadata") if isinstance(serialized, dict) else None, metadata):
        if not isinstance(source, dict):
            continue
        for key in ("display_name", "toolkit_name", "toolkit_type", "agent_type", "original_name"):
            if key in source:
                result[key] = _json_value(source[key])
    return result


def _is_mcp_authorization_required(error: BaseException) -> bool:
    """Recognize the current SDK exception across development reloads."""

    return error.__class__.__name__ == "McpAuthorizationRequired"


def _model_name(serialized: Any, metadata: Any) -> str:
    if isinstance(metadata, dict):
        for key in ("ls_model_name", "model_name"):
            if isinstance(metadata.get(key), str) and metadata[key]:
                return _bounded_text(metadata[key], "model")
    if isinstance(serialized, dict) and isinstance(serialized.get("name"), str):
        return _bounded_text(serialized["name"], "model")
    return "model"


def _run_id(value: Any) -> str:
    selected = str(value)
    if not selected or len(selected.encode("utf-8")) > 512 or any(c in selected for c in "\x00\r\n"):
        raise InvalidInput("The agent callback run identity is malformed.")
    return selected


def _bounded_text(value: Any, fallback: str) -> str:
    if isinstance(value, str) and value and len(value.encode("utf-8")) <= 2048:
        return value
    return fallback


def _trace_text(value: Any) -> str | None:
    if value is None:
        return None
    if not isinstance(value, str):
        try:
            value = json.dumps(_json_value(value), ensure_ascii=False, separators=(",", ":"))
        except (TypeError, ValueError):
            value = str(value)
    encoded = value.encode("utf-8", errors="replace")
    if len(encoded) > _MAX_TRACE_TEXT_BYTES:
        encoded = encoded[:_MAX_TRACE_TEXT_BYTES]
        value = encoded.decode("utf-8", errors="ignore")
    return value


def _compact_skill(raw: dict[str, Any], *, name: str) -> dict[str, Any]:
    """Return the current UI/persistence skill identity without its body."""

    return {
        "skill_id": _json_value(raw.get("skill_id")),
        "name": name,
        "icon_meta": _json_value(raw.get("icon_meta")),
    }


def _valid_compact_skill(raw: Any) -> dict[str, Any] | None:
    if not isinstance(raw, dict):
        return None
    skill_id = raw.get("skill_id")
    name = raw.get("name")
    icon_meta = raw.get("icon_meta")
    if (
        not isinstance(skill_id, int)
        or isinstance(skill_id, bool)
        or skill_id <= 0
        or not isinstance(name, str)
        or not name.strip()
        or not isinstance(icon_meta, (dict, type(None)))
    ):
        return None
    return {
        "skill_id": skill_id,
        "name": name,
        "icon_meta": _json_value(icon_meta),
    }


def _register_unique_skill(
    registry: dict[Any, dict[str, Any] | None],
    key: Any,
    identity: dict[str, Any],
) -> None:
    if key not in registry:
        registry[key] = dict(identity)
        return
    existing = registry[key]
    if existing != identity:
        registry[key] = None


def _immediate_parent_name(hierarchy: dict[str, Any]) -> str | None:
    path = hierarchy.get("parent_agent_path")
    if isinstance(path, list) and path:
        last = path[-1]
        if isinstance(last, dict):
            name = last.get("name")
            if isinstance(name, str) and name.strip():
                return name.strip().lower()
    parent = hierarchy.get("parent_agent_name")
    if isinstance(parent, str) and parent.strip():
        return parent.strip().lower()
    return None


def _public_load_skill_output(output: str | None) -> str | None:
    """Keep browser trace status while withholding the instruction body."""

    if output is None:
        return None
    match = _LOADED_SKILL_PREFIX_RE.match(output) or (
        _LOAD_SKILL_ALREADY_ACTIVE_RE.match(output)
    )
    if match is None:
        return "The requested skill is active."
    return f'Skill "{match.group(1)}" is active.'


def _public_application_details(application: Any) -> Any:
    """Project application metadata without attached skill instructions."""

    projected = _json_value(application)
    if not isinstance(projected, dict):
        return projected
    version_details = projected.get("version_details")
    if not isinstance(version_details, dict):
        return projected
    skills = version_details.get("skills")
    if not isinstance(skills, list):
        return projected
    public_skills = []
    for raw in skills:
        if not isinstance(raw, dict):
            public_skills.append(raw)
            continue
        skill = dict(raw)
        skill.pop("instructions", None)
        public_skills.append(skill)
    public_version = dict(version_details)
    public_version["skills"] = public_skills
    public_application = dict(projected)
    public_application["version_details"] = public_version
    return public_application


def _json_value(value: Any, *, depth: int = 0) -> Any:
    if depth > 8:
        return None
    if value is None or isinstance(value, (str, int, bool)):
        return value
    if isinstance(value, float):
        return value if value == value and abs(value) != float("inf") else None
    if isinstance(value, dict):
        return {str(key): _json_value(item, depth=depth + 1) for key, item in list(value.items())[:256]}
    if isinstance(value, (list, tuple)):
        return [_json_value(item, depth=depth + 1) for item in value[:256]]
    return str(value)


def _now() -> str:
    return datetime.now(tz=timezone.utc).isoformat()
