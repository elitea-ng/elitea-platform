from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class InternalToolScopeV1(_message.Message):
    __slots__ = ("project_id", "scope")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    scope: str
    def __init__(self, project_id: _Optional[int] = ..., scope: _Optional[str] = ...) -> None: ...

class InternalToolRefusalV1(_message.Message):
    __slots__ = ("http_status", "body")
    HTTP_STATUS_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    http_status: int
    body: bytes
    def __init__(self, http_status: _Optional[int] = ..., body: _Optional[bytes] = ...) -> None: ...

class InternalToolRpcErrorV1(_message.Message):
    __slots__ = ("code", "message")
    CODE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    code: int
    message: str
    def __init__(self, code: _Optional[int] = ..., message: _Optional[str] = ...) -> None: ...

class InternalToolListRequestV1(_message.Message):
    __slots__ = ("scope",)
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    scope: InternalToolScopeV1
    def __init__(self, scope: _Optional[_Union[InternalToolScopeV1, _Mapping]] = ...) -> None: ...

class InternalToolListResponseV1(_message.Message):
    __slots__ = ("result_json", "error")
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    result_json: bytes
    error: InternalToolRpcErrorV1
    def __init__(self, result_json: _Optional[bytes] = ..., error: _Optional[_Union[InternalToolRpcErrorV1, _Mapping]] = ...) -> None: ...

class InternalToolCallRequestV1(_message.Message):
    __slots__ = ("scope", "name", "arguments_json")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_JSON_FIELD_NUMBER: _ClassVar[int]
    scope: InternalToolScopeV1
    name: str
    arguments_json: bytes
    def __init__(self, scope: _Optional[_Union[InternalToolScopeV1, _Mapping]] = ..., name: _Optional[str] = ..., arguments_json: _Optional[bytes] = ...) -> None: ...

class InternalToolProgressV1(_message.Message):
    __slots__ = ("stage",)
    STAGE_FIELD_NUMBER: _ClassVar[int]
    stage: str
    def __init__(self, stage: _Optional[str] = ...) -> None: ...

class InternalToolCallFrameV1(_message.Message):
    __slots__ = ("progress", "result_json", "error")
    PROGRESS_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    progress: InternalToolProgressV1
    result_json: bytes
    error: InternalToolRpcErrorV1
    def __init__(self, progress: _Optional[_Union[InternalToolProgressV1, _Mapping]] = ..., result_json: _Optional[bytes] = ..., error: _Optional[_Union[InternalToolRpcErrorV1, _Mapping]] = ...) -> None: ...

class InternalToolInvokeRequestV1(_message.Message):
    __slots__ = ("project_id", "toolkit_id", "tool_name", "tool_params_json", "request_id")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    TOOL_NAME_FIELD_NUMBER: _ClassVar[int]
    TOOL_PARAMS_JSON_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    project_id: int
    toolkit_id: str
    tool_name: str
    tool_params_json: bytes
    request_id: str
    def __init__(self, project_id: _Optional[int] = ..., toolkit_id: _Optional[str] = ..., tool_name: _Optional[str] = ..., tool_params_json: _Optional[bytes] = ..., request_id: _Optional[str] = ...) -> None: ...

class InternalToolInvokeFrameV1(_message.Message):
    __slots__ = ("progress", "result_json")
    PROGRESS_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    progress: InternalToolProgressV1
    result_json: bytes
    def __init__(self, progress: _Optional[_Union[InternalToolProgressV1, _Mapping]] = ..., result_json: _Optional[bytes] = ...) -> None: ...
