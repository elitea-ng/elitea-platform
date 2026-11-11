//! Invocation-local ADK-Rust session and Runner assembly.
//!
//! The ordinary profile treats Main's frozen input as the complete turn
//! snapshot. It creates one claim-bound session service and one exclusive
//! Runner per authorized invocation. The capability-disabled default remains
//! invocation-local; production composition can inject the fenced `PostgreSQL`
//! service without changing the Runner or agent shape. Stable, pseudonymous
//! ADK identities keep tenant and principal references out of provider/session
//! diagnostics while preserving the current thread boundary.

#![allow(dead_code)] // Production capability registration remains disabled.

pub(crate) use super::model_checkpoint::ValidatedModelCheckpoint;

use std::collections::{BTreeSet, HashMap};
use std::sync::Arc;

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::graph::Checkpointer;
use adk_rust::session::{
    AppendEventRequest, CreateRequest, DeleteRequest, GetRequest, InMemorySessionService,
    ListRequest, Session, SessionService,
};
use adk_rust::{
    AdkError, AdkIdentity, Agent, Content, Event, EventStream, GenerateContentConfig,
    InvocationContext, Llm, RunConfig, SessionId, ToolConcurrencyConfig, ToolExecutionStrategy,
    Toolset, UserId,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;
use serde_json::{Map, Value};
use sqlx::PgPool;

use super::application_tools::{
    ApplicationEventReceiver, ApplicationEventStreamingAgent, ApplicationResumeCoordinator,
    SkippedApplicationChild, install_nested_application_resume, prepare_nested_application_resume,
    skipped_application_children_notice_text,
};
use super::assembly::OrdinaryNoToolProfile;
use super::attachments;
use super::context_management::ContextManagementPlan;
use super::direct_hitl::{
    DirectDelegatedAuthorizationContinuation, DirectHitlDecision, DirectHitlDecisionSet,
    DirectHitlError, DirectHitlErrorCode, DirectHitlRunInput, ResolvedDirectHitlDecision,
    ResolvedDirectHitlStart,
};
use super::events::{
    AgentEventProjectionContext, AgentEventProjectionError, AgentEventProjectionErrorCode,
    AgentEventProjector, ApplicationToolPresentationCatalog, CompletedAgentBrowserOutput,
    OrdinaryProjectionInput, PipelineProjectionInput, ToolkitAttributionCatalog,
};
use super::graph::compiler::{PipelineConfigurationError, PipelineDefinition};
use super::graph::resume::{PipelineResumeError, PipelineResumeErrorCode};
use super::graph::{
    EliteaGraphAgent, PIPELINE_COMPLETED_CONTENT, PipelineNodeEventReceiver,
    PipelineNodeEventStreamingAgent,
};
use super::internal_tools::{
    ASK_USER_METADATA_KEY, ASK_USER_TOOL_NAME, AskUserRequest, InternalToolCatalog,
    encode_ask_user_request,
};
use super::request::{AgentExecutionRequest, UserInput};
use super::runtime::{
    AssembledNativeAgentInvocation, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
    NativeAgentCompletionSelector, NativeAgentInvocation, PipelineNativeStart,
};
use super::sensitive_tools::SensitiveToolCatalog;
use super::tool_namespacing::{RenamedTool, renamed_tools_notice_text};
use crate::protocol::command::VerifiedAgentCommand;
use crate::protocol::control::{ClaimBoundSessionAuthority, SessionWriterClaimBinding};
use crate::protocol::elitea::runtime::v1::{AgentExecutionCommandV1, worker_command_v1};
use crate::state::{
    CheckpointLimits, CheckpointWriterAuthority, PostgresCheckpointError, PostgresCheckpointer,
    PostgresSessionError, PostgresSessionService, SessionLimits, SessionWriterAuthority,
    StateWriterLease,
};
use crate::toolkits::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DelegatedAuthorizationCatalog,
    encode_delegated_authorization_requirement,
};
use crate::toolkits::{LeftOutToolkit, left_out_toolkits_notice_text};

const APP_NAME: &str = "elitea-agent-v1";
const ROOT_AGENT_NAME: &str = "elitea-agent";
const USER_ID_DOMAIN: &[u8] = b"elitea.adk.user.v1\0";
const SESSION_ID_DOMAIN: &[u8] = b"elitea.adk.session.v1\0";
const DEFINITION_DIGEST_DOMAIN: &[u8] = b"elitea.adk.agent-definition.v1\0";
const APPLICATION_DEFINITION_KIND: &[u8] = b"application";
const ADHOC_DEFINITION_KIND: &[u8] = b"adhoc";
const MAX_PUBLIC_APPLICATION_DETAILS_BYTES: usize = 32 * 1_024;
const APPLICATION_CAPABILITY_ID: &str = "agent.execute.application.v1";
const ADHOC_CAPABILITY_ID: &str = "agent.execute.adhoc.v1";
const FROZEN_HISTORY_EVENT_DOMAIN: &[u8] = b"elitea.adk.frozen-history-event.v1\0";
const HISTORY_MESSAGE_EVENT_DOMAIN: &[u8] = b"elitea.adk.history-message-event.v1\0";
const MAX_PARALLEL_APPLICATION_CALLS: usize = 8;
/// The user turn of an output continuation.
///
/// A REASONING-ONLY cut (no visible output; Main sends an empty
/// `truncated_content`) cannot be continued from where it stopped: the hidden
/// reasoning is deliberately not carried (agent.proto, `truncated_content`),
/// so the model would restart the same reasoning under the same output
/// allowance and be cut again (live 4.45.1: Continue for ever). The prompt
/// therefore asks for the answer DIRECTLY, with as little reasoning as the task
/// allows. A model with a reasoning effort set runs at `low` for this call; a
/// model with no effort gets no reasoning knob, because none is proven safe for
/// it (`continuation_reasoning_effort` in ordinary.rs lists each family). The
/// allowance is not raised: it is the user's own setting, and the SDK's nested
/// continuation never raises a configured limit either
/// (`_continuation_max_tokens`, "reduce, but never increase"). A second
/// reasoning-only cut ends the turn as `OUTPUT_CONTINUATION_EXHAUSTED` instead of
/// another Continue (`AgentEventProjector::finish_after_eos`).
fn output_continuation_prompt(original_request: &str, visible_content: &str) -> String {
    if visible_content.is_empty() {
        return format!(
            "The previous attempt used its whole output-token limit on internal reasoning and produced no visible output. Answer now, directly. Keep internal reasoning to the minimum the task needs and start the visible answer immediately; the same output-token limit applies. Follow the original request exactly. Output only the answer and do not refer to the previous attempt.\n\nOriginal request:\n{original_request}"
        );
    }
    let visible_words = visible_content.split_whitespace().count();
    format!(
        "The previous assistant output was cut off by its output-token limit after approximately {visible_words} visible words. Continue exactly where it stopped. Output only the missing ending. Do not repeat or restart content. Preserve the original request constraints and any leading whitespace required at the seam. The exact visible assistant output is included because it might not be present in durable model history. Continue after its final character.\n\nOriginal request:\n{original_request}\n\nExact visible assistant output so far:\n<visible-assistant-output>\n{visible_content}\n</visible-assistant-output>"
    )
}

/// Build the turn's human message: the user's own text, then this turn's
/// attachment chunks (#606).
///
/// The attachments are spliced AFTER the user's text, the order the model reads
/// them in, mirroring the SDK adapter (`sdk_adapter.py:926-937`,
/// `human_message_content`). With no attachments this is a bare
/// `Content::new("user").with_text(..)`, so an ordinary turn is untouched.
///
/// `attachments` is the RESOLVED chunk list, not `payload.input_attachments`:
/// the assembly reads each pending document over the live claim first and
/// splices its text in beside its header (`attachments::read_attachment_documents`
/// → `attachments::resolved_attachment_chunks`). It defaults to the payload's
/// own list, so any path that performs no read renders exactly what arrived —
/// which is also what makes a file the platform will not serve reach the model
/// as its name alone rather than failing the turn.
fn ordinary_user_content(
    request: &AgentExecutionRequest,
    output_continuation: bool,
    user_text: String,
    attachments: &[Value],
) -> Content {
    let base = Content::new("user").with_text(if output_continuation {
        output_continuation_prompt(
            &user_text,
            request
                .payload
                .truncated_content
                .as_deref()
                .unwrap_or_default(),
        )
    } else {
        user_text
    });
    attachments::append_attachment_parts(base, attachments)
}

/// The uuid of this turn's question (#1831): Main's `current_message_id`,
/// which it sets only on a turn that writes its own question. An output
/// continuation or a resume of any kind runs on an earlier question
/// (`writes_question` false), so none even if the input named one. A
/// regeneration asks its question again under that question's own uuid,
/// which Main names since #1903, so the question is tagged like a fresh one.
fn current_message_id(request: &AgentExecutionRequest, writes_question: bool) -> Option<String> {
    writes_question
        .then(|| request.payload.current_message_id.clone())
        .flatten()
}

/// Whether this turn asks a question of its own: not an output continuation
/// and not a resume of any kind, which run on an earlier question. A
/// regeneration asks its question again.
fn writes_own_question(
    request: &AgentExecutionRequest,
    output_continuation: bool,
    pipeline_resume: Option<(bool, bool)>,
) -> bool {
    let payload = &request.payload;
    !(payload.should_continue
        || payload.hitl_resume
        || output_continuation
        || pipeline_resume.is_some_and(|(should_continue, hitl)| should_continue || hitl))
}

/// The short id of the message `message_id` (#1831), over its canonical
/// uuid form, so the question of this turn and the same message in a later
/// turn's history always carry the same id.
fn short_message_ref(message_id: &str) -> String {
    use elitea_agent_runtime::subagents::message_ref::message_ref;
    match uuid::Uuid::try_parse(message_id) {
        Ok(id) => message_ref(&id.hyphenated().to_string()),
        Err(_) => message_ref(&message_id.to_ascii_lowercase()),
    }
}

/// #1831: neutralize every id-like line in the text of `content` and put
/// `[message <short>]` before its first text part, so only this line names
/// a message. Answers the text of its first `limit` parts after the line,
/// as the model reads it, or `None` for a content without text there (it is
/// left untagged). Parts past `limit` (this turn's attachment chunks) are
/// not the message's own text, and a later turn's history does not hold
/// them, so they cannot be quoted under its id.
fn tag_message(content: &mut Content, short: &str, limit: usize) -> Option<String> {
    use elitea_agent_runtime::subagents::message_ref::{neutralize, tag};
    let mut quotable = String::new();
    let mut tagged = false;
    for (index, part) in content.parts.iter_mut().enumerate() {
        let adk_rust::Part::Text { text } = part else {
            continue;
        };
        if let std::borrow::Cow::Owned(neutral) = neutralize(text) {
            *text = neutral;
        }
        if index >= limit {
            continue;
        }
        if !quotable.is_empty() {
            quotable.push_str("\n\n");
        }
        quotable.push_str(text);
        if !tagged {
            text.insert_str(0, &tag(short));
            tagged = true;
        }
    }
    tagged.then_some(quotable)
}

/// Dispatch policy chosen from the complete frozen root tool snapshot.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) enum NativeToolExecutionMode {
    #[default]
    Sequential,
    /// Every model-callable root tool is a saved Application participant.
    ParallelApplications,
}

#[derive(Default)]
pub(crate) struct ApplicationRuntimeProjection {
    presentations: ApplicationToolPresentationCatalog,
    events: Option<ApplicationEventReceiver>,
    resume: Option<ApplicationResumeCoordinator>,
}

impl ApplicationRuntimeProjection {
    #[must_use]
    pub(crate) const fn streaming(
        presentations: ApplicationToolPresentationCatalog,
        events: ApplicationEventReceiver,
        resume: ApplicationResumeCoordinator,
    ) -> Self {
        Self {
            presentations,
            events: Some(events),
            resume: Some(resume),
        }
    }

    #[must_use]
    pub(crate) const fn from_presentations(
        presentations: ApplicationToolPresentationCatalog,
    ) -> Self {
        Self {
            presentations,
            events: None,
            resume: None,
        }
    }

    #[cfg(test)]
    pub(crate) const fn coordinated_fixture(
        presentations: ApplicationToolPresentationCatalog,
        resume: ApplicationResumeCoordinator,
    ) -> Self {
        Self {
            presentations,
            events: None,
            resume: Some(resume),
        }
    }

    #[cfg(test)]
    pub(crate) const fn presentations(presentations: ApplicationToolPresentationCatalog) -> Self {
        Self::from_presentations(presentations)
    }

    pub(crate) fn event_receiver(&self) -> Option<ApplicationEventReceiver> {
        self.events.clone()
    }
    pub(crate) fn resume_coordinator(&self) -> Option<ApplicationResumeCoordinator> {
        self.resume.clone()
    }
    pub(crate) fn presentation_catalog(&self) -> ApplicationToolPresentationCatalog {
        self.presentations.clone()
    }

    pub(crate) async fn install_scope_resume(
        &self,
        events: &[Event],
        decisions: Vec<ResolvedDirectHitlDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let coordinator = self.resume.as_ref().ok_or_else(invalid_configuration)?;
        install_nested_application_resume(events, decisions, &self.presentations, coordinator).await
    }

    /// [`Self::install_scope_resume`] for one fan-out member's decision.
    pub(crate) async fn install_scope_member_resume(
        &self,
        events: &[Event],
        decisions: Vec<ResolvedDirectHitlDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let coordinator = self.resume.as_ref().ok_or_else(invalid_configuration)?;
        super::application_tools::install_nested_application_member_resume(
            events,
            decisions,
            &self.presentations,
            coordinator,
        )
        .await
    }

    pub(crate) async fn install_scope_static_resume(
        &self,
        events: &[Event],
        decisions: Vec<super::graph::static_tool_pause::StaticToolDecision>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let coordinator = self.resume.as_ref().ok_or_else(invalid_configuration)?;
        super::application_tools::install_static_application_resume(
            events,
            decisions,
            &self.presentations,
            coordinator,
        )
        .await
    }

    pub(crate) fn insert_pipeline_presentation(
        &mut self,
        tool_name: String,
        display_name: String,
        children: ApplicationToolPresentationCatalog,
    ) -> Result<(), AgentEventProjectionError> {
        self.presentations.insert_runtime(
            tool_name,
            display_name,
            "pipeline".to_owned(),
            "nested-model".to_owned(),
            children,
            super::events::ApplicationToolGuardCatalogs::default(),
        )
    }

    pub(crate) fn insert_presentation(
        &mut self,
        tool_name: String,
        display_name: String,
        agent_type: String,
    ) -> Result<(), AgentEventProjectionError> {
        self.presentations
            .insert(tool_name, display_name, agent_type)
    }
}

/// Invocation-owned toolsets and the runtime catalogs that describe them.
pub(crate) struct OrdinaryRuntimeBindings {
    toolsets: Vec<Arc<dyn Toolset>>,
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
    /// Attached application children this worker cannot build (#973). They
    /// bind no tool and are named in the session's opening notice, beside the
    /// skipped internal tools.
    skipped_application_children: Vec<SkippedApplicationChild>,
    /// Tools two toolsets both published, and what each is called now (#983).
    /// Named in the session's opening notice beside the two above.
    renamed_tools: Vec<RenamedTool>,
    /// Configured toolkits left out for a setting this runtime refuses
    /// (#1207 review round 2), named in the same opening notice.
    left_out_toolkits: Vec<LeftOutToolkit>,
    /// The toolkit each bound tool came from, named on its browser frames.
    toolkit_attribution: ToolkitAttributionCatalog,
    application_runtime: ApplicationRuntimeProjection,
    instruction_plan: super::instruction_authority::InstructionPlan,
    /// The Smart Tools Selection index for the system prompt, when the mode
    /// is on and has enough tools to be worth it.
    lazy_tools_index: Option<String>,
    /// The cloud workspace's argument-aware confirmation (ADR-0034): a
    /// destructive or escaping `run_command` is a HITL interrupt.
    tool_call_confirmation: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
    /// Client contract 1.27: the user messages queued against the running
    /// turn, taken by the root agent at each safe point. Only the ordinary
    /// root agent takes them; a pipeline or a child agent never does.
    steering: Option<Arc<dyn crate::state::TurnSteering>>,
    /// The run's public egress policy and ledger (#1490). Delegated-auth
    /// discovery runs after a tool call, outside any egress scope, so it is
    /// handed the run here; never set, nothing public is reached.
    egress: Arc<elitea_agent_runtime::egress::RunEgress>,
    /// FU-287: the durable root session the Runner refreshes by ordinal.
    /// Unset (`InvocationLocal`, tests), the Runner re-reads it whole.
    event_source: Option<Arc<PostgresSessionService>>,
}

impl OrdinaryRuntimeBindings {
    #[must_use]
    pub(crate) fn new(
        toolsets: Vec<Arc<dyn Toolset>>,
        sensitive_tools: SensitiveToolCatalog,
        delegated_authorization: DelegatedAuthorizationCatalog,
        application_runtime: ApplicationRuntimeProjection,
    ) -> Self {
        Self {
            toolsets,
            sensitive_tools,
            delegated_authorization,
            instruction_plan: super::instruction_authority::InstructionPlan::default(),
            internal_tools: InternalToolCatalog::empty(),
            skipped_application_children: Vec::new(),
            renamed_tools: Vec::new(),
            left_out_toolkits: Vec::new(),
            toolkit_attribution: ToolkitAttributionCatalog::default(),
            application_runtime,
            lazy_tools_index: None,
            tool_call_confirmation: None,
            steering: None,
            egress: elitea_agent_runtime::egress::RunEgress::closed(),
            event_source: None,
        }
    }

    /// Refresh the Runner's view from this durable session by ordinal (FU-287).
    #[must_use]
    pub(crate) fn with_event_source(mut self, source: Option<Arc<PostgresSessionService>>) -> Self {
        self.event_source = source;
        self
    }

    /// The run whose egress policy delegated-auth discovery runs under.
    #[must_use]
    pub(crate) fn with_egress(
        mut self,
        egress: Arc<elitea_agent_runtime::egress::RunEgress>,
    ) -> Self {
        self.egress = egress;
        self
    }

    /// Take steering messages at the root agent's safe points.
    #[must_use]
    pub(crate) fn with_steering(
        mut self,
        steering: Option<Arc<dyn crate::state::TurnSteering>>,
    ) -> Self {
        self.steering = steering;
        self
    }

    /// Ask a person before a destructive or escaping `run_command` of the
    /// cloud workspace (ADR-0034 decision 9): `gate` decides per call, and
    /// the workspace command joins the sensitive catalog as a CONDITIONAL
    /// entry, so its interrupt is projected and resumed like any sensitive
    /// tool's, and an approved call replays in the continuation's workspace.
    ///
    /// # Errors
    ///
    /// The catalog already names a `local.run_command`.
    pub(crate) fn with_workspace_gate(
        mut self,
        gate: Arc<dyn adk_rust::agent::ToolCallConfirmation>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        self.sensitive_tools =
            std::mem::take(&mut self.sensitive_tools).with_workspace_command()?;
        self.tool_call_confirmation = Some(gate);
        Ok(self)
    }

    /// Add the offered cloud browser tools (ADR-0049) to the sensitive
    /// catalog as conditional entries; see
    /// [`SensitiveToolCatalog::with_browser_tools`].
    ///
    /// # Errors
    ///
    /// A name is not a valid tool identity.
    pub(crate) fn with_browser_tools<'a>(
        mut self,
        tools: impl IntoIterator<Item = (&'a str, &'a str)>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        self.sensitive_tools =
            std::mem::take(&mut self.sensitive_tools).with_browser_tools(tools)?;
        Ok(self)
    }

    /// Wrap the turn's argument-aware gate in the cloud browser's
    /// (ADR-0049 decision 3): `compose` gets the gate so far.
    #[must_use]
    pub(crate) fn with_browser_gate(
        mut self,
        compose: impl FnOnce(
            Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
        ) -> Arc<dyn adk_rust::agent::ToolCallConfirmation>,
    ) -> Self {
        self.tool_call_confirmation = Some(compose(self.tool_call_confirmation.take()));
        self
    }

    /// Ask a person before each `open_pull_request` of the cloud executor
    /// (ADR-0034 decision 10, amendment 2026-10-26): the tool joins the
    /// sensitive catalog as a name-wide entry, so its interrupt is projected,
    /// approved and resumed like any sensitive tool's.
    ///
    /// # Errors
    ///
    /// Another tool of the turn already has the name.
    pub(crate) fn with_workspace_pull_request_approval(
        mut self,
    ) -> Result<Self, NativeAgentAssemblyError> {
        self.sensitive_tools =
            std::mem::take(&mut self.sensitive_tools).with_workspace_pull_request()?;
        Ok(self)
    }

    #[must_use]
    fn with_tool_call_confirmation(
        mut self,
        gate: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
    ) -> Self {
        self.tool_call_confirmation = gate;
        self
    }

    #[must_use]
    pub(crate) fn with_lazy_tools_index(mut self, index: Option<String>) -> Self {
        self.lazy_tools_index = index;
        self
    }

    /// What the system prompt gets after the agent's own instructions, in the
    /// SDK's order and with its separators: the internal tools' guidance
    /// joined by `\n\n---\n\n`, and then the Smart Tools Selection index
    /// after a blank line (the SDK appends it per call, to the system message).
    #[must_use]
    pub(crate) fn instruction_suffix(&self, instructions: &str) -> String {
        let text = compose_instructions(
            instructions,
            self.internal_tools.instruction_addendum(),
            self.lazy_tools_index.as_deref(),
        );
        if text.len() > elitea_agent_runtime::request::MAX_AGENT_INSTRUCTION_BYTES {
            // Instructions admitted at their own bound leave no room for the
            // guidance: serve the agent's own text rather than fail the turn.
            tracing::warn!(
                event = "agent_internal_tool_guidance_omitted",
                "the agent's instructions leave no room for the internal tools' guidance"
            );
            return instructions.to_owned();
        }
        text
    }

    #[must_use]
    pub(crate) const fn with_internal_tools(mut self, internal_tools: InternalToolCatalog) -> Self {
        self.internal_tools = internal_tools;
        self
    }

    pub(crate) fn with_instruction_plan(
        mut self,
        plan: super::instruction_authority::InstructionPlan,
    ) -> Self {
        self.instruction_plan = plan;
        self
    }

    #[must_use]
    pub(crate) fn with_skipped_application_children(
        mut self,
        skipped: Vec<SkippedApplicationChild>,
    ) -> Self {
        self.skipped_application_children = skipped;
        self
    }

    #[must_use]
    pub(crate) fn with_renamed_tools(mut self, renamed: Vec<RenamedTool>) -> Self {
        self.renamed_tools = renamed;
        self
    }

    #[must_use]
    pub(crate) fn with_left_out_toolkits(mut self, refused: Vec<LeftOutToolkit>) -> Self {
        self.left_out_toolkits = refused;
        self
    }

    #[must_use]
    pub(crate) fn with_toolkit_attribution(mut self, catalog: ToolkitAttributionCatalog) -> Self {
        self.toolkit_attribution = catalog;
        self
    }

    /// Add the cloud executor's workspace toolset (ADR-0034), after the
    /// configured toolkits.
    #[must_use]
    pub(crate) fn with_workspace_toolset(mut self, toolset: Arc<dyn Toolset>) -> Self {
        self.toolsets.push(toolset);
        self
    }

    /// Add the runtime's read-only sub-agents (#1226), last: `bind` gets the
    /// toolsets bound so far (a child is offered their read-only tools) and
    /// the names a child must never be offered, the tools that need a
    /// confirmation or a delegated authorization, and returns the toolset.
    #[must_use]
    pub(crate) fn with_subagents(
        mut self,
        bind: impl FnOnce(&[Arc<dyn Toolset>], Vec<String>) -> Arc<dyn Toolset>,
    ) -> Self {
        let excluded = self
            .sensitive_tools
            .every_tool_name()
            .chain(self.delegated_authorization.tool_names())
            .map(ToOwned::to_owned)
            .collect();
        let toolset = bind(&self.toolsets, excluded);
        self.toolsets.push(toolset);
        self
    }

    fn has_confirmation_guards(&self) -> bool {
        !self.sensitive_tools.is_empty()
            || !self.delegated_authorization.is_empty()
            || !self.internal_tools.is_empty()
    }
}

pub(crate) struct PipelineRuntimeBindings {
    pub(crate) nodes: super::graph::compiler::PipelineNodeRuntimes,
    pub(crate) applications: ApplicationRuntimeProjection,
    pub(crate) node_events: Option<PipelineNodeEventReceiver>,
    /// Attachments a pipeline `agent` node's Agent left out (FU-243), named on the notice.
    pub(crate) skipped_application_children: Vec<SkippedApplicationChild>,
}

fn application_event_agent(
    agent: Arc<dyn Agent>,
    events: ApplicationEventReceiver,
    applications: ApplicationToolPresentationCatalog,
    lineage: Option<ApplicationResumeCoordinator>,
) -> Arc<dyn Agent> {
    Arc::new(
        ApplicationEventStreamingAgent::new(agent, events, applications).with_call_lineage(lineage),
    )
}

pub(crate) fn delegated_authorization_agent(
    agent: Arc<dyn Agent>,
    authorization: DelegatedAuthorizationCatalog,
    egress: Arc<elitea_agent_runtime::egress::RunEgress>,
) -> Arc<dyn Agent> {
    if authorization.is_empty() {
        agent
    } else {
        Arc::new(DelegatedAuthorizationEventAgent {
            inner: agent,
            authorization,
            egress,
        })
    }
}

pub(crate) fn clarifying_question_agent(
    agent: Arc<dyn Agent>,
    internal_tools: InternalToolCatalog,
) -> Arc<dyn Agent> {
    if internal_tools.ask_user_enabled() {
        Arc::new(ClarifyingQuestionEventAgent { inner: agent })
    } else {
        agent
    }
}

struct ClarifyingQuestionEventAgent {
    inner: Arc<dyn Agent>,
}

#[async_trait]
impl Agent for ClarifyingQuestionEventAgent {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        self.inner.sub_agents()
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let mut events = self.inner.run(context).await?;
        Ok(Box::pin(async_stream::stream! {
            while let Some(event) = adk_rust::futures::StreamExt::next(&mut events).await {
                let mut event = match event {
                    Ok(event) => event,
                    Err(error) => {
                        yield Err(error);
                        return;
                    }
                };
                if let Some(request) = event.actions.tool_confirmation.as_ref()
                    && request.tool_name == ASK_USER_TOOL_NAME
                {
                    let encoded = AskUserRequest::from_arguments(&request.args)
                        .ok()
                        .and_then(|request| encode_ask_user_request(&request));
                    let Some(encoded) = encoded else {
                        yield Err(AdkError::agent("ask_user produced an invalid clarification request"));
                        return;
                    };
                    if event
                        .provider_metadata
                        .insert(ASK_USER_METADATA_KEY.to_owned(), encoded)
                        .is_some()
                    {
                        yield Err(AdkError::agent("ask_user clarification metadata was duplicated"));
                        return;
                    }
                }
                yield Ok(event);
            }
        }))
    }
}

struct DelegatedAuthorizationEventAgent {
    inner: Arc<dyn Agent>,
    authorization: DelegatedAuthorizationCatalog,
    /// The run's egress policy: discovery GETs a URL the server's challenge
    /// named, so it is held to the run's allowlist and lands in its ledger.
    egress: Arc<elitea_agent_runtime::egress::RunEgress>,
}

#[async_trait]
impl Agent for DelegatedAuthorizationEventAgent {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        self.inner.sub_agents()
    }

    async fn run(&self, context: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let mut events = self.inner.run(context).await?;
        let authorization = self.authorization.clone();
        let egress = Arc::clone(&self.egress);
        let declined_scope = authorization
            .encode_declined_scope()
            .map_err(|()| AdkError::agent("delegated authorization scope exceeds its bound"))?;
        Ok(Box::pin(async_stream::stream! {
            while let Some(event) = adk_rust::futures::StreamExt::next(&mut events).await {
                let mut event = match event {
                    Ok(event) => event,
                    Err(error) => {
                        yield Err(error);
                        return;
                    }
                };
                if event.actions.tool_confirmation.is_some()
                    && let Some(scope) = &declined_scope
                {
                    event.provider_metadata.insert(crate::toolkits::DELEGATED_AUTHORIZATION_SCOPE_KEY.to_owned(), scope.clone());
                }
                if let Some(request) = event.actions.tool_confirmation.as_ref()
                    && let Some(requirement) = authorization.requirement_for(&request.tool_name)
                {
                    let requirement = requirement.resolve_public_metadata(&egress).await;
                    let Some(encoded) = encode_delegated_authorization_requirement(&requirement) else {
                        yield Err(AdkError::agent("delegated authorization metadata could not be encoded"));
                        return;
                    };
                    if event
                        .provider_metadata
                        .insert(DELEGATED_AUTHORIZATION_METADATA_KEY.to_owned(), encoded)
                        .is_some()
                    {
                        yield Err(AdkError::agent("delegated authorization metadata was duplicated"));
                        return;
                    }
                }
                yield Ok(event);
            }
        }))
    }
}

/// Non-authority command fields needed by local ADK and browser projection.
///
/// This value is created only from the already authenticated command and has
/// no claim, fence, PAT, output, settlement, or command-bus capability. Fields stay
/// private so assembly cannot accidentally substitute request-carried routing
/// values for command-authenticated identity.
pub(crate) struct AuthorizedNativeCommandBinding {
    tenant_id: String,
    principal_ref: String,
    resource_project_id: String,
    projection_project_id: String,
    execution_id: String,
    generation: u64,
    client_stream_id: String,
    client_message_id: String,
    sio_event: String,
    /// The original command deadline, in Unix milliseconds. Fan-out nodes and
    /// Code admission are bounded by it (`OrdinaryNativeAgentPlan::execution_deadline`).
    deadline_unix_millis: i64,
    /// The execution's public egress policy and ledger (#1490), the same
    /// instance every path of this execution uses.
    egress: std::sync::Arc<elitea_agent_runtime::egress::RunEgress>,
    /// The execution's approval policy (ADR-0041 decision 5, field 68): the
    /// root agent's tier and the categories that ask at it. Every nested
    /// callee of this execution uses it.
    approval: std::sync::Arc<elitea_agent_runtime::approval_policy::ApprovalPolicy>,
    /// The execution's run caps meter (ADR-0041 decision 3, field 69): one
    /// meter for every path of this execution, nested callees included.
    run_caps: std::sync::Arc<elitea_agent_runtime::run_caps::RunCapsMeter>,
    /// The execution's content controls (ADR-0043 decisions 3 and 4, field
    /// 71): the disclosure marker, the preamble and the run's untrusted
    /// reads, shared by every path of this execution.
    content: std::sync::Arc<elitea_agent_runtime::preamble::ContentControls>,
    /// The run's unattended grant (ADR-0045), when the input has one.
    unattended: Option<std::sync::Arc<super::unattended::UnattendedGrant>>,
}

impl AuthorizedNativeCommandBinding {
    /// The binding of a verified command to the run's input. The input's
    /// platform preamble (ADR-0043 decision 3) must be the one the command
    /// names by revision and digest.
    pub(crate) fn from_verified(
        verified: &VerifiedAgentCommand,
        request: &super::request::AgentExecutionRequest,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let command = verified.command();
        let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
            command.capability_command.as_ref()
        else {
            return Err(invalid_input());
        };
        if command.generation == 0 {
            return Err(invalid_input());
        }
        // Verification validated the policy; a policy that does not read
        // fails the command closed rather than run without its approvals.
        let approval = crate::protocol::approval_policy::policy_from_command(command)
            .map_err(|_| invalid_input())?;
        let content = crate::protocol::preamble::controls_from_command(
            command,
            &approval,
            request.payload.platform_preamble.as_ref(),
        )
        .map_err(|_| invalid_input())?;
        Ok(Self::new(command, agent, approval, content)
            .with_unattended_grant(verified.unattended_grant().cloned()))
    }

    fn new(
        command: &crate::protocol::elitea::runtime::v1::WorkerCommandV1,
        agent: &AgentExecutionCommandV1,
        approval: std::sync::Arc<elitea_agent_runtime::approval_policy::ApprovalPolicy>,
        content: std::sync::Arc<elitea_agent_runtime::preamble::ContentControls>,
    ) -> Self {
        Self {
            tenant_id: command.tenant_id.clone(),
            principal_ref: command.principal_ref.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            client_stream_id: agent.client_stream_id.clone(),
            client_message_id: agent.client_message_id.clone(),
            sio_event: agent.sio_event.clone(),
            deadline_unix_millis: command.deadline_unix_millis,
            egress: crate::protocol::egress::run_egress(command),
            content,
            approval,
            run_caps: crate::protocol::run_caps::meter_from_command(command),
            unattended: None,
        }
    }

    /// Set the run's unattended grant (ADR-0045).
    #[must_use]
    pub(crate) fn with_unattended_grant(
        self,
        grant: Option<super::unattended::UnattendedGrant>,
    ) -> Self {
        Self {
            unattended: grant.map(std::sync::Arc::new),
            ..self
        }
    }

    /// The run's unattended grant (ADR-0045), if any.
    pub(crate) const fn unattended_grant(
        &self,
    ) -> Option<&std::sync::Arc<super::unattended::UnattendedGrant>> {
        self.unattended.as_ref()
    }

    /// A new step-limit stop when the run's unattended grant continues at
    /// the step limit (ADR-0045 decision 4), else `None`.
    fn step_limit_stop(&self) -> Option<Arc<super::unattended::StepLimitStop>> {
        self.unattended
            .as_ref()
            .filter(|grant| grant.continue_step_limit())
            .map(|_| Arc::new(super::unattended::StepLimitStop::default()))
    }

    /// The claimed execution.
    pub(crate) fn execution_id(&self) -> &str {
        &self.execution_id
    }

    /// The execution's public egress policy and ledger (#1490).
    pub(crate) const fn egress(&self) -> &std::sync::Arc<elitea_agent_runtime::egress::RunEgress> {
        &self.egress
    }

    /// The execution's approval policy (ADR-0041 decision 5).
    pub(crate) const fn approval_policy(
        &self,
    ) -> &std::sync::Arc<elitea_agent_runtime::approval_policy::ApprovalPolicy> {
        &self.approval
    }

    /// The execution's run caps meter (ADR-0041 decision 3).
    pub(crate) const fn run_caps(
        &self,
    ) -> &std::sync::Arc<elitea_agent_runtime::run_caps::RunCapsMeter> {
        &self.run_caps
    }

    /// The execution's content controls (ADR-0043 decisions 3 and 4).
    pub(crate) const fn content_controls(
        &self,
    ) -> &std::sync::Arc<elitea_agent_runtime::preamble::ContentControls> {
        &self.content
    }

    #[cfg(test)]
    pub(super) fn fixture() -> Self {
        Self {
            tenant_id: "tenant-1".to_owned(),
            principal_ref: "user:42".to_owned(),
            resource_project_id: "17".to_owned(),
            projection_project_id: "9".to_owned(),
            execution_id: "execution-one".to_owned(),
            generation: 3,
            client_stream_id: "conversation-1".to_owned(),
            client_message_id: "message-1".to_owned(),
            sio_event: "chat_predict".to_owned(),
            deadline_unix_millis: 4_102_444_800_000,
            egress: elitea_agent_runtime::egress::RunEgress::open(),
            approval: elitea_agent_runtime::approval_policy::ApprovalPolicy::none(),
            run_caps: elitea_agent_runtime::run_caps::RunCapsMeter::new(
                elitea_agent_runtime::run_caps::RunCapLimits::worker_default(),
            ),
            content: elitea_agent_runtime::preamble::ContentControls::worker_default(),
            unattended: None,
        }
    }

    #[cfg(test)]
    pub(super) fn fixture_for_execution(execution_id: &str) -> Self {
        Self {
            execution_id: execution_id.to_owned(),
            ..Self::fixture()
        }
    }

    #[cfg(test)]
    pub(super) fn with_deadline(self, deadline_unix_millis: i64) -> Self {
        Self {
            deadline_unix_millis,
            ..self
        }
    }
}

/// Fully validated local plan built before the execution PAT is redeemed.
pub(crate) struct OrdinaryNativeAgentPlan {
    user_id: UserId,
    session_id: SessionId,
    user_content: Content,
    generation_config: GenerateContentConfig,
    max_iterations: u32,
    projection: AgentEventProjectionContext,
    thread_id: String,
    chat_history: Vec<Content>,
    /// ADR-0050 decision 9c: one reference per `chat_history` entry, or none
    /// for an input from before field 71.
    chat_history_refs: Vec<super::request::ChatHistoryRef>,
    /// The chat message group of this turn's own question (#1831): Main's
    /// `current_message_id`. `None` on a continuation, a resume, an
    /// addressed agent's reused question, or a regeneration from a Main
    /// that does not name its question (#1903).
    current_message_id: Option<String>,
    /// The context view mode Main built `chat_history` under (#1672), or
    /// `None` for an input from before the view.
    context_view_history: Option<String>,
    /// Main's digest of every eligibility setting of the view (#1902), or
    /// `None` for an input from before `ContextViewV1.binding`.
    context_view_binding: Option<String>,
    /// This turn writes a question of its own (not a continuation or a
    /// resume): the session will hold it as this turn's user content.
    writes_question: bool,
    /// [`Self::tag_message_refs`] ran: the history and the question carry
    /// their `[message m-…]` ids (#1831, #1865).
    tags_messages: bool,
    context_management: ContextManagementPlan,
    step_compaction: Option<super::context_management::CompactionPolicy>,
    capability_id: &'static str,
    definition_digest: [u8; 32],
    tenant_id: String,
    resource_project_id: String,
    projection_project_id: String,
    execution_id: String,
    generation: u64,
    regenerate: bool,
    instruction_plan: super::instruction_authority::InstructionPlan,
    deadline_unix_millis: i64,
    /// This invocation's stop after a failed dispatch record, shared by every
    /// dispatch journal and guarded model of the run (`dispatch_journal`).
    dispatch_abort: super::dispatch_journal::DispatchAbort,
    /// FU-265: the paused execution a pipeline continuation re-enters, as
    /// Main named it inside the admission that authorized the decision.
    paused_writer: Option<super::graph::continuation_writer::PausedWriter>,
    /// ADR-0045 decision 4: the stop that completes the run at its step
    /// limit. Only a run whose unattended grant sets `continue_step_limit`
    /// has it.
    step_limit_stop: Option<Arc<super::unattended::StepLimitStop>>,
}

impl OrdinaryNativeAgentPlan {
    /// FU-265: name the paused execution a continuation re-enters.
    pub(super) fn with_paused_writer(
        mut self,
        writer: Option<super::graph::continuation_writer::PausedWriter>,
    ) -> Self {
        self.paused_writer = writer;
        self
    }

    /// The original command deadline as a monotonic instant: fan-out nodes
    /// and Code admission (W3-1) are bounded by it. A deadline already passed
    /// is refused: the run lifecycle ends it as `DeadlineExceeded` anyway, and
    /// a fan-out must never start past it.
    pub(super) fn execution_deadline(
        &self,
    ) -> Result<tokio::time::Instant, NativeAgentAssemblyError> {
        execution_deadline_at(
            self.deadline_unix_millis,
            chrono::Utc::now().timestamp_millis(),
            tokio::time::Instant::now(),
        )
    }

    /// W3-1: the deadline Code admission measures against. Unlike
    /// [`Self::execution_deadline`], a passed deadline is not an assembly
    /// failure: an agent or pipeline that never runs Code still assembles and
    /// the lifecycle ends it as `DeadlineExceeded`, and every fresh Code
    /// attempt is refused by admission (nothing is left of the window).
    pub(super) fn code_admission_deadline(
        &self,
    ) -> Result<tokio::time::Instant, NativeAgentAssemblyError> {
        code_admission_deadline_at(
            self.deadline_unix_millis,
            chrono::Utc::now().timestamp_millis(),
            tokio::time::Instant::now(),
        )
    }

    pub(super) fn model_scope_sessions(
        &self,
        sessions: super::model_scope::ModelScopeBackend,
    ) -> super::model_scope::ModelScopeSessions {
        super::model_scope::ModelScopeSessions::new(
            sessions,
            self.execution_id.clone(),
            self.generation,
            self.definition_digest,
        )
        .with_root_identity(self.session_identity().ok())
        .with_dispatch_abort(self.dispatch_abort.clone())
    }

    /// The durable conversation thread this turn belongs to.
    ///
    /// #973 reads it so a saved pipeline child can namespace its own graph
    /// checkpoint thread under the conversation rather than under a value the
    /// child could choose.
    #[must_use]
    pub(crate) fn thread_id(&self) -> &str {
        &self.thread_id
    }

    /// The conversation (`client_stream_id`, `chat_conversations.uuid`) the
    /// turn belongs to: its Office host is confined to that conversation's
    /// files in the platform's system buckets.
    #[must_use]
    pub(crate) fn conversation_id(&self) -> &str {
        self.projection.stream_id()
    }

    /// #1831 (ADR-0050 decision 9d): show the model, before each message a
    /// `spawn_agent` excerpt may quote, its short id
    /// ([`elitea_agent_runtime::subagents::message_ref`]), and answer what
    /// the model then reads of each: the short id, the chat message uuid
    /// it stands for and the message's text after the id line. The messages
    /// are the history's user-role entries (the person's messages and every
    /// other participant's, enveloped) and this turn's question. The
    /// reader's own answers carry no id.
    ///
    /// Only the worker's copy changes, before the session is seeded or
    /// reconciled, never Main's history or its context-view digest. The id
    /// comes from the message's uuid alone, so a message has the same bytes
    /// on every turn and the provider prompt cache still matches. Called
    /// only when the sub-agent tools are bound.
    pub(crate) fn tag_message_refs(&mut self) -> Vec<(String, String, String)> {
        self.tags_messages = true;
        let mut view = Vec::new();
        if self.chat_history_refs.len() == self.chat_history.len() {
            for (reference, content) in self.chat_history_refs.iter().zip(&mut self.chat_history) {
                if content.role != "user" {
                    continue;
                }
                let short = short_message_ref(&reference.message_id);
                if let Some(text) = tag_message(content, &short, usize::MAX) {
                    view.push((short, reference.message_id.clone(), text));
                }
            }
        }
        if let Some(id) = &self.current_message_id {
            let short = short_message_ref(id);
            // The first part is the person's text (`ordinary_user_content`);
            // the attachment chunks after it are not the message's own.
            if let Some(text) = tag_message(&mut self.user_content, &short, 1) {
                view.push((short, id.clone(), text));
            }
        }
        view
    }

    /// What this turn binds the reader's durable session to (#1672, #1865,
    /// #1902): the context view mode and settings, and whether every
    /// quotable message the session holds after this turn's question carries
    /// its id.
    fn history_binding(&self) -> HistoryBinding {
        // A view is bound only with a reference per entry: without them the
        // session could never record it and would re-seed every turn.
        let referenced = self.chat_history_refs.len() == self.chat_history.len();
        HistoryBinding {
            view: self.context_view_history.clone().filter(|_| referenced),
            settings: self.context_view_binding.clone().filter(|_| referenced),
            fresh: self.writes_question,
            tagging: self.tags_messages,
            // Only a regeneration whose Main did not name its question
            // (before #1903) asks a quotable question again without its id.
            // An addressed agent's turn has no id either, but its user
            // content is Main's note, not a message; the person's question is
            // in its tagged history.
            tagged: self.tags_messages
                && !(self.writes_question && self.regenerate && self.current_message_id.is_none()),
        }
    }

    /// `attachments` is the RESOLVED chunk list for this turn — see
    /// [`ordinary_user_content`]. Callers that performed no read pass
    /// `&request.payload.input_attachments`, which renders exactly what
    /// admission stored.
    pub(crate) fn from_authorized(
        request: &AgentExecutionRequest,
        profile: &OrdinaryNoToolProfile,
        binding: &AuthorizedNativeCommandBinding,
        attachments: &[Value],
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::from_authorized_mode(request, profile, binding, attachments, None, false)
    }

    pub(crate) fn from_authorized_output_continuation(
        request: &AgentExecutionRequest,
        profile: &OrdinaryNoToolProfile,
        binding: &AuthorizedNativeCommandBinding,
        attachments: &[Value],
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::from_authorized_mode(request, profile, binding, attachments, None, true)
    }

    pub(crate) fn from_authorized_pipeline(
        request: &AgentExecutionRequest,
        profile: &OrdinaryNoToolProfile,
        binding: &AuthorizedNativeCommandBinding,
        attachments: &[Value],
        should_continue: bool,
        hitl_resume: bool,
    ) -> Result<Self, NativeAgentAssemblyError> {
        Self::from_authorized_mode(
            request,
            profile,
            binding,
            attachments,
            Some((should_continue, hitl_resume)),
            false,
        )
    }

    #[allow(clippy::too_many_lines)] // One linear projection of the request onto the plan's fields.
    fn from_authorized_mode(
        request: &AgentExecutionRequest,
        profile: &OrdinaryNoToolProfile,
        binding: &AuthorizedNativeCommandBinding,
        attachments: &[Value],
        pipeline_resume: Option<(bool, bool)>,
        output_continuation: bool,
    ) -> Result<Self, NativeAgentAssemblyError> {
        let user_text = match &request.payload.user_input {
            UserInput::Text(text) => text.clone(),
            UserInput::ContentBlocks(_) => return Err(invalid_input()),
        };
        let writes_question = writes_own_question(request, output_continuation, pipeline_resume);
        let thread_id = request
            .payload
            .thread_id
            .as_ref()
            .ok_or_else(invalid_input)?
            .clone();
        let user_id = UserId::new(stable_identity(
            USER_ID_DOMAIN,
            &[&binding.tenant_id, &binding.principal_ref],
        ))
        .map_err(|_| invalid_configuration())?;
        let session_id = SessionId::new(stable_identity(
            SESSION_ID_DOMAIN,
            &[
                &binding.tenant_id,
                &binding.resource_project_id,
                &binding.projection_project_id,
                &thread_id,
            ],
        ))
        .map_err(|_| invalid_configuration())?;
        let application_details = public_application_details(&request.payload.application)?;
        let execution_generation = request
            .payload
            .execution_generation
            .clone()
            .unwrap_or_else(|| binding.generation.to_string());
        let projection_input = OrdinaryProjectionInput {
            stream_id: binding.client_stream_id.clone(),
            message_id: binding.client_message_id.clone(),
            execution_generation,
            sio_event: binding.sio_event.clone(),
            thread_id: thread_id.clone(),
            project_id: current_numeric_identity(&binding.resource_project_id),
            chat_project_id: current_numeric_identity(&binding.projection_project_id),
            root_agent_name: ROOT_AGENT_NAME.to_owned(),
            model_name: profile.model_name().to_owned(),
            application_details,
            should_continue: output_continuation,
            continuation_prefix: output_continuation
                .then(|| request.payload.truncated_content.clone())
                .flatten(),
        };
        let projection = if let Some((should_continue, hitl_resume)) = pipeline_resume {
            AgentEventProjectionContext::pipeline(PipelineProjectionInput {
                ordinary: projection_input,
                checkpoint_thread_id: session_id.to_string(),
                should_continue,
                hitl_resume,
            })
        } else {
            AgentEventProjectionContext::ordinary(projection_input)
        }
        .map_err(projection_configuration)?;
        let capability_id = match request.kind {
            super::request::AgentExecutionKind::Application => APPLICATION_CAPABILITY_ID,
            super::request::AgentExecutionKind::Adhoc => ADHOC_CAPABILITY_ID,
        };
        let projection =
            projection.with_instruction_skills(profile.instruction_plan().public_active());
        // #606: the turn's own attachments are spliced into the human message
        // after the user's text by `ordinary_user_content`.
        let user_content =
            ordinary_user_content(request, output_continuation, user_text, attachments);
        Ok(Self {
            user_id,
            session_id,
            user_content,
            generation_config: GenerateContentConfig {
                temperature: profile.temperature(),
                max_output_tokens: profile
                    .max_tokens()
                    .and_then(|value| i32::try_from(value).ok()),
                ..GenerateContentConfig::default()
            },
            max_iterations: profile.step_limit(),
            projection,
            thread_id,
            chat_history: profile.chat_history().to_vec(),
            chat_history_refs: request.payload.chat_history_refs.clone(),
            current_message_id: current_message_id(request, writes_question),
            context_view_history: request.payload.context_view_history.clone(),
            context_view_binding: request.payload.context_view_binding.clone(),
            writes_question,
            tags_messages: false,
            context_management: profile.context_management(),
            step_compaction: profile.step_compaction(),
            capability_id,
            definition_digest: stable_definition_digest(request)?,
            tenant_id: binding.tenant_id.clone(),
            resource_project_id: binding.resource_project_id.clone(),
            projection_project_id: binding.projection_project_id.clone(),
            execution_id: binding.execution_id.clone(),
            generation: binding.generation,
            regenerate: request.payload.is_regenerate,
            instruction_plan: profile.instruction_plan().clone(),
            deadline_unix_millis: binding.deadline_unix_millis,
            dispatch_abort: super::dispatch_journal::DispatchAbort::default(),
            paused_writer: None,
            step_limit_stop: binding.step_limit_stop(),
        })
    }

    /// The fenced root-session writer for this execution's dispatch records.
    pub(super) fn dispatch_journal(
        &self,
        sessions: Arc<dyn SessionService>,
    ) -> Result<super::dispatch_journal::DispatchJournal, NativeAgentAssemblyError> {
        Ok(super::dispatch_journal::DispatchJournal::new(
            sessions,
            self.session_identity()?,
            super::dispatch_journal::DispatchOwner::new(self.execution_id.clone(), self.generation),
            self.dispatch_abort.clone(),
        ))
    }

    pub(super) fn session_id(&self) -> &str {
        self.session_id.as_ref()
    }

    /// The root session's identity, the one the Runner and the direct HITL
    /// resume read and write.
    pub(super) fn session_identity(&self) -> Result<AdkIdentity, NativeAgentAssemblyError> {
        let app_name = adk_rust::AppName::new(APP_NAME).map_err(|_| invalid_configuration())?;
        Ok(AdkIdentity::new(
            app_name,
            self.user_id.clone(),
            self.session_id.clone(),
        ))
    }

    pub(super) fn resource_project_id(&self) -> &str {
        &self.resource_project_id
    }

    #[cfg(test)]
    pub(super) fn user_id(&self) -> &str {
        self.user_id.as_ref()
    }

    #[cfg(test)]
    pub(super) fn user_text(&self) -> String {
        self.user_content
            .parts
            .iter()
            .filter_map(|part| match part {
                adk_rust::Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect()
    }

    #[cfg(test)]
    pub(super) const fn definition_digest(&self) -> [u8; 32] {
        self.definition_digest
    }
}

/// Runtime-selected ADK session persistence behind the common Runner entrypoint.
///
/// The `PostgreSQL` variant is constructible only inside the worker crate and
/// still requires the one-use claim-bound session grant at invocation time.
/// Selecting a backend never grants output, settlement, or tool authority.
pub(crate) enum NativeSessionBackend {
    InvocationLocal,
    Postgres {
        pool: PgPool,
        limits: SessionLimits,
    },
    #[cfg(test)]
    Injected(Arc<dyn SessionService>),
}

impl NativeSessionBackend {
    #[must_use]
    pub(crate) const fn invocation_local() -> Self {
        Self::InvocationLocal
    }

    #[must_use]
    pub(crate) const fn postgres(pool: PgPool, limits: SessionLimits) -> Self {
        Self::Postgres { pool, limits }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn injected(service: Arc<dyn SessionService>) -> Self {
        Self::Injected(service)
    }

    #[must_use]
    pub(crate) const fn name(&self) -> &'static str {
        match self {
            Self::InvocationLocal => "invocation_local",
            Self::Postgres { .. } => "postgres",
            #[cfg(test)]
            Self::Injected(_) => "injected",
        }
    }

    /// Whether this backend can restore the exact session that emitted HITL.
    #[must_use]
    pub(crate) const fn supports_resume(&self) -> bool {
        match self {
            Self::InvocationLocal => false,
            Self::Postgres { .. } => true,
            #[cfg(test)]
            Self::Injected(_) => true,
        }
    }

    /// Read and validate recovery state before runtime credential redemption.
    /// This path does not construct a model, toolset, Runner, or new user turn.
    pub(crate) async fn inspect_model_checkpoint(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
    ) -> Result<ValidatedModelCheckpoint, NativeAgentAssemblyError> {
        if plan.regenerate || !self.supports_resume() {
            return Err(invalid_configuration());
        }
        let sessions = self.open(authority, state_writer_lease, plan).await?;
        let session = sessions
            .get(GetRequest {
                app_name: APP_NAME.to_owned(),
                user_id: plan.user_id.to_string(),
                session_id: plan.session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await
            .map_err(|_| dependency_unavailable())?;
        super::model_checkpoint::ModelCheckpointWriter::new(
            sessions,
            plan.execution_id.clone(),
            plan.generation,
            plan.definition_digest,
        )
        .inspect(
            session.as_ref(),
            matches!(plan.context_management, ContextManagementPlan::Summarize(_)),
        )
        .map_err(|_| invalid_configuration())?
        .ok_or_else(invalid_configuration)
    }

    pub(crate) async fn open(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
    ) -> Result<Arc<dyn SessionService>, NativeAgentAssemblyError> {
        self.open_with_model_scopes(authority, state_writer_lease, plan)
            .await
            .map(|(sessions, ..)| sessions)
    }

    pub(super) async fn open_with_model_scopes(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
    ) -> Result<
        (
            Arc<dyn SessionService>,
            super::model_scope::ModelScopeSessions,
            Option<Arc<PostgresSessionService>>,
        ),
        NativeAgentAssemblyError,
    > {
        let claim = authority.into_writer_binding();
        if claim.tenant_id != plan.tenant_id
            || claim.resource_project_id != plan.resource_project_id
            || claim.projection_project_id != plan.projection_project_id
            || claim.execution_id != plan.execution_id
            || claim.generation != plan.generation
        {
            return Err(authorization_failed());
        }
        match self {
            Self::InvocationLocal => {
                drop(claim);
                let service: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
                let scopes = plan.model_scope_sessions(
                    super::model_scope::ModelScopeBackend::Local(service.clone()),
                );
                Ok((service, scopes, None))
            }
            Self::Postgres { pool, limits } => {
                let resource_project_id = claim
                    .resource_project_id
                    .parse::<i32>()
                    .ok()
                    .filter(|value| *value > 0)
                    .ok_or_else(invalid_configuration)?;
                let projection_project_id = claim
                    .projection_project_id
                    .parse::<i32>()
                    .ok()
                    .filter(|value| *value > 0)
                    .ok_or_else(invalid_configuration)?;
                let fence_token: [u8; 32] = claim
                    .fence_token
                    .as_slice()
                    .try_into()
                    .map_err(|_| invalid_configuration())?;
                let writer = SessionWriterAuthority::new(
                    claim.tenant_id,
                    resource_project_id,
                    projection_project_id,
                    plan.capability_id,
                    plan.definition_digest,
                    plan.thread_id.clone(),
                    APP_NAME.to_owned(),
                    plan.user_id.to_string(),
                    plan.session_id.to_string(),
                    claim.execution_id,
                    claim.generation,
                    claim.claim_id,
                    claim.claim_attempt,
                    claim.lease_epoch,
                    claim.claim_started_at_unix_micros,
                    claim.workload_session_id,
                    claim.producer_id,
                    fence_token,
                )
                .map_err(|error| session_activation_error(&error))?;
                let service = PostgresSessionService::activate(
                    pool.clone(),
                    writer,
                    *limits,
                    state_writer_lease,
                )
                .await
                .map_err(|error| session_activation_error(&error))?;
                let service = Arc::new(service);
                let scopes = plan.model_scope_sessions(
                    super::model_scope::ModelScopeBackend::Postgres(service.clone()),
                );
                Ok((service.clone(), scopes, Some(service)))
            }
            #[cfg(test)]
            Self::Injected(service) => {
                drop(claim);
                let scopes = plan.model_scope_sessions(
                    super::model_scope::ModelScopeBackend::Local(service.clone()),
                );
                Ok((Arc::clone(service), scopes, None))
            }
        }
    }
}

/// One storage profile that activates graph conversation and frontier state.
///
/// A stored pipeline cannot use invocation-local state because its first node
/// may pause. The `PostgreSQL` variant consumes one claim and derives both
/// writers from that same immutable fence. No additional interrupt table or
/// caller-selected checkpoint identity is introduced.
pub(crate) enum NativePipelineStateBackend {
    Postgres {
        pool: PgPool,
        session_limits: SessionLimits,
        checkpoint_limits: CheckpointLimits,
        /// The private content client that carries the claim-fenced fan-out
        /// decision routes (the node-recovery control transport).
        fanout_decisions: Option<Arc<crate::transport::InputContentClient>>,
    },
    #[cfg(test)]
    Injected {
        sessions: Arc<dyn SessionService>,
        checkpointer: Arc<dyn Checkpointer>,
    },
}

impl NativePipelineStateBackend {
    #[must_use]
    pub(crate) const fn postgres(
        pool: PgPool,
        session_limits: SessionLimits,
        checkpoint_limits: CheckpointLimits,
    ) -> Self {
        Self::Postgres {
            pool,
            session_limits,
            checkpoint_limits,
            fanout_decisions: None,
        }
    }

    /// Lend the content client the fan-out decision routes use. Each claim
    /// then gets its own claim-fenced decision source in its state services.
    #[must_use]
    pub(crate) fn with_fanout_decisions(
        mut self,
        client: Arc<crate::transport::InputContentClient>,
    ) -> Self {
        match &mut self {
            Self::Postgres {
                fanout_decisions, ..
            } => *fanout_decisions = Some(client),
            #[cfg(test)]
            Self::Injected { .. } => {}
        }
        self
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn injected(
        sessions: Arc<dyn SessionService>,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Self {
        Self::Injected {
            sessions,
            checkpointer,
        }
    }

    pub(super) async fn open(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
        pipeline_definition: &PipelineDefinition,
    ) -> Result<PipelineStateServices, NativeAgentAssemblyError> {
        let paths = pipeline_definition
            .application_node_ids()
            .chain(pipeline_definition.parallel_llm_branch_ids())
            .map(str::to_owned)
            .collect::<Vec<_>>();
        self.open_with_application_paths(
            authority,
            state_writer_lease,
            plan,
            pipeline_definition,
            &paths,
        )
        .await
    }

    pub(super) async fn open_with_application_paths(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
        pipeline_definition: &PipelineDefinition,
        application_paths: &[String],
    ) -> Result<PipelineStateServices, NativeAgentAssemblyError> {
        let claim = authority.into_writer_binding();
        if claim.tenant_id != plan.tenant_id
            || claim.resource_project_id != plan.resource_project_id
            || claim.projection_project_id != plan.projection_project_id
            || claim.execution_id != plan.execution_id
            || claim.generation != plan.generation
        {
            return Err(authorization_failed());
        }
        match self {
            Self::Postgres {
                pool,
                session_limits,
                checkpoint_limits,
                fanout_decisions,
            } => {
                activate_pipeline_postgres(
                    pool,
                    (*session_limits, *checkpoint_limits),
                    fanout_decisions.as_ref(),
                    claim,
                    state_writer_lease,
                    plan,
                    pipeline_definition,
                    application_paths,
                )
                .await
            }
            #[cfg(test)]
            Self::Injected {
                sessions,
                checkpointer,
            } => {
                drop(claim);
                drop(state_writer_lease);
                Ok(PipelineStateServices {
                    event_source: None,
                    sessions: Arc::clone(sessions),
                    checkpointer: Arc::clone(checkpointer),
                    parallel_authority: None,
                    node_recovery_authority: None,
                    fanout_decisions: None,
                    model_scopes: plan.model_scope_sessions(
                        super::model_scope::ModelScopeBackend::Local(sessions.clone()),
                    ),
                })
            }
        }
    }
}

/// This claim's fenced decision source. The ledger routes accept only an
/// ordinary claim of this exact execution; an identity outside the route
/// grammar leaves the source unavailable, and the fan-out composition refuses
/// the run instead of fetching.
fn claim_decision_source(
    client: Option<&Arc<crate::transport::InputContentClient>>,
    claim: &SessionWriterClaimBinding,
    fence_token: &[u8; 32],
) -> Option<Arc<crate::execution::fanout_decisions::FanoutDecisionClient>> {
    client.and_then(|client| {
        crate::execution::fanout_decisions::FanoutDecisionClient::for_claim(
            Arc::clone(client),
            &claim.execution_id,
            claim.generation,
            &claim.claim_id,
            fence_token,
        )
        .ok()
        .map(Arc::new)
    })
}

#[allow(clippy::too_many_arguments)] // Keep claim, checkpoint, continuation proof, and Runner assembly visibly ordered.
async fn activate_pipeline_postgres(
    pool: &PgPool,
    (session_limits, checkpoint_limits): (SessionLimits, CheckpointLimits),
    fanout_decisions: Option<&Arc<crate::transport::InputContentClient>>,
    claim: SessionWriterClaimBinding,
    state_writer_lease: Arc<dyn StateWriterLease>,
    plan: &OrdinaryNativeAgentPlan,
    pipeline_definition: &PipelineDefinition,
    application_paths: &[String],
) -> Result<PipelineStateServices, NativeAgentAssemblyError> {
    let resource_project_id = claim
        .resource_project_id
        .parse::<i32>()
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(invalid_configuration)?;
    let projection_project_id = claim
        .projection_project_id
        .parse::<i32>()
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(invalid_configuration)?;
    let fence_token: [u8; 32] = claim
        .fence_token
        .as_slice()
        .try_into()
        .map_err(|_| invalid_configuration())?;
    let fanout_decisions = claim_decision_source(fanout_decisions, &claim, &fence_token);
    let session_writer = SessionWriterAuthority::new(
        claim.tenant_id.clone(),
        resource_project_id,
        projection_project_id,
        plan.capability_id,
        plan.definition_digest,
        plan.thread_id.clone(),
        APP_NAME.to_owned(),
        plan.user_id.to_string(),
        plan.session_id.to_string(),
        claim.execution_id.clone(),
        claim.generation,
        claim.claim_id.clone(),
        claim.claim_attempt,
        claim.lease_epoch,
        claim.claim_started_at_unix_micros,
        claim.workload_session_id.clone(),
        claim.producer_id.clone(),
        fence_token,
    )
    .map_err(|error| session_activation_error(&error))?;
    let checkpoint_writer = CheckpointWriterAuthority::new(
        claim.tenant_id,
        resource_project_id,
        projection_project_id,
        plan.capability_id,
        pipeline_definition.definition_digest(),
        plan.session_id.to_string(),
        claim.execution_id,
        claim.generation,
        claim.claim_id,
        claim.claim_attempt,
        claim.lease_epoch,
        claim.claim_started_at_unix_micros,
        claim.workload_session_id,
        claim.producer_id,
        fence_token,
    )
    .map_err(|error| checkpoint_activation_error(&error))?;
    let sessions = PostgresSessionService::activate(
        pool.clone(),
        session_writer,
        session_limits,
        Arc::clone(&state_writer_lease),
    )
    .await
    .map_err(|error| session_activation_error(&error))?
    .with_replay_equivalence(super::pipeline::scoped_applications::canonical_replay_form);
    let checkpointer = PostgresCheckpointer::activate(
        pool.clone(),
        checkpoint_writer,
        checkpoint_limits,
        state_writer_lease,
    )
    .await
    .map_err(|error| checkpoint_activation_error(&error))?;
    let checkpointer = checkpointer
        .with_application_paths(application_paths)
        .await
        .map_err(|error| checkpoint_activation_error(&error))?;
    let sessions = Arc::new(sessions);
    let model_scopes = plan.model_scope_sessions(super::model_scope::ModelScopeBackend::Postgres(
        sessions.clone(),
    ));
    let holder = Arc::new(checkpointer);
    let node_recovery_authority: Arc<dyn super::graph::node_recovery_runtime::NodeRecoveryFactory> =
        holder.clone();
    let authority: Arc<dyn super::graph::ParallelCheckpointAuthority> = holder;
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    Ok(PipelineStateServices {
        event_source: Some(Arc::clone(&sessions)),
        sessions,
        checkpointer,
        parallel_authority: Some(authority),
        node_recovery_authority: Some(node_recovery_authority),
        fanout_decisions,
        model_scopes,
    })
}

pub(super) struct PipelineStateServices {
    sessions: Arc<dyn SessionService>,
    /// FU-287: the durable root session, when the Runner refreshes it by ordinal.
    event_source: Option<Arc<PostgresSessionService>>,
    checkpointer: Arc<dyn Checkpointer>,
    parallel_authority: Option<Arc<dyn super::graph::ParallelCheckpointAuthority>>,
    node_recovery_authority:
        Option<Arc<dyn super::graph::node_recovery_runtime::NodeRecoveryFactory>>,
    /// This claim's fenced decision source, when the deployment lends the
    /// content client. The fan-out composition plugs it into Parallel.
    fanout_decisions: Option<Arc<crate::execution::fanout_decisions::FanoutDecisionClient>>,
    pub(super) model_scopes: super::model_scope::ModelScopeSessions,
}

impl PipelineStateServices {
    pub(super) async fn inspect_node_recovery(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        definition: &PipelineDefinition,
        receipt: &super::graph::node_recovery_receipt::NodeRecoveryRequiredReceipt,
    ) -> Result<super::node_recovery_checkpoint::OpenedNodeRecoveryVisit, NativeAgentAssemblyError>
    {
        use super::graph::node_recovery_runtime::NodeAttemptActivation;
        use adk_rust::graph::{ExecutionConfig, NodeContext};
        let checkpoint = self
            .checkpointer
            .load(plan.session_id.as_str())
            .await
            .map_err(|_| dependency_unavailable())?
            .ok_or_else(invalid_configuration)?;
        if !receipt.validate()
            || receipt.graph_thread != plan.session_id.as_str()
            || checkpoint.thread_id != receipt.graph_thread
            || checkpoint.step as u64 != receipt.step
            || checkpoint.pending_nodes.as_slice() != [receipt.node_id.as_str()]
            || checkpoint.cleared_interrupt.is_some()
            || checkpoint.metadata.get("elitea.pipeline.execution.v1")
                != Some(&serde_json::json!([plan.execution_id, plan.generation]))
        {
            return Err(invalid_configuration());
        }
        let (node_digest, policy) = definition
            .node_recovery_spec(&receipt.node_id)
            .ok_or_else(invalid_configuration)?;
        let context = NodeContext::new(
            checkpoint.state,
            ExecutionConfig::new(&receipt.graph_thread),
            checkpoint.step,
        );
        let activation =
            NodeAttemptActivation::from_context(&receipt.node_id, node_digest, &context)
                .map_err(|_| invalid_configuration())?;
        let journal = self
            .node_recovery_authority
            .as_ref()
            .ok_or_else(invalid_configuration)?
            .open(&activation, &policy)
            .await
            .map_err(|_| invalid_configuration())?;
        journal
            .verify_pending_receipt(&activation, receipt)
            .await
            .map_err(|_| invalid_configuration())?;
        // A node recovery resumes this execution's own node attempt only.
        let (evidence, _) = self
            .inspect_checkpoint(plan, definition, None, &BTreeSet::new())
            .await?;
        let projector = definition
            .node_result_projector(&receipt.node_id, &plan.execution_id, plan.generation)
            .map_err(|_| invalid_configuration())?;
        Ok(
            super::node_recovery_checkpoint::OpenedNodeRecoveryVisit::inspected(
                activation,
                journal,
                evidence,
                receipt.clone(),
                context,
                projector,
            ),
        )
    }

    pub(super) fn node_recovery_authority(
        &self,
    ) -> Option<Arc<dyn super::graph::node_recovery_runtime::NodeRecoveryFactory>> {
        self.node_recovery_authority.clone()
    }
    /// This claim's decision source for the fan-out runner.
    pub(super) fn fanout_decisions(
        &self,
    ) -> Option<Arc<crate::execution::fanout_decisions::FanoutDecisionClient>> {
        self.fanout_decisions.clone()
    }

    /// Preserve the single `PostgreSQL` capability holder before trait erasure.
    pub(super) fn parallel_authority(
        &self,
    ) -> Option<Arc<dyn super::graph::ParallelCheckpointAuthority>> {
        self.parallel_authority.clone()
    }

    /// Root-only read through the already admitted exact-thread checkpointer owner.
    pub(super) async fn application_checkpoint(
        &self,
        plan: &OrdinaryNativeAgentPlan,
    ) -> Result<Option<adk_rust::graph::Checkpoint>, NativeAgentAssemblyError> {
        self.checkpointer
            .load(plan.session_id())
            .await
            .map_err(|_| dependency_unavailable())
    }

    /// The recovery walk below one frontier Agent node (FU-249, PM-2): it
    /// returns the first-level child row the evidence binds. Deeper rows cannot
    /// be read before assembly (their paths are admitted with the composition),
    /// so the assembly-time walk (`validates`) checks every deeper level against
    /// the frozen definitions and fails closed with a distinct cause: each
    /// deeper row's activation stamp names its thread and the Agent node
    /// definition Main froze, and the leaf is parked on an unowned Parallel
    /// node of the frozen leaf definition.
    async fn walk_child_frontier(
        &self,
        mut thread: String,
        mut level: Option<(&PipelineDefinition, &dyn FrozenRecoveryLevel)>,
        validates: bool,
    ) -> Result<serde_json::Value, NativeAgentAssemblyError> {
        use elitea_agent_runtime::graph::application_activation as activation;
        let mut bound = None;
        // The digest of the Agent node that stamps the next level's rows.
        let mut stamped_by = None;
        for depth in 0..MAX_RECOVERY_LEVELS {
            let Some(child) = self
                .checkpointer
                .load(&thread)
                .await
                .map_err(|_| dependency_unavailable())?
            else {
                // FU-248: a child lost inside its first call has saved no row.
                // Only a child that starts at a direct toolkit/MCP node may
                // resume from there: that call's visit-scoped journal decides
                // it. Before assembly the first level's absence is bound as is
                // and assembly decides with the frozen definition.
                // W3-2b: the same for a saved child the root's Agent node runs
                // that starts at an unowned Code node: its attempt's journal,
                // scoped to this visit, decides it.
                if validates
                    && level.is_some_and(|(definition, _)| {
                        definition.starts_at_direct_tool()
                            || depth == 0 && definition.starts_at_code()
                    })
                {
                    return Ok(bound.unwrap_or(serde_json::Value::Null));
                }
                if !validates && depth == 0 {
                    return Ok(serde_json::Value::Null);
                }
                return Err(child_refused(CHILD_MISSING));
            };
            if depth == 0 {
                bound = Some(serde_json::to_value(&child).map_err(|_| invalid_configuration())?);
            } else if !child
                .metadata
                .contains_key(activation::ACTIVATION_METADATA_KEY)
            {
                return Err(child_refused(CHILD_UNSTAMPED));
            } else if validates
                && (stamped_by.is_none()
                    || activation::stamped_definition_digest(&child.metadata, &thread)
                        != stamped_by)
            {
                return Err(child_refused(CHILD_DEFINITION_MISMATCH));
            }
            if super::graph::holds_parallel_occurrence(&child) {
                if validates
                    && !level.is_some_and(|(definition, _)| {
                        definition.is_unowned_fanout(&child.pending_nodes[0])
                    })
                {
                    return Err(child_refused(CHILD_FRONTIER_UNSUPPORTED));
                }
                return bound.ok_or_else(invalid_configuration);
            }
            let [pending] = child.pending_nodes.as_slice() else {
                return Err(child_refused(CHILD_FRONTIER_UNSUPPORTED));
            };
            if !validates {
                // Before assembly: one pending node is all the structure this
                // inspect can see; assembly walks it to the occurrence.
                return bound.ok_or_else(invalid_configuration);
            }
            let (child_definition, child_level) =
                level.ok_or_else(|| child_refused(CHILD_DEFINITION_MISMATCH))?;
            if !child_definition.has_node(pending) {
                return Err(child_refused(CHILD_DEFINITION_MISMATCH));
            }
            // FU-248: a child lost inside a direct toolkit/MCP node's call is a
            // leaf too. Its journal, scoped to this visit, decides the call:
            // a committed result replays, a started call ends typed and is
            // never repeated, a call that never started runs once.
            if child_definition.is_unowned_direct_tool(pending) {
                return bound.ok_or_else(invalid_configuration);
            }
            // W3-2b: a saved child the root's Agent node runs, lost inside an
            // unowned Code node's attempt, is a leaf too: the attempt's journal,
            // scoped to this visit, re-attaches (R), re-runs a lost runtime
            // once (I), reconciles from owner proof (C) or ends typed, never
            // repeated. Only at the first level: deeper Code is refused (B4).
            if depth == 0 && child_definition.is_unowned_code(pending) {
                return bound.ok_or_else(invalid_configuration);
            }
            let aliases = child_level.admitted_aliases();
            if !child_definition
                .recovery_frontier_supported(std::slice::from_ref(pending), &aliases)
            {
                return Err(child_refused(CHILD_FRONTIER_UNSUPPORTED));
            }
            // A supported ordinary node on the leaf: the child stopped outside
            // any Parallel occurrence, so nothing resumes blind.
            let alias = application_alias(child_definition, pending)
                .ok_or_else(|| child_refused(CHILD_NOT_PARKED))?;
            if !aliases.contains(alias) {
                return Err(child_refused(CHILD_DEFINITION_MISMATCH));
            }
            stamped_by = child_definition.application_config_digest(pending);
            level = child_level.child(alias);
            thread = format!("{thread}/{pending}");
        }
        Err(child_refused(CHILD_NOT_PARKED))
    }

    /// FU-265: whether `checkpoint`, the latest root row, is the paused row
    /// this continuation re-enters. One proof for the first attempt and the
    /// reclaim: the row is this execution's own (it saved already, `None`), or
    /// it was written by exactly the paused writer Main named, is a pause
    /// frontier on this run's root thread, and the root session's latest pause
    /// event names exactly that row (the restored session and that pause's
    /// index). The refusal logs ids and generations only.
    pub(super) async fn prove_continuation_row(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        paused: &super::graph::continuation_writer::PausedWriter,
        checkpoint: &adk_rust::graph::Checkpoint,
    ) -> Result<Option<(Box<dyn adk_rust::session::Session>, usize)>, NativeAgentAssemblyError>
    {
        let writer = checkpoint.metadata.get("elitea.pipeline.execution.v1");
        let refuse = |cause: &'static str| {
            tracing::warn!(
                event = "pipeline_recovery_frontier_refused",
                cause,
                execution_id = %plan.execution_id,
                generation = plan.generation,
                expected_execution_id = paused.execution_id(),
                expected_generation = paused.generation(),
                found_writer = %writer.map_or_else(String::new, ToString::to_string),
            );
            child_refused(cause)
        };
        if writer == Some(&serde_json::json!([plan.execution_id, plan.generation])) {
            return Ok(None);
        }
        if !paused.is_writer(writer) {
            return Err(refuse(ROOT_WRITER_MISMATCH));
        }
        if checkpoint.thread_id != plan.session_id.as_str()
            || checkpoint.pending_nodes.is_empty()
            || checkpoint.cleared_interrupt.is_some()
        {
            return Err(refuse(CONTINUATION_PAUSE_UNBOUND));
        }
        let session = restore_pipeline_session(self, plan).await?;
        let events = session.events().all();
        // The latest pause event names exactly this row: the paused execution's
        // frontier. Events after it are this continuation's own (its input and
        // records, `continuation_entry`); the continuation's resolver binds the
        // decision to the pause again when it runs.
        let pause = events.iter().rposition(|event| {
            adk_rust::graph::interrupt::GraphInterruptPayload::from_event(event).is_some()
        });
        let bound = pause
            .and_then(|index| {
                adk_rust::graph::interrupt::GraphInterruptPayload::from_event(&events[index])
            })
            .is_some_and(|pause| {
                pause.thread_id == plan.session_id.as_str()
                    && pause.checkpoint_id == checkpoint.checkpoint_id
            });
        match pause {
            Some(index) if bound => Ok(Some((session, index))),
            _ => Err(refuse(CONTINUATION_PAUSE_UNBOUND)),
        }
    }

    /// FU-269: how a continuation that re-enters the paused row its writer
    /// Main named starts, on the first attempt and on a reclaim. The row is
    /// proven (`prove_continuation_row`), then the events after the pause are
    /// read (`dispatch_journal::owned_continuation_suffix`, E5): none, a first
    /// attempt; this execution's own suffix, a re-run whose resolvers read the
    /// session ending at the pause (E6); anything else is refused typed.
    pub(super) async fn continuation_entry(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        paused: &super::graph::continuation_writer::PausedWriter,
    ) -> Result<ContinuationEntry, NativeAgentAssemblyError> {
        let checkpoint = self
            .checkpointer
            .load(plan.session_id.as_ref())
            .await
            .map_err(|_| dependency_unavailable())?
            .ok_or_else(|| child_refused(CONTINUATION_PAUSE_UNBOUND))?;
        let Some((session, pause)) = self
            .prove_continuation_row(plan, paused, &checkpoint)
            .await?
        else {
            // This execution saved its own row: an ordinary checkpoint resume.
            return Ok(ContinuationEntry::default());
        };
        let events = session.events().all();
        let owner =
            super::dispatch_journal::DispatchOwner::new(plan.execution_id.clone(), plan.generation);
        let mut owns = super::application_tools::ContinuationSuffix::default();
        // A text continuation's own input is its user event (E4).
        let own_input = serde_json::to_value(&plan.user_content).ok();
        let suffix =
            super::dispatch_journal::owned_continuation_suffix(&events, pause, &owner, |event| {
                owns.owns(event, |content| {
                    super::application_tools::is_pipeline_resume_marker(content)
                        || serde_json::to_value(content).ok() == own_input
                })
            })
            .map_err(|_| {
                tracing::warn!(
                    event = "pipeline_recovery_frontier_refused",
                    cause = CONTINUATION_SUFFIX_UNOWNED,
                    execution_id = %plan.execution_id,
                    generation = plan.generation,
                    expected_execution_id = paused.execution_id(),
                    expected_generation = paused.generation(),
                );
                child_refused(CONTINUATION_SUFFIX_UNOWNED)
            })?;
        let pause_event = &events[pause];
        let card = super::graph::resume::pause_interrupt_ids(
            pause_event,
            ROOT_AGENT_NAME,
            plan.session_id.as_ref(),
        )
        .and_then(|cards| cards.into_iter().next())
        .unwrap_or_else(|| checkpoint.checkpoint_id.clone());
        let continuing = Some(ContinuingPause {
            pause_id: pause_event.id.clone(),
            pause_invocation_id: pause_event.invocation_id.clone(),
            card,
        });
        Ok(match suffix {
            None => ContinuationEntry {
                session: Some(session),
                continuing,
                rerun: false,
                input_written: false,
            },
            Some(len) => ContinuationEntry {
                input_written: events[len..].iter().any(|event| event.author == "user"),
                session: Some(Box::new(super::graph::continuation_writer::PauseView::new(
                    session, len,
                ))),
                continuing,
                rerun: true,
            },
        })
    }

    pub(super) async fn inspect_checkpoint(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        definition: &PipelineDefinition,
        wake: Option<&super::graph::fanout_wake::FanoutWake>,
        frozen: &dyn FrozenRecoveryLevel,
    ) -> Result<(ValidatedModelCheckpoint, RecoveryFrontier), NativeAgentAssemblyError> {
        let checkpoint = self
            .checkpointer
            .load(plan.session_id.as_ref())
            .await
            .map_err(|_| dependency_unavailable())?
            .ok_or_else(invalid_configuration)?;
        let writer = checkpoint.metadata.get("elitea.pipeline.execution.v1");
        let frontier = if writer == Some(&serde_json::json!([plan.execution_id, plan.generation])) {
            RecoveryFrontier::Own
        } else if wake.is_some_and(|wake| wake.is_parked_writer(writer))
            && !checkpoint.pending_nodes.is_empty()
        {
            // A wake lost before its first root save: the latest root checkpoint
            // is still the park that the parked execution named by the wake
            // wrote, which it re-enters. Any other writer is refused.
            RecoveryFrontier::WakePark
        } else if let Some(paused) = plan.paused_writer.as_ref() {
            // FU-269: a continuation lost before its first root save. Its row
            // and the suffix it left are proven exactly as on the first attempt
            // (`continuation_entry`); it runs again as the continuation from
            // that row, and the journal and dispatch records decide each call.
            self.continuation_entry(plan, paused).await?;
            RecoveryFrontier::ContinuationPause
        } else {
            tracing::warn!(
                event = "pipeline_recovery_frontier_refused",
                cause = ROOT_WRITER_MISMATCH,
                execution_id = %plan.execution_id,
                generation = plan.generation,
                found_writer = %writer.map_or_else(String::new, ToString::to_string),
            );
            return Err(child_refused(ROOT_WRITER_MISMATCH));
        };
        if checkpoint.thread_id != plan.session_id.as_str()
            || checkpoint.cleared_interrupt.is_some()
        {
            return Err(invalid_configuration());
        }
        // The continuation re-enters its paused row itself: its resolver binds
        // the decision to that row, and no frontier is resumed blind.
        if frontier == RecoveryFrontier::ContinuationPause {
            return Ok((
                recovery_evidence(plan, &checkpoint, Vec::<Value>::new())?,
                frontier,
            ));
        }
        let aliases = frozen.admitted_aliases();
        if !definition.recovery_frontier_supported(&checkpoint.pending_nodes, &aliases) {
            // A root Agent node running a saved Agent is not a frontier
            // (FU-323 makes it one): refused typed and registered, ids only.
            if let Some(node) =
                definition.saved_agent_frontier_node(&checkpoint.pending_nodes, &aliases)
            {
                let code = elitea_faults::codes::PIPELINE_RECOVERY_SAVED_AGENT_FRONTIER;
                elitea_faults::fault!(
                    tracing::Level::WARN,
                    code = code,
                    event = "pipeline_recovery_frontier_refused",
                    execution_id = %plan.execution_id,
                    generation = plan.generation,
                    node_id = node,
                    support_ref = %plan.execution_id
                );
                return Err(invalid_configuration().with_cause(code.as_str(), None));
            }
            return Err(invalid_configuration());
        }
        // FU-249: an Agent node on the frontier resumes its saved child pipeline
        // only where the child is parked inside a fixed Parallel occurrence (its
        // branches resume from their own checkpoints and receipts). PM-2: a child
        // whose single pending node is itself an Agent node running a pipeline
        // Main froze is walked down, level by level, to that occurrence. Any
        // other child frontier, an effectful node included, is not resumed blind.
        // The first-level child row is bound into the evidence; deeper rows
        // cannot be read before assembly (their paths are admitted with the
        // composition), so the assembly-time walk checks every deeper level
        // against the frozen definitions and fails closed.
        let mut children = Vec::new();
        for node in definition.application_frontier_nodes(&checkpoint.pending_nodes) {
            let level = application_alias(definition, node).and_then(|alias| frozen.child(alias));
            children.push(
                self.walk_child_frontier(
                    format!("{}/{node}", checkpoint.thread_id),
                    level,
                    frozen.validates(),
                )
                .await?,
            );
        }
        Ok((recovery_evidence(plan, &checkpoint, children)?, frontier))
    }
}

/// Bind all frontier data, including graph state, attempts and child receipts.
/// Convert through Value to order map keys before hashing.
fn recovery_evidence<C: serde::Serialize>(
    plan: &OrdinaryNativeAgentPlan,
    checkpoint: &adk_rust::graph::Checkpoint,
    children: Vec<C>,
) -> Result<ValidatedModelCheckpoint, NativeAgentAssemblyError> {
    let value = serde_json::to_value(checkpoint).map_err(|_| invalid_configuration())?;
    let bytes = if children.is_empty() {
        serde_json::to_vec(&("elitea.pipeline.recovery.v1", plan.definition_digest, value))
    } else {
        serde_json::to_vec(&(
            "elitea.pipeline.recovery.v2",
            plan.definition_digest,
            value,
            children,
        ))
    }
    .map_err(|_| invalid_configuration())?;
    let digest = ring::digest::digest(&ring::digest::SHA256, &bytes);
    Ok(ValidatedModelCheckpoint::from_graph(
        plan.execution_id.clone(),
        plan.generation,
        digest
            .as_ref()
            .try_into()
            .map_err(|_| invalid_configuration())?,
    ))
}

/// FU-269: the pause a continuation's `continuing` record names (E3).
pub(super) struct ContinuingPause {
    pause_id: String,
    pause_invocation_id: String,
    /// One card of the pause (an Application pause's first card), else the
    /// paused row's checkpoint id.
    card: String,
}

/// FU-269: how a continuation that re-enters a paused row starts
/// (`PipelineStateServices::continuation_entry`). The default is a start
/// without a named paused writer, unchanged.
#[derive(Default)]
pub(super) struct ContinuationEntry {
    /// The root session the row proof restored (E7): the start reads it
    /// instead of restoring it again. On a re-run it ends at the pause (E6).
    session: Option<Box<dyn adk_rust::session::Session>>,
    /// The pause this continuation's `continuing` record names.
    continuing: Option<ContinuingPause>,
    /// A reclaim re-running behind its own owner-proven suffix: its
    /// `continuing` record is already in place.
    rerun: bool,
    /// The suffix already holds this continuation's user event (E4): the
    /// Runner adds none.
    input_written: bool,
}

/// One level of the Main-frozen saved-pipeline composition a recovery walk may
/// descend through (FU-249; PM-2: a Parallel below the first Agent-node level).
pub(crate) trait FrozenRecoveryLevel: Send + Sync {
    /// Whether the walk can check node kinds against frozen definitions here.
    /// Before assembly only the first-level child is readable; assembly walks
    /// every level against the frozen definitions (the same evidence, or the
    /// run is refused).
    fn validates(&self) -> bool {
        true
    }
    /// The aliases of the saved pipelines Main froze at this level.
    fn admitted_aliases(&self) -> BTreeSet<String>;
    /// The frozen saved pipeline `alias` runs: its definition and its own level.
    fn child(&self, alias: &str) -> Option<(&PipelineDefinition, &dyn FrozenRecoveryLevel)>;
}

/// Aliases only (before assembly): no frozen child definition to check.
impl FrozenRecoveryLevel for BTreeSet<String> {
    fn validates(&self) -> bool {
        false
    }

    fn admitted_aliases(&self) -> BTreeSet<String> {
        self.clone()
    }

    fn child(&self, _alias: &str) -> Option<(&PipelineDefinition, &dyn FrozenRecoveryLevel)> {
        None
    }
}

/// The most saved-pipeline levels a recovery walk descends (the composition
/// depth bound).
const MAX_RECOVERY_LEVELS: usize = super::pipeline::composition::MAX_PIPELINE_COMPOSITION_DEPTH;

/// A recovery walk refusal (PM-2): typed, with a distinct cause per check.
const CHILD_MISSING: &str = "pipeline.recovery.child_missing";
const CHILD_UNSTAMPED: &str = "pipeline.recovery.child_unstamped";
const CHILD_FRONTIER_UNSUPPORTED: &str = "pipeline.recovery.child_frontier_unsupported";
const CHILD_DEFINITION_MISMATCH: &str = "pipeline.recovery.child_definition_mismatch";
const CHILD_NOT_PARKED: &str = "pipeline.recovery.child_not_parked";
/// FU-265: the latest root row is neither this execution's nor the paused
/// writer its continuation names.
const ROOT_WRITER_MISMATCH: &str = "pipeline.recovery.root_writer_mismatch";
/// FU-265: the named paused row is not this continuation's pause.
const CONTINUATION_PAUSE_UNBOUND: &str = "pipeline.recovery.continuation_pause_unbound";
/// FU-269: the events after the paused row's pause are not this
/// continuation's own owner-proven suffix (`owned_continuation_suffix`).
const CONTINUATION_SUFFIX_UNOWNED: &str = "pipeline.recovery.continuation_suffix_unowned";

fn child_refused(cause: &'static str) -> NativeAgentAssemblyError {
    invalid_configuration().with_cause(cause, None)
}

/// Whether `code` is one of the recovery walk's refusals (PM-2).
pub(crate) fn is_recovery_frontier_cause(code: &str) -> bool {
    [
        CHILD_MISSING,
        CHILD_UNSTAMPED,
        CHILD_FRONTIER_UNSUPPORTED,
        CHILD_DEFINITION_MISMATCH,
        CHILD_NOT_PARKED,
        ROOT_WRITER_MISMATCH,
        CONTINUATION_PAUSE_UNBOUND,
        CONTINUATION_SUFFIX_UNOWNED,
    ]
    .contains(&code)
}

/// The Agent node `node`'s saved-participant alias in `definition`.
fn application_alias<'a>(definition: &'a PipelineDefinition, node: &str) -> Option<&'a str> {
    definition
        .application_nodes()
        .find(|(id, _)| *id == node)
        .map(|(_, selection)| selection.alias())
}

/// Where a pipeline's model-checkpoint recovery resumes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum RecoveryFrontier {
    /// This execution's own latest root checkpoint.
    Own,
    /// A fan-out wake whose process was lost before its first root save: it
    /// runs again as the wake from the park it was admitted against (class I:
    /// the decision ledger and the children's receipts deduplicate it).
    WakePark,
    /// A continuation whose process was lost before its first root save
    /// (FU-269): it runs again as the continuation from the paused row its
    /// writer Main named, behind its own owner-proven suffix. Each call's
    /// journal and dispatch records decide it (R / C / I).
    ContinuationPause,
}

/// One bound provider invocation that remains paired with its completion.
///
/// Implementations may return a cloned ADK model handle only while the owner
/// itself remains the completion selector stored beside the exclusive Runner.
/// This avoids loose model/result values at the authorized composition layer.
pub(crate) trait BoundOrdinaryAgentModel: Send + 'static {
    fn adk_model(&self) -> Arc<dyn Llm>;

    /// A separate summary binding must not capture the answer or spend chat turns.
    fn summarization_model(&self) -> Option<Arc<dyn Llm>> {
        None
    }

    fn request_budget(&self) -> Option<Arc<dyn super::context_budget::ModelRequestBudget>> {
        None
    }

    fn provider_model(&self) -> Arc<dyn Llm> {
        super::replay_history::provider_model(self.adk_model())
    }

    fn take_completed_text(self) -> Result<String, NativeAgentAssemblyError>;

    /// The agent's own provider model without its answer-shaping wrappers
    /// (output contract, source numbering): an idle compaction replays the
    /// last request on it and its answer is a summary, not the agent's.
    fn idle_adk_model(&self) -> Arc<dyn Llm> {
        self.adk_model()
    }

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
        None
    }

    /// The run's source count for the grounding indicator (ADR-0043
    /// decision 5), when the bound agent must cite.
    fn grounding_sources(&self) -> Option<super::grounding_model::GroundingSources> {
        None
    }
}

/// Read-only provider completion used only while ADK persists a terminal event.
///
/// ADK streams delta events through the agent and stores only the terminal
/// event in `SessionService`. Repeating the full text on that terminal model
/// response corrupts the agent's in-process accumulator. This separate view
/// lets the session boundary enrich its private durable copy without changing
/// the event that continues through the Runner and browser projector.
pub(crate) trait DurableModelCompletion: Send + Sync {
    fn snapshot(&self) -> adk_rust::Result<Option<String>>;

    /// Discard a provider result rejected before any of its text was accepted.
    /// A consumed/persisted completion must never become reusable.
    fn discard_unaccepted(&self) -> adk_rust::Result<()> {
        Err(super::model_checkpoint::output::failed(
            super::model_checkpoint::output::ContinuationFailure::DiscardUnavailable,
        ))
    }
}

/// Test-only: exit the process with this code before the Runner persists a
/// confirmation (pause) event: a Worker that dies after a replayed message and
/// before ADK's pre-check pause on an undecided sibling is durable.
#[cfg(test)]
pub(crate) static EXIT_BEFORE_CONFIRMATION_APPEND: std::sync::atomic::AtomicI32 =
    std::sync::atomic::AtomicI32::new(0);

#[cfg(test)]
fn exit_before_confirmation_append(event: &Event) {
    if event.actions.tool_confirmation.is_some()
        && let code @ 1.. =
            EXIT_BEFORE_CONFIRMATION_APPEND.load(std::sync::atomic::Ordering::Acquire)
    {
        std::process::exit(code);
    }
}

pub(super) struct RunnerSessionService {
    inner: Arc<dyn SessionService>,
    completion: Option<Arc<dyn DurableModelCompletion>>,
    omit_empty_user_input: bool,
    compaction_view: Option<([u8; 32], String)>,
    /// FU-287: the durable session the Runner's refresh reads incrementally.
    event_source: Option<Arc<PostgresSessionService>>,
}

impl RunnerSessionService {
    pub(super) fn new(
        inner: Arc<dyn SessionService>,
        completion: Option<Arc<dyn DurableModelCompletion>>,
    ) -> Self {
        Self {
            inner,
            completion,
            omit_empty_user_input: false,
            compaction_view: None,
            event_source: None,
        }
    }

    /// FU-287: refresh the Runner's view from this durable session by ordinal
    /// instead of re-reading it whole. Without it, or off `PostgreSQL`, the
    /// Runner re-reads through [`SessionService::get`] as before.
    pub(super) fn with_event_source(mut self, source: Option<Arc<PostgresSessionService>>) -> Self {
        self.event_source = source;
        self
    }

    /// Give `runner` the incremental refresh of this service's session.
    pub(super) fn refreshing(
        &self,
        runner: adk_rust::runner::Runner,
        identity: AdkIdentity,
    ) -> adk_rust::runner::Runner {
        match &self.event_source {
            Some(source) => runner.with_session_event_refresher(Arc::new(
                super::runner_view::IncrementalRunnerView::new(
                    Arc::clone(source),
                    identity,
                    self.compaction_view.clone(),
                ),
            )),
            None => runner,
        }
    }

    pub(super) fn with_history_scope(mut self, definition: [u8; 32], agent_name: &str) -> Self {
        self.compaction_view = Some((definition, agent_name.to_owned()));
        self
    }

    fn with_recovery(mut self, enabled: bool) -> Self {
        self.omit_empty_user_input = enabled;
        self
    }

    fn omit_resume_input(&self, event: &Event) -> bool {
        self.omit_empty_user_input
            && event.author == "user"
            && event
                .llm_response
                .content
                .as_ref()
                .is_some_and(|content| content.role == "user" && content.parts.is_empty())
    }

    fn durable_event(&self, mut event: Event) -> adk_rust::Result<Event> {
        // ADK 2.3.0 already stores the whole streamed turn on its terminal
        // event, one part per provider delta. Keep the durable copy's shape:
        // the turn's text as one part, no snapshot enrichment needed.
        if super::events::restates_streamed_turn(&event) {
            if let Some(content) = event.llm_response.content.as_mut() {
                merge_adjacent_text_parts(content);
            }
            return Ok(event);
        }
        let Some(completion) = &self.completion else {
            return Ok(event);
        };
        if event.llm_response.partial || !event.llm_response.turn_complete {
            return Ok(event);
        }
        let already_has_text = event.llm_response.content.as_ref().is_some_and(|content| {
            content
                .parts
                .iter()
                .any(|part| matches!(part, adk_rust::Part::Text { text } if !text.is_empty()))
        });
        if already_has_text {
            return Ok(event);
        }
        let Some(text) = completion.snapshot()? else {
            tracing::warn!(
                event = "agent_session_terminal_completion_unavailable",
                event_id = %event.id,
                "the terminal ADK session event could not be enriched with its streamed completion"
            );
            return Ok(event);
        };
        if text.is_empty() {
            return Ok(event);
        }
        let content = event
            .llm_response
            .content
            .get_or_insert_with(|| Content::new("model"));
        tracing::debug!(
            event = "agent_session_terminal_completion_enriched",
            event_id = %event.id,
            completion_bytes = text.len(),
            "enriched the private durable terminal event with its streamed completion"
        );
        content.parts.insert(0, adk_rust::Part::Text { text });
        Ok(event)
    }
}

/// Join each run of adjacent text parts into one. Text parts carry nothing
/// but their text, so the model sees the same message.
fn merge_adjacent_text_parts(content: &mut Content) {
    let mut merged: Vec<adk_rust::Part> = Vec::with_capacity(content.parts.len());
    for part in std::mem::take(&mut content.parts) {
        if let adk_rust::Part::Text { text } = &part
            && let Some(adk_rust::Part::Text { text: previous }) = merged.last_mut()
        {
            previous.push_str(text);
            continue;
        }
        merged.push(part);
    }
    content.parts = merged;
}

#[async_trait]
impl SessionService for RunnerSessionService {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.create(req).await
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        super::runner_history::read(
            self.inner.as_ref(),
            req,
            self.compaction_view
                .as_ref()
                .map(|(digest, agent)| (*digest, agent.as_str())),
        )
        .await
    }

    async fn list(&self, req: ListRequest) -> adk_rust::Result<Vec<Box<dyn Session>>> {
        self.inner.list(req).await
    }

    async fn delete(&self, req: DeleteRequest) -> adk_rust::Result<()> {
        self.inner.delete(req).await
    }

    async fn append_event(&self, session_id: &str, event: Event) -> adk_rust::Result<()> {
        if self.omit_resume_input(&event) {
            return Ok(());
        }
        #[cfg(test)]
        exit_before_confirmation_append(&event);
        self.inner
            .append_event(session_id, self.durable_event(event)?)
            .await
    }

    async fn append_event_for_identity(&self, req: AppendEventRequest) -> adk_rust::Result<()> {
        if self.omit_resume_input(&req.event) {
            return Ok(());
        }
        #[cfg(test)]
        exit_before_confirmation_append(&req.event);
        self.inner
            .append_event_for_identity(AppendEventRequest {
                identity: req.identity,
                event: self.durable_event(req.event)?,
            })
            .await
    }

    async fn delete_all_sessions(&self, app_name: &str, user_id: &str) -> adk_rust::Result<()> {
        self.inner.delete_all_sessions(app_name, user_id).await
    }

    async fn rewind(
        &self,
        session_id: &str,
        target_event_id: &str,
    ) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.rewind(session_id, target_event_id).await
    }

    async fn rewind_steps(
        &self,
        session_id: &str,
        steps: usize,
    ) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.rewind_steps(session_id, steps).await
    }

    async fn health_check(&self) -> adk_rust::Result<()> {
        self.inner.health_check().await
    }
}

/// Consuming ordinary completion selector retained beside the Runner.
pub(crate) struct OrdinaryAgentCompletion<M> {
    model: M,
    thread_id: String,
    /// ADR-0034: the cloud executor's workspace, published once the run
    /// completes or pauses, and never when it fails.
    workspace: Option<Arc<crate::workspace_run::turn::WorkspaceTurn>>,
    /// ADR-0045 decision 4: set when the run's unattended grant continues
    /// at the step limit.
    step_limit: Option<Arc<super::unattended::StepLimitStop>>,
    /// ADR-0049 decision 8: the run's cloud browser session. A completed
    /// run ends it with `RUN_ENDED` (a viewer may still look), and a paused
    /// one (`PAUSED_HITL`) keeps it for the resume. Every other exit drops
    /// the selector (a failure, a cap, a cancellation, a lost lease), and
    /// the guard ends the session at once.
    browser: crate::browser_run::BrowserRunEnd,
    /// ADR-0050 decision 9b2: an idle compaction completes with an empty
    /// result; the model's text was the summary, never the agent's answer.
    idle_compaction: bool,
}

impl<M> OrdinaryAgentCompletion<M> {
    #[must_use]
    pub(crate) fn with_workspace(
        mut self,
        workspace: Option<Arc<crate::workspace_run::turn::WorkspaceTurn>>,
    ) -> Self {
        self.workspace = workspace;
        self
    }

    #[must_use]
    pub(crate) fn with_browser(
        mut self,
        browser: Option<Arc<dyn elitea_agent_runtime::toolkits::BrowserSession>>,
    ) -> Self {
        self.browser = crate::browser_run::BrowserRunEnd::new(browser);
        self
    }
}

#[async_trait]
impl<M> NativeAgentCompletionSelector for OrdinaryAgentCompletion<M>
where
    M: BoundOrdinaryAgentModel,
{
    async fn select(mut self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        // A turn wrapped up for a take back was stopped cooperatively: it may
        // have no final text of its own, and it still completes (its work is
        // published) with the take-back note. A turn that finished on its own
        // before the stop fired keeps its answer as it is.
        let wrapped_up = self
            .workspace
            .as_ref()
            .is_some_and(|workspace| workspace.wrap_up().stopped());
        // A run stopped at its step limit under an unattended grant may also
        // have no final text. It completes with the text it has.
        let step_limited = self.step_limit.as_ref().is_some_and(|stop| stop.reached());
        let completed = if self.idle_compaction {
            drop(self.model);
            Ok(String::new())
        } else {
            self.model.take_completed_text()
        };
        let mut content = match completed {
            Ok(content) => content,
            Err(_) if wrapped_up || step_limited => String::new(),
            Err(error) => {
                if let Some(workspace) = &self.workspace {
                    let _ = workspace
                        .finish(crate::workspace_run::turn::TurnEnd::Failed)
                        .await;
                }
                self.browser.end(true).await;
                return Err(error);
            }
        };
        self.browser.end(false).await;
        if let Some(workspace) = &self.workspace {
            let stamp = workspace
                .finish(crate::workspace_run::turn::TurnEnd::Completed)
                .await
                .map_err(|error| super::workspace::publish_error(&error))?;
            // The visible turn notice of contract 6.4: which files held
            // harvested values (names, never values) and what was done.
            if let Some(notice) = stamp
                .as_ref()
                .and_then(|stamp| crate::workspace_run::file_redact::notice(&stamp.redactions))
            {
                if !content.is_empty() {
                    content.push_str("\n\n");
                }
                content.push_str(&notice);
            }
            if wrapped_up {
                if !content.is_empty() {
                    content.push_str("\n\n");
                }
                content.push_str(crate::workspace_run::wrap_up::NOTE);
            }
        }
        CompletedAgentBrowserOutput::ordinary(content, self.thread_id)
            .map_err(|error| completed_output_error(&error))
    }

    async fn select_static_pause(
        mut self,
    ) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        // The turn waits for the static pause to be continued: its work so far
        // is published as at any pause.
        self.paused().await?;
        CompletedAgentBrowserOutput::ordinary(String::new(), self.thread_id)
            .map_err(|error| completed_output_error(&error))
    }

    async fn paused(&mut self) -> Result<(), NativeAgentAssemblyError> {
        if let Some(workspace) = &self.workspace {
            workspace
                .finish(crate::workspace_run::turn::TurnEnd::Paused)
                .await
                .map_err(|error| super::workspace::publish_error(&error))?;
        }
        // The pause keeps the browser session for the person and the resume.
        self.browser.keep();
        Ok(())
    }

    fn wrap_up(&self) -> Option<Arc<crate::workspace_run::wrap_up::WrapUp>> {
        self.workspace.as_ref().map(|workspace| workspace.wrap_up())
    }

    fn step_limit_stop(&self) -> Option<Arc<super::unattended::StepLimitStop>> {
        self.step_limit.clone()
    }

    /// The run ends without completing: its browser session ends now, while
    /// the claim is live, so the ENDED report reaches Main before the claim
    /// is released.
    async fn abandon(&mut self) {
        self.browser.end(true).await;
    }
}

/// Fallback completion for a pipeline that emitted no selected public result.
pub(crate) struct PipelineAgentCompletion {
    thread_id: String,
}

#[async_trait]
impl NativeAgentCompletionSelector for PipelineAgentCompletion {
    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        CompletedAgentBrowserOutput::ordinary(PIPELINE_COMPLETED_CONTENT.to_owned(), self.thread_id)
            .map_err(|error| completed_output_error(&error))
    }

    /// A pipeline root reports its own static pause through its graph result
    /// (`pipeline_static_v1`), never through an Agent's static tool inventory.
    async fn select_static_pause(
        self,
    ) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        self.select().await
    }
}

fn fanout_wake_without_park() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "the fan-out wake has no parked graph to continue",
    )
    .with_cause("graph.fanout.wake_without_park", None)
}

/// Convert the command deadline to a monotonic instant, given one reading
/// of each clock. The remaining time must be positive.
fn execution_deadline_at(
    deadline_unix_millis: i64,
    now_unix_millis: i64,
    now: tokio::time::Instant,
) -> Result<tokio::time::Instant, NativeAgentAssemblyError> {
    let remaining = deadline_unix_millis
        .checked_sub(now_unix_millis)
        .and_then(|millis| u64::try_from(millis).ok())
        .filter(|millis| *millis > 0)
        .ok_or_else(fanout_deadline_passed)?;
    now.checked_add(std::time::Duration::from_millis(remaining))
        .ok_or_else(invalid_configuration)
}

/// [`execution_deadline_at`] for Code admission: a passed deadline is `now`.
fn code_admission_deadline_at(
    deadline_unix_millis: i64,
    now_unix_millis: i64,
    now: tokio::time::Instant,
) -> Result<tokio::time::Instant, NativeAgentAssemblyError> {
    if deadline_unix_millis <= now_unix_millis {
        return Ok(now);
    }
    execution_deadline_at(deadline_unix_millis, now_unix_millis, now)
}

fn fanout_deadline_passed() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the execution deadline has already passed",
    )
    .with_cause("graph.fanout.deadline_passed", None)
}

pub(super) const PIPELINE_RESUME_MARKER: &str = "[elitea:pipeline-resume:v1]";

/// How a pipeline's fixed Parallel nodes are composed with the turn boundary.
#[derive(Clone)]
pub(super) enum FanoutComposition {
    /// Production: a pipeline that holds a fixed Parallel node (its own or a
    /// saved child's) binds it through `FanoutTurnAuthority` with the claim's
    /// fenced decision ledger.
    Bound,
    /// Test-only: bind Parallel with the test binding and this decision
    /// source (else the claim's ledger when the backend lends one).
    #[cfg(test)]
    BoundForTests(Option<Arc<dyn elitea_agent_runtime::graph::fanout::runner::DecisionSource>>),
}

/// The root checkpointer, the node runtimes it binds, and the run's fan-out
/// latch when Parallel is bound.
type PipelineTurn = (
    Arc<dyn Checkpointer>,
    super::graph::compiler::PipelineNodeRuntimes,
    Option<Arc<super::graph::fanout_control::FanoutCancellation>>,
);

/// The root graph's turn boundary, and the node runtimes it binds.
fn pipeline_turn(
    fanout: &FanoutComposition,
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
    is_resume: bool,
    has_parallel: bool,
    node_runtimes: super::graph::compiler::PipelineNodeRuntimes,
) -> Result<PipelineTurn, NativeAgentAssemblyError> {
    if !has_parallel {
        return Ok((
            Arc::new(super::graph::turn_checkpointer::TurnCheckpointer::new(
                Arc::clone(&state.checkpointer),
                &plan.execution_id,
                plan.generation,
                is_resume,
            )),
            node_runtimes,
            None,
        ));
    }
    // The same Arc is the root checkpointer and the binding's parent, so
    // `bind_parallel_authority` binds through the turn boundary.
    let authority: Arc<dyn super::graph::ParallelCheckpointAuthority> = Arc::new(
        super::graph::fanout_turn::FanoutTurnAuthority::new(
            state
                .parallel_authority()
                .ok_or_else(invalid_configuration)?,
            plan.session_id(),
            &plan.execution_id,
            plan.generation,
            is_resume,
        )
        .map_err(|_| invalid_configuration())?,
    );
    let decisions = state.fanout_decisions().map(|client| {
        client as Arc<dyn elitea_agent_runtime::graph::fanout::runner::DecisionSource>
    });
    let binding = match fanout {
        // Without the claim's ledger no card could ever be decided: refused.
        FanoutComposition::Bound => super::graph::compiler::ParallelCompilerBinding::new(
            Arc::clone(&authority),
            plan.execution_deadline()?,
            decisions.ok_or_else(fanout_decisions_unavailable)?,
        ),
        #[cfg(test)]
        FanoutComposition::BoundForTests(source) => {
            super::graph::compiler::ParallelCompilerBinding::for_tests(
                Arc::clone(&authority),
                plan.execution_deadline()?,
                source.clone().or(decisions),
            )
        }
    };
    let binding = binding.with_root_session(Arc::new(PipelineRootSession {
        sessions: Arc::clone(&state.sessions),
        user_id: plan.user_id.to_string(),
        session_id: plan.session_id.to_string(),
    }));
    let stop = binding.cancellation();
    Ok((authority, node_runtimes.with_parallel(binding), Some(stop)))
}

/// The run's root session, read when a saved Agent member's own pause is
/// decided (FU-246).
struct PipelineRootSession {
    sessions: Arc<dyn SessionService>,
    user_id: String,
    session_id: String,
}

#[async_trait::async_trait]
impl super::graph::compiler::RootSessionSource for PipelineRootSession {
    async fn root_session(
        &self,
    ) -> Result<Box<dyn adk_rust::session::Session>, adk_rust::graph::GraphError> {
        self.sessions
            .get(GetRequest {
                app_name: APP_NAME.to_owned(),
                user_id: self.user_id.clone(),
                session_id: self.session_id.clone(),
                num_recent_events: None,
                after: None,
            })
            .await
            .map_err(|_| adk_rust::graph::GraphError::NodeExecutionFailed {
                node: "parallel".to_owned(),
                message:
                    "graph.parallel.decision_refused: the run's root session could not be read"
                        .to_owned(),
            })
    }
}

fn fanout_decisions_unavailable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::DependencyUnavailable,
        "the fan-out decision ledger is unavailable",
    )
    .with_cause("graph.fanout.decisions_unavailable", None)
}

/// Build one admitted graph Runner with activated durable state contracts.
#[allow(clippy::too_many_lines)] // Preserve the ordered claim, checkpoint, instruction, and Runner composition.
pub(super) async fn assemble_pipeline_native(
    plan: OrdinaryNativeAgentPlan,
    definition: PipelineDefinition,
    start: PipelineNativeStart,
    state: PipelineStateServices,
    runtime: PipelineRuntimeBindings,
    fanout: FanoutComposition,
) -> Result<AssembledNativeAgentInvocation<PipelineAgentCompletion>, NativeAgentAssemblyError> {
    let PipelineRuntimeBindings {
        nodes: node_runtimes,
        applications: application_runtime,
        node_events,
        skipped_application_children,
    } = runtime;
    let ApplicationRuntimeProjection {
        presentations: application_tools,
        events: application_events,
        resume: application_resume,
    } = application_runtime;
    let node_runtimes = match state.node_recovery_authority() {
        Some(authority) => node_runtimes.with_node_recovery_authority(authority),
        None => node_runtimes,
    };
    let printer_catalog = definition.printer_pause_catalog();
    let static_catalog = definition.static_pause_catalog();
    let static_family = super::graph::static_pause::StaticPauseFamily::new(
        static_catalog.clone(),
        node_runtimes
            .composition_definitions()
            .map(|definitions| {
                definitions
                    .iter()
                    .map(|(path, definition)| (path.clone(), definition.static_pause_catalog()))
                    .collect()
            })
            .unwrap_or_default(),
    );
    let start = promote_printer_pause(start, &state, &plan, &printer_catalog).await?;
    let text_resume = matches!(&start, PipelineNativeStart::Text(_));
    let checkpoint_recovery = matches!(&start, PipelineNativeStart::Checkpoint);
    let entry = match plan.paused_writer.as_ref() {
        Some(paused) if start.reenters_paused_row() => {
            state.continuation_entry(&plan, paused).await?
        }
        _ => ContinuationEntry::default(),
    };
    // E4: a re-run whose suffix holds its user event adds none.
    let input_written = entry.input_written;
    let resume = resolve_pipeline_start(
        start,
        entry,
        &state,
        &plan,
        &printer_catalog,
        &static_family,
        &application_tools,
        application_resume.as_ref(),
        node_runtimes.application_scopes(),
    )
    .await?;
    let is_resume = resume.is_some();
    // The run binds the fan-out authority when the root or a saved child
    // pipeline beneath it holds a Parallel or a Map (FU-249, T-2A3): a child's
    // binds through the run's binding.
    let has_parallel = definition.has_fanout_nodes()
        || node_runtimes
            .composition_definitions()
            .is_some_and(|definitions| {
                definitions
                    .values()
                    .any(PipelineDefinition::has_fanout_nodes)
            });
    let (turn_checkpointer, node_runtimes, fanout_stop) = pipeline_turn(
        &fanout,
        &state,
        &plan,
        is_resume,
        has_parallel,
        node_runtimes,
    )?;
    let graph = definition
        .compile_with_runtime(ROOT_AGENT_NAME, turn_checkpointer, resume, &node_runtimes)
        .map_err(|error| pipeline_configuration_error(&error))?;
    let root_identity = plan.session_identity()?;
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content,
        generation_config: _,
        max_iterations: _,
        projection,
        thread_id,
        chat_history: _,
        chat_history_refs: _,
        current_message_id: _,
        context_view_history: _,
        context_view_binding: _,
        writes_question: _,
        tags_messages: _,
        context_management: _,
        step_compaction: _,
        capability_id: _,
        definition_digest: _,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id: _,
        generation: _,
        regenerate: _,
        instruction_plan,
        deadline_unix_millis: _,
        dispatch_abort,
        paused_writer: _,
        step_limit_stop: _,
    } = plan;
    // Node models consume the admitted policy. The Runner never summarizes
    // deterministic graph state or introduces a pipeline-wide summary model.
    // TurnCheckpointer isolates fresh input without deleting recovery history.
    let agent: Arc<dyn Agent> = Arc::new(
        EliteaGraphAgent::new(graph)
            .with_printer_interrupts(Arc::clone(&state.checkpointer), printer_catalog)
            .with_static_interrupts(Arc::clone(&state.checkpointer), static_catalog),
    );
    let agent = instruction_plan.wrap(agent);
    let agent = node_events.map_or(agent.clone(), |events| {
        Arc::new(PipelineNodeEventStreamingAgent::new(agent, events)) as Arc<dyn Agent>
    });
    let agent = application_events.map_or(agent.clone(), |events| {
        application_event_agent(
            agent,
            events,
            application_tools.clone(),
            application_resume.clone(),
        )
    });
    // A failed dispatch record ends the run even when no model step follows it.
    let agent = dispatch_abort.guard_agent(agent);
    let runner_sessions = Arc::new(
        RunnerSessionService::new(state.sessions, None)
            .with_recovery(checkpoint_recovery)
            .with_event_source(state.event_source),
    );
    let runner = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(runner_sessions.clone())
        .build()
        .map_err(|_| invalid_configuration())?;
    let runner = runner_sessions.refreshing(runner, root_identity);
    // FU-243: a pipeline root names what its Agent nodes left out, like an ordinary root.
    let projection =
        projection.with_skipped_capabilities(super::skipped_capabilities::skipped_capabilities(
            &[],
            InternalToolCatalog::empty(),
            &skipped_application_children,
        ));
    let projector = AgentEventProjector::with_tool_catalogs(
        projection,
        SensitiveToolCatalog::default(),
        application_tools,
    )
    .map_err(projection_configuration)?;
    let input = if checkpoint_recovery || input_written {
        Content::new("user")
    } else if text_resume {
        user_content
    } else if is_resume {
        Content::new("user").with_text(PIPELINE_RESUME_MARKER)
    } else {
        user_content
    };
    let invocation = NativeAgentInvocation::new(runner, user_id, session_id, input);
    Ok(AssembledNativeAgentInvocation::new(
        match fanout_stop {
            Some(latch) => invocation.with_fanout_stop(latch),
            None => invocation,
        },
        projector,
        PipelineAgentCompletion { thread_id },
    ))
}

/// The graph half of restarting a pipeline's lineage (#1904), shared by a
/// regeneration and a re-seed so the two never clear different threads. ADK
/// loads the latest checkpoint even without an explicit resume ID, so the
/// frontier goes before the graph starts again; the checkpointer binds the
/// deletion to this exact claim, thread and definition. The caller ends the
/// session itself.
async fn delete_pipeline_frontier(
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
) -> Result<(), NativeAgentAssemblyError> {
    state
        .checkpointer
        .delete(plan.session_id.as_ref())
        .await
        .map_err(|_| dependency_unavailable())
}

#[allow(clippy::too_many_arguments, clippy::too_many_lines)] // Keep claim, checkpoint, continuation proof, and Runner assembly visibly ordered.
async fn resolve_pipeline_start(
    start: PipelineNativeStart,
    mut entry: ContinuationEntry,
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
    printer_catalog: &super::graph::PrinterPauseCatalog,
    static_family: &super::graph::static_pause::StaticPauseFamily,
    application_tools: &ApplicationToolPresentationCatalog,
    application_resume: Option<&ApplicationResumeCoordinator>,
    application_scopes: Option<
        &super::pipeline::scoped_applications::PipelineApplicationScopeRegistry,
    >,
) -> Result<Option<super::graph::resume::PipelineResume>, NativeAgentAssemblyError> {
    if matches!(start, PipelineNativeStart::Regenerate) {
        delete_pipeline_frontier(state, plan).await?;
        reset_session_for_regeneration(state.sessions.as_ref(), &plan.user_id, &plan.session_id)
            .await?;
    }
    // FU-269: with Main's paused writer the row was proven and the session
    // restored once (`continuation_entry`); the arms read that session. A
    // continuation records `continuing` once, before its Runner starts (E3).
    let mut continuing_written = entry.rerun;
    let session_for = async |entry: &mut ContinuationEntry| match entry.session.take() {
        Some(session) => Ok(session),
        None => restore_pipeline_session(state, plan).await,
    };
    let resume = match start {
        PipelineNativeStart::Checkpoint => {
            restore_pipeline_session(state, plan).await?;
            None
        }
        PipelineNativeStart::Fresh | PipelineNativeStart::Regenerate => {
            let binding = plan.history_binding();
            // #1672: a session seeded under another view starts again from
            // this one, with its graph frontier, as a regeneration does. Only
            // a fresh start re-seeds: a resume keeps the frontier.
            let HistorySession {
                session,
                created,
                reseeded: _,
                rebound,
            } = open_history_session(
                state.sessions.as_ref(),
                &plan.user_id,
                &plan.session_id,
                &plan.chat_history_refs,
                plan.chat_history.len(),
                plan.definition_digest,
                &binding,
                async || delete_pipeline_frontier(state, plan).await,
            )
            .await?;
            let identity = session
                .try_identity()
                .map_err(|_| invalid_configuration())?;
            if created {
                seed_frozen_history(
                    state.sessions.as_ref(),
                    &identity,
                    plan.chat_history.clone(),
                    &plan.chat_history_refs,
                    plan.definition_digest,
                    &binding,
                )
                .await?;
            } else {
                reconcile_history(
                    state.sessions.as_ref(),
                    &identity,
                    session.as_ref(),
                    &plan.chat_history,
                    &plan.chat_history_refs,
                    plan.definition_digest,
                    &binding,
                    rebound,
                )
                .await?;
                if !rebound {
                    record_history_binding(
                        state.sessions.as_ref(),
                        &identity,
                        session.as_ref(),
                        &binding,
                    )
                    .await?;
                }
            }
            None
        }
        PipelineNativeStart::StaticTools(decisions) => {
            let session = session_for(&mut entry).await?;
            let events = session.events().all();
            let journal = plan.dispatch_journal(Arc::clone(&state.sessions))?;
            // The same bound-pause rule as a sensitive decision: the pause is the
            // last event, or this execution's own dead continuation follows it.
            let interrupt = super::dispatch_journal::continuation_pause(
                &events,
                |event| {
                    event
                        .provider_metadata
                        .contains_key(adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY)
                },
                journal.owner(),
                |pause| {
                    super::graph::resume::pause_interrupt_ids(
                        pause,
                        ROOT_AGENT_NAME,
                        plan.session_id.as_ref(),
                    )
                },
                super::application_tools::pipeline_continuation_suffix(),
            )
            .ok_or_else(|| {
                // Advanced, or another execution's continuation: a stale card.
                pipeline_resume_error(&super::graph::resume::PipelineResumeError::stale())
            })?;
            let binding = super::events::pipeline_application_event_binding(
                interrupt,
                ROOT_AGENT_NAME,
                plan.session_id.as_ref(),
            )
            .map_err(projection_configuration)?;
            application_scopes
                .ok_or_else(invalid_configuration)?
                .install_root_static_continuation(
                    plan.session_id.as_ref(),
                    state.checkpointer.as_ref(),
                    &binding,
                    events_through_pause(&events, &interrupt.id)?,
                    decisions,
                )
                .await?;
            let card = binding
                .interrupt_ids()
                .first()
                .ok_or_else(invalid_configuration)?;
            if !continuing_written {
                journal
                    .record_continuing(&interrupt.id, &interrupt.invocation_id, card)
                    .await
                    .map_err(|_| dependency_unavailable())?;
                continuing_written = true;
            }
            Some(super::graph::resume::PipelineResume::empty())
        }
        PipelineNativeStart::Hitl(decision) => {
            let session = session_for(&mut entry).await?;
            let owner = super::dispatch_journal::DispatchOwner::new(
                plan.execution_id.clone(),
                plan.generation,
            );
            let resolved = decision
                .resolve(
                    session.as_ref(),
                    state.checkpointer.as_ref(),
                    ROOT_AGENT_NAME,
                    plan.session_id.as_ref(),
                    &owner,
                )
                .await
                .map_err(|error| pipeline_resume_error(&error))?;
            let (resume, application) = resolved.into_parts();
            if let Some(super::graph::resume::PipelineApplicationContinuation {
                decisions,
                pause_id,
                pause_invocation_id,
                binding,
                events,
            }) = application
            {
                // Every paused sibling is decided in one resume, on either resolver: a partial
                // set would run the approved siblings and then fail mid-stream on the rest.
                let expected = binding
                    .interrupt_ids()
                    .iter()
                    .map(String::as_str)
                    .collect::<std::collections::BTreeSet<_>>();
                let submitted = decisions
                    .iter()
                    .map(ResolvedDirectHitlDecision::interrupt_id)
                    .collect::<std::collections::BTreeSet<_>>();
                if submitted != expected {
                    return Err(NativeAgentAssemblyError::new(
                        NativeAgentAssemblyErrorCode::InvalidInput,
                        "the checkpointed pipeline decision was rejected",
                    ));
                }
                let card = decisions
                    .first()
                    .map(|decision| decision.interrupt_id().to_owned())
                    .ok_or_else(invalid_configuration)?;
                if let Some(scopes) = application_scopes {
                    scopes
                        .install_root_continuation(
                            plan.session_id.as_ref(),
                            state.checkpointer.as_ref(),
                            &binding,
                            events_through_pause(&events, &pause_id)?,
                            decisions,
                        )
                        .await?;
                } else {
                    // The legacy resolver keeps its coordinator ownership.
                    let coordinator = application_resume.ok_or_else(invalid_configuration)?;
                    install_nested_application_resume(
                        &events,
                        decisions,
                        application_tools,
                        coordinator,
                    )
                    .await?;
                }
                // Owner proof of everything this continuation forwards before it
                // finishes: a replacement of this execution continues behind it.
                if !continuing_written {
                    plan.dispatch_journal(Arc::clone(&state.sessions))?
                        .record_continuing(&pause_id, &pause_invocation_id, &card)
                        .await
                        .map_err(|_| dependency_unavailable())?;
                    continuing_written = true;
                }
            }
            Some(resume)
        }
        PipelineNativeStart::FanoutWake(wake) => {
            restore_pipeline_session(state, plan).await?;
            // A wake re-enters the parked fan-out node with unchanged business
            // state: an explicit resume with an empty resume state, so the
            // turn boundary reads the parked frontier of the earlier execution.
            // Without a parked frontier the graph would start from its entry,
            // which would run the wake as a fresh turn.
            let parked = state
                .checkpointer
                .load(plan.session_id.as_ref())
                .await
                .map_err(|_| dependency_unavailable())?;
            // The parked frontier must be the one the parked execution wrote.
            if parked.is_none_or(|checkpoint| {
                checkpoint.pending_nodes.is_empty()
                    || !wake
                        .is_parked_writer(checkpoint.metadata.get("elitea.pipeline.execution.v1"))
            }) {
                return Err(fanout_wake_without_park());
            }
            Some(super::graph::resume::PipelineResume::empty())
        }
        PipelineNativeStart::McpAuthorization(continuation) => {
            let session = session_for(&mut entry).await?;
            Some(
                continuation
                    .resolve(
                        session.as_ref(),
                        state.checkpointer.as_ref(),
                        ROOT_AGENT_NAME,
                        plan.session_id.as_ref(),
                    )
                    .await
                    .map_err(|error| pipeline_resume_error(&error))?,
            )
        }
        PipelineNativeStart::Text(continuation) => {
            let session = session_for(&mut entry).await?;
            Some(
                continuation
                    .resolve_with_family(
                        session.as_ref(),
                        state.checkpointer.as_ref(),
                        ROOT_AGENT_NAME,
                        plan.session_id.as_ref(),
                        printer_catalog,
                        static_family,
                        &plan
                            .user_content
                            .parts
                            .iter()
                            .filter_map(|part| match part {
                                adk_rust::Part::Text { text } => Some(text.as_str()),
                                _ => None,
                            })
                            .collect::<Vec<_>>()
                            .join("\n"),
                    )
                    .await
                    .map_err(|error| pipeline_resume_error(&error))?
                    .preserve_descendant_static_inputs(),
            )
        }
    };
    // E3: the continuation's `continuing` record, once, before its Runner
    // starts: the first event after the pause, the owner proof a re-run of
    // this execution reads (`owned_continuation_suffix`). Only with a paused
    // writer; it carries no content, so history, trace and replay ignore it.
    if let Some(pause) = entry.continuing.as_ref().filter(|_| !continuing_written) {
        plan.dispatch_journal(Arc::clone(&state.sessions))?
            .record_continuing(&pause.pause_id, &pause.pause_invocation_id, &pause.card)
            .await
            .map_err(|_| dependency_unavailable())?;
    }
    Ok(resume)
}

/// Demo issue 5: a Printer pause tells the user "To resume the pipeline -
/// type anything...", but Main sends the typed text as an ordinary (fresh)
/// start, so the run restarted at the entry point. Resume instead when this
/// thread's session ends at the exact latest Printer checkpoint, as the SDK
/// does for any ordinary input at a static interrupt. Every other state keeps
/// the fresh run. The probe reads only this claim's own session and
/// checkpoint, which Main already authorized for this conversation.
async fn promote_printer_pause(
    start: PipelineNativeStart,
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
    printer_catalog: &super::graph::PrinterPauseCatalog,
) -> Result<PipelineNativeStart, NativeAgentAssemblyError> {
    use super::graph::resume::PrinterContinuation;
    if !matches!(start, PipelineNativeStart::Fresh) || printer_catalog.is_empty() {
        return Ok(start);
    }
    let session = match state
        .sessions
        .get(GetRequest {
            app_name: APP_NAME.to_owned(),
            user_id: plan.user_id.to_string(),
            session_id: plan.session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
    {
        Ok(session) => session,
        Err(error) if error.code == "session.not_found" => return Ok(start),
        Err(_) => return Err(dependency_unavailable()),
    };
    match PrinterContinuation::ordinary_message()
        .resolve(
            super::graph::resume::PrinterResumeContext::new(
                session.as_ref(),
                state.checkpointer.as_ref(),
                ROOT_AGENT_NAME,
                plan.session_id.as_ref(),
                printer_catalog,
            ),
            &plan
                .user_content
                .parts
                .iter()
                .filter_map(|part| match part {
                    adk_rust::Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("\n"),
        )
        .await
    {
        Ok(_) => Ok(PipelineNativeStart::Text(
            super::graph::static_pause::PipelineTextContinuation::ordinary_message(),
        )),
        Err(error) if PrinterContinuation::probe_means_fresh_run(&error) => Ok(start),
        Err(error) => Err(pipeline_resume_error(&error)),
    }
}

async fn restore_pipeline_session(
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
) -> Result<Box<dyn adk_rust::session::Session>, NativeAgentAssemblyError> {
    state
        .sessions
        .get(GetRequest {
            app_name: APP_NAME.to_owned(),
            user_id: plan.user_id.to_string(),
            session_id: plan.session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// Build one fresh ADK session, direct `LlmAgent`, and exclusive Runner.
pub(crate) async fn assemble_ordinary_native<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    toolsets: Vec<Arc<dyn Toolset>>,
    sensitive_tools: SensitiveToolCatalog,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    assemble_ordinary_native_with_sessions(model, plan, toolsets, sensitive_tools, sessions).await
}

/// Build a direct `LlmAgent` and exclusive Runner over an injected session service.
///
/// Both direct and future graph agents use this same ADK session boundary; the
/// graph checkpointer remains an additional graph-only dependency rather than
/// a replacement for `SessionService`.
pub(crate) async fn assemble_ordinary_native_with_sessions<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    toolsets: Vec<Arc<dyn Toolset>>,
    sensitive_tools: SensitiveToolCatalog,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_ordinary_native_with_sessions_and_options(
        model,
        plan,
        toolsets,
        sensitive_tools,
        ApplicationRuntimeProjection::default(),
        NativeToolExecutionMode::Sequential,
        sessions,
    )
    .await
}

/// Build the direct Runner with an explicitly admitted tool dispatch policy.
pub(crate) async fn assemble_ordinary_native_with_sessions_and_options<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    toolsets: Vec<Arc<dyn Toolset>>,
    sensitive_tools: SensitiveToolCatalog,
    application_runtime: ApplicationRuntimeProjection,
    execution_mode: NativeToolExecutionMode,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        model,
        plan,
        OrdinaryRuntimeBindings::new(
            toolsets,
            sensitive_tools,
            DelegatedAuthorizationCatalog::default(),
            application_runtime,
        ),
        execution_mode,
        sessions,
    )
    .await
}

pub(crate) async fn assemble_ordinary_native_with_sessions_and_runtime_catalogs<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    execution_mode: NativeToolExecutionMode,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_ordinary_with_checkpoint_mode(model, plan, runtime, execution_mode, sessions, false)
        .await
        .map(|(assembled, _)| assembled)
}

/// Rebuild a pending recovery from the same execution checkpoint.
/// Missing or non-model checkpoints never fall back to an ordinary invocation.
pub(crate) async fn assemble_ordinary_native_from_checkpoint<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    execution_mode: NativeToolExecutionMode,
    sessions: Arc<dyn SessionService>,
) -> Result<
    super::runtime::PendingRecoveredAgentInvocation<OrdinaryAgentCompletion<M>>,
    NativeAgentAssemblyError,
>
where
    M: BoundOrdinaryAgentModel,
{
    let (assembled, checkpoint) = assemble_ordinary_with_checkpoint_mode(
        model,
        plan,
        runtime,
        execution_mode,
        sessions,
        true,
    )
    .await?;
    Ok(super::runtime::PendingRecoveredAgentInvocation::new(
        assembled,
        checkpoint.ok_or_else(invalid_configuration)?,
    ))
}

#[allow(clippy::too_many_lines)] // Keep ordered session recovery and Runner assembly together.
async fn assemble_ordinary_with_checkpoint_mode<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    execution_mode: NativeToolExecutionMode,
    sessions: Arc<dyn SessionService>,
    checkpoint_recovery: bool,
) -> Result<
    (
        AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>,
        Option<ValidatedModelCheckpoint>,
    ),
    NativeAgentAssemblyError,
>
where
    M: BoundOrdinaryAgentModel,
{
    let root_identity = plan.session_identity()?;
    let binding = plan.history_binding();
    let event_source = runtime.event_source.clone();
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        mut user_content,
        generation_config,
        max_iterations,
        projection,
        thread_id,
        chat_history,
        chat_history_refs,
        current_message_id: _,
        context_view_history: _,
        context_view_binding: _,
        writes_question: _,
        tags_messages: _,
        context_management,
        step_compaction,
        capability_id: _,
        definition_digest,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id,
        generation,
        regenerate,
        instruction_plan: _,
        deadline_unix_millis: _,
        dispatch_abort: _,
        paused_writer: _,
        step_limit_stop,
    } = plan;
    // `session_compaction` (#1368): the desktop's step compaction prepares
    // every root request when the model's limits are frozen; without them the
    // plan falls back to the Runner composition below, as before.
    let step_compaction = match (
        step_compaction,
        model.request_budget(),
        model.summarization_model(),
    ) {
        (Some(policy), Some(budget), Some(summary_model)) => {
            Some(Arc::new(super::context_management::StepCompaction::new(
                policy,
                budget,
                summary_model,
                Arc::new(super::context_management::NoCompactionHost),
            )))
        }
        _ => None,
    };
    let durable_context_plan = match (
        &context_management,
        model.request_budget(),
        model.summarization_model(),
    ) {
        (ContextManagementPlan::Summarize(plan), Some(budget), Some(summary_model))
            if step_compaction.is_none() =>
        {
            Some((plan.clone(), budget, summary_model))
        }
        _ => None,
    };
    let context_compaction = if durable_context_plan.is_some() || step_compaction.is_some() {
        None
    } else {
        context_management.prepare_runner_composition(model.summarization_model())?
    };
    let parallel = execution_mode == NativeToolExecutionMode::ParallelApplications;
    if parallel && runtime.has_confirmation_guards() {
        return Err(invalid_configuration());
    }
    // Both copied out before `runtime` moves into `build_runtime_agent` below
    // — `InternalToolCatalog` is `Copy` for exactly this (see its own doc
    // comment), and the skipped-child list is a handful of short strings.
    // #866/#973: this is what lets a skipped internal tool, or an attached
    // child this worker cannot build, become a notice IN THE RUN rather than
    // only a log line no user reads.
    let internal_tools = runtime.internal_tools;
    if checkpoint_recovery && regenerate {
        return Err(invalid_configuration());
    }
    let notices = FreshSessionNotices::from_runtime(&runtime);
    if regenerate {
        reset_session_for_regeneration(sessions.as_ref(), &user_id, &session_id).await?;
    }
    let HistorySession {
        session,
        created,
        reseeded,
        rebound,
    } = if checkpoint_recovery {
        let session = sessions
            .get(GetRequest {
                app_name: APP_NAME.to_owned(),
                user_id: user_id.to_string(),
                session_id: session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await
            .map_err(|_| dependency_unavailable())?;
        HistorySession {
            session,
            created: false,
            reseeded: false,
            rebound: false,
        }
    } else {
        // #1672: a session seeded under another view starts a fresh lineage
        // from this one, as a regeneration does; its compaction root goes
        // with it. An ordinary agent has no graph frontier to clear.
        open_history_session(
            sessions.as_ref(),
            &user_id,
            &session_id,
            &chat_history_refs,
            chat_history.len(),
            definition_digest,
            &binding,
            async || Ok(()),
        )
        .await?
    };
    let identity = session
        .try_identity()
        .map_err(|_| invalid_configuration())?;
    if created {
        seed_frozen_history(
            sessions.as_ref(),
            &identity,
            chat_history,
            &chat_history_refs,
            definition_digest,
            &binding,
        )
        .await?;
        // #866/#973: a fresh session is exactly where `seed_frozen_history`
        // seeds the frozen prior transcript, so it is also the one place to
        // seed the "could not honour" notices once, without repeating them on
        // every later turn of the same conversation — the same "once, at
        // session creation" shape the frozen-history seed itself uses.
        seed_fresh_session_notices(sessions.as_ref(), &identity, internal_tools, &notices).await?;
    } else if !checkpoint_recovery {
        reconcile_history(
            sessions.as_ref(),
            &identity,
            session.as_ref(),
            &chat_history,
            &chat_history_refs,
            definition_digest,
            &binding,
            rebound,
        )
        .await?;
        if !rebound {
            record_history_binding(sessions.as_ref(), &identity, session.as_ref(), &binding)
                .await?;
        }
    }
    let bootstrap = match (reseeded, created, rebound) {
        (true, _, _) => "reseeded",
        (false, true, _) => "seeded",
        (false, false, true) => "rebound",
        (false, false, false) => "restored",
    };
    tracing::Span::current().record("session_bootstrap", bootstrap);
    tracing::debug!(
        session_bootstrap = bootstrap,
        "prepared the ADK session for the native agent runner"
    );
    let durable_context = durable_context_plan
        .map(|(plan, budget, summary_model)| {
            super::context_compaction::DurableContextCompaction::new(
                plan,
                budget,
                summary_model,
                definition_digest,
                session.as_ref(),
            )
            .map(Arc::new)
        })
        .transpose()
        .map_err(|_| invalid_configuration())?;
    let checkpoint = super::model_checkpoint::ModelCheckpointWriter::new(
        sessions.clone(),
        execution_id,
        generation,
        definition_digest,
    )
    .with_request_budget(model.request_budget())
    .with_context_compaction(durable_context)
    .with_step_compaction(step_compaction)
    .with_steering(runtime.steering.clone());
    let checkpoint = if checkpoint_recovery {
        let restored = checkpoint
            .restore(session.as_ref())
            .map_err(|_| invalid_configuration())?;
        if !restored.is_recovery() {
            return Err(invalid_configuration());
        }
        restored
    } else {
        checkpoint
    };
    let evidence = checkpoint.validated_checkpoint();
    let recovering = checkpoint.is_recovery();
    if recovering {
        // Keep the existing user turn. The callback restores the exact pending
        // model request and removes this empty ADK Runner input from later calls.
        user_content = Content::new("user");
    }
    let (agent, mut projector) = build_runtime_agent(
        model.provider_model(),
        generation_config,
        max_iterations,
        projection,
        runtime,
        parallel,
        Some(checkpoint),
    )?;
    if recovering {
        projector.mark_checkpoint_recovery();
    }
    projector.attach_grounding(model.grounding_sources());
    let runner_sessions = Arc::new(
        RunnerSessionService::new(sessions, model.durable_completion())
            .with_recovery(recovering)
            .with_history_scope(definition_digest, ROOT_AGENT_NAME)
            .with_event_source(event_source),
    );
    let mut runner_builder = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(runner_sessions.clone());
    if let Some(compaction) = context_compaction {
        runner_builder = runner_builder.context_compaction(compaction);
    }
    let runner = runner_builder
        .build()
        .map_err(|_| invalid_configuration())?;
    let runner = runner_sessions.refreshing(runner, root_identity);
    let invocation = if parallel {
        NativeAgentInvocation::new_with_run_config(
            runner,
            user_id,
            session_id,
            user_content,
            parallel_application_run_config(),
        )
    } else {
        NativeAgentInvocation::new(runner, user_id, session_id, user_content)
    };
    Ok((
        AssembledNativeAgentInvocation::new(
            invocation,
            projector,
            OrdinaryAgentCompletion {
                model,
                thread_id,
                workspace: None,
                step_limit: step_limit_stop,
                browser: crate::browser_run::BrowserRunEnd::new(None),
                idle_compaction: false,
            },
        ),
        evidence,
    ))
}

/// Assemble one idle compaction (ADR-0050 decision 9b2, #1610) over the
/// claim-fenced session: a Runner whose root agent never runs the agent loop
/// and yields only one `agent_context_status` frame. The empty Runner input
/// is not persisted, so no user message is appended.
pub(crate) async fn assemble_idle_compaction<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    let identity = plan.session_identity()?;
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        projection,
        thread_id,
        context_management,
        step_compaction,
        definition_digest,
        step_limit_stop,
        ..
    } = plan;
    // The tools are bound so the agent's system prompt is the one its last
    // request was sent with; none is ever called.
    drop(runtime);
    restore_or_create_session(sessions.as_ref(), &user_id, &session_id).await?;
    // Only durable root compaction (`elitea.context.root.v1`) has a record
    // the next turn reads; step compaction (`session_compaction`, #1368)
    // keeps its summary in the history snapshot of a model request, which an
    // idle run does not write. Without one the job completes doing nothing.
    let run = match (
        &context_management,
        step_compaction.is_none(),
        model.request_budget(),
    ) {
        (ContextManagementPlan::Summarize(compaction), true, Some(budget)) => {
            Some(super::idle_compaction::IdleCompactionRun {
                sessions: Arc::clone(&sessions),
                identity,
                model: super::replay_history::provider_model(model.idle_adk_model()),
                budget,
                definition_digest,
                agent_name: ROOT_AGENT_NAME.to_owned(),
                guidance: compaction.summary_instructions.clone(),
                completion: model.durable_completion(),
            })
        }
        _ => None,
    };
    let agent: Arc<dyn Agent> = Arc::new(super::idle_compaction::IdleCompactionAgent::new(
        ROOT_AGENT_NAME,
        run,
    ));
    let mut projector = AgentEventProjector::new(projection).map_err(projection_configuration)?;
    projector.mark_idle_compaction();
    let runner_sessions = Arc::new(
        RunnerSessionService::new(sessions, None)
            .with_recovery(true)
            .with_history_scope(definition_digest, ROOT_AGENT_NAME),
    );
    let runner = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(runner_sessions)
        .build()
        .map_err(|_| invalid_configuration())?;
    Ok(AssembledNativeAgentInvocation::new(
        NativeAgentInvocation::new(runner, user_id, session_id, Content::new("user")),
        projector,
        OrdinaryAgentCompletion {
            model,
            thread_id,
            workspace: None,
            step_limit: step_limit_stop,
            browser: crate::browser_run::BrowserRunEnd::new(None),
            idle_compaction: true,
        },
    ))
}

/// The argument-aware confirmation gate and the steering source of the root
/// agent, each only when the turn has one.
fn bind_turn_hooks(
    mut builder: LlmAgentBuilder,
    tool_call_confirmation: Option<Arc<dyn adk_rust::agent::ToolCallConfirmation>>,
    steering: Option<Arc<dyn crate::state::TurnSteering>>,
) -> LlmAgentBuilder {
    if let Some(gate) = tool_call_confirmation {
        builder = builder.tool_call_confirmation(gate);
    }
    if let Some(source) = steering {
        let source: Arc<dyn adk_rust::agent::SteeringSource> = source;
        builder = builder.steering_source(source);
    }
    builder
}

fn build_runtime_agent(
    model: Arc<dyn Llm>,
    generation_config: GenerateContentConfig,
    max_iterations: u32,
    projection: AgentEventProjectionContext,
    runtime: OrdinaryRuntimeBindings,
    parallel: bool,
    checkpoint: Option<super::model_checkpoint::ModelCheckpointWriter>,
) -> Result<(Arc<dyn Agent>, AgentEventProjector), NativeAgentAssemblyError> {
    // The user-visible half of what the turn could not serve, on the turn's own
    // node events. Done HERE, where every path (a fresh turn, a checkpoint
    // recovery and a human-in-the-loop or authorization RESUME) builds its
    // agent, so a resumed turn's final message carries the list too.
    let projection =
        projection.with_skipped_capabilities(super::skipped_capabilities::skipped_capabilities(
            &runtime.left_out_toolkits,
            runtime.internal_tools,
            &runtime.skipped_application_children,
        ));
    let OrdinaryRuntimeBindings {
        toolsets,
        sensitive_tools,
        delegated_authorization,
        internal_tools,
        // Reported by the session seed, never by the agent graph (#973/#983).
        skipped_application_children: _,
        renamed_tools: _,
        left_out_toolkits: _,
        toolkit_attribution,
        application_runtime,
        instruction_plan,
        // Already part of the bound model's system instruction (ordinary.rs).
        lazy_tools_index: _,
        tool_call_confirmation,
        steering,
        egress,
        // Read by the Runner assembly before the runtime is taken apart.
        event_source: _,
    } = runtime;
    let mut delegated_authorization = delegated_authorization;
    let (model, toolsets) = crate::toolkits::bind_authorization_model_tools(
        model,
        toolsets,
        &mut delegated_authorization,
    )
    .map_err(|_| invalid_configuration())?;
    let checkpoint = checkpoint.map(|checkpoint| {
        checkpoint.with_application_tools(application_runtime.presentations.agent_tool_names())
    });
    let model = checkpoint.as_ref().map_or_else(
        || model.clone(),
        |checkpoint| checkpoint.delegation_model(model.clone()),
    );
    let mut builder = LlmAgentBuilder::new(ROOT_AGENT_NAME)
        .model(model)
        .generate_content_config(generation_config)
        .max_iterations(max_iterations)
        .disallow_transfer_to_parent(true)
        .disallow_transfer_to_peers(true);
    builder = instruction_plan.bind_builder(builder);
    let context_events = if let Some(checkpoint) = checkpoint {
        // Persist the prepared request, including authoritative instructions.
        // Recovery substitutes that exact request after fresh tool binding.
        let (events, receiver) = super::graph::pipeline_node_event_channel();
        builder = checkpoint.with_context_events(events).bind(builder);
        Some(receiver)
    } else {
        None
    };
    if parallel {
        builder = builder.tool_execution_strategy(ToolExecutionStrategy::Parallel);
    }
    for toolset in toolsets {
        builder = builder.toolset(toolset);
    }
    for tool_name in sensitive_tools
        .tool_names()
        .filter(|name| !delegated_authorization.is_declined(name))
    {
        builder = builder.require_tool_confirmation(tool_name);
    }
    for tool_name in delegated_authorization.tool_names() {
        builder = builder.require_tool_confirmation(tool_name);
    }
    if internal_tools.ask_user_enabled() {
        builder = builder.require_tool_confirmation(ASK_USER_TOOL_NAME);
    }
    // ADR-0043 decision 4: a candidate call after an untrusted read asks.
    let tool_call_confirmation = sensitive_tools.untrusted_act_gate(tool_call_confirmation);
    builder = bind_turn_hooks(builder, tool_call_confirmation, steering);
    let ApplicationRuntimeProjection {
        presentations: application_tools,
        events: application_events,
        resume: application_resume,
    } = application_runtime;
    let agent: Arc<dyn Agent> = Arc::new(builder.build().map_err(|_| invalid_configuration())?);
    let agent: Arc<dyn Agent> = context_events.map_or(agent.clone(), |events| {
        Arc::new(PipelineNodeEventStreamingAgent::new(agent, events))
    });
    let agent = instruction_plan.wrap(agent);
    let agent = delegated_authorization_agent(agent, delegated_authorization.clone(), egress);
    let agent = clarifying_question_agent(agent, internal_tools);
    let agent = application_events.map_or(agent.clone(), |events| {
        application_event_agent(
            agent,
            events,
            application_tools.clone(),
            application_resume.clone(),
        )
    });
    let projector = AgentEventProjector::with_runtime_catalogs(
        projection,
        sensitive_tools,
        delegated_authorization,
        application_tools,
    )
    .map_err(projection_configuration)?
    .with_toolkit_attribution(toolkit_attribution);
    Ok((agent, projector))
}

fn parallel_application_run_config() -> RunConfig {
    RunConfig::builder()
        .tool_concurrency(ToolConcurrencyConfig {
            max_concurrency: Some(MAX_PARALLEL_APPLICATION_CALLS),
            ..ToolConcurrencyConfig::default()
        })
        .build()
}

/// Rebuild one direct `LlmAgent` around an exact persisted sensitive call.
///
/// ADK 2.0.0 does not expose a suspended `LlmAgent` frame after restart. This
/// capability-disabled seam therefore uses a one-shot model adapter to emit the
/// already-proven call once, then lets ADK's normal confirmation and
/// `ToolExecutor` path apply the decision and continue with the bound provider.
/// Approved effects are rejected before Runner construction; denied effects
/// use a local blocked-result adapter and never dispatch the real tool.
pub(crate) async fn assemble_direct_hitl_resume_with_sessions<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    toolsets: Vec<Arc<dyn Toolset>>,
    sensitive_tools: SensitiveToolCatalog,
    decision: DirectHitlDecision,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_direct_hitl_resume_with_sessions_and_applications(
        model,
        plan,
        OrdinaryRuntimeBindings::new(
            toolsets,
            sensitive_tools,
            DelegatedAuthorizationCatalog::default(),
            ApplicationRuntimeProjection::default(),
        ),
        DirectHitlDecisionSet::single(decision),
        sessions,
    )
    .await
}

/// Resume one direct confirmation while retaining saved-participant streaming.
pub(crate) async fn assemble_direct_hitl_resume_with_sessions_and_applications<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    decisions: DirectHitlDecisionSet,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_direct_resume_with_sessions_and_applications(
        model,
        plan,
        runtime,
        DirectResumeStart::Sensitive(decisions),
        sessions,
    )
    .await
}

pub(crate) async fn assemble_delegated_authorization_resume_with_sessions<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    continuation: DirectDelegatedAuthorizationContinuation,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    assemble_direct_resume_with_sessions_and_applications(
        model,
        plan,
        runtime,
        DirectResumeStart::DelegatedAuthorization(continuation),
        sessions,
    )
    .await
}

enum DirectResumeStart {
    Sensitive(DirectHitlDecisionSet),
    DelegatedAuthorization(DirectDelegatedAuthorizationContinuation),
}

struct PreparedDirectResume {
    model: Arc<dyn Llm>,
    run_input: DirectHitlRunInput,
    runtime: OrdinaryRuntimeBindings,
    parallel_applications: bool,
}

#[allow(clippy::too_many_lines)] // Keep claim, checkpoint, continuation proof, and Runner assembly visibly ordered.
async fn prepare_direct_resume(
    model: Arc<dyn Llm>,
    stored: &dyn Session,
    runtime: OrdinaryRuntimeBindings,
    start: DirectResumeStart,
    journal: &super::dispatch_journal::DispatchJournal,
) -> Result<PreparedDirectResume, NativeAgentAssemblyError> {
    let OrdinaryRuntimeBindings {
        toolsets,
        sensitive_tools,
        mut delegated_authorization,
        internal_tools,
        // Reported by the session seed, never by the agent graph (#973/#983).
        skipped_application_children: _,
        renamed_tools: _,
        left_out_toolkits: _,
        toolkit_attribution,
        application_runtime,
        instruction_plan,
        // Already part of the bound model's system instruction (ordinary.rs).
        lazy_tools_index: _,
        tool_call_confirmation,
        steering,
        egress,
        // Read by the Runner assembly before the runtime is taken apart.
        event_source: _,
    } = runtime;
    let events = stored.events().all();
    let resolved = match start {
        DirectResumeStart::Sensitive(decisions) => decisions
            .resolve(stored)
            .map_err(|error| direct_hitl_error(&error))?,
        DirectResumeStart::DelegatedAuthorization(continuation) => {
            ResolvedDirectHitlStart::Direct(Box::new(
                continuation
                    .resolve(stored)
                    .map_err(|error| direct_hitl_error(&error))?,
            ))
        }
    };
    let ApplicationRuntimeProjection {
        presentations: application_tools,
        events: application_events,
        resume,
    } = application_runtime;
    let (model, run_input, toolsets, parallel_applications) = match resolved {
        ResolvedDirectHitlStart::Direct(decision) => {
            // Another execution's dispatch record for this card: it already
            // continued it, so this submission is stale (not "may have started").
            let owned = super::dispatch_journal::decision_records_owned_by(
                &events,
                journal.owner(),
                decision.invocation_id(),
                decision.interrupt_id(),
            )
            .map_err(|_| {
                direct_hitl_error(&DirectHitlError::new(DirectHitlErrorCode::CorruptSession))
            })?;
            if !owned {
                return Err(direct_hitl_error(&DirectHitlError::new(
                    DirectHitlErrorCode::StaleDecision,
                )));
            }
            decision.restore_authorization_scope(&mut delegated_authorization);
            let replay = if decision.is_delegated_authorization() {
                (*decision)
                    .into_delegated_authorization_replay(&mut delegated_authorization)
                    .map_err(|error| direct_hitl_error(&error))?
            } else if decision.is_clarifying_question() {
                (*decision)
                    .into_clarifying_question_replay()
                    .map_err(|error| direct_hitl_error(&error))?
            } else {
                (*decision)
                    .into_direct_replay(&sensitive_tools)
                    .map_err(|error| direct_hitl_error(&error))?
            };
            // An approved sensitive-tool replay journals each call that may not
            // simply run again before its effect (one root append; the Runner
            // persists the outcome). Read-only calls re-run and cost none; a
            // clarifying or authorization continuation journals nothing, and no
            // tool of either starts after the invocation's stop.
            let (replay_model, run_input, toolsets) = replay
                .bind(model)
                .into_parts(
                    toolsets,
                    Some(journal),
                    super::dispatch_journal::JournalOutcomes::ByRunner,
                    &sensitive_tools,
                )
                .map_err(|error| direct_hitl_error(&error))?;
            (replay_model, run_input, toolsets, false)
        }
        ResolvedDirectHitlStart::StaticTools(decisions) => {
            let coordinator = resume.as_ref().ok_or_else(invalid_configuration)?;
            let prepared = super::application_tools::prepare_static_application_resume(
                &events,
                decisions,
                &application_tools,
                coordinator,
                model,
            )
            .await?;
            let (replay_model, user_content, run_config) = prepared.into_parts();
            (
                replay_model,
                DirectHitlRunInput::from_parts(user_content, run_config),
                toolsets,
                true,
            )
        }
        ResolvedDirectHitlStart::Nested(decisions) => {
            let coordinator = resume.as_ref().ok_or_else(invalid_configuration)?;
            let prepared = prepare_nested_application_resume(
                &events,
                decisions,
                &application_tools,
                coordinator,
                model,
                journal.owner(),
            )
            .await?;
            let (replay_model, user_content, run_config) = prepared.into_parts();
            (
                replay_model,
                DirectHitlRunInput::from_parts(user_content, run_config),
                toolsets,
                true,
            )
        }
    };
    Ok(PreparedDirectResume {
        model,
        run_input,
        runtime: OrdinaryRuntimeBindings::new(
            toolsets,
            sensitive_tools,
            delegated_authorization,
            ApplicationRuntimeProjection {
                presentations: application_tools,
                events: application_events,
                resume,
            },
        )
        .with_internal_tools(internal_tools)
        .with_instruction_plan(instruction_plan)
        .with_toolkit_attribution(toolkit_attribution)
        .with_tool_call_confirmation(tool_call_confirmation)
        .with_steering(steering)
        .with_egress(egress),
        parallel_applications,
    })
}

async fn assemble_direct_resume_with_sessions_and_applications<M>(
    model: M,
    plan: OrdinaryNativeAgentPlan,
    runtime: OrdinaryRuntimeBindings,
    start: DirectResumeStart,
    sessions: Arc<dyn SessionService>,
) -> Result<AssembledNativeAgentInvocation<OrdinaryAgentCompletion<M>>, NativeAgentAssemblyError>
where
    M: BoundOrdinaryAgentModel,
{
    let journal = plan.dispatch_journal(Arc::clone(&sessions))?;
    let root_identity = plan.session_identity()?;
    let event_source = runtime.event_source.clone();
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content: _,
        generation_config,
        max_iterations,
        projection,
        thread_id,
        chat_history: _,
        chat_history_refs: _,
        current_message_id: _,
        context_view_history: _,
        context_view_binding: _,
        writes_question: _,
        tags_messages: _,
        context_management,
        // A direct resume has no model checkpoint to prepare requests in:
        // it keeps the Runner composition of the plan.
        step_compaction: _,
        capability_id: _,
        definition_digest,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id: _,
        generation: _,
        regenerate: _,
        instruction_plan: _,
        deadline_unix_millis: _,
        dispatch_abort: _,
        paused_writer: _,
        step_limit_stop,
    } = plan;
    let context_compaction =
        context_management.prepare_runner_composition(model.summarization_model())?;
    let stored = sessions
        .get(GetRequest {
            app_name: APP_NAME.to_owned(),
            user_id: user_id.to_string(),
            session_id: session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .map_err(|_| dependency_unavailable())?;
    let prepared = prepare_direct_resume(
        model.provider_model(),
        stored.as_ref(),
        runtime,
        start,
        &journal,
    )
    .await?;
    // Every model step and the root run stop after a failed dispatch record.
    let (agent, mut projector) = build_runtime_agent(
        journal.abort().guard(prepared.model),
        generation_config,
        max_iterations,
        projection,
        prepared.runtime,
        prepared.parallel_applications,
        None,
    )?;
    projector.attach_grounding(model.grounding_sources());
    let agent = journal.abort().guard_agent(agent);
    let runner_sessions = Arc::new(
        RunnerSessionService::new(sessions, model.durable_completion())
            .with_history_scope(definition_digest, ROOT_AGENT_NAME)
            .with_event_source(event_source),
    );
    let mut runner_builder = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(runner_sessions.clone());
    if let Some(compaction) = context_compaction {
        runner_builder = runner_builder.context_compaction(compaction);
    }
    let runner = runner_builder
        .build()
        .map_err(|_| invalid_configuration())?;
    let runner = runner_sessions.refreshing(runner, root_identity);
    Ok(AssembledNativeAgentInvocation::new(
        NativeAgentInvocation::new_direct_hitl(runner, user_id, session_id, prepared.run_input),
        projector,
        OrdinaryAgentCompletion {
            model,
            thread_id,
            workspace: None,
            step_limit: step_limit_stop,
            browser: crate::browser_run::BrowserRunEnd::new(None),
            idle_compaction: false,
        },
    ))
}

fn direct_hitl_error(error: &DirectHitlError) -> NativeAgentAssemblyError {
    super::direct_hitl::assembly_error(error, "the direct tool continuation replay was rejected")
}

async fn restore_or_create_session(
    sessions: &dyn SessionService,
    user_id: &UserId,
    session_id: &SessionId,
) -> Result<(Box<dyn adk_rust::session::Session>, bool), NativeAgentAssemblyError> {
    let user_id = user_id.to_string();
    let session_id = session_id.to_string();
    match sessions
        .get(GetRequest {
            app_name: APP_NAME.to_owned(),
            user_id: user_id.clone(),
            session_id: session_id.clone(),
            num_recent_events: None,
            after: None,
        })
        .await
    {
        Ok(session) => Ok((session, false)),
        Err(error) if error.code == "session.not_found" => sessions
            .create(CreateRequest {
                app_name: APP_NAME.to_owned(),
                user_id,
                session_id: Some(session_id),
                state: HashMap::new(),
            })
            .await
            .map(|session| (session, true))
            .map_err(|_| dependency_unavailable()),
        Err(_) => Err(dependency_unavailable()),
    }
}

async fn reset_session_for_regeneration(
    sessions: &dyn SessionService,
    user_id: &UserId,
    session_id: &SessionId,
) -> Result<(), NativeAgentAssemblyError> {
    let user_id = user_id.to_string();
    let session_id = session_id.to_string();
    match sessions
        .get(GetRequest {
            app_name: APP_NAME.to_owned(),
            user_id: user_id.clone(),
            session_id: session_id.clone(),
            num_recent_events: Some(0),
            after: None,
        })
        .await
    {
        Ok(_) => delete_present_session(sessions, &user_id, &session_id).await,
        Err(error) if error.code == "session.not_found" => Ok(()),
        Err(_) => Err(dependency_unavailable()),
    }
}

/// Delete a session this turn has just read (#1904): no probe first.
async fn delete_present_session(
    sessions: &dyn SessionService,
    user_id: &str,
    session_id: &str,
) -> Result<(), NativeAgentAssemblyError> {
    sessions
        .delete(DeleteRequest {
            app_name: APP_NAME.to_owned(),
            user_id: user_id.to_owned(),
            session_id: session_id.to_owned(),
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// Create the session a re-seed has just deleted (#1904): no probe first.
async fn create_session(
    sessions: &dyn SessionService,
    user_id: &UserId,
    session_id: &SessionId,
) -> Result<Box<dyn adk_rust::session::Session>, NativeAgentAssemblyError> {
    sessions
        .create(CreateRequest {
            app_name: APP_NAME.to_owned(),
            user_id: user_id.to_string(),
            session_id: Some(session_id.to_string()),
            state: HashMap::new(),
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// How a fresh turn opens the reader's durable session (#1672, #1904,
/// #1905): the session restored (and, when it provably holds what the view
/// holds, rebound to it), or a new lineage seeded from the view.
struct HistorySession {
    session: Box<dyn adk_rust::session::Session>,
    /// No session existed, or a re-seed deleted it: seed it from the view.
    created: bool,
    /// The restored session was of another view and is seeded again.
    reseeded: bool,
    /// The restored session was rebound to this view without a re-seed:
    /// its marker already names this turn's binding.
    rebound: bool,
}

/// Restore the reader's session for a turn and decide its lineage (#1672).
/// A session bound to another view is rebound when it provably holds what
/// this view holds (#1905), else `restart` ends the old lineage (for a
/// pipeline, its graph frontier too) and a new session is created to be
/// seeded: one read, one delete and one create (#1904).
#[allow(clippy::too_many_arguments)] // The turn's identity, its view and its restart, read once.
async fn open_history_session(
    sessions: &dyn SessionService,
    user_id: &UserId,
    session_id: &SessionId,
    refs: &[super::request::ChatHistoryRef],
    history_len: usize,
    definition_digest: [u8; 32],
    binding: &HistoryBinding,
    restart: impl AsyncFnOnce() -> Result<(), NativeAgentAssemblyError>,
) -> Result<HistorySession, NativeAgentAssemblyError> {
    let (session, created) = restore_or_create_session(sessions, user_id, session_id).await?;
    if created {
        return Ok(HistorySession {
            session,
            created,
            reseeded: false,
            rebound: false,
        });
    }
    match history_decision(
        session.as_ref(),
        binding,
        refs,
        history_len,
        definition_digest,
    ) {
        HistoryDecision::Keep => Ok(HistorySession {
            session,
            created,
            reseeded: false,
            rebound: false,
        }),
        HistoryDecision::Rebind => {
            let identity = session
                .try_identity()
                .map_err(|_| invalid_configuration())?;
            append_reconciliation_marker(sessions, &identity, &[], binding).await?;
            Ok(HistorySession {
                session,
                created,
                reseeded: false,
                rebound: true,
            })
        }
        HistoryDecision::Reseed => {
            restart().await?;
            delete_present_session(sessions, &user_id.to_string(), &session_id.to_string()).await?;
            Ok(HistorySession {
                session: create_session(sessions, user_id, session_id).await?,
                created: true,
                reseeded: true,
                rebound: false,
            })
        }
    }
}

async fn seed_frozen_history(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    chat_history: Vec<Content>,
    refs: &[super::request::ChatHistoryRef],
    definition_digest: [u8; 32],
    binding: &HistoryBinding,
) -> Result<(), NativeAgentAssemblyError> {
    // A view of no messages (`none`, or a first turn) is still bound to its
    // mode (#1672); history without references never is, as its entries are
    // named by ordinal and a marker would make the next turn append them again.
    let marked = refs.len() == chat_history.len() && (!refs.is_empty() || binding.view.is_some());
    for (ordinal, content) in chat_history.into_iter().enumerate() {
        // ADR-0050 decision 9c: with references, each seeded event is named
        // by its message, so a later turn's reconciliation knows it is here.
        let id = match refs.get(ordinal) {
            Some(reference) => history_message_event_id(definition_digest, &reference.message_id),
            None => frozen_history_event_id(
                definition_digest,
                u64::try_from(ordinal).map_err(|_| resource_exhausted())?,
            ),
        };
        append_history_event(sessions, identity, id, content).await?;
    }
    if marked {
        append_reconciliation_marker(sessions, identity, &[], binding).await?;
    }
    Ok(())
}

async fn append_history_event(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    id: String,
    content: Content,
) -> Result<(), NativeAgentAssemblyError> {
    let mut event = Event::with_id(id, "frozen-current-history");
    event.author = if content.role == "user" {
        "user".to_owned()
    } else {
        ROOT_AGENT_NAME.to_owned()
    };
    event.llm_response.content = Some(content);
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// The session state key that says its history entries are named by message
/// (ADR-0050 decision 9c). Its value lists the messages a session seeded
/// before field 71 holds under ordinal ids (`fh-`), so they are never
/// appended again. A session seeded with references lists none.
///
/// #1672: `view` is the context view mode the session was seeded under, and
/// `tagged` says every quotable message it holds carries its `[message m-…]`
/// id (#1865). A session without `view` was seeded before either.
/// #1902: `settings` is Main's binding of the view's eligibility settings; a
/// session without it was bound before the field, to the mode alone.
const HISTORY_RECONCILED_KEY: &str = "elitea.history.reconciled.v1";

/// What a turn binds the reader's durable session to (#1672, #1865, #1902).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(super) struct HistoryBinding {
    /// The context view mode of this turn's history; `None` for an input
    /// from before the view, which never re-seeds.
    view: Option<String>,
    /// Main's digest of every eligibility setting of the view (#1902);
    /// `None` for an input from before it, bound to `view` alone.
    settings: Option<String>,
    /// This turn asks its own question. A continuation or a resume runs on
    /// the session it paused in and never re-seeds it.
    fresh: bool,
    /// The sub-agent tools are bound: this turn's history carries ids.
    tagging: bool,
    /// After this turn, every quotable message the session holds from it
    /// (history and question) carries its id.
    tagged: bool,
}

/// What a fresh turn does with the reader's restored session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum HistoryDecision {
    /// It is bound to this view: reconcile it.
    Keep,
    /// It is bound to another view, or to none, but holds exactly what this
    /// view holds (#1905): record this binding and keep its tool results,
    /// compaction summary and cached prefix.
    Rebind,
    /// Start a fresh lineage from this view (#1672).
    Reseed,
}

/// Whether a restored session must start a fresh lineage from this turn's
/// view (#1672): it was seeded under another mode or settings (#1902), or
/// before the view was recorded (it may hold side talk the view now leaves
/// out, or hold entries in an order a widened view does not have), and it
/// cannot be shown to hold what this view holds (#1905). With the sub-agent
/// tools bound, a session holding untagged messages is re-seeded too
/// (#1865), so every message the brief view offers is one the model saw
/// with its id; no check can prove an untagged session tagged.
fn history_decision(
    session: &dyn adk_rust::session::Session,
    binding: &HistoryBinding,
    refs: &[super::request::ChatHistoryRef],
    history_len: usize,
    definition_digest: [u8; 32],
) -> HistoryDecision {
    let Some(view) = binding.view.as_deref().filter(|_| binding.fresh) else {
        return HistoryDecision::Keep;
    };
    let marker = session.state().get(HISTORY_RECONCILED_KEY);
    let field = |key: &str| marker.as_ref().and_then(|marker| marker.get(key).cloned());
    let bound = field("view").as_ref().and_then(Value::as_str) == Some(view)
        && binding.settings.as_deref().is_none_or(|settings| {
            field("settings").as_ref().and_then(Value::as_str) == Some(settings)
        });
    if binding.tagging && field("tagged").as_ref().and_then(Value::as_bool) != Some(true) {
        return HistoryDecision::Reseed;
    }
    if bound {
        HistoryDecision::Keep
    } else if refs.len() == history_len
        && session_holds_only_view(session, marker.as_ref(), refs, definition_digest)
    {
        HistoryDecision::Rebind
    } else {
        HistoryDecision::Reseed
    }
}

/// #1905: whether a session bound to another view (or to none) provably
/// holds what this view holds, so it can be rebound without a re-seed:
///
/// - every history event it holds is named by its message (`hm-`): none by
///   ordinal (`fh-`, seeded before references) and no marker lists one;
/// - each of those messages is in this view, in the view's order: nothing
///   the view leaves out (side talk, messages before a later join, a
///   narrower mode) is in the session;
/// - every message of the view written before the session's last entry from
///   it, or before the reader's own last turn, is there: a widened view
///   would otherwise append older messages after recent turns. Newer ones
///   are appended by the reconciliation, in order.
///
/// The reader's own turns are not history events: the session holds them
/// under every view, as it always has.
fn session_holds_only_view(
    session: &dyn adk_rust::session::Session,
    marker: Option<&Value>,
    refs: &[super::request::ChatHistoryRef],
    definition_digest: [u8; 32],
) -> bool {
    if marker.is_some_and(|marker| !covered_messages(marker).is_empty()) {
        return false;
    }
    let positions = refs
        .iter()
        .enumerate()
        .map(|(index, reference)| {
            (
                history_message_event_id(definition_digest, &reference.message_id),
                index,
            )
        })
        .collect::<HashMap<_, _>>();
    let mut present = BTreeSet::new();
    let mut last = None;
    for event in session.events().all() {
        if event.id.starts_with("fh-") {
            return false;
        }
        if !event.id.starts_with("hm-") {
            continue;
        }
        let Some(&index) = positions.get(event.id.as_str()) else {
            return false;
        };
        if last.is_some_and(|last| index <= last) {
            return false;
        }
        last = Some(index);
        present.insert(index);
    }
    let frontier = last.max(refs.iter().rposition(|reference| reference.readers_own));
    frontier.is_none_or(|frontier| {
        refs[..=frontier]
            .iter()
            .enumerate()
            .all(|(index, reference)| reference.readers_own || present.contains(&index))
    })
}

/// After a restored session's turn: record that a turn without ids made it
/// hold an untagged message. Written only on that change, so a turn that
/// changes nothing appends nothing.
async fn record_history_binding(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    session: &dyn adk_rust::session::Session,
    binding: &HistoryBinding,
) -> Result<(), NativeAgentAssemblyError> {
    if binding.view.is_none() || binding.tagged {
        return Ok(());
    }
    let Some(marker) = session.state().get(HISTORY_RECONCILED_KEY) else {
        return Ok(());
    };
    if marker.get("tagged").and_then(Value::as_bool) != Some(true) {
        return Ok(());
    }
    // The session stays bound to the view it was seeded under: a turn that
    // does not re-seed (a continuation, a resume) never records its own
    // view, or a narrowed mode would read as already applied.
    let recorded = HistoryBinding {
        view: marker
            .get("view")
            .and_then(Value::as_str)
            .map(str::to_owned),
        settings: marker
            .get("settings")
            .and_then(Value::as_str)
            .map(str::to_owned),
        ..binding.clone()
    };
    let covered = covered_messages(&marker);
    let covered = covered.iter().map(String::as_str).collect::<Vec<_>>();
    append_reconciliation_marker(sessions, identity, &covered, &recorded).await
}

/// A content-less event that sets [`HISTORY_RECONCILED_KEY`]; the model
/// history skips events without content.
async fn append_reconciliation_marker(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    covered: &[&str],
    binding: &HistoryBinding,
) -> Result<(), NativeAgentAssemblyError> {
    let mut event = Event::new("frozen-current-history");
    "elitea-history".clone_into(&mut event.author);
    let mut marker = serde_json::json!({"version": 1, "covered": covered});
    if let Some(view) = &binding.view {
        marker["view"] = Value::from(view.as_str());
        marker["tagged"] = Value::from(binding.tagged);
        if let Some(settings) = &binding.settings {
            marker["settings"] = Value::from(settings.as_str());
        }
    }
    event
        .actions
        .state_delta
        .insert(HISTORY_RECONCILED_KEY.to_owned(), marker);
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// ADR-0050 decision 9c (#1603): a restored session holds the history it was
/// seeded with and the reader's own turns since, never what other
/// participants wrote later. Append, labelled and once, the visible
/// messages it has not seen; the reader's own turns already hold the rest.
///
/// A session seeded before field 71 names its seed by ordinal. Its first
/// reconciliation treats the seeded prefix and everything up to the
/// reader's last own entry as seen, records those messages in the marker,
/// and appends only what came after; later turns match by message.
#[allow(clippy::too_many_arguments)]
async fn reconcile_history(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    session: &dyn adk_rust::session::Session,
    chat_history: &[Content],
    refs: &[super::request::ChatHistoryRef],
    definition_digest: [u8; 32],
    binding: &HistoryBinding,
    rebound: bool,
) -> Result<(), NativeAgentAssemblyError> {
    if refs.is_empty() || refs.len() != chat_history.len() {
        return Ok(());
    }
    let events = session.events().all();
    let present = events
        .iter()
        .map(|event| event.id.as_str())
        .collect::<BTreeSet<_>>();
    // #1905: a rebound session's marker was just written, covering nothing;
    // `session` was read before it.
    let marker = if rebound {
        Some(serde_json::json!({}))
    } else {
        session.state().get(HISTORY_RECONCILED_KEY)
    };
    let (start, covered) = if let Some(marker) = &marker {
        (0, covered_messages(marker))
    } else {
        // The ordinal seed is at most one event per history entry.
        let seeded = (0..refs.len())
            .take_while(|ordinal| {
                u64::try_from(*ordinal).is_ok_and(|ordinal| {
                    present.contains(frozen_history_event_id(definition_digest, ordinal).as_str())
                })
            })
            .count();
        let start = refs
            .iter()
            .rposition(|reference| reference.readers_own)
            .map_or(0, |index| index + 1)
            .max(seeded);
        let covered = refs[..start]
            .iter()
            .filter(|reference| !reference.readers_own)
            .map(|reference| reference.message_id.clone())
            .collect();
        (start, covered)
    };
    for (reference, content) in refs.iter().zip(chat_history).skip(start) {
        if reference.readers_own || covered.contains(&reference.message_id) {
            continue;
        }
        let id = history_message_event_id(definition_digest, &reference.message_id);
        if !present.contains(id.as_str()) {
            append_history_event(sessions, identity, id, content.clone()).await?;
        }
    }
    if marker.is_none() {
        let covered = covered.iter().map(String::as_str).collect::<Vec<_>>();
        // #1672: only a fresh turn, which re-seeds a session of another
        // view, may bind one. A continuation that first reaches a session
        // from before the view leaves it unbound, so the next fresh turn
        // still re-seeds it.
        let unbound = HistoryBinding::default();
        let recorded = if binding.fresh { binding } else { &unbound };
        append_reconciliation_marker(sessions, identity, &covered, recorded).await?;
    }
    Ok(())
}

fn covered_messages(marker: &Value) -> BTreeSet<String> {
    marker
        .get("covered")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .map(str::to_owned)
        .collect()
}

/// #866: append ONE role-`"tool"` event naming every internal tool this
/// invocation's `InternalToolCatalog` skipped, so the run itself says what
/// was requested and not honoured — before this, `InternalToolCatalog::
/// from_values`'s own `agent_internal_tool_skipped` log line was the ONLY
/// trace, visible to an operator reading logs and to nobody else: not the
/// user who turned the toggle on, not the model answering for them.
///
/// A no-op when nothing was skipped (`skipped_tools_notice_text` returns
/// `None`) — the common case, and the same "build nothing, append nothing"
/// shape `seed_frozen_history` already has for an empty `chat_history`.
///
/// Content role is `"tool"` with a plain `Part::Text`, the same shape
/// `adk_core::Event::tool_progress` uses for a tool's own streamed output
/// (`adk-core-2.2.0/src/event.rs`) — chosen over a synthesized
/// `Part::FunctionCall`/`Part::FunctionResponse` pair because there is no
/// real function-call id to pair it with (this runs BEFORE the model's first
/// turn, seeding history rather than responding to a call the model made),
/// and an orphaned `FunctionResponse` risks the model provider's own history
/// validation rejecting the turn outright — a worse failure than the
/// pre-#866 silent skip this fix exists to replace.
async fn seed_skipped_internal_tools_notice(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    internal_tools: InternalToolCatalog,
) -> Result<(), NativeAgentAssemblyError> {
    let Some(notice) = internal_tools.skipped_tools_notice_text() else {
        return Ok(());
    };
    let mut event = Event::new("elitea-skipped-internal-tools");
    "system".clone_into(&mut event.author);
    event.set_content(Content {
        role: "tool".to_owned(),
        parts: vec![adk_rust::Part::Text { text: notice }],
    });
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// The non-`Copy` half of what a fresh session's notices are built from,
/// copied out before `runtime` moves into `build_runtime_agent`.
///
/// One value rather than one binding per notice: the list has grown twice
/// (#973, #983) and each growth otherwise adds a line to the caller AND an
/// argument to the callee, for data that is always read together.
struct FreshSessionNotices {
    skipped_application_children: Vec<SkippedApplicationChild>,
    renamed_tools: Vec<RenamedTool>,
    left_out_toolkits: Vec<LeftOutToolkit>,
}

impl FreshSessionNotices {
    fn from_runtime(runtime: &OrdinaryRuntimeBindings) -> Self {
        Self {
            skipped_application_children: runtime.skipped_application_children.clone(),
            renamed_tools: runtime.renamed_tools.clone(),
            left_out_toolkits: runtime.left_out_toolkits.clone(),
        }
    }
}

/// The four "what this run could not honour" notices, seeded together at the
/// one point a session is created — see each of them for what it reports and
/// why. They are one call so the caller keeps saying `if created { seed … }`
/// once rather than growing a list.
async fn seed_fresh_session_notices(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    internal_tools: InternalToolCatalog,
    notices: &FreshSessionNotices,
) -> Result<(), NativeAgentAssemblyError> {
    seed_skipped_internal_tools_notice(sessions, identity, internal_tools).await?;
    seed_skipped_application_children_notice(
        sessions,
        identity,
        &notices.skipped_application_children,
    )
    .await?;
    seed_renamed_tools_notice(sessions, identity, &notices.renamed_tools).await?;
    seed_left_out_toolkits_notice(sessions, identity, &notices.left_out_toolkits).await
}

/// #1207 review round 2: append ONE role-`"tool"` event naming every
/// configured toolkit this invocation left out because its configuration
/// asks for a setting the Rust worker refuses (plain HTTP, `verify_ssl=false`,
/// a reserved custom header), with the reason.
///
/// Same place, same shape and the same reasons as the notices above. Before
/// it, such a toolkit was skipped with one worker log line while the
/// capability snapshot listed it as supported: the user saw an enabled
/// toolkit that did nothing, and the model could not say why.
async fn seed_left_out_toolkits_notice(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    refused: &[LeftOutToolkit],
) -> Result<(), NativeAgentAssemblyError> {
    let Some(notice) = left_out_toolkits_notice_text(refused) else {
        return Ok(());
    };
    let mut event = Event::new("elitea-refused-toolkits");
    "system".clone_into(&mut event.author);
    event.set_content(Content {
        role: "tool".to_owned(),
        parts: vec![adk_rust::Part::Text { text: notice }],
    });
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// #983: append ONE role-`"tool"` event naming every tool this invocation had
/// to rename, and the connection each renamed name belongs to.
///
/// Same place, same shape and the same reasons as the two notices above it.
/// This one carries more than an operator's record: an agent's instructions
/// were written against the name the server publishes (`echo`), and after a
/// collision the model is offered `conn_a__echo` instead. Without this line the
/// model reads an instruction naming a function it cannot see, and the run
/// ends in an apology rather than a call. A no-op for every agent whose
/// toolsets publish distinct names, which is nearly all of them.
async fn seed_renamed_tools_notice(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    renamed: &[RenamedTool],
) -> Result<(), NativeAgentAssemblyError> {
    let Some(notice) = renamed_tools_notice_text(renamed) else {
        return Ok(());
    };
    let mut event = Event::new("elitea-renamed-tools");
    "system".clone_into(&mut event.author);
    event.set_content(Content {
        role: "tool".to_owned(),
        parts: vec![adk_rust::Part::Text { text: notice }],
    });
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// #973: append ONE role-`"tool"` event naming every attached application
/// child this worker skipped, in the same place and the same shape
/// `seed_skipped_internal_tools_notice` uses one function up — see its comment
/// for why the role is `"tool"` and why a synthesized call/response pair would
/// be worse.
///
/// A no-op when nothing was skipped, which is every run that attaches only
/// agents.
async fn seed_skipped_application_children_notice(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    skipped: &[SkippedApplicationChild],
) -> Result<(), NativeAgentAssemblyError> {
    let Some(notice) = skipped_application_children_notice_text(skipped) else {
        return Ok(());
    };
    let mut event = Event::new("elitea-skipped-application-children");
    "system".clone_into(&mut event.author);
    event.set_content(Content {
        role: "tool".to_owned(),
        parts: vec![adk_rust::Part::Text { text: notice }],
    });
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event,
        })
        .await
        .map_err(|_| dependency_unavailable())
}

/// One message's history event in a reader's session (ADR-0050 decision 9c).
fn history_message_event_id(definition_digest: [u8; 32], message_id: &str) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(HISTORY_MESSAGE_EVENT_DOMAIN);
    context.update(&definition_digest);
    context.update(message_id.as_bytes());
    format!("hm-{}", URL_SAFE_NO_PAD.encode(context.finish().as_ref()))
}

fn frozen_history_event_id(definition_digest: [u8; 32], ordinal: u64) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(FROZEN_HISTORY_EVENT_DOMAIN);
    context.update(&definition_digest);
    context.update(&ordinal.to_be_bytes());
    format!("fh-{}", URL_SAFE_NO_PAD.encode(context.finish().as_ref()))
}

fn stable_identity(domain: &[u8], fields: &[&str]) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(domain);
    for field in fields {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field.as_bytes());
    }
    format!("e1:{}", URL_SAFE_NO_PAD.encode(context.finish().as_ref()))
}

/// Derive a secret-free session lineage independent of one request envelope.
///
/// Saved applications are isolated by their immutable application/version
/// identity. Ad-hoc turns deliberately share one lineage inside the already
/// tenant/project/thread-scoped session so models and bound toolsets may change
/// between chat turns without discarding conversation state.
fn stable_definition_digest(
    request: &AgentExecutionRequest,
) -> Result<[u8; 32], NativeAgentAssemblyError> {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(DEFINITION_DIGEST_DOMAIN);
    match request.kind {
        super::request::AgentExecutionKind::Application => {
            let application_id = positive_json_identity(request.payload.application.get("id"))?;
            let version_id = positive_json_identity(request.payload.application.get("version_id"))?;
            context.update(APPLICATION_DEFINITION_KIND);
            context.update(&application_id.to_be_bytes());
            context.update(&version_id.to_be_bytes());
        }
        super::request::AgentExecutionKind::Adhoc => {
            context.update(ADHOC_DEFINITION_KIND);
        }
    }
    let mut definition_digest = [0_u8; 32];
    definition_digest.copy_from_slice(context.finish().as_ref());
    Ok(definition_digest)
}

fn positive_json_identity(value: Option<&Value>) -> Result<u64, NativeAgentAssemblyError> {
    value
        .and_then(Value::as_u64)
        .filter(|value| (1..=i64::MAX.cast_unsigned()).contains(value))
        .ok_or_else(invalid_input)
}

fn current_numeric_identity(value: &str) -> Value {
    value
        .parse::<u64>()
        .ok()
        .filter(|value| *value > 0)
        .map_or_else(|| Value::String(value.to_owned()), Value::from)
}

fn public_application_details(
    application: &Map<String, Value>,
) -> Result<Value, NativeAgentAssemblyError> {
    let mut application = application.clone();
    application.remove("instructions");
    if let Some(Value::Object(version)) = application.get_mut("version_details")
        && let Some(Value::Array(skills)) = version.get_mut("skills")
    {
        for skill in skills {
            if let Value::Object(skill) = skill {
                skill.remove("instructions");
            }
        }
    }
    if let Some(Value::Object(version)) = application.get_mut("version_details") {
        version.remove("instructions");
        version.remove("project_context");
        // A toolkit's settings hold its resolved credentials (a token, a
        // password, a private key), and these details are published on the
        // final `full_message` to the browser and the replay log.
        if let Some(Value::Array(tools)) = version.get_mut("tools") {
            for tool in tools {
                if let Value::Object(tool) = tool {
                    tool.remove("settings");
                }
            }
        }
    }
    let application = Value::Object(application);
    let length = serde_json::to_vec(&application)
        .map_err(|_| invalid_input())?
        .len();
    if length > MAX_PUBLIC_APPLICATION_DETAILS_BYTES {
        return Err(resource_exhausted());
    }
    Ok(application)
}

fn projection_configuration(_: AgentEventProjectionError) -> NativeAgentAssemblyError {
    invalid_input()
}

fn completed_output_error(error: &AgentEventProjectionError) -> NativeAgentAssemblyError {
    if error.code() == AgentEventProjectionErrorCode::ResourceExhausted {
        resource_exhausted()
    } else {
        invalid_result()
    }
}

fn invalid_configuration() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the invocation-local native agent session could not be configured",
    )
}

fn invalid_input() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "the invocation-local native agent input is malformed",
    )
}

fn invalid_result() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidResult,
        "the invocation-local native agent result is malformed",
    )
}

fn resource_exhausted() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the invocation-local native agent state exceeds its approved limit",
    )
}

fn authorization_failed() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::AuthorizationFailed,
        "the claim-bound native agent session does not match its command",
    )
}

fn dependency_unavailable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::DependencyUnavailable,
        "the invocation-local native agent session is unavailable",
    )
}

fn session_activation_error(error: &PostgresSessionError) -> NativeAgentAssemblyError {
    let code = match error {
        PostgresSessionError::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        PostgresSessionError::InvalidScope | PostgresSessionError::WriterNotCurrent => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        PostgresSessionError::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
        PostgresSessionError::CorruptStoredState | PostgresSessionError::EventConflict => {
            NativeAgentAssemblyErrorCode::InvalidResult
        }
        PostgresSessionError::StorageUnavailable { .. }
        | PostgresSessionError::StorageFailure { .. } => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the claim-bound native agent session is unavailable")
}

fn checkpoint_activation_error(error: &PostgresCheckpointError) -> NativeAgentAssemblyError {
    let code = match error {
        PostgresCheckpointError::InvalidConfiguration(_) => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        PostgresCheckpointError::InvalidScope(_) | PostgresCheckpointError::WriterNotCurrent => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        PostgresCheckpointError::ResourceExhausted(_) => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        PostgresCheckpointError::CheckpointConflict
        | PostgresCheckpointError::CorruptStoredState => {
            NativeAgentAssemblyErrorCode::InvalidResult
        }
        PostgresCheckpointError::StorageUnavailable { .. }
        | PostgresCheckpointError::StorageFailure { .. } => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the claim-bound pipeline checkpoint is unavailable")
}

/// The session a scoped continuation of the pause `pause_id` is rebuilt from:
/// every event up to and including that pause. A dead continuation of this
/// execution after it (`dispatch_journal::continuation_pause`) is set aside, as
/// an ordinary root sets its own aside (`live_nested_events`). Its forwarded
/// child events are not the child's history: a persisted result would make the
/// decided call look finished. The journal facts are already in the decisions.
fn events_through_pause<'a>(
    events: &'a [adk_rust::Event],
    pause_id: &str,
) -> Result<&'a [adk_rust::Event], NativeAgentAssemblyError> {
    events
        .iter()
        .position(|event| event.id == pause_id)
        .map(|index| &events[..=index])
        .ok_or_else(invalid_configuration)
}

fn pipeline_resume_error(error: &PipelineResumeError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        PipelineResumeErrorCode::InvalidInput
        | PipelineResumeErrorCode::StaleDecision
        | PipelineResumeErrorCode::CorruptSession => NativeAgentAssemblyErrorCode::InvalidInput,
        PipelineResumeErrorCode::UnsupportedCapability => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        // An approved call may have run; the run ends with that registered reason.
        PipelineResumeErrorCode::MayHaveStarted => {
            return super::direct_hitl::recovery_refused_error();
        }
        PipelineResumeErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the checkpointed pipeline decision was rejected")
}

fn pipeline_configuration_error(error: &PipelineConfigurationError) -> NativeAgentAssemblyError {
    super::runtime::pipeline_configuration_assembly_error(
        error,
        "the stored pipeline could not be compiled",
    )
}

/// The system instruction: the agent's own `instructions`, then the internal
/// tools' guidance after the SDK's separator, then the Smart Tools Selection
/// index after a blank line. The ONE function that builds it, and the one the
/// fit check measures, so the check and the build cannot disagree.
#[must_use]
pub(crate) fn compose_instructions(
    instructions: &str,
    guidance: Option<&str>,
    index: Option<&str>,
) -> String {
    let mut text = instructions.to_owned();
    if let Some(guidance) = guidance {
        if text.is_empty() {
            text.push_str(guidance.trim_start_matches('\n'));
        } else {
            text.push_str("\n\n---\n\n");
            text.push_str(guidance);
        }
    }
    if let Some(index) = index {
        if !text.is_empty() {
            text.push_str("\n\n");
        }
        text.push_str(index);
    }
    text
}

/// Whether the Smart Tools Selection `index` fits in the system prompt beside
/// the agent's `instructions` and the other internal tools' guidance: the
/// length of the string [`compose_instructions`] would build, against the cap
/// [`OrdinaryRuntimeBindings::instruction_suffix`] enforces.
#[must_use]
pub(crate) fn lazy_index_fits(
    instructions: &str,
    internal_tools: InternalToolCatalog,
    index: &str,
) -> bool {
    compose_instructions(
        instructions,
        internal_tools.instruction_addendum(),
        Some(index),
    )
    .len()
        <= elitea_agent_runtime::request::MAX_AGENT_INSTRUCTION_BYTES
}

#[cfg(test)]
mod public_application_metadata_tests {
    use super::*;

    #[test]
    fn large_instructions_do_not_consume_public_event_metadata_capacity() {
        let instructions = "private pipeline instructions ".repeat(4096);
        let application = serde_json::json!({
            "id": 83,
            "name": "Inspection pipeline",
            "instructions": instructions,
            "version_details": {
                "id": 90,
                "agent_type": "pipeline",
                "instructions": instructions,
                "skills": [{"id": 7, "version_id": 8, "instructions": "skill body"}],
                "project_context": {"instructions": "project body"}
            }
        });
        let original = application.as_object().expect("application");
        let projected = public_application_details(original).expect("bounded public metadata");
        assert_eq!(projected["id"], 83);
        assert_eq!(projected["version_details"]["id"], 90);
        assert_eq!(projected["version_details"]["agent_type"], "pipeline");
        assert_eq!(projected["version_details"]["skills"][0]["version_id"], 8);
        assert!(projected.get("instructions").is_none());
        assert!(projected["version_details"].get("instructions").is_none());
        assert!(
            projected["version_details"]
                .get("project_context")
                .is_none()
        );
        assert!(
            projected["version_details"]["skills"][0]
                .get("instructions")
                .is_none()
        );
        assert_eq!(original["version_details"]["instructions"], instructions);
        assert_eq!(original["instructions"], instructions);
    }

    #[test]
    fn toolkit_credentials_never_reach_public_event_metadata() {
        let application = serde_json::json!({
            "id": 83,
            "version_details": {
                "id": 90,
                "tools": [{
                    "id": 2,
                    "name": "jira",
                    "type": "jira",
                    "settings": {"jira_configuration": {"token": "canary-token"}}
                }]
            }
        });
        let original = application.as_object().expect("application");
        let projected = public_application_details(original).expect("bounded public metadata");
        assert_eq!(projected["version_details"]["tools"][0]["name"], "jira");
        assert!(
            projected["version_details"]["tools"][0]
                .get("settings")
                .is_none()
        );
        assert!(!projected.to_string().contains("canary-token"));
        assert_eq!(
            original["version_details"]["tools"][0]["settings"]["jira_configuration"]["token"],
            "canary-token"
        );
    }
}

#[cfg(test)]
mod fanout_start_tests {
    use super::*;
    use crate::agents::assembly::OrdinaryNoToolProfile;
    use crate::agents::assembly_tests::ordinary_request;
    use crate::agents::request::AgentExecutionKind;
    use crate::agents::runtime::NativeAgentAssemblyCause;

    const NOW_UNIX_MILLIS: i64 = 1_800_000_000_000;

    #[test]
    fn the_remaining_command_time_becomes_the_fanout_instant() {
        let now = tokio::time::Instant::now();
        let deadline = execution_deadline_at(NOW_UNIX_MILLIS + 1_500, NOW_UNIX_MILLIS, now)
            .expect("a future deadline is accepted");
        assert_eq!(deadline - now, std::time::Duration::from_millis(1_500));
        let last = execution_deadline_at(NOW_UNIX_MILLIS + 1, NOW_UNIX_MILLIS, now)
            .expect("one millisecond left is still a deadline");
        assert_eq!(last - now, std::time::Duration::from_millis(1));
    }

    #[test]
    fn a_deadline_that_has_passed_is_refused_typed() {
        let now = tokio::time::Instant::now();
        for deadline in [NOW_UNIX_MILLIS, NOW_UNIX_MILLIS - 1, 0, -1, i64::MIN] {
            let error = execution_deadline_at(deadline, NOW_UNIX_MILLIS, now)
                .expect_err("a passed deadline never starts a fan-out");
            assert_eq!(
                error.code(),
                NativeAgentAssemblyErrorCode::InvalidConfiguration
            );
            assert_eq!(
                error.cause().map(NativeAgentAssemblyCause::code),
                Some("graph.fanout.deadline_passed")
            );
        }
    }

    #[test]
    fn a_passed_deadline_admits_no_code_and_does_not_fail_assembly() {
        let now = tokio::time::Instant::now();
        for deadline in [NOW_UNIX_MILLIS, NOW_UNIX_MILLIS - 1, 0, -1, i64::MIN] {
            let instant = code_admission_deadline_at(deadline, NOW_UNIX_MILLIS, now)
                .expect("a passed deadline is not an assembly failure");
            assert_eq!(instant, now, "nothing is left for a Code attempt");
        }
        let future = code_admission_deadline_at(NOW_UNIX_MILLIS + 1_500, NOW_UNIX_MILLIS, now)
            .expect("a future deadline is accepted");
        assert_eq!(future - now, std::time::Duration::from_millis(1_500));
    }

    #[test]
    fn the_plan_carries_the_command_deadline() {
        let request = ordinary_request(AgentExecutionKind::Application);
        let profile = OrdinaryNoToolProfile::validate(&request).expect("profile");
        let plan = |deadline| {
            OrdinaryNativeAgentPlan::from_authorized(
                &request,
                &profile,
                &AuthorizedNativeCommandBinding::fixture().with_deadline(deadline),
                &[],
            )
            .expect("plan")
        };
        let future = chrono::Utc::now().timestamp_millis() + 60_000;
        let remaining = plan(future)
            .execution_deadline()
            .expect("a running command has time left")
            .saturating_duration_since(tokio::time::Instant::now());
        assert!(
            remaining > std::time::Duration::from_secs(50)
                && remaining <= std::time::Duration::from_mins(1),
            "{remaining:?}"
        );
        let passed = chrono::Utc::now().timestamp_millis() - 1;
        assert!(plan(passed).execution_deadline().is_err());
    }

    fn wake_start() -> PipelineNativeStart {
        let mut payload = ordinary_request(AgentExecutionKind::Application).payload;
        payload.user_input = crate::agents::request::UserInput::Text(String::new());
        payload.meta.insert(
            super::super::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
            serde_json::from_str(include_str!(
                "../../../../libs/jsonschema/runtime/v1/fixtures/fanout-wake-v1.json"
            ))
            .expect("wake fixture"),
        );
        super::super::runtime::pipeline_continuation_start(&payload).expect("wake start")
    }

    /// Resolve a wake against a session and a root frontier of `pending` nodes.
    /// The parked execution the wake fixture names (`fanout-wake-v1.json`).
    fn parked_writer() -> serde_json::Value {
        serde_json::json!(["0123456789abcdef0123456789abcdef", 1])
    }

    async fn resolve_wake(
        pending: Option<Vec<String>>,
        writer: serde_json::Value,
    ) -> Result<Option<super::super::graph::resume::PipelineResume>, NativeAgentAssemblyError> {
        let request = ordinary_request(AgentExecutionKind::Application);
        let profile = OrdinaryNoToolProfile::validate(&request).expect("profile");
        let plan = OrdinaryNativeAgentPlan::from_authorized(
            &request,
            &profile,
            &AuthorizedNativeCommandBinding::fixture(),
            &[],
        )
        .expect("plan");
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        sessions
            .create(adk_rust::session::CreateRequest {
                app_name: APP_NAME.to_owned(),
                user_id: plan.user_id.to_string(),
                session_id: Some(plan.session_id.to_string()),
                state: HashMap::new(),
            })
            .await
            .expect("parked session");
        let checkpointer: Arc<dyn Checkpointer> =
            Arc::new(adk_rust::graph::MemoryCheckpointer::new());
        if let Some(pending) = pending {
            let mut parked = adk_rust::graph::Checkpoint::new(
                plan.session_id.as_ref(),
                HashMap::from([("pre_done".to_owned(), serde_json::json!(true))]),
                2,
                pending,
            );
            parked
                .metadata
                .insert("elitea.pipeline.execution.v1".to_owned(), writer);
            checkpointer.save(&parked).await.expect("parked frontier");
        }
        let state = PipelineStateServices {
            event_source: None,
            sessions: Arc::clone(&sessions),
            checkpointer,
            parallel_authority: None,
            node_recovery_authority: None,
            fanout_decisions: None,
            model_scopes: plan.model_scope_sessions(
                super::super::model_scope::ModelScopeBackend::Local(sessions),
            ),
        };
        resolve_pipeline_start(
            wake_start(),
            ContinuationEntry::default(),
            &state,
            &plan,
            &super::super::graph::PrinterPauseCatalog::default(),
            &super::super::graph::static_pause::StaticPauseFamily::new(
                super::super::graph::static_pause::StaticPauseCatalog::default(),
                std::collections::BTreeMap::new(),
            ),
            &ApplicationToolPresentationCatalog::default(),
            None,
            None,
        )
        .await
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_wake_resumes_the_parked_frontier_with_an_empty_resume_state() {
        let resume = resolve_wake(Some(vec!["gather".to_owned()]), parked_writer())
            .await
            .expect("a parked frontier admits the wake")
            .expect("a wake is an explicit resume, so the turn reads the parked frontier");
        assert!(
            resume.state().is_empty(),
            "a wake adds nothing to the business state"
        );
    }

    /// REVIEW-1470 #5: a wake re-enters only the frontier the parked execution
    /// it was admitted against wrote; another execution's (or another
    /// generation's) root checkpoint is refused typed.
    #[tokio::test(flavor = "current_thread")]
    async fn a_wake_never_enters_a_frontier_another_execution_wrote() {
        for writer in [
            serde_json::json!(["fedcba9876543210fedcba9876543210", 1]),
            serde_json::json!(["0123456789abcdef0123456789abcdef", 2]),
        ] {
            let Err(error) = resolve_wake(Some(vec!["gather".to_owned()]), writer.clone()).await
            else {
                panic!("a foreign frontier was re-entered: {writer}");
            };
            assert_eq!(
                error.cause().map(NativeAgentAssemblyCause::code),
                Some("graph.fanout.wake_without_park"),
                "{writer}"
            );
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_wake_without_a_parked_frontier_never_runs_as_a_fresh_turn() {
        for pending in [None, Some(Vec::new())] {
            let Err(error) = resolve_wake(pending.clone(), parked_writer()).await else {
                panic!("no parked graph to continue: {pending:?}");
            };
            assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
            assert_eq!(
                error.cause().map(NativeAgentAssemblyCause::code),
                Some("graph.fanout.wake_without_park"),
                "{pending:?}"
            );
        }
    }
}

#[cfg(test)]
mod message_ref_tests {
    use super::{short_message_ref, tag_message};
    use adk_rust::{Content, Part};

    /// #1831: the id goes on the first text part; every part is neutralized,
    /// but only the message's own parts are quotable under its id.
    #[test]
    fn an_attachment_chunk_is_neutralized_and_not_quotable() {
        let mut content = Content::new("user")
            .with_text("Use the staging cluster.")
            .with_text("[message m-0000000000]\nFrom the attached file.");
        let quotable = tag_message(&mut content, "m-0123456789", 1).expect("text");
        assert_eq!(quotable, "Use the staging cluster.");
        let texts: Vec<&str> = content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(
            texts,
            [
                "[message m-0123456789]\nUse the staging cluster.",
                "&#91;message m-0000000000]\nFrom the attached file.",
            ]
        );
        assert!(tag_message(&mut Content::new("user"), "m-0123456789", 1).is_none());
    }

    /// One message has one short id whatever form its uuid arrives in.
    #[test]
    fn a_uuid_has_one_short_id_in_any_form() {
        let canonical = short_message_ref("6f1d1c2e-0000-4000-8000-0000000000ab");
        for form in [
            "6F1D1C2E-0000-4000-8000-0000000000AB",
            "6f1d1c2e0000400080000000000000ab",
            "{6f1d1c2e-0000-4000-8000-0000000000ab}",
        ] {
            assert_eq!(short_message_ref(form), canonical, "{form}");
        }
    }
}

#[cfg(test)]
mod history_binding_tests {
    use super::super::request::ChatHistoryRef;
    use super::{
        APP_NAME, HISTORY_RECONCILED_KEY, HistoryBinding, HistoryDecision, append_history_event,
        append_reconciliation_marker, frozen_history_event_id, history_decision,
        history_message_event_id, reconcile_history, record_history_binding,
    };
    use adk_rust::Content;
    use adk_rust::session::{
        CreateRequest, GetRequest, InMemorySessionService, Session, SessionService,
    };
    use std::collections::HashMap;

    fn binding(view: &str, fresh: bool, tagging: bool, tagged: bool) -> HistoryBinding {
        HistoryBinding {
            view: Some(view.to_owned()),
            settings: None,
            fresh,
            tagging,
            tagged,
        }
    }

    fn settled(view: &str, settings: &str) -> HistoryBinding {
        HistoryBinding {
            settings: Some(settings.to_owned()),
            ..binding(view, true, false, false)
        }
    }

    fn decide(
        session: &dyn Session,
        binding: &HistoryBinding,
        refs: &[ChatHistoryRef],
    ) -> HistoryDecision {
        history_decision(session, binding, refs, refs.len(), [0; 32])
    }

    fn reference(n: u32, readers_own: bool) -> ChatHistoryRef {
        ChatHistoryRef {
            message_id: format!("00000000-0000-4000-8000-{n:012}"),
            readers_own,
        }
    }

    /// A session seeded under `full` with `settings`, holding `held` as
    /// history events named by message.
    async fn seeded(
        sessions: &InMemorySessionService,
        held: &[&ChatHistoryRef],
        settings: Option<&str>,
    ) -> Box<dyn Session> {
        let identity = created(sessions).await.try_identity().expect("identity");
        for reference in held {
            append_history_event(
                sessions,
                &identity,
                history_message_event_id([0; 32], &reference.message_id),
                Content::new("user").with_text(&reference.message_id),
            )
            .await
            .expect("held");
        }
        let bound = HistoryBinding {
            settings: settings.map(str::to_owned),
            ..binding("full", true, false, false)
        };
        append_reconciliation_marker(sessions, &identity, &[], &bound)
            .await
            .expect("marked");
        restored(sessions).await
    }

    async fn restored(sessions: &InMemorySessionService) -> Box<dyn Session> {
        sessions
            .get(GetRequest {
                app_name: APP_NAME.to_owned(),
                user_id: "u".to_owned(),
                session_id: "s".to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("session")
    }

    async fn created(sessions: &InMemorySessionService) -> Box<dyn Session> {
        sessions
            .create(CreateRequest {
                app_name: APP_NAME.to_owned(),
                user_id: "u".to_owned(),
                session_id: Some("s".to_owned()),
                state: HashMap::new(),
            })
            .await
            .expect("created")
    }

    /// #1672 review: a turn that does not re-seed (an output continuation
    /// with untagged content) keeps the view the session was seeded under,
    /// so a narrowed mode still re-seeds on the next fresh turn.
    #[tokio::test]
    async fn a_continuation_never_records_its_own_view() {
        let sessions = InMemorySessionService::new();
        let identity = created(&sessions).await.try_identity().expect("identity");
        append_reconciliation_marker(
            &sessions,
            &identity,
            &[],
            &binding("full", true, true, true),
        )
        .await
        .expect("seeded under full");
        let continuation = binding("none", false, false, false);
        let session = restored(&sessions).await;
        assert_eq!(
            decide(session.as_ref(), &continuation, &[]),
            HistoryDecision::Keep
        );
        record_history_binding(&sessions, &identity, session.as_ref(), &continuation)
            .await
            .expect("recorded");
        let session = restored(&sessions).await;
        let marker = session.state().get(HISTORY_RECONCILED_KEY).expect("marker");
        assert_eq!(marker["view"], "full");
        assert_eq!(marker["tagged"], false);
        // Still bound to `full`, so a fresh `none` turn is not kept; the
        // session holds no history the narrower view leaves out (#1905).
        assert_eq!(
            decide(session.as_ref(), &binding("none", true, false, false), &[]),
            HistoryDecision::Rebind
        );
    }

    /// #1672 review: a continuation that first reaches a session from before
    /// the view leaves it unbound, so the next fresh turn re-seeds it once.
    #[tokio::test]
    async fn a_continuation_leaves_an_old_session_unbound() {
        let sessions = InMemorySessionService::new();
        let session = created(&sessions).await;
        let identity = session.try_identity().expect("identity");
        let history = [Content::new("user").with_text("kickoff")];
        let refs = [reference(301, false)];
        reconcile_history(
            &sessions,
            &identity,
            session.as_ref(),
            &history,
            &refs,
            [0; 32],
            &binding("full", false, false, false),
            false,
        )
        .await
        .expect("reconciled");
        let session = restored(&sessions).await;
        let marker = session.state().get(HISTORY_RECONCILED_KEY).expect("marker");
        assert!(marker.get("view").is_none(), "{marker}");
        // Unbound, so the next fresh turn is not kept: it holds exactly the
        // view and is rebound (#1905), but an untagged session is re-seeded
        // once the sub-agent tools are bound (#1865).
        assert_eq!(
            decide(session.as_ref(), &binding("full", true, false, true), &refs),
            HistoryDecision::Rebind
        );
        assert_eq!(
            decide(session.as_ref(), &binding("full", true, true, true), &refs),
            HistoryDecision::Reseed
        );
    }

    /// #1902: the mode is not the whole view. A changed setting under the
    /// same mode is not kept; a session bound before settings were sent, or
    /// an input from before them, is bound to the mode alone.
    #[tokio::test]
    async fn a_settings_change_under_the_same_mode_is_not_kept() {
        let kickoff = reference(1, false);
        let side = reference(2, false);
        let sessions = InMemorySessionService::new();
        let session = seeded(&sessions, &[&kickoff, &side], Some("s1")).await;
        let refs = [kickoff.clone(), side.clone()];
        assert_eq!(
            decide(session.as_ref(), &settled("full", "s1"), &refs),
            HistoryDecision::Keep
        );
        assert_eq!(
            decide(
                session.as_ref(),
                &binding("full", true, false, false),
                &refs
            ),
            HistoryDecision::Keep,
            "an input from before settings is bound to the mode"
        );
        // Side talk turned off: the session holds a message the view drops.
        assert_eq!(
            decide(session.as_ref(), &settled("full", "s2"), &[kickoff.clone()]),
            HistoryDecision::Reseed
        );
        // A setting that changes nothing this session holds rebinds it.
        assert_eq!(
            decide(session.as_ref(), &settled("full", "s2"), &refs),
            HistoryDecision::Rebind
        );
        let old = InMemorySessionService::new();
        let session = seeded(&old, &[&kickoff], None).await;
        assert_eq!(
            decide(session.as_ref(), &settled("full", "s1"), &[kickoff]),
            HistoryDecision::Rebind,
            "a session bound before settings is not kept, but provably holds the view"
        );
    }

    /// #1905: the proof that a session holds only what the view holds.
    #[tokio::test]
    async fn a_session_is_rebound_only_when_it_provably_holds_the_view() {
        let one = reference(1, false);
        let two = reference(2, false);
        let three = reference(4, false);
        let stray = reference(9, false);
        // The reader's own turn sits between `two` and `three`.
        let view = [one.clone(), two.clone(), reference(3, true), three.clone()];
        let fresh = binding("since_joined", true, false, false);
        let cases: [(&[&ChatHistoryRef], HistoryDecision, &str); 5] = [
            (
                &[&one, &two],
                HistoryDecision::Rebind,
                "newer messages are appended",
            ),
            (
                &[&one, &two, &three],
                HistoryDecision::Rebind,
                "every held message in order",
            ),
            (
                &[&two],
                HistoryDecision::Reseed,
                "a message before the reader's turn is missing",
            ),
            (
                &[&two, &one],
                HistoryDecision::Reseed,
                "held out of the view's order",
            ),
            (
                &[&one, &two, &stray],
                HistoryDecision::Reseed,
                "holds what the view drops",
            ),
        ];
        for (held, expected, why) in cases {
            let sessions = InMemorySessionService::new();
            let session = seeded(&sessions, held, None).await;
            assert_eq!(decide(session.as_ref(), &fresh, &view), expected, "{why}");
        }
        let sessions = InMemorySessionService::new();
        let session = seeded(&sessions, &[&one], None).await;
        assert_eq!(
            history_decision(session.as_ref(), &fresh, &view[..1], 2, [0; 32]),
            HistoryDecision::Reseed,
            "history without a reference for every entry is never proven"
        );
        let ordinal = InMemorySessionService::new();
        let identity = created(&ordinal).await.try_identity().expect("identity");
        append_history_event(
            &ordinal,
            &identity,
            frozen_history_event_id([0; 32], 0),
            Content::new("user").with_text("kickoff"),
        )
        .await
        .expect("ordinal seed");
        let session = restored(&ordinal).await;
        assert_eq!(
            decide(session.as_ref(), &fresh, &view),
            HistoryDecision::Reseed,
            "an ordinal seed names no message"
        );
    }
}
