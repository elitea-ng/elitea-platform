//! Invocation-local event bridge for native pipeline LLM nodes.
//!
//! `GraphAgent` owns checkpointed state and emits the selected terminal result,
//! while an `LlmAgent` node owns the model/tool loop. ADK intentionally does not
//! retain those inner events in the graph checkpoint. This bounded side channel
//! forwards them to the outer Runner stream without putting browser progress or
//! provider request payloads in graph state.

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::{
    AdkError, Agent, Content, ErrorCategory, ErrorComponent, Event, EventStream,
    FunctionResponseData, InvocationContext, Part,
};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::{Mutex, mpsc};

use super::yaml::valid_graph_id;
use crate::agents::events::{
    DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY,
};

const PIPELINE_NODE_EVENT_CHANNEL_CAPACITY: usize = 64;
const ADK_LLM_REQUEST_METADATA_KEY: &str = "gcp.vertex.agent.llm_request";
const ADK_LLM_RESPONSE_METADATA_KEY: &str = "gcp.vertex.agent.llm_response";

/// Stable private marker consumed by the Elitea browser event projector.
pub(crate) const PIPELINE_NODE_METADATA_KEY: &str = "elitea.pipeline.node_name";
/// Private graph channel used only because ADK `SubgraphNode` does not carry
/// the parent's invocation metadata into the child graph.
pub(crate) const PIPELINE_NODE_EVENT_SCOPE_STATE_KEY: &str =
    "__elitea_pipeline_node_event_scope_v1";
pub(crate) const PIPELINE_NODE_EVENT_SCOPE_WRAPPER_KEY: &str = "elitea_event_scope";
const MAX_EVENT_SCOPE_IDENTITY_BYTES: usize = 480;

enum PipelineNodeEventSignal {
    Event(PipelineNodeEventData),
    OutputContinuationFailed,
}

struct PipelineNodeEventData {
    node_name: Option<String>,
    scope: Option<PipelineNodeEventScope>,
    event: Box<Event>,
}

/// The already-public Application invocation identity to apply to child
/// pipeline node events. This is an internal transport value; browser output
/// remains the existing `parent_agent_*` contract produced by the projector.
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineNodeEventScope {
    parent_call_id: String,
    agent_name: String,
    checkpoint_thread_id: String,
}

impl PipelineNodeEventScope {
    pub(crate) fn new(
        parent_call_id: &str,
        agent_name: &str,
        checkpoint_thread_id: &str,
    ) -> adk_rust::Result<Self> {
        let scope = Self {
            parent_call_id: parent_call_id.to_owned(),
            agent_name: agent_name.to_owned(),
            checkpoint_thread_id: checkpoint_thread_id.to_owned(),
        };
        scope.validate()?;
        Ok(scope)
    }

    pub(crate) fn from_state(value: Option<&Value>) -> adk_rust::Result<Option<Self>> {
        value
            .filter(|value| !value.is_null())
            .map(|value| {
                serde_json::from_value::<Self>(value.clone())
                    .map_err(|_| pipeline_node_event_channel_error())
                    .and_then(|scope| {
                        scope.validate()?;
                        Ok(scope)
                    })
            })
            .transpose()
    }

    pub(crate) fn to_state_value(&self) -> adk_rust::Result<Value> {
        self.validate()?;
        serde_json::to_value(self).map_err(|_| pipeline_node_event_channel_error())
    }

    pub(crate) fn validate(&self) -> adk_rust::Result<()> {
        if valid_event_identity(&self.parent_call_id)
            && valid_event_identity(&self.agent_name)
            && valid_event_identity(&self.checkpoint_thread_id)
        {
            Ok(())
        } else {
            Err(pipeline_node_event_channel_error())
        }
    }

    #[must_use]
    pub(crate) fn parent_call_id(&self) -> &str {
        &self.parent_call_id
    }

    #[must_use]
    pub(crate) fn agent_name(&self) -> &str {
        &self.agent_name
    }

    #[must_use]
    pub(crate) fn checkpoint_thread_id(&self) -> &str {
        &self.checkpoint_thread_id
    }
}

#[derive(Clone)]
pub(crate) struct PipelineNodeEventSender {
    inner: mpsc::Sender<PipelineNodeEventSignal>,
}

#[derive(Clone)]
pub(crate) struct PipelineNodeEventReceiver {
    inner: Arc<Mutex<Option<mpsc::Receiver<PipelineNodeEventSignal>>>>,
}

#[must_use]
pub(crate) fn pipeline_node_event_channel() -> (PipelineNodeEventSender, PipelineNodeEventReceiver)
{
    let (sender, receiver) = mpsc::channel(PIPELINE_NODE_EVENT_CHANNEL_CAPACITY);
    (
        PipelineNodeEventSender { inner: sender },
        PipelineNodeEventReceiver {
            inner: Arc::new(Mutex::new(Some(receiver))),
        },
    )
}

impl PipelineNodeEventSender {
    /// Preserve the registered continuation failure across ADK graph error wrapping.
    pub(crate) async fn send_output_continuation_failure(&self) -> adk_rust::Result<()> {
        self.inner
            .send(PipelineNodeEventSignal::OutputContinuationFailed)
            .await
            .map_err(|_| pipeline_node_event_channel_error())
    }

    /// Reuse the invocation-owned bridge for before-model progress. The outer
    /// wrapper supplies the agent identity; graph forwarding adds node scope.
    pub(crate) async fn send_context_status(&self, event: Event) -> adk_rust::Result<()> {
        if crate::agents::context_status::ModelContextStatus::from_event(&event)?.is_none() {
            return Err(pipeline_node_event_channel_error());
        }
        self.inner
            .send(PipelineNodeEventSignal::Event(PipelineNodeEventData {
                node_name: None,
                scope: None,
                event: Box::new(event),
            }))
            .await
            .map_err(|_| pipeline_node_event_channel_error())
    }

    /// Forward one ordinary model/tool event. Confirmations stay owned by the
    /// graph interrupt, and incremental tool-progress events remain closed
    /// until the public projection has a bounded schema for them.
    pub(crate) async fn send(
        &self,
        node_name: &str,
        scope: Option<&PipelineNodeEventScope>,
        mut event: Event,
    ) -> adk_rust::Result<()> {
        if !valid_graph_id(node_name)
            || event.actions.tool_confirmation.is_some()
            || event.tool_progress_stream().is_some()
            || event
                .provider_metadata
                .contains_key(PIPELINE_NODE_METADATA_KEY)
        {
            return Err(pipeline_node_event_channel_error());
        }
        event.llm_request = None;
        event.provider_metadata.remove(ADK_LLM_REQUEST_METADATA_KEY);
        event
            .provider_metadata
            .remove(ADK_LLM_RESPONSE_METADATA_KEY);
        self.inner
            .send(PipelineNodeEventSignal::Event(PipelineNodeEventData {
                node_name: Some(node_name.to_owned()),
                scope: scope.cloned(),
                event: Box::new(event),
            }))
            .await
            .map_err(|_| pipeline_node_event_channel_error())
    }

    pub(crate) async fn send_application_start(
        &self,
        tool_name: &str,
        call_id: &str,
    ) -> adk_rust::Result<()> {
        let mut event = Event::new("pipeline_application_started");
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: tool_name.to_owned(),
                args: json!({}),
                id: Some(call_id.to_owned()),
                thought_signature: None,
            }],
        });
        self.send_application_event(tool_name, call_id, event).await
    }

    pub(crate) async fn send_application_end(
        &self,
        tool_name: &str,
        call_id: &str,
    ) -> adk_rust::Result<()> {
        let mut event = Event::new("pipeline_application_completed");
        event.llm_response.content = Some(Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: FunctionResponseData::new(
                    tool_name,
                    json!({"response": "Pipeline completed."}),
                ),
                id: Some(call_id.to_owned()),
                annotations: None,
            }],
        });
        self.send_application_event(tool_name, call_id, event).await
    }

    async fn send_application_event(
        &self,
        tool_name: &str,
        call_id: &str,
        event: Event,
    ) -> adk_rust::Result<()> {
        if !valid_event_identity(tool_name) || !valid_event_identity(call_id) {
            return Err(pipeline_node_event_channel_error());
        }
        self.inner
            .send(PipelineNodeEventSignal::Event(PipelineNodeEventData {
                node_name: None,
                scope: None,
                event: Box::new(event),
            }))
            .await
            .map_err(|_| pipeline_node_event_channel_error())
    }
}

pub(crate) struct PipelineNodeEventStreamingAgent {
    inner: Arc<dyn Agent>,
    events: PipelineNodeEventReceiver,
}

impl PipelineNodeEventStreamingAgent {
    #[must_use]
    pub(crate) fn new(inner: Arc<dyn Agent>, events: PipelineNodeEventReceiver) -> Self {
        Self { inner, events }
    }
}

#[async_trait]
impl Agent for PipelineNodeEventStreamingAgent {
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
        let mut node_events = self
            .events
            .inner
            .lock()
            .await
            .take()
            .ok_or_else(pipeline_node_event_channel_error)?;
        let root_invocation_id = ctx.invocation_id().to_owned();
        let root_author = ctx.agent_name().to_owned();
        let root_branch = ctx.branch().to_owned();
        let mut root_events = self.inner.run(ctx).await?;
        let stream = async_stream::stream! {
            let mut node_events_open = true;
            loop {
                tokio::select! {
                    biased;
                    signal = node_events.recv(), if node_events_open => {
                        match signal {
                            Some(signal) => {
                                match pipeline_node_signal_event(
                                    signal,
                                    &root_invocation_id,
                                    &root_author,
                                    &root_branch,
                                ) {
                                    Ok(event) => yield Ok(event),
                                    Err(error) => {
                                        yield Err(error);
                                        return;
                                    }
                                }
                            }
                            None => node_events_open = false,
                        }
                    }
                    event = root_events.next() => {
                        if let Some(event) = event {
                            while let Ok(signal) = node_events.try_recv() {
                                match pipeline_node_signal_event(
                                    signal,
                                    &root_invocation_id,
                                    &root_author,
                                    &root_branch,
                                ) {
                                    Ok(event) => yield Ok(event),
                                    Err(error) => {
                                        yield Err(error);
                                        return;
                                    }
                                }
                            }
                            yield event;
                        } else {
                            while let Ok(signal) = node_events.try_recv() {
                                match pipeline_node_signal_event(
                                    signal,
                                    &root_invocation_id,
                                    &root_author,
                                    &root_branch,
                                ) {
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

fn pipeline_node_signal_event(
    signal: PipelineNodeEventSignal,
    root_invocation_id: &str,
    root_author: &str,
    root_branch: &str,
) -> adk_rust::Result<Event> {
    let signal = match signal {
        PipelineNodeEventSignal::Event(signal) => signal,
        PipelineNodeEventSignal::OutputContinuationFailed => {
            return Err(crate::agents::model_checkpoint::output::exhausted());
        }
    };
    let mut event = *signal.event;
    if let Some(node_name) = signal.node_name
        && (!valid_graph_id(&node_name)
            || event
                .provider_metadata
                .insert(PIPELINE_NODE_METADATA_KEY.to_owned(), node_name)
                .is_some())
    {
        return Err(pipeline_node_event_channel_error());
    }
    if let Some(scope) = signal.scope {
        scope.validate()?;
        if event
            .provider_metadata
            .insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                root_invocation_id.to_owned(),
            )
            .is_some()
            || event
                .provider_metadata
                .insert(
                    DESCENDANT_PARENT_CALL_KEY.to_owned(),
                    scope.parent_call_id.clone(),
                )
                .is_some()
        {
            return Err(pipeline_node_event_channel_error());
        }
        event.invocation_id = format!("pipeline-child:{}", scope.parent_call_id);
        event.author = scope.agent_name;
        event.branch.clear();
        if event
            .provider_metadata
            .insert(
                DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                scope.checkpoint_thread_id,
            )
            .is_some()
        {
            return Err(pipeline_node_event_channel_error());
        }
    } else {
        root_invocation_id.clone_into(&mut event.invocation_id);
        root_author.clone_into(&mut event.author);
        root_branch.clone_into(&mut event.branch);
    }
    Ok(event)
}

fn valid_event_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_EVENT_SCOPE_IDENTITY_BYTES
        && !value.chars().any(char::is_control)
}

fn pipeline_node_event_channel_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Agent,
        ErrorCategory::Internal,
        "elitea_pipeline.node_event_channel_unavailable",
        "the pipeline node event channel is unavailable",
    )
}
