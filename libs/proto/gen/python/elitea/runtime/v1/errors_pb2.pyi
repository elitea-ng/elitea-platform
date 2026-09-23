from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RuntimeErrorCodeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RUNTIME_ERROR_CODE_V1_UNSPECIFIED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_INVALID_INPUT: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_STALE_FENCE: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_CANCELLED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_INTERNAL: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED: _ClassVar[RuntimeErrorCodeV1]
    RUNTIME_ERROR_CODE_V1_OUTPUT_CONTINUATION_EXHAUSTED: _ClassVar[RuntimeErrorCodeV1]
RUNTIME_ERROR_CODE_V1_UNSPECIFIED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_INCOMPATIBLE_VERSION: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_INVALID_INPUT: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_STALE_FENCE: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_CANCELLED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_INTERNAL: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_DEADLINE_EXCEEDED: RuntimeErrorCodeV1
RUNTIME_ERROR_CODE_V1_OUTPUT_CONTINUATION_EXHAUSTED: RuntimeErrorCodeV1

class RuntimeErrorV1(_message.Message):
    __slots__ = ("code", "safe_message", "retryable")
    CODE_FIELD_NUMBER: _ClassVar[int]
    SAFE_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    RETRYABLE_FIELD_NUMBER: _ClassVar[int]
    code: RuntimeErrorCodeV1
    safe_message: str
    retryable: bool
    def __init__(self, code: _Optional[_Union[RuntimeErrorCodeV1, str]] = ..., safe_message: _Optional[str] = ..., retryable: bool = ...) -> None: ...
