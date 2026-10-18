from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class EgressHostReachedV1(_message.Message):
    __slots__ = ("host", "port", "connections", "bytes_up", "bytes_down")
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    CONNECTIONS_FIELD_NUMBER: _ClassVar[int]
    BYTES_UP_FIELD_NUMBER: _ClassVar[int]
    BYTES_DOWN_FIELD_NUMBER: _ClassVar[int]
    host: str
    port: int
    connections: int
    bytes_up: int
    bytes_down: int
    def __init__(self, host: _Optional[str] = ..., port: _Optional[int] = ..., connections: _Optional[int] = ..., bytes_up: _Optional[int] = ..., bytes_down: _Optional[int] = ...) -> None: ...

class EgressHostRefusedV1(_message.Message):
    __slots__ = ("host", "port", "reason", "count")
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    host: str
    port: int
    reason: str
    count: int
    def __init__(self, host: _Optional[str] = ..., port: _Optional[int] = ..., reason: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...

class ReportRunSummaryRequestV1(_message.Message):
    __slots__ = ("replica_id", "execution_id", "project_id", "final", "replica_shutdown", "token_expires_at_unix", "connections", "bytes_up", "bytes_down", "hosts_reached", "hosts_refused", "other_hosts")
    REPLICA_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    FINAL_FIELD_NUMBER: _ClassVar[int]
    REPLICA_SHUTDOWN_FIELD_NUMBER: _ClassVar[int]
    TOKEN_EXPIRES_AT_UNIX_FIELD_NUMBER: _ClassVar[int]
    CONNECTIONS_FIELD_NUMBER: _ClassVar[int]
    BYTES_UP_FIELD_NUMBER: _ClassVar[int]
    BYTES_DOWN_FIELD_NUMBER: _ClassVar[int]
    HOSTS_REACHED_FIELD_NUMBER: _ClassVar[int]
    HOSTS_REFUSED_FIELD_NUMBER: _ClassVar[int]
    OTHER_HOSTS_FIELD_NUMBER: _ClassVar[int]
    replica_id: str
    execution_id: str
    project_id: int
    final: bool
    replica_shutdown: bool
    token_expires_at_unix: int
    connections: int
    bytes_up: int
    bytes_down: int
    hosts_reached: _containers.RepeatedCompositeFieldContainer[EgressHostReachedV1]
    hosts_refused: _containers.RepeatedCompositeFieldContainer[EgressHostRefusedV1]
    other_hosts: int
    def __init__(self, replica_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., project_id: _Optional[int] = ..., final: bool = ..., replica_shutdown: bool = ..., token_expires_at_unix: _Optional[int] = ..., connections: _Optional[int] = ..., bytes_up: _Optional[int] = ..., bytes_down: _Optional[int] = ..., hosts_reached: _Optional[_Iterable[_Union[EgressHostReachedV1, _Mapping]]] = ..., hosts_refused: _Optional[_Iterable[_Union[EgressHostRefusedV1, _Mapping]]] = ..., other_hosts: _Optional[int] = ...) -> None: ...

class ReportRunSummaryResponseV1(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
