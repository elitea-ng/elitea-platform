"""Strict mapping for the reference-only agent execution contract."""

from __future__ import annotations

import hashlib
from collections.abc import Sequence
from typing import Any

from google.protobuf.message import DecodeError

from elitea.runtime.v1 import agent_pb2

from elitea_worker.agents.attachments import (
    AttachmentContentWriteback,
    validate_input_attachments,
)
from elitea_worker.constants import MAX_AGENT_INPUT_BYTES
from elitea_worker.execution.errors import InvalidInput, ResourceExhausted
from elitea_worker.fixtures.bundle import parse_json_value
from elitea_worker.handlers.agent import (
    AgentExecutionInputBinding,
    AgentExecutionKind,
    AgentExecutionPayload,
    AgentExecutionRequest,
    AgentExecutionResult,
)

AGENT_INPUT_SCHEMA_REVISION = "elitea.runtime.agent-execution-input.v1"
AGENT_RESULT_MEDIA_TYPE = "application/vnd.elitea.agent-execution-result.v1+json"
AGENT_RESULT_CLASSIFICATION = "tenant-confidential"


def parse_agent_execution_input(raw: bytes) -> agent_pb2.AgentExecutionInputV1:
    if not raw or len(raw) > MAX_AGENT_INPUT_BYTES:
        raise ResourceExhausted("The agent execution input exceeds its limit.")
    message = agent_pb2.AgentExecutionInputV1()
    try:
        message.ParseFromString(raw)
    except DecodeError as exc:
        raise InvalidInput("The agent execution input is malformed.") from exc
    known = agent_pb2.AgentExecutionInputV1()
    known.CopyFrom(message)
    known.DiscardUnknownFields()
    canonical = known.SerializeToString(deterministic=True)
    if canonical != raw:
        raise InvalidInput("The agent execution input is not canonical protocol v1.")
    if message.schema_revision != AGENT_INPUT_SCHEMA_REVISION:
        raise InvalidInput("The agent execution input revision is not supported.")
    return message


def request_from(
    message: agent_pb2.AgentExecutionInputV1,
    *,
    kind: AgentExecutionKind,
    input_bundle_id: str,
    input_bundle_digest: bytes,
    request_entry_id: str,
    request_immutable_version: str,
    request_content_digest: bytes,
) -> AgentExecutionRequest:
    if (
        not input_bundle_id
        or len(input_bundle_digest) != 32
        or not request_entry_id
        or not request_immutable_version
        or len(request_content_digest) != 32
    ):
        raise InvalidInput("The agent execution input binding is malformed.")

    llm = _json_object(message.llm, "llm")
    chat_history = _json_list(message.chat_history, "chat history")
    user_input = parse_json_value(message.user_input)
    if not isinstance(user_input, (str, list)):
        raise InvalidInput("The agent user input must be text or content blocks.")
    tools = _json_list(message.tools, "tools")
    application = _json_object(message.application, "application")
    internal_tools = _string_list(message.internal_tools, "internal tools")
    mcp_tokens = _json_object(message.mcp_tokens, "MCP tokens")
    ignored_mcp_servers = _json_list(
        message.ignored_mcp_servers, "ignored MCP servers"
    )
    user_declined_mcp_servers = _json_list(
        message.user_declined_mcp_servers, "declined MCP servers"
    )
    hitl_decisions = _json_list(message.hitl_decisions, "HITL decisions")
    meta = _json_object(message.meta, "metadata")
    context_settings = _json_object(message.context_settings, "context settings")
    invoked_skills = _json_list(message.invoked_skills, "invoked skills")
    applied_skills = _json_list(message.applied_skills, "applied skills")
    attached_skills = _json_list(message.attached_skills, "attached skills")
    # #606: a JSON array was all this field ever had to be, because nothing
    # consumed it. Now that the chunks go in front of a model, their shape is
    # checked HERE — see elitea_worker.agents.attachments for the contract and
    # for why an unknown chunk type is refused instead of forwarded.
    input_attachments = validate_input_attachments(
        _json_list(message.input_attachments, "input attachments")
    )
    parallel_reconcile = _optional_json_object(
        message.parallel_reconcile, "parallel reconcile"
    )
    parallel_terminal_errors = _json_list(
        message.parallel_terminal_errors, "parallel terminal errors"
    )
    toolkit_guardrails = _toolkit_guardrails_policy(message.toolkit_guardrails)
    next_input_suggestion = _next_input_suggestion_policy(
        message.next_input_suggestion
    )
    if message.steps_limit < 1 and message.HasField("steps_limit"):
        raise InvalidInput("The agent step limit must be positive.")
    if (
        message.hitl_resume
        and not message.HasField("hitl_action")
        and not hitl_decisions
    ):
        raise InvalidInput("A HITL resume decision is required.")
    if kind is AgentExecutionKind.APPLICATION:
        _validate_application_identity(application)
    elif kind is AgentExecutionKind.ADHOC:
        if not _model_name(llm):
            raise InvalidInput("The ad-hoc agent model is required.")
    else:
        raise InvalidInput("The agent execution kind is not supported.")

    return AgentExecutionRequest(
        kind=kind,
        input_bundle_id=input_bundle_id,
        input_bundle_digest=input_bundle_digest,
        request_binding=AgentExecutionInputBinding(
            entry_id=request_entry_id,
            immutable_version=request_immutable_version,
            content_digest=request_content_digest,
        ),
        payload=AgentExecutionPayload(
            llm=llm,
            chat_history=chat_history,
            user_input=user_input,
            thread_id=message.thread_id if message.HasField("thread_id") else None,
            checkpoint_id=(
                message.checkpoint_id if message.HasField("checkpoint_id") else None
            ),
            debug=message.debug,
            tools=tools,
            application=application,
            internal_tools=internal_tools,
            steps_limit=message.steps_limit if message.HasField("steps_limit") else None,
            mcp_tokens=mcp_tokens,
            ignored_mcp_servers=ignored_mcp_servers,
            user_declined_mcp_servers=user_declined_mcp_servers,
            should_continue=message.should_continue,
            hitl_resume=message.hitl_resume,
            hitl_action=(message.hitl_action if message.HasField("hitl_action") else None),
            hitl_value=(message.hitl_value if message.HasField("hitl_value") else None),
            hitl_decisions=hitl_decisions,
            execution_generation=(
                message.execution_generation
                if message.HasField("execution_generation")
                else None
            ),
            is_regenerate=message.is_regenerate,
            meta=meta,
            conversation_id=(
                message.conversation_id if message.HasField("conversation_id") else None
            ),
            persona=message.persona or "generic",
            context_settings=context_settings,
            supports_vision=message.supports_vision,
            return_chat_history=message.return_chat_history,
            invoked_skills=invoked_skills,
            applied_skills=applied_skills,
            auto_approve_sensitive_actions=message.auto_approve_sensitive_actions,
            attached_skills=attached_skills,
            input_attachments=input_attachments,
            parallel_reconcile=parallel_reconcile,
            parallel_terminal_errors=parallel_terminal_errors,
            next_input_suggestion=next_input_suggestion,
            toolkit_guardrails=toolkit_guardrails,
            exception_handling_enabled=(
                message.exception_handling_enabled
                if message.HasField("exception_handling_enabled")
                else None
            ),
            debug_mode=(message.debug_mode if message.HasField("debug_mode") else None),
            project_context=_project_context_snapshot(message),
        ),
    )


def bind_result_artifact(
    result: AgentExecutionResult,
    *,
    artifact_id: str,
    immutable_version: str,
    byte_length: int,
    digest: bytes,
    attachment_contents: Sequence[AttachmentContentWriteback] = (),
) -> agent_pb2.AgentExecutionResultV1:
    """Bind the terminal artifact reference and any attachment write-back (#607).

    ``attachment_contents`` arrives already budgeted by
    ``attachment_content_writebacks``; this function only encodes it. The budget
    is not re-applied here on purpose — two places deciding what fits is two
    places that can disagree, and the one that knows which entries were dropped
    is the one that can say so in a log.
    """

    if (
        not artifact_id
        or not immutable_version
        or isinstance(byte_length, bool)
        or byte_length < 1
        or len(digest) != 32
    ):
        raise InvalidInput("The agent result artifact binding is malformed.")
    for writeback in attachment_contents:
        if not writeback.item_id or not writeback.content:
            raise InvalidInput("The agent attachment content binding is malformed.")
    request = result.request
    return agent_pb2.AgentExecutionResultV1(
        attachment_contents=[
            agent_pb2.AgentExecutionAttachmentContentV1(
                item_id=writeback.item_id,
                content=writeback.content,
            )
            for writeback in attachment_contents
        ],
        input_bundle_id=request.input_bundle_id,
        input_bundle_digest=_digest(request.input_bundle_digest),
        request_entry_id=request.request_binding.entry_id,
        request_immutable_version=request.request_binding.immutable_version,
        request_content_digest=_digest(request.request_binding.content_digest),
        terminal_state=_terminal_state(result.sdk_result),
        result_artifact=agent_pb2.AgentExecutionArtifactReferenceV1(
            artifact_id=artifact_id,
            immutable_version=immutable_version,
            media_type=AGENT_RESULT_MEDIA_TYPE,
            byte_length=byte_length,
            digest=_digest(digest),
            classification=AGENT_RESULT_CLASSIFICATION,
        ),
    )


def _json_object(raw: bytes, name: str) -> dict[str, Any]:
    value = parse_json_value(raw)
    if not isinstance(value, dict):
        raise InvalidInput(f"The agent {name} must be an object.")
    return value


def _optional_json_object(raw: bytes, name: str) -> dict[str, Any] | None:
    value = parse_json_value(raw)
    if value is None:
        return None
    if not isinstance(value, dict):
        raise InvalidInput(f"The agent {name} must be an object or null.")
    return value


# The guardrail fields the admission path sends, and the only ones accepted.
#
# Declared as a closed set for the same reason the suggestion policy's is: an
# unexpected key means the two sides disagree about the contract, and silently
# ignoring it would let a field be added on one side and never noticed missing
# on the other.
_TOOLKIT_GUARDRAIL_FIELDS = frozenset(
    {
        "blocked_toolkits",
        "blocked_tools",
        "sensitive_tools",
        "sensitive_action_company_name",
        "sensitive_action_message_template",
    }
)


def _toolkit_guardrails_policy(raw: bytes) -> dict[str, Any] | None:
    """Decode the per-run toolkit guardrails, or None when the field is absent.

    ABSENT is not EMPTY. An empty payload means this command came from a
    platform that does not send the field, and the worker must be left on its
    environment configuration exactly as before — that is what keeps existing
    deployments working. An empty *object* means the platform resolved a policy
    and it is empty, which is a real instruction: nothing is blocked and nothing
    is sensitive.
    """
    if not raw:
        return None
    value = parse_json_value(raw)
    if value is None:
        return None
    if not isinstance(value, dict):
        raise InvalidInput("The agent toolkit guardrails must be an object or null.")
    if set(value) - _TOOLKIT_GUARDRAIL_FIELDS:
        raise InvalidInput("The agent toolkit guardrails contain unsupported fields.")

    for name in ("sensitive_action_company_name", "sensitive_action_message_template"):
        if name in value and not isinstance(value[name], str):
            raise InvalidInput(f"The agent guardrail {name} must be a string.")
    if "blocked_toolkits" in value:
        _guardrail_name_list(value["blocked_toolkits"], "blocked_toolkits")
    for name in ("blocked_tools", "sensitive_tools"):
        mapping = value.get(name)
        if mapping is None:
            continue
        if not isinstance(mapping, dict):
            raise InvalidInput(f"The agent guardrail {name} must be an object.")
        for toolkit, tools in mapping.items():
            if not isinstance(toolkit, str):
                raise InvalidInput(f"The agent guardrail {name} keys must be strings.")
            _guardrail_name_list(tools, name)
    return value


def _guardrail_name_list(value: Any, name: str) -> None:
    """Every entry of a guardrail name list must be a string."""
    if not isinstance(value, list):
        raise InvalidInput(f"The agent guardrail {name} must be a list of names.")
    for entry in value:
        if not isinstance(entry, str):
            raise InvalidInput(f"The agent guardrail {name} entries must be strings.")


def _next_input_suggestion_policy(raw: bytes) -> dict[str, Any]:
    if not raw:
        return {}
    value = parse_json_value(raw)
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise InvalidInput(
            "The agent next input suggestion policy must be an object or null."
        )
    allowed = {"enabled", "min_response_chars", "timeout_seconds"}
    if set(value) - allowed:
        raise InvalidInput(
            "The agent next input suggestion policy contains unsupported fields."
        )
    enabled = value.get("enabled", False)
    min_chars = value.get("min_response_chars", 150)
    timeout = value.get("timeout_seconds", 15)
    if not isinstance(enabled, bool):
        raise InvalidInput(
            "The agent next input suggestion enabled flag must be boolean."
        )
    if (
        isinstance(min_chars, bool)
        or not isinstance(min_chars, int)
        or min_chars < 1
        or min_chars > 100_000
    ):
        raise InvalidInput(
            "The agent next input suggestion minimum length is invalid."
        )
    if (
        isinstance(timeout, bool)
        or not isinstance(timeout, int)
        or timeout < 1
        or timeout > 300
    ):
        raise InvalidInput(
            "The agent next input suggestion timeout is invalid."
        )
    return {
        "enabled": enabled,
        "min_response_chars": min_chars,
        "timeout_seconds": timeout,
    }


def _json_list(raw: bytes, name: str) -> list[Any]:
    value = parse_json_value(raw)
    if not isinstance(value, list):
        raise InvalidInput(f"The agent {name} must be a list.")
    return value


def _string_list(raw: bytes, name: str) -> list[str]:
    value = _json_list(raw, name)
    if any(not isinstance(item, str) for item in value):
        raise InvalidInput(f"The agent {name} must contain only strings.")
    return value


def _validate_application_identity(application: dict[str, Any]) -> None:
    application_id = application.get("id")
    version_id = application.get("version_id")
    version_details = application.get("version_details")
    ids_present = (
        isinstance(application_id, int)
        and not isinstance(application_id, bool)
        and application_id > 0
        and isinstance(version_id, int)
        and not isinstance(version_id, bool)
        and version_id > 0
    )
    if not ids_present and not isinstance(version_details, dict):
        raise InvalidInput("The configured application identity is required.")


def _model_name(llm: dict[str, Any]) -> str | None:
    kwargs = llm.get("kwargs")
    if not isinstance(kwargs, dict):
        return None
    model = kwargs.get("model")
    return model if isinstance(model, str) and model else None


def _terminal_state(value: dict[str, Any]) -> int:
    if value.get("parallel_dispatch"):
        return agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_PARKED_CHILDREN
    if value.get("paused"):
        if value.get("pause_type") == "mcp_auth":
            return agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH
        return agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_HITL
    if value.get("hitl_interrupt") or value.get("hitl_interrupts"):
        return agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_HITL
    return agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED


def _digest(value: bytes):
    from elitea.runtime.v1 import common_pb2

    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )


def _project_context_snapshot(message: agent_pb2.AgentExecutionInputV1) -> dict[str, str] | None:
    if not message.HasField("project_context"):
        return None
    snapshot = message.project_context
    clean = agent_pb2.ProjectContextSnapshotV1()
    clean.CopyFrom(snapshot)
    clean.DiscardUnknownFields()
    if (
        clean.SerializeToString() != snapshot.SerializeToString()
        or not snapshot.id or len(snapshot.id) > 256
        or not snapshot.scope or len(snapshot.scope) > 256
        or not snapshot.content
        or snapshot.revision != hashlib.sha256(snapshot.content.encode("utf-8")).hexdigest()
    ):
        raise InvalidInput("The project context snapshot is invalid.")
    return {name: getattr(snapshot, name) for name in (
        "id", "revision", "scope", "content", "activation_description"
    )}
