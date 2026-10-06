from __future__ import annotations

import asyncio
import hashlib
from pathlib import Path
from types import SimpleNamespace

import pytest
from elitea.runtime.v1 import envelope_pb2

from elitea_worker.execution.errors import WorkerError
from elitea_worker.protocol.codec import (
    TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_signed_command,
)
from elitea_worker.transport.nats_jetstream import JetStreamCommandConsumer


_ROOT = Path(__file__).parents[4]
_FIXTURE = _ROOT / "testdata/proto/runtime/v1/configuration-validation/valid"
_STREAM = "ELITEA_RT_V1_VALIDATE"
_CONSUMER = "elitea-configuration-worker-v1"


class MessageSpy:
    """One JetStream message; records every reply the worker sends on it."""

    def __init__(self, body: bytes, delivery_id: str, headers: dict[str, str]) -> None:
        self.subject = (
            "elitea.rt.v1.validate.d."
            + hashlib.sha256(delivery_id.encode()).hexdigest()
        )
        self.data = body
        self.headers = headers
        self.calls: list[tuple[str, object]] = []
        self.is_acked = False
        self.metadata = SimpleNamespace(
            stream=_STREAM,
            consumer=_CONSUMER,
            sequence=SimpleNamespace(stream=1, consumer=1),
            num_delivered=1,
        )

    async def ack_sync(self, timeout: float = 1.0):
        self.calls.append(("ack_sync", timeout))
        self.is_acked = True
        return SimpleNamespace(data=b"")


class SubscriptionSpy:
    def __init__(self, message: MessageSpy) -> None:
        self.message = message

    async def fetch(self, batch: int = 1, timeout: float | None = 5):
        return [self.message]


class DeadLetterSpy:
    def __init__(self) -> None:
        self.puts: list[tuple[str, bytes]] = []

    async def put(self, key: str, value: bytes) -> int:
        self.puts.append((key, value))
        return 1


def _signed() -> tuple[envelope_pb2.WorkerExecutionEnvelopeV1, bytes]:
    envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(
        (_FIXTURE / "envelope.pb").read_bytes()
    )
    return envelope, envelope.signed_command.SerializeToString(deterministic=True)


def test_cross_language_command_message_is_the_signed_reference_and_never_content_or_output() -> None:
    async def run() -> None:
        envelope, signed = _signed()
        settings = (_FIXTURE / "settings.json").read_bytes()
        expected_output = (_FIXTURE / "expected-output.pb").read_bytes()
        _, command = parse_and_verify_signed_command(
            signed, authenticator=TestOnlyConformanceHmacAuthenticator()
        )
        message = MessageSpy(
            signed,
            command.idempotency_key,
            {
                "Nats-Msg-Id": hashlib.sha256(command.idempotency_key.encode()).hexdigest(),
                "Elitea-Delivery-Id": command.idempotency_key,
            },
        )
        dead_letters = DeadLetterSpy()
        consumer = JetStreamCommandConsumer(
            SubscriptionSpy(message),
            stream=_STREAM,
            consumer=_CONSUMER,
            worker_name="worker-1",
            dead_letters=dead_letters,
        )

        deliveries = await consumer.fetch()
        assert len(deliveries) == 1
        assert deliveries[0].signed_envelope == signed
        assert settings not in signed
        assert expected_output not in signed
        parsed_signed, parsed = parse_and_verify_signed_command(
            deliveries[0].signed_envelope,
            authenticator=TestOnlyConformanceHmacAuthenticator(),
        )
        assert parsed_signed == envelope.signed_command
        assert parsed.input_bundle_ref.input_bundle_id
        # No publication surface at all: the worker can only answer a delivery.
        for name in ("publish", "xadd", "put", "create", "add_stream", "add_consumer"):
            assert not hasattr(consumer, name)

        await consumer.ack_after_settlement(deliveries[0], parsed.idempotency_key)
        assert [name for name, _ in message.calls] == ["ack_sync"]
        assert dead_letters.puts == []
        assert all(settings not in repr(call).encode() for call in message.calls)
        assert all(expected_output not in repr(call).encode() for call in message.calls)

    asyncio.run(run())


@pytest.mark.parametrize("trailer", [b"settings", b"output", b"result", b"image"])
def test_a_body_carrying_anything_beyond_the_signed_envelope_is_refused(
    trailer: bytes,
) -> None:
    """The body is exactly the signed envelope; inline data cannot ride along.

    A protobuf field appended to the envelope (here field 15, length-delimited)
    is refused by the canonical scan before any signature is trusted. Headers
    are never read into a delivery at all.
    """

    _, signed = _signed()
    body = signed + bytes([0x7A, len(trailer)]) + trailer
    with pytest.raises(WorkerError) as caught:
        parse_and_verify_signed_command(
            body, authenticator=TestOnlyConformanceHmacAuthenticator()
        )
    assert caught.value.retryable is False
