from __future__ import annotations

import hashlib
import hmac
import json
import threading
from types import SimpleNamespace
from typing import Any

import pytest

from elitea.runtime.v1 import (
    agent_pb2,
    command_pb2,
    common_pb2,
    envelope_pb2,
    input_pb2,
)

from elitea_sdk.runtime.exceptions import BudgetExceededError

from elitea_worker.agents.sdk_adapter import EliteaSdkAgentAdapter, SdkBudgetExceeded
from elitea_worker.constants import (
    AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
    AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
    CONFORMANCE_HMAC_KEY,
    CONFORMANCE_HMAC_KEY_ID,
    ENVELOPE_SCHEMA_REVISION,
    LIMITS_REVISION,
    PROTOCOL_REVISION,
)
from elitea_worker.execution.errors import InvalidInput, UnsupportedCapability
from elitea_worker.handlers.agent import (
    AgentExecutionHandler,
    AgentExecutionKind,
)
from elitea_worker.protocol.agent import (
    AGENT_INPUT_SCHEMA_REVISION,
    bind_result_artifact,
    parse_agent_execution_input,
    request_from,
)
from elitea_worker.protocol.codec import (
    TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_signed_command,
)


def _json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def _input(*, application: bool = True) -> agent_pb2.AgentExecutionInputV1:
    app = (
        {
            "id": 11,
            "name": "reviewer",
            "version_id": 22,
            "variables": None,
            "version_details": {"meta": {"step_limit": 17}},
        }
        if application
        else {"instructions": "Be concise"}
    )
    return agent_pb2.AgentExecutionInputV1(
        schema_revision=AGENT_INPUT_SCHEMA_REVISION,
        llm=_json(
            {
                "kwargs": {
                    "model": "gpt-test",
                    "max_tokens": 512,
                    "stream": True,
                    "api_key": "must-not-be-forwarded",
                    "base_url": "https://untrusted.example",
                }
            }
        ),
        chat_history=_json([{"role": "user", "content": "earlier"}]),
        user_input=_json("current"),
        thread_id="thread-1",
        tools=_json([]),
        application=_json(app),
        internal_tools=_json([]),
        mcp_tokens=_json({}),
        ignored_mcp_servers=_json([]),
        user_declined_mcp_servers=_json([]),
        hitl_decisions=_json([]),
        meta=_json({}),
        persona="generic",
        context_settings=_json({}),
        supports_vision=True,
        invoked_skills=_json([]),
        applied_skills=_json([]),
        attached_skills=_json([]),
        input_attachments=_json([]),
        parallel_reconcile=_json(None),
        parallel_terminal_errors=_json([]),
    )


@pytest.mark.parametrize("application", [True, False])
def test_model_context_limits_are_a_recognized_shared_wire_field(application):
    message = _input(application=application)
    assert not message.HasField("model_context_limits")
    message.model_context_limits.CopyFrom(agent_pb2.ModelContextLimitsV1(
        context_window_tokens=1_000_000, max_output_tokens=128_000,
        max_output_fallback=True, max_input_tokens=872_000,
    ))
    decoded = parse_agent_execution_input(message.SerializeToString())
    assert decoded.model_context_limits == message.model_context_limits
    request = request_from(
        decoded, kind=AgentExecutionKind.APPLICATION if application else AgentExecutionKind.ADHOC,
        input_bundle_id="bundle", input_bundle_digest=b"b" * 32,
        request_entry_id="request", request_immutable_version="v1", request_content_digest=b"c" * 32,
    )
    # The shared wire accepts the snapshot; this Rust policy does not change SDK kwargs.
    assert request.payload.user_input == "current"


def _request(*, application: bool = True):
    message = _input(application=application)
    return request_from(
        message,
        kind=(
            AgentExecutionKind.APPLICATION
            if application
            else AgentExecutionKind.ADHOC
        ),
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        request_entry_id="agent-request",
        request_immutable_version="v1",
        request_content_digest=b"r" * 32,
    )


def _signed_agent_command(*, application: bool) -> bytes:
    capability_id = (
        AGENT_EXECUTE_APPLICATION_CAPABILITY_ID
        if application
        else AGENT_EXECUTE_ADHOC_CAPABILITY_ID
    )
    command_type = (
        command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION
        if application
        else command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
    )
    command = command_pb2.WorkerCommandV1(
        protocol_revision=PROTOCOL_REVISION,
        command_id="command-1",
        idempotency_key="outbox-1",
        command_type=command_type,
        execution_id="execution-1",
        generation=1,
        dispatch_ordinal=1,
        root_execution_id="execution-1",
        tenant_id="tenant-1",
        resource_project_id="7",
        projection_project_id="7",
        principal_ref="user:11",
        input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
            input_bundle_id="bundle-1",
            immutable_version="v1",
            digest=common_pb2.DigestV1(
                algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
                value=b"b" * 32,
            ),
            byte_length=123,
            media_type="application/x-protobuf",
        ),
        capability_id=capability_id,
        capability_version="1",
        resource_class="agent",
        isolation_class="shared-claim-scoped-authority",
        priority=1,
        deadline_unix_millis=1_700_000_000_000,
        limits_revision=LIMITS_REVISION,
        agent_execution=agent_pb2.AgentExecutionCommandV1(
            request_entry_id="agent-request",
            client_stream_id="conversation-1",
            client_message_id="message-1",
            sio_event="chat_predict",
        ),
    )
    command_bytes = command.SerializeToString(deterministic=True)
    return envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=(
            envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256
        ),
        key_id=CONFORMANCE_HMAC_KEY_ID,
        signature=hmac.new(
            CONFORMANCE_HMAC_KEY,
            command_bytes,
            hashlib.sha256,
        ).digest(),
        worker_command_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(command_bytes).digest(),
        ),
        worker_command_bytes=command_bytes,
    ).SerializeToString(deterministic=True)


def test_agent_input_is_canonical_and_strictly_typed() -> None:
    message = _input()
    raw = message.SerializeToString(deterministic=True)

    parsed = parse_agent_execution_input(raw)
    request = request_from(
        parsed,
        kind=AgentExecutionKind.APPLICATION,
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        request_entry_id="agent-request",
        request_immutable_version="v1",
        request_content_digest=b"r" * 32,
    )

    assert request.payload.application["id"] == 11
    assert request.payload.user_input == "current"
    assert request.payload.supports_vision is True

    with pytest.raises(InvalidInput, match="canonical"):
        parse_agent_execution_input(raw + b"\xa0\x06\x01")


def test_agent_input_preserves_bounded_next_input_suggestion_policy() -> None:
    message = _input()
    message.next_input_suggestion = _json(
        {
            "enabled": True,
            "min_response_chars": 150,
            "timeout_seconds": 15,
        }
    )

    request = request_from(
        message,
        kind=AgentExecutionKind.APPLICATION,
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        request_entry_id="agent-request",
        request_immutable_version="v1",
        request_content_digest=b"r" * 32,
    )

    assert request.payload.next_input_suggestion == {
        "enabled": True,
        "min_response_chars": 150,
        "timeout_seconds": 15,
    }


@pytest.mark.parametrize(
    "policy",
    [
        [],
        {"enabled": "yes"},
        {"enabled": True, "min_response_chars": 0},
        {"enabled": True, "timeout_seconds": 0},
        {"enabled": True, "unexpected": True},
    ],
)
def test_agent_input_rejects_invalid_next_input_suggestion_policy(policy) -> None:
    message = _input()
    message.next_input_suggestion = _json(policy)

    with pytest.raises(InvalidInput, match="next input suggestion"):
        request_from(
            message,
            kind=AgentExecutionKind.APPLICATION,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            request_entry_id="agent-request",
            request_immutable_version="v1",
            request_content_digest=b"r" * 32,
        )


def test_agent_input_rejects_wrong_semantic_shapes() -> None:
    message = _input(application=False)
    message.llm = _json({"kwargs": {}})

    with pytest.raises(InvalidInput, match="model"):
        request_from(
            message,
            kind=AgentExecutionKind.ADHOC,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            request_entry_id="agent-request",
            request_immutable_version="v1",
            request_content_digest=b"r" * 32,
        )


def test_agent_input_accepts_plural_hitl_resume_without_scalar_action() -> None:
    message = _input()
    message.hitl_resume = True
    message.should_continue = True
    message.hitl_decisions = _json(
        [
            {
                "interrupt_id": "hitl-name",
                "tool_call_id": "tool-name",
                "action": "approve",
                "value": "",
            },
            {
                "interrupt_id": "hitl-surname",
                "tool_call_id": "tool-surname",
                "action": "block_with_comment",
                "value": "Keep the surname artifact for review.",
            },
        ]
    )

    request = request_from(
        message,
        kind=AgentExecutionKind.APPLICATION,
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        request_entry_id="agent-request",
        request_immutable_version="v1",
        request_content_digest=b"r" * 32,
    )

    assert request.payload.hitl_action is None
    assert request.payload.hitl_value is None
    assert request.payload.hitl_decisions == [
        {
            "interrupt_id": "hitl-name",
            "tool_call_id": "tool-name",
            "action": "approve",
            "value": "",
        },
        {
            "interrupt_id": "hitl-surname",
            "tool_call_id": "tool-surname",
            "action": "block_with_comment",
            "value": "Keep the surname artifact for review.",
        },
    ]


def test_agent_input_rejects_hitl_resume_without_any_decision() -> None:
    message = _input()
    message.hitl_resume = True
    message.should_continue = True

    with pytest.raises(InvalidInput, match="HITL resume decision"):
        request_from(
            message,
            kind=AgentExecutionKind.APPLICATION,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            request_entry_id="agent-request",
            request_immutable_version="v1",
            request_content_digest=b"r" * 32,
        )


@pytest.mark.parametrize("application", [True, False])
def test_signed_agent_command_accepts_exact_current_entrypoint(application: bool) -> None:
    _, command = parse_and_verify_signed_command(
        _signed_agent_command(application=application),
        authenticator=TestOnlyConformanceHmacAuthenticator(),
    )

    assert command.agent_execution.request_entry_id == "agent-request"
    assert command.agent_execution.client_stream_id == "conversation-1"
    assert command.root_execution_id == command.execution_id
    assert command.WhichOneof("capability_command") == "agent_execution"


def test_signed_agent_command_rejects_capability_and_entrypoint_mismatch() -> None:
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1.FromString(
        _signed_agent_command(application=True)
    )
    command = command_pb2.WorkerCommandV1.FromString(signed.worker_command_bytes)
    command.command_type = command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
    command_bytes = command.SerializeToString(deterministic=True)
    signed.worker_command_bytes = command_bytes
    signed.worker_command_digest.value = hashlib.sha256(command_bytes).digest()
    signed.signature = hmac.new(
        CONFORMANCE_HMAC_KEY,
        command_bytes,
        hashlib.sha256,
    ).digest()

    with pytest.raises(UnsupportedCapability):
        parse_and_verify_signed_command(
            signed.SerializeToString(deterministic=True),
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )


class _Port:
    def __init__(self) -> None:
        self.application_calls = 0
        self.adhoc_calls = 0

    def execute_application(self, payload):
        self.application_calls += 1
        return {"result": payload.user_input}

    def execute_adhoc(self, payload):
        self.adhoc_calls += 1
        return {"paused": True, "pause_type": "mcp_auth"}


def test_handler_delegates_once_to_each_current_entrypoint() -> None:
    port = _Port()
    handler = AgentExecutionHandler(port)

    application = handler.execute(_request())
    adhoc = handler.execute(_request(application=False))

    assert application.sdk_result == {"result": "current"}
    assert adhoc.sdk_result["pause_type"] == "mcp_auth"
    assert (port.application_calls, port.adhoc_calls) == (1, 1)

    artifact = bind_result_artifact(
        adhoc,
        artifact_id="artifact-1",
        immutable_version="v1",
        byte_length=123,
        digest=hashlib.sha256(b"result").digest(),
    )
    assert (
        artifact.terminal_state
        == agent_pb2.AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH
    )
    assert artifact.request_entry_id == "agent-request"


class _Executor:
    def __init__(
        self,
        result: dict[str, Any],
        state_history: list[Any] | None = None,
    ) -> None:
        self.result = result
        self.calls: list[tuple[dict[str, Any], dict[str, Any]]] = []
        self.state_history = list(state_history or [])

    def invoke(self, value, config):
        self.calls.append((value, config))
        return self.result

    def get_state_history(self, config):
        self.state_history_config = config
        return iter(self.state_history)


class _AuthorizationPauseCallback:
    def authorization_pause_result(self):
        return {
            "thread_id": "thread-1",
            "error": "Toolkit authorization is required.",
            "paused": True,
            "pause_type": "mcp_auth",
        }


class _Client:
    def __init__(self) -> None:
        self.application_executor = _Executor({"mode": "application"})
        self.adhoc_executor = _Executor({"mode": "adhoc"})
        self.application_calls: list[dict[str, Any]] = []
        self.llm_calls: list[tuple[str, dict[str, Any]]] = []
        self.adhoc_calls: list[dict[str, Any]] = []

    def application(self, **kwargs):
        self.application_calls.append(kwargs)
        return self.application_executor

    def get_llm(self, *, model_name, model_config):
        self.llm_calls.append((model_name, model_config))
        return SimpleNamespace(model=model_name)

    def predict_agent(self, **kwargs):
        self.adhoc_calls.append(kwargs)
        return self.adhoc_executor


class _SuggestionLLM:
    def __init__(
        self,
        result: Any = None,
        *,
        error: Exception | None = None,
        wait: bool = False,
    ) -> None:
        self.result = result
        self.error = error
        self.wait = wait
        self.calls: list[str] = []

    def invoke(self, prompt: str):
        self.calls.append(prompt)
        if self.wait:
            threading.Event().wait(1)
        if self.error is not None:
            raise self.error
        return self.result


class _SuggestionClient(_Client):
    def __init__(self, llm: Any) -> None:
        super().__init__()
        self.low_tier_llm = llm
        self.low_tier_calls: list[int] = []

    def get_low_tier_llm(self, *, max_tokens: int):
        self.low_tier_calls.append(max_tokens)
        if isinstance(self.low_tier_llm, Exception):
            raise self.low_tier_llm
        return self.low_tier_llm


def _adapter(client: _Client) -> EliteaSdkAgentAdapter:
    adapter = object.__new__(EliteaSdkAgentAdapter)
    adapter._client = client  # type: ignore[attr-defined]
    adapter._memory = "checkpoint-store"  # type: ignore[attr-defined]
    adapter._callbacks = ["current-callback"]  # type: ignore[attr-defined]
    return adapter


class _BudgetBlockedExecutor:
    """An SDK executor whose first invoke hits an exhausted budget."""

    def __init__(self, scope: str, message: str) -> None:
        self.scope = scope
        self.message = message

    def invoke(self, value, config):
        raise BudgetExceededError(self.message, self.scope)


class _BudgetBlockedClient(_Client):
    def __init__(self, scope: str, message: str) -> None:
        super().__init__()
        self.application_executor = _BudgetBlockedExecutor(scope, message)
        self.adhoc_executor = _BudgetBlockedExecutor(scope, message)


@pytest.mark.parametrize(
    "scope",
    ["project_budget_exceeded", "member_budget_exceeded"],
)
@pytest.mark.parametrize("application", [True, False])
def test_agent_execution_reports_a_budget_rejection_as_a_policy_outcome(
    scope: str, application: bool
) -> None:
    """A budget rejection must leave agent execution as the typed marker.

    Before the agent adapter had a budget boundary, only the INDEX path
    converted the SDK's typed rejection. Agent execution let it reach the
    delivery catch-all unclassified, and reported it as an internal fault —
    a retryable answer to a condition no retry can clear.

    Both scopes are covered because the two travel different routes out of the
    gateway, and only one of them used to arrive as a typed SDK exception at
    all.
    """

    canary = f"BUDGET_SECRET_CANARY_{scope}"
    adapter = _adapter(_BudgetBlockedClient(scope, canary))
    payload = _request(application=application).payload

    with pytest.raises(SdkBudgetExceeded) as caught:
        if application:
            adapter.execute_application(payload)
        else:
            adapter.execute_adhoc(payload)

    # The marker is data-free: the SDK message can quote the proxy, and the
    # worker's public diagnostics must not carry it.
    assert str(caught.value) == ""
    assert canary not in str(caught.value)


def test_sdk_adapter_preserves_constructor_split_without_forwarding_authority() -> None:
    client = _Client()
    adapter = _adapter(client)

    assert adapter.execute_application(_request().payload) == {"mode": "application"}
    assert adapter.execute_adhoc(_request(application=False).payload) == {
        "mode": "adhoc"
    }

    assert client.application_calls[0]["application_id"] == 11
    assert client.application_calls[0]["application_version_id"] == 22
    assert client.application_calls[0]["memory"] == "checkpoint-store"
    assert client.application_calls[0]["tools"] is None
    assert client.application_executor.calls[0][1]["recursion_limit"] == 17
    assert client.application_executor.calls[0][1]["configurable"] == {
        "thread_id": "thread-1",
        "invoked_skills": [],
        "attached_skills": [],
    }
    assert client.application_executor.calls[0][1]["callbacks"] == [
        "current-callback"
    ]
    assert client.llm_calls[0][0] == "gpt-test"
    assert "api_key" not in client.llm_calls[0][1]
    assert "base_url" not in client.llm_calls[0][1]
    assert client.adhoc_calls[0]["instructions"] == "Be concise"
    assert client.adhoc_calls[0]["memory"] == "checkpoint-store"
    assert client.adhoc_calls[0]["chat_history"] == [
        {"role": "user", "content": "earlier"}
    ]
    assert len(client.application_executor.calls[0][0]["messages"]) == 2


def test_sdk_adapter_preserves_saved_mcp_configuration_at_each_current_constructor() -> None:
    client = _Client()
    adapter = _adapter(client)
    mcp = {
        "id": 52,
        "type": "mcp",
        "name": "documentation-mcp",
        "toolkit_name": "documentation-mcp",
        "settings": {
            "url": "https://mcp.example.invalid/events",
            "selected_tools": ["search_docs"],
        },
        "meta": {"mcp": True},
    }

    application_payload = _request().payload
    application_payload.application["version_details"]["tools"] = [mcp]
    assert adapter.execute_application(application_payload) == {"mode": "application"}

    adhoc_payload = _request(application=False).payload
    object.__setattr__(adhoc_payload, "tools", [mcp])
    assert adapter.execute_adhoc(adhoc_payload) == {"mode": "adhoc"}

    assert client.application_calls[0]["version_details"]["tools"] == [mcp]
    assert client.adhoc_calls[0]["tools"] == [mcp]


def test_sdk_adapter_passes_current_runtime_skills_only_through_configurable() -> None:
    client = _Client()
    payload = _request().payload
    invoked = [
        {
            "skill_id": 7,
            "skill_version_id": 8,
            "name": "Review",
            "version_name": "base",
            "icon_meta": {"icon": "review"},
            "instructions": "Review carefully.",
        }
    ]
    attached = [
        {
            "skill_id": 9,
            "name": "Deploy",
            "description": "Deployment rules",
            "icon_meta": {"icon": "deploy"},
            "instructions": "Deploy safely.",
        }
    ]
    object.__setattr__(payload, "invoked_skills", invoked)
    object.__setattr__(payload, "applied_skills", [{"skill_id": 7, "name": "Review"}])
    object.__setattr__(payload, "attached_skills", attached)

    assert _adapter(client).execute_application(payload) == {"mode": "application"}

    configurable = client.application_executor.calls[0][1]["configurable"]
    assert configurable == {
        "thread_id": "thread-1",
        "invoked_skills": invoked,
        "attached_skills": attached,
    }
    assert client.application_calls[0]["version_details"] == {
        "meta": {"step_limit": 17}
    }


def test_sdk_adapter_generates_one_current_next_input_suggestion() -> None:
    llm = _SuggestionLLM(SimpleNamespace(content="  Yes, add the test.  "))
    client = _SuggestionClient(llm)
    payload = _request().payload
    object.__setattr__(
        payload,
        "next_input_suggestion",
        {"enabled": True, "min_response_chars": 5, "timeout_seconds": 1},
    )

    suggestion = _adapter(client).suggest_next_input(
        payload,
        output_text="The change is ready. Would you like a test?",
    )

    assert suggestion == "Yes, add the test."
    assert client.low_tier_calls == [64]
    assert len(llm.calls) == 1
    assert "The change is ready" in llm.calls[0]


@pytest.mark.parametrize(
    ("policy", "output", "llm", "expected_llm_calls"),
    [
        (
            {"enabled": False, "min_response_chars": 5, "timeout_seconds": 1},
            "long enough",
            _SuggestionLLM("unused"),
            0,
        ),
        (
            {"enabled": True, "min_response_chars": 50, "timeout_seconds": 1},
            "short",
            _SuggestionLLM("unused"),
            0,
        ),
        (
            {"enabled": True, "min_response_chars": 5, "timeout_seconds": 1},
            "long enough",
            None,
            1,
        ),
        (
            {"enabled": True, "min_response_chars": 5, "timeout_seconds": 1},
            "long enough",
            _SuggestionLLM("NONE"),
            1,
        ),
        (
            {"enabled": True, "min_response_chars": 5, "timeout_seconds": 1},
            "long enough",
            _SuggestionLLM(error=RuntimeError("model failed")),
            1,
        ),
        (
            {"enabled": True, "min_response_chars": 5, "timeout_seconds": 0.01},
            "long enough",
            _SuggestionLLM(wait=True),
            1,
        ),
    ],
)
def test_sdk_adapter_suppresses_optional_suggestion_failures(
    policy,
    output,
    llm,
    expected_llm_calls,
) -> None:
    client = _SuggestionClient(llm)
    payload = _request().payload
    object.__setattr__(payload, "next_input_suggestion", policy)

    assert _adapter(client).suggest_next_input(payload, output_text=output) is None
    assert len(client.low_tier_calls) == expected_llm_calls


def test_sdk_adapter_suppresses_low_tier_model_lookup_failure() -> None:
    client = _SuggestionClient(RuntimeError("configuration unavailable"))
    payload = _request().payload
    object.__setattr__(
        payload,
        "next_input_suggestion",
        {"enabled": True, "min_response_chars": 5, "timeout_seconds": 1},
    )

    assert (
        _adapter(client).suggest_next_input(payload, output_text="long enough")
        is None
    )
    assert client.low_tier_calls == [64]


def test_sdk_adapter_prefers_callback_authorization_pause_over_graph_result() -> None:
    client = _Client()
    adapter = _adapter(client)
    adapter._callbacks = [_AuthorizationPauseCallback()]  # type: ignore[attr-defined]

    assert adapter.execute_adhoc(_request(application=False).payload) == {
        "thread_id": "thread-1",
        "error": "Toolkit authorization is required.",
        "paused": True,
        "pause_type": "mcp_auth",
    }
    assert len(client.adhoc_executor.calls) == 1


def test_sdk_adapter_delegates_pipeline_yaml_through_the_existing_application_api() -> None:
    client = _Client()
    adapter = _adapter(client)
    payload = _request().payload
    pipeline_yaml = (
        "nodes:\n"
        "  - id: draft\n"
        "    type: llm\n"
        "  - id: approval\n"
        "    type: hitl\n"
        "edges:\n"
        "  - from: draft\n"
        "    to: approval\n"
    )
    payload.application["version_details"] = {
        "id": 22,
        "application_id": 11,
        "agent_type": "pipeline",
        "instructions": pipeline_yaml,
        "llm_settings": {"model_name": "gpt-test"},
        "meta": {"step_limit": 17},
        "tools": [],
    }

    assert adapter.execute_application(payload) == {"mode": "application"}

    assert len(client.application_calls) == 1
    version_details = client.application_calls[0]["version_details"]
    assert version_details["agent_type"] == "pipeline"
    assert version_details["instructions"] == pipeline_yaml
    assert client.application_executor.calls[0][1]["recursion_limit"] == 17


def test_sdk_adapter_rejects_an_unrecoverable_random_thread() -> None:
    request = _request()
    object.__setattr__(request.payload, "thread_id", None)
    object.__setattr__(request.payload, "conversation_id", None)

    with pytest.raises(UnsupportedCapability, match="durable agent thread"):
        _adapter(_Client()).execute_application(request.payload)


def test_sdk_adapter_submits_projected_history_on_one_checkpoint_thread() -> None:
    client = _Client()
    adapter = _adapter(client)
    first = _request().payload
    second = _request().payload
    object.__setattr__(first, "chat_history", [])
    object.__setattr__(
        second,
        "chat_history",
        [
            {"role": "user", "content": "first turn"},
            {"role": "assistant", "content": "first response"},
        ],
    )
    object.__setattr__(first, "thread_id", "conversation-1")
    object.__setattr__(second, "thread_id", "conversation-1")
    object.__setattr__(first, "conversation_id", "conversation-1")
    object.__setattr__(second, "conversation_id", "conversation-1")
    object.__setattr__(first, "user_input", "first turn")
    object.__setattr__(second, "user_input", "second turn")

    adapter.execute_application(first)
    adapter.execute_application(second)

    assert len(client.application_executor.calls) == 2
    first_input, first_config = client.application_executor.calls[0]
    second_input, second_config = client.application_executor.calls[1]
    assert first_config["configurable"]["thread_id"] == "conversation-1"
    assert second_config["configurable"]["thread_id"] == "conversation-1"
    assert [message.content for message in first_input["messages"]] == ["first turn"]
    assert [
        message.get("content") if isinstance(message, dict) else message.content
        for message in second_input["messages"]
    ] == ["first turn", "first response", "second turn"]


class _CheckpointMemory:
    def __init__(self, pending_writes, *, thread_writes=None) -> None:
        self.pending_writes = pending_writes
        self.thread_writes = dict(thread_writes or {})
        self.deleted_threads: list[str] = []

    def get_tuple(self, config):
        thread_id = config["configurable"]["thread_id"]
        pending_writes = self.thread_writes.get(thread_id, self.pending_writes)
        return SimpleNamespace(pending_writes=pending_writes)

    def delete_thread(self, thread_id: str) -> None:
        self.deleted_threads.append(thread_id)


def test_sdk_adapter_repairs_only_an_explicit_failed_checkpoint() -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [("failed-task", "__error__", RuntimeError("redacted"))]
    )
    adapter._memory = memory  # type: ignore[attr-defined]

    adapter.execute_adhoc(_request(application=False).payload)

    assert memory.deleted_threads == ["thread-1"]
    assert len(client.adhoc_executor.calls) == 1


@pytest.mark.parametrize("application", [True, False])
def test_sdk_adapter_repairs_explicit_failed_direct_application_checkpoint(
    application: bool,
) -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [],
        thread_writes={
            "thread-1:release-notes": [
                ("failed-child-task", "__error__", RuntimeError("redacted"))
            ],
        },
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=application).payload
    child = {
        "type": "application",
        "name": "release-notes",
        "toolkit_name": "release-notes",
    }
    if application:
        payload.application["version_details"]["tools"] = [child]
    else:
        payload.tools.append(child)

    if application:
        adapter.execute_application(payload)
    else:
        adapter.execute_adhoc(payload)

    assert memory.deleted_threads == ["thread-1:release-notes"]


def test_sdk_adapter_preserves_direct_application_interrupt_checkpoint() -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [],
        thread_writes={
            "thread-1:release-notes": [
                ("paused-child-task", "__interrupt__", {"type": "hitl"})
            ],
        },
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=False).payload
    payload.tools.append(
        {
            "type": "application",
            "name": "release-notes",
            "toolkit_name": "release-notes",
        }
    )

    adapter.execute_adhoc(payload)

    assert memory.deleted_threads == []


def test_sdk_adapter_regeneration_resets_the_exact_thread_before_invoke() -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory([])
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=False).payload
    object.__setattr__(payload, "is_regenerate", True)

    adapter.execute_adhoc(payload)

    assert memory.deleted_threads == ["thread-1"]
    assert len(client.adhoc_executor.calls) == 1


@pytest.mark.parametrize(
    "pending_writes",
    [
        [],
        [("paused-task", "__interrupt__", {"type": "hitl"})],
        [("paused-task", "messages", [])],
    ],
)
def test_sdk_adapter_preserves_clean_pause_checkpoints(pending_writes) -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(pending_writes)
    adapter._memory = memory  # type: ignore[attr-defined]

    adapter.execute_adhoc(_request(application=False).payload)

    assert memory.deleted_threads == []
    assert len(client.adhoc_executor.calls) == 1


def test_sdk_adapter_rejects_unimplemented_generic_continue_instead_of_drifting() -> None:
    request = _request()
    object.__setattr__(request.payload, "should_continue", True)

    with pytest.raises(UnsupportedCapability, match="parity path"):
        _adapter(_Client()).execute_application(request.payload)


@pytest.mark.parametrize("application", [True, False])
def test_sdk_adapter_resumes_declined_toolkit_authorization_from_paused_checkpoint(
    application: bool,
) -> None:
    client = _Client()
    executor = client.application_executor if application else client.adhoc_executor
    executor.state_history = [
        SimpleNamespace(
            next=("agent",),
            config={"configurable": {"checkpoint_id": "checkpoint-auth-1"}},
        )
    ]
    payload = _request(application=application).payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "ignored_mcp_servers", ["https://sharepoint.example"])
    object.__setattr__(
        payload,
        "user_declined_mcp_servers",
        [
            {
                "server_url": "https://sharepoint.example",
                "tool_name": "list_items",
                "toolkit_type": "sharepoint",
                "skip_reason": "User skipped toolkit login for this run.",
            }
        ],
    )

    adapter = _adapter(client)
    if application:
        adapter.execute_application(payload)
    else:
        adapter.execute_adhoc(payload)

    invoke_input, invoke_config = executor.calls[0]
    assert "declined toolkit authorization" in invoke_input["input"]
    assert "current" in invoke_input["input"]
    assert invoke_config["configurable"] == {
        "thread_id": "thread-1",
        "checkpoint_id": "checkpoint-auth-1",
        "invoked_skills": [],
        "attached_skills": [],
    }
    assert invoke_config["should_continue"] is True


def test_sdk_adapter_resumes_completed_toolkit_authorization_from_paused_checkpoint() -> None:
    client = _Client()
    client.adhoc_executor.state_history = [
        SimpleNamespace(
            next=("agent",),
            config={"configurable": {"checkpoint_id": "checkpoint-auth-2"}},
        )
    ]
    payload = _request(application=False).payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(
        payload,
        "mcp_tokens",
        {"sharepoint": {"access_token": "claim-scoped-test-token"}},
    )

    _adapter(client).execute_adhoc(payload)

    invoke_input, invoke_config = client.adhoc_executor.calls[0]
    assert "authorization has been completed" in invoke_input["input"]
    assert invoke_config["configurable"]["checkpoint_id"] == "checkpoint-auth-2"


def test_sdk_adapter_does_not_resume_an_older_authorization_pause() -> None:
    client = _Client()
    client.adhoc_executor.state_history = [
        SimpleNamespace(next=(), config={"configurable": {"checkpoint_id": "latest"}}),
        SimpleNamespace(
            next=("agent",),
            config={"configurable": {"checkpoint_id": "stale-auth-pause"}},
        ),
    ]
    payload = _request(application=False).payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(
        payload,
        "user_declined_mcp_servers",
        [{"server_url": "https://sharepoint.example"}],
    )

    _adapter(client).execute_adhoc(payload)

    invoke_input, invoke_config = client.adhoc_executor.calls[0]
    assert "messages" in invoke_input
    assert "checkpoint_id" not in invoke_config["configurable"]
    assert "should_continue" not in invoke_config


@pytest.mark.parametrize("application", [True, False])
@pytest.mark.parametrize(
    ("action", "value"),
    [
        ("reject", ""),
        (
            "block_with_comment",
            "append the requested data before retrying the sensitive action",
        ),
    ],
)
def test_sdk_adapter_resumes_one_exact_hitl_without_deleting_checkpoint(
    application: bool,
    action: str,
    value: str,
) -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [("paused-task", "__interrupt__", {"type": "hitl"})]
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=application).payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", action)
    object.__setattr__(payload, "hitl_value", value)
    object.__setattr__(
        payload,
        "hitl_decisions",
        [
            {
                "interrupt_id": "interrupt-1",
                "action": action,
                "value": value,
            }
        ],
    )

    if application:
        adapter.execute_application(payload)
        invoke_input, invoke_config = client.application_executor.calls[0]
    else:
        adapter.execute_adhoc(payload)
        invoke_input, invoke_config = client.adhoc_executor.calls[0]

    assert memory.deleted_threads == []
    assert invoke_config["configurable"]["thread_id"] == "thread-1"
    assert invoke_input["hitl_resume"] is True
    assert invoke_input["hitl_action"] == action
    assert invoke_input["hitl_value"] == value
    assert invoke_input["hitl_decisions"] == [
        {
            "interrupt_id": "interrupt-1",
            "action": action,
            "value": value,
        }
    ]


@pytest.mark.parametrize("application", [True, False])
def test_sdk_adapter_resumes_a_clarification_with_its_decoded_answer(
    application: bool,
) -> None:
    """The `ask_user` answer, admitted and handed over as a MAPPING.

    Measured before this: `_require_in_process_hitl_resume` admitted
    {approve, reject, edit, block_with_comment} for a decision with no
    `guardrail_type`, and elitea-main omits that field for every root pause
    (`json:"guardrail_type,omitempty"`), so an answered clarification came back
    as `{"code":"UNSUPPORTED_CAPABILITY","safe_message":"The HITL action is not
    supported."}`. The SDK's own resume handler for it
    (`langraph_agent.py`, `guardrail_type == 'clarifying_question'`) was never
    reached, the run never finished, and the conversation was left holding a
    stale interrupt that refused every later turn.

    The value is asserted DECODED. The SDK's `_format_answer` maps answers onto
    the questions it asked only when it receives a mapping; handed the encoded
    string it renders the wire format into the model's tool result instead.
    """

    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [("paused-task", "__interrupt__", {"type": "hitl"})]
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    answer = '{"environment": "Staging", "regions": ["eu", "us"]}'
    payload = _request(application=application).payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "answer")
    object.__setattr__(payload, "hitl_value", answer)
    object.__setattr__(
        payload,
        "hitl_decisions",
        [{"interrupt_id": "interrupt-1", "action": "answer", "value": answer}],
    )

    if application:
        adapter.execute_application(payload)
        invoke_input, _ = client.application_executor.calls[0]
    else:
        adapter.execute_adhoc(payload)
        invoke_input, _ = client.adhoc_executor.calls[0]

    assert memory.deleted_threads == []
    assert invoke_input["hitl_resume"] is True
    assert invoke_input["hitl_action"] == "answer"
    assert invoke_input["hitl_value"] == {
        "environment": "Staging",
        "regions": ["eu", "us"],
    }
    # The decision list is the audit record of what the user sent and stays
    # byte-identical to it.
    assert invoke_input["hitl_decisions"] == [
        {"interrupt_id": "interrupt-1", "action": "answer", "value": answer}
    ]


def test_sdk_adapter_rejects_a_clarification_answer_with_no_answer() -> None:
    """An empty answer would resume the run with "User did not provide an answer."."""

    payload = _request().payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "answer")
    object.__setattr__(payload, "hitl_value", "")
    object.__setattr__(
        payload,
        "hitl_decisions",
        [{"interrupt_id": "interrupt-1", "action": "answer", "value": ""}],
    )

    with pytest.raises(UnsupportedCapability, match="value is required"):
        _adapter(_Client()).execute_application(payload)


def test_sdk_adapter_still_rejects_an_action_outside_the_root_set() -> None:
    """Admitting `answer` must not admit whatever else a caller invents."""

    payload = _request().payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "answer_all")
    object.__setattr__(payload, "hitl_value", "yes")
    object.__setattr__(
        payload,
        "hitl_decisions",
        [{"interrupt_id": "interrupt-1", "action": "answer_all", "value": "yes"}],
    )

    with pytest.raises(UnsupportedCapability, match="action is not supported"):
        _adapter(_Client()).execute_application(payload)


@pytest.mark.parametrize("application", [True, False])
@pytest.mark.parametrize("action", ["authorize", "skip"])
def test_sdk_adapter_forwards_normalized_delegated_authorization_decision(
    application: bool,
    action: str,
) -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [("paused-task", "__interrupt__", {"type": "hitl"})]
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=application).payload
    decision = {
        "interrupt_id": "mcp_auth_sharepoint_1",
        "tool_call_id": "call-sharepoint-search-1",
        "guardrail_type": "mcp_auth",
        "action": action,
        "value": "",
    }
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", action)
    object.__setattr__(payload, "hitl_value", "")
    object.__setattr__(payload, "hitl_decisions", [decision])
    if action == "authorize":
        object.__setattr__(
            payload,
            "mcp_tokens",
            {"https://sharepoint.example.test": {"access_token": "runtime-secret"}},
        )
    else:
        object.__setattr__(
            payload,
            "user_declined_mcp_servers",
            [{"server_url": "https://sharepoint.example.test"}],
        )

    if application:
        adapter.execute_application(payload)
        invoke_input, invoke_config = client.application_executor.calls[0]
    else:
        adapter.execute_adhoc(payload)
        invoke_input, invoke_config = client.adhoc_executor.calls[0]

    assert memory.deleted_threads == []
    assert invoke_config["configurable"]["thread_id"] == "thread-1"
    assert invoke_input["hitl_resume"] is True
    assert invoke_input["hitl_action"] == action
    assert invoke_input["hitl_value"] == ""
    assert invoke_input["hitl_decisions"] == [decision]


def test_sdk_adapter_rejects_hitl_without_exact_interrupt_identity() -> None:
    payload = _request().payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "approve")
    object.__setattr__(payload, "hitl_decisions", [])

    with pytest.raises(UnsupportedCapability, match="Between one and sixteen"):
        _adapter(_Client()).execute_application(payload)


def test_sdk_adapter_rejects_private_hitl_route_from_transport() -> None:
    payload = _request().payload
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "approve")
    object.__setattr__(
        payload,
        "hitl_decisions",
        [
            {
                "interrupt_id": "interrupt-1",
                "action": "approve",
                "child_thread_id": "child-thread",
            }
        ],
    )

    with pytest.raises(UnsupportedCapability, match="decision is malformed"):
        _adapter(_Client()).execute_application(payload)


@pytest.mark.parametrize("application", [True, False])
def test_sdk_adapter_resumes_one_atomic_parallel_hitl_decision_set(
    application: bool,
) -> None:
    client = _Client()
    adapter = _adapter(client)
    memory = _CheckpointMemory(
        [("paused-task", "__interrupt__", {"type": "hitl"})]
    )
    adapter._memory = memory  # type: ignore[attr-defined]
    payload = _request(application=application).payload
    decisions = [
        {"interrupt_id": "interrupt-1", "action": "approve"},
        {
            "interrupt_id": "interrupt-2",
            "tool_call_id": "tool-call-2",
            "action": "block_with_comment",
            "value": "archive first",
        },
    ]
    object.__setattr__(payload, "should_continue", True)
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", None)
    object.__setattr__(payload, "hitl_value", None)
    object.__setattr__(payload, "hitl_decisions", decisions)

    if application:
        adapter.execute_application(payload)
        invoke_input, invoke_config = client.application_executor.calls[0]
    else:
        adapter.execute_adhoc(payload)
        invoke_input, invoke_config = client.adhoc_executor.calls[0]

    assert memory.deleted_threads == []
    assert invoke_config["configurable"]["thread_id"] == "thread-1"
    assert invoke_input["hitl_resume"] is True
    assert invoke_input["hitl_action"] is None
    # The current SDK's scalar compatibility field remains a string even when
    # the authoritative resume is the plural decision set.
    assert invoke_input["hitl_value"] == ""
    assert invoke_input["hitl_decisions"] == decisions


def test_sdk_adapter_rejects_hitl_without_continuation_marker() -> None:
    payload = _request().payload
    object.__setattr__(payload, "hitl_resume", True)
    object.__setattr__(payload, "hitl_action", "approve")
    object.__setattr__(
        payload,
        "hitl_decisions",
        [{"interrupt_id": "interrupt-1", "action": "approve"}],
    )

    with pytest.raises(UnsupportedCapability, match="continuation marker"):
        _adapter(_Client()).execute_application(payload)


@pytest.mark.parametrize("application", [True, False])
@pytest.mark.parametrize("activation", ["", "When reviewing code"])
def test_project_context_snapshot_reaches_existing_sdk_boundary(application, activation, monkeypatch):
    from dataclasses import replace
    from elitea_worker.agents import sdk_adapter

    # This test checks constructor propagation; question-tool loading is independent.
    monkeypatch.setattr(sdk_adapter, "_install_ask_user_question_ids", lambda: None)
    snapshot = {
        "id": "project-context:7:9", "scope": "project:7",
        "content": " Project rules \n", "activation_description": activation,
    }
    snapshot["revision"] = hashlib.sha256(snapshot["content"].encode()).hexdigest()
    payload = replace(_request(application=application).payload, project_context=snapshot)
    client = _Client()
    adapter = _adapter(client)
    if application:
        payload.application["version_details"]["instructions"] = "Agent rules"
        adapter.execute_application(payload)
        call = client.application_calls[0]
        instructions = call["version_details"]["instructions"]
    else:
        adapter.execute_adhoc(payload)
        call = client.adhoc_calls[0]
        instructions = call["instructions"]
    if activation:
        assert call["project_context"] == snapshot
        assert call["project_context"] is not snapshot
        assert "Project rules" not in instructions
    else:
        assert instructions.startswith("# Project Context\n\n Project rules \n\n\n---\n\n")
        assert "project_context" not in call
    assert payload.project_context == snapshot


def test_project_context_wire_snapshot_rejects_revision_mismatch():
    message = _input()
    message.project_context.CopyFrom(agent_pb2.ProjectContextSnapshotV1(
        id="project-context:7:9", scope="project:7", content="Rules",
        revision=hashlib.sha256(b"Rules").hexdigest(), activation_description="When needed",
    ))
    request = request_from(
        message, kind=AgentExecutionKind.APPLICATION,
        input_bundle_id="bundle", input_bundle_digest=b"b" * 32,
        request_entry_id="request", request_immutable_version="v1", request_content_digest=b"c" * 32,
    )
    assert request.payload.project_context["content"] == "Rules"
    message.project_context.content = "Changed"
    with pytest.raises(InvalidInput):
        request_from(
            message, kind=AgentExecutionKind.APPLICATION,
            input_bundle_id="bundle", input_bundle_digest=b"b" * 32,
            request_entry_id="request", request_immutable_version="v1", request_content_digest=b"c" * 32,
        )
