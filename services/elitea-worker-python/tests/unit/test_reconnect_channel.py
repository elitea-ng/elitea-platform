from __future__ import annotations

import asyncio

import grpc
import pytest
from elitea.runtime.v1 import control_pb2, output_pb2

from elitea_worker.execution.delivery import _reconnectable_output
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DeadlineExceeded,
    DependencyUnavailable,
    ResourceExhausted,
    UnsupportedCapability,
)
from elitea_worker.transport import reconnect_channel
from elitea_worker.transport.control_grpc import ExecutionControlClient
from elitea_worker.transport.reconnect_channel import (
    ReconnectableControlPlane,
    ReconnectableOutputStub,
)

_METADATA = (("x-elitea-workload-session", "session-1"),)
_CONTROL_TARGET = "control.internal:8443"
_OUTPUT_TARGET = "output.internal:8444"


class _FailingStub:
    def __init__(self, code: grpc.StatusCode) -> None:
        self.calls = 0
        self._code = code
        self.ClaimCommand = self._rpc
        self.RenewLease = self._rpc
        self.BeginExecution = self._rpc
        self.AuthorizeInvocation = self._rpc
        self.ObserveDesiredState = self._rpc
        self.PrepareSettlement = self._rpc

    def _rpc(self, request, *, timeout, metadata):
        async def fail() -> None:
            self.calls += 1
            raise grpc.aio.AioRpcError(self._code, details="transport failure")

        return fail()


@pytest.mark.parametrize(
    ("code", "expected"),
    [
        (grpc.StatusCode.UNAVAILABLE, DependencyUnavailable),
        (grpc.StatusCode.CANCELLED, DependencyUnavailable),
        (grpc.StatusCode.UNAUTHENTICATED, AuthorizationFailure),
        (grpc.StatusCode.PERMISSION_DENIED, AuthorizationFailure),
        (grpc.StatusCode.DEADLINE_EXCEEDED, DeadlineExceeded),
        (grpc.StatusCode.RESOURCE_EXHAUSTED, ResourceExhausted),
        (grpc.StatusCode.UNIMPLEMENTED, UnsupportedCapability),
        (grpc.StatusCode.DATA_LOSS, DependencyUnavailable),
    ],
)
def test_control_transport_failure_maps_to_typed_safe_error(
    code: grpc.StatusCode,
    expected: type,
) -> None:
    async def run() -> None:
        stub = _FailingStub(code)
        client = ExecutionControlClient(
            stub,
            metadata=lambda: _METADATA,
        )
        with pytest.raises(expected) as caught:
            await client.renew_lease(control_pb2.RenewLeaseRequestV1())
        assert isinstance(caught.value.__cause__, grpc.aio.AioRpcError)

    asyncio.run(run())


@pytest.mark.parametrize(
    "method",
    [
        "claim_command",
        "renew_lease",
        "begin_execution",
        "authorize_invocation",
        "observe_desired_state",
        "prepare_settlement",
    ],
)
def test_every_control_rpc_maps_unavailable_to_retryable(
    method: str,
) -> None:
    async def run() -> None:
        stub = _FailingStub(grpc.StatusCode.UNAVAILABLE)
        client = ExecutionControlClient(
            stub,
            metadata=lambda: _METADATA,
        )
        request = _empty_request_for(method)
        with pytest.raises(DependencyUnavailable) as caught:
            await getattr(client, method)(request)
        assert caught.value.retryable is True
        assert stub.calls == 1

    asyncio.run(run())


def _empty_request_for(method: str) -> object:
    return {
        "claim_command": control_pb2.ClaimCommandRequestV1,
        "renew_lease": control_pb2.RenewLeaseRequestV1,
        "begin_execution": control_pb2.BeginExecutionRequestV1,
        "authorize_invocation": control_pb2.AuthorizeInvocationRequestV1,
        "observe_desired_state": control_pb2.ObserveDesiredStateRequestV1,
        "prepare_settlement": control_pb2.PrepareSettlementRequestV1,
    }[method]()


class _ControlBehavior:
    def __init__(
        self,
        *,
        response: control_pb2.RenewLeaseResponseV1 | None = None,
        error: grpc.aio.AioRpcError | None = None,
    ) -> None:
        self.response = response
        self.error = error
        self.calls: list[dict[str, object]] = []


class _FakeControlChannel:
    def __init__(self, behavior: _ControlBehavior) -> None:
        self.behavior = behavior
        self.closed = False
        self.close_grace: float | None = None

    async def close(self, *, grace: float | None = None) -> None:
        self.closed = True
        self.close_grace = grace

    def unary_unary(
        self,
        method,
        request_serializer=None,
        response_deserializer=None,
        _registered_method: bool = False,
    ):
        behavior = self.behavior

        def rpc(request, *, timeout, metadata):
            async def call() -> object:
                behavior.calls.append(
                    {
                        "method": method,
                        "request": request,
                        "timeout": timeout,
                        "metadata": metadata,
                    }
                )
                if behavior.error is not None:
                    raise behavior.error
                assert behavior.response is not None
                return behavior.response

            return call()

        return rpc


class _ChannelFactory:
    def __init__(self, behaviors: list[_ControlBehavior]) -> None:
        self.behaviors = behaviors
        self._fallback = behaviors[-1]
        self.channels: list[_FakeControlChannel] = []

    def __call__(
        self,
        target: str,
        *,
        root_certificates: bytes,
        certificate_chain: bytes,
        private_key: bytes,
    ) -> _FakeControlChannel:
        assert target == _CONTROL_TARGET
        assert root_certificates == b"ca"
        assert certificate_chain == b"certificate"
        assert private_key == b"private-key"
        behavior = self.behaviors.pop(0) if self.behaviors else self._fallback
        channel = _FakeControlChannel(behavior)
        self.channels.append(channel)
        return channel

    @property
    def current(self) -> _FakeControlChannel:
        assert self.channels
        return self.channels[-1]


def _plane(monkeypatch: pytest.MonkeyPatch, behaviors: list[_ControlBehavior]) -> tuple[ReconnectableControlPlane, _ChannelFactory]:
    factory = _ChannelFactory(behaviors)
    monkeypatch.setattr(reconnect_channel, "secure_control_channel", factory)
    plane = ReconnectableControlPlane(
        target=_CONTROL_TARGET,
        root_certificates=b"ca",
        certificate_chain=b"certificate",
        private_key=b"private-key",
        metadata=lambda: _METADATA,
        deadline_seconds=2.5,
    )
    return plane, factory


def _renew_response() -> control_pb2.RenewLeaseResponseV1:
    return control_pb2.RenewLeaseResponseV1(
        lease_expires_at_unix_millis=4_100_000_000_000,
        desired_state=1,
    )


def test_control_plane_re_resolves_channel_on_unavailable(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        good = _renew_response()
        plane, factory = _plane(
            monkeypatch,
            [
                _ControlBehavior(
                    error=grpc.aio.AioRpcError(
                        grpc.StatusCode.UNAVAILABLE,
                        details="main replica moved",
                    )
                ),
                _ControlBehavior(response=good),
            ],
        )
        request = control_pb2.RenewLeaseRequestV1()

        response = await plane.renew_lease(request)

        assert response == good
        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True
        assert factory.channels[1].closed is False
        assert len(factory.channels[0].behavior.calls) == 1
        assert factory.channels[1].behavior.calls == [
            {
                "method": "/elitea.runtime.v1.RuntimeControlService/RenewLease",
                "request": request,
                "timeout": 2.5,
                "metadata": _METADATA,
            }
        ]

    asyncio.run(run())


def test_control_plane_retries_once_and_gives_up_when_still_unavailable(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        unavailable = lambda: grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE,
            details="main replica moved",
        )
        plane, factory = _plane(
            monkeypatch,
            [
                _ControlBehavior(error=unavailable()),
                _ControlBehavior(error=unavailable()),
            ],
        )

        with pytest.raises(DependencyUnavailable) as caught:
            await plane.renew_lease(control_pb2.RenewLeaseRequestV1())

        assert caught.value.retryable is True
        assert isinstance(caught.value.__cause__, grpc.aio.AioRpcError)
        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True

    asyncio.run(run())


def test_control_plane_does_not_re_resolve_on_denial(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        denied = _ControlBehavior(
            error=grpc.aio.AioRpcError(
                grpc.StatusCode.PERMISSION_DENIED,
                details="fence rejected",
            )
        )
        plane, factory = _plane(monkeypatch, [denied])

        with pytest.raises(AuthorizationFailure):
            await plane.renew_lease(control_pb2.RenewLeaseRequestV1())

        assert len(factory.channels) == 1
        assert factory.channels[0].closed is False
        assert len(denied.calls) == 1

    asyncio.run(run())


def test_control_plane_close_closes_channel_and_refuses_calls(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        good = _renew_response()
        plane, factory = _plane(monkeypatch, [_ControlBehavior(response=good)])
        await plane.renew_lease(control_pb2.RenewLeaseRequestV1())

        await plane.close(grace=0.25)

        assert factory.channels[0].closed is True
        assert factory.channels[0].close_grace == 0.25
        with pytest.raises(DependencyUnavailable):
            await plane.renew_lease(control_pb2.RenewLeaseRequestV1())
        assert len(factory.channels) == 1

    asyncio.run(run())


def test_control_plane_re_resolve_adopts_concurrent_generation(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        good = _renew_response()
        plane, factory = _plane(
            monkeypatch,
            [
                _ControlBehavior(
                    error=grpc.aio.AioRpcError(
                        grpc.StatusCode.UNAVAILABLE,
                        details="main replica moved",
                    )
                ),
                _ControlBehavior(response=good),
            ],
        )
        client1 = plane._ensure_client()
        failed = factory.channels[0]
        close_started = asyncio.Event()
        close_gate = asyncio.Event()

        async def gated_close(*, grace: float | None = None) -> None:
            close_started.set()
            await close_gate.wait()
            failed.closed = True

        failed.close = gated_close
        re_resolve_task = asyncio.create_task(plane._re_resolve(client1))
        await close_started.wait()

        client2 = plane._ensure_client()
        assert plane._channel is factory.channels[1]

        close_gate.set()
        await re_resolve_task

        assert plane._channel is factory.channels[1]
        assert plane._client is client2
        assert failed.closed is True
        assert factory.channels[1].closed is False

        response = await plane.renew_lease(control_pb2.RenewLeaseRequestV1())
        assert response == good
        assert len(factory.channels[1].behavior.calls) == 1

    asyncio.run(run())


class _OutputBehavior:
    """Scripted write failures and ACK results per underlying stream call."""

    def __init__(
        self,
        write_errors: list[grpc.aio.AioRpcError | None],
        ack_script: list[grpc.aio.AioRpcError | output_pb2.ExecutionOutputAckV1] = (),
    ) -> None:
        self.write_errors = write_errors
        self.ack_script = list(ack_script)
        self.calls: list[_FakeOutputCall] = []


class _FakeStreamIterator:
    """Owns `__anext__` for one stream call, mirroring grpcio 1.84.0.

    The call object exposes its iterator only through `__aiter__`. This
    object consumes the scripted sequence. An exhausted sequence raises
    `StopAsyncIteration`.
    """

    def __init__(
        self,
        script: list[grpc.aio.AioRpcError | output_pb2.ExecutionOutputAckV1],
    ) -> None:
        self._script = script
        self._index = 0

    def __aiter__(self) -> _FakeStreamIterator:
        return self

    async def __anext__(self) -> output_pb2.ExecutionOutputAckV1:
        if self._index >= len(self._script):
            raise StopAsyncIteration
        entry = self._script[self._index]
        self._index += 1
        if isinstance(entry, grpc.aio.AioRpcError):
            raise entry
        assert isinstance(entry, output_pb2.ExecutionOutputAckV1)
        return entry


class _FakeOutputCall:
    def __init__(self, behavior: _OutputBehavior) -> None:
        self.behavior = behavior
        self.written: list[output_pb2.ExecutionOutputFrameV1] = []
        self.cancelled = False
        self._write_index = 0
        self._ack_iterator: _FakeStreamIterator | None = None

    def __aiter__(self) -> _FakeStreamIterator:
        # Mirror grpcio memoization: one call owns one stream iterator.
        if self._ack_iterator is None:
            self._ack_iterator = _FakeStreamIterator(self.behavior.ack_script)
        return self._ack_iterator

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        error = self.behavior.write_errors[self._write_index]
        self._write_index += 1
        if error is not None:
            raise error
        self.written.append(frame)

    async def done_writing(self) -> None:
        return None

    def cancel(self) -> bool:
        self.cancelled = True
        return True


class _FakeOutputChannel:
    def __init__(self, behavior: _OutputBehavior) -> None:
        self.behavior = behavior
        self.closed = False
        self.close_grace: float | None = None
        self.publishes = 0

    async def close(self, *, grace: float | None = None) -> None:
        self.closed = True
        self.close_grace = grace

    def stream_stream(
        self,
        method,
        request_serializer=None,
        response_deserializer=None,
        _registered_method: bool = False,
    ):
        channel = self
        assert method == "/elitea.runtime.v1.ExecutionOutputService/Publish"

        def factory(*, timeout, metadata):
            channel.publishes += 1
            call = _FakeOutputCall(channel.behavior)
            channel.behavior.calls.append(call)
            return call

        return factory


class _OutputChannelFactory:
    def __init__(self, behaviors: list[_OutputBehavior]) -> None:
        self.behaviors = behaviors
        self.channels: list[_FakeOutputChannel] = []
        self._fallback = behaviors[-1]

    def __call__(
        self,
        target: str,
        *,
        root_certificates: bytes,
        certificate_chain: bytes,
        private_key: bytes,
    ) -> _FakeOutputChannel:
        assert target == _OUTPUT_TARGET
        behavior = self.behaviors.pop(0) if self.behaviors else self._fallback
        channel = _FakeOutputChannel(behavior)
        self.channels.append(channel)
        return channel


def _output_stub(
    monkeypatch: pytest.MonkeyPatch,
    behaviors: list[_OutputBehavior],
) -> tuple[ReconnectableOutputStub, _OutputChannelFactory]:
    factory = _OutputChannelFactory(behaviors)
    monkeypatch.setattr(reconnect_channel, "secure_output_channel", factory)
    stub = ReconnectableOutputStub(
        target=_OUTPUT_TARGET,
        root_certificates=b"ca",
        certificate_chain=b"certificate",
        private_key=b"private-key",
    )
    return stub, factory


def test_output_stub_re_publishes_after_unavailable(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        stub, factory = _output_stub(
            monkeypatch,
            [
                _OutputBehavior(
                    [grpc.aio.AioRpcError(grpc.StatusCode.UNAVAILABLE)]
                ),
                _OutputBehavior([None]),
            ],
        )
        frame = output_pb2.ExecutionOutputFrameV1(sequence=1)
        call = stub.Publish(timeout=300.0, metadata=_METADATA)

        await call.write(frame)

        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True
        assert factory.channels[1].behavior.calls[0].written == [frame]

    asyncio.run(run())


def test_output_stub_still_unavailable_surfaces_reconnectable_failure(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        unavailable = lambda: grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE
        )
        stub, factory = _output_stub(
            monkeypatch,
            [
                _OutputBehavior([unavailable()]),
                _OutputBehavior([unavailable()]),
            ],
        )
        frame = output_pb2.ExecutionOutputFrameV1(sequence=1)
        call = stub.Publish(timeout=300.0, metadata=_METADATA)

        with pytest.raises(grpc.aio.AioRpcError) as caught:
            await call.write(frame)

        assert caught.value.code() is grpc.StatusCode.UNAVAILABLE
        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True
        try:
            raise RuntimeError("output stream is unavailable") from caught.value
        except RuntimeError as wrapped:
            assert _reconnectable_output(wrapped) is True

    asyncio.run(run())


def test_output_stub_does_not_re_publish_an_established_stream(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        unavailable = lambda: grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE
        )
        stub, factory = _output_stub(
            monkeypatch,
            [_OutputBehavior([None, unavailable()])],
        )
        call = stub.Publish(timeout=300.0, metadata=_METADATA)
        first = output_pb2.ExecutionOutputFrameV1(sequence=1)
        second = output_pb2.ExecutionOutputFrameV1(sequence=2)

        await call.write(first)
        with pytest.raises(grpc.aio.AioRpcError) as caught:
            await call.write(second)

        assert caught.value.code() is grpc.StatusCode.UNAVAILABLE
        assert len(factory.channels) == 1
        assert factory.channels[0].closed is False
        assert factory.channels[0].behavior.calls[0].written == [first]

    asyncio.run(run())


def test_output_stub_adopts_concurrent_re_resolution(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        unavailable = lambda: grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE
        )
        stub, factory = _output_stub(
            monkeypatch,
            [
                _OutputBehavior([unavailable()]),
                _OutputBehavior([None, None]),
            ],
        )
        call_a = stub.Publish(timeout=300.0, metadata=_METADATA)
        call_b = stub.Publish(timeout=300.0, metadata=_METADATA)
        frame_a = output_pb2.ExecutionOutputFrameV1(sequence=1)
        frame_b = output_pb2.ExecutionOutputFrameV1(sequence=1)

        await call_a.write(frame_a)
        await call_b.write(frame_b)

        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True
        assert factory.channels[1].closed is False
        assert factory.channels[1].behavior.calls[0].written == [frame_a]
        assert factory.channels[1].behavior.calls[1].written == [frame_b]

    asyncio.run(run())


def test_output_stub_fresh_stream_adopts_concurrent_generation(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        stub, factory = _output_stub(
            monkeypatch,
            [
                _OutputBehavior([None]),
                _OutputBehavior([None]),
            ],
        )
        stub._ensure_stub()
        failed = factory.channels[0]
        close_started = asyncio.Event()
        close_gate = asyncio.Event()

        async def gated_close(*, grace: float | None = None) -> None:
            close_started.set()
            await close_gate.wait()
            failed.closed = True

        failed.close = gated_close
        fresh_task = asyncio.create_task(
            stub._fresh_stream(
                failed_channel=failed,
                timeout=300.0,
                metadata=_METADATA,
            )
        )
        await close_started.wait()

        stub._ensure_stub()
        assert stub._channel is factory.channels[1]

        close_gate.set()
        stream = await fresh_task

        assert stub._channel is factory.channels[1]
        assert failed.closed is True
        assert factory.channels[1].closed is False
        assert factory.channels[0].publishes == 0
        assert factory.channels[1].publishes == 1
        assert stream is factory.channels[1].behavior.calls[-1]

    asyncio.run(run())


def test_output_stub_close_closes_channel_and_refuses_calls(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        stub, factory = _output_stub(
            monkeypatch,
            [_OutputBehavior([None])],
        )
        call = stub.Publish(timeout=300.0, metadata=_METADATA)
        await call.write(output_pb2.ExecutionOutputFrameV1(sequence=1))

        await stub.close(grace=0.25)

        assert factory.channels[0].closed is True
        assert factory.channels[0].close_grace == 0.25
        with pytest.raises(DependencyUnavailable):
            stub.Publish(timeout=300.0, metadata=_METADATA)
        assert len(factory.channels) == 1

    asyncio.run(run())


def test_output_stub_yields_ack_through_wrapper(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        ack = output_pb2.ExecutionOutputAckV1(committed_contiguous_sequence=1)
        stub, factory = _output_stub(
            monkeypatch,
            [_OutputBehavior([], ack_script=[ack])],
        )
        call = stub.Publish(timeout=300.0, metadata=_METADATA)

        received = await call.__anext__()

        assert received == ack
        assert len(factory.channels) == 1
        assert factory.channels[0].closed is False
        assert factory.channels[0].behavior.calls[0].written == []

    asyncio.run(run())


def test_output_stub_re_publishes_ack_stream_after_unavailable(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def run() -> None:
        unavailable = grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE,
            details="main replica moved",
        )
        ack = output_pb2.ExecutionOutputAckV1(committed_contiguous_sequence=1)
        stub, factory = _output_stub(
            monkeypatch,
            [
                _OutputBehavior([], ack_script=[unavailable]),
                _OutputBehavior([], ack_script=[ack]),
            ],
        )
        call = stub.Publish(timeout=300.0, metadata=_METADATA)

        received = await call.__anext__()

        assert received == ack
        assert len(factory.channels) == 2
        assert factory.channels[0].closed is True
        assert factory.channels[1].closed is False
        assert len(factory.channels[1].behavior.calls) == 1
        assert factory.channels[1].behavior.calls[0].written == []

    asyncio.run(run())
