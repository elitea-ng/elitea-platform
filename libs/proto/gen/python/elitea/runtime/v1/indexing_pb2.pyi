from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class IndexIngestStatusV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    INDEX_INGEST_STATUS_V1_UNSPECIFIED: _ClassVar[IndexIngestStatusV1]
    INDEX_INGEST_STATUS_V1_OK: _ClassVar[IndexIngestStatusV1]
    INDEX_INGEST_STATUS_V1_PARTLY_INDEXED: _ClassVar[IndexIngestStatusV1]
    INDEX_INGEST_STATUS_V1_ERROR: _ClassVar[IndexIngestStatusV1]

class IndexIngestTerminalStateV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    INDEX_INGEST_TERMINAL_STATE_V1_UNSPECIFIED: _ClassVar[IndexIngestTerminalStateV1]
    INDEX_INGEST_TERMINAL_STATE_V1_CREATED: _ClassVar[IndexIngestTerminalStateV1]
    INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED: _ClassVar[IndexIngestTerminalStateV1]
    INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX: _ClassVar[IndexIngestTerminalStateV1]
    INDEX_INGEST_TERMINAL_STATE_V1_FAILED: _ClassVar[IndexIngestTerminalStateV1]
    INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED: _ClassVar[IndexIngestTerminalStateV1]
INDEX_INGEST_STATUS_V1_UNSPECIFIED: IndexIngestStatusV1
INDEX_INGEST_STATUS_V1_OK: IndexIngestStatusV1
INDEX_INGEST_STATUS_V1_PARTLY_INDEXED: IndexIngestStatusV1
INDEX_INGEST_STATUS_V1_ERROR: IndexIngestStatusV1
INDEX_INGEST_TERMINAL_STATE_V1_UNSPECIFIED: IndexIngestTerminalStateV1
INDEX_INGEST_TERMINAL_STATE_V1_CREATED: IndexIngestTerminalStateV1
INDEX_INGEST_TERMINAL_STATE_V1_COMPLETED: IndexIngestTerminalStateV1
INDEX_INGEST_TERMINAL_STATE_V1_SCHEDULED_REINDEX: IndexIngestTerminalStateV1
INDEX_INGEST_TERMINAL_STATE_V1_FAILED: IndexIngestTerminalStateV1
INDEX_INGEST_TERMINAL_STATE_V1_PARTLY_INDEXED: IndexIngestTerminalStateV1

class IndexIngestCommandV1(_message.Message):
    __slots__ = ("toolkit_configuration_entry_id", "tool_parameters_entry_id", "llm_model_entry_id", "llm_configuration_entry_id", "mcp_tokens_entry_id", "client_stream_id", "client_message_id", "sio_event", "embedding_binding", "initiator")
    TOOLKIT_CONFIGURATION_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    TOOL_PARAMETERS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    LLM_MODEL_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    LLM_CONFIGURATION_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    MCP_TOKENS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_STREAM_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_MESSAGE_ID_FIELD_NUMBER: _ClassVar[int]
    SIO_EVENT_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_BINDING_FIELD_NUMBER: _ClassVar[int]
    INITIATOR_FIELD_NUMBER: _ClassVar[int]
    toolkit_configuration_entry_id: str
    tool_parameters_entry_id: str
    llm_model_entry_id: str
    llm_configuration_entry_id: str
    mcp_tokens_entry_id: str
    client_stream_id: str
    client_message_id: str
    sio_event: str
    embedding_binding: IndexIngestInputBindingV1
    initiator: str
    def __init__(self, toolkit_configuration_entry_id: _Optional[str] = ..., tool_parameters_entry_id: _Optional[str] = ..., llm_model_entry_id: _Optional[str] = ..., llm_configuration_entry_id: _Optional[str] = ..., mcp_tokens_entry_id: _Optional[str] = ..., client_stream_id: _Optional[str] = ..., client_message_id: _Optional[str] = ..., sio_event: _Optional[str] = ..., embedding_binding: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., initiator: _Optional[str] = ...) -> None: ...

class IndexIngestInputBindingV1(_message.Message):
    __slots__ = ("entry_id", "immutable_version", "content_digest")
    ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    entry_id: str
    immutable_version: str
    content_digest: _common_pb2.DigestV1
    def __init__(self, entry_id: _Optional[str] = ..., immutable_version: _Optional[str] = ..., content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ...) -> None: ...

class IndexIngestArtifactReferenceV1(_message.Message):
    __slots__ = ("artifact_id", "immutable_version", "media_type", "byte_length", "digest", "classification")
    ARTIFACT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    BYTE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    CLASSIFICATION_FIELD_NUMBER: _ClassVar[int]
    artifact_id: str
    immutable_version: str
    media_type: str
    byte_length: int
    digest: _common_pb2.DigestV1
    classification: str
    def __init__(self, artifact_id: _Optional[str] = ..., immutable_version: _Optional[str] = ..., media_type: _Optional[str] = ..., byte_length: _Optional[int] = ..., digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., classification: _Optional[str] = ...) -> None: ...

class IndexIngestSummaryV1(_message.Message):
    __slots__ = ("status", "message", "terminal_state", "indexed", "updated", "reindex", "indexed_documents", "indexed_chunks", "failed_chunks", "skipped", "embedding_model", "embedding_dimension")
    class SkippedEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: int
        def __init__(self, key: _Optional[str] = ..., value: _Optional[int] = ...) -> None: ...
    STATUS_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_STATE_FIELD_NUMBER: _ClassVar[int]
    INDEXED_FIELD_NUMBER: _ClassVar[int]
    UPDATED_FIELD_NUMBER: _ClassVar[int]
    REINDEX_FIELD_NUMBER: _ClassVar[int]
    INDEXED_DOCUMENTS_FIELD_NUMBER: _ClassVar[int]
    INDEXED_CHUNKS_FIELD_NUMBER: _ClassVar[int]
    FAILED_CHUNKS_FIELD_NUMBER: _ClassVar[int]
    SKIPPED_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_MODEL_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_DIMENSION_FIELD_NUMBER: _ClassVar[int]
    status: IndexIngestStatusV1
    message: str
    terminal_state: IndexIngestTerminalStateV1
    indexed: int
    updated: int
    reindex: bool
    indexed_documents: int
    indexed_chunks: int
    failed_chunks: int
    skipped: _containers.ScalarMap[str, int]
    embedding_model: str
    embedding_dimension: int
    def __init__(self, status: _Optional[_Union[IndexIngestStatusV1, str]] = ..., message: _Optional[str] = ..., terminal_state: _Optional[_Union[IndexIngestTerminalStateV1, str]] = ..., indexed: _Optional[int] = ..., updated: _Optional[int] = ..., reindex: bool = ..., indexed_documents: _Optional[int] = ..., indexed_chunks: _Optional[int] = ..., failed_chunks: _Optional[int] = ..., skipped: _Optional[_Mapping[str, int]] = ..., embedding_model: _Optional[str] = ..., embedding_dimension: _Optional[int] = ...) -> None: ...

class IndexIngestResultV1(_message.Message):
    __slots__ = ("input_bundle_id", "input_bundle_digest", "toolkit_configuration", "tool_parameters", "llm_model", "llm_configuration", "mcp_tokens", "result_artifact", "result_summary", "embedding_binding")
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_CONFIGURATION_FIELD_NUMBER: _ClassVar[int]
    TOOL_PARAMETERS_FIELD_NUMBER: _ClassVar[int]
    LLM_MODEL_FIELD_NUMBER: _ClassVar[int]
    LLM_CONFIGURATION_FIELD_NUMBER: _ClassVar[int]
    MCP_TOKENS_FIELD_NUMBER: _ClassVar[int]
    RESULT_ARTIFACT_FIELD_NUMBER: _ClassVar[int]
    RESULT_SUMMARY_FIELD_NUMBER: _ClassVar[int]
    EMBEDDING_BINDING_FIELD_NUMBER: _ClassVar[int]
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    toolkit_configuration: IndexIngestInputBindingV1
    tool_parameters: IndexIngestInputBindingV1
    llm_model: IndexIngestInputBindingV1
    llm_configuration: IndexIngestInputBindingV1
    mcp_tokens: IndexIngestInputBindingV1
    result_artifact: IndexIngestArtifactReferenceV1
    result_summary: IndexIngestSummaryV1
    embedding_binding: IndexIngestInputBindingV1
    def __init__(self, input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., toolkit_configuration: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., tool_parameters: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., llm_model: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., llm_configuration: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., mcp_tokens: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ..., result_artifact: _Optional[_Union[IndexIngestArtifactReferenceV1, _Mapping]] = ..., result_summary: _Optional[_Union[IndexIngestSummaryV1, _Mapping]] = ..., embedding_binding: _Optional[_Union[IndexIngestInputBindingV1, _Mapping]] = ...) -> None: ...
