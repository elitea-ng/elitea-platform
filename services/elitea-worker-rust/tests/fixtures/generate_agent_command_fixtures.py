"""Print deterministic signed agent-command fixtures from Python bindings."""

from __future__ import annotations

import hashlib
import hmac
import sys
from pathlib import Path


PLATFORM_ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(PLATFORM_ROOT / "libs/proto/gen/python"))

from cryptography.hazmat.primitives import serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric.ed25519 import (  # noqa: E402
    Ed25519PrivateKey,
)
from elitea.runtime.v1 import (  # noqa: E402
    agent_pb2,
    command_pb2,
    common_pb2,
    envelope_pb2,
    input_pb2,
)


CONFORMANCE_KEY = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"
CONFORMANCE_KEY_ID = "elitea-runtime-v1-conformance-hmac"
ED25519_DOMAIN = b"elitea.runtime.worker-command.ed25519.v1\x00"


def command(*, application: bool) -> command_pb2.WorkerCommandV1:
    return command_pb2.WorkerCommandV1(
        protocol_revision="elitea.runtime.v1",
        command_id="command-1",
        idempotency_key="outbox-1",
        command_type=(
            command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION
            if application
            else command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC
        ),
        execution_id="execution-1",
        generation=2,
        dispatch_ordinal=3,
        root_execution_id="execution-1",
        tenant_id="tenant-1",
        resource_project_id="7",
        projection_project_id="7",
        principal_ref="user:11",
        input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
            input_bundle_id="bundle-1",
            immutable_version="v1",
            digest=common_pb2.DigestV1(
                algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
                value=b"b" * 32,
            ),
            byte_length=771,
            media_type="application/x-protobuf",
        ),
        capability_id=(
            "agent.execute.application.v1"
            if application
            else "agent.execute.adhoc.v1"
        ),
        capability_version="1",
        resource_class="agent",
        isolation_class="shared-claim-scoped-authority",
        priority=4,
        deadline_unix_millis=1_700_000_000_000,
        traceparent="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
        tracestate="vendor=value",
        limits_revision="elitea.runtime.limits.conformance.v2",
        agent_execution=agent_pb2.AgentExecutionCommandV1(
            request_entry_id="agent-request",
            client_stream_id="conversation-1",
            client_message_id="message-1",
            sio_event="chat_predict" if application else "chat_continue_predict",
        ),
    )


def hmac_envelope(*, application: bool) -> bytes:
    raw = command(application=application).SerializeToString(deterministic=True)
    return envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=CONFORMANCE_KEY_ID,
        worker_command_bytes=raw,
        worker_command_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(raw).digest(),
        ),
        signature=hmac.new(CONFORMANCE_KEY, raw, hashlib.sha256).digest(),
    ).SerializeToString(deterministic=True)


def ed25519_envelope() -> tuple[bytes, bytes]:
    raw = command(application=True).SerializeToString(deterministic=True)
    private_key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
    public_key = private_key.public_key().public_bytes(
        serialization.Encoding.Raw,
        serialization.PublicFormat.Raw,
    )
    signing_input = ED25519_DOMAIN + len(raw).to_bytes(8, "big") + raw
    envelope = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_ED25519,
        key_id="runtime-signing-vector",
        worker_command_bytes=raw,
        worker_command_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(raw).digest(),
        ),
        signature=private_key.sign(signing_input),
    ).SerializeToString(deterministic=True)
    return envelope, public_key


ed25519, public = ed25519_envelope()
print(f"application_hmac={hmac_envelope(application=True).hex()}")
print(f"adhoc_hmac={hmac_envelope(application=False).hex()}")
print(f"application_ed25519={ed25519.hex()}")
print(f"ed25519_public={public.hex()}")
