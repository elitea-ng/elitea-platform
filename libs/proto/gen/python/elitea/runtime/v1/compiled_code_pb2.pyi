from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import envelope_pb2 as _envelope_pb2
from elitea.runtime.v1 import sandbox_pb2 as _sandbox_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RustCompiledSnapshotPurposeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RUST_COMPILED_SNAPSHOT_PURPOSE_V1_UNSPECIFIED: _ClassVar[RustCompiledSnapshotPurposeV1]
    RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE: _ClassVar[RustCompiledSnapshotPurposeV1]
    RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH: _ClassVar[RustCompiledSnapshotPurposeV1]
    RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ: _ClassVar[RustCompiledSnapshotPurposeV1]
    RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE: _ClassVar[RustCompiledSnapshotPurposeV1]
RUST_COMPILED_SNAPSHOT_PURPOSE_V1_UNSPECIFIED: RustCompiledSnapshotPurposeV1
RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE: RustCompiledSnapshotPurposeV1
RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH: RustCompiledSnapshotPurposeV1
RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ: RustCompiledSnapshotPurposeV1
RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE: RustCompiledSnapshotPurposeV1

class RustCompiledSnapshotBindingV1(_message.Message):
    __slots__ = ("revision", "reuse_policy", "tenant_id", "project_id", "base_prepared_request_sha256", "source_sha256", "compilation_image_digest", "execution_image_digest", "platform", "target", "policy_revision", "cargo_manifest_sha256", "cargo_lock_sha256", "cargo_config_sha256", "vendor_sha256", "toolchain_sha256", "adapter_sha256", "wrapper_sha256", "compiler_flags_sha256")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    REUSE_POLICY_FIELD_NUMBER: _ClassVar[int]
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BASE_PREPARED_REQUEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    SOURCE_SHA256_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_IMAGE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_IMAGE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PLATFORM_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    POLICY_REVISION_FIELD_NUMBER: _ClassVar[int]
    CARGO_MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    CARGO_LOCK_SHA256_FIELD_NUMBER: _ClassVar[int]
    CARGO_CONFIG_SHA256_FIELD_NUMBER: _ClassVar[int]
    VENDOR_SHA256_FIELD_NUMBER: _ClassVar[int]
    TOOLCHAIN_SHA256_FIELD_NUMBER: _ClassVar[int]
    ADAPTER_SHA256_FIELD_NUMBER: _ClassVar[int]
    WRAPPER_SHA256_FIELD_NUMBER: _ClassVar[int]
    COMPILER_FLAGS_SHA256_FIELD_NUMBER: _ClassVar[int]
    revision: int
    reuse_policy: str
    tenant_id: str
    project_id: int
    base_prepared_request_sha256: bytes
    source_sha256: bytes
    compilation_image_digest: str
    execution_image_digest: str
    platform: str
    target: str
    policy_revision: str
    cargo_manifest_sha256: bytes
    cargo_lock_sha256: bytes
    cargo_config_sha256: bytes
    vendor_sha256: bytes
    toolchain_sha256: bytes
    adapter_sha256: bytes
    wrapper_sha256: bytes
    compiler_flags_sha256: bytes
    def __init__(self, revision: _Optional[int] = ..., reuse_policy: _Optional[str] = ..., tenant_id: _Optional[str] = ..., project_id: _Optional[int] = ..., base_prepared_request_sha256: _Optional[bytes] = ..., source_sha256: _Optional[bytes] = ..., compilation_image_digest: _Optional[str] = ..., execution_image_digest: _Optional[str] = ..., platform: _Optional[str] = ..., target: _Optional[str] = ..., policy_revision: _Optional[str] = ..., cargo_manifest_sha256: _Optional[bytes] = ..., cargo_lock_sha256: _Optional[bytes] = ..., cargo_config_sha256: _Optional[bytes] = ..., vendor_sha256: _Optional[bytes] = ..., toolchain_sha256: _Optional[bytes] = ..., adapter_sha256: _Optional[bytes] = ..., wrapper_sha256: _Optional[bytes] = ..., compiler_flags_sha256: _Optional[bytes] = ...) -> None: ...

class RustCompiledSnapshotDescriptorV1(_message.Message):
    __slots__ = ("revision", "binding", "snapshot_key_sha256", "executable_sha256", "executable_bytes")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    BINDING_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_KEY_SHA256_FIELD_NUMBER: _ClassVar[int]
    EXECUTABLE_SHA256_FIELD_NUMBER: _ClassVar[int]
    EXECUTABLE_BYTES_FIELD_NUMBER: _ClassVar[int]
    revision: int
    binding: RustCompiledSnapshotBindingV1
    snapshot_key_sha256: bytes
    executable_sha256: bytes
    executable_bytes: int
    def __init__(self, revision: _Optional[int] = ..., binding: _Optional[_Union[RustCompiledSnapshotBindingV1, _Mapping]] = ..., snapshot_key_sha256: _Optional[bytes] = ..., executable_sha256: _Optional[bytes] = ..., executable_bytes: _Optional[int] = ...) -> None: ...

class RustCompiledSnapshotControlV1(_message.Message):
    __slots__ = ("revision", "binding", "snapshot_key_sha256", "descriptor_sha256")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    BINDING_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_KEY_SHA256_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_SHA256_FIELD_NUMBER: _ClassVar[int]
    revision: int
    binding: RustCompiledSnapshotBindingV1
    snapshot_key_sha256: bytes
    descriptor_sha256: bytes
    def __init__(self, revision: _Optional[int] = ..., binding: _Optional[_Union[RustCompiledSnapshotBindingV1, _Mapping]] = ..., snapshot_key_sha256: _Optional[bytes] = ..., descriptor_sha256: _Optional[bytes] = ...) -> None: ...

class OriginalCodeVisitRefV1(_message.Message):
    __slots__ = ("visit_id", "revision", "digest_sha256")
    VISIT_ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    DIGEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    visit_id: str
    revision: int
    digest_sha256: str
    def __init__(self, visit_id: _Optional[str] = ..., revision: _Optional[int] = ..., digest_sha256: _Optional[str] = ...) -> None: ...

class OriginalCodeVisitAccessV1(_message.Message):
    __slots__ = ("original_visit", "claim_id", "claim_attempt", "lease_epoch", "fence_sha256")
    ORIGINAL_VISIT_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ATTEMPT_FIELD_NUMBER: _ClassVar[int]
    LEASE_EPOCH_FIELD_NUMBER: _ClassVar[int]
    FENCE_SHA256_FIELD_NUMBER: _ClassVar[int]
    original_visit: OriginalCodeVisitRefV1
    claim_id: str
    claim_attempt: int
    lease_epoch: int
    fence_sha256: bytes
    def __init__(self, original_visit: _Optional[_Union[OriginalCodeVisitRefV1, _Mapping]] = ..., claim_id: _Optional[str] = ..., claim_attempt: _Optional[int] = ..., lease_epoch: _Optional[int] = ..., fence_sha256: _Optional[bytes] = ...) -> None: ...

class RustCompiledSnapshotGrantClaimsV1(_message.Message):
    __slots__ = ("revision", "tenant_id", "project_id", "execution_id", "activation_id", "request_digest", "submitter_workload_identity", "audience", "issued_at_unix_millis", "expires_at_unix_millis", "generation", "purpose", "base_prepared_request_sha256", "snapshot_key_sha256", "descriptor_sha256", "compilation_job_key", "compilation_runtime_id", "compilation_request_digest", "compilation_lease_epoch", "original_code_visit_access")
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
    PURPOSE_FIELD_NUMBER: _ClassVar[int]
    BASE_PREPARED_REQUEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_KEY_SHA256_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_SHA256_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_JOB_KEY_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_RUNTIME_ID_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_REQUEST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_LEASE_EPOCH_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_CODE_VISIT_ACCESS_FIELD_NUMBER: _ClassVar[int]
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
    purpose: RustCompiledSnapshotPurposeV1
    base_prepared_request_sha256: bytes
    snapshot_key_sha256: bytes
    descriptor_sha256: bytes
    compilation_job_key: bytes
    compilation_runtime_id: str
    compilation_request_digest: bytes
    compilation_lease_epoch: int
    original_code_visit_access: OriginalCodeVisitAccessV1
    def __init__(self, revision: _Optional[int] = ..., tenant_id: _Optional[str] = ..., project_id: _Optional[int] = ..., execution_id: _Optional[str] = ..., activation_id: _Optional[str] = ..., request_digest: _Optional[bytes] = ..., submitter_workload_identity: _Optional[str] = ..., audience: _Optional[str] = ..., issued_at_unix_millis: _Optional[int] = ..., expires_at_unix_millis: _Optional[int] = ..., generation: _Optional[int] = ..., purpose: _Optional[_Union[RustCompiledSnapshotPurposeV1, str]] = ..., base_prepared_request_sha256: _Optional[bytes] = ..., snapshot_key_sha256: _Optional[bytes] = ..., descriptor_sha256: _Optional[bytes] = ..., compilation_job_key: _Optional[bytes] = ..., compilation_runtime_id: _Optional[str] = ..., compilation_request_digest: _Optional[bytes] = ..., compilation_lease_epoch: _Optional[int] = ..., original_code_visit_access: _Optional[_Union[OriginalCodeVisitAccessV1, _Mapping]] = ...) -> None: ...

class AuthorizeRustCompiledSnapshotRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "signed_command", "activation_id", "audience", "purpose", "prepared_job_json", "binding_json", "compilation_job_key", "selected_descriptor_sha256", "original_code_visit")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    SIGNED_COMMAND_FIELD_NUMBER: _ClassVar[int]
    ACTIVATION_ID_FIELD_NUMBER: _ClassVar[int]
    AUDIENCE_FIELD_NUMBER: _ClassVar[int]
    PURPOSE_FIELD_NUMBER: _ClassVar[int]
    PREPARED_JOB_JSON_FIELD_NUMBER: _ClassVar[int]
    BINDING_JSON_FIELD_NUMBER: _ClassVar[int]
    COMPILATION_JOB_KEY_FIELD_NUMBER: _ClassVar[int]
    SELECTED_DESCRIPTOR_SHA256_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_CODE_VISIT_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    signed_command: _envelope_pb2.SignedWorkerCommandEnvelopeV1
    activation_id: str
    audience: str
    purpose: RustCompiledSnapshotPurposeV1
    prepared_job_json: bytes
    binding_json: bytes
    compilation_job_key: bytes
    selected_descriptor_sha256: bytes
    original_code_visit: OriginalCodeVisitRefV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., signed_command: _Optional[_Union[_envelope_pb2.SignedWorkerCommandEnvelopeV1, _Mapping]] = ..., activation_id: _Optional[str] = ..., audience: _Optional[str] = ..., purpose: _Optional[_Union[RustCompiledSnapshotPurposeV1, str]] = ..., prepared_job_json: _Optional[bytes] = ..., binding_json: _Optional[bytes] = ..., compilation_job_key: _Optional[bytes] = ..., selected_descriptor_sha256: _Optional[bytes] = ..., original_code_visit: _Optional[_Union[OriginalCodeVisitRefV1, _Mapping]] = ...) -> None: ...

class AuthorizeRustCompiledSnapshotResponseV1(_message.Message):
    __slots__ = ("grant", "descriptor_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTOR_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    descriptor_json: bytes
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., descriptor_json: _Optional[bytes] = ...) -> None: ...
