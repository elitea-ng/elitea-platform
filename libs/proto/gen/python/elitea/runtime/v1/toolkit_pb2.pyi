from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ToolkitCallToolStatusV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TOOLKIT_CALL_TOOL_STATUS_V1_UNSPECIFIED: _ClassVar[ToolkitCallToolStatusV1]
    TOOLKIT_CALL_TOOL_STATUS_V1_OK: _ClassVar[ToolkitCallToolStatusV1]
    TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR: _ClassVar[ToolkitCallToolStatusV1]
    TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT: _ClassVar[ToolkitCallToolStatusV1]
    TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL: _ClassVar[ToolkitCallToolStatusV1]
TOOLKIT_CALL_TOOL_STATUS_V1_UNSPECIFIED: ToolkitCallToolStatusV1
TOOLKIT_CALL_TOOL_STATUS_V1_OK: ToolkitCallToolStatusV1
TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR: ToolkitCallToolStatusV1
TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT: ToolkitCallToolStatusV1
TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL: ToolkitCallToolStatusV1

class ToolkitAvailableToolsCommandV1(_message.Message):
    __slots__ = ("toolkit_type", "settings_entry_id")
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    toolkit_type: str
    settings_entry_id: str
    def __init__(self, toolkit_type: _Optional[str] = ..., settings_entry_id: _Optional[str] = ...) -> None: ...

class ToolkitAvailableToolsArtifactReferenceV1(_message.Message):
    __slots__ = ("artifact_id", "immutable_version", "media_type", "byte_length", "digest", "classification")
    ARTIFACT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    BYTE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    CLASSIFICATION_FIELD_NUMBER: _ClassVar[int]
    artifact_id: str
    immutable_version: str
    media_type: str
    byte_length: int
    digest: _common_pb2.DigestV1
    classification: str
    def __init__(self, artifact_id: _Optional[str] = ..., immutable_version: _Optional[str] = ..., media_type: _Optional[str] = ..., byte_length: _Optional[int] = ..., digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., classification: _Optional[str] = ...) -> None: ...

class ToolkitAvailableToolsResultV1(_message.Message):
    __slots__ = ("toolkit_type", "input_bundle_id", "input_bundle_digest", "settings_entry_id", "settings_entry_version", "settings_content_digest", "result_artifact")
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_VERSION_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RESULT_ARTIFACT_FIELD_NUMBER: _ClassVar[int]
    toolkit_type: str
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    settings_entry_id: str
    settings_entry_version: str
    settings_content_digest: _common_pb2.DigestV1
    result_artifact: ToolkitAvailableToolsArtifactReferenceV1
    def __init__(self, toolkit_type: _Optional[str] = ..., input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., settings_entry_id: _Optional[str] = ..., settings_entry_version: _Optional[str] = ..., settings_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., result_artifact: _Optional[_Union[ToolkitAvailableToolsArtifactReferenceV1, _Mapping]] = ...) -> None: ...

class ToolkitCallToolCommandV1(_message.Message):
    __slots__ = ("toolkit_type", "settings_entry_id", "tool_name", "arguments_entry_id", "toolkit_id", "toolkit_version")
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    TOOL_NAME_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_VERSION_FIELD_NUMBER: _ClassVar[int]
    toolkit_type: str
    settings_entry_id: str
    tool_name: str
    arguments_entry_id: str
    toolkit_id: str
    toolkit_version: str
    def __init__(self, toolkit_type: _Optional[str] = ..., settings_entry_id: _Optional[str] = ..., tool_name: _Optional[str] = ..., arguments_entry_id: _Optional[str] = ..., toolkit_id: _Optional[str] = ..., toolkit_version: _Optional[str] = ...) -> None: ...

class ToolkitCallToolArtifactReferenceV1(_message.Message):
    __slots__ = ("artifact_id", "immutable_version", "media_type", "byte_length", "digest", "classification")
    ARTIFACT_ID_FIELD_NUMBER: _ClassVar[int]
    IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    MEDIA_TYPE_FIELD_NUMBER: _ClassVar[int]
    BYTE_LENGTH_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    CLASSIFICATION_FIELD_NUMBER: _ClassVar[int]
    artifact_id: str
    immutable_version: str
    media_type: str
    byte_length: int
    digest: _common_pb2.DigestV1
    classification: str
    def __init__(self, artifact_id: _Optional[str] = ..., immutable_version: _Optional[str] = ..., media_type: _Optional[str] = ..., byte_length: _Optional[int] = ..., digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., classification: _Optional[str] = ...) -> None: ...

class ToolkitCallToolSummaryV1(_message.Message):
    __slots__ = ("status", "result_json", "truncated", "error_message")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    ERROR_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    status: ToolkitCallToolStatusV1
    result_json: str
    truncated: bool
    error_message: str
    def __init__(self, status: _Optional[_Union[ToolkitCallToolStatusV1, str]] = ..., result_json: _Optional[str] = ..., truncated: bool = ..., error_message: _Optional[str] = ...) -> None: ...

class ToolkitCallToolResultV1(_message.Message):
    __slots__ = ("toolkit_type", "tool_name", "input_bundle_id", "input_bundle_digest", "settings_entry_id", "settings_entry_version", "settings_content_digest", "arguments_entry_id", "arguments_content_digest", "result_artifact", "result_summary")
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    TOOL_NAME_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_VERSION_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RESULT_ARTIFACT_FIELD_NUMBER: _ClassVar[int]
    RESULT_SUMMARY_FIELD_NUMBER: _ClassVar[int]
    toolkit_type: str
    tool_name: str
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    settings_entry_id: str
    settings_entry_version: str
    settings_content_digest: _common_pb2.DigestV1
    arguments_entry_id: str
    arguments_content_digest: _common_pb2.DigestV1
    result_artifact: ToolkitCallToolArtifactReferenceV1
    result_summary: ToolkitCallToolSummaryV1
    def __init__(self, toolkit_type: _Optional[str] = ..., tool_name: _Optional[str] = ..., input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., settings_entry_id: _Optional[str] = ..., settings_entry_version: _Optional[str] = ..., settings_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., arguments_entry_id: _Optional[str] = ..., arguments_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., result_artifact: _Optional[_Union[ToolkitCallToolArtifactReferenceV1, _Mapping]] = ..., result_summary: _Optional[_Union[ToolkitCallToolSummaryV1, _Mapping]] = ...) -> None: ...
