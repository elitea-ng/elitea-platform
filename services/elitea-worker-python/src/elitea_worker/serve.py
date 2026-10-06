"""Production ``elitea-worker serve`` composition and bounded intake loop."""

from __future__ import annotations

import asyncio
import hashlib
import json
import signal
import sys
import time
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import httpx
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from elitea.runtime.v1 import command_pb2

from elitea_worker.agent_current_runtime_capabilities import (
    require_agent_current_runtime_capabilities,
)
from elitea_worker.agents.checkpoint import CurrentAgentCheckpointFactory
from elitea_worker.agents.client_context import ClaimBoundEliteaClientContextFactory
from elitea_worker.agents.sdk_adapter import EliteaSdkAdapter
from elitea_worker.config import (
    RuntimeDeployConfig,
    load_deploy_config,
    read_regular_file,
    validate_private_directory,
)
from elitea_worker.constants import (
    AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
    AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
    INDEX_INGEST_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_ID,
)
from elitea_worker.execution.delivery import (
    AgentExecutionDeliveryProcessor,
    ConfigurationValidationDeliveryProcessor,
    ControlPlane,
    DeliveryDisposition,
    DeliveryResult,
    IndexClientContextFactory,
    IndexIngestDeliveryProcessor,
    ToolkitCallToolDeliveryProcessor,
)
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DependencyUnavailable,
    InvalidInput,
    WorkerError,
)
from elitea_worker.execution.quarantine import FileQuarantineStore
from elitea_worker.execution.supervisor import ExecutionSupervisor
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.indexing_runtime_capabilities import (
    require_indexing_runtime_capabilities,
)
from elitea_worker.protocol.codec import (
    Ed25519CommandAuthenticator,
    parse_and_verify_signed_command,
)
from elitea_worker.security import RuntimeTrustMaterial
from elitea_worker.transport.input_content import (
    ClaimBoundInputRequestBuilder,
    ScopedInputContentClient,
)
from elitea_worker.transport.output_grpc import (
    GeneratedOutputStub,
    OutputGrpcSession,
)
from elitea_worker.transport.reconnect_channel import (
    ReconnectableControlPlane,
    ReconnectableOutputStub,
)
from elitea_worker.transport.output_spool import EncryptedOutputSpool
from elitea_worker.transport.nats_jetstream import (
    DEAD_LETTER_BUCKET,
    TERMINAL_POISON,
    CommandDelivery,
    CommandSignatureRejected,
    JetStreamCommandConsumer,
    NatsCommandBusConnection,
    bind_command_consumer,
    require_subject_names_command,
    runtime_api_prefix,
)
from elitea_worker.transport.runtime_context import ClaimBoundEliteaTokenClient


class DeliveryConsumer(Protocol):
    """The serve loop's whole view of the command bus.

    Intake, heartbeat and the three non-ack answers. The ack itself is not
    here: only the delivery processor may acknowledge, and only after the
    terminal PostgreSQL receipt.
    """

    @property
    def delivery_batch_size(self) -> int: ...

    async def fetch(
        self,
        *,
        count: int | None = None,
    ) -> tuple[CommandDelivery, ...]: ...

    async def in_progress(self, deliveries: Sequence[CommandDelivery]) -> int: ...

    async def retry_later(self, delivery: CommandDelivery) -> None: ...

    async def park(self, delivery: CommandDelivery) -> None: ...

    async def terminate(self, delivery: CommandDelivery) -> None: ...

    async def record_dead_letter(
        self, delivery: CommandDelivery, *, reason: str
    ) -> None: ...


#: Claim answers that leave the command for a later delivery. Each is answered
#: with NakWithDelay(retry delay), which frees the slot now instead of holding
#: it until AckWait.
_RETRY_LATER_DISPOSITIONS = frozenset(
    {
        DeliveryDisposition.OWNED_ELSEWHERE_NOACK,
        DeliveryDisposition.RECOVERY_REQUIRED_NOACK,
        DeliveryDisposition.RETRY_LATER_NOACK,
    }
)

#: The ERROR log line the dead-letter alert pairs with the bucket being
#: non-empty (docs/runtime-command-bus.md, "Dead letter").
DEAD_LETTERED_EVENT = "worker_command.dead_lettered"
#: The ERROR line for a poison whose dead-letter record could not be written.
#: The poison is then NOT parked: it is nak'd with the retry delay, so the
#: next delivery retries the record (without running the command again).
DEAD_LETTER_WRITE_FAILED_EVENT = "worker_command.dead_letter_write_failed"
_ERROR_EVENTS = frozenset({DEAD_LETTERED_EVENT, DEAD_LETTER_WRITE_FAILED_EVENT})


@dataclass(slots=True)
class _ExecutionLock:
    lock: asyncio.Lock
    users: int = 0


@dataclass(slots=True)
class _ShutdownBudget:
    """One process-local deadline shared by drain and dependency closure."""

    timeout_seconds: float
    deadline: float | None = None
    armed: asyncio.Event = field(default_factory=asyncio.Event)

    def arm(self) -> float:
        if self.deadline is None:
            self.deadline = asyncio.get_running_loop().time() + self.timeout_seconds
            self.armed.set()
        return self.deadline

    def remaining(self) -> float:
        deadline = self.deadline
        if deadline is None:
            deadline = self.arm()
        return max(0.0, deadline - asyncio.get_running_loop().time())


class QuarantineStore(Protocol):
    """The durable, process-local half of the quarantine.

    Narrow by design, like the command consumer beside it: record one key,
    read them all back, and nothing else. A worker that decided a delivery is
    hopeless is not the component that may decide it is repaired.
    """

    @property
    def cap(self) -> int: ...

    async def load(self) -> frozenset[str]: ...

    async def add(self, entry_id: str, *, reason_code: str) -> bool: ...


class WorkerServeLoop:
    """Bounded pull queue with no data-plane publication surface."""

    def __init__(
        self,
        *,
        consumer: DeliveryConsumer,
        process_delivery: Callable[[CommandDelivery], Awaitable[DeliveryResult]],
        max_concurrency: int,
        queue_capacity: int,
        in_progress_interval_millis: int,
        dependency_retry_millis: int,
        shutdown_timeout_millis: int,
        event_sink: Callable[[str, WorkerError | None], None] | None = None,
        quarantine_store: QuarantineStore | None = None,
    ) -> None:
        if (
            min(
                max_concurrency,
                queue_capacity,
                in_progress_interval_millis,
                dependency_retry_millis,
                shutdown_timeout_millis,
            )
            < 1
            or queue_capacity < max_concurrency
        ):
            raise ValueError("worker serve-loop bounds are invalid")
        self._consumer = consumer
        self._process = process_delivery
        self._max_concurrency = max_concurrency
        self._queue: asyncio.Queue[CommandDelivery | None] = asyncio.Queue(
            queue_capacity
        )
        self._in_progress_interval = in_progress_interval_millis / 1000
        self._dependency_retry = dependency_retry_millis / 1000
        self._shutdown_timeout = shutdown_timeout_millis / 1000
        # Every message this process owns, queued or active, keyed by its
        # stream sequence. The value is the NEWEST delivery of that sequence:
        # a redelivery that arrives while the first is still running is not
        # run twice, but its reply subject is the one the server now tracks,
        # so heartbeats go there.
        self._owned: dict[tuple[str, int], CommandDelivery] = {}
        # Dead-letter keys this process refuses to run again. A poison message
        # is naked with the contract's 24h delay, so the server keeps it away
        # for that long; this set (and the durable store behind it) makes the
        # one redelivery after the delay, before the stream's MaxAge removes
        # it, a park instead of a second run.
        #
        # Bounded like every other buffer here: an unbounded set would turn a
        # broken dependency into a memory leak. The cap is the same order as the
        # ownership window, so it can hold every delivery this worker could
        # have in flight, and one more round of them.
        self._quarantined: set[str] = set()
        self._quarantine_cap = 2 * (queue_capacity + max_concurrency)
        # Optional on purpose: without a store the decision lasts this
        # process, and the server-side 24h delay still holds across restarts.
        self._quarantine_store = quarantine_store
        # One token covers each fetched, queued, or actively processed message.
        # Reserved BEFORE the pull, so a fetch never asks for more than this
        # worker can hold (the semaphore-before-pull rule).
        self._ownership_slots: asyncio.Queue[None] = asyncio.Queue(
            queue_capacity + max_concurrency
        )
        for _ in range(queue_capacity + max_concurrency):
            self._ownership_slots.put_nowait(None)
        # Held by a heartbeat round for its whole send, and by every delayed
        # nak around "stop owning it, then nak". A +WPI that reaches the server
        # AFTER a -NAK with a delay resets that message's redelivery timer to
        # AckWait (nats-server consumer.go progressUpdate), so a 24h poison
        # delay, or the retry delay, would collapse into a 60s loop. Under this
        # lock no heartbeat round can still be carrying a message that has
        # been nak'd, and no later round sees it: it is no longer owned.
        self._answer_lock = asyncio.Lock()
        self._event_sink = event_sink or (lambda _event, _error: None)
        #: Poison deliveries this process dead-lettered (an in-process count;
        #: the worker has no metrics endpoint, the alert is the bucket).
        self.dead_lettered = 0
        #: Dead-letter records the bucket refused (in-process; carried on every
        #: DEAD_LETTER_WRITE_FAILED_EVENT line as the running total).
        self.dead_letter_write_failures = 0
        # Poison this process decided on but could not record yet: the next
        # delivery of the key retries the record instead of being parked.
        # Bounded like the quarantine it belongs to.
        self._unrecorded: dict[str, WorkerError] = {}

    async def run(self, stop: asyncio.Event) -> None:
        if stop.is_set():
            return
        await self._load_durable_quarantine()
        intake = asyncio.create_task(
            self._intake_loop(stop),
            name="elitea-command-intake",
        )
        heartbeat = asyncio.create_task(
            self._heartbeat_loop(),
            name="elitea-command-heartbeat",
        )
        workers = tuple(
            asyncio.create_task(self._worker(), name=f"elitea-delivery-{index}")
            for index in range(self._max_concurrency)
        )
        stop_waiter = asyncio.create_task(
            stop.wait(),
            name="elitea-worker-stop",
        )
        background = (intake, heartbeat, *workers)
        shutdown_complete = False
        try:
            done, _ = await asyncio.wait(
                (stop_waiter, *background),
                return_when=asyncio.FIRST_COMPLETED,
            )
            if stop_waiter not in done:
                failed = next(task for task in background if task in done)
                await _raise_unexpected_background_exit(failed)

            # Graceful shutdown: stop pulling, keep heartbeating what is owned
            # until it ends or the deadline passes. Nothing is acked or naked
            # on the way out; AckWait redelivers whatever is left.
            intake.cancel()
            await asyncio.gather(intake, return_exceptions=True)
            drain = asyncio.create_task(
                self._queue.join(),
                name="elitea-delivery-drain",
            )
            try:
                drained, _ = await asyncio.wait(
                    (drain, heartbeat, *workers),
                    timeout=self._shutdown_timeout,
                    return_when=asyncio.FIRST_COMPLETED,
                )
                if drain not in drained:
                    failed = next(
                        (task for task in (heartbeat, *workers) if task in drained),
                        None,
                    )
                    if failed is not None:
                        await _raise_unexpected_background_exit(failed)
                    raise DependencyUnavailable(
                        "The worker did not drain before its shutdown deadline."
                    )
                await drain
            except asyncio.CancelledError:
                _detach_cancelled_tasks((drain, *workers))
                raise
            finally:
                if not drain.done():
                    _detach_cancelled_tasks((drain,))
                heartbeat.cancel()
                await asyncio.gather(heartbeat, return_exceptions=True)
            for _ in workers:
                await self._queue.put(None)
            await asyncio.gather(*workers)
            shutdown_complete = True
        finally:
            stop_waiter.cancel()
            await asyncio.gather(stop_waiter, return_exceptions=True)
            if not shutdown_complete:
                _detach_cancelled_tasks(background)

    async def _intake_loop(self, stop: asyncio.Event) -> None:
        """Reserve capacity, then pull at most that many messages."""

        while not stop.is_set():
            reserved = 0
            try:
                reserved = await self._reserve_delivery_capacity()
                if stop.is_set():
                    break
                deliveries = await self._consumer.fetch(count=reserved)
                accepted_reservation = reserved
                reserved = 0
                await self._enqueue_reserved(
                    deliveries,
                    reserved=accepted_reservation,
                )
            except asyncio.CancelledError:
                raise
            except WorkerError as exc:
                self._event_sink("nats_fetch_rejected", exc)
                await _wait_or_stop(stop, self._dependency_retry)
            except Exception:
                self._event_sink("nats_fetch_unavailable", DependencyUnavailable())
                await _wait_or_stop(stop, self._dependency_retry)
            finally:
                self._release_delivery_capacity(reserved)

    async def _load_durable_quarantine(self) -> None:
        """Adopt this worker's earlier refusals before pulling any command.

        Ordering is the point: seeding AFTER intake starts would leave a window
        in which a delivery a previous process parked is executed once more.

        A failure to read is NOT fatal. Refusing to start would turn a
        degraded-but-working worker into an outage; the cost of continuing is
        that a parked delivery the server redelivers after its 24h delay runs
        once more and is dead-lettered again. Announced either way.
        """
        if self._quarantine_store is None:
            return
        try:
            recorded = await self._quarantine_store.load()
        except asyncio.CancelledError:
            raise
        except WorkerError as exc:
            self._event_sink("quarantine_load_rejected", exc)
            return
        except Exception:
            self._event_sink("quarantine_load_unavailable", DependencyUnavailable())
            return
        self._quarantined.update(recorded)
        if recorded:
            self._event_sink("quarantine_loaded", None)
        # A record that lost lines is still usable, but the deliveries it lost
        # each run once more before being parked again — so it is reported.
        if getattr(self._quarantine_store, "malformed_lines", 0):
            self._event_sink(
                "quarantine_record_damaged",
                InvalidInput("The quarantine record contains unreadable lines."),
            )

    async def _persist_quarantine(self, key: str, error: WorkerError) -> None:
        """Make the refusal outlive this process. Never fatal."""
        if self._quarantine_store is None:
            return
        try:
            stored = await self._quarantine_store.add(key, reason_code=error.code)
        except asyncio.CancelledError:
            raise
        except WorkerError as exc:
            self._event_sink("quarantine_write_rejected", exc)
            return
        except Exception:
            self._event_sink("quarantine_write_unavailable", DependencyUnavailable())
            return
        if not stored:
            self._event_sink("quarantine_store_full", error)

    async def _heartbeat_loop(self) -> None:
        while True:
            await asyncio.sleep(self._in_progress_interval)
            try:
                await self._heartbeat_owned()
            except asyncio.CancelledError:
                raise
            except WorkerError as exc:
                self._event_sink("nats_in_progress_rejected", exc)
            except Exception:
                self._event_sink("nats_in_progress_unavailable", DependencyUnavailable())

    async def _heartbeat_owned(self) -> None:
        async with self._answer_lock:
            owned = tuple(self._owned.values())
            if owned:
                await self._consumer.in_progress(owned)

    def _disown(self, key: tuple[str, int]) -> CommandDelivery | None:
        """Stop owning (and heartbeating) one message; free its slot.

        Returns the NEWEST delivery of it: when the server redelivered the
        message while it was still running here, the newer copy's reply
        subject is the one the server tracks, so that copy is the one an
        answer must go to — and, being no longer owned, it is never
        heartbeated or answered again.
        """

        delivery = self._owned.pop(key, None)
        if delivery is not None:
            self._release_delivery_capacity(1)
        return delivery

    async def _answer_owned(
        self,
        answer: Callable[[CommandDelivery], Awaitable[None]],
        delivery: CommandDelivery,
        event: str,
    ) -> None:
        """A delayed nak of an owned message, never crossed by a +WPI."""

        key = (delivery.stream, delivery.stream_sequence)
        async with self._answer_lock:
            current = self._disown(key) or delivery
            await self._answer(answer, current, event)

    async def _reserve_delivery_capacity(self) -> int:
        await self._ownership_slots.get()
        reserved = 1
        while reserved < self._consumer.delivery_batch_size:
            try:
                self._ownership_slots.get_nowait()
            except asyncio.QueueEmpty:
                break
            reserved += 1
        return reserved

    def _release_delivery_capacity(self, count: int) -> None:
        for _ in range(count):
            self._ownership_slots.put_nowait(None)

    async def _enqueue_reserved(
        self,
        deliveries: tuple[CommandDelivery, ...],
        *,
        reserved: int,
    ) -> None:
        """Register the complete fetched batch before a queue put can block."""

        if len(deliveries) > reserved:
            self._release_delivery_capacity(reserved)
            raise DependencyUnavailable(
                "JetStream returned more messages than the worker reserved."
            )

        accepted: list[CommandDelivery] = []
        parked: list[CommandDelivery] = []
        for delivery in deliveries:
            key = (delivery.stream, delivery.stream_sequence)
            if key in self._owned:
                # A redelivery of a message still running here (AckWait passed
                # before a heartbeat landed). Not run twice; the newest reply
                # subject takes over the heartbeat.
                self._owned[key] = delivery
                self._release_delivery_capacity(1)
                continue
            # A delivery this worker already dead-lettered is parked again
            # BEFORE it can reach a worker task. Re-running it would only
            # reproduce the same refusal. Silent on purpose: the decision was
            # announced once, by the worker that made it.
            if (
                delivery.rejection is None
                and delivery.dead_letter_key in self._quarantined
            ):
                parked.append(delivery)
                self._release_delivery_capacity(1)
                continue
            self._owned[key] = delivery
            accepted.append(delivery)
        self._release_delivery_capacity(reserved - len(deliveries))

        for delivery in parked:
            error = self._unrecorded.get(delivery.dead_letter_key)
            if error is not None:
                # Decided, never recorded: retry the record, not the command.
                await self._dispose_poison(delivery, error, owned=False)
            else:
                await self._answer(self._consumer.park, delivery, "nats_park")

        queued = 0
        try:
            for delivery in accepted:
                await self._queue.put(delivery)
                queued += 1
        except BaseException:
            for delivery in accepted[queued:]:
                key = (delivery.stream, delivery.stream_sequence)
                if key in self._owned:
                    del self._owned[key]
                    self._release_delivery_capacity(1)
            raise

    async def _answer(
        self,
        answer: Callable[[CommandDelivery], Awaitable[None]],
        delivery: CommandDelivery,
        event: str,
    ) -> None:
        """Send one nak; a failure is reported, and AckWait is the fallback."""
        try:
            await answer(delivery)
        except asyncio.CancelledError:
            raise
        except WorkerError as exc:
            self._event_sink(f"{event}_rejected", exc)
        except Exception:
            self._event_sink(f"{event}_unavailable", DependencyUnavailable())

    async def _dead_letter(self, delivery: CommandDelivery, error: WorkerError) -> None:
        """Poison: one dead-letter record, then NakWithDelay(24h), an ERROR line.

        Never Term: that frees the delivery subject and PostgreSQL re-offers
        the poison every 30 seconds. The message stays PENDING and the server
        keeps it away for the poison delay.

        The record comes FIRST. A poison parked without a record is invisible
        to the alert, so a failed write is answered with the ordinary retry
        delay instead (see :meth:`_dispose_poison`).
        """
        if delivery.is_settled:
            # The processor already acknowledged it after a terminal receipt;
            # a failure after that point is reported, never dead-lettered.
            return
        key = delivery.dead_letter_key
        if isinstance(error, TERMINAL_POISON):
            # Terminated, not parked: a re-offer of the same delivery fails the
            # same pre-claim check, so there is nothing to quarantine.
            await self._dispose_poison(delivery, error, owned=True)
            return
        if key not in self._quarantined:
            if len(self._quarantined) < self._quarantine_cap:
                self._quarantined.add(key)
            else:
                # Announced rather than silently widened: past the cap this
                # process forgets the decision; the server's delay still holds.
                self._event_sink("delivery_quarantine_full", error)
        await self._dispose_poison(delivery, error, owned=True)

    async def _dispose_poison(
        self,
        delivery: CommandDelivery,
        error: WorkerError,
        *,
        owned: bool,
    ) -> None:
        """Record, then park; or, when the record fails, retry later."""

        key = delivery.dead_letter_key
        try:
            await self._consumer.record_dead_letter(delivery, reason=error.code)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self.dead_letter_write_failures += 1
            terminal = isinstance(error, TERMINAL_POISON)
            if not terminal and (
                key in self._unrecorded or len(self._unrecorded) < self._quarantine_cap
            ):
                self._unrecorded[key] = error
            self._event_sink(
                DEAD_LETTER_WRITE_FAILED_EVENT,
                _dead_letter_write_failure_notice(
                    error,
                    delivery,
                    total=self.dead_letter_write_failures,
                    cause=exc,
                ),
            )
            await self._answer_poison(
                self._consumer.retry_later, delivery, "nats_nak", owned=owned
            )
            return
        self._unrecorded.pop(key, None)
        self.dead_lettered += 1
        self._event_sink(DEAD_LETTERED_EVENT, _dead_letter_notice(error, delivery))
        if isinstance(error, TERMINAL_POISON):
            await self._answer_poison(
                self._consumer.terminate, delivery, "nats_term", owned=owned
            )
            return
        await self._answer_poison(
            self._consumer.park, delivery, "nats_park", owned=owned
        )
        await self._persist_quarantine(key, error)

    async def _answer_poison(
        self,
        answer: Callable[[CommandDelivery], Awaitable[None]],
        delivery: CommandDelivery,
        event: str,
        *,
        owned: bool,
    ) -> None:
        if owned:
            await self._answer_owned(answer, delivery, event)
        else:
            await self._answer(answer, delivery, event)

    async def _worker(self) -> None:
        while True:
            delivery = await self._queue.get()
            if delivery is None:
                self._queue.task_done()
                return
            key = (delivery.stream, delivery.stream_sequence)
            try:
                if delivery.rejection is not None:
                    # The transport shape itself is poison; nothing to decode.
                    self._event_sink("delivery_rejected", delivery.rejection)
                    await self._dead_letter(delivery, delivery.rejection)
                    continue
                result = await self._process(delivery)
                self._event_sink(result.disposition.value, result.execution_error)
                if result.disposition in _RETRY_LATER_DISPOSITIONS:
                    await self._answer_owned(
                        self._consumer.retry_later, delivery, "nats_nak"
                    )
            except asyncio.CancelledError:
                raise
            except WorkerError as exc:
                self._event_sink("delivery_rejected", exc)
                if exc.retryable:
                    await self._answer_owned(
                        self._consumer.retry_later, delivery, "nats_nak"
                    )
                else:
                    # `retryable: false` means re-running cannot change the
                    # answer: a decode or signature failure, a subject that
                    # does not name the signed command, a command this worker
                    # cannot serve, or an output nothing can deliver. Measured
                    # on the earlier transport: one undeliverable output rejected every
                    # 15-45s for 13 minutes. Dead-letter it once instead.
                    await self._dead_letter(delivery, exc)
            except Exception as exc:
                _emit_unexpected_delivery_failure(exc)
                self._event_sink("delivery_unavailable", DependencyUnavailable())
                await self._answer_owned(
                    self._consumer.retry_later, delivery, "nats_nak"
                )
            finally:
                self._disown(key)
                self._queue.task_done()


def _dead_letter_notice(
    error: WorkerError,
    delivery: CommandDelivery,
) -> WorkerError:
    """The one-shot, operator-facing statement that a delivery was poison.

    It keeps the original `code` and `retryable` so the event still classifies
    the same way, and names where to look. The stream, sequence and key are
    infrastructure coordinates, not execution content; the delivery ID and the
    command itself are never printed.
    """
    if isinstance(error, TERMINAL_POISON):
        return WorkerError(
            code=error.code,
            safe_message=(
                f"{error.safe_message} "
                f"Message {delivery.entry_id} (delivery {delivery.num_delivered}) is "
                f"poison that can never verify: it is recorded in "
                f"{DEAD_LETTER_BUCKET} under {delivery.dead_letter_key} and "
                f"TERMINATED, freeing its stream capacity. A re-offer of the same "
                f"delivery fails the same check before any claim."
            ),
            exit_code=error.exit_code,
            retryable=error.retryable,
        )
    return WorkerError(
        code=error.code,
        safe_message=(
            f"{error.safe_message} "
            f"Message {delivery.entry_id} (delivery {delivery.num_delivered}) is "
            f"poison: it is left PENDING with a 24h redelivery delay and recorded "
            f"in {DEAD_LETTER_BUCKET} under {delivery.dead_letter_key}. Re-running "
            f"it cannot change the outcome; the execution's own output is lost. "
            f"Clearing it requires the server-side recovery named above."
        ),
        exit_code=error.exit_code,
        retryable=error.retryable,
    )


def _dead_letter_write_failure_notice(
    error: WorkerError,
    delivery: CommandDelivery,
    *,
    total: int,
    cause: BaseException,
) -> WorkerError:
    """The operator-facing statement that a poison could not be recorded."""

    reason = cause.code if isinstance(cause, WorkerError) else "DEPENDENCY_UNAVAILABLE"
    return WorkerError(
        code=error.code,
        safe_message=(
            f"Message {delivery.entry_id} (delivery {delivery.num_delivered}) is "
            f"poison, but its record under {delivery.dead_letter_key} could not be "
            f"written to {DEAD_LETTER_BUCKET} ({reason}). It is NOT parked: it is "
            f"nak'd with the retry delay so its next delivery retries the record, "
            f"without running the command again in this process. "
            f"dead_letter_write_failures_total={total}."
        ),
        exit_code=error.exit_code,
        retryable=error.retryable,
    )


def _emit_unexpected_delivery_failure(error: Exception) -> None:
    """Emit operator-useful code locations without exception text or locals."""

    def describe(value: BaseException) -> dict[str, object]:
        frames: list[dict[str, object]] = []
        traceback = value.__traceback__
        while traceback is not None:
            frame = traceback.tb_frame
            frames.append(
                {
                    "module": str(frame.f_globals.get("__name__", "")),
                    "function": frame.f_code.co_name,
                    "line": traceback.tb_lineno,
                }
            )
            traceback = traceback.tb_next
        value_type = type(value)
        return {
            "exception_module": value_type.__module__,
            "exception_name": value_type.__name__,
            "frames": frames[-8:],
        }

    causes: list[dict[str, object]] = []
    current = error.__cause__ or error.__context__
    while current is not None and len(causes) < 4:
        causes.append(describe(current))
        current = current.__cause__ or current.__context__
    error_type = type(error)
    print(
        json.dumps(
            {
                "event": "delivery_internal_failure",
                "causes": causes,
                "exception_module": error_type.__module__,
                "exception_name": error_type.__name__,
                "frames": describe(error)["frames"],
            },
            sort_keys=True,
            separators=(",", ":"),
        ),
        file=sys.stderr,
        flush=True,
    )


class ProductionDeliveryProcessor:
    """Build exactly one execution-bound encrypted output spool per delivery."""

    def __init__(
        self,
        *,
        config: RuntimeDeployConfig,
        trust: RuntimeTrustMaterial,
        supervisor: ExecutionSupervisor,
        handler: ConfigurationValidationHandler,
        control: ControlPlane,
        command_acker: JetStreamCommandConsumer,
        input_client: ScopedInputContentClient,
        output_stub: GeneratedOutputStub,
        index_client_context_factory: IndexClientContextFactory | None = None,
        agent_checkpoint_factory: CurrentAgentCheckpointFactory | None = None,
    ) -> None:
        self._config = config
        self._trust = trust
        self._supervisor = supervisor
        self._handler = handler
        self._control = control
        self._acker = command_acker
        self._input = input_client
        self._input_builder = ClaimBoundInputRequestBuilder(
            origin=config.content_origin
        )
        self._output_stub = output_stub
        self._index_client_context_factory = index_client_context_factory
        self._agent_checkpoint_factory = agent_checkpoint_factory
        self._authenticator = Ed25519CommandAuthenticator(trust.signing_keys)
        self._spool_root = validate_private_directory(
            config.spool_root,
            description="output spool root",
        )
        self._locks: dict[bytes, _ExecutionLock] = {}
        self._metadata = (
            ("x-elitea-workload-session", config.workload_session_id),
            ("x-elitea-producer-id", config.producer_id),
        )

    async def process(self, delivery: CommandDelivery) -> DeliveryResult:
        try:
            _, command = parse_and_verify_signed_command(
                delivery.signed_envelope,
                authenticator=self._authenticator,
            )
        except AuthorizationFailure as exc:
            # A digest or signature that does not verify can never verify:
            # recorded, then terminated (TERMINAL_POISON), not parked 24h.
            raise CommandSignatureRejected(exc.safe_message) from exc
        # After the signature, before the claim: the subject's hash token must
        # name this signed command's delivery, or the message is poison.
        require_subject_names_command(delivery, command.idempotency_key)
        binding = _execution_spool_binding(command, self._config.producer_id)
        lock_state = self._locks.get(binding)
        if lock_state is None:
            lock_state = _ExecutionLock(asyncio.Lock())
            self._locks[binding] = lock_state
        lock_state.users += 1
        try:
            async with lock_state.lock:
                return await self._process_bound(delivery, command, binding)
        finally:
            lock_state.users -= 1
            if lock_state.users == 0 and self._locks.get(binding) is lock_state:
                self._locks.pop(binding, None)

    async def _process_bound(
        self,
        delivery: CommandDelivery,
        command: command_pb2.WorkerCommandV1,
        binding: bytes,
    ) -> DeliveryResult:
        spool_path = _prepare_execution_spool(self._spool_root, binding)
        limits = self._config.limits
        spool_overhead = limits.output_max_queued_frames * 64
        spool_key = HKDF(
            algorithm=hashes.SHA256(),
            length=32,
            salt=None,
            info=b"elitea.runtime.output-spool-key.v1\x00" + binding,
        ).derive(self._trust.spool_master_key)

        def output_session() -> OutputGrpcSession:
            spool = EncryptedOutputSpool(
                spool_path,
                key=spool_key,
                stream_aad=b"elitea.runtime.output-spool-aad.v1\x00" + binding,
                max_frames=limits.output_max_queued_frames,
                max_bytes=limits.output_max_queued_bytes + spool_overhead,
                max_frame_bytes=limits.output_max_frame_bytes,
            )
            return OutputGrpcSession(
                self._output_stub,
                spool=spool,
                metadata=lambda: self._metadata,
                max_queued_frames=limits.output_max_queued_frames,
                max_queued_bytes=limits.output_max_queued_bytes,
                max_frame_bytes=limits.output_max_frame_bytes,
                stream_deadline_seconds=limits.output_stream_deadline_millis / 1000,
            )

        if command.capability_id == INDEX_INGEST_CAPABILITY_ID:
            processor = IndexIngestDeliveryProcessor(
                supervisor=self._supervisor,
                client_context_factory=self._index_client_context_factory,
                control=self._control,
                command_acker=self._acker,
                input_client=self._input,
                input_request_builder=self._input_builder,
                output_session_factory=output_session,
                signed_command_authenticator=self._authenticator,
                workload_session_id=self._config.workload_session_id,
                producer_id=self._config.producer_id,
                clock_unix_millis=lambda: int(time.time() * 1000),
                output_ack_timeout_seconds=limits.output_ack_timeout_millis / 1000,
                max_output_sessions=limits.output_max_sessions,
                lease_poll_interval_seconds=limits.lease_poll_interval_millis / 1000,
            )
        elif command.capability_id == TOOLKIT_CALL_TOOL_CAPABILITY_ID:
            processor = ToolkitCallToolDeliveryProcessor(
                supervisor=self._supervisor,
                client_context_factory=self._index_client_context_factory,
                control=self._control,
                command_acker=self._acker,
                input_client=self._input,
                input_request_builder=self._input_builder,
                output_session_factory=output_session,
                signed_command_authenticator=self._authenticator,
                workload_session_id=self._config.workload_session_id,
                producer_id=self._config.producer_id,
                clock_unix_millis=lambda: int(time.time() * 1000),
                output_ack_timeout_seconds=limits.output_ack_timeout_millis / 1000,
                max_output_sessions=limits.output_max_sessions,
                lease_poll_interval_seconds=limits.lease_poll_interval_millis / 1000,
            )
        elif command.capability_id in {
            AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
            AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
        }:
            checkpoint_factory = self._agent_checkpoint_factory
            if checkpoint_factory is None:
                raise DependencyUnavailable(
                    "The durable agent checkpoint store is unavailable."
                )
            processor = AgentExecutionDeliveryProcessor(
                supervisor=self._supervisor,
                client_context_factory=self._index_client_context_factory,
                checkpoint_factory=checkpoint_factory,
                control=self._control,
                command_acker=self._acker,
                input_client=self._input,
                input_request_builder=self._input_builder,
                output_session_factory=output_session,
                signed_command_authenticator=self._authenticator,
                workload_session_id=self._config.workload_session_id,
                producer_id=self._config.producer_id,
                clock_unix_millis=lambda: int(time.time() * 1000),
                output_ack_timeout_seconds=limits.output_ack_timeout_millis / 1000,
                max_output_sessions=limits.output_max_sessions,
                lease_poll_interval_seconds=limits.lease_poll_interval_millis / 1000,
            )
        else:
            processor = ConfigurationValidationDeliveryProcessor(
                supervisor=self._supervisor,
                handler=self._handler,
                control=self._control,
                command_acker=self._acker,
                input_client=self._input,
                input_request_builder=self._input_builder,
                output_session_factory=output_session,
                signed_command_authenticator=self._authenticator,
                workload_session_id=self._config.workload_session_id,
                producer_id=self._config.producer_id,
                clock_unix_millis=lambda: int(time.time() * 1000),
                output_ack_timeout_seconds=limits.output_ack_timeout_millis / 1000,
                max_output_sessions=limits.output_max_sessions,
                lease_poll_interval_seconds=limits.lease_poll_interval_millis / 1000,
            )
        result = await processor.process(delivery)
        if result.disposition not in {
            DeliveryDisposition.OWNED_ELSEWHERE_NOACK,
            DeliveryDisposition.RETRY_LATER_NOACK,
        }:
            _remove_empty_spool(spool_path)
        return result


async def serve_from_config(path: Path) -> None:
    """Load production config and run until SIGINT/SIGTERM requests drain."""

    stop = asyncio.Event()
    remove_signal_handlers = _install_signal_handlers(stop)
    try:
        config = load_deploy_config(path)
        # Fail before credentials are read or any control/data connection is
        # opened when the immutable image cannot execute its advertised
        # indexing profile. Signal ownership is established first because SDK
        # imports can be slow on a cold container.
        require_indexing_runtime_capabilities()
        require_agent_current_runtime_capabilities()
        # Let a signal queued while synchronous capability imports were running
        # set ``stop`` before deployment composition reads credentials.
        await asyncio.sleep(0)
        await serve_deployment(config, stop=stop)
    finally:
        remove_signal_handlers()


async def serve_deployment(
    config: RuntimeDeployConfig,
    *,
    stop: asyncio.Event | None = None,
) -> None:
    """Run one deployment and expose only stable failures to the CLI."""

    try:
        await _serve_deployment_inner(config, stop=stop)
    except WorkerError:
        raise
    except Exception as exc:
        # Startup and cleanup use the same safe public error boundary. Raw TLS,
        # filesystem, NATS and channel failures remain available only as the
        # chained cause for in-process diagnostics.
        raise DependencyUnavailable() from exc


async def _serve_deployment_inner(
    config: RuntimeDeployConfig,
    *,
    stop: asyncio.Event | None = None,
) -> None:
    own_stop = stop is None
    stop = stop or asyncio.Event()
    remove_signal_handlers = _install_signal_handlers(stop) if own_stop else lambda: None
    shutdown_budget = _ShutdownBudget(
        timeout_seconds=config.limits.shutdown_timeout_millis / 1000
    )
    if stop.is_set():
        shutdown_budget.arm()
    shutdown_observer = asyncio.create_task(
        _observe_shutdown(stop, shutdown_budget),
        name="elitea-shutdown-deadline",
    )
    nats: NatsCommandBusConnection | None = None
    control: ReconnectableControlPlane | None = None
    output: ReconnectableOutputStub | None = None
    http_client: httpx.AsyncClient | None = None
    supervisor: ExecutionSupervisor | None = None
    try:
        trust = RuntimeTrustMaterial.load(config)
        validate_private_directory(config.spool_root, description="output spool root")
        limits = config.limits
        # The command bus: the elitea-worker identity's mTLS connection, named
        # by consumer_id for observability. The certificate is the identity.
        nats = NatsCommandBusConnection(
            url=config.nats_url,
            name=config.consumer_id,
            tls=config.nats_tls,
            connect_timeout_seconds=limits.grpc_deadline_millis / 1000,
            event_sink=_emit_runtime_event,
        )
        if not await _wait_for_nats(nats, config, stop):
            return
        # Bind, never create: the stream, the durable and the dead-letter
        # bucket belong to the NATS bootstrap Job. Absence or drift refuses
        # to start.
        consumer = await bind_command_consumer(
            nats.client,
            stream=config.nats_stream,
            consumer=config.nats_consumer,
            worker_name=config.consumer_id,
            fetch_batch=limits.nats_fetch_batch,
            fetch_expires_millis=limits.nats_fetch_expires_millis,
            retry_delay_millis=limits.nats_retry_delay_millis,
            ack_timeout_seconds=limits.grpc_deadline_millis / 1000,
            max_message_bytes=limits.max_transport_message_bytes,
            max_payload_bytes=limits.max_transport_payload_bytes,
            # With an identity the worker is in the WORKER account and reaches
            # the RUNTIME durables through the imports mapped to
            # JS.RUNTIME.API; without one (compose) through $JS.API.
            api_prefix=runtime_api_prefix(config.nats_tls),
        )
        # The shared, alertable record is the dead-letter bucket. This file
        # keeps a parked delivery from running again after its 24h delay; see
        # execution/quarantine.py. v2: keyed by dead-letter key, not by the
        # old transport entry ID.
        quarantine_store = FileQuarantineStore(config.spool_root / "quarantine.v2")
        metadata = (
            ("x-elitea-workload-session", config.workload_session_id),
            ("x-elitea-producer-id", config.producer_id),
        )
        control = ReconnectableControlPlane(
            target=config.control_target,
            root_certificates=trust.ca_bytes,
            certificate_chain=trust.certificate_bytes,
            private_key=trust.private_key_bytes,
            metadata=lambda: metadata,
            deadline_seconds=limits.grpc_deadline_millis / 1000,
        )
        output = ReconnectableOutputStub(
            target=config.output_target,
            root_certificates=trust.ca_bytes,
            certificate_chain=trust.certificate_bytes,
            private_key=trust.private_key_bytes,
        )
        http_client = httpx.AsyncClient(
            verify=trust.http_client_context(),
            http1=False,
            http2=True,
            follow_redirects=False,
            max_redirects=0,
            trust_env=False,
            timeout=httpx.Timeout(limits.content_timeout_millis / 1000),
            limits=httpx.Limits(
                max_connections=limits.http_max_connections,
                max_keepalive_connections=limits.http_max_keepalive_connections,
            ),
        )
        content = ScopedInputContentClient(
            http_client,
            allowed_origins=frozenset({config.content_origin}),
            max_content_bytes=limits.content_max_body_bytes,
            timeout_seconds=limits.content_timeout_millis / 1000,
            require_http2=True,
        )
        index_client_context_factory = ClaimBoundEliteaClientContextFactory(
            base_url=config.platform_origin,
            token_fetcher=ClaimBoundEliteaTokenClient(
                http_client,
                origin=config.content_origin,
                timeout_seconds=limits.content_timeout_millis / 1000,
                require_http2=True,
            ),
        )
        supervisor = ExecutionSupervisor(
            max_workers=limits.sync_max_workers,
            max_in_flight=limits.sync_max_in_flight,
            max_deliveries=limits.delivery_max_concurrency,
            admission_timeout_seconds=limits.admission_timeout_millis / 1000,
            drain_timeout_seconds=limits.shutdown_timeout_millis / 1000,
        )
        await supervisor.__aenter__()
        agent_checkpoint_factory = None
        if config.agent_checkpoint_connection_path is not None:
            agent_checkpoint_factory = CurrentAgentCheckpointFactory(
                fallback_connection_string=read_regular_file(
                    config.agent_checkpoint_connection_path,
                    max_bytes=16 * 1024,
                    private=True,
                    description="agent checkpoint connection file",
                ).decode("utf-8"),
            )
        processor = ProductionDeliveryProcessor(
            config=config,
            trust=trust,
            supervisor=supervisor,
            handler=ConfigurationValidationHandler(EliteaSdkAdapter()),
            control=control,
            command_acker=consumer,
            input_client=content,
            output_stub=output,
            index_client_context_factory=index_client_context_factory,
            agent_checkpoint_factory=agent_checkpoint_factory,
        )
        runtime = WorkerServeLoop(
            consumer=consumer,
            process_delivery=processor.process,
            max_concurrency=limits.delivery_max_concurrency,
            queue_capacity=limits.delivery_queue_capacity,
            in_progress_interval_millis=limits.nats_in_progress_interval_millis,
            dependency_retry_millis=limits.dependency_retry_millis,
            shutdown_timeout_millis=limits.shutdown_timeout_millis,
            event_sink=_emit_runtime_event,
            quarantine_store=quarantine_store,
        )
        await _run_with_shutdown_budget(runtime, stop, shutdown_budget)
    finally:
        remove_signal_handlers()
        shutdown_observer.cancel()
        await asyncio.gather(shutdown_observer, return_exceptions=True)
        shutdown_budget.arm()
        await _close_runtime_resources(
            supervisor=supervisor,
            http_client=http_client,
            control=control,
            output=output,
            nats=nats,
            budget=shutdown_budget,
        )


async def _observe_shutdown(
    stop: asyncio.Event,
    budget: _ShutdownBudget,
) -> None:
    await stop.wait()
    budget.arm()


async def _run_with_shutdown_budget(
    runtime: WorkerServeLoop,
    stop: asyncio.Event,
    budget: _ShutdownBudget,
) -> None:
    """Give runtime drain no more than the process-wide shutdown budget."""

    runtime_task = asyncio.create_task(runtime.run(stop), name="elitea-worker-runtime")
    armed_task = asyncio.create_task(budget.armed.wait(), name="elitea-shutdown-armed")
    try:
        done, _ = await asyncio.wait(
            (runtime_task, armed_task),
            return_when=asyncio.FIRST_COMPLETED,
        )
        if runtime_task in done:
            await runtime_task
            return

        remaining = budget.remaining()
        if remaining > 0:
            done, _ = await asyncio.wait((runtime_task,), timeout=remaining)
            if runtime_task in done:
                await runtime_task
                return

        runtime_task.cancel()
        _detach_cancelled_tasks((runtime_task,))
        raise DependencyUnavailable(
            "The worker did not drain before its shutdown deadline."
        )
    except asyncio.CancelledError:
        _detach_cancelled_tasks((runtime_task,))
        raise
    finally:
        armed_task.cancel()
        await asyncio.gather(armed_task, return_exceptions=True)


async def _close_runtime_resources(
    *,
    supervisor: ExecutionSupervisor | None,
    http_client: httpx.AsyncClient | None,
    control: ReconnectableControlPlane | None,
    output: ReconnectableOutputStub | None,
    nats: NatsCommandBusConnection | None,
    budget: _ShutdownBudget,
) -> None:
    """Close every owned dependency concurrently within one remaining budget."""

    if supervisor is not None:
        supervisor.stop_admission()

    if all(
        resource is None
        for resource in (
            supervisor,
            http_client,
            control,
            output,
            nats,
        )
    ):
        return

    remaining = budget.remaining()
    if remaining <= 0:
        raise DependencyUnavailable(
            "The worker did not close before its shutdown deadline."
        )

    closers: list[Awaitable[object]] = []
    if supervisor is not None:
        closers.append(supervisor.shutdown(timeout_seconds=remaining))
    if http_client is not None:
        closers.append(http_client.aclose())
    if control is not None:
        closers.append(control.close(grace=remaining))
    if output is not None:
        closers.append(output.close(grace=remaining))
    if nats is not None:
        closers.append(nats.aclose())

    tasks = tuple(
        asyncio.create_task(closer, name=f"elitea-runtime-close-{index}")
        for index, closer in enumerate(closers)
    )
    try:
        done, pending = await asyncio.wait(tasks, timeout=budget.remaining())
    except BaseException:
        _detach_cancelled_tasks(tasks)
        raise
    if pending:
        _detach_cancelled_tasks(pending)

    failure: BaseException | None = None
    for task in done:
        try:
            task.result()
        except asyncio.CancelledError as exc:
            failure = failure or exc
        except BaseException as exc:
            failure = failure or exc

    if pending:
        raise DependencyUnavailable(
            "The worker did not close before its shutdown deadline."
        )
    if failure is not None:
        raise failure


def _detach_cancelled_tasks(
    tasks: tuple[asyncio.Task[object], ...] | set[asyncio.Task[object]],
) -> None:
    """Cancel deadline-expired tasks without extending the global deadline."""

    for task in tasks:
        task.cancel()
        task.add_done_callback(_consume_task_result)


def _consume_task_result(task: asyncio.Task[object]) -> None:
    try:
        task.exception()
    except asyncio.CancelledError:
        return


async def _raise_unexpected_background_exit(task: asyncio.Task[object]) -> None:
    """Turn any silent sibling exit into one stable process-level failure."""

    try:
        await task
    except asyncio.CancelledError as exc:
        raise DependencyUnavailable(
            "A worker runtime task was cancelled unexpectedly."
        ) from exc
    except Exception as exc:
        raise DependencyUnavailable(
            "A worker runtime task stopped unexpectedly."
        ) from exc
    raise DependencyUnavailable("A worker runtime task exited unexpectedly.")


async def _wait_for_nats(
    connection: NatsCommandBusConnection,
    config: RuntimeDeployConfig,
    stop: asyncio.Event,
) -> bool:
    """Connect, retrying until connected or asked to stop.

    nats-py itself keeps retrying an unreachable server (reconnect attempts
    are unbounded) and reports each failure through the connection's error
    callback; this loop covers a connect that gives up anyway.
    """

    retry = config.limits.dependency_retry_millis / 1000
    while not stop.is_set():
        try:
            connected = await _connect_or_stop(connection, stop)
            if connected is None:
                return False
            return True
        except asyncio.CancelledError:
            raise
        except Exception:
            _emit_runtime_event("nats_startup_unavailable", DependencyUnavailable())
            await _wait_or_stop(stop, retry)
    return False


async def _connect_or_stop(
    connection: NatsCommandBusConnection,
    stop: asyncio.Event,
) -> bool | None:
    """Do not let a connecting NATS socket delay a requested shutdown."""

    connect_task = asyncio.create_task(
        connection.connect(), name="elitea-nats-startup-connect"
    )
    stop_task = asyncio.create_task(stop.wait(), name="elitea-nats-startup-stop")
    try:
        done, _ = await asyncio.wait(
            (connect_task, stop_task),
            return_when=asyncio.FIRST_COMPLETED,
        )
        if stop_task in done:
            _detach_cancelled_tasks((connect_task,))
            return None
        await connect_task
        return True
    except asyncio.CancelledError:
        _detach_cancelled_tasks((connect_task,))
        raise
    finally:
        stop_task.cancel()
        await asyncio.gather(stop_task, return_exceptions=True)


def _execution_spool_binding(
    command: command_pb2.WorkerCommandV1,
    producer_id: str,
) -> bytes:
    values = (
        command.tenant_id,
        command.resource_project_id,
        command.projection_project_id,
        command.command_id,
        command.execution_id,
        str(command.generation),
        producer_id,
    )
    if any(not value for value in values):
        raise ValueError("execution spool identity is incomplete")
    encoded = tuple(value.encode("utf-8") for value in values)
    return b"elitea.runtime.execution-spool.v1\x00" + b"\x00".join(
        len(value).to_bytes(4, "big") + value for value in encoded
    )


def _prepare_execution_spool(root: Path, binding: bytes) -> Path:
    path = root / hashlib.sha256(binding).hexdigest()
    try:
        path.mkdir(mode=0o700, exist_ok=True)
    except OSError as exc:
        raise DependencyUnavailable(
            "The execution output spool is unavailable."
        ) from exc
    # ``exist_ok`` follows an existing directory symlink. Revalidate the exact
    # derived child before encrypted spool code can open anything beneath it.
    return validate_private_directory(path, description="execution output spool")


def _remove_empty_spool(path: Path) -> None:
    try:
        if path.is_dir() and not any(path.iterdir()):
            path.rmdir()
    except OSError:
        # Empty-directory cleanup is hygiene only; durable output was already
        # removed after its ACK and settlement. Never roll back command ACK.
        return


async def _wait_or_stop(stop: asyncio.Event, seconds: float) -> None:
    try:
        await asyncio.wait_for(stop.wait(), timeout=seconds)
    except TimeoutError:
        return


def _install_signal_handlers(stop: asyncio.Event) -> Callable[[], None]:
    loop = asyncio.get_running_loop()
    installed: list[signal.Signals] = []
    for event in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(event, stop.set)
            installed.append(event)
        except (NotImplementedError, RuntimeError):
            continue

    def remove() -> None:
        for event in installed:
            loop.remove_signal_handler(event)

    return remove


def _emit_runtime_event(event: str, error: WorkerError | None) -> None:
    diagnostic: dict[str, object] = {"event": event}
    if event in _ERROR_EVENTS:
        diagnostic["level"] = "error"
    if error is not None:
        diagnostic.update(
            code=error.code,
            retryable=error.retryable,
            safe_message=error.safe_message,
        )
    print(
        json.dumps(diagnostic, sort_keys=True, separators=(",", ":")),
        file=sys.stderr,
        flush=True,
    )


__all__ = [
    "ProductionDeliveryProcessor",
    "WorkerServeLoop",
    "serve_deployment",
    "serve_from_config",
]
