"""One delivered ``configuration.validate.v1`` execution transaction.

The processor keeps transport ownership explicit: Redis carries only the
signed reference command, control gRPC claims and settles, HTTPS retrieves the
claim-bound settings, and output gRPC carries the result. Redis is acknowledged
only after a server-proven no-authority terminal receipt or a terminal output
ACK and settlement receipt.
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import math
import sys
import threading
from collections import deque
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from enum import Enum
from typing import Any, Protocol

from elitea.runtime.v1 import (
    agent_pb2,
    command_pb2,
    common_pb2,
    control_pb2,
    envelope_pb2,
    errors_pb2,
    indexing_pb2,
    node_event_pb2,
    output_pb2,
    toolkit_pb2,
)

from elitea_worker.agents.client_context import (
    EliteaClientContext,
    RuntimeExecutionClaim,
)
from elitea_worker.agents.checkpoint import CurrentAgentCheckpointFactory
from elitea_worker.agents.sdk_adapter import (
    EliteaSdkAgentAdapter,
    EliteaSdkIndexingAdapter,
    EliteaSdkToolkitToolAdapter,
    SdkBudgetExceeded,
)
from elitea_worker.constants import (
    AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
    AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
    AGENT_EXECUTION_CAPABILITY_VERSION,
    AGENT_EXECUTION_REQUEST_ROLE,
    AGENT_INPUT_MEDIA_TYPE,
    INDEX_INGEST_CAPABILITY_ID,
    INDEX_INGEST_CAPABILITY_VERSION,
    MAX_BUNDLE_ENTRIES,
    MAX_AGENT_INPUT_BYTES,
    MAX_SAFE_STRING_BYTES,
    MAX_SETTINGS_BYTES,
    OUTPUT_SCHEMA_REVISION,
    TOOLKIT_CALL_TOOL_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
)
from elitea_worker.execution.errors import (
    AmbiguousExecutionRecovery,
    AuthorizationFailure,
    DeadlineExceeded,
    DependencyUnavailable,
    ExecutionCancelled,
    IncompatibleVersion,
    InternalFailure,
    InvalidInput,
    OutputCancellationWon,
    OutputDeadlineWon,
    ResourceExhausted,
    UnsupportedCapability,
    WorkerError,
)
from elitea_worker.execution.supervisor import ExecutionRunner
from elitea_worker.execution.sync_executor import (
    SyncExecutorAdmissionRejected,
    SyncExecutorReservation,
)
from elitea_worker.fixtures.bundle import (
    FixtureContent,
    FixtureEntry,
    parse_json_value,
    parse_settings_json,
    project_input_manifest_entries,
)
from elitea_worker.handlers.indexing import (
    CurrentIndexNodeEventCallback,
    CurrentIndexNodeEventContext,
    IndexIngestHandler,
    IndexIngestInputBinding,
    ResolvedIndexIngestInput,
)
from elitea_worker.handlers.agent import (
    AgentExecutionHandler,
    AgentExecutionKind,
    AgentExecutionRequest,
    AgentExecutionResult,
)
from elitea_worker.handlers.agent_events import (
    CurrentAgentNodeEventCallback,
    CurrentAgentNodeEventContext,
    nested_skill_registry_from_payload,
)
from elitea_worker.handlers.toolkit_call_tool import (
    ResolvedToolkitCallToolInput,
    ToolkitCallToolHandler,
    ToolkitCallToolInputBinding,
)
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.protocol.codec import (
    VerifiedWorkerCommand,
    SignedCommandAuthenticator,
    build_node_event_output_frame,
    build_output_frame,
    parse_and_verify_signed_command,
    parse_execution_input_bundle,
    validation_request_from,
)
from elitea_worker.protocol.indexing import (
    bind_result_summary,
    request_from,
    resolve_embedding_binding,
)
from elitea_worker.protocol.agent import (
    bind_result_artifact as bind_agent_result_artifact,
    parse_agent_execution_input,
    request_from as agent_request_from,
)
from elitea_worker.protocol.toolkit_call_tool import (
    bind_result_summary as bind_tool_run_result_summary,
    request_from as tool_run_request_from,
    unsupported_toolkit_result,
)
from elitea_worker.protocol.node_event import (
    InvalidCurrentNodeEvent,
    encode_current_node_event_json,
)
from elitea_worker.transport.input_content import (
    ClaimBoundInputReference,
    ClaimBoundInputRequestBuilder,
    ScopedInputContentClient,
)
from elitea_worker.transport.redis_commands import RedisCommandDelivery


_TERMINAL_OUTCOMES = frozenset(
    {
        common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED,
        common_pb2.EXECUTION_OUTCOME_V1_FAILED,
        common_pb2.EXECUTION_OUTCOME_V1_CANCELLED,
        common_pb2.EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN,
    }
)

_KNOWN_DESIRED_STATES = frozenset(
    {
        common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        common_pb2.DESIRED_EXECUTION_STATE_V1_DRAINING,
    }
)
_DEADLINE_RETIREMENT_SAFE_MESSAGE = (
    "The execution deadline was exceeded before worker authority was granted."
)

_INDEX_TOOLKIT_CONFIGURATION_ROLE = "index.toolkit_configuration"
_INDEX_TOOL_PARAMETERS_ROLE = "index.tool_parameters"
_INDEX_LLM_MODEL_ROLE = "index.llm_model"
_INDEX_LLM_CONFIGURATION_ROLE = "index.llm_configuration"
_INDEX_MCP_TOKENS_ROLE = "index.mcp_tokens"
_INDEX_EMBEDDING_BINDING_ROLE = "index.embedding_binding"
_INDEX_INPUT_AUDIENCE = "elitea.runtime.input.read.v1"
_INDEX_INTERNAL_FAILURE_FRAME_LIMIT = 8

# The two tool-run bundle entries. They are separate roles, not one blob,
# because they have different trust: the settings are redeemed by the platform
# from the saved toolkit row, the arguments come from the caller. Naming them
# apart is what lets the claim check refuse a bundle that swapped them.
_TOOL_RUN_SETTINGS_ROLE = "toolkit.call_tool.settings"
_TOOL_RUN_ARGUMENTS_ROLE = "toolkit.call_tool.arguments"


class ControlPlane(Protocol):
    async def claim_command(
        self, request: control_pb2.ClaimCommandRequestV1
    ) -> control_pb2.ClaimCommandResponseV1: ...

    async def prepare_settlement(
        self, request: control_pb2.PrepareSettlementRequestV1
    ) -> control_pb2.PrepareSettlementResponseV1: ...

    async def begin_execution(
        self, request: control_pb2.BeginExecutionRequestV1
    ) -> control_pb2.BeginExecutionResponseV1: ...

    async def authorize_invocation(
        self, request: control_pb2.AuthorizeInvocationRequestV1
    ) -> control_pb2.AuthorizeInvocationResponseV1: ...

    async def renew_lease(
        self, request: control_pb2.RenewLeaseRequestV1
    ) -> control_pb2.RenewLeaseResponseV1: ...

    async def observe_desired_state(
        self, request: control_pb2.ObserveDesiredStateRequestV1
    ) -> control_pb2.ObserveDesiredStateResponseV1: ...

class CommandSettlementAcker(Protocol):
    async def ack_after_settlement(
        self,
        delivery: RedisCommandDelivery,
        stable_delivery_id: str,
    ) -> None: ...


class OutputSession(Protocol):
    @property
    def has_pending_replay(self) -> bool: ...
    @property
    def pending_replay_frame(self) -> output_pb2.ExecutionOutputFrameV1 | None: ...
    def replays(self, frame: output_pb2.ExecutionOutputFrameV1) -> bool: ...
    async def reconcile_pending_through(self, sequence: int) -> None: ...
    async def replace_pending_exact(
        self,
        expected: output_pb2.ExecutionOutputFrameV1,
        replacement: output_pb2.ExecutionOutputFrameV1,
    ) -> None: ...

    async def replace_pending_cancelled_recovery(
        self,
        expected: output_pb2.ExecutionOutputFrameV1,
        replacement: output_pb2.ExecutionOutputFrameV1,
    ) -> None: ...
    async def replace_pending_ambiguous_recovery(
        self,
        expected: output_pb2.ExecutionOutputFrameV1,
        replacement: output_pb2.ExecutionOutputFrameV1,
    ) -> None: ...
    async def start(self) -> None: ...
    async def send(self, frame: output_pb2.ExecutionOutputFrameV1) -> None: ...
    async def wait_for_ack(self, sequence: int, timeout_seconds: float) -> None: ...
    async def close(self) -> None: ...


class DeliveryDisposition(Enum):
    EXECUTED_SETTLED_ACKED = "executed_settled_acked"
    RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED = "recovered_local_output_settled_acked"
    RECOVERED_TERMINAL_SETTLED_ACKED = "recovered_terminal_settled_acked"
    RECOVERED_SETTLEMENT_ACKED = "recovered_settlement_acked"
    TERMINAL_REDELIVERY_ACKED = "terminal_redelivery_acked"
    OWNED_ELSEWHERE_NOACK = "owned_elsewhere_noack"
    RECOVERY_REQUIRED_NOACK = "recovery_required_noack"
    RETRY_LATER_NOACK = "retry_later_noack"


@dataclass(frozen=True, slots=True)
class DeliveryResult:
    disposition: DeliveryDisposition
    output_frame: output_pb2.ExecutionOutputFrameV1 | None = None
    settlement_receipt_id: str | None = None
    execution_error: WorkerError | None = None


@dataclass(frozen=True, slots=True)
class _AcceptedClaim:
    verified: VerifiedWorkerCommand
    content: FixtureContent
    claim_id: str
    claim_handoff_watermark: int


@dataclass(frozen=True, slots=True)
class _AcceptedIndexClaim:
    verified: VerifiedWorkerCommand
    entries: tuple[FixtureEntry, ...]
    claim_id: str
    claim_handoff_watermark: int


@dataclass(frozen=True, slots=True)
class _AcceptedAgentClaim:
    verified: VerifiedWorkerCommand
    entry: FixtureEntry
    claim_id: str
    claim_handoff_watermark: int


@dataclass(frozen=True, slots=True)
class _ResolvedIndexInputs:
    toolkit_configuration: ResolvedIndexIngestInput
    tool_parameters: ResolvedIndexIngestInput
    llm_model: ResolvedIndexIngestInput | None
    llm_configuration: ResolvedIndexIngestInput | None
    mcp_tokens: ResolvedIndexIngestInput | None
    embedding_binding: ResolvedIndexIngestInput | None
    client_context: EliteaClientContext


@dataclass(frozen=True, slots=True)
class _ResolvedAgentInputs:
    request: AgentExecutionRequest
    client_context: EliteaClientContext


class _IndexProgressTransportFailure(RuntimeError):
    """Progress delivery lost its exact durable sequence authority."""


class _AgentTerminalFailure(RuntimeError):
    """The SDK returned a failed turn instead of raising for it.

    The stage name is operator diagnostics for the worker log only. It never
    crosses the output boundary: the runtime error message is canonicalized by
    code in `protocol/codec.py`.
    """

    def __init__(self, stage: str) -> None:
        super().__init__(stage)
        self.stage = stage


def _emit_index_internal_failure(
    *,
    stage: str,
    execution_id: str,
    error: Exception,
    sdk_failure_category: str | None = None,
) -> None:
    head_limit = _INDEX_INTERNAL_FAILURE_FRAME_LIMIT // 2
    tail_limit = _INDEX_INTERNAL_FAILURE_FRAME_LIMIT - head_limit
    head: list[dict[str, object]] = []
    tail: deque[dict[str, object]] = deque(maxlen=tail_limit)
    traceback = error.__traceback__
    while traceback is not None:
        code = traceback.tb_frame.f_code
        filename = code.co_filename.rsplit("/", 1)[-1].rsplit("\\", 1)[-1]
        frame = {
            "file": filename or "<unknown>",
            "function": code.co_name,
            "line": traceback.tb_lineno,
        }
        if len(head) < head_limit:
            head.append(frame)
        else:
            tail.append(frame)
        traceback = traceback.tb_next
    error_type = type(error)
    diagnostic = {
        "event": "index_ingest_internal_failure",
        "stage": stage,
        "execution_id": execution_id,
        "exception_module": error_type.__module__,
        "exception_name": error_type.__name__,
        "frames": [*head, *tail],
    }
    if sdk_failure_category is not None:
        diagnostic["sdk_failure_category"] = sdk_failure_category
    print(
        json.dumps(diagnostic, sort_keys=True, separators=(",", ":")),
        file=sys.stderr,
        flush=True,
    )


def _sdk_failure_category(sdk_result: object) -> str | None:
    """Classify the SDK's flattened failure without logging its error text."""

    if not isinstance(sdk_result, dict) or sdk_result.get("success") is not False:
        return None
    error = sdk_result.get("error")
    if not isinstance(error, str):
        return "malformed_failure"
    if sdk_result.get("toolkit_config") is None:
        return "toolkit_configuration"
    if error.startswith("Failed to create LLM instance '"):
        return "llm_creation"
    if error.startswith("Failed to instantiate toolkit '"):
        return "toolkit_instantiation"
    if error.startswith("Tool execution failed: "):
        return "tool_execution"
    if error.startswith("Method execution failed: "):
        return "method_execution"
    if error.startswith("Tool '") and " not found in toolkit '" in error:
        return "tool_not_found"
    if error.startswith("Tool '") and error.endswith(" is not callable"):
        return "tool_not_callable"
    if isinstance(sdk_result.get("debug_error"), str):
        return "toolkit_configuration"
    return "unclassified_failure"


class _IndexProgressOutput:
    """Thread-to-async bridge for one strictly ordered index output stream."""

    def __init__(
        self,
        *,
        verified: VerifiedWorkerCommand,
        initial_output: OutputSession,
        output_session_factory: Callable[[], OutputSession],
        clock_unix_millis: Callable[[], int],
        claim_handoff_watermark: int,
        output_ack_timeout_seconds: float,
        max_output_sessions: int,
    ) -> None:
        if claim_handoff_watermark < 0 or claim_handoff_watermark >= (1 << 64) - 1:
            raise InvalidInput("The index output handoff watermark is malformed.")
        self._verified = verified
        self._output = initial_output
        self._output_session_factory = output_session_factory
        self._clock = clock_unix_millis
        self._watermark = claim_handoff_watermark
        self._ack_timeout = output_ack_timeout_seconds
        self._max_sessions = max_output_sessions
        self._loop = asyncio.get_running_loop()
        self._loop_thread_id = threading.get_ident()
        self._publish_lock = asyncio.Lock()
        self._next_sequence = claim_handoff_watermark + 1
        self._started = False
        self._rejected_frame: output_pb2.ExecutionOutputFrameV1 | None = None
        self._fatal: _IndexProgressTransportFailure | None = None
        self._terminal_winner: WorkerError | None = None
        self._cancellation_check: Callable[[], None] | None = None

    @property
    def terminal_sequence(self) -> int:
        return self._next_sequence

    def bind_cancellation_check(self, check: Callable[[], None]) -> None:
        """Stop accepting new browser progress after durable cancellation."""

        self._cancellation_check = check

    def publish_from_sdk(self, event: node_event_pb2.NodeEventV1) -> None:
        """Block the synchronous callback until Main durably ACKs its event."""

        if threading.get_ident() == self._loop_thread_id:
            failure = _IndexProgressTransportFailure(
                "index progress callback ran on the output event-loop thread"
            )
            self._fatal = failure
            raise failure
        future = asyncio.run_coroutine_threadsafe(self._publish(event), self._loop)
        try:
            future.result()
        except Exception:
            # ``CurrentIndexNodeEventCallback`` records and re-raises this exact
            # failure after the SDK call. Do not turn an uncertain sequence into
            # an ordinary terminal result here.
            raise

    async def publish_from_delivery(
        self, event: node_event_pb2.NodeEventV1
    ) -> None:
        """Publish a lifecycle event built after the synchronous SDK returns."""

        await self._publish(event)

    async def _publish(self, event: node_event_pb2.NodeEventV1) -> None:
        async with self._publish_lock:
            if self._fatal is not None:
                raise self._fatal
            if self._terminal_winner is not None:
                # The first rejected frame is retained for terminal
                # replacement. Current synchronous SDKs can catch the first
                # callback exception and keep emitting status events; those
                # later callbacks must not turn an authoritative cancellation
                # into a retryable transport failure.
                return
            if self._cancellation_check is not None:
                try:
                    self._cancellation_check()
                except ExecutionCancelled as error:
                    # Desired-state cancellation can arrive between SDK
                    # callbacks, before a progress frame is sent. Remember it
                    # so a sync SDK that swallows this first exception cannot
                    # turn later callbacks into repeated failures.
                    self._terminal_winner = error
                    raise
            frame = build_node_event_output_frame(
                self._verified,
                event,
                sequence=self._next_sequence,
                occurred_at_unix_millis=_runtime_now(self._clock),
                claim_handoff_watermark=self._watermark,
            )
            for attempt in range(self._max_sessions):
                output = self._output
                try:
                    if not self._started:
                        await output.start()
                        self._started = True
                    if output.has_pending_replay:
                        if not output.replays(frame):
                            raise AuthorizationFailure(
                                "The durable output spool does not match this index progress event."
                            )
                    else:
                        await output.send(frame)
                    await output.wait_for_ack(frame.sequence, self._ack_timeout)
                    self._next_sequence += 1
                    return
                except Exception as error:
                    if _is_output_cancellation_winner(error):
                        self._rejected_frame = frame
                        self._terminal_winner = ExecutionCancelled()
                        await self._close_active()
                        raise self._terminal_winner from error
                    if _is_output_deadline_winner(error):
                        self._rejected_frame = frame
                        self._terminal_winner = DeadlineExceeded()
                        await self._close_active()
                        raise self._terminal_winner from error
                    await self._close_active()
                    if (
                        attempt + 1 >= self._max_sessions
                        or not isinstance(error, (RuntimeError, TimeoutError))
                        or not _reconnectable_output(error)
                    ):
                        failure = _IndexProgressTransportFailure(
                            "index progress output could not be durably acknowledged"
                        )
                        self._fatal = failure
                        raise failure from error
                    self._output = self._output_session_factory()
            raise AssertionError("bounded index progress session loop did not terminate")

    async def prepare_terminal(
        self,
        frame: output_pb2.ExecutionOutputFrameV1,
    ) -> tuple[OutputSession, bool]:
        """Return the active stream or atomically replace a rejected progress frame."""

        if self._fatal is not None:
            raise self._fatal
        rejected = self._rejected_frame
        if rejected is None:
            return self._output, self._started
        if int(frame.sequence) != int(rejected.sequence):
            raise AuthorizationFailure(
                "The terminal output does not replace the rejected progress sequence."
            )
        replacement = self._output_session_factory()
        pending = replacement.pending_replay_frame
        if (
            pending is None
            or not replacement.has_pending_replay
            or not replacement.replays(rejected)
        ):
            raise AuthorizationFailure(
                "The rejected index progress frame disappeared before terminal replacement."
            )
        await replacement.replace_pending_exact(rejected, frame)
        self._output = replacement
        self._started = False
        self._rejected_frame = None
        return replacement, False

    async def close(self) -> None:
        await self._close_active()

    async def _close_active(self) -> None:
        if self._started:
            await self._output.close()
            self._started = False


IndexClientContextFactory = Callable[
    [RuntimeExecutionClaim],
    Awaitable[EliteaClientContext],
]


class _ClaimLeaseMonitor:
    """Renew one unchanged claim fence and observe server-owned desired state."""

    def __init__(
        self,
        *,
        control: ControlPlane,
        receipt: control_pb2.ClaimReceiptV1,
        clock_unix_millis: Callable[[], int],
        interval_seconds: float,
    ) -> None:
        if interval_seconds <= 0:
            raise ValueError("lease polling interval must be positive")
        self._control = control
        self._identity = common_pb2.ExecutionIdentityV1.FromString(
            receipt.identity.SerializeToString(deterministic=True)
        )
        self._fence = common_pb2.ExecutionFenceV1.FromString(
            receipt.fence.SerializeToString(deterministic=True)
        )
        seed = (
            receipt.identity.SerializeToString(deterministic=True)
            + b"\x00"
            + receipt.fence.SerializeToString(deterministic=True)
            + b"\x00"
            + receipt.claim_id.encode("utf-8")
        )
        self._renewal_prefix = f"lease-renew:{hashlib.sha256(seed).hexdigest()}"
        self._clock = clock_unix_millis
        self._interval = interval_seconds
        self._lease_expires_at = int(receipt.lease_expires_at_unix_millis)
        self._renewal_sequence = 0
        self._failure: WorkerError | None = None
        self._cancellation: ExecutionCancelled | None = None
        self._state_changed = asyncio.Event()
        self._stop = asyncio.Event()
        self._poll_lock = asyncio.Lock()
        self._task: asyncio.Task[None] | None = None

    def start(self) -> None:
        if self._task is not None:
            raise RuntimeError("lease monitor is already started")
        self._task = asyncio.create_task(self._run(), name="elitea-claim-lease")

    async def check_now(self) -> None:
        self._raise_fatal_failure()
        async with self._poll_lock:
            self._raise_fatal_failure()
            await self._poll_and_record()
        self.raise_if_failed()

    def raise_if_failed(self) -> None:
        self._raise_fatal_failure()
        if self._cancellation is not None:
            raise self._cancellation

    async def wait_for_state_change(self) -> None:
        """Wait for server-owned cancellation or a fatal lease failure."""

        await self._state_changed.wait()
        self.raise_if_failed()

    def _raise_fatal_failure(self) -> None:
        if self._failure is not None:
            raise self._failure

    async def stop(self) -> None:
        self._stop.set()
        if self._task is not None:
            await asyncio.gather(self._task, return_exceptions=True)
            self._task = None

    async def _run(self) -> None:
        loop = asyncio.get_running_loop()
        next_poll = loop.time() + self._interval
        while not self._stop.is_set():
            delay = next_poll - loop.time()
            if delay > 0:
                try:
                    await asyncio.wait_for(self._stop.wait(), timeout=delay)
                    return
                except TimeoutError:
                    pass
            try:
                async with self._poll_lock:
                    self._raise_fatal_failure()
                    await self._poll_and_record()
            except WorkerError:
                return

            next_poll += self._interval
            now = loop.time()
            if next_poll <= now:
                skipped = int((now - next_poll) // self._interval) + 1
                next_poll += skipped * self._interval

    async def _poll_and_record(self) -> None:
        try:
            await self._poll_once()
        except ExecutionCancelled as error:
            # Desired cancellation is remembered for the next execution
            # boundary, but is not a lease failure. Continue renewing while a
            # synchronous callable may still be running or producing effects.
            self._cancellation = error
            self._state_changed.set()
        except WorkerError as error:
            self._failure = error
            self._state_changed.set()
            raise
        except Exception as error:
            failure = DependencyUnavailable()
            self._failure = failure
            raise failure from error

    async def _poll_once(self) -> None:
        self._renewal_sequence += 1
        renewal = await self._control.renew_lease(
            control_pb2.RenewLeaseRequestV1(
                identity=self._identity,
                fence=self._fence,
                idempotency_key=f"{self._renewal_prefix}:{self._renewal_sequence}",
            )
        )
        if renewal.HasField("rejection"):
            raise _worker_error_from_runtime(renewal.rejection)
        renewed_expiry = int(renewal.lease_expires_at_unix_millis)

        observed = await self._control.observe_desired_state(
            control_pb2.ObserveDesiredStateRequestV1(
                identity=self._identity,
                fence=self._fence,
            )
        )
        if observed.HasField("rejection"):
            raise _worker_error_from_runtime(observed.rejection)
        now = _runtime_now(self._clock)
        minimum_margin_millis = math.ceil(self._interval * 2_000)
        if (
            renewed_expiry < self._lease_expires_at
            or renewed_expiry - now < minimum_margin_millis
        ):
            raise AuthorizationFailure("The renewed claim lease is malformed or expired.")
        self._lease_expires_at = renewed_expiry
        _require_running_desired_state(int(renewal.desired_state))
        _require_running_desired_state(int(observed.desired_state))


async def _await_with_lease_state(
    operation: Awaitable[object],
    lease: _ClaimLeaseMonitor,
) -> object:
    """Race an async pre-SDK edge with durable desired-state observation.

    Stop wins ties deliberately. The operation has not entered the bounded
    synchronous SDK executor yet, so cancelling its task is cooperative and
    cannot abandon an SDK thread or release its reservation prematurely.
    """

    operation_task = asyncio.create_task(operation)
    state_task = asyncio.create_task(lease.wait_for_state_change())
    try:
        done, _ = await asyncio.wait(
            (operation_task, state_task),
            return_when=asyncio.FIRST_COMPLETED,
        )
        if state_task in done:
            # ``wait_for_state_change`` raises the authoritative typed cause.
            await state_task
            raise AssertionError("lease state change returned without a cause")
        return await operation_task
    finally:
        if not operation_task.done():
            operation_task.cancel()
        if not state_task.done():
            state_task.cancel()
        await asyncio.gather(operation_task, state_task, return_exceptions=True)


class ConfigurationValidationDeliveryProcessor:
    """Processes one Redis delivery without adding retry or business policy."""

    def __init__(
        self,
        *,
        supervisor: ExecutionRunner,
        handler: ConfigurationValidationHandler | None,
        control: ControlPlane,
        command_acker: CommandSettlementAcker,
        input_client: ScopedInputContentClient,
        input_request_builder: ClaimBoundInputRequestBuilder,
        output_session_factory: Callable[[], OutputSession],
        signed_command_authenticator: SignedCommandAuthenticator,
        workload_session_id: str,
        producer_id: str,
        clock_unix_millis: Callable[[], int],
        output_ack_timeout_seconds: float = 15.0,
        max_output_sessions: int = 2,
        lease_poll_interval_seconds: float = 10.0,
    ) -> None:
        if (
            not _bounded_text(workload_session_id)
            or not _bounded_text(producer_id)
            or output_ack_timeout_seconds <= 0
            or isinstance(max_output_sessions, bool)
            or not isinstance(max_output_sessions, int)
            or max_output_sessions < 1
            or max_output_sessions > 8
            or lease_poll_interval_seconds <= 0
        ):
            raise ValueError("delivered worker identity and limits are required")
        self._supervisor = supervisor
        self._handler = handler
        self._control = control
        self._command_acker = command_acker
        self._input_client = input_client
        self._input_request_builder = input_request_builder
        self._output_session_factory = output_session_factory
        self._signed_command_authenticator = signed_command_authenticator
        self._workload_session_id = workload_session_id
        self._producer_id = producer_id
        self._clock = clock_unix_millis
        self._output_ack_timeout = output_ack_timeout_seconds
        self._max_output_sessions = max_output_sessions
        self._lease_poll_interval = lease_poll_interval_seconds

    async def process(self, delivery: RedisCommandDelivery) -> DeliveryResult:
        try:
            return await self._supervisor.run(
                lambda: self._process_admitted(delivery)
            )
        except SyncExecutorAdmissionRejected:
            # Admission is decided before parsing, claim, input, output, or ACK.
            # The untouched Redis delivery can be retried on another worker.
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)

    async def _process_admitted(
        self,
        delivery: RedisCommandDelivery,
    ) -> DeliveryResult:
        signed, command = parse_and_verify_signed_command(
            delivery.signed_envelope,
            authenticator=self._signed_command_authenticator,
        )
        response = await self._control.claim_command(
            control_pb2.ClaimCommandRequestV1(
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                signed_command=signed,
            )
        )
        receipt = _claim_receipt(response)
        _validate_receipt_identity(receipt, command)
        disposition = int(receipt.disposition)
        if (
            disposition != control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK
            and receipt.HasField("retirement")
        ):
            raise InvalidInput(
                "The claim disposition has unexpected retirement material."
            )
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK:
            _validate_retired_receipt(receipt)
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(DeliveryDisposition.TERMINAL_REDELIVERY_ACKED)
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_OBSOLETE_ACK:
            _validate_obsolete_receipt(receipt)
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(DeliveryDisposition.TERMINAL_REDELIVERY_ACKED)
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_SETTLED_ACK:
            _require_no_recovery(receipt)
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(DeliveryDisposition.TERMINAL_REDELIVERY_ACKED)
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK:
            _require_no_recovery(receipt)
            _validate_active_fence(
                receipt,
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                now_unix_millis=_runtime_now(self._clock),
            )
            output = self._output_session_factory()
            pending = output.pending_replay_frame
            if output.has_pending_replay != (pending is not None):
                raise InvalidInput("The durable output spool state is inconsistent.")
            if pending is None:
                return DeliveryResult(DeliveryDisposition.OWNED_ELSEWHERE_NOACK)
            return await self._recover_pending_output(
                delivery=delivery,
                frame=pending,
                output=output,
                receipt=receipt,
                verified=_verified_claim_command(signed, command, receipt),
                allow_nonterminal=True,
            )
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK:
            _require_no_recovery(receipt)
            return DeliveryResult(DeliveryDisposition.RETRY_LATER_NOACK)
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK:
            _validate_recover_running_receipt(
                receipt,
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                now_unix_millis=_runtime_now(self._clock),
            )
            output = self._output_session_factory()
            pending = output.pending_replay_frame
            if output.has_pending_replay != (pending is not None):
                raise InvalidInput("The durable output spool state is inconsistent.")
            verified = _verified_claim_command(signed, command, receipt)
            if (
                receipt.desired_state
                == common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
            ):
                return await self._recover_cancelled_running(
                    delivery=delivery,
                    frame=pending,
                    output=output,
                    receipt=receipt,
                    verified=verified,
                )
            if pending is not None:
                return await self._recover_pending_output(
                    delivery=delivery,
                    frame=pending,
                    output=output,
                    receipt=receipt,
                    verified=verified,
                    allow_nonterminal=True,
                )
            return DeliveryResult(DeliveryDisposition.RECOVERY_REQUIRED_NOACK)
        if disposition == (
            control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
        ):
            _validate_recover_running_receipt(
                receipt,
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                now_unix_millis=_runtime_now(self._clock),
            )
            output = self._output_session_factory()
            pending = output.pending_replay_frame
            if output.has_pending_replay != (pending is not None):
                raise InvalidInput("The durable output spool state is inconsistent.")
            verified = _verified_claim_command(signed, command, receipt)
            if pending is not None:
                if (
                    self._validate_pending_output(
                        pending,
                        command=verified.command,
                        receipt=receipt,
                    )
                    and not _pending_output_binding_matches(pending, receipt)
                    and receipt.desired_state
                    == common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING
                    and int(receipt.claim_handoff_watermark)
                    == int(pending.sequence) - 1
                ):
                    return await self._recover_ambiguous_running(
                        delivery=delivery,
                        output=output,
                        receipt=receipt,
                        verified=verified,
                        replace_pending=pending,
                    )
                return await self._recover_pending_output(
                    delivery=delivery,
                    frame=pending,
                    output=output,
                    receipt=receipt,
                    verified=verified,
                    allow_nonterminal=True,
                )
            return await self._recover_ambiguous_running(
                delivery=delivery,
                output=output,
                receipt=receipt,
                verified=verified,
            )
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK:
            now = _runtime_now(self._clock)
            _validate_active_fence(
                receipt,
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                now_unix_millis=now,
            )
            recovery = _terminal_ack_recovery(receipt)
            settlement = await self._control.prepare_settlement(
                control_pb2.PrepareSettlementRequestV1(
                    identity=receipt.identity,
                    fence=receipt.fence,
                    proposal=recovery.proposal,
                    proposal_digest=recovery.proposal_digest,
                    idempotency_key=recovery.idempotency_key,
                )
            )
            receipt_id = _settlement_receipt(
                settlement,
                expected_outcome=int(recovery.proposal.requested_outcome),
            )
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(
                DeliveryDisposition.RECOVERED_TERMINAL_SETTLED_ACKED,
                settlement_receipt_id=receipt_id,
            )
        if disposition == control_pb2.CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT:
            _validate_active_fence(
                receipt,
                workload_session_id=self._workload_session_id,
                producer_id=self._producer_id,
                now_unix_millis=_runtime_now(self._clock),
            )
            recovery = _prepared_settlement_recovery(receipt)
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(
                DeliveryDisposition.RECOVERED_SETTLEMENT_ACKED,
                settlement_receipt_id=recovery.settlement_receipt_id,
            )
        if disposition != control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED:
            raise InvalidInput("The claim disposition is malformed.")

        _require_no_recovery(receipt)
        claim_now = _runtime_now(self._clock)
        accepted = self._accept_claim(
            signed=signed,
            command=command,
            receipt=receipt,
            workload_session_id=self._workload_session_id,
            producer_id=self._producer_id,
            now_unix_millis=claim_now,
        )

        output = self._output_session_factory()
        pending = output.pending_replay_frame
        if output.has_pending_replay != (pending is not None):
            raise InvalidInput("The durable output spool state is inconsistent.")
        if pending is not None:
            return await self._recover_pending_output(
                delivery=delivery,
                frame=pending,
                output=output,
                receipt=receipt,
                verified=accepted.verified,
                allow_nonterminal=False,
            )

        progress = self._new_progress_output(accepted, output)
        lease = _ClaimLeaseMonitor(
            control=self._control,
            receipt=receipt,
            clock_unix_millis=self._clock,
            interval_seconds=self._lease_poll_interval,
        )
        reservation = await self._reserve_execution_capacity()
        try:
            if not await self._begin_execution(receipt):
                return DeliveryResult(DeliveryDisposition.RECOVERY_REQUIRED_NOACK)
            lease.start()
            if progress is not None:
                progress.bind_cancellation_check(lease.raise_if_failed)
            cancellation: ExecutionCancelled | None = None
            try:
                await lease.check_now()
            except ExecutionCancelled as error:
                cancellation = error

            if cancellation is not None:
                outcome = cancellation
            else:
                try:
                    _raise_if_execution_deadline_exceeded(command, self._clock)
                    self._validate_capability_identity(command)
                    resolved_input = await self._resolve_inputs_with_lease(
                        accepted,
                        receipt=receipt,
                        lease=lease,
                    )
                    _raise_if_execution_deadline_exceeded(command, self._clock)
                except WorkerError as error:
                    outcome = error
                else:
                    try:
                        lease.raise_if_failed()
                    except ExecutionCancelled as error:
                        outcome = error
                    else:
                        try:
                            _raise_if_execution_deadline_exceeded(
                                command,
                                self._clock,
                            )
                            outcome = await self._execute_resolved(
                                accepted,
                                resolved_input,
                                receipt=receipt,
                                progress=progress,
                            )
                            # A synchronous SDK call cannot be cancelled safely.
                            # Retain claim ownership until it exits, then prevent a
                            # result completed after the durable command deadline
                            # from becoming the first output dispatch.
                            _raise_if_execution_deadline_exceeded(
                                command,
                                self._clock,
                            )
                        except SyncExecutorAdmissionRejected:
                            # No business callable was submitted. Leave the claim
                            # and Redis delivery unacknowledged so the lease can
                            # expire and the scheduler can admit it elsewhere.
                            return DeliveryResult(
                                DeliveryDisposition.RETRY_LATER_NOACK
                            )
                        except WorkerError as error:
                            outcome = error

                try:
                    await lease.check_now()
                except ExecutionCancelled as error:
                    outcome = error

            occurred_at = _runtime_now(self._clock)
            # This is the output linearization boundary. After the first
            # dispatch, stable spool/output identity must win even if the wall
            # clock later crosses the command deadline.
            if (
                not isinstance(outcome, ExecutionCancelled)
                and occurred_at >= int(command.deadline_unix_millis)
            ):
                outcome = DeadlineExceeded()
            frame = build_output_frame(
                accepted.verified,
                outcome,
                occurred_at_unix_millis=occurred_at,
                claim_handoff_watermark=accepted.claim_handoff_watermark,
                sequence=(progress.terminal_sequence if progress is not None else 1),
            )
            terminal_output = output
            terminal_output_started = False
            if progress is not None:
                terminal_output, terminal_output_started = (
                    await progress.prepare_terminal(frame)
                )
            frame = await self._publish_with_terminal_linearization(
                frame,
                verified=accepted.verified,
                claim_handoff_watermark=accepted.claim_handoff_watermark,
                initial_output=terminal_output,
                initial_output_started=terminal_output_started,
            )
            # Publication returned only after a bound durable ACK. Do not apply a
            # later wall-clock deadline here: settlement must preserve and finish
            # that immutable output rather than synthesize a replacement.
            receipt_id = await self._prepare_frame_settlement(frame)
            await self._command_acker.ack_after_settlement(
                delivery, command.idempotency_key
            )
            return DeliveryResult(
                DeliveryDisposition.EXECUTED_SETTLED_ACKED,
                output_frame=frame,
                settlement_receipt_id=receipt_id,
                execution_error=outcome if isinstance(outcome, WorkerError) else None,
            )
        finally:
            if reservation is not None:
                reservation.release()
            if progress is not None:
                await progress.close()
            await lease.stop()

    async def _reserve_execution_capacity(
        self,
    ) -> SyncExecutorReservation | None:
        return None

    async def _begin_execution(
        self,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> bool:
        _ = receipt
        return True

    def _new_progress_output(
        self,
        accepted: _AcceptedClaim | _AcceptedIndexClaim | _AcceptedAgentClaim,
        output: OutputSession,
    ) -> _IndexProgressOutput | None:
        _ = accepted, output
        return None

    def _accept_claim(
        self,
        *,
        signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
        command: command_pb2.WorkerCommandV1,
        receipt: control_pb2.ClaimReceiptV1,
        workload_session_id: str,
        producer_id: str,
        now_unix_millis: int,
    ) -> _AcceptedClaim:
        return _accepted_claim(
            signed=signed,
            command=command,
            receipt=receipt,
            workload_session_id=workload_session_id,
            producer_id=producer_id,
            now_unix_millis=now_unix_millis,
        )

    def _validate_capability_identity(
        self,
        command: command_pb2.WorkerCommandV1,
    ) -> None:
        if self._handler is None:
            raise InternalFailure()
        selected = command.configuration_validation
        self._handler.validate_binding(
            configuration_type=selected.configuration_type,
            catalog_revision=selected.catalog_revision,
            catalog_digest=bytes(selected.catalog_digest.value),
            schema_id=selected.schema_id,
            schema_revision=selected.schema_revision,
            schema_digest=bytes(selected.schema_digest.value),
        )

    async def _resolve_inputs(
        self,
        accepted: _AcceptedClaim,
        *,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> bytes:
        grant = self._input_request_builder.build(
            _claim_bound_reference(
                accepted.content,
                receipt=receipt,
                claim_id=accepted.claim_id,
            )
        )
        return await self._input_client.fetch(grant)

    async def _resolve_inputs_with_lease(
        self,
        accepted: _AcceptedClaim | _AcceptedIndexClaim | _AcceptedAgentClaim,
        *,
        receipt: control_pb2.ClaimReceiptV1,
        lease: _ClaimLeaseMonitor,
    ) -> object:
        """Cancel only pre-SDK asynchronous edges when the server says Stop.

        The synchronous SDK bridge deliberately is not wrapped here: Python
        cannot safely stop a running SDK thread. The lease remains live until
        that callable exits and its terminal cancellation frame is fenced.
        """

        return await _await_with_lease_state(
            self._resolve_inputs(accepted, receipt=receipt),
            lease,
        )

    async def _execute_resolved(
        self,
        accepted: _AcceptedClaim,
        resolved_input: object,
        *,
        receipt: control_pb2.ClaimReceiptV1,
        progress: _IndexProgressOutput | None,
    ) -> Any:
        if (
            self._handler is None
            or not isinstance(resolved_input, bytes)
            or progress is not None
        ):
            raise InternalFailure()
        settings = parse_settings_json(resolved_input)
        request = validation_request_from(
            accepted.verified,
            input_bundle_id=receipt.input_bundle.input_bundle_id,
            input_bundle_digest=bytes(receipt.input_bundle_ref.digest.value),
            settings_entry_version=accepted.content.immutable_version,
            settings_content_digest=accepted.content.digest,
            settings=settings,
        )
        return await self._supervisor.run_sync(
            self._handler.execute,
            request,
        )

    def _validate_pending_output(
        self,
        frame: output_pb2.ExecutionOutputFrameV1,
        *,
        command: command_pb2.WorkerCommandV1,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> bool:
        return _validate_pending_output(frame, command=command, receipt=receipt)

    async def _recover_pending_output(
        self,
        *,
        delivery: RedisCommandDelivery,
        frame: output_pb2.ExecutionOutputFrameV1,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
        allow_nonterminal: bool,
    ) -> DeliveryResult:
        terminal = self._validate_pending_output(
            frame,
            command=verified.command,
            receipt=receipt,
        )
        if not _pending_output_binding_matches(frame, receipt):
            if (
                not terminal
                and int(receipt.claim_handoff_watermark) >= int(frame.sequence)
            ):
                # ClaimCommand is authenticated control-plane state. Its
                # contiguous handoff watermark proves this stale-fence event is
                # already durable without trusting or re-sending local bytes.
                await output.reconcile_pending_through(
                    int(receipt.claim_handoff_watermark)
                )
                return DeliveryResult(
                    DeliveryDisposition.RECOVERY_REQUIRED_NOACK,
                    output_frame=frame,
                )
            raise AuthorizationFailure(
                "The durable output spool uses a different claim fence; "
                "server-side recovery is required."
            )
        if terminal:
            return await self._recover_local_output(
                delivery=delivery,
                frame=frame,
                output=output,
                receipt=receipt,
                verified=verified,
            )
        if not allow_nonterminal:
            raise AuthorizationFailure(
                "A durable NodeEvent requires running execution recovery."
            )
        return await self._recover_local_node_event(
            frame=frame,
            output=output,
            receipt=receipt,
        )

    async def _recover_local_node_event(
        self,
        *,
        frame: output_pb2.ExecutionOutputFrameV1,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> DeliveryResult:
        lease = _ClaimLeaseMonitor(
            control=self._control,
            receipt=receipt,
            clock_unix_millis=self._clock,
            interval_seconds=self._lease_poll_interval,
        )
        lease.start()
        try:
            # A non-terminal replay has no settlement proposal and never ACKs
            # the Redis command. Future redelivery remains recovery-only, so
            # the SDK cannot be invoked twice.
            await self._publish_output(frame, initial_output=output)
            return DeliveryResult(
                DeliveryDisposition.RECOVERY_REQUIRED_NOACK,
                output_frame=frame,
            )
        finally:
            await lease.stop()

    async def _recover_cancelled_running(
        self,
        *,
        delivery: RedisCommandDelivery,
        frame: output_pb2.ExecutionOutputFrameV1 | None,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
    ) -> DeliveryResult:
        # Cancellation changes desired state, not current lease authority. Keep
        # the exact recovered fence alive while replaying a bounded nonterminal
        # frame and settling its sole cancellation terminal.
        lease = _ClaimLeaseMonitor(
            control=self._control,
            receipt=receipt,
            clock_unix_millis=self._clock,
            interval_seconds=self._lease_poll_interval,
        )
        lease.start()
        try:
            return await self._recover_cancelled_running_under_lease(
                delivery=delivery,
                frame=frame,
                output=output,
                receipt=receipt,
                verified=verified,
            )
        finally:
            await lease.stop()

    async def _recover_ambiguous_running(
        self,
        *,
        delivery: RedisCommandDelivery,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
        replace_pending: output_pb2.ExecutionOutputFrameV1 | None = None,
    ) -> DeliveryResult:
        """Settle an ambiguous invocation recovery without invoking the SDK.

        Main issues this recovery-only fence only after an invocation was
        durably authorized and its predecessor lease expired. Neither absence
        nor an unsigned old-fence local terminal proves the SDK outcome.
        """

        failure = AmbiguousExecutionRecovery()
        frame = build_output_frame(
            verified,
            failure,
            occurred_at_unix_millis=_runtime_now(self._clock),
            claim_handoff_watermark=int(receipt.claim_handoff_watermark),
            sequence=int(receipt.claim_handoff_watermark) + 1,
        )
        self._validate_pending_output(
            frame,
            command=verified.command,
            receipt=receipt,
        )
        if replace_pending is not None:
            await output.replace_pending_ambiguous_recovery(
                replace_pending,
                frame,
            )
        lease = _ClaimLeaseMonitor(
            control=self._control,
            receipt=receipt,
            clock_unix_millis=self._clock,
            interval_seconds=self._lease_poll_interval,
        )
        lease.start()
        try:
            frame = await self._publish_with_terminal_linearization(
                frame,
                verified=verified,
                claim_handoff_watermark=int(receipt.claim_handoff_watermark),
                initial_output=output,
            )
            receipt_id = await self._prepare_frame_settlement(frame)
            await self._command_acker.ack_after_settlement(
                delivery, verified.command.idempotency_key
            )
            execution_error: WorkerError = failure
            if _is_cancellation_frame(frame):
                execution_error = ExecutionCancelled()
            return DeliveryResult(
                DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED,
                output_frame=frame,
                settlement_receipt_id=receipt_id,
                execution_error=execution_error,
            )
        finally:
            await lease.stop()

    async def _recover_cancelled_running_under_lease(
        self,
        *,
        delivery: RedisCommandDelivery,
        frame: output_pb2.ExecutionOutputFrameV1 | None,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
    ) -> DeliveryResult:
        """Dispose a cancelled recovered execution without re-entering SDK work.

        ``RECOVER_RUNNING_NOACK`` is normally a no-authority recovery state.
        When its server-owned desired state is CANCELLED, it is instead an
        authenticated recovery authority for exactly one terminal cancellation.
        No input, client context, executor reservation or BeginExecution call
        is permitted on this path.
        """

        watermark = int(receipt.claim_handoff_watermark)
        terminal_sequence = watermark + 1
        if frame is not None:
            terminal = self._validate_pending_output(
                frame,
                command=verified.command,
                receipt=receipt,
            )
            if terminal:
                if not _pending_output_binding_matches(frame, receipt):
                    if watermark != int(frame.sequence) - 1:
                        raise AuthorizationFailure(
                            "The durable terminal output spool is not contiguous "
                            "with the cancellation recovery watermark."
                        )
                    # Main has authenticated cancellation under a replacement
                    # fence and has not accepted this old-fence terminal. The
                    # unsigned local result cannot win across that fence.
                    # Replace it atomically with the canonical cancellation at
                    # the same sequence; never re-enter the SDK.
                    return await self._settle_cancelled_recovery(
                        delivery=delivery,
                        receipt=receipt,
                        verified=verified,
                        sequence=int(frame.sequence),
                        initial_output=output,
                        replace_pending=frame,
                        allow_cancelled_fence_rebind=True,
                    )
                return await self._recover_local_output(
                    delivery=delivery,
                    frame=frame,
                    output=output,
                    receipt=receipt,
                    verified=verified,
                )
            if not _pending_output_binding_matches(frame, receipt):
                if watermark >= int(frame.sequence):
                    await output.reconcile_pending_through(watermark)
                else:
                    # The old-fence progress frame was never accepted by Main:
                    # the authenticated handoff watermark is still behind it.
                    # It is non-terminal and therefore cannot decide the
                    # outcome. Atomically replace the local-only frame with
                    # the replacement fence's first contiguous cancellation;
                    # do not publish, replay, or otherwise revive old SDK
                    # progress under a new fence.
                    return await self._settle_cancelled_recovery(
                        delivery=delivery,
                        receipt=receipt,
                        verified=verified,
                        # The server did not accept this old-fence progress,
                        # but its local spool sequence remains the durable CAS
                        # key. The terminal result may use that sequence;
                        # only progress projection requires contiguity.
                        sequence=int(frame.sequence),
                        initial_output=output,
                        replace_pending=frame,
                        allow_cancelled_fence_rebind=True,
                    )
            else:
                try:
                    await self._publish_output(frame, initial_output=output)
                except (RuntimeError, OutputCancellationWon, OutputDeadlineWon) as error:
                    if not _is_output_cancellation_winner(error):
                        raise
                    # Main rejected this exact non-terminal frame because
                    # cancellation already won. Replace its pending bytes with
                    # the canonical terminal at the same contiguous sequence.
                    return await self._settle_cancelled_recovery(
                        delivery=delivery,
                        receipt=receipt,
                        verified=verified,
                        sequence=int(frame.sequence),
                        initial_output=output,
                        replace_pending=frame,
                    )
                terminal_sequence = int(frame.sequence) + 1

        return await self._settle_cancelled_recovery(
            delivery=delivery,
            receipt=receipt,
            verified=verified,
            sequence=terminal_sequence,
            initial_output=self._output_session_factory(),
        )

    async def _settle_cancelled_recovery(
        self,
        *,
        delivery: RedisCommandDelivery,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
        sequence: int,
        initial_output: OutputSession,
        replace_pending: output_pb2.ExecutionOutputFrameV1 | None = None,
        allow_cancelled_fence_rebind: bool = False,
    ) -> DeliveryResult:
        if sequence <= int(receipt.claim_handoff_watermark):
            raise InvalidInput("The recovered cancellation sequence is malformed.")
        frame = build_output_frame(
            verified,
            ExecutionCancelled(),
            occurred_at_unix_millis=_runtime_now(self._clock),
            claim_handoff_watermark=int(receipt.claim_handoff_watermark),
            sequence=sequence,
        )
        if replace_pending is not None:
            if (
                not initial_output.has_pending_replay
                or initial_output.pending_replay_frame != replace_pending
            ):
                raise AuthorizationFailure(
                    "The rejected progress frame disappeared before cancellation recovery."
                )
            if allow_cancelled_fence_rebind:
                await initial_output.replace_pending_cancelled_recovery(
                    replace_pending, frame
                )
            else:
                await initial_output.replace_pending_exact(replace_pending, frame)
        frame = await self._publish_with_terminal_linearization(
            frame,
            verified=verified,
            claim_handoff_watermark=int(receipt.claim_handoff_watermark),
            initial_output=initial_output,
        )
        receipt_id = await self._prepare_frame_settlement(frame)
        await self._command_acker.ack_after_settlement(
            delivery, verified.command.idempotency_key
        )
        return DeliveryResult(
            DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED,
            output_frame=frame,
            settlement_receipt_id=receipt_id,
            execution_error=ExecutionCancelled(),
        )

    async def _recover_local_output(
        self,
        *,
        delivery: RedisCommandDelivery,
        frame: output_pb2.ExecutionOutputFrameV1,
        output: OutputSession,
        receipt: control_pb2.ClaimReceiptV1,
        verified: VerifiedWorkerCommand,
    ) -> DeliveryResult:
        lease = _ClaimLeaseMonitor(
            control=self._control,
            receipt=receipt,
            clock_unix_millis=self._clock,
            interval_seconds=self._lease_poll_interval,
        )
        lease.start()
        try:
            # Attempt the exact spooled frame before consulting a later desired
            # state. Its durable ACK proves that output already won; only the
            # frame-bound ErrOutputCancelled response may authorize replacement.
            frame = await self._publish_with_terminal_linearization(
                frame,
                verified=verified,
                claim_handoff_watermark=int(receipt.claim_handoff_watermark),
                initial_output=output,
            )
            receipt_id = await self._prepare_frame_settlement(frame)
            await self._command_acker.ack_after_settlement(
                delivery, verified.command.idempotency_key
            )
            return DeliveryResult(
                DeliveryDisposition.RECOVERED_LOCAL_OUTPUT_SETTLED_ACKED,
                output_frame=frame,
                settlement_receipt_id=receipt_id,
            )
        finally:
            await lease.stop()

    async def _publish_with_terminal_linearization(
        self,
        frame: output_pb2.ExecutionOutputFrameV1,
        *,
        verified: VerifiedWorkerCommand,
        claim_handoff_watermark: int,
        initial_output: OutputSession,
        initial_output_started: bool = False,
    ) -> output_pb2.ExecutionOutputFrameV1:
        try:
            await self._publish_output(
                frame,
                initial_output=initial_output,
                initial_output_started=initial_output_started,
            )
            return frame
        except (RuntimeError, OutputCancellationWon, OutputDeadlineWon) as error:
            if _is_output_cancellation_winner(error):
                if _is_cancellation_frame(frame):
                    raise
                replacement_outcome: WorkerError = ExecutionCancelled()
            elif _is_output_deadline_winner(error):
                if _is_deadline_frame(frame):
                    raise
                replacement_outcome = DeadlineExceeded()
            else:
                raise

        replacement = build_output_frame(
            verified,
            replacement_outcome,
            # Preserve the original terminal occurrence time so a crash/replay
            # after the atomic spool CAS remains byte-for-byte identical.
            occurred_at_unix_millis=int(frame.occurred_at_unix_millis),
            claim_handoff_watermark=claim_handoff_watermark,
            sequence=int(frame.sequence),
        )
        replacement_output = self._output_session_factory()
        pending = replacement_output.pending_replay_frame
        if pending is None or not replacement_output.has_pending_replay:
            raise AuthorizationFailure(
                "The durable output spool disappeared before terminal replacement."
            )
        await replacement_output.replace_pending_exact(frame, replacement)
        await self._publish_output(replacement, initial_output=replacement_output)
        return replacement

    async def _prepare_frame_settlement(
        self,
        frame: output_pb2.ExecutionOutputFrameV1,
    ) -> str:
        proposal_raw = frame.settlement_proposal.SerializeToString(deterministic=True)
        settlement = await self._control.prepare_settlement(
            control_pb2.PrepareSettlementRequestV1(
                identity=frame.identity,
                fence=frame.fence,
                proposal=frame.settlement_proposal,
                proposal_digest=common_pb2.DigestV1(
                    algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
                    value=hashlib.sha256(proposal_raw).digest(),
                ),
                idempotency_key=frame.settlement_proposal.prepare_idempotency_key,
            )
        )
        return _settlement_receipt(
            settlement,
            expected_outcome=int(frame.settlement_proposal.requested_outcome),
        )

    async def _publish_output(
        self,
        frame: output_pb2.ExecutionOutputFrameV1,
        *,
        initial_output: OutputSession | None = None,
        initial_output_started: bool = False,
    ) -> None:
        for attempt in range(self._max_output_sessions):
            output = (
                initial_output
                if attempt == 0 and initial_output is not None
                else self._output_session_factory()
            )
            already_started = (
                attempt == 0
                and initial_output is not None
                and initial_output_started
            )
            if output.has_pending_replay and not output.replays(frame):
                raise AuthorizationFailure(
                    "The durable output spool does not match this execution frame."
                )
            try:
                if not already_started:
                    await output.start()
                if not output.has_pending_replay:
                    await output.send(frame)
                await output.wait_for_ack(frame.sequence, self._output_ack_timeout)
                return
            except (RuntimeError, TimeoutError) as exc:
                if attempt + 1 >= self._max_output_sessions or not _reconnectable_output(exc):
                    raise
            finally:
                await output.close()
        raise AssertionError("bounded output session loop did not terminate")


class IndexIngestDeliveryProcessor(ConfigurationValidationDeliveryProcessor):
    """Index-specific inputs and SDK call over the shared durable lifecycle."""

    def __init__(
        self,
        *,
        supervisor: ExecutionRunner,
        client_context_factory: IndexClientContextFactory | None,
        control: ControlPlane,
        command_acker: CommandSettlementAcker,
        input_client: ScopedInputContentClient,
        input_request_builder: ClaimBoundInputRequestBuilder,
        output_session_factory: Callable[[], OutputSession],
        signed_command_authenticator: SignedCommandAuthenticator,
        workload_session_id: str,
        producer_id: str,
        clock_unix_millis: Callable[[], int],
        output_ack_timeout_seconds: float = 15.0,
        max_output_sessions: int = 2,
        lease_poll_interval_seconds: float = 10.0,
    ) -> None:
        super().__init__(
            supervisor=supervisor,
            handler=None,
            control=control,
            command_acker=command_acker,
            input_client=input_client,
            input_request_builder=input_request_builder,
            output_session_factory=output_session_factory,
            signed_command_authenticator=signed_command_authenticator,
            workload_session_id=workload_session_id,
            producer_id=producer_id,
            clock_unix_millis=clock_unix_millis,
            output_ack_timeout_seconds=output_ack_timeout_seconds,
            max_output_sessions=max_output_sessions,
            lease_poll_interval_seconds=lease_poll_interval_seconds,
        )
        self._client_context_factory = client_context_factory

    async def _reserve_execution_capacity(self) -> SyncExecutorReservation:
        return await self._supervisor.reserve_sync()

    async def _begin_execution(
        self,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> bool:
        response = await self._control.begin_execution(
            control_pb2.BeginExecutionRequestV1(
                identity=receipt.identity,
                fence=receipt.fence,
            )
        )
        if response.HasField("rejection"):
            if int(response.disposition) != (
                control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_UNSPECIFIED
            ):
                raise InvalidInput("The begin execution response is ambiguous.")
            raise _worker_error_from_runtime(response.rejection)
        disposition = int(response.disposition)
        if disposition == control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW:
            return True
        if (
            disposition
            == control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED
        ):
            return False
        raise InvalidInput("The begin execution disposition is malformed.")

    def _new_progress_output(
        self,
        accepted: _AcceptedClaim | _AcceptedIndexClaim,
        output: OutputSession,
    ) -> _IndexProgressOutput | None:
        if not isinstance(accepted, _AcceptedIndexClaim):
            raise InternalFailure()
        return _IndexProgressOutput(
            verified=accepted.verified,
            initial_output=output,
            output_session_factory=self._output_session_factory,
            clock_unix_millis=self._clock,
            claim_handoff_watermark=accepted.claim_handoff_watermark,
            output_ack_timeout_seconds=self._output_ack_timeout,
            max_output_sessions=self._max_output_sessions,
        )

    def _accept_claim(
        self,
        *,
        signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
        command: command_pb2.WorkerCommandV1,
        receipt: control_pb2.ClaimReceiptV1,
        workload_session_id: str,
        producer_id: str,
        now_unix_millis: int,
    ) -> _AcceptedIndexClaim:
        return _accepted_index_claim(
            signed=signed,
            command=command,
            receipt=receipt,
            workload_session_id=workload_session_id,
            producer_id=producer_id,
            now_unix_millis=now_unix_millis,
        )

    def _validate_capability_identity(
        self,
        command: command_pb2.WorkerCommandV1,
    ) -> None:
        if (
            command.capability_id != INDEX_INGEST_CAPABILITY_ID
            or command.capability_version != INDEX_INGEST_CAPABILITY_VERSION
            or command.command_type
            != command_pb2.WORKER_COMMAND_TYPE_V1_INDEX_INGEST
            or command.WhichOneof("capability_command") != "index_ingest"
        ):
            raise UnsupportedCapability()

    async def _resolve_inputs(
        self,
        accepted: _AcceptedIndexClaim,
        *,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> _ResolvedIndexInputs:
        resolved: dict[str, ResolvedIndexIngestInput] = {}
        for entry in accepted.entries:
            grant = self._input_request_builder.build(
                _claim_bound_reference(
                    entry.content,
                    receipt=receipt,
                    claim_id=accepted.claim_id,
                )
            )
            if entry.semantic_role == _INDEX_EMBEDDING_BINDING_ROLE:
                # The binding is intentionally non-secret and immutable. Fetch
                # the admitted source bytes directly so the content client
                # verifies their exact length and SHA-256, rather than accepting
                # a materialized derivative.
                raw = await self._input_client.fetch(grant)
            else:
                raw = await self._input_client.fetch_materialized(
                    grant,
                    source_immutable_version=entry.immutable_version,
                )
            resolved[entry.semantic_role] = ResolvedIndexIngestInput(
                binding=IndexIngestInputBinding(
                    entry_id=entry.entry_id,
                    immutable_version=entry.immutable_version,
                    content_digest=entry.content.digest,
                ),
                value=parse_json_value(raw),
            )
        embedding = resolve_embedding_binding(
            resolved[_INDEX_TOOLKIT_CONFIGURATION_ROLE],
            resolved.get(_INDEX_EMBEDDING_BINDING_ROLE),
        )
        factory = self._client_context_factory
        if factory is None:
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        claim = RuntimeExecutionClaim(
            execution_id=receipt.identity.execution_id,
            generation=int(receipt.identity.generation),
            claim_id=accepted.claim_id,
            fence_token=bytes(receipt.fence.fence_token),
            resource_project_id=receipt.identity.resource_project_id,
        )
        try:
            context = await factory(claim)
            if str(context.project_id) != receipt.identity.resource_project_id:
                raise AuthorizationFailure(
                    "The claim-scoped SDK project identity does not match the execution."
                )
        except WorkerError:
            raise
        except Exception as error:
            _emit_index_internal_failure(
                stage="input_context",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None
        return _ResolvedIndexInputs(
            toolkit_configuration=resolved[_INDEX_TOOLKIT_CONFIGURATION_ROLE],
            tool_parameters=resolved[_INDEX_TOOL_PARAMETERS_ROLE],
            llm_model=resolved.get(_INDEX_LLM_MODEL_ROLE),
            llm_configuration=resolved.get(_INDEX_LLM_CONFIGURATION_ROLE),
            mcp_tokens=resolved.get(_INDEX_MCP_TOKENS_ROLE),
            embedding_binding=(
                None if embedding is None else embedding.input
            ),
            client_context=context,
        )

    async def _execute_resolved(
        self,
        accepted: _AcceptedIndexClaim,
        resolved_input: object,
        *,
        receipt: control_pb2.ClaimReceiptV1,
        progress: _IndexProgressOutput | None,
    ) -> indexing_pb2.IndexIngestResultV1:
        if (
            not isinstance(resolved_input, _ResolvedIndexInputs)
            or progress is None
        ):
            raise InternalFailure()
        command = accepted.verified.command
        callback = CurrentIndexNodeEventCallback(
            CurrentIndexNodeEventContext(
                stream_id=command.index_ingest.client_stream_id,
                task_id=command.execution_id,
                initiator=command.index_ingest.initiator,
                project_id=_current_numeric_identity(command.resource_project_id),
                user_id=_current_user_identity(command.principal_ref),
                toolkit_id=_current_toolkit_id(
                    resolved_input.toolkit_configuration.value
                ),
                index_name=_current_index_name(
                    resolved_input.tool_parameters.value
                ),
                message_id=command.index_ingest.client_message_id,
                sio_event=command.index_ingest.sio_event,
                display_name=_current_toolkit_display_name(
                    resolved_input.toolkit_configuration.value
                ),
            ),
            progress.publish_from_sdk,
        )
        request = request_from(
            accepted.verified.command.index_ingest,
            input_bundle_id=receipt.input_bundle.input_bundle_id,
            input_bundle_digest=bytes(receipt.input_bundle_ref.digest.value),
            toolkit_configuration=resolved_input.toolkit_configuration,
            tool_parameters=resolved_input.tool_parameters,
            llm_model=resolved_input.llm_model,
            llm_configuration=resolved_input.llm_configuration,
            mcp_tokens=resolved_input.mcp_tokens,
            embedding_binding=resolved_input.embedding_binding,
            runtime_config={
                "callbacks": [callback],
                "metadata": {
                    "initiator": command.index_ingest.initiator,
                    "tool_name": "index_data",
                    "display_name": _current_toolkit_display_name(
                        resolved_input.toolkit_configuration.value
                    ),
                },
            },
        )
        try:
            handler = IndexIngestHandler(
                EliteaSdkIndexingAdapter.from_context(resolved_input.client_context),
                self._supervisor,
            )
            await self._authorize_invocation(receipt)
            result = await handler.execute(request)
            callback.raise_if_failed()
            try:
                projected = bind_result_summary(result)
            except InternalFailure as error:
                await _publish_missing_index_failure_status(callback, progress)
                tool_event = callback.finish_tool(success=False)
                if tool_event is not None:
                    await progress.publish_from_delivery(tool_event)
                callback.raise_if_failed()
                _emit_index_internal_failure(
                    stage="result_projection",
                    execution_id=receipt.identity.execution_id,
                    error=error,
                    sdk_failure_category=_sdk_failure_category(result.sdk_result),
                )
                raise
            if (
                projected.result_summary.status
                == indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
            ):
                await _publish_index_summary_correction(
                    callback,
                    progress,
                    "failed",
                )
            elif (
                projected.result_summary.status
                == indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED
            ):
                await _publish_index_summary_correction(
                    callback,
                    progress,
                    "partly_indexed",
                )
            projected = bind_result_summary(result, callback.terminal_status())
            tool_event = callback.finish_tool(
                success=projected.result_summary.status
                != indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
            )
            if tool_event is not None:
                await progress.publish_from_delivery(tool_event)
            callback.raise_if_failed()
            return projected
        except _IndexProgressTransportFailure:
            raise
        except WorkerError as error:
            if not isinstance(error, ExecutionCancelled):
                await _publish_missing_index_failure_status(callback, progress)
            tool_event = callback.finish_tool(success=False)
            if tool_event is not None:
                await progress.publish_from_delivery(tool_event)
            callback.raise_if_failed()
            raise
        except Exception as error:
            await _publish_missing_index_failure_status(callback, progress)
            tool_event = callback.finish_tool(success=False)
            if tool_event is not None:
                await progress.publish_from_delivery(tool_event)
            callback.raise_if_failed()
            if isinstance(error, SdkBudgetExceeded):
                # RuntimeErrorV1 has no budget-specific wire code yet. Preserve
                # the policy failure as the existing non-retryable,
                # canonical RESOURCE_EXHAUSTED contract without exposing the
                # SDK/proxy message or whether a member or project budget won.
                raise ResourceExhausted() from None
            _emit_index_internal_failure(
                stage="execute",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None

    async def _authorize_invocation(
        self,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> None:
        response = await self._control.authorize_invocation(
            control_pb2.AuthorizeInvocationRequestV1(
                identity=receipt.identity,
                fence=receipt.fence,
            )
        )
        if response.HasField("rejection"):
            if int(response.disposition) != (
                control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_UNSPECIFIED
            ):
                raise InvalidInput(
                    "The invocation authorization response is ambiguous."
                )
            raise _worker_error_from_runtime(response.rejection)
        disposition = int(response.disposition)
        if disposition == (
            control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW
        ):
            return
        if disposition == (
            control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED
        ):
            raise AmbiguousExecutionRecovery()
        raise InvalidInput("The invocation authorization disposition is malformed.")


@dataclass(frozen=True, slots=True)
class _AcceptedToolRunClaim:
    verified: VerifiedWorkerCommand
    settings: FixtureEntry
    arguments: FixtureEntry
    claim_id: str
    claim_handoff_watermark: int


@dataclass(frozen=True, slots=True)
class _ResolvedToolRunInputs:
    settings: ResolvedToolkitCallToolInput
    arguments: ResolvedToolkitCallToolInput
    client_context: EliteaClientContext


class ToolkitCallToolDeliveryProcessor(IndexIngestDeliveryProcessor):
    """One toolkit tool run over the shared durable lifecycle.

    It inherits the index processor's capacity reservation, BeginExecution and
    AuthorizeInvocation, because a tool run has the same obligations: it holds
    a bounded synchronous slot, it effects provider work, and it must be
    authorized under a live claim before the SDK is touched.

    It emits NO progress frames. A tool run is one call with one answer; the
    index path streams NodeEvents because indexing takes minutes and the user
    watches it. Inventing progress here would create a second projection with
    no reader.
    """

    def _new_progress_output(
        self,
        accepted: _AcceptedClaim | _AcceptedIndexClaim,
        output: OutputSession,
    ) -> None:
        return None

    def _accept_claim(
        self,
        *,
        signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
        command: command_pb2.WorkerCommandV1,
        receipt: control_pb2.ClaimReceiptV1,
        workload_session_id: str,
        producer_id: str,
        now_unix_millis: int,
    ) -> _AcceptedToolRunClaim:
        return _accepted_tool_run_claim(
            signed=signed,
            command=command,
            receipt=receipt,
            workload_session_id=workload_session_id,
            producer_id=producer_id,
            now_unix_millis=now_unix_millis,
        )

    def _validate_capability_identity(
        self,
        command: command_pb2.WorkerCommandV1,
    ) -> None:
        if (
            command.capability_id != TOOLKIT_CALL_TOOL_CAPABILITY_ID
            or command.capability_version != TOOLKIT_CALL_TOOL_CAPABILITY_VERSION
            or command.command_type
            != command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL
            or command.WhichOneof("capability_command") != "toolkit_call_tool"
        ):
            raise UnsupportedCapability()

    async def _resolve_inputs(
        self,
        accepted: _AcceptedToolRunClaim,
        *,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> _ResolvedToolRunInputs:
        if not isinstance(accepted, _AcceptedToolRunClaim):
            raise InternalFailure()
        resolved: dict[str, ResolvedToolkitCallToolInput] = {}
        for entry in (accepted.settings, accepted.arguments):
            grant = self._input_request_builder.build(
                _claim_bound_reference(
                    entry.content,
                    receipt=receipt,
                    claim_id=accepted.claim_id,
                )
            )
            # Materialized, not raw. The content service redeems the settings
            # credential placeholders only after the claim is authorized, so a
            # raw fetch would hand the SDK an unusable configuration.
            raw = await self._input_client.fetch_materialized(
                grant,
                source_immutable_version=entry.immutable_version,
            )
            resolved[entry.semantic_role] = ResolvedToolkitCallToolInput(
                binding=ToolkitCallToolInputBinding(
                    entry_id=entry.entry_id,
                    immutable_version=entry.immutable_version,
                    content_digest=entry.content.digest,
                ),
                value=parse_json_value(raw),
            )
        factory = self._client_context_factory
        if factory is None:
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        claim = RuntimeExecutionClaim(
            execution_id=receipt.identity.execution_id,
            generation=int(receipt.identity.generation),
            claim_id=accepted.claim_id,
            fence_token=bytes(receipt.fence.fence_token),
            resource_project_id=receipt.identity.resource_project_id,
        )
        try:
            context = await factory(claim)
            if str(context.project_id) != receipt.identity.resource_project_id:
                raise AuthorizationFailure(
                    "The claim-scoped SDK project identity does not match the execution."
                )
        except WorkerError:
            raise
        except Exception as error:
            _emit_index_internal_failure(
                stage="input_context",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None
        return _ResolvedToolRunInputs(
            settings=resolved[_TOOL_RUN_SETTINGS_ROLE],
            arguments=resolved[_TOOL_RUN_ARGUMENTS_ROLE],
            client_context=context,
        )

    async def _execute_resolved(
        self,
        accepted: _AcceptedToolRunClaim,
        resolved_input: object,
        *,
        receipt: control_pb2.ClaimReceiptV1,
        progress: object | None,
    ) -> toolkit_pb2.ToolkitCallToolResultV1:
        if not isinstance(resolved_input, _ResolvedToolRunInputs):
            raise InternalFailure()
        command = accepted.verified.command.toolkit_call_tool
        input_bundle_id = receipt.input_bundle.input_bundle_id
        input_bundle_digest = bytes(receipt.input_bundle_ref.digest.value)
        request = tool_run_request_from(
            command,
            input_bundle_id=input_bundle_id,
            input_bundle_digest=input_bundle_digest,
            settings=resolved_input.settings,
            arguments=resolved_input.arguments,
            runtime_config={
                "metadata": {
                    "initiator": "user",
                    "tool_name": command.tool_name,
                }
            },
        )
        try:
            adapter = EliteaSdkToolkitToolAdapter.from_context(
                resolved_input.client_context
            )
        except DependencyUnavailable as error:
            # The image cannot build this toolkit family. REFUSE the run with
            # the reason, do not skip it: a skipped command settles nothing, so
            # the caller's bounded wait would report "slow" for a condition
            # that never changes.
            return unsupported_toolkit_result(
                command,
                input_bundle_id=input_bundle_id,
                input_bundle_digest=input_bundle_digest,
                reason=error.safe_message,
            )
        handler = ToolkitCallToolHandler(adapter, self._supervisor)
        await self._authorize_invocation(receipt)
        try:
            result = await handler.execute(request)
        except WorkerError:
            raise
        except SdkBudgetExceeded:
            # RuntimeErrorV1 has no budget-specific wire code yet. Preserve the
            # policy failure as the canonical non-retryable RESOURCE_EXHAUSTED
            # contract without exposing the SDK or proxy message.
            raise ResourceExhausted() from None
        except Exception as error:
            _emit_index_internal_failure(
                stage="execute",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None
        return bind_tool_run_result_summary(result)


def _accepted_tool_run_claim(
    *,
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> _AcceptedToolRunClaim:
    entries = _validated_claim_entries(
        command=command,
        receipt=receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    selected = command.toolkit_call_tool
    references = (
        (selected.settings_entry_id, _TOOL_RUN_SETTINGS_ROLE),
        (selected.arguments_entry_id, _TOOL_RUN_ARGUMENTS_ROLE),
    )
    by_id = {entry.entry_id: entry for entry in entries}
    accepted_entries: list[FixtureEntry] = []
    for entry_id, role in references:
        entry = by_id.get(entry_id)
        if (
            entry is None
            or entry.semantic_role != role
            or entry.immutable_version != entry.content.immutable_version
            or entry.content.required_grant_audience != _INDEX_INPUT_AUDIENCE
            or entry.content.byte_length > MAX_SETTINGS_BYTES
        ):
            raise InvalidInput("A tool-run input binding is malformed.")
        accepted_entries.append(entry)
    if len(accepted_entries) != len(entries):
        raise InvalidInput("The tool-run input manifest contains an unbound entry.")
    return _AcceptedToolRunClaim(
        verified=_verified_claim_command(signed, command, receipt),
        settings=accepted_entries[0],
        arguments=accepted_entries[1],
        claim_id=receipt.claim_id,
        claim_handoff_watermark=int(receipt.claim_handoff_watermark),
    )


class AgentExecutionDeliveryProcessor(IndexIngestDeliveryProcessor):
    """Initial current-compatible agent execution over the durable lifecycle."""

    def __init__(
        self,
        *,
        checkpoint_factory: CurrentAgentCheckpointFactory,
        **kwargs: Any,
    ) -> None:
        if checkpoint_factory is None:
            raise ValueError("agent checkpoint factory is required")
        super().__init__(**kwargs)
        self._checkpoint_factory = checkpoint_factory

    def _new_progress_output(
        self,
        accepted: _AcceptedClaim | _AcceptedIndexClaim | _AcceptedAgentClaim,
        output: OutputSession,
    ) -> _IndexProgressOutput | None:
        if not isinstance(accepted, _AcceptedAgentClaim):
            raise InternalFailure()
        return _IndexProgressOutput(
            verified=accepted.verified,
            initial_output=output,
            output_session_factory=self._output_session_factory,
            clock_unix_millis=self._clock,
            claim_handoff_watermark=accepted.claim_handoff_watermark,
            output_ack_timeout_seconds=self._output_ack_timeout,
            max_output_sessions=self._max_output_sessions,
        )

    def _accept_claim(
        self,
        *,
        signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
        command: command_pb2.WorkerCommandV1,
        receipt: control_pb2.ClaimReceiptV1,
        workload_session_id: str,
        producer_id: str,
        now_unix_millis: int,
    ) -> _AcceptedAgentClaim:
        return _accepted_agent_claim(
            signed=signed,
            command=command,
            receipt=receipt,
            workload_session_id=workload_session_id,
            producer_id=producer_id,
            now_unix_millis=now_unix_millis,
        )

    def _validate_capability_identity(
        self,
        command: command_pb2.WorkerCommandV1,
    ) -> None:
        if (
            command.capability_version != AGENT_EXECUTION_CAPABILITY_VERSION
            or command.WhichOneof("capability_command") != "agent_execution"
        ):
            raise UnsupportedCapability()
        pairs = {
            (
                AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
                command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION,
            ),
            (
                AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
                command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC,
            ),
        }
        if (command.capability_id, command.command_type) not in pairs:
            raise UnsupportedCapability()

    async def _resolve_inputs(
        self,
        accepted: _AcceptedAgentClaim,
        *,
        receipt: control_pb2.ClaimReceiptV1,
    ) -> _ResolvedAgentInputs:
        grant = self._input_request_builder.build(
            _claim_bound_reference(
                accepted.entry.content,
                receipt=receipt,
                claim_id=accepted.claim_id,
            )
        )
        raw = await self._input_client.fetch_materialized(
            grant,
            source_immutable_version=accepted.entry.immutable_version,
        )
        message = parse_agent_execution_input(raw)
        kind = (
            AgentExecutionKind.APPLICATION
            if accepted.verified.command.capability_id
            == AGENT_EXECUTE_APPLICATION_CAPABILITY_ID
            else AgentExecutionKind.ADHOC
        )
        request = agent_request_from(
            message,
            kind=kind,
            input_bundle_id=receipt.input_bundle.input_bundle_id,
            input_bundle_digest=bytes(receipt.input_bundle_ref.digest.value),
            request_entry_id=accepted.entry.entry_id,
            request_immutable_version=accepted.entry.immutable_version,
            request_content_digest=accepted.entry.content.digest,
        )
        factory = self._client_context_factory
        if factory is None:
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        claim = RuntimeExecutionClaim(
            execution_id=receipt.identity.execution_id,
            generation=int(receipt.identity.generation),
            claim_id=accepted.claim_id,
            fence_token=bytes(receipt.fence.fence_token),
            resource_project_id=receipt.identity.resource_project_id,
        )
        try:
            context = await factory(claim)
            if str(context.project_id) != receipt.identity.resource_project_id:
                raise AuthorizationFailure(
                    "The claim-scoped SDK project identity does not match the execution."
                )
        except WorkerError:
            raise
        except Exception as error:
            _emit_agent_internal_failure(
                stage="input_context",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None
        return _ResolvedAgentInputs(request=request, client_context=context)

    async def _execute_resolved(
        self,
        accepted: _AcceptedAgentClaim,
        resolved_input: object,
        *,
        receipt: control_pb2.ClaimReceiptV1,
        progress: _IndexProgressOutput | None,
    ) -> agent_pb2.AgentExecutionResultV1:
        if not isinstance(resolved_input, _ResolvedAgentInputs) or progress is None:
            raise InternalFailure()
        command = accepted.verified.command
        payload = resolved_input.request.payload
        callback = CurrentAgentNodeEventCallback(
            CurrentAgentNodeEventContext(
                execution_id=command.execution_id,
                stream_id=command.agent_execution.client_stream_id,
                message_id=command.agent_execution.client_message_id,
                execution_generation=(
                    payload.execution_generation
                    or str(command.generation)
                ),
                sio_event=command.agent_execution.sio_event,
                thread_id=(
                    payload.thread_id
                    or payload.conversation_id
                    or command.agent_execution.client_stream_id
                ),
                project_id=_current_numeric_identity(command.resource_project_id),
                chat_project_id=_current_numeric_identity(command.projection_project_id),
            ),
            progress.publish_from_sdk,
        )
        callback.configure_skills(
            applied_skills=payload.applied_skills,
            attached_skills=payload.attached_skills,
            nested_skill_registry=nested_skill_registry_from_payload(payload),
        )
        adapter = EliteaSdkAgentAdapter.from_context(
            resolved_input.client_context,
            callbacks=[callback],
            checkpoint_factory=self._checkpoint_factory,
        )
        handler = AgentExecutionHandler(adapter)
        await self._authorize_invocation(receipt)

        def invoke_agent():
            callback.emit_agent_start(invoked_skills=payload.invoked_skills)
            try:
                result = handler.execute(resolved_input.request)
            except Exception as error:
                if not callback.capture_materialization_authorization(error):
                    raise
                paused = callback.authorization_pause_result()
                if paused is None:
                    raise InvalidInput(
                        "The delegated authorization request identity is unavailable."
                    )
                result = AgentExecutionResult(
                    request=resolved_input.request,
                    sdk_result=paused,
                )
            callback.raise_if_failed()
            # A failed graph node does not always raise. The pinned SDK
            # converts one into message content, and it also returns a
            # flattened failure envelope. Both arrive here as an ordinary
            # object, and `AgentExecutionResultV1` has no failed terminal
            # state: every one of them settles SUCCEEDED. Classify the result
            # BEFORE any response event is published, so a failed turn leaves
            # this worker as a RuntimeErrorV1 frame and reaches the
            # conversation as the same error turn a raised failure produces.
            failure_stage = callback.terminal_failure_stage(result.sdk_result)
            if failure_stage is not None:
                raise _AgentTerminalFailure(failure_stage)
            completed_content = callback.completed_response_content(result.sdk_result)
            if completed_content is not None:
                callback.emit_completed_response(result.sdk_result)
                callback.raise_if_failed()
                try:
                    suggestion = adapter.suggest_next_input(
                        payload,
                        output_text=completed_content,
                    )
                    if suggestion:
                        callback.emit_next_input_suggestion(suggestion)
                except Exception:
                    # Suggestions are best-effort and must never replace the
                    # primary execution's authoritative terminal outcome.
                    pass
            terminal_event = callback.emit_terminal(
                result.sdk_result,
                payload,
                response_emitted=completed_content is not None,
            )
            callback.raise_if_failed()
            return result, terminal_event

        try:
            result, terminal_event = await self._supervisor.run_sync(invoke_agent)
            browser_result = encode_current_node_event_json(terminal_event)
            digest = hashlib.sha256(browser_result).digest()
            artifact_kind = (
                "hitl-interrupt"
                if terminal_event.type == "agent_hitl_interrupt"
                else (
                    "mcp-authorization-required"
                    if terminal_event.type == "mcp_authorization_required"
                    else "full-message"
                )
            )
            return bind_agent_result_artifact(
                result,
                artifact_id=(
                    f"node-event:{command.execution_id}:{artifact_kind}"
                ),
                immutable_version=f"sha256:{digest.hex()}",
                byte_length=len(browser_result),
                digest=digest,
                # #607: whatever this turn extracted from its attachments, so
                # elitea-main can persist it and the NEXT turn can see the file.
                # Empty on every turn that carried no document, and empty for a
                # document whose read failed. Reported on every terminal state,
                # not only completion: a turn that pauses for HITL already paid
                # for the read, and losing it would make the resume pay again.
                attachment_contents=adapter.attachment_content_writebacks,
            )
        except _IndexProgressTransportFailure:
            raise
        except WorkerError:
            raise
        except _AgentTerminalFailure as error:
            # The turn ran and reported a failure. It is terminal: a retry
            # replays the same graph over the same immutable input and fails
            # the same way. INTERNAL is the non-retryable arm, and it is the
            # exact outcome a raised node failure already produces.
            _emit_agent_internal_failure(
                stage=f"terminal_{error.stage}",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None
        except Exception as error:
            if isinstance(error, SdkBudgetExceeded):
                # The same mapping the index path makes, for the same reason.
                # A budget rejection is a policy outcome, and it is terminal:
                # no retry clears an exhausted budget. RESOURCE_EXHAUSTED is
                # the canonical non-retryable contract for it.
                #
                # Reporting it as InternalFailure, which is what happened
                # before the agent adapter had a budget boundary, told the
                # caller the worker had broken and invited a retry that could
                # only fail again.
                #
                # RuntimeErrorV1 still has no budget-specific wire code, so the
                # SDK/proxy message and the scope that won (member or project)
                # do not cross this boundary.
                raise ResourceExhausted() from None
            if _is_mcp_dependency_failure(error):
                _emit_agent_internal_failure(
                    stage="mcp_materialization",
                    execution_id=receipt.identity.execution_id,
                    error=error,
                )
                raise DependencyUnavailable() from None
            _emit_agent_internal_failure(
                stage="execute",
                execution_id=receipt.identity.execution_id,
                error=error,
            )
            raise InternalFailure() from None


def _is_mcp_dependency_failure(error: BaseException) -> bool:
    """Classify only bounded SDK MCP failures as dependency outcomes."""

    if error.__class__.__name__ == "McpAuthorizationRequired":
        return False
    visited: set[int] = set()
    pending: list[BaseException] = [error]
    while pending:
        current = pending.pop()
        identity = id(current)
        if identity in visited:
            continue
        visited.add(identity)
        traceback = current.__traceback__
        while traceback is not None:
            module = str(traceback.tb_frame.f_globals.get("__name__", ""))
            if module.startswith(
                (
                    "elitea_sdk.runtime.toolkits.mcp",
                    "elitea_sdk.runtime.utils.mcp_",
                    "elitea_sdk.runtime.tools.mcp_",
                    "langchain_mcp_adapters.",
                    "mcp.",
                )
            ):
                return True
            traceback = traceback.tb_next
        for nested in (current.__cause__, current.__context__):
            if isinstance(nested, BaseException):
                pending.append(nested)
    return False


def _emit_agent_internal_failure(
    *,
    stage: str,
    execution_id: str,
    error: Exception,
) -> None:
    frames: list[dict[str, object]] = []
    traceback = error.__traceback__
    while traceback is not None:
        code = traceback.tb_frame.f_code
        filename = code.co_filename.rsplit("/", 1)[-1].rsplit("\\", 1)[-1]
        frames.append(
            {
                "file": filename or "<unknown>",
                "function": code.co_name,
                "line": traceback.tb_lineno,
            }
        )
        traceback = traceback.tb_next
    error_type = type(error)
    print(
        json.dumps(
            {
                "event": "agent_execution_internal_failure",
                "stage": stage,
                "execution_id": execution_id,
                "exception_module": error_type.__module__,
                "exception_name": error_type.__name__,
                "frames": frames[-_INDEX_INTERNAL_FAILURE_FRAME_LIMIT:],
            },
            sort_keys=True,
            separators=(",", ":"),
        ),
        file=sys.stderr,
        flush=True,
    )


async def _publish_missing_index_failure_status(
    callback: CurrentIndexNodeEventCallback,
    progress: _IndexProgressOutput,
) -> None:
    event = callback.finish_index_status_on_failure()
    if event is not None:
        await progress.publish_from_delivery(event)
    callback.raise_if_failed()


async def _publish_index_summary_correction(
    callback: CurrentIndexNodeEventCallback,
    progress: _IndexProgressOutput,
    state: str,
) -> None:
    event = callback.finish_index_status_for_summary(
        state,
        correct_inconsistent=True,
    )
    if event is not None:
        await progress.publish_from_delivery(event)
    callback.raise_if_failed()


def _current_numeric_identity(value: str) -> int | str:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return value
    return parsed if parsed > 0 else value


def _current_user_identity(principal_ref: str) -> int | str:
    value = principal_ref.removeprefix("user:")
    return _current_numeric_identity(value)


def _current_toolkit_id(toolkit_configuration: Any) -> int | str | None:
    if not isinstance(toolkit_configuration, dict):
        return None
    value = toolkit_configuration.get("id", toolkit_configuration.get("toolkit_id"))
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, int):
        return value if value > 0 else None
    if isinstance(value, str) and value:
        parsed = _current_numeric_identity(value)
        return parsed if parsed else None
    return None


def _current_index_name(tool_parameters: Any) -> str | None:
    if not isinstance(tool_parameters, dict):
        return None
    value = tool_parameters.get("index_name")
    if (
        not isinstance(value, str)
        or not value
        or value != value.strip()
        or any(character in value for character in ("\x00", "\r", "\n"))
    ):
        return None
    try:
        return value if len(value.encode("utf-8")) <= MAX_SAFE_STRING_BYTES else None
    except UnicodeEncodeError:
        return None


def _current_toolkit_display_name(toolkit_configuration: Any) -> str:
    if not isinstance(toolkit_configuration, dict):
        return "index_data"
    for field in ("toolkit_name", "name", "type"):
        value = toolkit_configuration.get(field)
        if (
            isinstance(value, str)
            and value
            and len(value.encode("utf-8")) <= MAX_SAFE_STRING_BYTES
        ):
            return value
    return "index_data"


def _claim_receipt(response: control_pb2.ClaimCommandResponseV1) -> control_pb2.ClaimReceiptV1:
    if response.HasField("rejection"):
        if response.HasField("receipt"):
            raise InvalidInput("The claim response is ambiguous.")
        raise _worker_error_from_runtime(response.rejection)
    if not response.HasField("receipt"):
        raise InvalidInput("The claim response is missing its receipt.")
    return response.receipt


def _accepted_claim(
    *,
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> _AcceptedClaim:
    entries = _validated_claim_entries(
        command=command,
        receipt=receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    selected = [
        entry
        for entry in entries
        if entry.entry_id == command.configuration_validation.settings_entry_id
    ]
    if len(selected) != 1:
        raise InvalidInput("The selected settings entry is absent or ambiguous.")
    entry = selected[0]
    if (
        entry.semantic_role != "configuration.settings"
        or entry.immutable_version != entry.content.immutable_version
        or entry.content.required_grant_audience != _INDEX_INPUT_AUDIENCE
        or entry.content.byte_length > MAX_SETTINGS_BYTES
    ):
        raise InvalidInput("The selected settings entry is malformed.")

    return _AcceptedClaim(
        verified=_verified_claim_command(signed, command, receipt),
        content=entry.content,
        claim_id=receipt.claim_id,
        claim_handoff_watermark=int(receipt.claim_handoff_watermark),
    )


def _accepted_index_claim(
    *,
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> _AcceptedIndexClaim:
    entries = _validated_claim_entries(
        command=command,
        receipt=receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    selected = command.index_ingest
    references = (
        (selected.toolkit_configuration_entry_id, _INDEX_TOOLKIT_CONFIGURATION_ROLE, True),
        (selected.tool_parameters_entry_id, _INDEX_TOOL_PARAMETERS_ROLE, True),
        (selected.llm_model_entry_id, _INDEX_LLM_MODEL_ROLE, False),
        (selected.llm_configuration_entry_id, _INDEX_LLM_CONFIGURATION_ROLE, False),
        (selected.mcp_tokens_entry_id, _INDEX_MCP_TOKENS_ROLE, False),
    )
    by_id = {entry.entry_id: entry for entry in entries}
    accepted_entries: list[FixtureEntry] = []
    for entry_id, role, required in references:
        if not entry_id:
            if required:
                raise InvalidInput("A required index input binding is absent.")
            continue
        entry = by_id.get(entry_id)
        if (
            entry is None
            or entry.semantic_role != role
            or entry.immutable_version != entry.content.immutable_version
            or entry.content.required_grant_audience != _INDEX_INPUT_AUDIENCE
            or entry.content.byte_length > MAX_SETTINGS_BYTES
        ):
            raise InvalidInput("An index input binding is malformed.")
        accepted_entries.append(entry)
    if selected.HasField("embedding_binding"):
        binding = selected.embedding_binding
        entry = by_id.get(binding.entry_id)
        if (
            entry is None
            or entry.semantic_role != _INDEX_EMBEDDING_BINDING_ROLE
            or entry.immutable_version != entry.content.immutable_version
            or entry.content.required_grant_audience != _INDEX_INPUT_AUDIENCE
            or entry.content.byte_length > MAX_SETTINGS_BYTES
            or binding.immutable_version != entry.immutable_version
            or binding.content_digest.algorithm
            != common_pb2.DIGEST_ALGORITHM_V1_SHA256
            or not hmac.compare_digest(
                bytes(binding.content_digest.value),
                entry.content.digest,
            )
        ):
            raise InvalidInput("The embedding input binding is malformed.")
        accepted_entries.append(entry)
    if len(accepted_entries) != len(entries):
        raise InvalidInput("The index input manifest contains an unbound entry.")
    return _AcceptedIndexClaim(
        verified=_verified_claim_command(signed, command, receipt),
        entries=tuple(accepted_entries),
        claim_id=receipt.claim_id,
        claim_handoff_watermark=int(receipt.claim_handoff_watermark),
    )


def _accepted_agent_claim(
    *,
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> _AcceptedAgentClaim:
    entries = _validated_claim_entries(
        command=command,
        receipt=receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    selected = command.agent_execution
    matches = [entry for entry in entries if entry.entry_id == selected.request_entry_id]
    if len(matches) != 1 or len(entries) != 1:
        raise InvalidInput("The selected agent request is absent or ambiguous.")
    entry = matches[0]
    if (
        entry.semantic_role != AGENT_EXECUTION_REQUEST_ROLE
        or entry.immutable_version != entry.content.immutable_version
        or entry.content.media_type != AGENT_INPUT_MEDIA_TYPE
        or entry.content.required_grant_audience != _INDEX_INPUT_AUDIENCE
        or entry.content.byte_length > MAX_AGENT_INPUT_BYTES
    ):
        raise InvalidInput("The selected agent request is malformed.")
    return _AcceptedAgentClaim(
        verified=_verified_claim_command(signed, command, receipt),
        entry=entry,
        claim_id=receipt.claim_id,
        claim_handoff_watermark=int(receipt.claim_handoff_watermark),
    )


def _validated_claim_entries(
    *,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> tuple[FixtureEntry, ...]:
    _validate_active_fence(
        receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    if receipt.desired_state != common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING:
        raise AuthorizationFailure("The accepted claim desired state is malformed.")
    if not receipt.HasField("input_bundle_ref") or receipt.input_bundle_ref != command.input_bundle_ref:
        raise AuthorizationFailure("The accepted claim changed the immutable input reference.")
    if not receipt.HasField("input_bundle"):
        raise InvalidInput("The accepted claim is missing its input manifest.")

    manifest_raw = receipt.input_bundle.SerializeToString(deterministic=True)
    reference = command.input_bundle_ref
    if (
        len(manifest_raw) != reference.byte_length
        or not hmac.compare_digest(hashlib.sha256(manifest_raw).digest(), reference.digest.value)
    ):
        raise AuthorizationFailure("The accepted claim input manifest does not match its command.")
    manifest = parse_execution_input_bundle(manifest_raw)
    if (
        manifest.input_bundle_id != reference.input_bundle_id
        or manifest.immutable_version != reference.immutable_version
        or not manifest.entries
        or len(manifest.entries) > MAX_BUNDLE_ENTRIES
    ):
        raise InvalidInput("The accepted claim input manifest is malformed.")
    return project_input_manifest_entries(manifest)


def _claim_bound_reference(
    content: FixtureContent,
    *,
    receipt: control_pb2.ClaimReceiptV1,
    claim_id: str,
) -> ClaimBoundInputReference:
    return ClaimBoundInputReference(
        execution_id=receipt.identity.execution_id,
        generation=receipt.identity.generation,
        content_id=content.content_id,
        immutable_version=content.immutable_version,
        claim_id=claim_id,
        fence_token=bytes(receipt.fence.fence_token),
        expected_length=content.byte_length,
        expected_sha256=content.digest,
        media_type=content.media_type,
    )


def _verified_claim_command(
    signed: envelope_pb2.SignedWorkerCommandEnvelopeV1,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
) -> VerifiedWorkerCommand:
    envelope = envelope_pb2.WorkerExecutionEnvelopeV1()
    envelope.signed_command.CopyFrom(signed)
    envelope.fence.CopyFrom(receipt.fence)
    return VerifiedWorkerCommand(envelope=envelope, command=command)


def _validate_pending_output(
    frame: output_pb2.ExecutionOutputFrameV1,
    *,
    command: command_pb2.WorkerCommandV1,
    receipt: control_pb2.ClaimReceiptV1,
) -> bool:
    expected_stream = f"{command.execution_id}:{command.generation}"
    selected_capability = command.WhichOneof("capability_command")
    if selected_capability == "configuration_validation":
        expected_logical_output = (
            "configuration-validation:"
            f"{command.configuration_validation.configuration_revision_id}"
        )
        valid_sequence = frame.sequence == 1
    elif selected_capability in ("index_ingest", "agent_execution"):
        expected_logical_output = (
            f"index-ingest:{command.execution_id}"
            if selected_capability == "index_ingest"
            else f"agent-execution:{command.execution_id}"
        )
        valid_sequence = (
            frame.sequence >= 1
            and int(frame.claim_handoff_watermark) < int(frame.sequence)
        )
    else:
        raise UnsupportedCapability()
    if not frame.HasField("identity") or frame.identity != receipt.identity:
        raise AuthorizationFailure(
            "The durable output spool belongs to a different execution identity."
        )
    if (
        frame.output_schema_revision != OUTPUT_SCHEMA_REVISION
        or frame.stream_id != expected_stream
        or frame.event_id != f"{command.command_id}:{frame.sequence}"
        or not valid_sequence
        or frame.occurred_at_unix_millis <= 0
        or not _valid_sha256(frame.payload_digest)
    ):
        raise InvalidInput("The durable output frame is malformed.")

    if not frame.terminal:
        payload = frame.node_event
        payload_raw = payload.SerializeToString(deterministic=True)
        expected_progress_output = (
            f"node-event:{command.execution_id}:{frame.sequence}"
        )
        if (
            selected_capability not in ("index_ingest", "agent_execution")
            or frame.logical_output_id != expected_progress_output
            or frame.event_type
            != output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT
            or frame.WhichOneof("payload") != "node_event"
            or frame.HasField("settlement_proposal")
            or not hmac.compare_digest(
                hashlib.sha256(payload_raw).digest(),
                frame.payload_digest.value,
            )
        ):
            raise InvalidInput("The durable NodeEvent output frame is malformed.")
        try:
            encode_current_node_event_json(payload)
        except InvalidCurrentNodeEvent as error:
            raise InvalidInput(
                "The durable NodeEvent output frame is malformed."
            ) from error
        return False

    proposal = frame.settlement_proposal
    if (
        frame.logical_output_id != expected_logical_output
        or not frame.HasField("settlement_proposal")
        or proposal.proposal_id != f"{command.command_id}:settlement"
        or proposal.terminal_logical_output_id != frame.logical_output_id
        or proposal.terminal_event_id != frame.event_id
        or proposal.terminal_sequence != frame.sequence
        or proposal.terminal_payload_digest != frame.payload_digest
        or proposal.prepare_idempotency_key
        != f"{command.command_id}:prepare-settlement"
        or int(proposal.requested_outcome) not in _TERMINAL_OUTCOMES
    ):
        raise InvalidInput("The durable terminal output frame is malformed.")
    return True


def _pending_output_binding_matches(
    frame: output_pb2.ExecutionOutputFrameV1,
    receipt: control_pb2.ClaimReceiptV1,
) -> bool:
    return (
        frame.HasField("fence")
        and frame.fence == receipt.fence
        and int(frame.claim_handoff_watermark)
        == int(receipt.claim_handoff_watermark)
    )


def _require_running_desired_state(state: int) -> None:
    if state == common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING:
        return
    if state == common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED:
        raise ExecutionCancelled()
    if state == common_pb2.DESIRED_EXECUTION_STATE_V1_DRAINING:
        raise DependencyUnavailable("The execution is draining.")
    raise InvalidInput("The execution desired state is malformed.")


def _is_output_cancellation_winner(error: BaseException) -> bool:
    if isinstance(error, OutputCancellationWon):
        return True
    return isinstance(error, RuntimeError) and isinstance(
        error.__cause__, OutputCancellationWon
    )


def _is_output_deadline_winner(error: BaseException) -> bool:
    if isinstance(error, OutputDeadlineWon):
        return True
    return isinstance(error, RuntimeError) and isinstance(
        error.__cause__, OutputDeadlineWon
    )


def _is_cancellation_frame(frame: output_pb2.ExecutionOutputFrameV1) -> bool:
    return (
        frame.HasField("runtime_error")
        and frame.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
        and frame.HasField("settlement_proposal")
        and frame.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
    )


def _is_deadline_frame(frame: output_pb2.ExecutionOutputFrameV1) -> bool:
    return (
        frame.HasField("runtime_error")
        and frame.runtime_error.code
        == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
        and frame.runtime_error.safe_message == "The execution deadline was exceeded."
        and frame.runtime_error.retryable
        and frame.HasField("settlement_proposal")
        and frame.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_FAILED
    )


def _validate_receipt_identity(
    receipt: control_pb2.ClaimReceiptV1,
    command: command_pb2.WorkerCommandV1,
) -> None:
    expected = common_pb2.ExecutionIdentityV1(
        tenant_id=command.tenant_id,
        resource_project_id=command.resource_project_id,
        projection_project_id=command.projection_project_id,
        command_id=command.command_id,
        execution_id=command.execution_id,
        generation=command.generation,
    )
    if not receipt.HasField("identity") or receipt.identity != expected:
        raise AuthorizationFailure("The claim receipt identity does not match its command.")


def _validate_obsolete_receipt(receipt: control_pb2.ClaimReceiptV1) -> None:
    _require_no_worker_authority(receipt)
    if receipt.desired_state != common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED:
        raise InvalidInput("The obsolete claim receipt desired state is malformed.")


def _validate_retired_receipt(receipt: control_pb2.ClaimReceiptV1) -> None:
    _require_no_worker_authority(receipt)
    if int(receipt.desired_state) not in _KNOWN_DESIRED_STATES:
        raise InvalidInput("The retired claim receipt desired state is malformed.")
    if not receipt.HasField("retirement"):
        raise InvalidInput("The retired claim receipt is missing its retirement detail.")
    retirement = receipt.retirement
    if (
        retirement.code != errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED
        or retirement.safe_message != _DEADLINE_RETIREMENT_SAFE_MESSAGE
        or not retirement.retryable
    ):
        raise InvalidInput("The retired claim receipt detail is malformed.")


def _require_no_worker_authority(receipt: control_pb2.ClaimReceiptV1) -> None:
    if (
        receipt.HasField("fence")
        or receipt.lease_expires_at_unix_millis != 0
        or receipt.HasField("input_bundle_ref")
        or receipt.HasField("input_bundle")
        or receipt.claim_handoff_watermark != 0
        or receipt.claim_id
        or receipt.HasField("settlement_recovery")
    ):
        raise InvalidInput(
            "The no-authority claim receipt contains worker authority or business material."
        )


def _validate_active_fence(
    receipt: control_pb2.ClaimReceiptV1,
    *,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> None:
    if (
        not receipt.HasField("fence")
        or receipt.fence.workload_session_id != workload_session_id
        or receipt.fence.producer_id != producer_id
        or receipt.fence.claim_attempt < 1
        or receipt.fence.lease_epoch < 1
        or len(receipt.fence.fence_token) != 32
        or not _bounded_text(receipt.claim_id)
        or receipt.lease_expires_at_unix_millis <= now_unix_millis
    ):
        raise AuthorizationFailure("The claim fence is malformed or expired.")


def _validate_recover_running_receipt(
    receipt: control_pb2.ClaimReceiptV1,
    *,
    workload_session_id: str,
    producer_id: str,
    now_unix_millis: int,
) -> None:
    _validate_active_fence(
        receipt,
        workload_session_id=workload_session_id,
        producer_id=producer_id,
        now_unix_millis=now_unix_millis,
    )
    if (
        receipt.desired_state
        not in {
            common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
            common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
        }
        or receipt.HasField("input_bundle_ref")
        or receipt.HasField("input_bundle")
        or receipt.HasField("settlement_recovery")
    ):
        raise InvalidInput(
            "The running recovery receipt contains business or settlement material."
        )


def _terminal_ack_recovery(
    receipt: control_pb2.ClaimReceiptV1,
) -> control_pb2.SettlementRecoveryV1:
    if not receipt.HasField("settlement_recovery"):
        raise InvalidInput("The terminal ACK recovery receipt is missing.")
    recovery = receipt.settlement_recovery
    if (
        not recovery.HasField("proposal")
        or not recovery.HasField("proposal_digest")
        or not _bounded_text(recovery.idempotency_key)
        or recovery.settlement_receipt_id
        or recovery.outcome != common_pb2.EXECUTION_OUTCOME_V1_UNSPECIFIED
    ):
        raise InvalidInput("The terminal ACK recovery receipt is malformed.")
    proposal = recovery.proposal
    if (
        not _bounded_text(proposal.proposal_id)
        or not _bounded_text(proposal.terminal_logical_output_id)
        or not _bounded_text(proposal.terminal_event_id)
        or not _bounded_text(proposal.prepare_idempotency_key)
        or recovery.idempotency_key != proposal.prepare_idempotency_key
        or proposal.terminal_sequence < 1
        or int(proposal.requested_outcome) not in _TERMINAL_OUTCOMES
        or not _valid_sha256(proposal.terminal_payload_digest)
        or not _valid_sha256(recovery.proposal_digest)
    ):
        raise InvalidInput("The terminal ACK recovery proposal is malformed.")
    proposal_raw = proposal.SerializeToString(deterministic=True)
    if not hmac.compare_digest(
        hashlib.sha256(proposal_raw).digest(),
        recovery.proposal_digest.value,
    ):
        raise AuthorizationFailure("The terminal ACK recovery proposal digest is invalid.")
    return recovery


def _prepared_settlement_recovery(
    receipt: control_pb2.ClaimReceiptV1,
) -> control_pb2.SettlementRecoveryV1:
    if not receipt.HasField("settlement_recovery"):
        raise InvalidInput("The prepared settlement recovery receipt is missing.")
    recovery = receipt.settlement_recovery
    if (
        recovery.HasField("proposal")
        or recovery.HasField("proposal_digest")
        or recovery.idempotency_key
        or not _bounded_text(recovery.settlement_receipt_id)
        or int(recovery.outcome) not in _TERMINAL_OUTCOMES
    ):
        raise InvalidInput("The prepared settlement recovery receipt is malformed.")
    return recovery


def _require_no_recovery(receipt: control_pb2.ClaimReceiptV1) -> None:
    if receipt.HasField("settlement_recovery"):
        raise InvalidInput("The claim disposition has unexpected recovery material.")


def _settlement_receipt(
    response: control_pb2.PrepareSettlementResponseV1,
    *,
    expected_outcome: int,
) -> str:
    if response.HasField("rejection"):
        raise _worker_error_from_runtime(response.rejection)
    if (
        not _bounded_text(response.settlement_receipt_id)
        or int(response.outcome) != expected_outcome
    ):
        raise InvalidInput("The settlement receipt is malformed.")
    return response.settlement_receipt_id


def _worker_error_from_runtime(error: errors_pb2.RuntimeErrorV1) -> WorkerError:
    code = int(error.code)
    if code == errors_pb2.RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY:
        return UnsupportedCapability()
    if code == errors_pb2.RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION:
        return IncompatibleVersion()
    if code in {
        errors_pb2.RUNTIME_ERROR_CODE_V1_INVALID_INPUT,
        errors_pb2.RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION,
    }:
        return InvalidInput()
    if code == errors_pb2.RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED:
        return ResourceExhausted()
    if code in {
        errors_pb2.RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED,
        errors_pb2.RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED,
        errors_pb2.RUNTIME_ERROR_CODE_V1_STALE_FENCE,
    }:
        return AuthorizationFailure()
    if code == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED:
        return ExecutionCancelled()
    if code == errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED:
        return DeadlineExceeded()
    if code in {
        errors_pb2.RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE,
        errors_pb2.RUNTIME_ERROR_CODE_V1_INTERNAL,
    }:
        return DependencyUnavailable()
    return InvalidInput("The runtime control response is malformed.")


def _bounded_text(value: str) -> bool:
    return bool(value) and len(value.encode("utf-8")) <= MAX_SAFE_STRING_BYTES


def _valid_sha256(value: common_pb2.DigestV1) -> bool:
    return (
        value.algorithm == common_pb2.DIGEST_ALGORITHM_V1_SHA256
        and len(value.value) == 32
    )


def _runtime_now(clock: Callable[[], int]) -> int:
    value = clock()
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise InvalidInput("The runtime clock is malformed.")
    return value


def _raise_if_execution_deadline_exceeded(
    command: command_pb2.WorkerCommandV1,
    clock: Callable[[], int],
) -> None:
    if _runtime_now(clock) >= int(command.deadline_unix_millis):
        raise DeadlineExceeded()


def _reconnectable_output(error: RuntimeError | TimeoutError) -> bool:
    if isinstance(error, TimeoutError):
        return True
    cause = error.__cause__
    return not isinstance(
        cause,
        (AuthorizationFailure, DeadlineExceeded, ExecutionCancelled, InvalidInput),
    )
