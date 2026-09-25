"""Deadline-bound generated gRPC control client adapter."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Protocol, TypeVar

import grpc
from elitea.runtime.v1 import control_pb2
from google.protobuf.message import Message

from elitea_worker.constants import MAX_GRPC_REQUEST_BYTES, MAX_GRPC_RESPONSE_BYTES
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DeadlineExceeded,
    DependencyUnavailable,
    ResourceExhausted,
    UnsupportedCapability,
    WorkerError,
)


MetadataProvider = Callable[[], tuple[tuple[str, str], ...]]

_ControlRequest = TypeVar("_ControlRequest", bound=Message)
_ControlResponse = TypeVar("_ControlResponse", bound=Message)


class GeneratedControlStub(Protocol):
    """Structural protocol implemented by the generated control stub."""

    def ClaimCommand(
        self,
        request: control_pb2.ClaimCommandRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.ClaimCommandResponseV1]: ...

    def RenewLease(
        self,
        request: control_pb2.RenewLeaseRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.RenewLeaseResponseV1]: ...

    def BeginExecution(
        self,
        request: control_pb2.BeginExecutionRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.BeginExecutionResponseV1]: ...

    def AuthorizeInvocation(
        self,
        request: control_pb2.AuthorizeInvocationRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.AuthorizeInvocationResponseV1]: ...

    def ObserveDesiredState(
        self,
        request: control_pb2.ObserveDesiredStateRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.ObserveDesiredStateResponseV1]: ...

    def PrepareSettlement(
        self,
        request: control_pb2.PrepareSettlementRequestV1,
        *,
        timeout: float,
        metadata: tuple[tuple[str, str], ...],
    ) -> Awaitable[control_pb2.PrepareSettlementResponseV1]: ...


def secure_control_channel(
    target: str,
    *,
    root_certificates: bytes,
    certificate_chain: bytes,
    private_key: bytes,
) -> grpc.aio.Channel:
    if not target or not all((root_certificates, certificate_chain, private_key)):
        raise ValueError("verified target and workload certificates are required")
    credentials = grpc.ssl_channel_credentials(
        root_certificates=root_certificates,
        private_key=private_key,
        certificate_chain=certificate_chain,
    )
    return grpc.aio.secure_channel(
        target,
        credentials,
        options=(
            ("grpc.max_send_message_length", MAX_GRPC_REQUEST_BYTES),
            ("grpc.max_receive_message_length", MAX_GRPC_RESPONSE_BYTES),
        ),
    )


class ExecutionControlClient:
    """One attempt per call. The caller owns the retry policy.

    Each attempt maps a gRPC transport failure to a typed, safe
    ``WorkerError``. A stale control channel never reaches the delivery path
    as a raw exception.
    """

    def __init__(
        self,
        stub: GeneratedControlStub,
        *,
        metadata: MetadataProvider,
        deadline_seconds: float = 5.0,
    ) -> None:
        if deadline_seconds <= 0:
            raise ValueError("deadline_seconds must be positive")
        self._stub = stub
        self._metadata = metadata
        self._deadline = deadline_seconds

    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.ClaimCommand,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.RenewLease,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def begin_execution(
        self, request: control_pb2.BeginExecutionRequestV1
    ) -> control_pb2.BeginExecutionResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.BeginExecution,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def authorize_invocation(
        self, request: control_pb2.AuthorizeInvocationRequestV1
    ) -> control_pb2.AuthorizeInvocationResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.AuthorizeInvocation,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.ObserveDesiredState,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1:
        _require_wire_size(request, MAX_GRPC_REQUEST_BYTES, "control request")
        response = await self._typed_call(
            self._stub.PrepareSettlement,
            request,
        )
        _require_wire_size(response, MAX_GRPC_RESPONSE_BYTES, "control response")
        return response

    async def _typed_call(
        self,
        rpc: Callable[[_ControlRequest], Awaitable[_ControlResponse]],
        request: _ControlRequest,
    ) -> _ControlResponse:
        try:
            return await rpc(
                request,
                timeout=self._deadline,
                metadata=_validated_metadata(self._metadata()),
            )
        except grpc.aio.AioRpcError as error:
            raise _typed_control_failure(error) from error


def _typed_control_failure(error: grpc.aio.AioRpcError) -> WorkerError:
    """Map one gRPC transport status to the stable safe failure contract.

    The raw status stays chained as the cause for in-process diagnostics.
    A stale control channel surfaces as retryable ``DependencyUnavailable``
    so the claim lease keeps living. A server-side denial surfaces
    non-retryable.
    """

    code = error.code()
    if code in (
        grpc.StatusCode.UNAVAILABLE,
        grpc.StatusCode.CANCELLED,
    ):
        # CANCELLED names an RPC that the client or the server cancels.
        # The client cancels its own RPCs with close() during the shared
        # shutdown deadline. A recreated main replica tears down its
        # connections. The client sees CANCELLED for the in-flight RPCs.
        # The delivery winds down under that deadline and receives no
        # shutdown ACK. This mapping keeps the lease monitor and the claim
        # path on a stable, bounded retryable failure.
        return DependencyUnavailable()
    if code in (
        grpc.StatusCode.UNAUTHENTICATED,
        grpc.StatusCode.PERMISSION_DENIED,
    ):
        return AuthorizationFailure()
    if code is grpc.StatusCode.DEADLINE_EXCEEDED:
        return DeadlineExceeded()
    if code is grpc.StatusCode.RESOURCE_EXHAUSTED:
        return ResourceExhausted()
    if code is grpc.StatusCode.UNIMPLEMENTED:
        return UnsupportedCapability()
    return DependencyUnavailable()


def _require_wire_size(message: Message, max_bytes: int, description: str) -> None:
    if message.ByteSize() > max_bytes:
        raise ResourceExhausted(f"The {description} exceeds the transport limit.")


def _validated_metadata(
    metadata: tuple[tuple[str, str], ...],
) -> tuple[tuple[str, str], ...]:
    allowed = {"x-elitea-workload-session", "x-elitea-producer-id"}
    result: list[tuple[str, str]] = []
    seen: set[str] = set()
    for name, value in metadata:
        normalized = name.lower()
        if (
            normalized not in allowed
            or normalized in seen
            or not value
            or len(value.encode("utf-8")) > 256
            or "\r" in value
            or "\n" in value
        ):
            raise ValueError("control gRPC metadata is not allowlisted")
        seen.add(normalized)
        result.append((normalized, value))
    if "x-elitea-workload-session" not in seen:
        raise ValueError("control gRPC metadata is not allowlisted")
    return tuple(result)
