from elitea.runtime.v1 import agent_pb2 as _agent_pb2
from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from elitea.runtime.v1 import indexing_pb2 as _indexing_pb2
from elitea.runtime.v1 import node_event_pb2 as _node_event_pb2
from elitea.runtime.v1 import toolkit_pb2 as _toolkit_pb2
from elitea.runtime.v1 import validation_pb2 as _validation_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ExecutionOutputEventTypeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    EXECUTION_OUTPUT_EVENT_TYPE_V1_UNSPECIFIED: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS_RESULT: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT: _ClassVar[ExecutionOutputEventTypeV1]
    EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT: _ClassVar[ExecutionOutputEventTypeV1]
EXECUTION_OUTPUT_EVENT_TYPE_V1_UNSPECIFIED: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_CONFIGURATION_VALIDATION_RESULT: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_RUNTIME_ERROR: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS_RESULT: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_INDEX_INGEST_RESULT: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_NODE_EVENT: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_AGENT_EXECUTION_RESULT: ExecutionOutputEventTypeV1
EXECUTION_OUTPUT_EVENT_TYPE_V1_TOOLKIT_CALL_TOOL_RESULT: ExecutionOutputEventTypeV1

class SettlementProposalV1(_message.Message):
    __slots__ = ("proposal_id", "requested_outcome", "terminal_logical_output_id", "terminal_event_id", "terminal_sequence", "terminal_payload_digest", "prepare_idempotency_key")
    PROPOSAL_ID_FIELD_NUMBER: _ClassVar[int]
    REQUESTED_OUTCOME_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_LOGICAL_OUTPUT_ID_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_EVENT_ID_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_PAYLOAD_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PREPARE_IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    proposal_id: str
    requested_outcome: _common_pb2.ExecutionOutcomeV1
    terminal_logical_output_id: str
    terminal_event_id: str
    terminal_sequence: int
    terminal_payload_digest: _common_pb2.DigestV1
    prepare_idempotency_key: str
    def __init__(self, proposal_id: _Optional[str] = ..., requested_outcome: _Optional[_Union[_common_pb2.ExecutionOutcomeV1, str]] = ..., terminal_logical_output_id: _Optional[str] = ..., terminal_event_id: _Optional[str] = ..., terminal_sequence: _Optional[int] = ..., terminal_payload_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., prepare_idempotency_key: _Optional[str] = ...) -> None: ...

class ExecutionOutputFrameV1(_message.Message):
    __slots__ = ("output_schema_revision", "stream_id", "identity", "fence", "logical_output_id", "event_id", "sequence", "claim_handoff_watermark", "event_type", "occurred_at_unix_millis", "payload_digest", "terminal", "settlement_proposal", "configuration_validation", "runtime_error", "toolkit_available_tools", "index_ingest", "agent_execution", "toolkit_call_tool", "node_event")
    OUTPUT_SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    STREAM_ID_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    LOGICAL_OUTPUT_ID_FIELD_NUMBER: _ClassVar[int]
    EVENT_ID_FIELD_NUMBER: _ClassVar[int]
    SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_HANDOFF_WATERMARK_FIELD_NUMBER: _ClassVar[int]
    EVENT_TYPE_FIELD_NUMBER: _ClassVar[int]
    OCCURRED_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_DIGEST_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_FIELD_NUMBER: _ClassVar[int]
    SETTLEMENT_PROPOSAL_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_VALIDATION_FIELD_NUMBER: _ClassVar[int]
    RUNTIME_ERROR_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_AVAILABLE_TOOLS_FIELD_NUMBER: _ClassVar[int]
    INDEX_INGEST_FIELD_NUMBER: _ClassVar[int]
    AGENT_EXECUTION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_CALL_TOOL_FIELD_NUMBER: _ClassVar[int]
    NODE_EVENT_FIELD_NUMBER: _ClassVar[int]
    output_schema_revision: str
    stream_id: str
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    logical_output_id: str
    event_id: str
    sequence: int
    claim_handoff_watermark: int
    event_type: ExecutionOutputEventTypeV1
    occurred_at_unix_millis: int
    payload_digest: _common_pb2.DigestV1
    terminal: bool
    settlement_proposal: SettlementProposalV1
    configuration_validation: _validation_pb2.ConfigurationValidationResultV1
    runtime_error: _errors_pb2.RuntimeErrorV1
    toolkit_available_tools: _toolkit_pb2.ToolkitAvailableToolsResultV1
    index_ingest: _indexing_pb2.IndexIngestResultV1
    agent_execution: _agent_pb2.AgentExecutionResultV1
    toolkit_call_tool: _toolkit_pb2.ToolkitCallToolResultV1
    node_event: _node_event_pb2.NodeEventV1
    def __init__(self, output_schema_revision: _Optional[str] = ..., stream_id: _Optional[str] = ..., identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., logical_output_id: _Optional[str] = ..., event_id: _Optional[str] = ..., sequence: _Optional[int] = ..., claim_handoff_watermark: _Optional[int] = ..., event_type: _Optional[_Union[ExecutionOutputEventTypeV1, str]] = ..., occurred_at_unix_millis: _Optional[int] = ..., payload_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., terminal: bool = ..., settlement_proposal: _Optional[_Union[SettlementProposalV1, _Mapping]] = ..., configuration_validation: _Optional[_Union[_validation_pb2.ConfigurationValidationResultV1, _Mapping]] = ..., runtime_error: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., toolkit_available_tools: _Optional[_Union[_toolkit_pb2.ToolkitAvailableToolsResultV1, _Mapping]] = ..., index_ingest: _Optional[_Union[_indexing_pb2.IndexIngestResultV1, _Mapping]] = ..., agent_execution: _Optional[_Union[_agent_pb2.AgentExecutionResultV1, _Mapping]] = ..., toolkit_call_tool: _Optional[_Union[_toolkit_pb2.ToolkitCallToolResultV1, _Mapping]] = ..., node_event: _Optional[_Union[_node_event_pb2.NodeEventV1, _Mapping]] = ...) -> None: ...

class ExecutionOutputAckV1(_message.Message):
    __slots__ = ("stream_id", "identity", "fence", "committed_contiguous_sequence", "claim_handoff_watermark", "credit_frames", "credit_bytes", "desired_state", "rejection")
    STREAM_ID_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    COMMITTED_CONTIGUOUS_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_HANDOFF_WATERMARK_FIELD_NUMBER: _ClassVar[int]
    CREDIT_FRAMES_FIELD_NUMBER: _ClassVar[int]
    CREDIT_BYTES_FIELD_NUMBER: _ClassVar[int]
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    stream_id: str
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    committed_contiguous_sequence: int
    claim_handoff_watermark: int
    credit_frames: int
    credit_bytes: int
    desired_state: _common_pb2.DesiredExecutionStateV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, stream_id: _Optional[str] = ..., identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., committed_contiguous_sequence: _Optional[int] = ..., claim_handoff_watermark: _Optional[int] = ..., credit_frames: _Optional[int] = ..., credit_bytes: _Optional[int] = ..., desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...
