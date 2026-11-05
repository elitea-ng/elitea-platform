from elitea.runtime.v1 import sandbox_pb2 as _sandbox_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class BrowserEngineV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BROWSER_ENGINE_V1_UNSPECIFIED: _ClassVar[BrowserEngineV1]
    BROWSER_ENGINE_V1_FULL: _ClassVar[BrowserEngineV1]
    BROWSER_ENGINE_V1_LITE: _ClassVar[BrowserEngineV1]

class BrowserPointerKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BROWSER_POINTER_KIND_V1_UNSPECIFIED: _ClassVar[BrowserPointerKindV1]
    BROWSER_POINTER_KIND_V1_MOVE: _ClassVar[BrowserPointerKindV1]
    BROWSER_POINTER_KIND_V1_DOWN: _ClassVar[BrowserPointerKindV1]
    BROWSER_POINTER_KIND_V1_UP: _ClassVar[BrowserPointerKindV1]

class BrowserKeyKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BROWSER_KEY_KIND_V1_UNSPECIFIED: _ClassVar[BrowserKeyKindV1]
    BROWSER_KEY_KIND_V1_DOWN: _ClassVar[BrowserKeyKindV1]
    BROWSER_KEY_KIND_V1_UP: _ClassVar[BrowserKeyKindV1]
    BROWSER_KEY_KIND_V1_CHAR: _ClassVar[BrowserKeyKindV1]

class BrowserControlV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BROWSER_CONTROL_V1_UNSPECIFIED: _ClassVar[BrowserControlV1]
    BROWSER_CONTROL_V1_AGENT: _ClassVar[BrowserControlV1]
    BROWSER_CONTROL_V1_USER: _ClassVar[BrowserControlV1]

class BrowserSessionEndV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BROWSER_SESSION_END_V1_UNSPECIFIED: _ClassVar[BrowserSessionEndV1]
    BROWSER_SESSION_END_V1_RUN_ENDED: _ClassVar[BrowserSessionEndV1]
    BROWSER_SESSION_END_V1_NOW: _ClassVar[BrowserSessionEndV1]
BROWSER_ENGINE_V1_UNSPECIFIED: BrowserEngineV1
BROWSER_ENGINE_V1_FULL: BrowserEngineV1
BROWSER_ENGINE_V1_LITE: BrowserEngineV1
BROWSER_POINTER_KIND_V1_UNSPECIFIED: BrowserPointerKindV1
BROWSER_POINTER_KIND_V1_MOVE: BrowserPointerKindV1
BROWSER_POINTER_KIND_V1_DOWN: BrowserPointerKindV1
BROWSER_POINTER_KIND_V1_UP: BrowserPointerKindV1
BROWSER_KEY_KIND_V1_UNSPECIFIED: BrowserKeyKindV1
BROWSER_KEY_KIND_V1_DOWN: BrowserKeyKindV1
BROWSER_KEY_KIND_V1_UP: BrowserKeyKindV1
BROWSER_KEY_KIND_V1_CHAR: BrowserKeyKindV1
BROWSER_CONTROL_V1_UNSPECIFIED: BrowserControlV1
BROWSER_CONTROL_V1_AGENT: BrowserControlV1
BROWSER_CONTROL_V1_USER: BrowserControlV1
BROWSER_SESSION_END_V1_UNSPECIFIED: BrowserSessionEndV1
BROWSER_SESSION_END_V1_RUN_ENDED: BrowserSessionEndV1
BROWSER_SESSION_END_V1_NOW: BrowserSessionEndV1

class BrowserSessionSpecV1(_message.Message):
    __slots__ = ("revision", "name", "engine", "cpu_millis", "memory_bytes", "wall_seconds", "max_pages")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ENGINE_FIELD_NUMBER: _ClassVar[int]
    CPU_MILLIS_FIELD_NUMBER: _ClassVar[int]
    MEMORY_BYTES_FIELD_NUMBER: _ClassVar[int]
    WALL_SECONDS_FIELD_NUMBER: _ClassVar[int]
    MAX_PAGES_FIELD_NUMBER: _ClassVar[int]
    revision: int
    name: str
    engine: BrowserEngineV1
    cpu_millis: int
    memory_bytes: int
    wall_seconds: int
    max_pages: int
    def __init__(self, revision: _Optional[int] = ..., name: _Optional[str] = ..., engine: _Optional[_Union[BrowserEngineV1, str]] = ..., cpu_millis: _Optional[int] = ..., memory_bytes: _Optional[int] = ..., wall_seconds: _Optional[int] = ..., max_pages: _Optional[int] = ...) -> None: ...

class BrowserSessionEgressV1(_message.Message):
    __slots__ = ("proxy_url", "proxy_token")
    PROXY_URL_FIELD_NUMBER: _ClassVar[int]
    PROXY_TOKEN_FIELD_NUMBER: _ClassVar[int]
    proxy_url: str
    proxy_token: str
    def __init__(self, proxy_url: _Optional[str] = ..., proxy_token: _Optional[str] = ...) -> None: ...

class StartBrowserSessionRequestV1(_message.Message):
    __slots__ = ("grant", "spec", "egress")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    EGRESS_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: BrowserSessionSpecV1
    egress: BrowserSessionEgressV1
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[BrowserSessionSpecV1, _Mapping]] = ..., egress: _Optional[_Union[BrowserSessionEgressV1, _Mapping]] = ...) -> None: ...

class StartBrowserSessionResponseV1(_message.Message):
    __slots__ = ("created", "engine_version")
    CREATED_FIELD_NUMBER: _ClassVar[int]
    ENGINE_VERSION_FIELD_NUMBER: _ClassVar[int]
    created: bool
    engine_version: str
    def __init__(self, created: _Optional[bool] = ..., engine_version: _Optional[str] = ...) -> None: ...

class BrowserCallRequestV1(_message.Message):
    __slots__ = ("grant", "spec", "call_id", "name", "arguments_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    CALL_ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: BrowserSessionSpecV1
    call_id: str
    name: str
    arguments_json: str
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[BrowserSessionSpecV1, _Mapping]] = ..., call_id: _Optional[str] = ..., name: _Optional[str] = ..., arguments_json: _Optional[str] = ...) -> None: ...

class BrowserImageV1(_message.Message):
    __slots__ = ("media_type", "data")
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    media_type: str
    data: bytes
    def __init__(self, media_type: _Optional[str] = ..., data: _Optional[bytes] = ...) -> None: ...

class BrowserCallResponseV1(_message.Message):
    __slots__ = ("result_json", "image")
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    IMAGE_FIELD_NUMBER: _ClassVar[int]
    result_json: str
    image: BrowserImageV1
    def __init__(self, result_json: _Optional[str] = ..., image: _Optional[_Union[BrowserImageV1, _Mapping]] = ...) -> None: ...

class BrowserViewOpenV1(_message.Message):
    __slots__ = ("grant", "spec", "control", "max_fps")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    CONTROL_FIELD_NUMBER: _ClassVar[int]
    MAX_FPS_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: BrowserSessionSpecV1
    control: bool
    max_fps: int
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[BrowserSessionSpecV1, _Mapping]] = ..., control: _Optional[bool] = ..., max_fps: _Optional[int] = ...) -> None: ...

class BrowserPointerEventV1(_message.Message):
    __slots__ = ("kind", "x", "y", "button", "click_count", "modifiers")
    KIND_FIELD_NUMBER: _ClassVar[int]
    X_FIELD_NUMBER: _ClassVar[int]
    Y_FIELD_NUMBER: _ClassVar[int]
    BUTTON_FIELD_NUMBER: _ClassVar[int]
    CLICK_COUNT_FIELD_NUMBER: _ClassVar[int]
    MODIFIERS_FIELD_NUMBER: _ClassVar[int]
    kind: BrowserPointerKindV1
    x: float
    y: float
    button: int
    click_count: int
    modifiers: int
    def __init__(self, kind: _Optional[_Union[BrowserPointerKindV1, str]] = ..., x: _Optional[float] = ..., y: _Optional[float] = ..., button: _Optional[int] = ..., click_count: _Optional[int] = ..., modifiers: _Optional[int] = ...) -> None: ...

class BrowserWheelEventV1(_message.Message):
    __slots__ = ("x", "y", "delta_x", "delta_y", "modifiers")
    X_FIELD_NUMBER: _ClassVar[int]
    Y_FIELD_NUMBER: _ClassVar[int]
    DELTA_X_FIELD_NUMBER: _ClassVar[int]
    DELTA_Y_FIELD_NUMBER: _ClassVar[int]
    MODIFIERS_FIELD_NUMBER: _ClassVar[int]
    x: float
    y: float
    delta_x: float
    delta_y: float
    modifiers: int
    def __init__(self, x: _Optional[float] = ..., y: _Optional[float] = ..., delta_x: _Optional[float] = ..., delta_y: _Optional[float] = ..., modifiers: _Optional[int] = ...) -> None: ...

class BrowserKeyEventV1(_message.Message):
    __slots__ = ("kind", "key", "code", "text", "modifiers")
    KIND_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    MODIFIERS_FIELD_NUMBER: _ClassVar[int]
    kind: BrowserKeyKindV1
    key: str
    code: str
    text: str
    modifiers: int
    def __init__(self, kind: _Optional[_Union[BrowserKeyKindV1, str]] = ..., key: _Optional[str] = ..., code: _Optional[str] = ..., text: _Optional[str] = ..., modifiers: _Optional[int] = ...) -> None: ...

class BrowserHandBackV1(_message.Message):
    __slots__ = ("note",)
    NOTE_FIELD_NUMBER: _ClassVar[int]
    note: str
    def __init__(self, note: _Optional[str] = ...) -> None: ...

class BrowserInputEventV1(_message.Message):
    __slots__ = ("pointer", "wheel", "key", "take_over", "hand_back")
    POINTER_FIELD_NUMBER: _ClassVar[int]
    WHEEL_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    TAKE_OVER_FIELD_NUMBER: _ClassVar[int]
    HAND_BACK_FIELD_NUMBER: _ClassVar[int]
    pointer: BrowserPointerEventV1
    wheel: BrowserWheelEventV1
    key: BrowserKeyEventV1
    take_over: bool
    hand_back: BrowserHandBackV1
    def __init__(self, pointer: _Optional[_Union[BrowserPointerEventV1, _Mapping]] = ..., wheel: _Optional[_Union[BrowserWheelEventV1, _Mapping]] = ..., key: _Optional[_Union[BrowserKeyEventV1, _Mapping]] = ..., take_over: _Optional[bool] = ..., hand_back: _Optional[_Union[BrowserHandBackV1, _Mapping]] = ...) -> None: ...

class BrowserViewInputV1(_message.Message):
    __slots__ = ("open", "input", "ack")
    OPEN_FIELD_NUMBER: _ClassVar[int]
    INPUT_FIELD_NUMBER: _ClassVar[int]
    ACK_FIELD_NUMBER: _ClassVar[int]
    open: BrowserViewOpenV1
    input: BrowserInputEventV1
    ack: int
    def __init__(self, open: _Optional[_Union[BrowserViewOpenV1, _Mapping]] = ..., input: _Optional[_Union[BrowserInputEventV1, _Mapping]] = ..., ack: _Optional[int] = ...) -> None: ...

class BrowserFrameV1(_message.Message):
    __slots__ = ("seq", "jpeg", "width", "height", "url", "title", "control")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    JPEG_FIELD_NUMBER: _ClassVar[int]
    WIDTH_FIELD_NUMBER: _ClassVar[int]
    HEIGHT_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    CONTROL_FIELD_NUMBER: _ClassVar[int]
    seq: int
    jpeg: bytes
    width: int
    height: int
    url: str
    title: str
    control: BrowserControlV1
    def __init__(self, seq: _Optional[int] = ..., jpeg: _Optional[bytes] = ..., width: _Optional[int] = ..., height: _Optional[int] = ..., url: _Optional[str] = ..., title: _Optional[str] = ..., control: _Optional[_Union[BrowserControlV1, str]] = ...) -> None: ...

class BrowserViewNoticeV1(_message.Message):
    __slots__ = ("code",)
    CODE_FIELD_NUMBER: _ClassVar[int]
    code: str
    def __init__(self, code: _Optional[str] = ...) -> None: ...

class BrowserViewEndV1(_message.Message):
    __slots__ = ("code",)
    CODE_FIELD_NUMBER: _ClassVar[int]
    code: str
    def __init__(self, code: _Optional[str] = ...) -> None: ...

class BrowserViewOutputV1(_message.Message):
    __slots__ = ("screen", "notice", "end")
    SCREEN_FIELD_NUMBER: _ClassVar[int]
    NOTICE_FIELD_NUMBER: _ClassVar[int]
    END_FIELD_NUMBER: _ClassVar[int]
    screen: BrowserFrameV1
    notice: BrowserViewNoticeV1
    end: BrowserViewEndV1
    def __init__(self, screen: _Optional[_Union[BrowserFrameV1, _Mapping]] = ..., notice: _Optional[_Union[BrowserViewNoticeV1, _Mapping]] = ..., end: _Optional[_Union[BrowserViewEndV1, _Mapping]] = ...) -> None: ...

class DestroyBrowserSessionRequestV1(_message.Message):
    __slots__ = ("grant", "spec", "end")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    END_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: BrowserSessionSpecV1
    end: BrowserSessionEndV1
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[BrowserSessionSpecV1, _Mapping]] = ..., end: _Optional[_Union[BrowserSessionEndV1, str]] = ...) -> None: ...

class DestroyBrowserSessionResponseV1(_message.Message):
    __slots__ = ("destroyed", "linger_until_unix_seconds")
    DESTROYED_FIELD_NUMBER: _ClassVar[int]
    LINGER_UNTIL_UNIX_SECONDS_FIELD_NUMBER: _ClassVar[int]
    destroyed: bool
    linger_until_unix_seconds: int
    def __init__(self, destroyed: _Optional[bool] = ..., linger_until_unix_seconds: _Optional[int] = ...) -> None: ...
