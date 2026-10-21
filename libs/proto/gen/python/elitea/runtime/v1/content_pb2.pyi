from elitea.runtime.v1 import toolkit_pb2 as _toolkit_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SandboxBundleKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SANDBOX_BUNDLE_KIND_V1_UNSPECIFIED: _ClassVar[SandboxBundleKindV1]
    SANDBOX_BUNDLE_KIND_V1_PYTHON: _ClassVar[SandboxBundleKindV1]
    SANDBOX_BUNDLE_KIND_V1_NATIVE: _ClassVar[SandboxBundleKindV1]
SANDBOX_BUNDLE_KIND_V1_UNSPECIFIED: SandboxBundleKindV1
SANDBOX_BUNDLE_KIND_V1_PYTHON: SandboxBundleKindV1
SANDBOX_BUNDLE_KIND_V1_NATIVE: SandboxBundleKindV1

class RuntimeContentScopeV1(_message.Message):
    __slots__ = ("execution_id", "generation")
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    execution_id: str
    generation: int
    def __init__(self, execution_id: _Optional[str] = ..., generation: _Optional[int] = ...) -> None: ...

class GetInputRequestV1(_message.Message):
    __slots__ = ("scope", "content_id", "immutable_version")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    content_id: str
    immutable_version: str
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., content_id: _Optional[str] = ..., immutable_version: _Optional[str] = ...) -> None: ...

class InputContentHeaderV1(_message.Message):
    __slots__ = ("media_type", "length", "sha256", "source_sha256", "source_length", "source_immutable_version")
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    LENGTH_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    SOURCE_SHA256_FIELD_NUMBER: _ClassVar[int]
    SOURCE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    SOURCE_IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    media_type: str
    length: int
    sha256: bytes
    source_sha256: bytes
    source_length: int
    source_immutable_version: str
    def __init__(self, media_type: _Optional[str] = ..., length: _Optional[int] = ..., sha256: _Optional[bytes] = ..., source_sha256: _Optional[bytes] = ..., source_length: _Optional[int] = ..., source_immutable_version: _Optional[str] = ...) -> None: ...

class GetInputResponseV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: InputContentHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[InputContentHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class GetEliteaClientTokenRequestV1(_message.Message):
    __slots__ = ("scope", "accept_fields")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    ACCEPT_FIELDS_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    accept_fields: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., accept_fields: _Optional[_Iterable[str]] = ...) -> None: ...

class GetEliteaClientTokenResponseV1(_message.Message):
    __slots__ = ("context_json",)
    CONTEXT_JSON_FIELD_NUMBER: _ClassVar[int]
    context_json: bytes
    def __init__(self, context_json: _Optional[bytes] = ...) -> None: ...

class GetApplicationVersionRequestV1(_message.Message):
    __slots__ = ("scope", "application_id", "version_id")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    APPLICATION_ID_FIELD_NUMBER: _ClassVar[int]
    VERSION_ID_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    application_id: int
    version_id: int
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., application_id: _Optional[int] = ..., version_id: _Optional[int] = ...) -> None: ...

class GetApplicationVersionResponseV1(_message.Message):
    __slots__ = ("version_json",)
    VERSION_JSON_FIELD_NUMBER: _ClassVar[int]
    version_json: bytes
    def __init__(self, version_json: _Optional[bytes] = ...) -> None: ...

class GetAttachmentObjectRequestV1(_message.Message):
    __slots__ = ("scope", "bucket", "name")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    BUCKET_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    bucket: str
    name: str
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., bucket: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class GetAttachmentObjectResponseV1(_message.Message):
    __slots__ = ("document_json", "unreadable_reason")
    DOCUMENT_JSON_FIELD_NUMBER: _ClassVar[int]
    UNREADABLE_REASON_FIELD_NUMBER: _ClassVar[int]
    document_json: bytes
    unreadable_reason: str
    def __init__(self, document_json: _Optional[bytes] = ..., unreadable_reason: _Optional[str] = ...) -> None: ...

class ExecuteHTTPActionRequestV1(_message.Message):
    __slots__ = ("scope", "invocation_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    INVOCATION_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    invocation_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., invocation_json: _Optional[bytes] = ...) -> None: ...

class ExecuteHTTPActionResponseV1(_message.Message):
    __slots__ = ("receipt_json",)
    RECEIPT_JSON_FIELD_NUMBER: _ClassVar[int]
    receipt_json: bytes
    def __init__(self, receipt_json: _Optional[bytes] = ...) -> None: ...

class WriteSkillRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class WriteSkillResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class WriteProjectContextRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class WriteProjectContextResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class ListArtifactsRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ListArtifactsResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class ReadArtifactRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ReadArtifactResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class WriteArtifactRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class WriteArtifactResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class DeleteArtifactRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class DeleteArtifactResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class ResolveCodeWorkspaceRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ResolveCodeWorkspaceResponseV1(_message.Message):
    __slots__ = ("manifest_json", "workspace_root")
    MANIFEST_JSON_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_ROOT_FIELD_NUMBER: _ClassVar[int]
    manifest_json: bytes
    workspace_root: str
    def __init__(self, manifest_json: _Optional[bytes] = ..., workspace_root: _Optional[str] = ...) -> None: ...

class AdmitCodeDebugRequestV1(_message.Message):
    __slots__ = ("scope", "admission_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    ADMISSION_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    admission_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., admission_json: _Optional[bytes] = ...) -> None: ...

class AdmitCodeDebugResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class CommitCodeDebugRequestV1(_message.Message):
    __slots__ = ("scope", "visit_id", "snapshot_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    VISIT_ID_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    visit_id: str
    snapshot_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., visit_id: _Optional[str] = ..., snapshot_json: _Optional[bytes] = ...) -> None: ...

class CommitCodeDebugResponseV1(_message.Message):
    __slots__ = ("reference_json",)
    REFERENCE_JSON_FIELD_NUMBER: _ClassVar[int]
    reference_json: bytes
    def __init__(self, reference_json: _Optional[bytes] = ...) -> None: ...

class StepCodePlatformRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class StepCodePlatformResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class ReadCodePlatformEffectsRequestV1(_message.Message):
    __slots__ = ("scope", "request_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    request_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ReadCodePlatformEffectsResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class PollNodeRecoveryRequestV1(_message.Message):
    __slots__ = ("scope",)
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ...) -> None: ...

class PollNodeRecoveryResponseV1(_message.Message):
    __slots__ = ("control_json",)
    CONTROL_JSON_FIELD_NUMBER: _ClassVar[int]
    control_json: bytes
    def __init__(self, control_json: _Optional[bytes] = ...) -> None: ...

class AcknowledgeNodeRecoveryRequestV1(_message.Message):
    __slots__ = ("scope", "ack_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    ACK_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    ack_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., ack_json: _Optional[bytes] = ...) -> None: ...

class AcknowledgeNodeRecoveryResponseV1(_message.Message):
    __slots__ = ("outcome_json",)
    OUTCOME_JSON_FIELD_NUMBER: _ClassVar[int]
    outcome_json: bytes
    def __init__(self, outcome_json: _Optional[bytes] = ...) -> None: ...

class ReadNodeRecoveryResultRequestV1(_message.Message):
    __slots__ = ("scope", "content_id", "version")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_ID_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    content_id: str
    version: str
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., content_id: _Optional[str] = ..., version: _Optional[str] = ...) -> None: ...

class ReadNodeRecoveryResultResponseV1(_message.Message):
    __slots__ = ("result_json",)
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    result_json: bytes
    def __init__(self, result_json: _Optional[bytes] = ...) -> None: ...

class RegisterCodeVisitRequestV1(_message.Message):
    __slots__ = ("scope", "visit_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    VISIT_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    visit_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., visit_json: _Optional[bytes] = ...) -> None: ...

class RegisterCodeVisitResponseV1(_message.Message):
    __slots__ = ("visit_json",)
    VISIT_JSON_FIELD_NUMBER: _ClassVar[int]
    visit_json: bytes
    def __init__(self, visit_json: _Optional[bytes] = ...) -> None: ...

class RegisterCodeIntentRequestV1(_message.Message):
    __slots__ = ("scope", "intent_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    INTENT_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    intent_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., intent_json: _Optional[bytes] = ...) -> None: ...

class RegisterCodeIntentResponseV1(_message.Message):
    __slots__ = ("response_json",)
    RESPONSE_JSON_FIELD_NUMBER: _ClassVar[int]
    response_json: bytes
    def __init__(self, response_json: _Optional[bytes] = ...) -> None: ...

class PutToolkitDiscoveryResultRequestV1(_message.Message):
    __slots__ = ("scope", "content_id", "immutable_version", "result_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    content_id: str
    immutable_version: str
    result_json: bytes
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., content_id: _Optional[str] = ..., immutable_version: _Optional[str] = ..., result_json: _Optional[bytes] = ...) -> None: ...

class PutToolkitDiscoveryResultResponseV1(_message.Message):
    __slots__ = ("reference",)
    REFERENCE_FIELD_NUMBER: _ClassVar[int]
    reference: _toolkit_pb2.ToolkitAvailableToolsArtifactReferenceV1
    def __init__(self, reference: _Optional[_Union[_toolkit_pb2.ToolkitAvailableToolsArtifactReferenceV1, _Mapping]] = ...) -> None: ...

class GetToolkitDiscoveryResultRequestV1(_message.Message):
    __slots__ = ("scope", "content_id", "immutable_version")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    scope: RuntimeContentScopeV1
    content_id: str
    immutable_version: str
    def __init__(self, scope: _Optional[_Union[RuntimeContentScopeV1, _Mapping]] = ..., content_id: _Optional[str] = ..., immutable_version: _Optional[str] = ...) -> None: ...

class GetToolkitDiscoveryResultResponseV1(_message.Message):
    __slots__ = ("result_json",)
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    result_json: bytes
    def __init__(self, result_json: _Optional[bytes] = ...) -> None: ...

class ReadSandboxWorkspaceManifestRequestV1(_message.Message):
    __slots__ = ("manifest_sha256", "request_json")
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    manifest_sha256: str
    request_json: bytes
    def __init__(self, manifest_sha256: _Optional[str] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ReadSandboxWorkspaceManifestResponseV1(_message.Message):
    __slots__ = ("manifest_json",)
    MANIFEST_JSON_FIELD_NUMBER: _ClassVar[int]
    manifest_json: bytes
    def __init__(self, manifest_json: _Optional[bytes] = ...) -> None: ...

class ReadSandboxWorkspaceFileRequestV1(_message.Message):
    __slots__ = ("manifest_sha256", "content_sha256", "request_json")
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    CONTENT_SHA256_FIELD_NUMBER: _ClassVar[int]
    REQUEST_JSON_FIELD_NUMBER: _ClassVar[int]
    manifest_sha256: str
    content_sha256: str
    request_json: bytes
    def __init__(self, manifest_sha256: _Optional[str] = ..., content_sha256: _Optional[str] = ..., request_json: _Optional[bytes] = ...) -> None: ...

class ReadSandboxWorkspaceFileResponseV1(_message.Message):
    __slots__ = ("content",)
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    content: bytes
    def __init__(self, content: _Optional[bytes] = ...) -> None: ...

class SandboxContentFileHeaderV1(_message.Message):
    __slots__ = ("length", "sha256")
    LENGTH_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    length: int
    sha256: bytes
    def __init__(self, length: _Optional[int] = ..., sha256: _Optional[bytes] = ...) -> None: ...

class GetSandboxBundleRequestV1(_message.Message):
    __slots__ = ("kind", "root", "signed_grant")
    KIND_FIELD_NUMBER: _ClassVar[int]
    ROOT_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    kind: SandboxBundleKindV1
    root: str
    signed_grant: bytes
    def __init__(self, kind: _Optional[_Union[SandboxBundleKindV1, str]] = ..., root: _Optional[str] = ..., signed_grant: _Optional[bytes] = ...) -> None: ...

class GetSandboxBundleResponseV1(_message.Message):
    __slots__ = ("bundle_json",)
    BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    bundle_json: bytes
    def __init__(self, bundle_json: _Optional[bytes] = ...) -> None: ...

class PublishSandboxBundleRequestV1(_message.Message):
    __slots__ = ("kind", "root", "signed_grant", "bundle_json")
    KIND_FIELD_NUMBER: _ClassVar[int]
    ROOT_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    kind: SandboxBundleKindV1
    root: str
    signed_grant: bytes
    bundle_json: bytes
    def __init__(self, kind: _Optional[_Union[SandboxBundleKindV1, str]] = ..., root: _Optional[str] = ..., signed_grant: _Optional[bytes] = ..., bundle_json: _Optional[bytes] = ...) -> None: ...

class PublishSandboxBundleResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FetchSandboxBundleFileRequestV1(_message.Message):
    __slots__ = ("kind", "root", "name", "signed_grant")
    KIND_FIELD_NUMBER: _ClassVar[int]
    ROOT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    kind: SandboxBundleKindV1
    root: str
    name: str
    signed_grant: bytes
    def __init__(self, kind: _Optional[_Union[SandboxBundleKindV1, str]] = ..., root: _Optional[str] = ..., name: _Optional[str] = ..., signed_grant: _Optional[bytes] = ...) -> None: ...

class FetchSandboxBundleFileResponseV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: SandboxContentFileHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[SandboxContentFileHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class UploadSandboxBundleFileHeaderV1(_message.Message):
    __slots__ = ("kind", "root", "name", "signed_grant", "bundle_json")
    KIND_FIELD_NUMBER: _ClassVar[int]
    ROOT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    kind: SandboxBundleKindV1
    root: str
    name: str
    signed_grant: bytes
    bundle_json: bytes
    def __init__(self, kind: _Optional[_Union[SandboxBundleKindV1, str]] = ..., root: _Optional[str] = ..., name: _Optional[str] = ..., signed_grant: _Optional[bytes] = ..., bundle_json: _Optional[bytes] = ...) -> None: ...

class UploadSandboxBundleFileRequestV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: UploadSandboxBundleFileHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[UploadSandboxBundleFileHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class UploadSandboxBundleFileResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetCompiledSnapshotRequestV1(_message.Message):
    __slots__ = ("root", "signed_grant")
    ROOT_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    root: str
    signed_grant: bytes
    def __init__(self, root: _Optional[str] = ..., signed_grant: _Optional[bytes] = ...) -> None: ...

class GetCompiledSnapshotResponseV1(_message.Message):
    __slots__ = ("descriptor_json",)
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    descriptor_json: bytes
    def __init__(self, descriptor_json: _Optional[bytes] = ...) -> None: ...

class PublishCompiledSnapshotRequestV1(_message.Message):
    __slots__ = ("root", "signed_grant", "descriptor_json")
    ROOT_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    root: str
    signed_grant: bytes
    descriptor_json: bytes
    def __init__(self, root: _Optional[str] = ..., signed_grant: _Optional[bytes] = ..., descriptor_json: _Optional[bytes] = ...) -> None: ...

class PublishCompiledSnapshotResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class FetchCompiledSnapshotFileRequestV1(_message.Message):
    __slots__ = ("root", "name", "signed_grant")
    ROOT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    root: str
    name: str
    signed_grant: bytes
    def __init__(self, root: _Optional[str] = ..., name: _Optional[str] = ..., signed_grant: _Optional[bytes] = ...) -> None: ...

class FetchCompiledSnapshotFileResponseV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: SandboxContentFileHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[SandboxContentFileHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class UploadCompiledSnapshotFileHeaderV1(_message.Message):
    __slots__ = ("root", "name", "signed_grant", "descriptor_json")
    ROOT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SIGNED_GRANT_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    root: str
    name: str
    signed_grant: bytes
    descriptor_json: bytes
    def __init__(self, root: _Optional[str] = ..., name: _Optional[str] = ..., signed_grant: _Optional[bytes] = ..., descriptor_json: _Optional[bytes] = ...) -> None: ...

class UploadCompiledSnapshotFileRequestV1(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: UploadCompiledSnapshotFileHeaderV1
    chunk: bytes
    def __init__(self, header: _Optional[_Union[UploadCompiledSnapshotFileHeaderV1, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class UploadCompiledSnapshotFileResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
