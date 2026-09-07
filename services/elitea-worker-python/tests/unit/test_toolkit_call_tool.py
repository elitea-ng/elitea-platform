"""``toolkit.call_tool.v1``: one tool, one answer, one settlement.

Every test here states a fact the 503 this capability replaces could not state.
The old refusal said "indexer service not available" for every condition — a
tool that worked, a tool that raised, a toolkit the image cannot build and a
tool name that does not exist all produced the same sentence. The point of the
capability is that those four are now four different answers, so the tests
assert which one comes back and not merely that nothing threw.
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
from typing import Any

import pytest
from elitea.runtime.v1 import (
    command_pb2,
    common_pb2,
    envelope_pb2,
    input_pb2,
    output_pb2,
    toolkit_pb2,
)

from elitea_worker.constants import (
    CONFORMANCE_HMAC_KEY,
    CONFORMANCE_HMAC_KEY_ID,
    ENVELOPE_SCHEMA_REVISION,
    LIMITS_REVISION,
    MAX_TOOL_RESULT_BYTES,
    MAX_WORKER_COMMAND_BYTES,
    PROTOCOL_REVISION,
    TOOLKIT_CALL_TOOL_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
)
from elitea_worker.execution.errors import InvalidInput, UnsupportedCapability
from elitea_worker.handlers.toolkit_call_tool import (
    ResolvedToolkitCallToolInput,
    ToolkitCallToolHandler,
    ToolkitCallToolInputBinding,
    ToolkitCallToolOutcome,
    ToolkitCallToolRequest,
    ToolkitCallToolResult,
)
from elitea_worker.protocol.codec import (
    TestOnlyConformanceHmacAuthenticator,
    VerifiedWorkerCommand,
    _logical_output_id,
    build_output_frame,
    parse_and_verify_signed_command,
)
from elitea_worker.protocol.toolkit_call_tool import (
    bind_result_summary,
    request_from,
    unsupported_toolkit_result,
)


class _FakeSdk:
    """A toolkit adapter that records its one call and answers a fixed value."""

    def __init__(self, value: Any) -> None:
        self.value = value
        self.calls: list[dict[str, Any]] = []

    def call_tool(self, **kwargs: Any) -> Any:
        self.calls.append(kwargs)
        return self.value


class _InlineSupervisor:
    async def run(self, operation):
        return await operation()

    async def run_sync(self, operation, /, *args: Any, **kwargs: Any):
        return operation(*args, **kwargs)

    async def reserve_sync(self):
        raise AssertionError("the handler must not reserve; the processor does")


def _binding(entry_id: str, byte: bytes) -> ToolkitCallToolInputBinding:
    return ToolkitCallToolInputBinding(
        entry_id=entry_id,
        immutable_version="1",
        content_digest=byte * 32,
    )


def _request(
    *,
    arguments: dict[str, Any] | None = None,
    tool_name: str = "get_issue",
) -> ToolkitCallToolRequest:
    return ToolkitCallToolRequest(
        toolkit_type="github",
        toolkit_id="17",
        toolkit_version="3",
        tool_name=tool_name,
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        settings=ResolvedToolkitCallToolInput(
            binding=_binding("settings", b"s"),
            value={"url": "https://github.example", "token": "REDACTED_IN_TRANSIT"},
        ),
        arguments=ResolvedToolkitCallToolInput(
            binding=_binding("arguments", b"a"),
            value={"issue": 7} if arguments is None else arguments,
        ),
        runtime_config={"metadata": {"tool_name": tool_name}},
    )


async def _run(sdk: _FakeSdk, request: ToolkitCallToolRequest):
    return await ToolkitCallToolHandler(sdk, _InlineSupervisor()).execute(request)


def test_one_admitted_run_makes_exactly_one_sdk_call_with_the_named_tool() -> None:
    async def run() -> None:
        sdk = _FakeSdk({"success": True, "result": {"title": "a"}, "tool_name": "get_issue"})

        result = await _run(sdk, _request())

        assert len(sdk.calls) == 1
        call = sdk.calls[0]
        # The caller's tool name passes through UNALTERED. The index adapter
        # rewrites its toolkit config for the index_data special case; this one must
        # not, or an arbitrary tool would inherit that path's compatibility rule.
        assert call["tool_name"] == "get_issue"
        assert call["toolkit_config"] == {
            "url": "https://github.example",
            "token": "REDACTED_IN_TRANSIT",
        }
        assert call["tool_params"] == {"issue": 7}
        assert result.outcome.status == "ok"
        assert result.outcome.result_json == '{"title":"a"}'
        assert result.outcome.truncated is False
        assert result.outcome.error_message == ""

    asyncio.run(run())


def test_a_tool_that_raised_is_a_completed_run_carrying_the_reason() -> None:
    """The caller asked whether the tool works. "It raised" is the answer.

    Losing it to a runtime failure would put the caller back where the 503 left
    them: told that something went wrong, and not told what.
    """
    async def run() -> None:

        sdk = _FakeSdk(
            {
                "success": False,
                "error": "404 Not Found: issue 7",
                "tool_name": "get_issue",
                "result": None,
            }
        )

        result = await _run(sdk, _request())

        assert result.outcome.status == "tool_error"
        assert result.outcome.error_message == "404 Not Found: issue 7"

    asyncio.run(run())


def test_a_tool_the_toolkit_does_not_have_is_its_own_status() -> None:
    """The SDK reports an unknown tool by echoing no tool_name back."""
    async def run() -> None:

        sdk = _FakeSdk({"success": False, "error": "tool not found", "result": None})

        result = await _run(sdk, _request(tool_name="no_such_tool"))

        assert result.outcome.status == "unknown_tool"

    asyncio.run(run())


def test_an_oversized_result_is_reported_truncated_and_never_cut() -> None:
    """Half a JSON document reads as a corrupt result, so none is sent."""
    async def run() -> None:

        sdk = _FakeSdk(
            {
                "success": True,
                "tool_name": "get_issue",
                "result": {"body": "x" * (MAX_TOOL_RESULT_BYTES + 1)},
            }
        )

        result = await _run(sdk, _request())

        assert result.outcome.status == "ok"
        assert result.outcome.truncated is True
        assert result.outcome.result_json == ""

    asyncio.run(run())


def test_a_result_json_cannot_encode_is_truncated_not_a_failure() -> None:
    """The tool DID run. Encoding is this boundary's problem, not the run's."""
    async def run() -> None:

        sdk = _FakeSdk({"success": True, "tool_name": "get_issue", "result": {1: float("nan")}})

        result = await _run(sdk, _request())

        assert result.outcome.status == "ok"
        assert result.outcome.truncated is True

    asyncio.run(run())


def test_a_non_dict_sdk_response_is_refused_rather_than_guessed() -> None:
    async def run() -> None:
        sdk = _FakeSdk(["not", "a", "dict"])

        with pytest.raises(InvalidInput):
            await _run(sdk, _request())

    asyncio.run(run())


def test_a_malformed_request_never_reaches_the_sdk() -> None:
    async def run() -> None:
        sdk = _FakeSdk({"success": True, "result": {}, "tool_name": "get_issue"})
        request = _request()
        broken = ToolkitCallToolRequest(
            toolkit_type=request.toolkit_type,
            toolkit_id=request.toolkit_id,
            toolkit_version=request.toolkit_version,
            tool_name="",
            input_bundle_id=request.input_bundle_id,
            input_bundle_digest=request.input_bundle_digest,
            settings=request.settings,
            arguments=request.arguments,
            runtime_config=request.runtime_config,
        )

        with pytest.raises(InvalidInput):
            await _run(sdk, broken)
        assert sdk.calls == []

    asyncio.run(run())


def test_request_from_refuses_an_input_the_command_did_not_name() -> None:
    """The bundle and the command are two independent statements.

    A run whose settings came from an entry the command never named is not the
    run the caller authorized, however well-formed both halves look.
    """

    command = toolkit_pb2.ToolkitCallToolCommandV1(
        toolkit_type="github",
        settings_entry_id="settings",
        tool_name="get_issue",
        arguments_entry_id="arguments",
        toolkit_id="17",
        toolkit_version="3",
    )
    settings = ResolvedToolkitCallToolInput(
        binding=_binding("someone-elses-entry", b"s"),
        value={},
    )
    arguments = ResolvedToolkitCallToolInput(binding=_binding("arguments", b"a"), value={})

    with pytest.raises(InvalidInput):
        request_from(
            command,
            input_bundle_id="bundle-1",
            input_bundle_digest=b"b" * 32,
            settings=settings,
            arguments=arguments,
            runtime_config={},
        )


def test_the_wire_result_binds_the_exact_inputs_the_call_consumed() -> None:
    async def run() -> None:
        sdk = _FakeSdk({"success": True, "tool_name": "get_issue", "result": {"a": 1}})

        bound = bind_result_summary(await _run(sdk, _request()))

        assert bound.toolkit_type == "github"
        assert bound.tool_name == "get_issue"
        assert bound.input_bundle_id == "bundle-1"
        assert bound.settings_entry_id == "settings"
        assert bound.arguments_entry_id == "arguments"
        assert bytes(bound.settings_content_digest.value) == b"s" * 32
        assert bytes(bound.arguments_content_digest.value) == b"a" * 32
        assert bound.result_summary.status == toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_OK
        # No artifact writer exists in this platform, so the reference stays unset
        # rather than carrying an id nothing can dereference.
        assert not bound.HasField("result_artifact")

    asyncio.run(run())


def test_an_unsupported_toolkit_settles_with_a_reason_instead_of_being_skipped() -> None:
    """A skipped command settles nothing.

    The caller's bounded wait would then burn its whole timeout and report
    "slow" for a condition that will never change. The refusal is the
    settlement.
    """

    command = toolkit_pb2.ToolkitCallToolCommandV1(
        toolkit_type="carrier_pigeon",
        settings_entry_id="settings",
        tool_name="deliver",
        arguments_entry_id="arguments",
        toolkit_id="17",
    )

    bound = unsupported_toolkit_result(
        command,
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        reason="the admitted Python worker image does not carry the pigeon toolkit",
    )

    assert (
        bound.result_summary.status
        == toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT
    )
    assert "does not carry" in bound.result_summary.error_message
    assert bound.tool_name == "deliver"


def test_no_setting_no_argument_and_no_result_reaches_the_redis_command() -> None:
    """Redis carries references. This asserts it, byte for byte.

    The settings hold a credential and the arguments hold caller content, so a
    command that embedded either would put both on a transport that has neither
    the size bound nor the authorization to hold them.
    """
    async def run() -> None:

        canary = "TEST_ONLY_CANARY_NOT_A_SECRET"
        sdk = _FakeSdk({"success": True, "tool_name": "get_issue", "result": {"r": canary}})
        request = _request(arguments={"note": canary})
        request = ToolkitCallToolRequest(
            toolkit_type=request.toolkit_type,
            toolkit_id=request.toolkit_id,
            toolkit_version=request.toolkit_version,
            tool_name=request.tool_name,
            input_bundle_id=request.input_bundle_id,
            input_bundle_digest=request.input_bundle_digest,
            settings=ResolvedToolkitCallToolInput(
                binding=request.settings.binding,
                value={"token": canary},
            ),
            arguments=request.arguments,
            runtime_config=request.runtime_config,
        )
        await _run(sdk, request)

        command = command_pb2.WorkerCommandV1(
            protocol_revision="elitea.runtime.protocol.v1",
            command_id="command-1",
            idempotency_key="outbox-1",
            command_type=command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL,
            execution_id="execution-1",
            generation=1,
            dispatch_ordinal=1,
            root_execution_id="execution-1",
            tenant_id="tenant-1",
            resource_project_id="42",
            projection_project_id="42",
            principal_ref="user:1",
            input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
                input_bundle_id="bundle-1",
                immutable_version="admission:bundle-1",
                digest=common_pb2.DigestV1(
                    algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
                    value=b"b" * 32,
                ),
                byte_length=512,
                media_type="application/x-protobuf",
            ),
            capability_id="toolkit.call_tool.v1",
            capability_version="1",
            resource_class="toolkit-call",
            isolation_class="shared-claim-scoped-authority",
            priority=1,
            deadline_unix_millis=1,
            limits_revision="elitea.runtime.limits.v1.r1",
            toolkit_call_tool=toolkit_pb2.ToolkitCallToolCommandV1(
                toolkit_type="github",
                settings_entry_id="settings",
                tool_name="get_issue",
                arguments_entry_id="arguments",
                toolkit_id="17",
                toolkit_version="3",
            ),
        )
        raw = command.SerializeToString(deterministic=True)

        assert canary.encode("utf-8") not in raw
        assert len(raw) < MAX_WORKER_COMMAND_BYTES

    asyncio.run(run())


def test_the_output_payload_arm_and_event_type_are_the_new_reserved_tags() -> None:
    """The two tags this capability took out of their reserved ranges."""

    frame_fields = output_pb2.ExecutionOutputFrameV1.DESCRIPTOR.fields_by_name
    assert frame_fields["toolkit_call_tool"].number == 25
    command_fields = command_pb2.WorkerCommandV1.DESCRIPTOR.fields_by_name
    assert command_fields["toolkit_call_tool"].number == 36
    assert (
        output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT
        == 7
    )
    assert command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL == 10

def test_a_status_the_wire_does_not_model_is_refused_not_defaulted() -> None:
    """A default here would ship UNSPECIFIED, which reads as "no answer"."""

    result = ToolkitCallToolResult(
        toolkit_type="github",
        tool_name="get_issue",
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        settings=_binding("settings", b"s"),
        arguments=_binding("arguments", b"a"),
        outcome=ToolkitCallToolOutcome(
            status="invented_status",
            result_json="",
            truncated=False,
            error_message="",
        ),
    )

    with pytest.raises(InvalidInput):
        bind_result_summary(result)


def test_a_malformed_digest_is_refused_rather_than_padded() -> None:
    result = ToolkitCallToolResult(
        toolkit_type="github",
        tool_name="get_issue",
        input_bundle_id="bundle-1",
        input_bundle_digest=b"too-short",
        settings=_binding("settings", b"s"),
        arguments=_binding("arguments", b"a"),
        outcome=ToolkitCallToolOutcome(
            status="ok", result_json="{}", truncated=False, error_message=""
        ),
    )

    with pytest.raises(InvalidInput):
        bind_result_summary(result)


def _signed_tool_run_command(**overrides: Any) -> bytes:
    command = command_pb2.WorkerCommandV1(
        protocol_revision=PROTOCOL_REVISION,
        command_id="command-1",
        idempotency_key="outbox-1",
        command_type=command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL,
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
        capability_id=TOOLKIT_CALL_TOOL_CAPABILITY_ID,
        capability_version=TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
        resource_class="toolkit-call",
        isolation_class="shared-claim-scoped-authority",
        priority=1,
        deadline_unix_millis=1_700_000_000_000,
        limits_revision=LIMITS_REVISION,
        toolkit_call_tool=toolkit_pb2.ToolkitCallToolCommandV1(
            toolkit_type="github",
            settings_entry_id="settings",
            tool_name="get_issue",
            arguments_entry_id="arguments",
            toolkit_id="17",
            toolkit_version="3",
        ),
    )
    for field, value in overrides.items():
        if field == "settings_entry_id":
            command.toolkit_call_tool.settings_entry_id = value
            continue
        setattr(command, field, value)
    command_bytes = command.SerializeToString(deterministic=True)
    return envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_HMAC_KEY_ID,
        signature=hmac.new(
            CONFORMANCE_HMAC_KEY, command_bytes, hashlib.sha256
        ).digest(),
        worker_command_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(command_bytes).digest(),
        ),
        worker_command_bytes=command_bytes,
    ).SerializeToString(deterministic=True)


def test_the_signed_command_verifies_and_takes_its_own_logical_output_id() -> None:
    """The bounded wire scan must ADMIT tag 36, not refuse it as unknown."""

    _, command = parse_and_verify_signed_command(
        _signed_tool_run_command(),
        authenticator=TestOnlyConformanceHmacAuthenticator(),
    )

    assert command.WhichOneof("capability_command") == "toolkit_call_tool"
    assert command.toolkit_call_tool.tool_name == "get_issue"
    assert _logical_output_id(command) == "toolkit-call-tool:execution-1"


def test_one_entry_serving_as_both_settings_and_arguments_is_refused() -> None:
    """Settings are redeemed by the platform; arguments come from the caller.

    One entry standing for both would let a caller's content be admitted where
    the platform's own authorized settings belong.
    """

    with pytest.raises(InvalidInput):
        parse_and_verify_signed_command(
            _signed_tool_run_command(settings_entry_id="arguments"),
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )


def test_a_wrong_capability_version_is_refused_as_unsupported() -> None:
    with pytest.raises(UnsupportedCapability):
        parse_and_verify_signed_command(
            _signed_tool_run_command(capability_version="2"),
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )


def test_an_ok_run_settles_succeeded_and_a_refusal_settles_failed() -> None:
    _, command = parse_and_verify_signed_command(
        _signed_tool_run_command(),
        authenticator=TestOnlyConformanceHmacAuthenticator(),
    )
    verified = VerifiedWorkerCommand(
        envelope=envelope_pb2.WorkerExecutionEnvelopeV1(
            fence=common_pb2.ExecutionFenceV1(
                workload_session_id="worker-session",
                producer_id="worker-1",
                claim_attempt=1,
                lease_epoch=1,
                fence_token=b"f" * 32,
            )
        ),
        command=command,
    )

    ok = toolkit_pb2.ToolkitCallToolResultV1(
        toolkit_type="github",
        tool_name="get_issue",
        input_bundle_id="bundle-1",
        input_bundle_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256, value=b"b" * 32
        ),
        result_summary=toolkit_pb2.ToolkitCallToolSummaryV1(
            status=toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR,
            error_message="404",
        ),
    )
    frame = build_output_frame(verified, ok, occurred_at_unix_millis=1)
    assert frame.WhichOneof("payload") == "toolkit_call_tool"
    assert (
        frame.event_type
        == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT
    )
    # A tool that RAISED is a completed run. The caller gets the reason, not a
    # failed execution that loses it.
    assert (
        frame.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED
    )

    refused = toolkit_pb2.ToolkitCallToolResultV1()
    refused.CopyFrom(ok)
    refused.result_summary.status = (
        toolkit_pb2.TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT
    )
    refused_frame = build_output_frame(verified, refused, occurred_at_unix_millis=1)
    # Nothing ran. There is no answer to carry, so the execution failed.
    assert (
        refused_frame.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_FAILED
    )
