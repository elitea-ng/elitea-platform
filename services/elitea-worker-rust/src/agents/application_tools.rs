//! Claim-bound nested Elitea applications exposed through ADK `AgentTool`.
//!
//! Main admits exact application/version identities. During the one authorized
//! assembly phase this module resolves each identity once, revalidates the
//! bounded nesting graph, compiles a fresh direct `LlmAgent`, and presents it to
//! the parent model as a source-compatible `task` tool. Stored pipelines remain
//! owned by the graph compiler and fail closed here.

#![allow(dead_code)] // Production registration remains capability-gated.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
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
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use tokio::sync::{Mutex, mpsc};
use tracing::Instrument as _;

use super::application_pipeline::{
    ApplicationPipelineTool, MAX_PIPELINE_TOOL_PENDING_BYTES, PipelineApplicationBoundary,
    PipelineToolParentBinding, PipelineToolResume, pipeline_application_boundary,
    pipeline_application_pending_event, pipeline_boundary_original_call, pipeline_child_references,
    pipeline_pause_identity, pipeline_tool_description, rebind_pipeline_tool_boundary,
    retained_pipeline_application_events, validate_retained_pipeline_application_pause,
};
use super::assembly::{OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort};
use super::context_management::ContextManagementPlan;
use super::direct_hitl::{ResolvedDirectHitlDecision, sensitive_call_identity};
use super::events::{
    APPLICATION_BRANCH_ROOT, ApplicationToolGuardCatalogs, ApplicationToolPresentationCatalog,
    DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY,
};
use super::graph::pipeline_node_event_channel;
use super::internal_tools::{ASK_USER_TOOL_NAME, ASK_USER_TOOLSET_NAME, InternalToolCatalog};
use super::model_scope::{ModelScopeSessions, ScopedModelCheckpoint};
use super::pipeline::materialize_saved_pipeline_tool;
mod materialization;
#[cfg(test)]
mod materialization_tests;
mod static_resume;
use super::application_pipeline::{PipelineStaticToolResume, static_pipeline_tool_pause};
use super::graph::static_tool_pause::StaticToolDecision;
use super::pipeline::scoped_applications::PipelineApplicationScopeRoute;
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
    BoundModelFacade, ModelAdapterKind, ModelFacade, ModelFacadeError, ModelInvocation,
    ModelReasoningEffort,
};
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::ClaimScopedEliteaContext;
use crate::transport::runtime_context::SavedAgentFingerprint;
pub(super) use materialization::ApplicationMaterializationPath;
pub(crate) use static_resume::{
    install_static_application_resume, prepare_static_application_resume,
};

const MAX_APPLICATION_HOPS: usize = 25;
/// The one child `agent_type` this worker compiles as a nested `LlmAgent`.
const SUPPORTED_APPLICATION_AGENT_TYPE: &str = "agent";
/// The child `agent_type` compiled as a checkpointed graph tool (#973).
pub(crate) const PIPELINE_APPLICATION_AGENT_TYPE: &str = "pipeline";
/// Upper bound on the children one notice names, so a version with a hundred
/// unsupported references cannot write a hundred lines into the transcript.
const MAX_SKIPPED_APPLICATION_CHILDREN: usize = 8;
const MAX_SKIPPED_APPLICATION_LABEL_CHARS: usize = 96;
const MAX_AGENT_TIERS: usize = 3;
pub(super) const MAX_APPLICATION_TASK_BYTES: usize = 240 * 1_024;
const MAX_AGENT_DESCRIPTION_BYTES: usize = 4 * 1_024;
const MAX_DESCRIPTION_CAPABILITIES: usize = 16;
const APPLICATION_EVENT_CHANNEL_CAPACITY: usize = 64;
const MAX_PARALLEL_APPLICATION_CALLS: usize = 8;
const MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS: usize = 16;
const ADK_LLM_REQUEST_METADATA_KEY: &str = "gcp.vertex.agent.llm_request";
const ADK_LLM_RESPONSE_METADATA_KEY: &str = "gcp.vertex.agent.llm_response";
const NESTED_INTERRUPT_RESULT_KEY: &str = "__elitea_nested_interrupt_v1";
const APPLICATION_REPLAY_BATCH_KEY: &str = "elitea.application.replay_batch.v1";
const APPLICATION_RETAINED_PAUSE_KEY: &str = "elitea.application.retained_pause.v1";
const MAX_RETAINED_PAUSE_BYTES: usize = 512 * 1_024;
const APPLICATION_RETAINED_PIPELINE_KEY: &str = "elitea.application.retained_pipeline.v1";
const MAX_RETAINED_PIPELINE_EVENTS: usize = 512;

#[derive(Clone, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
struct ApplicationReplayBatch {
    event_id: String,
    interrupt_ids: BTreeSet<String>,
    call_ordinals: BTreeMap<String, usize>,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RetainedApplicationPause {
    interrupt_id: String,
    parent_call_id: String,
    events: Vec<Event>,
}
/// Minimal original projection data for an untouched saved graph family.
/// The original parent model event is proof data only; it is never reprojected.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct RetainedPipelinePause {
    schema: String,
    batch_event_id: String,
    ordinal: usize,
    parent_call_id: String,
    tool_name: String,
    arguments_digest: String,
    checkpoint_thread_id: String,
    #[serde(deserialize_with = "deserialize_unique_interrupt_ids")]
    interrupt_ids: BTreeSet<String>,
    original_call: Event,
    events: Vec<Event>,
}

fn deserialize_unique_interrupt_ids<'de, D>(deserializer: D) -> Result<BTreeSet<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let values = Vec::<String>::deserialize(deserializer)?;
    if values.is_empty() || values.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS {
        return Err(serde::de::Error::custom(
            "invalid retained pipeline interrupt count",
        ));
    }
    let count = values.len();
    let unique: BTreeSet<_> = values.into_iter().collect();
    if unique.len() != count {
        return Err(serde::de::Error::custom(
            "duplicate retained pipeline interrupts",
        ));
    }
    Ok(unique)
}

fn application_arguments_digest(arguments: &Value) -> Result<String, NativeAgentAssemblyError> {
    let encoded = serde_json::to_string(arguments).map_err(|_| invalid_configuration())?;
    Ok(super::instruction_authority::content_digest(&encoded))
}

impl RetainedPipelinePause {
    fn validate_basic(&self) -> Result<(), NativeAgentAssemblyError> {
        if self.schema != APPLICATION_RETAINED_PIPELINE_KEY
            || self.ordinal == 0
            || self.ordinal > MAX_PARALLEL_APPLICATION_CALLS
            || self.interrupt_ids.is_empty()
            || self.interrupt_ids.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS
            || self.events.is_empty()
            // The original call is one additional proof event within the total bound.
            || self.events.len() >= MAX_RETAINED_PIPELINE_EVENTS
            || [
                &self.batch_event_id,
                &self.parent_call_id,
                &self.tool_name,
                &self.checkpoint_thread_id,
            ]
            .into_iter()
            .any(|value| !valid_retained_pipeline_identity(value))
            || self
                .interrupt_ids
                .iter()
                .any(|value| !valid_retained_pipeline_identity(value))
            || self.original_call.id != self.batch_event_id
            || original_application_batch_id(&self.original_call)? != self.batch_event_id
            || serde_json::to_vec(self)
                .map_err(|_| invalid_configuration())?
                .len()
                > MAX_PIPELINE_TOOL_PENDING_BYTES
        {
            return Err(invalid_configuration());
        }
        let calls = self.original_call.tool_calls();
        let mut matching = calls
            .iter()
            .enumerate()
            .filter(|(_, call)| call.call_id == Some(self.parent_call_id.as_str()));
        let (index, call) = matching.next().ok_or_else(invalid_configuration)?;
        if matching.next().is_some()
            || index + 1 != self.ordinal
            || call.name != self.tool_name
            || application_arguments_digest(call.args)? != self.arguments_digest
        {
            return Err(invalid_configuration());
        }
        let mut ids = HashSet::new();
        for event in &self.events {
            if event.id == self.original_call.id
                || !ids.insert(&event.id)
                || !event.tool_results().is_empty()
                || event.actions.tool_confirmation_decision.is_some()
                || event
                    .provider_metadata
                    .contains_key(APPLICATION_RETAINED_PAUSE_KEY)
                || event
                    .provider_metadata
                    .contains_key(APPLICATION_RETAINED_PIPELINE_KEY)
            {
                return Err(invalid_configuration());
            }
        }
        Ok(())
    }

    fn pending_event(&self) -> Result<&Event, NativeAgentAssemblyError> {
        let mut pending = None;
        for event in &self.events {
            if pipeline_application_pending_event(event)? && pending.replace(event).is_some() {
                return Err(invalid_configuration());
            }
        }
        pending.ok_or_else(invalid_configuration)
    }

    fn validate(&self) -> Result<(), NativeAgentAssemblyError> {
        self.validate_basic()?;
        let pending = self.pending_event()?;
        if pending
            .provider_metadata
            .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
            != Some(&self.checkpoint_thread_id)
        {
            return Err(invalid_configuration());
        }
        validate_retained_pipeline_application_pause(
            &self.original_call,
            pending,
            &self.events,
            &self.interrupt_ids,
        )
    }
}

fn valid_retained_pipeline_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 1024 && !value.chars().any(char::is_control)
}

pub(crate) const PIPELINE_APPLICATION_NODE_METADATA_KEY: &str =
    "elitea.pipeline.application_node.v1";

type ApplicationIdentity = (u64, u64);
type ApplicationFuture<'a> = Pin<
    Box<dyn Future<Output = Result<Arc<BuiltApplication>, NativeAgentAssemblyError>> + Send + 'a>,
>;

pub(super) type ApplicationEventSender = mpsc::Sender<ApplicationEventSignal>;

pub(super) enum ApplicationEventSignal {
    ContainerEvent(Box<Event>),
    GraphDescendant {
        root_container_invocation_id: String,
        root_parent_call_id: String,
        root_checkpoint_thread_id: String,
        catalog: Arc<super::pipeline::composition::PipelineCheckpointCatalog>,
        lineage: Option<super::application_pipeline::PipelineToolCallLineage>,
        event: Box<Event>,
    },
    Event {
        container_invocation_id: String,
        parent_call_id: String,
        /// Set only for a saved PIPELINE child (#973): the thread its own graph
        /// pauses on, which the projector re-derives rather than trusts.
        checkpoint_thread_id: Option<String>,
        event: Box<Event>,
    },
    Fatal(ApplicationEventFailure),
}

#[derive(Clone, Copy)]
pub(super) enum ApplicationEventFailure {
    ChildExecution,
    Model(&'static str),
    OutputContinuation(super::model_checkpoint::output::ContinuationFailure),
}

#[derive(Clone)]
pub(crate) struct ApplicationEventReceiver {
    inner: Arc<Mutex<Option<mpsc::Receiver<ApplicationEventSignal>>>>,
}

impl ApplicationEventReceiver {
    pub(super) async fn take(&self) -> adk_rust::Result<mpsc::Receiver<ApplicationEventSignal>> {
        self.inner
            .lock()
            .await
            .take()
            .ok_or_else(application_event_channel_error)
    }

    pub(super) async fn restore(
        &self,
        receiver: mpsc::Receiver<ApplicationEventSignal>,
    ) -> adk_rust::Result<()> {
        let mut slot = self.inner.lock().await;
        if slot.is_some() {
            return Err(application_event_channel_error());
        }
        *slot = Some(receiver);
        Ok(())
    }
}

#[derive(Clone, Default)]
pub(crate) struct ApplicationResumeCoordinator {
    inner: Arc<Mutex<ApplicationResumeState>>,
}

#[derive(Default)]
struct ApplicationResumeState {
    root: Option<HashMap<String, ChildApplicationResume>>,
    call_lineage: HashMap<(String, String), super::application_pipeline::PipelineToolCallLineage>,
    by_parent_invocation: HashMap<String, HashMap<String, ChildApplicationResume>>,
}

pub(super) struct ChildApplicationResume {
    batch_event_id: String,
    tool_name: String,
    arguments: Value,
    ordinal: usize,
    history: Vec<Content>,
    action: ChildApplicationResumeAction,
}

impl ChildApplicationResume {
    pub(super) async fn retained_result(
        &self,
        ctx: &dyn ToolContext,
        tool_name: &str,
        arguments: &Value,
        sender: Option<&ApplicationEventSender>,
    ) -> adk_rust::Result<Option<Value>> {
        if self.tool_name != tool_name || &self.arguments != arguments {
            return Err(tool_input_error());
        }
        let (metadata_key, encoded, interrupt_ids) = match &self.action {
            ChildApplicationResumeAction::Retained(pause) => {
                if pause.parent_call_id != ctx.function_call_id() {
                    return Err(application_event_channel_error());
                }
                let encoded =
                    serde_json::to_string(pause).map_err(|_| application_event_channel_error())?;
                if encoded.len() > MAX_RETAINED_PAUSE_BYTES {
                    return Err(application_event_channel_error());
                }
                (
                    APPLICATION_RETAINED_PAUSE_KEY,
                    encoded,
                    BTreeSet::from([pause.interrupt_id.clone()]),
                )
            }
            ChildApplicationResumeAction::RetainedPipeline(pause) => {
                pause
                    .validate()
                    .map_err(|_| application_event_channel_error())?;
                if pause.batch_event_id != self.batch_event_id
                    || pause.ordinal != self.ordinal
                    || pause.parent_call_id != ctx.function_call_id()
                    || pause.tool_name != tool_name
                    || pause.arguments_digest
                        != application_arguments_digest(arguments)
                            .map_err(|_| tool_input_error())?
                {
                    return Err(tool_input_error());
                }
                let encoded =
                    serde_json::to_string(pause).map_err(|_| application_event_channel_error())?;
                (
                    APPLICATION_RETAINED_PIPELINE_KEY,
                    encoded,
                    pause.interrupt_ids.clone(),
                )
            }
            _ => return Ok(None),
        };
        let mut event = Event::new(ctx.invocation_id());
        ctx.agent_name().clone_into(&mut event.author);
        ctx.branch().clone_into(&mut event.branch);
        event
            .provider_metadata
            .insert(metadata_key.to_owned(), encoded);
        sender
            .ok_or_else(application_event_channel_error)?
            .send(ApplicationEventSignal::ContainerEvent(Box::new(event)))
            .await
            .map_err(|_| application_event_channel_error())?;
        Ok(Some(nested_interrupt_result(&interrupt_ids)))
    }

    pub(super) fn into_pipeline_with_static(
        self,
        tool_name: &str,
        arguments: &Value,
        definition: &super::graph::compiler::PipelineDefinition,
        runtimes: &super::graph::compiler::PipelineNodeRuntimes,
        thread: &str,
    ) -> adk_rust::Result<PreparedPipelineToolResume> {
        if self.tool_name != tool_name || &self.arguments != arguments {
            return Err(tool_input_error());
        }
        match self.action {
            ChildApplicationResumeAction::PipelineStatic(request) => {
                let checkpoint = request
                    .resolve(definition, runtimes, thread)
                    .map_err(|_| tool_input_error())?;
                Ok(PreparedPipelineToolResume {
                    checkpoint: Box::new(checkpoint),
                    scopes: Vec::new(),
                    static_scopes: Vec::new(),
                })
            }
            action => ChildApplicationResume { action, ..self }.into_pipeline(tool_name, arguments),
        }
    }

    /// Take the pipeline continuation this resume carries, proving first that
    /// it names the same tool and the same arguments the replay re-emitted.
    pub(super) fn into_pipeline(
        self,
        tool_name: &str,
        arguments: &Value,
    ) -> adk_rust::Result<PreparedPipelineToolResume> {
        if self.tool_name != tool_name || &self.arguments != arguments {
            return Err(tool_input_error());
        }
        match self.action {
            ChildApplicationResumeAction::Pipeline(checkpoint) => Ok(PreparedPipelineToolResume {
                checkpoint,
                scopes: Vec::new(),
                static_scopes: Vec::new(),
            }),
            ChildApplicationResumeAction::PipelineDescendants(resume) => {
                pipeline_resume_scope_ids(&resume).map_err(|_| tool_input_error())?;
                Ok(*resume)
            }
            _ => Err(tool_input_error()),
        }
    }
}

/// Resume the saved graph family before installing its exact ordinary child scopes.
/// The graph itself has no ordinary model history or model replay action.
pub(super) struct PreparedPipelineToolResume {
    pub(super) checkpoint: Box<PipelineToolResume>,
    pub(super) scopes: Vec<PipelineApplicationScopeDecisions>,
    pub(super) static_scopes: Vec<PipelineApplicationScopeStaticDecisions>,
}

pub(super) struct PipelineApplicationScopeStaticDecisions {
    pub(super) route: PipelineApplicationScopeRoute,
    pub(super) events: Vec<Event>,
    pub(super) decisions: Vec<StaticToolDecision>,
}

/// Selection is still subject to the rebuilt registry and exact ancestor checkpoints.
/// This bundle carries identities; it does not grant execution before that proof.
pub(super) struct PipelineApplicationScopeDecisions {
    pub(super) route: PipelineApplicationScopeRoute,
    pub(super) events: Vec<Event>,
    pub(super) decisions: Vec<ResolvedDirectHitlDecision>,
}

pub(super) enum ChildApplicationResumeAction {
    Retained(Box<RetainedApplicationPause>),
    RetainedPipeline(Box<RetainedPipelinePause>),
    Direct(Box<ResolvedDirectHitlDecision>),
    Nested(HashMap<String, ChildApplicationResume>),
    /// #973: a saved PIPELINE child re-enters its own graph at the node that
    /// paused, from the checkpoint its pause persisted on the parent's event.
    Pipeline(Box<PipelineToolResume>),
    PipelineStatic(Box<PipelineStaticToolResume>),
    /// An ordinary child pause inside a saved graph crosses a checkpoint boundary.
    PipelineDescendants(Box<PreparedPipelineToolResume>),
}

impl ApplicationResumeCoordinator {
    pub(super) async fn observe_call_lineage(
        &self,
        event: &Event,
    ) -> Result<(), NativeAgentAssemblyError> {
        let mut state = self.inner.lock().await;
        for (index, call) in event.tool_calls().iter().enumerate() {
            let id = call.call_id.ok_or_else(invalid_configuration)?;
            let value =
                super::application_pipeline::PipelineToolCallLineage::from_call(event, index)?;
            let key = (event.invocation_id.clone(), id.to_owned());
            if let Some(recorded) = state.call_lineage.get(&key) {
                if recorded != &value {
                    return Err(invalid_configuration());
                }
            } else {
                if state.call_lineage.len() >= 1024 {
                    return Err(resource_exhausted());
                }
                state.call_lineage.insert(key, value);
            }
        }
        Ok(())
    }
    pub(super) async fn pipeline_call_lineage(
        &self,
        invocation: &str,
        call: &str,
        name: &str,
        args: &Value,
    ) -> Result<super::application_pipeline::PipelineToolCallLineage, NativeAgentAssemblyError>
    {
        let value = self
            .inner
            .lock()
            .await
            .call_lineage
            .get(&(invocation.to_owned(), call.to_owned()))
            .cloned()
            .ok_or_else(invalid_configuration)?;
        if !value.matches(name, args)? {
            return Err(invalid_configuration());
        }
        Ok(value)
    }

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

    /// Probe the same exact route as `take` without consuming its installed resume.
    pub(super) async fn has_resume(&self, parent_invocation_id: &str, call_id: &str) -> bool {
        let stored = self.inner.lock().await;
        if let Some(calls) = stored.by_parent_invocation.get(parent_invocation_id) {
            return calls.contains_key(call_id);
        }
        stored
            .root
            .as_ref()
            .is_some_and(|calls| calls.contains_key(call_id))
    }

    pub(super) async fn take(
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
    batch_event_id: String,
    tool_name: String,
    arguments: Value,
    ordinal: usize,
    owned_invocation_id: String,
    history: Vec<Content>,
    decision: Option<ResolvedDirectHitlDecision>,
    children: HashMap<String, ChildApplicationResumeBuilder>,
    /// #973: set for a child PIPELINE, which has no history to replay and one
    /// checkpoint to re-enter instead.
    pipeline: Option<Box<PipelineToolResume>>,
    pipeline_descendants: Option<Box<PreparedPipelineToolResume>>,
    pipeline_static: Option<Box<PipelineStaticToolResume>>,
    pipeline_boundary_event_id: Option<String>,
    retained: Option<Box<RetainedApplicationPause>>,
    retained_pipeline: Option<Box<RetainedPipelinePause>>,
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
    let batch = application_replay_batch(&child_resumes, &submitted_interrupt_ids)?;
    coordinator.install_root(child_resumes).await?;
    let user_content = nested_resume_user_content(&submitted_interrupt_ids);
    Ok(PreparedNestedApplicationResume {
        model: Arc::new(ApplicationReplayModel {
            delegate,
            state: AtomicU8::new(REPLAY_APPLICATIONS_PENDING),
            calls,
            batch,
            previous_markers: nested_resume_markers(events)?,
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
        if decision.is_pipeline_node() {
            let root_event_id_of_call = insert_pipeline_resume(
                &mut builders,
                events,
                &mut submitted_interrupt_ids,
                decision,
            )?;
            bind_application_resume_batch(&mut root_event_id, &root_event_id_of_call)?;
            continue;
        }
        if let Some(boundary) = scoped_pipeline_boundary_for_decision(events, &decision)? {
            let chain = pipeline_boundary_call_chain(events, &boundary)?;
            let root_call = chain.last().ok_or_else(invalid_configuration)?;
            bind_application_resume_batch(&mut root_event_id, &root_call.event_id)?;
            if !submitted_interrupt_ids.insert(decision.interrupt_id().to_owned()) {
                return Err(unsupported_capability());
            }
            insert_pipeline_descendant_decision(
                &mut builders,
                &chain,
                events,
                applications,
                boundary,
                decision,
            )?;
            continue;
        }
        let chain = application_call_chain(events, &decision)?;
        let root_call = chain.last().ok_or_else(invalid_configuration)?;
        bind_application_resume_batch(&mut root_event_id, &root_call.event_id)?;
        if !submitted_interrupt_ids.insert(decision.interrupt_id().to_owned()) {
            return Err(unsupported_capability());
        }
        insert_resume_decision(&mut builders, &chain, events, applications, decision)?;
    }
    let root_event_id = root_event_id.ok_or_else(invalid_configuration)?;
    retain_pending_nested_decisions(
        events,
        &root_event_id,
        &submitted_interrupt_ids,
        &mut builders,
        applications,
    )?;
    let child_resumes = finish_resume_builders(builders)?;
    Ok((child_resumes, submitted_interrupt_ids))
}

fn bind_application_resume_batch(
    root_event_id: &mut Option<String>,
    candidate: &str,
) -> Result<(), NativeAgentAssemblyError> {
    if root_event_id
        .get_or_insert_with(|| candidate.to_owned())
        .as_str()
        != candidate
    {
        return Err(unsupported_capability());
    }
    Ok(())
}

/// Scope and checkpoint authority comes from the graph boundary helper.
fn scoped_pipeline_boundary_for_decision(
    events: &[Event],
    decision: &ResolvedDirectHitlDecision,
) -> Result<Option<PipelineApplicationBoundary>, NativeAgentAssemblyError> {
    let mut candidates = events.iter().filter(|event| {
        event.invocation_id == decision.invocation_id()
            && event
                .actions
                .tool_confirmation
                .as_ref()
                .is_some_and(|request| {
                    request.function_call_id.as_deref() == Some(decision.call_id())
                        && request.tool_name == decision.tool_name()
                        && &request.args == decision.arguments()
                })
    });
    let event = candidates.next().ok_or_else(invalid_configuration)?;
    if candidates.next().is_some() {
        return Err(invalid_configuration());
    }
    let boundary = pipeline_application_boundary(events, event)?;
    if let Some(boundary) = &boundary
        && (!boundary
            .scope_events
            .iter()
            .any(|original| original.id == event.id)
            || !boundary.scope_events.iter().any(|original| {
                original.tool_calls().iter().any(|call| {
                    call.call_id == Some(boundary.scope_route.application_call_id.as_str())
                })
            }))
    {
        return Err(invalid_configuration());
    }
    Ok(boundary)
}

/// Walk only the ordinary model calls above the saved graph, never its node calls.
fn pipeline_boundary_call_chain(
    events: &[Event],
    boundary: &PipelineApplicationBoundary,
) -> Result<Vec<ApplicationCallHop>, NativeAgentAssemblyError> {
    if boundary.checkpoint.thread_id() != boundary.checkpoint_thread_id {
        return Err(invalid_configuration());
    }
    let chain = application_call_chain_from(
        events,
        &boundary.pending_event.invocation_id,
        &boundary.container_invocation_id,
        &boundary.parent_call_id,
        &boundary.pending_event.branch,
    )?;
    let pipeline = chain.first().ok_or_else(invalid_configuration)?;
    if pipeline.tool_name != boundary.pending_event.author {
        return Err(invalid_configuration());
    }
    application_task(&pipeline.arguments)?;
    Ok(chain)
}

fn pipeline_boundary_builder<'a>(
    builders: &'a mut HashMap<String, ChildApplicationResumeBuilder>,
    hop: &ApplicationCallHop,
    history: Vec<Content>,
) -> Result<&'a mut ChildApplicationResumeBuilder, NativeAgentAssemblyError> {
    let builder =
        builders
            .entry(hop.call_id.clone())
            .or_insert_with(|| ChildApplicationResumeBuilder {
                batch_event_id: hop.event_id.clone(),
                tool_name: hop.tool_name.clone(),
                arguments: hop.arguments.clone(),
                ordinal: hop.ordinal,
                owned_invocation_id: hop.owned_invocation_id.clone(),
                history,
                decision: None,
                children: HashMap::new(),
                pipeline: None,
                pipeline_descendants: None,
                pipeline_static: None,
                pipeline_boundary_event_id: None,
                retained: None,
                retained_pipeline: None,
            });
    if builder.batch_event_id != hop.event_id
        || builder.tool_name != hop.tool_name
        || builder.arguments != hop.arguments
        || builder.ordinal != hop.ordinal
        || builder.owned_invocation_id != hop.owned_invocation_id
    {
        return Err(invalid_configuration());
    }
    Ok(builder)
}

fn insert_pipeline_descendant_decision(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    chain: &[ApplicationCallHop],
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    boundary: PipelineApplicationBoundary,
    decision: ResolvedDirectHitlDecision,
) -> Result<(), NativeAgentAssemblyError> {
    let mut current = builders;
    let mut catalog = applications;
    for (index, hop) in chain.iter().rev().enumerate() {
        let child_catalog = catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        if completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id) {
            return Err(invalid_configuration());
        }
        let is_pipeline = index + 1 == chain.len();
        let history = if is_pipeline {
            Vec::new()
        } else {
            let task = application_task_with_variables(&hop.arguments)?;
            let mut history = vec![Content::new("user").with_text(task)];
            history.extend(application_resume_history(
                events,
                &hop.owned_invocation_id,
            )?);
            history
        };
        let builder = pipeline_boundary_builder(current, hop, history)?;
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        if is_pipeline {
            if !builder.children.is_empty() {
                return Err(unsupported_capability());
            }
            return append_pipeline_scope_decision(builder, boundary, decision);
        }
        if builder.pipeline_descendants.is_some() {
            return Err(unsupported_capability());
        }
        current = &mut builder.children;
        catalog = child_catalog;
    }
    Err(invalid_configuration())
}

fn append_pipeline_scope_decision(
    builder: &mut ChildApplicationResumeBuilder,
    boundary: PipelineApplicationBoundary,
    decision: ResolvedDirectHitlDecision,
) -> Result<(), NativeAgentAssemblyError> {
    if boundary.scope_events.is_empty()
        || boundary.scope_events.len() > 512
        || serde_json::to_vec(&boundary.scope_events)
            .map_err(|_| invalid_configuration())?
            .len()
            > MAX_RETAINED_PAUSE_BYTES
        || builder
            .pipeline_boundary_event_id
            .as_ref()
            .is_some_and(|id| id != &boundary.pending_event.id)
    {
        return Err(invalid_configuration());
    }
    builder.pipeline_boundary_event_id = Some(boundary.pending_event.id);
    let resume = builder.pipeline_descendants.get_or_insert_with(|| {
        Box::new(PreparedPipelineToolResume {
            checkpoint: boundary.checkpoint,
            scopes: Vec::new(),
            static_scopes: Vec::new(),
        })
    });
    if resume.checkpoint.thread_id() != boundary.checkpoint_thread_id {
        return Err(invalid_configuration());
    }
    if let Some(scope) = resume
        .scopes
        .iter_mut()
        .find(|scope| scope.route == boundary.scope_route)
    {
        if scope
            .events
            .iter()
            .map(|event| &event.id)
            .ne(boundary.scope_events.iter().map(|event| &event.id))
        {
            return Err(invalid_configuration());
        }
        scope.decisions.push(decision);
    } else {
        resume.scopes.push(PipelineApplicationScopeDecisions {
            route: boundary.scope_route,
            events: boundary.scope_events,
            decisions: vec![decision],
        });
    }
    pipeline_descendant_interrupt_ids(&resume.scopes)?;
    Ok(())
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
        let ordinal = application_replay_ordinal(event, &parent_call_id)?.unwrap_or(ordinal);
        chain.push(ApplicationCallHop {
            event_id: original_application_batch_id(event)?,
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

/// One fresh nested-application branch beneath `parent_branch`.
///
/// The ordinal is per process and display-free — its only job is to make the
/// child's branch a STRICT descendant of the parent's, which is what hides the
/// child's events from the parent's own next turn (ADK
/// `event_belongs_to_branch`). Shared by the agent-child and pipeline-child
/// contexts so the two cannot drift about what a child branch looks like, and
/// so `valid_application_branch` keeps admitting both.
fn nested_application_branch(parent_branch: &str) -> String {
    static NEXT_BRANCH: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
    let ordinal = NEXT_BRANCH.fetch_add(1, Ordering::Relaxed);
    let parent_branch = if parent_branch.is_empty() {
        APPLICATION_BRANCH_ROOT
    } else {
        parent_branch
    };
    format!("{parent_branch}.application_{ordinal}")
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
        let task = application_task_with_variables(&hop.arguments)?;
        if completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id) {
            return Err(invalid_configuration());
        }
        let mut history = vec![Content::new("user").with_text(task)];
        history.extend(application_resume_history(
            events,
            &hop.owned_invocation_id,
        )?);
        let builder = current_builders
            .entry(hop.call_id.clone())
            .or_insert_with(|| ChildApplicationResumeBuilder {
                batch_event_id: hop.event_id.clone(),
                tool_name: hop.tool_name.clone(),
                arguments: hop.arguments.clone(),
                ordinal: hop.ordinal,
                owned_invocation_id: hop.owned_invocation_id.clone(),
                history: history.clone(),
                decision: None,
                children: HashMap::new(),
                pipeline: None,
                pipeline_descendants: None,
                pipeline_static: None,
                pipeline_boundary_event_id: None,
                retained: None,
                retained_pipeline: None,
            });
        if builder.batch_event_id != hop.event_id
            || builder.tool_name != hop.tool_name
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
                || builder.pipeline.is_some()
                || builder.pipeline_descendants.is_some()
                || builder.pipeline_static.is_some()
                || builder.retained.is_some()
                || builder.retained_pipeline.is_some()
                || builder.decision.replace(decision).is_some()
                || !builder.children.is_empty()
            {
                return Err(unsupported_capability());
            }
            return Ok(());
        }
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.pipeline_descendants.is_some()
            || builder.pipeline_static.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        current_builders = &mut builder.children;
        current_catalog = child_catalog;
    }
    Err(invalid_configuration())
}

/// Build the single-hop resume of one paused child PIPELINE (#973).
///
/// Depth is deliberately one. A pipeline child of a nested AGENT would need the
/// branch-walked chain above, and it carries no branch precisely because its
/// pause is a graph interrupt projected as the root of its own descendant
/// projector — so the two contracts are kept apart rather than blended.
/// Returns the id of the event carrying the parent's tool call, which is what
/// scopes the completeness check.
fn insert_pipeline_resume(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    events: &[Event],
    submitted_interrupt_ids: &mut HashSet<String>,
    decision: ResolvedDirectHitlDecision,
) -> Result<String, NativeAgentAssemblyError> {
    let route = decision
        .application_route()
        .ok_or_else(invalid_configuration)?
        .clone();
    // ONE nesting level, checked on the branch the child's own events carry.
    // A pipeline child of a nested AGENT would need the branch-walked chain
    // `application_call_chain` performs, so the two contracts stay apart.
    if application_branch_depth(route.branch()) != Some(1) {
        return Err(unsupported_capability());
    }
    if !submitted_interrupt_ids.insert(decision.interrupt_id().to_owned()) {
        return Err(unsupported_capability());
    }
    let (event, call, ordinal) = exact_application_call(
        events,
        route.container_invocation_id(),
        route.parent_call_id(),
    )?;
    if event
        .provider_metadata
        .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
        || event
            .provider_metadata
            .contains_key(DESCENDANT_PARENT_CALL_KEY)
    {
        return Err(unsupported_capability());
    }
    let root_event_id = original_application_batch_id(event)?;
    let ordinal = application_replay_ordinal(event, route.parent_call_id())?.unwrap_or(ordinal);
    if completed_application_calls(events, &root_event_id)?.contains_key(route.parent_call_id()) {
        return Err(invalid_configuration());
    }
    let tool_name = call.name.to_owned();
    let arguments = call.args.clone();
    let pipeline = decision
        .into_pipeline_resume()
        .ok_or_else(invalid_configuration)?;
    let builder = builders
        .entry(route.parent_call_id().to_owned())
        .or_insert_with(|| ChildApplicationResumeBuilder {
            batch_event_id: root_event_id.clone(),
            tool_name,
            arguments,
            ordinal,
            owned_invocation_id: String::new(),
            history: Vec::new(),
            decision: None,
            children: HashMap::new(),
            pipeline: None,
            pipeline_descendants: None,
            pipeline_static: None,
            pipeline_boundary_event_id: None,
            retained: None,
            retained_pipeline: None,
        });
    if builder.decision.is_some()
        || !builder.children.is_empty()
        || builder.pipeline_descendants.is_some()
        || builder.pipeline_static.is_some()
        || builder.retained.is_some()
        || builder.retained_pipeline.is_some()
        || builder.pipeline.replace(pipeline).is_some()
    {
        return Err(unsupported_capability());
    }
    Ok(root_event_id)
}

pub(super) fn application_task(arguments: &Value) -> Result<&str, NativeAgentAssemblyError> {
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

fn application_task_with_variables(arguments: &Value) -> Result<&str, NativeAgentAssemblyError> {
    let object = arguments.as_object().ok_or_else(invalid_configuration)?;
    if object.len() > 257
        || serde_json::to_vec(arguments)
            .map_err(|_| invalid_configuration())?
            .len()
            > MAX_APPLICATION_TASK_BYTES
    {
        return Err(invalid_configuration());
    }
    object
        .get("task")
        .and_then(Value::as_str)
        .filter(|task| !task.is_empty() && !task.contains('\0'))
        .ok_or_else(invalid_configuration)
}

fn finish_resume_builders(
    builders: HashMap<String, ChildApplicationResumeBuilder>,
) -> Result<HashMap<String, ChildApplicationResume>, NativeAgentAssemblyError> {
    builders
        .into_iter()
        .map(|(call_id, builder)| {
            let action = match (
                builder.decision,
                builder.pipeline,
                builder.children.is_empty(),
                builder.retained,
                builder.pipeline_descendants,
                builder.retained_pipeline,
                builder.pipeline_static,
            ) {
                (Some(decision), None, true, None, None, None, None) => {
                    ChildApplicationResumeAction::Direct(Box::new(decision))
                }
                (None, Some(pipeline), true, None, None, None, None) => {
                    ChildApplicationResumeAction::Pipeline(pipeline)
                }
                (None, None, false, None, None, None, None) => {
                    ChildApplicationResumeAction::Nested(finish_resume_builders(builder.children)?)
                }
                (None, None, true, Some(pause), None, None, None) => {
                    ChildApplicationResumeAction::Retained(pause)
                }
                (None, None, true, None, Some(resume), None, None) => {
                    pipeline_resume_scope_ids(&resume)?;
                    ChildApplicationResumeAction::PipelineDescendants(resume)
                }
                (None, None, true, None, None, Some(pause), None) => {
                    pause.validate()?;
                    ChildApplicationResumeAction::RetainedPipeline(pause)
                }
                (None, None, true, None, None, None, Some(request)) => {
                    ChildApplicationResumeAction::PipelineStatic(request)
                }
                _ => return Err(unsupported_capability()),
            };
            Ok((
                call_id,
                ChildApplicationResume {
                    batch_event_id: builder.batch_event_id,
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

fn pipeline_descendant_interrupt_ids(
    scopes: &[PipelineApplicationScopeDecisions],
) -> Result<BTreeSet<String>, NativeAgentAssemblyError> {
    if scopes.is_empty() || scopes.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS {
        return Err(invalid_configuration());
    }
    let mut ids = BTreeSet::new();
    for scope in scopes {
        if scope.decisions.is_empty()
            || scope.decisions.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS
        {
            return Err(invalid_configuration());
        }
        for decision in &scope.decisions {
            if !ids.insert(decision.interrupt_id().to_owned()) {
                return Err(invalid_configuration());
            }
            if ids.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS {
                return Err(resource_exhausted());
            }
        }
    }
    Ok(ids)
}

fn pipeline_resume_scope_ids(
    resume: &PreparedPipelineToolResume,
) -> Result<BTreeSet<String>, NativeAgentAssemblyError> {
    if resume.static_scopes.is_empty() {
        return pipeline_descendant_interrupt_ids(&resume.scopes);
    }
    if !resume.scopes.is_empty()
        || resume.static_scopes.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS
    {
        return Err(invalid_configuration());
    }
    let mut ids = BTreeSet::new();
    for scope in &resume.static_scopes {
        if scope.decisions.is_empty() {
            return Err(invalid_configuration());
        }
        for decision in &scope.decisions {
            if !ids.insert(decision.pause_id().to_owned())
                || ids.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS
            {
                return Err(invalid_configuration());
            }
        }
    }
    Ok(ids)
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
                ChildApplicationResumeAction::Retained(_)
                | ChildApplicationResumeAction::RetainedPipeline(_) => {}
                ChildApplicationResumeAction::Direct(decision) => {
                    if !interrupt_ids.insert(decision.interrupt_id().to_owned()) {
                        return Err(invalid_configuration());
                    }
                }
                ChildApplicationResumeAction::Nested(children) => {
                    collect(children, interrupt_ids)?;
                }
                ChildApplicationResumeAction::PipelineStatic(resume) => {
                    if !interrupt_ids.insert(resume.pause_id().to_owned()) {
                        return Err(invalid_configuration());
                    }
                }
                ChildApplicationResumeAction::Pipeline(resume) => {
                    if !interrupt_ids.insert(resume.interrupt_id().to_owned()) {
                        return Err(invalid_configuration());
                    }
                }
                ChildApplicationResumeAction::PipelineDescendants(resume) => {
                    for interrupt_id in pipeline_resume_scope_ids(resume)? {
                        if !interrupt_ids.insert(interrupt_id) {
                            return Err(invalid_configuration());
                        }
                    }
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

fn replay_batch(event: &Event) -> Result<Option<ApplicationReplayBatch>, NativeAgentAssemblyError> {
    let live = event
        .llm_response
        .provider_metadata
        .as_ref()
        .and_then(|metadata| metadata.get(APPLICATION_REPLAY_BATCH_KEY));
    let encoded = event
        .provider_metadata
        .get(ADK_LLM_RESPONSE_METADATA_KEY)
        .map(|encoded| {
            serde_json::from_str::<LlmResponse>(encoded).map_err(|_| invalid_configuration())
        })
        .transpose()?;
    let serialized = encoded
        .as_ref()
        .and_then(|response| response.provider_metadata.as_ref())
        .and_then(|metadata| metadata.get(APPLICATION_REPLAY_BATCH_KEY));
    let decode = |raw: Option<&Value>| {
        raw.map(|raw| {
            serde_json::from_value::<ApplicationReplayBatch>(raw.clone())
                .map_err(|_| invalid_configuration())
        })
        .transpose()
    };
    let live = decode(live)?;
    let serialized = decode(serialized)?;
    let batch = match (live, serialized) {
        (Some(live), Some(serialized)) if live != serialized => return Err(invalid_configuration()),
        (Some(value), _) | (_, Some(value)) => value,
        (None, None) => return Ok(None),
    };
    if batch.event_id.is_empty()
        || batch.event_id.len() > 512
        || batch.interrupt_ids.is_empty()
        || batch.interrupt_ids.len() > 16
    {
        return Err(invalid_configuration());
    }
    if batch.call_ordinals.is_empty()
        || batch.call_ordinals.len() > MAX_PARALLEL_APPLICATION_CALLS
        || batch
            .call_ordinals
            .values()
            .any(|ordinal| *ordinal == 0 || *ordinal > 16)
        || batch
            .call_ordinals
            .values()
            .copied()
            .collect::<HashSet<_>>()
            .len()
            != batch.call_ordinals.len()
    {
        return Err(invalid_configuration());
    }
    Ok(Some(batch))
}

pub(super) fn original_application_batch_id(
    event: &Event,
) -> Result<String, NativeAgentAssemblyError> {
    Ok(replay_batch(event)?.map_or_else(|| event.id.clone(), |batch| batch.event_id))
}

pub(super) fn application_replay_ordinal(
    event: &Event,
    call_id: &str,
) -> Result<Option<usize>, NativeAgentAssemblyError> {
    let Some(batch) = replay_batch(event)? else {
        return Ok(None);
    };
    batch
        .call_ordinals
        .get(call_id)
        .copied()
        .map(Some)
        .ok_or_else(invalid_configuration)
}

fn application_replay_batch(
    resumes: &HashMap<String, ChildApplicationResume>,
    interrupt_ids: &HashSet<String>,
) -> Result<ApplicationReplayBatch, NativeAgentAssemblyError> {
    let event_id = resumes
        .values()
        .next()
        .ok_or_else(invalid_configuration)?
        .batch_event_id
        .clone();
    if resumes
        .values()
        .any(|resume| resume.batch_event_id != event_id)
    {
        return Err(unsupported_capability());
    }
    Ok(ApplicationReplayBatch {
        event_id,
        interrupt_ids: interrupt_ids.iter().cloned().collect(),
        call_ordinals: resumes
            .iter()
            .map(|(id, resume)| (id.clone(), resume.ordinal))
            .collect(),
    })
}

/// Recover results only from the original invocation or an exact, typed replay of its batch.
fn completed_application_calls(
    events: &[Event],
    batch_event_id: &str,
) -> Result<HashMap<String, Value>, NativeAgentAssemblyError> {
    let original = events
        .iter()
        .find(|event| event.id == batch_event_id)
        .ok_or_else(invalid_configuration)?;
    let calls = original.tool_calls();
    let mut invocations = HashSet::from([original.invocation_id.clone()]);
    for event in events {
        if replay_batch(event)?.is_some_and(|batch| batch.event_id == batch_event_id) {
            if event.tool_calls().iter().any(|replayed| {
                !calls.iter().any(|call| {
                    replayed.call_id == call.call_id
                        && replayed.name == call.name
                        && replayed.args == call.args
                })
            }) {
                return Err(invalid_configuration());
            }
            invocations.insert(event.invocation_id.clone());
        }
    }
    let mut results = HashMap::new();
    for event in events
        .iter()
        .filter(|event| invocations.contains(&event.invocation_id))
    {
        for result in event.tool_results() {
            let Some(call_id) = result.call_id else {
                continue;
            };
            if calls
                .iter()
                .any(|call| call.call_id == Some(call_id) && call.name == result.name)
                && (is_nested_interrupt_result(result.response)
                    || results
                        .insert(call_id.to_owned(), result.response.clone())
                        .is_some())
            {
                return Err(invalid_configuration());
            }
        }
    }
    Ok(results)
}

fn application_resume_history(
    events: &[Event],
    owned_invocation_id: &str,
) -> Result<Vec<Content>, NativeAgentAssemblyError> {
    // Direct child resumes get a new invocation ID. Join their histories only
    // through the same original parent call, never through a shared tool name.
    let mut child_routes = HashMap::new();
    for event in events {
        if !event
            .provider_metadata
            .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
        {
            continue;
        }
        let (container, parent) =
            persisted_parent_route(event)?.ok_or_else(invalid_configuration)?;
        let (call_event, _, _) = exact_application_call(events, &container, &parent)?;
        let route = (original_application_batch_id(call_event)?, parent);
        if let Some(previous) = child_routes.insert(event.invocation_id.clone(), route.clone())
            && previous != route
        {
            return Err(ambiguous_child_invocation());
        }
    }
    let mut invocations = HashSet::from([owned_invocation_id.to_owned()]);
    for _ in 0..MAX_APPLICATION_HOPS {
        let mut changed = false;
        let routes: HashSet<_> = invocations
            .iter()
            .filter_map(|invocation| child_routes.get(invocation))
            .cloned()
            .collect();
        for (invocation, route) in &child_routes {
            if routes.contains(route) {
                changed |= invocations.insert(invocation.clone());
            }
        }
        for event in events {
            let Some(batch) = replay_batch(event)? else {
                continue;
            };
            let original = events
                .iter()
                .find(|candidate| candidate.id == batch.event_id)
                .ok_or_else(invalid_configuration)?;
            if invocations.contains(&original.invocation_id)
                || invocations.contains(&event.invocation_id)
            {
                changed |= invocations.insert(original.invocation_id.clone());
                changed |= invocations.insert(event.invocation_id.clone());
            }
        }
        if !changed {
            return Ok(events
                .iter()
                .filter(|event| invocations.contains(&event.invocation_id))
                .filter_map(|event| event.content().cloned())
                .collect());
        }
    }
    Err(resource_exhausted())
}

#[allow(clippy::too_many_lines)] // Keep original batch lineage, retained siblings, and transient projection in one ordered path.
fn retain_pending_nested_decisions(
    events: &[Event],
    root_event_id: &str,
    submitted: &HashSet<String>,
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    applications: &ApplicationToolPresentationCatalog,
) -> Result<(), NativeAgentAssemblyError> {
    let mut consumed = HashSet::new();
    for event in events {
        if let Some(batch) = replay_batch(event)?
            && batch.event_id == root_event_id
        {
            consumed.extend(batch.interrupt_ids);
        }
    }
    if !submitted.is_disjoint(&consumed) {
        return Err(invalid_configuration());
    }
    let mut pending = HashSet::new();
    let mut retained_pipelines = HashMap::new();
    for event in events {
        if event.actions.tool_confirmation.is_none() && pipeline_application_pending_event(event)? {
            continue;
        }
        if (event.actions.tool_confirmation.is_some()
            || (event
                .provider_metadata
                .contains_key(super::application_pipeline::BOUNDARY_LEDGER_KEY)
                && static_pipeline_tool_pause(event)?.is_some()))
            && scoped_pipeline_pending_candidate(
                events,
                event,
                root_event_id,
                &consumed,
                &mut pending,
                builders,
                &mut retained_pipelines,
            )?
        {
            continue;
        }
        let (interrupt_id, chain, pause_events) = if event.actions.tool_confirmation.is_some() {
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
            let (interrupt_id, pause_events) = ordinary_pause_events(events, event)?;
            (interrupt_id, chain, pause_events)
        } else if let Some(pause) = static_pipeline_tool_pause(event)? {
            let (original, _, _) = exact_application_call(
                events,
                &pause.container_invocation_id,
                &pause.parent_call_id,
            )?;
            pause.matches_original_call(original)?;
            let chain = application_call_chain_from(
                events,
                &event.invocation_id,
                &pause.container_invocation_id,
                &pause.parent_call_id,
                &event.branch,
            )?;
            (pause.pause_id, chain, vec![event.clone()])
        } else if let Some(pause) =
            pipeline_pause_identity(event).map_err(|_| invalid_configuration())?
        {
            let (call_event, call, ordinal) = exact_application_call(
                events,
                &pause.container_invocation_id,
                &pause.parent_call_id,
            )?;
            let chain = vec![ApplicationCallHop {
                event_id: original_application_batch_id(call_event)?,
                ordinal: application_replay_ordinal(call_event, &pause.parent_call_id)?
                    .unwrap_or(ordinal),
                call_id: pause.parent_call_id,
                tool_name: call.name.to_owned(),
                arguments: call.args.clone(),
                owned_invocation_id: event.invocation_id.clone(),
            }];
            (pause.interrupt_id, chain, vec![event.clone()])
        } else {
            continue;
        };
        if consumed.contains(&interrupt_id)
            || chain.last().is_none_or(|hop| hop.event_id != root_event_id)
        {
            continue;
        }
        let mut completed = false;
        for hop in &chain {
            completed |=
                completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id);
        }
        if completed {
            continue;
        }
        if !pending.insert(interrupt_id.clone()) {
            return Err(invalid_configuration());
        }
        if !submitted.contains(&interrupt_id) {
            insert_retained_pause(
                builders,
                &chain,
                events,
                applications,
                interrupt_id,
                pause_events,
            )?;
        }
    }
    if !submitted.is_subset(&pending) {
        return Err(invalid_configuration());
    }
    for candidate in retained_pipelines.into_values() {
        insert_retained_pipeline_candidate(builders, events, applications, &candidate)?;
    }
    Ok(())
}

struct RetainedPipelineCandidate {
    chain: Vec<ApplicationCallHop>,
    boundary: PipelineApplicationBoundary,
    interrupt_ids: BTreeSet<String>,
}

/// Recognize exact scoped pending IDs without replaying the saved graph as a model.
fn scoped_pipeline_pending_candidate(
    events: &[Event],
    event: &Event,
    root_event_id: &str,
    consumed: &HashSet<String>,
    pending: &mut HashSet<String>,
    builders: &HashMap<String, ChildApplicationResumeBuilder>,
    retained: &mut HashMap<(String, String), RetainedPipelineCandidate>,
) -> Result<bool, NativeAgentAssemblyError> {
    let interrupt_id = if let Some(pause) = static_pipeline_tool_pause(event)? {
        pause.pause_id
    } else {
        let request = event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(invalid_configuration)?;
        let call = request
            .function_call_id
            .as_deref()
            .ok_or_else(invalid_configuration)?;
        sensitive_call_identity(
            &event.invocation_id,
            call,
            &request.tool_name,
            &request.args,
        )
        .map_err(|_| invalid_configuration())?
        .0
    };
    if consumed.contains(&interrupt_id) {
        return Ok(true);
    }
    let Some(boundary) = pipeline_application_boundary(events, event)? else {
        return Ok(false);
    };
    let chain = pipeline_boundary_call_chain(events, &boundary)?;
    if chain.last().is_none_or(|hop| hop.event_id != root_event_id) {
        return Ok(true);
    }
    for hop in &chain {
        if completed_application_calls(events, &hop.event_id)?.contains_key(&hop.call_id) {
            return Ok(true);
        }
    }
    if !pending.insert(interrupt_id.clone()) {
        return Err(invalid_configuration());
    }
    let mut current = builders;
    let mut selected = None;
    for hop in chain.iter().rev() {
        let Some(builder) = current.get(&hop.call_id) else {
            selected = None;
            break;
        };
        if builder.batch_event_id != hop.event_id
            || builder.tool_name != hop.tool_name
            || builder.arguments != hop.arguments
            || builder.ordinal != hop.ordinal
        {
            return Err(invalid_configuration());
        }
        selected = Some(builder);
        current = &builder.children;
    }
    if let Some(builder) = selected {
        if builder.pipeline_descendants.is_none()
            || builder.pipeline_boundary_event_id.as_deref()
                != Some(boundary.pending_event.id.as_str())
        {
            return Err(unsupported_capability());
        }
        // The selected pipeline's scope coordinator retains untouched local ordinary children.
        return Ok(true);
    }
    retain_pipeline_candidate(retained, chain, boundary, interrupt_id)?;
    Ok(true)
}

fn retain_pipeline_candidate(
    retained: &mut HashMap<(String, String), RetainedPipelineCandidate>,
    chain: Vec<ApplicationCallHop>,
    boundary: PipelineApplicationBoundary,
    interrupt_id: String,
) -> Result<(), NativeAgentAssemblyError> {
    let pipeline = chain.first().ok_or_else(invalid_configuration)?;
    let key = (pipeline.event_id.clone(), pipeline.call_id.clone());
    if let Some(candidate) = retained.get_mut(&key) {
        if candidate.boundary.pending_event.id != boundary.pending_event.id
            || candidate.boundary.checkpoint_thread_id != boundary.checkpoint_thread_id
            || candidate.chain.len() != chain.len()
            || candidate.chain.iter().zip(&chain).any(|(left, right)| {
                left.event_id != right.event_id
                    || left.call_id != right.call_id
                    || left.ordinal != right.ordinal
                    || left.arguments != right.arguments
                    || left.tool_name != right.tool_name
            })
            || !candidate.interrupt_ids.insert(interrupt_id)
        {
            return Err(invalid_configuration());
        }
        if candidate.interrupt_ids.len() > MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS {
            return Err(resource_exhausted());
        }
    } else {
        retained.insert(
            key,
            RetainedPipelineCandidate {
                chain,
                boundary,
                interrupt_ids: BTreeSet::from([interrupt_id]),
            },
        );
    }
    Ok(())
}

fn retained_pipeline_pause(
    events: &[Event],
    candidate: &RetainedPipelineCandidate,
) -> Result<RetainedPipelinePause, NativeAgentAssemblyError> {
    let pipeline = candidate.chain.first().ok_or_else(invalid_configuration)?;
    let (original_call, batch_event_id, ordinal) =
        pipeline_boundary_original_call(events, &candidate.boundary)?;
    if batch_event_id != pipeline.event_id || ordinal != pipeline.ordinal {
        return Err(invalid_configuration());
    }
    let projection = retained_pipeline_application_events(
        events,
        &candidate.boundary,
        &candidate.interrupt_ids,
    )?;
    let pause = RetainedPipelinePause {
        schema: APPLICATION_RETAINED_PIPELINE_KEY.to_owned(),
        batch_event_id,
        ordinal,
        parent_call_id: pipeline.call_id.clone(),
        tool_name: pipeline.tool_name.clone(),
        arguments_digest: application_arguments_digest(&pipeline.arguments)?,
        checkpoint_thread_id: candidate.boundary.checkpoint_thread_id.clone(),
        interrupt_ids: candidate.interrupt_ids.clone(),
        original_call,
        events: projection,
    };
    pause.validate()?;
    Ok(pause)
}

fn insert_retained_pipeline_candidate(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    candidate: &RetainedPipelineCandidate,
) -> Result<(), NativeAgentAssemblyError> {
    let pause = retained_pipeline_pause(events, candidate)?;
    let mut current = builders;
    let mut catalog = applications;
    for (index, hop) in candidate.chain.iter().rev().enumerate() {
        let child_catalog = catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        let is_pipeline = index + 1 == candidate.chain.len();
        let history = if is_pipeline {
            Vec::new()
        } else {
            let task = application_task_with_variables(&hop.arguments)?;
            let mut history = vec![Content::new("user").with_text(task)];
            history.extend(application_resume_history(
                events,
                &hop.owned_invocation_id,
            )?);
            history
        };
        let builder = pipeline_boundary_builder(current, hop, history)?;
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.pipeline_descendants.is_some()
            || builder.pipeline_static.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        if is_pipeline {
            if !builder.children.is_empty() {
                return Err(unsupported_capability());
            }
            builder.retained_pipeline = Some(Box::new(pause));
            return Ok(());
        }
        current = &mut builder.children;
        catalog = child_catalog;
    }
    Err(invalid_configuration())
}

/// Retain the original confirmation with its one exact model call.
fn ordinary_pause_events(
    events: &[Event],
    event: &Event,
) -> Result<(String, Vec<Event>), NativeAgentAssemblyError> {
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
    let mut call_events = events
        .iter()
        .filter(|candidate| candidate.invocation_id == event.invocation_id)
        .filter(|candidate| {
            candidate.tool_calls().iter().any(|call| {
                call.call_id == Some(call_id)
                    && call.name == request.tool_name
                    && call.args == &request.args
            })
        });
    let call_event = call_events.next().ok_or_else(invalid_configuration)?;
    if call_events.next().is_some() {
        return Err(invalid_configuration());
    }
    Ok((interrupt_id, vec![call_event.clone(), event.clone()]))
}

fn insert_retained_pause(
    builders: &mut HashMap<String, ChildApplicationResumeBuilder>,
    chain: &[ApplicationCallHop],
    events: &[Event],
    applications: &ApplicationToolPresentationCatalog,
    interrupt_id: String,
    pause_events: Vec<Event>,
) -> Result<(), NativeAgentAssemblyError> {
    let mut current = builders;
    let mut catalog = applications;
    for (index, hop) in chain.iter().rev().enumerate() {
        let child_catalog = catalog
            .child_tools(&hop.tool_name)
            .ok_or_else(invalid_configuration)?;
        let task = application_task_with_variables(&hop.arguments)?;
        let is_static_leaf = index + 1 == chain.len()
            && pause_events
                .last()
                .map(static_pipeline_tool_pause)
                .transpose()?
                .flatten()
                .is_some();
        let mut history = if is_static_leaf {
            Vec::new()
        } else {
            vec![Content::new("user").with_text(task)]
        };
        if !is_static_leaf {
            history.extend(application_resume_history(
                events,
                &hop.owned_invocation_id,
            )?);
        }
        let builder =
            current
                .entry(hop.call_id.clone())
                .or_insert_with(|| ChildApplicationResumeBuilder {
                    batch_event_id: hop.event_id.clone(),
                    tool_name: hop.tool_name.clone(),
                    arguments: hop.arguments.clone(),
                    ordinal: hop.ordinal,
                    owned_invocation_id: hop.owned_invocation_id.clone(),
                    history,
                    decision: None,
                    children: HashMap::new(),
                    pipeline: None,
                    pipeline_descendants: None,
                    pipeline_static: None,
                    pipeline_boundary_event_id: None,
                    retained: None,
                    retained_pipeline: None,
                });
        if builder.batch_event_id != hop.event_id
            || builder.tool_name != hop.tool_name
            || builder.arguments != hop.arguments
            || builder.ordinal != hop.ordinal
            || builder.owned_invocation_id != hop.owned_invocation_id
        {
            return Err(invalid_configuration());
        }
        if index + 1 == chain.len() {
            if builder.decision.is_some()
                || builder.pipeline.is_some()
                || builder.pipeline_descendants.is_some()
                || builder.pipeline_static.is_some()
                || builder.retained_pipeline.is_some()
                || !builder.children.is_empty()
                || builder
                    .retained
                    .replace(Box::new(RetainedApplicationPause {
                        interrupt_id,
                        parent_call_id: hop.call_id.clone(),
                        events: pause_events,
                    }))
                    .is_some()
            {
                return Err(unsupported_capability());
            }
            return Ok(());
        }
        if builder.decision.is_some()
            || builder.pipeline.is_some()
            || builder.pipeline_descendants.is_some()
            || builder.pipeline_static.is_some()
            || builder.retained.is_some()
            || builder.retained_pipeline.is_some()
        {
            return Err(unsupported_capability());
        }
        current = &mut builder.children;
        catalog = child_catalog;
    }
    Err(invalid_configuration())
}

/// Reproject an untouched pause without adding another original confirmation to durable history.
pub(super) fn retained_application_events(
    event: &Event,
) -> Result<Option<Vec<Event>>, NativeAgentAssemblyError> {
    let (encoded, is_pipeline, max_bytes) = if let Some(encoded) = event
        .provider_metadata
        .get(APPLICATION_RETAINED_PIPELINE_KEY)
    {
        (encoded, true, MAX_PIPELINE_TOOL_PENDING_BYTES)
    } else if let Some(encoded) = event.provider_metadata.get(APPLICATION_RETAINED_PAUSE_KEY) {
        (encoded, false, MAX_RETAINED_PAUSE_BYTES)
    } else {
        return Ok(None);
    };
    if encoded.len() > max_bytes
        || event.provider_metadata.len() != 1
        || event.content().is_some()
        || event.actions.tool_confirmation.is_some()
        || event.actions.tool_confirmation_decision.is_some()
        || !event.actions.state_delta.is_empty()
        || !event.actions.artifact_delta.is_empty()
        || event.actions.transfer_to_agent.is_some()
        || event.actions.escalate
    {
        return Err(invalid_configuration());
    }
    if is_pipeline {
        let mut pause: RetainedPipelinePause =
            serde_json::from_str(encoded).map_err(|_| invalid_configuration())?;
        pause.validate()?;
        for retained in &mut pause.events {
            rebind_pipeline_tool_boundary(retained, &event.invocation_id)?;
        }
        return Ok(Some(pause.events));
    }
    let mut pause: RetainedApplicationPause =
        serde_json::from_str(encoded).map_err(|_| invalid_configuration())?;
    if pause.events.is_empty()
        || pause.events.len() > 2
        || pause.parent_call_id.is_empty()
        || pause.parent_call_id.len() > 512
    {
        return Err(invalid_configuration());
    }
    let confirmation = pause.events.last().ok_or_else(invalid_configuration)?;
    let expected = if let Some(request) = &confirmation.actions.tool_confirmation {
        if pause.events.len() != 2 {
            return Err(invalid_configuration());
        }
        let call_id = request
            .function_call_id
            .as_deref()
            .ok_or_else(invalid_configuration)?;
        let call_event = &pause.events[0];
        if call_event.invocation_id != confirmation.invocation_id
            || !call_event.tool_calls().iter().any(|call| {
                call.call_id == Some(call_id)
                    && call.name == request.tool_name
                    && call.args == &request.args
            })
        {
            return Err(invalid_configuration());
        }
        sensitive_call_identity(
            &confirmation.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .map_err(|_| invalid_configuration())?
        .0
    } else {
        if pause.events.len() != 1 {
            return Err(invalid_configuration());
        }
        if let Some(pause) = static_pipeline_tool_pause(confirmation)? {
            pause.pause_id
        } else {
            pipeline_pause_identity(confirmation)
                .map_err(|_| invalid_configuration())?
                .ok_or_else(invalid_configuration)?
                .interrupt_id
        }
    };
    if expected != pause.interrupt_id {
        return Err(invalid_configuration());
    }
    for retained in &mut pause.events {
        let (_, parent) = persisted_parent_route(retained)?.ok_or_else(invalid_configuration)?;
        if parent != pause.parent_call_id {
            return Err(invalid_configuration());
        }
        if static_pipeline_tool_pause(retained)?.is_some() {
            rebind_pipeline_tool_boundary(retained, &event.invocation_id)?;
        } else {
            retained.provider_metadata.insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                event.invocation_id.clone(),
            );
        }
    }
    Ok(Some(pause.events))
}

fn nested_resume_markers(events: &[Event]) -> Result<Vec<Content>, NativeAgentAssemblyError> {
    let mut invocations = HashSet::new();
    for event in events {
        if replay_batch(event)?.is_some() {
            invocations.insert(event.invocation_id.as_str());
        }
    }
    Ok(events
        .iter()
        .filter(|event| {
            event.author == "user" && invocations.contains(event.invocation_id.as_str())
        })
        .filter_map(|event| event.content().cloned())
        .collect())
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
    batch: ApplicationReplayBatch,
    previous_markers: Vec<Content>,
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
                    provider_metadata: Some(json!({APPLICATION_REPLAY_BATCH_KEY: self.batch})),
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
                    .generate_content(self.model_continuation(request)?, stream_response)
                    .await
            }
            Err(_) => {
                self.delegate
                    .generate_content(self.model_continuation(request)?, stream_response)
                    .await
            }
        }
    }
}

impl ApplicationReplayModel {
    fn model_continuation(&self, mut request: LlmRequest) -> adk_rust::Result<LlmRequest> {
        request.contents.retain(|content| {
            !self
                .previous_markers
                .iter()
                .any(|marker| content.role == marker.role && content.parts == marker.parts)
        });
        super::replay_history::model_continuation(request, &self.replay_marker)
    }

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
    code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
    pub(crate) model_facade: Arc<ModelFacade>,
    pub(crate) policy: Arc<ToolAdmissionPolicy>,
    pub(crate) mcp_connector: Arc<dyn McpConnector>,
    mcp_tokens: &'a Map<String, Value>,
    event_sender: Option<ApplicationEventSender>,
    resume: Option<ApplicationResumeCoordinator>,
    model_scopes: Option<ModelScopeSessions>,
    /// #973: the CONVERSATION thread a saved pipeline child namespaces its own
    /// checkpoint thread under. A caller that does not supply one gets no
    /// pipeline children — the pipeline PARENT is exactly that caller, because
    /// it owns its pipeline participants itself through its graph.
    conversation_thread_id: Option<String>,
    materialization: ApplicationMaterializationPath,
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
            code: None,
            model_facade,
            policy,
            mcp_connector,
            mcp_tokens,
            event_sender: None,
            resume: None,
            model_scopes: None,
            conversation_thread_id: None,
            materialization: ApplicationMaterializationPath::default(),
        }
    }

    pub(super) fn with_code(
        mut self,
        code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
    ) -> Self {
        self.code = code;
        self
    }

    pub(super) fn with_model_scopes(mut self, scopes: ModelScopeSessions) -> Self {
        self.model_scopes = Some(scopes);
        self
    }

    pub(super) fn with_materialization(mut self, path: ApplicationMaterializationPath) -> Self {
        self.materialization = path;
        self
    }

    /// Admit saved PIPELINE children, namespaced under this conversation's
    /// durable thread (#973).
    #[must_use]
    pub(crate) fn with_conversation_thread(mut self, thread_id: String) -> Self {
        self.conversation_thread_id = Some(thread_id);
        self
    }
}

/// One exact frozen Application alias bound to its invocation-owned ADK tool.
pub(crate) struct MaterializedApplicationTool {
    pub(crate) alias: String,
    pub(crate) agent_type: String,
    pub(crate) saved_agent_fingerprint: Option<SavedAgentFingerprint>,
    model_name: String,
    child_tools: ApplicationToolPresentationCatalog,
    sensitive_tools: SensitiveToolCatalog,
    delegated_authorization: DelegatedAuthorizationCatalog,
    internal_tools: InternalToolCatalog,
    pub(crate) tool: Arc<dyn Tool>,
}

struct BuiltApplication {
    saved_agent_fingerprint: Option<SavedAgentFingerprint>,
    instruction_template: String,
    variables: super::variables::AgentCallVariables,
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
) -> Result<
    (
        Option<MaterializedApplicationToolset>,
        Vec<SkippedApplicationChild>,
    ),
    NativeAgentAssemblyError,
> {
    let (runtime, skipped) = materialize_application_runtime(
        snapshot,
        platform,
        runtime_context,
        elitea_context,
        fallback_profile,
        dependencies,
        None,
    )
    .await?;
    let Some(MaterializedApplicationRuntime {
        tools,
        presentations,
        events,
        resume,
    }) = runtime
    else {
        return Ok((None, skipped));
    };
    Ok((
        Some(MaterializedApplicationToolset {
            toolset: Arc::new(BasicToolset::new(
                "elitea_nested_applications",
                tools.into_iter().map(|entry| entry.tool).collect(),
            )),
            presentations,
            events,
            resume,
        }),
        skipped,
    ))
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
) -> Result<
    (
        Option<MaterializedApplicationRuntime>,
        Vec<SkippedApplicationChild>,
    ),
    NativeAgentAssemblyError,
> {
    let (event_sender, event_receiver) = mpsc::channel(APPLICATION_EVENT_CHANNEL_CAPACITY);
    let resume = ApplicationResumeCoordinator::default();
    dependencies.event_sender = Some(event_sender);
    dependencies.resume = Some(resume.clone());
    // #973: read before the dependencies are consumed below. A caller that
    // declared no conversation thread gets no pipeline children.
    let MaterializedPipelineChildren {
        tools: pipeline_children,
        skipped: skipped_pipeline_children,
    } = materialize_pipeline_children(
        snapshot,
        platform,
        runtime_context,
        Arc::clone(&elitea_context),
        fallback_profile,
        &dependencies,
        selected_aliases,
    )
    .await?;
    let mut materialized = materialize_application_tools(
        snapshot,
        platform,
        runtime_context,
        elitea_context,
        fallback_profile,
        dependencies,
        selected_aliases,
    )
    .await?;
    materialized.extend(pipeline_children);
    // No TOOL means no descendant channel: `ApplicationEventStreamingAgent`
    // selects on a receiver whose only senders live in the tools, so installing
    // it with none would leave it selecting on a closed channel forever. The
    // skipped list still travels — it is the whole point of the degrade — but
    // it travels beside the runtime rather than inside it.
    if materialized.is_empty() {
        return Ok((None, skipped_pipeline_children));
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
    Ok((
        Some(MaterializedApplicationRuntime {
            tools: materialized,
            presentations,
            events: ApplicationEventReceiver {
                inner: Arc::new(Mutex::new(Some(event_receiver))),
            },
            resume,
        }),
        skipped_pipeline_children,
    ))
}

/// Compile every attached saved PIPELINE child into one callable tool (#973).
///
/// The per-child node-event channel is created here and owned by the tool: the
/// tool forwards what it drains onto the parent's descendant channel, so a
/// pipeline child's `llm` and `tool` node progress shows up in the parent's
/// transcript the same way a nested agent's does.
async fn materialize_pipeline_children(
    snapshot: &AdmittedToolSnapshot<'_>,
    platform: &PlatformClient,
    runtime_context: &ClaimBoundRuntimeContextAuthority,
    elitea_context: Arc<ClaimScopedEliteaContext>,
    fallback_profile: &OrdinaryNoToolProfile,
    dependencies: &ApplicationToolDependencies<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
) -> Result<MaterializedPipelineChildren, NativeAgentAssemblyError> {
    let Some(conversation_thread_id) = dependencies.conversation_thread_id.clone() else {
        return Ok(MaterializedPipelineChildren::default());
    };
    let references = pipeline_child_references(snapshot, selected_aliases);
    let mut tools = Vec::with_capacity(references.len());
    let mut skipped = Vec::new();
    for reference in references {
        let (node_events_sender, node_events_receiver) = pipeline_node_event_channel();
        // #990 review 4: one child this worker cannot build must not fail the
        // WHOLE turn. That was the original shape of #973 — an attached
        // pipeline bricked every turn of the agent, including the turns that
        // never mentioned it — and admitting the buildable ones must not
        // reintroduce it for the unbuildable rest (an `agent` node inside the
        // child, a stale tool snapshot, a deleted frozen version, a pause kind
        // this parent cannot resume). The child is skipped and NAMED instead,
        // the same honest degrade a skipped internal tool takes.
        let built = materialize_saved_pipeline_tool(
            platform,
            runtime_context,
            Arc::clone(&elitea_context),
            Arc::clone(&dependencies.model_facade),
            &dependencies.mcp_connector,
            dependencies.mcp_tokens,
            Arc::clone(&dependencies.policy),
            fallback_profile,
            node_events_sender,
            reference.identity,
            reference.project_id,
            dependencies
                .model_scopes
                .clone()
                .ok_or_else(invalid_configuration)?,
            dependencies.code.clone(),
            conversation_thread_id.clone(),
            dependencies.materialization.clone(),
        )
        .await;
        let (definition, runtimes, child_tools) = match built {
            Ok(built) => built,
            Err(error) => {
                tracing::warn!(
                    application_id = reference.identity.0,
                    version_id = reference.identity.1,
                    error_code = error.code().as_str(),
                    "an attached pipeline child could not be built and was skipped"
                );
                if skipped.len() < MAX_SKIPPED_APPLICATION_CHILDREN {
                    skipped.push(SkippedApplicationChild {
                        agent_type: PIPELINE_APPLICATION_AGENT_TYPE.to_owned(),
                        name: bounded_skipped_label(&reference.alias),
                    });
                }
                continue;
            }
        };
        let name = application_tool_name(reference.identity);
        let tool = Arc::new(ApplicationPipelineTool::new(
            name,
            pipeline_tool_description(&reference.alias, reference.description.as_deref()),
            definition,
            runtimes,
            node_events_receiver,
            PipelineToolParentBinding {
                conversation_thread_id: conversation_thread_id.clone(),
                event_sender: dependencies.event_sender.clone(),
                resume: dependencies.resume.clone(),
            },
        ));
        tools.push(MaterializedApplicationTool {
            alias: reference.alias,
            agent_type: "pipeline".to_owned(),
            saved_agent_fingerprint: None,
            // The child graph owns its own per-node models; the presentation
            // label names what the parent delegated to, not one model.
            model_name: "pipeline".to_owned(),
            child_tools,
            sensitive_tools: SensitiveToolCatalog::default(),
            delegated_authorization: DelegatedAuthorizationCatalog::default(),
            internal_tools: InternalToolCatalog::default(),
            tool,
        });
    }
    Ok(MaterializedPipelineChildren { tools, skipped })
}

/// What one snapshot's pipeline children became: the ones this parent can run,
/// and the ones it must SAY it could not.
#[derive(Default)]
pub(crate) struct MaterializedPipelineChildren {
    tools: Vec<MaterializedApplicationTool>,
    skipped: Vec<SkippedApplicationChild>,
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
        model_scopes: dependencies.model_scopes,
        code: dependencies.code,
        conversation_thread_id: dependencies.conversation_thread_id,
        materialization: dependencies.materialization,
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
            saved_agent_fingerprint: application.saved_agent_fingerprint,
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
    model_scopes: Option<ModelScopeSessions>,
    code: Option<Arc<dyn super::graph::CodeSandboxRuntime>>,
    conversation_thread_id: Option<String>,
    materialization: ApplicationMaterializationPath,
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
                // Unreachable from `application_references`, which filters
                // these out and reports them as skipped (#973). Kept as the
                // invariant for any other caller: this module compiles a
                // nested `LlmAgent`, and a stored pipeline is a graph the
                // pipeline assembler owns.
                if reference.agent_type != SUPPORTED_APPLICATION_AGENT_TYPE {
                    return Err(unsupported_capability());
                }
                if reference.project_id.is_some_and(|project_id| {
                    project_id != self.elitea_context.resource_project_id()
                }) {
                    return Err(invalid_configuration());
                }
                let next_path = self.materialization.enter_agent(identity)?;
                if let Some(application) = self.applications.get(&identity) {
                    tracing::Span::current().record("cache_hit", true);
                    return Ok(application.clone());
                }
                tracing::Span::current().record("cache_hit", false);
                if !self.resolving.insert(identity) {
                    return Err(invalid_configuration());
                }
                tracing::Span::current().record("stage", "resolve_version");
                let parent_path = std::mem::replace(&mut self.materialization, next_path);
                let result = self.build_uncached(reference, tier).await;
                self.materialization = parent_path;
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
        let saved_agent_fingerprint = loaded.saved_agent_fingerprint();
        let version = loaded.into_version_details();
        self.materialization.admit_version(&version)?;
        let profile = OrdinaryNoToolProfile::from_nested_version(&version, self.fallback_profile)?;
        let frozen = FrozenToolSnapshot::from_version_details(&version)
            .map_err(snapshot_error)?
            .apply_policy(self.policy.as_ref());
        let capabilities = configured_capabilities(&frozen);
        let (mut toolsets, sensitive_tools, delegated_authorization) =
            self.materialize_non_application_toolsets(&frozen).await?;
        let internal_tools = profile.internal_tools();
        // `None`: a NESTED child does not get the builder tools (#940 A8).
        // The toggles are conversation-scoped capabilities the user turned on
        // for the chat they are in; a child agent called as a tool is running
        // on the parent's behalf, and handing it a project WRITE the user
        // enabled for the parent's own turn would widen the toggle past what
        // was switched on. The parent keeps its own builder tools either way,
        // so nothing the user asked for is lost — the write happens one level
        // up, where it was authorized.
        toolsets.extend(internal_tools.toolsets(None));
        let has_non_application_tools = !toolsets.is_empty();
        let nested_references = application_references(&frozen, None)?;
        let parallel_applications = !nested_references.is_empty()
            && frozen
                .iter()
                .all(|reference| reference.kind() == FrozenToolKind::Application)
            && sensitive_tools.is_empty()
            && delegated_authorization.is_empty()
            && internal_tools.is_empty();
        let pipeline_children = self
            .materialize_nested_pipeline_children(&frozen, &profile)
            .await?;
        let mut reserved_toolsets = BTreeSet::from([ASK_USER_TOOLSET_NAME.to_owned()]);
        let (nested_toolset, child_tools) = self
            .build_nested_toolset(
                nested_references,
                pipeline_children,
                reference.identity,
                tier,
            )
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
            model_scopes: self
                .model_scopes
                .as_ref()
                .map(|scopes| scopes.with_application_tools(child_tools.agent_tool_names())),
        });
        Ok(Arc::new(BuiltApplication {
            saved_agent_fingerprint,
            instruction_template: version
                .get("instructions")
                .and_then(Value::as_str)
                .ok_or_else(invalid_configuration)?
                .to_owned(),
            variables: super::variables::AgentCallVariables::admit(&version)?,
            agent,
            model_name,
            child_tools,
            sensitive_tools,
            delegated_authorization,
            internal_tools,
        }))
    }

    async fn materialize_nested_pipeline_children(
        &self,
        frozen: &AdmittedToolSnapshot<'_>,
        profile: &OrdinaryNoToolProfile,
    ) -> Result<Vec<MaterializedApplicationTool>, NativeAgentAssemblyError> {
        if !self.materialization.scoped_ready() {
            return Ok(Vec::new());
        }
        if self.conversation_thread_id.is_none()
            && !pipeline_child_references(frozen, None).is_empty()
        {
            return Err(invalid_configuration());
        }
        let dependencies = ApplicationToolDependencies {
            code: self.code.clone(),
            model_facade: self.model_facade.clone(),
            policy: self.policy.clone(),
            mcp_connector: self.mcp_connector.clone(),
            mcp_tokens: self.mcp_tokens,
            event_sender: self.event_sender.clone(),
            resume: self.resume.clone(),
            model_scopes: self.model_scopes.clone(),
            conversation_thread_id: self.conversation_thread_id.clone(),
            materialization: self.materialization.clone(),
        };
        let built = materialize_pipeline_children(
            frozen,
            self.platform,
            self.runtime_context,
            self.elitea_context.clone(),
            profile,
            &dependencies,
            None,
        )
        .await?;
        // Scoped projection must contain every frozen pipeline attachment.
        if !built.skipped.is_empty() {
            return Err(unsupported_capability());
        }
        Ok(built.tools)
    }

    async fn build_nested_toolset(
        &mut self,
        references: Vec<ApplicationReference>,
        pipeline_children: Vec<MaterializedApplicationTool>,
        parent_identity: ApplicationIdentity,
        tier: usize,
    ) -> Result<
        (Option<Arc<dyn Toolset>>, ApplicationToolPresentationCatalog),
        NativeAgentAssemblyError,
    > {
        if references.is_empty() && pipeline_children.is_empty() {
            return Ok((None, ApplicationToolPresentationCatalog::default()));
        }
        let mut tools: Vec<Arc<dyn Tool>> =
            Vec::with_capacity(references.len() + pipeline_children.len());
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
        for entry in pipeline_children {
            presentations
                .insert_runtime(
                    entry.tool.name().to_owned(),
                    entry.alias,
                    entry.agent_type,
                    entry.model_name,
                    entry.child_tools,
                    ApplicationToolGuardCatalogs::new(
                        entry.sensitive_tools,
                        entry.delegated_authorization,
                        entry.internal_tools,
                    ),
                )
                .map_err(|_| invalid_configuration())?;
            tools.push(entry.tool);
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

/// One attached application child this worker cannot build, kept so the run
/// can SAY so (#973).
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) struct SkippedApplicationChild {
    /// The child's own `agent_type` — `pipeline` in every case measured, and
    /// `predict` for the other shape the picker can reach.
    pub(crate) agent_type: String,
    /// The alias the parent stores, which is the name the user picked.
    pub(crate) name: String,
}

/// The attached application children whose `agent_type` this worker does not
/// execute, in a stable (type, name) order.
///
/// #973: the agent editor's tool picker OFFERS pipelines — it runs a second
/// listing with `agents_type: 'pipeline'` — and the relation route stores the
/// reference on the parent version. This worker builds only `agent` children
/// (a stored pipeline is a graph the pipeline assembler owns, not an
/// `LlmAgent` this module can compile), and it used to REFUSE the whole
/// assembly over one: the refusal landed during assembly, before any model
/// call, so a single attached pipeline killed EVERY turn of that agent,
/// including the turns that never mentioned it. Nothing told the user what
/// they had broken.
///
/// The child is now skipped and reported, which is the same honest degrade
/// `swarm` and the other recognized-and-unimplemented internal tools already
/// take (#866, `InternalToolCatalog::skipped_platform_tools`): the agent keeps
/// working, the tool it cannot build is simply not offered to the model, and
/// the run carries one notice naming it. The capability itself — compiling a
/// pipeline child as a sub-graph tool whose HITL node surfaces through the
/// parent — remains #973's open half.
///
/// The python (SDK) worker builds these children, so the attachment is NOT
/// refused upstream: `elitea_sdk`'s application toolkit takes an `agent_type`
/// of `agent`, `pipeline` or `predict` (runtime/toolkits/application.py), and
/// the assistant's own "non-pipeline agents cannot have pipelines as toolkits"
/// check is commented out (runtime/langchain/assistant.py). Refusing the
/// relation write or hiding pipelines in the picker would therefore take a
/// working capability away from every SDK-worker deployment.
pub(crate) fn skipped_application_children(
    snapshot: &AdmittedToolSnapshot<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
    pipelines_admitted: bool,
) -> Vec<SkippedApplicationChild> {
    let mut skipped = BTreeSet::new();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Application)
        .filter(|reference| {
            selected_aliases.is_none_or(|aliases| aliases.contains(reference.toolkit_name()))
        })
    {
        let Some(agent_type) = reference.application_agent_type() else {
            continue;
        };
        if agent_type == SUPPORTED_APPLICATION_AGENT_TYPE
            || (pipelines_admitted && agent_type == PIPELINE_APPLICATION_AGENT_TYPE)
        {
            continue;
        }
        if skipped.len() >= MAX_SKIPPED_APPLICATION_CHILDREN {
            break;
        }
        skipped.insert(SkippedApplicationChild {
            agent_type: bounded_skipped_label(agent_type),
            name: bounded_skipped_label(reference.toolkit_name()),
        });
    }
    skipped.into_iter().collect()
}

/// One deterministic line per skipped child, in the same shape
/// `InternalToolCatalog::skipped_tools_notice_text` writes: the set decides the
/// text, never the order the version listed them in, so two otherwise
/// identical turns cannot produce two different notices.
pub(crate) fn skipped_application_children_notice_text(
    skipped: &[SkippedApplicationChild],
) -> Option<String> {
    if skipped.is_empty() {
        return None;
    }
    let mut lines = skipped
        .iter()
        .map(|child| {
            format!(
                "attached {} '{}' is not available on this worker",
                child.agent_type, child.name
            )
        })
        .collect::<Vec<_>>();
    lines.sort_unstable();
    lines.dedup();
    Some(lines.join("\n"))
}

/// Truncate a name or type before it reaches a notice the model reads. Both
/// come from stored rows, and neither has a length the runtime enforces.
fn bounded_skipped_label(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .take(MAX_SKIPPED_APPLICATION_LABEL_CHARS)
        .collect()
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
        // #973: a child this worker cannot build is SKIPPED here rather than
        // failing the assembly below. `skipped_application_children` reports
        // the same set to the run's notice; the `agent_type` guard in
        // `ApplicationAssemblyState::build` stays as the invariant for any
        // other caller.
        .filter(|reference| {
            reference.application_agent_type() == Some(SUPPORTED_APPLICATION_AGENT_TYPE)
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

#[derive(Clone)]
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
    model_scopes: Option<ModelScopeSessions>,
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
        let model = self.bind_model()?;
        let checkpoint = self.context_checkpoint(&model, None)?;
        let agent = self.build_agent(
            model.provider_model(),
            self.toolsets.clone(),
            self.delegated_authorization.clone(),
            checkpoint,
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
    fn bind_model(&self) -> adk_rust::Result<BoundModelFacade> {
        let invocation = ModelInvocation {
            response_schema: None,
            allow_text_continuation: false,
            context_budget: self.profile.context_budget(),
            model_name: self.profile.model_name().to_owned(),
            system_instruction: self.profile.instructions().to_owned(),
            max_tokens: self.profile.max_tokens(),
            reasoning_effort: self.profile.reasoning_effort().map(model_reasoning_effort),
            temperature: self.profile.temperature(),
            // A truncated final answer has its own bounded continuation allowance.
            // ADK still admits only step_limit logical model/tool iterations.
            max_model_turns: self.profile.step_limit()
                + crate::agents::request::MAX_OUTPUT_CONTINUATION_CALLS,
        };
        let adapter = match self.profile.model_provider() {
            OrdinaryModelProvider::OpenAiChat => ModelAdapterKind::OpenAiCompatible,
            OrdinaryModelProvider::NativeAnthropic => ModelAdapterKind::Anthropic,
        };
        self.model_facade
            .bind_with_summary(
                adapter,
                self.elitea_context.as_ref(),
                self.profile.model_project_id(),
                invocation,
                self.profile.summary_model(),
            )
            .map_err(model_error)
    }

    fn context_checkpoint(
        &self,
        model: &BoundModelFacade,
        replay_marker: Option<Content>,
    ) -> adk_rust::Result<Option<Arc<ScopedModelCheckpoint>>> {
        let plan = match self.profile.context_management() {
            ContextManagementPlan::Disabled => None,
            ContextManagementPlan::Summarize(plan) => Some(plan),
        };
        let storage = self
            .model_scopes
            .as_ref()
            .ok_or_else(agent_configuration_error)?;
        Ok(Some(
            storage.checkpoint(
                plan,
                model.request_budget(),
                model
                    .summarization_model()
                    .ok_or_else(agent_configuration_error)?,
                replay_marker,
                model.durable_completion(),
            ),
        ))
    }

    fn build_agent(
        &self,
        model: Arc<dyn adk_rust::Llm>,
        toolsets: Vec<Arc<dyn Toolset>>,
        mut authorization: DelegatedAuthorizationCatalog,
        checkpoint: Option<Arc<ScopedModelCheckpoint>>,
    ) -> adk_rust::Result<Arc<dyn Agent>> {
        let (model, toolsets) =
            crate::toolkits::bind_authorization_model_tools(model, toolsets, &mut authorization)?;
        let model = checkpoint.as_ref().map_or_else(
            || model.clone(),
            |checkpoint| checkpoint.clone().delegation_model(model.clone()),
        );
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
        builder = self.profile.instruction_plan().bind_builder(builder);
        if let Some(checkpoint) = &checkpoint {
            builder = checkpoint.clone().bind(builder);
        }
        for toolset in toolsets
            .into_iter()
            .chain(self.profile.instruction_plan().toolsets())
        {
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
        if self.parallel_applications && self.profile.instruction_plan().is_empty() {
            builder = builder.tool_execution_strategy(ToolExecutionStrategy::Parallel);
        }
        let agent = builder
            .build()
            .map(|agent| Arc::new(agent) as Arc<dyn Agent>)
            .map_err(|_| agent_configuration_error())?;
        let agent = checkpoint.map_or_else(
            || agent.clone(),
            |checkpoint| checkpoint.wrap(agent.clone()),
        );
        let agent = self.profile.instruction_plan().wrap(agent);
        let agent = delegated_authorization_agent(agent, authorization);
        Ok(clarifying_question_agent(agent, self.internal_tools))
    }

    fn prepare_resume(
        &self,
        action: ChildApplicationResumeAction,
        sensitive_tools: &SensitiveToolCatalog,
    ) -> adk_rust::Result<PreparedChildApplicationResume> {
        match action {
            // A pipeline continuation cannot reach an AGENT child: the two are
            // built by different materializers and keyed by different tools, so
            // arriving here means the resume was routed to the wrong child.
            ChildApplicationResumeAction::Pipeline(_)
            | ChildApplicationResumeAction::PipelineStatic(_)
            | ChildApplicationResumeAction::PipelineDescendants(_)
            | ChildApplicationResumeAction::Retained(_)
            | ChildApplicationResumeAction::RetainedPipeline(_) => {
                Err(application_event_channel_error())
            }
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
                let bound = self.bind_model()?;
                let replay_pending = replay.emits_pending_call();
                let prepared = replay.bind(bound.provider_model());
                let (model, run_input, toolsets) = prepared.into_parts(self.toolsets.clone());
                let (user_content, run_config) = run_input.into_parts();
                let checkpoint = self
                    .context_checkpoint(&bound, Some(user_content.clone()))?
                    .map(|checkpoint| checkpoint.with_replay_pending(replay_pending));
                Ok(PreparedChildApplicationResume {
                    agent: self.build_agent(model, toolsets, authorization, checkpoint)?,
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
                let batch = application_replay_batch(&children, &interrupt_ids)
                    .map_err(nested_resume_execution_error)?;
                let bound = self.bind_model()?;
                let checkpoint = self.context_checkpoint(&bound, Some(user_content.clone()))?;
                let model: Arc<dyn Llm> = Arc::new(ApplicationReplayModel {
                    delegate: bound.provider_model(),
                    state: AtomicU8::new(REPLAY_APPLICATIONS_PENDING),
                    calls,
                    batch,
                    previous_markers: Vec::new(),
                    replay_marker: user_content.clone(),
                });
                Ok(PreparedChildApplicationResume {
                    agent: self.build_agent(
                        model,
                        self.toolsets.clone(),
                        self.delegated_authorization.clone(),
                        checkpoint,
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
        let mut schema = json!({
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
        });
        if let Some(properties) = schema.get_mut("properties").and_then(Value::as_object_mut) {
            properties.extend(self.application.variables.schema_properties());
        }
        Some(schema)
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "response": {"type": "string"},
                "error": {"type": "string"},
                "failure": {
                    "type": "object",
                    "description": "A failed child result. Read recovery guidance before continuing.",
                    "properties": {
                        "code": {"type": "string"},
                        "message": {"type": "string"},
                        "recoverable": {"type": "boolean"},
                        "retryable": {"type": "boolean"},
                        "recovery_action": {"type": "string"},
                        "guidance": {"type": "string"},
                        "partial_output_available": {"type": "boolean"},
                        "partial_output": {"type": ["string", "null"]}
                    },
                    "required": ["code", "message", "recoverable", "retryable", "recovery_action", "guidance", "partial_output_available", "partial_output"],
                    "additionalProperties": false
                }
            },
            "oneOf": [{"required": ["response"]}, {"required": ["error", "failure"]}],
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
            Ok(result) if result.get("error").is_some() => {
                span.record("outcome", "failed");
                span.record("error_code", "model.output_continuation_failed");
            }
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
        let task = application_task_with_variables(&arguments).map_err(|_| tool_input_error())?;
        let local_agent = if arguments.as_object().is_some_and(|args| args.len() == 1) {
            self.application.agent.clone()
        } else {
            let values = self
                .application
                .variables
                .bind(arguments.as_object().ok_or_else(tool_input_error)?)
                .map_err(|_| tool_input_error())?;
            let mut agent = self.application.agent.as_ref().clone();
            agent.profile = agent
                .profile
                .with_instructions(values.render(&self.application.instruction_template));
            Arc::new(agent)
        };
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
        if let Some(resume) = &resume
            && let Some(result) = resume
                .retained_result(
                    ctx.as_ref(),
                    self.name(),
                    &arguments,
                    self.event_sender.as_ref(),
                )
                .await?
        {
            return Ok(result);
        }
        let (agent, content, run_config, history, children) = match resume {
            Some(resume) if resume.tool_name == self.name() && resume.arguments == arguments => {
                let prepared =
                    local_agent.prepare_resume(resume.action, &self.application.sensitive_tools)?;
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
                local_agent as Arc<dyn Agent>,
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
        let pipeline_node = pipeline_application_container(ctx, self.name())?;
        let mut stream = agent.run(child_context.clone()).await?;
        let mut final_response = None;
        let mut last_text = None;
        let mut application_batch = ApplicationCallBatch::default();
        while let Some(result) = stream.next().await {
            let mut event = match result {
                Ok(event) => event,
                Err(error) => {
                    if let Some(report) =
                        child_failure_report(&error, last_text.take(), pipeline_node)
                    {
                        tracing::error!(
                            event = "nested_application_failed",
                            application_id = self.identity.0,
                            version_id = self.identity.1,
                            invocation_id = %ctx.invocation_id(),
                            function_call_id = %ctx.function_call_id(),
                            error_code = error.code,
                            cause_message = report["failure"]["message"].as_str(),
                            recovery = report["failure"]["recovery_action"].as_str(),
                            failure_diagnostic = %crate::diagnostics::failure::capture()
                                .as_deref().unwrap_or("disabled_or_rate_limited"),
                            "child model failed; returning failure to the orchestrator"
                        );
                        return Ok(report);
                    }
                    let failure = if error.code == "model.output_continuation_failed" {
                        ApplicationEventFailure::OutputContinuation(
                            super::model_checkpoint::output::failure_reason(&error).unwrap_or(
                                super::model_checkpoint::output::ContinuationFailure::ChildFailure,
                            ),
                        )
                    } else if crate::protocol::output::model_failure(Some(error.code))
                        != crate::protocol::output::RuntimeFailureKind::Internal
                    {
                        ApplicationEventFailure::Model(error.code)
                    } else {
                        ApplicationEventFailure::ChildExecution
                    };
                    self.send_fatal(failure).await?;
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
            if super::instruction_authority::valid_state_delta(&event.actions.state_delta) {
                child_context
                    .session
                    .state
                    .write()
                    .map_err(|_| application_event_channel_error())?
                    .extend(event.actions.state_delta.clone());
            }
            if let Some(coordinator) = &self.resume {
                coordinator
                    .observe_call_lineage(&event)
                    .await
                    .map_err(|_| application_event_channel_error())?;
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
                checkpoint_thread_id: None,
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

/// A known child model failure is data for the parent, not a root failure.
/// Other errors keep their existing terminal/control handling.
pub(super) fn child_failure_report(
    error: &AdkError,
    partial: Option<String>,
    pipeline_node: bool,
) -> Option<Value> {
    use crate::protocol::output::{RuntimeFailureKind, model_failure, runtime_error_policy};
    if pipeline_node {
        return None;
    }
    let kind = model_failure(Some(error.code));
    if kind == RuntimeFailureKind::Internal {
        return None;
    }
    let (code, safe_message, retryable) = runtime_error_policy(kind);
    let continuation = kind == RuntimeFailureKind::OutputContinuationExhausted;
    let message = if continuation {
        super::model_checkpoint::output::failure_reason(error)
            .unwrap_or(super::model_checkpoint::output::ContinuationFailure::ChildFailure)
            .to_string()
    } else {
        safe_message.to_owned()
    };
    let recovery = match kind {
        RuntimeFailureKind::ModelAccessDenied
        | RuntimeFailureKind::ModelBudgetExhausted(_)
        | RuntimeFailureKind::CodePreparationUnconfirmed => "ask_administrator",
        _ if retryable => "verify_before_retry",
        _ => "revise_task",
    };
    let partial = partial.filter(|text| !text.is_empty());
    Some(json!({
        "error": message,
        "failure": {
            "code": code.as_str_name().trim_start_matches("RUNTIME_ERROR_CODE_V1_"),
            "message": message,
            "recoverable": true,
            "retryable": retryable,
            "recovery_action": recovery,
            "guidance": "The child task failed. Use only verified partial output. Follow the failure message and recovery action. Do not repeat the identical call automatically. Prior tool side effects may exist; verify them before repeating any action.",
            "partial_output_available": partial.is_some(),
            "partial_output": partial,
        }
    }))
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

pub(super) fn nested_interrupt_result(interrupt_ids: &BTreeSet<String>) -> Value {
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
    lineage: Option<ApplicationResumeCoordinator>,
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
            lineage: None,
        }
    }
    pub(crate) fn with_call_lineage(
        mut self,
        lineage: Option<ApplicationResumeCoordinator>,
    ) -> Self {
        self.lineage = lineage;
        self
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
        let lineage = self.lineage.clone();
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
                            if let Some(coordinator)=&lineage
                                && coordinator.observe_call_lineage(&event).await.is_err() {yield Err(application_event_channel_error());return;}

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

    fn state(&self) -> Option<&dyn State> {
        Some(self.inner.session().state())
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

#[allow(clippy::too_many_lines)] // Keep original batch lineage, retained siblings, and transient projection in one ordered path.
pub(super) fn application_signal_event(signal: ApplicationEventSignal) -> adk_rust::Result<Event> {
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
        ApplicationEventSignal::GraphDescendant {
            root_container_invocation_id,
            root_parent_call_id,
            root_checkpoint_thread_id,
            catalog,
            lineage,
            event,
        } => {
            let mut event = *event;
            let checkpoint_thread = match event
                .provider_metadata
                .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
            {
                Some(thread) => thread.clone(),
                None => super::pipeline::scoped_applications::checkpoint_thread_for_event(
                    &root_checkpoint_thread_id,
                    &catalog,
                    &event,
                )
                .map_err(|_| application_event_channel_error())?
                .ok_or_else(application_event_channel_error)?,
            };
            let admitted = if checkpoint_thread == root_checkpoint_thread_id {
                catalog.root.is_some()
            } else {
                checkpoint_thread
                    .strip_prefix(&format!("{root_checkpoint_thread_id}/"))
                    .is_some_and(|path| catalog.descendants.contains_key(path))
            };
            let has_inner_boundary = event
                .provider_metadata
                .contains_key(super::events::PIPELINE_TOOL_BOUNDARY_METADATA_KEY);
            if has_inner_boundary {
                super::application_pipeline::append_outer_boundary(
                    &mut event,
                    &root_container_invocation_id,
                    &root_parent_call_id,
                    &root_checkpoint_thread_id,
                    lineage.ok_or_else(application_event_channel_error)?,
                    &catalog,
                )
                .map_err(|_| application_event_channel_error())?;
                return Ok(event);
            }
            if !admitted
                || !event
                    .provider_metadata
                    .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
                || !event
                    .provider_metadata
                    .contains_key(DESCENDANT_PARENT_CALL_KEY)
            {
                return Err(application_event_channel_error());
            }
            stamp_pipeline_tool_boundary(
                &mut event,
                &root_container_invocation_id,
                &root_parent_call_id,
                &root_checkpoint_thread_id,
            )?;
            Ok(event)
        }
        ApplicationEventSignal::Event {
            container_invocation_id,
            parent_call_id,
            checkpoint_thread_id,
            event,
        } => {
            let mut event = *event;
            if event
                .provider_metadata
                .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
                || event
                    .provider_metadata
                    .contains_key(DESCENDANT_PARENT_CALL_KEY)
                || event
                    .provider_metadata
                    .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY)
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
            if let Some(checkpoint_thread_id) = checkpoint_thread_id {
                let container = event
                    .provider_metadata
                    .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
                    .cloned()
                    .ok_or_else(application_event_channel_error)?;
                let parent = event
                    .provider_metadata
                    .get(DESCENDANT_PARENT_CALL_KEY)
                    .cloned()
                    .ok_or_else(application_event_channel_error)?;
                stamp_pipeline_tool_boundary(
                    &mut event,
                    &container,
                    &parent,
                    &checkpoint_thread_id,
                )?;
                event.provider_metadata.insert(
                    DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                    checkpoint_thread_id,
                );
            }
            Ok(event)
        }
        ApplicationEventSignal::Fatal(ApplicationEventFailure::OutputContinuation(reason)) => {
            Err(super::model_checkpoint::output::failed(reason))
        }
        ApplicationEventSignal::Fatal(ApplicationEventFailure::ChildExecution) => {
            Err(child_execution_error())
        }
        ApplicationEventSignal::Fatal(ApplicationEventFailure::Model(code)) => Err(AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Internal,
            code,
            "child model execution failed",
        )),
    }
}

fn stamp_pipeline_tool_boundary(
    event: &mut Event,
    container: &str,
    call: &str,
    thread: &str,
) -> adk_rust::Result<()> {
    use super::events::PIPELINE_TOOL_BOUNDARY_METADATA_KEY;
    if [container, call, thread]
        .iter()
        .any(|value| value.is_empty() || value.len() > 1024 || value.chars().any(char::is_control))
        || event
            .provider_metadata
            .contains_key(PIPELINE_TOOL_BOUNDARY_METADATA_KEY)
    {
        return Err(application_event_channel_error());
    }
    let raw =
        json!({"schema":"elitea.pipeline.tool-boundary.v1", "container_invocation_id":container,
        "parent_call_id":call,"checkpoint_thread_id":thread})
        .to_string();
    if raw.len() > 4096 {
        return Err(application_event_channel_error());
    }
    event
        .provider_metadata
        .insert(PIPELINE_TOOL_BOUNDARY_METADATA_KEY.to_owned(), raw);
    Ok(())
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

pub(super) struct ApplicationToolInvocationContext {
    parent_ctx: Arc<dyn ToolContext>,
    agent: Arc<dyn Agent>,
    user_content: Content,
    invocation_id: String,
    branch: String,
    run_config: RunConfig,
    ended: AtomicBool,
    session: ApplicationToolSession,
}

pub(super) fn application_run_config() -> RunConfig {
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

    /// One child context for a saved PIPELINE participant (#973).
    ///
    /// Two fields differ from an agent child and both are load-bearing. The
    /// invocation id is DERIVED from the parent's function-call id rather than
    /// drawn from a process counter, because the pause's public interrupt
    /// identity is digested from it and the resume must recompute the same id
    /// in a later process. The session id IS the child graph's checkpoint
    /// thread, because ADK's `GraphAgent::run` builds its `ExecutionConfig`
    /// from `ctx.session_id()` and that thread is what the pause is bound to.
    /// The branch is the SAME nested-application tier an agent child gets, and
    /// it is load-bearing rather than cosmetic (#990 review 1): ADK's
    /// `event_belongs_to_branch` treats an EMPTY branch as visible to every
    /// branch, so a child event persisted without one is re-read into the
    /// ORDINARY parent's own conversation on its next turn — the child's
    /// monologue becomes the parent's, and an unmatched `tool_use` makes an
    /// Anthropic-shaped provider refuse the turn outright. The root agent runs
    /// on `APPLICATION_BRANCH_ROOT`, which does not see a deeper tier, so the
    /// child is filtered out exactly as an agent child is. The PROJECTOR
    /// clears it again before the descendant projector sees the event, because
    /// a graph interrupt is projected as the root of its own projector.
    pub(super) fn for_pipeline(
        parent_ctx: Arc<dyn ToolContext>,
        agent: Arc<dyn Agent>,
        user_content: Content,
        invocation_id: String,
        checkpoint_thread_id: String,
    ) -> Self {
        let branch = nested_application_branch(parent_ctx.branch());
        Self {
            session: ApplicationToolSession::new(
                checkpoint_thread_id,
                parent_ctx.app_name().to_owned(),
                parent_ctx.user_id().to_owned(),
                Vec::new(),
                HashMap::new(),
            ),
            parent_ctx,
            agent,
            user_content,
            invocation_id,
            branch,
            run_config: application_run_config(),
            ended: AtomicBool::new(false),
        }
    }

    fn with_resume(
        parent_ctx: Arc<dyn ToolContext>,
        agent: Arc<dyn Agent>,
        user_content: Content,
        mut run_config: RunConfig,
        mut history: Vec<Content>,
    ) -> Self {
        // Match Runner's current-input history boundary. LlmAgent replaces the last
        // user entry with user_content; on resume that entry must be the private
        // replay marker, not the original child task which the model still needs.
        history.push(user_content.clone());
        // The tool returns one complete child result. A direct replay supplies an
        // SSE config for root callers; retain its decisions but use the same
        // accumulated-event contract as a fresh child, not its final token delta.
        run_config.streaming_mode = StreamingMode::None;
        // Every child activation, including each resume of the same parent call, needs an id no
        // other activation in the session ever had. The persisted history joins a child's events
        // by this id across turns, and later turns may run in a replacement Worker process or on
        // another Worker, so a per-process counter would hand out ids already persisted.
        let invocation_id = format!("elitea-child-{}", uuid::Uuid::new_v4());
        let branch = nested_application_branch(parent_ctx.branch());
        let instruction_session_id = format!(
            "elitea-child-{}",
            super::instruction_authority::content_digest(&format!(
                "{}:{}:{}",
                parent_ctx.session_id(),
                parent_ctx.function_call_id(),
                agent.name()
            ))
        );
        let instruction_state = parent_ctx.session().map_or_else(HashMap::new, |session| {
            super::instruction_authority::state_for_child(
                session.state(),
                &instruction_session_id,
                agent.name(),
            )
        });
        Self {
            session: ApplicationToolSession::new(
                instruction_session_id,
                parent_ctx.app_name().to_owned(),
                parent_ctx.user_id().to_owned(),
                history,
                instruction_state,
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

    fn state(&self) -> Option<&dyn State> {
        Some(&self.session)
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
    fn new(
        id: String,
        app_name: String,
        user_id: String,
        history: Vec<Content>,
        instruction_state: HashMap<String, Value>,
    ) -> Self {
        Self {
            id,
            app_name,
            user_id,
            state: std::sync::RwLock::new(instruction_state),
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

pub(super) fn tool_input_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "elitea_nested_agent.invalid_task",
        "the nested agent task is invalid",
    )
}

pub(super) fn application_event_channel_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::Internal,
        "elitea_nested_agent.event_channel_unavailable",
        "the nested agent event channel is unavailable",
    )
}

pub(super) fn child_execution_error() -> AdkError {
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
        McpMaterializationErrorCode::UnsupportedAuthority
        | McpMaterializationErrorCode::RetiredSseEndpoint => {
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
    let message = if error.code() == McpMaterializationErrorCode::RetiredSseEndpoint {
        crate::toolkits::RETIRED_SSE_MESSAGE
    } else {
        "the nested application MCP toolsets are unavailable"
    };
    // #982: the requirement travels with the error so the lifecycle can name
    // the toolkit that challenged instead of failing the turn anonymously.
    NativeAgentAssemblyError::new(code, message).with_authorization(error.authorization().cloned())
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

pub(crate) const AMBIGUOUS_CHILD_INVOCATION_CODE: &str =
    "nested_application.ambiguous_child_invocation";

/// One child invocation id persisted under two different parent calls. Its history cannot be
/// joined to either call, so the continuation fails closed with its own typed reason. Histories
/// written before child ids were unique across processes can still carry such a collision.
fn ambiguous_child_invocation() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "a nested application invocation is bound to two parent calls",
    )
    .with_cause(AMBIGUOUS_CHILD_INVOCATION_CODE, None)
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

    // These fixtures exercise only the retained envelope's local integrity checks.
    // Typed checkpoint-family/scope validation is separately required before projection.
    fn retained_pipeline_shape_fixture() -> RetainedPipelinePause {
        let original_call = application_call_event(
            "pipeline-batch",
            "parent-invocation",
            APPLICATION_BRANCH_ROOT,
            "pipeline-call",
            "elitea_agent_41_v_51",
        );
        RetainedPipelinePause {
            schema: APPLICATION_RETAINED_PIPELINE_KEY.to_owned(),
            batch_event_id: original_call.id.clone(),
            ordinal: 1,
            parent_call_id: "pipeline-call".to_owned(),
            tool_name: "elitea_agent_41_v_51".to_owned(),
            arguments_digest: application_arguments_digest(original_call.tool_calls()[0].args)
                .unwrap(),
            checkpoint_thread_id: "pipeline-thread".to_owned(),
            interrupt_ids: BTreeSet::from(["interrupt-one".to_owned(), "interrupt-two".to_owned()]),
            original_call,
            events: vec![Event::with_id("scope-event", "scope-invocation")],
        }
    }

    fn retained_pipeline_probe_resume() -> ChildApplicationResume {
        let pause = retained_pipeline_shape_fixture();
        ChildApplicationResume {
            batch_event_id: pause.batch_event_id.clone(),
            tool_name: pause.tool_name.clone(),
            arguments: pause.original_call.tool_calls()[0].args.clone(),
            ordinal: pause.ordinal,
            history: Vec::new(),
            action: ChildApplicationResumeAction::RetainedPipeline(Box::new(pause)),
        }
    }

    #[tokio::test]
    async fn retained_pipeline_resume_probe_keeps_exact_parent_route_without_consuming() {
        let coordinator = ApplicationResumeCoordinator::default();
        coordinator
            .install_root(HashMap::from([(
                "pipeline-call".to_owned(),
                retained_pipeline_probe_resume(),
            )]))
            .await
            .unwrap();
        coordinator
            .install_children(
                "nested-owner".to_owned(),
                HashMap::from([("nested-call".to_owned(), retained_pipeline_probe_resume())]),
            )
            .await
            .unwrap();
        assert!(
            !coordinator
                .has_resume("nested-owner", "pipeline-call")
                .await
        );
        assert!(coordinator.has_resume("nested-owner", "nested-call").await);
        assert!(coordinator.has_resume("nested-owner", "nested-call").await);
        assert!(coordinator.has_resume("root-owner", "pipeline-call").await);
        assert!(coordinator.has_resume("root-owner", "pipeline-call").await);
        assert!(
            coordinator
                .take("root-owner", "pipeline-call")
                .await
                .unwrap()
                .is_some()
        );
        assert!(!coordinator.has_resume("root-owner", "pipeline-call").await);
        assert!(coordinator.has_resume("nested-owner", "nested-call").await);
    }

    #[test]
    fn retained_pipeline_shape_keeps_original_batch_arguments_and_ordinal() {
        let pause = retained_pipeline_shape_fixture();
        pause.validate_basic().unwrap();
        // A structurally valid local fixture has no checkpoint family authority.
        assert!(pause.validate().is_err());

        let mut edited_args = retained_pipeline_shape_fixture();
        edited_args.arguments_digest =
            application_arguments_digest(&json!({"task":"changed"})).unwrap();
        assert!(edited_args.validate_basic().is_err());
        let mut edited_ordinal = retained_pipeline_shape_fixture();
        edited_ordinal.ordinal = 2;
        assert!(edited_ordinal.validate_basic().is_err());
        let mut edited_batch = retained_pipeline_shape_fixture();
        edited_batch.batch_event_id = "other-batch".to_owned();
        assert!(edited_batch.validate_basic().is_err());
        let mut projected_proof = retained_pipeline_shape_fixture();
        projected_proof
            .events
            .push(projected_proof.original_call.clone());
        assert!(projected_proof.validate_basic().is_err());
    }

    #[test]
    fn retained_pipeline_codec_denies_duplicate_leaf_ids_and_v1_payload_confusion() {
        let mut encoded = serde_json::to_value(retained_pipeline_shape_fixture()).unwrap();
        encoded["interrupt_ids"] = json!(["interrupt-one", "interrupt-one"]);
        assert!(serde_json::from_value::<RetainedPipelinePause>(encoded).is_err());
        let pipeline_encoded = serde_json::to_string(&retained_pipeline_shape_fixture()).unwrap();
        assert!(serde_json::from_str::<RetainedApplicationPause>(&pipeline_encoded).is_err());
        let ordinary = RetainedApplicationPause {
            interrupt_id: "interrupt-one".to_owned(),
            parent_call_id: "pipeline-call".to_owned(),
            events: vec![Event::with_id(
                "ordinary-confirmation",
                "ordinary-invocation",
            )],
        };
        assert!(
            serde_json::from_value::<RetainedPipelinePause>(
                serde_json::to_value(ordinary).unwrap(),
            )
            .is_err()
        );
        let mut mixed = Event::new("replay-container");
        mixed.provider_metadata.insert(
            APPLICATION_RETAINED_PIPELINE_KEY.to_owned(),
            pipeline_encoded,
        );
        mixed
            .provider_metadata
            .insert(APPLICATION_RETAINED_PAUSE_KEY.to_owned(), "{}".to_owned());
        assert!(retained_application_events(&mixed).is_err());
    }

    #[test]
    fn retained_pipeline_shape_excludes_completed_results_and_duplicate_events() {
        let mut completed = retained_pipeline_shape_fixture();
        completed.events[0].llm_response.content = Some(Content {
            role: "tool".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: adk_rust::FunctionResponseData::new(
                    "completed-tool",
                    json!({"response":"already done"}),
                ),
                id: Some("completed-call".to_owned()),
                annotations: None,
            }],
        });
        assert!(completed.validate_basic().is_err());
        let mut duplicate = retained_pipeline_shape_fixture();
        duplicate.events.push(duplicate.events[0].clone());
        assert!(duplicate.validate_basic().is_err());
    }

    #[test]
    fn retained_pipeline_shape_enforces_combined_proof_and_family_bounds() {
        let mut oversized = retained_pipeline_shape_fixture();
        oversized.events[0].provider_metadata.insert(
            "bounded-family-fixture".to_owned(),
            "x".repeat(MAX_PIPELINE_TOOL_PENDING_BYTES),
        );
        assert!(oversized.validate_basic().is_err());
        let mut too_many = retained_pipeline_shape_fixture();
        too_many.events = (0..=MAX_RETAINED_PIPELINE_EVENTS)
            .map(|index| Event::with_id(format!("event-{index}"), "scope-invocation"))
            .collect();
        assert!(too_many.validate_basic().is_err());
        let mut too_many_ids = serde_json::to_value(retained_pipeline_shape_fixture()).unwrap();
        too_many_ids["interrupt_ids"] = json!(
            (0..=MAX_PIPELINE_APPLICATION_SCOPE_DECISIONS)
                .map(|index| format!("interrupt-{index}"))
                .collect::<Vec<_>>()
        );
        assert!(serde_json::from_value::<RetainedPipelinePause>(too_many_ids).is_err());
    }

    #[test]
    fn child_resume_task_accepts_bounded_variable_arguments_but_pipeline_api_stays_task_only() {
        let args = json!({"task":"continue", "audience":"operators"});
        assert_eq!(application_task_with_variables(&args).unwrap(), "continue");
        assert!(application_task(&args).is_err());
        assert!(application_task_with_variables(&json!({"task":""})).is_err());
        assert!(
            application_task_with_variables(
                &json!({"task":"work", "audience":"x".repeat(MAX_APPLICATION_TASK_BYTES)})
            )
            .is_err()
        );
    }

    #[test]
    fn child_fatal_channel_preserves_incomplete_continuation() {
        let error = application_signal_event(ApplicationEventSignal::Fatal(
            ApplicationEventFailure::OutputContinuation(
                crate::agents::model_checkpoint::output::ContinuationFailure::CallLimit,
            ),
        ))
        .unwrap_err();
        assert_eq!(error.code, "model.output_continuation_failed");
        assert!(matches!(
            crate::agents::model_checkpoint::output::failure_reason(&error),
            Some(crate::agents::model_checkpoint::output::ContinuationFailure::CallLimit)
        ));
    }

    #[test]
    fn child_continuation_is_an_error_report_not_a_successful_answer() {
        let error = crate::agents::model_checkpoint::output::failed(
            crate::agents::model_checkpoint::output::ContinuationFailure::CallLimit,
        );
        let report = child_failure_report(&error, Some("accepted prefix".into()), false).unwrap();
        assert!(report.get("response").is_none());
        assert!(report["error"].is_string());
        assert_eq!(report["failure"]["retryable"], false);
        assert_eq!(report["failure"]["recoverable"], true);
        assert_eq!(report["failure"]["partial_output"], "accepted prefix");
        assert_eq!(report["failure"]["partial_output_available"], true);
        assert_eq!(report["failure"]["code"], "OUTPUT_CONTINUATION_EXHAUSTED");
    }

    #[test]
    fn pipeline_agent_node_continuation_failure_cannot_become_tool_data() {
        let error = crate::agents::model_checkpoint::output::failed(
            crate::agents::model_checkpoint::output::ContinuationFailure::CallLimit,
        );
        assert!(child_failure_report(&error, Some("incomplete".into()), true).is_none());
    }

    #[test]
    fn child_control_failures_do_not_become_recoverable_reports() {
        for code in [
            "cancelled",
            "tool.authorization_required",
            "runtime.invalid_state",
        ] {
            let error = AdkError::new(
                ErrorComponent::Agent,
                ErrorCategory::Internal,
                code,
                "PRIVATE_PROVIDER_BODY",
            );
            assert!(child_failure_report(&error, None, false).is_none());
        }
        let error = AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Internal,
            "model.output_continuation_failed",
            "PRIVATE_PROVIDER_BODY",
        );
        let report = child_failure_report(&error, None, false).unwrap();
        assert!(!report.to_string().contains("PRIVATE_PROVIDER_BODY"));
        assert_eq!(report["failure"]["partial_output_available"], false);
    }

    #[test]
    fn known_child_model_failures_preserve_policy_without_provider_text() {
        for (code, public_code, retryable, recovery) in [
            (
                "model_gateway.rate_limited",
                "MODEL_RATE_LIMITED",
                true,
                "verify_before_retry",
            ),
            (
                "model_gateway.forbidden",
                "MODEL_ACCESS_DENIED",
                false,
                "ask_administrator",
            ),
            (
                "model_gateway.budget_exhausted",
                "MODEL_BUDGET_EXHAUSTED",
                false,
                "ask_administrator",
            ),
            (
                "context_budget_exceeded",
                "CONTEXT_BUDGET_EXCEEDED",
                false,
                "revise_task",
            ),
            (
                "pipeline.input_limit",
                "PIPELINE_INPUT_LIMIT",
                false,
                "revise_task",
            ),
            (
                "pipeline.result_invalid",
                "PIPELINE_RESULT_INVALID",
                false,
                "revise_task",
            ),
            (
                "anthropic_gateway.provider_error",
                "MODEL_PROVIDER_FAILURE",
                true,
                "verify_before_retry",
            ),
        ] {
            let error = AdkError::new(
                ErrorComponent::Model,
                ErrorCategory::Internal,
                code,
                "SECRET_PROVIDER_BODY",
            );
            let report = child_failure_report(&error, Some("partial".into()), false).unwrap();
            assert_eq!(report["failure"]["code"], public_code);
            assert_eq!(report["failure"]["retryable"], retryable);
            assert_eq!(report["failure"]["recovery_action"], recovery);
            assert!(report.get("response").is_none());
            assert!(!report.to_string().contains("SECRET_PROVIDER_BODY"));
            assert!(child_failure_report(&error, None, true).is_none());
            let terminal = application_signal_event(ApplicationEventSignal::Fatal(
                ApplicationEventFailure::Model(code),
            ))
            .unwrap_err();
            assert_eq!(terminal.code, code);
            assert!(!terminal.to_string().contains("SECRET_PROVIDER_BODY"));
        }
    }

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

    /// One agent child persisted under a parent call, then a later turn's child under its own
    /// re-persisted parent call (pipeline nodes re-persist their call each turn).
    fn child_under_turn(turn: &str, call_event_id: &str, child_invocation: &str) -> [Event; 2] {
        let call = application_call_event(
            call_event_id,
            turn,
            APPLICATION_BRANCH_ROOT,
            "pipeline:override_call:0",
            "elitea_agent_31_v_41",
        );
        let mut child = Event::with_id(&format!("{call_event_id}-child"), child_invocation);
        child.branch = format!("{APPLICATION_BRANCH_ROOT}.application_1");
        child.llm_response.content = Some(Content::new("model").with_text("child turn"));
        child.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            turn.to_owned(),
        );
        child.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            "pipeline:override_call:0".to_owned(),
        );
        [call, child]
    }

    #[test]
    fn a_child_invocation_id_under_two_parent_calls_fails_closed_with_its_reason() {
        let mut distinct = child_under_turn("turn-1", "call-turn-1", "elitea-child-a").to_vec();
        distinct.extend(child_under_turn("turn-2", "call-turn-2", "elitea-child-b"));
        assert_eq!(
            application_resume_history(&distinct, "elitea-child-b")
                .expect("distinct child ids each keep their own parent-call route")
                .len(),
            1
        );

        // A Worker that named children from a per-process counter reused `elitea-child-1` for
        // the resumed child after a restart: one id under two persisted parent-call events.
        let mut reused = child_under_turn("turn-1", "call-turn-1", "elitea-child-1").to_vec();
        reused.extend(child_under_turn("turn-2", "call-turn-2", "elitea-child-1"));
        let Err(error) = application_resume_history(&reused, "elitea-child-2") else {
            panic!("a reused child invocation id must fail closed");
        };
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        );
        assert_eq!(
            error.cause().map(|cause| cause.code()),
            Some(AMBIGUOUS_CHILD_INVOCATION_CODE)
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

#[cfg(test)]
mod pipeline_call_lineage_transport_tests {
    use super::*;
    #[test]
    fn same_bounded_replay_receipt_survives_descendant_transport_and_mismatches_fail_closed() {
        let batch = ApplicationReplayBatch {
            event_id: "original-batch".to_owned(),
            interrupt_ids: BTreeSet::from(["leaf-1".to_owned()]),
            call_ordinals: BTreeMap::from([("call-1".to_owned(), 2)]),
        };
        let mut event = Event::new("replay-runtime");
        event.llm_response.provider_metadata = Some(
            json!({"elitea.application.replay_batch.v1":serde_json::to_value(&batch).unwrap()}),
        );
        assert_eq!(
            original_application_batch_id(&event).unwrap(),
            "original-batch"
        );
        assert_eq!(
            application_replay_ordinal(&event, "call-1").unwrap(),
            Some(2)
        );
        event.provider_metadata.insert(
            ADK_LLM_RESPONSE_METADATA_KEY.to_owned(),
            serde_json::to_string(&event.llm_response).unwrap(),
        );
        assert_eq!(
            original_application_batch_id(&event).unwrap(),
            "original-batch"
        );
        let mut changed = event.llm_response.clone();
        changed.provider_metadata.as_mut().unwrap()[APPLICATION_REPLAY_BATCH_KEY]["event_id"] =
            json!("changed");
        event.provider_metadata.insert(
            ADK_LLM_RESPONSE_METADATA_KEY.to_owned(),
            serde_json::to_string(&changed).unwrap(),
        );
        assert!(original_application_batch_id(&event).is_err());
        event.provider_metadata.insert(
            ADK_LLM_RESPONSE_METADATA_KEY.to_owned(),
            "malformed".to_owned(),
        );
        assert!(original_application_batch_id(&event).is_err());
        event
            .provider_metadata
            .remove(ADK_LLM_RESPONSE_METADATA_KEY);
        event.llm_response.provider_metadata.as_mut().unwrap()[APPLICATION_REPLAY_BATCH_KEY]["call_ordinals"]
            ["call-1"] = json!(0);
        assert!(original_application_batch_id(&event).is_err());
    }
}
