from elitea.runtime.v1 import command_pb2 as _command_pb2
from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import envelope_pb2 as _envelope_pb2
from elitea.runtime.v1 import output_pb2 as _output_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RuntimeIdentityV1(_message.Message):
    __slots__ = ("implementation_name", "language", "runtime_version", "build_revision", "source_revision", "artifact_digest", "startup_mode", "conformance_report_digest")
    IMPLEMENTATION_NAME_FIELD_NUMBER: _ClassVar[int]
    LANGUAGE_FIELD_NUMBER: _ClassVar[int]
    RUNTIME_VERSION_FIELD_NUMBER: _ClassVar[int]
    BUILD_REVISION_FIELD_NUMBER: _ClassVar[int]
    SOURCE_REVISION_FIELD_NUMBER: _ClassVar[int]
    ARTIFACT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    STARTUP_MODE_FIELD_NUMBER: _ClassVar[int]
    CONFORMANCE_REPORT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    implementation_name: str
    language: str
    runtime_version: str
    build_revision: str
    source_revision: str
    artifact_digest: _common_pb2.DigestV1
    startup_mode: str
    conformance_report_digest: _common_pb2.DigestV1
    def __init__(self, implementation_name: _Optional[str] = ..., language: _Optional[str] = ..., runtime_version: _Optional[str] = ..., build_revision: _Optional[str] = ..., source_revision: _Optional[str] = ..., artifact_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., startup_mode: _Optional[str] = ..., conformance_report_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ...) -> None: ...

class ProtocolCompatibilityV1(_message.Message):
    __slots__ = ("minimum_major", "minimum_minor", "maximum_major", "maximum_minor", "signature_profiles", "required_feature_flags")
    MINIMUM_MAJOR_FIELD_NUMBER: _ClassVar[int]
    MINIMUM_MINOR_FIELD_NUMBER: _ClassVar[int]
    MAXIMUM_MAJOR_FIELD_NUMBER: _ClassVar[int]
    MAXIMUM_MINOR_FIELD_NUMBER: _ClassVar[int]
    SIGNATURE_PROFILES_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_FEATURE_FLAGS_FIELD_NUMBER: _ClassVar[int]
    minimum_major: int
    minimum_minor: int
    maximum_major: int
    maximum_minor: int
    signature_profiles: _containers.RepeatedScalarFieldContainer[_envelope_pb2.SignatureProfileV1]
    required_feature_flags: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, minimum_major: _Optional[int] = ..., minimum_minor: _Optional[int] = ..., maximum_major: _Optional[int] = ..., maximum_minor: _Optional[int] = ..., signature_profiles: _Optional[_Iterable[_Union[_envelope_pb2.SignatureProfileV1, str]]] = ..., required_feature_flags: _Optional[_Iterable[str]] = ...) -> None: ...

class SDKFrameworkCompatibilityV1(_message.Message):
    __slots__ = ("elitea_sdk_revision", "elitea_sdk_artifact_digest", "configuration_catalog_revision", "configuration_catalog_digest", "framework_revisions")
    ELITEA_SDK_REVISION_FIELD_NUMBER: _ClassVar[int]
    ELITEA_SDK_ARTIFACT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_CATALOG_REVISION_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_CATALOG_DIGEST_FIELD_NUMBER: _ClassVar[int]
    FRAMEWORK_REVISIONS_FIELD_NUMBER: _ClassVar[int]
    elitea_sdk_revision: str
    elitea_sdk_artifact_digest: _common_pb2.DigestV1
    configuration_catalog_revision: str
    configuration_catalog_digest: _common_pb2.DigestV1
    framework_revisions: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, elitea_sdk_revision: _Optional[str] = ..., elitea_sdk_artifact_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., configuration_catalog_revision: _Optional[str] = ..., configuration_catalog_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., framework_revisions: _Optional[_Iterable[str]] = ...) -> None: ...

class RuntimeCapabilityV1(_message.Message):
    __slots__ = ("capability_id", "capability_version", "accepted_command_types", "emitted_event_types", "interaction_model", "resource_classes", "feature_flags", "catalog_revision", "catalog_digest", "schema_id", "schema_revision", "schema_digest")
    CAPABILITY_ID_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_VERSION_FIELD_NUMBER: _ClassVar[int]
    ACCEPTED_COMMAND_TYPES_FIELD_NUMBER: _ClassVar[int]
    EMITTED_EVENT_TYPES_FIELD_NUMBER: _ClassVar[int]
    INTERACTION_MODEL_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_CLASSES_FIELD_NUMBER: _ClassVar[int]
    FEATURE_FLAGS_FIELD_NUMBER: _ClassVar[int]
    CATALOG_REVISION_FIELD_NUMBER: _ClassVar[int]
    CATALOG_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_ID_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_DIGEST_FIELD_NUMBER: _ClassVar[int]
    capability_id: str
    capability_version: str
    accepted_command_types: _containers.RepeatedScalarFieldContainer[_command_pb2.WorkerCommandTypeV1]
    emitted_event_types: _containers.RepeatedScalarFieldContainer[_output_pb2.ExecutionOutputEventTypeV1]
    interaction_model: str
    resource_classes: _containers.RepeatedScalarFieldContainer[str]
    feature_flags: _containers.RepeatedScalarFieldContainer[str]
    catalog_revision: str
    catalog_digest: _common_pb2.DigestV1
    schema_id: str
    schema_revision: str
    schema_digest: _common_pb2.DigestV1
    def __init__(self, capability_id: _Optional[str] = ..., capability_version: _Optional[str] = ..., accepted_command_types: _Optional[_Iterable[_Union[_command_pb2.WorkerCommandTypeV1, str]]] = ..., emitted_event_types: _Optional[_Iterable[_Union[_output_pb2.ExecutionOutputEventTypeV1, str]]] = ..., interaction_model: _Optional[str] = ..., resource_classes: _Optional[_Iterable[str]] = ..., feature_flags: _Optional[_Iterable[str]] = ..., catalog_revision: _Optional[str] = ..., catalog_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., schema_id: _Optional[str] = ..., schema_revision: _Optional[str] = ..., schema_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ...) -> None: ...

class RuntimeConstraintsV1(_message.Message):
    __slots__ = ("isolation_classes", "architectures", "child_process_support", "network_egress_classes", "artifact_support", "realtime_session_support")
    ISOLATION_CLASSES_FIELD_NUMBER: _ClassVar[int]
    ARCHITECTURES_FIELD_NUMBER: _ClassVar[int]
    CHILD_PROCESS_SUPPORT_FIELD_NUMBER: _ClassVar[int]
    NETWORK_EGRESS_CLASSES_FIELD_NUMBER: _ClassVar[int]
    ARTIFACT_SUPPORT_FIELD_NUMBER: _ClassVar[int]
    REALTIME_SESSION_SUPPORT_FIELD_NUMBER: _ClassVar[int]
    isolation_classes: _containers.RepeatedScalarFieldContainer[str]
    architectures: _containers.RepeatedScalarFieldContainer[str]
    child_process_support: bool
    network_egress_classes: _containers.RepeatedScalarFieldContainer[str]
    artifact_support: bool
    realtime_session_support: bool
    def __init__(self, isolation_classes: _Optional[_Iterable[str]] = ..., architectures: _Optional[_Iterable[str]] = ..., child_process_support: _Optional[bool] = ..., network_egress_classes: _Optional[_Iterable[str]] = ..., artifact_support: _Optional[bool] = ..., realtime_session_support: _Optional[bool] = ...) -> None: ...

class RuntimeLimitsProfileReferenceV1(_message.Message):
    __slots__ = ("limits_schema_revision", "limits_revisions", "resource_profile_classes")
    LIMITS_SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    LIMITS_REVISIONS_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_PROFILE_CLASSES_FIELD_NUMBER: _ClassVar[int]
    limits_schema_revision: str
    limits_revisions: _containers.RepeatedScalarFieldContainer[str]
    resource_profile_classes: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, limits_schema_revision: _Optional[str] = ..., limits_revisions: _Optional[_Iterable[str]] = ..., resource_profile_classes: _Optional[_Iterable[str]] = ...) -> None: ...

class RuntimeCapabilitiesV1(_message.Message):
    __slots__ = ("manifest_schema_revision", "runtime_identity", "protocol_compatibility", "sdk_framework_compatibility", "capabilities", "runtime_constraints", "limits_profiles")
    MANIFEST_SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    RUNTIME_IDENTITY_FIELD_NUMBER: _ClassVar[int]
    PROTOCOL_COMPATIBILITY_FIELD_NUMBER: _ClassVar[int]
    SDK_FRAMEWORK_COMPATIBILITY_FIELD_NUMBER: _ClassVar[int]
    CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    RUNTIME_CONSTRAINTS_FIELD_NUMBER: _ClassVar[int]
    LIMITS_PROFILES_FIELD_NUMBER: _ClassVar[int]
    manifest_schema_revision: str
    runtime_identity: RuntimeIdentityV1
    protocol_compatibility: ProtocolCompatibilityV1
    sdk_framework_compatibility: SDKFrameworkCompatibilityV1
    capabilities: _containers.RepeatedCompositeFieldContainer[RuntimeCapabilityV1]
    runtime_constraints: RuntimeConstraintsV1
    limits_profiles: RuntimeLimitsProfileReferenceV1
    def __init__(self, manifest_schema_revision: _Optional[str] = ..., runtime_identity: _Optional[_Union[RuntimeIdentityV1, _Mapping]] = ..., protocol_compatibility: _Optional[_Union[ProtocolCompatibilityV1, _Mapping]] = ..., sdk_framework_compatibility: _Optional[_Union[SDKFrameworkCompatibilityV1, _Mapping]] = ..., capabilities: _Optional[_Iterable[_Union[RuntimeCapabilityV1, _Mapping]]] = ..., runtime_constraints: _Optional[_Union[RuntimeConstraintsV1, _Mapping]] = ..., limits_profiles: _Optional[_Union[RuntimeLimitsProfileReferenceV1, _Mapping]] = ...) -> None: ...
