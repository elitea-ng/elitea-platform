"""Immutable build and first-slice compatibility pins."""

from __future__ import annotations

CONFIGURATION_VALIDATION_CAPABILITY_ID = "configuration.validate.v1"
TOOLKIT_AVAILABLE_TOOLS_CAPABILITY_ID = "toolkit.available_tools.v1"
INDEX_INGEST_CAPABILITY_ID = "index.ingest.v1"
INDEX_INGEST_CAPABILITY_VERSION = "2"
TOOLKIT_CALL_TOOL_CAPABILITY_ID = "toolkit.call_tool.v1"
TOOLKIT_CALL_TOOL_CAPABILITY_VERSION = "1"
AGENT_EXECUTE_APPLICATION_CAPABILITY_ID = "agent.execute.application.v1"
AGENT_EXECUTE_ADHOC_CAPABILITY_ID = "agent.execute.adhoc.v1"
AGENT_EXECUTION_CAPABILITY_VERSION = "1"
AGENT_EXECUTION_REQUEST_ROLE = "agent.execution_request"
AGENT_INPUT_MEDIA_TYPE = "application/vnd.elitea.agent-execution-input.v1+protobuf"
# Backward-compatible name used by the first validation slice.
CAPABILITY_ID = CONFIGURATION_VALIDATION_CAPABILITY_ID
RUNTIME_IMPLEMENTATION = "elitea-worker-python"
RUNTIME_VERSION = "0.1.0"

# The admitted current-runtime SDK is distribution 0.9.8. The standalone
# worker admits one exact reviewed source artifact rather than a floating
# branch or an unverified same-version rebuild.
SDK_SOURCE_REVISION = "b5113a129329b85d23c2d5c2bf55f18e307414ec"
SDK_DISTRIBUTION_VERSION = "0.9.8"
SDK_SOURCE_ARCHIVE_SHA256 = (
    "df398b4a378e7b88ea3b72511e31de13a17a02159c69fbada6f0b948a0ecf2d7"
)
SDK_SOURCE_PATCH_REVISIONS = (
    "5c9409779ac0a55f8bf74f6ef438977089187a14",
    "07ddc5cd67a0c747f83ca0e26421cde71bd97d9d",
)
SDK_PACKAGE_TREE_SHA256 = (
    "969b129405a279a95721ad7116c9c5cc82e1c7dd13df24207fbd5d9980488f72"
)
# The configuration catalog has its own compatibility revision. SDK 0.9.8
# preserves the exact admitted catalog digest, so upgrading the runtime must
# not invalidate already-admitted configuration-validation envelopes.
CONFIGURATION_CATALOG_REVISION = "a78d3654f99d8ff89ca7233f20a66d676e564f79"
CONFIGURATION_CATALOG_SHA256 = (
    "4a96e3ab8e3842ebf2645a851aeb12e3e2343f28e7d024c1a2960eb4ec254351"
)
# Index types remain derived from the exact admitted SDK source.
INDEX_TYPES_SOURCE_REVISION = SDK_SOURCE_REVISION

JSON_MEDIA_TYPES = frozenset({"application/json", "application/json; charset=utf-8"})
SCOPED_INPUT_MEDIA_TYPES = JSON_MEDIA_TYPES | frozenset({AGENT_INPUT_MEDIA_TYPE})
MAX_ENVELOPE_BYTES = 64 * 1024
MAX_WORKER_COMMAND_BYTES = 32 * 1024
MAX_SIGNED_ENVELOPE_BYTES = 48 * 1024
MAX_MANIFEST_BYTES = 64 * 1024
MAX_GRPC_REQUEST_BYTES = 64 * 1024
MAX_GRPC_RESPONSE_BYTES = 80 * 1024
MAX_SETTINGS_BYTES = 256 * 1024
# One tool result carried inline on the output data plane. It is a fraction
# of the 80 KiB gRPC response bound so that the frame it rides in, with its
# identity, fence and settlement proposal, still fits. A larger result is
# reported truncated, never silently shortened.
MAX_TOOL_RESULT_BYTES = 48 * 1024
MAX_AGENT_INPUT_BYTES = 1024 * 1024
MAX_BUNDLE_ENTRIES = 16
MAX_ISSUES = 64
MAX_JSON_DEPTH = 64
MAX_STRING_BYTES = 64 * 1024
MAX_SAFE_STRING_BYTES = 256

ENVELOPE_SCHEMA_REVISION = "elitea.runtime.signed-worker-command.v1"
PROTOCOL_REVISION = "elitea.runtime.v1"
CAPABILITY_VERSION = "1"
LIMITS_REVISION = "elitea.runtime.limits.conformance.v1"
CLAIM_LEASE_TTL_MILLIS = 30_000
MAX_LEASE_POLL_INTERVAL_MILLIS = 10_000
MIN_REDIS_RECLAIM_IDLE_MILLIS = 2 * CLAIM_LEASE_TTL_MILLIS
OUTPUT_SCHEMA_REVISION = "elitea.runtime.execution-output.v1"
CONFORMANCE_HMAC_KEY_ID = "elitea-runtime-v1-conformance-hmac"
CONFORMANCE_HMAC_KEY = b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"
CONFORMANCE_OCCURRED_AT_UNIX_MILLIS = 1_700_000_000_000
