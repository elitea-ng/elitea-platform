from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional

DESCRIPTOR: _descriptor.FileDescriptor

class ProtocolLimitsV1(_message.Message):
    __slots__ = ("limits_revision", "max_worker_command_bytes", "max_signed_envelope_bytes", "max_transport_payload_bytes", "max_transport_message_bytes", "max_input_manifest_bytes", "max_input_entries", "max_input_content_bytes", "max_output_frame_bytes", "max_validation_issues", "max_safe_string_bytes", "claim_lease_ttl_millis", "max_lease_poll_interval_millis", "max_grpc_request_bytes", "max_grpc_response_bytes")
    LIMITS_REVISION_FIELD_NUMBER: _ClassVar[int]
    MAX_WORKER_COMMAND_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_SIGNED_ENVELOPE_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_TRANSPORT_PAYLOAD_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_TRANSPORT_MESSAGE_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_INPUT_MANIFEST_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_INPUT_ENTRIES_FIELD_NUMBER: _ClassVar[int]
    MAX_INPUT_CONTENT_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_OUTPUT_FRAME_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_VALIDATION_ISSUES_FIELD_NUMBER: _ClassVar[int]
    MAX_SAFE_STRING_BYTES_FIELD_NUMBER: _ClassVar[int]
    CLAIM_LEASE_TTL_MILLIS_FIELD_NUMBER: _ClassVar[int]
    MAX_LEASE_POLL_INTERVAL_MILLIS_FIELD_NUMBER: _ClassVar[int]
    MAX_GRPC_REQUEST_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_GRPC_RESPONSE_BYTES_FIELD_NUMBER: _ClassVar[int]
    limits_revision: str
    max_worker_command_bytes: int
    max_signed_envelope_bytes: int
    max_transport_payload_bytes: int
    max_transport_message_bytes: int
    max_input_manifest_bytes: int
    max_input_entries: int
    max_input_content_bytes: int
    max_output_frame_bytes: int
    max_validation_issues: int
    max_safe_string_bytes: int
    claim_lease_ttl_millis: int
    max_lease_poll_interval_millis: int
    max_grpc_request_bytes: int
    max_grpc_response_bytes: int
    def __init__(self, limits_revision: _Optional[str] = ..., max_worker_command_bytes: _Optional[int] = ..., max_signed_envelope_bytes: _Optional[int] = ..., max_transport_payload_bytes: _Optional[int] = ..., max_transport_message_bytes: _Optional[int] = ..., max_input_manifest_bytes: _Optional[int] = ..., max_input_entries: _Optional[int] = ..., max_input_content_bytes: _Optional[int] = ..., max_output_frame_bytes: _Optional[int] = ..., max_validation_issues: _Optional[int] = ..., max_safe_string_bytes: _Optional[int] = ..., claim_lease_ttl_millis: _Optional[int] = ..., max_lease_poll_interval_millis: _Optional[int] = ..., max_grpc_request_bytes: _Optional[int] = ..., max_grpc_response_bytes: _Optional[int] = ...) -> None: ...
