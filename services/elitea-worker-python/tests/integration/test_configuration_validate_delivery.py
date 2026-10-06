from __future__ import annotations

import asyncio
import base64
import hashlib
import threading
from functools import lru_cache
from pathlib import Path

import httpx
import pytest
from elitea.runtime.v1 import (
    command_pb2,
    common_pb2,
    control_pb2,
    envelope_pb2,
    errors_pb2,
    input_pb2,
    output_pb2,
)

from elitea_worker.agents.sdk_adapter import EliteaSdkAdapter
from elitea_worker.constants import CONFORMANCE_OCCURRED_AT_UNIX_MILLIS
from elitea_worker.execution.delivery import (
    ConfigurationValidationDeliveryProcessor,
    DeliveryDisposition,
)
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DependencyUnavailable,
    ExecutionCancelled,
    InvalidInput,
)
from elitea_worker.execution.supervisor import ExecutionSupervisor
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.protocol.codec import (
    TestOnlyConformanceHmacAuthenticator,
    VerifiedWorkerCommand,
    build_output_frame,
)
from elitea_worker.transport.input_content import (
    ClaimBoundInputRequestBuilder,
    ScopedInputContentClient,
)
from elitea_worker.transport.output_grpc import OutputGrpcSession
from elitea_worker.transport.output_spool import EncryptedOutputSpool
from elitea_worker.transport.nats_jetstream import CommandDelivery

def _command_delivery(
    stream: str,
    entry_id: str,
    fields: dict[str, bytes],
) -> CommandDelivery:
    """A delivered command as the JetStream consumer hands it over.

    The processors read only the signed envelope; the coordinates are what
    the serve loop and the acker key on.
    """

    assert set(fields) == {"signed_envelope"}
    return CommandDelivery(
        stream="ELITEA_RT_V1_VALIDATE",
        consumer="elitea-configuration-worker-v1",
        subject=f"elitea.rt.v1.validate.d.{hashlib.sha256(f'{stream}/{entry_id}'.encode()).hexdigest()}",
        stream_sequence=int(entry_id.split("-")[0]) if entry_id.split("-")[0].isdigit() else 1,
        num_delivered=1,
        signed_envelope=fields["signed_envelope"],
    )


_ROOT = Path(__file__).parents[4]
_FIXTURES = _ROOT / "testdata/proto/runtime/v1/configuration-validation"
_WORKLOAD_SESSION = "workload-session-conformance-v1"
_PRODUCER = "python-reference-conformance-v1"


@lru_cache(maxsize=1)
def _handler() -> ConfigurationValidationHandler:
    return ConfigurationValidationHandler(EliteaSdkAdapter())


class _BindingAwareHandler:
    @staticmethod
    def validate_binding(**kwargs) -> None:
        _handler().validate_binding(**kwargs)


class InlineSupervisor:
    """Preserves the pre-executor behavior in transport-focused tests."""

    async def run(self, operation):
        return await operation()

    async def run_sync(self, operation, /, *args, **kwargs):
        return operation(*args, **kwargs)


class BlockingHandler(_BindingAwareHandler):
    def __init__(self) -> None:
        self.started = threading.Event()
        self.release = threading.Event()
        self.calls = 0

    def execute(self, request):
        self.calls += 1
        self.started.set()
        assert self.release.wait(timeout=2)
        return _handler().execute(request)


class Acker:
    def __init__(self) -> None:
        self.acked: list[CommandDelivery] = []
        self.stable_delivery_ids: list[str] = []

    async def ack_after_settlement(
        self,
        delivery: CommandDelivery,
        stable_delivery_id: str,
    ) -> None:
        signed = envelope_pb2.SignedWorkerCommandEnvelopeV1.FromString(
            delivery.signed_envelope
        )
        command = command_pb2.WorkerCommandV1.FromString(
            signed.worker_command_bytes
        )
        assert stable_delivery_id == command.idempotency_key
        self.acked.append(delivery)
        self.stable_delivery_ids.append(stable_delivery_id)


class Control:
    def __init__(
        self,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
        manifest: input_pb2.ExecutionInputBundleV1,
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.manifest = manifest
        self.claims: list[control_pb2.ClaimCommandRequestV1] = []
        self.renewals: list[control_pb2.RenewLeaseRequestV1] = []
        self.observations: list[control_pb2.ObserveDesiredStateRequestV1] = []
        self.settlements: list[control_pb2.PrepareSettlementRequestV1] = []

    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        self.claims.append(request)
        assert request.workload_session_id == _WORKLOAD_SESSION
        assert request.producer_id == _PRODUCER
        assert request.signed_command == self.envelope.signed_command
        return control_pb2.ClaimCommandResponseV1(
            receipt=control_pb2.ClaimReceiptV1(
                disposition=control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED,
                claim_id="claim-conformance-v1",
                identity=common_pb2.ExecutionIdentityV1(
                    tenant_id=self.command.tenant_id,
                    resource_project_id=self.command.resource_project_id,
                    projection_project_id=self.command.projection_project_id,
                    command_id=self.command.command_id,
                    execution_id=self.command.execution_id,
                    generation=self.command.generation,
                ),
                fence=self.envelope.fence,
                lease_expires_at_unix_millis=CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 60_000,
                # Rust state fencing consumes this additive receipt field. The
                # Python SDK/LangGraph execution path deliberately ignores it.
                claim_started_at_unix_micros=CONFORMANCE_OCCURRED_AT_UNIX_MILLIS * 1_000,
                input_bundle_ref=self.command.input_bundle_ref,
                input_bundle=self.manifest,
                desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            )
        )

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        self.renewals.append(request)
        assert request.identity.execution_id == self.command.execution_id
        assert request.fence == self.envelope.fence
        assert request.idempotency_key.startswith("lease-renew:")
        return control_pb2.RenewLeaseResponseV1(
            lease_expires_at_unix_millis=(
                CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 120_000
            ),
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        )

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        self.observations.append(request)
        assert request.identity.execution_id == self.command.execution_id
        assert request.fence == self.envelope.fence
        return control_pb2.ObserveDesiredStateResponseV1(
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        )

    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1:
        self.settlements.append(request)
        proposal = request.proposal.SerializeToString(deterministic=True)
        assert request.proposal_digest.algorithm == common_pb2.DIGEST_ALGORITHM_V1_SHA256
        assert request.proposal_digest.value == hashlib.sha256(proposal).digest()
        return control_pb2.PrepareSettlementResponseV1(
            settlement_receipt_id="settlement-conformance-v1",
            outcome=request.proposal.requested_outcome,
        )


class FakeOutputCall:
    def __init__(self) -> None:
        self.controls: asyncio.Queue[output_pb2.ExecutionOutputAckV1 | None] = (
            asyncio.Queue()
        )
        self.write_started = asyncio.Event()
        self.write_gate = asyncio.Event()
        self.frames: list[output_pb2.ExecutionOutputFrameV1] = []
        self.received_bound_control_while_write_blocked = False
        self.cancelled = False

    def __aiter__(self):
        return self

    async def __anext__(self) -> output_pb2.ExecutionOutputAckV1:
        value = await self.controls.get()
        if value is None:
            raise StopAsyncIteration
        return value

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        await self.write_gate.wait()
        self.frames.append(frame)
        await self.controls.put(_bound_ack(frame, committed=frame.sequence))

    async def done_writing(self) -> None:
        await self.controls.put(None)

    def cancel(self) -> bool:
        self.cancelled = True
        return True


class OutputStub:
    def __init__(self, call: FakeOutputCall) -> None:
        self.call = call
        self.invocation: dict[str, object] | None = None

    def Publish(self, **kwargs: object) -> FakeOutputCall:
        self.invocation = kwargs
        # The server grants a single identity-free bootstrap credit. Every
        # subsequent control/ACK is bound to the transmitted frame identity.
        self.call.controls.put_nowait(
            output_pb2.ExecutionOutputAckV1(
                credit_frames=1,
                credit_bytes=64 * 1024,
                desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            )
        )
        return self.call


class ImmediateOutput:
    def __init__(self, *, on_ack=None) -> None:
        self.frame: output_pb2.ExecutionOutputFrameV1 | None = None
        self.on_ack = on_ack

    @property
    def has_pending_replay(self) -> bool:
        return False

    @property
    def pending_replay_frame(self) -> output_pb2.ExecutionOutputFrameV1 | None:
        return None

    def replays(self, frame: output_pb2.ExecutionOutputFrameV1) -> bool:
        return False

    async def start(self) -> None:
        return None

    async def send(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.frame = frame

    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None:
        assert self.frame is not None and sequence == self.frame.sequence
        assert timeout_seconds > 0
        if self.on_ack is not None:
            self.on_ack()

    async def close(self) -> None:
        return None


class CrashAfterWriteCall(FakeOutputCall):
    def __init__(self) -> None:
        super().__init__()
        self.write_gate.set()

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        await self.write_gate.wait()
        self.frames.append(frame)
        await self.controls.put(None)


class AckReplayCall(FakeOutputCall):
    def __init__(self) -> None:
        super().__init__()
        self.write_gate.set()


class CancellationWinnerCall(FakeOutputCall):
    def __init__(self) -> None:
        super().__init__()
        self.write_gate.set()

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        self.frames.append(frame)
        await self.controls.put(_bound_cancellation_winner(frame))


class DeadlineWinnerCall(FakeOutputCall):
    def __init__(self) -> None:
        super().__init__()
        self.write_gate.set()

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        self.frames.append(frame)
        await self.controls.put(_bound_deadline_winner(frame))


class DependencyRejectionCall(FakeOutputCall):
    def __init__(self, *, retryable: bool) -> None:
        super().__init__()
        self.retryable = retryable
        self.write_gate.set()

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        self.frames.append(frame)
        ack = _bound_ack(frame, committed=0)
        ack.credit_frames = 0
        ack.credit_bytes = 0
        ack.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_UNSPECIFIED
        ack.rejection.code = (
            errors_pb2.RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE
        )
        ack.rejection.safe_message = (
            "The index metadata terminal projection is temporarily unavailable."
        )
        ack.rejection.retryable = self.retryable
        await self.controls.put(ack)


class StaleFenceCall(FakeOutputCall):
    def __init__(self) -> None:
        super().__init__()
        self.write_gate.set()

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        self.write_started.set()
        self.frames.append(frame)
        ack = _bound_ack(frame, committed=0)
        ack.credit_frames = 0
        ack.credit_bytes = 0
        ack.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_UNSPECIFIED
        ack.rejection.code = errors_pb2.RUNTIME_ERROR_CODE_V1_STALE_FENCE
        ack.rejection.safe_message = "The output fence is no longer current."
        await self.controls.put(ack)


class CrashThenReplayFactory:
    def __init__(self, spool: EncryptedOutputSpool) -> None:
        self.spool = spool
        self.calls: list[FakeOutputCall] = []

    def __call__(self) -> OutputGrpcSession:
        call: FakeOutputCall
        if not self.calls:
            call = CrashAfterWriteCall()
        else:
            call = AckReplayCall()
        self.calls.append(call)
        return OutputGrpcSession(
            OutputStub(call),
            spool=self.spool,
            metadata=lambda: (("x-elitea-workload-session", _WORKLOAD_SESSION),),
            max_queued_frames=2,
            max_queued_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )


class CancellationThenReplayFactory:
    def __init__(self, spool: EncryptedOutputSpool) -> None:
        self.spool = spool
        self.calls: list[FakeOutputCall] = []

    def __call__(self) -> OutputGrpcSession:
        call: FakeOutputCall = (
            CancellationWinnerCall() if not self.calls else AckReplayCall()
        )
        self.calls.append(call)
        return OutputGrpcSession(
            OutputStub(call),
            spool=self.spool,
            metadata=lambda: (("x-elitea-workload-session", _WORKLOAD_SESSION),),
            max_queued_frames=2,
            max_queued_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )


class DeadlineThenReplayFactory:
    def __init__(self, spool: EncryptedOutputSpool) -> None:
        self.spool = spool
        self.calls: list[FakeOutputCall] = []

    def __call__(self) -> OutputGrpcSession:
        call: FakeOutputCall = (
            DeadlineWinnerCall() if not self.calls else AckReplayCall()
        )
        self.calls.append(call)
        return OutputGrpcSession(
            OutputStub(call),
            spool=self.spool,
            metadata=lambda: (("x-elitea-workload-session", _WORKLOAD_SESSION),),
            max_queued_frames=2,
            max_queued_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )


class DependencyRejectionThenReplayFactory:
    def __init__(
        self, spool: EncryptedOutputSpool, *, retryable: bool
    ) -> None:
        self.spool = spool
        self.retryable = retryable
        self.calls: list[FakeOutputCall] = []

    def __call__(self) -> OutputGrpcSession:
        call: FakeOutputCall = (
            DependencyRejectionCall(retryable=self.retryable)
            if not self.calls
            else AckReplayCall()
        )
        self.calls.append(call)
        return OutputGrpcSession(
            OutputStub(call),
            spool=self.spool,
            metadata=lambda: (("x-elitea-workload-session", _WORKLOAD_SESSION),),
            max_queued_frames=2,
            max_queued_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )


class CountingInput:
    def __init__(self, content: bytes, *, forbidden: bool = False) -> None:
        self.content = content
        self.forbidden = forbidden
        self.calls = 0

    async def fetch(self, grant) -> bytes:
        if self.forbidden:
            raise AssertionError("recovery must not fetch business input")
        self.calls += 1
        return self.content


class MutableClock:
    def __init__(self, value: int) -> None:
        self.value = value

    def __call__(self) -> int:
        return self.value


class DeadlineControl(Control):
    def __init__(
        self,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
        manifest: input_pb2.ExecutionInputBundleV1,
        *,
        clock: MutableClock,
        expire_on_observation: int | None = None,
    ) -> None:
        super().__init__(command, envelope, manifest)
        self.clock = clock
        self.expire_on_observation = expire_on_observation
        self.observation_count = 0

    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        response = await super().claim_command(request)
        response.receipt.lease_expires_at_unix_millis = (
            self.command.deadline_unix_millis + 3_000_000
        )
        return response

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        response = await super().renew_lease(request)
        response.lease_expires_at_unix_millis = (
            self.command.deadline_unix_millis + 3_000_000
        )
        return response

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        response = await super().observe_desired_state(request)
        self.observation_count += 1
        if self.expire_on_observation == self.observation_count:
            self.clock.value = int(self.command.deadline_unix_millis)
        return response


class DeadlineAdvancingInput(CountingInput):
    def __init__(self, content: bytes, *, clock: MutableClock, deadline: int) -> None:
        super().__init__(content)
        self.clock = clock
        self.deadline = deadline

    async def fetch(self, grant) -> bytes:
        content = await super().fetch(grant)
        self.clock.value = self.deadline
        return content


class CountingHandler(_BindingAwareHandler):
    def __init__(
        self,
        *,
        clock: MutableClock | None = None,
        deadline: int | None = None,
        forbidden: bool = False,
    ) -> None:
        self.clock = clock
        self.deadline = deadline
        self.forbidden = forbidden
        self.calls = 0

    def execute(self, request):
        if self.forbidden:
            raise AssertionError("deadline-expired work must not enter the SDK")
        self.calls += 1
        result = _handler().execute(request)
        if self.clock is not None:
            assert self.deadline is not None
            self.clock.value = self.deadline
        return result


class LostSettlementResponseControl(Control):
    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1:
        self.settlements.append(request)
        raise DependencyUnavailable("Simulated crash or lost settlement response.")


class ActiveLeaseControl(Control):
    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        response = await super().claim_command(request)
        response.receipt.disposition = (
            control_pb2.CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK
        )
        response.receipt.ClearField("input_bundle_ref")
        response.receipt.ClearField("input_bundle")
        return response


class ActiveCancelledLeaseControl(ActiveLeaseControl):
    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        response = await super().claim_command(request)
        response.receipt.desired_state = (
            common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
        )
        return response

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        response = await super().renew_lease(request)
        response.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
        return response

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        response = await super().observe_desired_state(request)
        response.desired_state = common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
        return response


class RecoveryControl:
    def __init__(
        self,
        *,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
        disposition: int,
        recovery: control_pb2.SettlementRecoveryV1 | None,
        expected_prepare: control_pb2.PrepareSettlementRequestV1 | None,
        desired_state: int = common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        retirement: errors_pb2.RuntimeErrorV1 | None = None,
        include_authority: bool = True,
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.disposition = disposition
        self.recovery = recovery
        self.expected_prepare = expected_prepare
        self.desired_state = desired_state
        self.retirement = retirement
        self.include_authority = include_authority
        self.prepare_calls = 0

    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        assert request.signed_command == self.envelope.signed_command
        receipt = control_pb2.ClaimReceiptV1(
            disposition=self.disposition,
            identity=common_pb2.ExecutionIdentityV1(
                tenant_id=self.command.tenant_id,
                resource_project_id=self.command.resource_project_id,
                projection_project_id=self.command.projection_project_id,
                command_id=self.command.command_id,
                execution_id=self.command.execution_id,
                generation=self.command.generation,
            ),
            desired_state=self.desired_state,
        )
        if self.include_authority:
            receipt.fence.CopyFrom(self.envelope.fence)
            receipt.lease_expires_at_unix_millis = (
                CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 60_000
            )
            receipt.claim_id = "claim-recovery-v1"
        if self.recovery is not None:
            receipt.settlement_recovery.CopyFrom(self.recovery)
        if self.retirement is not None:
            receipt.retirement.CopyFrom(self.retirement)
        return control_pb2.ClaimCommandResponseV1(receipt=receipt)

    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1:
        self.prepare_calls += 1
        assert self.expected_prepare is not None
        assert request.proposal == self.expected_prepare.proposal
        assert request.proposal_digest == self.expected_prepare.proposal_digest
        assert request.idempotency_key == self.expected_prepare.idempotency_key
        return control_pb2.PrepareSettlementResponseV1(
            settlement_receipt_id="settlement-recovered-v1",
            outcome=request.proposal.requested_outcome,
        )


def _bound_ack(
    frame: output_pb2.ExecutionOutputFrameV1,
    *,
    committed: int,
) -> output_pb2.ExecutionOutputAckV1:
    return output_pb2.ExecutionOutputAckV1(
        stream_id=frame.stream_id,
        identity=frame.identity,
        fence=frame.fence,
        committed_contiguous_sequence=committed,
        claim_handoff_watermark=frame.claim_handoff_watermark,
        credit_frames=1,
        credit_bytes=64 * 1024,
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
    )


def _bound_cancellation_winner(
    frame: output_pb2.ExecutionOutputFrameV1,
) -> output_pb2.ExecutionOutputAckV1:
    ack = output_pb2.ExecutionOutputAckV1(
        stream_id=frame.stream_id,
        identity=frame.identity,
        fence=frame.fence,
        claim_handoff_watermark=frame.claim_handoff_watermark,
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
    )
    ack.rejection.code = errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
    ack.rejection.safe_message = (
        "Execution cancellation won before this output became durable."
    )
    return ack


def _bound_deadline_winner(
    frame: output_pb2.ExecutionOutputFrameV1,
) -> output_pb2.ExecutionOutputAckV1:
    ack = output_pb2.ExecutionOutputAckV1(
        stream_id=frame.stream_id,
        identity=frame.identity,
        fence=frame.fence,
        claim_handoff_watermark=frame.claim_handoff_watermark,
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
    )
    ack.rejection.code = errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
    ack.rejection.safe_message = "The execution deadline was exceeded."
    ack.rejection.retryable = True
    return ack


def _valid_delivery_case():
    fixture = _FIXTURES / "valid"
    envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
        (fixture / "envelope.pb").read_bytes()
    )
    command = command_pb2.WorkerCommandV1.FromString(
        envelope.signed_command.worker_command_bytes
    )
    manifest = input_pb2.ExecutionInputBundleV1.FromString(
        (fixture / "input-bundle.pb").read_bytes()
    )
    delivery = _command_delivery(
        "configuration-validation.v1",
        "sync-executor-1-0",
        {
            "signed_envelope": envelope.signed_command.SerializeToString(
                deterministic=True
            )
        },
    )
    return envelope, command, manifest, (fixture / "settings.json").read_bytes(), delivery


def _deadline_test_processor(
    *,
    control: DeadlineControl,
    clock: MutableClock,
    input_client,
    handler,
    output: ImmediateOutput,
    acker: Acker,
) -> ConfigurationValidationDeliveryProcessor:
    return ConfigurationValidationDeliveryProcessor(
        supervisor=InlineSupervisor(),
        handler=handler,
        control=control,
        command_acker=acker,
        input_client=input_client,
        input_request_builder=ClaimBoundInputRequestBuilder(
            origin="https://content.test"
        ),
        output_session_factory=lambda: output,
        signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
        workload_session_id=_WORKLOAD_SESSION,
        producer_id=_PRODUCER,
        clock_unix_millis=clock,
        lease_poll_interval_seconds=1_000.0,
    )


async def _wait_until(predicate, *, timeout_seconds: float = 1.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while not predicate():
        if asyncio.get_running_loop().time() >= deadline:
            raise AssertionError("condition was not reached before the test deadline")
        await asyncio.sleep(0.001)


@pytest.mark.parametrize("name", ["valid", "invalid", "unsupported"])
def test_same_golden_corpus_crosses_claim_content_output_and_settlement(
    name: str,
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / name
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        settings = (fixture / "settings.json").read_bytes()
        content = manifest.entries[0].content
        observed_requests: list[httpx.Request] = []

        def content_endpoint(request: httpx.Request) -> httpx.Response:
            observed_requests.append(request)
            assert request.url.path == (
                f"/executions/{command.execution_id}/generations/{command.generation}"
                f"/inputs/{content.content_id}/versions/{content.immutable_version}"
            )
            assert request.headers["x-elitea-claim-id"] == "claim-conformance-v1"
            assert request.headers["x-elitea-fence"] == base64.urlsafe_b64encode(
                envelope.fence.fence_token
            ).rstrip(b"=").decode("ascii")
            digest = base64.b64encode(hashlib.sha256(settings).digest()).decode("ascii")
            return httpx.Response(
                200,
                headers={
                    "Cache-Control": "private, no-store",
                    "Content-Digest": f"sha-256=:{digest}:",
                    "Content-Length": str(len(settings)),
                    "Content-Type": "application/json",
                },
                content=settings,
                request=request,
            )

        control = Control(command, envelope, manifest)
        acker = Acker()
        output_call = FakeOutputCall()
        output_stub = OutputStub(output_call)
        spool = EncryptedOutputSpool(
            tmp_path / f"spool-{name}",
            key=b"s" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        async with httpx.AsyncClient(transport=httpx.MockTransport(content_endpoint)) as http:
            processor = ConfigurationValidationDeliveryProcessor(
                supervisor=InlineSupervisor(),
                handler=_handler(),
                control=control,
                command_acker=acker,
                input_client=ScopedInputContentClient(
                    http,
                    allowed_origins=frozenset({"https://content.test"}),
                    max_content_bytes=256 * 1024,
                ),
                input_request_builder=ClaimBoundInputRequestBuilder(
                    origin="https://content.test"
                ),
                output_session_factory=lambda: OutputGrpcSession(
                    output_stub,
                    spool=spool,
                    metadata=lambda: (
                        ("x-elitea-workload-session", _WORKLOAD_SESSION),
                    ),
                    max_queued_frames=2,
                    max_queued_bytes=128 * 1024,
                    max_frame_bytes=64 * 1024,
                ),
                signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
                workload_session_id=_WORKLOAD_SESSION,
                producer_id=_PRODUCER,
                clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            )

            async def exercise_independent_receive() -> None:
                await output_call.write_started.wait()
                assert output_call.frames == []
                # A bound credit/control update is received while the send
                # coroutine remains blocked in its independent write path.
                probe = output_pb2.ExecutionOutputFrameV1.FromString(
                    (fixture / "expected-output.pb").read_bytes()
                )
                await output_call.controls.put(_bound_ack(probe, committed=0))
                while not output_call.controls.empty():
                    await asyncio.sleep(0)
                output_call.received_bound_control_while_write_blocked = True
                output_call.write_gate.set()

            unblock = asyncio.create_task(exercise_independent_receive())
            delivery = _command_delivery(
                stream="configuration-validation.v1",
                entry_id="1-0",
                fields={
                    "signed_envelope": envelope.signed_command.SerializeToString(
                        deterministic=True
                    )
                },
            )
            result = await processor.process(delivery)
            await unblock

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.SerializeToString(deterministic=True) == (
            fixture / "expected-output.pb"
        ).read_bytes()
        assert result.output_frame.fence == envelope.fence
        assert acker.acked == [delivery]
        assert len(control.claims) == 1
        assert len(control.renewals) >= 1
        assert len(control.observations) >= 1
        assert len(control.settlements) == 1
        assert control.settlements[0].fence == envelope.fence
        assert output_call.frames == [result.output_frame]
        assert output_call.received_bound_control_while_write_blocked
        assert output_call.cancelled
        assert spool.pending() == ()
        assert output_stub.invocation == {
            "timeout": 300.0,
            "metadata": (("x-elitea-workload-session", _WORKLOAD_SESSION),),
        }
        assert len(observed_requests) == (0 if name == "unsupported" else 1)

    asyncio.run(run())


def test_bus_redelivery_resumes_same_fence_spool_before_input_or_sdk(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        expected_bytes = (fixture / "expected-output.pb").read_bytes()
        expected = output_pb2.ExecutionOutputFrameV1.FromString(expected_bytes)
        spool = EncryptedOutputSpool(
            tmp_path / "redelivery-spool",
            key=b"d" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(expected.sequence, expected_bytes)
        output_call = AckReplayCall()
        control = ActiveLeaseControl(command, envelope, manifest)
        acker = Acker()

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("durable output recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: OutputGrpcSession(
                OutputStub(output_call),
                spool=spool,
                metadata=lambda: (
                    ("x-elitea-workload-session", _WORKLOAD_SESSION),
                ),
                max_queued_frames=2,
                max_queued_bytes=128 * 1024,
                max_frame_bytes=64 * 1024,
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "2-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert (
            result.disposition
            is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        )
        assert result.output_frame is not None
        assert result.output_frame.SerializeToString(deterministic=True) == expected_bytes
        assert result.output_frame.fence == envelope.fence
        assert len(output_call.frames) == 1
        assert output_call.frames[0].SerializeToString(deterministic=True) == expected_bytes
        assert len(control.settlements) == 1
        assert control.settlements[0].fence == envelope.fence
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_replacement_fence_spool_fails_closed_before_input_sdk_or_output(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        expected = output_pb2.ExecutionOutputFrameV1.FromString(
            (fixture / "expected-output.pb").read_bytes()
        )
        spool = EncryptedOutputSpool(
            tmp_path / "replacement-fence-spool",
            key=b"f" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(
            expected.sequence,
            expected.SerializeToString(deterministic=True),
        )
        replacement = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            envelope.SerializeToString(deterministic=True)
        )
        replacement.fence.claim_attempt += 1
        replacement.fence.lease_epoch += 1
        replacement.fence.fence_token = b"n" * 32
        control = ActiveLeaseControl(command, replacement, manifest)
        output_call = AckReplayCall()
        acker = Acker()

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("replacement-fence recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: OutputGrpcSession(
                OutputStub(output_call),
                spool=spool,
                metadata=lambda: (
                    ("x-elitea-workload-session", _WORKLOAD_SESSION),
                ),
                max_queued_frames=2,
                max_queued_bytes=128 * 1024,
                max_frame_bytes=64 * 1024,
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "2-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        with pytest.raises(AuthorizationFailure, match="different claim fence"):
            await processor.process(delivery)

        assert output_call.frames == []
        assert control.renewals == []
        assert control.observations == []
        assert control.settlements == []
        assert acker.acked == []
        assert spool.pending() != ()

    asyncio.run(run())


def test_active_lease_without_spool_never_reexecutes_business_logic() -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        control = ActiveLeaseControl(command, envelope, manifest)
        acker = Acker()

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("an active duplicate must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=ImmediateOutput,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "2-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.OWNED_ELSEWHERE_NOACK
        assert control.renewals == []
        assert control.observations == []
        assert control.settlements == []
        assert acker.acked == []

    asyncio.run(run())


def test_slow_fetch_renews_lease_and_observes_cancellation_before_sdk() -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )

        class CancellingControl(Control):
            async def observe_desired_state(
                self, request: control_pb2.ObserveDesiredStateRequestV1
            ) -> control_pb2.ObserveDesiredStateResponseV1:
                response = await super().observe_desired_state(request)
                if len(self.observations) >= 2:
                    response.desired_state = (
                        common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
                    )
                return response

        class SlowInput(CountingInput):
            async def fetch(self, grant) -> bytes:
                self.calls += 1
                await asyncio.sleep(0.04)
                return self.content

        class ForbiddenHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.calls = 0

            def execute(self, request):
                self.calls += 1
                raise AssertionError("observed cancellation must prevent SDK validation")

        control = CancellingControl(command, envelope, manifest)
        settings = SlowInput((fixture / "settings.json").read_bytes())
        handler = ForbiddenHandler()
        output = ImmediateOutput()
        acker = Acker()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=settings,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.005,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "1-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert settings.calls == 1
        assert handler.calls == 0
        assert len(control.renewals) >= 2
        assert len(control.observations) >= 2
        assert output.frame is not None
        assert (
            output.frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_delivery_admission_saturation_before_claim_returns_retry_without_ack() -> None:
    async def run() -> None:
        envelope, command, manifest, raw_settings, delivery = _valid_delivery_case()
        occupied_started = asyncio.Event()
        occupied_release = asyncio.Event()

        async def occupy_delivery() -> None:
            occupied_started.set()
            await occupied_release.wait()

        class ForbiddenHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.calls = 0

            def execute(self, request):
                self.calls += 1
                raise AssertionError("saturated admission must not run business logic")

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.02,
            drain_timeout_seconds=2,
        )
        occupied = asyncio.create_task(supervisor.run(occupy_delivery))
        await occupied_started.wait()
        control = Control(command, envelope, manifest)
        acker = Acker()
        settings = CountingInput(raw_settings, forbidden=True)
        handler = ForbiddenHandler()
        output_factory_calls = 0

        def forbidden_output_factory() -> ImmediateOutput:
            nonlocal output_factory_calls
            output_factory_calls += 1
            raise AssertionError("rejected delivery must not open an output session")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=supervisor,
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=settings,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=forbidden_output_factory,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.005,
        )

        try:
            result = await processor.process(delivery)
        finally:
            occupied_release.set()
            await occupied
            await supervisor.shutdown()

        assert result.disposition is DeliveryDisposition.RETRY_LATER_NOACK
        assert control.claims == []
        assert control.renewals == []
        assert control.observations == []
        assert control.settlements == []
        assert settings.calls == 0
        assert handler.calls == 0
        assert output_factory_calls == 0
        assert acker.acked == []

    asyncio.run(run())


def test_lease_margin_is_checked_after_observation_and_beats_cancellation() -> None:
    async def run() -> None:
        envelope, command, manifest, _, delivery = _valid_delivery_case()
        clock = [CONFORMANCE_OCCURRED_AT_UNIX_MILLIS]

        class ExpiringControl(Control):
            async def renew_lease(
                self, request: control_pb2.RenewLeaseRequestV1
            ) -> control_pb2.RenewLeaseResponseV1:
                response = await super().renew_lease(request)
                response.lease_expires_at_unix_millis = (
                    CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 60_005
                )
                return response

            async def observe_desired_state(
                self, request: control_pb2.ObserveDesiredStateRequestV1
            ) -> control_pb2.ObserveDesiredStateResponseV1:
                response = await super().observe_desired_state(request)
                clock[0] = CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 60_000
                response.desired_state = (
                    common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
                )
                return response

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("an inadequately renewed lease must not execute")

        control = ExpiringControl(command, envelope, manifest)
        output = ImmediateOutput()
        acker = Acker()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: clock[0],
            lease_poll_interval_seconds=0.005,
        )

        with pytest.raises(AuthorizationFailure, match="malformed or expired"):
            await processor.process(delivery)

        assert len(control.claims) == 1
        assert len(control.renewals) == 1
        assert len(control.observations) == 1
        assert control.settlements == []
        assert output.frame is None
        assert acker.acked == []

    asyncio.run(run())


def test_slow_control_calls_do_not_accumulate_lease_poll_drift() -> None:
    async def run() -> None:
        envelope, command, manifest, raw_settings, delivery = _valid_delivery_case()

        class DelayedControl(Control):
            def __init__(self) -> None:
                super().__init__(command, envelope, manifest)
                self.renewal_started_at: list[float] = []

            async def renew_lease(
                self, request: control_pb2.RenewLeaseRequestV1
            ) -> control_pb2.RenewLeaseResponseV1:
                self.renewal_started_at.append(asyncio.get_running_loop().time())
                await asyncio.sleep(0.03)
                return await super().renew_lease(request)

            async def observe_desired_state(
                self, request: control_pb2.ObserveDesiredStateRequestV1
            ) -> control_pb2.ObserveDesiredStateResponseV1:
                await asyncio.sleep(0.03)
                return await super().observe_desired_state(request)

        class BlockingFailureHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.started = threading.Event()
                self.release = threading.Event()

            def execute(self, request):
                self.started.set()
                assert self.release.wait(timeout=2)
                raise InvalidInput("fixed-cadence-test")

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        control = DelayedControl()
        handler = BlockingFailureHandler()
        output = ImmediateOutput()
        acker = Acker()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=supervisor,
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=CountingInput(raw_settings),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.1,
        )
        processing = asyncio.create_task(processor.process(delivery))

        try:
            await _wait_until(handler.started.is_set)
            await _wait_until(
                lambda: len(control.renewal_started_at) >= 4,
                timeout_seconds=1,
            )
            # Exclude the immediate admission check. Three scheduled starts span
            # two 100 ms periods; call latency must not be added to each period.
            assert (
                control.renewal_started_at[3] - control.renewal_started_at[1]
                < 0.27
            )
            handler.release.set()
            result = await processing
        finally:
            handler.release.set()
            if not processing.done():
                await processing
            await supervisor.shutdown()

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert output.frame is not None
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_process_cancellation_keeps_lease_until_running_thread_exits() -> None:
    async def run() -> None:
        envelope, command, manifest, raw_settings, delivery = _valid_delivery_case()
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        control = Control(command, envelope, manifest)
        acker = Acker()
        handler = BlockingHandler()
        output = ImmediateOutput()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=supervisor,
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=CountingInput(raw_settings),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.005,
        )
        processing = asyncio.create_task(processor.process(delivery))

        try:
            await _wait_until(handler.started.is_set)
            renewals_before_cancel = len(control.renewals)
            processing.cancel()
            await _wait_until(lambda: len(control.renewals) > renewals_before_cancel)

            assert not processing.done()
            assert handler.calls == 1
            assert output.frame is None
            assert control.settlements == []
            assert acker.acked == []

            handler.release.set()
            with pytest.raises(asyncio.CancelledError):
                await processing
        finally:
            handler.release.set()
            if not processing.done():
                with pytest.raises(asyncio.CancelledError):
                    await processing
            await supervisor.shutdown()

        assert handler.calls == 1
        assert output.frame is None
        assert control.settlements == []
        assert acker.acked == []

    asyncio.run(run())


def test_desired_state_cancellation_waits_for_sync_validation_then_emits_cancelled() -> None:
    async def run() -> None:
        envelope, command, manifest, raw_settings, delivery = _valid_delivery_case()
        handler = BlockingHandler()

        class CancellingControl(Control):
            def __init__(self) -> None:
                super().__init__(command, envelope, manifest)
                self.cancellation_observed = False
                self.renewals_at_cancellation = 0

            async def observe_desired_state(
                self, request: control_pb2.ObserveDesiredStateRequestV1
            ) -> control_pb2.ObserveDesiredStateResponseV1:
                response = await super().observe_desired_state(request)
                if handler.started.is_set():
                    if not self.cancellation_observed:
                        self.renewals_at_cancellation = len(self.renewals)
                    self.cancellation_observed = True
                    response.desired_state = (
                        common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
                    )
                return response

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        control = CancellingControl()
        acker = Acker()
        output = ImmediateOutput()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=supervisor,
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=CountingInput(raw_settings),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.005,
        )
        processing = asyncio.create_task(processor.process(delivery))

        try:
            await _wait_until(handler.started.is_set)
            await _wait_until(lambda: control.cancellation_observed)
            await _wait_until(
                lambda: len(control.renewals) > control.renewals_at_cancellation
            )
            assert not processing.done()
            assert handler.calls == 1
            assert output.frame is None
            assert control.settlements == []
            assert acker.acked == []

            handler.release.set()
            result = await processing
        finally:
            handler.release.set()
            if not processing.done():
                await processing
            await supervisor.shutdown()

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert handler.calls == 1
        assert len(control.renewals) >= 2
        assert output.frame is not None
        assert output.frame.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        assert (
            output.frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_fatal_lease_failure_after_cancellation_prevents_terminal_output() -> None:
    async def run() -> None:
        envelope, command, manifest, raw_settings, delivery = _valid_delivery_case()
        handler = BlockingHandler()

        class CancellationThenStaleControl(Control):
            def __init__(self) -> None:
                super().__init__(command, envelope, manifest)
                self.cancellation_observed = False
                self.stale_fence_returned = False

            async def renew_lease(
                self, request: control_pb2.RenewLeaseRequestV1
            ) -> control_pb2.RenewLeaseResponseV1:
                response = await super().renew_lease(request)
                if self.cancellation_observed:
                    self.stale_fence_returned = True
                    response.rejection.code = errors_pb2.RUNTIME_ERROR_CODE_V1_STALE_FENCE
                    response.rejection.safe_message = "The lease fence is stale."
                return response

            async def observe_desired_state(
                self, request: control_pb2.ObserveDesiredStateRequestV1
            ) -> control_pb2.ObserveDesiredStateResponseV1:
                response = await super().observe_desired_state(request)
                if handler.started.is_set():
                    self.cancellation_observed = True
                    response.desired_state = (
                        common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
                    )
                return response

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        control = CancellationThenStaleControl()
        acker = Acker()
        output = ImmediateOutput()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=supervisor,
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=CountingInput(raw_settings),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.005,
        )
        processing = asyncio.create_task(processor.process(delivery))

        try:
            await _wait_until(handler.started.is_set)
            await _wait_until(lambda: control.cancellation_observed)
            await _wait_until(lambda: control.stale_fence_returned)
            assert not processing.done()
            assert output.frame is None
            assert control.settlements == []
            assert acker.acked == []

            handler.release.set()
            with pytest.raises(AuthorizationFailure, match="authorization"):
                await processing
        finally:
            handler.release.set()
            if not processing.done():
                await asyncio.gather(processing, return_exceptions=True)
            await supervisor.shutdown()

        assert handler.calls == 1
        assert output.frame is None
        assert control.settlements == []
        assert acker.acked == []

    asyncio.run(run())


def test_output_stream_crash_replays_exact_spooled_frame_without_business_reexecution(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        settings = CountingInput((fixture / "settings.json").read_bytes())
        control = Control(command, envelope, manifest)
        acker = Acker()
        spool = EncryptedOutputSpool(
            tmp_path / "crash-spool",
            key=b"r" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        sessions = CrashThenReplayFactory(spool)

        class CountingHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.calls = 0

            def execute(self, request):
                self.calls += 1
                return _handler().execute(request)

        handler = CountingHandler()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=settings,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=2,
        )
        delivery = _command_delivery(
            stream="configuration-validation.v1",
            entry_id="1-0",
            fields={
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert len(sessions.calls) == 2
        first, replay = sessions.calls
        assert len(first.frames) == 1
        assert len(replay.frames) == 1
        original_bytes = first.frames[0].SerializeToString(deterministic=True)
        assert replay.frames[0].SerializeToString(deterministic=True) == original_bytes
        assert original_bytes == result.output_frame.SerializeToString(deterministic=True)
        assert settings.calls == 1
        assert handler.calls == 1
        assert len(control.claims) == 1
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


@pytest.mark.parametrize("retryable", [True, False], ids=["retryable", "terminal"])
def test_dependency_rejection_honors_retryability_without_losing_spooled_frame(
    tmp_path: Path,
    retryable: bool,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        frame = output_pb2.ExecutionOutputFrameV1.FromString(
            (fixture / "expected-output.pb").read_bytes()
        )
        spool = EncryptedOutputSpool(
            tmp_path / "retryable-rejection-spool",
            key=b"r" * 32,
            stream_aad=(
                f"{frame.identity.execution_id}:{frame.identity.generation}".encode()
            ),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        sessions = DependencyRejectionThenReplayFactory(
            spool,
            retryable=retryable,
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=None,
            control=Control(command, envelope, manifest),
            command_acker=Acker(),
            input_client=CountingInput(b""),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=2,
        )

        if retryable:
            await processor._publish_output(frame)
        else:
            with pytest.raises(RuntimeError, match="unavailable") as caught:
                await processor._publish_output(frame)
            assert isinstance(caught.value.__cause__, InvalidInput)

        assert len(sessions.calls) == (2 if retryable else 1)
        first = sessions.calls[0]
        assert len(first.frames) == 1
        expected = frame.SerializeToString(deterministic=True)
        assert first.frames[0].SerializeToString(deterministic=True) == expected
        if retryable:
            replay = sessions.calls[1]
            assert len(replay.frames) == 1
            assert replay.frames[0].SerializeToString(deterministic=True) == expected
            assert spool.pending() == ()
        else:
            pending = spool.pending()
            assert len(pending) == 1
            assert pending[0].payload == expected

    asyncio.run(run())


def test_output_cancellation_winner_replaces_only_the_exact_spooled_result(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        settings = CountingInput((fixture / "settings.json").read_bytes())
        control = Control(command, envelope, manifest)
        acker = Acker()
        spool = EncryptedOutputSpool(
            tmp_path / "cancellation-race-spool",
            key=b"c" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        sessions = CancellationThenReplayFactory(spool)

        class CountingHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.calls = 0

            def execute(self, request):
                self.calls += 1
                return _handler().execute(request)

        handler = CountingHandler()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=settings,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=1,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "3-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.HasField("runtime_error")
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert len(sessions.calls) == 2
        first, replacement = sessions.calls
        assert first.frames[0].SerializeToString(deterministic=True) == (
            fixture / "expected-output.pb"
        ).read_bytes()
        assert replacement.frames == [result.output_frame]
        assert settings.calls == 1
        assert handler.calls == 1
        assert all(
            observation.fence == envelope.fence for observation in control.observations
        )
        assert len(control.settlements) == 1
        assert (
            control.settlements[0].proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_output_deadline_winner_replaces_first_success_before_durable_ack(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        settings = CountingInput((fixture / "settings.json").read_bytes())
        control = Control(command, envelope, manifest)
        acker = Acker()
        spool = EncryptedOutputSpool(
            tmp_path / "deadline-race-spool",
            key=b"d" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        sessions = DeadlineThenReplayFactory(spool)

        class CountingHandler(_BindingAwareHandler):
            def __init__(self) -> None:
                self.calls = 0

            def execute(self, request):
                self.calls += 1
                return _handler().execute(request)

        handler = CountingHandler()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=settings,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=2,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "3-deadline-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.HasField("runtime_error")
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
        )
        assert result.output_frame.runtime_error.safe_message == (
            "The execution deadline was exceeded."
        )
        assert result.output_frame.runtime_error.retryable
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )
        assert len(sessions.calls) == 2
        first, replacement = sessions.calls
        expected_bytes = (fixture / "expected-output.pb").read_bytes()
        expected = output_pb2.ExecutionOutputFrameV1.FromString(expected_bytes)
        assert first.frames[0].SerializeToString(deterministic=True) == expected_bytes
        assert replacement.frames == [result.output_frame]
        assert (
            result.output_frame.occurred_at_unix_millis
            == expected.occurred_at_unix_millis
        )
        assert settings.calls == 1
        assert handler.calls == 1
        assert len(control.settlements) == 1
        assert (
            control.settlements[0].proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_recovery_replays_original_before_bound_cancellation_replacement(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        expected_bytes = (fixture / "expected-output.pb").read_bytes()
        expected = output_pb2.ExecutionOutputFrameV1.FromString(expected_bytes)
        spool = EncryptedOutputSpool(
            tmp_path / "cancellation-recovery-spool",
            key=b"q" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(expected.sequence, expected_bytes)
        sessions = CancellationThenReplayFactory(spool)
        control = ActiveLeaseControl(command, envelope, manifest)
        acker = Acker()
        forbidden_input = CountingInput(b"", forbidden=True)

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("durable recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=forbidden_input,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 1_000,
            max_output_sessions=1,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "4-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert (
            result.disposition
            is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        )
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        )
        assert len(sessions.calls) == 2
        assert (
            sessions.calls[0].frames[0].SerializeToString(deterministic=True)
            == expected_bytes
        )
        assert sessions.calls[1].frames == [result.output_frame]
        assert (
            result.output_frame.occurred_at_unix_millis
            == expected.occurred_at_unix_millis
        )
        assert forbidden_input.calls == 0
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_recovery_replaces_late_success_spool_after_bound_deadline_winner(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        expected_bytes = (fixture / "expected-output.pb").read_bytes()
        expected = output_pb2.ExecutionOutputFrameV1.FromString(expected_bytes)
        spool = EncryptedOutputSpool(
            tmp_path / "deadline-recovery-spool",
            key=b"l" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(expected.sequence, expected_bytes)
        sessions = DeadlineThenReplayFactory(spool)
        control = ActiveLeaseControl(command, envelope, manifest)
        acker = Acker()
        forbidden_input = CountingInput(b"", forbidden=True)

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("durable recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=forbidden_input,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=sessions,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 1_000,
            max_output_sessions=2,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "4-deadline-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert (
            result.disposition
            is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        )
        assert result.output_frame is not None
        assert (
            result.output_frame.runtime_error.code
            == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
        )
        assert result.output_frame.runtime_error.retryable
        assert len(sessions.calls) == 2
        assert (
            sessions.calls[0].frames[0].SerializeToString(deterministic=True)
            == expected_bytes
        )
        assert sessions.calls[1].frames == [result.output_frame]
        assert (
            result.output_frame.occurred_at_unix_millis
            == expected.occurred_at_unix_millis
        )
        assert forbidden_input.calls == 0
        assert len(control.settlements) == 1
        assert (
            control.settlements[0].proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_FAILED
        )
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_expired_cross_pod_claim_recovers_durable_cancellation_without_spool_or_work() -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        source = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            source.signed_command.worker_command_bytes
        )
        cancelled = build_output_frame(
            VerifiedWorkerCommand(envelope=source, command=command),
            ExecutionCancelled(),
            occurred_at_unix_millis=CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            claim_handoff_watermark=0,
        )
        proposal_raw = cancelled.settlement_proposal.SerializeToString(
            deterministic=True
        )
        proposal_digest = common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(proposal_raw).digest(),
        )
        replacement = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            source.SerializeToString(deterministic=True)
        )
        replacement.fence.workload_session_id = "replacement-session"
        replacement.fence.producer_id = "replacement-producer"
        replacement.fence.claim_attempt += 1
        replacement.fence.lease_epoch += 1
        replacement.fence.fence_token = b"r" * 32
        expected_prepare = control_pb2.PrepareSettlementRequestV1(
            identity=cancelled.identity,
            fence=replacement.fence,
            proposal=cancelled.settlement_proposal,
            proposal_digest=proposal_digest,
            idempotency_key=cancelled.settlement_proposal.prepare_idempotency_key,
        )
        control = RecoveryControl(
            command=command,
            envelope=replacement,
            disposition=control_pb2.CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK,
            recovery=control_pb2.SettlementRecoveryV1(
                proposal=cancelled.settlement_proposal,
                proposal_digest=proposal_digest,
                idempotency_key=cancelled.settlement_proposal.prepare_idempotency_key,
            ),
            expected_prepare=expected_prepare,
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        )
        prepared: list[control_pb2.PrepareSettlementRequestV1] = []
        original_prepare = control.prepare_settlement

        async def capture_prepare(request: control_pb2.PrepareSettlementRequestV1):
            prepared.append(request)
            return await original_prepare(request)

        control.prepare_settlement = capture_prepare  # type: ignore[method-assign]
        acker = Acker()
        delivery = _command_delivery(
            "configuration-validation.v1",
            "7-0",
            {
                "signed_envelope": source.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=_handler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: (_ for _ in ()).throw(
                AssertionError("durable cancellation recovery must not open output")
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id="replacement-session",
            producer_id="replacement-producer",
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )

        result = await processor.process(delivery)

        assert (
            result.disposition
            is DeliveryDisposition.RECOVERED_TERMINAL_SETTLED_ACKED
        )
        assert result.output_frame is None
        assert len(prepared) == 1
        assert prepared[0].fence == replacement.fence
        assert (
            prepared[0].proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
        )
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_recovery_replays_cancellation_frame_left_by_crash_after_atomic_cas(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        cancelled = build_output_frame(
            VerifiedWorkerCommand(envelope=envelope, command=command),
            ExecutionCancelled(),
            occurred_at_unix_millis=CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            claim_handoff_watermark=0,
        )
        cancelled_bytes = cancelled.SerializeToString(deterministic=True)
        spool = EncryptedOutputSpool(
            tmp_path / "post-cas-crash-spool",
            key=b"x" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(cancelled.sequence, cancelled_bytes)
        output_call = AckReplayCall()
        control = ActiveCancelledLeaseControl(command, envelope, manifest)
        acker = Acker()
        forbidden_input = CountingInput(b"", forbidden=True)

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("post-CAS recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=forbidden_input,
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: OutputGrpcSession(
                OutputStub(output_call),
                spool=spool,
                metadata=lambda: (
                    ("x-elitea-workload-session", _WORKLOAD_SESSION),
                ),
                max_queued_frames=2,
                max_queued_bytes=128 * 1024,
                max_frame_bytes=64 * 1024,
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=1,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "5-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        result = await processor.process(delivery)

        assert (
            result.disposition
            is DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED
        )
        assert result.output_frame is not None
        assert (
            result.output_frame.SerializeToString(deterministic=True)
            == cancelled_bytes
        )
        assert len(output_call.frames) == 1
        assert (
            output_call.frames[0].SerializeToString(deterministic=True)
            == cancelled_bytes
        )
        assert forbidden_input.calls == 0
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]
        assert spool.pending() == ()

    asyncio.run(run())


def test_generic_stale_output_rejection_never_rewrites_the_spool(
    tmp_path: Path,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        expected_bytes = (fixture / "expected-output.pb").read_bytes()
        expected = output_pb2.ExecutionOutputFrameV1.FromString(expected_bytes)
        spool = EncryptedOutputSpool(
            tmp_path / "generic-stale-spool",
            key=b"z" * 32,
            stream_aad=f"{command.execution_id}:{command.generation}".encode(),
            max_frames=4,
            max_bytes=128 * 1024,
            max_frame_bytes=64 * 1024,
        )
        spool.put(expected.sequence, expected_bytes)
        output_call = StaleFenceCall()
        control = ActiveLeaseControl(command, envelope, manifest)
        acker = Acker()

        class ForbiddenHandler(_BindingAwareHandler):
            def execute(self, request):
                raise AssertionError("durable recovery must not invoke the SDK")

        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=ForbiddenHandler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: OutputGrpcSession(
                OutputStub(output_call),
                spool=spool,
                metadata=lambda: (
                    ("x-elitea-workload-session", _WORKLOAD_SESSION),
                ),
                max_queued_frames=2,
                max_queued_bytes=128 * 1024,
                max_frame_bytes=64 * 1024,
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            max_output_sessions=1,
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "6-0",
            {
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )

        with pytest.raises(RuntimeError, match="unavailable") as caught:
            await processor.process(delivery)

        assert isinstance(caught.value.__cause__, AuthorizationFailure)
        assert len(output_call.frames) == 1
        assert output_call.frames[0].SerializeToString(deterministic=True) == expected_bytes
        assert control.settlements == []
        assert acker.acked == []
        pending = spool.pending()
        assert len(pending) == 1
        assert pending[0].payload == expected_bytes

    asyncio.run(run())


@pytest.mark.parametrize("prepared_before_loss", [False, True])
def test_redelivery_recovers_after_terminal_ack_without_rerunning_business_logic(
    prepared_before_loss: bool,
) -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        manifest = input_pb2.ExecutionInputBundleV1.FromString(
            (fixture / "input-bundle.pb").read_bytes()
        )
        delivery = _command_delivery(
            stream="configuration-validation.v1",
            entry_id="1-0",
            fields={
                "signed_envelope": envelope.signed_command.SerializeToString(
                    deterministic=True
                )
            },
        )
        acker = Acker()
        settings = CountingInput((fixture / "settings.json").read_bytes())
        lost = LostSettlementResponseControl(command, envelope, manifest)
        first_output = ImmediateOutput()
        common = {
            "supervisor": InlineSupervisor(),
            "handler": _handler(),
            "command_acker": acker,
            "input_request_builder": ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            "signed_command_authenticator": TestOnlyConformanceHmacAuthenticator(),
            "workload_session_id": _WORKLOAD_SESSION,
            "producer_id": _PRODUCER,
            "clock_unix_millis": lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        }
        first = ConfigurationValidationDeliveryProcessor(
            control=lost,
            input_client=settings,
            output_session_factory=lambda: first_output,
            **common,
        )
        with pytest.raises(DependencyUnavailable, match="Simulated"):
            await first.process(delivery)
        assert settings.calls == 1
        assert first_output.frame is not None
        assert acker.acked == []
        assert len(lost.settlements) == 1
        persisted = lost.settlements[0]

        if prepared_before_loss:
            disposition = control_pb2.CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT
            recovery = control_pb2.SettlementRecoveryV1(
                settlement_receipt_id="settlement-already-prepared-v1",
                outcome=persisted.proposal.requested_outcome,
            )
            expected_prepare = None
            expected_disposition = DeliveryDisposition.RECOVERED_SETTLEMENT_ACKED
        else:
            disposition = control_pb2.CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK
            recovery = control_pb2.SettlementRecoveryV1(
                proposal=persisted.proposal,
                proposal_digest=persisted.proposal_digest,
                idempotency_key=persisted.idempotency_key,
            )
            expected_prepare = persisted
            expected_disposition = DeliveryDisposition.RECOVERED_TERMINAL_SETTLED_ACKED
        recovery_control = RecoveryControl(
            command=command,
            envelope=envelope,
            disposition=disposition,
            recovery=recovery,
            expected_prepare=expected_prepare,
        )
        forbidden_input = CountingInput(b"", forbidden=True)

        def forbidden_output():
            raise AssertionError("recovery must not emit a second business output")

        recovered = ConfigurationValidationDeliveryProcessor(
            control=recovery_control,
            input_client=forbidden_input,
            output_session_factory=forbidden_output,
            **common,
        )
        result = await recovered.process(delivery)

        assert result.disposition is expected_disposition
        assert result.output_frame is None
        assert acker.acked == [delivery]
        assert forbidden_input.calls == 0
        assert recovery_control.prepare_calls == (0 if prepared_before_loss else 1)

    asyncio.run(run())


def test_cancellation_before_start_is_obsolete_acked_without_business_planes() -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        delivery = _command_delivery(
            "configuration-validation.v1",
            "1-0",
            {"signed_envelope": envelope.signed_command.SerializeToString(deterministic=True)},
        )
        acker = Acker()
        control = RecoveryControl(
            command=command,
            envelope=envelope,
            disposition=control_pb2.CLAIM_DISPOSITION_V1_OBSOLETE_ACK,
            recovery=None,
            expected_prepare=None,
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
            include_authority=False,
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=_handler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: (_ for _ in ()).throw(
                AssertionError("cancelled-before-start must not open output")
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.TERMINAL_REDELIVERY_ACKED
        assert acker.acked == [delivery]
        assert control.prepare_calls == 0

    asyncio.run(run())


def test_mismatched_claim_fence_fails_before_input_output_or_bus_ack() -> None:
    async def run() -> None:
        fixture = _FIXTURES / "valid"
        envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            (fixture / "envelope.pb").read_bytes()
        )
        command = command_pb2.WorkerCommandV1.FromString(
            envelope.signed_command.worker_command_bytes
        )
        mismatched = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
            envelope.SerializeToString(deterministic=True)
        )
        mismatched.fence.producer_id = "another-producer"
        delivery = _command_delivery(
            "configuration-validation.v1",
            "1-0",
            {"signed_envelope": envelope.signed_command.SerializeToString(deterministic=True)},
        )
        acker = Acker()
        control = RecoveryControl(
            command=command,
            envelope=mismatched,
            disposition=control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED,
            recovery=None,
            expected_prepare=None,
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=_handler(),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: (_ for _ in ()).throw(
                AssertionError("mismatched fence must not open output")
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )

        with pytest.raises(AuthorizationFailure, match="fence"):
            await processor.process(delivery)
        assert acker.acked == []

    asyncio.run(run())


def test_retired_deadline_is_fence_free_acked_without_business_planes() -> None:
    async def run() -> None:
        envelope, command, _, _, delivery = _valid_delivery_case()
        acker = Acker()
        control = RecoveryControl(
            command=command,
            envelope=envelope,
            disposition=control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            recovery=None,
            expected_prepare=None,
            retirement=errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
                safe_message=(
                    "The execution deadline was exceeded before worker authority "
                    "was granted."
                ),
                retryable=True,
            ),
            include_authority=False,
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=CountingHandler(forbidden=True),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: (_ for _ in ()).throw(
                AssertionError("retired work must not open output")
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )

        result = await processor.process(delivery)

        assert result.disposition is DeliveryDisposition.TERMINAL_REDELIVERY_ACKED
        assert acker.acked == [delivery]
        assert control.prepare_calls == 0

    asyncio.run(run())


@pytest.mark.parametrize(
    ("case", "disposition", "desired_state", "retirement", "include_authority"),
    [
        (
            "missing retirement",
            control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            None,
            False,
        ),
        (
            "non-retryable retirement",
            control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
                safe_message=(
                    "The execution deadline was exceeded before worker authority "
                    "was granted."
                ),
                retryable=False,
            ),
            False,
        ),
        (
            "wrong retirement code",
            control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED,
                safe_message=(
                    "The execution deadline was exceeded before worker authority "
                    "was granted."
                ),
                retryable=True,
            ),
            False,
        ),
        (
            "wrong retirement message",
            control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
                safe_message="raw internal timeout",
                retryable=True,
            ),
            False,
        ),
        (
            "retirement with authority",
            control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
                safe_message=(
                    "The execution deadline was exceeded before worker authority "
                    "was granted."
                ),
                retryable=True,
            ),
            True,
        ),
        (
            "retirement on obsolete disposition",
            control_pb2.CLAIM_DISPOSITION_V1_OBSOLETE_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
            errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
                safe_message=(
                    "The execution deadline was exceeded before worker authority "
                    "was granted."
                ),
                retryable=True,
            ),
            False,
        ),
        (
            "obsolete disposition with authority",
            control_pb2.CLAIM_DISPOSITION_V1_OBSOLETE_ACK,
            common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
            None,
            True,
        ),
    ],
)
def test_no_authority_receipts_fail_closed_when_malformed(
    case: str,
    disposition: int,
    desired_state: int,
    retirement: errors_pb2.RuntimeErrorV1 | None,
    include_authority: bool,
) -> None:
    async def run() -> None:
        envelope, command, _, _, delivery = _valid_delivery_case()
        acker = Acker()
        control = RecoveryControl(
            command=command,
            envelope=envelope,
            disposition=disposition,
            recovery=None,
            expected_prepare=None,
            desired_state=desired_state,
            retirement=retirement,
            include_authority=include_authority,
        )
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=InlineSupervisor(),
            handler=CountingHandler(forbidden=True),
            control=control,
            command_acker=acker,
            input_client=CountingInput(b"", forbidden=True),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: (_ for _ in ()).throw(
                AssertionError("malformed no-authority work must not open output")
            ),
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )

        with pytest.raises(InvalidInput):
            await processor.process(delivery)
        assert acker.acked == [], case
        assert control.prepare_calls == 0, case

    asyncio.run(run())


def test_deadline_before_input_skips_input_and_sdk_but_settles_failure() -> None:
    async def run() -> None:
        envelope, command, manifest, _, delivery = _valid_delivery_case()
        clock = MutableClock(int(command.deadline_unix_millis) - 1)
        control = DeadlineControl(
            command,
            envelope,
            manifest,
            clock=clock,
            expire_on_observation=1,
        )
        input_client = CountingInput(b"", forbidden=True)
        handler = CountingHandler(forbidden=True)
        output = ImmediateOutput()
        acker = Acker()
        processor = _deadline_test_processor(
            control=control,
            clock=clock,
            input_client=input_client,
            handler=handler,
            output=output,
            acker=acker,
        )

        result = await processor.process(delivery)

        _assert_deadline_failure(result)
        assert input_client.calls == 0
        assert handler.calls == 0
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_deadline_during_input_prevents_sync_execution() -> None:
    async def run() -> None:
        envelope, command, manifest, settings, delivery = _valid_delivery_case()
        deadline = int(command.deadline_unix_millis)
        clock = MutableClock(deadline - 1)
        control = DeadlineControl(command, envelope, manifest, clock=clock)
        input_client = DeadlineAdvancingInput(
            settings,
            clock=clock,
            deadline=deadline,
        )
        handler = CountingHandler(forbidden=True)
        output = ImmediateOutput()
        acker = Acker()
        processor = _deadline_test_processor(
            control=control,
            clock=clock,
            input_client=input_client,
            handler=handler,
            output=output,
            acker=acker,
        )

        result = await processor.process(delivery)

        _assert_deadline_failure(result)
        assert input_client.calls == 1
        assert handler.calls == 0
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_deadline_during_sync_discards_late_success_before_output() -> None:
    async def run() -> None:
        envelope, command, manifest, settings, delivery = _valid_delivery_case()
        deadline = int(command.deadline_unix_millis)
        clock = MutableClock(deadline - 1)
        control = DeadlineControl(command, envelope, manifest, clock=clock)
        input_client = CountingInput(settings)
        handler = CountingHandler(clock=clock, deadline=deadline)
        output = ImmediateOutput()
        acker = Acker()
        processor = _deadline_test_processor(
            control=control,
            clock=clock,
            input_client=input_client,
            handler=handler,
            output=output,
            acker=acker,
        )

        result = await processor.process(delivery)

        _assert_deadline_failure(result)
        assert input_client.calls == 1
        assert handler.calls == 1
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_deadline_immediately_before_first_output_replaces_success() -> None:
    async def run() -> None:
        envelope, command, manifest, settings, delivery = _valid_delivery_case()
        deadline = int(command.deadline_unix_millis)
        clock = MutableClock(deadline - 1)
        control = DeadlineControl(
            command,
            envelope,
            manifest,
            clock=clock,
            expire_on_observation=2,
        )
        input_client = CountingInput(settings)
        handler = CountingHandler()
        output = ImmediateOutput()
        acker = Acker()
        processor = _deadline_test_processor(
            control=control,
            clock=clock,
            input_client=input_client,
            handler=handler,
            output=output,
            acker=acker,
        )

        result = await processor.process(delivery)

        _assert_deadline_failure(result)
        assert input_client.calls == 1
        assert handler.calls == 1
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def test_durable_output_wins_if_deadline_expires_before_settlement() -> None:
    async def run() -> None:
        envelope, command, manifest, settings, delivery = _valid_delivery_case()
        deadline = int(command.deadline_unix_millis)
        clock = MutableClock(deadline - 1)
        control = DeadlineControl(command, envelope, manifest, clock=clock)
        output = ImmediateOutput(on_ack=lambda: setattr(clock, "value", deadline))
        acker = Acker()
        processor = _deadline_test_processor(
            control=control,
            clock=clock,
            input_client=CountingInput(settings),
            handler=CountingHandler(),
            output=output,
            acker=acker,
        )

        result = await processor.process(delivery)

        assert result.output_frame is not None
        assert result.output_frame.HasField("configuration_validation")
        assert not result.output_frame.HasField("runtime_error")
        assert (
            result.output_frame.settlement_proposal.requested_outcome
            == common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED
        )
        assert len(control.settlements) == 1
        assert acker.acked == [delivery]

    asyncio.run(run())


def _assert_deadline_failure(result) -> None:
    assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
    assert result.output_frame is not None
    assert result.output_frame.HasField("runtime_error")
    assert (
        result.output_frame.runtime_error.code
        == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
    )
    assert result.output_frame.runtime_error.safe_message == (
        "The execution deadline was exceeded."
    )
    assert result.output_frame.runtime_error.retryable is True
    assert (
        result.output_frame.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_FAILED
    )
