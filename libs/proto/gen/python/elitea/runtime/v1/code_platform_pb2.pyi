from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CodePlatformOperationV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CODE_PLATFORM_OPERATION_V1_UNSPECIFIED: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_APPLICATION_LIST: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_APPLICATION_GET: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_APPLICATION_VERSION_GET: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_USER_GET: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_TOOLKIT_LIST: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_TOOLKIT_CALL: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_SECRET_READ: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_BUCKET_EXISTS: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_BUCKET_CREATE: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_LIST: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_HEAD: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_READ: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_READ_CHUNK: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_BEGIN: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_CHUNK: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_COMMIT: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_APPEND: _ClassVar[CodePlatformOperationV1]
    CODE_PLATFORM_OPERATION_V1_ARTIFACT_DELETE: _ClassVar[CodePlatformOperationV1]

class CodePlatformReceiptStateV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CODE_PLATFORM_RECEIPT_STATE_V1_UNSPECIFIED: _ClassVar[CodePlatformReceiptStateV1]
    CODE_PLATFORM_RECEIPT_STATE_V1_PREPARED: _ClassVar[CodePlatformReceiptStateV1]
    CODE_PLATFORM_RECEIPT_STATE_V1_DISPATCHING: _ClassVar[CodePlatformReceiptStateV1]
    CODE_PLATFORM_RECEIPT_STATE_V1_UNCERTAIN: _ClassVar[CodePlatformReceiptStateV1]
    CODE_PLATFORM_RECEIPT_STATE_V1_COMMITTED: _ClassVar[CodePlatformReceiptStateV1]

class CodePlatformStepDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CODE_PLATFORM_STEP_DISPOSITION_V1_UNSPECIFIED: _ClassVar[CodePlatformStepDispositionV1]
    CODE_PLATFORM_STEP_DISPOSITION_V1_IDLE: _ClassVar[CodePlatformStepDispositionV1]
    CODE_PLATFORM_STEP_DISPOSITION_V1_COMMITTED: _ClassVar[CodePlatformStepDispositionV1]
    CODE_PLATFORM_STEP_DISPOSITION_V1_UNKNOWN: _ClassVar[CodePlatformStepDispositionV1]
CODE_PLATFORM_OPERATION_V1_UNSPECIFIED: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_APPLICATION_LIST: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_APPLICATION_GET: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_APPLICATION_VERSION_GET: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_USER_GET: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_TOOLKIT_LIST: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_TOOLKIT_CALL: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_SECRET_READ: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_BUCKET_EXISTS: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_BUCKET_CREATE: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_LIST: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_HEAD: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_READ: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_READ_CHUNK: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_BEGIN: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_CHUNK: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_WRITE_COMMIT: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_APPEND: CodePlatformOperationV1
CODE_PLATFORM_OPERATION_V1_ARTIFACT_DELETE: CodePlatformOperationV1
CODE_PLATFORM_RECEIPT_STATE_V1_UNSPECIFIED: CodePlatformReceiptStateV1
CODE_PLATFORM_RECEIPT_STATE_V1_PREPARED: CodePlatformReceiptStateV1
CODE_PLATFORM_RECEIPT_STATE_V1_DISPATCHING: CodePlatformReceiptStateV1
CODE_PLATFORM_RECEIPT_STATE_V1_UNCERTAIN: CodePlatformReceiptStateV1
CODE_PLATFORM_RECEIPT_STATE_V1_COMMITTED: CodePlatformReceiptStateV1
CODE_PLATFORM_STEP_DISPOSITION_V1_UNSPECIFIED: CodePlatformStepDispositionV1
CODE_PLATFORM_STEP_DISPOSITION_V1_IDLE: CodePlatformStepDispositionV1
CODE_PLATFORM_STEP_DISPOSITION_V1_COMMITTED: CodePlatformStepDispositionV1
CODE_PLATFORM_STEP_DISPOSITION_V1_UNKNOWN: CodePlatformStepDispositionV1

class CodePlatformJobBindingV1(_message.Message):
    __slots__ = ("revision", "activation_sha256", "prepared_request_sha256", "policy_sha256", "retained_runtime_id")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    ACTIVATION_SHA256_FIELD_NUMBER: _ClassVar[int]
    PREPARED_REQUEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    POLICY_SHA256_FIELD_NUMBER: _ClassVar[int]
    RETAINED_RUNTIME_ID_FIELD_NUMBER: _ClassVar[int]
    revision: int
    activation_sha256: bytes
    prepared_request_sha256: bytes
    policy_sha256: bytes
    retained_runtime_id: str
    def __init__(self, revision: _Optional[int] = ..., activation_sha256: _Optional[bytes] = ..., prepared_request_sha256: _Optional[bytes] = ..., policy_sha256: _Optional[bytes] = ..., retained_runtime_id: _Optional[str] = ...) -> None: ...

class CodePlatformCallV1(_message.Message):
    __slots__ = ("revision", "sequence", "operation", "exact_resource_json", "exact_arguments_json", "binary_payload")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    OPERATION_FIELD_NUMBER: _ClassVar[int]
    EXACT_RESOURCE_JSON_FIELD_NUMBER: _ClassVar[int]
    EXACT_ARGUMENTS_JSON_FIELD_NUMBER: _ClassVar[int]
    BINARY_PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    revision: int
    sequence: int
    operation: CodePlatformOperationV1
    exact_resource_json: bytes
    exact_arguments_json: bytes
    binary_payload: bytes
    def __init__(self, revision: _Optional[int] = ..., sequence: _Optional[int] = ..., operation: _Optional[_Union[CodePlatformOperationV1, str]] = ..., exact_resource_json: _Optional[bytes] = ..., exact_arguments_json: _Optional[bytes] = ..., binary_payload: _Optional[bytes] = ...) -> None: ...

class CodePlatformEffectReceiptV1(_message.Message):
    __slots__ = ("effect_sha256", "call_sha256", "sequence", "state", "encrypted_response_reference", "response_sha256", "owner_receipt")
    EFFECT_SHA256_FIELD_NUMBER: _ClassVar[int]
    CALL_SHA256_FIELD_NUMBER: _ClassVar[int]
    SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    ENCRYPTED_RESPONSE_REFERENCE_FIELD_NUMBER: _ClassVar[int]
    RESPONSE_SHA256_FIELD_NUMBER: _ClassVar[int]
    OWNER_RECEIPT_FIELD_NUMBER: _ClassVar[int]
    effect_sha256: bytes
    call_sha256: bytes
    sequence: int
    state: CodePlatformReceiptStateV1
    encrypted_response_reference: str
    response_sha256: bytes
    owner_receipt: str
    def __init__(self, effect_sha256: _Optional[bytes] = ..., call_sha256: _Optional[bytes] = ..., sequence: _Optional[int] = ..., state: _Optional[_Union[CodePlatformReceiptStateV1, str]] = ..., encrypted_response_reference: _Optional[str] = ..., response_sha256: _Optional[bytes] = ..., owner_receipt: _Optional[str] = ...) -> None: ...

class CodePlatformStepRequestV1(_message.Message):
    __slots__ = ("schema", "revision", "dispatch_activation", "prepared_request_fingerprint")
    SCHEMA_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    DISPATCH_ACTIVATION_FIELD_NUMBER: _ClassVar[int]
    PREPARED_REQUEST_FINGERPRINT_FIELD_NUMBER: _ClassVar[int]
    schema: str
    revision: int
    dispatch_activation: str
    prepared_request_fingerprint: str
    def __init__(self, schema: _Optional[str] = ..., revision: _Optional[int] = ..., dispatch_activation: _Optional[str] = ..., prepared_request_fingerprint: _Optional[str] = ...) -> None: ...

class CodePlatformStepResponseV1(_message.Message):
    __slots__ = ("schema", "revision", "disposition", "effect_id")
    SCHEMA_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    schema: str
    revision: int
    disposition: CodePlatformStepDispositionV1
    effect_id: str
    def __init__(self, schema: _Optional[str] = ..., revision: _Optional[int] = ..., disposition: _Optional[_Union[CodePlatformStepDispositionV1, str]] = ..., effect_id: _Optional[str] = ...) -> None: ...
