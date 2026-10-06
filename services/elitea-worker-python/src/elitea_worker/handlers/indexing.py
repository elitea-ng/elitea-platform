"""Bounded synchronous kernel for the current SDK ``index_data`` operation.

This module contains no Pylon task, event or response-wrapper behavior. Input
values have already been fetched through the scoped data plane and remain in
worker memory. The synchronous SDK call cannot be preempted; cancellation waits
for its thread to exit under ``ExecutionSupervisor.run_sync`` semantics.
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

from elitea_worker.agents.sdk_adapter import EliteaSdkIndexingAdapter
from elitea_worker.constants import MAX_SAFE_STRING_BYTES
from elitea_worker.execution.errors import InvalidInput, ResourceExhausted
from elitea_worker.execution.supervisor import ExecutionRunner
from elitea_worker.protocol.node_event import (
    MAX_CURRENT_NODE_EVENT_JSON_BYTES,
    InvalidCurrentNodeEvent,
    decode_current_node_event_json,
)


_CURRENT_INDEX_CUSTOM_EVENTS = {
    "thinking_step": (
        "agent_thinking_step",
        frozenset({"message", "tool_name", "toolkit"}),
    ),
    "thinking_step_update": (
        "agent_thinking_step_update",
        frozenset({"message", "tool_name", "toolkit", "markdown"}),
    ),
    "index_data_status": (
        "agent_index_data_status",
        frozenset(
            {
                "id",
                "index_name",
                "state",
                "error",
                "reindex",
                "indexed",
                "updated",
                "created_at",
                "updated_on",
                "toolkit_id",
            }
        ),
    ),
    "index_data_removed": (
        "agent_index_data_removed",
        frozenset({"index_name", "toolkit_id", "project_id"}),
    ),
}
_CURRENT_INDEX_TERMINAL_STATES = frozenset(
    {"completed", "failed", "partly_indexed", "scheduled_reindex", "cancelled"}
)

# SDK callbacks are durable, replayable UI output.  Treat progress text as an
# untrusted observation, not as a diagnostic channel: some current SDK tools
# interpolate their invocation ``kwargs`` into these messages.
_MAX_SAFE_PROGRESS_MESSAGE_BYTES = 1024
_SAFE_PROGRESS_FALLBACK = "Indexing progress updated."
_SAFE_INDEX_ERROR_FALLBACK = "Indexing reported an error."
_SAFE_INDEX_PARTIAL_FALLBACK = "Indexing completed with skipped content."
_LOADING_DOCUMENTS_STAGE = "Loading the documents to index."
_SENSITIVE_PROGRESS_PATTERNS = (
    re.compile(
        r"\b(?:api[_ -]?key|access[_ -]?token|auth[_ -]?token|private[_ -]?token|"
        r"client[_ -]?secret|password|credential|authorization|bearer)\b",
        re.IGNORECASE,
    ),
    re.compile(
        r"\b(?:chunking_config|toolkit_config|tool_params|llm_config(?:uration)?|"
        r"mcp_tokens|runtime_config|settings)\b",
        re.IGNORECASE,
    ),
    re.compile(r"\b(?:https?|ftp|postgres(?:ql)?|redis)://", re.IGNORECASE),
    re.compile(r"<[^>\r\n]{0,256}\bobject at 0x[0-9a-f]+>", re.IGNORECASE),
    re.compile(r"(?:^|[\s\"'])(?:/[^\s]+|[A-Za-z]:[\\/][^\s]+)"),
)
_OMIT_CURRENT_EVENT_FIELD = object()


@dataclass(frozen=True, slots=True)
class CurrentIndexNodeEventContext:
    """Current index-status fields derived from the claimed execution."""

    stream_id: str
    task_id: str
    initiator: str
    project_id: int | str
    user_id: int | str
    toolkit_id: int | str | None
    index_name: str | None = None
    message_id: str | None = None
    sio_event: str | None = None
    display_name: str = "index_data"


@dataclass(frozen=True, slots=True)
class CurrentIndexTerminalStatus:
    """Bounded terminal values observed from the current SDK callback."""

    state: str
    indexed: int
    updated: int
    reindex: bool | None


class CurrentIndexNodeEventCallback(BaseCallbackHandler):
    """Map current SDK index custom callbacks to bounded ``NodeEventV1``.

    The current Pylon callback also copied the whole materialized toolkit and
    tool-parameter dictionaries into response metadata. Those dictionaries can
    contain redeemed credentials and are not required to identify an index
    status. The language-neutral runtime instead carries the authoritative
    execution, project, user and toolkit identities while the SDK-owned status
    fields are projected to the small, safe values required by the current UI.
    """

    def __init__(
        self,
        context: CurrentIndexNodeEventContext,
        publish: Callable[[node_event_pb2.NodeEventV1], None],
    ) -> None:
        super().__init__()
        self.raise_error = True
        self._context = context
        self._publish = publish
        self._scheduled = context.initiator == "schedule"
        self._failure: Exception | None = None
        self._failure_lock = threading.Lock()
        self._tool_lock = threading.Lock()
        self._tool_run_id: str | None = None
        self._tool_started_at: str | None = None
        self._tool_finalized = False
        self._terminal_index_status_observed: str | None = None
        self._terminal_index_status_error: str | None = None
        self._terminal_index_status_indexed = 0
        self._terminal_index_status_updated = 0
        self._terminal_index_status_reindex: bool | None = None
        self._fallback_index_status_finalized = False

    def on_tool_start(
        self,
        serialized: dict[str, Any],
        input_str: str,
        *,
        run_id: Any,
        parent_run_id: Any | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        inputs: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        """Create the current UI tool chip without forwarding invocation data."""

        _ = serialized, input_str, tags, metadata, inputs, kwargs
        try:
            selected_run_id = _current_tool_run_id(run_id)
            now = datetime.now(tz=timezone.utc).isoformat()
            with self._tool_lock:
                if self._tool_run_id is not None:
                    if self._tool_run_id == selected_run_id:
                        return
                    return
                self._tool_run_id = selected_run_id
                self._tool_started_at = now
            if self._scheduled:
                return
            self._emit(
                "agent_tool_start",
                {
                    "tool_name": "index_data",
                    "tool_run_id": selected_run_id,
                    "timestamp_start": now,
                    "metadata": self._safe_tool_metadata(),
                },
                now=now,
            )
        except Exception as exc:
            self._record_failure(exc)
            raise

    def on_tool_end(
        self,
        output: Any,
        *,
        run_id: Any,
        parent_run_id: Any | None = None,
        **kwargs: Any,
    ) -> None:
        """Observe completion; the typed SDK result remains terminal authority."""

        _ = output, kwargs
        self._validate_active_tool_run(run_id)

    def on_tool_error(
        self,
        error: BaseException,
        *,
        run_id: Any,
        parent_run_id: Any | None = None,
        **kwargs: Any,
    ) -> None:
        """Observe failure without forwarding exceptions or tracebacks."""

        _ = error, kwargs
        self._validate_active_tool_run(run_id)

    def finish_tool(
        self, *, success: bool
    ) -> node_event_pb2.NodeEventV1 | None:
        """Build one safe lifecycle result for ordered async publication."""

        try:
            with self._tool_lock:
                if self._tool_run_id is None or self._tool_finalized:
                    return None
                run_id = self._tool_run_id
                started_at = self._tool_started_at
                self._tool_finalized = True
            if self._scheduled:
                return None
            now = datetime.now(tz=timezone.utc).isoformat()
            payload = {
                "tool_name": "index_data",
                "tool_run_id": run_id,
                "finish_reason": "stop" if success else "error",
                "timestamp_start": started_at,
                "timestamp_finish": now,
            }
            return self._build_event(
                "agent_tool_end" if success else "agent_tool_error",
                payload,
                now=now,
            )
        except Exception as exc:
            self._record_failure(exc)
            raise

    def finish_index_status_on_failure(
        self,
        *,
        correct_inconsistent: bool = False,
    ) -> node_event_pb2.NodeEventV1 | None:
        """Build one safe failed status when the SDK emitted no terminal status."""

        return self.finish_index_status_for_summary(
            "failed",
            correct_inconsistent=correct_inconsistent,
        )

    def finish_index_status_for_summary(
        self,
        state: str,
        *,
        correct_inconsistent: bool = False,
        error_message: str | None = None,
    ) -> node_event_pb2.NodeEventV1 | None:
        """Build a terminal correction for the reviewed result summary.

        ``error_message`` is a fixed safe sentence chosen by the result
        projection (never SDK text); it lets the live status name the same
        cause as the run's terminal error.

        A failed status the SDK already emitted counts as consistent only when
        it carries that same sentence. The SDK emits its own ``failed`` status
        with the raw provider text before it re-raises, and that text is
        usually projected to the generic fallback (a GitHub 403 body contains
        ``{``). Skipping the correction then would leave the live status on the
        generic sentence while the summary names the cause (#6876).
        """

        if state not in {"failed", "partly_indexed"}:
            raise ValueError("unsupported terminal index correction")
        if error_message is not None and (
            state != "failed"
            or not isinstance(error_message, str)
            or not error_message
        ):
            raise ValueError("unsupported terminal index error message")
        try:
            with self._tool_lock:
                observed = self._terminal_index_status_observed
                if self._fallback_index_status_finalized:
                    return None
                if observed == state and not (
                    correct_inconsistent
                    and error_message is not None
                    and self._terminal_index_status_error != error_message
                ):
                    return None
                if observed is not None and not correct_inconsistent:
                    return None
                self._fallback_index_status_finalized = True
                indexed = (
                    self._terminal_index_status_indexed
                    if state == "partly_indexed"
                    else 0
                )
                updated = (
                    self._terminal_index_status_updated
                    if state == "partly_indexed"
                    else 0
                )
            now = datetime.now(tz=timezone.utc).isoformat()
            payload = {
                "task_id": self._context.task_id,
                "index_name": self._context.index_name,
                "state": state,
                "error": (
                    (error_message or _SAFE_INDEX_ERROR_FALLBACK)
                    if state == "failed"
                    else _SAFE_INDEX_PARTIAL_FALLBACK
                ),
                "indexed": indexed,
                "updated": updated,
                "toolkit_id": self._context.toolkit_id,
                "initiator": self._context.initiator,
                "project_id": self._context.project_id,
                "user_id": self._context.user_id,
            }
            return self._build_event(
                "agent_index_data_status",
                payload,
                now=now,
            )
        except Exception as exc:
            self._record_failure(exc)
            raise

    def on_custom_event(
        self,
        name: str,
        data: Any,
        *,
        run_id: Any,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> None:
        """Handle the current progress callbacks emitted by index toolkits."""

        _ = tags, metadata, kwargs
        selected = _CURRENT_INDEX_CUSTOM_EVENTS.get(name)
        if selected is None:
            return
        if self._scheduled and name in {"thinking_step", "thinking_step_update"}:
            return
        try:
            if not isinstance(data, dict):
                raise InvalidInput("The index progress callback is malformed.")
            event_type, fields = selected
            selected_run_id = _current_tool_run_id(run_id)
            with self._tool_lock:
                if self._tool_run_id is not None:
                    selected_run_id = self._tool_run_id
            now = datetime.now(tz=timezone.utc).isoformat()
            payload = {
                "name": name,
                "run_id": selected_run_id,
                "tool_run_id": selected_run_id,
                "metadata": self._safe_tool_metadata(),
                "datetime": now,
                **{
                    field: value
                    for field in sorted(fields)
                    if field in data
                    if (
                        value := _project_current_event_field(
                            name, field, data[field]
                        )
                    )
                    is not _OMIT_CURRENT_EVENT_FIELD
                },
            }
            if name == "index_data_status":
                payload.update(
                    {
                        "task_id": self._context.task_id,
                        "initiator": self._context.initiator,
                        "project_id": self._context.project_id,
                        "user_id": self._context.user_id,
                    }
                )
                if (
                    not payload.get("toolkit_id")
                    and self._context.toolkit_id is not None
                ):
                    payload["toolkit_id"] = self._context.toolkit_id
            elif name == "index_data_removed":
                if "project_id" not in payload:
                    payload["project_id"] = self._context.project_id
                if (
                    not payload.get("toolkit_id")
                    and self._context.toolkit_id is not None
                ):
                    payload["toolkit_id"] = self._context.toolkit_id

            self._emit(event_type, payload, now=now)
            if (
                name == "index_data_status"
                and payload.get("state") in _CURRENT_INDEX_TERMINAL_STATES
            ):
                with self._tool_lock:
                    self._terminal_index_status_observed = payload.get("state")
                    observed_error = payload.get("error")
                    self._terminal_index_status_error = (
                        observed_error if isinstance(observed_error, str) else None
                    )
                    indexed = payload.get("indexed")
                    updated = payload.get("updated")
                    self._terminal_index_status_indexed = (
                        indexed
                        if isinstance(indexed, int)
                        and not isinstance(indexed, bool)
                        and 0 <= indexed < 1 << 64
                        else 0
                    )
                    self._terminal_index_status_updated = (
                        updated
                        if isinstance(updated, int)
                        and not isinstance(updated, bool)
                        and 0 <= updated < 1 << 64
                        else 0
                    )
                    reindex = payload.get("reindex")
                    self._terminal_index_status_reindex = (
                        reindex if isinstance(reindex, bool) else None
                    )
        except Exception as exc:
            self._record_failure(exc)
            raise

    def terminal_status(self) -> CurrentIndexTerminalStatus | None:
        """Return the exact safe terminal callback snapshot, if one arrived."""

        with self._tool_lock:
            state = self._terminal_index_status_observed
            if state is None:
                return None
            return CurrentIndexTerminalStatus(
                state=state,
                indexed=self._terminal_index_status_indexed,
                updated=self._terminal_index_status_updated,
                reindex=self._terminal_index_status_reindex,
            )

    def _validate_active_tool_run(self, run_id: Any) -> None:
        try:
            selected_run_id = _current_tool_run_id(run_id)
            with self._tool_lock:
                if self._tool_run_id is not None and self._tool_run_id != selected_run_id:
                    return
        except Exception as exc:
            self._record_failure(exc)
            raise

    def _safe_tool_metadata(self) -> dict[str, str]:
        display_name = self._context.display_name
        if not _valid_text(display_name):
            display_name = "index_data"
        return {
            "initiator": self._context.initiator,
            "tool_name": "index_data",
            "display_name": display_name,
        }

    def _emit(self, event_type: str, payload: dict[str, Any], *, now: str) -> None:
        self._publish(self._build_event(event_type, payload, now=now))

    def _build_event(
        self, event_type: str, payload: dict[str, Any], *, now: str
    ) -> node_event_pb2.NodeEventV1:
        raw = json.dumps(
            {
                "type": event_type,
                "stream_id": self._context.stream_id or None,
                "message_id": self._context.message_id or None,
                "content": None,
                "response_metadata": payload,
                "references": [],
                "sio_event": self._context.sio_event or None,
                "created_at": now,
            },
            ensure_ascii=False,
            allow_nan=False,
            separators=(",", ":"),
        ).encode("utf-8")
        if len(raw) > MAX_CURRENT_NODE_EVENT_JSON_BYTES:
            raise ResourceExhausted(
                "The index progress event exceeds the approved output limit."
            )
        try:
            return decode_current_node_event_json(raw)
        except InvalidCurrentNodeEvent as exc:
            raise InvalidInput("The index progress callback is malformed.") from exc

    def _record_failure(self, failure: Exception) -> None:
        with self._failure_lock:
            if self._failure is None:
                self._failure = failure

    def raise_if_failed(self) -> None:
        """Surface callback errors that the synchronous SDK deliberately catches."""

        with self._failure_lock:
            failure = self._failure
        if failure is not None:
            raise failure


def _project_current_event_field(name: str, field: str, value: Any) -> Any:
    """Keep the current event shape while rejecting unbounded callback data."""

    if field == "message":
        return _project_progress_message(value, _SAFE_PROGRESS_FALLBACK)
    if field == "error":
        if value is None:
            return None
        return _project_progress_message(value, _SAFE_INDEX_ERROR_FALLBACK)
    if field in {"tool_name", "toolkit"}:
        return _project_progress_label(value, field)
    if field == "markdown":
        return value if isinstance(value, bool) else _OMIT_CURRENT_EVENT_FIELD
    return _project_scalar(value)


def _project_progress_message(value: Any, fallback: str) -> str:
    """Project progress text to a bounded, credential-free UI message."""

    if not isinstance(value, str):
        return fallback
    # This is the current SDK's known unsafe message shape.  Preserve the UI
    # stage without copying the interpolated invocation kwargs that follow it.
    if value.startswith("Loading the documents to index..."):
        return _LOADING_DOCUMENTS_STAGE
    if not _safe_progress_text(value, _MAX_SAFE_PROGRESS_MESSAGE_BYTES):
        return fallback
    return value


def _project_progress_label(value: Any, fallback: str) -> str:
    if not isinstance(value, str) or not _safe_progress_text(value, 256):
        return fallback
    return value


def _project_scalar(value: Any) -> Any:
    """Only callback scalar fields required by the current UI are retained."""

    if value is None or isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, str) and _safe_progress_text(value, 256):
        return value
    return _OMIT_CURRENT_EVENT_FIELD


def _safe_progress_text(value: str, maximum_bytes: int) -> bool:
    try:
        encoded = value.encode("utf-8")
    except UnicodeError:
        return False
    if not encoded or len(encoded) > maximum_bytes:
        return False
    if any(character in value for character in ("\x00", "\r", "\n", "{", "[")):
        return False
    return not any(pattern.search(value) for pattern in _SENSITIVE_PROGRESS_PATTERNS)


def _current_tool_run_id(value: Any) -> str:
    selected = str(value)
    if not _valid_text(selected):
        raise InvalidInput("The index callback run identity is malformed.")
    return selected


@dataclass(frozen=True, slots=True)
class IndexIngestInputBinding:
    entry_id: str
    immutable_version: str
    content_digest: bytes


@dataclass(frozen=True, slots=True)
class ResolvedIndexIngestInput:
    binding: IndexIngestInputBinding
    value: Any


@dataclass(frozen=True, slots=True)
class IndexIngestRequest:
    input_bundle_id: str
    input_bundle_digest: bytes
    toolkit_configuration: ResolvedIndexIngestInput
    tool_parameters: ResolvedIndexIngestInput
    llm_model: ResolvedIndexIngestInput | None
    llm_configuration: ResolvedIndexIngestInput | None
    mcp_tokens: ResolvedIndexIngestInput | None
    runtime_config: dict[str, Any]
    embedding_binding: ResolvedIndexIngestInput | None = None


@dataclass(frozen=True, slots=True)
class IndexIngestResult:
    """Trusted-memory SDK result plus immutable input bindings.

    ``sdk_result`` can contain protected values inherited from the current SDK
    response and must not be serialized directly. A later output composer owns
    the reviewed current-response projection and artifact persistence.
    """

    input_bundle_id: str
    input_bundle_digest: bytes
    toolkit_configuration: IndexIngestInputBinding
    tool_parameters: IndexIngestInputBinding
    llm_model: IndexIngestInputBinding | None
    llm_configuration: IndexIngestInputBinding | None
    mcp_tokens: IndexIngestInputBinding | None
    sdk_result: dict[str, Any]
    embedding_binding: IndexIngestInputBinding | None = None


class IndexIngestHandler:
    """Invoke the current synchronous SDK business method through one bound.

    One admitted kernel invocation makes one SDK call. Command-bus redelivery can run
    a later invocation again; this class makes no exactly-once-effect claim.
    """

    def __init__(
        self,
        sdk: EliteaSdkIndexingAdapter,
        supervisor: ExecutionRunner,
    ) -> None:
        self._sdk = sdk
        self._supervisor = supervisor

    async def execute(self, request: IndexIngestRequest) -> IndexIngestResult:
        _validate_request(request)
        llm_configuration = _optional_value(request.llm_configuration) or {}
        sdk_result = await self._supervisor.run_sync(
            self._sdk.ingest,
            toolkit_config=request.toolkit_configuration.value,
            tool_params=request.tool_parameters.value,
            runtime_config=request.runtime_config,
            llm_model=_effective_llm_model(request.llm_model, llm_configuration),
            # The current wrapper uses kwargs.get("llm_settings", {}), so an
            # absent reference maps to an empty dict rather than None.
            llm_config=llm_configuration,
            mcp_tokens=_optional_value(request.mcp_tokens),
        )
        return IndexIngestResult(
            input_bundle_id=request.input_bundle_id,
            input_bundle_digest=request.input_bundle_digest,
            toolkit_configuration=request.toolkit_configuration.binding,
            tool_parameters=request.tool_parameters.binding,
            llm_model=_optional_binding(request.llm_model),
            llm_configuration=_optional_binding(request.llm_configuration),
            mcp_tokens=_optional_binding(request.mcp_tokens),
            embedding_binding=_optional_binding(request.embedding_binding),
            sdk_result=sdk_result,
        )


def _validate_request(request: IndexIngestRequest) -> None:
    if (
        not _valid_text(request.input_bundle_id)
        or not isinstance(request.input_bundle_digest, bytes)
        or len(request.input_bundle_digest) != 32
    ):
        raise InvalidInput()
    required = (request.toolkit_configuration, request.tool_parameters)
    optional = (
        request.llm_model,
        request.llm_configuration,
        request.mcp_tokens,
        request.embedding_binding,
    )
    if any(not _valid_binding(item.binding) for item in required):
        raise InvalidInput()
    if any(
        item is not None and not _valid_binding(item.binding) for item in optional
    ):
        raise InvalidInput()
    if (
        not isinstance(request.toolkit_configuration.value, dict)
        or not request.toolkit_configuration.value
    ):
        raise InvalidInput()
    if not isinstance(request.tool_parameters.value, dict):
        raise InvalidInput()
    if request.llm_model is not None and not isinstance(request.llm_model.value, str):
        raise InvalidInput()
    if request.llm_configuration is not None and not isinstance(
        request.llm_configuration.value, dict
    ):
        raise InvalidInput()
    if request.mcp_tokens is not None and not isinstance(
        request.mcp_tokens.value, dict
    ):
        raise InvalidInput()
    if not isinstance(request.runtime_config, dict):
        raise InvalidInput()


def _valid_binding(binding: IndexIngestInputBinding) -> bool:
    return bool(
        _valid_text(binding.entry_id)
        and _valid_text(binding.immutable_version)
        and isinstance(binding.content_digest, bytes)
        and len(binding.content_digest) == 32
    )


def _valid_text(value: object) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        return len(value.encode("utf-8")) <= MAX_SAFE_STRING_BYTES
    except UnicodeEncodeError:
        return False


def _optional_value(value: ResolvedIndexIngestInput | None) -> Any:
    return None if value is None else value.value


def _optional_binding(
    value: ResolvedIndexIngestInput | None,
) -> IndexIngestInputBinding | None:
    return None if value is None else value.binding


def _effective_llm_model(
    value: ResolvedIndexIngestInput | None,
    llm_configuration: dict[str, Any],
) -> str | None:
    model = _optional_value(value)
    if not model:
        model = llm_configuration.get("model_name")
    if model is not None and not isinstance(model, str):
        raise InvalidInput()
    return model
