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
    WORKSPACE_REFUSAL_CODE_V1_NO_UPSTREAM: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_DIFF_TOO_LARGE: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_DIFF_BINARY: _ClassVar[WorkspaceRefusalCodeV1]
    WORKSPACE_REFUSAL_CODE_V1_DIFF_EMPTY: _ClassVar[WorkspaceRefusalCodeV1]
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
WORKSPACE_REFUSAL_CODE_V1_NO_UPSTREAM: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_DIFF_TOO_LARGE: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_DIFF_BINARY: WorkspaceRefusalCodeV1
WORKSPACE_REFUSAL_CODE_V1_DIFF_EMPTY: WorkspaceRefusalCodeV1

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

class WorkspacePullRequestDiffRequestV1(_message.Message):
    __slots__ = ("claim",)
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    claim: WorkspaceClaimV1
    def __init__(self, claim: _Optional[_Union[WorkspaceClaimV1, _Mapping]] = ...) -> None: ...

class WorkspacePullRequestDiffHeaderV1(_message.Message):
    __slots__ = ("upstream_sha", "upstream_branch", "remote_url", "cloud_sha", "files", "bytes", "diff_sha256")
    UPSTREAM_SHA_FIELD_NUMBER: _ClassVar[int]
    UPSTREAM_BRANCH_FIELD_NUMBER: _ClassVar[int]
    REMOTE_URL_FIELD_NUMBER: _ClassVar[int]
    CLOUD_SHA_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    DIFF_SHA256_FIELD_NUMBER: _ClassVar[int]
    upstream_sha: str
    upstream_branch: str
    remote_url: str
    cloud_sha: str
    files: int
    bytes: int
    diff_sha256: str
    def __init__(self, upstream_sha: _Optional[str] = ..., upstream_branch: _Optional[str] = ..., remote_url: _Optional[str] = ..., cloud_sha: _Optional[str] = ..., files: _Optional[int] = ..., bytes: _Optional[int] = ..., diff_sha256: _Optional[str] = ...) -> None: ...

class WorkspacePullRequestDiffFrameV1(_message.Message):
    __slots__ = ("header", "chunk", "refusal", "rejection")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    REFUSAL_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    header: WorkspacePullRequestDiffHeaderV1
    chunk: bytes
    refusal: WorkspaceRefusalV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, header: _Optional[_Union[WorkspacePullRequestDiffHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ..., refusal: _Optional[_Union[WorkspaceRefusalV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

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
