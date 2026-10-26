from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class WorkspaceRefusalCodeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKSPACE_REFUSAL_CODE_V1_UNSPECIFIED: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_NOT_A_WORKSPACE_TURN: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_REF_NOT_ALLOWED: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_SEGMENT_MISSING: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_CHECKSUM_MISMATCH: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_UNSUPPORTED_OBJECT_FORMAT: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_SEGMENT_TOO_LARGE: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_QUOTA_EXCEEDED: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_NO_FORGE_CREDENTIAL: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_PULL_REQUESTS_DISABLED: _ClassVar[WorkspaceRefusalCodeV1]

class WorkspaceExecutorEventKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKSPACE_EXECUTOR_EVENT_KIND_V1_UNSPECIFIED: _ClassVar[WorkspaceExecutorEventKindV1]
    WORKSPACE_EXECUTOR_EVENT_KIND_V1_SCAN_BLOCKED: _ClassVar[WorkspaceExecutorEventKindV1]
    WORKSPACE_EXECUTOR_EVENT_KIND_V1_PULL_REQUEST_OPENED: _ClassVar[WorkspaceExecutorEventKindV1]
    WORKSPACE_EXECUTOR_EVENT_KIND_V1_BRANCH_PUSHED: _ClassVar[WorkspaceExecutorEventKindV1]

class WorkspaceChangeStatusV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKSPACE_CHANGE_STATUS_V1_UNSPECIFIED: _ClassVar[WorkspaceChangeStatusV1]
    WORKSPACE_CHANGE_STATUS_V1_ADDED: _ClassVar[WorkspaceChangeStatusV1]
    WORKSPACE_CHANGE_STATUS_V1_MODIFIED: _ClassVar[WorkspaceChangeStatusV1]
    WORKSPACE_CHANGE_STATUS_V1_DELETED: _ClassVar[WorkspaceChangeStatusV1]
    WORKSPACE_CHANGE_STATUS_V1_RENAMED: _ClassVar[WorkspaceChangeStatusV1]

class WorkspaceChangeScopeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKSPACE_CHANGE_SCOPE_V1_UNSPECIFIED: _ClassVar[WorkspaceChangeScopeV1]
    WORKSPACE_CHANGE_SCOPE_V1_TURN: _ClassVar[WorkspaceChangeScopeV1]
    WORKSPACE_CHANGE_SCOPE_V1_CUMULATIVE: _ClassVar[WorkspaceChangeScopeV1]
WORKSPACE_REFUSAL_CODE_V1_UNSPECIFIED: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_NOT_A_WORKSPACE_TURN: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_REF_NOT_ALLOWED: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_SEGMENT_MISSING: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_CHECKSUM_MISMATCH: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_UNSUPPORTED_OBJECT_FORMAT: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_SEGMENT_TOO_LARGE: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_QUOTA_EXCEEDED: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_NO_FORGE_CREDENTIAL: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_PULL_REQUESTS_DISABLED: WorkspaceRefusalCodeV1
WORKSPACE_EXECUTOR_EVENT_KIND_V1_UNSPECIFIED: WorkspaceExecutorEventKindV1
WORKSPACE_EXECUTOR_EVENT_KIND_V1_SCAN_BLOCKED: WorkspaceExecutorEventKindV1
WORKSPACE_EXECUTOR_EVENT_KIND_V1_PULL_REQUEST_OPENED: WorkspaceExecutorEventKindV1
WORKSPACE_EXECUTOR_EVENT_KIND_V1_BRANCH_PUSHED: WorkspaceExecutorEventKindV1
WORKSPACE_CHANGE_STATUS_V1_UNSPECIFIED: WorkspaceChangeStatusV1
WORKSPACE_CHANGE_STATUS_V1_ADDED: WorkspaceChangeStatusV1
WORKSPACE_CHANGE_STATUS_V1_MODIFIED: WorkspaceChangeStatusV1
WORKSPACE_CHANGE_STATUS_V1_DELETED: WorkspaceChangeStatusV1
WORKSPACE_CHANGE_STATUS_V1_RENAMED: WorkspaceChangeStatusV1
WORKSPACE_CHANGE_SCOPE_V1_UNSPECIFIED: WorkspaceChangeScopeV1
WORKSPACE_CHANGE_SCOPE_V1_TURN: WorkspaceChangeScopeV1
WORKSPACE_CHANGE_SCOPE_V1_CUMULATIVE: WorkspaceChangeScopeV1

class WorkspaceClaimV1(_message.Message):
    __slots__ = ("identity", "fence", "claim_id", "thread_uuid", "epoch")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    THREAD_UUID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    claim_id: str
    thread_uuid: str
    epoch: int
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., claim_id: _Optional[str] = ..., thread_uuid: _Optional[str] = ..., epoch: _Optional[int] = ...) -> None: ...

class WorkspaceRefV1(_message.Message):
    __slots__ = ("name", "sha")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SHA_FIELD_NUMBER: _ClassVar[int]
    name: str
    sha: str
    def __init__(self, name: _Optional[str] = ..., sha: _Optional[str] = ...) -> None: ...

class WorkspaceRefUpdateV1(_message.Message):
    __slots__ = ("name", "old", "new")
    NAME_FIELD_NUMBER: _ClassVar[int]
    OLD_FIELD_NUMBER: _ClassVar[int]
    NEW_FIELD_NUMBER: _ClassVar[int]
    name: str
    old: str
    new: str
    def __init__(self, name: _Optional[str] = ..., old: _Optional[str] = ..., new: _Optional[str] = ...) -> None: ...

class WorkspaceRefusalV1(_message.Message):
    __slots__ = ("code", "current")
    CODE_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    code: WorkspaceRefusalCodeV1
    current: _containers.RepeatedCompositeFieldContainer[WorkspaceRefV1]
    def __init__(self, code: _Optional[_Union[WorkspaceRefusalCodeV1, str]] = ..., current: _Optional[_Iterable[_Union[WorkspaceRefV1, _Mapping]]] = ...) -> None: ...

class WorkspaceSegmentV1(_message.Message):
    __slots__ = ("seq", "kind", "sha256", "bytes", "prerequisites", "refs")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    PREREQUISITES_FIELD_NUMBER: _ClassVar[int]
    REFS_FIELD_NUMBER: _ClassVar[int]
    seq: int
    kind: str
    sha256: str
    bytes: int
    prerequisites: _containers.RepeatedScalarFieldContainer[str]
    refs: _containers.RepeatedCompositeFieldContainer[WorkspaceRefV1]
    def __init__(self, seq: _Optional[int] = ..., kind: _Optional[str] = ..., sha256: _Optional[str] = ..., bytes: _Optional[int] = ..., prerequisites: _Optional[_Iterable[str]] = ..., refs: _Optional[_Iterable[_Union[WorkspaceRefV1, _Mapping]]] = ...) -> None: ...

class WorkspaceFetchPlanRequestV1(_message.Message):
    __slots__ = ("claim", "since_seq")
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    SINCE_SEQ_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    since_seq: int
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ..., since_seq: _Optional[int] = ...) -> None: ...

class WorkspaceFetchPlanV1(_message.Message):
    __slots__ = ("head_seq", "refs", "segments")
    HEAD_SEQ_FIELD_NUMBER: _ClassVar[int]
    REFS_FIELD_NUMBER: _ClassVar[int]
    SEGMENTS_FIELD_NUMBER: _ClassVar[int]
    head_seq: int
    refs: _containers.RepeatedCompositeFieldContainer[WorkspaceRefV1]
    segments: _containers.RepeatedCompositeFieldContainer[WorkspaceSegmentV1]
    def __init__(self, head_seq: _Optional[int] = ..., refs: _Optional[_Iterable[_Union[WorkspaceRefV1, _Mapping]]] = ..., segments: _Optional[_Iterable[_Union[WorkspaceSegmentV1, _Mapping]]] = ...) -> None: ...

class WorkspaceSegmentEndV1(_message.Message):
    __slots__ = ("sha256", "bytes")
    SHA256_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    sha256: str
    bytes: int
    def __init__(self, sha256: _Optional[str] = ..., bytes: _Optional[int] = ...) -> None: ...

class WorkspaceFetchFrameV1(_message.Message):
    __slots__ = ("plan", "segment_chunk", "segment_end", "refusal", "rejection")
    PLAN_FIELD_NUMBER: _ClassVar[int]
    SEGMENT_CHUNK_FIELD_NUMBER: _ClassVar[int]
    SEGMENT_END_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    plan: WorkspaceFetchPlanV1
    segment_chunk: bytes
    segment_end: WorkspaceSegmentEndV1
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, plan: _Optional[_Union[WorkspaceFetchPlanV1, _Mapping]] = ..., segment_chunk: _Optional[bytes] = ..., segment_end: _Optional[_Union[WorkspaceSegmentEndV1, _Mapping]] = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class WorkspaceUploadHeaderV1(_message.Message):
    __slots__ = ("claim", "sha256", "bytes")
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    sha256: str
    bytes: int
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ..., sha256: _Optional[str] = ..., bytes: _Optional[int] = ...) -> None: ...

class WorkspaceUploadFrameV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: WorkspaceUploadHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[WorkspaceUploadHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class WorkspaceUploadResponseV1(_message.Message):
    __slots__ = ("exists", "refusal", "rejection")
    EXISTS_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    exists: bool
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, exists: bool = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class WorkspacePublishRequestV1(_message.Message):
    __slots__ = ("claim", "kind", "segment_sha256", "bytes", "prerequisites", "refs", "object_format")
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SEGMENT_SHA256_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    PREREQUISITES_FIELD_NUMBER: _ClassVar[int]
    REFS_FIELD_NUMBER: _ClassVar[int]
    OBJECT_FORMAT_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    kind: str
    segment_sha256: str
    bytes: int
    prerequisites: _containers.RepeatedScalarFieldContainer[str]
    refs: _containers.RepeatedCompositeFieldContainer[WorkspaceRefUpdateV1]
    object_format: str
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ..., kind: _Optional[str] = ..., segment_sha256: _Optional[str] = ..., bytes: _Optional[int] = ..., prerequisites: _Optional[_Iterable[str]] = ..., refs: _Optional[_Iterable[_Union[WorkspaceRefUpdateV1, _Mapping]]] = ..., object_format: _Optional[str] = ...) -> None: ...

class WorkspacePublishResponseV1(_message.Message):
    __slots__ = ("seq", "refs", "refusal", "rejection")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    REFS_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    seq: int
    refs: _containers.RepeatedCompositeFieldContainer[WorkspaceRefV1]
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, seq: _Optional[int] = ..., refs: _Optional[_Iterable[_Union[WorkspaceRefV1, _Mapping]]] = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class WorkspaceEnvSecretRequestV1(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ...) -> None: ...

class WorkspaceEnvVarV1(_message.Message):
    __slots__ = ("name", "value")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: str
    def __init__(self, name: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class WorkspaceEnvSecretFrameV1(_message.Message):
    __slots__ = ("vars", "end_count", "refusal", "rejection")
    VARS_FIELD_NUMBER: _ClassVar[int]
    END_COUNT_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    vars: WorkspaceEnvVarsV1
    end_count: int
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, vars: _Optional[_Union[WorkspaceEnvVarsV1, _Mapping]] = ..., end_count: _Optional[int] = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class WorkspaceEnvVarsV1(_message.Message):
    __slots__ = ("vars",)
    VARS_FIELD_NUMBER: _ClassVar[int]
    vars: _containers.RepeatedCompositeFieldContainer[WorkspaceEnvVarV1]
    def __init__(self, vars: _Optional[_Iterable[_Union[WorkspaceEnvVarV1, _Mapping]]] = ...) -> None: ...

class WorkspaceForgeCredentialRequestV1(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ...) -> None: ...

class WorkspaceForgeCredentialResponseV1(_message.Message):
    __slots__ = ("configuration_json", "refusal", "rejection", "forge")
    CONFIGURATION_JSON_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    FORGE_FIELD_NUMBER: _ClassVar[int]
    configuration_json: str
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    forge: str
    def __init__(self, configuration_json: _Optional[str] = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., forge: _Optional[str] = ...) -> None: ...

class WorkspaceRecordEventRequestV1(_message.Message):
    __slots__ = ("claim", "kind", "findings", "rules", "toolkit", "branch", "base", "number", "url", "files", "commits")
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    FINDINGS_FIELD_NUMBER: _ClassVar[int]
    RULES_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    BASE_FIELD_NUMBER: _ClassVar[int]
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    COMMITS_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    kind: WorkspaceExecutorEventKindV1
    findings: int
    rules: _containers.RepeatedScalarFieldContainer[str]
    toolkit: str
    branch: str
    base: str
    number: int
    url: str
    files: int
    commits: int
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ..., kind: _Optional[_Union[WorkspaceExecutorEventKindV1, str]] = ..., findings: _Optional[int] = ..., rules: _Optional[_Iterable[str]] = ..., toolkit: _Optional[str] = ..., branch: _Optional[str] = ..., base: _Optional[str] = ..., number: _Optional[int] = ..., url: _Optional[str] = ..., files: _Optional[int] = ..., commits: _Optional[int] = ...) -> None: ...

class WorkspaceRecordEventResponseV1(_message.Message):
    __slots__ = ("recorded", "refusal", "rejection")
    RECORDED_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    recorded: bool
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, recorded: bool = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class WorkspaceChangedFileV1(_message.Message):
    __slots__ = ("scope", "path", "from_path", "status", "additions", "deletions", "binary", "too_large", "diff")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    FROM_PATH_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    ADDITIONS_FIELD_NUMBER: _ClassVar[int]
    DELETIONS_FIELD_NUMBER: _ClassVar[int]
    BINARY_FIELD_NUMBER: _ClassVar[int]
    TOO_LARGE_FIELD_NUMBER: _ClassVar[int]
    DIFF_FIELD_NUMBER: _ClassVar[int]
    scope: WorkspaceChangeScopeV1
    path: str
    from_path: str
    status: WorkspaceChangeStatusV1
    additions: int
    deletions: int
    binary: bool
    too_large: bool
    diff: str
    def __init__(self, scope: _Optional[_Union[WorkspaceChangeScopeV1, str]] = ..., path: _Optional[str] = ..., from_path: _Optional[str] = ..., status: _Optional[_Union[WorkspaceChangeStatusV1, str]] = ..., additions: _Optional[int] = ..., deletions: _Optional[int] = ..., binary: bool = ..., too_large: bool = ..., diff: _Optional[str] = ...) -> None: ...

class WorkspaceChangesHeaderV1(_message.Message):
    __slots__ = ("claim", "commit", "parent", "base", "turn_omitted", "cumulative_omitted", "depth")
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    COMMIT_FIELD_NUMBER: _ClassVar[int]
    PARENT_FIELD_NUMBER: _ClassVar[int]
    BASE_FIELD_NUMBER: _ClassVar[int]
    TURN_OMITTED_FIELD_NUMBER: _ClassVar[int]
    CUMULATIVE_OMITTED_FIELD_NUMBER: _ClassVar[int]
    DEPTH_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    commit: str
    parent: str
    base: str
    turn_omitted: int
    cumulative_omitted: int
    depth: int
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ..., commit: _Optional[str] = ..., parent: _Optional[str] = ..., base: _Optional[str] = ..., turn_omitted: _Optional[int] = ..., cumulative_omitted: _Optional[int] = ..., depth: _Optional[int] = ...) -> None: ...

class WorkspaceChangesFrameV1(_message.Message):
    __slots__ = ("header", "file")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    FILE_FIELD_NUMBER: _ClassVar[int]
    header: WorkspaceChangesHeaderV1
    file: WorkspaceChangedFileV1
    def __init__(self, header: _Optional[_Union[WorkspaceChangesHeaderV1, _Mapping]] = ..., file: _Optional[_Union[WorkspaceChangedFileV1, _Mapping]] = ...) -> None: ...

class WorkspaceRecordChangesResponseV1(_message.Message):
    __slots__ = ("recorded", "refusal", "rejection")
    RECORDED_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    recorded: bool
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, recorded: bool = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...
