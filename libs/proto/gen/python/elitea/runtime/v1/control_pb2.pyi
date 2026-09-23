from elitea.runtime.v1 import common_pb2 as _common_pb2
from elitea.runtime.v1 import envelope_pb2 as _envelope_pb2
from elitea.runtime.v1 import errors_pb2 as _errors_pb2
from elitea.runtime.v1 import input_pb2 as _input_pb2
from elitea.runtime.v1 import output_pb2 as _output_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
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
BEGIN_EXECUTION_DISPOSITION_V1_UNSPECIFIED: BeginExecutionDispositionV1
BEGIN_EXECUTION_DISPOSITION_V1_STARTED_NOW: BeginExecutionDispositionV1
BEGIN_EXECUTION_DISPOSITION_V1_ALREADY_STARTED: BeginExecutionDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_UNSPECIFIED: AuthorizeInvocationDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_AUTHORIZED_NOW: AuthorizeInvocationDispositionV1
AUTHORIZE_INVOCATION_DISPOSITION_V1_ALREADY_AUTHORIZED: AuthorizeInvocationDispositionV1

class ClaimCommandRequestV1(_message.Message):
    __slots__ = ("workload_session_id", "producer_id", "signed_command", "agent_model_checkpoint_recovery")
    WORKLOAD_SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    PRODUCER_ID_FIELD_NUMBER: _ClassVar[int]
    SIGNED_COMMAND_FIELD_NUMBER: _ClassVar[int]
    AGENT_MODEL_CHECKPOINT_RECOVERY_FIELD_NUMBER: _ClassVar[int]
    workload_session_id: str
    producer_id: str
    signed_command: _envelope_pb2.SignedWorkerCommandEnvelopeV1
    agent_model_checkpoint_recovery: bool
    def __init__(self, workload_session_id: _Optional[str] = ..., producer_id: _Optional[str] = ..., signed_command: _Optional[_Union[_envelope_pb2.SignedWorkerCommandEnvelopeV1, _Mapping]] = ..., agent_model_checkpoint_recovery: bool = ...) -> None: ...

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
    __slots__ = ("disposition", "identity", "fence", "lease_expires_at_unix_millis", "input_bundle_ref", "input_bundle", "desired_state", "claim_handoff_watermark", "claim_id", "settlement_recovery", "retirement", "claim_started_at_unix_micros")
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
    def __init__(self, disposition: _Optional[_Union[ClaimDispositionV1, str]] = ..., identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ..., lease_expires_at_unix_millis: _Optional[int] = ..., input_bundle_ref: _Optional[_Union[_input_pb2.ExecutionInputBundleReferenceV1, _Mapping]] = ..., input_bundle: _Optional[_Union[_input_pb2.ExecutionInputBundleV1, _Mapping]] = ..., desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., claim_handoff_watermark: _Optional[int] = ..., claim_id: _Optional[str] = ..., settlement_recovery: _Optional[_Union[SettlementRecoveryV1, _Mapping]] = ..., retirement: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ..., claim_started_at_unix_micros: _Optional[int] = ...) -> None: ...

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
    __slots__ = ("lease_expires_at_unix_millis", "desired_state", "rejection")
    LEASE_EXPIRES_AT_UNIX_MILLIS_FIELD_NUMBER: _ClassVar[int]
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    lease_expires_at_unix_millis: int
    desired_state: _common_pb2.DesiredExecutionStateV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, lease_expires_at_unix_millis: _Optional[int] = ..., desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

class ObserveDesiredStateRequestV1(_message.Message):
    __slots__ = ("identity", "fence")
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    FENCE_FIELD_NUMBER: _ClassVar[int]
    identity: _common_pb2.ExecutionIdentityV1
    fence: _common_pb2.ExecutionFenceV1
    def __init__(self, identity: _Optional[_Union[_common_pb2.ExecutionIdentityV1, _Mapping]] = ..., fence: _Optional[_Union[_common_pb2.ExecutionFenceV1, _Mapping]] = ...) -> None: ...

class ObserveDesiredStateResponseV1(_message.Message):
    __slots__ = ("desired_state", "rejection")
    DESIRED_STATE_FIELD_NUMBER: _ClassVar[int]
    REJECTION_FIELD_NUMBER: _ClassVar[int]
    desired_state: _common_pb2.DesiredExecutionStateV1
    rejection: _errors_pb2.RuntimeErrorV1
    def __init__(self, desired_state: _Optional[_Union[_common_pb2.DesiredExecutionStateV1, str]] = ..., rejection: _Optional[_Union[_errors_pb2.RuntimeErrorV1, _Mapping]] = ...) -> None: ...

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
