from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CredentialEnvelopeKindV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CREDENTIAL_ENVELOPE_KIND_V1_UNSPECIFIED: _ClassVar[CredentialEnvelopeKindV1]
    CREDENTIAL_ENVELOPE_KIND_V1_STATIC: _ClassVar[CredentialEnvelopeKindV1]
    CREDENTIAL_ENVELOPE_KIND_V1_OAUTH_ACCESS: _ClassVar[CredentialEnvelopeKindV1]
    CREDENTIAL_ENVELOPE_KIND_V1_DYNAMIC_LEASE: _ClassVar[CredentialEnvelopeKindV1]
CREDENTIAL_ENVELOPE_KIND_V1_UNSPECIFIED: CredentialEnvelopeKindV1
CREDENTIAL_ENVELOPE_KIND_V1_STATIC: CredentialEnvelopeKindV1
CREDENTIAL_ENVELOPE_KIND_V1_OAUTH_ACCESS: CredentialEnvelopeKindV1
CREDENTIAL_ENVELOPE_KIND_V1_DYNAMIC_LEASE: CredentialEnvelopeKindV1

class BoundToolInvocationV1(_message.Message):
    __slots__ = ("invocation_id", "tool_call_id", "toolkit_id", "operation_id", "resource_ref", "arguments_digest", "credential_reference_id")
    INVOCATION_ID_FIELD_NUMBER: _ClassVar[int]
    TOOL_CALL_ID_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_REF_FIELD_NUMBER: _ClassVar[int]
    ARGUMENTS_DIGEST_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_REFERENCE_ID_FIELD_NUMBER: _ClassVar[int]
    invocation_id: str
    tool_call_id: str
    toolkit_id: int
    operation_id: str
    resource_ref: str
    arguments_digest: _common_pb2.DigestV1
    credential_reference_id: str
    def __init__(self, invocation_id: _Optional[str] = ..., tool_call_id: _Optional[str] = ..., toolkit_id: _Optional[int] = ..., operation_id: _Optional[str] = ..., resource_ref: _Optional[str] = ..., arguments_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., credential_reference_id: _Optional[str] = ...) -> None: ...

class CredentialEnvelopeV1(_message.Message):
    __slots__ = ("redemption_id", "kind", "material", "not_after_unix_millis", "target_identity", "obligations", "decision_id")
    class MaterialEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    REDEMPTION_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    MATERIAL_FIELD_NUMBER: _ClassVar[int]
    NOT_AFTER_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    TARGET_IDENTITY_FIELD_NUMBER: _ClassVar[int]
    OBLIGATIONS_FIELD_NUMBER: _ClassVar[int]
    DECISION_ID_FIELD_NUMBER: _ClassVar[int]
    redemption_id: str
    kind: CredentialEnvelopeKindV1
    material: _containers.ScalarMap[str, str]
    not_after_unix_millis: int
    target_identity: str
    obligations: _containers.RepeatedScalarFieldContainer[str]
    decision_id: str
    def __init__(self, redemption_id: _Optional[str] = ..., kind: _Optional[_Union[CredentialEnvelopeKindV1, str]] = ..., material: _Optional[_Mapping[str, str]] = ..., not_after_unix_millis: _Optional[int] = ..., target_identity: _Optional[str] = ..., obligations: _Optional[_Iterable[str]] = ..., decision_id: _Optional[str] = ...) -> None: ...

class CredentialRedeemDenialV1(_message.Message):
    __slots__ = ("code", "decision_id", "authorization_reference")
    CODE_FIELD_NUMBER: _ClassVar[int]
    DECISION_ID_FIELD_NUMBER: _ClassVar[int]
    AUTHORIZATION_REFERENCE_FIELD_NUMBER: _ClassVar[int]
    code: str
    decision_id: str
    authorization_reference: str
    def __init__(self, code: _Optional[str] = ..., decision_id: _Optional[str] = ..., authorization_reference: _Optional[str] = ...) -> None: ...
