//! Closed ADK-Rust 2.0.0 to current `NodeEventV1` projection.
//!
//! The current compatibility profile handles bounded root-agent model/tool
//! events, direct sensitive-tool confirmations, dynamic stored-pipeline HITL
//! interrupts, direct MCP authorization and compiler-owned Printer
//! `interrupt_after` checkpoints. Transfers, citations, delegated MCP
//! authorization, arbitrary static graph breakpoints,
//! pipeline custom events and multi-agent branches remain closed until their
//! typed Elitea identities are available. Production construction remains
//! capability-gated behind the authorized lifecycle and durable resume owner.

#![allow(dead_code)] // Production construction waits for authorized progress delivery.

use std::collections::{BTreeMap, HashSet};
use std::fmt;

use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::{Event, FinishReason, Part};
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use chrono::{DateTime, SecondsFormat, Utc};
use ring::digest;
use serde::Deserialize;
use serde_json::{Value, json};

use super::direct_hitl::sensitive_call_identity;
use super::graph::{
    PIPELINE_APPLICATION_HITL_SCHEMA, PIPELINE_COMPLETED_CONTENT, PIPELINE_COMPLETED_METADATA_KEY,
    PIPELINE_COMPLETED_METADATA_VALUE, PIPELINE_NODE_METADATA_KEY, PRINTER_PAUSE_METADATA_KEY,
    PipelineLlmReplayEnvelope, PipelineNodeEventScope, PrinterPauseMetadata,
};
use super::internal_tools::{
    ASK_USER_ANSWER_ACTION, ASK_USER_GUARDRAIL_TYPE, ASK_USER_METADATA_KEY, ASK_USER_TOOL_NAME,
    InternalToolCatalog, decode_ask_user_request,
};
use super::sensitive_tools::SensitiveToolCatalog;
use crate::protocol::ProtocolError;
use crate::protocol::elitea::runtime::v1::NodeEventV1;
use crate::protocol::node_event::{
    MAX_CURRENT_NODE_EVENT_JSON_BYTES, encode_current_node_event_json,
};
use crate::toolkits::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DelegatedAuthorizationCatalog,
    DelegatedAuthorizationRequirement, decode_delegated_authorization_requirement,
};

const MAX_TOOL_CALLS_PER_MODEL_TURN: usize = 16;
const MAX_TOOL_RESULT_BYTES: usize = 1024 * 1024;
const TOOL_RESULT_CHUNK_BYTES: usize = 8 * 1024;
const MAX_TOOL_RESULT_CHUNKS: usize = MAX_TOOL_RESULT_BYTES / TOOL_RESULT_CHUNK_BYTES + 1;
const MAX_PROJECTED_EVENTS_PER_ADK_EVENT: usize =
    3 + 2 * MAX_TOOL_CALLS_PER_MODEL_TURN * MAX_TOOL_RESULT_CHUNKS;
const MAX_ADK_EVENT_ID_BYTES: usize = 512;
const MAX_ADK_PARTS_PER_EVENT: usize = 256;
const MAX_CONTEXT_TEXT_BYTES: usize = 2_048;
const MAX_COMPLETED_CONTENT_BYTES: usize = 60 * 1_024;
const MAX_TOOL_EVENT_VALUE_BYTES: usize = 40 * 1_024;
const PIPELINE_HITL_DIGEST_DOMAIN: &[u8] = b"elitea.pipeline-hitl-interrupt.v1\0";
const PIPELINE_TOOL_HITL_DIGEST_DOMAIN: &[u8] = b"elitea.pipeline-tool-hitl-interrupt.v1\0";
const PIPELINE_MCP_AUTH_DIGEST_DOMAIN: &[u8] = b"elitea.pipeline-mcp-auth-interrupt.v1\0";
const PIPELINE_CLARIFYING_DIGEST_DOMAIN: &[u8] = b"elitea.pipeline-clarifying-interrupt.v1\0";
const PIPELINE_HITL_SCHEMA: &str = "elitea.graph.hitl-interrupt.v1";
const PIPELINE_HITL_INTERACTION_TYPE: &str = "pipeline_hitl_node";
const PIPELINE_HITL_HISTORY_CONTRACT_VERSION: u8 = 1;
const PIPELINE_TOOL_HITL_SCHEMA: &str = "elitea.graph.tool-confirmation.v1";
const PIPELINE_MCP_AUTH_SCHEMA: &str = "elitea.graph.mcp-authorization.v1";
const PIPELINE_CLARIFYING_SCHEMA: &str = "elitea.graph.clarifying-question.v1";
const MAX_PIPELINE_HITL_MESSAGE_BYTES: usize = 8 * 1024;
const MAX_NESTED_PIPELINE_CHECKPOINTS: usize = 3;
const MAX_PIPELINE_INTERRUPT_MESSAGE_BYTES: usize = MAX_PIPELINE_HITL_MESSAGE_BYTES
    + MAX_NESTED_PIPELINE_CHECKPOINTS * (MAX_PIPELINE_NODE_IDENTITY_BYTES + 2);
const MAX_PIPELINE_NODE_IDENTITY_BYTES: usize = 128;
const MAX_AGENT_PATH_TIERS: usize = 3;
pub(crate) const APPLICATION_BRANCH_ROOT: &str = "elitea.saved_applications";
pub(crate) const DESCENDANT_CONTAINER_INVOCATION_KEY: &str =
    "elitea.descendant.container_invocation_id";
pub(crate) const DESCENDANT_PARENT_CALL_KEY: &str = "elitea.descendant.parent_call_id";
pub(crate) const DESCENDANT_CHECKPOINT_THREAD_KEY: &str = "elitea.descendant.checkpoint_thread_id";

/// Stable, low-cardinality event projection failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AgentEventProjectionErrorCode {
    InvalidState,
    UnsupportedCapability,
    ProviderFailure,
    ResourceExhausted,
    InvalidOutput,
}

impl AgentEventProjectionErrorCode {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidState => "agent_event.invalid_state",
            Self::UnsupportedCapability => "agent_event.unsupported_capability",
            Self::ProviderFailure => "agent_event.provider_failure",
            Self::ResourceExhausted => "agent_event.resource_exhausted",
            Self::InvalidOutput => "agent_event.invalid_output",
        }
    }
}

/// Data-free projector error.
///
/// Provider messages, model metadata, prompts, tool arguments and event
/// payloads are never included in `Display`, `Debug`, or `Error::source`.
pub(crate) struct AgentEventProjectionError {
    code: AgentEventProjectionErrorCode,
    protocol: Option<ProtocolError>,
}

impl AgentEventProjectionError {
    const fn invalid_state() -> Self {
        Self {
            code: AgentEventProjectionErrorCode::InvalidState,
            protocol: None,
        }
    }

    const fn unsupported() -> Self {
        Self {
            code: AgentEventProjectionErrorCode::UnsupportedCapability,
            protocol: None,
        }
    }

    const fn provider_failure() -> Self {
        Self {
            code: AgentEventProjectionErrorCode::ProviderFailure,
            protocol: None,
        }
    }

    fn output(error: ProtocolError) -> Self {
        let code = if matches!(error, ProtocolError::ResourceExhausted(_)) {
            AgentEventProjectionErrorCode::ResourceExhausted
        } else {
            AgentEventProjectionErrorCode::InvalidOutput
        };
        Self {
            code,
            protocol: Some(error),
        }
    }

    #[must_use]
    pub(crate) const fn code(&self) -> AgentEventProjectionErrorCode {
        self.code
    }
}

impl fmt::Debug for AgentEventProjectionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AgentEventProjectionError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AgentEventProjectionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AgentEventProjectionErrorCode::InvalidState => {
                "the agent event projector is not in the required lifecycle state"
            }
            AgentEventProjectionErrorCode::UnsupportedCapability => {
                "the ADK event is not supported by the current agent compatibility profile"
            }
            AgentEventProjectionErrorCode::ProviderFailure => {
                "the model provider reported an agent event failure"
            }
            AgentEventProjectionErrorCode::ResourceExhausted => {
                "the projected agent event exceeds its approved output limit"
            }
            AgentEventProjectionErrorCode::InvalidOutput => {
                "the projected agent event is malformed"
            }
        })
    }
}

impl std::error::Error for AgentEventProjectionError {}

/// Bounded projection result.
///
/// A caller sends and durably acknowledges every event in order before polling
/// the ADK stream again. The event slots stay heap-owned so nested projection
/// and async delivery never copy an 11 KiB inline array through the executor
/// stack; capacity grows only up to the admitted per-event maximum.
pub(crate) struct ProjectedAgentEventBatch {
    events: Vec<NodeEventV1>,
}

impl ProjectedAgentEventBatch {
    fn new() -> Self {
        Self {
            events: Vec::with_capacity(3 + 2 * MAX_TOOL_CALLS_PER_MODEL_TURN),
        }
    }

    fn push(&mut self, event: NodeEventV1) -> Result<(), AgentEventProjectionError> {
        if self.events.len() == MAX_PROJECTED_EVENTS_PER_ADK_EVENT {
            return Err(AgentEventProjectionError::invalid_state());
        }
        self.events.push(event);
        Ok(())
    }

    #[must_use]
    pub(crate) const fn len(&self) -> usize {
        self.events.len()
    }

    #[must_use]
    pub(crate) const fn is_empty(&self) -> bool {
        self.events.is_empty()
    }
}

impl IntoIterator for ProjectedAgentEventBatch {
    type Item = NodeEventV1;
    type IntoIter = std::vec::IntoIter<NodeEventV1>;

    fn into_iter(self) -> Self::IntoIter {
        self.events.into_iter()
    }
}

/// Sanitized browser-facing identity and presentation snapshot.
///
/// All fields are private. The future authorized assembler must remove skill
/// instruction bodies, credentials and private application settings before it
/// can construct this value.
pub(crate) struct AgentEventProjectionContext {
    stream_id: String,
    message_id: String,
    execution_generation: String,
    sio_event: String,
    thread_id: String,
    project_id: Value,
    chat_project_id: Value,
    root_agent_name: String,
    model_name: String,
    application_details: Value,
    graph_checkpoint_thread_id: Option<String>,
    should_continue: bool,
    hitl_resume: bool,
    parallel_reconcile: bool,
    invoked_skills: Vec<Value>,
    applied_skills: Vec<Value>,
    output_limit_behavior: OutputLimitBehavior,
    continuation_prefix: Option<String>,
}

#[derive(Clone, Copy)]
enum OutputLimitBehavior {
    UserControlled,
    Suppressed,
}

/// Validated ordinary-turn values derived from the authenticated command and
/// admitted input before provider execution begins.
pub(crate) struct OrdinaryProjectionInput {
    pub(crate) stream_id: String,
    pub(crate) message_id: String,
    pub(crate) execution_generation: String,
    pub(crate) sio_event: String,
    pub(crate) thread_id: String,
    pub(crate) project_id: Value,
    pub(crate) chat_project_id: Value,
    pub(crate) root_agent_name: String,
    pub(crate) model_name: String,
    pub(crate) application_details: Value,
    pub(crate) should_continue: bool,
    pub(crate) continuation_prefix: Option<String>,
}

/// Frozen public labels for saved participants exposed as model-callable tools.
///
/// The provider-visible tool name remains the execution identity. Presentation
/// data is joined only while projecting browser events so two calls to the same
/// participant retain distinct provider call IDs and UI accordions.
#[derive(Clone, Default)]
pub(crate) struct ApplicationToolPresentationCatalog {
    by_tool_name: BTreeMap<String, ApplicationToolPresentation>,
}

#[derive(Clone)]
struct ApplicationToolPresentation {
    display_name: String,
    agent_type: String,
    model_name: String,
    child_tools: ApplicationToolPresentationCatalog,
    guards: ApplicationToolGuardCatalogs,
}

#[derive(Clone, Default)]
pub(crate) struct ApplicationToolGuardCatalogs {
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
}

impl ApplicationToolGuardCatalogs {
    pub(crate) const fn new(
        sensitive_tools: SensitiveToolCatalog,
        delegated_authorization: DelegatedAuthorizationCatalog,
        internal_tools: InternalToolCatalog,
    ) -> Self {
        Self {
            sensitive_tools,
            delegated_authorization,
            internal_tools,
        }
    }
}

impl ApplicationToolPresentation {
    fn toolkit_type(&self) -> &'static str {
        if self.agent_type == "pipeline" {
            "pipeline"
        } else {
            "application"
        }
    }
}

impl ApplicationToolPresentationCatalog {
    pub(crate) fn insert(
        &mut self,
        tool_name: String,
        display_name: String,
        agent_type: String,
    ) -> Result<(), AgentEventProjectionError> {
        self.insert_runtime(
            tool_name,
            display_name,
            agent_type,
            "nested-model".to_owned(),
            Self::default(),
            ApplicationToolGuardCatalogs::default(),
        )
    }

    pub(crate) fn insert_runtime(
        &mut self,
        tool_name: String,
        display_name: String,
        agent_type: String,
        model_name: String,
        child_tools: Self,
        guards: ApplicationToolGuardCatalogs,
    ) -> Result<(), AgentEventProjectionError> {
        if !valid_tool_identity(&tool_name)
            || display_name.is_empty()
            || display_name.len() > MAX_CONTEXT_TEXT_BYTES
            || display_name.chars().any(char::is_control)
            || !matches!(agent_type.as_str(), "agent" | "pipeline")
            || model_name.is_empty()
            || model_name.len() > MAX_CONTEXT_TEXT_BYTES
            || model_name.chars().any(char::is_control)
            || self
                .by_tool_name
                .insert(
                    tool_name,
                    ApplicationToolPresentation {
                        display_name,
                        agent_type,
                        model_name,
                        child_tools,
                        guards,
                    },
                )
                .is_some()
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        Ok(())
    }

    fn get(&self, tool_name: &str) -> Option<&ApplicationToolPresentation> {
        self.by_tool_name.get(tool_name)
    }

    #[must_use]
    pub(crate) fn contains_runtime_tool(&self, tool_name: &str) -> bool {
        self.by_tool_name.contains_key(tool_name)
    }

    pub(crate) fn child_tools(&self, tool_name: &str) -> Option<&Self> {
        self.get(tool_name)
            .map(|presentation| &presentation.child_tools)
    }

    pub(crate) fn nested_sensitive_tools(&self, tool_name: &str) -> Option<&SensitiveToolCatalog> {
        self.get(tool_name)
            .map(|presentation| &presentation.guards.sensitive_tools)
    }

    pub(crate) fn nested_internal_tools(&self, tool_name: &str) -> Option<InternalToolCatalog> {
        self.get(tool_name)
            .map(|presentation| presentation.guards.internal_tools)
    }

    #[must_use]
    pub(crate) fn has_guarded_descendant(&self) -> bool {
        self.by_tool_name.values().any(|presentation| {
            !presentation.guards.sensitive_tools.is_empty()
                || !presentation.guards.delegated_authorization.is_empty()
                || !presentation.guards.internal_tools.is_empty()
                || presentation.child_tools.has_guarded_descendant()
        })
    }
}

/// Pipeline-only projection identity kept separate from the public chat thread.
pub(crate) struct PipelineProjectionInput {
    pub(crate) ordinary: OrdinaryProjectionInput,
    pub(crate) checkpoint_thread_id: String,
    pub(crate) should_continue: bool,
    pub(crate) hitl_resume: bool,
}

impl AgentEventProjectionContext {
    pub(crate) fn ordinary(
        input: OrdinaryProjectionInput,
    ) -> Result<Self, AgentEventProjectionError> {
        let context = Self {
            stream_id: input.stream_id,
            message_id: input.message_id,
            execution_generation: input.execution_generation,
            sio_event: input.sio_event,
            thread_id: input.thread_id,
            project_id: input.project_id,
            chat_project_id: input.chat_project_id,
            root_agent_name: input.root_agent_name,
            model_name: input.model_name,
            application_details: input.application_details,
            graph_checkpoint_thread_id: None,
            should_continue: input.should_continue,
            hitl_resume: false,
            parallel_reconcile: false,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            output_limit_behavior: OutputLimitBehavior::UserControlled,
            continuation_prefix: input.continuation_prefix,
        };
        validate_context(&context)?;
        Ok(context)
    }

    pub(crate) fn pipeline(
        input: PipelineProjectionInput,
    ) -> Result<Self, AgentEventProjectionError> {
        let mut context = Self::ordinary(input.ordinary)?;
        context.graph_checkpoint_thread_id = Some(input.checkpoint_thread_id);
        context.should_continue = input.should_continue;
        context.hitl_resume = input.hitl_resume;
        context.output_limit_behavior = OutputLimitBehavior::Suppressed;
        validate_context(&context)?;
        Ok(context)
    }

    fn nested(
        &self,
        root_agent_name: String,
        model_name: String,
        graph_checkpoint_thread_id: Option<String>,
    ) -> Result<Self, AgentEventProjectionError> {
        let context = Self {
            stream_id: self.stream_id.clone(),
            message_id: self.message_id.clone(),
            execution_generation: self.execution_generation.clone(),
            sio_event: self.sio_event.clone(),
            thread_id: self.thread_id.clone(),
            project_id: self.project_id.clone(),
            chat_project_id: self.chat_project_id.clone(),
            root_agent_name,
            model_name,
            application_details: self.application_details.clone(),
            graph_checkpoint_thread_id,
            should_continue: self.should_continue,
            hitl_resume: self.hitl_resume,
            parallel_reconcile: self.parallel_reconcile,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            output_limit_behavior: OutputLimitBehavior::Suppressed,
            continuation_prefix: None,
        };
        validate_context(&context)?;
        Ok(context)
    }
}

#[cfg(test)]
impl AgentEventProjectionContext {
    pub(crate) fn fixture(application_details: Value) -> Self {
        Self {
            stream_id: "conversation-1".to_owned(),
            message_id: "message-1".to_owned(),
            execution_generation: "generation-1".to_owned(),
            sio_event: "chat_predict".to_owned(),
            thread_id: "thread-1".to_owned(),
            project_id: json!(7),
            chat_project_id: json!(7),
            root_agent_name: "root-agent".to_owned(),
            model_name: "model-1".to_owned(),
            application_details,
            graph_checkpoint_thread_id: None,
            should_continue: false,
            hitl_resume: false,
            parallel_reconcile: false,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            output_limit_behavior: OutputLimitBehavior::UserControlled,
            continuation_prefix: None,
        }
    }

    pub(crate) fn output_continuation_fixture(
        application_details: Value,
        previous_content: &str,
    ) -> Self {
        let mut context = Self::fixture(application_details);
        context.should_continue = true;
        context.continuation_prefix = Some(previous_content.to_owned());
        context
    }

    pub(crate) fn pipeline_fixture(application_details: Value) -> Self {
        let mut context = Self::fixture(application_details);
        context.graph_checkpoint_thread_id = Some("thread-1".to_owned());
        context.output_limit_behavior = OutputLimitBehavior::Suppressed;
        context
    }
}

struct ActiveModelTurn {
    event_id: String,
    timestamp_start: String,
    content: String,
    thinking: String,
}

struct CompletedModelTurn {
    output_limited: bool,
}

#[derive(Clone)]
struct ActiveToolCall {
    name: String,
    /// Exact provider-produced arguments retained only for result correlation
    /// and the private sensitive-call identity. Browser events must use
    /// `public_arguments` instead.
    arguments: Value,
    public_arguments: Value,
    timestamp_start: String,
    application: Option<ApplicationToolPresentation>,
    sibling_ordinal: Option<usize>,
    pipeline_node_name: Option<String>,
}

#[derive(Clone)]
struct AgentPathTier {
    name: String,
    call_id: String,
    sibling_ordinal: Option<usize>,
}

struct DescendantAgentProjector {
    tier: AgentPathTier,
    projector: Box<AgentEventProjector>,
}

/// Sanitized completion selected by the application/ad-hoc assembler.
///
/// ADK stream EOS and a model final-response event are not sufficient to pick
/// the application result: stored pipelines can declare a different terminal
/// output key. Production construction stays closed until that result adapter
/// is implemented.
pub(crate) struct CompletedAgentBrowserOutput {
    content: String,
    thread_id: String,
    execution_finished: bool,
    context_info: Value,
}

impl CompletedAgentBrowserOutput {
    pub(crate) fn ordinary(
        content: String,
        thread_id: String,
    ) -> Result<Self, AgentEventProjectionError> {
        validate_public_text(&thread_id)?;
        if content.len() > MAX_COMPLETED_CONTENT_BYTES {
            return Err(AgentEventProjectionError::output(
                ProtocolError::ResourceExhausted(
                    "the completed agent content exceeds its approved limit",
                ),
            ));
        }
        if content.is_empty() || content.contains('\0') {
            return Err(AgentEventProjectionError::output(
                ProtocolError::InvalidInput("the completed agent content is malformed"),
            ));
        }
        Ok(Self {
            content,
            thread_id,
            execution_finished: true,
            context_info: Value::Null,
        })
    }
}

#[cfg(test)]
impl CompletedAgentBrowserOutput {
    pub(crate) fn fixture(content: &str) -> Self {
        Self {
            content: content.to_owned(),
            thread_id: "thread-1".to_owned(),
            execution_finished: true,
            context_info: Value::Null,
        }
    }
}

struct OrdinaryModelEvent {
    content: String,
    thinking: String,
    closes_turn: bool,
    output_limited: bool,
    timestamp: String,
}

enum ProjectionState {
    Created,
    Started,
    Active(ActiveModelTurn),
    Complete(CompletedModelTurn),
    PrinterComplete,
    Paused,
    Finished,
}

/// Stateful ordinary-text compatibility projector.
pub(crate) struct AgentEventProjector {
    context: AgentEventProjectionContext,
    state: ProjectionState,
    invocation_id: Option<String>,
    active_tools: BTreeMap<String, ActiveToolCall>,
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    application_tools: ApplicationToolPresentationCatalog,
    descendants: BTreeMap<String, DescendantAgentProjector>,
    pipeline_result: Option<String>,
    saw_pipeline_node_events: bool,
    continuation_overlap: Option<ContinuationOverlap>,
}

const MAX_CONTINUATION_OVERLAP_CHARS: usize = 150;

struct ContinuationOverlap {
    previous_content: String,
    raw_content: String,
    removed_prefix_bytes: Option<usize>,
    injected_separator: bool,
}

impl ContinuationOverlap {
    fn new(previous_content: String) -> Self {
        Self {
            previous_content,
            raw_content: String::new(),
            removed_prefix_bytes: None,
            injected_separator: false,
        }
    }

    fn project(
        &mut self,
        current: String,
        closes_turn: bool,
    ) -> Result<String, AgentEventProjectionError> {
        let (raw_content, _) = merge_stream_value(&self.raw_content, current)?;
        self.raw_content = raw_content;
        if self.removed_prefix_bytes.is_none() {
            if !closes_turn && self.raw_content.chars().count() <= MAX_CONTINUATION_OVERLAP_CHARS {
                return Ok(String::new());
            }
            let trimmed = trim_continuation_overlap(&self.previous_content, &self.raw_content);
            self.injected_separator = trimmed
                .strip_prefix(' ')
                .is_some_and(|content| content == self.raw_content);
            self.removed_prefix_bytes = Some(self.raw_content.len().saturating_sub(trimmed.len()));
        }
        let removed = self.removed_prefix_bytes.unwrap_or_default();
        let projected = self
            .raw_content
            .get(removed..)
            .map(ToOwned::to_owned)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if self.injected_separator {
            let mut separated = String::with_capacity(projected.len().saturating_add(1));
            separated.push(' ');
            separated.push_str(&projected);
            Ok(separated)
        } else {
            Ok(projected)
        }
    }

    fn completed(&self, content: &str) -> String {
        trim_continuation_overlap(&self.previous_content, content)
    }
}

impl AgentEventProjector {
    pub(crate) fn new(
        context: AgentEventProjectionContext,
    ) -> Result<Self, AgentEventProjectionError> {
        Self::with_sensitive_tools(context, SensitiveToolCatalog::default())
    }

    pub(crate) fn with_sensitive_tools(
        context: AgentEventProjectionContext,
        sensitive_tools: SensitiveToolCatalog,
    ) -> Result<Self, AgentEventProjectionError> {
        Self::with_tool_catalogs(
            context,
            sensitive_tools,
            ApplicationToolPresentationCatalog::default(),
        )
    }

    pub(crate) fn with_tool_catalogs(
        context: AgentEventProjectionContext,
        sensitive_tools: SensitiveToolCatalog,
        application_tools: ApplicationToolPresentationCatalog,
    ) -> Result<Self, AgentEventProjectionError> {
        Self::with_runtime_catalogs(
            context,
            sensitive_tools,
            DelegatedAuthorizationCatalog::default(),
            application_tools,
        )
    }

    pub(crate) fn with_runtime_catalogs(
        context: AgentEventProjectionContext,
        sensitive_tools: SensitiveToolCatalog,
        delegated_authorization: DelegatedAuthorizationCatalog,
        application_tools: ApplicationToolPresentationCatalog,
    ) -> Result<Self, AgentEventProjectionError> {
        validate_context(&context)?;
        let continuation_overlap = context
            .continuation_prefix
            .clone()
            .filter(|content| !content.is_empty())
            .map(ContinuationOverlap::new);
        Ok(Self {
            context,
            state: ProjectionState::Created,
            invocation_id: None,
            active_tools: BTreeMap::new(),
            sensitive_tools,
            delegated_authorization,
            application_tools,
            descendants: BTreeMap::new(),
            pipeline_result: None,
            saw_pipeline_node_events: false,
            continuation_overlap,
        })
    }

    /// Emit the current `agent_start` event before the ADK stream is started.
    /// The lifecycle must durably acknowledge this batch before calling
    /// `NativeAgentInvocation::start`.
    pub(crate) fn start(
        &mut self,
        occurred_at: DateTime<Utc>,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(self.state, ProjectionState::Created) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let event = self.event(
            "agent_start",
            &Value::Null,
            None,
            &json!({
                "invoked_skills": self.context.invoked_skills,
                "should_continue": self.context.should_continue,
            }),
            occurred_at,
        )?;
        self.state = ProjectionState::Started;
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(event)?;
        Ok(batch)
    }

    /// Project one ordinary root-agent ADK event.
    ///
    /// The returned batch contains at most four inline events. The caller must
    /// persist/send/ACK them in iteration order before polling ADK again.
    pub(crate) fn project(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        validate_event_id(&event.id)?;
        validate_invocation_id(&event.invocation_id)?;
        let has_descendant_container = event
            .provider_metadata
            .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY);
        let has_descendant_call = event
            .provider_metadata
            .contains_key(DESCENDANT_PARENT_CALL_KEY);
        let has_descendant_checkpoint = event
            .provider_metadata
            .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY);
        if !(has_descendant_container || has_descendant_call)
            && let Some(child_interrupt) = nested_pipeline_interrupt_child_event(
                event,
                &self.context.root_agent_name,
                self.context.graph_checkpoint_thread_id.as_deref(),
            )?
        {
            let batch = self.project_descendant_event(&child_interrupt.event)?;
            return replace_nested_interrupt_identity(
                batch,
                &child_interrupt.interrupt_id,
                &child_interrupt.call_digest,
            );
        }
        if has_descendant_container || has_descendant_call {
            if !(has_descendant_container && has_descendant_call) {
                return Err(AgentEventProjectionError::invalid_state());
            }
            return self.project_descendant_event(event);
        }
        if has_descendant_checkpoint {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if pipeline_node_name(event)?.is_some() {
            self.saw_pipeline_node_events = true;
        }
        if self
            .invocation_id
            .as_deref()
            .is_some_and(|expected| expected != event.invocation_id)
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if event.actions.tool_confirmation.is_some() {
            let batch = if event.provider_metadata.contains_key(ASK_USER_METADATA_KEY) {
                self.project_clarifying_question(event)?
            } else if event
                .provider_metadata
                .contains_key(DELEGATED_AUTHORIZATION_METADATA_KEY)
            {
                self.project_delegated_authorization_confirmation(event)?
            } else {
                self.project_sensitive_confirmation(event)?
            };
            self.bind_invocation_id(event);
            return Ok(batch);
        }
        if event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY) {
            let batch = self.project_graph_interrupt(event)?;
            self.bind_invocation_id(event);
            return Ok(batch);
        }
        if event
            .provider_metadata
            .contains_key(PIPELINE_COMPLETED_METADATA_KEY)
        {
            let batch = self.project_pipeline_completion(event)?;
            self.bind_invocation_id(event);
            return Ok(batch);
        }
        validate_adk_event(event, &self.context.root_agent_name)?;
        let tool_calls = event.tool_calls();
        let tool_results = event.tool_results();
        if !tool_calls.is_empty() && !tool_results.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if !tool_results.is_empty() {
            let batch = self.project_tool_results(event, &tool_results)?;
            self.bind_invocation_id(event);
            return Ok(batch);
        }
        let Some(model_event) = ordinary_model_event(event, !tool_calls.is_empty())? else {
            self.bind_invocation_id(event);
            return Ok(ProjectedAgentEventBatch::new());
        };
        let mut batch = self.project_model_event(event, model_event)?;
        if !tool_calls.is_empty() {
            self.project_tool_starts(event, &tool_calls, &mut batch)?;
        }
        self.bind_invocation_id(event);
        Ok(batch)
    }

    fn project_descendant_event(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let container_invocation_id = event
            .provider_metadata
            .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
            .filter(|value| valid_tool_identity(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let parent_call_id = event
            .provider_metadata
            .get(DESCENDANT_PARENT_CALL_KEY)
            .filter(|value| valid_tool_identity(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        self.route_descendant(event, container_invocation_id, parent_call_id)?
            .ok_or_else(AgentEventProjectionError::invalid_state)
    }

    fn route_descendant(
        &mut self,
        event: &Event,
        container_invocation_id: &str,
        parent_call_id: &str,
    ) -> Result<Option<ProjectedAgentEventBatch>, AgentEventProjectionError> {
        if self.invocation_id.as_deref() == Some(container_invocation_id) {
            return self
                .project_immediate_descendant(event, parent_call_id)
                .map(Some);
        }
        for descendant in self.descendants.values_mut() {
            let Some(batch) = descendant.projector.route_descendant(
                event,
                container_invocation_id,
                parent_call_id,
            )?
            else {
                continue;
            };
            return overlay_batch_hierarchy(batch, std::slice::from_ref(&descendant.tier))
                .map(Some);
        }
        Ok(None)
    }

    fn project_immediate_descendant(
        &mut self,
        event: &Event,
        parent_call_id: &str,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let active = self
            .active_tools
            .get(parent_call_id)
            .cloned()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let application = active
            .application
            .clone()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let checkpoint_thread_id = descendant_checkpoint_thread(
            event,
            &application,
            self.context.graph_checkpoint_thread_id.as_deref(),
            parent_call_id,
        )?;
        if !self.descendants.contains_key(parent_call_id) {
            let nested_context = self.context.nested(
                active.name.clone(),
                application.model_name.clone(),
                checkpoint_thread_id.clone(),
            )?;
            let mut projector = Self::with_runtime_catalogs(
                nested_context,
                application.guards.sensitive_tools.clone(),
                application.guards.delegated_authorization.clone(),
                application.child_tools.clone(),
            )?;
            let _start = projector.start(event.timestamp)?;
            self.descendants.insert(
                parent_call_id.to_owned(),
                DescendantAgentProjector {
                    tier: AgentPathTier {
                        name: application.display_name,
                        call_id: parent_call_id.to_owned(),
                        sibling_ordinal: active.sibling_ordinal,
                    },
                    projector: Box::new(projector),
                },
            );
        }
        let descendant = self
            .descendants
            .get_mut(parent_call_id)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if descendant.projector.context.graph_checkpoint_thread_id != checkpoint_thread_id {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let mut child_event = event.clone();
        child_event
            .provider_metadata
            .remove(DESCENDANT_CONTAINER_INVOCATION_KEY);
        child_event
            .provider_metadata
            .remove(DESCENDANT_PARENT_CALL_KEY);
        child_event
            .provider_metadata
            .remove(DESCENDANT_CHECKPOINT_THREAD_KEY);
        let batch = descendant.projector.project(&child_event)?;
        overlay_batch_hierarchy(batch, std::slice::from_ref(&descendant.tier))
    }

    fn project_graph_interrupt(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let payload = GraphInterruptPayload::from_event(event)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if event
            .provider_metadata
            .contains_key(PRINTER_PAUSE_METADATA_KEY)
        {
            return self.project_pipeline_printer(event, &payload);
        }
        let (interrupt_data, _) = pipeline_interrupt_data(&payload, &payload.thread_id)?;
        let guardrail_type = interrupt_data
            .get("guardrail_type")
            .and_then(Value::as_str)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        match guardrail_type {
            "pipeline_hitl" => self.project_pipeline_hitl(event, &payload),
            "sensitive_tool" => self.project_pipeline_tool_confirmation(event, &payload),
            "mcp_auth" => self.project_pipeline_mcp_authorization(event, &payload),
            ASK_USER_GUARDRAIL_TYPE => self.project_pipeline_clarifying_question(event, &payload),
            "application_sensitive_tool" => {
                self.project_pipeline_application_confirmation(event, &payload)
            }
            _ => Err(AgentEventProjectionError::unsupported()),
        }
    }

    fn project_pipeline_application_confirmation(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_application_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        let _active = self
            .active_tools
            .get(binding.application_call_id())
            .filter(|active| {
                active.name == binding.application_tool_name() && active.application.is_some()
            })
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let descendant = self
            .descendants
            .get(binding.application_call_id())
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if !matches!(self.state, ProjectionState::Complete(_)) || !descendant.projector.is_paused()
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        self.state = ProjectionState::Paused;
        Ok(ProjectedAgentEventBatch::new())
    }

    fn project_sensitive_confirmation(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        validate_confirmation_event(event, &self.context.root_agent_name)?;
        if !matches!(self.state, ProjectionState::Complete(_)) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let request = event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_tool_identity(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if !valid_tool_identity(&request.tool_name) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_tool_event_value(&request.args)?;
        let active = self
            .active_tools
            .get(call_id)
            .filter(|active| active.name == request.tool_name && active.arguments == request.args)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let policy = self
            .sensitive_tools
            .policy_for(&request.tool_name)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let (interrupt_id, call_digest) = sensitive_call_identity(
            &event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .map_err(|error| match error.code() {
            super::direct_hitl::DirectHitlErrorCode::ResourceExhausted => {
                AgentEventProjectionError {
                    code: AgentEventProjectionErrorCode::ResourceExhausted,
                    protocol: None,
                }
            }
            _ => AgentEventProjectionError::invalid_state(),
        })?;
        let tool_args = mask_sensitive_arguments(&active.arguments, 0)?;
        let message = policy.policy_message().to_owned();
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "message": message.clone(),
            "hitl_interrupt": true,
            "hitl_interrupts": [{
                "type": "hitl",
                "interrupt_id": interrupt_id,
                "call_digest": call_digest,
                "guardrail_type": "sensitive_tool",
                "node_name": "sensitive_tool_guard",
                "message": policy.policy_message(),
                "available_actions": ["approve", "reject", "block_with_comment"],
                "routes": {},
                "tool_call_id": call_id,
                "tool_name": request.tool_name,
                "toolkit_name": policy.toolkit_name(),
                "toolkit_type": policy.toolkit_type(),
                "action_label": policy.action_name(),
                "tool_args": tool_args,
                "policy_message": policy.policy_message(),
            }],
            "node_name": "sensitive_tool_guard",
            "available_actions": ["approve", "reject", "block_with_comment"],
            "routes": {},
            "edit_state_key": Value::Null,
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_hitl_interrupt",
            &Value::String(message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_clarifying_question(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        validate_confirmation_event(event, &self.context.root_agent_name)?;
        if !matches!(self.state, ProjectionState::Complete(_)) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let request = event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_tool_identity(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if request.tool_name != ASK_USER_TOOL_NAME {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_tool_event_value(&request.args)?;
        let _active = self
            .active_tools
            .get(call_id)
            .filter(|active| active.name == request.tool_name && active.arguments == request.args)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let questions = event
            .provider_metadata
            .get(ASK_USER_METADATA_KEY)
            .and_then(|value| decode_ask_user_request(value))
            .filter(|value| value.matches_arguments(&request.args))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let (interrupt_id, call_digest) = sensitive_call_identity(
            &event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
        let message = questions.message().to_owned();
        let question_values = questions.questions_value();
        let tool_args = questions.arguments_value();
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "message": message,
            "hitl_interrupt": true,
            "hitl_interrupts": [{
                "type": "hitl",
                "interrupt_id": interrupt_id,
                "call_digest": call_digest,
                "guardrail_type": ASK_USER_GUARDRAIL_TYPE,
                "node_name": ASK_USER_TOOL_NAME,
                "message": message,
                "questions": question_values,
                "available_actions": [ASK_USER_ANSWER_ACTION],
                "routes": {},
                "tool_call_id": call_id,
                "tool_name": ASK_USER_TOOL_NAME,
                "toolkit_name": ASK_USER_TOOL_NAME,
                "toolkit_type": "internal",
                "tool_args": tool_args,
            }],
            "node_name": ASK_USER_TOOL_NAME,
            "available_actions": [ASK_USER_ANSWER_ACTION],
            "routes": {},
            "edit_state_key": Value::Null,
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_hitl_interrupt",
            &Value::String(message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_delegated_authorization_confirmation(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        validate_confirmation_event(event, &self.context.root_agent_name)?;
        if !matches!(self.state, ProjectionState::Complete(_)) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let request = event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_tool_identity(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        validate_tool_event_value(&request.args)?;
        let _active = self
            .active_tools
            .get(call_id)
            .filter(|active| active.name == request.tool_name && active.arguments == request.args)
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let requirement = event
            .provider_metadata
            .get(DELEGATED_AUTHORIZATION_METADATA_KEY)
            .and_then(|value| decode_delegated_authorization_requirement(value))
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if self
            .delegated_authorization
            .requirement_for(&request.tool_name)
            .is_none_or(|expected| !expected.same_authority(&requirement))
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let (interrupt_id, call_digest) = sensitive_call_identity(
            &event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
        let message = requirement.user_message();
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "root_thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "guardrail_type": "mcp_auth",
            "interrupt_id": interrupt_id,
            "call_digest": call_digest,
            "node_name": "delegated_authorization_guard",
            "message": message,
            "available_actions": ["authorize", "skip"],
            "tool_call_id": call_id,
            "tool_name": request.tool_name,
            "toolkit_name": requirement.toolkit_name(),
            "toolkit_type": requirement.toolkit_type(),
            "tool_args": {},
            "server_url": requirement.server_url(),
            "resource_metadata_url": requirement.resource_metadata_url(),
            "www_authenticate": requirement.www_authenticate(),
            "resource_metadata": requirement.resource_metadata(),
            "authorization_servers": requirement.authorization_servers(),
            "resume_strategy": "root",
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "mcp_authorization_required",
            &Value::String(message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_pipeline_printer(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(self.state, ProjectionState::Started) || !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_printer_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        self.pipeline_result = Some(binding.output);
        self.state = ProjectionState::PrinterComplete;
        Ok(ProjectedAgentEventBatch::new())
    }

    fn project_pipeline_hitl(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Complete(_)
        ) || !self.active_tools.is_empty()
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_hitl_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        let data = binding.data;
        let message = data.message.clone();
        let pending = json!({
            "type": "hitl",
            "interaction_type": data.interaction_type,
            "history_contract_version": data.history_contract_version,
            "interrupt_id": binding.interrupt_id,
            "call_digest": binding.call_digest,
            "guardrail_type": "pipeline_hitl",
            "node_name": data.node_name,
            "message": data.message,
            "available_actions": data.available_actions,
            "routes": data.routes,
            "edit_state_key": data.edit_state_key,
            "definition_digest": data.definition_digest,
        });
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "message": data.message,
            "interaction_type": data.interaction_type,
            "history_contract_version": data.history_contract_version,
            "hitl_interrupt": pending,
            "hitl_interrupts": [pending],
            "node_name": data.node_name,
            "available_actions": data.available_actions,
            "routes": data.routes,
            "edit_state_key": data.edit_state_key,
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_hitl_interrupt",
            &Value::String(message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_pipeline_tool_confirmation(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_tool_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Complete(_)
        ) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if let Some(replay) = binding.llm_replay() {
            let active = self
                .active_tools
                .get(binding.tool_call_id())
                .filter(|active| active.name == binding.tool_name())
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            if !replay
                .matches_pending_call(
                    binding.tool_call_id(),
                    binding.tool_name(),
                    &active.arguments,
                )
                .map_err(|_| AgentEventProjectionError::invalid_state())?
            {
                return Err(AgentEventProjectionError::invalid_state());
            }
        } else if !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let data = binding.data;
        let pending = json!({
            "type": "hitl",
            "interrupt_id": binding.interrupt_id,
            "call_digest": binding.call_digest,
            "guardrail_type": "sensitive_tool",
            "node_name": data.node_name,
            "message": data.message,
            "available_actions": data.available_actions,
            "routes": {},
            "tool_call_id": data.tool_call_id,
            "tool_name": data.tool_name,
            "toolkit_name": data.toolkit_name,
            "toolkit_type": data.toolkit_type,
            "action_label": data.action_label,
            "tool_args": data.tool_args,
            "policy_message": data.policy_message,
            "definition_digest": data.definition_digest,
            "argument_digest": data.argument_digest,
        });
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "message": data.message,
            "hitl_interrupt": pending,
            "hitl_interrupts": [pending],
            "node_name": data.node_name,
            "available_actions": data.available_actions,
            "routes": {},
            "edit_state_key": Value::Null,
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_hitl_interrupt",
            &Value::String(data.message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_pipeline_mcp_authorization(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Complete(_)
        ) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_mcp_auth_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        if let Some(replay) = binding.llm_replay() {
            let active = self
                .active_tools
                .get(binding.tool_call_id())
                .filter(|active| active.name == binding.tool_name())
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            if !replay
                .matches_pending_call(
                    binding.tool_call_id(),
                    binding.tool_name(),
                    &active.arguments,
                )
                .map_err(|_| AgentEventProjectionError::invalid_state())?
            {
                return Err(AgentEventProjectionError::invalid_state());
            }
        } else if !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let data = binding.data;
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "root_thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "guardrail_type": "mcp_auth",
            "interrupt_id": binding.interrupt_id,
            "call_digest": binding.call_digest,
            "node_name": data.node_name,
            "message": data.message,
            "available_actions": data.available_actions,
            "tool_call_id": data.tool_call_id,
            "tool_name": data.tool_name,
            "toolkit_name": data.toolkit_name,
            "toolkit_type": data.toolkit_type,
            "tool_args": {},
            "server_url": data.server_url,
            "resource_metadata_url": data.resource_metadata_url,
            "www_authenticate": data.www_authenticate,
            "resource_metadata": data.resource_metadata,
            "authorization_servers": data.authorization_servers(),
            "resume_strategy": "root",
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "mcp_authorization_required",
            &Value::String(data.message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_pipeline_clarifying_question(
        &mut self,
        event: &Event,
        payload: &GraphInterruptPayload,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Complete(_)
        ) {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let checkpoint_thread_id = self
            .context
            .graph_checkpoint_thread_id
            .as_deref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let binding = pipeline_clarifying_event_binding_from_payload(
            event,
            payload,
            &self.context.root_agent_name,
            checkpoint_thread_id,
        )?;
        let active = self
            .active_tools
            .get(binding.tool_call_id())
            .filter(|active| active.name == binding.tool_name())
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if !binding
            .llm_replay()
            .matches_pending_call(
                binding.tool_call_id(),
                binding.tool_name(),
                &active.arguments,
            )
            .map_err(|_| AgentEventProjectionError::invalid_state())?
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let data = binding.data;
        let message = data.message.clone();
        let pending = json!({
            "type": "hitl",
            "interrupt_id": binding.interrupt_id,
            "call_digest": binding.call_digest,
            "guardrail_type": ASK_USER_GUARDRAIL_TYPE,
            "node_name": data.node_name,
            "message": data.message,
            "questions": data.questions,
            "available_actions": [ASK_USER_ANSWER_ACTION],
            "routes": {},
            "tool_call_id": data.tool_call_id,
            "tool_name": ASK_USER_TOOL_NAME,
            "toolkit_name": ASK_USER_TOOL_NAME,
            "toolkit_type": "internal",
            "tool_args": data.tool_args,
            "definition_digest": data.definition_digest,
            "argument_digest": data.argument_digest,
        });
        let metadata = json!({
            "thread_id": self.context.thread_id,
            "chat_project_id": self.context.chat_project_id,
            "message": message,
            "hitl_interrupt": pending,
            "hitl_interrupts": [pending],
            "node_name": data.node_name,
            "available_actions": [ASK_USER_ANSWER_ACTION],
            "routes": {},
            "edit_state_key": Value::Null,
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_hitl_interrupt",
            &Value::String(message),
            None,
            &metadata,
            event.timestamp,
        )?)?;
        self.state = ProjectionState::Paused;
        Ok(batch)
    }

    fn project_pipeline_completion(
        &mut self,
        event: &Event,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if self.context.graph_checkpoint_thread_id.is_none()
            || !matches!(
                self.state,
                ProjectionState::Started | ProjectionState::Complete(_)
            )
            || !self.active_tools.is_empty()
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_adk_event(event, &self.context.root_agent_name)?;
        let content = event
            .content()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let [Part::Text { text }] = content.parts.as_slice() else {
            return Err(AgentEventProjectionError::invalid_state());
        };
        let terminal = text == PIPELINE_COMPLETED_CONTENT;
        let reused_result = match event
            .provider_metadata
            .get(super::graph::PIPELINE_REUSED_RESULT_METADATA_KEY)
        {
            None => false,
            Some(value) if value == "v1" => true,
            Some(_) => return Err(AgentEventProjectionError::invalid_state()),
        };
        if content.role != "assistant"
            || text.is_empty()
            || text.len() > MAX_COMPLETED_CONTENT_BYTES
            || text.contains('\0')
            || event.provider_metadata.len() != 1 + usize::from(reused_result)
            || event
                .provider_metadata
                .get(PIPELINE_COMPLETED_METADATA_KEY)
                .map(String::as_str)
                != Some(PIPELINE_COMPLETED_METADATA_VALUE)
            || !event.actions.state_delta.is_empty()
            || event.llm_response.partial
            || event.llm_response.interrupted
            || event.llm_response.finish_reason.is_some()
            || event.llm_response.usage_metadata.is_some()
            || event.llm_response.provider_metadata.is_some()
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let batch = if terminal {
            ProjectedAgentEventBatch::new()
        } else {
            self.pipeline_result = Some(text.clone());
            if self.saw_pipeline_node_events || reused_result {
                ProjectedAgentEventBatch::new()
            } else {
                self.project_model_event(
                    event,
                    OrdinaryModelEvent {
                        content: text.clone(),
                        thinking: String::new(),
                        closes_turn: true,
                        output_limited: false,
                        timestamp: event
                            .timestamp
                            .to_rfc3339_opts(SecondsFormat::AutoSi, false),
                    },
                )?
            }
        };
        self.state = ProjectionState::Complete(CompletedModelTurn {
            output_limited: false,
        });
        Ok(batch)
    }

    #[must_use]
    pub(crate) fn is_paused(&self) -> bool {
        matches!(self.state, ProjectionState::Paused)
            || self
                .descendants
                .values()
                .any(|descendant| descendant.projector.is_paused())
    }

    fn project_model_event(
        &mut self,
        event: &Event,
        model_event: OrdinaryModelEvent,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let timestamp = model_event.timestamp;
        let output_limited = model_event.output_limited;
        let starts_turn = matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Complete(_)
        );
        if starts_turn && !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let (event_id, timestamp_start, previous_content, previous_thinking) = match &self.state {
            ProjectionState::Started | ProjectionState::Complete(_) => {
                (event.id.as_str(), timestamp.as_str(), "", "")
            }
            ProjectionState::Active(turn) => {
                if turn.event_id != event.id {
                    return Err(AgentEventProjectionError::unsupported());
                }
                (
                    turn.event_id.as_str(),
                    turn.timestamp_start.as_str(),
                    turn.content.as_str(),
                    turn.thinking.as_str(),
                )
            }
            ProjectionState::Created
            | ProjectionState::PrinterComplete
            | ProjectionState::Paused
            | ProjectionState::Finished => {
                return Err(AgentEventProjectionError::invalid_state());
            }
        };
        let model_content = if let Some(overlap) = self.continuation_overlap.as_mut() {
            overlap.project(model_event.content, model_event.closes_turn)?
        } else {
            model_event.content
        };
        let (next_content, content_delta) = merge_stream_value(previous_content, model_content)?;
        let (next_thinking, thinking_delta) =
            merge_stream_value(previous_thinking, model_event.thinking)?;
        let next_timestamp_start = timestamp_start.to_owned();

        let mut batch = ProjectedAgentEventBatch::new();
        if starts_turn {
            batch.push(self.model_start_event(event, &timestamp)?)?;
        }

        if let Some(chunk) = self.model_chunk_event(event, content_delta, thinking_delta)? {
            batch.push(chunk)?;
        }

        if model_event.closes_turn {
            let (response_tool_name, response_tool_metadata) = model_step_presentation(event)?;
            let step = json!({
                "tool_run_id": event_id,
                "type": "ChatGeneration",
                "text": next_content,
                "thinking": next_thinking,
                "timestamp_start": next_timestamp_start,
                "timestamp_finish": timestamp,
                "message": {"response_metadata": {
                    "model_name": self.context.model_name,
                    "tool_name": response_tool_name,
                    "metadata": response_tool_metadata,
                }},
            });
            batch.push(self.event(
                "agent_llm_end",
                &Value::Null,
                None,
                &json!({"tool_run_id": event.id, "thinking_steps": [step.clone()]}),
                event.timestamp,
            )?)?;
            batch.push(self.event(
                "partial_message",
                &Value::Null,
                None,
                &json!({
                    "project_id": self.context.project_id,
                    "chat_project_id": self.context.chat_project_id,
                    "thread_id": self.context.thread_id,
                    "thinking_steps": [step],
                    "tool_calls": {},
                    "additional_response_meta": {},
                    "invoked_skills": self.context.applied_skills,
                }),
                event.timestamp,
            )?)?;
            self.state = ProjectionState::Complete(CompletedModelTurn { output_limited });
            if let Some(confirmation) = self.output_limit_confirmation(event, output_limited)? {
                batch.push(confirmation)?;
            }
        } else {
            self.state = ProjectionState::Active(ActiveModelTurn {
                event_id: event.id.clone(),
                timestamp_start: next_timestamp_start,
                content: next_content,
                thinking: next_thinking,
            });
        }
        Ok(batch)
    }

    fn model_chunk_event(
        &self,
        event: &Event,
        content_delta: String,
        thinking_delta: String,
    ) -> Result<Option<NodeEventV1>, AgentEventProjectionError> {
        if content_delta.is_empty() && thinking_delta.is_empty() {
            return Ok(None);
        }
        let content = if content_delta.is_empty() {
            Value::Null
        } else {
            Value::String(content_delta)
        };
        self.event(
            "agent_llm_chunk",
            &content,
            (!thinking_delta.is_empty()).then_some(thinking_delta),
            &json!({"tool_run_id": event.id}),
            event.timestamp,
        )
        .map(Some)
    }

    fn output_limit_confirmation(
        &self,
        event: &Event,
        output_limited: bool,
    ) -> Result<Option<NodeEventV1>, AgentEventProjectionError> {
        if !output_limited
            || matches!(
                self.context.output_limit_behavior,
                OutputLimitBehavior::Suppressed
            )
        {
            return Ok(None);
        }
        self.event(
            "agent_requires_confirmation",
            &Value::String("Continue".to_owned()),
            None,
            &json!({
                "tool_run_id": event.id,
                "thread_id": self.context.thread_id,
                "finish_reason": "length",
            }),
            event.timestamp,
        )
        .map(Some)
    }

    fn project_tool_starts(
        &mut self,
        event: &Event,
        calls: &[adk_rust::ToolCallView<'_>],
        batch: &mut ProjectedAgentEventBatch,
    ) -> Result<(), AgentEventProjectionError> {
        if calls.len() > MAX_TOOL_CALLS_PER_MODEL_TURN || !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let timestamp = event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        let mut ids = HashSet::with_capacity(calls.len());
        let mut pending = Vec::with_capacity(calls.len());
        let pipeline_node_name = pipeline_node_name(event)?.map(str::to_owned);
        for (index, call) in calls.iter().enumerate() {
            let id = call
                .call_id
                .filter(|value| valid_tool_identity(value))
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            if !ids.insert(id) || !valid_tool_identity(call.name) {
                return Err(AgentEventProjectionError::invalid_state());
            }
            validate_tool_event_value(call.args)?;
            let public_arguments = if self.sensitive_tools.policy_for(call.name).is_some() {
                mask_sensitive_arguments(call.args, 0)?
            } else {
                call.args.clone()
            };
            let active = ActiveToolCall {
                name: call.name.to_owned(),
                arguments: call.args.clone(),
                public_arguments,
                timestamp_start: timestamp.clone(),
                application: self.application_tools.get(call.name).cloned(),
                sibling_ordinal: self.application_tools.get(call.name).map(|_| index + 1),
                pipeline_node_name: pipeline_node_name.clone(),
            };
            let entry = tool_entry(id, &active, None, None, None, None);
            batch.push(self.event(
                "agent_tool_start",
                &Value::Null,
                None,
                &entry,
                event.timestamp,
            )?)?;
            batch.push(self.tool_partial_event(id, &entry, event.timestamp)?)?;
            pending.push((id.to_owned(), active));
        }
        self.active_tools.extend(pending);
        Ok(())
    }

    fn project_tool_results(
        &mut self,
        event: &Event,
        results: &[adk_rust::ToolResultView<'_>],
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if results.len() > MAX_TOOL_CALLS_PER_MODEL_TURN
            || !matches!(self.state, ProjectionState::Complete(_))
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let timestamp_finish = event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        let mut completed = Vec::with_capacity(results.len());
        let mut ids = HashSet::with_capacity(results.len());
        let mut batch = ProjectedAgentEventBatch::new();
        for result in results {
            let id = result
                .call_id
                .filter(|value| valid_tool_identity(value))
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            if !ids.insert(id) {
                return Err(AgentEventProjectionError::invalid_state());
            }
            let active = self
                .active_tools
                .get(id)
                .filter(|active| active.name == result.name)
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            let serialized = serde_json::to_string(result.response)
                .map_err(|_| AgentEventProjectionError::invalid_state())?;
            let error = result
                .response
                .as_object()
                .and_then(|value| value.get("error"))
                .and_then(Value::as_str);
            if serialized.len() > MAX_TOOL_EVENT_VALUE_BYTES {
                self.project_tool_result_chunks(
                    &mut batch,
                    event,
                    id,
                    active,
                    &serialized,
                    error.is_some(),
                )?;
                completed.push(id.to_owned());
                continue;
            }
            let output = error
                .is_none()
                .then(|| serde_json::to_string(result.response))
                .transpose()
                .map_err(|_| AgentEventProjectionError::invalid_state())?;
            let finish_reason = if error.is_some() { "error" } else { "stop" };
            let entry = tool_entry(
                id,
                active,
                Some(timestamp_finish.as_str()),
                Some(finish_reason),
                output.as_deref(),
                error,
            );
            batch.push(self.event(
                if error.is_some() {
                    "agent_tool_error"
                } else {
                    "agent_tool_end"
                },
                &error.map_or(Value::Null, |value| Value::String(value.to_owned())),
                None,
                &entry,
                event.timestamp,
            )?)?;
            batch.push(self.tool_partial_event(id, &entry, event.timestamp)?)?;
            completed.push(id.to_owned());
        }
        for id in completed {
            self.active_tools.remove(&id);
        }
        Ok(batch)
    }

    fn project_tool_result_chunks(
        &self,
        batch: &mut ProjectedAgentEventBatch,
        event: &Event,
        id: &str,
        active: &ActiveToolCall,
        serialized: &str,
        is_error: bool,
    ) -> Result<(), AgentEventProjectionError> {
        let timestamp_finish = event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        if serialized.len() > MAX_TOOL_RESULT_BYTES {
            return Err(AgentEventProjectionError {
                code: AgentEventProjectionErrorCode::ResourceExhausted,
                protocol: None,
            });
        }
        let hash = ring::digest::digest(&ring::digest::SHA256, serialized.as_bytes());
        let mut encoded_hash = String::with_capacity(64);
        for byte in hash.as_ref() {
            const HEX: &[u8; 16] = b"0123456789abcdef";
            encoded_hash.push(char::from(HEX[usize::from(byte >> 4)]));
            encoded_hash.push(char::from(HEX[usize::from(byte & 15)]));
        }
        let hash = encoded_hash;
        let mut offset = 0;
        while offset < serialized.len() {
            let mut end = (offset + TOOL_RESULT_CHUNK_BYTES).min(serialized.len());
            while !serialized.is_char_boundary(end) {
                end -= 1;
            }
            let final_chunk = end == serialized.len();
            let final_error = final_chunk && is_error;
            // The complete error remains in tool_output. Keep lifecycle metadata bounded.
            let error = final_error.then_some("Tool execution failed. See tool output.");
            let mut entry = tool_entry(
                id,
                active,
                final_chunk.then_some(timestamp_finish.as_str()),
                final_chunk.then_some(if is_error { "error" } else { "stop" }),
                Some(&serialized[offset..end]),
                error,
            );
            let object = entry
                .as_object_mut()
                .ok_or_else(AgentEventProjectionError::invalid_state)?;
            object.remove("tool_inputs");
            object.insert(
                "tool_output_chunk_v1".to_owned(),
                json!({
                    "offset_bytes": offset, "total_bytes": serialized.len(),
                    "sha256": hash, "final": final_chunk,
                }),
            );
            // Persist and validate each chunk before its browser lifecycle frame.
            batch.push(self.tool_partial_event(id, &entry, event.timestamp)?)?;
            batch.push(self.event(
                if final_error {
                    "agent_tool_error"
                } else {
                    "agent_tool_end"
                },
                &error.map_or(Value::Null, |value| Value::String(value.to_owned())),
                None,
                &entry,
                event.timestamp,
            )?)?;
            offset = end;
        }
        Ok(())
    }

    fn tool_partial_event(
        &self,
        id: &str,
        entry: &Value,
        occurred_at: DateTime<Utc>,
    ) -> Result<NodeEventV1, AgentEventProjectionError> {
        let tool_calls = serde_json::Map::from_iter([(id.to_owned(), entry.clone())]);
        self.event(
            "partial_message",
            &Value::Null,
            None,
            &json!({
                "project_id": self.context.project_id,
                "chat_project_id": self.context.chat_project_id,
                "thread_id": self.context.thread_id,
                "thinking_steps": [],
                "tool_calls": tool_calls,
                "additional_response_meta": {},
                "invoked_skills": self.context.applied_skills,
            }),
            occurred_at,
        )
    }

    fn model_start_event(
        &self,
        event: &Event,
        timestamp: &str,
    ) -> Result<NodeEventV1, AgentEventProjectionError> {
        let pipeline_node_name = pipeline_node_name(event)?;
        let tool_name = pipeline_node_name.unwrap_or("Thinking step");
        let metadata = pipeline_node_name.map_or_else(
            || json!({"ls_model_name": self.context.model_name}),
            |name| {
                json!({
                    "ls_model_name": self.context.model_name,
                    "langgraph_node": name,
                    "original_name": name,
                })
            },
        );
        self.event(
            "agent_llm_start",
            &Value::Null,
            None,
            &json!({
                "tool_name": tool_name,
                "tool_run_id": event.id,
                "metadata": metadata,
                "timestamp_start": timestamp,
                "model_name": self.context.model_name,
            }),
            event.timestamp,
        )
    }

    /// Emit the selected completed result only after ADK reaches EOS.
    pub(crate) fn finish_after_eos(
        &mut self,
        completion: CompletedAgentBrowserOutput,
        occurred_at: DateTime<Utc>,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let printer_checkpoint = matches!(self.state, ProjectionState::PrinterComplete);
        let output_limited = match &self.state {
            ProjectionState::Complete(turn) => turn.output_limited,
            ProjectionState::PrinterComplete => false,
            _ => return Err(AgentEventProjectionError::invalid_state()),
        };
        if !self.active_tools.is_empty() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_public_text(&completion.thread_id)?;
        let CompletedAgentBrowserOutput {
            content,
            thread_id,
            mut execution_finished,
            context_info,
        } = completion;
        execution_finished &= !printer_checkpoint;
        let content = self.pipeline_result.take().unwrap_or(content);
        let content = self
            .continuation_overlap
            .as_ref()
            .map_or(content.clone(), |overlap| overlap.completed(&content));
        let mut batch = ProjectedAgentEventBatch::new();
        let response = Value::String(content);
        if execution_finished {
            batch.push(self.event(
                "pipeline_finish",
                &response,
                None,
                &json!({
                    "finish_reason": "finished",
                    "next_step": "END",
                    "thread_id": thread_id,
                }),
                occurred_at,
            )?)?;
        }
        batch.push(self.event(
            "agent_response",
            &response,
            None,
            &json!({
                "finish_reason": if output_limited { "length" } else { "stop" },
                "thread_id": thread_id,
            }),
            occurred_at,
        )?)?;
        batch.push(self.event(
            "full_message",
            &response,
            None,
            &json!({
                "project_id": self.context.project_id,
                "chat_project_id": self.context.chat_project_id,
                "application_details": self.context.application_details,
                "thread_id": thread_id,
                "llm_start_timestamp": Value::Null,
                "additional_response_meta": {},
                "files_modified": [],
                "image_thumbnails": {},
                "index_statuses": {},
                "chat_history_tokens_input": 0,
                "llm_response_tokens_output": 0,
                "should_continue": self.context.should_continue,
                "output_limit_reached": output_limited,
                "hitl_resume": self.context.hitl_resume,
                "parallel_reconcile": self.context.parallel_reconcile,
                "context_info": context_info,
                "invoked_skills": self.context.applied_skills,
            }),
            occurred_at,
        )?)?;
        self.state = ProjectionState::Finished;
        Ok(batch)
    }

    fn bind_invocation_id(&mut self, event: &Event) {
        if self.invocation_id.is_none() {
            self.invocation_id = Some(event.invocation_id.clone());
        }
    }

    fn event(
        &self,
        event_type: &str,
        content: &Value,
        thinking: Option<String>,
        response_metadata: &Value,
        occurred_at: DateTime<Utc>,
    ) -> Result<NodeEventV1, AgentEventProjectionError> {
        let event = NodeEventV1 {
            r#type: event_type.to_owned(),
            stream_id: Some(self.context.stream_id.clone()),
            message_id: Some(self.context.message_id.clone()),
            question_id: None,
            content: serde_json::to_vec(content).map_err(|_| {
                AgentEventProjectionError::output(ProtocolError::InvalidInput(
                    "the projected agent event content is malformed",
                ))
            })?,
            thinking,
            response_metadata: serde_json::to_vec(response_metadata).map_err(|_| {
                AgentEventProjectionError::output(ProtocolError::InvalidInput(
                    "the projected agent event metadata is malformed",
                ))
            })?,
            references: b"[]".to_vec(),
            sio_event: Some(self.context.sio_event.clone()),
            created_at: Some(occurred_at.to_rfc3339_opts(SecondsFormat::AutoSi, false)),
            parent_message_id: None,
            agent_name: None,
            execution_generation: Some(self.context.execution_generation.clone()),
        };
        encode_current_node_event_json(&event).map_err(AgentEventProjectionError::output)?;
        Ok(event)
    }
}

fn validate_context(
    context: &AgentEventProjectionContext,
) -> Result<(), AgentEventProjectionError> {
    let required = [
        context.stream_id.as_str(),
        context.message_id.as_str(),
        context.execution_generation.as_str(),
        context.sio_event.as_str(),
        context.thread_id.as_str(),
        context.root_agent_name.as_str(),
        context.model_name.as_str(),
    ];
    if required.iter().any(|value| {
        value.is_empty()
            || value.len() > MAX_CONTEXT_TEXT_BYTES
            || value
                .bytes()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    }) {
        return Err(AgentEventProjectionError::invalid_state());
    }
    if context
        .graph_checkpoint_thread_id
        .as_deref()
        .is_some_and(|value| validate_public_text(value).is_err())
        || context
            .continuation_prefix
            .as_deref()
            .is_some_and(|value| value.len() > 64 * 1_024 || value.contains('\0'))
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn validate_public_text(value: &str) -> Result<(), AgentEventProjectionError> {
    if value.is_empty()
        || value.len() > MAX_CONTEXT_TEXT_BYTES
        || value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn validate_adk_event(
    event: &Event,
    root_agent_name: &str,
) -> Result<(), AgentEventProjectionError> {
    let confirmation_decision_is_tool_result =
        event.actions.tool_confirmation_decision.is_some_and(|_| {
            event.content().is_some_and(|content| {
                content.parts.len() == 1
                    && matches!(content.parts.first(), Some(Part::FunctionResponse { .. }))
            })
        });
    if event.author != root_agent_name
        || !valid_application_branch(&event.branch)
        || event.llm_response.interrupted
        || event.llm_response.error_code.is_some()
        || event.llm_response.error_message.is_some()
        || event.llm_response.citation_metadata.is_some()
        || !event.long_running_tool_ids.is_empty()
        || event.tool_progress_stream().is_some()
        || event.actions.transfer_to_agent.is_some()
        || event.actions.escalate
        || event.actions.tool_confirmation.is_some()
        || (event.actions.tool_confirmation_decision.is_some()
            && !confirmation_decision_is_tool_result)
        || event.actions.compaction.is_some()
        || event.actions.route.is_some()
        || !event.actions.artifact_delta.is_empty()
    {
        if event.llm_response.error_code.is_some() || event.llm_response.error_message.is_some() {
            return Err(AgentEventProjectionError::provider_failure());
        }
        return Err(AgentEventProjectionError::unsupported());
    }
    if event.content().is_some()
        && (!event.actions.state_delta.is_empty() || event.actions.skip_summarization)
    {
        return Err(AgentEventProjectionError::unsupported());
    }
    Ok(())
}

fn pipeline_node_name(event: &Event) -> Result<Option<&str>, AgentEventProjectionError> {
    event
        .provider_metadata
        .get(PIPELINE_NODE_METADATA_KEY)
        .map(|value| {
            if valid_pipeline_node_identity(value) {
                Ok(value.as_str())
            } else {
                Err(AgentEventProjectionError::invalid_state())
            }
        })
        .transpose()
}

fn model_step_presentation(event: &Event) -> Result<(Value, Value), AgentEventProjectionError> {
    Ok(pipeline_node_name(event)?.map_or_else(
        || (Value::Null, json!({})),
        |name| {
            (
                json!(name),
                json!({"langgraph_node": name, "original_name": name}),
            )
        },
    ))
}

fn validate_confirmation_event(
    event: &Event,
    root_agent_name: &str,
) -> Result<(), AgentEventProjectionError> {
    if event.llm_response.error_code.is_some() || event.llm_response.error_message.is_some() {
        return Err(AgentEventProjectionError::provider_failure());
    }
    if event.author != root_agent_name
        || !valid_application_branch(&event.branch)
        || !event.llm_response.interrupted
        || !event.llm_response.turn_complete
        || event.llm_response.citation_metadata.is_some()
        || !event.long_running_tool_ids.is_empty()
        || event.tool_progress_stream().is_some()
        || event.actions.transfer_to_agent.is_some()
        || event.actions.escalate
        || event.actions.tool_confirmation_decision.is_some()
        || event.actions.compaction.is_some()
        || event.actions.route.is_some()
        || !event.actions.artifact_delta.is_empty()
        || !event.actions.state_delta.is_empty()
        || event.actions.skip_summarization
    {
        return Err(AgentEventProjectionError::unsupported());
    }
    Ok(())
}

fn valid_application_branch(value: &str) -> bool {
    if value.is_empty() || value == APPLICATION_BRANCH_ROOT {
        return true;
    }
    value.len() <= MAX_CONTEXT_TEXT_BYTES
        && value
            .strip_prefix(APPLICATION_BRANCH_ROOT)
            .and_then(|suffix| suffix.strip_prefix('.'))
            .is_some_and(|suffix| {
                suffix.split('.').all(|segment| {
                    segment.strip_prefix("application_").is_some_and(|ordinal| {
                        !ordinal.is_empty() && ordinal.bytes().all(|byte| byte.is_ascii_digit())
                    })
                })
            })
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PipelineHitlData {
    schema_revision: String,
    #[serde(rename = "type")]
    interrupt_type: String,
    interaction_type: String,
    history_contract_version: u8,
    guardrail_type: String,
    node_name: String,
    message: String,
    available_actions: Vec<String>,
    routes: BTreeMap<String, String>,
    edit_state_key: Option<String>,
    definition_digest: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NestedPipelineInterruptData {
    subgraph: String,
    thread: String,
    checkpoint_id: String,
    #[serde(default, rename = "elitea_event_scope")]
    event_scope: Option<PipelineNodeEventScope>,
    data: Value,
}

pub(crate) struct NestedPipelineCheckpoint {
    node_name: String,
    thread_id: String,
    checkpoint_id: String,
}

impl NestedPipelineCheckpoint {
    #[must_use]
    pub(crate) fn node_name(&self) -> &str {
        &self.node_name
    }

    #[must_use]
    pub(crate) fn thread_id(&self) -> &str {
        &self.thread_id
    }

    #[must_use]
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }
}

impl PipelineHitlData {
    fn validate(&self, graph_message: &str) -> Result<(), AgentEventProjectionError> {
        if self.schema_revision != PIPELINE_HITL_SCHEMA
            || self.interrupt_type != "hitl"
            || self.interaction_type != PIPELINE_HITL_INTERACTION_TYPE
            || self.history_contract_version != PIPELINE_HITL_HISTORY_CONTRACT_VERSION
            || self.guardrail_type != "pipeline_hitl"
            || self.message != graph_message
            || !valid_pipeline_node_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || self.routes.is_empty()
            || self.routes.len() > 3
            || self.available_actions.is_empty()
            || self.available_actions.len() > 3
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let mut seen = HashSet::new();
        for action in &self.available_actions {
            if !matches!(action.as_str(), "approve" | "reject" | "edit")
                || !seen.insert(action.as_str())
                || !self.routes.contains_key(action)
            {
                return Err(AgentEventProjectionError::invalid_state());
            }
            if action == "edit"
                && (self.edit_state_key.is_none()
                    || self
                        .routes
                        .get(action)
                        .is_some_and(|target| target == "END"))
            {
                return Err(AgentEventProjectionError::invalid_state());
            }
        }
        for (action, target) in &self.routes {
            if !matches!(action.as_str(), "approve" | "reject" | "edit")
                || (target != "END" && !valid_pipeline_node_identity(target))
            {
                return Err(AgentEventProjectionError::invalid_state());
            }
        }
        if self
            .edit_state_key
            .as_deref()
            .is_some_and(|key| !valid_pipeline_state_key(key))
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        Ok(())
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PipelineToolHitlData {
    schema_revision: String,
    #[serde(rename = "type")]
    interrupt_type: String,
    guardrail_type: String,
    node_name: String,
    message: String,
    available_actions: Vec<String>,
    routes: BTreeMap<String, String>,
    definition_digest: String,
    tool_call_id: String,
    tool_name: String,
    toolkit_name: String,
    toolkit_type: String,
    action_label: String,
    tool_args: Value,
    argument_digest: String,
    policy_message: String,
    #[serde(default)]
    llm_replay: Option<PipelineLlmReplayEnvelope>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PipelineClarifyingData {
    schema_revision: String,
    #[serde(rename = "type")]
    interrupt_type: String,
    guardrail_type: String,
    node_name: String,
    message: String,
    questions: Value,
    available_actions: Vec<String>,
    routes: BTreeMap<String, String>,
    definition_digest: String,
    tool_call_id: String,
    tool_name: String,
    tool_args: Value,
    argument_digest: String,
    llm_replay: PipelineLlmReplayEnvelope,
}

impl PipelineClarifyingData {
    fn validate(&self, graph_message: &str) -> Result<(), AgentEventProjectionError> {
        let request = super::internal_tools::AskUserRequest::from_arguments(&self.tool_args)
            .map_err(|_| AgentEventProjectionError::invalid_state())?;
        if self.schema_revision != PIPELINE_CLARIFYING_SCHEMA
            || self.interrupt_type != "hitl"
            || self.guardrail_type != ASK_USER_GUARDRAIL_TYPE
            || self.message != graph_message
            || self.message != request.message()
            || self.questions != request.questions_value()
            || self.available_actions != [ASK_USER_ANSWER_ACTION]
            || !self.routes.is_empty()
            || !valid_pipeline_node_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || !valid_sha256_label(&self.argument_digest)
            || !valid_tool_identity(&self.tool_call_id)
            || self.tool_name != ASK_USER_TOOL_NAME
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        self.llm_replay
            .validate()
            .map_err(|_| AgentEventProjectionError::invalid_state())?;
        if self.llm_replay.definition_digest() != self.definition_digest {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_pipeline_hitl_message(&self.message)
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PipelineMcpAuthData {
    schema_revision: String,
    #[serde(rename = "type")]
    interrupt_type: String,
    guardrail_type: String,
    node_name: String,
    message: String,
    available_actions: Vec<String>,
    routes: BTreeMap<String, String>,
    definition_digest: String,
    tool_call_id: String,
    tool_name: String,
    toolkit_name: String,
    toolkit_type: String,
    tool_args: Value,
    argument_digest: String,
    server_url: String,
    resource_metadata_url: Option<String>,
    www_authenticate: Option<String>,
    #[serde(default)]
    resource_metadata: Option<Value>,
    #[serde(default)]
    llm_replay: Option<PipelineLlmReplayEnvelope>,
}

impl PipelineMcpAuthData {
    fn authorization_servers(&self) -> Option<&[Value]> {
        self.resource_metadata
            .as_ref()
            .and_then(Value::as_object)
            .and_then(|metadata| metadata.get("authorization_servers"))
            .and_then(Value::as_array)
            .map(Vec::as_slice)
    }

    fn validate(&self, graph_message: &str) -> Result<(), AgentEventProjectionError> {
        if self.schema_revision != PIPELINE_MCP_AUTH_SCHEMA
            || self.interrupt_type != "hitl"
            || self.guardrail_type != "mcp_auth"
            || self.message != graph_message
            || self.available_actions != ["authorize", "skip"]
            || !self.routes.is_empty()
            || !valid_pipeline_node_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || !valid_sha256_label(&self.argument_digest)
            || !valid_tool_identity(&self.tool_call_id)
            || !valid_tool_identity(&self.tool_name)
            || !valid_tool_identity(&self.toolkit_name)
            || !valid_tool_identity(&self.toolkit_type)
            || self.tool_args != json!({})
            || !valid_https_public_metadata_url(&self.server_url)
            || self.resource_metadata_url.as_deref().is_some_and(|value| {
                !valid_https_public_metadata_url(value)
                    || (self.toolkit_type == "mcp" && !same_origin(value, &self.server_url))
            })
            || self.www_authenticate.as_deref().is_some_and(|value| {
                value.is_empty() || value.len() > 16 * 1024 || value.chars().any(char::is_control)
            })
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let requirement = DelegatedAuthorizationRequirement::new(
            self.toolkit_name.clone(),
            self.toolkit_type.clone(),
            self.server_url.clone(),
            self.resource_metadata_url.clone(),
            self.www_authenticate.clone(),
        )
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let requirement = match &self.resource_metadata {
            Some(metadata) => requirement.with_resource_metadata(metadata.clone()),
            None => Some(requirement),
        }
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
        if requirement.resource_metadata() != self.resource_metadata.as_ref() {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if let Some(replay) = &self.llm_replay {
            replay
                .validate()
                .map_err(|_| AgentEventProjectionError::invalid_state())?;
            if replay.definition_digest() != self.definition_digest {
                return Err(AgentEventProjectionError::invalid_state());
            }
        }
        validate_pipeline_hitl_message(&self.message)
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PipelineApplicationHitlData {
    schema_revision: String,
    #[serde(rename = "type")]
    interrupt_type: String,
    guardrail_type: String,
    node_name: String,
    message: String,
    definition_digest: String,
    application_call_id: String,
    application_tool_name: String,
    interrupt_ids: Vec<String>,
}

impl PipelineApplicationHitlData {
    fn validate(&self, graph_message: &str) -> Result<(), AgentEventProjectionError> {
        if self.schema_revision != PIPELINE_APPLICATION_HITL_SCHEMA
            || self.interrupt_type != "hitl_checkpoint"
            || self.guardrail_type != "application_sensitive_tool"
            || self.message != graph_message
            || !valid_pipeline_node_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || !valid_tool_identity(&self.application_call_id)
            || !valid_tool_identity(&self.application_tool_name)
            || self.interrupt_ids.is_empty()
            || self.interrupt_ids.len() > 16
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let mut ids = HashSet::with_capacity(self.interrupt_ids.len());
        if self
            .interrupt_ids
            .iter()
            .any(|identity| !valid_tool_identity(identity) || !ids.insert(identity.as_str()))
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        Ok(())
    }
}

impl PipelineToolHitlData {
    fn validate(&self, graph_message: &str) -> Result<(), AgentEventProjectionError> {
        if self.schema_revision != PIPELINE_TOOL_HITL_SCHEMA
            || self.interrupt_type != "hitl"
            || self.guardrail_type != "sensitive_tool"
            || self.message != graph_message
            || self.policy_message != self.message
            || !valid_pipeline_node_identity(&self.node_name)
            || !valid_sha256_label(&self.definition_digest)
            || !valid_sha256_label(&self.argument_digest)
            || !valid_tool_identity(&self.tool_call_id)
            || !valid_tool_identity(&self.tool_name)
            || !valid_tool_identity(&self.toolkit_name)
            || !valid_tool_identity(&self.toolkit_type)
            || !valid_tool_identity(&self.action_label)
            || !self.routes.is_empty()
            || self.available_actions != ["approve", "reject", "block_with_comment"]
            || self.message.is_empty()
            || self.message.len() > MAX_PIPELINE_HITL_MESSAGE_BYTES
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        validate_tool_event_value(&self.tool_args)?;
        if mask_sensitive_arguments(&self.tool_args, 0)? != self.tool_args {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if let Some(replay) = &self.llm_replay {
            replay
                .validate()
                .map_err(|_| AgentEventProjectionError::invalid_state())?;
            if replay.definition_digest() != self.definition_digest {
                return Err(AgentEventProjectionError::invalid_state());
            }
        }
        Ok(())
    }
}

/// Exact private graph interrupt identity reconstructed from one persisted ADK
/// event. Browser projection exposes only `interrupt_id`; checkpoint routing
/// stays on the worker side.
pub(crate) struct PipelineHitlEventBinding {
    interrupt_id: String,
    call_digest: String,
    checkpoint_id: String,
    nested_checkpoints: Vec<NestedPipelineCheckpoint>,
    data: PipelineHitlData,
}

/// Exact private identity for a graph Toolkit-node confirmation.
pub(crate) struct PipelineToolHitlEventBinding {
    interrupt_id: String,
    call_digest: String,
    checkpoint_id: String,
    nested_checkpoints: Vec<NestedPipelineCheckpoint>,
    data: PipelineToolHitlData,
}

pub(crate) struct PipelineClarifyingEventBinding {
    interrupt_id: String,
    call_digest: String,
    checkpoint_id: String,
    nested_checkpoints: Vec<NestedPipelineCheckpoint>,
    data: PipelineClarifyingData,
}

impl PipelineClarifyingEventBinding {
    pub(crate) fn interrupt_id(&self) -> &str {
        &self.interrupt_id
    }

    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    pub(crate) fn pending_node_name(&self) -> &str {
        self.nested_checkpoints.first().map_or(
            self.data.node_name.as_str(),
            NestedPipelineCheckpoint::node_name,
        )
    }

    pub(crate) fn nested_checkpoints(&self) -> &[NestedPipelineCheckpoint] {
        &self.nested_checkpoints
    }

    pub(crate) fn node_name(&self) -> &str {
        &self.data.node_name
    }

    pub(crate) fn definition_digest(&self) -> &str {
        &self.data.definition_digest
    }

    pub(crate) fn tool_call_id(&self) -> &str {
        &self.data.tool_call_id
    }

    pub(crate) fn tool_name(&self) -> &str {
        &self.data.tool_name
    }

    pub(crate) fn argument_digest(&self) -> &str {
        &self.data.argument_digest
    }

    pub(crate) const fn llm_replay(&self) -> &PipelineLlmReplayEnvelope {
        &self.data.llm_replay
    }
}

pub(crate) struct PipelineMcpAuthEventBinding {
    interrupt_id: String,
    call_digest: String,
    checkpoint_id: String,
    nested_checkpoints: Vec<NestedPipelineCheckpoint>,
    data: PipelineMcpAuthData,
}

impl PipelineMcpAuthEventBinding {
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    pub(crate) fn pending_node_name(&self) -> &str {
        self.nested_checkpoints.first().map_or(
            self.data.node_name.as_str(),
            NestedPipelineCheckpoint::node_name,
        )
    }

    pub(crate) fn nested_checkpoints(&self) -> &[NestedPipelineCheckpoint] {
        &self.nested_checkpoints
    }

    pub(crate) fn node_name(&self) -> &str {
        &self.data.node_name
    }

    pub(crate) fn definition_digest(&self) -> &str {
        &self.data.definition_digest
    }

    pub(crate) fn tool_call_id(&self) -> &str {
        &self.data.tool_call_id
    }

    pub(crate) fn argument_digest(&self) -> &str {
        &self.data.argument_digest
    }

    pub(crate) fn server_url(&self) -> &str {
        &self.data.server_url
    }

    pub(crate) fn tool_name(&self) -> &str {
        &self.data.tool_name
    }

    pub(crate) fn toolkit_name(&self) -> &str {
        &self.data.toolkit_name
    }

    pub(crate) fn toolkit_type(&self) -> &str {
        &self.data.toolkit_type
    }

    pub(crate) fn resource_metadata_url(&self) -> Option<&str> {
        self.data.resource_metadata_url.as_deref()
    }

    pub(crate) fn www_authenticate(&self) -> Option<&str> {
        self.data.www_authenticate.as_deref()
    }

    pub(crate) fn resource_metadata(&self) -> Option<&Value> {
        self.data.resource_metadata.as_ref()
    }

    pub(crate) fn llm_replay(&self) -> Option<&PipelineLlmReplayEnvelope> {
        self.data.llm_replay.as_ref()
    }
}

pub(crate) struct PipelineApplicationHitlEventBinding {
    checkpoint_id: String,
    data: PipelineApplicationHitlData,
}

impl PipelineApplicationHitlEventBinding {
    #[must_use]
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    #[must_use]
    pub(crate) fn node_name(&self) -> &str {
        &self.data.node_name
    }

    #[must_use]
    pub(crate) fn definition_digest(&self) -> &str {
        &self.data.definition_digest
    }

    #[must_use]
    pub(crate) fn application_call_id(&self) -> &str {
        &self.data.application_call_id
    }

    #[must_use]
    pub(crate) fn application_tool_name(&self) -> &str {
        &self.data.application_tool_name
    }

    #[must_use]
    pub(crate) fn interrupt_ids(&self) -> &[String] {
        &self.data.interrupt_ids
    }
}

/// Exact private identity for one compiler-owned static Printer checkpoint.
pub(crate) struct PipelinePrinterEventBinding {
    checkpoint_id: String,
    output: String,
    metadata: PrinterPauseMetadata,
}

impl PipelinePrinterEventBinding {
    #[must_use]
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    #[must_use]
    pub(crate) fn output(&self) -> &str {
        &self.output
    }

    #[must_use]
    pub(crate) fn node_name(&self) -> &str {
        &self.metadata.node_name
    }

    #[must_use]
    pub(crate) fn reset_node_name(&self) -> &str {
        &self.metadata.reset_node_name
    }

    #[must_use]
    pub(crate) fn definition_digest(&self) -> &str {
        &self.metadata.definition_digest
    }

    #[must_use]
    pub(crate) fn node_digest(&self) -> &str {
        &self.metadata.node_digest
    }
}

impl PipelineToolHitlEventBinding {
    #[must_use]
    pub(crate) fn interrupt_id(&self) -> &str {
        &self.interrupt_id
    }

    #[must_use]
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    #[must_use]
    pub(crate) fn pending_node_name(&self) -> &str {
        self.nested_checkpoints.first().map_or(
            self.data.node_name.as_str(),
            NestedPipelineCheckpoint::node_name,
        )
    }

    #[must_use]
    pub(crate) fn nested_checkpoints(&self) -> &[NestedPipelineCheckpoint] {
        &self.nested_checkpoints
    }

    #[must_use]
    pub(crate) fn node_name(&self) -> &str {
        &self.data.node_name
    }

    #[must_use]
    pub(crate) fn definition_digest(&self) -> &str {
        &self.data.definition_digest
    }

    #[must_use]
    pub(crate) fn argument_digest(&self) -> &str {
        &self.data.argument_digest
    }

    #[must_use]
    pub(crate) fn tool_call_id(&self) -> &str {
        &self.data.tool_call_id
    }

    #[must_use]
    pub(crate) fn tool_name(&self) -> &str {
        &self.data.tool_name
    }

    #[must_use]
    pub(crate) fn toolkit_name(&self) -> &str {
        &self.data.toolkit_name
    }

    #[must_use]
    pub(crate) fn toolkit_type(&self) -> &str {
        &self.data.toolkit_type
    }

    #[must_use]
    pub(crate) fn action_label(&self) -> &str {
        &self.data.action_label
    }

    #[must_use]
    pub(crate) fn llm_replay(&self) -> Option<&PipelineLlmReplayEnvelope> {
        self.data.llm_replay.as_ref()
    }
}

impl PipelineHitlEventBinding {
    #[must_use]
    pub(crate) fn interrupt_id(&self) -> &str {
        &self.interrupt_id
    }

    #[must_use]
    pub(crate) fn checkpoint_id(&self) -> &str {
        &self.checkpoint_id
    }

    #[must_use]
    pub(crate) fn pending_node_name(&self) -> &str {
        self.nested_checkpoints.first().map_or(
            self.data.node_name.as_str(),
            NestedPipelineCheckpoint::node_name,
        )
    }

    #[must_use]
    pub(crate) fn nested_checkpoints(&self) -> &[NestedPipelineCheckpoint] {
        &self.nested_checkpoints
    }

    #[must_use]
    pub(crate) fn node_name(&self) -> &str {
        &self.data.node_name
    }

    #[must_use]
    pub(crate) fn definition_digest(&self) -> &str {
        &self.data.definition_digest
    }

    #[must_use]
    pub(crate) fn allows(&self, action: &str) -> bool {
        self.data
            .available_actions
            .iter()
            .any(|candidate| candidate == action)
    }
}

/// Validate and bind a persisted graph interrupt without projecting it again.
///
/// This is the restart boundary used by the pipeline resume adapter. It shares
/// the exact parser and digest with browser projection so those paths cannot
/// disagree about which checkpoint a public interrupt authorizes.
pub(crate) fn pipeline_hitl_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineHitlEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_hitl_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

/// Validate and bind a persisted direct Toolkit confirmation interrupt.
pub(crate) fn pipeline_tool_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineToolHitlEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_tool_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

pub(crate) fn pipeline_clarifying_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineClarifyingEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_clarifying_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

pub(crate) fn pipeline_mcp_auth_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineMcpAuthEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_mcp_auth_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

/// Validate and bind an internal graph checkpoint for descendant Application
/// confirmations. The child confirmation events remain the only public cards.
pub(crate) fn pipeline_application_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineApplicationHitlEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_application_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

/// Validate and bind one persisted static Printer interruption.
pub(crate) fn pipeline_printer_event_binding(
    event: &Event,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelinePrinterEventBinding, AgentEventProjectionError> {
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    pipeline_printer_event_binding_from_payload(event, &payload, root_agent_name, thread_id)
}

fn pipeline_printer_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelinePrinterEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event_with_metadata_count(event, root_agent_name, 2)?;
    let metadata = serde_json::from_str::<PrinterPauseMetadata>(
        event
            .provider_metadata
            .get(PRINTER_PAUSE_METADATA_KEY)
            .ok_or_else(AgentEventProjectionError::invalid_state)?,
    )
    .map_err(|_| AgentEventProjectionError::invalid_state())?;
    if !metadata.validate()
        || payload.kind != "after"
        || payload.node.as_deref() != Some(metadata.node_name.as_str())
        || payload.message.is_some()
        || payload.data.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
        || event
            .provider_metadata
            .keys()
            .any(|key| key != INTERRUPT_METADATA_KEY && key != PRINTER_PAUSE_METADATA_KEY)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let content = event
        .content()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let [Part::Text { text }] = content.parts.as_slice() else {
        return Err(AgentEventProjectionError::invalid_state());
    };
    validate_pipeline_hitl_message(text)?;
    Ok(PipelinePrinterEventBinding {
        checkpoint_id: payload.checkpoint_id.clone(),
        output: text.clone(),
        metadata,
    })
}

fn pipeline_hitl_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineHitlEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event(event, root_agent_name)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let message = payload
        .message
        .as_deref()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    validate_pipeline_interrupt_envelope_message(message)?;
    let (raw_data, nested_checkpoints) = pipeline_interrupt_data(payload, thread_id)?;
    let data = serde_json::from_value::<PipelineHitlData>(raw_data)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    validate_pipeline_interrupt_message(message, &data.message, &nested_checkpoints)?;
    data.validate(&data.message)?;
    let (interrupt_id, call_digest) = pipeline_hitl_identity(&event.invocation_id, payload, &data)?;
    Ok(PipelineHitlEventBinding {
        interrupt_id,
        call_digest,
        checkpoint_id: payload.checkpoint_id.clone(),
        nested_checkpoints,
        data,
    })
}

fn pipeline_tool_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineToolHitlEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event(event, root_agent_name)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let message = payload
        .message
        .as_deref()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    validate_pipeline_interrupt_envelope_message(message)?;
    let (raw_data, nested_checkpoints) = pipeline_interrupt_data(payload, thread_id)?;
    let data = serde_json::from_value::<PipelineToolHitlData>(raw_data)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    validate_pipeline_interrupt_message(message, &data.message, &nested_checkpoints)?;
    data.validate(&data.message)?;
    let (interrupt_id, call_digest) =
        pipeline_tool_hitl_identity(&event.invocation_id, payload, &data)?;
    Ok(PipelineToolHitlEventBinding {
        interrupt_id,
        call_digest,
        checkpoint_id: payload.checkpoint_id.clone(),
        nested_checkpoints,
        data,
    })
}

fn pipeline_clarifying_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineClarifyingEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event(event, root_agent_name)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let message = payload
        .message
        .as_deref()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    validate_pipeline_interrupt_envelope_message(message)?;
    let (raw_data, nested_checkpoints) = pipeline_interrupt_data(payload, thread_id)?;
    let data = serde_json::from_value::<PipelineClarifyingData>(raw_data)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    validate_pipeline_interrupt_message(message, &data.message, &nested_checkpoints)?;
    data.validate(&data.message)?;
    let (interrupt_id, call_digest) =
        pipeline_clarifying_identity(&event.invocation_id, payload, &data)?;
    Ok(PipelineClarifyingEventBinding {
        interrupt_id,
        call_digest,
        checkpoint_id: payload.checkpoint_id.clone(),
        nested_checkpoints,
        data,
    })
}

fn pipeline_mcp_auth_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineMcpAuthEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event(event, root_agent_name)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let message = payload
        .message
        .as_deref()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    validate_pipeline_interrupt_envelope_message(message)?;
    let (raw_data, nested_checkpoints) = pipeline_interrupt_data(payload, thread_id)?;
    let data = serde_json::from_value::<PipelineMcpAuthData>(raw_data)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    validate_pipeline_interrupt_message(message, &data.message, &nested_checkpoints)?;
    data.validate(&data.message)?;
    let (interrupt_id, call_digest) =
        pipeline_mcp_auth_identity(&event.invocation_id, payload, &data)?;
    Ok(PipelineMcpAuthEventBinding {
        interrupt_id,
        call_digest,
        checkpoint_id: payload.checkpoint_id.clone(),
        nested_checkpoints,
        data,
    })
}

fn pipeline_application_event_binding_from_payload(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    thread_id: &str,
) -> Result<PipelineApplicationHitlEventBinding, AgentEventProjectionError> {
    validate_graph_interrupt_event(event, root_agent_name)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let message = payload
        .message
        .as_deref()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    validate_pipeline_interrupt_envelope_message(message)?;
    let (raw_data, nested_checkpoints) = pipeline_interrupt_data(payload, thread_id)?;
    if !nested_checkpoints.is_empty() {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let data = serde_json::from_value::<PipelineApplicationHitlData>(raw_data)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    data.validate(message)?;
    Ok(PipelineApplicationHitlEventBinding {
        checkpoint_id: payload.checkpoint_id.clone(),
        data,
    })
}

struct NestedPipelineInterruptProjection {
    event: Event,
    interrupt_id: String,
    call_digest: String,
}

fn nested_pipeline_interrupt_child_event(
    event: &Event,
    root_agent_name: &str,
    root_thread_id: Option<&str>,
) -> Result<Option<NestedPipelineInterruptProjection>, AgentEventProjectionError> {
    if !event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY) {
        return Ok(None);
    }
    let Some(root_thread_id) = root_thread_id else {
        return Ok(None);
    };
    let payload = GraphInterruptPayload::from_event(event)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let Some(raw) = payload.data.as_ref() else {
        return Ok(None);
    };
    if raw.get("subgraph").is_none() {
        return Ok(None);
    }
    let nested = serde_json::from_value::<NestedPipelineInterruptData>(raw.clone())
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    let Some(scope) = nested.event_scope.as_ref() else {
        return Ok(None);
    };
    validate_graph_interrupt_event(event, root_agent_name)?;
    scope
        .validate()
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    let expected_thread = format!("{root_thread_id}/{}", nested.subgraph);
    let expected_message_prefix = format!("{}: ", nested.subgraph);
    let child_message = payload
        .message
        .as_deref()
        .and_then(|message| message.strip_prefix(&expected_message_prefix))
        .filter(|message| !message.is_empty())
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    if payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != root_thread_id
        || !valid_graph_checkpoint_identity(&payload.checkpoint_id)
        || !valid_pipeline_node_identity(&nested.subgraph)
        || nested.thread != expected_thread
        || !valid_graph_checkpoint_identity(&nested.checkpoint_id)
        || scope.checkpoint_thread_id() != nested.thread
        || pipeline_application_call_node(scope.parent_call_id()) != Some(nested.subgraph.as_str())
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let (raw_data, _) = pipeline_interrupt_data(&payload, root_thread_id)?;
    let guardrail_type = raw_data
        .get("guardrail_type")
        .and_then(Value::as_str)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let Some((interrupt_id, call_digest)) = nested_pipeline_interrupt_identity(
        event,
        &payload,
        root_agent_name,
        root_thread_id,
        guardrail_type,
    )?
    else {
        return Ok(None);
    };
    let child_payload = GraphInterruptPayload {
        kind: payload.kind,
        node: None,
        message: Some(child_message.to_owned()),
        data: Some(nested.data),
        thread_id: nested.thread,
        checkpoint_id: nested.checkpoint_id,
    };
    let mut child_event = event.clone();
    child_event.invocation_id = format!("pipeline-child:{}", scope.parent_call_id());
    scope.agent_name().clone_into(&mut child_event.author);
    child_event.branch.clear();
    child_event.provider_metadata.clear();
    child_event.provider_metadata.insert(
        INTERRUPT_METADATA_KEY.to_owned(),
        child_payload.to_metadata_value(),
    );
    child_event.provider_metadata.insert(
        DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
        event.invocation_id.clone(),
    );
    child_event.provider_metadata.insert(
        DESCENDANT_PARENT_CALL_KEY.to_owned(),
        scope.parent_call_id().to_owned(),
    );
    child_event.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        scope.checkpoint_thread_id().to_owned(),
    );
    Ok(Some(NestedPipelineInterruptProjection {
        event: child_event,
        interrupt_id,
        call_digest,
    }))
}

fn nested_pipeline_interrupt_identity(
    event: &Event,
    payload: &GraphInterruptPayload,
    root_agent_name: &str,
    root_thread_id: &str,
    guardrail_type: &str,
) -> Result<Option<(String, String)>, AgentEventProjectionError> {
    let identity = match guardrail_type {
        "pipeline_hitl" => {
            let binding = pipeline_hitl_event_binding_from_payload(
                event,
                payload,
                root_agent_name,
                root_thread_id,
            )?;
            (binding.interrupt_id, binding.call_digest)
        }
        "sensitive_tool" => {
            let binding = pipeline_tool_event_binding_from_payload(
                event,
                payload,
                root_agent_name,
                root_thread_id,
            )?;
            (binding.interrupt_id, binding.call_digest)
        }
        "mcp_auth" => {
            let binding = pipeline_mcp_auth_event_binding_from_payload(
                event,
                payload,
                root_agent_name,
                root_thread_id,
            )?;
            (binding.interrupt_id, binding.call_digest)
        }
        ASK_USER_GUARDRAIL_TYPE => {
            let binding = pipeline_clarifying_event_binding_from_payload(
                event,
                payload,
                root_agent_name,
                root_thread_id,
            )?;
            (binding.interrupt_id, binding.call_digest)
        }
        _ => return Ok(None),
    };
    Ok(Some(identity))
}

fn pipeline_interrupt_data(
    payload: &GraphInterruptPayload,
    parent_thread_id: &str,
) -> Result<(Value, Vec<NestedPipelineCheckpoint>), AgentEventProjectionError> {
    let mut raw = payload
        .data
        .clone()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let mut expected_parent_thread = parent_thread_id.to_owned();
    let mut checkpoints = Vec::new();
    while raw.get("subgraph").is_some() {
        if checkpoints.len() == MAX_NESTED_PIPELINE_CHECKPOINTS {
            return Err(AgentEventProjectionError::unsupported());
        }
        let nested = serde_json::from_value::<NestedPipelineInterruptData>(raw)
            .map_err(|_| AgentEventProjectionError::invalid_state())?;
        let expected_thread = format!("{expected_parent_thread}/{}", nested.subgraph);
        if !valid_pipeline_node_identity(&nested.subgraph)
            || nested.thread != expected_thread
            || !valid_graph_checkpoint_identity(&nested.checkpoint_id)
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        if let Some(scope) = nested.event_scope.as_ref()
            && (scope.validate().is_err()
                || scope.checkpoint_thread_id() != nested.thread
                || pipeline_application_call_node(scope.parent_call_id())
                    != Some(nested.subgraph.as_str()))
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        expected_parent_thread.clone_from(&nested.thread);
        raw = nested.data;
        checkpoints.push(NestedPipelineCheckpoint {
            node_name: nested.subgraph,
            thread_id: nested.thread,
            checkpoint_id: nested.checkpoint_id,
        });
    }
    Ok((raw, checkpoints))
}

fn validate_pipeline_interrupt_message(
    outer_message: &str,
    inner_message: &str,
    nested: &[NestedPipelineCheckpoint],
) -> Result<(), AgentEventProjectionError> {
    let mut expected = inner_message.to_owned();
    for checkpoint in nested.iter().rev() {
        expected = format!("{}: {expected}", checkpoint.node_name);
    }
    if outer_message != expected {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn validate_graph_interrupt_event(
    event: &Event,
    root_agent_name: &str,
) -> Result<(), AgentEventProjectionError> {
    validate_graph_interrupt_event_with_metadata_count(event, root_agent_name, 1)
}

fn validate_graph_interrupt_event_with_metadata_count(
    event: &Event,
    root_agent_name: &str,
    metadata_count: usize,
) -> Result<(), AgentEventProjectionError> {
    if event.author != root_agent_name
        || (!event.branch.is_empty() && event.branch != APPLICATION_BRANCH_ROOT)
        || event.provider_metadata.len() != metadata_count
        || event.llm_response.partial
        || event.llm_response.turn_complete
        || event.llm_response.interrupted
        || event.llm_response.finish_reason.is_some()
        || event.llm_response.usage_metadata.is_some()
        || event.llm_response.citation_metadata.is_some()
        || event.llm_response.error_code.is_some()
        || event.llm_response.error_message.is_some()
        || event.llm_response.provider_metadata.is_some()
        || event.llm_response.interaction_id.is_some()
        || event.llm_request.is_some()
        || !event.tool_calls().is_empty()
        || !event.tool_results().is_empty()
        || !event.long_running_tool_ids.is_empty()
        || event.tool_progress_stream().is_some()
        || event.actions.transfer_to_agent.is_some()
        || event.actions.escalate
        || event.actions.tool_confirmation.is_some()
        || event.actions.tool_confirmation_decision.is_some()
        || event.actions.compaction.is_some()
        || event.actions.route.is_some()
        || !event.actions.artifact_delta.is_empty()
        || !event.actions.state_delta.is_empty()
        || event.actions.skip_summarization
    {
        return Err(AgentEventProjectionError::unsupported());
    }
    let content = event
        .content()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    if content.role != "assistant" || content.parts.len() != 1 {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let Part::Text { text } = &content.parts[0] else {
        return Err(AgentEventProjectionError::invalid_state());
    };
    if text.is_empty() || text.len() > MAX_PIPELINE_INTERRUPT_MESSAGE_BYTES + 32 {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn pipeline_hitl_identity(
    invocation_id: &str,
    payload: &GraphInterruptPayload,
    data: &PipelineHitlData,
) -> Result<(String, String), AgentEventProjectionError> {
    let canonical = canonical_json(
        payload
            .data
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?,
    )?;
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(PIPELINE_HITL_DIGEST_DOMAIN);
    for field in [
        invocation_id.as_bytes(),
        payload.thread_id.as_bytes(),
        payload.checkpoint_id.as_bytes(),
        data.node_name.as_bytes(),
        data.definition_digest.as_bytes(),
        canonical.as_slice(),
    ] {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field);
    }
    let digest = context.finish();
    Ok((
        format!("hitl_g1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref())),
        format!("sha256:{}", hex(digest.as_ref())),
    ))
}

fn pipeline_tool_hitl_identity(
    invocation_id: &str,
    payload: &GraphInterruptPayload,
    data: &PipelineToolHitlData,
) -> Result<(String, String), AgentEventProjectionError> {
    let canonical = canonical_json(
        payload
            .data
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?,
    )?;
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(PIPELINE_TOOL_HITL_DIGEST_DOMAIN);
    for field in [
        invocation_id.as_bytes(),
        payload.thread_id.as_bytes(),
        payload.checkpoint_id.as_bytes(),
        data.node_name.as_bytes(),
        data.definition_digest.as_bytes(),
        data.tool_call_id.as_bytes(),
        data.argument_digest.as_bytes(),
        canonical.as_slice(),
    ] {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field);
    }
    let digest = context.finish();
    Ok((
        format!("hitl_gt1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref())),
        format!("sha256:{}", hex(digest.as_ref())),
    ))
}

fn pipeline_mcp_auth_identity(
    invocation_id: &str,
    payload: &GraphInterruptPayload,
    data: &PipelineMcpAuthData,
) -> Result<(String, String), AgentEventProjectionError> {
    let canonical = canonical_json(
        payload
            .data
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?,
    )?;
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(PIPELINE_MCP_AUTH_DIGEST_DOMAIN);
    for field in [
        invocation_id.as_bytes(),
        payload.thread_id.as_bytes(),
        payload.checkpoint_id.as_bytes(),
        data.node_name.as_bytes(),
        data.definition_digest.as_bytes(),
        data.tool_call_id.as_bytes(),
        data.argument_digest.as_bytes(),
        canonical.as_slice(),
    ] {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field);
    }
    let digest = context.finish();
    Ok((
        format!("mcp_auth_g1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref())),
        format!("sha256:{}", hex(digest.as_ref())),
    ))
}

fn pipeline_clarifying_identity(
    invocation_id: &str,
    payload: &GraphInterruptPayload,
    data: &PipelineClarifyingData,
) -> Result<(String, String), AgentEventProjectionError> {
    let canonical = canonical_json(
        payload
            .data
            .as_ref()
            .ok_or_else(AgentEventProjectionError::invalid_state)?,
    )?;
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(PIPELINE_CLARIFYING_DIGEST_DOMAIN);
    for field in [
        invocation_id.as_bytes(),
        payload.thread_id.as_bytes(),
        payload.checkpoint_id.as_bytes(),
        data.node_name.as_bytes(),
        data.definition_digest.as_bytes(),
        data.tool_call_id.as_bytes(),
        data.argument_digest.as_bytes(),
        canonical.as_slice(),
    ] {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field);
    }
    let digest = context.finish();
    Ok((
        format!("hitl_gq1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref())),
        format!("sha256:{}", hex(digest.as_ref())),
    ))
}

fn valid_https_public_metadata_url(value: &str) -> bool {
    reqwest::Url::parse(value).is_ok_and(|url| {
        url.scheme() == "https"
            && url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.fragment().is_none()
    })
}

fn same_origin(left: &str, right: &str) -> bool {
    let Ok(left) = reqwest::Url::parse(left) else {
        return false;
    };
    let Ok(right) = reqwest::Url::parse(right) else {
        return false;
    };
    left.scheme() == right.scheme()
        && left.host_str() == right.host_str()
        && left.port_or_known_default() == right.port_or_known_default()
}

fn validate_pipeline_hitl_message(value: &str) -> Result<(), AgentEventProjectionError> {
    if value.is_empty()
        || value.len() > MAX_PIPELINE_HITL_MESSAGE_BYTES
        || value.chars().any(|character| {
            character == '\0'
                || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
        })
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn validate_pipeline_interrupt_envelope_message(
    value: &str,
) -> Result<(), AgentEventProjectionError> {
    if value.is_empty()
        || value.len() > MAX_PIPELINE_INTERRUPT_MESSAGE_BYTES
        || value.chars().any(|character| {
            character == '\0'
                || (character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
        })
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn valid_pipeline_node_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_PIPELINE_NODE_IDENTITY_BYTES
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':'))
}

fn valid_pipeline_state_key(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && !value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

fn valid_graph_checkpoint_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 512 && !value.chars().any(char::is_control)
}

fn valid_sha256_label(value: &str) -> bool {
    value.len() == 71
        && value.starts_with("sha256:")
        && value[7..].bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn canonical_json(value: &Value) -> Result<Vec<u8>, AgentEventProjectionError> {
    serde_json::to_vec(&canonical_value(value, 0)?)
        .map_err(|_| AgentEventProjectionError::invalid_state())
}

fn canonical_value(value: &Value, depth: usize) -> Result<Value, AgentEventProjectionError> {
    if depth > 64 {
        return Err(AgentEventProjectionError {
            code: AgentEventProjectionErrorCode::ResourceExhausted,
            protocol: None,
        });
    }
    match value {
        Value::Array(values) => values
            .iter()
            .map(|value| canonical_value(value, depth + 1))
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Value::Object(values) => {
            let mut keys = values.keys().collect::<Vec<_>>();
            keys.sort_unstable();
            let mut object = serde_json::Map::with_capacity(values.len());
            for key in keys {
                let value = values
                    .get(key)
                    .ok_or_else(AgentEventProjectionError::invalid_state)?;
                object.insert(key.clone(), canonical_value(value, depth + 1)?);
            }
            Ok(Value::Object(object))
        }
        value => Ok(value.clone()),
    }
}

pub(crate) fn mask_sensitive_arguments(
    value: &Value,
    depth: usize,
) -> Result<Value, AgentEventProjectionError> {
    if depth > 64 {
        return Err(AgentEventProjectionError {
            code: AgentEventProjectionErrorCode::ResourceExhausted,
            protocol: None,
        });
    }
    match value {
        Value::Array(values) => values
            .iter()
            .map(|value| mask_sensitive_arguments(value, depth + 1))
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Value::Object(values) => {
            let mut masked = serde_json::Map::with_capacity(values.len());
            for (key, value) in values {
                let value = if sensitive_argument_key(key) {
                    Value::String("***".to_owned())
                } else {
                    mask_sensitive_arguments(value, depth + 1)?
                };
                masked.insert(key.clone(), value);
            }
            Ok(Value::Object(masked))
        }
        value => Ok(value.clone()),
    }
}

fn sensitive_argument_key(key: &str) -> bool {
    let normalized = key
        .chars()
        .filter(char::is_ascii_alphanumeric)
        .flat_map(char::to_lowercase)
        .collect::<String>();
    [
        "password",
        "passwd",
        "token",
        "secret",
        "apikey",
        "authorization",
        "credential",
        "cookie",
        "privatekey",
        "accesskey",
    ]
    .iter()
    .any(|needle| normalized.contains(needle))
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    output
}

fn ordinary_model_event(
    event: &Event,
    allow_function_calls: bool,
) -> Result<Option<OrdinaryModelEvent>, AgentEventProjectionError> {
    let Some(content) = event.content() else {
        let closes_turn =
            event.llm_response.turn_complete || event.llm_response.finish_reason.is_some();
        if !closes_turn {
            return Ok(None);
        }
        if !event.actions.state_delta.is_empty() || event.actions.skip_summarization {
            return Err(AgentEventProjectionError::unsupported());
        }
        let output_limited = output_limited(event)?;
        return Ok(Some(OrdinaryModelEvent {
            content: String::new(),
            thinking: String::new(),
            closes_turn: true,
            output_limited,
            timestamp: event
                .timestamp
                .to_rfc3339_opts(SecondsFormat::AutoSi, false),
        }));
    };
    if content.role != "model" && content.role != "assistant" {
        return Err(AgentEventProjectionError::unsupported());
    }
    if !bounded_logical_parts(&content.parts) {
        return Err(AgentEventProjectionError {
            code: AgentEventProjectionErrorCode::ResourceExhausted,
            protocol: None,
        });
    }
    let (content, thinking) = ordinary_text_parts(content.parts.as_slice(), allow_function_calls)?;
    let closes_turn = event.llm_response.turn_complete || !event.llm_response.partial;
    let output_limited = if closes_turn {
        output_limited(event)?
    } else {
        false
    };
    Ok(Some(OrdinaryModelEvent {
        content,
        thinking,
        closes_turn,
        output_limited,
        timestamp: event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false),
    }))
}

fn bounded_logical_parts(parts: &[Part]) -> bool {
    // ADK's non-streaming result keeps one text/thinking part per provider
    // delta. Count adjacent fragments as one logical block, without modifying
    // durable content or signatures. Also bound empty-fragment scanning.
    if parts.len() > MAX_CURRENT_NODE_EVENT_JSON_BYTES {
        return false;
    }
    let adjacent_fragments = parts
        .windows(2)
        .filter(|pair| {
            matches!(
                pair,
                [Part::Text { .. }, Part::Text { .. }]
                    | [Part::Thinking { .. }, Part::Thinking { .. }]
            )
        })
        .count();
    parts.len() - adjacent_fragments <= MAX_ADK_PARTS_PER_EVENT
}

fn output_limited(event: &Event) -> Result<bool, AgentEventProjectionError> {
    match event.llm_response.finish_reason {
        None | Some(FinishReason::Stop) => Ok(false),
        Some(FinishReason::MaxTokens) => Ok(true),
        _ => Err(AgentEventProjectionError::unsupported()),
    }
}

fn ordinary_text_parts(
    parts: &[Part],
    allow_function_calls: bool,
) -> Result<(String, String), AgentEventProjectionError> {
    let mut content = String::new();
    let mut thinking = String::new();
    for part in parts {
        match part {
            Part::Text { text } => extend_bounded(&mut content, text)?,
            Part::Thinking {
                thinking: value, ..
            } => extend_bounded(&mut thinking, value)?,
            Part::FunctionCall { .. } if allow_function_calls => {}
            Part::InlineData { .. }
            | Part::FileData { .. }
            | Part::FunctionCall { .. }
            | Part::FunctionResponse { .. }
            | Part::ServerToolCall { .. }
            | Part::ServerToolResponse { .. }
            | Part::EmbeddedResource { .. } => {
                return Err(AgentEventProjectionError::unsupported());
            }
        }
    }
    Ok((content, thinking))
}

fn replace_nested_interrupt_identity(
    batch: ProjectedAgentEventBatch,
    interrupt_id: &str,
    call_digest: &str,
) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
    if !valid_tool_identity(interrupt_id) || !valid_sha256_label(call_digest) {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let mut replaced = ProjectedAgentEventBatch::new();
    let mut count = 0_usize;
    for mut event in batch {
        if event.r#type != "agent_hitl_interrupt" || count != 0 {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let mut metadata: Value = serde_json::from_slice(&event.response_metadata)
            .map_err(|_| AgentEventProjectionError::invalid_state())?;
        let object = metadata
            .as_object_mut()
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let direct = object
            .get_mut("hitl_interrupt")
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let direct_identity =
            replace_pending_interrupt_identity(direct, interrupt_id, call_digest)?;
        let pending = object
            .get_mut("hitl_interrupts")
            .and_then(Value::as_array_mut)
            .filter(|pending| pending.len() == 1)
            .and_then(|pending| pending.first_mut())
            .ok_or_else(AgentEventProjectionError::invalid_state)?;
        let list_identity = replace_pending_interrupt_identity(pending, interrupt_id, call_digest)?;
        if direct_identity != list_identity {
            return Err(AgentEventProjectionError::invalid_state());
        }
        event.response_metadata = serde_json::to_vec(&metadata)
            .map_err(|_| AgentEventProjectionError::invalid_state())?;
        encode_current_node_event_json(&event).map_err(AgentEventProjectionError::output)?;
        replaced.push(event)?;
        count += 1;
    }
    if count != 1 {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(replaced)
}

fn replace_pending_interrupt_identity(
    pending: &mut Value,
    interrupt_id: &str,
    call_digest: &str,
) -> Result<(String, String), AgentEventProjectionError> {
    let object = pending
        .as_object_mut()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let previous_interrupt_id = object
        .get("interrupt_id")
        .and_then(Value::as_str)
        .filter(|value| valid_tool_identity(value))
        .ok_or_else(AgentEventProjectionError::invalid_state)?
        .to_owned();
    let previous_call_digest = object
        .get("call_digest")
        .and_then(Value::as_str)
        .filter(|value| valid_sha256_label(value))
        .ok_or_else(AgentEventProjectionError::invalid_state)?
        .to_owned();
    object.insert(
        "interrupt_id".to_owned(),
        Value::String(interrupt_id.to_owned()),
    );
    object.insert(
        "call_digest".to_owned(),
        Value::String(call_digest.to_owned()),
    );
    Ok((previous_interrupt_id, previous_call_digest))
}

fn overlay_batch_hierarchy(
    batch: ProjectedAgentEventBatch,
    prefix: &[AgentPathTier],
) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
    let mut overlaid = ProjectedAgentEventBatch::new();
    for mut event in batch {
        overlay_event_hierarchy(&mut event, prefix)?;
        overlaid.push(event)?;
    }
    Ok(overlaid)
}

fn overlay_event_hierarchy(
    event: &mut NodeEventV1,
    prefix: &[AgentPathTier],
) -> Result<(), AgentEventProjectionError> {
    if prefix.is_empty() || prefix.len() > MAX_AGENT_PATH_TIERS {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let mut metadata: Value = serde_json::from_slice(&event.response_metadata)
        .map_err(|_| AgentEventProjectionError::invalid_state())?;
    overlay_hierarchy_value(&mut metadata, prefix, 0)?;
    event.response_metadata =
        serde_json::to_vec(&metadata).map_err(|_| AgentEventProjectionError::invalid_state())?;
    encode_current_node_event_json(event).map_err(AgentEventProjectionError::output)?;
    Ok(())
}

fn overlay_hierarchy_value(
    value: &mut Value,
    prefix: &[AgentPathTier],
    depth: usize,
) -> Result<(), AgentEventProjectionError> {
    if depth > 4 {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let Value::Object(object) = value else {
        return Ok(());
    };
    overlay_hierarchy_object(object, prefix)?;
    for key in ["metadata", "tool_meta", "hitl_interrupt", "message"] {
        if let Some(child) = object.get_mut(key) {
            overlay_hierarchy_value(child, prefix, depth + 1)?;
        }
    }
    for key in ["thinking_steps", "hitl_interrupts"] {
        if let Some(Value::Array(children)) = object.get_mut(key) {
            for child in children {
                overlay_hierarchy_value(child, prefix, depth + 1)?;
            }
        }
    }
    if let Some(Value::Object(tool_calls)) = object.get_mut("tool_calls") {
        for child in tool_calls.values_mut() {
            overlay_hierarchy_value(child, prefix, depth + 1)?;
        }
    }
    Ok(())
}

fn overlay_hierarchy_object(
    object: &mut serde_json::Map<String, Value>,
    prefix: &[AgentPathTier],
) -> Result<(), AgentEventProjectionError> {
    let mut path = prefix.to_vec();
    if let Some(existing) = object.get("parent_agent_path") {
        let Value::Array(existing) = existing else {
            return Err(AgentEventProjectionError::invalid_state());
        };
        for tier in existing {
            path.push(parse_agent_path_tier(tier)?);
        }
    }
    if path.len() > MAX_AGENT_PATH_TIERS {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let owner = path
        .last()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    object.insert(
        "parent_agent_path".to_owned(),
        Value::Array(path.iter().map(agent_path_tier_value).collect()),
    );
    insert_or_validate_hierarchy_string(object, "parent_agent_name", &owner.name, false)?;
    insert_or_validate_hierarchy_string(object, "parent_agent_call_id", &owner.call_id, true)?;
    Ok(())
}

fn insert_or_validate_hierarchy_string(
    object: &mut serde_json::Map<String, Value>,
    key: &str,
    fallback: &str,
    tool_identity: bool,
) -> Result<(), AgentEventProjectionError> {
    match object.get(key) {
        None | Some(Value::Null) => {
            object.insert(key.to_owned(), Value::String(fallback.to_owned()));
        }
        Some(Value::String(value))
            if if tool_identity {
                valid_tool_identity(value)
            } else {
                !value.is_empty()
                    && value.len() <= MAX_CONTEXT_TEXT_BYTES
                    && !value.chars().any(char::is_control)
            } => {}
        Some(_) => return Err(AgentEventProjectionError::invalid_state()),
    }
    Ok(())
}

fn parse_agent_path_tier(value: &Value) -> Result<AgentPathTier, AgentEventProjectionError> {
    let object = value
        .as_object()
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let name = object
        .get("name")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty()
                && value.len() <= MAX_CONTEXT_TEXT_BYTES
                && !value.chars().any(char::is_control)
        })
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let call_id = object
        .get("call_id")
        .and_then(Value::as_str)
        .filter(|value| valid_tool_identity(value))
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let sibling_ordinal = object
        .get("sibling_ordinal")
        .filter(|value| !value.is_null())
        .map(|value| {
            value
                .as_u64()
                .and_then(|value| usize::try_from(value).ok())
                .filter(|value| *value > 0 && *value <= MAX_TOOL_CALLS_PER_MODEL_TURN)
                .ok_or_else(AgentEventProjectionError::invalid_state)
        })
        .transpose()?;
    Ok(AgentPathTier {
        name: name.to_owned(),
        call_id: call_id.to_owned(),
        sibling_ordinal,
    })
}

fn agent_path_tier_value(tier: &AgentPathTier) -> Value {
    let mut value = json!({
        "name": tier.name,
        "call_id": tier.call_id,
    });
    if let Some(sibling_ordinal) = tier.sibling_ordinal {
        value["sibling_ordinal"] = json!(sibling_ordinal);
    }
    value
}

fn tool_entry(
    id: &str,
    active: &ActiveToolCall,
    timestamp_finish: Option<&str>,
    finish_reason: Option<&str>,
    output: Option<&str>,
    error: Option<&str>,
) -> Value {
    let mut entry = json!({
        "tool_name": active.name,
        "tool_run_id": id,
        "run_id": id,
        "tool_meta": {"name": active.name, "metadata": {}},
        "tool_inputs": active.public_arguments,
        "metadata": {},
        "timestamp_start": active.timestamp_start,
        "timestamp_finish": timestamp_finish,
        "finish_reason": finish_reason,
        "tool_output": output,
        "error": error,
    });
    if let Some(application) = active.application.as_ref() {
        let metadata = json!({
            "original_name": application.display_name,
            "display_name": application.display_name,
            "agent_type": application.agent_type,
            "toolkit_type": application.toolkit_type(),
            "parent_agent_call_id": id,
            "parent_agent_path": [],
            "sibling_ordinal": active.sibling_ordinal,
        });
        let Value::Object(object) = &mut entry else {
            return entry;
        };
        object.insert("metadata".to_owned(), metadata.clone());
        object.insert("parent_agent_call_id".to_owned(), json!(id));
        object.insert("parent_agent_path".to_owned(), json!([]));
        object.insert("sibling_ordinal".to_owned(), json!(active.sibling_ordinal));
        object.insert(
            "tool_meta".to_owned(),
            json!({
                "name": active.name,
                "display_name": application.display_name,
                "metadata": metadata,
            }),
        );
    } else if let Some(node_name) = active.pipeline_node_name.as_deref() {
        let metadata = json!({
            "langgraph_node": node_name,
            "original_name": node_name,
        });
        let Value::Object(object) = &mut entry else {
            return entry;
        };
        object.insert("metadata".to_owned(), metadata.clone());
        object.insert(
            "tool_meta".to_owned(),
            json!({
                "name": active.name,
                "metadata": metadata,
            }),
        );
    }
    entry
}

fn valid_tool_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_ADK_EVENT_ID_BYTES
        && !value.chars().any(char::is_control)
}

fn descendant_checkpoint_thread(
    event: &Event,
    application: &ApplicationToolPresentation,
    parent_thread_id: Option<&str>,
    parent_call_id: &str,
) -> Result<Option<String>, AgentEventProjectionError> {
    let marker = event
        .provider_metadata
        .get(DESCENDANT_CHECKPOINT_THREAD_KEY);
    if application.agent_type != "pipeline" {
        return if marker.is_none() {
            Ok(None)
        } else {
            Err(AgentEventProjectionError::invalid_state())
        };
    }
    let checkpoint_thread_id = marker
        .filter(|value| valid_tool_identity(value))
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    let parent_thread_id = parent_thread_id.ok_or_else(AgentEventProjectionError::invalid_state)?;
    let node_name = pipeline_application_call_node(parent_call_id)
        .ok_or_else(AgentEventProjectionError::invalid_state)?;
    if checkpoint_thread_id != &format!("{parent_thread_id}/{node_name}") {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(Some(checkpoint_thread_id.clone()))
}

fn pipeline_application_call_node(call_id: &str) -> Option<&str> {
    let (node_name, step) = call_id.strip_prefix("pipeline:")?.rsplit_once(':')?;
    (valid_pipeline_node_identity(node_name)
        && !step.is_empty()
        && step.bytes().all(|byte| byte.is_ascii_digit()))
    .then_some(node_name)
}

fn validate_tool_event_value(value: &Value) -> Result<(), AgentEventProjectionError> {
    if serde_json::to_vec(value).is_ok_and(|encoded| encoded.len() <= MAX_TOOL_EVENT_VALUE_BYTES) {
        Ok(())
    } else {
        Err(AgentEventProjectionError {
            code: AgentEventProjectionErrorCode::ResourceExhausted,
            protocol: None,
        })
    }
}

fn validate_event_id(value: &str) -> Result<(), AgentEventProjectionError> {
    if value.is_empty()
        || value.len() > MAX_ADK_EVENT_ID_BYTES
        || value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    {
        return Err(AgentEventProjectionError::invalid_state());
    }
    Ok(())
}

fn validate_invocation_id(value: &str) -> Result<(), AgentEventProjectionError> {
    validate_event_id(value)
}

fn merge_stream_value(
    previous: &str,
    current: String,
) -> Result<(String, String), AgentEventProjectionError> {
    if previous.is_empty() {
        if current.len() > MAX_CURRENT_NODE_EVENT_JSON_BYTES {
            return Err(AgentEventProjectionError {
                code: AgentEventProjectionErrorCode::ResourceExhausted,
                protocol: None,
            });
        }
        let delta = current.clone();
        Ok((current, delta))
    } else if let Some(delta) = current.strip_prefix(previous) {
        let delta = delta.to_owned();
        Ok((current, delta))
    } else {
        let mut accumulated = previous.to_owned();
        extend_bounded(&mut accumulated, &current)?;
        Ok((accumulated, current))
    }
}

fn trim_continuation_overlap(existing_tail: &str, incoming_content: &str) -> String {
    let stripped = incoming_content.trim_start_matches(['\n', '\r']);
    let existing = existing_tail.chars().collect::<Vec<_>>();
    let incoming = stripped.chars().collect::<Vec<_>>();
    let max_overlap = existing
        .len()
        .min(incoming.len())
        .min(MAX_CONTINUATION_OVERLAP_CHARS);

    for overlap in (4..=max_overlap).rev() {
        let suffix = &existing[existing.len() - overlap..];
        if suffix != &incoming[..overlap]
            || !suffix.iter().any(|character| character.is_alphanumeric())
        {
            continue;
        }
        let starts_at_boundary = overlap == existing.len()
            || !(existing[existing.len() - overlap - 1].is_alphanumeric()
                && suffix[0].is_alphanumeric());
        let ends_at_boundary = overlap == incoming.len()
            || !(suffix[overlap - 1].is_alphanumeric() && incoming[overlap].is_alphanumeric());
        if starts_at_boundary && ends_at_boundary {
            return incoming[overlap..].iter().collect();
        }
    }
    if existing
        .last()
        .is_some_and(|character| character.is_alphabetic())
        && incoming
            .first()
            .is_some_and(|character| character.is_alphabetic())
    {
        let mut separated = String::with_capacity(incoming_content.len().saturating_add(1));
        separated.push(' ');
        separated.push_str(incoming_content);
        return separated;
    }
    incoming_content.to_owned()
}

fn extend_bounded(target: &mut String, value: &str) -> Result<(), AgentEventProjectionError> {
    if target.len().saturating_add(value.len()) > MAX_CURRENT_NODE_EVENT_JSON_BYTES {
        return Err(AgentEventProjectionError {
            code: AgentEventProjectionErrorCode::ResourceExhausted,
            protocol: None,
        });
    }
    target.push_str(value);
    Ok(())
}
