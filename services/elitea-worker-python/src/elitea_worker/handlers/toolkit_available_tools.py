"""Legacy-compatible ``toolkit.available_tools.v1`` handler kernel.

This file prepares a data-plane artifact only. It does not publish a command-bus
message, emit a gRPC output frame, or persist an artifact. Production content
storage and delivery are intentionally a later composition slice.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any

from elitea_worker.agents.sdk_adapter import EliteaSdkToolkitAdapter
from elitea_worker.execution.errors import InvalidInput


RESULT_MEDIA_TYPE = "application/vnd.elitea.toolkit-available-tools.v1+json"
RESULT_CLASSIFICATION = "tenant-confidential"


@dataclass(frozen=True, slots=True)
class ToolkitAvailableToolsRequest:
    toolkit_type: str
    input_bundle_id: str
    input_bundle_digest: bytes
    settings_entry_id: str
    settings_entry_version: str
    settings_content_digest: bytes
    settings: dict[str, Any]


@dataclass(frozen=True, slots=True)
class PreparedToolkitAvailableToolsArtifact:
    """Canonical bytes for a future scoped data-plane artifact write."""

    media_type: str
    classification: str
    content: bytes
    digest: bytes


@dataclass(frozen=True, slots=True)
class ToolkitAvailableToolsResult:
    toolkit_type: str
    input_bundle_id: str
    input_bundle_digest: bytes
    settings_entry_id: str
    settings_entry_version: str
    settings_content_digest: bytes
    artifact: PreparedToolkitAvailableToolsArtifact


class ToolkitAvailableToolsHandler:
    """Preserve the current-baseline wrapper's returned values and escaping errors."""

    def __init__(self, sdk: EliteaSdkToolkitAdapter) -> None:
        self._sdk = sdk

    def execute(self, request: ToolkitAvailableToolsRequest) -> ToolkitAvailableToolsResult:
        _validate_request(request)
        try:
            # Exactly one SDK call. Do not normalize the type/settings or move
            # toolkit-specific enumeration logic into the worker.
            value = self._sdk.get_toolkit_available_tools(
                toolkit_type=request.toolkit_type,
                settings=request.settings,
            )
        except Exception as error:  # noqa: BLE001 - exact current-baseline response semantics
            # The current Pylon wrapper catches Exception and returns this exact
            # value shape. We intentionally omit its exception log: exception
            # text is protected result data and must not become a log side
            # channel. BaseException retains normal shutdown semantics.
            value = {"tools": [], "args_schemas": {}, "error": str(error)}

        content = _canonical_json(value)
        artifact = PreparedToolkitAvailableToolsArtifact(
            media_type=RESULT_MEDIA_TYPE,
            classification=RESULT_CLASSIFICATION,
            content=content,
            digest=hashlib.sha256(content).digest(),
        )
        return ToolkitAvailableToolsResult(
            toolkit_type=request.toolkit_type,
            input_bundle_id=request.input_bundle_id,
            input_bundle_digest=request.input_bundle_digest,
            settings_entry_id=request.settings_entry_id,
            settings_entry_version=request.settings_entry_version,
            settings_content_digest=request.settings_content_digest,
            artifact=artifact,
        )


def _validate_request(request: ToolkitAvailableToolsRequest) -> None:
    required = (
        request.input_bundle_id,
        request.settings_entry_id,
        request.settings_entry_version,
    )
    digests = (request.input_bundle_digest, request.settings_content_digest)
    if (
        not isinstance(request.toolkit_type, str)
        or any(not value for value in required)
        or any(len(value) != 32 for value in digests)
        or not isinstance(request.settings, dict)
    ):
        raise InvalidInput()


def _canonical_json(value: dict[str, Any]) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
