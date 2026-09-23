from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import threading
from dataclasses import dataclass
from typing import Any
from uuid import UUID

import pytest
from elitea.runtime.v1 import (
    command_pb2,
    common_pb2,
    control_pb2,
    envelope_pb2,
    errors_pb2,
    indexing_pb2,
    input_pb2,
    node_event_pb2,
    output_pb2,
)

from elitea_worker.agents.client_context import EliteaClientContext, IndexExecutionClaim
from elitea_worker.agents import sdk_adapter as sdk_adapter_module
from elitea_worker.agents.sdk_adapter import (
    EliteaSdkIndexingAdapter,
    SdkBudgetExceeded,
)
from elitea_worker.constants import (
    CONFORMANCE_HMAC_KEY,
    CONFORMANCE_HMAC_KEY_ID,
    ENVELOPE_SCHEMA_REVISION,
)
from elitea_worker.execution.delivery import (
    DeliveryDisposition,
    IndexIngestDeliveryProcessor,
)
from elitea_worker.execution.errors import (
    AmbiguousExecutionRecovery,
    AuthorizationFailure,
    InternalFailure,
    InvalidInput,
    OutputCancellationWon,
    UnsupportedCapability,
)
from elitea_worker.protocol.codec import (
    TestOnlyConformanceHmacAuthenticator,
    VerifiedWorkerCommand,
    build_node_event_output_frame,
    build_output_frame,
    parse_and_verify_signed_command,
)
from elitea_worker.protocol.indexing import (
    INDEX_INGEST_FAILURE_SAFE_MESSAGE,
    bind_result_summary,
)
from elitea_worker.protocol.node_event import encode_current_node_event_json
from elitea_worker.handlers.indexing import IndexIngestInputBinding, IndexIngestResult
from elitea_worker.transport.input_content import ClaimBoundInputRequestBuilder
from elitea_worker.transport.redis_commands import RedisCommandDelivery

_NOW = 1_700_000_000_000
_WORKLOAD = "index-worker-1"
_PRODUCER = "index-producer-1"


class InlineSupervisor:
    def __init__(self) -> None:
        self.reserved = False
        self.submitted = False

    async def run(self, operation):
        return await operation()

    async def run_sync(self, operation, /, *args, **kwargs):
        assert self.reserved
        self.submitted = True
        return await asyncio.to_thread(operation, *args, **kwargs)

    async def reserve_sync(self):
        assert not self.reserved
        self.reserved = True
        return InlineReservation(self)


class InlineReservation:
    def __init__(self, supervisor: InlineSupervisor) -> None:
        self._supervisor = supervisor

    def release(self) -> None:
        self._supervisor.reserved = False


class RecordingSdk:
    def __init__(
        self,
        result: dict[str, Any],
        *,
        custom_events: list[tuple[str, dict[str, Any]]] | None = None,
        catch_callback_errors: bool = False,
        emit_tool_lifecycle: bool = False,
    ) -> None:
        self.result = result
        self.custom_events = custom_events or []
        self.catch_callback_errors = catch_callback_errors
        self.emit_tool_lifecycle = emit_tool_lifecycle
        self.calls: list[dict[str, Any]] = []
        self.callback_errors: list[Exception] = []

    def ingest(self, **kwargs: Any) -> dict[str, Any]:
        self.calls.append(kwargs)
        run_id = UUID("00000000-0000-0000-0000-000000000007")
        parent_run_id = UUID("00000000-0000-0000-0000-000000000099")
        if self.emit_tool_lifecycle:
            for callback in kwargs["runtime_config"]["callbacks"]:
                callback.on_tool_start(
                    {
                        "name": "index_data",
                        "description": "REDEEMED_LIFECYCLE_CANARY",
                    },
                    "REDEEMED_LIFECYCLE_CANARY",
                    run_id=run_id,
                    parent_run_id=parent_run_id,
                    metadata={"credential": "REDEEMED_LIFECYCLE_CANARY"},
                    inputs={"token": "REDEEMED_LIFECYCLE_CANARY"},
                )
        for name, data in self.custom_events:
            for callback in kwargs["runtime_config"]["callbacks"]:
                try:
                    callback.on_custom_event(
                        name,
                        data,
                        run_id=run_id,
                        metadata=kwargs["runtime_config"].get("metadata"),
                    )
                except Exception as exc:
                    if not self.catch_callback_errors:
                        raise
                    self.callback_errors.append(exc)
        if self.emit_tool_lifecycle:
            for callback in kwargs["runtime_config"]["callbacks"]:
                callback.on_tool_end(
                    {"credential": "REDEEMED_LIFECYCLE_CANARY"},
                    run_id=run_id,
                    parent_run_id=parent_run_id,
                )
        return self.result


class InputContextDiagnosticError(RuntimeError):
    pass


class ExecuteDiagnosticError(RuntimeError):
    pass


def _raise_execute_diagnostic(depth: int, message: str) -> None:
    if depth > 0:
        _raise_execute_diagnostic(depth - 1, message)
        return
    raise ExecuteDiagnosticError(message)


class InputClient:
    def __init__(self, values: dict[str, bytes], sequence: list[str]) -> None:
        self.values = values
        self.sequence = sequence
        self.calls = 0

    async def fetch_materialized(self, grant, *, source_immutable_version: str) -> bytes:
        self.calls += 1
        self.sequence.append(f"input:{grant.url.rsplit('/', 3)[-3]}")
        assert source_immutable_version
        return self.values[grant.url.rsplit("/", 3)[-3]]

    async def fetch(self, grant) -> bytes:
        self.calls += 1
        self.sequence.append(f"input:{grant.url.rsplit('/', 3)[-3]}")
        raw = self.values[grant.url.rsplit("/", 3)[-3]]
        assert len(raw) == grant.expected_length
        assert hmac.compare_digest(hashlib.sha256(raw).digest(), grant.expected_sha256)
        return raw


class Acker:
    def __init__(self) -> None:
        self.calls = 0

    async def ack_after_settlement(self, delivery, stable_delivery_id: str) -> None:
        assert stable_delivery_id == "index-idempotency-1"
        self.calls += 1


class Output:
    def __init__(self, pending: output_pb2.ExecutionOutputFrameV1 | None = None) -> None:
        self.frame = pending
        self.frames: list[output_pb2.ExecutionOutputFrameV1] = []
        self.sent = 0
        self.started = 0

    @property
    def has_pending_replay(self) -> bool:
        return self.frame is not None

    @property
    def pending_replay_frame(self) -> output_pb2.ExecutionOutputFrameV1 | None:
        return self.frame

    def replays(self, frame: output_pb2.ExecutionOutputFrameV1) -> bool:
        return self.frame is not None and self.frame == frame

    async def replace_pending_exact(self, expected, replacement) -> None:
        assert self.frame == expected
        self.frame = replacement
        self.frames.append(replacement)

    async def replace_pending_cancelled_recovery(self, expected, replacement) -> None:
        assert self.frame == expected
        self.frame = replacement
        self.frames.append(replacement)

    async def replace_pending_ambiguous_recovery(self, expected, replacement) -> None:
        assert self.frame == expected
        self.frame = replacement
        self.frames.append(replacement)

    async def start(self) -> None:
        self.started += 1

    async def send(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        assert self.frame is None
        self.frame = frame
        self.frames.append(frame)
        self.sent += 1

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        assert (
            self.frame is not None
            and sequence == self.frame.sequence
            and timeout_seconds > 0
        )
        self.frame = None

    async def reconcile_pending_through(self, sequence: int) -> None:
        assert self.frame is not None
        assert int(self.frame.sequence) <= sequence
        self.frame = None

    async def close(self) -> None:
        return None


class AckLossOutput(Output):
    def __init__(
        self, pending: output_pb2.ExecutionOutputFrameV1 | None
    ) -> None:
        super().__init__(pending)
        self.lose_ack = True

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        if self.lose_ack:
            raise RuntimeError("output stream is unavailable")
        await super().wait_for_ack(sequence, timeout_seconds)


class TerminalCancellationWinnerOutput(Output):
    """Model Main accepting cancellation before an ambiguous failure."""

    def __init__(self) -> None:
        super().__init__()
        self.cancel_once = True

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        if self.cancel_once:
            self.cancel_once = False
            try:
                raise OutputCancellationWon()
            except OutputCancellationWon as exc:
                raise RuntimeError("cancellation won terminal linearization") from exc
        await super().wait_for_ack(sequence, timeout_seconds)


class GatedOutput(Output):
    def __init__(self) -> None:
        super().__init__()
        self.waiting_for_ack = asyncio.Event()
        self.release_ack = asyncio.Event()

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        self.waiting_for_ack.set()
        await self.release_ack.wait()
        await super().wait_for_ack(sequence, timeout_seconds)


class BlockingInput:
    """An async input edge that proves Stop cancels before SDK submission."""

    def __init__(self) -> None:
        self.started = asyncio.Event()
        self.release = asyncio.Event()
        self.cancelled = False

    async def fetch_materialized(self, *args, **kwargs) -> bytes:
        _ = args, kwargs
        self.started.set()
        try:
            await self.release.wait()
        except asyncio.CancelledError:
            self.cancelled = True
            raise
        raise AssertionError("cancelled input fetch unexpectedly resumed")


class BlockingSdk:
    """A synchronous SDK call which cannot be preempted by Python."""

    def __init__(self) -> None:
        self.started = threading.Event()
        self.release = threading.Event()
        self.calls = 0

    def ingest(self, **kwargs: Any) -> dict[str, Any]:
        _ = kwargs
        self.calls += 1
        self.started.set()
        assert self.release.wait(timeout=2)
        return {"success": True, "result": {"status": "ok", "message": "late"}}


@dataclass
class SharedPendingOutput:
    frame: output_pb2.ExecutionOutputFrameV1 | None = None


class CancellationWinnerOutput:
    def __init__(
        self,
        shared: SharedPendingOutput,
        *,
        reject_progress: bool,
        frames: list[output_pb2.ExecutionOutputFrameV1],
    ) -> None:
        self.shared = shared
        self.reject_progress = reject_progress
        self.frames = frames

    @property
    def has_pending_replay(self) -> bool:
        return self.shared.frame is not None

    @property
    def pending_replay_frame(self) -> output_pb2.ExecutionOutputFrameV1 | None:
        return self.shared.frame

    def replays(self, frame: output_pb2.ExecutionOutputFrameV1) -> bool:
        return self.shared.frame == frame

    async def replace_pending_exact(self, expected, replacement) -> None:
        assert self.shared.frame == expected
        self.shared.frame = replacement
        self.frames.append(replacement)

    async def replace_pending_cancelled_recovery(self, expected, replacement) -> None:
        assert self.shared.frame == expected
        self.shared.frame = replacement
        self.frames.append(replacement)

    async def replace_pending_ambiguous_recovery(self, expected, replacement) -> None:
        assert self.shared.frame == expected
        self.shared.frame = replacement
        self.frames.append(replacement)

    async def start(self) -> None:
        return None

    async def send(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        assert self.shared.frame is None
        self.shared.frame = frame
        self.frames.append(frame)

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        assert self.shared.frame is not None
        assert self.shared.frame.sequence == sequence
        assert timeout_seconds > 0
        if self.reject_progress and self.shared.frame.HasField("node_event"):
            try:
                raise OutputCancellationWon()
            except OutputCancellationWon as exc:
                raise RuntimeError("output stream is unavailable") from exc
        self.shared.frame = None

    async def close(self) -> None:
        return None


class Control:
    def __init__(
        self,
        case: "Case",
        *,
        active: bool = False,
        claim_handoff_watermark: int = 0,
        begin_dispositions: list[int] | None = None,
        invocation_dispositions: list[int] | None = None,
        claim_disposition: int | None = None,
        receipt_fence: common_pb2.ExecutionFenceV1 | None = None,
        desired_state: int = common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
    ) -> None:
        self.case = case
        self.active = active
        self.claim_handoff_watermark = claim_handoff_watermark
        self.claim_disposition = claim_disposition
        self.receipt_fence = (
            receipt_fence if receipt_fence is not None else case.fence
        )
        self.settlements = 0
        self.begins = 0
        self.begin_dispositions = begin_dispositions or [
            control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW
        ]
        self.invocations = 0
        self.invocation_dispositions = invocation_dispositions or [
            control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW
        ]
        self.desired_state = desired_state
        self.cancellation_observed = asyncio.Event()

    async def claim_command(self, request):
        disposition = (
            self.claim_disposition
            if self.claim_disposition is not None
            else (
                control_pb2.CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK
                if self.active
                else control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED
            )
        )
        receipt = control_pb2.ClaimReceiptV1(
            disposition=disposition,
            claim_id="claim-1",
            identity=_identity(self.case.command),
            fence=self.receipt_fence,
            lease_expires_at_unix_millis=_NOW + 120_000,
            desired_state=self.desired_state,
            claim_handoff_watermark=self.claim_handoff_watermark,
        )
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED:
            receipt.input_bundle_ref.CopyFrom(self.case.command.input_bundle_ref)
            receipt.input_bundle.CopyFrom(self.case.manifest)
        return control_pb2.ClaimCommandResponseV1(receipt=receipt)

    async def renew_lease(self, request):
        self._record_desired_state()
        return control_pb2.RenewLeaseResponseV1(
            lease_expires_at_unix_millis=_NOW + 180_000,
            desired_state=self.desired_state,
        )

    async def begin_execution(self, request):
        self.begins += 1
        disposition = (
            self.begin_dispositions.pop(0)
            if self.begin_dispositions
            else control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED
        )
        return control_pb2.BeginExecutionResponseV1(
            disposition=disposition,
        )

    async def authorize_invocation(self, request):
        self.invocations += 1
        disposition = (
            self.invocation_dispositions.pop(0)
            if self.invocation_dispositions
            else control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED
        )
        return control_pb2.AuthorizeInvocationResponseV1(
            disposition=disposition,
        )

    async def observe_desired_state(self, request):
        self._record_desired_state()
        return control_pb2.ObserveDesiredStateResponseV1(
            desired_state=self.desired_state,
        )

    def _record_desired_state(self) -> None:
        if self.desired_state == common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED:
            self.cancellation_observed.set()

    async def prepare_settlement(self, request):
        self.settlements += 1
        return control_pb2.PrepareSettlementResponseV1(
            settlement_receipt_id="settlement-1",
            outcome=request.proposal.requested_outcome,
        )


@dataclass(frozen=True)
class Case:
    command: command_pb2.WorkerCommandV1
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1
    manifest: input_pb2.ExecutionInputBundleV1
    fence: common_pb2.ExecutionFenceV1
    values: dict[str, bytes]

    @property
    def delivery(self) -> RedisCommandDelivery:
        return RedisCommandDelivery(
            "index-ingest.v1",
            "1-0",
            {"signed_envelope": self.signed.SerializeToString(deterministic=True)},
        )


@pytest.mark.parametrize("sdk_success", [True, False])
def test_index_delivery_invokes_sdk_once_and_emits_only_safe_terminal_fields(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    sdk_success: bool,
) -> None:
    case = _case()
    canary = "REDEEMED_SECRET_CANARY"

    async def run() -> None:
        sdk_result = (
            {
                "success": True,
                "result": {"status": "ok", "message": "Indexed 3 documents"},
                "toolkit_config": {"settings": {"token": canary}},
            }
            if sdk_success
            else {
                "success": False,
                "error": f"raw failure containing {canary}",
                "toolkit_config": {"settings": {"token": canary}},
            }
        )
        sdk = RecordingSdk(sdk_result, emit_tool_lifecycle=True)
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )
        sequence: list[str] = []

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            sequence.append("context")
            assert claim.resource_project_id == "42"
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        output = Output()
        control = Control(case)
        acker = Acker()
        processor = _processor(
            case,
            control=control,
            input_client=InputClient(case.values, sequence),
            output=output,
            acker=acker,
            context_factory=context_factory,
        )

        result = await processor.process(case.delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert len(sdk.calls) == 1
        assert sequence[-1] == "context"
        assert len(sequence) == 6
        assert control.settlements == 1
        assert acker.calls == 1
        encoded = result.output_frame.SerializeToString(deterministic=True)
        assert canary.encode() not in encoded
        assert b"REDEEMED_LIFECYCLE_CANARY" not in b"".join(
            frame.SerializeToString(deterministic=True) for frame in output.frames
        )
        assert [frame.node_event.type for frame in output.frames[:-1]] == (
            ["agent_tool_start", "agent_tool_end"]
            if sdk_success
            else [
                "agent_tool_start",
                "agent_index_data_status",
                "agent_tool_error",
            ]
        )
        assert output.frames[-1] == result.output_frame
        assert result.output_frame.settlement_proposal.requested_outcome == (
            common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED
            if sdk_success
            else common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )
        if sdk_success:
            assert result.output_frame.event_type == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT
            assert result.output_frame.index_ingest.result_summary.status == indexing_pb2.INDEX_INGEST_STATUS_V1_OK
            assert result.output_frame.index_ingest.result_summary.message == "Indexed 3 documents"
            assert sdk.calls[0]["toolkit_config"]["settings"]["token"] == "github-secret"
            runtime_config = sdk.calls[0]["runtime_config"]
            assert len(runtime_config["callbacks"]) == 1
            assert runtime_config["metadata"] == {
                "initiator": "user",
                "tool_name": "index_data",
                "display_name": "github",
            }
        else:
            assert (
                result.output_frame.event_type
                == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT
            )
            assert (
                result.output_frame.index_ingest.result_summary.status
                == indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
            )
            assert (
                result.output_frame.index_ingest.result_summary.message
                == INDEX_INGEST_FAILURE_SAFE_MESSAGE
            )
            failure_events = [
                frame.node_event
                for frame in output.frames
                if frame.HasField("node_event")
                and frame.node_event.type == "agent_index_data_status"
            ]
            assert len(failure_events) == 1
            failure = json.loads(
                encode_current_node_event_json(failure_events[0])
            )["response_metadata"]
            assert failure == {
                "task_id": case.command.execution_id,
                "index_name": "docs",
                "state": "failed",
                "error": "Indexing reported an error.",
                "indexed": 0,
                "updated": 0,
                "toolkit_id": None,
                "initiator": "user",
                "project_id": 42,
                "user_id": 7,
            }

    asyncio.run(run())
    captured = capsys.readouterr()
    assert captured.err == ""


@pytest.mark.parametrize(
    ("sdk_status", "index_state", "wire_status", "outcome", "tool_event"),
    [
        (
            "partly_indexed",
            "partly_indexed",
            indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED,
            common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED,
            "agent_tool_end",
        ),
        (
            "error",
            "failed",
            indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR,
            common_pb2.EXECUTION_OUTCOME_V1_FAILED,
            "agent_tool_error",
        ),
    ],
)
def test_index_delivery_preserves_sdk_partial_and_all_failed_status_once(
    monkeypatch: pytest.MonkeyPatch,
    sdk_status: str,
    index_state: str,
    wire_status: int,
    outcome: int,
    tool_event: str,
) -> None:
    async def run() -> None:
        case = _case()
        message = (
            "Indexed 8 documents; 2 chunks failed."
            if sdk_status == "partly_indexed"
            else "Failed to index documents."
        )
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {"status": sdk_status, "message": message},
            },
            custom_events=[
                (
                    "index_data_status",
                    {
                        "index_name": "docs",
                        "state": index_state,
                        "error": None if sdk_status == "partly_indexed" else message,
                        "indexed": 8 if sdk_status == "partly_indexed" else 0,
                        "updated": 0,
                        "toolkit_id": 9,
                    },
                )
            ],
            emit_tool_lifecycle=True,
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        output = Output()
        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=output,
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert [frame.node_event.type for frame in output.frames[:-1]] == [
            "agent_tool_start",
            "agent_index_data_status",
            tool_event,
        ]
        statuses = [
            json.loads(encode_current_node_event_json(frame.node_event))
            for frame in output.frames
            if frame.HasField("node_event")
            and frame.node_event.type == "agent_index_data_status"
        ]
        assert len(statuses) == 1
        assert statuses[0]["response_metadata"]["state"] == index_state
        assert result.output_frame.index_ingest.result_summary.status == wire_status
        assert result.output_frame.index_ingest.result_summary.message == message
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == outcome
        )

    asyncio.run(run())


def test_index_delivery_corrects_zero_output_parser_failure(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {
                    "status": "ok",
                    "message": (
                        "Successfully indexed 20 documents (0 chunks).\n"
                        "Skipped items (21 total):\n"
                        "  - Documents with errors (20): first, second\n"
                        "  - Runtime skipped (errors) (1): test_case.md"
                    ),
                },
            },
            custom_events=[
                (
                    "index_data_status",
                    {
                        "index_name": "docs",
                        "state": "completed",
                        "error": None,
                        "indexed": 20,
                        "updated": 0,
                        "toolkit_id": 9,
                    },
                )
            ],
            emit_tool_lifecycle=True,
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        output = Output()
        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=output,
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert [frame.node_event.type for frame in output.frames[:-1]] == [
            "agent_tool_start",
            "agent_index_data_status",
            "agent_index_data_status",
            "agent_tool_error",
        ]
        statuses = [
            json.loads(encode_current_node_event_json(frame.node_event))[
                "response_metadata"
            ]
            for frame in output.frames
            if frame.HasField("node_event")
            and frame.node_event.type == "agent_index_data_status"
        ]
        assert [status["state"] for status in statuses] == [
            "completed",
            "failed",
        ]
        assert statuses[-1]["indexed"] == 0
        assert statuses[-1]["error"] == "Indexing reported an error."
        assert (
            result.output_frame.index_ingest.result_summary.status
            == indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
        )
        assert (
            result.output_frame.index_ingest.result_summary.message
            == INDEX_INGEST_FAILURE_SAFE_MESSAGE
        )
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )

    asyncio.run(run())


def test_embedding_admission_record_preserves_sdk_proxy_input_and_never_enters_redis(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    case = _embedding_case()
    client_calls: list[dict[str, Any]] = []

    class FakeEliteAClient:
        def __init__(self, **kwargs: Any) -> None:
            assert kwargs == {
                "project_id": 42,
                "base_url": "https://elitea.internal",
                "auth_token": "actor-pat",
            }

        def test_toolkit_tool(self, **kwargs: Any) -> dict[str, Any]:
            client_calls.append(kwargs)
            return {
                "success": True,
                "result": {"status": "ok", "message": "Indexed with bound model"},
            }

    monkeypatch.setattr(
        sdk_adapter_module,
        "_indexing_client_type",
        lambda: FakeEliteAClient,
    )

    async def run() -> None:
        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            assert claim.resource_project_id == "42"
            return EliteaClientContext(
                42,
                "https://elitea.internal",
                "actor-pat",
            )

        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert len(client_calls) == 1
        assert (
            client_calls[0]["toolkit_config"]["settings"]["embedding_model"]
            == "embedding-current"
        )
        assert (
            json.loads(case.values["content-1"])["settings"]["embedding_model"]
            == "embedding-current"
        )
        assert result.output_frame is not None
        terminal = result.output_frame.index_ingest
        assert terminal.HasField("embedding_binding")
        assert terminal.embedding_binding.entry_id == "embedding-binding"

        signed_bytes = case.delivery.signed_envelope
        assert b"42_embedding-current" not in signed_bytes
        assert b"configuration_digest" not in signed_bytes
        assert len(signed_bytes) < 64 * 1024

    asyncio.run(run())


def test_binding_aware_worker_rejects_index_capability_v1_before_claim() -> None:
    case = _embedding_case()
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1.FromString(
        case.delivery.signed_envelope
    )
    command = command_pb2.WorkerCommandV1.FromString(signed.worker_command_bytes)
    command.capability_version = "1"
    command_raw = command.SerializeToString(deterministic=True)
    signed.worker_command_bytes = command_raw
    signed.worker_command_digest.CopyFrom(_digest(hashlib.sha256(command_raw).digest()))
    signed.signature = hmac.new(
        CONFORMANCE_HMAC_KEY,
        command_raw,
        hashlib.sha256,
    ).digest()

    with pytest.raises(UnsupportedCapability):
        parse_and_verify_signed_command(
            signed.SerializeToString(deterministic=True),
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )


def test_worker_rejects_unknown_index_initiator_before_claim() -> None:
    case = _case()
    command = command_pb2.WorkerCommandV1.FromString(
        case.command.SerializeToString(deterministic=True)
    )
    command.index_ingest.initiator = "automation"
    case = _resign_case(case, command)

    with pytest.raises(InvalidInput):
        parse_and_verify_signed_command(
            case.delivery.signed_envelope,
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )


@pytest.mark.parametrize("failure", ["missing", "mismatched", "pre_binding"])
def test_required_embedding_binding_fails_closed_before_sdk(
    monkeypatch: pytest.MonkeyPatch,
    failure: str,
) -> None:
    case = _embedding_case()
    if failure == "missing":
        case = _case()
        values = dict(case.values)
        toolkit = json.loads(values["content-1"])
        toolkit["settings"]["embedding_model"] = "embedding-current"
        values["content-1"] = json.dumps(
            toolkit, sort_keys=True, separators=(",", ":")
        ).encode()
        case = Case(
            command=case.command,
            signed=case.signed,
            manifest=case.manifest,
            fence=case.fence,
            values=values,
        )
    elif failure == "mismatched":
        values = dict(case.values)
        binding = json.loads(values["content-6"])
        binding["model_name"] = "other-model"
        raw = json.dumps(
            binding, sort_keys=True, separators=(",", ":")
        ).encode()
        values["content-6"] = raw
        version = f"sha256:{hashlib.sha256(raw).hexdigest()}"
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            case.manifest.SerializeToString(deterministic=True)
        )
        manifest.entries[5].immutable_version = version
        manifest.entries[5].content.immutable_version = version
        manifest.entries[5].content.byte_length = len(raw)
        manifest.entries[5].content.digest.CopyFrom(
            _digest(hashlib.sha256(raw).digest())
        )
        command = command_pb2.WorkerCommandV1.FromString(
            case.command.SerializeToString(deterministic=True)
        )
        command.index_ingest.embedding_binding.immutable_version = version
        command.index_ingest.embedding_binding.content_digest.CopyFrom(
            _digest(hashlib.sha256(raw).digest())
        )
        case = _with_manifest(
            Case(
                command=command,
                signed=case.signed,
                manifest=case.manifest,
                fence=case.fence,
                values=values,
            ),
            manifest,
        )
    else:
        command = command_pb2.WorkerCommandV1.FromString(
            case.command.SerializeToString(deterministic=True)
        )
        command.index_ingest.ClearField("embedding_binding")
        case = _resign_case(case, command)

    async def run() -> None:
        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(
                lambda cls, context: (_ for _ in ()).throw(
                    AssertionError("invalid binding must not construct SDK")
                )
            ),
        )
        operation = _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)
        if failure == "pre_binding":
            with pytest.raises(InvalidInput):
                await operation
        else:
            result = await operation
            assert isinstance(result.execution_error, InvalidInput)

    asyncio.run(run())


def test_embedding_reference_digest_must_match_claim_manifest() -> None:
    case = _embedding_case()
    command = command_pb2.WorkerCommandV1.FromString(
        case.command.SerializeToString(deterministic=True)
    )
    command.index_ingest.embedding_binding.content_digest.value = b"x" * 32
    case = _resign_case(case, command)

    async def run() -> None:
        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("mismatched binding must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("mismatched binding must not redeem SDK authority")

        with pytest.raises(InvalidInput):
            await _processor(
                case,
                control=Control(case),
                input_client=ForbiddenInput(),
                output=Output(),
                acker=Acker(),
                context_factory=forbidden_context,
            ).process(case.delivery)

    asyncio.run(run())


def test_lost_begin_response_replay_never_resolves_inputs_or_invokes_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    case = _case()

    class ForbiddenInput:
        async def fetch_materialized(self, *args, **kwargs):
            raise AssertionError("ALREADY_STARTED must not resolve business inputs")

    class ForbiddenSdk:
        @classmethod
        def from_context(cls, context):
            raise AssertionError("ALREADY_STARTED must not construct the SDK")

    monkeypatch.setattr(EliteaSdkIndexingAdapter, "from_context", ForbiddenSdk.from_context)

    async def run() -> None:
        async def forbidden_context(claim):
            raise AssertionError("ALREADY_STARTED must not redeem SDK authority")

        supervisor = InlineSupervisor()

        class CapacityAwareControl(Control):
            async def begin_execution(self, request):
                assert supervisor.reserved
                return await super().begin_execution(request)

        control = CapacityAwareControl(
            case,
            begin_dispositions=[
                control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED
            ],
        )
        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=Output(),
            acker=Acker(),
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
        assert control.begins == 1
        assert supervisor.submitted is False
        assert supervisor.reserved is False

    asyncio.run(run())


@pytest.mark.parametrize("initiator", ["user", "schedule"])
def test_begin_replay_cannot_double_submit_index_sdk(
    monkeypatch: pytest.MonkeyPatch,
    initiator: str,
) -> None:
    case = _scheduled_case() if initiator == "schedule" else _case()
    sdk = RecordingSdk(
        {"success": True, "result": {"status": "ok", "message": "Indexed once"}}
    )
    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: sdk),
    )

    async def run() -> None:
        async def context_factory(claim):
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        control = Control(
            case,
            begin_dispositions=[
                control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW,
                control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED,
            ],
        )
        supervisor = InlineSupervisor()
        processor = _processor(
            case,
            control=control,
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
            supervisor=supervisor,
        )

        first = await processor.process(case.delivery)
        replay = await processor.process(case.delivery)

        assert first.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert replay.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
        assert control.begins == 2
        assert len(sdk.calls) == 1
        assert sdk.calls[0]["runtime_config"]["metadata"]["initiator"] == initiator
        assert supervisor.reserved is False

    asyncio.run(run())

def test_pre_submission_crash_is_recoverable_without_duplicate_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    case = _case()
    sdk = RecordingSdk(
        {"success": True, "result": {"status": "ok", "message": "Indexed once"}}
    )
    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: sdk),
    )

    class SimulatedProcessCrash(BaseException):
        pass

    async def run() -> None:
        async def crash_before_authorization(claim):
            raise SimulatedProcessCrash()

        first_control = Control(case)
        first_supervisor = InlineSupervisor()
        with pytest.raises(SimulatedProcessCrash):
            await _processor(
                case,
                control=first_control,
                input_client=InputClient(case.values, []),
                output=Output(),
                acker=Acker(),
                context_factory=crash_before_authorization,
                supervisor=first_supervisor,
            ).process(case.delivery)
        assert first_control.begins == 1
        assert first_control.invocations == 0
        assert not first_supervisor.submitted
        assert not sdk.calls

        async def context_factory(claim):
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        replacement_control = Control(case)
        result = await _processor(
            case,
            control=replacement_control,
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
            supervisor=InlineSupervisor(),
        ).process(case.delivery)
        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert replacement_control.begins == 1
        assert replacement_control.invocations == 1
        assert len(sdk.calls) == 1

    asyncio.run(run())

def test_invocation_authorization_replay_fails_terminal_without_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    case = _case()
    sdk = RecordingSdk(
        {"success": True, "result": {"status": "ok", "message": "must not run"}}
    )
    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: sdk),
    )

    async def run() -> None:
        async def context_factory(claim):
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        control = Control(
            case,
            invocation_dispositions=[
                control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED
            ],
        )
        supervisor = InlineSupervisor()
        acker = Acker()
        output = Output()
        result = await _processor(
            case,
            control=control,
            input_client=InputClient(case.values, []),
            output=output,
            acker=acker,
            context_factory=context_factory,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert isinstance(result.execution_error, AmbiguousExecutionRecovery)
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.safe_message
            == "The runtime operation failed."
        )
        assert control.invocations == 1
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted
        assert not sdk.calls
        assert len(output.frames) == 2
        status, terminal = output.frames
        assert status.node_event.type == "agent_index_data_status"
        metadata = json.loads(
            encode_current_node_event_json(status.node_event)
        )["response_metadata"]
        assert metadata == {
            "task_id": case.command.execution_id,
            "index_name": "docs",
            "state": "failed",
            "error": "Indexing reported an error.",
            "indexed": 0,
            "updated": 0,
            "toolkit_id": None,
            "initiator": "user",
            "project_id": 42,
            "user_id": 7,
        }
        assert terminal == result.output_frame
        assert terminal.sequence == 2

    asyncio.run(run())

def test_post_submission_crash_recovers_once_without_reinvoking_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    case = _case()

    class SimulatedProcessCrash(BaseException):
        pass

    class CrashSdk:
        def __init__(self) -> None:
            self.calls = 0

        def ingest(self, **kwargs):
            self.calls += 1
            raise SimulatedProcessCrash()

    sdk = CrashSdk()
    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: sdk),
    )

    async def run() -> None:
        async def context_factory(claim):
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        first_control = Control(case)
        first_supervisor = InlineSupervisor()
        with pytest.raises(SimulatedProcessCrash):
            await _processor(
                case,
                control=first_control,
                input_client=InputClient(case.values, []),
                output=Output(),
                acker=Acker(),
                context_factory=context_factory,
                supervisor=first_supervisor,
            ).process(case.delivery)
        assert first_control.invocations == 1
        assert first_supervisor.submitted
        assert sdk.calls == 1

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("ambiguous recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("ambiguous recovery must not create SDK context")

        replacement_control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
            ),
        )
        acker = Acker()
        result = await _processor(
            case,
            control=replacement_control,
            input_client=ForbiddenInput(),
            output=Output(),
            acker=acker,
            context_factory=forbidden_context,
            supervisor=InlineSupervisor(),
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert isinstance(result.execution_error, AmbiguousExecutionRecovery)
        assert result.output_frame is not None
        assert result.output_frame.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL
        assert replacement_control.begins == 0
        assert replacement_control.invocations == 0
        assert replacement_control.settlements == 1
        assert acker.calls == 1
        assert sdk.calls == 1

    asyncio.run(run())


def test_ambiguous_running_recovery_cancellation_wins_without_sdk() -> None:
    async def run() -> None:
        case = _case()
        output = TerminalCancellationWinnerOutput()
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
            ),
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("ambiguous recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("ambiguous recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert len(output.frames) == 2
        assert (
            output.frames[0].runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL
        )
        assert output.frames[1] == result.output_frame
        assert control.begins == 0
        assert control.invocations == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_ambiguous_running_recovery_reuses_lost_terminal_on_redelivery() -> None:
    async def run() -> None:
        case = _case()
        output = AckLossOutput(None)
        acker = Acker()
        supervisor = InlineSupervisor()
        controls: list[Control] = []

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("ambiguous recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("ambiguous recovery must not create SDK context")

        def processor() -> IndexIngestDeliveryProcessor:
            control = Control(
                case,
                claim_disposition=(
                    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
                ),
            )
            controls.append(control)
            return _processor(
                case,
                control=control,
                input_client=ForbiddenInput(),
                output=output,
                acker=acker,
                context_factory=forbidden_context,
                supervisor=supervisor,
            )

        with pytest.raises(RuntimeError, match="unavailable"):
            await processor().process(case.delivery)
        pending = output.pending_replay_frame
        assert pending is not None and pending.terminal
        assert controls[0].settlements == 0
        assert acker.calls == 0

        output.lose_ack = False
        result = await processor().process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame == pending
        assert output.frame is None
        assert len(output.frames) == 1
        assert sum(control.settlements for control in controls) == 1
        assert all(control.begins == 0 for control in controls)
        assert all(control.invocations == 0 for control in controls)
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_ambiguous_recovery_replaces_old_fence_terminal_without_sdk_reentry() -> None:
    async def run() -> None:
        case = _case()
        old_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        old_fence.fence_token = b"o" * 32
        replacement_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        replacement_fence.claim_attempt += 1
        replacement_fence.lease_epoch += 1
        replacement_fence.fence_token = b"n" * 32
        old_envelope = envelope_pb2.WorkerExecutionEnvelopeV1(
            signed_command=case.signed,
            fence=old_fence,
        )
        pending = build_output_frame(
            VerifiedWorkerCommand(envelope=old_envelope, command=case.command),
            bind_result_summary(_terminal_result()),
            occurred_at_unix_millis=_NOW,
            claim_handoff_watermark=0,
            sequence=6,
        )
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
            ),
            claim_handoff_watermark=5,
            receipt_fence=replacement_fence,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("terminal recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("terminal recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.fence == replacement_fence
        assert result.output_frame.claim_handoff_watermark == 5
        assert result.output_frame.sequence == pending.sequence
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL
        )
        assert result.output_frame.runtime_error.safe_message == (
            "The runtime operation failed."
        )
        assert result.output_frame.payload_digest != pending.payload_digest
        assert output.frame is None
        assert output.frames == [result.output_frame]
        assert control.begins == 0
        assert control.invocations == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_live_running_recovery_without_spool_waits_for_current_owner() -> None:
    async def run() -> None:
        case = _case()
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("live recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("live recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=Output(),
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
        assert result.output_frame is None
        assert control.begins == 0
        assert control.invocations == 0
        assert control.settlements == 0
        assert acker.calls == 0
        assert not supervisor.submitted

    asyncio.run(run())


def test_input_context_internal_failure_emits_credential_safe_diagnostic(
    capsys: pytest.CaptureFixture[str],
) -> None:
    case = _case()
    canary = "INPUT_CONTEXT_SECRET_CANARY"

    async def run() -> None:
        async def context_factory(
            claim: IndexExecutionClaim,
        ) -> EliteaClientContext:
            raise InputContextDiagnosticError(canary)

        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL
        )
        assert (
            result.output_frame.runtime_error.safe_message
            == "The runtime operation failed."
        )
        assert canary.encode() not in result.output_frame.SerializeToString(
            deterministic=True
        )

    asyncio.run(run())

    captured = capsys.readouterr()
    diagnostic = _single_index_internal_failure(captured.err)
    assert diagnostic["stage"] == "input_context"
    assert diagnostic["execution_id"] == case.command.execution_id
    assert diagnostic["exception_module"] == InputContextDiagnosticError.__module__
    assert diagnostic["exception_name"] == InputContextDiagnosticError.__name__
    _assert_safe_diagnostic_frames(diagnostic["frames"])
    assert any(
        frame["function"] == "context_factory" for frame in diagnostic["frames"]
    )
    assert canary not in captured.err


def test_execute_internal_failure_emits_bounded_credential_safe_diagnostic(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    case = _case()
    canary = "EXECUTE_SECRET_CANARY"

    class RaisingSdk:
        def ingest(self, **kwargs: Any) -> dict[str, Any]:
            _raise_execute_diagnostic(16, canary)
            raise AssertionError("unreachable")

    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: RaisingSdk()),
    )

    async def run() -> None:
        async def context_factory(
            claim: IndexExecutionClaim,
        ) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL
        )
        assert (
            result.output_frame.runtime_error.safe_message
            == "The runtime operation failed."
        )
        assert canary.encode() not in result.output_frame.SerializeToString(
            deterministic=True
        )

    asyncio.run(run())

    captured = capsys.readouterr()
    diagnostic = _single_index_internal_failure(captured.err)
    assert diagnostic["stage"] == "execute"
    assert diagnostic["execution_id"] == case.command.execution_id
    assert diagnostic["exception_module"] == ExecuteDiagnosticError.__module__
    assert diagnostic["exception_name"] == ExecuteDiagnosticError.__name__
    _assert_safe_diagnostic_frames(diagnostic["frames"])
    assert len(diagnostic["frames"]) == 8
    assert any(
        frame["function"] == "_execute_resolved"
        for frame in diagnostic["frames"]
    )
    assert any(
        frame["function"] == "_raise_execute_diagnostic"
        for frame in diagnostic["frames"]
    )
    assert canary not in captured.err


def test_sdk_budget_exhaustion_maps_to_safe_non_retryable_resource_failure(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    case = _case()
    canary = "BUDGET_SECRET_CANARY"

    class BudgetBlockedSdk:
        def ingest(self, **kwargs: Any) -> dict[str, Any]:
            raise SdkBudgetExceeded()

    monkeypatch.setattr(
        EliteaSdkIndexingAdapter,
        "from_context",
        classmethod(lambda cls, context: BudgetBlockedSdk()),
    )

    async def run() -> None:
        async def context_factory(
            claim: IndexExecutionClaim,
        ) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED
        )
        assert (
            result.output_frame.runtime_error.safe_message
            == "The execution exceeded an approved resource limit."
        )
        assert result.output_frame.runtime_error.retryable is False
        assert canary.encode() not in result.output_frame.SerializeToString(
            deterministic=True
        )

    asyncio.run(run())

    captured = capsys.readouterr()
    assert "ELITEA_INDEX_INTERNAL_FAILURE" not in captured.err
    assert canary not in captured.err


def test_current_sdk_index_progress_is_acked_before_contiguous_terminal(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        canary = "REDEEMED_SECRET_CANARY"
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {"status": "ok", "message": "Indexed 3 documents"},
            },
            custom_events=[
                (
                    "thinking_step",
                    {
                        "message": "20 files processed",
                        "tool_name": "loader",
                        "toolkit": "EliteaGitHubAPIWrapper",
                        "toolkit_config": {
                            "settings": {"private_token": canary}
                        },
                    },
                ),
                (
                    "index_data_status",
                    {
                        "id": "index-meta-1",
                        "index_name": "docs",
                        "state": "completed",
                        "error": None,
                        "reindex": False,
                        "indexed": 3,
                        "updated": 0,
                        "created_at": 1_700_000_000.0,
                        "updated_on": 1_700_000_010.0,
                        "toolkit_id": 9,
                        "credential": canary,
                    },
                )
            ],
            emit_tool_lifecycle=True,
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        output = GatedOutput()
        processing = asyncio.create_task(_processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=output,
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery))
        await asyncio.wait_for(output.waiting_for_ack.wait(), timeout=1)
        assert not processing.done()
        assert [frame.sequence for frame in output.frames] == [1]
        output.release_ack.set()
        result = await processing

        assert result.output_frame is not None
        assert result.output_frame.sequence == 5
        assert result.output_frame.settlement_proposal.terminal_sequence == 5
        assert [frame.sequence for frame in output.frames] == [1, 2, 3, 4, 5]
        start, thinking, status, end, terminal = output.frames
        for progress in (start, thinking, status, end):
            assert not progress.terminal
            assert not progress.HasField("settlement_proposal")
            assert (
                progress.event_type
                == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT
            )
        assert terminal == result.output_frame
        assert start.node_event.type == "agent_tool_start"
        assert end.node_event.type == "agent_tool_end"

        browser_event = json.loads(
            encode_current_node_event_json(thinking.node_event)
        )
        assert browser_event["type"] == "agent_thinking_step"
        assert browser_event["stream_id"] == case.command.index_ingest.client_stream_id
        assert browser_event["message_id"] == case.command.index_ingest.client_message_id
        assert browser_event["sio_event"] == case.command.index_ingest.sio_event
        assert browser_event["response_metadata"]["message"] == "20 files processed"
        assert browser_event["response_metadata"]["tool_name"] == "loader"
        assert (
            browser_event["response_metadata"]["toolkit"]
            == "EliteaGitHubAPIWrapper"
        )
        assert browser_event["response_metadata"]["metadata"] == {
            "initiator": "user",
            "tool_name": "index_data",
            "display_name": "github",
        }
        assert canary not in json.dumps(browser_event)

        browser_event = json.loads(
            encode_current_node_event_json(status.node_event)
        )
        assert browser_event["type"] == "agent_index_data_status"
        assert browser_event["stream_id"] == case.command.index_ingest.client_stream_id
        metadata = browser_event["response_metadata"]
        assert metadata["task_id"] == case.command.execution_id
        assert metadata["initiator"] == "user"
        assert metadata["project_id"] == 42
        assert metadata["user_id"] == 7
        assert metadata["toolkit_id"] == 9
        assert metadata["index_name"] == "docs"
        assert canary not in json.dumps(browser_event)

    asyncio.run(run())


def test_scheduled_index_keeps_durable_status_and_terminal_without_browser_stream(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _scheduled_case()
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {"status": "ok", "message": "Scheduled index complete"},
            },
            custom_events=[
                ("thinking_step", {"message": "10 files processed"}),
                ("thinking_step_update", {"message": "20 files processed"}),
                (
                    "index_data_status",
                    {
                        "index_name": "docs",
                        "state": "scheduled_reindex",
                        "toolkit_id": 9,
                        "indexed": 17,
                        "updated": 4,
                        "reindex": True,
                    },
                ),
                (
                    "index_data_removed",
                    {"index_name": "obsolete", "toolkit_id": 9, "project_id": 42},
                ),
            ],
            emit_tool_lifecycle=True,
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        output = Output()
        result = await _processor(
            case,
            control=Control(case),
            input_client=InputClient(case.values, []),
            output=output,
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert [frame.sequence for frame in output.frames] == [1, 2, 3]
        status, removed, terminal = output.frames
        assert [status.node_event.type, removed.node_event.type] == [
            "agent_index_data_status",
            "agent_index_data_removed",
        ]
        assert terminal == result.output_frame
        assert terminal.terminal
        assert terminal.settlement_proposal.terminal_sequence == 3
        summary = terminal.index_ingest.result_summary
        assert (
            summary.terminal_state
            == indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX
        )
        assert summary.indexed == 17
        assert summary.updated == 4
        assert summary.HasField("reindex")
        assert summary.reindex is True
        assert sdk.calls[0]["runtime_config"]["metadata"]["initiator"] == "schedule"
        for frame in (status, removed):
            current = json.loads(encode_current_node_event_json(frame.node_event))
            assert current["stream_id"] is None
            assert current["message_id"] is None
            assert current["sio_event"] is None
            assert current["response_metadata"]["metadata"]["initiator"] == "schedule"
        status_metadata = json.loads(
            encode_current_node_event_json(status.node_event)
        )["response_metadata"]
        assert status_metadata["initiator"] == "schedule"
        assert status_metadata["task_id"] == case.command.execution_id

    asyncio.run(run())


def test_claim_handoff_watermark_seeds_the_next_terminal_sequence(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {"status": "ok", "message": "Already resumed"},
            }
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        result = await _processor(
            case,
            control=Control(case, claim_handoff_watermark=3),
            input_client=InputClient(case.values, []),
            output=Output(),
            acker=Acker(),
            context_factory=context_factory,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert result.output_frame.sequence == 4
        assert result.output_frame.claim_handoff_watermark == 3
        assert result.output_frame.event_id == f"{case.command.command_id}:4"
        assert result.output_frame.settlement_proposal.terminal_sequence == 4

    asyncio.run(run())


def test_progress_cancellation_winner_replaces_exact_sequence_and_settles(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        sdk = RecordingSdk(
            {
                "success": True,
                "result": {"status": "ok", "message": "late success"},
            },
            custom_events=[
                (
                    "index_data_status",
                    {
                        "index_name": "docs",
                        "state": "in_progress",
                        "toolkit_id": 9,
                    },
                ),
                # Current synchronous SDKs may catch the first callback
                # exception and continue emitting. The terminal cancellation
                # winner must remain authoritative rather than becoming a
                # retryable progress transport failure on this second event.
                (
                    "index_data_status",
                    {
                        "index_name": "docs",
                        "state": "still_running",
                        "toolkit_id": 9,
                    },
                ),
            ],
            # The current BaseIndexerToolkit catches callback failures and the
            # phase-one synchronous call remains non-preemptible.
            catch_callback_errors=True,
        )
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        shared = SharedPendingOutput()
        frames: list[output_pb2.ExecutionOutputFrameV1] = []
        sessions = 0

        def output_factory() -> CancellationWinnerOutput:
            nonlocal sessions
            sessions += 1
            return CancellationWinnerOutput(
                shared,
                reject_progress=sessions == 1,
                frames=frames,
            )

        control = Control(case)
        acker = Acker()
        result = await _processor(
            case,
            control=control,
            input_client=InputClient(case.values, []),
            output=None,
            output_factory=output_factory,
            acker=acker,
            context_factory=context_factory,
        ).process(case.delivery)

        assert len(sdk.callback_errors) == 1
        assert result.output_frame is not None
        assert result.output_frame.sequence == 1
        assert result.output_frame.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        assert result.output_frame.settlement_proposal.terminal_sequence == 1
        assert frames[0].HasField("node_event")
        assert not frames[0].terminal
        assert frames[1] == result.output_frame
        assert shared.frame is None
        assert control.settlements == 1
        assert acker.calls == 1

    asyncio.run(run())


def test_stop_cancels_async_index_input_and_settles_one_cancelled_terminal() -> None:
    async def run() -> None:
        case = _case()
        control = Control(case)
        input_client = BlockingInput()
        acker = Acker()

        async def forbidden_context(claim: IndexExecutionClaim) -> EliteaClientContext:
            _ = claim
            raise AssertionError("Stop before SDK input resolution reached client context")

        processing = asyncio.create_task(
            _processor(
                case,
                control=control,
                input_client=input_client,
                output=Output(),
                acker=acker,
                context_factory=forbidden_context,
                lease_poll_interval_seconds=0.001,
            ).process(case.delivery)
        )
        await asyncio.wait_for(input_client.started.wait(), timeout=1)
        control.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
        await asyncio.wait_for(control.cancellation_observed.wait(), timeout=1)
        result = await asyncio.wait_for(processing, timeout=1)

        assert input_client.cancelled
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert control.settlements == 1
        assert acker.calls == 1

    asyncio.run(run())


def test_stop_waits_for_sync_sdk_then_fences_its_late_success_as_cancelled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        sdk = BlockingSdk()
        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: sdk),
        )

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            _ = claim
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        control = Control(case)
        output = Output()
        acker = Acker()
        processing = asyncio.create_task(
            _processor(
                case,
                control=control,
                input_client=InputClient(case.values, []),
                output=output,
                acker=acker,
                context_factory=context_factory,
                lease_poll_interval_seconds=0.001,
            ).process(case.delivery)
        )
        assert await asyncio.to_thread(sdk.started.wait, 1)
        control.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
        await asyncio.wait_for(control.cancellation_observed.wait(), timeout=1)
        assert not processing.done()
        sdk.release.set()
        result = await asyncio.wait_for(processing, timeout=1)

        assert sdk.calls == 1
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert len(output.frames) == 1
        assert output.frames[0] == result.output_frame
        assert control.settlements == 1
        assert acker.calls == 1

    asyncio.run(run())


def test_pending_index_output_recovers_without_input_context_or_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        safe_result = bind_result_summary(_terminal_result())
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1(
            signed_command=case.signed,
            fence=case.fence,
        )
        pending = build_output_frame(
            VerifiedWorkerCommand(envelope=envelope, command=case.command),
            safe_result,
            occurred_at_unix_millis=_NOW,
            claim_handoff_watermark=0,
        )
        output = Output(pending)
        control = Control(case, active=True)
        acker = Acker()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("recovery must not fetch SDK authority")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: (_ for _ in ()).throw(AssertionError())),
        )
        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame == pending
        assert output.sent == 0
        assert output.started == 1
        assert control.settlements == 1
        assert acker.calls == 1

    asyncio.run(run())


def test_pending_node_event_replays_without_settlement_input_or_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        pending = _pending_node_event(case)
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("NodeEvent recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("NodeEvent recovery must not fetch SDK authority")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: (_ for _ in ()).throw(AssertionError())),
        )
        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
        assert result.output_frame == pending
        assert not pending.terminal
        assert not pending.HasField("settlement_proposal")
        assert output.frame is None
        assert output.sent == 0
        assert output.started == 1
        assert control.begins == 0
        assert control.settlements == 0
        assert acker.calls == 0
        assert not supervisor.submitted

    asyncio.run(run())


def test_node_event_ack_loss_replays_on_redis_redelivery_without_sdk(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        pending = _pending_node_event(case)
        output = AckLossOutput(pending)
        acker = Acker()
        supervisor = InlineSupervisor()
        controls: list[Control] = []

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("NodeEvent recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("NodeEvent recovery must not fetch SDK authority")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: (_ for _ in ()).throw(AssertionError())),
        )

        def processor() -> IndexIngestDeliveryProcessor:
            control = Control(
                case,
                claim_disposition=(
                    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
                ),
            )
            controls.append(control)
            return _processor(
                case,
                control=control,
                input_client=ForbiddenInput(),
                output=output,
                acker=acker,
                context_factory=forbidden_context,
                supervisor=supervisor,
            )

        with pytest.raises(RuntimeError, match="unavailable"):
            await processor().process(case.delivery)
        assert output.frame == pending
        assert acker.calls == 0

        output.lose_ack = False
        result = await processor().process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
        assert result.output_frame == pending
        assert output.frame is None
        assert output.sent == 0
        assert output.started == 3
        assert all(control.begins == 0 for control in controls)
        assert all(control.settlements == 0 for control in controls)
        assert acker.calls == 0
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_replays_progress_then_settles_once() -> None:
    async def run() -> None:
        case = _case()
        pending = _pending_node_event(case)
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.sequence == 2
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert len(output.frames) == 1
        assert output.frames[0] == result.output_frame
        assert output.frame is None
        assert control.begins == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_without_spool_uses_handoff_next_sequence() -> None:
    async def run() -> None:
        case = _case()
        output = Output()
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            claim_handoff_watermark=3,
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert result.output_frame.sequence == 4
        assert result.output_frame.claim_handoff_watermark == 3
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert control.begins == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_discards_uncommitted_old_fence_progress() -> None:
    async def run() -> None:
        case = _case()
        old_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        old_fence.fence_token = b"o" * 32
        replacement_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        replacement_fence.claim_attempt += 1
        replacement_fence.lease_epoch += 1
        replacement_fence.fence_token = b"n" * 32
        # This is the production-shaped case: a local old-fence progress frame
        # reached sequence 6, but Main has no durable progress (watermark 0).
        pending = _pending_node_event(case, fence=old_fence, sequence=6)
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            # Main has authenticated contiguous progress through 5 even
            # though this worker's old fence still has only its local seq-6
            # spool frame. The replacement terminal must be seq 6 / hwm 5.
            claim_handoff_watermark=5,
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
            receipt_fence=replacement_fence,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.terminal
        assert result.output_frame.sequence == 6
        assert result.output_frame.claim_handoff_watermark == 5
        assert result.output_frame.fence == replacement_fence
        assert output.frames == [result.output_frame]
        assert output.frame is None
        assert control.begins == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_replaces_old_fence_terminal_spool() -> None:
    async def run() -> None:
        case = _case()
        old_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        old_fence.fence_token = b"o" * 32
        replacement_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        replacement_fence.claim_attempt += 1
        replacement_fence.lease_epoch += 1
        replacement_fence.fence_token = b"n" * 32
        old_envelope = envelope_pb2.WorkerExecutionEnvelopeV1(
            signed_command=case.signed,
            fence=old_fence,
        )
        pending = build_output_frame(
            VerifiedWorkerCommand(envelope=old_envelope, command=case.command),
            bind_result_summary(_terminal_result()),
            occurred_at_unix_millis=_NOW,
            claim_handoff_watermark=0,
        )
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
            receipt_fence=replacement_fence,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.terminal
        assert result.output_frame.sequence == pending.sequence
        assert result.output_frame.runtime_error.code == (
            errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert result.output_frame.fence == replacement_fence
        assert output.frame is None
        assert control.begins == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_replaces_rejected_progress_at_same_sequence() -> None:
    async def run() -> None:
        case = _case()
        pending = _pending_node_event(case)
        shared = SharedPendingOutput(pending)
        frames: list[output_pb2.ExecutionOutputFrameV1] = []
        output = CancellationWinnerOutput(
            shared,
            reject_progress=True,
            frames=frames,
        )
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=None,
            output_factory=lambda: output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.output_frame is not None
        assert result.output_frame.sequence == pending.sequence
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert frames == [result.output_frame]
        assert shared.frame is None
        assert control.begins == 0
        assert control.settlements == 1
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


def test_cancelled_running_recovery_reuses_lost_terminal_on_redelivery() -> None:
    async def run() -> None:
        case = _case()
        output = AckLossOutput(None)
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("cancelled recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("cancelled recovery must not create SDK context")

        def processor() -> IndexIngestDeliveryProcessor:
            return _processor(
                case,
                control=Control(
                    case,
                    claim_disposition=(
                        control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
                    ),
                    desired_state=(
                        common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
                    ),
                ),
                input_client=ForbiddenInput(),
                output=output,
                acker=acker,
                context_factory=forbidden_context,
                supervisor=supervisor,
            )

        with pytest.raises(RuntimeError, match="unavailable"):
            await processor().process(case.delivery)
        pending = output.pending_replay_frame
        assert pending is not None and pending.terminal
        assert acker.calls == 0

        output.lose_ack = False
        result = await processor().process(case.delivery)

        assert result.disposition is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        assert result.output_frame == pending
        assert output.frame is None
        assert len(output.frames) == 1
        assert not supervisor.submitted
        assert acker.calls == 1

    asyncio.run(run())


def test_settled_cancelled_recovery_acks_without_reinvoking_worker() -> None:
    async def run() -> None:
        case = _case()
        control = Control(
            case,
            claim_disposition=control_pb2.CLAIM_DISPOSITION_V1_SETTLED_ACK,
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("settled cancellation must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("settled cancellation must not create SDK context")

        result = await _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=Output(),
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)

        assert result.disposition is DeliveryDisposition.TERMINAL_REDELIVERY_ACKED
        assert control.begins == 0
        assert control.settlements == 0
        assert acker.calls == 1
        assert not supervisor.submitted

    asyncio.run(run())


@pytest.mark.parametrize(
    ("handoff_watermark", "reconciled"),
    [(1, True), (0, False)],
)
def test_stale_node_event_spool_requires_authenticated_handoff_reconciliation(
    monkeypatch: pytest.MonkeyPatch,
    handoff_watermark: int,
    reconciled: bool,
) -> None:
    async def run() -> None:
        case = _case()
        old_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        old_fence.fence_token = b"o" * 32
        current_fence = common_pb2.ExecutionFenceV1.FromString(
            case.fence.SerializeToString(deterministic=True)
        )
        current_fence.claim_attempt += 1
        current_fence.lease_epoch += 1
        current_fence.fence_token = b"n" * 32
        pending = _pending_node_event(case, fence=old_fence)
        output = Output(pending)
        control = Control(
            case,
            claim_disposition=(
                control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
            ),
            claim_handoff_watermark=handoff_watermark,
            receipt_fence=current_fence,
        )
        acker = Acker()
        supervisor = InlineSupervisor()

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("stale recovery must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("stale recovery must not fetch SDK authority")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(lambda cls, context: (_ for _ in ()).throw(AssertionError())),
        )
        processing = _processor(
            case,
            control=control,
            input_client=ForbiddenInput(),
            output=output,
            acker=acker,
            context_factory=forbidden_context,
            supervisor=supervisor,
        ).process(case.delivery)
        if reconciled:
            result = await processing
            assert result.disposition is DeliveryDisposition.RECOVERY_REQUIRED_NOACK
            assert result.output_frame == pending
            assert output.frame is None
        else:
            with pytest.raises(AuthorizationFailure, match="server-side recovery"):
                await processing
            assert output.frame == pending

        assert output.started == 0
        assert output.sent == 0
        assert control.begins == 0
        assert control.settlements == 0
        assert acker.calls == 0
        assert not supervisor.submitted

    asyncio.run(run())


def test_deadline_during_context_acquisition_prevents_sdk_execution(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        case = _case()
        clock = [_NOW]
        sequence: list[str] = []

        async def context_factory(claim: IndexExecutionClaim) -> EliteaClientContext:
            sequence.append("context")
            clock[0] = int(case.command.deadline_unix_millis)
            return EliteaClientContext(42, "https://elitea.internal", "actor-pat")

        monkeypatch.setattr(
            EliteaSdkIndexingAdapter,
            "from_context",
            classmethod(
                lambda cls, context: (_ for _ in ()).throw(
                    AssertionError("deadline-expired work must not enter the SDK")
                )
            ),
        )
        output = Output()
        control = Control(case)
        acker = Acker()

        result = await _processor(
            case,
            control=control,
            input_client=InputClient(case.values, sequence),
            output=output,
            acker=acker,
            context_factory=context_factory,
            clock=lambda: clock[0],
        ).process(case.delivery)

        assert sequence[-1] == "context"
        assert len(sequence) == 6
        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
        )
        assert control.settlements == 1
        assert acker.calls == 1

    asyncio.run(run())


def test_index_delivery_rejects_wrong_manifest_role_before_data_or_authority() -> None:
    async def run() -> None:
        original = _case()
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            original.manifest.SerializeToString(deterministic=True)
        )
        manifest.entries[0].semantic_role = "index.llm_model"
        case = _with_manifest(original, manifest)

        class ForbiddenInput:
            async def fetch_materialized(self, *args, **kwargs):
                raise AssertionError("rejected manifest must not fetch input")

        async def forbidden_context(claim):
            raise AssertionError("rejected manifest must not fetch SDK authority")

        control = Control(case)
        acker = Acker()
        with pytest.raises(InvalidInput):
            await _processor(
                case,
                control=control,
                input_client=ForbiddenInput(),
                output=Output(),
                acker=acker,
                context_factory=forbidden_context,
            ).process(case.delivery)

        assert control.settlements == 0
        assert acker.calls == 0

    asyncio.run(run())


def _processor(
    case,
    *,
    control,
    input_client,
    output,
    acker,
    context_factory,
    clock=None,
    output_factory=None,
    supervisor=None,
    lease_poll_interval_seconds=10,
):
    clock = clock or (lambda: _NOW)
    return IndexIngestDeliveryProcessor(
        supervisor=supervisor or InlineSupervisor(),
        client_context_factory=context_factory,
        control=control,
        command_acker=acker,
        input_client=input_client,
        input_request_builder=ClaimBoundInputRequestBuilder(
            origin="https://content.internal"
        ),
        output_session_factory=output_factory or (lambda: output),
        signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
        workload_session_id=_WORKLOAD,
        producer_id=_PRODUCER,
        clock_unix_millis=clock,
        lease_poll_interval_seconds=lease_poll_interval_seconds,
    )


def _pending_node_event(
    case: Case,
    *,
    fence: common_pb2.ExecutionFenceV1 | None = None,
    sequence: int = 1,
) -> output_pb2.ExecutionOutputFrameV1:
    envelope = envelope_pb2.WorkerExecutionEnvelopeV1(
        signed_command=case.signed,
        fence=fence if fence is not None else case.fence,
    )
    return build_node_event_output_frame(
        VerifiedWorkerCommand(envelope=envelope, command=case.command),
        node_event_pb2.NodeEventV1(
            type="agent_thinking_step",
            stream_id=case.command.execution_id,
            content=b"null",
            response_metadata=b"{}",
            references=b"[]",
        ),
        sequence=sequence,
        occurred_at_unix_millis=_NOW,
        claim_handoff_watermark=0,
    )


def _single_index_internal_failure(stderr: str) -> dict[str, Any]:
    lines = [line for line in stderr.splitlines() if line]
    assert len(lines) == 1
    diagnostic = json.loads(lines[0])
    assert set(diagnostic) in ({
        "event",
        "stage",
        "execution_id",
        "exception_module",
        "exception_name",
        "frames",
    }, {
        "event",
        "stage",
        "execution_id",
        "exception_module",
        "exception_name",
        "frames",
        "sdk_failure_category",
    })
    assert diagnostic["event"] == "index_ingest_internal_failure"
    return diagnostic


def _assert_safe_diagnostic_frames(frames: object) -> None:
    assert isinstance(frames, list)
    assert 0 < len(frames) <= 8
    for frame in frames:
        assert isinstance(frame, dict)
        assert set(frame) == {"file", "function", "line"}
        assert "/" not in frame["file"]
        assert "\\" not in frame["file"]
        assert isinstance(frame["function"], str)
        assert isinstance(frame["line"], int)


def _case() -> Case:
    sources = (
        ("toolkit-config", "index.toolkit_configuration", {"type": "github", "settings": {"token": "{{secret.GITHUB_TOKEN}}"}}, {"type": "github", "settings": {"token": "github-secret"}}),
        ("tool-params", "index.tool_parameters", {"index_name": "docs"}, {"index_name": "docs"}),
        ("llm-model", "index.llm_model", "gpt-test", "gpt-test"),
        ("llm-config", "index.llm_configuration", {"model_name": "gpt-test"}, {"model_name": "gpt-test"}),
        ("mcp-tokens", "index.mcp_tokens", {}, {}),
    )
    entries = []
    values: dict[str, bytes] = {}
    for ordinal, (entry_id, role, source, materialized) in enumerate(sources, start=1):
        raw = json.dumps(source, sort_keys=True, separators=(",", ":")).encode()
        version = f"sha256:{hashlib.sha256(raw).hexdigest()}"
        content_id = f"content-{ordinal}"
        entries.append(
            input_pb2.ExecutionInputEntryV1(
                entry_id=entry_id,
                immutable_version=version,
                semantic_role=role,
                content=input_pb2.ScopedContentReferenceV1(
                    content_id=content_id,
                    immutable_version=version,
                    media_type="application/json",
                    byte_length=len(raw),
                    digest=_digest(hashlib.sha256(raw).digest()),
                    classification="tenant-confidential",
                    required_grant_audience="elitea.runtime.input.read.v1",
                ),
            )
        )
        values[content_id] = json.dumps(
            materialized,
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
    manifest = input_pb2.ExecutionInputBundleV1(
        input_bundle_id="index-bundle-1",
        immutable_version="admission:index-bundle-1",
        entries=entries,
    )
    manifest_raw = manifest.SerializeToString(deterministic=True)
    command = command_pb2.WorkerCommandV1(
        protocol_revision="elitea.runtime.v1",
        command_id="index-command-1",
        idempotency_key="index-idempotency-1",
        command_type=command_pb2.WORKER_COMMAND_TYPE_V1_INDEX_INGEST,
        execution_id="index-execution-1",
        generation=1,
        dispatch_ordinal=1,
        root_execution_id="index-execution-1",
        tenant_id="tenant-1",
        resource_project_id="42",
        projection_project_id="42",
        principal_ref="user:7",
        input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
            input_bundle_id=manifest.input_bundle_id,
            immutable_version=manifest.immutable_version,
            digest=_digest(hashlib.sha256(manifest_raw).digest()),
            byte_length=len(manifest_raw),
            media_type="application/x-protobuf",
        ),
        capability_id="index.ingest.v1",
        capability_version="2",
        resource_class="indexing",
        isolation_class="shared",
        priority=1,
        deadline_unix_millis=_NOW + 60_000,
        limits_revision="elitea.runtime.limits.conformance.v2",
        index_ingest=indexing_pb2.IndexIngestCommandV1(
            toolkit_configuration_entry_id="toolkit-config",
            tool_parameters_entry_id="tool-params",
            llm_model_entry_id="llm-model",
            llm_configuration_entry_id="llm-config",
            mcp_tokens_entry_id="mcp-tokens",
            client_stream_id="conversation-1",
            client_message_id="message-1",
            sio_event="chat_predict",
            initiator="user",
        ),
    )
    command_raw = command.SerializeToString(deterministic=True)
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_HMAC_KEY_ID,
        worker_command_bytes=command_raw,
        worker_command_digest=_digest(hashlib.sha256(command_raw).digest()),
        signature=hmac.new(CONFORMANCE_HMAC_KEY, command_raw, hashlib.sha256).digest(),
    )
    return Case(
        command=command,
        signed=signed,
        manifest=manifest,
        fence=common_pb2.ExecutionFenceV1(
            workload_session_id=_WORKLOAD,
            claim_attempt=1,
            lease_epoch=1,
            producer_id=_PRODUCER,
            fence_token=b"f" * 32,
        ),
        values=values,
    )


def _embedding_case() -> Case:
    original = _case()
    manifest = input_pb2.ExecutionInputBundleV1.FromString(
        original.manifest.SerializeToString(deterministic=True)
    )
    values = dict(original.values)
    toolkit_source = {
        "type": "github",
        "settings": {
            "token": "{{secret.GITHUB_TOKEN}}",
            "embedding_model": "embedding-current",
        },
    }
    toolkit_raw = json.dumps(
        toolkit_source, sort_keys=True, separators=(",", ":")
    ).encode()
    toolkit_version = f"sha256:{hashlib.sha256(toolkit_raw).hexdigest()}"
    manifest.entries[0].immutable_version = toolkit_version
    manifest.entries[0].content.immutable_version = toolkit_version
    manifest.entries[0].content.byte_length = len(toolkit_raw)
    manifest.entries[0].content.digest.CopyFrom(
        _digest(hashlib.sha256(toolkit_raw).digest())
    )
    values["content-1"] = json.dumps(
        {
            "type": "github",
            "settings": {
                "token": "github-secret",
                "embedding_model": "embedding-current",
            },
        },
        sort_keys=True,
        separators=(",", ":"),
    ).encode()

    binding = {
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
    }
    binding_raw = json.dumps(binding, sort_keys=True, separators=(",", ":")).encode()
    binding_version = f"sha256:{hashlib.sha256(binding_raw).hexdigest()}"
    manifest.entries.append(
        input_pb2.ExecutionInputEntryV1(
            entry_id="embedding-binding",
            immutable_version=binding_version,
            semantic_role="index.embedding_binding",
            content=input_pb2.ScopedContentReferenceV1(
                content_id="content-6",
                immutable_version=binding_version,
                media_type="application/json",
                byte_length=len(binding_raw),
                digest=_digest(hashlib.sha256(binding_raw).digest()),
                classification="tenant-confidential",
                required_grant_audience="elitea.runtime.input.read.v1",
            ),
        )
    )
    values["content-6"] = binding_raw

    command = command_pb2.WorkerCommandV1.FromString(
        original.command.SerializeToString(deterministic=True)
    )
    command.index_ingest.embedding_binding.CopyFrom(
        indexing_pb2.IndexIngestInputBindingV1(
            entry_id="embedding-binding",
            immutable_version=binding_version,
            content_digest=_digest(hashlib.sha256(binding_raw).digest()),
        )
    )
    return _with_manifest(
        Case(
            command=command,
            signed=original.signed,
            manifest=original.manifest,
            fence=original.fence,
            values=values,
        ),
        manifest,
    )


def _scheduled_case() -> Case:
    original = _case()
    command = command_pb2.WorkerCommandV1.FromString(
        original.command.SerializeToString(deterministic=True)
    )
    command.index_ingest.initiator = "schedule"
    command.index_ingest.ClearField("client_stream_id")
    command.index_ingest.ClearField("client_message_id")
    command.index_ingest.ClearField("sio_event")
    return _resign_case(original, command)


def _resign_case(case: Case, command: command_pb2.WorkerCommandV1) -> Case:
    command_raw = command.SerializeToString(deterministic=True)
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_HMAC_KEY_ID,
        worker_command_bytes=command_raw,
        worker_command_digest=_digest(hashlib.sha256(command_raw).digest()),
        signature=hmac.new(
            CONFORMANCE_HMAC_KEY,
            command_raw,
            hashlib.sha256,
        ).digest(),
    )
    return Case(
        command=command,
        signed=signed,
        manifest=case.manifest,
        fence=case.fence,
        values=case.values,
    )


def _terminal_result() -> IndexIngestResult:
    case = _case()
    return IndexIngestResult(
        input_bundle_id="index-bundle-1",
        input_bundle_digest=hashlib.sha256(
            case.manifest.SerializeToString(deterministic=True)
        ).digest(),
        toolkit_configuration=IndexIngestInputBinding(
            "toolkit-config",
            case.manifest.entries[0].immutable_version,
            bytes(case.manifest.entries[0].content.digest.value),
        ),
        tool_parameters=IndexIngestInputBinding(
            "tool-params",
            case.manifest.entries[1].immutable_version,
            bytes(case.manifest.entries[1].content.digest.value),
        ),
        llm_model=IndexIngestInputBinding(
            "llm-model",
            case.manifest.entries[2].immutable_version,
            bytes(case.manifest.entries[2].content.digest.value),
        ),
        llm_configuration=IndexIngestInputBinding(
            "llm-config",
            case.manifest.entries[3].immutable_version,
            bytes(case.manifest.entries[3].content.digest.value),
        ),
        mcp_tokens=IndexIngestInputBinding(
            "mcp-tokens",
            case.manifest.entries[4].immutable_version,
            bytes(case.manifest.entries[4].content.digest.value),
        ),
        sdk_result={"success": True, "result": {"status": "ok", "message": "done"}},
    )


def _with_manifest(
    case: Case,
    manifest: input_pb2.ExecutionInputBundleV1,
) -> Case:
    command = command_pb2.WorkerCommandV1.FromString(
        case.command.SerializeToString(deterministic=True)
    )
    manifest_raw = manifest.SerializeToString(deterministic=True)
    command.input_bundle_ref.byte_length = len(manifest_raw)
    command.input_bundle_ref.digest.CopyFrom(
        _digest(hashlib.sha256(manifest_raw).digest())
    )
    command_raw = command.SerializeToString(deterministic=True)
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_HMAC_KEY_ID,
        worker_command_bytes=command_raw,
        worker_command_digest=_digest(hashlib.sha256(command_raw).digest()),
        signature=hmac.new(
            CONFORMANCE_HMAC_KEY,
            command_raw,
            hashlib.sha256,
        ).digest(),
    )
    return Case(
        command=command,
        signed=signed,
        manifest=manifest,
        fence=case.fence,
        values=case.values,
    )


def _digest(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )


def _identity(command: command_pb2.WorkerCommandV1) -> common_pb2.ExecutionIdentityV1:
    return common_pb2.ExecutionIdentityV1(
        tenant_id=command.tenant_id,
        resource_project_id=command.resource_project_id,
        projection_project_id=command.projection_project_id,
        command_id=command.command_id,
        execution_id=command.execution_id,
        generation=command.generation,
    )
