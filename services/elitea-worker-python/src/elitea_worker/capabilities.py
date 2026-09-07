"""Canonical generated capability manifest for diagnostics and registration."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from google.protobuf.json_format import MessageToDict

from elitea.config.v1 import capability_manifest_pb2
from elitea.runtime.v1 import command_pb2, common_pb2, envelope_pb2, output_pb2

from elitea_worker.constants import (
    AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
    AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
    AGENT_EXECUTION_CAPABILITY_VERSION,
    CAPABILITY_ID,
    CAPABILITY_VERSION,
    CONFIGURATION_CATALOG_REVISION,
    CONFIGURATION_CATALOG_SHA256,
    INDEX_INGEST_CAPABILITY_ID,
    INDEX_INGEST_CAPABILITY_VERSION,
    LIMITS_REVISION,
    RUNTIME_IMPLEMENTATION,
    RUNTIME_VERSION,
    SDK_PACKAGE_TREE_SHA256,
    SDK_SOURCE_REVISION,
    TOOLKIT_AVAILABLE_TOOLS_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
)


def capability_message(
    *,
    startup_mode: str = "offline",
) -> capability_manifest_pb2.RuntimeCapabilitiesV1:
    if startup_mode not in {"offline", "serve"}:
        raise ValueError("unsupported startup mode")
    if startup_mode == "serve":
        raise ValueError(
            "serve capabilities require an injected production signature profile"
        )
    worker_source_digest = _worker_source_digest()
    return capability_manifest_pb2.RuntimeCapabilitiesV1(
        manifest_schema_revision="elitea.runtime-capabilities.v1",
        runtime_identity=capability_manifest_pb2.RuntimeIdentityV1(
            implementation_name=RUNTIME_IMPLEMENTATION,
            language="python",
            runtime_version=RUNTIME_VERSION,
            build_revision=RUNTIME_VERSION,
            source_revision=worker_source_digest.hex(),
            artifact_digest=_digest(worker_source_digest),
            startup_mode=startup_mode,
            conformance_report_digest=_digest(
                _expected_conformance_report_digest(worker_source_digest)
            ),
        ),
        protocol_compatibility=capability_manifest_pb2.ProtocolCompatibilityV1(
            minimum_major=1,
            minimum_minor=0,
            maximum_major=1,
            maximum_minor=0,
            signature_profiles=[
                envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
            ],
            required_feature_flags=[
                "reference-only-command-v1",
                "separate-control-output-v1",
            ],
        ),
        sdk_framework_compatibility=(
            capability_manifest_pb2.SDKFrameworkCompatibilityV1(
                elitea_sdk_revision=SDK_SOURCE_REVISION,
                elitea_sdk_artifact_digest=_digest(
                    bytes.fromhex(SDK_PACKAGE_TREE_SHA256)
                ),
                configuration_catalog_revision=CONFIGURATION_CATALOG_REVISION,
                configuration_catalog_digest=_digest(
                    bytes.fromhex(CONFIGURATION_CATALOG_SHA256)
                ),
                framework_revisions=["pydantic-v2"],
            )
        ),
        capabilities=[
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=CAPABILITY_ID,
                capability_version=CAPABILITY_VERSION,
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                interaction_model="durable_job",
                resource_classes=["validation-small"],
                feature_flags=[
                    "sdk-registry-validation",
                    "per-type-schema-binding",
                    "reference-only-input",
                    "safe-validation-errors",
                ],
                catalog_revision=CONFIGURATION_CATALOG_REVISION,
                catalog_digest=_digest(bytes.fromhex(CONFIGURATION_CATALOG_SHA256)),
            ),
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=TOOLKIT_AVAILABLE_TOOLS_CAPABILITY_ID,
                capability_version="1",
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                # The exact SDK adapter and artifact-reference contract are
                # implemented and parity-tested. Production claim/input/
                # artifact/output composition is deliberately not advertised
                # as a runnable durable job in this slice.
                interaction_model="contract_handler_parity",
                resource_classes=["toolkit-catalog"],
                feature_flags=[
                    "current-sdk-delegate",
                    "reference-only-input",
                    "artifact-reference-output",
                    "production-delivery-not-wired",
                ],
            ),
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=INDEX_INGEST_CAPABILITY_ID,
                capability_version=INDEX_INGEST_CAPABILITY_VERSION,
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_INDEX_INGEST,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                # Claim/input/runtime-context/output/settlement are composed on
                # the shared durable serve path. The current synchronous SDK
                # call remains bounded by the worker executor.
                interaction_model="durable_job",
                resource_classes=["indexing"],
                feature_flags=[
                    "current-sdk-delegate",
                    "reference-only-input",
                    "claim-bound-materialization",
                    "claim-bound-runtime-context",
                    "bounded-sync-sdk-execution",
                    "typed-inline-terminal-result",
                ],
            ),
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
                capability_version=AGENT_EXECUTION_CAPABILITY_VERSION,
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                interaction_model="durable_job",
                resource_classes=["agent-execution"],
                feature_flags=[
                    "current-sdk-delegate",
                    "reference-only-input",
                    "claim-bound-runtime-context",
                    "bounded-sync-sdk-execution",
                    "durable-checkpoint-resume",
                    "toolkit-authorization-continuation",
                    "typed-inline-terminal-result",
                ],
            ),
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=AGENT_EXECUTE_ADHOC_CAPABILITY_ID,
                capability_version=AGENT_EXECUTION_CAPABILITY_VERSION,
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                interaction_model="durable_job",
                resource_classes=["agent-execution"],
                feature_flags=[
                    "current-sdk-delegate",
                    "reference-only-input",
                    "claim-bound-runtime-context",
                    "bounded-sync-sdk-execution",
                    "durable-checkpoint-resume",
                    "toolkit-authorization-continuation",
                    "typed-inline-terminal-result",
                ],
            ),
            capability_manifest_pb2.RuntimeCapabilityV1(
                capability_id=TOOLKIT_CALL_TOOL_CAPABILITY_ID,
                capability_version=TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
                accepted_command_types=[
                    command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL,
                ],
                emitted_event_types=[
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT,
                    output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
                ],
                # durable_job, not contract_handler_parity. Running a toolkit
                # tool effects provider work: it writes tickets and pushes
                # commits. That needs durable effect identity, claim-bound
                # credentials and a settlement, which is exactly what this
                # interaction model names and what toolkit.available_tools.v1
                # deliberately does not claim.
                interaction_model="durable_job",
                resource_classes=["toolkit-call"],
                feature_flags=[
                    "current-sdk-delegate",
                    "reference-only-input",
                    "claim-bound-materialization",
                    "claim-bound-runtime-context",
                    "bounded-sync-sdk-execution",
                    "typed-inline-terminal-result",
                ],
            ),
        ],
        runtime_constraints=capability_manifest_pb2.RuntimeConstraintsV1(
            isolation_classes=["shared-claim-scoped-authority"],
            architectures=["amd64", "arm64"],
            child_process_support=False,
            network_egress_classes=[
                "scoped-input-content-only",
                "claim-bound-runtime-context",
                "sdk-toolkit-configured",
            ],
            artifact_support=True,
            realtime_session_support=False,
        ),
        limits_profiles=capability_manifest_pb2.RuntimeLimitsProfileReferenceV1(
            limits_schema_revision="elitea.runtime.limits.v1",
            limits_revisions=[LIMITS_REVISION],
            resource_profile_classes=[
                "validation-small",
                "toolkit-catalog",
                "indexing",
                "agent-execution",
                "toolkit-call",
            ],
        ),
    )


def capability_document() -> dict[str, Any]:
    return MessageToDict(
        capability_message(),
        always_print_fields_with_no_presence=True,
    )


def capability_json() -> str:
    return json.dumps(capability_document(), sort_keys=True, separators=(",", ":"))


def conformance_identity_fields() -> dict[str, str]:
    return {
        "runtime_source_digest": f"sha256:{_worker_source_digest().hex()}",
        "sdk_artifact_digest": f"sha256:{SDK_PACKAGE_TREE_SHA256}",
    }


def _digest(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )


def _worker_source_digest() -> bytes:
    root = Path(__file__).resolve().parent
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.py")):
        relative = path.relative_to(root).as_posix().encode("utf-8")
        content = path.read_bytes()
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    return digest.digest()


def _expected_conformance_report_digest(worker_source_digest: bytes) -> bytes:
    report = {
        "schema_version": "elitea.runtime-conformance-report.v1",
        "suite": "runtime-v1",
        "capability": CAPABILITY_ID,
        "status": "passed",
        **{
            "runtime_source_digest": f"sha256:{worker_source_digest.hex()}",
            "sdk_artifact_digest": f"sha256:{SDK_PACKAGE_TREE_SHA256}",
        },
        "cases": [
            {"case": name, "status": "passed"}
            for name in ("valid", "invalid", "unsupported")
        ],
    }
    encoded = json.dumps(report, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).digest()
