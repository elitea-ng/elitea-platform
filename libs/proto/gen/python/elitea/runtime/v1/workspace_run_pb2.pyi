from elitea.runtime.v1 import sandbox_pb2 as _sandbox_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class WorkspaceRunOperationV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WORKSPACE_RUN_OPERATION_V1_UNSPECIFIED: _ClassVar[WorkspaceRunOperationV1]
    WORKSPACE_RUN_OPERATION_V1_UNPACK: _ClassVar[WorkspaceRunOperationV1]
    WORKSPACE_RUN_OPERATION_V1_SERVE: _ClassVar[WorkspaceRunOperationV1]
    WORKSPACE_RUN_OPERATION_V1_PACK: _ClassVar[WorkspaceRunOperationV1]
WORKSPACE_RUN_OPERATION_V1_UNSPECIFIED: WorkspaceRunOperationV1
WORKSPACE_RUN_OPERATION_V1_UNPACK: WorkspaceRunOperationV1
WORKSPACE_RUN_OPERATION_V1_SERVE: WorkspaceRunOperationV1
WORKSPACE_RUN_OPERATION_V1_PACK: WorkspaceRunOperationV1

class WorkspaceRunSpecV1(_message.Message):
    __slots__ = ("revision", "name", "workspace_bytes", "egress", "cpu_millis", "memory_bytes", "pids_limit", "wall_seconds", "egress_proxy")
    REVISION_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_BYTES_FIELD_NUMBER: _ClassVar[int]
    EGRESS_FIELD_NUMBER: _ClassVar[int]
    CPU_MILLIS_FIELD_NUMBER: _ClassVar[int]
    MEMORY_BYTES_FIELD_NUMBER: _ClassVar[int]
    PIDS_LIMIT_FIELD_NUMBER: _ClassVar[int]
    WALL_SECONDS_FIELD_NUMBER: _ClassVar[int]
    EGRESS_PROXY_FIELD_NUMBER: _ClassVar[int]
    revision: int
    name: str
    workspace_bytes: int
    egress: bool
    cpu_millis: int
    memory_bytes: int
    pids_limit: int
    wall_seconds: int
    egress_proxy: bool
    def __init__(self, revision: _Optional[int] = ..., name: _Optional[str] = ..., workspace_bytes: _Optional[int] = ..., egress: bool = ..., cpu_millis: _Optional[int] = ..., memory_bytes: _Optional[int] = ..., pids_limit: _Optional[int] = ..., wall_seconds: _Optional[int] = ..., egress_proxy: bool = ...) -> None: ...

class StartWorkspaceRunRequestV1(_message.Message):
    __slots__ = ("grant", "spec")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: WorkspaceRunSpecV1
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[WorkspaceRunSpecV1, _Mapping]] = ...) -> None: ...

class StartWorkspaceRunResponseV1(_message.Message):
    __slots__ = ("created",)
    CREATED_FIELD_NUMBER: _ClassVar[int]
    created: bool
    def __init__(self, created: bool = ...) -> None: ...

class WorkspaceRunLimitsV1(_message.Message):
    __slots__ = ("max_files", "max_file_bytes", "max_total_bytes")
    MAX_FILES_FIELD_NUMBER: _ClassVar[int]
    MAX_FILE_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_TOTAL_BYTES_FIELD_NUMBER: _ClassVar[int]
    max_files: int
    max_file_bytes: int
    max_total_bytes: int
    def __init__(self, max_files: _Optional[int] = ..., max_file_bytes: _Optional[int] = ..., max_total_bytes: _Optional[int] = ...) -> None: ...

class WorkspaceRunExecOpenV1(_message.Message):
    __slots__ = ("grant", "spec", "operation", "limits", "policy_json")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    OPERATION_FIELD_NUMBER: _ClassVar[int]
    LIMITS_FIELD_NUMBER: _ClassVar[int]
    POLICY_JSON_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: WorkspaceRunSpecV1
    operation: WorkspaceRunOperationV1
    limits: WorkspaceRunLimitsV1
    policy_json: str
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[WorkspaceRunSpecV1, _Mapping]] = ..., operation: _Optional[_Union[WorkspaceRunOperationV1, str]] = ..., limits: _Optional[_Union[WorkspaceRunLimitsV1, _Mapping]] = ..., policy_json: _Optional[str] = ...) -> None: ...

class WorkspaceRunExecInputV1(_message.Message):
    __slots__ = ("open", "stdin", "stdin_end")
    OPEN_FIELD_NUMBER: _ClassVar[int]
    STDIN_FIELD_NUMBER: _ClassVar[int]
    STDIN_END_FIELD_NUMBER: _ClassVar[int]
    open: WorkspaceRunExecOpenV1
    stdin: bytes
    stdin_end: bool
    def __init__(self, open: _Optional[_Union[WorkspaceRunExecOpenV1, _Mapping]] = ..., stdin: _Optional[bytes] = ..., stdin_end: bool = ...) -> None: ...

class WorkspaceRunExecExitV1(_message.Message):
    __slots__ = ("exit_code", "failure_code")
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    FAILURE_CODE_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    failure_code: str
    def __init__(self, exit_code: _Optional[int] = ..., failure_code: _Optional[str] = ...) -> None: ...

class WorkspaceRunExecOutputV1(_message.Message):
    __slots__ = ("stdout", "exit")
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    EXIT_FIELD_NUMBER: _ClassVar[int]
    stdout: bytes
    exit: WorkspaceRunExecExitV1
    def __init__(self, stdout: _Optional[bytes] = ..., exit: _Optional[_Union[WorkspaceRunExecExitV1, _Mapping]] = ...) -> None: ...

class DestroyWorkspaceRunRequestV1(_message.Message):
    __slots__ = ("grant", "spec")
    GRANT_FIELD_NUMBER: _ClassVar[int]
    SPEC_FIELD_NUMBER: _ClassVar[int]
    grant: _sandbox_pb2.SignedSandboxJobGrantV1
    spec: WorkspaceRunSpecV1
    def __init__(self, grant: _Optional[_Union[_sandbox_pb2.SignedSandboxJobGrantV1, _Mapping]] = ..., spec: _Optional[_Union[WorkspaceRunSpecV1, _Mapping]] = ...) -> None: ...

class DestroyWorkspaceRunResponseV1(_message.Message):
    __slots__ = ("destroyed",)
    DESTROYED_FIELD_NUMBER: _ClassVar[int]
    destroyed: bool
    def __init__(self, destroyed: bool = ...) -> None: ...
