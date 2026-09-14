from elitea.runtime.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AgentExecutionTerminalStateV1(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    AGENT_EXECUTION_TERMINAL_STATE_V1_UNSPECIFIED: _ClassVar[AgentExecutionTerminalStateV1]
    AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED: _ClassVar[AgentExecutionTerminalStateV1]
    AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_HITL: _ClassVar[AgentExecutionTerminalStateV1]
    AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH: _ClassVar[AgentExecutionTerminalStateV1]
    AGENT_EXECUTION_TERMINAL_STATE_V1_PARKED_CHILDREN: _ClassVar[AgentExecutionTerminalStateV1]
AGENT_EXECUTION_TERMINAL_STATE_V1_UNSPECIFIED: AgentExecutionTerminalStateV1
AGENT_EXECUTION_TERMINAL_STATE_V1_COMPLETED: AgentExecutionTerminalStateV1
AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_HITL: AgentExecutionTerminalStateV1
AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_MCP_AUTH: AgentExecutionTerminalStateV1
AGENT_EXECUTION_TERMINAL_STATE_V1_PARKED_CHILDREN: AgentExecutionTerminalStateV1

class AgentExecutionCommandV1(_message.Message):
    __slots__ = ("request_entry_id", "client_stream_id", "client_message_id", "sio_event")
    REQUEST_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_STREAM_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_MESSAGE_ID_FIELD_NUMBER: _ClassVar[int]
    SIO_EVENT_FIELD_NUMBER: _ClassVar[int]
    request_entry_id: str
    client_stream_id: str
    client_message_id: str
    sio_event: str
    def __init__(self, request_entry_id: _Optional[str] = ..., client_stream_id: _Optional[str] = ..., client_message_id: _Optional[str] = ..., sio_event: _Optional[str] = ...) -> None: ...

class AgentExecutionInputV1(_message.Message):
    __slots__ = ("schema_revision", "llm", "chat_history", "user_input", "thread_id", "checkpoint_id", "debug", "tools", "application", "internal_tools", "steps_limit", "mcp_tokens", "ignored_mcp_servers", "user_declined_mcp_servers", "should_continue", "hitl_resume", "hitl_action", "hitl_value", "hitl_decisions", "execution_generation", "is_regenerate", "meta", "conversation_id", "persona", "context_settings", "supports_vision", "return_chat_history", "invoked_skills", "applied_skills", "auto_approve_sensitive_actions", "attached_skills", "input_attachments", "parallel_reconcile", "parallel_terminal_errors", "exception_handling_enabled", "debug_mode", "next_input_suggestion", "toolkit_guardrails", "truncated_content", "project_context", "model_context_limits")
    SCHEMA_REVISION_FIELD_NUMBER: _ClassVar[int]
    LLM_FIELD_NUMBER: _ClassVar[int]
    CHAT_HISTORY_FIELD_NUMBER: _ClassVar[int]
    USER_INPUT_FIELD_NUMBER: _ClassVar[int]
    THREAD_ID_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    DEBUG_FIELD_NUMBER: _ClassVar[int]
    TOOLS_FIELD_NUMBER: _ClassVar[int]
    APPLICATION_FIELD_NUMBER: _ClassVar[int]
    INTERNAL_TOOLS_FIELD_NUMBER: _ClassVar[int]
    STEPS_LIMIT_FIELD_NUMBER: _ClassVar[int]
    MCP_TOKENS_FIELD_NUMBER: _ClassVar[int]
    IGNORED_MCP_SERVERS_FIELD_NUMBER: _ClassVar[int]
    USER_DECLINED_MCP_SERVERS_FIELD_NUMBER: _ClassVar[int]
    SHOULD_CONTINUE_FIELD_NUMBER: _ClassVar[int]
    HITL_RESUME_FIELD_NUMBER: _ClassVar[int]
    HITL_ACTION_FIELD_NUMBER: _ClassVar[int]
    HITL_VALUE_FIELD_NUMBER: _ClassVar[int]
    HITL_DECISIONS_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_GENERATION_FIELD_NUMBER: _ClassVar[int]
    IS_REGENERATE_FIELD_NUMBER: _ClassVar[int]
    META_FIELD_NUMBER: _ClassVar[int]
    CONVERSATION_ID_FIELD_NUMBER: _ClassVar[int]
    PERSONA_FIELD_NUMBER: _ClassVar[int]
    CONTEXT_SETTINGS_FIELD_NUMBER: _ClassVar[int]
    SUPPORTS_VISION_FIELD_NUMBER: _ClassVar[int]
    RETURN_CHAT_HISTORY_FIELD_NUMBER: _ClassVar[int]
    INVOKED_SKILLS_FIELD_NUMBER: _ClassVar[int]
    APPLIED_SKILLS_FIELD_NUMBER: _ClassVar[int]
    AUTO_APPROVE_SENSITIVE_ACTIONS_FIELD_NUMBER: _ClassVar[int]
    ATTACHED_SKILLS_FIELD_NUMBER: _ClassVar[int]
    INPUT_ATTACHMENTS_FIELD_NUMBER: _ClassVar[int]
    PARALLEL_RECONCILE_FIELD_NUMBER: _ClassVar[int]
    PARALLEL_TERMINAL_ERRORS_FIELD_NUMBER: _ClassVar[int]
    EXCEPTION_HANDLING_ENABLED_FIELD_NUMBER: _ClassVar[int]
    DEBUG_MODE_FIELD_NUMBER: _ClassVar[int]
    NEXT_INPUT_SUGGESTION_FIELD_NUMBER: _ClassVar[int]
    TOOLKIT_GUARDRAILS_FIELD_NUMBER: _ClassVar[int]
    TRUNCATED_CONTENT_FIELD_NUMBER: _ClassVar[int]
    PROJECT_CONTEXT_FIELD_NUMBER: _ClassVar[int]
    MODEL_CONTEXT_LIMITS_FIELD_NUMBER: _ClassVar[int]
    schema_revision: str
    llm: bytes
    chat_history: bytes
    user_input: bytes
    thread_id: str
    checkpoint_id: str
    debug: bool
    tools: bytes
    application: bytes
    internal_tools: bytes
    steps_limit: int
    mcp_tokens: bytes
    ignored_mcp_servers: bytes
    user_declined_mcp_servers: bytes
    should_continue: bool
    hitl_resume: bool
    hitl_action: str
    hitl_value: str
    hitl_decisions: bytes
    execution_generation: str
    is_regenerate: bool
    meta: bytes
    conversation_id: str
    persona: str
    context_settings: bytes
    supports_vision: bool
    return_chat_history: bool
    invoked_skills: bytes
    applied_skills: bytes
    auto_approve_sensitive_actions: bool
    attached_skills: bytes
    input_attachments: bytes
    parallel_reconcile: bytes
    parallel_terminal_errors: bytes
    exception_handling_enabled: bool
    debug_mode: bool
    next_input_suggestion: bytes
    toolkit_guardrails: bytes
    truncated_content: bytes
    project_context: ProjectContextSnapshotV1
    model_context_limits: ModelContextLimitsV1
    def __init__(self, schema_revision: _Optional[str] = ..., llm: _Optional[bytes] = ..., chat_history: _Optional[bytes] = ..., user_input: _Optional[bytes] = ..., thread_id: _Optional[str] = ..., checkpoint_id: _Optional[str] = ..., debug: bool = ..., tools: _Optional[bytes] = ..., application: _Optional[bytes] = ..., internal_tools: _Optional[bytes] = ..., steps_limit: _Optional[int] = ..., mcp_tokens: _Optional[bytes] = ..., ignored_mcp_servers: _Optional[bytes] = ..., user_declined_mcp_servers: _Optional[bytes] = ..., should_continue: bool = ..., hitl_resume: bool = ..., hitl_action: _Optional[str] = ..., hitl_value: _Optional[str] = ..., hitl_decisions: _Optional[bytes] = ..., execution_generation: _Optional[str] = ..., is_regenerate: bool = ..., meta: _Optional[bytes] = ..., conversation_id: _Optional[str] = ..., persona: _Optional[str] = ..., context_settings: _Optional[bytes] = ..., supports_vision: bool = ..., return_chat_history: bool = ..., invoked_skills: _Optional[bytes] = ..., applied_skills: _Optional[bytes] = ..., auto_approve_sensitive_actions: bool = ..., attached_skills: _Optional[bytes] = ..., input_attachments: _Optional[bytes] = ..., parallel_reconcile: _Optional[bytes] = ..., parallel_terminal_errors: _Optional[bytes] = ..., exception_handling_enabled: bool = ..., debug_mode: bool = ..., next_input_suggestion: _Optional[bytes] = ..., toolkit_guardrails: _Optional[bytes] = ..., truncated_content: _Optional[bytes] = ..., project_context: _Optional[_Union[ProjectContextSnapshotV1, _Mapping]] = ..., model_context_limits: _Optional[_Union[ModelContextLimitsV1, _Mapping]] = ...) -> None: ...

class ModelContextLimitsV1(_message.Message):
    __slots__ = ("context_window_tokens", "max_output_tokens", "context_window_fallback", "max_output_fallback", "max_input_tokens")
    CONTEXT_WINDOW_TOKENS_FIELD_NUMBER: _ClassVar[int]
    MAX_OUTPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    CONTEXT_WINDOW_FALLBACK_FIELD_NUMBER: _ClassVar[int]
    MAX_OUTPUT_FALLBACK_FIELD_NUMBER: _ClassVar[int]
    MAX_INPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    context_window_tokens: int
    max_output_tokens: int
    context_window_fallback: bool
    max_output_fallback: bool
    max_input_tokens: int
    def __init__(self, context_window_tokens: _Optional[int] = ..., max_output_tokens: _Optional[int] = ..., context_window_fallback: bool = ..., max_output_fallback: bool = ..., max_input_tokens: _Optional[int] = ...) -> None: ...

class ProjectContextSnapshotV1(_message.Message):
    __slots__ = ("id", "revision", "scope", "content", "activation_description")
    ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    ACTIVATION_DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    id: str
    revision: str
    scope: str
    content: str
    activation_description: str
    def __init__(self, id: _Optional[str] = ..., revision: _Optional[str] = ..., scope: _Optional[str] = ..., content: _Optional[str] = ..., activation_description: _Optional[str] = ...) -> None: ...

class AgentExecutionArtifactReferenceV1(_message.Message):
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

class AgentExecutionAttachmentContentV1(_message.Message):
    __slots__ = ("item_id", "content")
    ITEM_ID_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    item_id: str
    content: bytes
    def __init__(self, item_id: _Optional[str] = ..., content: _Optional[bytes] = ...) -> None: ...

class AgentExecutionResultV1(_message.Message):
    __slots__ = ("input_bundle_id", "input_bundle_digest", "request_entry_id", "request_immutable_version", "request_content_digest", "terminal_state", "result_artifact", "attachment_contents")
    INPUT_BUNDLE_ID_FIELD_NUMBER: _ClassVar[int]
    INPUT_BUNDLE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ENTRY_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_IMMUTABLE_VERSION_FIELD_NUMBER: _ClassVar[int]
    REQUEST_CONTENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_STATE_FIELD_NUMBER: _ClassVar[int]
    RESULT_ARTIFACT_FIELD_NUMBER: _ClassVar[int]
    ATTACHMENT_CONTENTS_FIELD_NUMBER: _ClassVar[int]
    input_bundle_id: str
    input_bundle_digest: _common_pb2.DigestV1
    request_entry_id: str
    request_immutable_version: str
    request_content_digest: _common_pb2.DigestV1
    terminal_state: AgentExecutionTerminalStateV1
    result_artifact: AgentExecutionArtifactReferenceV1
    attachment_contents: _containers.RepeatedCompositeFieldContainer[AgentExecutionAttachmentContentV1]
    def __init__(self, input_bundle_id: _Optional[str] = ..., input_bundle_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., request_entry_id: _Optional[str] = ..., request_immutable_version: _Optional[str] = ..., request_content_digest: _Optional[_Union[_common_pb2.DigestV1, _Mapping]] = ..., terminal_state: _Optional[_Union[AgentExecutionTerminalStateV1, str]] = ..., result_artifact: _Optional[_Union[AgentExecutionArtifactReferenceV1, _Mapping]] = ..., attachment_contents: _Optional[_Iterable[_Union[AgentExecutionAttachmentContentV1, _Mapping]]] = ...) -> None: ...
