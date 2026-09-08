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
    install_nested_application_resume, prepare_nested_application_resume,
};
use super::assembly::OrdinaryNoToolProfile;
use super::attachments;
use super::context_management::ContextManagementPlan;
use super::direct_hitl::{
    DirectDelegatedAuthorizationContinuation, DirectHitlDecision, DirectHitlDecisionSet,
    DirectHitlError, DirectHitlErrorCode, DirectHitlRunInput, ResolvedDirectHitlStart,
};
use super::events::{
    AgentEventProjectionContext, AgentEventProjectionError, AgentEventProjectionErrorCode,
    AgentEventProjector, ApplicationToolPresentationCatalog, CompletedAgentBrowserOutput,
    OrdinaryProjectionInput, PipelineProjectionInput,
};
use super::graph::compiler::PipelineDefinition;
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
fn output_continuation_prompt(original_request: &str, visible_content: &str) -> String {
    if visible_content.is_empty() {
        return format!(
            "The previous attempt reached its output-token limit during internal reasoning before it produced visible output. Produce the complete answer now. Follow the original request exactly. Output only the answer and do not refer to the previous attempt.\n\nOriginal request:\n{original_request}"
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

    #[cfg(test)]
    #[must_use]
    pub(crate) const fn presentations(presentations: ApplicationToolPresentationCatalog) -> Self {
        Self {
            presentations,
            events: None,
            resume: None,
        }
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
    application_runtime: ApplicationRuntimeProjection,
}

impl OrdinaryRuntimeBindings {
    #[must_use]
    pub(crate) const fn new(
        toolsets: Vec<Arc<dyn Toolset>>,
        sensitive_tools: SensitiveToolCatalog,
        delegated_authorization: DelegatedAuthorizationCatalog,
        application_runtime: ApplicationRuntimeProjection,
    ) -> Self {
        Self {
            toolsets,
            sensitive_tools,
            delegated_authorization,
            internal_tools: InternalToolCatalog::empty(),
            application_runtime,
        }
    }

    #[must_use]
    pub(crate) const fn with_internal_tools(mut self, internal_tools: InternalToolCatalog) -> Self {
        self.internal_tools = internal_tools;
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
}

fn application_event_agent(
    agent: Arc<dyn Agent>,
    events: ApplicationEventReceiver,
    applications: ApplicationToolPresentationCatalog,
) -> Arc<dyn Agent> {
    Arc::new(ApplicationEventStreamingAgent::new(
        agent,
        events,
        applications,
    ))
}

pub(crate) fn delegated_authorization_agent(
    agent: Arc<dyn Agent>,
    authorization: DelegatedAuthorizationCatalog,
) -> Arc<dyn Agent> {
    if authorization.is_empty() {
        agent
    } else {
        Arc::new(DelegatedAuthorizationEventAgent {
            inner: agent,
            authorization,
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
                    && let Some(requirement) = authorization.requirement_for(&request.tool_name)
                {
                    let Some(encoded) = encode_delegated_authorization_requirement(requirement) else {
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
/// no claim, fence, PAT, output, settlement, or Redis capability. Fields stay
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
        }
    }

    #[cfg(test)]
    pub(super) fn fixture() -> Self {
        Self {
            tenant_id: "tenant-1".to_owned(),
            principal_ref: "user:42".to_owned(),
            resource_project_id: "17".to_owned(),
            projection_project_id: "9".to_owned(),
            execution_id: "execution/one".to_owned(),
            generation: 3,
            client_stream_id: "conversation-1".to_owned(),
            client_message_id: "message-1".to_owned(),
            sio_event: "chat_predict".to_owned(),
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
    capability_id: &'static str,
    definition_digest: [u8; 32],
    tenant_id: String,
    resource_project_id: String,
    projection_project_id: String,
    execution_id: String,
    generation: u64,
    regenerate: bool,
}

impl OrdinaryNativeAgentPlan {
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
            capability_id,
            definition_digest: stable_definition_digest(request)?,
            tenant_id: binding.tenant_id.clone(),
            resource_project_id: binding.resource_project_id.clone(),
            projection_project_id: binding.projection_project_id.clone(),
            execution_id: binding.execution_id.clone(),
            generation: binding.generation,
            regenerate: request.payload.is_regenerate,
        })
    }

    #[cfg(test)]
    pub(super) fn session_id(&self) -> &str {
        self.session_id.as_ref()
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

    pub(crate) async fn open(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
    ) -> Result<Arc<dyn SessionService>, NativeAgentAssemblyError> {
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
                Ok(Arc::new(InMemorySessionService::new()))
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
                Ok(Arc::new(service))
            }
            #[cfg(test)]
            Self::Injected(service) => {
                drop(claim);
                Ok(Arc::clone(service))
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

    async fn open(
        &self,
        authority: ClaimBoundSessionAuthority,
        state_writer_lease: Arc<dyn StateWriterLease>,
        plan: &OrdinaryNativeAgentPlan,
        pipeline_definition_digest: [u8; 32],
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
                    pipeline_definition_digest,
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
                })
            }
        }
    }
}

async fn activate_pipeline_postgres(
    pool: &PgPool,
    session_limits: SessionLimits,
    checkpoint_limits: CheckpointLimits,
    claim: SessionWriterClaimBinding,
    state_writer_lease: Arc<dyn StateWriterLease>,
    plan: &OrdinaryNativeAgentPlan,
    pipeline_definition_digest: [u8; 32],
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
        pipeline_definition_digest,
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
    .map_err(|error| session_activation_error(&error))?;
    let checkpointer = PostgresCheckpointer::activate(
        pool.clone(),
        checkpoint_writer,
        checkpoint_limits,
        state_writer_lease,
    )
    .await
    .map_err(|error| checkpoint_activation_error(&error))?;
    Ok(PipelineStateServices {
        sessions: Arc::new(sessions),
        checkpointer: Arc::new(checkpointer),
    })
}

struct PipelineStateServices {
    sessions: Arc<dyn SessionService>,
    checkpointer: Arc<dyn Checkpointer>,
}

/// One bound provider invocation that remains paired with its completion.
///
/// Implementations may return a cloned ADK model handle only while the owner
/// itself remains the completion selector stored beside the exclusive Runner.
/// This avoids loose model/result values at the authorized composition layer.
pub(crate) trait BoundOrdinaryAgentModel: Send + 'static {
    fn adk_model(&self) -> Arc<dyn Llm>;

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
}

struct CompletionPersistingSessionService {
    inner: Arc<dyn SessionService>,
    completion: Arc<dyn DurableModelCompletion>,
}

impl CompletionPersistingSessionService {
    fn new(inner: Arc<dyn SessionService>, completion: Arc<dyn DurableModelCompletion>) -> Self {
        Self { inner, completion }
    }

    fn durable_event(&self, mut event: Event) -> adk_rust::Result<Event> {
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
        let Some(text) = self.completion.snapshot()? else {
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
impl SessionService for CompletionPersistingSessionService {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.create(req).await
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.inner.get(req).await
    }

    async fn list(&self, req: ListRequest) -> adk_rust::Result<Vec<Box<dyn Session>>> {
        self.inner.list(req).await
    }

    async fn delete(&self, req: DeleteRequest) -> adk_rust::Result<()> {
        self.inner.delete(req).await
    }

    async fn append_event(&self, session_id: &str, event: Event) -> adk_rust::Result<()> {
        self.inner
            .append_event(session_id, self.durable_event(event)?)
            .await
    }

    async fn append_event_for_identity(&self, req: AppendEventRequest) -> adk_rust::Result<()> {
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
}

#[async_trait]
impl<M> NativeAgentCompletionSelector for OrdinaryAgentCompletion<M>
where
    M: BoundOrdinaryAgentModel,
{
    async fn select(self) -> Result<CompletedAgentBrowserOutput, NativeAgentAssemblyError> {
        let content = self.model.take_completed_text()?;
        CompletedAgentBrowserOutput::ordinary(content, self.thread_id)
            .map_err(|error| completed_output_error(&error))
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
}

const PIPELINE_RESUME_MARKER: &str = "[elitea:pipeline-resume:v1]";

/// Activate both durable state contracts and build one admitted graph Runner.
pub(crate) async fn assemble_pipeline_native(
    plan: OrdinaryNativeAgentPlan,
    definition: PipelineDefinition,
    start: PipelineNativeStart,
    session_authority: ClaimBoundSessionAuthority,
    state_writer_lease: Arc<dyn StateWriterLease>,
    backend: &NativePipelineStateBackend,
    runtime: PipelineRuntimeBindings,
) -> Result<AssembledNativeAgentInvocation<PipelineAgentCompletion>, NativeAgentAssemblyError> {
    let PipelineRuntimeBindings {
        nodes: node_runtimes,
        applications: application_runtime,
        node_events,
    } = runtime;
    let ApplicationRuntimeProjection {
        presentations: application_tools,
        events: application_events,
        resume: application_resume,
    } = application_runtime;
    let state = backend
        .open(
            session_authority,
            state_writer_lease,
            &plan,
            definition.definition_digest(),
        )
        .await?;
    let printer_catalog = definition.printer_pause_catalog();
    let printer_resume = matches!(&start, PipelineNativeStart::Printer(_));
    let resume = resolve_pipeline_start(
        start,
        &state,
        &plan,
        &printer_catalog,
        &application_tools,
        application_resume.as_ref(),
    )
    .await?;
    let is_resume = resume.is_some();
    let graph = definition
        .compile_with_runtime(
            ROOT_AGENT_NAME,
            Arc::clone(&state.checkpointer),
            resume,
            &node_runtimes,
        )
        .map_err(|error| pipeline_configuration_error(error.code()))?;
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content,
        generation_config: _,
        max_iterations: _,
        projection,
        thread_id,
        chat_history: _,
        context_management,
        capability_id: _,
        definition_digest: _,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id: _,
        generation: _,
        regenerate: _,
    } = plan;
    // The pipeline graph binds one model per node, so there is no single
    // transcript-wide model to summarize with: an active plan is refused.
    if context_management
        .prepare_runner_composition(None)?
        .is_some()
    {
        return Err(invalid_configuration());
    }
    let agent = pipeline_graph_agent(graph, &state.checkpointer, printer_catalog, is_resume);
    let agent = node_events.map_or(agent.clone(), |events| {
        Arc::new(PipelineNodeEventStreamingAgent::new(agent, events)) as Arc<dyn Agent>
    });
    let agent = application_events.map_or(agent.clone(), |events| {
        application_event_agent(agent, events, application_tools.clone())
    });
    let runner = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(state.sessions)
        .build()
        .map_err(|_| invalid_configuration())?;
    let projector = AgentEventProjector::with_tool_catalogs(
        projection,
        SensitiveToolCatalog::default(),
        application_tools,
    )
    .map_err(projection_configuration)?;
    let input = if printer_resume {
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

/// One graph agent, told whether this invocation STARTS a run or CONTINUES a
/// paused one.
///
/// A turn that is not continuing a pause must not inherit the previous run's
/// checkpoint — `EliteaGraphAgent::starting_a_fresh_run` carries what
/// inheriting it does to the answer.
fn pipeline_graph_agent(
    graph: adk_rust::graph::GraphAgent,
    checkpointer: &Arc<dyn Checkpointer>,
    printer_catalog: super::graph::PrinterPauseCatalog,
    is_resume: bool,
) -> Arc<dyn Agent> {
    let agent = EliteaGraphAgent::new(graph)
        .with_printer_interrupts(Arc::clone(checkpointer), printer_catalog);
    if is_resume {
        return Arc::new(agent);
    }
    Arc::new(agent.starting_a_fresh_run(Arc::clone(checkpointer)))
}

async fn resolve_pipeline_start(
    start: PipelineNativeStart,
    state: &PipelineStateServices,
    plan: &OrdinaryNativeAgentPlan,
    printer_catalog: &super::graph::PrinterPauseCatalog,
    application_tools: &ApplicationToolPresentationCatalog,
    application_resume: Option<&ApplicationResumeCoordinator>,
) -> Result<Option<super::graph::resume::PipelineResume>, NativeAgentAssemblyError> {
    Ok(match start {
        PipelineNativeStart::Fresh => {
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
        PipelineNativeStart::Hitl(decision) => {
            let session = restore_pipeline_session(state, plan).await?;
            let resolved = decision
                .resolve(
                    session.as_ref(),
                    state.checkpointer.as_ref(),
                    ROOT_AGENT_NAME,
                    plan.session_id.as_ref(),
                )
                .await
                .map_err(|error| pipeline_resume_error(&error))?;
            let (resume, application_decisions) = resolved.into_parts();
            if let Some(decisions) = application_decisions {
                let coordinator = application_resume.ok_or_else(invalid_configuration)?;
                install_nested_application_resume(
                    &session.events().all(),
                    decisions,
                    application_tools,
                    coordinator,
                )
                .await?;
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
        PipelineNativeStart::Printer(continuation) => {
            let session = restore_pipeline_session(state, plan).await?;
            Some(
                continuation
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
                    .map_err(|error| pipeline_resume_error(&error))?,
            )
        }
    })
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
    let OrdinaryNativeAgentPlan {
        user_id,
        session_id,
        user_content,
        generation_config,
        max_iterations,
        projection,
        thread_id,
        chat_history,
        context_management,
        capability_id: _,
        definition_digest,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id: _,
        generation: _,
        regenerate,
    } = plan;
    let context_compaction =
        context_management.prepare_runner_composition(Some(model.adk_model()))?;
    let parallel = execution_mode == NativeToolExecutionMode::ParallelApplications;
    if parallel && runtime.has_confirmation_guards() {
        return Err(invalid_configuration());
    }
    let (agent, projector) = build_runtime_agent(
        model.adk_model(),
        generation_config,
        max_iterations,
        projection,
        runtime,
        parallel,
    )?;
    if regenerate {
        reset_session_for_regeneration(sessions.as_ref(), &user_id, &session_id).await?;
    }
    let (session, created) =
        restore_or_create_session(sessions.as_ref(), &user_id, &session_id).await?;
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
    }
    tracing::Span::current().record(
        "session_bootstrap",
        if created { "seeded" } else { "restored" },
    );
    tracing::debug!(
        session_bootstrap = if created { "seeded" } else { "restored" },
        "prepared the ADK session for the native agent runner"
    );
    let runner_sessions: Arc<dyn SessionService> = model.durable_completion().map_or_else(
        || Arc::clone(&sessions),
        |completion| {
            Arc::new(CompletionPersistingSessionService::new(
                Arc::clone(&sessions),
                completion,
            ))
        },
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
    Ok(AssembledNativeAgentInvocation::new(
        invocation,
        projector,
        OrdinaryAgentCompletion { model, thread_id },
    ))
}

fn build_runtime_agent(
    model: Arc<dyn Llm>,
    generation_config: GenerateContentConfig,
    max_iterations: u32,
    projection: AgentEventProjectionContext,
    runtime: OrdinaryRuntimeBindings,
    parallel: bool,
) -> Result<(Arc<dyn Agent>, AgentEventProjector), NativeAgentAssemblyError> {
    let OrdinaryRuntimeBindings {
        toolsets,
        sensitive_tools,
        delegated_authorization,
        internal_tools,
        application_runtime,
    } = runtime;
    let mut builder = LlmAgentBuilder::new(ROOT_AGENT_NAME)
        .model(model)
        .generate_content_config(generation_config)
        .max_iterations(max_iterations)
        .disallow_transfer_to_parent(true)
        .disallow_transfer_to_peers(true);
    if parallel {
        builder = builder.tool_execution_strategy(ToolExecutionStrategy::Parallel);
    }
    for toolset in toolsets {
        builder = builder.toolset(toolset);
    }
    for tool_name in sensitive_tools.tool_names() {
        builder = builder.require_tool_confirmation(tool_name);
    }
    for tool_name in delegated_authorization.tool_names() {
        builder = builder.require_tool_confirmation(tool_name);
    }
    if internal_tools.ask_user_enabled() {
        builder = builder.require_tool_confirmation(ASK_USER_TOOL_NAME);
    }
    let ApplicationRuntimeProjection {
        presentations: application_tools,
        events: application_events,
        resume: _,
    } = application_runtime;
    let agent: Arc<dyn Agent> = Arc::new(builder.build().map_err(|_| invalid_configuration())?);
    let agent = delegated_authorization_agent(agent, delegated_authorization.clone());
    let agent = clarifying_question_agent(agent, internal_tools);
    let agent = application_events.map_or(agent.clone(), |events| {
        application_event_agent(agent, events, application_tools.clone())
    });
    let projector = AgentEventProjector::with_runtime_catalogs(
        projection,
        sensitive_tools,
        delegated_authorization,
        application_tools,
    )
    .map_err(projection_configuration)?;
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

async fn prepare_direct_resume(
    model: Arc<dyn Llm>,
    stored: &dyn Session,
    runtime: OrdinaryRuntimeBindings,
    start: DirectResumeStart,
) -> Result<PreparedDirectResume, NativeAgentAssemblyError> {
    let OrdinaryRuntimeBindings {
        toolsets,
        sensitive_tools,
        delegated_authorization,
        internal_tools,
        application_runtime,
    } = runtime;
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
            let replay = if decision.is_delegated_authorization() {
                (*decision)
                    .into_delegated_authorization_replay(&delegated_authorization)
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
            let prepared = replay.bind(model);
            let (replay_model, run_input, toolsets) = prepared.into_parts(toolsets);
            (replay_model, run_input, toolsets, false)
        }
        ResolvedDirectHitlStart::Nested(decisions) => {
            let coordinator = resume.as_ref().ok_or_else(invalid_configuration)?;
            let prepared = prepare_nested_application_resume(
                &stored.events().all(),
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
                resume: None,
            },
        )
        .with_internal_tools(internal_tools),
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
        capability_id: _,
        definition_digest: _,
        tenant_id: _,
        resource_project_id: _,
        projection_project_id: _,
        execution_id: _,
        generation: _,
        regenerate: _,
    } = plan;
    let context_compaction =
        context_management.prepare_runner_composition(Some(model.adk_model()))?;
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
    let prepared =
        prepare_direct_resume(model.adk_model(), stored.as_ref(), runtime, start).await?;
    let (agent, projector) = build_runtime_agent(
        prepared.model,
        generation_config,
        max_iterations,
        projection,
        prepared.runtime,
        prepared.parallel_applications,
    )?;
    let mut runner_builder = adk_rust::runner::Runner::builder()
        .app_name(APP_NAME)
        .agent(agent)
        .session_service(sessions);
    if let Some(compaction) = context_compaction {
        runner_builder = runner_builder.context_compaction(compaction);
    }
    let runner = runner_builder
        .build()
        .map_err(|_| invalid_configuration())?;
    Ok(AssembledNativeAgentInvocation::new(
        NativeAgentInvocation::new_direct_hitl(runner, user_id, session_id, prepared.run_input),
        projector,
        OrdinaryAgentCompletion { model, thread_id },
    ))
}

fn direct_hitl_error(error: &DirectHitlError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        DirectHitlErrorCode::InvalidInput
        | DirectHitlErrorCode::StaleDecision
        | DirectHitlErrorCode::CorruptSession => NativeAgentAssemblyErrorCode::InvalidInput,
        DirectHitlErrorCode::UnsupportedCapability => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        DirectHitlErrorCode::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
    };
    NativeAgentAssemblyError::new(code, "the direct tool continuation replay was rejected")
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
    if let Some(Value::Object(version)) = application.get_mut("version_details")
        && let Some(Value::Array(skills)) = version.get_mut("skills")
    {
        for skill in skills {
            if let Value::Object(skill) = skill {
                skill.remove("instructions");
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

fn pipeline_resume_error(error: &PipelineResumeError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        PipelineResumeErrorCode::InvalidInput
        | PipelineResumeErrorCode::StaleDecision
        | PipelineResumeErrorCode::CorruptSession => NativeAgentAssemblyErrorCode::InvalidInput,
        PipelineResumeErrorCode::UnsupportedCapability => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        PipelineResumeErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the checkpointed pipeline decision was rejected")
}

fn pipeline_configuration_error(code: &str) -> NativeAgentAssemblyError {
    let code = match code {
        "graph.pipeline.configuration_resource_exhausted" => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        "graph.pipeline.unsupported_capability" => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        "graph.pipeline.malformed_yaml" | "graph.pipeline.invalid_configuration" => {
            NativeAgentAssemblyErrorCode::InvalidInput
        }
        _ => NativeAgentAssemblyErrorCode::InvalidConfiguration,
    };
    NativeAgentAssemblyError::new(code, "the stored pipeline could not be compiled")
}
