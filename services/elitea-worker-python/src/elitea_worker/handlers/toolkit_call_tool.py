"""``toolkit.call_tool.v1`` handler kernel: run ONE tool of ONE toolkit.

The kernel makes exactly one SDK call and returns a bounded typed result. It
does not publish a Redis message, emit an output frame, or settle; the delivery
processor owns all of that, exactly as it does for ``index.ingest.v1``.

Why a tool that RAISED is a successful run here. The caller asked whether the
tool works against its saved settings. "It raised, and here is the sentence it
raised with" answers that question, so it is a TOOL_ERROR outcome carried in
the result, not a runtime failure that loses the answer. Only a failure of the
runtime itself — a claim lost, a deadline passed, the SDK budget exhausted —
escapes as a WorkerError.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

from elitea_worker.agents.sdk_adapter import EliteaSdkToolkitToolAdapter
from elitea_worker.constants import MAX_TOOL_RESULT_BYTES
from elitea_worker.execution.errors import InvalidInput
from elitea_worker.execution.supervisor import ExecutionRunner


STATUS_OK = "ok"
STATUS_TOOL_ERROR = "tool_error"
STATUS_UNKNOWN_TOOL = "unknown_tool"


@dataclass(frozen=True, slots=True)
class ToolkitCallToolInputBinding:
    entry_id: str
    immutable_version: str
    content_digest: bytes


@dataclass(frozen=True, slots=True)
class ResolvedToolkitCallToolInput:
    binding: ToolkitCallToolInputBinding
    value: Any


@dataclass(frozen=True, slots=True)
class ToolkitCallToolRequest:
    toolkit_type: str
    toolkit_id: str
    toolkit_version: str
    tool_name: str
    input_bundle_id: str
    input_bundle_digest: bytes
    settings: ResolvedToolkitCallToolInput
    arguments: ResolvedToolkitCallToolInput
    runtime_config: dict[str, Any]
    llm_model: str | None = None
    llm_configuration: dict[str, Any] | None = None
    mcp_tokens: dict[str, Any] | None = None


@dataclass(frozen=True, slots=True)
class ToolkitCallToolOutcome:
    """The bounded typed form of one tool run.

    ``result_json`` is the canonical JSON encoding of the SDK return value, or
    an empty string when the run produced no value. ``truncated`` is true when
    the encoding exceeded the inline bound; the encoding is then DROPPED rather
    than cut, because half a JSON document reads as a corrupt result and this
    boundary must never hand a caller one.
    """

    status: str
    result_json: str
    truncated: bool
    error_message: str


@dataclass(frozen=True, slots=True)
class ToolkitCallToolResult:
    toolkit_type: str
    tool_name: str
    input_bundle_id: str
    input_bundle_digest: bytes
    settings: ToolkitCallToolInputBinding
    arguments: ToolkitCallToolInputBinding
    outcome: ToolkitCallToolOutcome


class ToolkitCallToolHandler:
    """Invoke one SDK tool through one bounded synchronous execution slot.

    One admitted kernel invocation makes one SDK call. Redis redelivery can run
    a later invocation again; this class makes no exactly-once-effect claim,
    and the same warning that stands over ``IndexIngestHandler`` stands here.
    Running a toolkit tool IS effecting provider work.
    """

    def __init__(
        self,
        sdk: EliteaSdkToolkitToolAdapter,
        supervisor: ExecutionRunner,
    ) -> None:
        self._sdk = sdk
        self._supervisor = supervisor

    async def execute(self, request: ToolkitCallToolRequest) -> ToolkitCallToolResult:
        _validate_request(request)
        sdk_result = await self._supervisor.run_sync(
            self._sdk.call_tool,
            toolkit_config=request.settings.value,
            tool_name=request.tool_name,
            tool_params=request.arguments.value,
            runtime_config=request.runtime_config,
            llm_model=request.llm_model,
            # The current wrapper uses kwargs.get("llm_settings", {}), so an
            # absent reference maps to an empty dict rather than None.
            llm_config=request.llm_configuration or {},
            mcp_tokens=request.mcp_tokens,
        )
        return ToolkitCallToolResult(
            toolkit_type=request.toolkit_type,
            tool_name=request.tool_name,
            input_bundle_id=request.input_bundle_id,
            input_bundle_digest=request.input_bundle_digest,
            settings=request.settings.binding,
            arguments=request.arguments.binding,
            outcome=_outcome_from(sdk_result),
        )


def _outcome_from(sdk_result: Any) -> ToolkitCallToolOutcome:
    """Project the SDK response onto the closed status set.

    ``EliteAClient.test_toolkit_tool`` answers a dict carrying ``success``,
    ``result`` and ``error``. It reports an unknown tool name through the same
    shape, so the message is the only thing that separates "the tool is not
    there" from "the tool ran and failed" — and this worker does not guess
    between them from message text. UNKNOWN_TOOL is reserved for the case the
    SDK states outright, which today is an absent ``tool_name`` echo.
    """

    if not isinstance(sdk_result, dict):
        # The SDK contract says dict. Anything else is a result this boundary
        # cannot describe, and inventing a status for it would hide a real
        # incompatibility behind a successful-looking run.
        raise InvalidInput("The toolkit tool returned an unsupported result shape.")

    error_message = _safe_sentence(sdk_result.get("error"))
    if sdk_result.get("success") is True:
        status = STATUS_OK
    elif not sdk_result.get("tool_name"):
        status = STATUS_UNKNOWN_TOOL
    else:
        status = STATUS_TOOL_ERROR

    encoded = _canonical_json(sdk_result.get("result"))
    if encoded is None:
        return ToolkitCallToolOutcome(
            status=status,
            result_json="",
            truncated=True,
            error_message=error_message,
        )
    return ToolkitCallToolOutcome(
        status=status,
        result_json=encoded,
        truncated=False,
        error_message=error_message,
    )


def _canonical_json(value: Any) -> str | None:
    """Encode one result value, or return None when it does not fit or encode.

    A value the SDK returned that JSON cannot represent is not an error of the
    run: the tool did execute. It is reported as truncated, which is exactly
    what "the result did not survive the boundary" means to a caller.
    """

    if value is None:
        return ""
    try:
        encoded = json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
            default=str,
        )
    except (TypeError, ValueError):
        return None
    if len(encoded.encode("utf-8")) > MAX_TOOL_RESULT_BYTES:
        return None
    return encoded


def _safe_sentence(value: Any) -> str:
    """Bound one error sentence.

    The SDK's error text is provider content. It is bounded here and never
    logged: an exception string is protected result data and must not become a
    log side channel.
    """

    if not isinstance(value, str) or not value:
        return ""
    encoded = value.encode("utf-8")[:MAX_TOOL_RESULT_BYTES]
    return encoded.decode("utf-8", errors="ignore")


def _validate_request(request: ToolkitCallToolRequest) -> None:
    required = (
        request.toolkit_type,
        request.tool_name,
        request.input_bundle_id,
        request.settings.binding.entry_id,
        request.settings.binding.immutable_version,
        request.arguments.binding.entry_id,
        request.arguments.binding.immutable_version,
    )
    digests = (
        request.input_bundle_digest,
        request.settings.binding.content_digest,
        request.arguments.binding.content_digest,
    )
    if (
        any(not isinstance(value, str) or not value for value in required)
        or any(len(value) != 32 for value in digests)
        or not isinstance(request.settings.value, dict)
        or not isinstance(request.arguments.value, dict)
        or not isinstance(request.runtime_config, dict)
    ):
        raise InvalidInput("The toolkit tool-run request is malformed.")
