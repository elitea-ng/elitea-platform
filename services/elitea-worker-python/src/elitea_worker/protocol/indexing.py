"""Pure typed mapping for the reference-only ``index.ingest.v1`` boundary."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

from elitea.runtime.v1 import common_pb2, indexing_pb2

from elitea_worker.constants import MAX_SAFE_STRING_BYTES
from elitea_worker.execution.errors import InternalFailure, InvalidInput, ResourceExhausted
from elitea_worker.handlers.indexing import (
    IndexIngestInputBinding,
    IndexIngestRequest,
    IndexIngestResult,
    CurrentIndexTerminalStatus,
    ResolvedIndexIngestInput,
)

RESULT_MEDIA_TYPE = "application/vnd.elitea.index-ingest-result.v1+json"
RESULT_CLASSIFICATION = "tenant-confidential"
MAX_RESULT_SUMMARY_MESSAGE_BYTES = 48 * 1024
INDEX_INGEST_FAILURE_SAFE_MESSAGE = "Indexing failed before completion."

# #6876: the SDK classifies an index failure into ``error_class`` and
# ``retriable`` beside its free-form ``error``. The free-form text stays inside
# the worker (it can carry endpoints and credential-adjacent data); the typed
# pair selects one of these fixed sentences instead, so the index run's
# terminal error names the cause and its remedy rather than one generic
# sentence for a spent quota, a refused credential and a wrong parameter alike.
# Unknown or malformed values keep the generic message: no label is safer than
# a wrong one.
INDEX_INGEST_RATE_LIMITED_SAFE_MESSAGE = (
    "Indexing failed because the source is temporarily unavailable or "
    "rate-limited. Run the index again later; a credential with a higher API "
    "quota avoids the limit."
)
INDEX_INGEST_QUOTA_UNRESUMABLE_SAFE_MESSAGE = (
    "Indexing failed because the source is rate-limited and this run cannot "
    "resume from its progress. Raise the API quota for the credential, or "
    "narrow the index scope, then run the index again."
)
INDEX_INGEST_CREDENTIAL_REFUSED_SAFE_MESSAGE = (
    "Indexing failed because the source refused the toolkit credential. Check "
    "that the credential is valid and can read this source."
)
INDEX_INGEST_INPUT_REJECTED_SAFE_MESSAGE = (
    "Indexing failed because the source rejected the index settings. Check the "
    "toolkit and index parameters."
)
_SDK_ERROR_CLASS_MESSAGES = {
    "policy": INDEX_INGEST_CREDENTIAL_REFUSED_SAFE_MESSAGE,
    "input": INDEX_INGEST_INPUT_REJECTED_SAFE_MESSAGE,
    "tool_internal": INDEX_INGEST_FAILURE_SAFE_MESSAGE,
}
# The fixed sentences that name a specific cause. Only these may replace the
# live index status's generic error: every one is a constant of this module,
# never SDK text, so it needs no further projection.
CLASSIFIED_INDEX_FAILURE_MESSAGES = frozenset(
    {
        INDEX_INGEST_RATE_LIMITED_SAFE_MESSAGE,
        INDEX_INGEST_QUOTA_UNRESUMABLE_SAFE_MESSAGE,
        INDEX_INGEST_CREDENTIAL_REFUSED_SAFE_MESSAGE,
        INDEX_INGEST_INPUT_REJECTED_SAFE_MESSAGE,
    }
)

_SUMMARY_STATUS = {
    "ok": indexing_pb2.INDEX_INGEST_STATUS_V1_OK,
    "partly_indexed": indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED,
    "error": indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR,
}
_TERMINAL_STATE = {
    "created": indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_CREATED,
    "completed": indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED,
    "scheduled_reindex": (
        indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX
    ),
    "failed": indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_FAILED,
    "partly_indexed": (
        indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED
    ),
}
_CURRENT_INDEX_SUCCESS_COUNTS = re.compile(
    r"\ASuccessfully indexed \d+ documents \((\d+) chunks\)\."
)
_CURRENT_INDEX_ERROR_SKIP_MARKERS = (
    "\n  - Files with read errors (",
    "\n  - Documents with errors (",
    "\n  - Runtime skipped (errors) (",
)
_EMBEDDING_BINDING_SCHEMA = "elitea.index.embedding-binding.v2"
_MAX_EMBEDDING_IDENTITY_BYTES = 1024
_CANONICAL_UUID = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
)
_SHA256_DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")


@dataclass(frozen=True, slots=True)
class ResolvedEmbeddingBinding:
    input: ResolvedIndexIngestInput
    model_name: str
    resolved_model_group: str


def resolve_embedding_binding(
    toolkit_configuration: ResolvedIndexIngestInput,
    embedding_input: ResolvedIndexIngestInput | None,
) -> ResolvedEmbeddingBinding | None:
    toolkit = toolkit_configuration.value
    if not isinstance(toolkit, dict):
        raise InvalidInput("The index toolkit configuration is malformed.")
    settings = toolkit.get("settings")
    if not isinstance(settings, dict):
        raise InvalidInput("The index toolkit settings are malformed.")
    if "embedding_model" not in settings:
        if embedding_input is not None:
            raise InvalidInput("An unexpected embedding binding was supplied.")
        return None
    model_name = settings.get("embedding_model")
    if not _valid_embedding_identity(model_name):
        raise InvalidInput("The required embedding model is malformed.")
    if embedding_input is None:
        raise InvalidInput("The required embedding binding is absent.")
    document = embedding_input.value
    if not isinstance(document, dict) or set(document) - {
        "schema_version",
        "model_name",
        "resolved_model_group",
        "route",
        "model_project_id",
        "configuration_project_id",
        "configuration_uuid",
        "configuration_digest",
    }:
        raise InvalidInput("The embedding binding is malformed.")
    required = (
        document.get("schema_version"),
        document.get("model_name"),
        document.get("resolved_model_group"),
        document.get("route"),
    )
    model_project_id = document.get("model_project_id", 0)
    configuration_project_id = document.get("configuration_project_id", 0)
    configuration_uuid = document.get("configuration_uuid", "")
    configuration_digest = document.get("configuration_digest", "")
    has_configuration_identity = bool(
        configuration_project_id or configuration_uuid or configuration_digest
    )
    if (
        document.get("schema_version") != _EMBEDDING_BINDING_SCHEMA
        or any(not _valid_embedding_identity(value) for value in required)
        or document.get("model_name") != model_name
        or not _valid_optional_project_id(model_project_id)
        or not _valid_optional_project_id(configuration_project_id)
    ):
        raise InvalidInput("The embedding binding does not match the admitted model.")
    route = document["route"]
    resolved_model_group = document["resolved_model_group"]
    if route == "raw":
        valid_route = resolved_model_group == model_name
    elif route in {"project", "public"}:
        valid_route = _valid_prefixed_embedding_group(
            resolved_model_group,
            model_name,
        )
    else:
        valid_route = False
    if not valid_route:
        raise InvalidInput("The embedding binding is malformed.")
    if has_configuration_identity:
        if (
            configuration_project_id < 1
            or not isinstance(configuration_uuid, str)
            or not _CANONICAL_UUID.fullmatch(configuration_uuid)
            or not isinstance(configuration_digest, str)
            or not _SHA256_DIGEST.fullmatch(configuration_digest)
        ):
            raise InvalidInput("The embedding binding is malformed.")
    if (
        model_project_id
        and configuration_project_id
        and model_project_id != configuration_project_id
    ):
        raise InvalidInput("The embedding binding is malformed.")
    return ResolvedEmbeddingBinding(
        input=embedding_input,
        model_name=model_name,
        resolved_model_group=resolved_model_group,
    )


def request_from(
    command: indexing_pb2.IndexIngestCommandV1,
    *,
    input_bundle_id: str,
    input_bundle_digest: bytes,
    toolkit_configuration: ResolvedIndexIngestInput,
    tool_parameters: ResolvedIndexIngestInput,
    llm_model: ResolvedIndexIngestInput | None,
    llm_configuration: ResolvedIndexIngestInput | None,
    mcp_tokens: ResolvedIndexIngestInput | None,
    runtime_config: dict[str, Any],
    embedding_binding: ResolvedIndexIngestInput | None = None,
) -> IndexIngestRequest:
    expected = (
        (command.toolkit_configuration_entry_id, toolkit_configuration),
        (command.tool_parameters_entry_id, tool_parameters),
        (command.llm_model_entry_id, llm_model),
        (command.llm_configuration_entry_id, llm_configuration),
        (command.mcp_tokens_entry_id, mcp_tokens),
        (
            command.embedding_binding.entry_id
            if command.HasField("embedding_binding")
            else "",
            embedding_binding,
        ),
    )
    if any(not _matches(entry_id, value) for entry_id, value in expected):
        raise InvalidInput(
            "An index-ingest input does not match its command reference."
        )
    return IndexIngestRequest(
        input_bundle_id=input_bundle_id,
        input_bundle_digest=input_bundle_digest,
        toolkit_configuration=toolkit_configuration,
        tool_parameters=tool_parameters,
        llm_model=llm_model,
        llm_configuration=llm_configuration,
        mcp_tokens=mcp_tokens,
        embedding_binding=embedding_binding,
        runtime_config=runtime_config,
    )


def bind_result_artifact(
    result: IndexIngestResult,
    *,
    artifact_id: str,
    immutable_version: str,
    byte_length: int,
    digest: bytes,
) -> indexing_pb2.IndexIngestResultV1:
    """Bind a separately persisted, reviewed result projection.

    The raw trusted-memory ``sdk_result`` is intentionally not inspected or
    serialized here.
    """

    text = (artifact_id, immutable_version, result.input_bundle_id)
    if (
        any(not isinstance(value, str) or not value for value in text)
        or len(digest) != 32
    ):
        raise InvalidInput("The index-ingest result artifact identity is malformed.")
    if any(not _valid_text(value) for value in text):
        raise ResourceExhausted("The index-ingest result reference exceeds its limit.")
    if (
        isinstance(byte_length, bool)
        or not isinstance(byte_length, int)
        or byte_length < 1
        or byte_length >= 1 << 64
    ):
        raise InvalidInput("The index-ingest result artifact length is malformed.")
    message = indexing_pb2.IndexIngestResultV1(
        input_bundle_id=result.input_bundle_id,
        input_bundle_digest=_digest(result.input_bundle_digest),
        toolkit_configuration=_binding(result.toolkit_configuration),
        tool_parameters=_binding(result.tool_parameters),
        result_artifact=indexing_pb2.IndexIngestArtifactReferenceV1(
            artifact_id=artifact_id,
            immutable_version=immutable_version,
            media_type=RESULT_MEDIA_TYPE,
            byte_length=byte_length,
            digest=_digest(digest),
            classification=RESULT_CLASSIFICATION,
        ),
    )
    _copy_optional(message.llm_model, result.llm_model)
    _copy_optional(message.llm_configuration, result.llm_configuration)
    _copy_optional(message.mcp_tokens, result.mcp_tokens)
    _copy_optional(message.embedding_binding, result.embedding_binding)
    return message


def bind_result_summary(
    result: IndexIngestResult,
    terminal_status: CurrentIndexTerminalStatus | None = None,
) -> indexing_pb2.IndexIngestResultV1:
    """Project only the reviewed terminal fields from trusted SDK memory.

    The current SDK outer object can contain redeemed toolkit configuration,
    model settings and callback state. This function deliberately reads only
    ``success``/``error`` and the nested ``result.status``/``result.message``.
    No other SDK field can enter protobuf serialization.
    """

    sdk_result = result.sdk_result
    if not isinstance(sdk_result, dict):
        raise InternalFailure()
    success = sdk_result.get("success")
    if success is False:
        # The current SDK error can contain endpoint, toolkit, model or
        # credential-adjacent data. Preserve the business-failure outcome while
        # deliberately improving the legacy disclosure behavior: only this
        # fixed safe message crosses the worker boundary.
        error = sdk_result.get("error")
        if not isinstance(error, str) or not error:
            raise InternalFailure()
        status = indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR
        message = index_failure_safe_message(sdk_result)
    else:
        if success is not True:
            raise InternalFailure()
        nested = sdk_result.get("result")
        if not isinstance(nested, dict):
            raise InternalFailure()
        status = _SUMMARY_STATUS.get(nested.get("status"))
        message = nested.get("message")
        if (
            status is None
            or not isinstance(message, str)
            or not message
            or "\x00" in message
        ):
            raise InternalFailure()
        try:
            message_bytes = message.encode("utf-8")
        except UnicodeEncodeError as exc:
            raise InternalFailure() from exc
        if len(message_bytes) > MAX_RESULT_SUMMARY_MESSAGE_BYTES:
            raise ResourceExhausted(
                "The index-ingest result exceeds the approved output limit."
            )
        status, message = _normalize_current_sdk_summary(status, message)

    summary = indexing_pb2.IndexIngestSummaryV1(
        status=status,
        message=message,
    )
    if terminal_status is not None:
        terminal_state = _TERMINAL_STATE.get(terminal_status.state)
        if (
            terminal_state is None
            or isinstance(terminal_status.indexed, bool)
            or not isinstance(terminal_status.indexed, int)
            or not 0 <= terminal_status.indexed < 1 << 64
            or isinstance(terminal_status.updated, bool)
            or not isinstance(terminal_status.updated, int)
            or not 0 <= terminal_status.updated < 1 << 64
            or (
                terminal_status.reindex is not None
                and not isinstance(terminal_status.reindex, bool)
            )
        ):
            raise InternalFailure()
        summary.terminal_state = terminal_state
        summary.indexed = terminal_status.indexed
        summary.updated = terminal_status.updated
        if terminal_status.reindex is not None:
            summary.reindex = terminal_status.reindex
    elif status == indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR:
        summary.terminal_state = (
            indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_FAILED
        )
    elif status == indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED:
        summary.terminal_state = (
            indexing_pb2.INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED
        )

    bound = indexing_pb2.IndexIngestResultV1(
        input_bundle_id=result.input_bundle_id,
        input_bundle_digest=_digest(result.input_bundle_digest),
        toolkit_configuration=_binding(result.toolkit_configuration),
        tool_parameters=_binding(result.tool_parameters),
        result_summary=summary,
    )
    _copy_optional(bound.llm_model, result.llm_model)
    _copy_optional(bound.llm_configuration, result.llm_configuration)
    _copy_optional(bound.mcp_tokens, result.mcp_tokens)
    _copy_optional(bound.embedding_binding, result.embedding_binding)
    return bound


def index_failure_safe_message(sdk_result: object) -> str:
    """Map the SDK's typed failure classification to one fixed safe sentence.

    Reads only ``error_class`` and ``retriable``. ``retriable`` is the SDK's
    statement about THIS run: an infrastructure failure the run cannot resume
    from (a spent quota under Clean Index, for example) is reported as not
    retriable, and running it again unchanged would only spend the next quota
    window the same way.
    """

    if not isinstance(sdk_result, dict):
        return INDEX_INGEST_FAILURE_SAFE_MESSAGE
    error_class = sdk_result.get("error_class")
    retriable = sdk_result.get("retriable")
    if not isinstance(error_class, str) or (
        retriable is not None and not isinstance(retriable, bool)
    ):
        return INDEX_INGEST_FAILURE_SAFE_MESSAGE
    if error_class == "infrastructure":
        if retriable is False:
            return INDEX_INGEST_QUOTA_UNRESUMABLE_SAFE_MESSAGE
        return INDEX_INGEST_RATE_LIMITED_SAFE_MESSAGE
    return _SDK_ERROR_CLASS_MESSAGES.get(
        error_class, INDEX_INGEST_FAILURE_SAFE_MESSAGE
    )


def _normalize_current_sdk_summary(status: int, message: str) -> tuple[int, str]:
    """Correct a pinned-SDK success that contains parser/runtime failures.

    SDK 0.9.6 exposes its structured indexing statistics only through a
    deterministic terminal message. In particular, document parser failures
    are counted as skipped items and can otherwise be returned as ``ok`` even
    when zero chunks were produced. Keep this compatibility parser generic:
    the same Markdown/content path is shared by multiple toolkit families.
    """

    if (
        status != indexing_pb2.INDEX_INGEST_STATUS_V1_OK
        or not any(marker in message for marker in _CURRENT_INDEX_ERROR_SKIP_MARKERS)
    ):
        return status, message
    match = _CURRENT_INDEX_SUCCESS_COUNTS.match(message)
    if match is None:
        return (
            indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR,
            INDEX_INGEST_FAILURE_SAFE_MESSAGE,
        )
    if int(match.group(1)) > 0:
        return indexing_pb2.INDEX_INGEST_STATUS_V1_PARTLY_INDEXED, message
    return (
        indexing_pb2.INDEX_INGEST_STATUS_V1_ERROR,
        INDEX_INGEST_FAILURE_SAFE_MESSAGE,
    )


def _matches(entry_id: str, value: ResolvedIndexIngestInput | None) -> bool:
    if value is None:
        return not entry_id
    return bool(entry_id) and value.binding.entry_id == entry_id


def _binding(value: IndexIngestInputBinding) -> indexing_pb2.IndexIngestInputBindingV1:
    if not _valid_text(value.entry_id) or not _valid_text(value.immutable_version):
        raise ResourceExhausted("The index-ingest input binding exceeds its limit.")
    return indexing_pb2.IndexIngestInputBindingV1(
        entry_id=value.entry_id,
        immutable_version=value.immutable_version,
        content_digest=_digest(value.content_digest),
    )


def _copy_optional(
    target: indexing_pb2.IndexIngestInputBindingV1,
    value: IndexIngestInputBinding | None,
) -> None:
    if value is not None:
        target.CopyFrom(_binding(value))


def _digest(value: bytes) -> common_pb2.DigestV1:
    if not isinstance(value, bytes) or len(value) != 32:
        raise InvalidInput("An index-ingest digest binding is malformed.")
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )


def _valid_text(value: object) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        return len(value.encode("utf-8")) <= MAX_SAFE_STRING_BYTES
    except UnicodeEncodeError:
        return False


def _valid_embedding_identity(value: object) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return bool(
        len(encoded) <= _MAX_EMBEDDING_IDENTITY_BYTES
        and not any(character in value for character in ("\x00", "\r", "\n"))
    )


def _valid_optional_project_id(value: object) -> bool:
    return bool(
        not isinstance(value, bool)
        and isinstance(value, int)
        and 0 <= value < 1 << 31
    )


def _valid_prefixed_embedding_group(group: str, model: str) -> bool:
    suffix = f"_{model}"
    if not group.endswith(suffix) or len(group) <= len(suffix):
        return False
    prefix = group[: -len(suffix)]
    if not prefix.isascii() or not prefix.isdecimal():
        return False
    try:
        project_id = int(prefix, 10)
    except ValueError:
        return False
    return 0 < project_id < 1 << 31
