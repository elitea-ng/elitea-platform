from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ExecutionSecurityContextV1(_message.Message):
    __slots__ = ("schema_revision", "profile", "policy_revision_id", "policy_digest", "tenant_project_id", "subject_principal_ref", "agent_binding_id", "agent_binding_epoch", "mandate_id", "mandate_epoch", "delegation_id", "delegation_epoch", "caller", "definition_digest", "actor_chain", "issued_at_unix_millis", "agent_session_ref", "application_id", "application_version_id", "credential_references")
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    POLICY_REVISION_ID_FIELD_NUMBER: _ClassVar[int]
    POLICY_DIGEST_FIELD_NUMBER: _ClassVar[int]
    TENANT_PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_PRINCIPAL_REF_FIELD_NUMBER: _ClassVar[int]
    AGENT_BINDING_ID_FIELD_NUMBER: _ClassVar[int]
    AGENT_BINDING_EPOCH_FIELD_NUMBER: _ClassVar[int]
    MANDATE_ID_FIELD_NUMBER: _ClassVar[int]
    MANDATE_EPOCH_FIELD_NUMBER: _ClassVar[int]
    DELEGATION_ID_FIELD_NUMBER: _ClassVar[int]
    DELEGATION_EPOCH_FIELD_NUMBER: _ClassVar[int]
    CALLER_FIELD_NUMBER: _ClassVar[int]
    DEFINITION_DIGEST_FIELD_NUMBER: _ClassVar[int]
    ACTOR_CHAIN_FIELD_NUMBER: _ClassVar[int]
    ISSUED_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    AGENT_SESSION_REF_FIELD_NUMBER: _ClassVar[int]
    APPLICATION_ID_FIELD_NUMBER: _ClassVar[int]
    APPLICATION_VERSION_ID_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_REFERENCES_FIELD_NUMBER: _ClassVar[int]
    schema_revision: str
    profile: str
    policy_revision_id: str
    policy_digest: _common_pb2.DigestV1
    tenant_project_id: int
    subject_principal_ref: str
    agent_binding_id: str
    agent_binding_epoch: int
    mandate_id: str
    mandate_epoch: int
    delegation_id: str
    delegation_epoch: int
    caller: CallerContextV1
    definition_digest: _common_pb2.DigestV1
    actor_chain: _containers.RepeatedCompositeFieldContainer[ActorLinkV1]
    issued_at_unix_millis: int
    agent_session_ref: str
    application_id: int
    application_version_id: int
    credential_references: _containers.RepeatedCompositeFieldContainer[GovernedCredentialReferenceV1]
    def __init__(self, schema_revision: _Optional[str] = ..., profile: _Optional[str] = ..., policy_revision_id: _Optional[str] = ..., policy_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., tenant_project_id: _Optional[int] = ..., subject_principal_ref: _Optional[str] = ..., agent_binding_id: _Optional[str] = ..., agent_binding_epoch: _Optional[int] = ..., mandate_id: _Optional[str] = ..., mandate_epoch: _Optional[int] = ..., delegation_id: _Optional[str] = ..., delegation_epoch: _Optional[int] = ..., caller: _Optional[_Union[CallerContextV1, _Mapping]] = ..., definition_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., actor_chain: _Optional[_Iterable[_Union[ActorLinkV1, _Mapping]]] = ..., issued_at_unix_millis: _Optional[int] = ..., agent_session_ref: _Optional[str] = ..., application_id: _Optional[int] = ..., application_version_id: _Optional[int] = ..., credential_references: _Optional[_Iterable[_Union[GovernedCredentialReferenceV1, _Mapping]]] = ...) -> None: ...

class GovernedCredentialReferenceV1(_message.Message):
    __slots__ = ("toolkit_id", "credential_reference_id", "target_binding_id", "generation", "toolkit_digest")
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_REFERENCE_ID_FIELD_NUMBER: _ClassVar[int]
    TARGET_BINDING_ID_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    toolkit_id: int
    credential_reference_id: str
    target_binding_id: str
    generation: int
    toolkit_digest: _common_pb2.DigestV1
    def __init__(self, toolkit_id: _Optional[int] = ..., credential_reference_id: _Optional[str] = ..., target_binding_id: _Optional[str] = ..., generation: _Optional[int] = ..., toolkit_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ...) -> None: ...

class CallerContextV1(_message.Message):
    __slots__ = ("authentication_source", "calling_application_id")
    AUTHENTICATION_SOURCE_FIELD_NUMBER: _ClassVar[int]
    CALLING_APPLICATION_ID_FIELD_NUMBER: _ClassVar[int]
    authentication_source: str
    calling_application_id: str
    def __init__(self, authentication_source: _Optional[str] = ..., calling_application_id: _Optional[str] = ...) -> None: ...

class ActorLinkV1(_message.Message):
    __slots__ = ("kind", "ref")
    KIND_FIELD_NUMBER: _ClassVar[int]
    REF_FIELD_NUMBER: _ClassVar[int]
    kind: str
    ref: str
    def __init__(self, kind: _Optional[str] = ..., ref: _Optional[str] = ...) -> None: ...

class ExecutionSecurityContextReferenceV1(_message.Message):
    __slots__ = ("content_entry_id", "digest", "schema_revision")
    CONTENT_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    content_entry_id: str
    digest: _common_pb2.DigestV1
    schema_revision: str
    def __init__(self, content_entry_id: _Optional[str] = ..., digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., schema_revision: _Optional[str] = ...) -> None: ...
