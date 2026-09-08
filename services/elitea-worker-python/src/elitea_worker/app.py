"""Explicit composition for the first offline and delivered validation slices."""

from __future__ import annotations

from pathlib import Path

from elitea.runtime.v1 import output_pb2

from elitea_worker.agents.sdk_adapter import EliteaSdkAdapter
from elitea_worker.constants import (
    CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
    INDEX_INGEST_CAPABILITY_ID,
    INDEX_INGEST_CAPABILITY_VERSION,
    TOOLKIT_CALL_TOOL_CAPABILITY_ID,
    TOOLKIT_CALL_TOOL_CAPABILITY_VERSION,
)
from elitea_worker.execution.delivery import (
    AgentExecutionDeliveryProcessor,
    ConfigurationValidationDeliveryProcessor,
    IndexIngestDeliveryProcessor,
    ToolkitCallToolDeliveryProcessor,
)
from elitea_worker.execution.errors import WorkerError
from elitea_worker.execution.registry import CapabilityRegistration, CapabilityRegistry
from elitea_worker.fixtures.bundle import FixtureBundle
from elitea_worker.handlers.indexing import IndexIngestHandler
from elitea_worker.handlers.toolkit_available_tools import ToolkitAvailableToolsHandler
from elitea_worker.handlers.toolkit_call_tool import ToolkitCallToolHandler
from elitea_worker.handlers.validation import ConfigurationValidationHandler
from elitea_worker.protocol.codec import (
    VerifiedWorkerCommand,
    TestOnlyConformanceHmacAuthenticator,
    build_output_frame,
    read_and_verify_envelope,
    validation_request_from,
    write_output_frame,
)


def build_static_handler_registry(
    *,
    validation: ConfigurationValidationHandler,
    toolkit_available_tools: ToolkitAvailableToolsHandler,
    index_ingest: IndexIngestHandler,
    toolkit_call_tool: ToolkitCallToolHandler,
) -> CapabilityRegistry:
    """Return the compile-time handler set; runtime kwargs cannot add code."""

    return CapabilityRegistry(
        (
            CapabilityRegistration("configuration.validate.v1", 1, validation.execute),
            CapabilityRegistration(
                "toolkit.available_tools.v1",
                1,
                toolkit_available_tools.execute,
            ),
            CapabilityRegistration(
                INDEX_INGEST_CAPABILITY_ID,
                int(INDEX_INGEST_CAPABILITY_VERSION),
                index_ingest.execute,
            ),
            CapabilityRegistration(
                TOOLKIT_CALL_TOOL_CAPABILITY_ID,
                int(TOOLKIT_CALL_TOOL_CAPABILITY_VERSION),
                toolkit_call_tool.execute,
            ),
        )
    )


class OfflineValidationWorker:
    """Conformance-only composition; it has no Redis or network dependency."""

    def __init__(self) -> None:
        self._handler = ConfigurationValidationHandler(EliteaSdkAdapter())
        self._authenticator = TestOnlyConformanceHmacAuthenticator()

    def validate_envelope(self, envelope_path: Path) -> VerifiedWorkerCommand:
        verified = read_and_verify_envelope(
            envelope_path,
            authenticator=self._authenticator,
        )
        command = verified.command.configuration_validation
        self._handler.validate_binding(
            configuration_type=command.configuration_type,
            catalog_revision=command.catalog_revision,
            catalog_digest=bytes(command.catalog_digest.value),
            schema_id=command.schema_id,
            schema_revision=command.schema_revision,
            schema_digest=bytes(command.schema_digest.value),
        )
        return verified

    def execute(
        self,
        *,
        envelope_path: Path,
        fixture_bundle_path: Path,
        output_path: Path,
    ) -> output_pb2.ExecutionOutputFrameV1:
        verified = read_and_verify_envelope(
            envelope_path,
            authenticator=self._authenticator,
        )
        try:
            reference = verified.command.input_bundle_ref
            bundle = FixtureBundle.load(
                fixture_bundle_path,
                expected_bundle_id=reference.input_bundle_id,
                expected_digest=bytes(reference.digest.value),
                expected_bundle_version=reference.immutable_version,
                expected_byte_length=reference.byte_length,
            )
            resolved = bundle.resolve_json(
                verified.command.configuration_validation.settings_entry_id
            )
            request = validation_request_from(
                verified,
                input_bundle_id=resolved.bundle_id,
                input_bundle_digest=resolved.bundle_digest,
                settings_entry_version=resolved.entry.immutable_version,
                settings_content_digest=resolved.entry.content.digest,
                settings=resolved.json_value,
            )
            outcome = self._handler.execute(request)
        except WorkerError as error:
            outcome = error
        frame = build_output_frame(
            verified,
            outcome,
            occurred_at_unix_millis=CONFORMANCE_OCCURRED_AT_UNIX_MILLIS,
        )
        write_output_frame(output_path, frame)
        return frame


__all__ = [
    "AgentExecutionDeliveryProcessor",
    "ConfigurationValidationDeliveryProcessor",
    "IndexIngestDeliveryProcessor",
    "OfflineValidationWorker",
    "ToolkitCallToolDeliveryProcessor",
    "build_static_handler_registry",
]
