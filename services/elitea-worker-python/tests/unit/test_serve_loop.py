from __future__ import annotations

import asyncio
import hashlib
import json
from collections.abc import Sequence
from types import SimpleNamespace

import pytest

import elitea_worker.serve as serve_module
from elitea_worker.execution.delivery import (
    DeliveryDisposition,
    DeliveryResult,
)
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DependencyUnavailable,
    InvalidInput,
    WorkerError,
)
from elitea_worker.serve import (
    DEAD_LETTERED_EVENT,
    ProductionDeliveryProcessor,
    WorkerServeLoop,
    _ShutdownBudget,
    _close_runtime_resources,
    _prepare_execution_spool,
    _wait_for_nats,
)
from elitea_worker.transport.nats_jetstream import (
    CommandDelivery,
    CommandSignatureRejected,
    SubjectTokenMismatch,
    TransportMessageRejected,
)


STREAM = "ELITEA_RT_V1_INDEX"
CONSUMER = "elitea-index-worker-v1"


def _delivery(sequence: int, *, num_delivered: int = 1, delivery_id: str | None = None) -> CommandDelivery:
    token = hashlib.sha256((delivery_id or f"outbox-{sequence}").encode()).hexdigest()
    return CommandDelivery(
        stream=STREAM,
        consumer=CONSUMER,
        subject=f"elitea.rt.v1.index.d.{token}",
        stream_sequence=sequence,
        num_delivered=num_delivered,
        signed_envelope=b"reference",
    )


class FakeConsumer:
    """The serve loop's view of the bus, recording every answer it gives.

    ``redeliver`` re-offers the LAST delivery on every later pull, the way the
    server redelivers a message after AckWait or after a nak delay.
    """

    def __init__(
        self,
        deliveries: tuple[CommandDelivery, ...],
        *,
        batch: int = 2,
        redeliver: bool = False,
        fetch_seconds: float = 0.001,
    ) -> None:
        self._deliveries = deliveries
        self._offset = 0
        self._batch = batch
        self._redeliver = redeliver
        self._fetch_seconds = fetch_seconds
        self.fetch_counts: list[int] = []
        self.in_progress_calls: list[tuple[CommandDelivery, ...]] = []
        self.retried: list[CommandDelivery] = []
        self.parked: list[CommandDelivery] = []
        self.dead_lettered: list[tuple[CommandDelivery, str]] = []
        self.dead_letter_error: Exception | None = None
        self.dead_letter_failures: list[tuple[CommandDelivery, str]] = []
        self.terminated: list[CommandDelivery] = []
        self.answers: list[str] = []

    @property
    def delivery_batch_size(self) -> int:
        return self._batch

    async def fetch(self, *, count: int | None = None) -> tuple[CommandDelivery, ...]:
        assert count is not None and 1 <= count <= self._batch
        self.fetch_counts.append(count)
        if self._offset < len(self._deliveries):
            end = self._offset + count
            deliveries = self._deliveries[self._offset : end]
            self._offset = min(end, len(self._deliveries))
            return deliveries
        await asyncio.sleep(self._fetch_seconds)
        if self._redeliver and self._deliveries:
            last = self._deliveries[-1]
            return (
                CommandDelivery(
                    stream=last.stream,
                    consumer=last.consumer,
                    subject=last.subject,
                    stream_sequence=last.stream_sequence,
                    num_delivered=len(self.fetch_counts),
                    signed_envelope=last.signed_envelope,
                    rejection=last.rejection,
                ),
            )
        return ()

    async def in_progress(self, deliveries: Sequence[CommandDelivery]) -> int:
        self.in_progress_calls.append(tuple(deliveries))
        return len(deliveries)

    async def retry_later(self, delivery: CommandDelivery) -> None:
        self.retried.append(delivery)

    async def park(self, delivery: CommandDelivery) -> None:
        self.answers.append("park")
        self.parked.append(delivery)

    async def terminate(self, delivery: CommandDelivery) -> None:
        self.answers.append("term")
        self.terminated.append(delivery)

    async def release(self, delivery: CommandDelivery) -> None:
        self.answers.append("release")

    async def record_dead_letter(self, delivery: CommandDelivery, *, reason: str) -> None:
        if self.dead_letter_error is not None:
            self.dead_letter_failures.append((delivery, reason))
            raise self.dead_letter_error
        self.answers.append("record")
        self.dead_lettered.append((delivery, reason))


def _runtime(consumer: FakeConsumer, process, **overrides) -> WorkerServeLoop:
    arguments = {
        "consumer": consumer,
        "process_delivery": process,
        "max_concurrency": 1,
        "queue_capacity": 1,
        "in_progress_interval_millis": 1,
        "dependency_retry_millis": 1,
        "shutdown_timeout_millis": 1000,
    }
    arguments.update(overrides)
    return WorkerServeLoop(**arguments)


def test_serve_loop_bounds_workers_and_drains() -> None:
    async def run() -> None:
        deliveries = tuple(_delivery(index) for index in range(1, 4))
        consumer = FakeConsumer(deliveries)
        stop = asyncio.Event()
        active = 0
        peak = 0
        processed: list[int] = []

        async def process(delivery: CommandDelivery) -> DeliveryResult:
            nonlocal active, peak
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(0.01)
            processed.append(delivery.stream_sequence)
            active -= 1
            if len(processed) == len(deliveries):
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = _runtime(
            consumer,
            process,
            max_concurrency=2,
            queue_capacity=2,
            in_progress_interval_millis=100,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.5)

        assert sorted(processed) == [1, 2, 3]
        assert peak == 2
        # RETRY_LATER is answered with NakWithDelay, freeing the slot now.
        assert sorted(d.stream_sequence for d in consumer.retried) == [1, 2, 3]

    asyncio.run(run())


def test_serve_loop_sustains_the_configured_delivery_cap() -> None:
    """The serve loop runs the full delivery cap at once, not a queue behind it.

    Pinning the cap in CI is the load check for issue #967: a regression that
    serialized the slots would make 96 flows of 0.1 s take 9.6 s instead of
    three 0.1 s waves. `peak == cap` proves the slots run together; the
    elapsed bound keeps the waves from hiding behind one another.
    """

    async def run() -> None:
        cap = 32
        flow_seconds = 0.1
        waves = 3
        total = cap * waves
        deliveries = tuple(_delivery(index) for index in range(1, total + 1))
        consumer = FakeConsumer(deliveries, batch=64)
        stop = asyncio.Event()
        active = 0
        peak = 0
        processed: list[int] = []

        async def process(delivery: CommandDelivery) -> DeliveryResult:
            nonlocal active, peak
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(flow_seconds)
            processed.append(delivery.stream_sequence)
            active -= 1
            if len(processed) == total:
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        started = asyncio.get_running_loop().time()
        runtime = _runtime(
            consumer,
            process,
            max_concurrency=cap,
            queue_capacity=2 * cap,
            in_progress_interval_millis=100,
            shutdown_timeout_millis=5_000,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=2.0)
        elapsed = asyncio.get_running_loop().time() - started

        assert len(processed) == total
        assert set(processed) == set(range(1, total + 1))
        assert peak == cap
        assert elapsed <= 1.2 * (waves * flow_seconds) + 0.5

    asyncio.run(run())


def test_serve_loop_reports_settled_execution_error_without_another_answer() -> None:
    async def run() -> None:
        consumer = FakeConsumer((_delivery(1),))
        stop = asyncio.Event()
        events: list[tuple[str, object]] = []
        failure = AuthorizationFailure("The scoped content grant was rejected.")

        async def process(_: CommandDelivery) -> DeliveryResult:
            stop.set()
            return DeliveryResult(
                DeliveryDisposition.EXECUTED_SETTLED_ACKED,
                execution_error=failure,
            )

        runtime = _runtime(
            consumer,
            process,
            in_progress_interval_millis=100,
            event_sink=lambda event, error: events.append((event, error)),
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

        assert events == [("executed_settled_acked", failure)]
        # The processor acked after the settlement receipt; the loop adds no nak.
        assert consumer.retried == consumer.parked == consumer.dead_lettered == []

    asyncio.run(run())


@pytest.mark.parametrize(
    "disposition",
    [
        DeliveryDisposition.OWNED_ELSEWHERE_NOACK,
        DeliveryDisposition.RECOVERY_REQUIRED_NOACK,
        DeliveryDisposition.RETRY_LATER_NOACK,
    ],
)
def test_every_not_now_disposition_is_a_retry_delay_nak(
    disposition: DeliveryDisposition,
) -> None:
    async def run() -> None:
        delivery = _delivery(1)
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            stop.set()
            return DeliveryResult(disposition)

        await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=0.2)

        assert consumer.retried == [delivery]
        assert consumer.dead_lettered == []

    asyncio.run(run())


def test_serve_loop_reports_redacted_code_location_for_unexpected_failure(
    capsys: pytest.CaptureFixture[str],
) -> None:
    consumer = FakeConsumer((_delivery(1),))

    async def run() -> None:
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            stop.set()
            raise ValueError("credential-shaped diagnostic must not be logged")

        runtime = _runtime(consumer, process, in_progress_interval_millis=100)
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

    asyncio.run(run())
    diagnostic = json.loads(capsys.readouterr().err)
    assert diagnostic["event"] == "delivery_internal_failure"
    assert diagnostic["exception_name"] == "ValueError"
    assert diagnostic["frames"][-1]["function"] == "process"
    assert diagnostic["causes"] == []
    assert "credential-shaped" not in json.dumps(diagnostic)
    # Unknown is not poison: it goes back with the retry delay.
    assert len(consumer.retried) == 1
    assert consumer.dead_lettered == []


def test_a_retryable_rejection_is_retried_not_dead_lettered() -> None:
    async def run() -> None:
        consumer = FakeConsumer((_delivery(1),))
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            stop.set()
            raise DependencyUnavailable("control plane unavailable")

        await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=0.2)

        assert len(consumer.retried) == 1
        assert consumer.dead_lettered == []

    asyncio.run(run())


def test_serve_loop_does_not_run_a_redelivered_owned_message_concurrently() -> None:
    """AckWait can pass before a heartbeat lands; the redelivery is not run.

    The newest delivery of the sequence takes over the heartbeat, because its
    reply subject is the one the server now tracks.
    """

    async def run() -> None:
        consumer = FakeConsumer((_delivery(1),), redeliver=True)
        stop = asyncio.Event()
        calls = 0

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            while len(consumer.fetch_counts) < 3 or not any(
                call and call[0].num_delivered > 1
                for call in consumer.in_progress_calls
            ):
                await asyncio.sleep(0)
            stop.set()
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=1.0)

        assert calls == 1
        assert consumer.in_progress_calls[0][0].stream_sequence == 1
        assert consumer.retried == consumer.parked == []

    asyncio.run(run())


def test_serve_loop_dead_letters_a_non_retryable_delivery_once() -> None:
    """A delivery that cannot succeed runs ONCE and is dead-lettered once.

    Measured under the Redis transport: one undeliverable output rejected
    every 15-45s for 13 minutes, because `retryable: false` was printed and
    ignored. Here the poison is naked for 24h and recorded; if the server
    offers it again (after the delay) it is parked without running.
    """

    async def run() -> None:
        delivery = _delivery(1)
        consumer = FakeConsumer((delivery,), redeliver=True)
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, WorkerError | None]] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise AuthorizationFailure(
                "The durable output spool uses a different claim fence; "
                "server-side recovery is required."
            )

        async def stop_after_repeated_offers() -> None:
            while len(consumer.parked) < 3:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer,
            process,
            event_sink=lambda event, error: events.append((event, error)),
        )
        watcher = asyncio.create_task(stop_after_repeated_offers())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert calls == 1, f"the doomed delivery ran {calls} times, not once"
        assert consumer.dead_lettered == [(delivery, "AUTHORIZATION_FAILED")]
        assert len(consumer.parked) >= 3
        assert consumer.retried == []
        assert runtime.dead_lettered == 1

        announced = [error for event, error in events if event == DEAD_LETTERED_EVENT]
        assert len(announced) == 1, "the dead letter must be announced exactly once"
        notice = announced[0]
        assert notice is not None
        assert notice.code == "AUTHORIZATION_FAILED"
        assert delivery.entry_id in notice.safe_message
        assert delivery.dead_letter_key in notice.safe_message
        assert "PENDING" in notice.safe_message
        assert not any(event == "delivery_quarantine_full" for event, _ in events)
        assert any(event == "delivery_rejected" for event, _ in events)

    asyncio.run(run())


def test_dead_lettered_event_is_an_error_line(capsys: pytest.CaptureFixture[str]) -> None:
    assert DEAD_LETTERED_EVENT == "worker_command.dead_lettered"
    serve_module._emit_runtime_event(
        DEAD_LETTERED_EVENT, InvalidInput("The signed command is malformed.")
    )
    line = json.loads(capsys.readouterr().err)
    assert line["event"] == "worker_command.dead_lettered"
    assert line["level"] == "error"
    assert line["code"] == "INVALID_INPUT"


def test_a_poison_transport_shape_is_dead_lettered_without_processing() -> None:
    async def run() -> None:
        rejected = CommandDelivery(
            stream=STREAM,
            consumer=CONSUMER,
            subject="elitea.rt.v1.index.d.not-a-hash",
            stream_sequence=5,
            num_delivered=1,
            signed_envelope=b"",
            rejection=TransportMessageRejected("SUBJECT_MALFORMED", "bad subject"),
        )
        consumer = FakeConsumer((rejected,))
        stop = asyncio.Event()
        calls = 0

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        async def stop_after_dead_letter() -> None:
            while not consumer.dead_lettered:
                await asyncio.sleep(0)
            stop.set()

        watcher = asyncio.create_task(stop_after_dead_letter())
        try:
            await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert calls == 0
        assert consumer.dead_lettered == [(rejected, "SUBJECT_MALFORMED")]

    asyncio.run(run())


def test_an_unwritable_dead_letter_is_retried_never_parked_without_a_record(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """D3: no poison is parked (or terminated) without its record.

    The failed write is counted and announced on its own ERROR line, the
    poison is nak'd with the ordinary retry delay instead of 24h, and its next
    delivery retries the RECORD without running the command again.
    """

    async def run() -> None:
        delivery = _delivery(1)
        consumer = FakeConsumer((delivery,), redeliver=True)
        consumer.dead_letter_error = DependencyUnavailable("bucket unreachable")
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, WorkerError | None]] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise InvalidInput("The signed command is malformed.")

        async def heal_then_stop() -> None:
            while len(consumer.dead_letter_failures) < 2:
                await asyncio.sleep(0)
            consumer.dead_letter_error = None
            while not consumer.parked:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer, process, event_sink=lambda event, error: events.append((event, error))
        )
        watcher = asyncio.create_task(heal_then_stop())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert calls == 1, "a poison whose record failed ran again"
        # Every failed write was answered with the retry delay, not parked.
        assert len(consumer.retried) >= 2
        assert runtime.dead_letter_write_failures >= 2
        failed = [error for event, error in events if event == serve_module.DEAD_LETTER_WRITE_FAILED_EVENT]
        assert len(failed) == runtime.dead_letter_write_failures
        assert failed[-1] is not None
        assert f"dead_letter_write_failures_total={len(failed)}" in failed[-1].safe_message
        # Healed: the record is written and only THEN the poison is parked
        # and announced, once.
        assert consumer.dead_lettered and consumer.dead_lettered[0][1] == "INVALID_INPUT"
        assert [event for event, _ in events].count(DEAD_LETTERED_EVENT) == 1
        assert runtime.dead_lettered == 1

    asyncio.run(run())
    serve_module._emit_runtime_event(
        serve_module.DEAD_LETTER_WRITE_FAILED_EVENT, InvalidInput("x")
    )
    line = json.loads(capsys.readouterr().err.strip().splitlines()[-1])
    assert line["event"] == "worker_command.dead_letter_write_failed"
    assert line["level"] == "error"


class FakeQuarantineStore:
    """In-memory stand-in for the durable record, with the same contract."""

    def __init__(self, recorded: frozenset[str] = frozenset(), *, cap: int = 8) -> None:
        self.recorded = set(recorded)
        self.added: list[tuple[str, str]] = []
        self.load_calls = 0
        self._cap = cap
        self.load_error: Exception | None = None

    @property
    def cap(self) -> int:
        return self._cap

    async def load(self) -> frozenset[str]:
        self.load_calls += 1
        if self.load_error is not None:
            raise self.load_error
        return frozenset(self.recorded)

    async def add(self, entry_id: str, *, reason_code: str) -> bool:
        self.added.append((entry_id, reason_code))
        if len(self.recorded) >= self._cap:
            return False
        self.recorded.add(entry_id)
        return True


def test_serve_loop_never_runs_a_durably_quarantined_delivery() -> None:
    """A restart must not re-run what a previous process already dead-lettered.

    The server's 24h nak delay covers the restart itself; this covers the one
    redelivery after that delay. `calls == 0` is the assertion that matters.
    """

    async def run() -> None:
        delivery = _delivery(1)
        consumer = FakeConsumer((delivery,), redeliver=True)
        store = FakeQuarantineStore(frozenset({delivery.dead_letter_key}))
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, object]] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        async def stop_after_repeated_offers() -> None:
            while len(consumer.parked) < 3:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer,
            process,
            event_sink=lambda event, error: events.append((event, error)),
            quarantine_store=store,
        )
        watcher = asyncio.create_task(stop_after_repeated_offers())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert store.load_calls == 1, "the durable record is read exactly once"
        assert calls == 0, f"a parked delivery ran {calls} times"
        assert consumer.dead_lettered == []
        assert any(event[0] == "quarantine_loaded" for event in events)

    asyncio.run(run())


def test_serve_loop_records_a_quarantine_durably() -> None:
    """The refusal is written through, keyed by the dead-letter key."""

    async def run() -> None:
        delivery = _delivery(7)
        consumer = FakeConsumer((delivery,))
        store = FakeQuarantineStore()
        stop = asyncio.Event()
        events: list[tuple[str, object]] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            raise AuthorizationFailure("fence moved; server-side recovery is required.")

        async def stop_after_write() -> None:
            while not store.added:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer,
            process,
            event_sink=lambda event, error: events.append((event, error)),
            quarantine_store=store,
        )
        watcher = asyncio.create_task(stop_after_write())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert store.added == [(delivery.dead_letter_key, "AUTHORIZATION_FAILED")]
        assert not any(event[0] == "quarantine_store_full" for event in events)

    asyncio.run(run())


def test_serve_loop_still_quarantines_when_the_durable_load_fails() -> None:
    """A store outage must degrade, not resume the spin or refuse to start."""

    async def run() -> None:
        consumer = FakeConsumer((_delivery(9),), redeliver=True)
        store = FakeQuarantineStore()
        store.load_error = RuntimeError("disk is gone")
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, object]] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise AuthorizationFailure("fence moved")

        async def stop_after_repeated_offers() -> None:
            while len(consumer.parked) < 3:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer,
            process,
            event_sink=lambda event, error: events.append((event, error)),
            quarantine_store=store,
        )
        watcher = asyncio.create_task(stop_after_repeated_offers())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert any(event[0] == "quarantine_load_unavailable" for event in events)
        assert calls == 1, f"the doomed delivery ran {calls} times, not once"

    asyncio.run(run())


def test_serve_loop_heartbeats_queued_and_active_messages() -> None:
    async def run() -> None:
        deliveries = tuple(_delivery(index) for index in range(1, 4))
        consumer = FakeConsumer(deliveries, batch=3)
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            while len(consumer.in_progress_calls) < 2:
                await asyncio.sleep(0)
            stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        await _runtime(consumer, process, queue_capacity=3).run(stop)

        first = consumer.in_progress_calls[0]
        assert sorted(d.stream_sequence for d in first) == [1, 2, 3]

    asyncio.run(run())


def test_serve_loop_limits_fetch_to_unowned_delivery_capacity() -> None:
    async def run() -> None:
        deliveries = tuple(_delivery(index) for index in range(1, 4))
        consumer = FakeConsumer(deliveries, batch=3)
        stop = asyncio.Event()
        first_started = asyncio.Event()
        release_first = asyncio.Event()
        processed: list[int] = []

        async def process(delivery: CommandDelivery) -> DeliveryResult:
            processed.append(delivery.stream_sequence)
            if delivery.stream_sequence == 1:
                first_started.set()
                await release_first.wait()
            if len(processed) == len(deliveries):
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        running = asyncio.create_task(_runtime(consumer, process).run(stop))
        await asyncio.wait_for(first_started.wait(), timeout=0.2)

        while not consumer.in_progress_calls:
            await asyncio.sleep(0)
        # One active plus one queued: two permits, so the pull asked for two.
        assert consumer.fetch_counts == [2]
        assert sorted(d.stream_sequence for d in consumer.in_progress_calls[0]) == [1, 2]

        release_first.set()
        await asyncio.wait_for(running, timeout=0.5)
        assert processed == [1, 2, 3]

    asyncio.run(run())


def test_serve_loop_keeps_heartbeat_alive_while_shutdown_drains() -> None:
    async def run() -> None:
        stop = asyncio.Event()
        heartbeat_after_stop = asyncio.Event()

        class ShutdownAwareConsumer(FakeConsumer):
            async def in_progress(self, deliveries: Sequence[CommandDelivery]) -> int:
                refreshed = await super().in_progress(deliveries)
                if stop.is_set():
                    heartbeat_after_stop.set()
                return refreshed

        consumer = ShutdownAwareConsumer((_delivery(1),))
        processing = asyncio.Event()
        release = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            processing.set()
            await release.wait()
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        running = asyncio.create_task(_runtime(consumer, process).run(stop))
        await asyncio.wait_for(processing.wait(), timeout=0.2)

        stop.set()
        fetches_at_stop = len(consumer.fetch_counts)
        await asyncio.wait_for(heartbeat_after_stop.wait(), timeout=0.2)
        assert not running.done()

        release.set()
        await asyncio.wait_for(running, timeout=0.2)
        # Shutdown stopped the pulls.
        assert len(consumer.fetch_counts) <= fetches_at_stop + 1

    asyncio.run(run())


def test_shutdown_deadline_leaves_owned_work_unanswered_for_ackwait() -> None:
    async def run() -> None:
        consumer = FakeConsumer((_delivery(1),))
        stop = asyncio.Event()
        processing = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            processing.set()
            await asyncio.Event().wait()
            raise AssertionError("unreachable")

        running = asyncio.create_task(
            _runtime(consumer, process, shutdown_timeout_millis=20).run(stop)
        )
        await asyncio.wait_for(processing.wait(), timeout=0.2)
        stop.set()

        with pytest.raises(DependencyUnavailable, match="shutdown deadline"):
            await asyncio.wait_for(running, timeout=0.5)
        await asyncio.sleep(0)
        # No ack, no nak: AckWait redelivers the message to a live worker.
        assert consumer.retried == consumer.parked == consumer.dead_lettered == []

    asyncio.run(run())


def test_serve_loop_reports_a_failed_pull_and_keeps_pulling() -> None:
    async def run() -> None:
        stop = asyncio.Event()
        events: list[str] = []

        class FlakyConsumer(FakeConsumer):
            async def fetch(self, *, count: int | None = None):
                self.fetch_counts.append(count or 0)
                if len(self.fetch_counts) == 1:
                    raise ConnectionError("disconnected")
                if len(self.fetch_counts) == 2:
                    raise InvalidInput("JetStream delivered a message without metadata.")
                stop.set()
                return ()

        consumer = FlakyConsumer(())
        runtime = _runtime(
            consumer,
            lambda _delivery: asyncio.sleep(0),
            event_sink=lambda event, _error: events.append(event),
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.5)

        assert events[:2] == ["nats_fetch_unavailable", "nats_fetch_rejected"]
        assert len(consumer.fetch_counts) >= 3

    asyncio.run(run())


def test_serve_loop_fails_when_background_task_exits_unexpectedly() -> None:
    class ExitingHeartbeatRuntime(WorkerServeLoop):
        async def _heartbeat_loop(self) -> None:
            return

    async def run() -> None:
        runtime = ExitingHeartbeatRuntime(
            consumer=FakeConsumer(()),
            process_delivery=lambda _delivery: asyncio.sleep(0),
            max_concurrency=1,
            queue_capacity=1,
            in_progress_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )

        with pytest.raises(DependencyUnavailable, match="exited unexpectedly"):
            await asyncio.wait_for(runtime.run(asyncio.Event()), timeout=0.2)

    asyncio.run(run())


def test_production_processor_refuses_a_subject_that_does_not_name_the_command(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """After the signature and before the claim: a mismatch is poison."""

    monkeypatch.setattr(
        serve_module,
        "parse_and_verify_signed_command",
        lambda raw, authenticator: (None, SimpleNamespace(idempotency_key="outbox-2")),
    )

    def binding_must_not_run(*_args, **_kwargs):
        raise AssertionError("the claim path ran for a mismatched subject")

    monkeypatch.setattr(serve_module, "_execution_spool_binding", binding_must_not_run)
    processor = ProductionDeliveryProcessor.__new__(ProductionDeliveryProcessor)
    processor._authenticator = object()  # type: ignore[attr-defined]

    with pytest.raises(SubjectTokenMismatch) as caught:
        asyncio.run(processor.process(_delivery(1, delivery_id="outbox-1")))
    assert caught.value.retryable is False


def test_production_processor_turns_a_signature_failure_into_terminal_poison(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def refuse(raw, authenticator):
        raise AuthorizationFailure("The signed worker command signature is invalid.")

    monkeypatch.setattr(serve_module, "parse_and_verify_signed_command", refuse)
    processor = ProductionDeliveryProcessor.__new__(ProductionDeliveryProcessor)
    processor._authenticator = object()  # type: ignore[attr-defined]

    with pytest.raises(CommandSignatureRejected) as caught:
        asyncio.run(processor.process(_delivery(1)))
    assert caught.value.code == "AUTHORIZATION_FAILED"
    assert caught.value.retryable is False


@pytest.mark.parametrize(
    "error",
    [
        CommandSignatureRejected("The signed worker command signature is invalid."),
        SubjectTokenMismatch(),
    ],
)
def test_unverifiable_poison_is_recorded_then_terminated_never_parked(
    error: WorkerError,
) -> None:
    """S2: it can never become valid, so Term frees its stream capacity.

    The record comes first. Nothing is quarantined: a PostgreSQL re-offer of
    the same delivery fails the same pre-claim check and is terminated again.
    """

    async def run() -> None:
        delivery = _delivery(1)
        consumer = FakeConsumer((delivery,), redeliver=True)
        stop = asyncio.Event()
        calls = 0
        events: list[str] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise error

        async def stop_after_two() -> None:
            while len(consumer.terminated) < 2:
                await asyncio.sleep(0)
            stop.set()

        runtime = _runtime(
            consumer, process, event_sink=lambda event, _error: events.append(event)
        )
        watcher = asyncio.create_task(stop_after_two())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert consumer.parked == consumer.retried == []
        assert consumer.answers[:4] == ["record", "term", "record", "term"]
        assert consumer.dead_lettered[0] == (delivery, error.code)
        assert calls >= 2
        assert events.count(DEAD_LETTERED_EVENT) >= 2

    asyncio.run(run())


def test_a_terminal_poison_whose_record_fails_is_retried_not_terminated() -> None:
    async def run() -> None:
        consumer = FakeConsumer((_delivery(1),))
        consumer.dead_letter_error = DependencyUnavailable("bucket unreachable")
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            raise SubjectTokenMismatch()

        async def stop_after_retry() -> None:
            while not consumer.retried:
                await asyncio.sleep(0)
            stop.set()

        watcher = asyncio.create_task(stop_after_retry())
        try:
            await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert consumer.terminated == consumer.parked == []
        assert len(consumer.retried) == 1

    asyncio.run(run())


class _HungCloser:
    def __init__(self) -> None:
        self.started = asyncio.Event()
        self.cancelled = asyncio.Event()

    async def hang(self) -> None:
        self.started.set()
        try:
            await asyncio.Event().wait()
        finally:
            self.cancelled.set()


class _HungSupervisor(_HungCloser):
    def __init__(self) -> None:
        super().__init__()
        self.admission_stopped = False
        self.timeout_seconds: float | None = None

    def stop_admission(self) -> None:
        self.admission_stopped = True

    async def shutdown(self, *, timeout_seconds: float) -> None:
        self.timeout_seconds = timeout_seconds
        await self.hang()


class _HungHttpClient(_HungCloser):
    async def aclose(self) -> None:
        await self.hang()


class _HungChannel(_HungCloser):
    def __init__(self) -> None:
        super().__init__()
        self.grace: float | None = None

    async def close(self, *, grace: float) -> None:
        self.grace = grace
        await self.hang()


class _HungNatsConnection(_HungCloser):
    async def aclose(self) -> None:
        await self.hang()


def test_runtime_dependency_closes_share_one_global_deadline() -> None:
    async def run() -> None:
        supervisor = _HungSupervisor()
        http_client = _HungHttpClient()
        control_channel = _HungChannel()
        output_channel = _HungChannel()
        nats = _HungNatsConnection()
        closers = (
            supervisor,
            http_client,
            control_channel,
            output_channel,
            nats,
        )
        budget = _ShutdownBudget(timeout_seconds=0.05)
        budget.arm()
        started = asyncio.get_running_loop().time()

        with pytest.raises(DependencyUnavailable, match="shutdown deadline"):
            await _close_runtime_resources(
                supervisor=supervisor,  # type: ignore[arg-type]
                http_client=http_client,  # type: ignore[arg-type]
                control=control_channel,  # type: ignore[arg-type]
                output=output_channel,  # type: ignore[arg-type]
                nats=nats,  # type: ignore[arg-type]
                budget=budget,
            )

        elapsed = asyncio.get_running_loop().time() - started
        await asyncio.sleep(0)
        assert elapsed < 0.2
        assert supervisor.admission_stopped
        assert all(closer.started.is_set() for closer in closers)
        assert all(closer.cancelled.is_set() for closer in closers)
        assert supervisor.timeout_seconds is not None
        assert supervisor.timeout_seconds <= 0.05
        assert control_channel.grace is not None
        assert control_channel.grace <= 0.05
        assert output_channel.grace is not None
        assert output_channel.grace <= 0.05

    asyncio.run(run())


def test_shutdown_interrupts_a_hung_nats_startup_connect() -> None:
    class HungConnection:
        def __init__(self) -> None:
            self.started = asyncio.Event()
            self.cancelled = asyncio.Event()

        async def connect(self) -> None:
            self.started.set()
            try:
                await asyncio.Event().wait()
            finally:
                self.cancelled.set()

    async def run() -> None:
        connection = HungConnection()
        stop = asyncio.Event()
        config = SimpleNamespace(
            limits=SimpleNamespace(dependency_retry_millis=1000)
        )
        waiting = asyncio.create_task(
            _wait_for_nats(connection, config, stop)  # type: ignore[arg-type]
        )
        await connection.started.wait()
        stop.set()

        assert await asyncio.wait_for(waiting, timeout=0.2) is False
        await asyncio.sleep(0)
        assert connection.cancelled.is_set()

    asyncio.run(run())


def test_a_failed_nats_startup_connect_is_retried() -> None:
    class FlakyConnection:
        def __init__(self) -> None:
            self.attempts = 0

        async def connect(self) -> None:
            self.attempts += 1
            if self.attempts == 1:
                raise OSError("no servers")

    async def run() -> None:
        connection = FlakyConnection()
        config = SimpleNamespace(limits=SimpleNamespace(dependency_retry_millis=1))
        assert await _wait_for_nats(connection, config, asyncio.Event()) is True  # type: ignore[arg-type]
        assert connection.attempts == 2

    asyncio.run(run())


def test_execution_spool_rejects_existing_directory_symlink(tmp_path) -> None:
    root = tmp_path / "spool"
    root.mkdir(mode=0o700)
    outside = tmp_path / "outside"
    outside.mkdir(mode=0o700)
    binding = b"execution-binding"
    derived = root / hashlib.sha256(binding).hexdigest()
    derived.symlink_to(outside, target_is_directory=True)

    with pytest.raises(InvalidInput, match="unsafe"):
        _prepare_execution_spool(root, binding)


def test_serve_deployment_maps_cleanup_failure_to_safe_error(monkeypatch) -> None:
    async def fail_during_cleanup(_config, *, stop=None) -> None:
        del stop
        raise OSError("private-runtime-path-must-not-leak")

    monkeypatch.setattr(serve_module, "_serve_deployment_inner", fail_during_cleanup)

    async def run() -> None:
        with pytest.raises(DependencyUnavailable) as caught:
            await serve_module.serve_deployment(object())  # type: ignore[arg-type]

        assert caught.value.safe_message == "A required runtime dependency is unavailable."
        assert "private-runtime-path-must-not-leak" not in str(caught.value)

    asyncio.run(run())


def test_a_failure_after_the_ack_is_reported_never_dead_lettered() -> None:
    async def run() -> None:
        settled_message = SimpleNamespace(is_acked=True)
        delivery = CommandDelivery(
            stream=STREAM,
            consumer=CONSUMER,
            subject=_delivery(1).subject,
            stream_sequence=1,
            num_delivered=1,
            signed_envelope=b"reference",
            message=settled_message,
        )
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()
        events: list[str] = []

        async def process(_: CommandDelivery) -> DeliveryResult:
            stop.set()
            raise InvalidInput("spool cleanup failed after the settlement ack")

        await asyncio.wait_for(
            _runtime(
                consumer, process, event_sink=lambda event, _error: events.append(event)
            ).run(stop),
            timeout=0.5,
        )

        assert "delivery_rejected" in events
        assert DEAD_LETTERED_EVENT not in events
        assert consumer.dead_lettered == []

    asyncio.run(run())


# ── D2: no +WPI after a delayed nak ─────────────────────────────────────────


class OrderedConsumer(FakeConsumer):
    """Records the ORDER in which answers reach the server.

    A heartbeat round's +WPI is recorded when its send COMPLETES, after
    ``wpi_gate`` opens, which is how a round that started before a nak can
    still land after it on a slow connection.
    """

    def __init__(self, *args, **kwargs) -> None:
        super().__init__(*args, **kwargs)
        self.wire: list[tuple[str, int, int]] = []
        self.wpi_gate: asyncio.Event | None = None
        self.wpi_started = asyncio.Event()

    async def in_progress(self, deliveries: Sequence[CommandDelivery]) -> int:
        self.wpi_started.set()
        if self.wpi_gate is not None:
            await self.wpi_gate.wait()
        for delivery in deliveries:
            self.wire.append(("+WPI", delivery.stream_sequence, delivery.num_delivered))
        return await super().in_progress(deliveries)

    async def retry_later(self, delivery: CommandDelivery) -> None:
        self.wire.append(("-NAK", delivery.stream_sequence, delivery.num_delivered))
        # The server honours the delay: no redelivery inside this test.
        self._redeliver = False
        await super().retry_later(delivery)

    async def park(self, delivery: CommandDelivery) -> None:
        self.wire.append(("-NAK24h", delivery.stream_sequence, delivery.num_delivered))
        await super().park(delivery)


def _no_wpi_after_nak(wire: list[tuple[str, int, int]], sequence: int) -> None:
    naks = [
        index
        for index, (kind, seq, _) in enumerate(wire)
        if kind.startswith("-NAK") and seq == sequence
    ]
    assert naks, f"no nak of {sequence} reached the wire: {wire}"
    after = [
        entry for entry in wire[naks[0] + 1 :] if entry[0] == "+WPI" and entry[1] == sequence
    ]
    assert after == [], f"a +WPI followed the delayed nak of {sequence}: {wire}"


@pytest.mark.parametrize("poison", [False, True])
def test_a_heartbeat_round_in_flight_never_lands_after_a_delayed_nak(poison: bool) -> None:
    async def run() -> None:
        consumer = OrderedConsumer((_delivery(1),))
        consumer.wpi_gate = asyncio.Event()
        stop = asyncio.Event()

        async def process(_: CommandDelivery) -> DeliveryResult:
            # A heartbeat round has snapshot this delivery and is mid-send...
            await consumer.wpi_started.wait()
            # ...and the processing ends while it is still in flight.
            asyncio.get_running_loop().call_later(0.02, consumer.wpi_gate.set)
            if poison:
                raise InvalidInput("The signed command is malformed.")
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        async def stop_after_answer() -> None:
            while not consumer.retried and not consumer.dead_lettered:
                await asyncio.sleep(0)
            # Several more heartbeat periods: none may carry sequence 1 now.
            await asyncio.sleep(0.05)
            stop.set()

        watcher = asyncio.create_task(stop_after_answer())
        try:
            await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=2.0)
        finally:
            watcher.cancel()

        _no_wpi_after_nak(consumer.wire, 1)
        # The in-flight round did land — before the nak, not after it.
        assert consumer.wire[0][0] == "+WPI"

    asyncio.run(run())


def test_a_redelivered_owned_message_is_answered_on_its_newest_copy_once() -> None:
    """The server tracks the newest delivery; that copy gets the nak and is
    then neither heartbeated nor answered again."""

    async def run() -> None:
        consumer = OrderedConsumer((_delivery(1),), redeliver=True)
        stop = asyncio.Event()
        calls = 0

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            while not any(entry[0] == "+WPI" and entry[2] > 1 for entry in consumer.wire):
                await asyncio.sleep(0)
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        async def stop_after_answer() -> None:
            while not consumer.retried:
                await asyncio.sleep(0)
            await asyncio.sleep(0.02)
            stop.set()

        watcher = asyncio.create_task(stop_after_answer())
        try:
            await asyncio.wait_for(_runtime(consumer, process).run(stop), timeout=2.0)
        finally:
            watcher.cancel()

        assert calls == 1
        # The FIRST nak is the newest copy's; the processing future's own
        # (older) copy is never answered.
        nak = next(entry for entry in consumer.wire if entry[0] == "-NAK")
        assert nak[2] > 1, consumer.wire
        assert all(delivery.num_delivered > 1 for delivery in consumer.retried[:1])
        _no_wpi_after_nak(consumer.wire, 1)

    asyncio.run(run())


# ── D4: unstarted work goes back at shutdown ────────────────────────────────


def test_shutdown_releases_queued_unstarted_deliveries_without_delay() -> None:
    async def run() -> None:
        released: list[int] = []

        class ReleasingConsumer(FakeConsumer):
            async def release(self, delivery: CommandDelivery) -> None:
                released.append(delivery.stream_sequence)

        consumer = ReleasingConsumer((_delivery(1), _delivery(2), _delivery(3)), batch=3)
        stop = asyncio.Event()
        started: list[int] = []
        running = asyncio.Event()
        finish = asyncio.Event()

        async def process(delivery: CommandDelivery) -> DeliveryResult:
            started.append(delivery.stream_sequence)
            running.set()
            await finish.wait()
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        loop = _runtime(consumer, process, max_concurrency=1, queue_capacity=2)
        task = asyncio.create_task(loop.run(stop))
        await asyncio.wait_for(running.wait(), timeout=1.0)
        # 1 runs; 2 and 3 are fetched and owned but never started.
        while len(loop._owned) < 3:  # noqa: SLF001
            await asyncio.sleep(0)
        stop.set()
        while sorted(released) != [2, 3]:
            await asyncio.sleep(0)
        # The running one drains normally, and nothing else starts.
        finish.set()
        await asyncio.wait_for(task, timeout=1.0)

        assert started == [1]
        assert sorted(released) == [2, 3]
        assert consumer.retried == consumer.parked == []
        assert loop._owned == {}  # noqa: SLF001

    asyncio.run(run())


# ── D1: hold only what can start now, plus one small prefetch ───────────────


def test_a_replica_holds_at_most_its_free_workers_plus_one_fetch_batch() -> None:
    """KEDA counts num_ack_pending: a held-but-unstarted command is invisible
    capacity no other replica can take. With 4 workers, a 64-deep queue and a
    fetch batch of 2, the replica owns at most 4 + 2 = 6, never 4 + 64."""

    async def run() -> None:
        deliveries = tuple(_delivery(index) for index in range(1, 41))
        consumer = FakeConsumer(deliveries, batch=2)
        stop = asyncio.Event()
        release = asyncio.Event()
        running_now = 0
        peak_owned = 0

        loop = _runtime(consumer, None, max_concurrency=4, queue_capacity=64)

        async def process(_: CommandDelivery) -> DeliveryResult:
            nonlocal running_now
            running_now += 1
            await release.wait()
            running_now -= 1
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        loop._process = process  # noqa: SLF001
        task = asyncio.create_task(loop.run(stop))
        while running_now < 4:
            await asyncio.sleep(0)
        for _ in range(200):
            peak_owned = max(peak_owned, len(loop._owned))  # noqa: SLF001
            await asyncio.sleep(0)
        assert peak_owned == 6, peak_owned
        assert all(count <= 2 for count in consumer.fetch_counts)
        # Saturated: no pull is outstanding while every slot is held.
        fetches = len(consumer.fetch_counts)
        await asyncio.sleep(0.01)
        assert len(consumer.fetch_counts) == fetches
        stop.set()
        release.set()
        await asyncio.wait_for(task, timeout=1.0)

    asyncio.run(run())
