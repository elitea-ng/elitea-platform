//! Reuse the claim-fenced session service for independent child model histories.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::StreamExt as _;
use adk_rust::session::{AppendEventRequest, CreateRequest, GetRequest, SessionService};
use adk_rust::{
    AdkError, AdkIdentity, Agent, BeforeModelResult, Content, Event, EventStream,
    InvocationContext, Llm, LlmRequest, ReadonlyContext,
};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use tokio::sync::OnceCell;

use super::context_budget::ModelRequestBudget;
use super::context_compaction::DurableContextCompaction;
use super::context_management::ContextCompactionPlan;
use super::model_checkpoint::ModelCheckpointWriter;

const COMPLETION_KEY: &str = "elitea.model.completion.v1";

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ModelCompletion {
    version: u8,
    execution_id: String,
    generation: u64,
    definition_digest: [u8; 32],
    agent_name: String,
    event_id: String,
    invocation_id: String,
}

fn successful_terminal(event: &Event, agent_name: &str) -> bool {
    event.author == agent_name
        && event.llm_response.turn_complete
        && event.is_final_response()
        && !event.llm_response.partial
        && !event.llm_response.interrupted
        && event.llm_response.error_code.is_none()
        && event.llm_response.error_message.is_none()
        && event.actions.tool_confirmation.is_none()
        && event.long_running_tool_ids.is_empty()
        && event.llm_response.content.as_ref().is_none_or(|content| {
            !content.parts.iter().any(|part| {
                matches!(
                    part,
                    adk_rust::Part::FunctionCall { .. } | adk_rust::Part::FunctionResponse { .. }
                )
            })
        })
}

#[derive(Clone)]
pub(super) enum ModelScopeBackend {
    Local(Arc<dyn SessionService>),
    Postgres(Arc<crate::state::PostgresSessionService>),
}

impl ModelScopeBackend {
    async fn open(&self, identity: &AdkIdentity) -> adk_rust::Result<Arc<dyn SessionService>> {
        match self {
            Self::Local(service) => Ok(service.clone()),
            Self::Postgres(root) => root
                .model_scope(identity)
                .await
                .map(|service| Arc::new(service) as Arc<dyn SessionService>)
                .map_err(crate::state::PostgresSessionError::into_adk),
        }
    }
}

/// Shared storage and claim identity, never a shared child history or summary.
#[derive(Clone)]
pub(super) struct ModelScopeSessions {
    sessions: ModelScopeBackend,
    execution_id: String,
    generation: u64,
    definition_digest: [u8; 32],
    node_scope: Option<String>,
    recover_pending_models: bool,
}

impl ModelScopeSessions {
    pub(super) fn new(
        sessions: ModelScopeBackend,
        execution_id: String,
        generation: u64,
        definition_digest: [u8; 32],
    ) -> Self {
        Self {
            sessions,
            execution_id,
            generation,
            definition_digest,
            node_scope: None,
            recover_pending_models: false,
        }
    }

    /// Enable restoration only after root checkpoint recovery is authorized.
    pub(super) fn with_pending_model_recovery(mut self) -> Self {
        self.recover_pending_models = true;
        self
    }

    pub(super) fn for_node(&self, identity: &str) -> Self {
        let mut scopes = self.clone();
        scopes.node_scope = Some(identity.to_owned());
        scopes
    }

    fn identity(&self, context: &dyn ReadonlyContext) -> adk_rust::Result<AdkIdentity> {
        let mut identity = context.try_identity()?;
        // Existing child session identity already contains parent, call, and
        // saved version identity. Separate executions even if a provider reuses a call ID.
        let source = serde_json::to_string(&(
            "elitea.model.scope.v1",
            &self.execution_id,
            self.generation,
            context.session_id(),
            context.agent_name(),
        ))
        .map_err(|_| invalid_scope())?;
        let source = if let Some(node_scope) = &self.node_scope {
            serde_json::to_string(&(source, node_scope)).map_err(|_| invalid_scope())?
        } else {
            source
        };
        identity.session_id = format!(
            "elitea-model-{}",
            super::instruction_authority::content_digest(&source)
        )
        .try_into()
        .map_err(|_| invalid_scope())?;
        Ok(identity)
    }

    pub(super) fn checkpoint(
        &self,
        plan: ContextCompactionPlan,
        budget: Arc<dyn ModelRequestBudget>,
        model: Arc<dyn Llm>,
        replay_marker: Option<Content>,
        completion: Option<Arc<dyn super::session::DurableModelCompletion>>,
    ) -> Arc<ScopedModelCheckpoint> {
        let (context_events, context_receiver) = super::graph::pipeline_node_event_channel();
        Arc::new(ScopedModelCheckpoint {
            storage: self.clone(),
            plan,
            budget,
            model,
            writer: OnceCell::new(),
            skip_replay_model: AtomicBool::new(replay_marker.is_some()),
            replay_marker,
            completion,
            context_events,
            context_receiver,
        })
    }
}

/// One instance per child invocation. Repeated calls do not share mutable state.
pub(super) struct ScopedModelCheckpoint {
    storage: ModelScopeSessions,
    plan: ContextCompactionPlan,
    budget: Arc<dyn ModelRequestBudget>,
    model: Arc<dyn Llm>,
    writer: OnceCell<ScopedWriter>,
    skip_replay_model: AtomicBool,
    replay_marker: Option<Content>,
    completion: Option<Arc<dyn super::session::DurableModelCompletion>>,
    context_events: super::graph::PipelineNodeEventSender,
    context_receiver: super::graph::PipelineNodeEventReceiver,
}

struct ScopedWriter {
    identity: AdkIdentity,
    sessions: Arc<dyn SessionService>,
    checkpoint: ModelCheckpointWriter,
    completed: Option<Content>,
}

impl ScopedModelCheckpoint {
    fn completed_content(
        &self,
        session: &dyn adk_rust::session::Session,
        agent_name: &str,
    ) -> adk_rust::Result<Option<Content>> {
        let Some(value) = session.state().get(COMPLETION_KEY) else {
            return Ok(None);
        };
        let receipt: ModelCompletion =
            serde_json::from_value(value.clone()).map_err(|_| invalid_scope())?;
        if receipt.version != 1
            || receipt.execution_id != self.storage.execution_id
            || receipt.generation != self.storage.generation
            || receipt.definition_digest != self.storage.definition_digest
            || receipt.agent_name != agent_name
        {
            return Err(invalid_scope());
        }
        let events = session.events().all();
        let index = events
            .iter()
            .rposition(|event| {
                event.id == receipt.event_id
                    && event.invocation_id == receipt.invocation_id
                    && event.actions.state_delta.get(COMPLETION_KEY) == Some(&value)
            })
            .ok_or_else(invalid_scope)?;
        // A later request supersedes this receipt. It must recover its own boundary.
        if events[index + 1..].iter().any(|event| {
            event
                .actions
                .state_delta
                .contains_key(super::model_checkpoint::CHECKPOINT_KEY)
                || event.llm_response.content.is_some()
        }) {
            return Ok(None);
        }
        let event = &events[index];
        if !successful_terminal(event, agent_name) {
            return Err(invalid_scope());
        }
        let content = event
            .llm_response
            .content
            .as_ref()
            .ok_or_else(invalid_scope)?;
        if content.role != "model" || content.parts.is_empty() {
            return Err(invalid_scope());
        }
        Ok(Some(content.clone()))
    }

    async fn completed_event(
        &self,
        context: &dyn ReadonlyContext,
    ) -> adk_rust::Result<Option<Event>> {
        let writer = self.writer(context).await?;
        Ok(writer.completed.as_ref().map(|content| {
            // A receipt is delivery evidence, not a second provider call or state update.
            let mut event = Event::new(context.invocation_id());
            context.agent_name().clone_into(&mut event.author);
            context.branch().clone_into(&mut event.branch);
            event.set_content(content.clone());
            event.llm_response.turn_complete = true;
            event
        }))
    }

    pub(super) fn with_replay_pending(self: Arc<Self>, pending: bool) -> Arc<Self> {
        self.skip_replay_model.store(pending, Ordering::Release);
        self
    }
    async fn writer(&self, context: &dyn ReadonlyContext) -> adk_rust::Result<&ScopedWriter> {
        let identity = self.storage.identity(context)?;
        let writer = self
            .writer
            .get_or_try_init(|| async {
                let sessions: Arc<dyn SessionService> = Arc::new(
                    super::session::RunnerSessionService::new(
                        self.storage.sessions.open(&identity).await?,
                        self.completion.clone(),
                    )
                    .with_history_scope(self.storage.definition_digest, context.agent_name()),
                );
                let (session, existing) = match sessions
                    .get(GetRequest {
                        app_name: identity.app_name.to_string(),
                        user_id: identity.user_id.to_string(),
                        session_id: identity.session_id.to_string(),
                        num_recent_events: None,
                        after: None,
                    })
                    .await
                {
                    Ok(session) => (session, true),
                    Err(error) if error.code == "session.not_found" => {
                        let state = context.state().map_or_else(Default::default, |state| {
                            super::instruction_authority::state_for_child(
                                state,
                                context.session_id(),
                                context.agent_name(),
                            )
                        });
                        if !state.is_empty()
                            && !super::instruction_authority::valid_state_delta(&state)
                        {
                            return Err(invalid_scope());
                        }
                        let session = sessions
                            .create(CreateRequest {
                                app_name: identity.app_name.to_string(),
                                user_id: identity.user_id.to_string(),
                                session_id: Some(identity.session_id.to_string()),
                                state,
                            })
                            .await?;
                        (session, false)
                    }
                    Err(error) => return Err(error),
                };
                let compaction = Arc::new(DurableContextCompaction::new(
                    self.plan.clone(),
                    self.budget.clone(),
                    self.model.clone(),
                    self.storage.definition_digest,
                    session.as_ref(),
                )?);
                // Loading a summary does not grant permission to replay a child tool.
                // Parent recovery must authorize its own pending call separately.
                let checkpoint = ModelCheckpointWriter::new(
                    sessions.clone(),
                    self.storage.execution_id.clone(),
                    self.storage.generation,
                    self.storage.definition_digest,
                )
                .with_request_budget(Some(self.budget.clone()))
                .with_context_events(self.context_events.clone())
                .with_context_compaction(Some(compaction));
                let recovering =
                    existing && self.storage.recover_pending_models && self.replay_marker.is_none();
                let completed = if recovering {
                    self.completed_content(session.as_ref(), context.agent_name())?
                } else {
                    None
                };
                let checkpoint = if recovering && completed.is_none() {
                    checkpoint.restore(session.as_ref())?
                } else {
                    checkpoint
                };
                Ok(ScopedWriter {
                    identity: identity.clone(),
                    sessions,
                    checkpoint,
                    completed,
                })
            })
            .await?;
        if writer.identity != identity {
            return Err(invalid_scope());
        }
        Ok(writer)
    }

    async fn append(
        &self,
        context: &dyn ReadonlyContext,
        mut event: Event,
    ) -> adk_rust::Result<()> {
        // Match ADK Runner persistence: streamed deltas share the terminal ID.
        // The existing completion adapter enriches only the stored terminal copy.
        if event.llm_response.partial {
            return Ok(());
        }
        let writer = self.writer(context).await?;
        if successful_terminal(&event, context.agent_name()) {
            let receipt = ModelCompletion {
                version: 1,
                execution_id: self.storage.execution_id.clone(),
                generation: self.storage.generation,
                definition_digest: self.storage.definition_digest,
                agent_name: context.agent_name().to_owned(),
                event_id: event.id.clone(),
                invocation_id: event.invocation_id.clone(),
            };
            event.actions.state_delta.insert(
                COMPLETION_KEY.to_owned(),
                serde_json::to_value(receipt).map_err(|_| invalid_scope())?,
            );
        }
        event.llm_request = None;
        event
            .provider_metadata
            .remove("gcp.vertex.agent.llm_request");
        event
            .provider_metadata
            .remove("gcp.vertex.agent.llm_response");
        writer
            .sessions
            .append_event_for_identity(AppendEventRequest {
                identity: writer.identity.clone(),
                event,
            })
            .await
    }

    pub(super) fn wrap(self: Arc<Self>, inner: Arc<dyn Agent>) -> Arc<dyn Agent> {
        Arc::new(ModelScopeAgent {
            inner: Arc::new(super::graph::PipelineNodeEventStreamingAgent::new(
                inner,
                self.context_receiver.clone(),
            )),
            checkpoint: self,
        })
    }

    async fn before_model(
        &self,
        context: &dyn ReadonlyContext,
        request: LlmRequest,
    ) -> adk_rust::Result<BeforeModelResult> {
        let writer = self.writer(context).await?;
        // A resumed guard emits a deterministic pending call first.
        // Only subsequent requests reach the provider and need compaction.
        if self.skip_replay_model.swap(false, Ordering::AcqRel) {
            return Ok(BeforeModelResult::Continue(request));
        }
        let request = if let Some(marker) = &self.replay_marker {
            super::replay_history::model_continuation(request, marker)?
        } else {
            request
        };
        writer
            .checkpoint
            .before_model(
                writer.identity.clone(),
                context.invocation_id(),
                context.agent_name(),
                request,
            )
            .await
    }

    pub(super) fn bind(self: Arc<Self>, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        let model = self.clone();
        builder
            .retain_prepared_history(true)
            .before_model_callback(Box::new(move |context, request| {
                let scope = model.clone();
                Box::pin(async move { scope.before_model(context.as_ref(), request).await })
            }))
            .before_tool_callback(Box::new(move |context| {
                let scope = self.clone();
                Box::pin(async move {
                    let writer = scope.writer(context.as_ref()).await?;
                    writer
                        .checkpoint
                        .before_tool(writer.identity.clone(), context.invocation_id())
                        .await?;
                    Ok(None)
                })
            }))
    }
}

struct ModelScopeAgent {
    inner: Arc<dyn Agent>,
    checkpoint: Arc<ScopedModelCheckpoint>,
}

#[async_trait]
impl Agent for ModelScopeAgent {
    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        self.inner.sub_agents()
    }
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        if let Some(event) = self.checkpoint.completed_event(ctx.as_ref()).await? {
            return Ok(Box::pin(adk_rust::futures::stream::once(async {
                Ok(event)
            })));
        }
        let mut stream = self.inner.run(ctx.clone()).await?;
        let checkpoint = self.checkpoint.clone();
        Ok(Box::pin(async_stream::try_stream! {
            while let Some(event) = stream.next().await {
                let event = event?;
                checkpoint.append(ctx.as_ref(), event.clone()).await?;
                yield event;
            }
        }))
    }
}

fn invalid_scope() -> AdkError {
    AdkError::session("The child model checkpoint identity is invalid.")
}

#[cfg(test)]
#[path = "model_scope_tests.rs"]
mod tests;
