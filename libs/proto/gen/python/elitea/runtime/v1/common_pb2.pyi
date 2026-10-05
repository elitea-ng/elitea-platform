from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DigestAlgorithmV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DIGEST_ALGORITHM_V1_UNSPECIFIED: _ClassVar[DigestAlgorithmV1]
    DIGEST_ALGORITHM_V1_SHA256: _ClassVar[DigestAlgorithmV1]

class DesiredExecutionStateV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DESIRED_EXECUTION_STATE_V1_UNSPECIFIED: _ClassVar[DesiredExecutionStateV1]
    DESIRED_EXECUTION_STATE_V1_RUNNING: _ClassVar[DesiredExecutionStateV1]
    DESIRED_EXECUTION_STATE_V1_CANCELLED: _ClassVar[DesiredExecutionStateV1]
    DESIRED_EXECUTION_STATE_V1_DRAINING: _ClassVar[DesiredExecutionStateV1]
    DESIRED_EXECUTION_STATE_V1_SUSPENDED: _ClassVar[DesiredExecutionStateV1]

class ExecutionOutcomeV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    EXECUTION_OUTCOME_V1_UNSPECIFIED: _ClassVar[ExecutionOutcomeV1]
    EXECUTION_OUTCOME_V1_SUCCEEDED: _ClassVar[ExecutionOutcomeV1]
    EXECUTION_OUTCOME_V1_FAILED: _ClassVar[ExecutionOutcomeV1]
    EXECUTION_OUTCOME_V1_CANCELLED: _ClassVar[ExecutionOutcomeV1]
    EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN: _ClassVar[ExecutionOutcomeV1]
DIGEST_ALGORITHM_V1_UNSPECIFIED: DigestAlgorithmV1
DIGEST_ALGORITHM_V1_SHA256: DigestAlgorithmV1
DESIRED_EXECUTION_STATE_V1_UNSPECIFIED: DesiredExecutionStateV1
DESIRED_EXECUTION_STATE_V1_RUNNING: DesiredExecutionStateV1
DESIRED_EXECUTION_STATE_V1_CANCELLED: DesiredExecutionStateV1
DESIRED_EXECUTION_STATE_V1_DRAINING: DesiredExecutionStateV1
DESIRED_EXECUTION_STATE_V1_SUSPENDED: DesiredExecutionStateV1
EXECUTION_OUTCOME_V1_UNSPECIFIED: ExecutionOutcomeV1
EXECUTION_OUTCOME_V1_SUCCEEDED: ExecutionOutcomeV1
EXECUTION_OUTCOME_V1_FAILED: ExecutionOutcomeV1
EXECUTION_OUTCOME_V1_CANCELLED: ExecutionOutcomeV1
EXECUTION_OUTCOME_V1_OUTCOME_UNKNOWN: ExecutionOutcomeV1

class DigestV1(_message.Message):
    __slots__ = ("algorithm", "value")
    ALGORITHM_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    algorithm: DigestAlgorithmV1
    value: bytes
    def __init__(self, algorithm: _Optional[_Union[DigestAlgorithmV1, str]] = ..., value: _Optional[bytes] = ...) -> None: ...

class ExecutionIdentityV1(_message.Message):
    __slots__ = ("tenant_id", "resource_project_id", "projection_project_id", "command_id", "execution_id", "generation")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECTION_PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    COMMAND_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    resource_project_id: str
    projection_project_id: str
    command_id: str
    execution_id: str
    generation: int
    def __init__(self, tenant_id: _Optional[str] = ..., resource_project_id: _Optional[str] = ..., projection_project_id: _Optional[str] = ..., command_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., generation: _Optional[int] = ...) -> None: ...

class ExecutionFenceV1(_message.Message):
    __slots__ = ("workload_session_id", "claim_attempt", "lease_epoch", "producer_id", "fence_token")
    WORKLOAD_SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    CLAIM_ATTEMPT_FIELD_NUMBER: _ClassVar[int]
    LEASE_EPOCH_FIELD_NUMBER: _ClassVar[int]
    PRODUCER_ID_FIELD_NUMBER: _ClassVar[int]
    FENCE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    workload_session_id: str
    claim_attempt: int
    lease_epoch: int
    producer_id: str
    fence_token: bytes
    def __init__(self, workload_session_id: _Optional[str] = ..., claim_attempt: _Optional[int] = ..., lease_epoch: _Optional[int] = ..., producer_id: _Optional[str] = ..., fence_token: _Optional[bytes] = ...) -> None: ...
