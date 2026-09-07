from elitea.runtime.v1 import agent_pb2 as _agent_pb2
from elitea.runtime.v1 import indexing_pb2 as _indexing_pb2
from elitea.runtime.v1 import input_pb2 as _input_pb2
from elitea.runtime.v1 import toolkit_pb2 as _toolkit_pb2
from elitea.runtime.v1 import validation_pb2 as _validation_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class WorkerCommandTypeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKER_COMMAND_TYPE_V1_UNSPECIFIED: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_START: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_RESUME: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_RETRY: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_RECONCILE: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_INDEX_INGEST: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC: _ClassVar[WorkerCommandTypeV1]
    WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL: _ClassVar[WorkerCommandTypeV1]
WORKER_COMMAND_TYPE_V1_UNSPECIFIED: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_START: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_RESUME: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_RETRY: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_RECONCILE: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_CONFIGURATION_VALIDATE: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_TOOLKIT_AVAILABLE_TOOLS: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_INDEX_INGEST: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC: WorkerCommandTypeV1
WORKER_COMMAND_TYPE_V1_TOOLKIT_CALL_TOOL: WorkerCommandTypeV1

class WorkerCommandV1(_message.Message):
    __slots__ = ("protocol_revision", "command_id", "idempotency_key", "command_type", "execution_id", "generation", "dispatch_ordinal", "root_execution_id", "parent_execution_id", "parent_call_id", "tenant_id", "resource_project_id", "projection_project_id", "principal_ref", "input_bundle_ref", "capability_id", "capability_version", "resource_class", "isolation_class", "priority", "deadline_unix_millis", "traceparent", "tracestate", "limits_revision", "configuration_validation", "toolkit_available_tools", "index_ingest", "agent_execution", "toolkit_call_tool")
    PROTOCOL_REVISION_FIELD_NUMBER: _ClassVar[int]
    COMMAND_ID_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    COMMAND_TYPE_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    DISPATCH_ORDINAL_FIELD_NUMBER: _ClassVar[int]
    ROOT_EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    PARENT_EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    PARENT_CALL_ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECTION_PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PRINCIPAL_REF_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_REF_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_ID_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_VERSION_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_CLASS_FIELD_NUMBER: _ClassVar[int]
    ISOLATION_CLASS_FIELD_NUMBER: _ClassVar[int]
    PRIORITY_FIELD_NUMBER: _ClassVar[int]
    DEADLINE_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    TRACEPARENT_FIELD_NUMBER: _ClassVar[int]
    TRACESTATE_FIELD_NUMBER: _ClassVar[int]
    LIMITS_REVISION_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_VALIDATION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_AVAILABLE_TOOLS_FIELD_NUMBER: _ClassVar[int]
    INDEX_INGEST_FIELD_NUMBER: _ClassVar[int]
    AGENT_EXECUTION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_CALL_TOOL_FIELD_NUMBER: _ClassVar[int]
    protocol_revision: str
    command_id: str
    idempotency_key: str
    command_type: WorkerCommandTypeV1
    execution_id: str
    generation: int
    dispatch_ordinal: int
    root_execution_id: str
    parent_execution_id: str
    parent_call_id: str
    tenant_id: str
    resource_project_id: str
    projection_project_id: str
    principal_ref: str
    input_bundle_ref: _input_pb2.ExecutionInputBundleReferenceV1
    capability_id: str
    capability_version: str
    resource_class: str
    isolation_class: str
    priority: int
    deadline_unix_millis: int
    traceparent: str
    tracestate: str
    limits_revision: str
    configuration_validation: _validation_pb2.ConfigurationValidationCommandV1
    toolkit_available_tools: _toolkit_pb2.ToolkitAvailableToolsCommandV1
    index_ingest: _indexing_pb2.IndexIngestCommandV1
    agent_execution: _agent_pb2.AgentExecutionCommandV1
    toolkit_call_tool: _toolkit_pb2.ToolkitCallToolCommandV1
    def __init__(self, protocol_revision: _Optional[str] = ..., command_id: _Optional[str] = ..., idempotency_key: _Optional[str] = ..., command_type: _Optional[_Union[WorkerCommandTypeV1, str]] = ..., execution_id: _Optional[str] = ..., generation: _Optional[int] = ..., dispatch_ordinal: _Optional[int] = ..., root_execution_id: _Optional[str] = ..., parent_execution_id: _Optional[str] = ..., parent_call_id: _Optional[str] = ..., tenant_id: _Optional[str] = ..., resource_project_id: _Optional[str] = ..., projection_project_id: _Optional[str] = ..., principal_ref: _Optional[str] = ..., input_bundle_ref: _Optional[_Union[_input_pb2.ExecutionInputBundleReferenceV1, _Mapping]] = ..., capability_id: _Optional[str] = ..., capability_version: _Optional[str] = ..., resource_class: _Optional[str] = ..., isolation_class: _Optional[str] = ..., priority: _Optional[int] = ..., deadline_unix_millis: _Optional[int] = ..., traceparent: _Optional[str] = ..., tracestate: _Optional[str] = ..., limits_revision: _Optional[str] = ..., configuration_validation: _Optional[_Union[_validation_pb2.ConfigurationValidationCommandV1, _Mapping]] = ..., toolkit_available_tools: _Optional[_Union[_toolkit_pb2.ToolkitAvailableToolsCommandV1, _Mapping]] = ..., index_ingest: _Optional[_Union[_indexing_pb2.IndexIngestCommandV1, _Mapping]] = ..., agent_execution: _Optional[_Union[_agent_pb2.AgentExecutionCommandV1, _Mapping]] = ..., toolkit_call_tool: _Optional[_Union[_toolkit_pb2.ToolkitCallToolCommandV1, _Mapping]] = ...) -> None: ...
