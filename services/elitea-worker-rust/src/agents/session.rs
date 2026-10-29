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

use std::collections::HashMap;
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
        }
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
    /// The execution's public egress policy and ledger (#1490), the same
    /// instance every path of this execution uses.
    egress: std::sync::Arc<elitea_agent_runtime::egress::RunEgress>,
}

impl AuthorizedNativeCommandBinding {
    pub(crate) fn from_verified(
        verified: &VerifiedAgentCommand,
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
        Ok(Self::new(command, agent))
    }

    fn new(
        command: &crate::protocol::elitea::runtime::v1::WorkerCommandV1,
        agent: &AgentExecutionCommandV1,
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
            egress: crate::protocol::egress::run_egress(command),
        }
    }

    /// The execution's public egress policy and ledger (#1490).
    pub(crate) const fn egress(&self) -> &std::sync::Arc<elitea_agent_runtime::egress::RunEgress> {
        &self.egress
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
            egress: elitea_agent_runtime::egress::RunEgress::open(),
        }
    }

    #[cfg(test)]
    pub(super) fn fixture_for_execution(execution_id: &str) -> Self {
        Self {
            execution_id: execution_id.to_owned(),
            ..Self::fixture()
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
    /// This invocation's stop after a failed dispatch record, shared by every
    /// dispatch journal and guarded model of the run (`dispatch_journal`).
    dispatch_abort: super::dispatch_journal::DispatchAbort,
}

impl OrdinaryNativeAgentPlan {
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
            dispatch_abort: super::dispatch_journal::DispatchAbort::default(),
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
            .map(|(sessions, _)| sessions)
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
                Ok((service, scopes))
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
                Ok((service, scopes))
            }
            #[cfg(test)]
            Self::Injected(service) => {
                drop(claim);
                let scopes = plan.model_scope_sessions(
                    super::model_scope::ModelScopeBackend::Local(service.clone()),
                );
                Ok((Arc::clone(service), scopes))
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
        }
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
            } => {
                activate_pipeline_postgres(
                    pool,
                    *session_limits,
                    *checkpoint_limits,
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
                    sessions: Arc::clone(sessions),
                    checkpointer: Arc::clone(checkpointer),
                    parallel_authority: None,
                    node_recovery_authority: None,
                    model_scopes: plan.model_scope_sessions(
                        super::model_scope::ModelScopeBackend::Local(sessions.clone()),
                    ),
                })
            }
        }
    }
}

#[allow(clippy::too_many_arguments)] // Keep claim, checkpoint, continuation proof, and Runner assembly visibly ordered.
async fn activate_pipeline_postgres(
    pool: &PgPool,
    session_limits: SessionLimits,
    checkpoint_limits: CheckpointLimits,
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
        sessions,
        checkpointer,
        parallel_authority: Some(authority),
        node_recovery_authority: Some(node_recovery_authority),
        model_scopes,
    })
}

pub(super) struct PipelineStateServices {
    sessions: Arc<dyn SessionService>,
    checkpointer: Arc<dyn Checkpointer>,
    parallel_authority: Option<Arc<dyn super::graph::ParallelCheckpointAuthority>>,
    node_recovery_authority:
        Option<Arc<dyn super::graph::node_recovery_runtime::NodeRecoveryFactory>>,
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
        let evidence = self.inspect_checkpoint(plan, definition).await?;
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
    /// Preserve the single `PostgreSQL` capability holder before trait erasure.
    #[allow(dead_code)] // Fixed Parallel admission is still gated.
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

    pub(super) async fn inspect_checkpoint(
        &self,
        plan: &OrdinaryNativeAgentPlan,
        definition: &PipelineDefinition,
    ) -> Result<ValidatedModelCheckpoint, NativeAgentAssemblyError> {
        let checkpoint = self
            .checkpointer
            .load(plan.session_id.as_ref())
            .await
            .map_err(|_| dependency_unavailable())?
            .ok_or_else(invalid_configuration)?;
        if checkpoint.thread_id != plan.session_id.as_str()
            || checkpoint.metadata.get("elitea.pipeline.execution.v1")
                != Some(&serde_json::json!([plan.execution_id, plan.generation]))
            || !definition.recovery_frontier_supported(&checkpoint.pending_nodes)
            || checkpoint.cleared_interrupt.is_some()
        {
            return Err(invalid_configuration());
        }
        // Bind all frontier data, including graph state, attempts and child receipts.
        // Convert through Value to order map keys before hashing.
        let value = serde_json::to_value(&checkpoint).map_err(|_| invalid_configuration())?;
        let bytes =
            serde_json::to_vec(&("elitea.pipeline.recovery.v1", plan.definition_digest, value))
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

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
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

#[async_trait]
impl SessionService for RunnerSessionService {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.create(req).await
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        let session = self.inner.get(req).await?;
        super::runner_history::project(
            session,
            self.compaction_view
                .as_ref()
                .map(|(digest, agent)| (*digest, agent.as_str())),
        )
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
}

#[async_trait]
impl<M> NativeAgentCompletionSelector for OrdinaryAgentCompletion<M>
where
    M: BoundOrdinaryAgentModel,
{
    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        // A turn wrapped up for a take back was stopped cooperatively: it may
        // have no final text of its own, and it still completes (its work is
        // published) with the take-back note. A turn that finished on its own
        // before the stop fired keeps its answer as it is.
        let wrapped_up = self
            .workspace
            .as_ref()
            .is_some_and(|workspace| workspace.wrap_up().stopped());
        let mut content = match self.model.take_completed_text() {
            Ok(content) => content,
            Err(_) if wrapped_up => String::new(),
            Err(error) => {
                if let Some(workspace) = &self.workspace {
                    let _ = workspace
                        .finish(crate::workspace_run::turn::TurnEnd::Failed)
                        .await;
                }
                return Err(error);
            }
        };
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
        Ok(())
    }

    fn wrap_up(&self) -> Option<Arc<crate::workspace_run::wrap_up::WrapUp>> {
        self.workspace.as_ref().map(|workspace| workspace.wrap_up())
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

pub(super) const PIPELINE_RESUME_MARKER: &str = "[elitea:pipeline-resume:v1]";

/// Build one admitted graph Runner with activated durable state contracts.
#[allow(clippy::too_many_lines)] // Preserve the ordered claim, checkpoint, instruction, and Runner composition.
pub(super) async fn assemble_pipeline_native(
    plan: OrdinaryNativeAgentPlan,
    definition: PipelineDefinition,
    start: PipelineNativeStart,
    state: PipelineStateServices,
    runtime: PipelineRuntimeBindings,
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
    let resume = resolve_pipeline_start(
        start,
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
    let turn_checkpointer = Arc::new(super::graph::turn_checkpointer::TurnCheckpointer::new(
        Arc::clone(&state.checkpointer),
        &plan.execution_id,
        plan.generation,
        is_resume,
    ));
    let graph = definition
        .compile_with_runtime(ROOT_AGENT_NAME, turn_checkpointer, resume, &node_runtimes)
        .map_err(|error| pipeline_configuration_error(&error))?;
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content,
        generation_config: _,
        max_iterations: _,
        projection,
        thread_id,
        chat_history: _,
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
        dispatch_abort,
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
    let runner = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(Arc::new(
            RunnerSessionService::new(state.sessions, None).with_recovery(checkpoint_recovery),
        ))
        .build()
        .map_err(|_| invalid_configuration())?;
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
    let input = if checkpoint_recovery {
        Content::new("user")
    } else if text_resume {
        user_content
    } else if is_resume {
        Content::new("user").with_text(PIPELINE_RESUME_MARKER)
    } else {
        user_content
    };
    Ok(AssembledNativeAgentInvocation::new(
        NativeAgentInvocation::new(runner, user_id, session_id, input),
        projector,
        PipelineAgentCompletion { thread_id },
    ))
}

#[allow(clippy::too_many_arguments, clippy::too_many_lines)] // Keep claim, checkpoint, continuation proof, and Runner assembly visibly ordered.
async fn resolve_pipeline_start(
    start: PipelineNativeStart,
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
        // ADK loads the latest checkpoint even without an explicit resume ID.
        // Regeneration must remove that frontier before the graph starts again.
        // Both services bind deletion to this exact claim, thread and definition.
        state
            .checkpointer
            .delete(plan.session_id.as_ref())
            .await
            .map_err(|_| dependency_unavailable())?;
        reset_session_for_regeneration(state.sessions.as_ref(), &plan.user_id, &plan.session_id)
            .await?;
    }
    Ok(match start {
        PipelineNativeStart::Checkpoint => {
            restore_pipeline_session(state, plan).await?;
            None
        }
        PipelineNativeStart::Fresh | PipelineNativeStart::Regenerate => {
            let (session, created) =
                restore_or_create_session(state.sessions.as_ref(), &plan.user_id, &plan.session_id)
                    .await?;
            if created {
                let identity = session
                    .try_identity()
                    .map_err(|_| invalid_configuration())?;
                seed_frozen_history(
                    state.sessions.as_ref(),
                    &identity,
                    plan.chat_history.clone(),
                    plan.definition_digest,
                )
                .await?;
            }
            None
        }
        PipelineNativeStart::StaticTools(decisions) => {
            let session = restore_pipeline_session(state, plan).await?;
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
            journal
                .record_continuing(&interrupt.id, &interrupt.invocation_id, card)
                .await
                .map_err(|_| dependency_unavailable())?;
            Some(super::graph::resume::PipelineResume::empty())
        }
        PipelineNativeStart::Hitl(decision) => {
            let session = restore_pipeline_session(state, plan).await?;
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
                plan.dispatch_journal(Arc::clone(&state.sessions))?
                    .record_continuing(&pause_id, &pause_invocation_id, &card)
                    .await
                    .map_err(|_| dependency_unavailable())?;
            }
            Some(resume)
        }
        PipelineNativeStart::McpAuthorization(continuation) => {
            let session = restore_pipeline_session(state, plan).await?;
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
            let session = restore_pipeline_session(state, plan).await?;
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
    })
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
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        mut user_content,
        generation_config,
        max_iterations,
        projection,
        thread_id,
        chat_history,
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
        dispatch_abort: _,
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
    let (session, created) = if checkpoint_recovery {
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
        (session, false)
    } else {
        restore_or_create_session(sessions.as_ref(), &user_id, &session_id).await?
    };
    let identity = session
        .try_identity()
        .map_err(|_| invalid_configuration())?;
    if created {
        seed_frozen_history(
            sessions.as_ref(),
            &identity,
            chat_history,
            definition_digest,
        )
        .await?;
        // #866/#973: a fresh session is exactly where `seed_frozen_history`
        // seeds the frozen prior transcript, so it is also the one place to
        // seed the "could not honour" notices once, without repeating them on
        // every later turn of the same conversation — the same "once, at
        // session creation" shape the frozen-history seed itself uses.
        seed_fresh_session_notices(sessions.as_ref(), &identity, internal_tools, &notices).await?;
    }
    tracing::Span::current().record(
        "session_bootstrap",
        if created { "seeded" } else { "restored" },
    );
    tracing::debug!(
        session_bootstrap = if created { "seeded" } else { "restored" },
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
    let runner_sessions = Arc::new(
        RunnerSessionService::new(sessions, model.durable_completion())
            .with_recovery(recovering)
            .with_history_scope(definition_digest, ROOT_AGENT_NAME),
    );
    let mut runner_builder = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(runner_sessions);
    if let Some(compaction) = context_compaction {
        runner_builder = runner_builder.context_compaction(compaction);
    }
    let runner = runner_builder
        .build()
        .map_err(|_| invalid_configuration())?;
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
            },
        ),
        evidence,
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
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content: _,
        generation_config,
        max_iterations,
        projection,
        thread_id,
        chat_history: _,
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
        dispatch_abort: _,
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
    let (agent, projector) = build_runtime_agent(
        journal.abort().guard(prepared.model),
        generation_config,
        max_iterations,
        projection,
        prepared.runtime,
        prepared.parallel_applications,
        None,
    )?;
    let agent = journal.abort().guard_agent(agent);
    let mut runner_builder = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(Arc::new(
            RunnerSessionService::new(sessions, model.durable_completion())
                .with_history_scope(definition_digest, ROOT_AGENT_NAME),
        ));
    if let Some(compaction) = context_compaction {
        runner_builder = runner_builder.context_compaction(compaction);
    }
    let runner = runner_builder
        .build()
        .map_err(|_| invalid_configuration())?;
    Ok(AssembledNativeAgentInvocation::new(
        NativeAgentInvocation::new_direct_hitl(runner, user_id, session_id, prepared.run_input),
        projector,
        OrdinaryAgentCompletion {
            model,
            thread_id,
            workspace: None,
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
        Ok(_) => sessions
            .delete(DeleteRequest {
                app_name: APP_NAME.to_owned(),
                user_id,
                session_id,
            })
            .await
            .map_err(|_| dependency_unavailable()),
        Err(error) if error.code == "session.not_found" => Ok(()),
        Err(_) => Err(dependency_unavailable()),
    }
}

async fn seed_frozen_history(
    sessions: &dyn SessionService,
    identity: &AdkIdentity,
    chat_history: Vec<Content>,
    definition_digest: [u8; 32],
) -> Result<(), NativeAgentAssemblyError> {
    for (ordinal, content) in chat_history.into_iter().enumerate() {
        let ordinal = u64::try_from(ordinal).map_err(|_| resource_exhausted())?;
        let mut event = Event::with_id(
            frozen_history_event_id(definition_digest, ordinal),
            "frozen-current-history",
        );
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
            .map_err(|_| dependency_unavailable())?;
    }
    Ok(())
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
