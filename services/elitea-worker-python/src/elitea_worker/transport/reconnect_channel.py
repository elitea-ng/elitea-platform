"""Reconnecting gRPC control and output transports.

Claim and lease state live in PostgreSQL, not in either endpoint's memory.
A recreated main replica still honors a live claim. The transport dies with
the old replica: a channel opened at process start points at the old address.
These handles re-resolve the service target. They close the stale channel
and open a fresh one. The new channel re-queries DNS for the target.

A transient replica recreation cannot stop lease renewal or strand terminal
output. The control plane re-resolves on UNAVAILABLE and CANCELLED statuses. The
output plane re-resolves on UNAVAILABLE status only. Both re-resolve at most
once per channel generation. A server-side denial propagates as its
non-retryable typed error.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from typing import TypeVar

import grpc
from elitea.runtime.v1 import control_pb2, control_pb2_grpc, output_pb2, output_pb2_grpc

from elitea_worker.execution.errors import DependencyUnavailable
from elitea_worker.transport.control_grpc import (
    ExecutionControlClient,
    MetadataProvider,
    secure_control_channel,
)
from elitea_worker.transport.output_grpc import (
    GeneratedOutputStub,
    OutputStreamCall,
    secure_output_channel,
)

_ControlResponse = TypeVar("_ControlResponse")


class ReconnectableControlPlane:
    """Control-plane client that re-resolves its channel on a retryable failure.

    One ``ExecutionControlClient`` serves each channel generation. On a
    retryable transport failure the plane closes the stale channel and opens
    a fresh one under a lock. It retries the call once on the new channel.
    If the retry still fails, it propagates the retryable failure.
    """

    def __init__(
        self,
        *,
        target: str,
        root_certificates: bytes,
        certificate_chain: bytes,
        private_key: bytes,
        metadata: MetadataProvider,
        deadline_seconds: float = 5.0,
    ) -> None:
        if not target:
            raise ValueError("control target is required")
        self._target = target
        self._root_certificates = root_certificates
        self._certificate_chain = certificate_chain
        self._private_key = private_key
        self._metadata = metadata
        self._deadline = deadline_seconds
        self._lock = asyncio.Lock()
        self._channel: grpc.aio.Channel | None = None
        self._client: ExecutionControlClient | None = None
        self._closed = False

    def _ensure_client(self) -> ExecutionControlClient:
        if self._closed:
            raise DependencyUnavailable("The control channel is closed.")
        if self._client is None:
            self._channel = secure_control_channel(
                self._target,
                root_certificates=self._root_certificates,
                certificate_chain=self._certificate_chain,
                private_key=self._private_key,
            )
            self._client = ExecutionControlClient(
                control_pb2_grpc.RuntimeControlServiceStub(self._channel),
                metadata=self._metadata,
                deadline_seconds=self._deadline,
            )
        return self._client

    async def _re_resolve(self, failed: ExecutionControlClient) -> None:
        """Swap in a fresh channel for the failed control channel.

        Null the channel and client under the lock. Close the failed channel
        without holding the lock. A concurrent caller can install a fresh
        channel while the close is in flight. On reacquire, adopt that channel
        if the fields are set. Create a new channel only when they stay empty.
        Two callers cannot close each other's fresh channel.
        """

        async with self._lock:
            if self._client is not failed:
                return
            channel, self._channel = self._channel, None
            self._client = None
        if channel is not None:
            await channel.close()
        async with self._lock:
            if self._closed:
                return
            if self._client is not None:
                return
            self._channel = secure_control_channel(
                self._target,
                root_certificates=self._root_certificates,
                certificate_chain=self._certificate_chain,
                private_key=self._private_key,
            )
            self._client = ExecutionControlClient(
                control_pb2_grpc.RuntimeControlServiceStub(self._channel),
                metadata=self._metadata,
                deadline_seconds=self._deadline,
            )

    async def _attempt_once(
        self,
        operation: Callable[[ExecutionControlClient], Awaitable[_ControlResponse]],
    ) -> _ControlResponse:
        client = self._ensure_client()
        try:
            return await operation(client)
        except DependencyUnavailable:
            await self._re_resolve(client)
            return await operation(self._ensure_client())

    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        return await self._attempt_once(lambda client: client.claim_command(request))

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        return await self._attempt_once(lambda client: client.renew_lease(request))

    async def begin_execution(
        self, request: control_pb2.BeginExecutionRequestV1
    ) -> control_pb2.BeginExecutionResponseV1:
        return await self._attempt_once(lambda client: client.begin_execution(request))

    async def authorize_invocation(
        self, request: control_pb2.AuthorizeInvocationRequestV1
    ) -> control_pb2.AuthorizeInvocationResponseV1:
        return await self._attempt_once(
            lambda client: client.authorize_invocation(request)
        )

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        return await self._attempt_once(
            lambda client: client.observe_desired_state(request)
        )

    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1:
        return await self._attempt_once(
            lambda client: client.prepare_settlement(request)
        )

    async def close(self, *, grace: float | None = None) -> None:
        async with self._lock:
            channel, self._channel = self._channel, None
            self._client = None
            self._closed = True
        if channel is not None:
            await channel.close(grace=grace)


class _ReconnectablePublishCall:
    """One Publish stream that re-publishes itself while still unestablished.

    The session's send and receive tasks share this object. A stream is
    unestablished until its first frame write or its first received ACK.
    Only an unestablished stream allows a channel swap, because it owes no
    durable frame on the old stream. An established stream surfaces transport
    failures unchanged. The session's bounded reconnect loop then re-publishes
    from the durable spool on a fresh call.
    """

    def __init__(
        self,
        owner: ReconnectableOutputStub,
        call: OutputStreamCall,
        *,
        channel: grpc.aio.Channel | None,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> None:
        self._owner = owner
        self._call = call
        self._channel = channel
        self._timeout = timeout
        self._metadata = metadata
        self._established = False

    async def _recover(self) -> None:
        self._call = await self._owner._fresh_stream(
            failed_channel=self._channel,
            timeout=self._timeout,
            metadata=self._metadata,
        )
        self._channel = self._owner._channel

    async def write(self, frame: output_pb2.ExecutionOutputFrameV1) -> None:
        try:
            await self._call.write(frame)
        except grpc.aio.AioRpcError as error:
            if (
                error.code() is not grpc.StatusCode.UNAVAILABLE
                or self._established
            ):
                raise
            await self._recover()
            await self._call.write(frame)
        self._established = True

    def __aiter__(self) -> _ReconnectablePublishCall:
        return self

    async def __anext__(self) -> output_pb2.ExecutionOutputAckV1:
        try:
            ack = await self._call.__anext__()
        except grpc.aio.AioRpcError as error:
            if (
                error.code() is not grpc.StatusCode.UNAVAILABLE
                or self._established
            ):
                raise
            await self._recover()
            ack = await self._call.__anext__()
        self._established = True
        return ack

    async def done_writing(self) -> None:
        await self._call.done_writing()

    def cancel(self) -> bool:
        return bool(self._call.cancel())


class ReconnectableOutputStub:
    """Output stub that re-resolves its channel when a stream is UNAVAILABLE.

    ``Publish`` returns a stream object. While the stream is still
    unestablished, it retries once on a fresh stream over a re-resolved
    channel. After a frame writes or an ACK arrives, the stream is
    established. Transport failures then propagate. The session's bounded
    reconnect loop owns recovery from the durable spool.
    """

    def __init__(
        self,
        *,
        target: str,
        root_certificates: bytes,
        certificate_chain: bytes,
        private_key: bytes,
    ) -> None:
        if not target:
            raise ValueError("output target is required")
        self._target = target
        self._root_certificates = root_certificates
        self._certificate_chain = certificate_chain
        self._private_key = private_key
        self._lock = asyncio.Lock()
        self._channel: grpc.aio.Channel | None = None
        self._stub: GeneratedOutputStub | None = None
        self._closed = False

    def _ensure_stub(self) -> GeneratedOutputStub:
        if self._closed:
            raise DependencyUnavailable("The output channel is closed.")
        if self._stub is None:
            self._channel = secure_output_channel(
                self._target,
                root_certificates=self._root_certificates,
                certificate_chain=self._certificate_chain,
                private_key=self._private_key,
            )
            self._stub = output_pb2_grpc.ExecutionOutputServiceStub(self._channel)
        return self._stub

    def Publish(
        self,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> _ReconnectablePublishCall:
        stub = self._ensure_stub()
        return _ReconnectablePublishCall(
            self,
            stub.Publish(timeout=timeout, metadata=metadata),
            channel=self._channel,
            timeout=timeout,
            metadata=metadata,
        )

    async def _fresh_stream(
        self,
        *,
        failed_channel: grpc.aio.Channel | None,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> OutputStreamCall:
        """Return a stream on a live channel, re-resolving if it is stale.

        Null the channel and stub under the lock. Close the failed channel
        without holding the lock. A concurrent recovery can install a fresh
        channel while the close is in flight. On reacquire, adopt that stream
        if the fields are set. Open a new stream only when they stay empty.
        Two deliveries cannot close each other's channel.
        """

        async with self._lock:
            if self._closed:
                raise DependencyUnavailable("The output channel is closed.")
            if self._channel is not failed_channel:
                return self._ensure_stub().Publish(timeout=timeout, metadata=metadata)
            channel, self._channel = self._channel, None
            self._stub = None
        if channel is not None:
            await channel.close()
        async with self._lock:
            if self._closed:
                raise DependencyUnavailable("The output channel is closed.")
            if self._channel is not None:
                return self._ensure_stub().Publish(timeout=timeout, metadata=metadata)
            self._channel = secure_output_channel(
                self._target,
                root_certificates=self._root_certificates,
                certificate_chain=self._certificate_chain,
                private_key=self._private_key,
            )
            self._stub = output_pb2_grpc.ExecutionOutputServiceStub(self._channel)
            return self._stub.Publish(timeout=timeout, metadata=metadata)

    async def close(self, *, grace: float | None = None) -> None:
        async with self._lock:
            channel, self._channel = self._channel, None
            self._stub = None
            self._closed = True
        if channel is not None:
            await channel.close(grace=grace)
