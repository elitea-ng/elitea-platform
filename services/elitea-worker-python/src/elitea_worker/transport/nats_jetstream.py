"""The runtime command bus: a bounded NATS JetStream pull consumer.

``docs/runtime-command-bus.md`` is the normative contract this module encodes;
``services/elitea-main/internal/transport/commandbus/contract.go`` is the
producer's copy of the same names, and a unit test pins the two together.

This is intentionally the only worker module that knows the NATS client API.
It has no publish, result, output or callback method: the worker binds to the
durable pull consumer the bootstrap Job created, pulls, sends ``+WPI``
heartbeats for what it owns, acknowledges only after a terminal PostgreSQL
receipt, naks with a delay, and records a poison command in the dead-letter
bucket. It never creates, edits or deletes a stream or a consumer. It
terminates (``Term``) exactly two kinds of poison, after recording them: a
command whose signed envelope fails verification and one whose subject does
not name the signed command. Neither can ever become valid, and ``Term``
frees the stream capacity a 24h nak would hold; a PostgreSQL re-offer of the
same delivery fails the same check before any claim and is terminated again.
Every other poison is nak'd for 24h and stays PENDING on its subject.

Only the signed envelope in the message body is authority. The subject's hash
token and the headers are compared with the verified command, never trusted
instead of it.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import re
import ssl
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol
from urllib.parse import urlsplit

from elitea_worker import (
    _PRIVATE_PLANE_SSL_CONTEXT,
    _PRIVATE_PLANE_SSL_CONTEXT_BASE,
)
from elitea_worker.execution.errors import (
    DependencyUnavailable,
    InvalidInput,
    WorkerError,
)


# ── The contract (docs/runtime-command-bus.md, contract.go) ─────────────────

SUBJECT_ROOT = "elitea.rt.v1"
STREAM_PREFIX = "ELITEA_RT_V1_"

STREAM_VALIDATE = "ELITEA_RT_V1_VALIDATE"
STREAM_AGENT = "ELITEA_RT_V1_AGENT"
STREAM_INDEX = "ELITEA_RT_V1_INDEX"

CONSUMER_VALIDATE = "elitea-configuration-worker-v1"
CONSUMER_AGENT = "elitea-agent-worker-v1"
CONSUMER_INDEX = "elitea-index-worker-v1"

#: Each stream and the one durable pull consumer the bootstrap creates on it.
KNOWN_STREAMS: dict[str, str] = {
    STREAM_VALIDATE: CONSUMER_VALIDATE,
    STREAM_AGENT: CONSUMER_AGENT,
    STREAM_INDEX: CONSUMER_INDEX,
}

HEADER_MSG_ID = "Nats-Msg-Id"
HEADER_DELIVERY_ID = "Elitea-Delivery-Id"

DEAD_LETTER_BUCKET = "ELITEA_RT_V1_DEADLETTER"
# The dead-letter reason of a message of this durable whose metadata cannot be read.
_UNDECODABLE_REASON = "METADATA_UNREADABLE"
DEAD_LETTER_SCHEMA = "elitea.runtime.dead-letter.v1"

#: The only inbox prefix the permission table grants the elitea-worker user.
INBOX_PREFIX = "_INBOX_elitea-worker"

#: The JetStream API prefix of the worker's OWN account. With a client
#: identity that account is WORKER, which holds the dead-letter bucket and
#: nothing else; in compose's plaintext posture it is the one global account.
DEFAULT_API_PREFIX = "$JS.API"
#: The prefix under which the WORKER account imports the RUNTIME account's
#: consumer API (CONSUMER.INFO and CONSUMER.MSG.NEXT of the three durables).
#: The worker is NOT a RUNTIME user: the server publishes a JetStream API
#: answer on the requester's reply subject without checking it against the
#: requester's permissions, so a RUNTIME user allowed to pull could name a
#: command subject as the reply and have the server store the answer in a
#: command stream. Across the service import the answer lands in WORKER.
#: deploy/helm/nats/values.yaml maps the imports to this prefix, and
#: libs/go/natsconn (WorkerRuntimeJSAPIPrefix) holds the same string.
RUNTIME_API_PREFIX = "JS.RUNTIME.API"

ACK_WAIT_SECONDS = 60
#: NakWithDelay for poison. Never Term: see the module docstring.
POISON_DELAY_SECONDS = 24 * 60 * 60

MAX_FETCH_BATCH = 64
MAX_FETCH_EXPIRES_MILLIS = 30_000
MAX_TRANSPORT_MESSAGE_BYTES = 64 * 1024
MAX_TRANSPORT_PAYLOAD_BYTES = 48 * 1024

_TOKEN_RE = re.compile(r"[0-9a-f]{64}")
_REASON_RE = re.compile(r"[A-Z][A-Z0-9_]{0,63}")
_MAX_STABLE_DELIVERY_ID_BYTES = 256
_MAX_SUBJECT_BYTES = 256
_NANOSECONDS = 1_000_000_000


def route_token(stream: str) -> str:
    """``ELITEA_RT_V1_AGENT`` -> ``agent`` (1-32 of [A-Z0-9], letter first)."""

    if not isinstance(stream, str) or not stream.startswith(STREAM_PREFIX):
        raise ValueError("runtime command stream is not an ELITEA_RT_V1_<ROUTE> name")
    route = stream[len(STREAM_PREFIX) :]
    if not route or len(route) > 32 or not route.isascii():
        raise ValueError("runtime command stream is not an ELITEA_RT_V1_<ROUTE> name")
    for index, character in enumerate(route):
        if "A" <= character <= "Z" or (index > 0 and "0" <= character <= "9"):
            continue
        raise ValueError("runtime command stream is not an ELITEA_RT_V1_<ROUTE> name")
    return route.lower()


def filter_subject(stream: str) -> str:
    return f"{SUBJECT_ROOT}.{route_token(stream)}.d.*"


def delivery_token(delivery_id: str) -> str:
    """``hex(sha256(delivery_id))``: a delivery ID may hold '.', '*', '>'."""

    return hashlib.sha256(delivery_id.encode("utf-8")).hexdigest()


def delivery_subject(stream: str, delivery_id: str) -> str:
    return f"{SUBJECT_ROOT}.{route_token(stream)}.d.{delivery_token(delivery_id)}"


def dead_letter_key(stream: str, delivery_id: str) -> str:
    return f"{route_token(stream)}.{delivery_token(delivery_id)}"


def runtime_api_prefix(tls: "NatsTlsPaths | None") -> str:
    """The JetStream API prefix that reaches the command streams' durables.

    With a client identity (the chart's posture) the worker is in the WORKER
    account and reaches RUNTIME's consumer API through the imports mapped to
    :data:`RUNTIME_API_PREFIX`. Without one (compose's plaintext posture, one
    global account) the streams are in the worker's own account.
    """

    return RUNTIME_API_PREFIX if tls is not None else DEFAULT_API_PREFIX


def validate_route(stream: str, consumer: str) -> None:
    """The (stream, durable) pair must be one the bootstrap creates."""

    expected = KNOWN_STREAMS.get(stream)
    if expected is None:
        raise ValueError(
            "runtime command stream must be one of "
            + ", ".join(sorted(KNOWN_STREAMS))
        )
    if consumer != expected:
        raise ValueError(
            f"runtime command stream {stream} is consumed by the durable {expected}"
        )


# ── One delivered message ────────────────────────────────────────────────────


@dataclass(frozen=True, slots=True, eq=False)
class CommandDelivery:
    """One JetStream delivery of one signed command.

    ``rejection`` is set when the transport shape itself is poison (a subject
    that is not ``elitea.rt.v1.<route>.d.<64 hex>``, an oversized body): the
    serve loop dead-letters such a delivery without decoding it.
    """

    stream: str
    consumer: str
    subject: str
    stream_sequence: int
    num_delivered: int
    signed_envelope: bytes
    message: Any = field(default=None, repr=False)
    rejection: WorkerError | None = None

    @property
    def entry_id(self) -> str:
        """The producer's entry ID shape, ``<stream>:<sequence>``."""

        return f"{self.stream}:{self.stream_sequence}"

    @property
    def delivery_token(self) -> str:
        """The subject's last token: ``sha256(delivery_id)`` when well formed."""

        return self.subject.rpartition(".")[2]

    @property
    def route(self) -> str:
        return route_token(self.stream)

    @property
    def dead_letter_key(self) -> str:
        """``<route>.<sha256(delivery_id)>``.

        A malformed subject token is hashed again, so the key is always a valid
        KV key and still names exactly this subject.
        """

        token = self.delivery_token
        if _TOKEN_RE.fullmatch(token) is None:
            token = hashlib.sha256(token.encode("utf-8", "replace")).hexdigest()
        return f"{self.route}.{token}"

    @property
    def is_settled(self) -> bool:
        """True once this worker acked, nakked or otherwise answered it."""

        message = self.message
        return bool(message is not None and getattr(message, "is_acked", False))


def require_subject_names_command(
    delivery: CommandDelivery,
    idempotency_key: str,
) -> None:
    """The subject's hash token must name the signed command's delivery.

    Called after the signature is verified and before the claim. A mismatch is
    poison: a misrouted or forged subject never reaches PostgreSQL.
    """

    if (
        not isinstance(idempotency_key, str)
        or not idempotency_key
        or delivery.delivery_token != delivery_token(idempotency_key)
    ):
        raise SubjectTokenMismatch()


class CommandSignatureRejected(WorkerError):
    """The signed envelope failed verification (digest or Ed25519 signature)."""

    def __init__(self, safe_message: str) -> None:
        super().__init__("AUTHORIZATION_FAILED", safe_message, exit_code=4)


class SubjectTokenMismatch(WorkerError):
    def __init__(self) -> None:
        super().__init__(
            "SUBJECT_TOKEN_MISMATCH",
            "The command subject does not name the signed command's delivery.",
            exit_code=2,
        )


#: Poison that is recorded and then TERMINATED rather than nak'd for 24h: it
#: fails a check that happens before any claim and can never pass.
TERMINAL_POISON: tuple[type[WorkerError], ...] = (
    CommandSignatureRejected,
    SubjectTokenMismatch,
)


class DeadLetterUnrecorded(DependencyUnavailable):
    """The dead-letter bucket did not take the record."""

    def __init__(self) -> None:
        super().__init__("The dead-letter record could not be written.")


class TransportMessageRejected(WorkerError):
    def __init__(self, code: str, safe_message: str) -> None:
        super().__init__(code, safe_message, exit_code=2)


# ── The consumer ─────────────────────────────────────────────────────────────


class PullSubscription(Protocol):
    async def fetch(self, batch: int = 1, timeout: float | None = 5) -> list[Any]: ...


class DeadLetterStore(Protocol):
    async def put(self, key: str, value: bytes) -> int: ...


class JetStreamCommandConsumer:
    """Bounded intake, heartbeat, settlement and dead-letter for one durable."""

    def __init__(
        self,
        subscription: PullSubscription,
        *,
        stream: str,
        consumer: str,
        worker_name: str,
        dead_letters: DeadLetterStore,
        fetch_batch: int = 8,
        fetch_expires_millis: int = 1_000,
        retry_delay_millis: int = ACK_WAIT_SECONDS * 1_000,
        ack_timeout_seconds: float = 5.0,
        max_message_bytes: int = MAX_TRANSPORT_MESSAGE_BYTES,
        max_payload_bytes: int = MAX_TRANSPORT_PAYLOAD_BYTES,
        clock_unix_millis: Callable[[], int] | None = None,
        event_sink: Callable[[str, WorkerError | None], None] | None = None,
    ) -> None:
        validate_route(stream, consumer)
        if not worker_name:
            raise ValueError("the worker's client name is required")
        if not 1 <= fetch_batch <= MAX_FETCH_BATCH:
            raise ValueError("JetStream fetch batch is outside the runtime-v1 bound")
        if not 1 <= fetch_expires_millis <= MAX_FETCH_EXPIRES_MILLIS:
            raise ValueError("JetStream fetch expiry is outside the runtime-v1 bound")
        if retry_delay_millis < 1 or ack_timeout_seconds <= 0:
            raise ValueError("JetStream delays must be positive")
        if not 1 <= max_payload_bytes <= max_message_bytes:
            raise ValueError("the payload limit cannot exceed the message limit")
        self._subscription = subscription
        self._stream = stream
        self._consumer = consumer
        self._route = route_token(stream)
        self._subject_prefix = f"{SUBJECT_ROOT}.{self._route}.d."
        self._worker_name = worker_name
        self._dead_letters = dead_letters
        self._fetch_batch = fetch_batch
        self._fetch_expires = fetch_expires_millis / 1_000
        self._retry_delay = retry_delay_millis / 1_000
        self._ack_timeout = ack_timeout_seconds
        self._max_message_bytes = max_message_bytes
        self._max_payload_bytes = max_payload_bytes
        self._clock = clock_unix_millis or (lambda: int(time.time() * 1_000))
        self._events = event_sink or (lambda _event, _error: None)
        self._ack_prefix = f"$JS.ACK.{stream}.{consumer}."

    @property
    def stream(self) -> str:
        return self._stream

    @property
    def consumer(self) -> str:
        return self._consumer

    @property
    def delivery_batch_size(self) -> int:
        return self._fetch_batch

    async def fetch(self, *, count: int | None = None) -> tuple[CommandDelivery, ...]:
        """Pull at most ``count`` messages, waiting at most the fetch expiry.

        An empty pull is ``()``, not an error: nats-py raises TimeoutError for
        it, and that is the old XREADGROUP BLOCK running out.
        """

        batch = self._bounded_count(count)
        try:
            messages = await self._subscription.fetch(batch, timeout=self._fetch_expires)
        except TimeoutError:
            return ()
        messages = list(messages or ())
        if len(messages) > batch:
            # nats-py hands back messages a timed-out earlier pull left in the
            # inbox together with this pull's. Those were never reserved, so
            # they go straight back for another delivery rather than being
            # held, unheartbeated, until AckWait.
            excess = messages[batch:]
            messages = messages[:batch]
            for message in excess:
                try:
                    await message.nak()
                except Exception:
                    continue
        # Per message: one undecodable message must not discard the batch it
        # arrived in (the others would sit unheartbeated until AckWait).
        deliveries: list[CommandDelivery] = []
        for message in messages:
            try:
                deliveries.append(self._decode(message))
            except InvalidInput as exc:
                await self._dispose_undecodable(message, exc)
        return tuple(deliveries)

    async def _dispose_undecodable(self, message: Any, error: InvalidInput) -> None:
        """Answer only what this durable may answer; skip the rest.

        A reply subject of THIS durable with unreadable metadata can never
        decode: it is dead-lettered, then terminated. Like every terminated
        poison it is never terminated without its record: when the record
        cannot be written it is nak'd with the retry delay, so the next
        delivery retries the record. A reply of another stream or consumer
        (or none) cannot be answered by this identity at all — the
        permission table grants it this durable's ack subjects only — so it
        is reported and left to its own consumer's AckWait.
        """

        reply = getattr(message, "reply", None)
        if isinstance(reply, str) and reply.startswith(self._ack_prefix):
            try:
                metadata = message.metadata
                foreign = (
                    metadata.stream != self._stream or metadata.consumer != self._consumer
                )
            except Exception:
                foreign = False
            if not foreign:
                delivery = self._undecodable_delivery(message, reply)
                try:
                    await self.record_dead_letter(delivery, reason=_UNDECODABLE_REASON)
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    self._events("nats_delivery_undecodable_unrecorded", exc)
                    try:
                        await message.nak(delay=self._retry_delay)
                    except asyncio.CancelledError:
                        raise
                    except Exception:
                        self._events("nats_nak_unavailable", DependencyUnavailable())
                    return
                self._events("nats_delivery_undecodable_terminated", error)
                try:
                    await message.term()
                except asyncio.CancelledError:
                    raise
                except Exception:
                    self._events("nats_term_unavailable", DependencyUnavailable())
                return
        self._events("nats_delivery_foreign_skipped", error)

    def _undecodable_delivery(self, message: Any, reply: str) -> CommandDelivery:
        """Where the record points for a message whose metadata is unreadable.

        The ack subject ``$JS.ACK.<stream>.<consumer>.<delivered>.<sseq>...``
        carries the coordinates; a field that does not parse is recorded as 0.
        """

        def number(index: int) -> int:
            fields = reply[len(self._ack_prefix) :].split(".")
            try:
                value = int(fields[index])
            except (IndexError, ValueError):
                return 0
            return value if value >= 0 else 0

        subject = getattr(message, "subject", "")
        return CommandDelivery(
            stream=self._stream,
            consumer=self._consumer,
            subject=subject if isinstance(subject, str) else "",
            stream_sequence=number(1),
            num_delivered=number(0),
            signed_envelope=b"",
            message=message,
        )

    async def in_progress(self, deliveries: Sequence[CommandDelivery]) -> int:
        """``+WPI`` for every owned, unanswered message; resets AckWait.

        Transport liveness only: it never grants the business claim. Every
        message is attempted even when one fails, then one failure is raised.
        """

        refreshed = 0
        failed = False
        for delivery in deliveries:
            message = delivery.message
            if message is None or delivery.is_settled:
                continue
            try:
                await message.in_progress()
                refreshed += 1
            except asyncio.CancelledError:
                raise
            except Exception:
                failed = True
        if failed:
            raise DependencyUnavailable("JetStream refused an in-progress heartbeat.")
        return refreshed

    async def ack_after_settlement(
        self,
        delivery: CommandDelivery,
        stable_delivery_id: str,
    ) -> None:
        """Double-ack one message after its settlement is durable.

        The caller owns the ordering guarantee: this must not be called before
        the terminal PostgreSQL receipt has been validated. An ack of a message
        this worker already acknowledged is idempotent success, and so is the
        server's answer for one some other delivery already acknowledged.
        """

        if delivery.stream != self._stream or delivery.consumer != self._consumer:
            raise InvalidInput("The delivered command belongs to another stream.")
        if not _valid_stable_delivery_id(stable_delivery_id):
            raise InvalidInput("The stable delivery identity is malformed.")
        require_subject_names_command(delivery, stable_delivery_id)
        message = delivery.message
        if message is None:
            raise InvalidInput("The delivered command has no JetStream reply.")
        if delivery.is_settled:
            return
        try:
            response = await message.ack_sync(timeout=self._ack_timeout)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            if delivery.is_settled:
                return
            raise DependencyUnavailable(
                "JetStream did not confirm the command acknowledgement."
            ) from exc
        payload = getattr(response, "data", b"") or b""
        if payload:
            # The server answers a double ack with an empty message; anything
            # else is an API error it chose to report.
            raise DependencyUnavailable(
                "JetStream did not confirm the command acknowledgement."
            )

    async def retry_later(self, delivery: CommandDelivery) -> None:
        """``NakWithDelay(retry delay)``: the claim said not now."""

        await self._nak(delivery, self._retry_delay)

    async def park(self, delivery: CommandDelivery) -> None:
        """``NakWithDelay(24h)`` without a new record (already dead-lettered)."""

        await self._nak(delivery, POISON_DELAY_SECONDS)

    async def release(self, delivery: CommandDelivery) -> None:
        """A nak WITHOUT delay: give a message this worker never started back.

        Graceful shutdown answers every queued, unstarted message this way, so
        another replica takes it now instead of after AckWait.
        """

        message = delivery.message
        if message is None or delivery.is_settled:
            return
        await message.nak()

    async def terminate(self, delivery: CommandDelivery) -> None:
        """``Term``: only for :data:`TERMINAL_POISON`, only after its record."""

        message = delivery.message
        if message is None or delivery.is_settled:
            return
        await message.term()

    async def record_dead_letter(
        self, delivery: CommandDelivery, *, reason: str
    ) -> None:
        """Write the one dead-letter record for a poison delivery.

        The record says where to look, never what the command said: no
        envelope bytes, delivery ID or command field. It is written BEFORE the
        poison is answered: a poison parked or terminated without a record
        would be invisible to the alert, so the caller answers a failed write
        with the ordinary retry delay instead and the next delivery retries
        the record.
        """

        record = self.dead_letter_record(delivery, reason=reason)
        try:
            await self._dead_letters.put(delivery.dead_letter_key, record)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            raise DeadLetterUnrecorded() from exc

    def dead_letter_record(self, delivery: CommandDelivery, *, reason: str) -> bytes:
        if not isinstance(reason, str) or _REASON_RE.fullmatch(reason) is None:
            reason = "UNCLASSIFIED"
        subject = delivery.subject
        if len(subject.encode("utf-8", "replace")) > _MAX_SUBJECT_BYTES:
            subject = subject[:_MAX_SUBJECT_BYTES]
        value = {
            "schema": DEAD_LETTER_SCHEMA,
            "stream": delivery.stream,
            "consumer": delivery.consumer,
            "subject": subject,
            "stream_sequence": int(delivery.stream_sequence),
            "num_delivered": int(delivery.num_delivered),
            "reason": reason,
            "worker": self._worker_name,
            "recorded_at_unix_millis": int(self._clock()),
        }
        return json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8")

    async def _nak(self, delivery: CommandDelivery, delay_seconds: float) -> None:
        message = delivery.message
        if message is None or delivery.is_settled:
            return
        await message.nak(delay=delay_seconds)

    def _bounded_count(self, count: int | None) -> int:
        if count is None:
            return self._fetch_batch
        if (
            isinstance(count, bool)
            or not isinstance(count, int)
            or not 1 <= count <= self._fetch_batch
        ):
            raise ValueError("JetStream fetch count is outside the configured batch")
        return count

    def _decode(self, message: Any) -> CommandDelivery:
        try:
            metadata = message.metadata
            stream = metadata.stream
            consumer = metadata.consumer
            sequence = int(metadata.sequence.stream)
            delivered = int(metadata.num_delivered)
        except Exception as exc:
            raise InvalidInput("JetStream delivered a message without metadata.") from exc
        if stream != self._stream or consumer != self._consumer:
            raise InvalidInput("JetStream delivered a message of another consumer.")
        subject = message.subject if isinstance(message.subject, str) else ""
        data = message.data
        payload = bytes(data) if isinstance(data, (bytes, bytearray, memoryview)) else b""
        rejection: WorkerError | None = None
        token = subject[len(self._subject_prefix) :]
        if not subject.startswith(self._subject_prefix) or _TOKEN_RE.fullmatch(token) is None:
            rejection = TransportMessageRejected(
                "SUBJECT_MALFORMED",
                "The command subject is not elitea.rt.v1.<route>.d.<sha256>.",
            )
        elif not payload or len(payload) > self._max_payload_bytes:
            rejection = TransportMessageRejected(
                "PAYLOAD_SIZE_REJECTED",
                "The command body is empty or exceeds the transport payload limit.",
            )
        elif len(payload) + _header_bytes(message.headers) > self._max_message_bytes:
            rejection = TransportMessageRejected(
                "MESSAGE_SIZE_REJECTED",
                "The command exceeds the transport message limit.",
            )
        return CommandDelivery(
            stream=stream,
            consumer=consumer,
            subject=subject,
            stream_sequence=sequence,
            num_delivered=delivered,
            signed_envelope=payload if rejection is None else b"",
            message=message,
            rejection=rejection,
        )


def _header_bytes(headers: Any) -> int:
    """The wire size of a header block, as the server counts it."""

    if not headers:
        return 0
    total = len(b"NATS/1.0\r\n\r\n")
    for name, value in dict(headers).items():
        total += len(str(name).encode("utf-8")) + len(str(value).encode("utf-8")) + 4
    return total


def _valid_stable_delivery_id(value: object) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return len(encoded) <= _MAX_STABLE_DELIVERY_ID_BYTES and not any(
        character in value for character in ("\r", "\n", "\x00")
    )


# ── Bind: verify, never create ───────────────────────────────────────────────


class CommandBusAbsent(DependencyUnavailable):
    """A stream, durable or bucket the bootstrap owns does not exist."""


class CommandBusDrift(WorkerError):
    """A stream or durable exists but is not the contract's shape."""

    def __init__(self, safe_message: str) -> None:
        super().__init__("COMMAND_BUS_DRIFT", safe_message, exit_code=3)


class ApiRequester(Protocol):
    async def request(self, subject: str, payload: bytes = b"", timeout: float = 0.5) -> Any: ...


async def verify_command_bus(
    client: ApiRequester,
    *,
    stream: str,
    consumer: str,
    fetch_batch: int,
    fetch_expires_millis: int,
    timeout_seconds: float,
    api_prefix: str = DEFAULT_API_PREFIX,
) -> None:
    """Refuse to start unless the durable is the contract's.

    A raw JetStream API read, so every field the contract names is compared as
    the server reports it, including the ones nats-py's dataclasses drop. The
    STREAM is not read: the worker has no STREAM.INFO grant (elitea-main, the
    stream's writer, verifies its shape at boot), and the durable's filter
    subject already names the stream's subjects.
    """

    validate_route(stream, consumer)
    if api_prefix not in (DEFAULT_API_PREFIX, RUNTIME_API_PREFIX):
        raise ValueError("the JetStream API prefix is not one the command bus uses")
    consumer_info = await _api_read(
        client,
        f"{api_prefix}.CONSUMER.INFO.{stream}.{consumer}",
        timeout_seconds,
        f"consumer {stream}/{consumer}",
    )
    verify_consumer_config(
        consumer_info.get("config"),
        stream=stream,
        consumer=consumer,
        fetch_batch=fetch_batch,
        fetch_expires_millis=fetch_expires_millis,
    )


def verify_consumer_config(
    config: Any,
    *,
    stream: str,
    consumer: str,
    fetch_batch: int,
    fetch_expires_millis: int,
) -> None:
    if not isinstance(config, dict):
        raise CommandBusDrift(f"The consumer {stream}/{consumer} reported no configuration.")
    expected_filter = filter_subject(stream)
    problems: list[str] = []
    if config.get("durable_name") != consumer:
        problems.append("durable_name")
    if config.get("deliver_subject"):
        problems.append("pull")
    if config.get("ack_policy") != "explicit":
        problems.append("ack_policy")
    if config.get("ack_wait") != ACK_WAIT_SECONDS * _NANOSECONDS:
        problems.append("ack_wait")
    if config.get("max_deliver") != -1:
        problems.append("max_deliver")
    if config.get("deliver_policy") != "all":
        problems.append("deliver_policy")
    filters = config.get("filter_subjects")
    single = config.get("filter_subject")
    if not (
        (single == expected_filter and not filters)
        or (not single and filters == [expected_filter])
    ):
        problems.append("filter_subject")
    if config.get("backoff"):
        problems.append("backoff")
    if config.get("headers_only"):
        problems.append("headers_only")
    max_batch = config.get("max_batch") or 0
    if isinstance(max_batch, int) and 0 < max_batch < fetch_batch:
        problems.append("max_batch")
    max_expires = config.get("max_expires") or 0
    if isinstance(max_expires, int) and 0 < max_expires < fetch_expires_millis * 1_000_000:
        problems.append("max_expires")
    if problems:
        raise CommandBusDrift(
            f"The consumer {stream}/{consumer} is not the command-bus contract's "
            f"shape ({', '.join(problems)})."
        )


async def _api_read(
    client: ApiRequester,
    subject: str,
    timeout_seconds: float,
    description: str,
) -> dict[str, Any]:
    try:
        response = await client.request(subject, b"", timeout=timeout_seconds)
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        raise DependencyUnavailable(
            f"The JetStream API did not answer for the {description}."
        ) from exc
    try:
        body = json.loads(bytes(response.data))
    except (TypeError, ValueError) as exc:
        raise DependencyUnavailable(
            f"The JetStream API answered malformed JSON for the {description}."
        ) from exc
    if not isinstance(body, dict):
        raise DependencyUnavailable(
            f"The JetStream API answered malformed JSON for the {description}."
        )
    error = body.get("error")
    if error is not None:
        code = error.get("code") if isinstance(error, dict) else None
        if code == 404:
            raise CommandBusAbsent(
                f"The {description} does not exist; the NATS bootstrap Job creates it "
                "and a worker never does."
            )
        raise DependencyUnavailable(
            f"The JetStream API refused to describe the {description}."
        )
    return body


async def bind_command_consumer(
    client: Any,
    *,
    stream: str,
    consumer: str,
    worker_name: str,
    fetch_batch: int,
    fetch_expires_millis: int,
    retry_delay_millis: int,
    ack_timeout_seconds: float,
    max_message_bytes: int = MAX_TRANSPORT_MESSAGE_BYTES,
    max_payload_bytes: int = MAX_TRANSPORT_PAYLOAD_BYTES,
    api_prefix: str = DEFAULT_API_PREFIX,
    event_sink: Callable[[str, WorkerError | None], None] | None = None,
) -> JetStreamCommandConsumer:
    """Verify the durable and the dead-letter bucket, then bind to both.

    Two JetStream contexts: the durable through ``api_prefix`` (the RUNTIME
    import when the worker presents an identity, see
    :func:`runtime_api_prefix`), and the dead-letter bucket through the
    worker's own account's default ``$JS.API``. Ack subjects are each
    delivery's reply subject either way.
    """

    await verify_command_bus(
        client,
        stream=stream,
        consumer=consumer,
        fetch_batch=fetch_batch,
        fetch_expires_millis=fetch_expires_millis,
        timeout_seconds=ack_timeout_seconds,
        api_prefix=api_prefix,
    )
    commands = client.jetstream(prefix=api_prefix, timeout=ack_timeout_seconds)
    own = client.jetstream(timeout=ack_timeout_seconds)
    try:
        dead_letters = await own.key_value(DEAD_LETTER_BUCKET)
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        if type(exc).__name__ == "BucketNotFoundError":
            raise CommandBusAbsent(
                f"The dead-letter bucket {DEAD_LETTER_BUCKET} does not exist; the NATS "
                "bootstrap Job creates it and a worker never does."
            ) from exc
        raise DependencyUnavailable(
            f"The dead-letter bucket {DEAD_LETTER_BUCKET} could not be bound."
        ) from exc
    subscription = await commands.pull_subscribe_bind(consumer, stream=stream)
    return JetStreamCommandConsumer(
        subscription,
        stream=stream,
        consumer=consumer,
        worker_name=worker_name,
        dead_letters=dead_letters,
        fetch_batch=fetch_batch,
        fetch_expires_millis=fetch_expires_millis,
        retry_delay_millis=retry_delay_millis,
        ack_timeout_seconds=ack_timeout_seconds,
        max_message_bytes=max_message_bytes,
        max_payload_bytes=max_payload_bytes,
        event_sink=event_sink,
    )


# ── Connection: mTLS, reloaded on every reconnect ────────────────────────────


@dataclass(frozen=True, slots=True)
class NatsTlsPaths:
    """The elitea-worker identity's mTLS material (all three, or none)."""

    ca_path: Path
    certificate_path: Path
    private_key_path: Path


class _ExactCANatsContext(_PRIVATE_PLANE_SSL_CONTEXT):  # type: ignore[misc, valid-type]
    """The standard-library context, recognisable after truststore injection.

    The pinned SDK injects truststore at import, which REPLACES
    ``ssl.SSLContext`` process-wide. nats-py upgrades the socket with
    ``loop.start_tls``, and asyncio refuses any context that is not an
    instance of the CURRENT ``ssl.SSLContext`` — so the exact-CA context the
    private plane needs (never the system roots truststore would bring) was
    refused with a TypeError. Reporting the current class through
    ``__class__`` passes that isinstance check while every method that
    actually runs is still the standard library's, bound to the deployed CA.
    """

    @property  # type: ignore[misc]
    def __class__(self) -> type:  # noqa: D105
        current = ssl.SSLContext
        if isinstance(current, type) and issubclass(current, _PRIVATE_PLANE_SSL_CONTEXT_BASE):
            return current
        return _ExactCANatsContext


def nats_client_context(paths: NatsTlsPaths) -> ssl.SSLContext:
    """TLS 1.3, the deployed private CA only, the elitea-worker client cert.

    Read from disk on every call, so a reconnect presents rotated material.
    """

    try:
        context = _ExactCANatsContext(ssl.PROTOCOL_TLS_CLIENT)
        context.load_verify_locations(cafile=str(paths.ca_path))
    except (OSError, ssl.SSLError) as exc:
        raise InvalidInput("The NATS CA bundle is invalid.") from exc
    _PRIVATE_PLANE_SSL_CONTEXT_BASE.minimum_version.__set__(
        context, ssl.TLSVersion.TLSv1_3
    )
    context.check_hostname = True
    _PRIVATE_PLANE_SSL_CONTEXT_BASE.verify_mode.__set__(context, ssl.CERT_REQUIRED)
    try:
        context.load_cert_chain(
            certfile=str(paths.certificate_path),
            keyfile=str(paths.private_key_path),
        )
    except (OSError, ssl.SSLError) as exc:
        raise InvalidInput("The NATS client TLS identity is invalid.") from exc
    return context


def nats_servers(url: str) -> list[str]:
    """Split and re-check a ``nats_url`` (config.py validates it first)."""

    servers = [part for part in url.split(",")]
    if not servers or any(not server for server in servers):
        raise ValueError("NATS URL is malformed")
    for server in servers:
        parsed = urlsplit(server)
        if (
            parsed.scheme not in ("nats", "tls")
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
        ):
            raise ValueError("NATS URL is malformed")
    return servers


EventSink = Callable[[str, WorkerError | None], None]


class NatsCommandBusConnection:
    """One NATS connection for the command bus, with per-reconnect TLS reload.

    nats-py reads ``options["tls"]`` on every (re)connect. A fresh context is
    built from disk before each reconnect (the disconnected callback runs
    before the reconnect loop) and again after every failed attempt while
    reconnecting, so rotated certificate material is presented without a
    process restart. A reload that fails keeps the previous context and is
    reported; the next attempt reads the files again.
    """

    def __init__(
        self,
        *,
        url: str,
        name: str,
        tls: NatsTlsPaths | None,
        connect_timeout_seconds: float,
        event_sink: EventSink,
        client_factory: Callable[[], Any] | None = None,
        context_factory: Callable[[NatsTlsPaths], ssl.SSLContext] = nats_client_context,
    ) -> None:
        self._servers = nats_servers(url)
        schemes = {urlsplit(server).scheme for server in self._servers}
        if schemes == {"tls"} and tls is None:
            raise ValueError("tls:// requires the NATS client TLS material")
        if schemes != {"tls"} and tls is not None:
            raise ValueError("NATS client TLS material requires tls:// URLs")
        if not name or connect_timeout_seconds <= 0:
            raise ValueError("NATS connection name and timeout are required")
        self._name = name
        self._tls = tls
        self._timeout = connect_timeout_seconds
        self._events = event_sink
        self._context_factory = context_factory
        if client_factory is None:
            from nats.aio.client import Client

            client_factory = Client
        self.client = client_factory()

    def _refresh_tls(self) -> None:
        if self._tls is None:
            return
        try:
            context = self._context_factory(self._tls)
        except WorkerError as exc:
            self._events("nats_tls_reload_rejected", exc)
            return
        self.client.options["tls"] = context

    async def _disconnected(self) -> None:
        self._events("nats_disconnected", None)
        self._refresh_tls()

    async def _reconnected(self) -> None:
        self._events("nats_reconnected", None)

    async def _error(self, _error: Exception) -> None:
        self._events("nats_connection_error", DependencyUnavailable())
        if getattr(self.client, "is_reconnecting", False):
            self._refresh_tls()

    async def _closed(self) -> None:
        self._events("nats_closed", None)

    async def connect(self) -> Any:
        options: dict[str, Any] = {
            "servers": self._servers,
            "name": self._name,
            "inbox_prefix": INBOX_PREFIX,
            "connect_timeout": self._timeout,
            "allow_reconnect": True,
            "max_reconnect_attempts": -1,
            "reconnect_time_wait": 2,
            "error_cb": self._error,
            "disconnected_cb": self._disconnected,
            "reconnected_cb": self._reconnected,
            "closed_cb": self._closed,
        }
        if self._tls is not None:
            options["tls"] = self._context_factory(self._tls)
        await self.client.connect(**options)
        return self.client

    async def aclose(self) -> None:
        client = self.client
        if getattr(client, "is_closed", True):
            return
        # Close, not drain: draining would ack nothing but would wait on the
        # pull inbox; owned messages are left for AckWait redelivery.
        await client.close()


__all__ = [
    "ACK_WAIT_SECONDS",
    "CommandBusAbsent",
    "CommandBusDrift",
    "CommandDelivery",
    "CommandSignatureRejected",
    "DEAD_LETTER_BUCKET",
    "DEAD_LETTER_SCHEMA",
    "DEFAULT_API_PREFIX",
    "DeadLetterUnrecorded",
    "INBOX_PREFIX",
    "JetStreamCommandConsumer",
    "KNOWN_STREAMS",
    "NatsCommandBusConnection",
    "NatsTlsPaths",
    "POISON_DELAY_SECONDS",
    "RUNTIME_API_PREFIX",
    "SubjectTokenMismatch",
    "TERMINAL_POISON",
    "bind_command_consumer",
    "dead_letter_key",
    "delivery_subject",
    "delivery_token",
    "filter_subject",
    "nats_client_context",
    "require_subject_names_command",
    "route_token",
    "runtime_api_prefix",
    "validate_route",
    "verify_command_bus",
]
