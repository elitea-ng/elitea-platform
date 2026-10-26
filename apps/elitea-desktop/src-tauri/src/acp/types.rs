// Types written from the Agent Client Protocol schema
// (https://github.com/agentclientprotocol/agent-client-protocol), schema/v1 @
// e2d83349bc9e28a4059ea3acefa1c91c966f74e2. Apache License 2.0.
//
//! The ACP v1 messages this agent reads and writes, as plain serde types.
//!
//! The official `agent-client-protocol` crates are not linked: they turn on
//! serde_json `preserve_order` (ADR-0029 decision 2). Only what the agent
//! side uses is here: the client-side `fs/*` and `terminal/*` methods are
//! never called (files and commands stay under our workspace and sandbox).
//!
//! Inbound messages come from another process and are untrusted: required
//! fields are required, enums are closed, and the only open values are the
//! schema's own (`rawInput`, `rawOutput`). Unknown fields (`_meta`, fields a
//! newer client adds) are ignored, as the schema allows.

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// The one protocol version this agent speaks.
pub const PROTOCOL_VERSION: u16 = 1;

/// The schema commit these types were written from (`fixtures/`).
#[cfg(test)]
pub const SCHEMA_COMMIT: &str = "e2d83349bc9e28a4059ea3acefa1c91c966f74e2";

// ---- initialize ----------------------------------------------------------

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct InitializeRequest {
    pub protocol_version: u16,
    #[serde(default)]
    pub client_capabilities: ClientCapabilities,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_info: Option<Implementation>,
}

/// What the client can serve. Read, never relied on: v1 uses neither its
/// files nor its terminals.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ClientCapabilities {
    #[serde(default)]
    pub fs: FileSystemCapabilities,
    #[serde(default)]
    pub terminal: bool,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct FileSystemCapabilities {
    #[serde(default)]
    pub read_text_file: bool,
    #[serde(default)]
    pub write_text_file: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Implementation {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    pub version: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct InitializeResponse {
    pub protocol_version: u16,
    pub agent_capabilities: AgentCapabilities,
    pub auth_methods: Vec<AuthMethod>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent_info: Option<Implementation>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct AgentCapabilities {
    #[serde(default)]
    pub load_session: bool,
    #[serde(default)]
    pub prompt_capabilities: PromptCapabilities,
    #[serde(default)]
    pub mcp_capabilities: McpCapabilities,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct PromptCapabilities {
    #[serde(default)]
    pub image: bool,
    #[serde(default)]
    pub audio: bool,
    #[serde(default)]
    pub embedded_context: bool,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct McpCapabilities {
    #[serde(default)]
    pub http: bool,
    #[serde(default)]
    pub sse: bool,
}

/// An agent-run sign-in method. This agent offers none: it uses the
/// desktop app's stored sign-in.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct AuthMethod {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct AuthenticateRequest {
    pub method_id: String,
}

// ---- sessions ------------------------------------------------------------

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct NewSessionRequest {
    pub cwd: String,
    pub mcp_servers: Vec<McpServer>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub additional_directories: Option<Vec<String>>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct NewSessionResponse {
    pub session_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub modes: Option<SessionModeState>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct LoadSessionRequest {
    pub session_id: String,
    pub cwd: String,
    pub mcp_servers: Vec<McpServer>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub additional_directories: Option<Vec<String>>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct LoadSessionResponse {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub modes: Option<SessionModeState>,
}

/// An MCP server the client asks the agent to connect to. Parsed (the
/// request is invalid without the field) and never started: a client
/// cannot add a process or a connection to a local turn.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(untagged)]
pub enum McpServer {
    Http(McpServerRemote<HttpTag>),
    Sse(McpServerRemote<SseTag>),
    Stdio(McpServerStdio),
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub enum HttpTag {
    #[serde(rename = "http")]
    Http,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub enum SseTag {
    #[serde(rename = "sse")]
    Sse,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct McpServerRemote<T> {
    #[serde(rename = "type")]
    pub kind: T,
    pub name: String,
    pub url: String,
    pub headers: Vec<NameValue>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct McpServerStdio {
    pub name: String,
    pub command: String,
    pub args: Vec<String>,
    pub env: Vec<NameValue>,
}

/// An `EnvVariable` or an `HttpHeader`: the same shape.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct NameValue {
    pub name: String,
    pub value: String,
}

impl McpServer {
    pub fn name(&self) -> &str {
        match self {
            Self::Http(server) => &server.name,
            Self::Sse(server) => &server.name,
            Self::Stdio(server) => &server.name,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SessionModeState {
    pub current_mode_id: String,
    pub available_modes: Vec<SessionMode>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct SessionMode {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SetSessionModeRequest {
    pub session_id: String,
    pub mode_id: String,
}

// ---- prompt turn ---------------------------------------------------------

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct PromptRequest {
    pub session_id: String,
    pub prompt: Vec<ContentBlock>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct PromptResponse {
    pub stop_reason: StopReason,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum StopReason {
    EndTurn,
    MaxTokens,
    MaxTurnRequests,
    Refusal,
    Cancelled,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct CancelNotification {
    pub session_id: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ContentBlock {
    Text(TextContent),
    Image(ImageContent),
    Audio(AudioContent),
    ResourceLink(ResourceLink),
    Resource(EmbeddedResource),
}

impl ContentBlock {
    /// A plain text block.
    pub fn text(text: impl Into<String>) -> Self {
        Self::Text(TextContent {
            text: text.into(),
            annotations: None,
        })
    }
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Annotations {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub audience: Option<Vec<Role>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_modified: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub priority: Option<f64>,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Role {
    Assistant,
    User,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct TextContent {
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<Annotations>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ImageContent {
    pub data: String,
    pub mime_type: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub uri: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<Annotations>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct AudioContent {
    pub data: String,
    pub mime_type: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<Annotations>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ResourceLink {
    pub uri: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mime_type: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<Annotations>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct EmbeddedResource {
    pub resource: ResourceContents,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub annotations: Option<Annotations>,
}

/// `TextResourceContents` or `BlobResourceContents`.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(untagged)]
pub enum ResourceContents {
    Text {
        uri: String,
        #[serde(rename = "mimeType", default, skip_serializing_if = "Option::is_none")]
        mime_type: Option<String>,
        text: String,
    },
    Blob {
        uri: String,
        #[serde(rename = "mimeType", default, skip_serializing_if = "Option::is_none")]
        mime_type: Option<String>,
        blob: String,
    },
}

impl ResourceContents {
    pub fn uri(&self) -> &str {
        match self {
            Self::Text { uri, .. } | Self::Blob { uri, .. } => uri,
        }
    }
}

// ---- session/update ------------------------------------------------------

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct SessionNotification {
    pub session_id: String,
    pub update: SessionUpdate,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(tag = "sessionUpdate", rename_all = "snake_case")]
pub enum SessionUpdate {
    UserMessageChunk(ContentChunk),
    AgentMessageChunk(ContentChunk),
    AgentThoughtChunk(ContentChunk),
    ToolCall(ToolCall),
    ToolCallUpdate(ToolCallUpdate),
    Plan(Plan),
    AvailableCommandsUpdate(AvailableCommandsUpdate),
    CurrentModeUpdate(CurrentModeUpdate),
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ContentChunk {
    pub content: ContentBlock,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message_id: Option<String>,
}

impl ContentChunk {
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            content: ContentBlock::text(text),
            message_id: None,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ToolCall {
    pub tool_call_id: String,
    pub title: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default)]
    pub kind: ToolKind,
    #[serde(default)]
    pub status: ToolCallStatus,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub content: Vec<ToolCallContent>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub locations: Vec<ToolCallLocation>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw_input: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw_output: Option<Value>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ToolCallUpdate {
    pub tool_call_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub title: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<ToolKind>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub status: Option<ToolCallStatus>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<Vec<ToolCallContent>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub locations: Option<Vec<ToolCallLocation>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw_input: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub raw_output: Option<Value>,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ToolKind {
    Read,
    Edit,
    Delete,
    Move,
    Search,
    Execute,
    Think,
    Fetch,
    SwitchMode,
    #[default]
    Other,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ToolCallStatus {
    #[default]
    Pending,
    InProgress,
    Completed,
    Failed,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ToolCallContent {
    Content {
        content: ContentBlock,
    },
    Diff {
        path: String,
        #[serde(rename = "oldText", default, skip_serializing_if = "Option::is_none")]
        old_text: Option<String>,
        #[serde(rename = "newText")]
        new_text: String,
    },
    Terminal {
        #[serde(rename = "terminalId")]
        terminal_id: String,
    },
}

impl ToolCallContent {
    pub fn text(text: impl Into<String>) -> Self {
        Self::Content {
            content: ContentBlock::text(text),
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct ToolCallLocation {
    pub path: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub line: Option<u32>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Plan {
    pub entries: Vec<PlanEntry>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct PlanEntry {
    pub content: String,
    pub priority: PlanEntryPriority,
    pub status: PlanEntryStatus,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PlanEntryPriority {
    High,
    Medium,
    Low,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PlanEntryStatus {
    Pending,
    InProgress,
    Completed,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct AvailableCommandsUpdate {
    pub available_commands: Vec<AvailableCommand>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct AvailableCommand {
    pub name: String,
    pub description: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input: Option<AvailableCommandInput>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct AvailableCommandInput {
    pub hint: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct CurrentModeUpdate {
    pub current_mode_id: String,
}

// ---- session/request_permission (agent → client) -------------------------

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct RequestPermissionRequest {
    pub session_id: String,
    pub tool_call: ToolCallUpdate,
    pub options: Vec<PermissionOption>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct PermissionOption {
    pub option_id: String,
    pub name: String,
    pub kind: PermissionOptionKind,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PermissionOptionKind {
    AllowOnce,
    AllowAlways,
    RejectOnce,
    RejectAlways,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct RequestPermissionResponse {
    pub outcome: RequestPermissionOutcome,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(tag = "outcome", rename_all = "snake_case")]
pub enum RequestPermissionOutcome {
    Cancelled,
    Selected {
        #[serde(rename = "optionId")]
        option_id: String,
    },
}
