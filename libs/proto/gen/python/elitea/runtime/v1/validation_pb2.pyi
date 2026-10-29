from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ConfigurationValidationCommandV1(_message.Message):
    __slots__ = ("configuration_revision_id", "configuration_type", "catalog_revision", "catalog_digest", "schema_id", "schema_revision", "schema_digest", "settings_entry_id")
    CONFIGURATION_REVISION_ID_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_TYPE_FIELD_NUMBER: _ClassVar[int]
    CATALOG_REVISION_FIELD_NUMBER: _ClassVar[int]
    CATALOG_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_ID_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    configuration_revision_id: str
    configuration_type: str
    catalog_revision: str
    catalog_digest: _common_pb2.DigestV1
    schema_id: str
    schema_revision: str
    schema_digest: _common_pb2.DigestV1
    settings_entry_id: str
    def __init__(self, configuration_revision_id: _Optional[str] = ..., configuration_type: _Optional[str] = ..., catalog_revision: _Optional[str] = ..., catalog_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., schema_id: _Optional[str] = ..., schema_revision: _Optional[str] = ..., schema_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., settings_entry_id: _Optional[str] = ...) -> None: ...

class ConfigurationValidationIssueV1(_message.Message):
    __slots__ = ("code", "json_pointer", "safe_message")
    CODE_FIELD_NUMBER: _ClassVar[int]
    JSON_POINTER_FIELD_NUMBER: _ClassVar[int]
    SAFE_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    code: str
    json_pointer: str
    safe_message: str
    def __init__(self, code: _Optional[str] = ..., json_pointer: _Optional[str] = ..., safe_message: _Optional[str] = ...) -> None: ...

class ConfigurationValidationResultV1(_message.Message):
    __slots__ = ("configuration_revision_id", "configuration_type", "catalog_revision", "catalog_digest", "schema_id", "schema_revision", "schema_digest", "input_bundle_id", "input_bundle_digest", "settings_entry_id", "settings_entry_version", "settings_content_digest", "valid", "issues")
    CONFIGURATION_REVISION_ID_FIELD_NUMBER: _ClassVar[int]
    CONFIGURATION_TYPE_FIELD_NUMBER: _ClassVar[int]
    CATALOG_REVISION_FIELD_NUMBER: _ClassVar[int]
    CATALOG_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_ID_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_DIGEST_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_ENTRY_VERSION_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    VALID_FIELD_NUMBER: _ClassVar[int]
    ISSUES_FIELD_NUMBER: _ClassVar[int]
    configuration_revision_id: str
    configuration_type: str
    catalog_revision: str
    catalog_digest: _common_pb2.DigestV1
    schema_id: str
    schema_revision: str
    schema_digest: _common_pb2.DigestV1
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    settings_entry_id: str
    settings_entry_version: str
    settings_content_digest: _common_pb2.DigestV1
    valid: bool
    issues: _containers.RepeatedCompositeFieldContainer[ConfigurationValidationIssueV1]
    def __init__(self, configuration_revision_id: _Optional[str] = ..., configuration_type: _Optional[str] = ..., catalog_revision: _Optional[str] = ..., catalog_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., schema_id: _Optional[str] = ..., schema_revision: _Optional[str] = ..., schema_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., settings_entry_id: _Optional[str] = ..., settings_entry_version: _Optional[str] = ..., settings_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., valid: _Optional[bool] = ..., issues: _Optional[_Iterable[_Union[ConfigurationValidationIssueV1, _Mapping]]] = ...) -> None: ...
