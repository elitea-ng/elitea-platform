from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ArtifactRuntimeClaimV1(_message.Message):
    __slots__ = ("execution_id", "generation", "claim_id", "fence")
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    execution_id: str
    generation: int
    claim_id: str
    fence: str
    def __init__(self, execution_id: _Optional[str] = ..., generation: _Optional[int] = ..., claim_id: _Optional[str] = ..., fence: _Optional[str] = ...) -> None: ...

class ArtifactRefusalV1(_message.Message):
    __slots__ = ("http_status", "code", "message")
    HTTP_STATUS_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    http_status: int
    code: str
    message: str
    def __init__(self, http_status: _Optional[int] = ..., code: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...

class ArtifactListBucketsRequestV1(_message.Message):
    __slots__ = ("project_id",)
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    def __init__(self, project_id: _Optional[int] = ...) -> None: ...

class ArtifactBucketV1(_message.Message):
    __slots__ = ("name", "retention_days", "size_bytes", "object_count")
    NAME_FIELD_NUMBER: _ClassVar[int]
    RETENTION_DAYS_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    OBJECT_COUNT_FIELD_NUMBER: _ClassVar[int]
    name: str
    retention_days: int
    size_bytes: int
    object_count: int
    def __init__(self, name: _Optional[str] = ..., retention_days: _Optional[int] = ..., size_bytes: _Optional[int] = ..., object_count: _Optional[int] = ...) -> None: ...

class ArtifactListBucketsResponseV1(_message.Message):
    __slots__ = ("buckets",)
    BUCKETS_FIELD_NUMBER: _ClassVar[int]
    buckets: _containers.RepeatedCompositeFieldContainer[ArtifactBucketV1]
    def __init__(self, buckets: _Optional[_Iterable[_Union[ArtifactBucketV1, _Mapping]]] = ...) -> None: ...

class ArtifactCreateBucketResponseV1(_message.Message):
    __slots__ = ("bucket",)
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    bucket: ArtifactBucketV1
    def __init__(self, bucket: _Optional[_Union[ArtifactBucketV1, _Mapping]] = ...) -> None: ...

class ArtifactCreateBucketRequestV1(_message.Message):
    __slots__ = ("project_id", "name", "retention_days")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    RETENTION_DAYS_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    name: str
    retention_days: int
    def __init__(self, project_id: _Optional[int] = ..., name: _Optional[str] = ..., retention_days: _Optional[int] = ...) -> None: ...

class ArtifactListObjectsRequestV1(_message.Message):
    __slots__ = ("project_id", "bucket", "prefix", "delimiter", "page_size", "cursor", "max_pages", "claim")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    PREFIX_FIELD_NUMBER: _ClassVar[int]
    DELIMITER_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    MAX_PAGES_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    bucket: str
    prefix: str
    delimiter: str
    page_size: int
    cursor: str
    max_pages: int
    claim: ArtifactRuntimeClaimV1
    def __init__(self, project_id: _Optional[int] = ..., bucket: _Optional[str] = ..., prefix: _Optional[str] = ..., delimiter: _Optional[str] = ..., page_size: _Optional[int] = ..., cursor: _Optional[str] = ..., max_pages: _Optional[int] = ..., claim: _Optional[_Union[ArtifactRuntimeClaimV1, _Mapping]] = ...) -> None: ...

class ArtifactObjectInfoV1(_message.Message):
    __slots__ = ("key", "size_bytes", "media_type", "etag", "modified_at")
    KEY_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    ETAG_FIELD_NUMBER: _ClassVar[int]
    MODIFIED_AT_FIELD_NUMBER: _ClassVar[int]
    key: str
    size_bytes: int
    media_type: str
    etag: str
    modified_at: str
    def __init__(self, key: _Optional[str] = ..., size_bytes: _Optional[int] = ..., media_type: _Optional[str] = ..., etag: _Optional[str] = ..., modified_at: _Optional[str] = ...) -> None: ...

class ArtifactListObjectsResponseV1(_message.Message):
    __slots__ = ("objects", "common_prefixes", "next_cursor")
    OBJECTS_FIELD_NUMBER: _ClassVar[int]
    COMMON_PREFIXES_FIELD_NUMBER: _ClassVar[int]
    NEXT_CURSOR_FIELD_NUMBER: _ClassVar[int]
    objects: _containers.RepeatedCompositeFieldContainer[ArtifactObjectInfoV1]
    common_prefixes: _containers.RepeatedScalarFieldContainer[str]
    next_cursor: str
    def __init__(self, objects: _Optional[_Iterable[_Union[ArtifactObjectInfoV1, _Mapping]]] = ..., common_prefixes: _Optional[_Iterable[str]] = ..., next_cursor: _Optional[str] = ...) -> None: ...

class ArtifactObjectRefV1(_message.Message):
    __slots__ = ("project_id", "bucket", "key", "claim")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    bucket: str
    key: str
    claim: ArtifactRuntimeClaimV1
    def __init__(self, project_id: _Optional[int] = ..., bucket: _Optional[str] = ..., key: _Optional[str] = ..., claim: _Optional[_Union[ArtifactRuntimeClaimV1, _Mapping]] = ...) -> None: ...

class ArtifactStatRequestV1(_message.Message):
    __slots__ = ("object",)
    OBJECT_FIELD_NUMBER: _ClassVar[int]
    object: ArtifactObjectRefV1
    def __init__(self, object: _Optional[_Union[ArtifactObjectRefV1, _Mapping]] = ...) -> None: ...

class ArtifactStatResponseV1(_message.Message):
    __slots__ = ("info",)
    INFO_FIELD_NUMBER: _ClassVar[int]
    info: ArtifactObjectInfoV1
    def __init__(self, info: _Optional[_Union[ArtifactObjectInfoV1, _Mapping]] = ...) -> None: ...

class ArtifactGetObjectRequestV1(_message.Message):
    __slots__ = ("object",)
    OBJECT_FIELD_NUMBER: _ClassVar[int]
    object: ArtifactObjectRefV1
    def __init__(self, object: _Optional[_Union[ArtifactObjectRefV1, _Mapping]] = ...) -> None: ...

class ArtifactDeleteRequestV1(_message.Message):
    __slots__ = ("object",)
    OBJECT_FIELD_NUMBER: _ClassVar[int]
    object: ArtifactObjectRefV1
    def __init__(self, object: _Optional[_Union[ArtifactObjectRefV1, _Mapping]] = ...) -> None: ...

class ArtifactGetObjectResponseV1(_message.Message):
    __slots__ = ("info", "data")
    INFO_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    info: ArtifactObjectInfoV1
    data: bytes
    def __init__(self, info: _Optional[_Union[ArtifactObjectInfoV1, _Mapping]] = ..., data: _Optional[bytes] = ...) -> None: ...

class ArtifactPutObjectHeadV1(_message.Message):
    __slots__ = ("project_id", "bucket", "key", "media_type", "overwrite", "claim")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    OVERWRITE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    bucket: str
    key: str
    media_type: str
    overwrite: bool
    claim: ArtifactRuntimeClaimV1
    def __init__(self, project_id: _Optional[int] = ..., bucket: _Optional[str] = ..., key: _Optional[str] = ..., media_type: _Optional[str] = ..., overwrite: _Optional[bool] = ..., claim: _Optional[_Union[ArtifactRuntimeClaimV1, _Mapping]] = ...) -> None: ...

class ArtifactPutObjectRequestV1(_message.Message):
    __slots__ = ("head", "data")
    HEAD_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    head: ArtifactPutObjectHeadV1
    data: bytes
    def __init__(self, head: _Optional[_Union[ArtifactPutObjectHeadV1, _Mapping]] = ..., data: _Optional[bytes] = ...) -> None: ...

class ArtifactPutObjectResponseV1(_message.Message):
    __slots__ = ("info",)
    INFO_FIELD_NUMBER: _ClassVar[int]
    info: ArtifactObjectInfoV1
    def __init__(self, info: _Optional[_Union[ArtifactObjectInfoV1, _Mapping]] = ...) -> None: ...

class ArtifactDeleteResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ArtifactDeleteBatchRequestV1(_message.Message):
    __slots__ = ("project_id", "bucket", "keys", "claim")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    KEYS_FIELD_NUMBER: _ClassVar[int]
    CLAIM_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    bucket: str
    keys: _containers.RepeatedScalarFieldContainer[str]
    claim: ArtifactRuntimeClaimV1
    def __init__(self, project_id: _Optional[int] = ..., bucket: _Optional[str] = ..., keys: _Optional[_Iterable[str]] = ..., claim: _Optional[_Union[ArtifactRuntimeClaimV1, _Mapping]] = ...) -> None: ...

class ArtifactDeleteFailureV1(_message.Message):
    __slots__ = ("key", "code", "message")
    KEY_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    key: str
    code: str
    message: str
    def __init__(self, key: _Optional[str] = ..., code: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...

class ArtifactDeleteBatchResponseV1(_message.Message):
    __slots__ = ("deleted", "failed")
    DELETED_FIELD_NUMBER: _ClassVar[int]
    FAILED_FIELD_NUMBER: _ClassVar[int]
    deleted: _containers.RepeatedScalarFieldContainer[str]
    failed: _containers.RepeatedCompositeFieldContainer[ArtifactDeleteFailureV1]
    def __init__(self, deleted: _Optional[_Iterable[str]] = ..., failed: _Optional[_Iterable[_Union[ArtifactDeleteFailureV1, _Mapping]]] = ...) -> None: ...
