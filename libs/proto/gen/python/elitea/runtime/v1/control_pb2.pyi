from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import compiled_code_pb2 as _compiled_code_pb2
from elitea.runtime.v1 import egress_audit_pb2 as _egress_audit_pb2
from elitea.runtime.v1 import envelope_pb2 as _envelope_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from elitea.runtime.v1 import input_pb2 as _input_pb2
from elitea.runtime.v1 import output_pb2 as _output_pb2
from elitea.runtime.v1 import run_step_pb2 as _run_step_pb2
from elitea.runtime.v1 import sandbox_pb2 as _sandbox_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ClaimDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CLAIM_DISPOSITION_V1_UNSPECIFIED: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_ACCEPTED: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_SETTLED_ACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_OBSOLETE_ACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RETIRED_ACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_AGENT_MODEL_CHECKPOINT: _ClassVar[ClaimDispositionV1]
    CLAIM_DISPOSITION_V1_RECOVER_NODE_VISIT: _ClassVar[ClaimDispositionV1]

class BeginExecutionDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    BEGIN_EXECUTION_DISPOSITION_V1_UNSPECIFIED: _ClassVar[BeginExecutionDispositionV1]
    BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW: _ClassVar[BeginExecutionDispositionV1]
    BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED: _ClassVar[BeginExecutionDispositionV1]

class AuthorizeInvocationDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    AUTHORIZE_INVOCATION_DISPOSITION_V1_UNSPECIFIED: _ClassVar[AuthorizeInvocationDispositionV1]
    AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW: _ClassVar[AuthorizeInvocationDispositionV1]
    AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED: _ClassVar[AuthorizeInvocationDispositionV1]

class RemoveIndexDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    REMOVE_INDEX_DISPOSITION_V1_UNSPECIFIED: _ClassVar[RemoveIndexDispositionV1]
    REMOVE_INDEX_DISPOSITION_V1_REMOVED: _ClassVar[RemoveIndexDispositionV1]
    REMOVE_INDEX_DISPOSITION_V1_NOT_FOUND: _ClassVar[RemoveIndexDispositionV1]
    REMOVE_INDEX_DISPOSITION_V1_FORBIDDEN: _ClassVar[RemoveIndexDispositionV1]
    REMOVE_INDEX_DISPOSITION_V1_ACTIVE_RUN: _ClassVar[RemoveIndexDispositionV1]
    REMOVE_INDEX_DISPOSITION_V1_SHARED_WRITE_REFUSED: _ClassVar[RemoveIndexDispositionV1]

class DelegationAssertionDispositionV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DELEGATION_ASSERTION_DISPOSITION_V1_UNSPECIFIED: _ClassVar[DelegationAssertionDispositionV1]
    DELEGATION_ASSERTION_DISPOSITION_V1_ISSUED: _ClassVar[DelegationAssertionDispositionV1]
    DELEGATION_ASSERTION_DISPOSITION_V1_DISABLED: _ClassVar[DelegationAssertionDispositionV1]
CLAIM_DISPOSITION_V1_UNSPECIFIED: ClaimDispositionV1
CLAIM_DISPOSITION_V1_ACCEPTED: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT: ClaimDispositionV1
CLAIM_DISPOSITION_V1_SETTLED_ACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_OBSOLETE_ACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_ACTIVE_LEASE_NOACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RETRY_LATER_NOACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RETIRED_ACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_RUNNING_NOACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_AMBIGUOUS_INVOCATION_NOACK: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_AGENT_MODEL_CHECKPOINT: ClaimDispositionV1
CLAIM_DISPOSITION_V1_RECOVER_NODE_VISIT: ClaimDispositionV1
BEGIN_EXECUTION_DISPOSITION_V1_UNSPECIFIED: BeginExecutionDispositionV1
BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW: BeginExecutionDispositionV1
BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED: BeginExecutionDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_UNSPECIFIED: AuthorizeInvocationDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW: AuthorizeInvocationDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED: AuthorizeInvocationDispositionV1
REMOVE_INDEX_DISPOSITION_V1_UNSPECIFIED: RemoveIndexDispositionV1
REMOVE_INDEX_DISPOSITION_V1_REMOVED: RemoveIndexDispositionV1
REMOVE_INDEX_DISPOSITION_V1_NOT_FOUND: RemoveIndexDispositionV1
REMOVE_INDEX_DISPOSITION_V1_FORBIDDEN: RemoveIndexDispositionV1
REMOVE_INDEX_DISPOSITION_V1_ACTIVE_RUN: RemoveIndexDispositionV1
REMOVE_INDEX_DISPOSITION_V1_SHARED_WRITE_REFUSED: RemoveIndexDispositionV1
DELEGATION_ASSERTION_DISPOSITION_V1_UNSPECIFIED: DelegationAssertionDispositionV1
DELEGATION_ASSERTION_DISPOSITION_V1_ISSUED: DelegationAssertionDispositionV1
DELEGATION_ASSERTION_DISPOSITION_V1_DISABLED: DelegationAssertionDispositionV1

class ClaimCommandRequestV1(_message.Message):
    __slots__ = ("workload_session_id", "producer_id", "signed_command", "agent_model_checkpoint_recovery", "node_recovery", "governed_security_context_v1", "fanout_interrupt_decisions")
    WORKLOAD_SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    PRODUCER_ID_FIELD_NUMBER: _ClassVar[int]
    SIGNED_COMMAND_FIELD_NUMBER: _ClassVar[int]
    AGENT_MODEL_CHECKPOINT_RECOVERY_FIELD_NUMBER: _ClassVar[int]
    NODE_RECOVERY_FIELD_NUMBER: _ClassVar[int]
    GOVERNED_SECURITY_CONTEXT_V1_FIELD_NUMBER: _ClassVar[int]
    FANOUT_INTERRUPT_DECISIONS_FIELD_NUMBER: _ClassVar[int]
    workload_session_id: str
    producer_id: str
    signed_command: _envelope_pb2.SignedWorkerCommandEnvelopeV1
    agent_model_checkpoint_recovery: bool
    node_recovery: bool
    governed_security_context_v1: bool
    fanout_interrupt_decisions: bool
    def __init__(self, workload_session_id: _Optional[str] = ..., producer_id: _Optional[str] = ..., signed_command: _Optional[_Union[_envelope_pb2.SignedWorkerCommandEnvelopeV1, _Mapping]] = ..., agent_model_checkpoint_recovery: _Optional[bool] = ..., node_recovery: _Optional[bool] = ..., governed_security_context_v1: _Optional[bool] = ..., fanout_interrupt_decisions: _Optional[bool] = ...) -> None: ...

class SettlementRecoveryV1(_message.Message):
    __slots__ = ("proposal", "proposal_digest", "idempotency_key", "settlement_receipt_id", "outcome")
    PROPOSAL_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_DIGEST_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    SETTLEMENT_RECEIPT_ID_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    proposal: _output_pb2.SettlementProposalV1
    proposal_digest: _common_pb2.DigestV1
    idempotency_key: str
    settlement_receipt_id: str
    outcome: _common_pb2.ExecutionOutcomeV1
    def __init__(self, proposal: _Optional[_Union[_output_pb2.SettlementProposalV1, _Mapping]] = ..., proposal_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., idempotency_key: _Optional[str] = ..., settlement_receipt_id: _Optional[str] = ..., outcome: _Optional[_Union[_common_pb2.ExecutionOutcomeV1, str]] = ...) -> None: ...

class ClaimReceiptV1(_message.Message):
    __slots__ = ("disposition", "identity", "fence", "lease_expires_at_unix_millis", "input_bundle_ref", "input_bundle", "desired_state", "claim_handoff_watermark", "claim_id", "settlement_recovery", "retirement", "claim_started_at_unix_micros", "node_recovery_receipt_json", "vector_token", "toolkit_typed_refusals")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    LEASE_EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_REF_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_FIELD_NUMBER: _ClassVar[int]
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_HANDOFF_WATERMARK_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    SETTLEMENT_RECOVERY_FIELD_NUMBER: _ClassVar[int]
    RETIREMENT_FIELD_NUMBER: _ClassVar[int]
    CLAIM_STARTED_AT_UNIX_MICROS_FIELD_NUMBER: _ClassVar[int]
    NODE_RECOVERY_RECEIPT_JSON_FIELD_NUMBER: _ClassVar[int]
    VECTOR_TOKEN_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_TYPED_REFUSALS_FIELD_NUMBER: _ClassVar[int]
    disposition: ClaimDispositionV1
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    lease_expires_at_unix_millis: int
    input_bundle_ref: _input_pb2.ExecutionInputBundleReferenceV1
    input_bundle: _input_pb2.ExecutionInputBundleV1
    desired_state: _common_pb2.DesiredExecutionStateV1
    claim_handoff_watermark: int
    claim_id: str
    settlement_recovery: SettlementRecoveryV1
    retirement: _errors_pb2.RuntimeErrorV1
    claim_started_at_unix_micros: int
    node_recovery_receipt_json: bytes
    vector_token: VectorClaimTokenV1
    toolkit_typed_refusals: bool
    def __init__(self, disposition: _Optional[_Union[ClaimDispositionV1, str]] = ..., identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., lease_expires_at_unix_millis: _Optional[int] = ..., input_bundle_ref: _Optional[_Union[_input_pb2.ExecutionInputBundleReferenceV1, _Mapping]] = ..., input_bundle: _Optional[_Union[_input_pb2.ExecutionInputBundleV1, _Mapping]] = ..., desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., claim_handoff_watermark: _Optional[int] = ..., claim_id: _Optional[str] = ..., settlement_recovery: _Optional[_Union[SettlementRecoveryV1, _Mapping]] = ..., retirement: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., claim_started_at_unix_micros: _Optional[int] = ..., node_recovery_receipt_json: _Optional[bytes] = ..., vector_token: _Optional[_Union[VectorClaimTokenV1, _Mapping]] = ..., toolkit_typed_refusals: _Optional[bool] = ...) -> None: ...

class VectorClaimTokenV1(_message.Message):
    __slots__ = ("bearer", "expires_at_unix_millis", "allowed_sources")
    BEARER_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    ALLOWED_SOURCES_FIELD_NUMBER: _ClassVar[int]
    bearer: str
    expires_at_unix_millis: int
    allowed_sources: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, bearer: _Optional[str] = ..., expires_at_unix_millis: _Optional[int] = ..., allowed_sources: _Optional[_Iterable[str]] = ...) -> None: ...

class ClaimCommandResponseV1(_message.Message):
    __slots__ = ("receipt", "rejection")
    RECEIPT_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    receipt: ClaimReceiptV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, receipt: _Optional[_Union[ClaimReceiptV1, _Mapping]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class BeginExecutionRequestV1(_message.Message):
    __slots__ = ("identity", "fence")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ...) -> None: ...

class BeginExecutionResponseV1(_message.Message):
    __slots__ = ("disposition", "rejection")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    disposition: BeginExecutionDispositionV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, disposition: _Optional[_Union[BeginExecutionDispositionV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class AuthorizeInvocationRequestV1(_message.Message):
    __slots__ = ("identity", "fence")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ...) -> None: ...

class AuthorizeInvocationResponseV1(_message.Message):
    __slots__ = ("disposition", "rejection")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    disposition: AuthorizeInvocationDispositionV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, disposition: _Optional[_Union[AuthorizeInvocationDispositionV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class AuthorizeAgentModelCheckpointRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "checkpoint_digest")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    checkpoint_digest: _common_pb2.DigestV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., checkpoint_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ...) -> None: ...

class AuthorizeAgentModelCheckpointResponseV1(_message.Message):
    __slots__ = ("disposition", "rejection")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    disposition: AuthorizeInvocationDispositionV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, disposition: _Optional[_Union[AuthorizeInvocationDispositionV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class RenewLeaseRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "idempotency_key")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    idempotency_key: str
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class RenewLeaseResponseV1(_message.Message):
    __slots__ = ("lease_expires_at_unix_millis", "desired_state", "rejection", "workspace_wrap_up")
    LEASE_EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_WRAP_UP_FIELD_NUMBER: _ClassVar[int]
    lease_expires_at_unix_millis: int
    desired_state: _common_pb2.DesiredExecutionStateV1
    rejection: _errors_pb2.RuntimeErrorV1
    workspace_wrap_up: bool
    def __init__(self, lease_expires_at_unix_millis: _Optional[int] = ..., desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., workspace_wrap_up: _Optional[bool] = ...) -> None: ...

class ObserveDesiredStateRequestV1(_message.Message):
    __slots__ = ("identity", "fence")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ...) -> None: ...

class ObserveDesiredStateResponseV1(_message.Message):
    __slots__ = ("desired_state", "rejection", "workspace_wrap_up")
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_WRAP_UP_FIELD_NUMBER: _ClassVar[int]
    desired_state: _common_pb2.DesiredExecutionStateV1
    rejection: _errors_pb2.RuntimeErrorV1
    workspace_wrap_up: bool
    def __init__(self, desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., workspace_wrap_up: _Optional[bool] = ...) -> None: ...

class PrepareSettlementRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "proposal", "proposal_digest", "idempotency_key")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_FIELD_NUMBER: _ClassVar[int]
    PROPOSAL_DIGEST_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    proposal: _output_pb2.SettlementProposalV1
    proposal_digest: _common_pb2.DigestV1
    idempotency_key: str
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., proposal: _Optional[_Union[_output_pb2.SettlementProposalV1, _Mapping]] = ..., proposal_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class PrepareSettlementResponseV1(_message.Message):
    __slots__ = ("settlement_receipt_id", "outcome", "rejection")
    SETTLEMENT_RECEIPT_ID_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    settlement_receipt_id: str
    outcome: _common_pb2.ExecutionOutcomeV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, settlement_receipt_id: _Optional[str] = ..., outcome: _Optional[_Union[_common_pb2.ExecutionOutcomeV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class RemoveIndexRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "claim_id", "toolkit_id", "index_name", "index_id")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ID_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_ID_FIELD_NUMBER: _ClassVar[int]
    INDEX_NAME_FIELD_NUMBER: _ClassVar[int]
    INDEX_ID_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    claim_id: str
    toolkit_id: int
    index_name: str
    index_id: str
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., claim_id: _Optional[str] = ..., toolkit_id: _Optional[int] = ..., index_name: _Optional[str] = ..., index_id: _Optional[str] = ...) -> None: ...

class RemoveIndexResponseV1(_message.Message):
    __slots__ = ("disposition", "rejection")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    disposition: RemoveIndexDispositionV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, disposition: _Optional[_Union[RemoveIndexDispositionV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class ClaimSteeringMessagesRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "acknowledged_steering_ids", "acknowledge_only")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    ACKNOWLEDGED_STEERING_IDS_FIELD_NUMBER: _ClassVar[int]
    ACKNOWLEDGE_ONLY_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    acknowledged_steering_ids: _containers.RepeatedScalarFieldContainer[str]
    acknowledge_only: bool
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., acknowledged_steering_ids: _Optional[_Iterable[str]] = ..., acknowledge_only: _Optional[bool] = ...) -> None: ...

class SteeringMessageV1(_message.Message):
    __slots__ = ("steering_id", "content", "queued_at_unix_millis")
    STEERING_ID_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    QUEUED_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    steering_id: str
    content: str
    queued_at_unix_millis: int
    def __init__(self, steering_id: _Optional[str] = ..., content: _Optional[str] = ..., queued_at_unix_millis: _Optional[int] = ...) -> None: ...

class ClaimSteeringMessagesResponseV1(_message.Message):
    __slots__ = ("messages", "rejection")
    MESSAGES_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    messages: _containers.RepeatedCompositeFieldContainer[SteeringMessageV1]
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, messages: _Optional[_Iterable[_Union[SteeringMessageV1, _Mapping]]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class ReportEgressSummaryRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "hosts_reached", "hosts_refused", "other_hosts")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    HOSTS_REACHED_FIELD_NUMBER: _ClassVar[int]
    HOSTS_REFUSED_FIELD_NUMBER: _ClassVar[int]
    OTHER_HOSTS_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    hosts_reached: _containers.RepeatedCompositeFieldContainer[_egress_audit_pb2.EgressHostReachedV1]
    hosts_refused: _containers.RepeatedCompositeFieldContainer[_egress_audit_pb2.EgressHostRefusedV1]
    other_hosts: int
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., hosts_reached: _Optional[_Iterable[_Union[_egress_audit_pb2.EgressHostReachedV1, _Mapping]]] = ..., hosts_refused: _Optional[_Iterable[_Union[_egress_audit_pb2.EgressHostRefusedV1, _Mapping]]] = ..., other_hosts: _Optional[int] = ...) -> None: ...

class ReportEgressSummaryResponseV1(_message.Message):
    __slots__ = ("rejection",)
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class MintDelegationAssertionRequestV1(_message.Message):
    __slots__ = ("identity", "fence", "audience")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    AUDIENCE_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    audience: str
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., audience: _Optional[str] = ...) -> None: ...

class MintDelegationAssertionResponseV1(_message.Message):
    __slots__ = ("disposition", "assertion", "expires_at_unix_millis", "required", "rejection")
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    ASSERTION_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    disposition: DelegationAssertionDispositionV1
    assertion: str
    expires_at_unix_millis: int
    required: bool
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, disposition: _Optional[_Union[DelegationAssertionDispositionV1, str]] = ..., assertion: _Optional[str] = ..., expires_at_unix_millis: _Optional[int] = ..., required: _Optional[bool] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...
