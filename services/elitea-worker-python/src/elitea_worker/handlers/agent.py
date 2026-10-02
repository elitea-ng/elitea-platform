"""Parity kernel for the two current agent execution entry points.

This module owns no transport, SDK imports, checkpoint store or browser
projection. It selects only the current configured-application versus ad-hoc
SDK operation and preserves the exact immutable input binding. Production
registration remains gated on callbacks, resume, checkpoint and child-dispatch
parity.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import Any, Protocol


class AgentExecutionKind(Enum):
    APPLICATION = "application"
    ADHOC = "adhoc"


@dataclass(frozen=True, slots=True)
class AgentExecutionInputBinding:
    entry_id: str
    immutable_version: str
    content_digest: bytes


@dataclass(frozen=True, slots=True)
class AgentExecutionPayload:
    llm: dict[str, Any]
    chat_history: list[Any]
    user_input: str | list[Any]
    thread_id: str | None
    checkpoint_id: str | None
    debug: bool
    tools: list[Any]
    application: dict[str, Any]
    internal_tools: list[str]
    steps_limit: int | None
    mcp_tokens: dict[str, Any]
    ignored_mcp_servers: list[Any]
    user_declined_mcp_servers: list[Any]
    should_continue: bool
    hitl_resume: bool
    hitl_action: str | None
    hitl_value: str | None
    hitl_decisions: list[Any]
    execution_generation: str | None
    is_regenerate: bool
    meta: dict[str, Any]
    conversation_id: str | None
    persona: str
    context_settings: dict[str, Any]
    supports_vision: bool
    return_chat_history: bool
    invoked_skills: list[Any]
    applied_skills: list[Any]
    auto_approve_sensitive_actions: bool
    attached_skills: list[Any]
    input_attachments: list[Any]
    parallel_reconcile: dict[str, Any] | None
    parallel_terminal_errors: list[Any]
    next_input_suggestion: dict[str, Any]
    # The platform-wide toolkit guardrails resolved at admission, or None when
    # the field was absent. None and {} are DIFFERENT: absent leaves the worker
    # on its environment configuration (ELITEA_SENSITIVE_TOOLS and friends),
    # which is what keeps a deployment that predates the field working; an empty
    # object means the platform resolved a policy and it is empty.
    toolkit_guardrails: dict[str, Any] | None
    exception_handling_enabled: bool | None
    debug_mode: bool | None
    project_context: dict[str, str] | None = None


@dataclass(frozen=True, slots=True)
class AgentExecutionRequest:
    kind: AgentExecutionKind
    input_bundle_id: str
    input_bundle_digest: bytes
    request_binding: AgentExecutionInputBinding
    payload: AgentExecutionPayload


@dataclass(frozen=True, slots=True)
class AgentExecutionResult:
    request: AgentExecutionRequest
    sdk_result: dict[str, Any]


class AgentSdkPort(Protocol):
    def execute_application(self, payload: AgentExecutionPayload) -> dict[str, Any]: ...

    def execute_adhoc(self, payload: AgentExecutionPayload) -> dict[str, Any]: ...


class AgentExecutionHandler:
    """Delegate exactly once to the selected current SDK semantic entry point."""

    def __init__(self, adapter: AgentSdkPort) -> None:
        self._adapter = adapter

    def execute(self, request: AgentExecutionRequest) -> AgentExecutionResult:
        if request.kind is AgentExecutionKind.APPLICATION:
            result = self._adapter.execute_application(request.payload)
        elif request.kind is AgentExecutionKind.ADHOC:
            result = self._adapter.execute_adhoc(request.payload)
        else:  # pragma: no cover - Enum construction prevents this in production.
            raise ValueError("unsupported agent execution kind")
        if not isinstance(result, dict):
            raise TypeError("the SDK agent result must be an object")
        return AgentExecutionResult(request=request, sdk_result=result)
