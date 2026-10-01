from __future__ import annotations

import asyncio
import hashlib
import json
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
)
from elitea_worker.serve import (
    WorkerServeLoop,
    _ShutdownBudget,
    _close_runtime_resources,
    _prepare_execution_spool,
    _wait_for_redis,
)
from elitea_worker.transport.redis_commands import RedisCommandDelivery


class FakeConsumer:
    def __init__(self, deliveries: tuple[RedisCommandDelivery, ...]) -> None:
        self._deliveries = deliveries
        self._read_offset = 0
        self.read_counts: list[int] = []
        self.read_blocks: list[int] = []
        self.reclaim_counts: list[int] = []
        self.reclaim_calls = 0
        self.heartbeat_calls: list[tuple[str, ...]] = []

    @property
    def delivery_batch_size(self) -> int:
        return 2

    @property
    def heartbeat_batch_size(self) -> int:
        return 2

    async def read(
        self,
        *,
        count: int | None = None,
        block_ms: int | None = None,
    ) -> tuple[RedisCommandDelivery, ...]:
        assert count is not None
        assert block_ms is not None
        self.read_counts.append(count)
        self.read_blocks.append(block_ms)
        if self._read_offset < len(self._deliveries):
            end = self._read_offset + count
            deliveries = self._deliveries[self._read_offset : end]
            self._read_offset = end
            return deliveries
        await asyncio.sleep(block_ms / 1_000)
        return ()

    async def reclaim_page(
        self,
        *,
        min_idle_ms: int,
        start_id: str = "0-0",
        count: int | None = None,
    ):
        assert count is not None
        self.reclaim_counts.append(count)
        self.reclaim_calls += 1
        return "0-0", self._deliveries[:count][:1]

    async def heartbeat_pending(
        self,
        entry_ids: tuple[str, ...],
    ) -> tuple[str, ...]:
        self.heartbeat_calls.append(entry_ids)
        return entry_ids


def test_serve_loop_bounds_workers_and_drains() -> None:
    async def run() -> None:
        deliveries = tuple(
            RedisCommandDelivery(
                "commands.v1",
                f"{index}-0",
                {"signed_envelope": b"reference"},
            )
            for index in range(1, 4)
        )
        consumer = FakeConsumer(deliveries)
        stop = asyncio.Event()
        active = 0
        peak = 0
        processed: list[str] = []

        async def process(delivery: RedisCommandDelivery) -> DeliveryResult:
            nonlocal active, peak
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(0.01)
            processed.append(delivery.entry_id)
            active -= 1
            if len(processed) == len(deliveries):
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=2,
            queue_capacity=2,
            reclaim_idle_millis=1,
            reclaim_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

        assert sorted(processed) == ["1-0", "2-0", "3-0"]
        assert peak == 2

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
        deliveries = tuple(
            RedisCommandDelivery(
                "commands.v1",
                f"{index}-0",
                {"signed_envelope": b"reference"},
            )
            for index in range(1, total + 1)
        )

        class NoReclaim(FakeConsumer):
            async def reclaim_page(
                self,
                *,
                min_idle_ms: int,
                start_id: str = "0-0",
                count: int | None = None,
            ):
                del min_idle_ms, count
                self.reclaim_calls += 1
                return start_id, ()

        consumer = NoReclaim(deliveries)
        stop = asyncio.Event()
        active = 0
        peak = 0
        processed: list[str] = []

        async def process(delivery: RedisCommandDelivery) -> DeliveryResult:
            nonlocal active, peak
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(flow_seconds)
            processed.append(delivery.entry_id)
            active -= 1
            if len(processed) == total:
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        started = asyncio.get_running_loop().time()
        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=cap,
            queue_capacity=2 * cap,
            reclaim_idle_millis=60_000,
            reclaim_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=5_000,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=2.0)
        elapsed = asyncio.get_running_loop().time() - started

        assert len(processed) == total
        assert set(processed) == {f"{index}-0" for index in range(1, total + 1)}
        assert peak == cap
        assert elapsed <= 1.2 * (waves * flow_seconds) + 0.5

    asyncio.run(run())


def test_serve_loop_reports_settled_execution_error_to_event_sink() -> None:
    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()
        events: list[tuple[str, object]] = []
        failure = AuthorizationFailure("The scoped content grant was rejected.")

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            stop.set()
            return DeliveryResult(
                DeliveryDisposition.EXECUTED_SETTLED_ACKED,
                execution_error=failure,
            )

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1_000,
            reclaim_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1_000,
            event_sink=lambda event, error: events.append((event, error)),
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

        assert events == [("executed_settled_acked", failure)]

    asyncio.run(run())


def test_serve_loop_reports_redacted_code_location_for_unexpected_failure(
    capsys: pytest.CaptureFixture[str],
) -> None:
    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            stop.set()
            raise ValueError("credential-shaped diagnostic must not be logged")

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1_000,
            reclaim_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1_000,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

    asyncio.run(run())
    diagnostic = json.loads(capsys.readouterr().err)
    assert diagnostic["event"] == "delivery_internal_failure"
    assert diagnostic["exception_name"] == "ValueError"
    assert diagnostic["frames"][-1]["function"] == "process"
    assert diagnostic["causes"] == []
    assert "credential-shaped" not in json.dumps(diagnostic)


def test_serve_loop_does_not_run_reclaimed_duplicate_concurrently() -> None:
    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()
        calls = 0

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            while consumer.reclaim_calls == 0 or not consumer.heartbeat_calls:
                await asyncio.sleep(0)
            stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        await asyncio.wait_for(runtime.run(stop), timeout=0.2)

        assert calls == 1
        assert consumer.reclaim_calls >= 1
        assert max(consumer.read_blocks) <= 1
        assert consumer.heartbeat_calls[0] == ("1-0",)

    asyncio.run(run())


def test_serve_loop_quarantines_a_non_retryable_delivery_instead_of_respinning() -> None:
    """A delivery that cannot succeed is executed ONCE, however often it returns.

    The entry is never ACKed on the WorkerError path — correctly, since the
    output it owes was never delivered — so XAUTOCLAIM keeps handing it back.
    `retryable` was printed and ignored, so the same doomed command ran again on
    every reclaim turn. Measured on the standalone stack: one undeliverable
    output rejected every 15-45s for 13 minutes.

    FakeConsumer.reclaim_page returns the same entry on every call, so
    `reclaim_calls >= 4` proves the entry really was offered repeatedly. Pairing
    that with `calls == 1` is what discriminates the fix from the defect: before
    it, the two numbers rose together.
    """

    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, object]] = []

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise AuthorizationFailure(
                "The durable output spool uses a different claim fence; "
                "server-side recovery is required."
            )

        async def stop_after_repeated_offers() -> None:
            while consumer.reclaim_calls < 4:
                await asyncio.sleep(0)
            stop.set()

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
            event_sink=lambda event, error: events.append((event, error)),
        )
        watcher = asyncio.create_task(stop_after_repeated_offers())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert consumer.reclaim_calls >= 4, "the entry was not offered repeatedly"
        assert calls == 1, f"the doomed delivery ran {calls} times, not once"

        quarantined = [event for event in events if event[0] == "delivery_quarantined"]
        assert len(quarantined) == 1, "the quarantine must be announced exactly once"

        # Actionable, not just distinct: the operator gets the entry and the
        # remedy, because the underlying refusal names neither.
        notice = quarantined[0][1]
        assert notice is not None
        assert notice.code == "AUTHORIZATION_FAILED"
        assert "1-0" in notice.safe_message
        assert "commands.v1" in notice.safe_message
        assert "PENDING" in notice.safe_message

        assert not any(event[0] == "delivery_quarantine_full" for event in events)
        # The original rejection is still reported; quarantine adds to it.
        assert any(event[0] == "delivery_rejected" for event in events)

    asyncio.run(run())


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
    """A restart must not re-run what a previous process already parked.

    This is the half that in-memory quarantine cannot do. Before the durable
    record, every worker start re-ran each parked command exactly once and
    parked it again — the spin, once per process rather than once per reclaim.

    `calls == 0` is the assertion that matters: the entry is offered (read AND
    reclaim both return it) and never executed.
    """

    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        store = FakeQuarantineStore(frozenset({"1-0"}))
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, object]] = []

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            return DeliveryResult(DeliveryDisposition.EXECUTED_SETTLED_ACKED)

        async def stop_after_repeated_offers() -> None:
            while consumer.reclaim_calls < 3:
                await asyncio.sleep(0)
            stop.set()

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
            event_sink=lambda event, error: events.append((event, error)),
            quarantine_store=store,
        )
        watcher = asyncio.create_task(stop_after_repeated_offers())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert store.load_calls == 1, "the durable record is read exactly once"
        assert consumer.reclaim_calls >= 3, "the entry was not offered repeatedly"
        assert calls == 0, f"a parked delivery ran {calls} times"
        assert any(event[0] == "quarantine_loaded" for event in events)

    asyncio.run(run())


def test_serve_loop_records_a_quarantine_durably() -> None:
    """The refusal is written through, with the code that caused it."""

    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "7-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        store = FakeQuarantineStore()
        stop = asyncio.Event()
        events: list[tuple[str, object]] = []

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            raise AuthorizationFailure(
                "The durable output spool uses a different claim fence; "
                "server-side recovery is required."
            )

        async def stop_after_write() -> None:
            while not store.added:
                await asyncio.sleep(0)
            stop.set()

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
            event_sink=lambda event, error: events.append((event, error)),
            quarantine_store=store,
        )
        watcher = asyncio.create_task(stop_after_write())
        try:
            await asyncio.wait_for(runtime.run(stop), timeout=1.0)
        finally:
            watcher.cancel()

        assert store.added == [("7-0", "AUTHORIZATION_FAILED")]
        assert "7-0" in store.recorded
        assert not any(event[0] == "quarantine_store_full" for event in events)

    asyncio.run(run())


def test_serve_loop_still_quarantines_when_the_durable_load_fails() -> None:
    """A store outage must degrade, not resume the spin or refuse to start.

    Announced and then ignored: the process-local quarantine still holds, so the
    doomed delivery runs ONCE rather than on every reclaim turn.
    """

    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "9-0",
            {"signed_envelope": b"reference"},
        )
        consumer = FakeConsumer((delivery,))
        store = FakeQuarantineStore()
        store.load_error = RuntimeError("redis is gone")
        stop = asyncio.Event()
        calls = 0
        events: list[tuple[str, object]] = []

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            nonlocal calls
            calls += 1
            raise AuthorizationFailure("fence moved")

        async def stop_after_repeated_offers() -> None:
            while consumer.reclaim_calls < 4:
                await asyncio.sleep(0)
            stop.set()

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
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


def test_serve_loop_heartbeats_queued_and_active_entries_in_bounded_batches() -> None:
    async def run() -> None:
        deliveries = tuple(
            RedisCommandDelivery(
                "commands.v1",
                f"{index}-0",
                {"signed_envelope": b"reference"},
            )
            for index in range(1, 4)
        )
        consumer = FakeConsumer(deliveries)
        stop = asyncio.Event()

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            while len(consumer.heartbeat_calls) < 2:
                await asyncio.sleep(0)
            stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=3,
            reclaim_idle_millis=100,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        await runtime.run(stop)

        assert consumer.heartbeat_calls[:2] == [("1-0", "2-0"), ("3-0",)]

    asyncio.run(run())


def test_serve_loop_limits_fetch_to_unowned_delivery_capacity() -> None:
    async def run() -> None:
        deliveries = tuple(
            RedisCommandDelivery(
                "commands.v1",
                f"{index}-0",
                {"signed_envelope": b"reference"},
            )
            for index in range(1, 4)
        )

        class ThreeEntryBatch(FakeConsumer):
            @property
            def delivery_batch_size(self) -> int:
                return 3

            async def reclaim_page(
                self,
                *,
                min_idle_ms: int,
                start_id: str = "0-0",
                count: int | None = None,
            ):
                del min_idle_ms, count
                return start_id, ()

        consumer = ThreeEntryBatch(deliveries)
        stop = asyncio.Event()
        first_started = asyncio.Event()
        release_first = asyncio.Event()
        processed: list[str] = []

        async def process(delivery: RedisCommandDelivery) -> DeliveryResult:
            processed.append(delivery.entry_id)
            if delivery.entry_id == "1-0":
                first_started.set()
                await release_first.wait()
            if len(processed) == len(deliveries):
                stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=100,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        running = asyncio.create_task(runtime.run(stop))
        await asyncio.wait_for(first_started.wait(), timeout=0.2)

        while not consumer.heartbeat_calls:
            await asyncio.sleep(0)
        assert consumer.read_counts == [2]
        assert consumer.heartbeat_calls[0] == ("1-0", "2-0")

        release_first.set()
        await asyncio.wait_for(running, timeout=0.5)
        assert processed == ["1-0", "2-0", "3-0"]

    asyncio.run(run())


def test_serve_loop_keeps_heartbeat_alive_while_shutdown_drains() -> None:
    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )
        stop = asyncio.Event()
        heartbeat_after_stop = asyncio.Event()

        class ShutdownAwareConsumer(FakeConsumer):
            async def heartbeat_pending(
                self,
                entry_ids: tuple[str, ...],
            ) -> tuple[str, ...]:
                refreshed = await super().heartbeat_pending(entry_ids)
                if stop.is_set():
                    heartbeat_after_stop.set()
                return refreshed

        consumer = ShutdownAwareConsumer((delivery,))
        processing = asyncio.Event()
        release = asyncio.Event()

        async def process(_: RedisCommandDelivery) -> DeliveryResult:
            processing.set()
            await release.wait()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=100,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        running = asyncio.create_task(runtime.run(stop))
        await asyncio.wait_for(processing.wait(), timeout=0.2)

        stop.set()
        await asyncio.wait_for(heartbeat_after_stop.wait(), timeout=0.2)
        assert not running.done()

        release.set()
        await asyncio.wait_for(running, timeout=0.2)

    asyncio.run(run())


def test_serve_loop_reclaims_pending_delivery_without_new_read() -> None:
    async def run() -> None:
        delivery = RedisCommandDelivery(
            "commands.v1",
            "1-0",
            {"signed_envelope": b"reference"},
        )

        class ReclaimOnly(FakeConsumer):
            async def read(
                self,
                *,
                count: int | None = None,
                block_ms: int | None = None,
            ):
                del count
                assert block_ms is not None
                await asyncio.sleep(block_ms / 1_000)
                return ()

        consumer = ReclaimOnly((delivery,))
        stop = asyncio.Event()
        processed: list[str] = []

        async def process(item: RedisCommandDelivery) -> DeliveryResult:
            processed.append(item.entry_id)
            stop.set()
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=process,
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=1,
            reclaim_interval_millis=1,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )
        await runtime.run(stop)

        assert processed == ["1-0"]
        assert consumer.reclaim_calls >= 1

    asyncio.run(run())


def test_serve_loop_fails_when_background_task_exits_unexpectedly() -> None:
    class ExitingHeartbeatRuntime(WorkerServeLoop):
        async def _heartbeat_loop(self) -> None:
            return

    async def run() -> None:
        consumer = FakeConsumer(())
        runtime = ExitingHeartbeatRuntime(
            consumer=consumer,
            process_delivery=lambda _delivery: asyncio.sleep(0),
            max_concurrency=1,
            queue_capacity=1,
            reclaim_idle_millis=100,
            reclaim_interval_millis=100,
            dependency_retry_millis=1,
            shutdown_timeout_millis=1000,
        )

        with pytest.raises(DependencyUnavailable, match="exited unexpectedly"):
            await asyncio.wait_for(runtime.run(asyncio.Event()), timeout=0.2)

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


class _HungRedisClient(_HungCloser):
    async def aclose(self) -> None:
        await self.hang()


def test_runtime_dependency_closes_share_one_global_deadline() -> None:
    async def run() -> None:
        supervisor = _HungSupervisor()
        http_client = _HungHttpClient()
        control_channel = _HungChannel()
        output_channel = _HungChannel()
        redis_client = _HungRedisClient()
        closers = (
            supervisor,
            http_client,
            control_channel,
            output_channel,
            redis_client,
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
                redis_client=redis_client,  # type: ignore[arg-type]
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


def test_shutdown_interrupts_hung_redis_startup_ping() -> None:
    class HungRedis:
        def __init__(self) -> None:
            self.started = asyncio.Event()
            self.cancelled = asyncio.Event()

        async def ping(self) -> bool:
            self.started.set()
            try:
                await asyncio.Event().wait()
            finally:
                self.cancelled.set()
            return True

    async def run() -> None:
        redis = HungRedis()
        stop = asyncio.Event()
        config = SimpleNamespace(
            limits=SimpleNamespace(dependency_retry_millis=1000)
        )
        waiting = asyncio.create_task(
            _wait_for_redis(redis, config, stop)  # type: ignore[arg-type]
        )
        await redis.started.wait()
        stop.set()

        assert await asyncio.wait_for(waiting, timeout=0.2) is False
        await asyncio.sleep(0)
        assert redis.cancelled.is_set()

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
