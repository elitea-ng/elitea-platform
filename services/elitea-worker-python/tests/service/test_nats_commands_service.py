"""Live command-bus test: the worker's REAL transport on the secured NATS server.

``go run ./libs/go/natsconn/natstest/cmd/natstest-serve -out <file>`` runs the
NATS chart's own rendered nats.conf, the test PKI and the real bootstrap.sh,
and writes the environment document this test reads from
``ELITEA_TEST_NATS_SECURE_ENV``. Every worker-side call here is presented as
the ``elitea-worker`` certificate identity, under the chart's permission
table; the producer side is ``elitea-main-runtime``. The last test reads the
server log and fails on any permission violation by the worker identity.

Without ``ELITEA_TEST_NATS_SECURE_ENV`` the module skips, unless
``ELITEA_REQUIRE_NATS_SECURE_TEST=1`` (CI sets it): then a missing environment
FAILS, because a permission test that skipped reads exactly like one that
passed.

Tests share one server and its durables, and must not run concurrently
against the same server. Each uses its own delivery IDs.
"""

from __future__ import annotations

import asyncio
import json
import os
import ssl
import time
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import pytest

from elitea_worker.execution.delivery import DeliveryResult
from elitea_worker.execution.errors import InvalidInput
from elitea_worker.serve import DEAD_LETTERED_EVENT, WorkerServeLoop
from elitea_worker.transport.nats_jetstream import (
    DEAD_LETTER_BUCKET,
    DEAD_LETTER_SCHEMA,
    CommandBusDrift,
    CommandDelivery,
    JetStreamCommandConsumer,
    NatsCommandBusConnection,
    NatsTlsPaths,
    bind_command_consumer,
    delivery_subject,
    delivery_token,
)


_ENV = "ELITEA_TEST_NATS_SECURE_ENV"
_REQUIRE = "ELITEA_REQUIRE_NATS_SECURE_TEST"
_INDEX = "ELITEA_RT_V1_INDEX"
_INDEX_DURABLE = "elitea-index-worker-v1"
_VALIDATE = "ELITEA_RT_V1_VALIDATE"
_VALIDATE_DURABLE = "elitea-configuration-worker-v1"
_WORKER = "elitea-worker"
_PRODUCER = "elitea-main-runtime"
_BOOTSTRAP = "elitea-nats-bootstrap-runtime"


def _environment() -> dict[str, Any]:
    path = os.environ.get(_ENV)
    if not path:
        if os.environ.get(_REQUIRE) == "1":
            pytest.fail(f"{_REQUIRE}=1 but {_ENV} is not set; natstest-serve did not run")
        pytest.skip(f"set {_ENV} (natstest-serve -out <file>) to run the live NATS test")
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        pytest.fail(f"{_ENV} does not name a readable natstest-serve document: {exc}")


@pytest.fixture(scope="module")
def environment() -> dict[str, Any]:
    return _environment()


def _paths(env: dict[str, Any], identity: str) -> NatsTlsPaths:
    material = env["identities"][identity]
    return NatsTlsPaths(
        ca_path=Path(material["ca"]),
        certificate_path=Path(material["cert"]),
        private_key_path=Path(material["key"]),
    )


def _plain_context(env: dict[str, Any], identity: str) -> ssl.SSLContext:
    """A test-side client context for the non-worker identities."""

    material = env["identities"][identity]
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    context.load_verify_locations(material["ca"])
    context.load_cert_chain(material["cert"], material["key"])
    return context


@asynccontextmanager
async def _client(env: dict[str, Any], identity: str) -> AsyncIterator[Any]:
    import nats

    client = await nats.connect(
        env["url"],
        tls=_plain_context(env, identity),
        inbox_prefix=env["identities"][identity]["inbox_prefix"],
        name=f"python-service-test-{identity}",
        allow_reconnect=False,
        connect_timeout=5,
    )
    try:
        yield client
    finally:
        await client.close()


@asynccontextmanager
async def _worker(
    env: dict[str, Any],
    events: list[tuple[str, Any]] | None = None,
) -> AsyncIterator[NatsCommandBusConnection]:
    """The worker's own connection class, as the elitea-worker identity."""

    sink = events if events is not None else []
    connection = NatsCommandBusConnection(
        url=env["url"],
        name=f"python-service-test-worker-{uuid.uuid4().hex[:8]}",
        tls=_paths(env, _WORKER),
        connect_timeout_seconds=5,
        event_sink=lambda event, error: sink.append((event, error)),
    )
    await connection.connect()
    try:
        yield connection
    finally:
        await connection.aclose()


async def _bind(
    connection: NatsCommandBusConnection,
    *,
    stream: str = _INDEX,
    consumer: str = _INDEX_DURABLE,
    retry_delay_millis: int = 1_000,
    worker_name: str = "python-service-test-worker",
) -> JetStreamCommandConsumer:
    return await bind_command_consumer(
        connection.client,
        stream=stream,
        consumer=consumer,
        worker_name=worker_name,
        fetch_batch=8,
        fetch_expires_millis=500,
        retry_delay_millis=retry_delay_millis,
        ack_timeout_seconds=5.0,
    )


async def _publish(producer: Any, delivery_id: str, body: bytes, stream: str = _INDEX) -> int:
    """Publish exactly as elitea-main does: subject, headers, expected stream."""

    ack = await producer.jetstream().publish(
        delivery_subject(stream, delivery_id),
        body,
        stream=stream,
        headers={
            "Nats-Msg-Id": delivery_token(delivery_id),
            "Elitea-Delivery-Id": delivery_id,
        },
    )
    return int(ack.seq)


async def _live_copy(producer: Any, stream: str, delivery_id: str) -> bool:
    """The producer's direct get of the delivery subject's live message."""

    # The last-by-subject form the producer's grant names:
    # $JS.API.DIRECT.GET.<stream>.<subject>.
    response = await producer.request(
        f"$JS.API.DIRECT.GET.{stream}.{delivery_subject(stream, delivery_id)}",
        b"",
        timeout=5,
    )
    status = (response.headers or {}).get("Status")
    if status == "404":
        return False
    assert status in (None, ""), f"direct get answered {status}"
    return True


async def _fetch_one(
    consumer: JetStreamCommandConsumer,
    delivery_id: str,
    *,
    seconds: float,
) -> CommandDelivery | None:
    """Pull until the delivery appears; anything else is left unanswered.

    Messages of earlier tests are poison-delayed or acked, so in practice
    nothing else is offered.
    """

    token = delivery_token(delivery_id)
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        for delivery in await consumer.fetch(count=1):
            if delivery.delivery_token == token:
                return delivery
    return None


def test_ack_after_settlement_removes_the_message_and_is_idempotent(
    environment: dict[str, Any],
) -> None:
    async def run() -> None:
        delivery_id = f"py-ack-{uuid.uuid4()}"
        async with _client(environment, _PRODUCER) as producer, _worker(environment) as worker:
            consumer = await _bind(worker)
            sequence = await _publish(producer, delivery_id, b"reference-only-envelope")
            assert await _live_copy(producer, _INDEX, delivery_id)

            delivery = await _fetch_one(consumer, delivery_id, seconds=5)
            assert delivery is not None
            assert delivery.stream_sequence == sequence
            assert delivery.num_delivered == 1
            assert delivery.signed_envelope == b"reference-only-envelope"
            assert delivery.rejection is None

            await consumer.ack_after_settlement(delivery, delivery_id)
            # WorkQueue: the double ack removed the message, freeing capacity.
            assert not await _live_copy(producer, _INDEX, delivery_id)
            # A second ack of the same delivery is idempotent success.
            await consumer.ack_after_settlement(delivery, delivery_id)

    asyncio.run(run())


def test_retry_later_nak_redelivers_after_the_delay_not_before(
    environment: dict[str, Any],
) -> None:
    async def run() -> None:
        delivery_id = f"py-nak-{uuid.uuid4()}"
        async with _client(environment, _PRODUCER) as producer, _worker(environment) as worker:
            consumer = await _bind(worker, retry_delay_millis=2_000)
            await _publish(producer, delivery_id, b"retry-me")

            first = await _fetch_one(consumer, delivery_id, seconds=5)
            assert first is not None
            naked_at = time.monotonic()
            await consumer.retry_later(first)

            # Not redelivered inside the delay...
            assert await _fetch_one(consumer, delivery_id, seconds=1.0) is None
            # ...and redelivered after it, as a second delivery.
            second = await _fetch_one(consumer, delivery_id, seconds=6)
            assert second is not None
            assert time.monotonic() - naked_at >= 1.9
            assert second.num_delivered == 2
            assert second.stream_sequence == first.stream_sequence

            await consumer.ack_after_settlement(second, delivery_id)
            assert not await _live_copy(producer, _INDEX, delivery_id)

    asyncio.run(run())


def test_in_progress_keeps_ownership_past_ack_wait(environment: dict[str, Any]) -> None:
    """+WPI every 5s holds a message past the 60s AckWait; nobody else gets it.

    A second worker connection pulls from the same durable the whole time.
    Without the heartbeat the message would be redelivered to it at 60s.
    """

    async def run() -> None:
        delivery_id = f"py-wpi-{uuid.uuid4()}"
        token = delivery_token(delivery_id)
        async with (
            _client(environment, _PRODUCER) as producer,
            _worker(environment) as owner_connection,
            _worker(environment) as peer_connection,
        ):
            owner = await _bind(owner_connection)
            peer = await _bind(peer_connection)
            await _publish(producer, delivery_id, b"long-running")
            delivery = await _fetch_one(owner, delivery_id, seconds=5)
            assert delivery is not None

            stolen: list[CommandDelivery] = []
            held_until = time.monotonic() + 66
            next_beat = time.monotonic()
            while time.monotonic() < held_until:
                if time.monotonic() >= next_beat:
                    assert await owner.in_progress([delivery]) == 1
                    next_beat += 5
                for offered in await peer.fetch(count=1):
                    if offered.delivery_token == token:
                        stolen.append(offered)

            assert stolen == [], "the heartbeat did not keep the message owned"
            await owner.ack_after_settlement(delivery, delivery_id)
            assert not await _live_copy(producer, _INDEX, delivery_id)

    asyncio.run(run())


def test_poison_is_dead_lettered_and_left_pending_with_the_real_serve_loop(
    environment: dict[str, Any],
) -> None:
    async def run() -> None:
        delivery_id = f"py-poison-{uuid.uuid4()}"
        worker_name = f"python-service-test-poison-{uuid.uuid4().hex[:8]}"
        events: list[tuple[str, Any]] = []
        async with (
            _client(environment, _PRODUCER) as producer,
            _client(environment, _BOOTSTRAP) as bootstrap,
            _worker(environment) as connection,
        ):
            consumer = await _bind(connection, worker_name=worker_name)
            recorded: list[tuple[str, bytes]] = []
            real_store = consumer._dead_letters  # noqa: SLF001 - observe, then delegate

            class ObservedStore:
                async def put(self, key: str, value: bytes) -> int:
                    revision = await real_store.put(key, value)
                    recorded.append((key, value))
                    return revision

            consumer._dead_letters = ObservedStore()  # noqa: SLF001
            sequence = await _publish(producer, delivery_id, b"not-a-signed-envelope")
            stop = asyncio.Event()
            processed = 0

            async def process(delivery: CommandDelivery) -> DeliveryResult:
                nonlocal processed
                if delivery.delivery_token == delivery_token(delivery_id):
                    processed += 1
                raise InvalidInput("The signed command envelope is malformed.")

            async def stop_after_record() -> None:
                while not recorded:
                    await asyncio.sleep(0.05)
                stop.set()

            loop = WorkerServeLoop(
                consumer=consumer,
                process_delivery=process,
                max_concurrency=1,
                queue_capacity=1,
                in_progress_interval_millis=1_000,
                dependency_retry_millis=100,
                shutdown_timeout_millis=5_000,
                event_sink=lambda event, error: events.append((event, error)),
            )
            watcher = asyncio.create_task(stop_after_record())
            try:
                await asyncio.wait_for(loop.run(stop), timeout=20)
            finally:
                watcher.cancel()

            assert processed == 1
            key = f"index.{delivery_token(delivery_id)}"
            assert [stored_key for stored_key, _ in recorded] == [key]
            record = json.loads(recorded[0][1])
            assert record["schema"] == DEAD_LETTER_SCHEMA
            assert record["stream"] == _INDEX
            assert record["consumer"] == _INDEX_DURABLE
            assert record["subject"] == delivery_subject(_INDEX, delivery_id)
            assert record["stream_sequence"] == sequence
            assert record["num_delivered"] == 1
            assert record["reason"] == "INVALID_INPUT"
            assert record["worker"] == worker_name
            assert b"not-a-signed-envelope" not in recorded[0][1]
            assert delivery_id.encode() not in recorded[0][1]
            assert any(event == DEAD_LETTERED_EVENT for event, _ in events)

            # The record is in the bucket (read with the bootstrap identity's
            # stream info, the only read any identity holds on it).
            info = await bootstrap.request(
                f"$JS.API.STREAM.INFO.KV_{DEAD_LETTER_BUCKET}",
                json.dumps({"subjects_filter": f"$KV.{DEAD_LETTER_BUCKET}.{key}"}).encode(),
                timeout=5,
            )
            subjects = json.loads(info.data)["state"].get("subjects") or {}
            assert subjects.get(f"$KV.{DEAD_LETTER_BUCKET}.{key}") == 1

            # Never Term: the poison stays PENDING, occupying its subject, so a
            # PostgreSQL re-offer stays a no-op; the 24h nak keeps it away.
            assert await _live_copy(producer, _INDEX, delivery_id)
            assert await _fetch_one(consumer, delivery_id, seconds=1.5) is None

    asyncio.run(run())


def test_bind_refuses_a_drifted_durable(
    environment: dict[str, Any],
) -> None:
    """A durable the bootstrap would not have created is refused at bind.

    The absent-stream leg this test used to run (delete the stream as the
    bootstrap, bind, re-create) cannot run on the chart's permissions any
    more: since #1076 no identity may delete or purge a stream. The mapping of
    a missing stream or durable to CommandBusAbsent is covered by
    tests/unit/test_nats_jetstream.py::test_bind_refuses_an_absent_stream_or_durable.
    """

    async def run() -> None:
        async with _client(environment, _BOOTSTRAP) as bootstrap, _worker(environment) as worker:
            # A bound, correct VALIDATE route first.
            await _bind(worker, stream=_VALIDATE, consumer=_VALIDATE_DURABLE)

            info = json.loads(
                (
                    await bootstrap.request(
                        f"$JS.API.CONSUMER.INFO.{_VALIDATE}.{_VALIDATE_DURABLE}",
                        b"",
                        timeout=5,
                    )
                ).data
            )
            original = {key: value for key, value in info["config"].items() if key != "metadata"}

            async def update_consumer(config: dict[str, Any]) -> None:
                # The subject the RUNTIME bootstrap may use: CREATE with the
                # durable's name and filter (what the nats CLI sends).
                response = await bootstrap.request(
                    f"$JS.API.CONSUMER.CREATE.{_VALIDATE}.{_VALIDATE_DURABLE}.{config['filter_subject']}",
                    json.dumps({"stream_name": _VALIDATE, "config": config, "action": "update"}).encode(),
                    timeout=5,
                )
                body = json.loads(response.data)
                assert "error" not in body, body

            await update_consumer(dict(original, ack_wait=30_000_000_000))
            try:
                with pytest.raises(CommandBusDrift):
                    await _bind(worker, stream=_VALIDATE, consumer=_VALIDATE_DURABLE)
            finally:
                await update_consumer(original)
            await _bind(worker, stream=_VALIDATE, consumer=_VALIDATE_DURABLE)

    asyncio.run(run())


def test_the_worker_identity_hit_no_permission_violation(
    environment: dict[str, Any],
) -> None:
    """Runs last (file order): every call above stayed inside the chart's grants.

    A canary first: the PRODUCER identity publishes to a subject its grant
    denies, and the check must find that line. Otherwise a log that records no
    violations at all (wrong file, wrong format, logging off) would pass the
    worker check vacuously.
    """

    canary = f"$KV.canary-{uuid.uuid4().hex}"

    async def violate_as_producer() -> None:
        async with _client(environment, _PRODUCER) as producer:
            await producer.publish(canary, b"")
            await producer.flush(timeout=5)
            await asyncio.sleep(0.5)

    asyncio.run(violate_as_producer())

    def violations(identity: str) -> list[str]:
        marker = f'"$G/user:{environment["identities"][identity]["user"]}"'
        log = Path(environment["log"]).read_text(encoding="utf-8", errors="replace")
        return [
            line
            for line in log.splitlines()
            if marker in line
            and (" - Publish Violation - " in line or " - Subscription Violation - " in line)
        ]

    assert any(canary in line for line in violations(_PRODUCER)), (
        "the canary violation is not in the server log; the check below would be vacuous"
    )
    worker = violations(_WORKER)
    assert worker == [], "\n".join(worker)
