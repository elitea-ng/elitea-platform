#!/usr/bin/env python3
"""Generate deterministic, credential-free configuration validation vectors."""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import sys
from pathlib import Path


sys.dont_write_bytecode = True

REPO_ROOT = Path(__file__).resolve().parents[2]
PROTO_PYTHON = REPO_ROOT / "libs" / "proto" / "gen" / "python"
sys.path.insert(0, str(PROTO_PYTHON))

from elitea.runtime.v1 import command_pb2  # noqa: E402
from elitea.runtime.v1 import common_pb2  # noqa: E402
from elitea.runtime.v1 import envelope_pb2  # noqa: E402
from elitea.runtime.v1 import errors_pb2  # noqa: E402
from elitea.runtime.v1 import input_pb2  # noqa: E402
from elitea.runtime.v1 import limits_pb2  # noqa: E402
from elitea.runtime.v1 import output_pb2  # noqa: E402
from elitea.runtime.v1 import validation_pb2  # noqa: E402


CORPUS_ROOT = (
    REPO_ROOT
    / "testdata"
    / "proto"
    / "runtime"
    / "v1"
    / "configuration-validation"
)
SDK_REVISION = "a78d3654f99d8ff89ca7233f20a66d676e564f79"
SCHEMA_DIGEST = bytes.fromhex(
    "1c43c41a5304c6f73c68deebd37ba70f8c2266a59dfd4f9d4fa20b819e7ab3f1"
)
CATALOG_DIGEST = bytes.fromhex(
    "4a96e3ab8e3842ebf2645a851aeb12e3e2343f28e7d024c1a2960eb4ec254351"
)
TEST_KEY_ID = "elitea-runtime-v1-conformance-hmac"
TEST_KEY = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"
PROTOCOL_REVISION = "elitea.runtime.v1"
ENVELOPE_SCHEMA_REVISION = "elitea.runtime.signed-worker-command.v1"
OUTPUT_SCHEMA_REVISION = "elitea.runtime.execution-output.v1"
LIMITS_REVISION = "elitea.runtime.limits.conformance.v2"
TEST_OCCURRED_AT_UNIX_MILLIS = 1700000000000
CONFORMANCE_LIMITS = {
    "limits_revision": LIMITS_REVISION,
    "max_worker_command_bytes": 32768,
    "max_signed_envelope_bytes": 49152,
    "max_redis_field_bytes": 49152,
    "max_redis_entry_bytes": 65536,
    "max_input_manifest_bytes": 65536,
    "max_input_entries": 16,
    "max_input_content_bytes": 262144,
    "max_output_frame_bytes": 65536,
    "max_validation_issues": 64,
    "max_safe_string_bytes": 256,
    "claim_lease_ttl_millis": 30000,
    "max_lease_poll_interval_millis": 10000,
    "max_grpc_request_bytes": 65536,
    "max_grpc_response_bytes": 81920,
}
OPENAPI_CREDENTIAL_FREE_PROFILE_ID = "OPENAPI_CREDENTIAL_FREE_V1"
OPENAPI_ALLOWED_NON_SECRET_FIELDS = [
    "auth_type",
    "client_id",
    "configuration_uuid",
    "custom_header_name",
    "method",
    "oauth_discovery_endpoint",
    "scope",
    "token_url",
]
OPENAPI_MODEL_SECRET_FIELDS = ["api_key", "client_secret"]


def openapi_credential_free_profile() -> dict[str, object]:
    return {
        "schema_version": "elitea.configuration.credential-free-profile.v1",
        "profile_id": OPENAPI_CREDENTIAL_FREE_PROFILE_ID,
        "configuration_type": "openapi",
        "source_model": {
            "repository": "EliteaAI/elitea-sdk",
            "revision": SDK_REVISION,
            "model": "elitea_sdk.configurations.openapi.OpenApiConfiguration",
            "schema_digest": f"sha256:{SCHEMA_DIGEST.hex()}",
        },
        "top_level_field_policy": {
            "allowed_non_secret_fields": OPENAPI_ALLOWED_NON_SECRET_FIELDS,
            "forbidden_model_secret_fields": OPENAPI_MODEL_SECRET_FIELDS,
            "forbidden_secret_field_presence": "REJECT_REGARDLESS_OF_VALUE_INCLUDING_NULL_OR_EMPTY",
            "unknown_fields": "REJECT_BEFORE_SDK",
            "field_names": "CASE_SENSITIVE_EXACT",
        },
        "top_level_value_policy": {
            "allowed_json_shapes": ["BOOLEAN", "NULL", "NUMBER", "STRING"],
            "forbidden_json_shapes": ["ARRAY", "OBJECT"],
            "max_string_utf8_bytes": 65536,
            "number_policy": "FINITE_JSON_NUMBER",
        },
        "value_semantics": {
            "algorithm": "OpenApiConfiguration.model_validate(settings)",
            "sdk_model_modified": False,
            "profile_scope": "TOP_LEVEL_FIELD_CLASSIFICATION_AND_BOUNDED_SCALAR_SHAPE_ONLY",
            "connection_check": "NOT_CALLED",
        },
        "intentional_security_differences": [
            {
                "code": "SECRET_FIELD_PRESENCE_REJECTED",
                "legacy": "MODEL_VALIDATE_ACCEPTS_SOME_SECRET_FIELDS_INCLUDING_NULL",
                "target": "INVALID_INPUT_BEFORE_SDK",
            },
            {
                "code": "UNKNOWN_TOP_LEVEL_FIELD_REJECTED",
                "legacy": "EXTRA_ALLOW",
                "target": "INVALID_INPUT_BEFORE_SDK",
            },
            {
                "code": "CONTAINER_VALUE_REJECTED",
                "legacy": "MODEL_VALIDATE_REPORTS_CONTAINER_TYPE_ERRORS",
                "target": "INVALID_INPUT_BEFORE_SDK",
            },
        ],
    }


def _parity_case(
    case_id: str,
    settings: dict[str, object],
    legacy_outcome: str,
    *,
    error_types: list[str] | None = None,
    error_locations: list[list[str | int]] | None = None,
    profile_outcome: str = "ADMIT",
    profile_reason: str = "KNOWN_NON_SECRET_FIELDS",
    security_difference: str | None = None,
) -> dict[str, object]:
    errors = {
        "types": error_types or [],
        "locations": error_locations or [],
    }
    sdk = (
        {
            "invoked": True,
            "call_count": 1,
            "algorithm": "OpenApiConfiguration.model_validate(settings)",
            "outcome": legacy_outcome,
            "errors": errors,
        }
        if profile_outcome == "ADMIT"
        else {"invoked": False, "call_count": 0}
    )
    return {
        "id": case_id,
        "settings": settings,
        "legacy_model_validate": {
            "outcome": legacy_outcome,
            "errors": errors,
        },
        "target": {
            "profile": {"outcome": profile_outcome, "reason": profile_reason},
            "sdk": sdk,
            "intentional_security_difference": security_difference,
        },
    }


def openapi_legacy_parity_matrix() -> dict[str, object]:
    cases = [
        _parity_case("empty_object", {}, "VALID"),
        _parity_case("method_default_literal", {"method": "default"}, "VALID"),
        _parity_case("method_basic_literal", {"method": "Basic"}, "VALID"),
        _parity_case(
            "method_lowercase_basic",
            {"method": "basic"},
            "INVALID",
            error_types=["literal_error"],
            error_locations=[["method"]],
        ),
        _parity_case(
            "partial_oauth_client_id",
            {"client_id": "TEST_ONLY_PUBLIC_CLIENT"},
            "INVALID",
            error_types=["value_error"],
            error_locations=[[]],
        ),
        _parity_case(
            "null_non_secret_fields",
            {
                "auth_type": None,
                "client_id": None,
                "configuration_uuid": None,
                "custom_header_name": None,
                "method": None,
                "oauth_discovery_endpoint": None,
                "scope": None,
                "token_url": None,
            },
            "VALID",
        ),
        _parity_case(
            "delegated_oauth_discovery_without_client",
            {"oauth_discovery_endpoint": "https://identity.example.invalid"},
            "INVALID",
            error_types=["value_error"],
            error_locations=[[]],
        ),
        _parity_case(
            "runtime_configuration_uuid",
            {"configuration_uuid": "configuration-openapi-test-only"},
            "VALID",
        ),
        _parity_case(
            "custom_header_string_not_coerced",
            {"custom_header_name": 123},
            "INVALID",
            error_types=["string_type"],
            error_locations=[["custom_header_name"]],
        ),
        _parity_case(
            "custom_header_name_x_api_key_is_non_secret_value",
            {"auth_type": "Custom", "custom_header_name": "X-API-Key"},
            "VALID",
        ),
        _parity_case(
            "custom_mode_without_key_or_header",
            {"auth_type": "Custom"},
            "VALID",
        ),
        _parity_case(
            "all_known_non_secret_fields",
            {
                "auth_type": "Bearer",
                "client_id": "TEST_ONLY_PUBLIC_CLIENT",
                "custom_header_name": "X-Custom",
                "method": "default",
                "scope": "read",
                "token_url": "https://example.invalid/token",
            },
            "INVALID",
            error_types=["value_error"],
            error_locations=[[]],
        ),
        _parity_case(
            "legacy_unknown_extra",
            {"non_sensitive_extension": "preserved-by-legacy"},
            "VALID",
            profile_outcome="REJECT",
            profile_reason="UNKNOWN_TOP_LEVEL_FIELD",
            security_difference="UNKNOWN_TOP_LEVEL_FIELD_REJECTED",
        ),
        _parity_case(
            "api_key_value",
            {"api_key": "TEST_ONLY_SECRET_CANARY"},
            "VALID",
            profile_outcome="REJECT",
            profile_reason="SECRET_FIELD_PRESENT",
            security_difference="SECRET_FIELD_PRESENCE_REJECTED",
        ),
        _parity_case(
            "api_key_null",
            {"api_key": None},
            "VALID",
            profile_outcome="REJECT",
            profile_reason="SECRET_FIELD_PRESENT",
            security_difference="SECRET_FIELD_PRESENCE_REJECTED",
        ),
        _parity_case(
            "client_secret_null",
            {"client_secret": None},
            "VALID",
            profile_outcome="REJECT",
            profile_reason="SECRET_FIELD_PRESENT",
            security_difference="SECRET_FIELD_PRESENCE_REJECTED",
        ),
        _parity_case(
            "nested_x_api_key_legacy_extra_bypass",
            {"extension": {"X-API-Key": "TEST_ONLY_SECRET_CANARY"}},
            "VALID",
            profile_outcome="REJECT",
            profile_reason="UNKNOWN_TOP_LEVEL_FIELD",
            security_difference="UNKNOWN_TOP_LEVEL_FIELD_REJECTED",
        ),
        _parity_case(
            "container_canary_under_allowed_field",
            {"scope": {"note": "TEST_ONLY_SECRET_CANARY"}},
            "INVALID",
            error_types=["string_type"],
            error_locations=[["scope"]],
            profile_outcome="REJECT",
            profile_reason="CONTAINER_VALUE",
            security_difference="CONTAINER_VALUE_REJECTED",
        ),
    ]
    return {
        "schema_version": "elitea.configuration-validation.legacy-parity-matrix.v1",
        "profile_id": OPENAPI_CREDENTIAL_FREE_PROFILE_ID,
        "source_revisions": {
            "indexer_worker": "d55520115dacc3dac41ee45d85a4d705b149dadf",
            "elitea_sdk": SDK_REVISION,
        },
        "test_data_notice": "TEST_ONLY values are non-secret canaries.",
        "cases": cases,
    }


def canonical_json(value: object) -> bytes:
    """Return the test-profile JSON form: compact sorted UTF-8 plus one LF."""
    return (
        json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )


def canonical_json_no_lf(value: object) -> bytes:
    """Return compact sorted UTF-8 used by the pinned SDK schema/catalog."""
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def sha256(value: bytes) -> bytes:
    return hashlib.sha256(value).digest()


def digest(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=sha256(value),
    )


def known_digest(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )


def deterministic(message: object) -> bytes:
    return message.SerializeToString(deterministic=True)


def fixture_files(case: str, settings: object, configuration_type: str) -> dict[str, bytes]:
    settings_bytes = canonical_json(settings)
    settings_digest = sha256(settings_bytes)
    settings_hex = settings_digest.hex()
    bundle_id = f"configuration-validation-{case}-bundle-v1"
    configuration_revision_id = f"configuration-revision-{case}-v1"
    bundle_version = configuration_revision_id
    entry_id = "settings"
    entry_version = configuration_revision_id
    content_id = f"configuration-validation-{case}-settings-v1"

    input_bundle = input_pb2.ExecutionInputBundleV1(
        input_bundle_id=bundle_id,
        immutable_version=bundle_version,
        entries=[
            input_pb2.ExecutionInputEntryV1(
                entry_id=entry_id,
                immutable_version=entry_version,
                semantic_role="configuration.settings",
                content=input_pb2.ScopedContentReferenceV1(
                    content_id=content_id,
                    immutable_version=entry_version,
                    media_type="application/json",
                    byte_length=len(settings_bytes),
                    digest=known_digest(settings_digest),
                    classification="synthetic",
                    required_grant_audience="elitea.runtime.input.read.v1",
                ),
            )
        ],
    )
    input_bundle_bytes = deterministic(input_bundle)
    input_bundle_digest = sha256(input_bundle_bytes)

    locator = {
        "schema_version": "elitea.runtime.fixture-bundle.v1",
        "profile": "TEST_ONLY_OFFLINE_CONFORMANCE_V1",
        "canonicalization": "ELITEA_TEST_JSON_CANONICAL_V1",
        "input_bundle_manifest": {
            "blob_name": "input-bundle.pb",
            "media_type": "application/x-protobuf",
            "byte_length": len(input_bundle_bytes),
            "digest": f"sha256:{input_bundle_digest.hex()}",
        },
        "entries": [
            {
                "entry_id": entry_id,
                "immutable_version": entry_version,
                "semantic_role": "configuration.settings",
                "content": {
                    "content_id": content_id,
                    "immutable_version": entry_version,
                    "media_type": "application/json",
                    "byte_length": len(settings_bytes),
                    "digest": f"sha256:{settings_hex}",
                    "classification": "synthetic",
                    "required_grant_audience": "elitea.runtime.input.read.v1",
                    "blob_name": f"blobs/sha256/{settings_hex}",
                },
            }
        ],
    }
    locator_bytes = canonical_json(locator)

    command = command_pb2.WorkerCommandV1(
        protocol_revision=PROTOCOL_REVISION,
        command_id=f"command-configuration-validation-{case}-v1",
        idempotency_key=f"idempotency-configuration-validation-{case}-v1",
        command_type=command_pb2.WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE,
        execution_id=f"execution-configuration-validation-{case}-v1",
        generation=1,
        dispatch_ordinal=1,
        root_execution_id=f"execution-configuration-validation-{case}-v1",
        tenant_id="tenant-conformance-v1",
        resource_project_id="project-conformance-v1",
        projection_project_id="project-conformance-v1",
        principal_ref="principal-conformance-v1",
        input_bundle_ref={
            "input_bundle_id": bundle_id,
            "immutable_version": bundle_version,
            "digest": known_digest(input_bundle_digest),
            "byte_length": len(input_bundle_bytes),
            "media_type": "application/x-protobuf",
        },
        capability_id="configuration.validate.v1",
        capability_version="1",
        resource_class="short-validation",
        isolation_class="conformance",
        priority=1,
        deadline_unix_millis=4102444800000,
        limits_revision=LIMITS_REVISION,
        configuration_validation={
            "configuration_revision_id": configuration_revision_id,
            "configuration_type": configuration_type,
            "catalog_revision": SDK_REVISION,
            "catalog_digest": known_digest(CATALOG_DIGEST),
            "schema_id": "elitea.configuration.openapi",
            "schema_revision": SDK_REVISION,
            "schema_digest": known_digest(SCHEMA_DIGEST),
            "settings_entry_id": entry_id,
        },
    )
    command_bytes = deterministic(command)
    signed = envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision=ENVELOPE_SCHEMA_REVISION,
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256,
        key_id=TEST_KEY_ID,
        worker_command_bytes=command_bytes,
        worker_command_digest=digest(command_bytes),
        signature=hmac.new(TEST_KEY, command_bytes, hashlib.sha256).digest(),
    )
    fence = common_pb2.ExecutionFenceV1(
        workload_session_id="workload-session-conformance-v1",
        claim_attempt=1,
        lease_epoch=1,
        producer_id="python-reference-conformance-v1",
        fence_token=sha256(f"test-only-fence:{case}".encode("ascii")),
    )
    execution_envelope = envelope_pb2.WorkerExecutionEnvelopeV1(
        signed_command=signed,
        fence=fence,
    )

    identity = common_pb2.ExecutionIdentityV1(
        tenant_id=command.tenant_id,
        resource_project_id=command.resource_project_id,
        projection_project_id=command.projection_project_id,
        command_id=command.command_id,
        execution_id=command.execution_id,
        generation=command.generation,
    )
    logical_output_id = (
        "configuration-validation:"
        f"{command.configuration_validation.configuration_revision_id}"
    )
    event_id = f"{command.command_id}:1"

    if case == "unsupported":
        payload = errors_pb2.RuntimeErrorV1(
            code=errors_pb2.RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY,
            safe_message="Configuration type is not supported.",
            retryable=False,
        )
        event_type = output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR
        outcome = common_pb2.EXECUTION_OUTCOME_V1_FAILED
        payload_field = "runtime_error"
    else:
        issues = []
        if case == "invalid":
            issues.append(
                validation_pb2.ConfigurationValidationIssueV1(
                    code="VALUE_NOT_ALLOWED",
                    json_pointer="/auth_type",
                    safe_message="Value is not one of the allowed choices.",
                )
            )
        payload = validation_pb2.ConfigurationValidationResultV1(
            configuration_revision_id=command.configuration_validation.configuration_revision_id,
            configuration_type=configuration_type,
            catalog_revision=SDK_REVISION,
            catalog_digest=known_digest(CATALOG_DIGEST),
            schema_id="elitea.configuration.openapi",
            schema_revision=SDK_REVISION,
            schema_digest=known_digest(SCHEMA_DIGEST),
            input_bundle_id=bundle_id,
            input_bundle_digest=known_digest(input_bundle_digest),
            settings_entry_id=entry_id,
            settings_entry_version=entry_version,
            settings_content_digest=known_digest(settings_digest),
            valid=case == "valid",
            issues=issues,
        )
        event_type = (
            output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT
        )
        outcome = common_pb2.EXECUTION_OUTCOME_V1_SUCCEEDED
        payload_field = "configuration_validation"

    payload_bytes = deterministic(payload)
    payload_digest = digest(payload_bytes)
    proposal = output_pb2.SettlementProposalV1(
        proposal_id=f"{command.command_id}:settlement",
        requested_outcome=outcome,
        terminal_logical_output_id=logical_output_id,
        terminal_event_id=event_id,
        terminal_sequence=1,
        terminal_payload_digest=payload_digest,
        prepare_idempotency_key=f"{command.command_id}:prepare-settlement",
    )
    output = output_pb2.ExecutionOutputFrameV1(
        output_schema_revision=OUTPUT_SCHEMA_REVISION,
        stream_id=f"{command.execution_id}:{command.generation}",
        identity=identity,
        fence=fence,
        logical_output_id=logical_output_id,
        event_id=event_id,
        sequence=1,
        event_type=event_type,
        occurred_at_unix_millis=TEST_OCCURRED_AT_UNIX_MILLIS,
        payload_digest=payload_digest,
        terminal=True,
        settlement_proposal=proposal,
    )
    getattr(output, payload_field).CopyFrom(payload)

    cancelled_payload = errors_pb2.RuntimeErrorV1(
        code=errors_pb2.RUNTIME_ERROR_CODE_V1_CANCELLED,
        safe_message="Execution was cancelled.",
        retryable=False,
    )
    cancelled_payload_digest = digest(deterministic(cancelled_payload))
    cancelled_output = output_pb2.ExecutionOutputFrameV1(
        output_schema_revision=OUTPUT_SCHEMA_REVISION,
        stream_id=output.stream_id,
        identity=identity,
        fence=fence,
        logical_output_id=logical_output_id,
        event_id=event_id,
        sequence=1,
        event_type=output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR,
        occurred_at_unix_millis=TEST_OCCURRED_AT_UNIX_MILLIS,
        payload_digest=cancelled_payload_digest,
        terminal=True,
        settlement_proposal=output_pb2.SettlementProposalV1(
            proposal_id=f"{command.command_id}:settlement",
            requested_outcome=common_pb2.EXECUTION_OUTCOME_V1_CANCELLED,
            terminal_logical_output_id=logical_output_id,
            terminal_event_id=event_id,
            terminal_sequence=1,
            terminal_payload_digest=cancelled_payload_digest,
            prepare_idempotency_key=f"{command.command_id}:prepare-settlement",
        ),
        runtime_error=cancelled_payload,
    )

    return {
        "settings.json": settings_bytes,
        f"blobs/sha256/{settings_hex}": settings_bytes,
        "fixture-bundle.json": locator_bytes,
        "input-bundle.pb": input_bundle_bytes,
        "envelope.pb": deterministic(execution_envelope),
        "expected-output.pb": deterministic(output),
        "expected-cancelled-output.pb": deterministic(cancelled_output),
    }


def all_files() -> dict[str, bytes]:
    cases = {
        "valid": ({}, "openapi"),
        "invalid": ({"auth_type": "Digest"}, "openapi"),
        "unsupported": ({}, "unknown-fixture-type"),
    }
    generated: dict[str, bytes] = {
        "openapi-credential-free-profile.json": canonical_json(
            openapi_credential_free_profile()
        ),
        "openapi-legacy-parity-matrix.json": canonical_json(
            openapi_legacy_parity_matrix()
        ),
        "conformance-profile.json": canonical_json(
            {
                "schema_version": "elitea.runtime.offline-conformance-profile.v1",
                "profile": "TEST_ONLY_OFFLINE_CONFORMANCE_V1",
                "warning": "Public test material. Production runtimes and servers must reject this profile.",
                "signature": {
                    "profile": "SIGNATURE_PROFILE_V1_TEST_ONLY_HMAC_SHA256",
                    "key_id": TEST_KEY_ID,
                    "public_test_key_encoding": "utf-8",
                    "public_test_key": TEST_KEY.decode("ascii"),
                    "signed_bytes": "exact worker_command_bytes before protobuf parsing",
                },
                "json_canonicalization": {
                    "profile": "ELITEA_TEST_JSON_CANONICAL_V1",
                    "encoding": "utf-8",
                    "keys": "lexicographically sorted",
                    "separators": ", and : with no surrounding whitespace",
                    "numbers": "finite JSON values only",
                    "suffix": "one LF byte",
                },
                "input_bundle_binding": {
                    "authoritative_file": "input-bundle.pb",
                    "media_type": "application/x-protobuf",
                    "encoding": "deterministic ExecutionInputBundleV1 protobuf bytes",
                    "digest_usage": "WorkerCommand input reference and validation result bind these exact bytes",
                    "fixture_bundle_json": "test-only locator cross-checked field-for-field against the verified protobuf manifest",
                },
                "fixed_clock_unix_millis": TEST_OCCURRED_AT_UNIX_MILLIS,
            }
        ),
        "conformance-limits.json": canonical_json(CONFORMANCE_LIMITS),
        "conformance-limits.pb": deterministic(
            limits_pb2.ProtocolLimitsV1(**CONFORMANCE_LIMITS)
        ),
    }
    for case, (settings, configuration_type) in cases.items():
        for name, value in fixture_files(case, settings, configuration_type).items():
            generated[f"{case}/{name}"] = value
    return generated


def write_files(root: Path, generated: dict[str, bytes]) -> None:
    for relative, value in generated.items():
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value)


def check_files(generated: dict[str, bytes]) -> int:
    mismatches = []
    for relative, expected in generated.items():
        path = CORPUS_ROOT / relative
        if not path.is_file() or path.read_bytes() != expected:
            mismatches.append(relative)
    if mismatches:
        print("configuration-validation fixtures differ:", file=sys.stderr)
        for relative in mismatches:
            print(f"  {relative}", file=sys.stderr)
        return 1

    expected_paths = {CORPUS_ROOT / relative for relative in generated}
    actual_paths = {
        path
        for case in ("valid", "invalid", "unsupported")
        for path in (CORPUS_ROOT / case).rglob("*")
        if path.is_file()
    }
    extras = sorted(actual_paths - expected_paths)
    if extras:
        print("unexpected configuration-validation fixture files:", file=sys.stderr)
        for path in extras:
            print(f"  {path.relative_to(CORPUS_ROOT)}", file=sys.stderr)
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--check",
        action="store_true",
        help="verify checked-in vectors instead of writing them",
    )
    args = parser.parse_args()
    generated = all_files()
    if args.check:
        return check_files(generated)
    write_files(CORPUS_ROOT, generated)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
