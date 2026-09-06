from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

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

class ToolkitExecuteReadCommandV1(_message.Message):
    __slots__ = ("request_entry_id",)
    REQUEST_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    request_entry_id: str
    def __init__(self, request_entry_id: _Optional[str] = ...) -> None: ...

class ToolkitExecuteReadInputV1(_message.Message):
    __slots__ = ("schema_revision", "toolkit", "toolkit_type", "toolkit_name", "tool_name", "arguments", "toolkit_guardrails")
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_NAME_FIELD_NUMBER: _ClassVar[int]
    TOOL_NAME_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_GUARDRAILS_FIELD_NUMBER: _ClassVar[int]
    schema_revision: str
    toolkit: bytes
    toolkit_type: str
    toolkit_name: str
    tool_name: str
    arguments: bytes
    toolkit_guardrails: bytes
    def __init__(self, schema_revision: _Optional[str] = ..., toolkit: _Optional[bytes] = ..., toolkit_type: _Optional[str] = ..., toolkit_name: _Optional[str] = ..., tool_name: _Optional[str] = ..., arguments: _Optional[bytes] = ..., toolkit_guardrails: _Optional[bytes] = ...) -> None: ...

class ToolkitExecuteReadResultV1(_message.Message):
    __slots__ = ("input_bundle_id", "input_bundle_digest", "request_entry_id", "request_entry_version", "request_content_digest", "result_json", "toolkit_type", "toolkit_name", "tool_name")
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ENTRY_VERSION_FIELD_NUMBER: _ClassVar[int]
    REQUEST_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RESULT_JSON_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_TYPE_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_NAME_FIELD_NUMBER: _ClassVar[int]
    TOOL_NAME_FIELD_NUMBER: _ClassVar[int]
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    request_entry_id: str
    request_entry_version: str
    request_content_digest: _common_pb2.DigestV1
    result_json: bytes
    toolkit_type: str
    toolkit_name: str
    tool_name: str
    def __init__(self, input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., request_entry_id: _Optional[str] = ..., request_entry_version: _Optional[str] = ..., request_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., result_json: _Optional[bytes] = ..., toolkit_type: _Optional[str] = ..., toolkit_name: _Optional[str] = ..., tool_name: _Optional[str] = ...) -> None: ...
