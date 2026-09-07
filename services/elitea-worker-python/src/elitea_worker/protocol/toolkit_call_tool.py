"""Pure contract mapping for ``toolkit.call_tool.v1``.

This module maps between the wire command and the handler kernel. It creates no
output frame and stores nothing; ``build_output_frame`` and the delivery
processor own those steps.

The result rides as a bounded typed SUMMARY, not as an artifact reference. That
is a deliberate difference from ``toolkit.available_tools.v1``, whose artifact
form has never been reachable because no artifact writer exists in this
platform. ``index.ingest.v1`` already carries its terminal result as a typed
inline summary on the authenticated output data plane, and this capability
rides the same plane, so it takes the same form. The proto keeps a
``result_artifact`` field for the day a writer exists.
"""

from __future__ import annotations

from elitea.runtime.v1 import common_pb2, toolkit_pb2

from elitea_worker.execution.errors import InvalidInput
from elitea_worker.handlers.toolkit_call_tool import (
    STATUS_OK,
    STATUS_TOOL_ERROR,
    STATUS_UNKNOWN_TOOL,
    ResolvedToolkitCallToolInput,
    ToolkitCallToolRequest,
    ToolkitCallToolResult,
)


_STATUS = {
    STATUS_OK: toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_OK,
    STATUS_TOOL_ERROR: toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR,
    STATUS_UNKNOWN_TOOL: toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL,
}


def request_from(
    command: toolkit_pb2.ToolkitCallToolCommandV1,
    *,
    input_bundle_id: str,
    input_bundle_digest: bytes,
    settings: ResolvedToolkitCallToolInput,
    arguments: ResolvedToolkitCallToolInput,
    runtime_config: dict,
    llm_model: str | None = None,
    llm_configuration: dict | None = None,
    mcp_tokens: dict | None = None,
) -> ToolkitCallToolRequest:
    """Bind resolved inputs to the command that named them.

    The entry-id check is the whole point of this function. The bundle manifest
    and the command are two independent statements about which content this run
    consumes; a run whose settings came from an entry the command did not name
    is not the run the caller authorized.
    """

    expected = (
        (command.settings_entry_id, settings),
        (command.arguments_entry_id, arguments),
    )
    if any(not _matches(entry_id, value) for entry_id, value in expected):
        raise InvalidInput("A tool-run input does not match its command reference.")
    return ToolkitCallToolRequest(
        toolkit_type=command.toolkit_type,
        toolkit_id=command.toolkit_id,
        toolkit_version=command.toolkit_version,
        tool_name=command.tool_name,
        input_bundle_id=input_bundle_id,
        input_bundle_digest=input_bundle_digest,
        settings=settings,
        arguments=arguments,
        runtime_config=runtime_config,
        llm_model=llm_model,
        llm_configuration=llm_configuration,
        mcp_tokens=mcp_tokens,
    )


def bind_result_summary(
    result: ToolkitCallToolResult,
) -> toolkit_pb2.ToolkitCallToolResultV1:
    """Project the bounded terminal fields onto the wire result."""

    status = _STATUS.get(result.outcome.status)
    if status is None:
        raise InvalidInput("The tool-run outcome status is not a known value.")
    return toolkit_pb2.ToolkitCallToolResultV1(
        toolkit_type=result.toolkit_type,
        tool_name=result.tool_name,
        input_bundle_id=result.input_bundle_id,
        input_bundle_digest=_digest(result.input_bundle_digest),
        settings_entry_id=result.settings.entry_id,
        settings_entry_version=result.settings.immutable_version,
        settings_content_digest=_digest(result.settings.content_digest),
        arguments_entry_id=result.arguments.entry_id,
        arguments_content_digest=_digest(result.arguments.content_digest),
        result_summary=toolkit_pb2.ToolkitCallToolSummaryV1(
            status=status,
            result_json=result.outcome.result_json,
            truncated=result.outcome.truncated,
            error_message=result.outcome.error_message,
        ),
    )


def unsupported_toolkit_result(
    command: toolkit_pb2.ToolkitCallToolCommandV1,
    *,
    input_bundle_id: str,
    input_bundle_digest: bytes,
    reason: str,
) -> toolkit_pb2.ToolkitCallToolResultV1:
    """Settle a toolkit type this worker image cannot build.

    This exists so that an unsupported toolkit is REFUSED with a reason rather
    than skipped. A skipped command settles nothing, so the caller's bounded
    wait burns its whole timeout and reports "slow" for a condition that will
    never change. The refusal is the settlement.
    """

    return toolkit_pb2.ToolkitCallToolResultV1(
        toolkit_type=command.toolkit_type,
        tool_name=command.tool_name,
        input_bundle_id=input_bundle_id,
        input_bundle_digest=_digest(input_bundle_digest),
        settings_entry_id=command.settings_entry_id,
        arguments_entry_id=command.arguments_entry_id,
        result_summary=toolkit_pb2.ToolkitCallToolSummaryV1(
            status=toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT,
            error_message=reason,
        ),
    )


def _matches(entry_id: str, resolved: ResolvedToolkitCallToolInput | None) -> bool:
    if not entry_id or resolved is None:
        return False
    return resolved.binding.entry_id == entry_id


def _digest(value: bytes) -> common_pb2.DigestV1:
    if len(value) != 32:
        raise InvalidInput("An output digest binding is malformed.")
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )
