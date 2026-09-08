from __future__ import annotations

from typing import Any

import pytest
from elitea.runtime.v1 import command_pb2, common_pb2, input_pb2, output_pb2

from elitea_worker.app import build_static_handler_registry
from elitea_worker.constants import MAX_WORKER_COMMAND_BYTES
from elitea_worker.execution.errors import UnsupportedCapability
from elitea_worker.handlers.indexing import IndexIngestHandler
from elitea_worker.handlers.toolkit_call_tool import ToolkitCallToolHandler
from elitea_worker.handlers.toolkit_available_tools import (
    ToolkitAvailableToolsHandler,
    ToolkitAvailableToolsRequest,
)
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.protocol.toolkit_available_tools import bind_result_artifact


class _SdkStub:
    def __init__(self, value: dict[str, Any]) -> None:
        self.value = value
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def get_toolkit_available_tools(
        self,
        toolkit_type: str,
        settings: dict[str, Any],
    ) -> dict[str, Any]:
        self.calls.append((toolkit_type, settings))
        return self.value


def test_bulk_settings_and_result_stay_out_of_control_and_output_frames() -> None:
    secret_canary = "TEST_ONLY_SETTINGS_CANARY_NOT_A_SECRET"
    settings = {
        "api_key": secret_canary,
        "spec": {"description": "s" * (1024 * 1024)},
    }
    sdk = _SdkStub(
        {
            "tools": [{"name": "large", "description": "r" * (1024 * 1024)}],
            "args_schemas": {},
        }
    )
    result = ToolkitAvailableToolsHandler(sdk).execute(_request(settings))

    command = command_pb2.WorkerCommandV1(
        protocol_revision="elitea.runtime.v1",
        command_id="command-1",
        idempotency_key="idempotency-1",
        command_type=command_pb2.WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS,
        execution_id="execution-1",
        generation=1,
        dispatch_ordinal=1,
        tenant_id="tenant-1",
        resource_project_id="project-1",
        projection_project_id="project-1",
        principal_ref="principal-1",
        input_bundle_ref=input_pb2.ExecutionInputBundleReferenceV1(
            input_bundle_id="bundle-1",
            immutable_version="1",
            digest=_digest(b"b" * 32),
            byte_length=256,
            media_type="application/x-protobuf",
        ),
        capability_id="toolkit.available_tools.v1",
        capability_version="1",
        resource_class="toolkit-catalog",
        isolation_class="shared",
        priority=1,
        deadline_unix_millis=1,
        limits_revision="elitea.runtime.limits.conformance.v1",
        toolkit_available_tools={
            "toolkit_type": "openapi",
            "settings_entry_id": "settings",
        },
    )
    command_bytes = command.SerializeToString(deterministic=True)
    assert len(command_bytes) < MAX_WORKER_COMMAND_BYTES
    assert secret_canary.encode() not in command_bytes
    assert b"description" not in command_bytes

    reference = bind_result_artifact(
        result,
        artifact_id="artifact-1",
        immutable_version="1",
    )
    frame = output_pb2.ExecutionOutputFrameV1(
        output_schema_revision="elitea.runtime.execution-output.v1",
        stream_id="execution-1:1",
        event_type=output_pb2.EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS_RESULT,
        toolkit_available_tools=reference,
    )
    frame_bytes = frame.SerializeToString(deterministic=True)

    assert len(result.artifact.content) > 1024 * 1024
    assert len(frame_bytes) < 1024
    assert b"r" * 1024 not in frame_bytes
    assert frame.WhichOneof("payload") == "toolkit_available_tools"
    assert reference.result_artifact.byte_length == len(result.artifact.content)
    assert bytes(reference.result_artifact.digest.value) == result.artifact.digest
    assert sdk.calls == [("openapi", settings)]


def test_static_registry_contains_exact_versioned_handler() -> None:
    toolkit = ToolkitAvailableToolsHandler(_SdkStub({"tools": [], "args_schemas": {}}))
    validation = object.__new__(ConfigurationValidationHandler)
    validation._sdk = object()
    index_ingest = object.__new__(IndexIngestHandler)
    toolkit_call_tool = object.__new__(ToolkitCallToolHandler)
    registry = build_static_handler_registry(
        validation=validation,
        toolkit_available_tools=toolkit,
        index_ingest=index_ingest,
        toolkit_call_tool=toolkit_call_tool,
    )

    assert registry.resolve("configuration.validate.v1", 1).__self__ is validation
    assert registry.resolve("toolkit.available_tools.v1", 1).__self__ is toolkit
    assert registry.resolve("index.ingest.v1", 2).__self__ is index_ingest
    assert registry.resolve("toolkit.call_tool.v1", 1).__self__ is toolkit_call_tool
    with pytest.raises(UnsupportedCapability):
        registry.resolve("index.ingest.v1", 1)
    with pytest.raises(UnsupportedCapability):
        registry.resolve("toolkit.call_tool.v1", 2)


def _request(settings: dict[str, Any]) -> ToolkitAvailableToolsRequest:
    return ToolkitAvailableToolsRequest(
        toolkit_type="openapi",
        input_bundle_id="bundle-1",
        input_bundle_digest=b"b" * 32,
        settings_entry_id="settings",
        settings_entry_version="1",
        settings_content_digest=b"s" * 32,
        settings=settings,
    )


def _digest(value: bytes) -> common_pb2.DigestV1:
    return common_pb2.DigestV1(
        algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
        value=value,
    )
