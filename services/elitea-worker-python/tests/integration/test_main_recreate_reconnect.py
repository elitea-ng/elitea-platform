"""Prove a recreated main replica cannot strand a live claim (#968)."""

from __future__ import annotations

import asyncio
import hashlib
import threading
from functools import lru_cache
from pathlib import Path

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
    _ClaimLeaseMonitor,
)
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DependencyUnavailable,
    ExecutionDraining,
    InternalFailure,
)
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.protocol.codec import TestOnlyConformanceHmacAuthenticator
from elitea_worker.transport.input_content import ClaimBoundInputRequestBuilder
from elitea_worker.transport.redis_commands import RedisCommandDelivery

_ROOT = Path(__file__).parents[4]
_FIXTURES = _ROOT / "testdata/proto/runtime/v1/configuration-validation"
_WORKLOAD_SESSION = "workload-session-conformance-v1"
_PRODUCER = "python-reference-conformance-v1"


@lru_cache(maxsize=1)
def _handler() -> ConfigurationValidationHandler:
    return ConfigurationValidationHandler(EliteaSdkAdapter())


def _valid_delivery_case() -> tuple[
    envelope_pb2.WorkerExecutionEnvelopeV1,
    command_pb2.WorkerCommandV1,
    input_pb2.ExecutionInputBundleV1,
    bytes,
    RedisCommandDelivery,
]:
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
    delivery = RedisCommandDelivery(
        "configuration-validation.v1",
        "1-0",
        {
            "signed_envelope": envelope.signed_command.SerializeToString(
                deterministic=True
            )
        },
    )
    return envelope, command, manifest, (fixture / "settings.json").read_bytes(), delivery


async def _wait_until(predicate, *, timeout_seconds: float = 5.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while not predicate():
        if asyncio.get_running_loop().time() >= deadline:
            raise AssertionError("the recreation window did not pass in time")
        await asyncio.sleep(0.005)


class RecreationControl:
    """Claims and renews one fence, losing the scripted renewals.

    The lost renewals model a main replica being recreated: the control
    endpoint is unreachable, which the typed transport maps to
    DependencyUnavailable.
    """

    def __init__(
        self,
        *,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
        manifest: input_pb2.ExecutionInputBundleV1,
        fail_renewal_sequences: frozenset[int] = frozenset(),
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.manifest = manifest
        self._fail_renewals = set(fail_renewal_sequences)
        self.claims: list[control_pb2.ClaimCommandRequestV1] = []
        self.renewals: list[control_pb2.RenewLeaseRequestV1] = []
        self.observations: list[control_pb2.ObserveDesiredStateRequestV1] = []
        self.settlements: list[control_pb2.PrepareSettlementRequestV1] = []
        self.failed_renewals = 0

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
                claim_id="claim-recreate-v1",
                identity=common_pb2.ExecutionIdentityV1(
                    tenant_id=self.command.tenant_id,
                    resource_project_id=self.command.resource_project_id,
                    projection_project_id=self.command.projection_project_id,
                    command_id=self.command.command_id,
                    execution_id=self.command.execution_id,
                    generation=self.command.generation,
                ),
                fence=self.envelope.fence,
                lease_expires_at_unix_millis=(
                    CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 60_000
                ),
                claim_started_at_unix_micros=(
                    CONFORMANCE_OCCURRED_AT_UNIX_MILLIS * 1_000
                ),
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
        sequence = int(request.idempotency_key.rsplit(":", 1)[-1])
        if sequence in self._fail_renewals:
            self.failed_renewals += 1
            raise DependencyUnavailable(
                "The main replica is being recreated."
            )
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
        assert request.proposal_digest.algorithm == (
            common_pb2.DIGEST_ALGORITHM_V1_SHA256
        )
        assert request.proposal_digest.value == hashlib.sha256(proposal).digest()
        return control_pb2.PrepareSettlementResponseV1(
            settlement_receipt_id="settlement-recreate-v1",
            outcome=request.proposal.requested_outcome,
        )


class DeniedLeaseControl:
    """Denies every renewal, as a replaced fence would."""

    def __init__(
        self,
        *,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.renewals: list[control_pb2.RenewLeaseRequestV1] = []

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        self.renewals.append(request)
        raise AuthorizationFailure("The claim fence was rejected.")

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        raise AssertionError("an observation must not follow a denied renewal")


class DrainingControl:
    """Returns a DRAINING desired state on every renewal, as a draining claim would."""

    def __init__(
        self,
        *,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.renewals: list[control_pb2.RenewLeaseRequestV1] = []

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        self.renewals.append(request)
        assert request.identity.execution_id == self.command.execution_id
        assert request.fence == self.envelope.fence
        return control_pb2.RenewLeaseResponseV1(
            lease_expires_at_unix_millis=(
                CONFORMANCE_OCCURRED_AT_UNIX_MILLIS + 120_000
            ),
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_DRAINING,
        )

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        assert request.identity.execution_id == self.command.execution_id
        assert request.fence == self.envelope.fence
        return control_pb2.ObserveDesiredStateResponseV1(
            desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        )


class InternalRejectionControl:
    """Returns an INTERNAL rejection on every renewal, as an unknown server error would."""

    def __init__(
        self,
        *,
        command: command_pb2.WorkerCommandV1,
        envelope: envelope_pb2.WorkerExecutionEnvelopeV1,
    ) -> None:
        self.command = command
        self.envelope = envelope
        self.renewals: list[control_pb2.RenewLeaseRequestV1] = []

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        self.renewals.append(request)
        assert request.identity.execution_id == self.command.execution_id
        assert request.fence == self.envelope.fence
        return control_pb2.RenewLeaseResponseV1(
            rejection=errors_pb2.RuntimeErrorV1(
                code=errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL,
                safe_message="The server error is unknown.",
                retryable=False,
            )
        )

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        raise AssertionError(
            "an observation must not follow an internal rejection"
        )


class _CountingInput:
    def __init__(self, content: bytes) -> None:
        self.content = content
        self.calls = 0

    async def fetch(self, grant) -> bytes:
        self.calls += 1
        return self.content


class _Acker:
    def __init__(self) -> None:
        self.acked: list[RedisCommandDelivery] = []
        self.stable_delivery_ids: list[str] = []

    async def ack_after_settlement(
        self, delivery: RedisCommandDelivery, stable_delivery_id: str
    ) -> None:
        self.acked.append(delivery)
        self.stable_delivery_ids.append(stable_delivery_id)


class _ImmediateOutput:
    def __init__(self) -> None:
        self.frame: output_pb2.ExecutionOutputFrameV1 | None = None

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
        assert self.frame is not None
        assert sequence == self.frame.sequence
        assert timeout_seconds > 0

    async def close(self) -> None:
        return None


class _RecreateBlockingHandler:
    def __init__(self) -> None:
        self.release = threading.Event()
        self.calls = 0

    def validate_binding(self, **kwargs) -> None:
        _handler().validate_binding(**kwargs)

    def execute(self, request):
        self.calls += 1
        assert self.release.wait(timeout=5.0)
        return _handler().execute(request)


class _ThreadedSupervisor:
    """Runs the SDK callable in a worker thread.

    The lease monitor polls on the event loop. The synchronous callable must
    not block that loop while it runs.
    """

    async def run(self, operation):
        return await operation()

    async def run_sync(self, operation, /, *args, **kwargs):
        return await asyncio.to_thread(operation, *args, **kwargs)


def test_lease_monitor_survives_main_recreation_window() -> None:
    async def run() -> None:
        envelope, command, manifest, _settings, _delivery = _valid_delivery_case()
        control = RecreationControl(
            command=command,
            envelope=envelope,
            manifest=manifest,
            fail_renewal_sequences=frozenset({2, 3, 4, 5}),
        )
        receipt = (
            await control.claim_command(
                control_pb2.ClaimCommandRequestV1(
                    workload_session_id=_WORKLOAD_SESSION,
                    producer_id=_PRODUCER,
                    signed_command=envelope.signed_command,
                )
            )
        ).receipt
        monitor = _ClaimLeaseMonitor(
            control=control,
            receipt=receipt,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            interval_seconds=0.02,
        )
        monitor.start()
        try:
            await _wait_until(
                lambda: len(control.renewals) >= 8 and control.failed_renewals == 4,
                timeout_seconds=5.0,
            )
            monitor.raise_if_failed()
        finally:
            await monitor.stop()

        assert control.failed_renewals == 4
        assert len(control.renewals) >= 8
        assert all(
            renewal.fence == envelope.fence for renewal in control.renewals
        )

    asyncio.run(run())


def test_lease_monitor_fails_closed_on_denied_renewal() -> None:
    async def run() -> None:
        envelope, command, manifest, _settings, _delivery = _valid_delivery_case()
        claim_control = RecreationControl(
            command=command, envelope=envelope, manifest=manifest
        )
        receipt = (
            await claim_control.claim_command(
                control_pb2.ClaimCommandRequestV1(
                    workload_session_id=_WORKLOAD_SESSION,
                    producer_id=_PRODUCER,
                    signed_command=envelope.signed_command,
                )
            )
        ).receipt
        monitor = _ClaimLeaseMonitor(
            control=DeniedLeaseControl(command=command, envelope=envelope),
            receipt=receipt,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            interval_seconds=0.02,
        )
        monitor.start()
        try:
            with pytest.raises(AuthorizationFailure):
                await asyncio.wait_for(
                    monitor.wait_for_state_change(), timeout=2.0
                )
            with pytest.raises(AuthorizationFailure):
                monitor.raise_if_failed()
        finally:
            await monitor.stop()

    asyncio.run(run())


def test_lease_monitor_fails_on_draining_desired_state() -> None:
    async def run() -> None:
        envelope, command, manifest, _settings, _delivery = _valid_delivery_case()
        claim_control = RecreationControl(
            command=command, envelope=envelope, manifest=manifest
        )
        receipt = (
            await claim_control.claim_command(
                control_pb2.ClaimCommandRequestV1(
                    workload_session_id=_WORKLOAD_SESSION,
                    producer_id=_PRODUCER,
                    signed_command=envelope.signed_command,
                )
            )
        ).receipt
        control = DrainingControl(command=command, envelope=envelope)
        monitor = _ClaimLeaseMonitor(
            control=control,
            receipt=receipt,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            interval_seconds=0.02,
        )
        monitor.start()
        try:
            with pytest.raises(ExecutionDraining) as caught:
                await asyncio.wait_for(
                    monitor.wait_for_state_change(), timeout=2.0
                )
            assert caught.value.safe_message == "The execution is draining."
            assert caught.value.code == "DEPENDENCY_UNAVAILABLE"
            assert caught.value.retryable is True
            assert caught.value.exit_code == 5
            with pytest.raises(ExecutionDraining):
                monitor.raise_if_failed()
            renewals_at_failure = len(control.renewals)
            await asyncio.sleep(0.1)
            assert len(control.renewals) == renewals_at_failure
        finally:
            await monitor.stop()

    asyncio.run(run())


def test_lease_monitor_fails_on_internal_rejection() -> None:
    async def run() -> None:
        envelope, command, manifest, _settings, _delivery = _valid_delivery_case()
        claim_control = RecreationControl(
            command=command, envelope=envelope, manifest=manifest
        )
        receipt = (
            await claim_control.claim_command(
                control_pb2.ClaimCommandRequestV1(
                    workload_session_id=_WORKLOAD_SESSION,
                    producer_id=_PRODUCER,
                    signed_command=envelope.signed_command,
                )
            )
        ).receipt
        control = InternalRejectionControl(command=command, envelope=envelope)
        monitor = _ClaimLeaseMonitor(
            control=control,
            receipt=receipt,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            interval_seconds=0.02,
        )
        monitor.start()
        try:
            with pytest.raises(InternalFailure) as caught:
                await asyncio.wait_for(
                    monitor.wait_for_state_change(), timeout=2.0
                )
            assert caught.value.code == "INTERNAL"
            assert caught.value.retryable is False
            with pytest.raises(InternalFailure):
                monitor.raise_if_failed()
            renewals_at_failure = len(control.renewals)
            await asyncio.sleep(0.1)
            assert len(control.renewals) == renewals_at_failure
        finally:
            await monitor.stop()

    asyncio.run(run())


def test_main_recreation_does_not_strand_a_live_delivery() -> None:
    async def run() -> None:
        envelope, command, manifest, settings, delivery = _valid_delivery_case()
        control = RecreationControl(
            command=command,
            envelope=envelope,
            manifest=manifest,
            fail_renewal_sequences=frozenset({2, 3, 4, 5}),
        )
        acker = _Acker()
        handler = _RecreateBlockingHandler()
        output = _ImmediateOutput()
        processor = ConfigurationValidationDeliveryProcessor(
            supervisor=_ThreadedSupervisor(),
            handler=handler,
            control=control,
            command_acker=acker,
            input_client=_CountingInput(settings),
            input_request_builder=ClaimBoundInputRequestBuilder(
                origin="https://content.test"
            ),
            output_session_factory=lambda: output,
            signed_command_authenticator=TestOnlyConformanceHmacAuthenticator(),
            workload_session_id=_WORKLOAD_SESSION,
            producer_id=_PRODUCER,
            clock_unix_millis=lambda: CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
            lease_poll_interval_seconds=0.05,
        )
        # Production imports the SDK at process start. Absorb that one-time
        # import cost here, before the timed recreation window, by running the
        # same binding validation the processor will run.
        selected = command.configuration_validation
        handler.validate_binding(
            configuration_type=selected.configuration_type,
            catalog_revision=selected.catalog_revision,
            catalog_digest=bytes(selected.catalog_digest.value),
            schema_id=selected.schema_id,
            schema_revision=selected.schema_revision,
            schema_digest=bytes(selected.schema_digest.value),
        )
        process_task = asyncio.create_task(processor.process(delivery))
        try:
            await _wait_until(
                lambda: control.failed_renewals == 4
                and len(control.renewals) >= 7,
                timeout_seconds=5.0,
            )
            handler.release.set()
            result = await asyncio.wait_for(process_task, timeout=10.0)
        finally:
            if not process_task.done():
                process_task.cancel()
                await asyncio.gather(process_task, return_exceptions=True)

        assert result.disposition is DeliveryDisposition.EXECUTED_SETTLED_ACKED
        assert result.output_frame is not None
        assert result.output_frame.fence == envelope.fence
        assert output.frame is result.output_frame
        assert len(acker.acked) == 1
        assert acker.stable_delivery_ids == [command.idempotency_key]
        assert control.failed_renewals == 4
        assert len(control.renewals) >= 8
        assert len(control.settlements) == 1
        assert control.settlements[0].fence == envelope.fence
        assert handler.calls == 1

    asyncio.run(run())
