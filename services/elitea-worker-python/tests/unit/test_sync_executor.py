from __future__ import annotations

import asyncio
import contextvars
import threading

import pytest

from elitea_worker.execution.errors import InvalidInput
from elitea_worker.execution.supervisor import ExecutionSupervisor
from elitea_worker.execution.sync_executor import (
    SyncExecutorNotAccepting,
    SyncExecutorSaturated,
)


async def _wait_until(predicate, *, timeout_seconds: float = 1.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while not predicate():
        if asyncio.get_running_loop().time() >= deadline:
            raise AssertionError("condition was not reached before the test deadline")
        await asyncio.sleep(0.001)


def test_executor_bounds_running_plus_queued_before_submit() -> None:
    async def run() -> None:
        release = threading.Event()
        first_started = threading.Event()
        calls: list[str] = []

        def blocking(name: str) -> str:
            calls.append(name)
            if name == "first":
                first_started.set()
            assert release.wait(timeout=2)
            return name

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=2,
            admission_timeout_seconds=0.02,
            drain_timeout_seconds=2,
        )
        first = asyncio.create_task(supervisor.run_sync(blocking, "first"))
        await _wait_until(first_started.is_set)
        second = asyncio.create_task(supervisor.run_sync(blocking, "second"))
        await asyncio.sleep(0)

        with pytest.raises(SyncExecutorSaturated):
            await supervisor.run_sync(blocking, "third")
        assert calls == ["first"]

        release.set()
        assert await first == "first"
        assert await second == "second"
        await supervisor.shutdown()

    asyncio.run(run())


def test_queued_cancellation_cancels_only_its_future_and_releases_capacity() -> None:
    async def run() -> None:
        release = threading.Event()
        first_started = threading.Event()
        calls: list[str] = []

        def blocking(name: str) -> str:
            calls.append(name)
            if name == "first":
                first_started.set()
            assert release.wait(timeout=2)
            return name

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=2,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        first = asyncio.create_task(supervisor.run_sync(blocking, "first"))
        await _wait_until(first_started.is_set)
        queued = asyncio.create_task(supervisor.run_sync(blocking, "cancelled"))
        await asyncio.sleep(0)
        queued.cancel()
        with pytest.raises(asyncio.CancelledError):
            await queued

        replacement = asyncio.create_task(supervisor.run_sync(blocking, "replacement"))
        await asyncio.sleep(0)
        release.set()

        assert await first == "first"
        assert await replacement == "replacement"
        assert calls == ["first", "replacement"]
        await supervisor.shutdown()

    asyncio.run(run())


def test_waiter_cancelled_before_admission_is_never_submitted() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()
        forbidden_calls = 0

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        def forbidden() -> None:
            nonlocal forbidden_calls
            forbidden_calls += 1

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.2,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)
        waiting = asyncio.create_task(supervisor.run_sync(forbidden))
        await asyncio.sleep(0)
        waiting.cancel()
        with pytest.raises(asyncio.CancelledError):
            await waiting

        release.set()
        await running
        assert forbidden_calls == 0
        assert await supervisor.run_sync(lambda: "available") == "available"
        await supervisor.shutdown()

    asyncio.run(run())


def test_running_cancellation_waits_for_thread_exit_and_keeps_capacity() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.02,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)
        running.cancel()
        await asyncio.sleep(0)
        assert not running.done()

        with pytest.raises(SyncExecutorSaturated):
            await supervisor.run_sync(lambda: None)
        assert not running.done()

        release.set()
        with pytest.raises(asyncio.CancelledError):
            await running
        assert await supervisor.run_sync(lambda: "released") == "released"
        await supervisor.shutdown()

    asyncio.run(run())


def test_copied_context_and_dedicated_thread_are_used() -> None:
    async def run() -> None:
        marker: contextvars.ContextVar[str] = contextvars.ContextVar("marker")
        marker.set("execution-context")
        event_loop_thread = threading.current_thread().name
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )

        def observe_and_mutate_context():
            observed = marker.get()
            marker.set("thread-local-mutation")
            return observed, threading.current_thread().name

        first = await supervisor.run_sync(observe_and_mutate_context)
        marker.set("second-execution-context")
        second = await supervisor.run_sync(observe_and_mutate_context)

        assert first[0] == "execution-context"
        assert second[0] == "second-execution-context"
        assert first[1] == second[1]
        assert first[1] != event_loop_thread
        assert first[1].startswith("elitea-sdk-sync")
        await supervisor.shutdown()

    asyncio.run(run())


def test_operation_worker_error_propagates_unchanged() -> None:
    async def run() -> None:
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        expected = InvalidInput("sentinel-safe-message")

        def fail() -> None:
            raise expected

        with pytest.raises(InvalidInput) as caught:
            await supervisor.run_sync(fail)
        assert caught.value is expected
        assert await supervisor.run_sync(lambda: "still-available") == "still-available"
        await supervisor.shutdown()

    asyncio.run(run())


def test_waiting_admission_is_woken_when_stop_is_requested() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.03,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)
        waiting = asyncio.create_task(supervisor.run_sync(lambda: None))
        await asyncio.sleep(0)
        supervisor.stop_admission()

        with pytest.raises(SyncExecutorNotAccepting):
            await supervisor.run_sync(lambda: None)
        with pytest.raises(SyncExecutorNotAccepting):
            await waiting

        release.set()
        await running
        await supervisor.shutdown()

    asyncio.run(run())


def test_waiting_delivery_admission_is_woken_when_stop_is_requested() -> None:
    async def run() -> None:
        release = asyncio.Event()
        started = asyncio.Event()
        forbidden_calls = 0

        async def blocking() -> None:
            started.set()
            await release.wait()

        async def forbidden() -> None:
            nonlocal forbidden_calls
            forbidden_calls += 1

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.2,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run(blocking))
        await started.wait()
        waiting = asyncio.create_task(supervisor.run(forbidden))
        await asyncio.sleep(0)
        supervisor.stop_admission()

        with pytest.raises(SyncExecutorNotAccepting):
            await waiting
        assert forbidden_calls == 0

        release.set()
        await running
        await supervisor.shutdown()

    asyncio.run(run())


def test_cancelled_shutdown_waits_for_running_thread_and_is_idempotent() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)
        closing = asyncio.create_task(supervisor.shutdown())
        await asyncio.sleep(0)
        closing.cancel()
        await asyncio.sleep(0.01)

        assert not closing.done()
        assert not running.done()
        assert any(
            thread.name.startswith("elitea-sdk-sync")
            for thread in threading.enumerate()
        )

        release.set()
        await running
        with pytest.raises(asyncio.CancelledError):
            await closing

        await supervisor.shutdown()
        await _wait_until(
            lambda: not any(
                thread.name.startswith("elitea-sdk-sync")
                for thread in threading.enumerate()
            )
        )

    asyncio.run(run())


def test_shutdown_can_be_retried_after_drain_timeout() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)

        with pytest.raises(TimeoutError, match="did not drain"):
            await supervisor.shutdown(timeout_seconds=0.01)
        assert not running.done()

        release.set()
        await running
        await supervisor.shutdown(timeout_seconds=1)
        with pytest.raises(SyncExecutorNotAccepting):
            await supervisor.run_sync(lambda: None)

    asyncio.run(run())


def test_stop_admission_drain_and_shutdown_are_explicit() -> None:
    async def run() -> None:
        release = threading.Event()
        started = threading.Event()

        def blocking() -> None:
            started.set()
            assert release.wait(timeout=2)

        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.1,
            drain_timeout_seconds=2,
        )
        running = asyncio.create_task(supervisor.run_sync(blocking))
        await _wait_until(started.is_set)
        supervisor.stop_admission()

        with pytest.raises(SyncExecutorNotAccepting):
            await supervisor.run_sync(lambda: None)
        draining = asyncio.create_task(supervisor.drain())
        await asyncio.sleep(0)
        assert not draining.done()

        release.set()
        await running
        await draining
        await supervisor.shutdown()
        with pytest.raises(SyncExecutorNotAccepting):
            await supervisor.run_sync(lambda: None)

    asyncio.run(run())


def test_reserved_slot_fences_capacity_before_submit_and_is_consumed_once() -> None:
    async def run() -> None:
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.01,
            drain_timeout_seconds=1,
        )
        reservation = await supervisor.reserve_sync()
        submitted: list[str] = []

        with pytest.raises(SyncExecutorSaturated):
            await asyncio.create_task(supervisor.run_sync(lambda: submitted.append("unsafe")))
        assert submitted == []

        assert await supervisor.run_sync(lambda: "started") == "started"
        with pytest.raises(RuntimeError, match="not available"):
            await supervisor.run_sync(lambda: "double-submit")
        reservation.release()

        assert await asyncio.create_task(supervisor.run_sync(lambda: "next")) == "next"
        await supervisor.shutdown()

    asyncio.run(run())


def test_unused_reserved_slot_is_released_without_submit() -> None:
    async def run() -> None:
        supervisor = ExecutionSupervisor(
            max_workers=1,
            max_in_flight=1,
            admission_timeout_seconds=0.01,
            drain_timeout_seconds=1,
        )
        reservation = await supervisor.reserve_sync()
        reservation.release()
        reservation.release()
        assert await supervisor.run_sync(lambda: "available") == "available"
        await supervisor.shutdown()

    asyncio.run(run())


def test_sync_pool_sustains_the_configured_in_flight_bound() -> None:
    """The synchronous pool runs the full in-flight bound on distinct threads.

    Pair with `test_serve_loop_sustains_the_configured_delivery_cap`: the
    delivery cap and this pool cap the same executions in series, so raising
    one without the other bounds the other. All 32 slots must hold a blocked
    handler at once, or the 32nd delivery stalls in admission.
    """

    async def run() -> None:
        bound = 32
        release = threading.Event()
        lock = threading.Lock()
        in_flight = 0
        peak = 0

        def blocking() -> None:
            nonlocal in_flight, peak
            with lock:
                in_flight += 1
                peak = max(peak, in_flight)
            assert release.wait(timeout=2)
            with lock:
                in_flight -= 1

        supervisor = ExecutionSupervisor(
            max_workers=bound,
            max_in_flight=bound,
            admission_timeout_seconds=5,
            drain_timeout_seconds=5,
        )
        tasks = [
            asyncio.create_task(supervisor.run_sync(blocking)) for _ in range(bound)
        ]
        await _wait_until(lambda: peak >= bound, timeout_seconds=5)
        assert peak == bound

        release.set()
        await asyncio.gather(*tasks)
        await supervisor.shutdown()

    asyncio.run(run())
