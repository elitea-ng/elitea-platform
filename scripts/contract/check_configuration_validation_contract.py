#!/usr/bin/env python3
"""Check checked-in configuration validation vectors and their safe bindings."""

from __future__ import annotations

import copy
import hashlib
import hmac
import json
import sys
from pathlib import Path

import jsonschema


sys.dont_write_bytecode = True

REPO_ROOT = Path(__file__).resolve().parents[2]
PROTO_PYTHON = REPO_ROOT / "libs" / "proto" / "gen" / "python"
sys.path.insert(0, str(PROTO_PYTHON))

from elitea.runtime.v1 import command_pb2  # noqa: E402
from elitea.runtime.v1 import common_pb2  # noqa: E402
from elitea.runtime.v1 import control_pb2  # noqa: E402
from elitea.runtime.v1 import envelope_pb2  # noqa: E402
from elitea.runtime.v1 import errors_pb2  # noqa: E402
from elitea.runtime.v1 import input_pb2  # noqa: E402
from elitea.runtime.v1 import limits_pb2  # noqa: E402
from elitea.runtime.v1 import output_pb2  # noqa: E402

from generate_configuration_validation_fixtures import (  # noqa: E402
    CATALOG_DIGEST,
    CONFORMANCE_LIMITS,
    CORPUS_ROOT,
    ENVELOPE_SCHEMA_REVISION,
    LIMITS_REVISION,
    OUTPUT_SCHEMA_REVISION,
    PROTOCOL_REVISION,
    SCHEMA_DIGEST,
    SDK_REVISION,
    TEST_KEY,
    TEST_KEY_ID,
    TEST_OCCURRED_AT_UNIX_MILLIS,
    canonical_json,
    canonical_json_no_lf,
    openapi_credential_free_profile,
    openapi_legacy_parity_matrix,
)


SCHEMA_PATH = (
    REPO_ROOT / "libs" / "jsonschema" / "runtime" / "v1" / "fixture-bundle.schema.json"
)
EXPECTED_RESULT_FIELDS = {
    "configuration_revision_id",
    "configuration_type",
    "catalog_revision",
    "catalog_digest",
    "schema_id",
    "schema_revision",
    "schema_digest",
    "input_bundle_id",
    "input_bundle_digest",
    "settings_entry_id",
    "settings_entry_version",
    "settings_content_digest",
    "valid",
    "issues",
}


def digest_bytes(value: bytes) -> bytes:
    return hashlib.sha256(value).digest()


def assert_sha256(value: common_pb2.DigestV1, expected: bytes) -> None:
    assert value.algorithm == common_pb2.DIGEST_ALGORITHM_V1_SHA256
    assert len(value.value) == 32
    assert value.value == expected


def load_case(case: str, schema: object) -> None:
    case_root = CORPUS_ROOT / case
    locator_bytes = (case_root / "fixture-bundle.json").read_bytes()
    assert len(locator_bytes) <= CONFORMANCE_LIMITS["max_input_manifest_bytes"]
    locator = json.loads(locator_bytes)
    jsonschema.Draft202012Validator(schema).validate(locator)
    assert canonical_json(locator) == locator_bytes

    manifest_descriptor = locator["input_bundle_manifest"]
    assert manifest_descriptor["blob_name"] == "input-bundle.pb"
    input_bundle_bytes = (case_root / manifest_descriptor["blob_name"]).read_bytes()
    input_bundle_digest = digest_bytes(input_bundle_bytes)
    assert len(input_bundle_bytes) <= CONFORMANCE_LIMITS["max_input_manifest_bytes"]
    assert manifest_descriptor["media_type"] == "application/x-protobuf"
    assert manifest_descriptor["byte_length"] == len(input_bundle_bytes)
    assert manifest_descriptor["digest"] == f"sha256:{input_bundle_digest.hex()}"
    input_bundle = input_pb2.ExecutionInputBundleV1.FromString(input_bundle_bytes)
    assert input_bundle.SerializeToString(deterministic=True) == input_bundle_bytes
    assert len(input_bundle.entries) == 1

    entry = input_bundle.entries[0]
    content_reference = entry.content
    locator_entry = locator["entries"][0]
    expected_locator_entry = {
        "entry_id": entry.entry_id,
        "immutable_version": entry.immutable_version,
        "semantic_role": entry.semantic_role,
        "content": {
            "content_id": content_reference.content_id,
            "immutable_version": content_reference.immutable_version,
            "media_type": content_reference.media_type,
            "byte_length": content_reference.byte_length,
            "digest": f"sha256:{bytes(content_reference.digest.value).hex()}",
            "classification": content_reference.classification,
            "required_grant_audience": content_reference.required_grant_audience,
            "blob_name": f"blobs/sha256/{bytes(content_reference.digest.value).hex()}",
        },
    }
    assert locator_entry == expected_locator_entry
    content_locator = locator_entry["content"]
    content = (case_root / content_locator["blob_name"]).read_bytes()
    content_digest = digest_bytes(content)
    assert content_locator["byte_length"] == len(content)
    assert content_locator["digest"] == f"sha256:{content_digest.hex()}"
    assert content_locator["blob_name"] == f"blobs/sha256/{content_digest.hex()}"
    assert_sha256(content_reference.digest, content_digest)
    assert (case_root / "settings.json").read_bytes() == content

    envelope_bytes = (case_root / "envelope.pb").read_bytes()
    assert len(envelope_bytes) <= CONFORMANCE_LIMITS["max_signed_envelope_bytes"]
    envelope = envelope_pb2.WorkerExecutionEnvelopeV1.FromString(envelope_bytes)
    signed = envelope.signed_command
    assert signed.envelope_schema_revision == ENVELOPE_SCHEMA_REVISION
    assert signed.signature_profile == envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256
    assert signed.key_id == TEST_KEY_ID
    assert len(signed.worker_command_bytes) <= CONFORMANCE_LIMITS["max_worker_command_bytes"]
    assert_sha256(signed.worker_command_digest, digest_bytes(signed.worker_command_bytes))
    assert hmac.compare_digest(
        signed.signature,
        hmac.new(TEST_KEY, signed.worker_command_bytes, hashlib.sha256).digest(),
    )
    tampered = signed.worker_command_bytes + b"\x00"
    assert not hmac.compare_digest(
        signed.signature,
        hmac.new(TEST_KEY, tampered, hashlib.sha256).digest(),
    )

    command = command_pb2.WorkerCommandV1.FromString(signed.worker_command_bytes)
    assert command.protocol_revision == PROTOCOL_REVISION
    assert command.command_type == command_pb2.WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE
    assert command.capability_id == "configuration.validate.v1"
    assert command.capability_version == "1"
    assert command.limits_revision == LIMITS_REVISION
    assert command.WhichOneof("capability_command") == "configuration_validation"
    assert command.input_bundle_ref.input_bundle_id == input_bundle.input_bundle_id
    assert command.input_bundle_ref.immutable_version == input_bundle.immutable_version
    assert command.input_bundle_ref.byte_length == len(input_bundle_bytes)
    assert command.input_bundle_ref.media_type == "application/x-protobuf"
    assert_sha256(command.input_bundle_ref.digest, input_bundle_digest)

    validation = command.configuration_validation
    assert validation.catalog_revision == SDK_REVISION
    assert validation.schema_revision == SDK_REVISION
    assert validation.schema_id == "elitea.configuration.openapi"
    assert validation.settings_entry_id == entry.entry_id
    assert_sha256(validation.catalog_digest, CATALOG_DIGEST)
    assert_sha256(validation.schema_digest, SCHEMA_DIGEST)

    output_bytes = (case_root / "expected-output.pb").read_bytes()
    assert len(output_bytes) <= CONFORMANCE_LIMITS["max_output_frame_bytes"]
    output = output_pb2.ExecutionOutputFrameV1.FromString(output_bytes)
    assert output.output_schema_revision == OUTPUT_SCHEMA_REVISION
    assert output.identity.command_id == command.command_id
    assert output.identity.execution_id == command.execution_id
    assert output.identity.generation == command.generation
    assert output.identity.tenant_id == command.tenant_id
    assert output.fence == envelope.fence
    assert output.terminal
    assert output.sequence == 1
    assert output.occurred_at_unix_millis == TEST_OCCURRED_AT_UNIX_MILLIS
    assert output.settlement_proposal.terminal_sequence == output.sequence
    assert output.settlement_proposal.terminal_event_id == output.event_id
    assert output.settlement_proposal.terminal_logical_output_id == output.logical_output_id

    payload_name = output.WhichOneof("payload")
    if case == "unsupported":
        assert validation.configuration_type == "unknown-fixture-type"
        assert payload_name == "runtime_error"
        assert output.event_type == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR
        assert output.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY
        assert output.runtime_error.safe_message == "Configuration type is not supported."
        assert not output.runtime_error.retryable
        payload = output.runtime_error
        expected_outcome = common_pb2.EXECUTION_OUTCOME_V1_FAILED
    else:
        assert validation.configuration_type == "openapi"
        assert payload_name == "configuration_validation"
        assert (
            output.event_type
            == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT
        )
        result = output.configuration_validation
        actual_fields = {field.name for field in result.DESCRIPTOR.fields}
        assert actual_fields == EXPECTED_RESULT_FIELDS
        assert result.configuration_revision_id == validation.configuration_revision_id
        assert result.configuration_type == validation.configuration_type
        assert result.input_bundle_id == input_bundle.input_bundle_id
        assert result.settings_entry_id == entry.entry_id
        assert result.settings_entry_version == entry.immutable_version
        assert_sha256(result.input_bundle_digest, input_bundle_digest)
        assert_sha256(result.settings_content_digest, content_digest)
        assert_sha256(result.catalog_digest, CATALOG_DIGEST)
        assert_sha256(result.schema_digest, SCHEMA_DIGEST)
        if case == "valid":
            assert result.valid
            assert not result.issues
        else:
            assert not result.valid
            assert len(result.issues) == 1
            issue = result.issues[0]
            assert issue.code == "VALUE_NOT_ALLOWED"
            assert issue.json_pointer == "/auth_type"
            assert issue.safe_message == "Value is not one of the allowed choices."
        payload = result
        expected_outcome = common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED

    payload_bytes = payload.SerializeToString(deterministic=True)
    assert_sha256(output.payload_digest, digest_bytes(payload_bytes))
    assert output.settlement_proposal.terminal_payload_digest == output.payload_digest
    assert output.settlement_proposal.requested_outcome == expected_outcome

    cancelled_bytes = (case_root / "expected-cancelled-output.pb").read_bytes()
    assert len(cancelled_bytes) <= CONFORMANCE_LIMITS["max_output_frame_bytes"]
    cancelled = output_pb2.ExecutionOutputFrameV1.FromString(cancelled_bytes)
    assert cancelled.output_schema_revision == output.output_schema_revision
    assert cancelled.stream_id == output.stream_id
    assert cancelled.identity == output.identity
    assert cancelled.fence == output.fence
    assert cancelled.logical_output_id == output.logical_output_id
    assert cancelled.event_id == output.event_id
    assert cancelled.sequence == output.sequence == 1
    assert cancelled.claim_handoff_watermark == output.claim_handoff_watermark
    assert cancelled.occurred_at_unix_millis == output.occurred_at_unix_millis
    assert cancelled.terminal
    assert cancelled.event_type == output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR
    assert cancelled.WhichOneof("payload") == "runtime_error"
    assert cancelled.runtime_error.code == errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED
    assert cancelled.runtime_error.safe_message == "Execution was cancelled."
    assert not cancelled.runtime_error.retryable
    cancellation_payload = cancelled.runtime_error.SerializeToString(deterministic=True)
    assert_sha256(cancelled.payload_digest, digest_bytes(cancellation_payload))
    assert cancelled.settlement_proposal.proposal_id == output.settlement_proposal.proposal_id
    assert (
        cancelled.settlement_proposal.prepare_idempotency_key
        == output.settlement_proposal.prepare_idempotency_key
    )
    assert (
        cancelled.settlement_proposal.requested_outcome
        == common_pb2.EXECUTION_OUTCOME_V1_CANCELLED
    )
    assert cancelled.settlement_proposal.terminal_logical_output_id == cancelled.logical_output_id
    assert cancelled.settlement_proposal.terminal_event_id == cancelled.event_id
    assert cancelled.settlement_proposal.terminal_sequence == cancelled.sequence
    assert cancelled.settlement_proposal.terminal_payload_digest == cancelled.payload_digest


def check_schema_negatives(schema: object) -> None:
    manifest = json.loads((CORPUS_ROOT / "valid" / "fixture-bundle.json").read_bytes())
    validator = jsonschema.Draft202012Validator(schema)

    traversal = copy.deepcopy(manifest)
    traversal["entries"][0]["content"]["blob_name"] = "../settings.json"
    assert not validator.is_valid(traversal)

    uppercase_digest = copy.deepcopy(manifest)
    uppercase_digest["entries"][0]["content"]["digest"] = uppercase_digest[
        "entries"
    ][0]["content"]["digest"].upper()
    assert not validator.is_valid(uppercase_digest)

    extra_field = copy.deepcopy(manifest)
    extra_field["credential"] = "forbidden"
    assert not validator.is_valid(extra_field)


def check_limits_profile() -> None:
    json_bytes = (CORPUS_ROOT / "conformance-limits.json").read_bytes()
    assert canonical_json(json.loads(json_bytes)) == json_bytes
    assert json.loads(json_bytes) == CONFORMANCE_LIMITS
    proto = limits_pb2.ProtocolLimitsV1.FromString(
        (CORPUS_ROOT / "conformance-limits.pb").read_bytes()
    )
    assert proto == limits_pb2.ProtocolLimitsV1(**CONFORMANCE_LIMITS)
    assert proto.max_worker_command_bytes < proto.max_signed_envelope_bytes
    assert proto.max_signed_envelope_bytes <= proto.max_transport_message_bytes
    assert proto.max_signed_envelope_bytes < proto.max_grpc_request_bytes
    assert proto.max_output_frame_bytes <= proto.max_grpc_request_bytes
    assert proto.max_input_manifest_bytes < proto.max_grpc_response_bytes


def check_offline_profile() -> None:
    profile_bytes = (CORPUS_ROOT / "conformance-profile.json").read_bytes()
    profile = json.loads(profile_bytes)
    assert canonical_json(profile) == profile_bytes
    assert profile["profile"] == "TEST_ONLY_OFFLINE_CONFORMANCE_V1"
    assert profile["signature"]["key_id"] == TEST_KEY_ID
    assert profile["signature"]["public_test_key"].encode("ascii") == TEST_KEY
    assert profile["fixed_clock_unix_millis"] == TEST_OCCURRED_AT_UNIX_MILLIS
    assert "reject" in profile["warning"].lower()


def check_catalog_snapshot() -> None:
    catalog_bytes = (CORPUS_ROOT / "configuration-catalog.json").read_bytes()
    assert canonical_json_no_lf(json.loads(catalog_bytes)) == catalog_bytes
    assert digest_bytes(catalog_bytes) == CATALOG_DIGEST
    catalog = json.loads(catalog_bytes)
    assert len(catalog["entries"]) == 32
    openapi = next(entry for entry in catalog["entries"] if entry["type"] == "openapi")
    assert digest_bytes(canonical_json_no_lf(openapi["schema"])) == SCHEMA_DIGEST
    assert openapi["validation_supported"] is True


def check_credential_free_profile_and_legacy_matrix() -> None:
    profile_path = CORPUS_ROOT / "openapi-credential-free-profile.json"
    profile_bytes = profile_path.read_bytes()
    profile = json.loads(profile_bytes)
    assert canonical_json(profile) == profile_bytes
    assert profile == openapi_credential_free_profile()
    assert profile["top_level_field_policy"]["allowed_non_secret_fields"] == [
        "auth_type",
        "client_id",
        "configuration_uuid",
        "custom_header_name",
        "method",
        "oauth_discovery_endpoint",
        "scope",
        "token_url",
    ]
    assert profile["top_level_field_policy"]["forbidden_model_secret_fields"] == [
        "api_key",
        "client_secret",
    ]
    assert profile["top_level_value_policy"]["allowed_json_shapes"] == [
        "BOOLEAN",
        "NULL",
        "NUMBER",
        "STRING",
    ]
    assert profile["top_level_value_policy"]["forbidden_json_shapes"] == [
        "ARRAY",
        "OBJECT",
    ]
    assert profile["value_semantics"] == {
        "algorithm": "OpenApiConfiguration.model_validate(settings)",
        "connection_check": "NOT_CALLED",
        "profile_scope": "TOP_LEVEL_FIELD_CLASSIFICATION_AND_BOUNDED_SCALAR_SHAPE_ONLY",
        "sdk_model_modified": False,
    }

    matrix_path = CORPUS_ROOT / "openapi-legacy-parity-matrix.json"
    matrix_bytes = matrix_path.read_bytes()
    matrix = json.loads(matrix_bytes)
    assert canonical_json(matrix) == matrix_bytes
    assert matrix == openapi_legacy_parity_matrix()
    cases = {case["id"]: case for case in matrix["cases"]}
    required = {
        "method_default_literal",
        "method_basic_literal",
        "method_lowercase_basic",
        "partial_oauth_client_id",
        "delegated_oauth_discovery_without_client",
        "runtime_configuration_uuid",
        "null_non_secret_fields",
        "custom_header_string_not_coerced",
        "custom_header_name_x_api_key_is_non_secret_value",
        "all_known_non_secret_fields",
        "legacy_unknown_extra",
        "api_key_null",
        "nested_x_api_key_legacy_extra_bypass",
        "container_canary_under_allowed_field",
    }
    assert required <= cases.keys()
    for case in cases.values():
        target = case["target"]
        if target["profile"]["outcome"] == "ADMIT":
            assert target["sdk"]["invoked"]
            assert target["sdk"]["call_count"] == 1
            assert target["sdk"]["outcome"] == case["legacy_model_validate"]["outcome"]
            assert target["sdk"]["errors"] == case["legacy_model_validate"]["errors"]
            assert target["intentional_security_difference"] is None
        else:
            assert target["profile"]["outcome"] == "REJECT"
            assert target["sdk"] == {"invoked": False, "call_count": 0}
            assert target["intentional_security_difference"] in {
                "SECRET_FIELD_PRESENCE_REJECTED",
                "UNKNOWN_TOP_LEVEL_FIELD_REJECTED",
                "CONTAINER_VALUE_REJECTED",
            }

    evidence = json.loads((CORPUS_ROOT / "legacy-evidence.json").read_bytes())
    assert evidence["credential_free_profile"]["artifact"] == profile_path.name
    assert evidence["credential_free_profile"]["profile_id"] == profile["profile_id"]
    assert evidence["legacy_parity_matrix"]["artifact"] == matrix_path.name
    assert evidence["legacy_parity_matrix"]["case_ids"] == sorted(cases)


def check_deadline_retirement_control_contract() -> None:
    assert errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED == 12
    assert control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK == 8

    retirement_field = control_pb2.ClaimReceiptV1.DESCRIPTOR.fields_by_name[
        "retirement"
    ]
    assert retirement_field.number == 11
    assert retirement_field.message_type.full_name == "elitea.runtime.v1.RuntimeErrorV1"

    receipt = control_pb2.ClaimReceiptV1(
        disposition=control_pb2.CLAIM_DISPOSITION_V1_RETIRED_ACK,
        identity=common_pb2.ExecutionIdentityV1(
            tenant_id="tenant-contract",
            resource_project_id="1",
            projection_project_id="1",
            command_id="command-contract",
            execution_id="execution-contract",
            generation=1,
        ),
        desired_state=common_pb2.DESIRED_EXECUTION_STATE_V1_RUNNING,
        retirement=errors_pb2.RuntimeErrorV1(
            code=errors_pb2.RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED,
            safe_message=(
                "The execution deadline was exceeded before worker authority was granted."
            ),
            retryable=True,
        ),
    )
    encoded = receipt.SerializeToString(deterministic=True)
    decoded = control_pb2.ClaimReceiptV1.FromString(encoded)
    assert decoded == receipt
    assert decoded.SerializeToString(deterministic=True) == encoded
    assert not decoded.HasField("fence")
    assert not decoded.HasField("input_bundle_ref")
    assert not decoded.HasField("input_bundle")
    assert not decoded.HasField("settlement_recovery")
    assert decoded.lease_expires_at_unix_millis == 0
    assert decoded.claim_handoff_watermark == 0
    assert decoded.claim_id == ""


def main() -> int:
    schema = json.loads(SCHEMA_PATH.read_bytes())
    jsonschema.Draft202012Validator.check_schema(schema)
    for case in ("valid", "invalid", "unsupported"):
        load_case(case, schema)
    check_schema_negatives(schema)
    check_limits_profile()
    check_offline_profile()
    check_catalog_snapshot()
    check_credential_free_profile_and_legacy_matrix()
    check_deadline_retirement_control_contract()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
