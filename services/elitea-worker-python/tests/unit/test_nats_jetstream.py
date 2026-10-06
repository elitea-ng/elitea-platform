"""The command bus transport over fakes (the live proof is tests/service)."""

from __future__ import annotations

import asyncio
import hashlib
import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from elitea_worker.execution.errors import (
    DependencyUnavailable,
    InvalidInput,
    WorkerError,
)
from elitea_worker.transport import nats_jetstream as bus
from elitea_worker.transport.nats_jetstream import (
    CommandBusAbsent,
    CommandBusDrift,
    CommandDelivery,
    JetStreamCommandConsumer,
    NatsCommandBusConnection,
    NatsTlsPaths,
    SubjectTokenMismatch,
    verify_command_bus,
)


_ROOT = Path(__file__).resolve().parents[4]
_GO_CONTRACT = _ROOT / "services/elitea-main/internal/transport/commandbus/contract.go"
_DOC = _ROOT / "docs/runtime-command-bus.md"

STREAM = "ELITEA_RT_V1_INDEX"
CONSUMER = "elitea-index-worker-v1"


def _token(delivery_id: str) -> str:
    return hashlib.sha256(delivery_id.encode()).hexdigest()


@dataclass
class FakeMsg:
    subject: str
    data: bytes
    headers: dict[str, str] | None = None
    stream: str = STREAM
    consumer: str = CONSUMER
    sequence: int = 1
    num_delivered: int = 1
    calls: list[tuple[str, Any]] = field(default_factory=list)
    ack_response: bytes = b""
    fail: set[str] = field(default_factory=set)
    _acked: bool = False

    @property
    def reply(self) -> str:
        return f"$JS.ACK.{self.stream}.{self.consumer}.{self.num_delivered}.{self.sequence}.1.0.0"

    @property
    def metadata(self) -> Any:
        return SimpleNamespace(
            stream=self.stream,
            consumer=self.consumer,
            sequence=SimpleNamespace(stream=self.sequence, consumer=self.sequence),
            num_delivered=self.num_delivered,
        )

    @property
    def is_acked(self) -> bool:
        return self._acked

    async def ack_sync(self, timeout: float = 1.0) -> Any:
        self.calls.append(("ack_sync", timeout))
        if "ack_sync" in self.fail:
            raise TimeoutError
        self._acked = True
        return SimpleNamespace(data=self.ack_response)

    async def nak(self, delay: float | None = None) -> None:
        self.calls.append(("nak", delay))
        if "nak" in self.fail:
            raise ConnectionError("nak lost")
        self._acked = True

    async def in_progress(self) -> None:
        self.calls.append(("in_progress", None))
        if "in_progress" in self.fail:
            raise ConnectionError("+WPI lost")

    async def term(self) -> None:
        self.calls.append(("term", None))
        if "term" in self.fail:
            raise ConnectionError("term lost")
        self._acked = True


def _message(delivery_id: str = "outbox-1", *, sequence: int = 1, **kwargs: Any) -> FakeMsg:
    token = _token(delivery_id)
    return FakeMsg(
        subject=f"elitea.rt.v1.index.d.{token}",
        data=kwargs.pop("data", b"signed-envelope"),
        headers=kwargs.pop(
            "headers",
            {"Nats-Msg-Id": token, "Elitea-Delivery-Id": delivery_id},
        ),
        sequence=sequence,
        **kwargs,
    )


class FakeSubscription:
    def __init__(self, batches: list[Any]) -> None:
        self.batches = list(batches)
        self.calls: list[tuple[int, float | None]] = []

    async def fetch(self, batch: int = 1, timeout: float | None = 5) -> list[Any]:
        self.calls.append((batch, timeout))
        if not self.batches:
            raise TimeoutError
        item = self.batches.pop(0)
        if isinstance(item, BaseException):
            raise item
        return item


class FakeKV:
    def __init__(self, *, fail: bool = False) -> None:
        self.records: dict[str, bytes] = {}
        self.fail = fail

    async def put(self, key: str, value: bytes) -> int:
        if self.fail:
            raise ConnectionError("bucket unreachable")
        self.records[key] = value
        return len(self.records)


def _consumer(
    subscription: FakeSubscription | None = None,
    *,
    kv: FakeKV | None = None,
    **kwargs: Any,
) -> JetStreamCommandConsumer:
    return JetStreamCommandConsumer(
        subscription or FakeSubscription([]),
        stream=STREAM,
        consumer=CONSUMER,
        worker_name="worker-pod-1",
        dead_letters=kv or FakeKV(),
        clock_unix_millis=lambda: 1_700_000_000_000,
        **kwargs,
    )


# ── The contract ────────────────────────────────────────────────────────────


def test_names_mirror_the_producer_contract_and_the_document() -> None:
    go = _GO_CONTRACT.read_text(encoding="utf-8")

    def go_const(name: str) -> str:
        match = re.search(rf'^\s*(?:const\s+)?{name}\s*=\s*"([^"]*)"', go, re.MULTILINE)
        assert match, name
        return match.group(1)

    assert bus.SUBJECT_ROOT == go_const("SubjectRoot")
    assert bus.STREAM_PREFIX == go_const("StreamPrefix")
    assert bus.STREAM_VALIDATE == go_const("StreamValidate")
    assert bus.STREAM_AGENT == go_const("StreamAgent")
    assert bus.STREAM_INDEX == go_const("StreamIndex")
    assert bus.CONSUMER_VALIDATE == go_const("ConsumerValidate")
    assert bus.CONSUMER_AGENT == go_const("ConsumerAgent")
    assert bus.CONSUMER_INDEX == go_const("ConsumerIndex")
    assert bus.HEADER_MSG_ID == go_const("HeaderMsgID")
    assert bus.HEADER_DELIVERY_ID == go_const("HeaderDeliveryID")
    assert bus.DEAD_LETTER_BUCKET == go_const("DeadLetterBucket")
    assert "AckWait = 60 * time.Second" in go
    assert "PoisonDelay = 24 * time.Hour" in go
    assert "MaxMessageBytes = 64 * 1024" in go
    assert "MaxRequestBatch   = 64" in go
    assert bus.ACK_WAIT_SECONDS == 60
    assert bus.POISON_DELAY_SECONDS == 24 * 3600
    assert bus.MAX_TRANSPORT_MESSAGE_BYTES == 64 * 1024
    assert bus.MAX_FETCH_BATCH == 64

    doc = _DOC.read_text(encoding="utf-8")
    assert bus.DEAD_LETTER_SCHEMA in doc
    assert f"`{bus.INBOX_PREFIX}`" in doc
    assert "`worker_command.dead_lettered`" in doc
    for name in (
        "nats_url",
        "nats_ca_path",
        "nats_certificate_path",
        "nats_private_key_path",
        "nats_stream",
        "nats_consumer",
        "limits.nats_fetch_batch",
        "limits.nats_fetch_expires_millis",
        "limits.nats_in_progress_interval_millis",
        "limits.nats_retry_delay_millis",
    ):
        assert f"`{name}`" in doc, name
    for stream, consumer in bus.KNOWN_STREAMS.items():
        assert f"| `{stream}` | `{bus.filter_subject(stream)}` | `{consumer}` |" in doc


def test_delivery_subject_hashes_the_delivery_id_with_the_documented_vector() -> None:
    assert (
        bus.delivery_token("outbox-1")
        == "ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
    )
    assert bus.delivery_subject(STREAM, "outbox-1") == (
        "elitea.rt.v1.index.d."
        "ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
    )
    assert bus.dead_letter_key("ELITEA_RT_V1_AGENT", "outbox-1") == (
        "agent.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
    )
    # A delivery ID may hold wildcard and token characters; the subject never does.
    assert "*" not in bus.delivery_subject(STREAM, "a.b *>c")
    assert bus.filter_subject("ELITEA_RT_V1_VALIDATE") == "elitea.rt.v1.validate.d.*"


@pytest.mark.parametrize(
    "stream",
    ["", "ELITEA_RT_V1_", "ELITEA_RT_V1_index", "ELITEA_RT_V1_1X", "RT_V1_INDEX", "ELITEA_RT_V1_" + "A" * 33],
)
def test_route_token_refuses_a_name_outside_the_contract(stream: str) -> None:
    with pytest.raises(ValueError):
        bus.route_token(stream)


def test_constructor_refuses_bounds_and_pairings_outside_the_contract() -> None:
    for kwargs in (
        {"fetch_batch": 0},
        {"fetch_batch": 65},
        {"fetch_expires_millis": 30_001},
        {"max_payload_bytes": 70_000},
    ):
        with pytest.raises(ValueError):
            _consumer(**kwargs)
    with pytest.raises(ValueError):
        JetStreamCommandConsumer(
            FakeSubscription([]),
            stream=STREAM,
            consumer="elitea-agent-worker-v1",
            worker_name="w",
            dead_letters=FakeKV(),
        )


# ── Intake ──────────────────────────────────────────────────────────────────


def test_fetch_pulls_at_most_the_reserved_count_with_the_configured_expiry() -> None:
    async def run() -> None:
        first, second = _message("a", sequence=7), _message("b", sequence=8)
        subscription = FakeSubscription([[first, second]])
        consumer = _consumer(subscription, fetch_batch=4, fetch_expires_millis=250)

        deliveries = await consumer.fetch(count=2)

        assert subscription.calls == [(2, 0.25)]
        assert [d.stream_sequence for d in deliveries] == [7, 8]
        assert deliveries[0].entry_id == f"{STREAM}:7"
        assert deliveries[0].delivery_token == _token("a")
        assert deliveries[0].dead_letter_key == f"index.{_token('a')}"
        assert deliveries[0].signed_envelope == b"signed-envelope"
        assert deliveries[0].rejection is None
        with pytest.raises(ValueError):
            await consumer.fetch(count=5)
        with pytest.raises(ValueError):
            await consumer.fetch(count=0)

    asyncio.run(run())


def test_an_empty_pull_is_no_messages_not_an_error() -> None:
    async def run() -> None:
        consumer = _consumer(FakeSubscription([TimeoutError()]))
        assert await consumer.fetch() == ()

    asyncio.run(run())


def test_messages_beyond_the_reservation_go_straight_back() -> None:
    async def run() -> None:
        messages = [_message(str(index), sequence=index) for index in range(3)]
        consumer = _consumer(FakeSubscription([messages]), fetch_batch=2)

        deliveries = await consumer.fetch(count=2)

        assert len(deliveries) == 2
        assert messages[2].calls == [("nak", None)]
        assert messages[0].calls == messages[1].calls == []

    asyncio.run(run())


def test_a_message_of_another_consumer_fails_the_pull() -> None:
    async def run() -> None:
        stray = _message(consumer="elitea-agent-worker-v1")
        with pytest.raises(InvalidInput):
            await _consumer(FakeSubscription([[stray]])).fetch()

    asyncio.run(run())


@pytest.mark.parametrize(
    ("subject", "data", "headers", "code"),
    [
        ("elitea.rt.v1.index.d.not-a-hash", b"x", None, "SUBJECT_MALFORMED"),
        ("elitea.rt.v1.index.d." + "A" * 64, b"x", None, "SUBJECT_MALFORMED"),
        ("elitea.rt.v1.agent.d." + "a" * 64, b"x", None, "SUBJECT_MALFORMED"),
        ("elitea.rt.v1.index.d." + "a" * 64, b"", None, "PAYLOAD_SIZE_REJECTED"),
        ("elitea.rt.v1.index.d." + "a" * 64, b"x" * (48 * 1024 + 1), None, "PAYLOAD_SIZE_REJECTED"),
        (
            "elitea.rt.v1.index.d." + "a" * 64,
            b"x" * (48 * 1024),
            {"Elitea-Delivery-Id": "y" * (17 * 1024)},
            "MESSAGE_SIZE_REJECTED",
        ),
    ],
)
def test_a_poison_transport_shape_is_delivered_as_a_rejection_without_its_body(
    subject: str,
    data: bytes,
    headers: dict[str, str] | None,
    code: str,
) -> None:
    async def run() -> None:
        message = FakeMsg(subject=subject, data=data, headers=headers)
        (delivery,) = await _consumer(FakeSubscription([[message]])).fetch()

        assert delivery.rejection is not None
        assert delivery.rejection.code == code
        assert delivery.rejection.retryable is False
        assert delivery.signed_envelope == b""
        assert re.fullmatch(r"index\.[0-9a-f]{64}", delivery.dead_letter_key)
        assert message.calls == []

    asyncio.run(run())


def test_a_body_at_the_exact_payload_bound_is_accepted() -> None:
    async def run() -> None:
        message = _message(data=b"x" * (48 * 1024))
        (delivery,) = await _consumer(FakeSubscription([[message]])).fetch()
        assert delivery.rejection is None

    asyncio.run(run())


# ── Heartbeat ───────────────────────────────────────────────────────────────


def test_in_progress_refreshes_every_unanswered_message_and_reports_one_failure() -> None:
    async def run() -> None:
        consumer = _consumer()
        lost = _message("lost", fail={"in_progress"})
        answered = _message("answered")
        answered._acked = True
        live = _message("live")
        deliveries = [
            _delivery_of(message) for message in (lost, answered, live)
        ]

        with pytest.raises(DependencyUnavailable):
            await consumer.in_progress(deliveries)

        assert live.calls == [("in_progress", None)]
        assert lost.calls == [("in_progress", None)]
        assert answered.calls == []
        assert await consumer.in_progress(deliveries[2:]) == 1

    asyncio.run(run())


def _delivery_of(message: FakeMsg) -> CommandDelivery:
    return CommandDelivery(
        stream=message.stream,
        consumer=message.consumer,
        subject=message.subject,
        stream_sequence=message.sequence,
        num_delivered=message.num_delivered,
        signed_envelope=message.data,
        message=message,
    )


# ── Settlement ──────────────────────────────────────────────────────────────


def test_ack_after_settlement_double_acks_once_and_is_idempotent() -> None:
    async def run() -> None:
        consumer = _consumer(ack_timeout_seconds=3.0)
        message = _message("outbox-1")
        delivery = _delivery_of(message)

        await consumer.ack_after_settlement(delivery, "outbox-1")
        await consumer.ack_after_settlement(delivery, "outbox-1")

        assert message.calls == [("ack_sync", 3.0)]

    asyncio.run(run())


def test_ack_refuses_a_subject_that_does_not_name_the_command_before_acking() -> None:
    async def run() -> None:
        message = _message("outbox-1")
        with pytest.raises(SubjectTokenMismatch) as caught:
            await _consumer().ack_after_settlement(_delivery_of(message), "outbox-2")
        assert caught.value.retryable is False
        assert message.calls == []

    asyncio.run(run())


def test_ack_refuses_a_delivery_of_another_stream_before_acking() -> None:
    async def run() -> None:
        message = _message("outbox-1", stream="ELITEA_RT_V1_AGENT")
        with pytest.raises(InvalidInput):
            await _consumer().ack_after_settlement(_delivery_of(message), "outbox-1")
        assert message.calls == []

    asyncio.run(run())


@pytest.mark.parametrize("stable", ["", "a\nb", "a\rb", "a\x00b", "x" * 257, "é" * 129])
def test_ack_refuses_a_malformed_stable_delivery_id(stable: str) -> None:
    async def run() -> None:
        message = _message(stable or "outbox-1")
        with pytest.raises(InvalidInput):
            await _consumer().ack_after_settlement(_delivery_of(message), stable)
        assert message.calls == []

    asyncio.run(run())


def test_ack_accepts_a_stable_delivery_id_at_the_exact_utf8_bound() -> None:
    async def run() -> None:
        stable = "é" * 128  # 256 UTF-8 bytes
        message = _message(stable)
        await _consumer().ack_after_settlement(_delivery_of(message), stable)
        assert [name for name, _ in message.calls] == ["ack_sync"]

    asyncio.run(run())


@pytest.mark.parametrize(
    "message",
    [
        _message("outbox-1", fail={"ack_sync"}),
        _message("outbox-1", ack_response=b'{"error":{"code":500}}'),
    ],
)
def test_ack_fails_closed_unless_the_server_confirms(message: FakeMsg) -> None:
    async def run() -> None:
        with pytest.raises(DependencyUnavailable) as caught:
            await _consumer().ack_after_settlement(_delivery_of(message), "outbox-1")
        assert caught.value.retryable is True

    asyncio.run(run())


def test_retry_later_naks_with_the_configured_delay_and_park_with_24h() -> None:
    async def run() -> None:
        consumer = _consumer(retry_delay_millis=45_000)
        retry, poison = _message("retry"), _message("poison")

        await consumer.retry_later(_delivery_of(retry))
        await consumer.retry_later(_delivery_of(retry))  # already answered
        await consumer.park(_delivery_of(poison))

        assert retry.calls == [("nak", 45.0)]
        assert poison.calls == [("nak", 86_400)]

    asyncio.run(run())


def test_record_dead_letter_records_where_to_look_never_what_it_said() -> None:
    async def run() -> None:
        kv = FakeKV()
        consumer = _consumer(kv=kv)
        message = _message("outbox-secret-id", sequence=42, num_delivered=3, data=b"ENVELOPE")

        await consumer.record_dead_letter(_delivery_of(message), reason="AUTHORIZATION_FAILED")

        # The record only: answering the poison is the serve loop's next step.
        assert message.calls == []
        key = f"index.{_token('outbox-secret-id')}"
        assert list(kv.records) == [key]
        raw = kv.records[key]
        assert json.loads(raw) == {
            "schema": "elitea.runtime.dead-letter.v1",
            "stream": STREAM,
            "consumer": CONSUMER,
            "subject": message.subject,
            "stream_sequence": 42,
            "num_delivered": 3,
            "reason": "AUTHORIZATION_FAILED",
            "worker": "worker-pod-1",
            "recorded_at_unix_millis": 1_700_000_000_000,
        }
        assert b"ENVELOPE" not in raw
        assert b"outbox-secret-id" not in raw

    asyncio.run(run())


def test_terminate_terms_once_and_only_an_unanswered_message() -> None:
    async def run() -> None:
        message = _message("unverifiable")
        consumer = _consumer()
        await consumer.terminate(_delivery_of(message))
        await consumer.terminate(_delivery_of(message))  # already answered
        assert message.calls == [("term", None)]
        assert set(bus.TERMINAL_POISON) == {
            bus.CommandSignatureRejected,
            bus.SubjectTokenMismatch,
        }

    asyncio.run(run())


def test_dead_letter_reason_is_a_low_cardinality_code() -> None:
    record = _consumer().dead_letter_record(
        _delivery_of(_message()), reason="free text: secret=1"
    )
    assert json.loads(record)["reason"] == "UNCLASSIFIED"


def test_an_unreachable_bucket_is_a_typed_failure_and_answers_nothing() -> None:
    async def run() -> None:
        message = _message()
        with pytest.raises(bus.DeadLetterUnrecorded) as caught:
            await _consumer(kv=FakeKV(fail=True)).record_dead_letter(
                _delivery_of(message), reason="INVALID_INPUT"
            )
        assert caught.value.retryable is True
        assert message.calls == []

    asyncio.run(run())


# ── Bind ────────────────────────────────────────────────────────────────────


# Exactly what nats-server 2.12 reports for the bootstrap's durable (measured
# against the chart's own secured server and bootstrap.sh).
_CONSUMER_CONFIG = {
    "durable_name": CONSUMER,
    "name": CONSUMER,
    "deliver_policy": "all",
    "ack_policy": "explicit",
    "ack_wait": 60_000_000_000,
    "max_deliver": -1,
    "filter_subject": "elitea.rt.v1.index.d.*",
    "replay_policy": "instant",
    "max_waiting": 512,
    "max_ack_pending": 1024,
    "max_batch": 64,
    "max_expires": 30_000_000_000,
}


class FakeApi:
    def __init__(self, responses: dict[str, Any]) -> None:
        self.responses = responses
        self.subjects: list[str] = []

    async def request(self, subject: str, payload: bytes = b"", timeout: float = 0.5) -> Any:
        self.subjects.append(subject)
        value = self.responses[subject]
        if isinstance(value, BaseException):
            raise value
        return SimpleNamespace(data=json.dumps(value).encode())


def _api(consumer: Any = None, *, prefix: str = "$JS.API") -> FakeApi:
    return FakeApi(
        {
            f"{prefix}.CONSUMER.INFO.{STREAM}.{CONSUMER}": consumer
            if consumer is not None
            else {"config": dict(_CONSUMER_CONFIG)},
        }
    )


async def _verify(api: FakeApi, **kwargs: Any) -> None:
    await verify_command_bus(
        api,
        stream=STREAM,
        consumer=CONSUMER,
        fetch_batch=kwargs.get("fetch_batch", 8),
        fetch_expires_millis=kwargs.get("fetch_expires_millis", 1000),
        timeout_seconds=1.0,
        api_prefix=kwargs.get("api_prefix", "$JS.API"),
    )


def test_bind_accepts_the_bootstrap_shape_through_one_read_only_api_call() -> None:
    api = _api()
    asyncio.run(_verify(api))
    # The durable only: the worker has no STREAM.INFO grant on a command
    # stream (S1); elitea-main, the stream's writer, verifies its shape.
    assert api.subjects == [f"$JS.API.CONSUMER.INFO.{STREAM}.{CONSUMER}"]
    # filter_subjects (the multi-filter form) with the one filter is the same.
    config = dict(_CONSUMER_CONFIG, filter_subjects=["elitea.rt.v1.index.d.*"])
    del config["filter_subject"]
    asyncio.run(_verify(_api(consumer={"config": config})))


def test_bind_reads_the_durable_through_the_runtime_import_prefix() -> None:
    api = _api(prefix=bus.RUNTIME_API_PREFIX)
    asyncio.run(_verify(api, api_prefix=bus.RUNTIME_API_PREFIX))
    assert api.subjects == [f"JS.RUNTIME.API.CONSUMER.INFO.{STREAM}.{CONSUMER}"]
    with pytest.raises(ValueError):
        asyncio.run(_verify(_api(), api_prefix="$JS.OTHER.API"))


def test_the_runtime_prefix_is_used_exactly_when_the_worker_presents_an_identity() -> None:
    assert bus.RUNTIME_API_PREFIX == "JS.RUNTIME.API"
    assert bus.runtime_api_prefix(_TLS) == "JS.RUNTIME.API"
    assert bus.runtime_api_prefix(None) == "$JS.API"
    # The Go side (natsconn) and the chart's import mapping hold the same
    # string; a drift there answers every bind "JetStream not enabled".
    natsconn = (_ROOT / "libs/go/natsconn/natsconn.go").read_text()
    match = re.search(r'WorkerRuntimeJSAPIPrefix\s*=\s*"([^"]+)"', natsconn)
    if match is not None:
        assert match.group(1) == bus.RUNTIME_API_PREFIX


class FakeJetStream:
    def __init__(self, owner: "FakeBindClient", prefix: str) -> None:
        self.owner = owner
        self.prefix = prefix

    async def key_value(self, bucket: str) -> Any:
        self.owner.kv_prefixes.append(self.prefix)
        return FakeKV()

    async def pull_subscribe_bind(self, consumer: str, *, stream: str) -> Any:
        self.owner.pull_prefixes.append(self.prefix)
        return FakeSubscription([])


class FakeBindClient(FakeApi):
    def __init__(self, prefix: str) -> None:
        super().__init__(
            {f"{prefix}.CONSUMER.INFO.{STREAM}.{CONSUMER}": {"config": dict(_CONSUMER_CONFIG)}}
        )
        self.kv_prefixes: list[str] = []
        self.pull_prefixes: list[str] = []

    def jetstream(self, *, prefix: str = "$JS.API", timeout: float = 5.0) -> FakeJetStream:
        return FakeJetStream(self, prefix)


@pytest.mark.parametrize("prefix", ["$JS.API", "JS.RUNTIME.API"])
def test_bind_pulls_through_the_prefix_and_dead_letters_through_the_own_account(
    prefix: str,
) -> None:
    client = FakeBindClient(prefix)
    consumer = asyncio.run(
        bus.bind_command_consumer(
            client,
            stream=STREAM,
            consumer=CONSUMER,
            worker_name="worker-1",
            fetch_batch=8,
            fetch_expires_millis=1000,
            retry_delay_millis=60_000,
            ack_timeout_seconds=1.0,
            api_prefix=prefix,
        )
    )
    assert consumer.consumer == CONSUMER
    assert client.subjects == [f"{prefix}.CONSUMER.INFO.{STREAM}.{CONSUMER}"]
    assert client.pull_prefixes == [prefix]
    # The dead-letter bucket lives in the worker's OWN account (WORKER, or
    # compose's global one): always the default API prefix.
    assert client.kv_prefixes == ["$JS.API"]


@pytest.mark.parametrize(
    ("field_name", "value"),
    [
        ("durable_name", "other"),
        ("deliver_subject", "push.inbox"),
        ("ack_policy", "none"),
        ("ack_wait", 30_000_000_000),
        ("max_deliver", 5),
        ("deliver_policy", "new"),
        ("filter_subject", "elitea.rt.v1.agent.d.*"),
        ("backoff", [1_000_000_000]),
        ("headers_only", True),
        ("max_batch", 4),
        ("max_expires", 500_000_000),
    ],
)
def test_bind_refuses_a_drifted_consumer(field_name: str, value: Any) -> None:
    config = dict(_CONSUMER_CONFIG, **{field_name: value})
    with pytest.raises(CommandBusDrift) as caught:
        asyncio.run(_verify(_api(consumer={"config": config})))
    assert caught.value.retryable is False


def test_bind_refuses_an_absent_durable() -> None:
    missing = {"error": {"code": 404, "err_code": 10014, "description": "consumer not found"}}
    with pytest.raises(CommandBusAbsent):
        asyncio.run(_verify(_api(consumer=missing)))
    with pytest.raises(DependencyUnavailable):
        asyncio.run(_verify(_api(consumer=TimeoutError())))


# ── Connection ──────────────────────────────────────────────────────────────


class FakeClient:
    def __init__(self) -> None:
        self.options: dict[str, Any] = {}
        self.connect_kwargs: dict[str, Any] = {}
        self.is_reconnecting = False
        self.is_closed = False
        self.closed = 0

    async def connect(self, **kwargs: Any) -> None:
        self.connect_kwargs = kwargs
        if "tls" in kwargs:
            self.options["tls"] = kwargs["tls"]

    async def close(self) -> None:
        self.closed += 1
        self.is_closed = True


_TLS = NatsTlsPaths(Path("/ca.crt"), Path("/tls.crt"), Path("/tls.key"))


def _connection(
    *,
    url: str = "tls://nats-0.nats:4222,tls://nats-1.nats:4222",
    tls: NatsTlsPaths | None = _TLS,
    contexts: list[Any] | None = None,
    events: list[tuple[str, WorkerError | None]] | None = None,
) -> NatsCommandBusConnection:
    produced = contexts if contexts is not None else []

    def factory(paths: NatsTlsPaths) -> Any:
        assert paths == _TLS
        if produced and produced[-1] == "fail-next":
            produced.pop()
            raise InvalidInput("The NATS client TLS identity is invalid.")
        context = object()
        produced.append(context)
        return context

    sink = events if events is not None else []
    return NatsCommandBusConnection(
        url=url,
        name="worker-pod-1",
        tls=tls,
        connect_timeout_seconds=5,
        event_sink=lambda event, error: sink.append((event, error)),
        client_factory=FakeClient,
        context_factory=factory,
    )


def test_connect_presents_the_worker_identity_on_the_granted_inbox() -> None:
    async def run() -> None:
        contexts: list[Any] = []
        connection = _connection(contexts=contexts)
        client = await connection.connect()

        kwargs = client.connect_kwargs
        assert kwargs["servers"] == ["tls://nats-0.nats:4222", "tls://nats-1.nats:4222"]
        assert kwargs["inbox_prefix"] == "_INBOX_elitea-worker"
        assert kwargs["name"] == "worker-pod-1"
        assert kwargs["tls"] is contexts[0]
        assert kwargs["allow_reconnect"] is True
        assert kwargs["max_reconnect_attempts"] == -1
        assert "user" not in kwargs and "token" not in kwargs and "password" not in kwargs

    asyncio.run(run())


def test_every_reconnect_reads_the_tls_material_again() -> None:
    async def run() -> None:
        contexts: list[Any] = []
        events: list[tuple[str, WorkerError | None]] = []
        connection = _connection(contexts=contexts, events=events)
        client = await connection.connect()
        callbacks = client.connect_kwargs

        await callbacks["disconnected_cb"]()
        assert client.options["tls"] is contexts[1]

        client.is_reconnecting = True
        await callbacks["error_cb"](OSError("handshake failed"))
        assert client.options["tls"] is contexts[2]

        # A reload that fails (material mid-rotation) keeps the last context
        # and is reported; the next attempt reads the files again.
        contexts.append("fail-next")
        await callbacks["error_cb"](OSError("handshake failed"))
        assert client.options["tls"] is contexts[2]
        assert any(event == "nats_tls_reload_rejected" for event, _ in events)

        await callbacks["reconnected_cb"]()
        assert ("nats_reconnected", None) in events

    asyncio.run(run())


def test_an_error_while_connected_does_not_rebuild_the_context() -> None:
    async def run() -> None:
        contexts: list[Any] = []
        connection = _connection(contexts=contexts)
        client = await connection.connect()
        await client.connect_kwargs["error_cb"](RuntimeError("slow consumer"))
        assert len(contexts) == 1

    asyncio.run(run())


def test_plaintext_and_tls_material_are_never_mixed() -> None:
    with pytest.raises(ValueError):
        _connection(url="tls://nats:4222", tls=None)
    with pytest.raises(ValueError):
        _connection(url="nats://nats:4222", tls=_TLS)
    with pytest.raises(ValueError):
        _connection(url="tls://user:pass@nats:4222")

    async def run() -> None:
        connection = _connection(url="nats://nats:4222", tls=None)
        client = await connection.connect()
        assert "tls" not in client.connect_kwargs
        await connection.aclose()
        await connection.aclose()
        assert client.closed == 1

    asyncio.run(run())
