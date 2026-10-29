from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RunStepKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RUN_STEP_KIND_V1_UNSPECIFIED: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_MODEL_CALL: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_TOOL_CALL: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_RETRIEVAL: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_APPROVAL: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_DELEGATION: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_CAP: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_ERROR: _ClassVar[RunStepKindV1]
    RUN_STEP_KIND_V1_OUTPUT: _ClassVar[RunStepKindV1]
RUN_STEP_KIND_V1_UNSPECIFIED: RunStepKindV1
RUN_STEP_KIND_V1_MODEL_CALL: RunStepKindV1
RUN_STEP_KIND_V1_TOOL_CALL: RunStepKindV1
RUN_STEP_KIND_V1_RETRIEVAL: RunStepKindV1
RUN_STEP_KIND_V1_APPROVAL: RunStepKindV1
RUN_STEP_KIND_V1_DELEGATION: RunStepKindV1
RUN_STEP_KIND_V1_CAP: RunStepKindV1
RUN_STEP_KIND_V1_ERROR: RunStepKindV1
RUN_STEP_KIND_V1_OUTPUT: RunStepKindV1

class RunStepContentV1(_message.Message):
    __slots__ = ("content", "media_type", "truncated", "original_size_bytes")
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    content: bytes
    media_type: str
    truncated: bool
    original_size_bytes: int
    def __init__(self, content: _Optional[bytes] = ..., media_type: _Optional[str] = ..., truncated: _Optional[bool] = ..., original_size_bytes: _Optional[int] = ...) -> None: ...

class RetrievedDocumentV1(_message.Message):
    __slots__ = ("document_id", "source", "score", "rank")
    DOCUMENT_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    SCORE_FIELD_NUMBER: _ClassVar[int]
    RANK_FIELD_NUMBER: _ClassVar[int]
    document_id: str
    source: str
    score: float
    rank: int
    def __init__(self, document_id: _Optional[str] = ..., source: _Optional[str] = ..., score: _Optional[float] = ..., rank: _Optional[int] = ...) -> None: ...

class RetrievalSummaryV1(_message.Message):
    __slots__ = ("index_name", "query", "filters_json", "top_k", "documents")
    INDEX_NAME_FIELD_NUMBER: _ClassVar[int]
    QUERY_FIELD_NUMBER: _ClassVar[int]
    FILTERS_JSON_FIELD_NUMBER: _ClassVar[int]
    TOP_K_FIELD_NUMBER: _ClassVar[int]
    DOCUMENTS_FIELD_NUMBER: _ClassVar[int]
    index_name: str
    query: str
    filters_json: str
    top_k: int
    documents: _containers.RepeatedCompositeFieldContainer[RetrievedDocumentV1]
    def __init__(self, index_name: _Optional[str] = ..., query: _Optional[str] = ..., filters_json: _Optional[str] = ..., top_k: _Optional[int] = ..., documents: _Optional[_Iterable[_Union[RetrievedDocumentV1, _Mapping]]] = ...) -> None: ...

class RunStepV1(_message.Message):
    __slots__ = ("step_id", "kind", "started_at_unix_ms", "ended_at_unix_ms", "node_id", "name", "toolkit_id", "summary_json", "retrieval", "inputs", "outputs", "cited_step_ids", "cited_document_ids")
    STEP_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    ENDED_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    NODE_ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_JSON_FIELD_NUMBER: _ClassVar[int]
    RETRIEVAL_FIELD_NUMBER: _ClassVar[int]
    INPUTS_FIELD_NUMBER: _ClassVar[int]
    OUTPUTS_FIELD_NUMBER: _ClassVar[int]
    CITED_STEP_IDS_FIELD_NUMBER: _ClassVar[int]
    CITED_DOCUMENT_IDS_FIELD_NUMBER: _ClassVar[int]
    step_id: str
    kind: RunStepKindV1
    started_at_unix_ms: int
    ended_at_unix_ms: int
    node_id: str
    name: str
    toolkit_id: str
    summary_json: str
    retrieval: RetrievalSummaryV1
    inputs: RunStepContentV1
    outputs: RunStepContentV1
    cited_step_ids: _containers.RepeatedScalarFieldContainer[str]
    cited_document_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, step_id: _Optional[str] = ..., kind: _Optional[_Union[RunStepKindV1, str]] = ..., started_at_unix_ms: _Optional[int] = ..., ended_at_unix_ms: _Optional[int] = ..., node_id: _Optional[str] = ..., name: _Optional[str] = ..., toolkit_id: _Optional[str] = ..., summary_json: _Optional[str] = ..., retrieval: _Optional[_Union[RetrievalSummaryV1, _Mapping]] = ..., inputs: _Optional[_Union[RunStepContentV1, _Mapping]] = ..., outputs: _Optional[_Union[RunStepContentV1, _Mapping]] = ..., cited_step_ids: _Optional[_Iterable[str]] = ..., cited_document_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class ReportRunStepsRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "steps")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    STEPS_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    steps: _containers.RepeatedCompositeFieldContainer[RunStepV1]
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., steps: _Optional[_Iterable[_Union[RunStepV1, _Mapping]]] = ...) -> None: ...

class ReportRunStepsResponseV1(_message.Message):
    __slots__ = ("rejection", "accepted")
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    ACCEPTED_FIELD_NUMBER: _ClassVar[int]
    rejection: _errors_pb2.RuntimeErrorV1
    accepted: int
    def __init__(self, rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., accepted: _Optional[int] = ...) -> None: ...
