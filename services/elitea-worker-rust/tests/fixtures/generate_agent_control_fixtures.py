"""Print deterministic Python worker/Main-compatible agent control fixtures."""

from __future__ import annotations

import hashlib
import hmac
import sys
from pathlib import Path


PLATFORM_ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(PLATFORM_ROOT / "libs/proto/gen/python"))

from elitea.runtime.v1 import (  # noqa: E402
    agent_pb2,
    command_pb2,
    common_pb2,
    control_pb2,
    envelope_pb2,
    errors_pb2,
    input_pb2,
    output_pb2,
)


CONFORMANCE_KEY = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"
CONFORMANCE_KEY_ID = "elitea-runtime-v1-conformance-hmac"


def sha256(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=hashlib.sha256(value).digest(),
    )


manifest = input_pb2.ExecutionInputBundleV1(
    input_bundle_id="bundle-1",
    immutable_version="v1",
    entries=[
        input_pb2.ExecutionInputEntryV1(
            entry_id="agent-request",
            immutable_version="request-v1",
            semantic_role="agent.execution_request",
            content=input_pb2.ScopedContentReferenceV1(
                content_id="agent-content-1",
                immutable_version="request-v1",
                media_type="application/vnd.elitea.agent-execution-input.v1+protobuf",
                byte_length=4096,
                digest=sha256(b"agent-request-content"),
                classification="project",
                required_grant_audience="elitea.runtime.input.read.v1",
            ),
        )
    ],
)
manifest_raw = manifest.SerializeToString(deterministic=True)

command = command_pb2.WorkerCommandV1(
    protocol_revision="elitea.runtime.v1",
    command_id="command-1",
    idempotency_key="outbox-1",
    command_type=command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION,
    execution_id="execution-1",
    generation=2,
    dispatch_ordinal=3,
    root_execution_id="execution-1",
    tenant_id="tenant-1",
    resource_project_id="7",
    projection_project_id="7",
    principal_ref="user:11",
    input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
        input_bundle_id=manifest.input_bundle_id,
        immutable_version=manifest.immutable_version,
        digest=sha256(manifest_raw),
        byte_length=len(manifest_raw),
        media_type="application/x-protobuf",
    ),
    capability_id="agent.execute.application.v1",
    capability_version="1",
    resource_class="agent",
    isolation_class="shared-claim-scoped-authority",
    priority=4,
    deadline_unix_millis=1_700_000_100_000,
    traceparent="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    tracestate="vendor=value",
    limits_revision="elitea.runtime.limits.conformance.v2",
    agent_execution=agent_pb2.AgentExecutionCommandV1(
        request_entry_id="agent-request",
        client_stream_id="conversation-1",
        client_message_id="message-1",
        sio_event="chat_predict",
    ),
)
command_raw = command.SerializeToString(deterministic=True)
signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
    envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
    signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
    key_id=CONFORMANCE_KEY_ID,
    worker_command_bytes=command_raw,
    worker_command_digest=sha256(command_raw),
    signature=hmac.new(CONFORMANCE_KEY, command_raw, hashlib.sha256).digest(),
)

identity = common_pb2.ExecutionIdentityV1(
    tenant_id=command.tenant_id,
    resource_project_id=command.resource_project_id,
    projection_project_id=command.projection_project_id,
    command_id=command.command_id,
    execution_id=command.execution_id,
    generation=command.generation,
)
fence = common_pb2.ExecutionFenceV1(
    workload_session_id="workload-1",
    claim_attempt=5,
    lease_epoch=5,
    producer_id="worker-1",
    fence_token=hashlib.sha256(b"fixture-fence-token").digest(),
)
claim = control_pb2.ClaimCommandResponseV1(
    receipt=control_pb2.ClaimReceiptV1(
        disposition=control_pb2.CLAIM_DISPOSITION_V1_ACCEPTED,
        identity=identity,
        fence=fence,
        lease_expires_at_unix_millis=1_700_000_030_000,
        input_bundle_ref=command.input_bundle_ref,
        input_bundle=manifest,
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        claim_handoff_watermark=4,
        claim_id="claim-5",
        claim_started_at_unix_micros=1_700_000_000_123_456,
    )
)
renewal_seed = (
    identity.SerializeToString(deterministic=True)
    + b"\x00"
    + fence.SerializeToString(deterministic=True)
    + b"\x00"
    + b"claim-5"
)


def recovery_claim(disposition: int) -> control_pb2.ClaimCommandResponseV1:
    response = control_pb2.ClaimCommandResponseV1.FromString(
        claim.SerializeToString(deterministic=True)
    )
    response.receipt.disposition = disposition
    response.receipt.ClearField("input_bundle_ref")
    response.receipt.ClearField("input_bundle")
    response.receipt.claim_started_at_unix_micros = 0
    return response


def no_authority_claim(
    disposition: int,
    desired_state: int,
) -> control_pb2.ClaimCommandResponseV1:
    response = recovery_claim(disposition)
    response.receipt.ClearField("fence")
    response.receipt.lease_expires_at_unix_millis = 0
    response.receipt.claim_handoff_watermark = 0
    response.receipt.claim_id = ""
    response.receipt.desired_state = desired_state
    return response


terminal_proposal = output_pb2.SettlementProposalV1(
    proposal_id="command-1:settlement",
    requested_outcome=common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED,
    terminal_logical_output_id="agent-execution:execution-1",
    terminal_event_id="command-1:5",
    terminal_sequence=5,
    terminal_payload_digest=sha256(b"terminal-agent-result"),
    prepare_idempotency_key="command-1:prepare-settlement",
)
terminal_proposal_raw = terminal_proposal.SerializeToString(deterministic=True)

recover_terminal = recovery_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK
)
recover_terminal.receipt.settlement_recovery.CopyFrom(
    control_pb2.SettlementRecoveryV1(
        proposal=terminal_proposal,
        proposal_digest=sha256(terminal_proposal_raw),
        idempotency_key=terminal_proposal.prepare_idempotency_key,
    )
)

recover_settlement = recovery_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT
)
recover_settlement.receipt.settlement_recovery.CopyFrom(
    control_pb2.SettlementRecoveryV1(
        settlement_receipt_id="settlement-receipt-recovery-1",
        outcome=common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED,
    )
)

settled = recovery_claim(control_pb2.CLAIM_DISPOSITION_V1_SETTLED_ACK)
obsolete = no_authority_claim(
    control_pb2.CLAIM_DISPOSITION_V1_OBSOLETE_ACK,
    common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED,
)
active = recovery_claim(control_pb2.CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK)
retry_later = no_authority_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK,
    common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
)
retired = no_authority_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
    common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
)
retired.receipt.retirement.CopyFrom(
    errors_pb2.RuntimeErrorV1(
        code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
        safe_message=(
            "The execution deadline was exceeded before worker authority was granted."
        ),
        retryable=True,
    )
)
recover_running = recovery_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK
)
recover_running.receipt.desired_state = (
    common_pb2.DESIRED_EXECUTION_STATE_V1_CANCELLED
)
recover_ambiguous = recovery_claim(
    control_pb2.CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK
)


def size_bound_pair(
    content_length: int,
) -> tuple[bytes, bytes]:
    bounded_manifest = input_pb2.ExecutionInputBundleV1.FromString(manifest_raw)
    bounded_manifest.entries[0].content.byte_length = content_length
    bounded_manifest_raw = bounded_manifest.SerializeToString(deterministic=True)
    bounded_command = command_pb2.WorkerCommandV1.FromString(command_raw)
    bounded_command.input_bundle_ref.digest.CopyFrom(sha256(bounded_manifest_raw))
    bounded_command.input_bundle_ref.byte_length = len(bounded_manifest_raw)
    bounded_command_raw = bounded_command.SerializeToString(deterministic=True)
    bounded_signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_KEY_ID,
        worker_command_bytes=bounded_command_raw,
        worker_command_digest=sha256(bounded_command_raw),
        signature=hmac.new(
            CONFORMANCE_KEY,
            bounded_command_raw,
            hashlib.sha256,
        ).digest(),
    )
    bounded_claim = control_pb2.ClaimCommandResponseV1.FromString(
        claim.SerializeToString(deterministic=True)
    )
    bounded_claim.receipt.input_bundle_ref.CopyFrom(
        bounded_command.input_bundle_ref
    )
    bounded_claim.receipt.input_bundle.CopyFrom(bounded_manifest)
    return (
        bounded_signed.SerializeToString(deterministic=True),
        bounded_claim.SerializeToString(deterministic=True),
    )


signed_at_limit, claim_at_limit = size_bound_pair(8 * 1024 * 1024)
signed_over_limit, claim_over_limit = size_bound_pair(8 * 1024 * 1024 + 1)

output_session_command = command_pb2.WorkerCommandV1.FromString(command_raw)
output_session_command.resource_project_id = "resource-1"
output_session_command.projection_project_id = "projection-1"
output_session_command.generation = 1
output_session_command_raw = output_session_command.SerializeToString(deterministic=True)
output_session_signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
    envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
    signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
    key_id=CONFORMANCE_KEY_ID,
    worker_command_bytes=output_session_command_raw,
    worker_command_digest=sha256(output_session_command_raw),
    signature=hmac.new(
        CONFORMANCE_KEY,
        output_session_command_raw,
        hashlib.sha256,
    ).digest(),
)

same_identity_changed_intent_command = command_pb2.WorkerCommandV1.FromString(
    command_raw
)
same_identity_changed_intent_command.deadline_unix_millis += 1
same_identity_changed_intent_raw = (
    same_identity_changed_intent_command.SerializeToString(deterministic=True)
)
same_identity_changed_intent_signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
    envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
    signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
    key_id=CONFORMANCE_KEY_ID,
    worker_command_bytes=same_identity_changed_intent_raw,
    worker_command_digest=sha256(same_identity_changed_intent_raw),
    signature=hmac.new(
        CONFORMANCE_KEY,
        same_identity_changed_intent_raw,
        hashlib.sha256,
    ).digest(),
)

adhoc_command = command_pb2.WorkerCommandV1.FromString(command_raw)
adhoc_command.command_type = command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
adhoc_command.capability_id = "agent.execute.adhoc.v1"
adhoc_command_raw = adhoc_command.SerializeToString(deterministic=True)
adhoc_signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
    envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
    signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
    key_id=CONFORMANCE_KEY_ID,
    worker_command_bytes=adhoc_command_raw,
    worker_command_digest=sha256(adhoc_command_raw),
    signature=hmac.new(
        CONFORMANCE_KEY,
        adhoc_command_raw,
        hashlib.sha256,
    ).digest(),
)

fixtures = {
    "signed_command": signed.SerializeToString(deterministic=True),
    "signed_command_adhoc": adhoc_signed.SerializeToString(deterministic=True),
    "signed_command_same_identity_changed_intent": (
        same_identity_changed_intent_signed.SerializeToString(deterministic=True)
    ),
    "signed_command_output_session": output_session_signed.SerializeToString(
        deterministic=True
    ),
    "accepted_claim": claim.SerializeToString(deterministic=True),
    "claim_recover_terminal_ack": recover_terminal.SerializeToString(deterministic=True),
    "claim_recover_settlement": recover_settlement.SerializeToString(deterministic=True),
    "claim_settled_ack": settled.SerializeToString(deterministic=True),
    "claim_obsolete_ack": obsolete.SerializeToString(deterministic=True),
    "claim_active_lease_noack": active.SerializeToString(deterministic=True),
    "claim_retry_later_noack": retry_later.SerializeToString(deterministic=True),
    "claim_retired_ack": retired.SerializeToString(deterministic=True),
    "claim_recover_running_noack": recover_running.SerializeToString(deterministic=True),
    "claim_recover_ambiguous_invocation_noack": recover_ambiguous.SerializeToString(
        deterministic=True
    ),
    "begin_started": control_pb2.BeginExecutionResponseV1(
        disposition=control_pb2.BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW
    ).SerializeToString(deterministic=True),
    "authorize_now": control_pb2.AuthorizeInvocationResponseV1(
        disposition=control_pb2.AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW
    ).SerializeToString(deterministic=True),
    "renew_running": control_pb2.RenewLeaseResponseV1(
        lease_expires_at_unix_millis=1_700_000_060_000,
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
    ).SerializeToString(deterministic=True),
    "observe_running": control_pb2.ObserveDesiredStateResponseV1(
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING
    ).SerializeToString(deterministic=True),
    "settlement_succeeded": control_pb2.PrepareSettlementResponseV1(
        settlement_receipt_id="settlement-receipt-1",
        outcome=common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED,
    ).SerializeToString(deterministic=True),
    "authorization_rejection": control_pb2.BeginExecutionResponseV1(
        rejection=errors_pb2.RuntimeErrorV1(
            code=errors_pb2.RUNTIME_ERROR_CODE_V1_STALE_FENCE,
            safe_message="fixture detail must not cross the Rust error boundary",
            retryable=False,
        )
    ).SerializeToString(deterministic=True),
    "signed_command_agent_input_at_limit": signed_at_limit,
    "accepted_claim_agent_input_at_limit": claim_at_limit,
    "signed_command_agent_input_over_limit": signed_over_limit,
    "accepted_claim_agent_input_over_limit": claim_over_limit,
}

for name, value in fixtures.items():
    print(f"{name}={value.hex()}")
print(f"renewal_key_1=lease-renew:{hashlib.sha256(renewal_seed).hexdigest()}:1")
