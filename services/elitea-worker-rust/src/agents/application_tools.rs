//! Claim-bound nested Elitea applications exposed through ADK `AgentTool`.
//!
//! Main admits exact application/version identities. During the one authorized
//! assembly phase this module resolves each identity once, revalidates the
//! bounded nesting graph, compiles a fresh direct `LlmAgent`, and presents it to
//! the parent model as a source-compatible `task` tool. Stored pipelines remain
//! owned by the graph compiler and fail closed here.

#![allow(dead_code)] // Production registration remains capability-gated.

use std::collections::{BTreeSet, HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::tool::BasicToolset;
use adk_rust::{
    AdkError, Agent, Artifacts, CallbackContext, Content, ErrorCategory, ErrorComponent, Event,
    EventStream, FinishReason, GenerateContentConfig, InvocationContext, Llm, LlmRequest,
    LlmResponse, LlmResponseStream, Memory, Part, ReadonlyContext, RunConfig, SchemaAdapter,
    SecretRequest, Session, State, StreamingMode, Tool, ToolConcurrencyConfig, ToolContext,
    ToolExecutionStrategy, Toolset,
};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use tokio::sync::{Mutex, mpsc};
use tracing::Instrument as _;

use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::direct_hitl::{ResolvedDirectHitlDecision, sensitive_call_identity};
use super::events::{
    APPLICATION_BRANCH_ROOT, ApplicationToolGuardCatalogs, ApplicationToolPresentationCatalog,
    DESCENDANT_CONTAINER_INVOCATION_KEY, DESCENDANT_PARENT_CALL_KEY,
};
use super::internal_tools::{ASK_USER_TOOL_NAME, ASK_USER_TOOLSET_NAME, InternalToolCatalog};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use super::sensitive_tools::{SensitiveToolCatalog, sensitive_tools_for_kind};
use super::session::{
    BoundOrdinaryAgentModel, clarifying_question_agent, delegated_authorization_agent,
};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::toolkits::{
    AdmittedToolSnapshot, DelegatedAuthorizationCatalog, FrozenToolKind, FrozenToolSnapshot,
    FrozenToolSnapshotErrorCode, McpConnector, McpMaterializationErrorCode, ToolAdmissionPolicy,
    ToolBindingError, ToolsetMaterializationErrorCode, bind_toolsets,
    materialize_configured_toolsets_with_tokens_and_authorization,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
use crate::transport::model_facade::{
    ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation, ModelReasoningEffort,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::ClaimScopedEliteaContext;

const MAX_APPLICATION_HOPS: usize = 25;
const MAX_AGENT_TIERS: usize = 3;
const MAX_APPLICATION_TASK_BYTES: usize = 240 * 1_024;
const MAX_AGENT_DESCRIPTION_BYTES: usize = 4 * 1_024;
const MAX_DESCRIPTION_CAPABILITIES: usize = 16;
const APPLICATION_EVENT_CHANNEL_CAPACITY: usize = 64;
const MAX_PARALLEL_APPLICATION_CALLS: usize = 8;
const ADK_LLM_REQUEST_METADATA_KEY: &str = "gcp.vertex.agent.llm_request";
const ADK_LLM_RESPONSE_METADATA_KEY: &str = "gcp.vertex.agent.llm_response";
const NESTED_INTERRUPT_RESULT_KEY: &str = "__elitea_nested_interrupt_v1";
pub(crate) const PIPELINE_APPLICATION_NODE_METADATA_KEY: &str =
    "elitea.pipeline.application_node.v1";

type ApplicationIdentity = (u64, u64);
type ApplicationFuture<'a> = Pin<
    Box<dyn Future<Output = Result<Arc<BuiltApplication>, NativeAgentAssemblyError>> + Send + 'a>,
>;

type ApplicationEventSender = mpsc::Sender<ApplicationEventSignal>;

enum ApplicationEventSignal {
    ContainerEvent(Box<Event>),
    Event {
        container_invocation_id: String,
        parent_call_id: String,
        event: Box<Event>,
    },
    Fatal(ApplicationEventFailure),
}

#[derive(Clone, Copy)]
enum ApplicationEventFailure {
    ChildExecution,
}

#[derive(Clone)]
pub(crate) struct ApplicationEventReceiver {
    inner: Arc<Mutex<Option<mpsc::Receiver<ApplicationEventSignal>>>>,
}

#[derive(Clone, Default)]
pub(crate) struct ApplicationResumeCoordinator {
    inner: Arc<Mutex<ApplicationResumeState>>,
}

#[derive(Default)]
struct ApplicationResumeState {
    root: Option<HashMap<String, ChildApplicationResume>>,
    by_parent_invocation: HashMap<String, HashMap<String, ChildApplicationResume>>,
}

struct ChildApplicationResume {
    tool_name: String,
    arguments: Value,
    ordinal: usize,
    history: Vec<Content>,
    action: ChildApplicationResumeAction,
}

enum ChildApplicationResumeAction {
    Direct(Box<ResolvedDirectHitlDecision>),
    Nested(HashMap<String, ChildApplicationResume>),
}

impl ApplicationResumeCoordinator {
    async fn install_root(
        &self,
        calls: HashMap<String, ChildApplicationResume>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let mut stored = self.inner.lock().await;
        if stored.root.is_some() || calls.is_empty() {
            return Err(invalid_configuration());
        }
        stored.root = Some(calls);
        Ok(())
    }

    async fn install_children(
        &self,
        parent_invocation_id: String,
        calls: HashMap<String, ChildApplicationResume>,
    ) -> adk_rust::Result<()> {
        let mut stored = self.inner.lock().await;
        if calls.is_empty()
            || stored
                .by_parent_invocation
                .insert(parent_invocation_id, calls)
                .is_some()
        {
            return Err(application_event_channel_error());
        }
        Ok(())
    }

    async fn take(
        &self,
        parent_invocation_id: &str,
        call_id: &str,
    ) -> adk_rust::Result<Option<ChildApplicationResume>> {
        let mut stored = self.inner.lock().await;
        if let Some(calls) = stored.by_parent_invocation.get_mut(parent_invocation_id) {
            let resume = calls
                .remove(call_id)
                .ok_or_else(application_event_channel_error)?;
            if calls.is_empty() {
                stored.by_parent_invocation.remove(parent_invocation_id);
            }
            return Ok(Some(resume));
        }
        let Some(calls) = stored.root.as_mut() else {
            return Ok(None);
        };
        let resume = calls
            .remove(call_id)
            .ok_or_else(application_event_channel_error)?;
        if calls.is_empty() {
            stored.root = None;
        }
        Ok(Some(resume))
    }
}

pub(crate) struct PreparedNestedApplicationResume {
    model: Arc<dyn Llm>,
    user_content: Content,
    run_config: RunConfig,
}

impl PreparedNestedApplicationResume {
    pub(crate) fn into_parts(self) -> (Arc<dyn Llm>, Content, RunConfig) {
        (self.model, self.user_content, self.run_config)
    }
}

#[derive(Clone)]
struct ApplicationReplayCall {
    call_id: String,
    tool_name: String,
    arguments: Value,
}

struct ApplicationCallHop {
    event_id: String,
    call_id: String,
    tool_name: String,
    arguments: Value,
    ordinal: usize,
    owned_invocation_id: String,
}

struct ChildApplicationResumeBuilder {
    tool_name: String,
    arguments: Value,
    ordinal: usize,
    owned_invocation_id: String,
    history: Vec<Content>,
    decision: Option<ResolvedDirectHitlDecision>,
    children: HashMap<String, ChildApplicationResumeBuilder>,
}

pub(crate) async fn prepare_nested_application_resume(
    events: &[Event],
    decisions: Vec<ResolvedDirectHitlDecision>,
    applications: &ApplicationToolPresentationCatalog,
    coordinator: &ApplicationResumeCoordinator,
    delegate: Arc<dyn Llm>,
) -> Result<PreparedNestedApplicationResume, NativeAgentAssemblyError> {
    let (child_resumes, submitted_interrupt_ids) =
        build_nested_application_resume(events, decisions, applications)?;
    let calls = application_replay_calls(&child_resumes)?;
    coordinator.install_root(child_resumes).await?;
    let user_content = nested_resume_user_content(&submitted_interrupt_ids);
    Ok(PreparedNestedApplicationResume {
        model: Arc::new(ApplicationReplayModel {
            delegate,
            state: AtomicU8::new(REPLAY_APPLICATIONS_PENDING),
            calls,
            replay_marker: user_content.clone(),
        }),
        user_content,
        run_config: application_run_config(),
    })
}

pub(crate) async fn install_nested_application_resume(
    events: &[Event],
    decisions: Vec<ResolvedDirectHitlDecision>,
    applications: &ApplicationToolPresentationCatalog,
    coordinator: &ApplicationResumeCoordinator,
) -> Result<(), NativeAgentAssemblyError> {
    let (child_resumes, _) = build_nested_application_resume(events, decisions, applications)?;
    coordinator.install_root(child_resumes).await
}

fn build_nested_application_resume(
    events: &[Event],
    decisions: Vec<ResolvedDirectHitlDecision>,
    applications: &ApplicationToolPresentationCatalog,
) -> Result<(HashMap<String, ChildApplicationResume>, HashSet<String>), NativeAgentAssemblyError> {
    let mut builders = HashMap::with_capacity(decisions.len());
    let mut root_event_id = None;
    let mut submitted_interrupt_ids = HashSet::with_capacity(decisions.len());
    for decision in decisions {
        let chain = application_call_chain(events, &decision)?;
        let root_call = chain.last().ok_or_else(invalid_configuration)?;
        if root_event_id
            .get_or_insert_with(|| root_call.event_id.clone())
            .as_str()
            != root_call.event_id
        {
            return Err(unsupported_capability());
        }
        if !submitted_interrupt_ids.insert(decision.interrupt_id().to_owned()) {
            return Err(unsupported_capability());
        }
        insert_resume_decision(&mut builders, &chain, events, applications, decision)?;
    }
    let root_event_id = root_event_id.ok_or_else(invalid_configuration)?;
    validate_complete_nested_decision_set(events, &root_event_id, &submitted_interrupt_ids)?;
    let child_resumes = finish_resume_builders(builders)?;
    Ok((child_resumes, submitted_interrupt_ids))
}

fn application_call_chain(
    events: &[Event],
    decision: &ResolvedDirectHitlDecision,
) -> Result<Vec<ApplicationCallHop>, NativeAgentAssemblyError> {
    let route = decision
        .application_route()
        .ok_or_else(invalid_configuration)?;
    application_call_chain_from(
        events,
        decision.invocation_id(),
        route.container_invocation_id(),
        route.parent_call_id(),
        route.branch(),
    )
}

fn application_call_chain_from(
    events: &[Event],
    leaf_invocation_id: &str,
    first_container_invocation_id: &str,
    first_parent_call_id: &str,
    branch: &str,
) -> Result<Vec<ApplicationCallHop>, NativeAgentAssemblyError> {
    let expected_depth = application_branch_depth(branch).ok_or_else(invalid_configuration)?;
    let mut owned_invocation_id = leaf_invocation_id.to_owned();
    let mut container_invocation_id = first_container_invocation_id.to_owned();
    let mut parent_call_id = first_parent_call_id.to_owned();
    let mut child_branch = branch.to_owned();
    let mut chain = Vec::with_capacity(expected_depth);
    loop {
        if chain.len() == MAX_AGENT_TIERS {
            return Err(resource_exhausted());
        }
        let (event, call, ordinal) =
            exact_application_call(events, &container_invocation_id, &parent_call_id)?;
        let parent_branch =
            application_parent_branch(&child_branch).ok_or_else(invalid_configuration)?;
        if event.branch != parent_branch {
            return Err(invalid_configuration());
        }
        chain.push(ApplicationCallHop {
            event_id: event.id.clone(),
            call_id: parent_call_id,
            tool_name: call.name.to_owned(),
            arguments: call.args.clone(),
            ordinal,
            owned_invocation_id,
        });
        child_branch = parent_branch.to_owned();
        let Some((next_container, next_parent)) = persisted_parent_route(event)? else {
            break;
        };
        owned_invocation_id = event.invocation_id.clone();
        container_invocation_id = next_container;
        parent_call_id = next_parent;
    }
    if chain.len() != expected_depth || child_branch != APPLICATION_BRANCH_ROOT {
        return Err(invalid_configuration());
    }
    Ok(chain)
}

fn exact_application_call<'a>(
    events: &'a [Event],
    container_invocation_id: &str,
    parent_call_id: &str,
) -> Result<(&'a Event, adk_rust::ToolCallView<'a>, usize), NativeAgentAssemblyError> {
    let mut matched = None;
    for event in events
        .iter()
        .filter(|event| event.invocation_id == container_invocation_id)
    {
        for (ordinal, call) in event.tool_calls().into_iter().enumerate() {
            if call.call_id == Some(parent_call_id)
                && matched.replace((event, call, ordinal + 1)).is_some()
            {
                return Err(invalid_configuration());
            }
        }
    }
    matched.ok_or_else(invalid_configuration)
}

fn persisted_parent_route(
    event: &Event,
) -> Result<Option<(String, String)>, NativeAgentAssemblyError> {
    let container = event
        .provider_metadata
        .get(DESCENDANT_CONTAINER_INVOCATION_KEY);
    let parent = event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY);
    match (container, parent) {
        (None, None) if event.branch == APPLICATION_BRANCH_ROOT => Ok(None),
        (Some(container), Some(parent))
            if !container.is_empty()
                && !parent.is_empty()
                && event.branch != APPLICATION_BRANCH_ROOT =>
        {
            Ok(Some((container.clone(), parent.clone())))
        }
        _ => Err(invalid_configuration()),
    }
}

fn application_branch_depth(branch: &str) -> Option<usize> {
    let suffix = branch
        .strip_prefix(APPLICATION_BRANCH_ROOT)?
        .strip_prefix('.')?;
    let mut depth = 0;
    for segment in suffix.split('.') {
        let ordinal = segment.strip_prefix("application_")?;
        if ordinal.is_empty() || !ordinal.bytes().all(|byte| byte.is_ascii_digit()) {
            return None;
        }
        depth += 1;
    }
    (depth > 0 && depth < MAX_AGENT_TIERS).then_some(depth)
}

fn application_parent_branch(branch: &str) -> Option<&str> {
    let (parent, segment) = branch.rsplit_once('.')?;
    let ordinal = segment.strip_prefix("application_")?;
    (!parent.is_empty() && !ordinal.is_empty() && ordinal.bytes().all(|byte| byte.is_ascii_digit()))
        .then_some(parent)
}

fn insert_resume_decision(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    leaf_to_root_chain: &[ApplicationCallHop],
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    decision: ResolvedDirectHitlDecision,
) -> Result<(), NativeAgentAssemblyError> {
    let mut current_builders = builders;
    let mut current_catalog = applications;
    let chain_length = leaf_to_root_chain.len();
    for (index, hop) in leaf_to_root_chain.iter().rev().enumerate() {
        let child_catalog = current_catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        let task = application_task(&hop.arguments)?;
        let mut history = vec![Content::new("user").with_text(task)];
        history.extend(events.iter().filter_map(|event| {
            (event.invocation_id == hop.owned_invocation_id)
                .then(|| event.content().cloned())
                .flatten()
        }));
        let builder = current_builders
            .entry(hop.call_id.clone())
            .or_insert_with(|| ChildApplicationResumeBuilder {
                tool_name: hop.tool_name.clone(),
                arguments: hop.arguments.clone(),
                ordinal: hop.ordinal,
                owned_invocation_id: hop.owned_invocation_id.clone(),
                history: history.clone(),
                decision: None,
                children: HashMap::new(),
            });
        if builder.tool_name != hop.tool_name
            || builder.arguments != hop.arguments
            || builder.ordinal != hop.ordinal
            || builder.owned_invocation_id != hop.owned_invocation_id
        {
            return Err(invalid_configuration());
        }
        if index + 1 == chain_length {
            let leaf_is_admitted = if decision.is_delegated_authorization() {
                true
            } else if decision.is_clarifying_question() {
                current_catalog
                    .nested_internal_tools(&hop.tool_name)
                    .is_some_and(InternalToolCatalog::ask_user_enabled)
            } else {
                current_catalog
                    .nested_sensitive_tools(&hop.tool_name)
                    .and_then(|tools| tools.policy_for(decision.tool_name()))
                    .is_some()
            };
            if !leaf_is_admitted
                || builder.decision.replace(decision).is_some()
                || !builder.children.is_empty()
            {
                return Err(unsupported_capability());
            }
            return Ok(());
        }
        if builder.decision.is_some() {
            return Err(unsupported_capability());
        }
        current_builders = &mut builder.children;
        current_catalog = child_catalog;
    }
    Err(invalid_configuration())
}

fn application_task(arguments: &Value) -> Result<&str, NativeAgentAssemblyError> {
    arguments
        .as_object()
        .filter(|object| object.len() == 1)
        .and_then(|object| object.get("task"))
        .and_then(Value::as_str)
        .filter(|task| {
            !task.is_empty() && task.len() <= MAX_APPLICATION_TASK_BYTES && !task.contains('\0')
        })
        .ok_or_else(invalid_configuration)
}

fn finish_resume_builders(
    builders: HashMap<String, ChildApplicationResumeBuilder>,
) -> Result<HashMap<String, ChildApplicationResume>, NativeAgentAssemblyError> {
    builders
        .into_iter()
        .map(|(call_id, builder)| {
            let action = match (builder.decision, builder.children.is_empty()) {
                (Some(decision), true) => ChildApplicationResumeAction::Direct(Box::new(decision)),
                (None, false) => {
                    ChildApplicationResumeAction::Nested(finish_resume_builders(builder.children)?)
                }
                _ => return Err(unsupported_capability()),
            };
            Ok((
                call_id,
                ChildApplicationResume {
                    tool_name: builder.tool_name,
                    arguments: builder.arguments,
                    ordinal: builder.ordinal,
                    history: builder.history,
                    action,
                },
            ))
        })
        .collect()
}

fn application_replay_calls(
    resumes: &HashMap<String, ChildApplicationResume>,
) -> Result<Vec<ApplicationReplayCall>, NativeAgentAssemblyError> {
    let mut calls = resumes
        .iter()
        .map(|(call_id, resume)| {
            (
                resume.ordinal,
                ApplicationReplayCall {
                    call_id: call_id.clone(),
                    tool_name: resume.tool_name.clone(),
                    arguments: resume.arguments.clone(),
                },
            )
        })
        .collect::<Vec<_>>();
    calls.sort_unstable_by_key(|(ordinal, _)| *ordinal);
    if calls.windows(2).any(|window| window[0].0 == window[1].0) {
        return Err(invalid_configuration());
    }
    Ok(calls.into_iter().map(|(_, call)| call).collect())
}

fn resume_interrupt_ids(
    resumes: &HashMap<String, ChildApplicationResume>,
) -> Result<HashSet<String>, NativeAgentAssemblyError> {
    fn collect(
        resumes: &HashMap<String, ChildApplicationResume>,
        interrupt_ids: &mut HashSet<String>,
    ) -> Result<(), NativeAgentAssemblyError> {
        for resume in resumes.values() {
            match &resume.action {
                ChildApplicationResumeAction::Direct(decision) => {
                    if !interrupt_ids.insert(decision.interrupt_id().to_owned()) {
                        return Err(invalid_configuration());
                    }
                }
                ChildApplicationResumeAction::Nested(children) => {
                    collect(children, interrupt_ids)?;
                }
            }
        }
        Ok(())
    }

    let mut interrupt_ids = HashSet::new();
    collect(resumes, &mut interrupt_ids)?;
    if interrupt_ids.is_empty() {
        return Err(invalid_configuration());
    }
    Ok(interrupt_ids)
}

fn validate_complete_nested_decision_set(
    events: &[Event],
    root_event_id: &str,
    submitted_interrupt_ids: &HashSet<String>,
) -> Result<(), NativeAgentAssemblyError> {
    let mut pending = HashSet::new();
    for event in events
        .iter()
        .filter(|event| event.actions.tool_confirmation.is_some())
    {
        let Some((container, parent)) = persisted_parent_route(event)? else {
            continue;
        };
        let chain = application_call_chain_from(
            events,
            &event.invocation_id,
            &container,
            &parent,
            &event.branch,
        )?;
        if chain
            .last()
            .is_none_or(|root_call| root_call.event_id != root_event_id)
        {
            continue;
        }
        let request = event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(invalid_configuration)?;
        let call_id = request
            .function_call_id
            .as_deref()
            .ok_or_else(invalid_configuration)?;
        let (interrupt_id, _) = sensitive_call_identity(
            &event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .map_err(|_| invalid_configuration())?;
        if !pending.insert(interrupt_id) {
            return Err(invalid_configuration());
        }
    }
    if &pending != submitted_interrupt_ids {
        return Err(unsupported_capability());
    }
    Ok(())
}

fn nested_resume_user_content(interrupt_ids: &HashSet<String>) -> Content {
    let mut interrupt_ids = interrupt_ids.iter().map(String::as_str).collect::<Vec<_>>();
    interrupt_ids.sort_unstable();
    Content::new("user").with_text(format!(
        "[Elitea nested HITL {}] Resume the exact paused saved-agent calls.",
        interrupt_ids.join(",")
    ))
}

const REPLAY_APPLICATIONS_PENDING: u8 = 0;
const REPLAY_APPLICATIONS_EMITTED: u8 = 1;
const REPLAY_APPLICATIONS_DELEGATING: u8 = 2;

struct ApplicationReplayModel {
    delegate: Arc<dyn Llm>,
    state: AtomicU8,
    calls: Vec<ApplicationReplayCall>,
    replay_marker: Content,
}

#[async_trait]
impl Llm for ApplicationReplayModel {
    fn name(&self) -> &str {
        self.delegate.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.delegate.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.delegate.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        match self.state.compare_exchange(
            REPLAY_APPLICATIONS_PENDING,
            REPLAY_APPLICATIONS_EMITTED,
            Ordering::AcqRel,
            Ordering::Acquire,
        ) {
            Ok(_) => {
                self.validate_calls(&request, ApplicationReplayState::Pending)?;
                let parts = self
                    .calls
                    .iter()
                    .map(|call| Part::FunctionCall {
                        name: call.tool_name.clone(),
                        args: call.arguments.clone(),
                        id: Some(call.call_id.clone()),
                        thought_signature: None,
                    })
                    .collect();
                let response = LlmResponse {
                    content: Some(Content {
                        role: "model".to_owned(),
                        parts,
                    }),
                    finish_reason: Some(FinishReason::Stop),
                    turn_complete: true,
                    ..LlmResponse::default()
                };
                Ok(Box::pin(stream::once(async move { Ok(response) })))
            }
            Err(REPLAY_APPLICATIONS_EMITTED) => {
                if self
                    .state
                    .compare_exchange(
                        REPLAY_APPLICATIONS_EMITTED,
                        REPLAY_APPLICATIONS_DELEGATING,
                        Ordering::AcqRel,
                        Ordering::Acquire,
                    )
                    .is_ok()
                {
                    self.validate_calls(&request, ApplicationReplayState::Completed)?;
                }
                self.delegate
                    .generate_content(
                        super::replay_history::model_continuation(request, &self.replay_marker)?,
                        stream_response,
                    )
                    .await
            }
            Err(_) => {
                self.delegate
                    .generate_content(
                        super::replay_history::model_continuation(request, &self.replay_marker)?,
                        stream_response,
                    )
                    .await
            }
        }
    }
}

impl ApplicationReplayModel {
    fn validate_calls(
        &self,
        request: &LlmRequest,
        expected: ApplicationReplayState,
    ) -> adk_rust::Result<()> {
        if self.calls.iter().all(|call| {
            request.tools.contains_key(&call.tool_name)
                && application_replay_state(request, call) == expected
        }) {
            return Ok(());
        }
        Err(application_event_channel_error())
    }
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum ApplicationReplayState {
    Missing,
    Pending,
    Completed,
}

fn application_replay_state(
    request: &LlmRequest,
    expected: &ApplicationReplayCall,
) -> ApplicationReplayState {
    let mut call = None;
    let mut result = None;
    for (position, part) in request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .enumerate()
    {
        match part {
            Part::FunctionCall {
                name,
                args,
                id: Some(call_id),
                ..
            } if call_id == &expected.call_id => {
                call = Some((
                    position,
                    name == &expected.tool_name && args == &expected.arguments,
                ));
            }
            Part::FunctionResponse {
                function_response,
                id: Some(call_id),
                ..
            } if call_id == &expected.call_id => {
                result = Some((position, function_response.name == expected.tool_name));
            }
            _ => {}
        }
    }
    let Some((call_position, true)) = call else {
        return ApplicationReplayState::Missing;
    };
    match result {
        Some((result_position, true)) if result_position > call_position => {
            ApplicationReplayState::Completed
        }
        Some((result_position, _)) if result_position > call_position => {
            ApplicationReplayState::Missing
        }
        _ => ApplicationReplayState::Pending,
    }
}

pub(crate) struct ApplicationToolDependencies<'a> {
    pub(crate) model_facade: Arc<ModelFacade>,
    pub(crate) policy: Arc<ToolAdmissionPolicy>,
    pub(crate) mcp_connector: Arc<dyn McpConnector>,
    mcp_tokens: &'a Map<String, Value>,
    event_sender: Option<ApplicationEventSender>,
    resume: Option<ApplicationResumeCoordinator>,
}

impl<'a> ApplicationToolDependencies<'a> {
    #[must_use]
    pub(crate) fn new(
        model_facade: Arc<ModelFacade>,
        policy: Arc<ToolAdmissionPolicy>,
        mcp_connector: Arc<dyn McpConnector>,
        mcp_tokens: &'a Map<String, Value>,
    ) -> Self {
        Self {
            model_facade,
            policy,
            mcp_connector,
            mcp_tokens,
            event_sender: None,
            resume: None,
        }
    }
}

/// One exact frozen Application alias bound to its invocation-owned ADK tool.
pub(crate) struct MaterializedApplicationTool {
    pub(crate) alias: String,
    pub(crate) agent_type: String,
    model_name: String,
    child_tools: ApplicationToolPresentationCatalog,
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
    pub(crate) tool: Arc<dyn Tool>,
}

struct BuiltApplication {
    agent: Arc<LazyNestedAgent>,
    model_name: String,
    child_tools: ApplicationToolPresentationCatalog,
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
}

/// One root toolset plus its frozen browser-presentation join.
pub(crate) struct MaterializedApplicationToolset {
    pub(crate) toolset: Arc<dyn Toolset>,
    pub(crate) presentations: ApplicationToolPresentationCatalog,
    pub(crate) events: ApplicationEventReceiver,
    pub(crate) resume: ApplicationResumeCoordinator,
}

/// Selected saved-agent tools plus their invocation-owned event projection.
pub(crate) struct MaterializedApplicationRuntime {
    pub(crate) tools: Vec<MaterializedApplicationTool>,
    pub(crate) presentations: ApplicationToolPresentationCatalog,
    pub(crate) events: ApplicationEventReceiver,
    pub(crate) resume: ApplicationResumeCoordinator,
}

/// Build the one nested-application toolset for the root direct agent.
pub(crate) async fn materialize_application_toolset(
    snapshot: &AdmittedToolSnapshot<'_>,
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    fallback_profile: &OrdinaryNoToolProfile,
    dependencies: ApplicationToolDependencies<'_>,
) -> Result<Option<MaterializedApplicationToolset>, NativeAgentAssemblyError> {
    let Some(runtime) = materialize_application_runtime(
        snapshot,
        platform,
        runtime_context,
        elitea_context,
        fallback_profile,
        dependencies,
        None,
    )
    .await?
    else {
        return Ok(None);
    };
    let MaterializedApplicationRuntime {
        tools,
        presentations,
        events,
        resume,
    } = runtime;
    Ok(Some(MaterializedApplicationToolset {
        toolset: Arc::new(BasicToolset::new(
            "elitea_nested_applications",
            tools.into_iter().map(|entry| entry.tool).collect(),
        )),
        presentations,
        events,
        resume,
    }))
}

/// Resolve selected saved agents with one typed descendant-event runtime.
pub(crate) async fn materialize_application_runtime(
    snapshot: &AdmittedToolSnapshot<'_>,
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    fallback_profile: &OrdinaryNoToolProfile,
    mut dependencies: ApplicationToolDependencies<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
) -> Result<Option<MaterializedApplicationRuntime>, NativeAgentAssemblyError> {
    let (event_sender, event_receiver) = mpsc::channel(APPLICATION_EVENT_CHANNEL_CAPACITY);
    let resume = ApplicationResumeCoordinator::default();
    dependencies.event_sender = Some(event_sender);
    dependencies.resume = Some(resume.clone());
    let materialized = materialize_application_tools(
        snapshot,
        platform,
        runtime_context,
        elitea_context,
        fallback_profile,
        dependencies,
        selected_aliases,
    )
    .await?;
    if materialized.is_empty() {
        return Ok(None);
    }
    let mut presentations = ApplicationToolPresentationCatalog::default();
    for entry in &materialized {
        presentations
            .insert_runtime(
                entry.tool.name().to_owned(),
                entry.alias.clone(),
                entry.agent_type.clone(),
                entry.model_name.clone(),
                entry.child_tools.clone(),
                ApplicationToolGuardCatalogs::new(
                    entry.sensitive_tools.clone(),
                    entry.delegated_authorization.clone(),
                    entry.internal_tools,
                ),
            )
            .map_err(|_| invalid_configuration())?;
    }
    Ok(Some(MaterializedApplicationRuntime {
        tools: materialized,
        presentations,
        events: ApplicationEventReceiver {
            inner: Arc::new(Mutex::new(Some(event_receiver))),
        },
        resume,
    }))
}

/// Resolve exact frozen saved applications without changing their graph alias.
pub(crate) async fn materialize_application_tools(
    snapshot: &AdmittedToolSnapshot<'_>,
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    fallback_profile: &OrdinaryNoToolProfile,
    dependencies: ApplicationToolDependencies<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
) -> Result<Vec<MaterializedApplicationTool>, NativeAgentAssemblyError> {
    let references = application_references(snapshot, selected_aliases)?;
    if references.is_empty() {
        return Ok(Vec::new());
    }
    let mut state = ApplicationAssemblyState {
        platform,
        runtime_context,
        model_facade: dependencies.model_facade,
        elitea_context,
        fallback_profile,
        policy: dependencies.policy,
        mcp_connector: dependencies.mcp_connector,
        mcp_tokens: dependencies.mcp_tokens,
        applications: HashMap::new(),
        resolving: HashSet::new(),
        hops: 0,
        event_sender: dependencies.event_sender,
        resume: dependencies.resume,
    };
    let mut tools = Vec::with_capacity(references.len());
    for reference in references {
        let identity = reference.identity;
        let alias = reference.name.clone();
        let agent_type = reference.agent_type.clone();
        let application = state.build(reference.clone(), 2).await?;
        tools.push(MaterializedApplicationTool {
            alias,
            agent_type,
            model_name: application.model_name.clone(),
            child_tools: application.child_tools.clone(),
            sensitive_tools: application.sensitive_tools.clone(),
            delegated_authorization: application.delegated_authorization.clone(),
            internal_tools: application.internal_tools,
            tool: Arc::new(ApplicationAgentTool::new(
                application,
                identity,
                state.event_sender.clone(),
                state.resume.clone(),
            )),
        });
    }
    Ok(tools)
}

struct ApplicationAssemblyState<'a> {
    platform: &'a PlatformClient,
    runtime_context: &'a ClaimBoundRuntimeContextAuthority,
    model_facade: Arc<ModelFacade>,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    fallback_profile: &'a OrdinaryNoToolProfile,
    policy: Arc<ToolAdmissionPolicy>,
    mcp_connector: Arc<dyn McpConnector>,
    mcp_tokens: &'a Map<String, Value>,
    applications: HashMap<ApplicationIdentity, Arc<BuiltApplication>>,
    resolving: HashSet<ApplicationIdentity>,
    hops: usize,
    event_sender: Option<ApplicationEventSender>,
    resume: Option<ApplicationResumeCoordinator>,
}

impl ApplicationAssemblyState<'_> {
    fn build(&mut self, reference: ApplicationReference, tier: usize) -> ApplicationFuture<'_> {
        Box::pin(async move {
            let identity = reference.identity;
            let span = tracing::info_span!(
                "agent.nested_application.assemble",
                application_id = identity.0,
                version_id = identity.1,
                tier,
                cache_hit = tracing::field::Empty,
                stage = tracing::field::Empty,
                outcome = tracing::field::Empty,
                error_code = tracing::field::Empty,
            );
            let result = async {
                self.hops = self.hops.saturating_add(1);
                if self.hops > MAX_APPLICATION_HOPS || tier > MAX_AGENT_TIERS {
                    return Err(resource_exhausted());
                }
                if reference.agent_type != "agent" {
                    return Err(unsupported_capability());
                }
                if reference.project_id.is_some_and(|project_id| {
                    project_id != self.elitea_context.resource_project_id()
                }) {
                    return Err(invalid_configuration());
                }
                if let Some(application) = self.applications.get(&identity) {
                    tracing::Span::current().record("cache_hit", true);
                    return Ok(application.clone());
                }
                tracing::Span::current().record("cache_hit", false);
                if !self.resolving.insert(identity) {
                    return Err(invalid_configuration());
                }
                tracing::Span::current().record("stage", "resolve_version");
                let result = self.build_uncached(reference, tier).await;
                self.resolving.remove(&identity);
                if let Ok(application) = &result {
                    self.applications.insert(identity, application.clone());
                }
                result
            }
            .instrument(span.clone())
            .await;
            match &result {
                Ok(_) => {
                    span.record("outcome", "assembled");
                }
                Err(error) => {
                    span.record("outcome", "failed");
                    span.record("error_code", error.code().as_str());
                }
            }
            result
        })
    }

    async fn build_uncached(
        &mut self,
        reference: ApplicationReference,
        tier: usize,
    ) -> Result<Arc<BuiltApplication>, NativeAgentAssemblyError> {
        let loaded = self
            .platform
            .resolve_application_version(
                self.runtime_context,
                reference.identity.0,
                reference.identity.1,
            )
            .await
            .map_err(NativeAgentAssemblyError::from)?;
        let version = loaded.into_version_details();
        let profile = OrdinaryNoToolProfile::from_nested_version(&version, self.fallback_profile)?;
        let frozen = FrozenToolSnapshot::from_version_details(&version)
            .map_err(snapshot_error)?
            .apply_policy(self.policy.as_ref());
        let capabilities = configured_capabilities(&frozen);
        let (mut toolsets, sensitive_tools, delegated_authorization) =
            self.materialize_non_application_toolsets(&frozen).await?;
        let internal_tools = profile.internal_tools();
        toolsets.extend(internal_tools.toolsets());
        let has_non_application_tools = !toolsets.is_empty();
        let nested_references = application_references(&frozen, None)?;
        let parallel_applications = !nested_references.is_empty()
            && frozen
                .iter()
                .all(|reference| reference.kind() == FrozenToolKind::Application)
            && sensitive_tools.is_empty()
            && delegated_authorization.is_empty()
            && internal_tools.is_empty();
        let mut reserved_toolsets = BTreeSet::from([ASK_USER_TOOLSET_NAME.to_owned()]);
        let (nested_toolset, child_tools) = self
            .build_nested_toolset(nested_references, reference.identity, tier)
            .await?;
        if let Some(nested_toolset) = nested_toolset {
            reserved_toolsets.insert(nested_toolset.name().to_owned());
            toolsets.push(nested_toolset);
        }
        if has_non_application_tools && child_tools.has_guarded_descendant() {
            return Err(unsupported_capability());
        }
        let binding = bind_toolsets(toolsets, &reserved_toolsets, "elitea_nested_tool_binding")
            .await
            .map_err(tool_binding_error)?;
        let sensitive_tools = sensitive_tools.bind_provider_names(&binding)?;
        let delegated_authorization = delegated_authorization
            .bind_provider_names(&binding)
            .map_err(|()| invalid_configuration())?;
        let toolsets = binding.into_toolsets();
        let description = application_description(&reference, &capabilities);
        let model_name = profile.model_name().to_owned();
        let sensitive_tool_names = sensitive_tools
            .tool_names()
            .map(str::to_owned)
            .collect::<Vec<_>>();
        let agent = Arc::new(LazyNestedAgent {
            name: application_tool_name(reference.identity),
            description,
            profile,
            toolsets,
            model_facade: self.model_facade.clone(),
            elitea_context: self.elitea_context.clone(),
            sub_agents: Vec::new(),
            sensitive_tool_names,
            delegated_authorization: delegated_authorization.clone(),
            internal_tools,
            parallel_applications,
        });
        Ok(Arc::new(BuiltApplication {
            agent,
            model_name,
            child_tools,
            sensitive_tools,
            delegated_authorization,
            internal_tools,
        }))
    }

    async fn build_nested_toolset(
        &mut self,
        references: Vec<ApplicationReference>,
        parent_identity: ApplicationIdentity,
        tier: usize,
    ) -> Result<
        (Option<Arc<dyn Toolset>>, ApplicationToolPresentationCatalog),
        NativeAgentAssemblyError,
    > {
        if references.is_empty() {
            return Ok((None, ApplicationToolPresentationCatalog::default()));
        }
        let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(references.len());
        let mut presentations = ApplicationToolPresentationCatalog::default();
        for reference in references {
            let identity = reference.identity;
            let alias = reference.name.clone();
            let agent_type = reference.agent_type.clone();
            let application = self.build(reference, tier + 1).await?;
            let tool = Arc::new(ApplicationAgentTool::new(
                application.clone(),
                identity,
                self.event_sender.clone(),
                self.resume.clone(),
            ));
            presentations
                .insert_runtime(
                    tool.name().to_owned(),
                    alias,
                    agent_type,
                    application.model_name.clone(),
                    application.child_tools.clone(),
                    ApplicationToolGuardCatalogs::new(
                        application.sensitive_tools.clone(),
                        application.delegated_authorization.clone(),
                        application.internal_tools,
                    ),
                )
                .map_err(|_| invalid_configuration())?;
            tools.push(tool);
        }
        let name = format!("elitea_nested_{}_{}", parent_identity.0, parent_identity.1);
        Ok((
            Some(Arc::new(BasicToolset::new(name, tools))),
            presentations,
        ))
    }

    async fn materialize_non_application_toolsets(
        &self,
        frozen: &AdmittedToolSnapshot<'_>,
    ) -> Result<
        (
            Vec<Arc<dyn Toolset>>,
            SensitiveToolCatalog,
            DelegatedAuthorizationCatalog,
        ),
        NativeAgentAssemblyError,
    > {
        let (mut toolsets, mut delegated_authorization) =
            materialize_configured_toolsets_with_tokens_and_authorization(
                frozen,
                &self.policy,
                self.mcp_tokens,
            )
            .await
            .map_err(toolset_error)?;
        let mut sensitive_tools = sensitive_tools_for_kind(
            frozen,
            FrozenToolKind::Configured,
            &toolsets,
            self.policy.as_ref(),
        )
        .await?;
        let (mut mcp_toolsets, mcp_delegated_authorization) =
            materialize_mcp_toolsets_with_tokens_and_authorization(
                frozen,
                self.mcp_connector.as_ref(),
                &self.policy,
                self.mcp_tokens,
            )
            .await
            .map_err(|error| mcp_toolset_error(&error))?;
        delegated_authorization
            .merge(mcp_delegated_authorization)
            .map_err(|()| invalid_configuration())?;
        sensitive_tools.merge(
            sensitive_tools_for_kind(
                frozen,
                FrozenToolKind::Mcp,
                &mcp_toolsets,
                self.policy.as_ref(),
            )
            .await?,
        )?;
        toolsets.append(&mut mcp_toolsets);
        Ok((toolsets, sensitive_tools, delegated_authorization))
    }
}

#[derive(Clone)]
struct ApplicationReference {
    identity: ApplicationIdentity,
    name: String,
    description: Option<String>,
    agent_type: String,
    project_id: Option<u64>,
}

fn application_references(
    snapshot: &AdmittedToolSnapshot<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
) -> Result<Vec<ApplicationReference>, NativeAgentAssemblyError> {
    let mut seen = HashSet::new();
    let mut references = Vec::new();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Application)
        .filter(|reference| {
            selected_aliases.is_none_or(|aliases| aliases.contains(reference.toolkit_name()))
        })
    {
        let identity = reference
            .application_identity()
            .ok_or_else(invalid_configuration)?;
        if seen.insert(identity) {
            references.push(ApplicationReference {
                identity,
                name: reference.toolkit_name().to_owned(),
                description: reference.application_description().map(str::to_owned),
                agent_type: reference
                    .application_agent_type()
                    .ok_or_else(invalid_configuration)?
                    .to_owned(),
                project_id: reference.application_project_id(),
            });
        }
    }
    Ok(references)
}

fn configured_capabilities(snapshot: &AdmittedToolSnapshot<'_>) -> Vec<String> {
    let mut seen = HashSet::new();
    snapshot
        .iter()
        .filter(|reference| reference.kind() != FrozenToolKind::Application)
        .filter_map(|reference| {
            let label = reference.toolkit_name();
            if seen.len() >= MAX_DESCRIPTION_CAPABILITIES || !seen.insert(label.to_owned()) {
                None
            } else {
                Some(label.to_owned())
            }
        })
        .collect()
}

fn application_tool_name(identity: ApplicationIdentity) -> String {
    format!("elitea_agent_{}_v_{}", identity.0, identity.1)
}

fn application_description(reference: &ApplicationReference, capabilities: &[String]) -> String {
    let mut description = format!(
        "Delegate a self-contained task to the saved Elitea agent '{}'. Use this tool when that agent's purpose and configured capabilities match the work; include all context needed in task.",
        reference.name
    );
    if let Some(base) = reference
        .description
        .as_deref()
        .filter(|value| !value.is_empty())
    {
        push_bounded(&mut description, " Purpose: ", base);
    }
    if !capabilities.is_empty() {
        push_bounded(
            &mut description,
            " Configured capabilities: ",
            &capabilities.join(", "),
        );
    }
    push_bounded(
        &mut description,
        " ",
        "The child runs its own frozen instructions, model ownership, and toolsets and returns its final response.",
    );
    description
}

fn push_bounded(target: &mut String, separator: &str, value: &str) {
    let available = MAX_AGENT_DESCRIPTION_BYTES.saturating_sub(target.len());
    if available <= separator.len() {
        return;
    }
    target.push_str(separator);
    let remaining = MAX_AGENT_DESCRIPTION_BYTES.saturating_sub(target.len());
    if value.len() <= remaining {
        target.push_str(value);
        return;
    }
    let boundary = value
        .char_indices()
        .map(|(index, _)| index)
        .take_while(|index| *index <= remaining)
        .last()
        .unwrap_or(0);
    target.push_str(&value[..boundary]);
}

struct LazyNestedAgent {
    name: String,
    description: String,
    profile: OrdinaryNoToolProfile,
    toolsets: Vec<Arc<dyn Toolset>>,
    model_facade: Arc<ModelFacade>,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    sub_agents: Vec<Arc<dyn Agent>>,
    sensitive_tool_names: Vec<String>,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
    parallel_applications: bool,
}

#[async_trait]
impl Agent for LazyNestedAgent {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &self.sub_agents
    }

    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let agent = self.build_agent(
            self.bind_model()?,
            self.toolsets.clone(),
            self.delegated_authorization.clone(),
        )?;
        agent.run(ctx).await
    }
}

struct PreparedChildApplicationResume {
    agent: Arc<dyn Agent>,
    user_content: Content,
    run_config: RunConfig,
    children: Option<HashMap<String, ChildApplicationResume>>,
}

impl LazyNestedAgent {
    fn bind_model(&self) -> adk_rust::Result<Arc<dyn adk_rust::Llm>> {
        let invocation = ModelInvocation {
            model_name: self.profile.model_name().to_owned(),
            system_instruction: self.profile.instructions().to_owned(),
            max_tokens: self.profile.max_tokens(),
            reasoning_effort: self.profile.reasoning_effort().map(model_reasoning_effort),
            temperature: self.profile.temperature(),
            max_model_turns: self.profile.step_limit(),
        };
        let adapter = match self.profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
            OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
        };
        self.model_facade
            .bind(
                adapter,
                self.elitea_context.as_ref(),
                self.profile.model_project_id(),
                invocation,
            )
            .map(|model| model.provider_model())
            .map_err(model_error)
    }

    fn build_agent(
        &self,
        model: Arc<dyn adk_rust::Llm>,
        toolsets: Vec<Arc<dyn Toolset>>,
        mut authorization: DelegatedAuthorizationCatalog,
    ) -> adk_rust::Result<Arc<dyn Agent>> {
        let (model, toolsets) =
            crate::toolkits::bind_authorization_model_tools(model, toolsets, &mut authorization)?;
        let mut builder = LlmAgentBuilder::new(self.name.clone())
            .description(self.description.clone())
            .model(model)
            .generate_content_config(GenerateContentConfig {
                temperature: self.profile.temperature(),
                max_output_tokens: self
                    .profile
                    .max_tokens()
                    .and_then(|value| i32::try_from(value).ok()),
                ..GenerateContentConfig::default()
            })
            .max_iterations(self.profile.step_limit())
            .disallow_transfer_to_parent(true)
            .disallow_transfer_to_peers(true);
        for toolset in toolsets {
            builder = builder.toolset(toolset);
        }
        for tool_name in self
            .sensitive_tool_names
            .iter()
            .filter(|name| !authorization.is_declined(name))
        {
            builder = builder.require_tool_confirmation(tool_name);
        }
        for tool_name in authorization.tool_names() {
            builder = builder.require_tool_confirmation(tool_name);
        }
        if self.internal_tools.ask_user_enabled() {
            builder = builder.require_tool_confirmation(ASK_USER_TOOL_NAME);
        }
        if self.parallel_applications {
            builder = builder.tool_execution_strategy(ToolExecutionStrategy::Parallel);
        }
        let agent = builder
            .build()
            .map(|agent| Arc::new(agent) as Arc<dyn Agent>)
            .map_err(|_| agent_configuration_error())?;
        let agent = delegated_authorization_agent(agent, authorization);
        Ok(clarifying_question_agent(agent, self.internal_tools))
    }

    fn prepare_resume(
        &self,
        action: ChildApplicationResumeAction,
        sensitive_tools: &SensitiveToolCatalog,
    ) -> adk_rust::Result<PreparedChildApplicationResume> {
        match action {
            ChildApplicationResumeAction::Direct(decision) => {
                let mut authorization = self.delegated_authorization.clone();
                decision.restore_authorization_scope(&mut authorization);
                let replay = if decision.is_delegated_authorization() {
                    (*decision).into_delegated_authorization_replay(&mut authorization)
                } else if decision.is_clarifying_question() {
                    (*decision).into_clarifying_question_replay()
                } else {
                    (*decision).into_direct_replay(sensitive_tools)
                }
                .map_err(direct_hitl_execution_error)?;
                let prepared = replay.bind(self.bind_model()?);
                let (model, run_input, toolsets) = prepared.into_parts(self.toolsets.clone());
                let (user_content, run_config) = run_input.into_parts();
                Ok(PreparedChildApplicationResume {
                    agent: self.build_agent(model, toolsets, authorization)?,
                    user_content,
                    run_config,
                    children: None,
                })
            }
            ChildApplicationResumeAction::Nested(children) => {
                let calls =
                    application_replay_calls(&children).map_err(nested_resume_execution_error)?;
                let interrupt_ids =
                    resume_interrupt_ids(&children).map_err(nested_resume_execution_error)?;
                let user_content = nested_resume_user_content(&interrupt_ids);
                let model: Arc<dyn Llm> = Arc::new(ApplicationReplayModel {
                    delegate: self.bind_model()?,
                    state: AtomicU8::new(REPLAY_APPLICATIONS_PENDING),
                    calls,
                    replay_marker: user_content.clone(),
                });
                Ok(PreparedChildApplicationResume {
                    agent: self.build_agent(
                        model,
                        self.toolsets.clone(),
                        self.delegated_authorization.clone(),
                    )?,
                    user_content,
                    run_config: application_run_config(),
                    children: Some(children),
                })
            }
        }
    }
}

struct ApplicationAgentTool {
    application: Arc<BuiltApplication>,
    identity: ApplicationIdentity,
    event_sender: Option<ApplicationEventSender>,
    resume: Option<ApplicationResumeCoordinator>,
}

impl ApplicationAgentTool {
    fn new(
        application: Arc<BuiltApplication>,
        identity: ApplicationIdentity,
        event_sender: Option<ApplicationEventSender>,
        resume: Option<ApplicationResumeCoordinator>,
    ) -> Self {
        Self {
            application,
            identity,
            event_sender,
            resume,
        }
    }
}

#[async_trait]
impl Tool for ApplicationAgentTool {
    fn name(&self) -> &str {
        self.application.agent.name()
    }

    fn description(&self) -> &str {
        self.application.agent.description()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "task": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_APPLICATION_TASK_BYTES,
                    "description": "Required self-contained task for this saved agent. Include all context the child needs; maximum 240 KiB UTF-8."
                }
            },
            "required": ["task"],
            "additionalProperties": false
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {"response": {"type": "string"}},
            "required": ["response"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        ctx: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let span = tracing::info_span!(
            "agent.nested_application.invoke",
            application_id = self.identity.0,
            version_id = self.identity.1,
            invocation_id = %ctx.invocation_id(),
            function_call_id = %ctx.function_call_id(),
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
        );
        let result = self
            .invoke_child(ctx, arguments)
            .instrument(span.clone())
            .await;
        match &result {
            Ok(_) => {
                span.record("outcome", "succeeded");
            }
            Err(error) => {
                span.record("outcome", "failed");
                span.record("error_code", error.code);
            }
        }
        result
    }
}

impl ApplicationAgentTool {
    async fn invoke_child(
        &self,
        ctx: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let task = application_task(&arguments).map_err(|_| tool_input_error())?;
        let pipeline_container = pipeline_application_container(ctx.as_ref(), self.name())?;
        if pipeline_container {
            self.send_container_call(ctx.as_ref(), &arguments).await?;
        }
        let resume = match &self.resume {
            Some(coordinator) => {
                coordinator
                    .take(ctx.invocation_id(), ctx.function_call_id())
                    .await?
            }
            None => None,
        };
        let (agent, content, run_config, history, children) = match resume {
            Some(resume) if resume.tool_name == self.name() && resume.arguments == arguments => {
                let prepared = self
                    .application
                    .agent
                    .prepare_resume(resume.action, &self.application.sensitive_tools)?;
                (
                    prepared.agent,
                    prepared.user_content,
                    prepared.run_config,
                    resume.history,
                    prepared.children,
                )
            }
            Some(_) => return Err(tool_input_error()),
            None => (
                self.application.agent.clone() as Arc<dyn Agent>,
                Content::new("user").with_text(task),
                application_run_config(),
                Vec::new(),
                None,
            ),
        };
        let child_context = Arc::new(ApplicationToolInvocationContext::with_resume(
            ctx.clone(),
            Arc::clone(&agent),
            content,
            run_config,
            history,
        ));
        if let Some(children) = children {
            self.resume
                .as_ref()
                .ok_or_else(application_event_channel_error)?
                .install_children(child_context.invocation_id().to_owned(), children)
                .await?;
        }
        let result = self.drain_child(ctx.as_ref(), agent, child_context).await?;
        if pipeline_container && !is_nested_interrupt_result(&result) {
            self.send_container_result(ctx.as_ref(), result.clone())
                .await?;
        }
        Ok(result)
    }

    async fn drain_child(
        &self,
        ctx: &dyn ToolContext,
        agent: Arc<dyn Agent>,
        child_context: Arc<ApplicationToolInvocationContext>,
    ) -> adk_rust::Result<Value> {
        let child_branch = child_context.branch().to_owned();
        let mut stream = agent.run(child_context.clone()).await?;
        let mut final_response = None;
        let mut last_text = None;
        let mut application_batch = ApplicationCallBatch::default();
        while let Some(result) = stream.next().await {
            let mut event = match result {
                Ok(event) => event,
                Err(error) => {
                    self.send_fatal(ApplicationEventFailure::ChildExecution)
                        .await?;
                    return Err(error);
                }
            };
            if event.branch.is_empty() {
                event.branch.clone_from(&child_branch);
            } else if event.branch != child_branch {
                self.send_fatal(ApplicationEventFailure::ChildExecution)
                    .await?;
                return Err(application_event_channel_error());
            }
            application_batch.observe_calls(&event, &self.application.child_tools)?;
            if event.llm_response.interrupted || event.actions.tool_confirmation.is_some() {
                let interrupt_ids = confirmation_interrupt_ids(&event)?;
                self.send_event(ctx, event).await?;
                return Ok(nested_interrupt_result(&interrupt_ids));
            }
            if let Some(text) = event_text(&event) {
                last_text = Some(text.clone());
                if event.is_final_response() {
                    final_response = Some(text);
                }
            }
            match application_batch.observe_results(&mut event)? {
                ApplicationResultDisposition::Forward => {
                    self.send_event(ctx, event).await?;
                }
                ApplicationResultDisposition::Suppress => {}
                ApplicationResultDisposition::ForwardAndPause => {
                    self.send_event(ctx, event).await?;
                    return Ok(nested_interrupt_result(application_batch.interrupt_ids()));
                }
                ApplicationResultDisposition::SuppressAndPause => {
                    return Ok(nested_interrupt_result(application_batch.interrupt_ids()));
                }
            }
        }
        Ok(json!({
            "response": final_response
                .or(last_text)
                .unwrap_or_else(|| "No response from agent".to_owned())
        }))
    }

    async fn send_container_call(
        &self,
        ctx: &dyn ToolContext,
        arguments: &Value,
    ) -> adk_rust::Result<()> {
        let mut event = pipeline_application_event(ctx);
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: self.name().to_owned(),
                args: arguments.clone(),
                id: Some(ctx.function_call_id().to_owned()),
                thought_signature: None,
            }],
        });
        self.send_container_event(event).await
    }

    async fn send_container_result(
        &self,
        ctx: &dyn ToolContext,
        result: Value,
    ) -> adk_rust::Result<()> {
        let mut event = pipeline_application_event(ctx);
        event.llm_response.content = Some(Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: adk_rust::FunctionResponseData::new(self.name(), result),
                id: Some(ctx.function_call_id().to_owned()),
                annotations: None,
            }],
        });
        self.send_container_event(event).await
    }

    async fn send_container_event(&self, event: Event) -> adk_rust::Result<()> {
        let Some(sender) = &self.event_sender else {
            return Err(application_event_channel_error());
        };
        sender
            .send(ApplicationEventSignal::ContainerEvent(Box::new(event)))
            .await
            .map_err(|_| application_event_channel_error())
    }

    async fn send_event(&self, ctx: &dyn ToolContext, event: Event) -> adk_rust::Result<()> {
        let Some(sender) = &self.event_sender else {
            return Ok(());
        };
        let mut event = event;
        event.llm_request = None;
        event.provider_metadata.remove(ADK_LLM_REQUEST_METADATA_KEY);
        event
            .provider_metadata
            .remove(ADK_LLM_RESPONSE_METADATA_KEY);
        sender
            .send(ApplicationEventSignal::Event {
                container_invocation_id: ctx.invocation_id().to_owned(),
                parent_call_id: ctx.function_call_id().to_owned(),
                event: Box::new(event),
            })
            .await
            .map_err(|_| application_event_channel_error())
    }

    async fn send_fatal(&self, failure: ApplicationEventFailure) -> adk_rust::Result<()> {
        let Some(sender) = &self.event_sender else {
            return Ok(());
        };
        sender
            .send(ApplicationEventSignal::Fatal(failure))
            .await
            .map_err(|_| application_event_channel_error())
    }
}

fn event_text(event: &Event) -> Option<String> {
    let content = event.content()?;
    let mut text = String::new();
    for part in &content.parts {
        if let adk_rust::Part::Text { text: value } = part {
            text.push_str(value);
        }
    }
    (!text.is_empty()).then_some(text)
}

#[derive(Default)]
struct ApplicationCallBatch {
    pending: HashSet<String>,
    interrupted: bool,
    interrupt_ids: BTreeSet<String>,
}

enum ApplicationResultDisposition {
    Forward,
    Suppress,
    ForwardAndPause,
    SuppressAndPause,
}

impl ApplicationCallBatch {
    fn observe_calls(
        &mut self,
        event: &Event,
        applications: &ApplicationToolPresentationCatalog,
    ) -> adk_rust::Result<()> {
        let calls = event.tool_calls();
        let application_calls = calls
            .iter()
            .filter(|call| applications.contains_runtime_tool(call.name))
            .collect::<Vec<_>>();
        if application_calls.is_empty() {
            return Ok(());
        }
        if !self.pending.is_empty() || self.interrupted {
            return Err(application_event_channel_error());
        }
        for call in application_calls {
            let call_id = call
                .call_id
                .filter(|value| !value.is_empty())
                .ok_or_else(application_event_channel_error)?;
            if !self.pending.insert(call_id.to_owned()) {
                return Err(application_event_channel_error());
            }
        }
        Ok(())
    }

    fn observe_results(
        &mut self,
        event: &mut Event,
    ) -> adk_rust::Result<ApplicationResultDisposition> {
        let results = event
            .tool_results()
            .into_iter()
            .filter_map(|result| {
                result.call_id.map(|call_id| {
                    (
                        call_id.to_owned(),
                        nested_application_interrupt_ids(result.response),
                    )
                })
            })
            .collect::<Vec<_>>();
        let mut suppressed_ids = HashSet::new();
        for (call_id, interrupt_ids) in results {
            if !self.pending.remove(&call_id) {
                continue;
            }
            if let Some(interrupt_ids) = interrupt_ids {
                if !suppressed_ids.insert(call_id) {
                    return Err(application_event_channel_error());
                }
                for interrupt_id in interrupt_ids {
                    if !self.interrupt_ids.insert(interrupt_id) {
                        return Err(application_event_channel_error());
                    }
                }
                self.interrupted = true;
            }
        }
        if !suppressed_ids.is_empty() {
            let content = event
                .llm_response
                .content
                .as_mut()
                .ok_or_else(application_event_channel_error)?;
            content.parts.retain(|part| {
                !matches!(
                    part,
                    adk_rust::Part::FunctionResponse {
                        function_response,
                        id: Some(call_id),
                        ..
                    } if suppressed_ids.contains(call_id)
                        && is_nested_interrupt_result(&function_response.response)
                )
            });
        }
        let suppress = !suppressed_ids.is_empty()
            && event
                .llm_response
                .content
                .as_ref()
                .is_none_or(|content| content.parts.is_empty());
        let pause = self.interrupted && self.pending.is_empty();
        Ok(match (suppress, pause) {
            (false, false) => ApplicationResultDisposition::Forward,
            (true, false) => ApplicationResultDisposition::Suppress,
            (false, true) => ApplicationResultDisposition::ForwardAndPause,
            (true, true) => ApplicationResultDisposition::SuppressAndPause,
        })
    }

    fn interrupt_ids(&self) -> &BTreeSet<String> {
        &self.interrupt_ids
    }
}

fn confirmation_interrupt_ids(event: &Event) -> adk_rust::Result<BTreeSet<String>> {
    let request = event
        .actions
        .tool_confirmation
        .as_ref()
        .ok_or_else(application_event_channel_error)?;
    let call_id = request
        .function_call_id
        .as_deref()
        .ok_or_else(application_event_channel_error)?;
    let (interrupt_id, _) = sensitive_call_identity(
        &event.invocation_id,
        call_id,
        &request.tool_name,
        &request.args,
    )
    .map_err(|_| application_event_channel_error())?;
    Ok(BTreeSet::from([interrupt_id]))
}

fn nested_interrupt_result(interrupt_ids: &BTreeSet<String>) -> Value {
    json!({NESTED_INTERRUPT_RESULT_KEY: interrupt_ids})
}

pub(crate) fn nested_application_interrupt_ids(value: &Value) -> Option<BTreeSet<String>> {
    let object = value.as_object().filter(|object| object.len() == 1)?;
    let values = object.get(NESTED_INTERRUPT_RESULT_KEY)?.as_array()?;
    if values.is_empty() || values.len() > 16 {
        return None;
    }
    let mut ids = BTreeSet::new();
    for value in values {
        let identity = value.as_str().filter(|identity| {
            !identity.is_empty() && identity.len() <= 512 && !identity.chars().any(char::is_control)
        })?;
        if !ids.insert(identity.to_owned()) {
            return None;
        }
    }
    Some(ids)
}

fn is_nested_interrupt_result(value: &Value) -> bool {
    nested_application_interrupt_ids(value).is_some()
}

pub(crate) struct ApplicationEventStreamingAgent {
    inner: Arc<dyn Agent>,
    events: ApplicationEventReceiver,
    applications: ApplicationToolPresentationCatalog,
}

impl ApplicationEventStreamingAgent {
    #[must_use]
    pub(crate) fn new(
        inner: Arc<dyn Agent>,
        events: ApplicationEventReceiver,
        applications: ApplicationToolPresentationCatalog,
    ) -> Self {
        Self {
            inner,
            events,
            applications,
        }
    }
}

#[async_trait]
impl Agent for ApplicationEventStreamingAgent {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        self.inner.sub_agents()
    }

    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let mut child_events = self
            .events
            .inner
            .lock()
            .await
            .take()
            .ok_or_else(application_event_channel_error)?;
        let root_ctx = Arc::new(ApplicationRootInvocationContext {
            inner: ctx,
            branch: APPLICATION_BRANCH_ROOT.to_owned(),
        });
        let mut root_events = self.inner.run(root_ctx).await?;
        let applications = self.applications.clone();
        let stream = async_stream::stream! {
            let mut application_batch = ApplicationCallBatch::default();
            loop {
                tokio::select! {
                    biased;
                    signal = child_events.recv() => {
                        if let Some(signal) = signal {
                            match application_signal_event(signal) {
                                Ok(event) => yield Ok(event),
                                Err(error) => {
                                    yield Err(error);
                                    return;
                                }
                            }
                        }
                    }
                    event = root_events.next() => {
                        if let Some(event) = event {
                            while let Ok(signal) = child_events.try_recv() {
                                match application_signal_event(signal) {
                                    Ok(event) => yield Ok(event),
                                    Err(error) => {
                                        yield Err(error);
                                        return;
                                    }
                                }
                            }
                            let mut event = match event {
                                Ok(event) => event,
                                Err(error) => {
                                    yield Err(error);
                                    return;
                                }
                            };
                            if event.branch.is_empty() {
                                APPLICATION_BRANCH_ROOT.clone_into(&mut event.branch);
                            } else if event.branch != APPLICATION_BRANCH_ROOT {
                                yield Err(application_event_channel_error());
                                return;
                            }
                            if let Err(error) = application_batch.observe_calls(&event, &applications) {
                                yield Err(error);
                                return;
                            }
                            let disposition = match application_batch.observe_results(&mut event) {
                                Ok(disposition) => disposition,
                                Err(error) => {
                                    yield Err(error);
                                    return;
                                }
                            };
                            match disposition {
                                ApplicationResultDisposition::Forward => yield Ok(event),
                                ApplicationResultDisposition::Suppress => {}
                                ApplicationResultDisposition::ForwardAndPause => {
                                    yield Ok(event);
                                    return;
                                }
                                ApplicationResultDisposition::SuppressAndPause => return,
                            }
                        } else {
                            while let Ok(signal) = child_events.try_recv() {
                                match application_signal_event(signal) {
                                    Ok(event) => yield Ok(event),
                                    Err(error) => {
                                        yield Err(error);
                                        return;
                                    }
                                }
                            }
                            return;
                        }
                    }
                }
            }
        };
        Ok(Box::pin(stream))
    }
}

struct ApplicationRootInvocationContext {
    inner: Arc<dyn InvocationContext>,
    branch: String,
}

#[async_trait]
impl ReadonlyContext for ApplicationRootInvocationContext {
    fn invocation_id(&self) -> &str {
        self.inner.invocation_id()
    }

    fn agent_name(&self) -> &str {
        self.inner.agent_name()
    }

    fn user_id(&self) -> &str {
        self.inner.user_id()
    }

    fn app_name(&self) -> &str {
        self.inner.app_name()
    }

    fn session_id(&self) -> &str {
        self.inner.session_id()
    }

    fn branch(&self) -> &str {
        &self.branch
    }

    fn user_content(&self) -> &Content {
        self.inner.user_content()
    }
}

#[async_trait]
impl CallbackContext for ApplicationRootInvocationContext {
    fn artifacts(&self) -> Option<Arc<dyn Artifacts>> {
        self.inner.artifacts()
    }

    fn tool_outcome(&self) -> Option<adk_rust::ToolOutcome> {
        self.inner.tool_outcome()
    }

    fn tool_name(&self) -> Option<&str> {
        self.inner.tool_name()
    }

    fn tool_input(&self) -> Option<&Value> {
        self.inner.tool_input()
    }

    fn shared_state(&self) -> Option<Arc<adk_rust::SharedState>> {
        self.inner.shared_state()
    }
}

#[async_trait]
impl InvocationContext for ApplicationRootInvocationContext {
    fn agent(&self) -> Arc<dyn Agent> {
        self.inner.agent()
    }

    fn memory(&self) -> Option<Arc<dyn Memory>> {
        self.inner.memory()
    }

    fn session(&self) -> &dyn Session {
        self.inner.session()
    }

    fn run_config(&self) -> &RunConfig {
        self.inner.run_config()
    }

    fn end_invocation(&self) {
        self.inner.end_invocation();
    }

    fn ended(&self) -> bool {
        self.inner.ended()
    }

    fn is_cancelled(&self) -> bool {
        self.inner.is_cancelled()
    }

    fn user_scopes(&self) -> Vec<String> {
        self.inner.user_scopes()
    }

    fn request_metadata(&self) -> HashMap<String, Value> {
        self.inner.request_metadata()
    }

    async fn get_secret(&self, name: &str) -> adk_rust::Result<Option<String>> {
        self.inner.get_secret(name).await
    }

    async fn get_secret_for(&self, request: &SecretRequest) -> adk_rust::Result<Option<String>> {
        self.inner.get_secret_for(request).await
    }
}

fn application_signal_event(signal: ApplicationEventSignal) -> adk_rust::Result<Event> {
    match signal {
        ApplicationEventSignal::ContainerEvent(event) => {
            let event = *event;
            if event
                .provider_metadata
                .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
                || event
                    .provider_metadata
                    .contains_key(DESCENDANT_PARENT_CALL_KEY)
            {
                return Err(application_event_channel_error());
            }
            Ok(event)
        }
        ApplicationEventSignal::Event {
            container_invocation_id,
            parent_call_id,
            event,
        } => {
            let mut event = *event;
            if event
                .provider_metadata
                .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
                || event
                    .provider_metadata
                    .contains_key(DESCENDANT_PARENT_CALL_KEY)
            {
                return Err(application_event_channel_error());
            }
            event.provider_metadata.insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                container_invocation_id,
            );
            event
                .provider_metadata
                .insert(DESCENDANT_PARENT_CALL_KEY.to_owned(), parent_call_id);
            Ok(event)
        }
        ApplicationEventSignal::Fatal(ApplicationEventFailure::ChildExecution) => {
            Err(child_execution_error())
        }
    }
}

fn pipeline_application_container(
    ctx: &dyn ToolContext,
    tool_name: &str,
) -> adk_rust::Result<bool> {
    let actions = ctx.actions();
    let Some(marker) = actions
        .state_delta
        .get(PIPELINE_APPLICATION_NODE_METADATA_KEY)
    else {
        return Ok(false);
    };
    let object = marker
        .as_object()
        .filter(|object| object.len() == 2)
        .ok_or_else(application_event_channel_error)?;
    if object.get("call_id").and_then(Value::as_str) != Some(ctx.function_call_id())
        || object.get("tool_name").and_then(Value::as_str) != Some(tool_name)
    {
        return Err(application_event_channel_error());
    }
    Ok(true)
}

fn pipeline_application_event(ctx: &dyn ToolContext) -> Event {
    let mut event = Event::new(ctx.invocation_id());
    ctx.agent_name().clone_into(&mut event.author);
    APPLICATION_BRANCH_ROOT.clone_into(&mut event.branch);
    event.llm_response.finish_reason = Some(FinishReason::Stop);
    event.llm_response.turn_complete = true;
    event
}

struct ApplicationToolInvocationContext {
    parent_ctx: Arc<dyn ToolContext>,
    agent: Arc<dyn Agent>,
    user_content: Content,
    invocation_id: String,
    branch: String,
    run_config: RunConfig,
    ended: AtomicBool,
    session: ApplicationToolSession,
}

fn application_run_config() -> RunConfig {
    RunConfig::builder()
        .streaming_mode(StreamingMode::None)
        .tool_concurrency(ToolConcurrencyConfig {
            max_concurrency: Some(MAX_PARALLEL_APPLICATION_CALLS),
            ..ToolConcurrencyConfig::default()
        })
        .build()
}

impl ApplicationToolInvocationContext {
    fn new(parent_ctx: Arc<dyn ToolContext>, agent: Arc<dyn Agent>, user_content: Content) -> Self {
        Self::with_resume(
            parent_ctx,
            agent,
            user_content,
            application_run_config(),
            Vec::new(),
        )
    }

    fn with_resume(
        parent_ctx: Arc<dyn ToolContext>,
        agent: Arc<dyn Agent>,
        user_content: Content,
        mut run_config: RunConfig,
        mut history: Vec<Content>,
    ) -> Self {
        static NEXT_INVOCATION: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        // Match Runner's current-input history boundary. LlmAgent replaces the last
        // user entry with user_content; on resume that entry must be the private
        // replay marker, not the original child task which the model still needs.
        history.push(user_content.clone());
        // The tool returns one complete child result. A direct replay supplies an
        // SSE config for root callers; retain its decisions but use the same
        // accumulated-event contract as a fresh child, not its final token delta.
        run_config.streaming_mode = StreamingMode::None;
        let ordinal = NEXT_INVOCATION.fetch_add(1, Ordering::Relaxed);
        let invocation_id = format!("elitea-child-{ordinal}");
        let parent_branch = if parent_ctx.branch().is_empty() {
            APPLICATION_BRANCH_ROOT
        } else {
            parent_ctx.branch()
        };
        let branch = format!("{parent_branch}.application_{ordinal}");
        Self {
            session: ApplicationToolSession::new(
                invocation_id.clone(),
                parent_ctx.app_name().to_owned(),
                parent_ctx.user_id().to_owned(),
                history,
            ),
            parent_ctx,
            agent,
            user_content,
            invocation_id,
            branch,
            run_config,
            ended: AtomicBool::new(false),
        }
    }
}

#[async_trait]
impl ReadonlyContext for ApplicationToolInvocationContext {
    fn invocation_id(&self) -> &str {
        &self.invocation_id
    }

    fn agent_name(&self) -> &str {
        self.agent.name()
    }

    fn user_id(&self) -> &str {
        self.parent_ctx.user_id()
    }

    fn app_name(&self) -> &str {
        self.parent_ctx.app_name()
    }

    fn session_id(&self) -> &str {
        self.session.id()
    }

    fn branch(&self) -> &str {
        &self.branch
    }

    fn user_content(&self) -> &Content {
        &self.user_content
    }
}

#[async_trait]
impl CallbackContext for ApplicationToolInvocationContext {
    fn artifacts(&self) -> Option<Arc<dyn Artifacts>> {
        None
    }

    fn shared_state(&self) -> Option<Arc<adk_rust::SharedState>> {
        self.parent_ctx.shared_state()
    }
}

#[async_trait]
impl InvocationContext for ApplicationToolInvocationContext {
    fn agent(&self) -> Arc<dyn Agent> {
        self.agent.clone()
    }

    fn memory(&self) -> Option<Arc<dyn Memory>> {
        None
    }

    fn session(&self) -> &dyn Session {
        &self.session
    }

    fn run_config(&self) -> &RunConfig {
        &self.run_config
    }

    fn end_invocation(&self) {
        self.ended.store(true, Ordering::SeqCst);
    }

    fn ended(&self) -> bool {
        self.ended.load(Ordering::SeqCst)
    }

    fn user_scopes(&self) -> Vec<String> {
        self.parent_ctx.user_scopes()
    }

    async fn get_secret(&self, name: &str) -> adk_rust::Result<Option<String>> {
        self.parent_ctx.get_secret(name).await
    }

    async fn get_secret_for(&self, request: &SecretRequest) -> adk_rust::Result<Option<String>> {
        match request.purpose.as_deref() {
            Some(purpose) => {
                self.parent_ctx
                    .get_secret_for_purpose(&request.name, purpose)
                    .await
            }
            None => self.parent_ctx.get_secret(&request.name).await,
        }
    }
}

struct ApplicationToolSession {
    id: String,
    app_name: String,
    user_id: String,
    state: std::sync::RwLock<HashMap<String, Value>>,
    history: Vec<Content>,
}

impl ApplicationToolSession {
    fn new(id: String, app_name: String, user_id: String, history: Vec<Content>) -> Self {
        Self {
            id,
            app_name,
            user_id,
            state: std::sync::RwLock::new(HashMap::new()),
            history,
        }
    }
}

impl Session for ApplicationToolSession {
    fn id(&self) -> &str {
        &self.id
    }

    fn app_name(&self) -> &str {
        &self.app_name
    }

    fn user_id(&self) -> &str {
        &self.user_id
    }

    fn state(&self) -> &dyn State {
        self
    }

    fn conversation_history(&self) -> Vec<Content> {
        self.history.clone()
    }
}

impl State for ApplicationToolSession {
    fn get(&self, key: &str) -> Option<Value> {
        self.state.read().ok()?.get(key).cloned()
    }

    fn set(&mut self, key: String, value: Value) {
        if adk_rust::validate_state_key(&key).is_err() {
            return;
        }
        if let Ok(mut state) = self.state.write() {
            state.insert(key, value);
        }
    }

    fn all(&self) -> HashMap<String, Value> {
        self.state
            .read()
            .map(|state| state.clone())
            .unwrap_or_default()
    }
}

const fn model_reasoning_effort(effort: ReasoningEffort) -> ModelReasoningEffort {
    match effort {
        ReasoningEffort::Low => ModelReasoningEffort::Low,
        ReasoningEffort::Medium => ModelReasoningEffort::Medium,
        ReasoningEffort::High => ModelReasoningEffort::High,
        ReasoningEffort::None => ModelReasoningEffort::None,
    }
}

fn model_error(error: ModelFacadeError) -> AdkError {
    let category = match error {
        ModelFacadeError::InvalidConfiguration | ModelFacadeError::InvalidInvocation => {
            ErrorCategory::InvalidInput
        }
        ModelFacadeError::ResourceExhausted => ErrorCategory::InvalidInput,
        ModelFacadeError::DependencyUnavailable => ErrorCategory::Unavailable,
    };
    AdkError::new(
        ErrorComponent::Model,
        category,
        "elitea_nested_agent.model_unavailable",
        "the nested agent model could not be bound",
    )
}

fn agent_configuration_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::InvalidInput,
        "elitea_nested_agent.invalid_configuration",
        "the nested agent configuration is invalid",
    )
}

fn tool_input_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "elitea_nested_agent.invalid_task",
        "the nested agent task is invalid",
    )
}

fn application_event_channel_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::Internal,
        "elitea_nested_agent.event_channel_unavailable",
        "the nested agent event channel is unavailable",
    )
}

fn child_execution_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::Unavailable,
        "elitea_nested_agent.execution_failed",
        "the nested agent execution failed",
    )
}

fn direct_hitl_execution_error(_error: super::direct_hitl::DirectHitlError) -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::InvalidInput,
        "elitea_nested_agent.invalid_resume",
        "the nested agent continuation is invalid",
    )
}

fn nested_resume_execution_error(_error: NativeAgentAssemblyError) -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::InvalidInput,
        "elitea_nested_agent.invalid_recursive_resume",
        "the recursive nested agent continuation is invalid",
    )
}

fn snapshot_error(error: crate::toolkits::FrozenToolSnapshotError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        FrozenToolSnapshotErrorCode::InvalidInput => NativeAgentAssemblyErrorCode::InvalidInput,
        FrozenToolSnapshotErrorCode::ResourceExhausted => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
    };
    NativeAgentAssemblyError::new(code, "the nested application tool snapshot is malformed")
}

fn toolset_error(error: crate::toolkits::ToolsetMaterializationError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        ToolsetMaterializationErrorCode::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ToolsetMaterializationErrorCode::UnsupportedToolkit => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        ToolsetMaterializationErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
        ToolsetMaterializationErrorCode::ResourceExhausted => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
    };
    NativeAgentAssemblyError::new(code, "the nested application toolsets are unavailable")
}

fn mcp_toolset_error(error: &crate::toolkits::McpMaterializationError) -> NativeAgentAssemblyError {
    let code = match error.code() {
        McpMaterializationErrorCode::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        McpMaterializationErrorCode::UnsupportedAuthority => {
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        }
        McpMaterializationErrorCode::AuthorizationRequired => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        McpMaterializationErrorCode::ResourceExhausted => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        McpMaterializationErrorCode::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the nested application MCP toolsets are unavailable")
}

fn tool_binding_error(error: ToolBindingError) -> NativeAgentAssemblyError {
    let code = match error {
        ToolBindingError::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ToolBindingError::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
        ToolBindingError::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(
        code,
        "the nested model-callable toolkit namespace is invalid",
    )
}

fn invalid_configuration() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the nested application graph is invalid",
    )
}

fn unsupported_capability() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the nested application kind is not yet available",
    )
}

fn resource_exhausted() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the nested application graph exceeds its approved limit",
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn application_call_event(
        id: &str,
        invocation_id: &str,
        branch: &str,
        call_id: &str,
        tool_name: &str,
    ) -> Event {
        let mut event = Event::with_id(id, invocation_id);
        event.branch = branch.to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: tool_name.to_owned(),
                args: json!({"task": "Resolve the delegated name"}),
                id: Some(call_id.to_owned()),
                thought_signature: None,
            }],
        });
        event
    }

    fn two_tier_application_events(parent_call_id: &str) -> Vec<Event> {
        let root = application_call_event(
            "root-call-event",
            "root-invocation",
            APPLICATION_BRANCH_ROOT,
            "call-orchestrator",
            "elitea_agent_31_v_41",
        );
        let mut child = application_call_event(
            "child-call-event",
            "orchestrator-invocation",
            &format!("{APPLICATION_BRANCH_ROOT}.application_1"),
            "call-resolver",
            "elitea_agent_32_v_42",
        );
        child.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            "root-invocation".to_owned(),
        );
        child.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            parent_call_id.to_owned(),
        );
        vec![root, child]
    }

    #[test]
    fn recursive_application_route_requires_an_exact_parent_chain() {
        let valid = application_call_chain_from(
            &two_tier_application_events("call-orchestrator"),
            "resolver-invocation",
            "orchestrator-invocation",
            "call-resolver",
            &format!("{APPLICATION_BRANCH_ROOT}.application_1.application_2"),
        )
        .expect("exact two-tier application chain");
        assert_eq!(valid.len(), 2);
        assert_eq!(valid[0].call_id, "call-resolver");
        assert_eq!(valid[1].call_id, "call-orchestrator");

        let Err(error) = application_call_chain_from(
            &two_tier_application_events("missing-root-call"),
            "resolver-invocation",
            "orchestrator-invocation",
            "call-resolver",
            &format!("{APPLICATION_BRANCH_ROOT}.application_1.application_2"),
        ) else {
            panic!("broken parent identity must fail closed");
        };
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        );

        let mut broken_branch = two_tier_application_events("call-orchestrator");
        broken_branch[1].branch = format!("{APPLICATION_BRANCH_ROOT}.application_9");
        let Err(error) = application_call_chain_from(
            &broken_branch,
            "resolver-invocation",
            "orchestrator-invocation",
            "call-resolver",
            &format!("{APPLICATION_BRANCH_ROOT}.application_1.application_2"),
        ) else {
            panic!("a mismatched branch ancestry must fail closed");
        };
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        );
    }

    #[test]
    fn recursive_application_route_rejects_a_fourth_agent_tier() {
        let Err(error) = application_call_chain_from(
            &two_tier_application_events("call-orchestrator"),
            "resolver-invocation",
            "orchestrator-invocation",
            "call-resolver",
            &format!("{APPLICATION_BRANCH_ROOT}.application_1.application_2.application_3"),
        ) else {
            panic!("a fourth agent tier must fail closed");
        };
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        );
    }
}
