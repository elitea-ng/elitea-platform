from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import envelope_pb2 as _envelope_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SandboxJobStatusV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SANDBOX_JOB_STATUS_V1_UNSPECIFIED: _ClassVar[SandboxJobStatusV1]
    SANDBOX_JOB_STATUS_V1_PENDING: _ClassVar[SandboxJobStatusV1]
    SANDBOX_JOB_STATUS_V1_COMPLETED: _ClassVar[SandboxJobStatusV1]
    SANDBOX_JOB_STATUS_V1_FAILED: _ClassVar[SandboxJobStatusV1]
    SANDBOX_JOB_STATUS_V1_CANCELLED: _ClassVar[SandboxJobStatusV1]
    SANDBOX_JOB_STATUS_V1_UNCERTAIN: _ClassVar[SandboxJobStatusV1]

class RustCompiledPublicationPhaseV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RUST_COMPILED_PUBLICATION_PHASE_V1_UNSPECIFIED: _ClassVar[RustCompiledPublicationPhaseV1]
    RUST_COMPILED_PUBLICATION_PHASE_V1_EXECUTABLE: _ClassVar[RustCompiledPublicationPhaseV1]
    RUST_COMPILED_PUBLICATION_PHASE_V1_RELEASE: _ClassVar[RustCompiledPublicationPhaseV1]
    RUST_COMPILED_PUBLICATION_PHASE_V1_READY: _ClassVar[RustCompiledPublicationPhaseV1]
SANDBOX_JOB_STATUS_V1_UNSPECIFIED: SandboxJobStatusV1
SANDBOX_JOB_STATUS_V1_PENDING: SandboxJobStatusV1
SANDBOX_JOB_STATUS_V1_COMPLETED: SandboxJobStatusV1
SANDBOX_JOB_STATUS_V1_FAILED: SandboxJobStatusV1
SANDBOX_JOB_STATUS_V1_CANCELLED: SandboxJobStatusV1
SANDBOX_JOB_STATUS_V1_UNCERTAIN: SandboxJobStatusV1
RUST_COMPILED_PUBLICATION_PHASE_V1_UNSPECIFIED: RustCompiledPublicationPhaseV1
RUST_COMPILED_PUBLICATION_PHASE_V1_EXECUTABLE: RustCompiledPublicationPhaseV1
RUST_COMPILED_PUBLICATION_PHASE_V1_RELEASE: RustCompiledPublicationPhaseV1
RUST_COMPILED_PUBLICATION_PHASE_V1_READY: RustCompiledPublicationPhaseV1

class AuthorizeSandboxJobRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "activation_id", "request_digest", "audience", "signed_command", "cancel_only", "dependency_bundle_sha256")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    ACTIVATION_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    AUDIENCE_FIELD_NUMBER: _ClassVar[int]
    SIGNED_COMMAND_FIELD_NUMBER: _ClassVar[int]
    CANCEL_ONLY_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_BUNDLE_SHA256_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    activation_id: str
    request_digest: bytes
    audience: str
    signed_command: _envelope_pb2.SignedWorkerCommandEnvelopeV1
    cancel_only: bool
    dependency_bundle_sha256: bytes
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., activation_id: _Optional[str] = ..., request_digest: _Optional[bytes] = ..., audience: _Optional[str] = ..., signed_command: _Optional[_Union[_envelope_pb2.SignedWorkerCommandEnvelopeV1, _Mapping]] = ..., cancel_only: bool = ..., dependency_bundle_sha256: _Optional[bytes] = ...) -> None: ...

class SandboxJobGrantClaimsV1(_message.Message):
    __slots__ = ("revision", "tenant_id", "project_id", "execution_id", "activation_id", "request_digest", "submitter_workload_identity", "audience", "issued_at_unix_millis", "expires_at_unix_millis", "generation", "cancel_only", "dependency_bundle_sha256")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    ACTIVATION_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SUBMITTER_WORKLOAD_IDENTITY_FIELD_NUMBER: _ClassVar[int]
    AUDIENCE_FIELD_NUMBER: _ClassVar[int]
    ISSUED_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    CANCEL_ONLY_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_BUNDLE_SHA256_FIELD_NUMBER: _ClassVar[int]
    revision: int
    tenant_id: str
    project_id: int
    execution_id: str
    activation_id: str
    request_digest: bytes
    submitter_workload_identity: str
    audience: str
    issued_at_unix_millis: int
    expires_at_unix_millis: int
    generation: int
    cancel_only: bool
    dependency_bundle_sha256: bytes
    def __init__(self, revision: _Optional[int] = ..., tenant_id: _Optional[str] = ..., project_id: _Optional[int] = ..., execution_id: _Optional[str] = ..., activation_id: _Optional[str] = ..., request_digest: _Optional[bytes] = ..., submitter_workload_identity: _Optional[str] = ..., audience: _Optional[str] = ..., issued_at_unix_millis: _Optional[int] = ..., expires_at_unix_millis: _Optional[int] = ..., generation: _Optional[int] = ..., cancel_only: bool = ..., dependency_bundle_sha256: _Optional[bytes] = ...) -> None: ...

class SignedSandboxJobGrantV1(_message.Message):
    __slots__ = ("key_id", "claims_bytes", "signature")
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    CLAIMS_BYTES_FIELD_NUMBER: _ClassVar[int]
    SIGNATURE_FIELD_NUMBER: _ClassVar[int]
    key_id: str
    claims_bytes: bytes
    signature: bytes
    def __init__(self, key_id: _Optional[str] = ..., claims_bytes: _Optional[bytes] = ..., signature: _Optional[bytes] = ...) -> None: ...

class AuthorizeSandboxJobResponseV1(_message.Message):
    __slots__ = ("grant", "rejection")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class HydrateSandboxWorkspaceRequestV1(_message.Message):
    __slots__ = ("grant", "prepared_job_json", "workspace_manifest_json", "control_json", "descriptor_json", "read_grant", "code_execution_intent_json", "file_index")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    PREPARED_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_MANIFEST_JSON_FIELD_NUMBER: _ClassVar[int]
    CONTROL_JSON_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    READ_GRANT_FIELD_NUMBER: _ClassVar[int]
    CODE_EXECUTION_INTENT_JSON_FIELD_NUMBER: _ClassVar[int]
    FILE_INDEX_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    prepared_job_json: bytes
    workspace_manifest_json: bytes
    control_json: bytes
    descriptor_json: bytes
    read_grant: SignedSandboxJobGrantV1
    code_execution_intent_json: bytes
    file_index: int
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., prepared_job_json: _Optional[bytes] = ..., workspace_manifest_json: _Optional[bytes] = ..., control_json: _Optional[bytes] = ..., descriptor_json: _Optional[bytes] = ..., read_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., code_execution_intent_json: _Optional[bytes] = ..., file_index: _Optional[int] = ...) -> None: ...

class HydrateSandboxWorkspaceResponseV1(_message.Message):
    __slots__ = ("status", "cursor")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    cursor: WorkspaceHydrationCursorV1
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., cursor: _Optional[_Union[WorkspaceHydrationCursorV1, _Mapping]] = ...) -> None: ...

class WorkspaceHydrationCursorV1(_message.Message):
    __slots__ = ("revision", "manifest_sha256", "next_file_index", "file_count", "ready")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    NEXT_FILE_INDEX_FIELD_NUMBER: _ClassVar[int]
    FILE_COUNT_FIELD_NUMBER: _ClassVar[int]
    READY_FIELD_NUMBER: _ClassVar[int]
    revision: int
    manifest_sha256: bytes
    next_file_index: int
    file_count: int
    ready: bool
    def __init__(self, revision: _Optional[int] = ..., manifest_sha256: _Optional[bytes] = ..., next_file_index: _Optional[int] = ..., file_count: _Optional[int] = ..., ready: bool = ...) -> None: ...

class SubmitSandboxJobRequestV1(_message.Message):
    __slots__ = ("grant", "code_execution_intent_json", "prepared_job_json", "dependency_content_grant", "dependency_bundle_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    CODE_EXECUTION_INTENT_JSON_FIELD_NUMBER: _ClassVar[int]
    PREPARED_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_CONTENT_GRANT_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    code_execution_intent_json: bytes
    prepared_job_json: bytes
    dependency_content_grant: SignedSandboxJobGrantV1
    dependency_bundle_json: bytes
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., code_execution_intent_json: _Optional[bytes] = ..., prepared_job_json: _Optional[bytes] = ..., dependency_content_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., dependency_bundle_json: _Optional[bytes] = ...) -> None: ...

class SubmitSandboxJobResponseV1(_message.Message):
    __slots__ = ("status", "result_json", "failure_code", "cleanup_pending")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    result_json: bytes
    failure_code: str
    cleanup_pending: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., result_json: _Optional[bytes] = ..., failure_code: _Optional[str] = ..., cleanup_pending: bool = ...) -> None: ...

class CancelSandboxJobRequestV1(_message.Message):
    __slots__ = ("grant",)
    GRANT_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ...) -> None: ...

class CancelSandboxJobResponseV1(_message.Message):
    __slots__ = ("status", "cleanup_pending")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    cleanup_pending: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., cleanup_pending: bool = ...) -> None: ...

class PrepareSandboxDependenciesRequestV1(_message.Message):
    __slots__ = ("grant", "preparation_job_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    PREPARATION_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    preparation_job_json: bytes
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., preparation_job_json: _Optional[bytes] = ...) -> None: ...

class PrepareSandboxDependenciesResponseV1(_message.Message):
    __slots__ = ("status", "bundle_json", "failure_code", "cleanup_pending")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    bundle_json: bytes
    failure_code: str
    cleanup_pending: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., bundle_json: _Optional[bytes] = ..., failure_code: _Optional[str] = ..., cleanup_pending: bool = ...) -> None: ...

class PublishSandboxDependenciesRequestV1(_message.Message):
    __slots__ = ("content_grant", "index")
    CONTENT_GRANT_FIELD_NUMBER: _ClassVar[int]
    INDEX_FIELD_NUMBER: _ClassVar[int]
    content_grant: SignedSandboxJobGrantV1
    index: int
    def __init__(self, content_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., index: _Optional[int] = ...) -> None: ...

class PublishSandboxDependenciesResponseV1(_message.Message):
    __slots__ = ("status", "failure_code", "cleanup_pending")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    failure_code: str
    cleanup_pending: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., failure_code: _Optional[str] = ..., cleanup_pending: bool = ...) -> None: ...

class HydrateSandboxDependenciesRequestV1(_message.Message):
    __slots__ = ("execution_grant", "content_grant", "prepared_job_json", "bundle_json", "index", "code_execution_intent_json")
    EXECUTION_GRANT_FIELD_NUMBER: _ClassVar[int]
    CONTENT_GRANT_FIELD_NUMBER: _ClassVar[int]
    PREPARED_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    INDEX_FIELD_NUMBER: _ClassVar[int]
    CODE_EXECUTION_INTENT_JSON_FIELD_NUMBER: _ClassVar[int]
    execution_grant: SignedSandboxJobGrantV1
    content_grant: SignedSandboxJobGrantV1
    prepared_job_json: bytes
    bundle_json: bytes
    index: int
    code_execution_intent_json: bytes
    def __init__(self, execution_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., content_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., prepared_job_json: _Optional[bytes] = ..., bundle_json: _Optional[bytes] = ..., index: _Optional[int] = ..., code_execution_intent_json: _Optional[bytes] = ...) -> None: ...

class HydrateSandboxDependenciesResponseV1(_message.Message):
    __slots__ = ("ready",)
    READY_FIELD_NUMBER: _ClassVar[int]
    ready: bool
    def __init__(self, ready: bool = ...) -> None: ...

class SubmitRustCompiledSnapshotRequestV1(_message.Message):
    __slots__ = ("grant", "prepared_job_json", "control_json", "descriptor_json", "read_grant", "dependency_content_grant", "dependency_bundle_json", "reconcile_only", "native_hydration_index", "code_execution_intent_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    PREPARED_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    CONTROL_JSON_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    READ_GRANT_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_CONTENT_GRANT_FIELD_NUMBER: _ClassVar[int]
    DEPENDENCY_BUNDLE_JSON_FIELD_NUMBER: _ClassVar[int]
    RECONCILE_ONLY_FIELD_NUMBER: _ClassVar[int]
    NATIVE_HYDRATION_INDEX_FIELD_NUMBER: _ClassVar[int]
    CODE_EXECUTION_INTENT_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: SignedSandboxJobGrantV1
    prepared_job_json: bytes
    control_json: bytes
    descriptor_json: bytes
    read_grant: SignedSandboxJobGrantV1
    dependency_content_grant: SignedSandboxJobGrantV1
    dependency_bundle_json: bytes
    reconcile_only: bool
    native_hydration_index: int
    code_execution_intent_json: bytes
    def __init__(self, grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., prepared_job_json: _Optional[bytes] = ..., control_json: _Optional[bytes] = ..., descriptor_json: _Optional[bytes] = ..., read_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., dependency_content_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., dependency_bundle_json: _Optional[bytes] = ..., reconcile_only: bool = ..., native_hydration_index: _Optional[int] = ..., code_execution_intent_json: _Optional[bytes] = ...) -> None: ...

class SubmitRustCompiledSnapshotResponseV1(_message.Message):
    __slots__ = ("status", "result_json", "descriptor_json", "compilation_job_key", "failure_code", "cleanup_pending", "needs_admission", "native_hydration_ready")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_JOB_KEY_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    NEEDS_ADMISSION_FIELD_NUMBER: _ClassVar[int]
    NATIVE_HYDRATION_READY_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    result_json: bytes
    descriptor_json: bytes
    compilation_job_key: bytes
    failure_code: str
    cleanup_pending: bool
    needs_admission: bool
    native_hydration_ready: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., result_json: _Optional[bytes] = ..., descriptor_json: _Optional[bytes] = ..., compilation_job_key: _Optional[bytes] = ..., failure_code: _Optional[str] = ..., cleanup_pending: bool = ..., needs_admission: bool = ..., native_hydration_ready: bool = ...) -> None: ...

class PublishRustCompiledSnapshotRequestV1(_message.Message):
    __slots__ = ("publish_grant", "phase")
    PUBLISH_GRANT_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    publish_grant: SignedSandboxJobGrantV1
    phase: RustCompiledPublicationPhaseV1
    def __init__(self, publish_grant: _Optional[_Union[SignedSandboxJobGrantV1, _Mapping]] = ..., phase: _Optional[_Union[RustCompiledPublicationPhaseV1, str]] = ...) -> None: ...

class PublishRustCompiledSnapshotResponseV1(_message.Message):
    __slots__ = ("status", "failure_code", "cleanup_pending")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    CLEANUP_PENDING_FIELD_NUMBER: _ClassVar[int]
    status: SandboxJobStatusV1
    failure_code: str
    cleanup_pending: bool
    def __init__(self, status: _Optional[_Union[SandboxJobStatusV1, str]] = ..., failure_code: _Optional[str] = ..., cleanup_pending: bool = ...) -> None: ...
