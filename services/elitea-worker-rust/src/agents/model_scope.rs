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
use tokio::sync::OnceCell;

use super::context_budget::ModelRequestBudget;
use super::context_compaction::DurableContextCompaction;
use super::context_management::ContextCompactionPlan;
use super::model_checkpoint::ModelCheckpointWriter;

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
        }
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
}

impl ScopedModelCheckpoint {
    pub(super) fn with_replay_pending(self: Arc<Self>, pending: bool) -> Arc<Self> {
        self.skip_replay_model.store(pending, Ordering::Release);
        self
    }
    async fn writer(&self, context: &dyn ReadonlyContext) -> adk_rust::Result<&ScopedWriter> {
        let identity = self.storage.identity(context)?;
        let writer = self
            .writer
            .get_or_try_init(|| async {
                let sessions: Arc<dyn SessionService> =
                    Arc::new(super::session::RunnerSessionService::new(
                        self.storage.sessions.open(&identity).await?,
                        self.completion.clone(),
                    ));
                let session = match sessions
                    .get(GetRequest {
                        app_name: identity.app_name.to_string(),
                        user_id: identity.user_id.to_string(),
                        session_id: identity.session_id.to_string(),
                        num_recent_events: None,
                        after: None,
                    })
                    .await
                {
                    Ok(session) => session,
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
                        sessions
                            .create(CreateRequest {
                                app_name: identity.app_name.to_string(),
                                user_id: identity.user_id.to_string(),
                                session_id: Some(identity.session_id.to_string()),
                                state,
                            })
                            .await?
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
                Ok(ScopedWriter {
                    identity: identity.clone(),
                    sessions,
                    checkpoint,
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
            .before_model(writer.identity.clone(), context.invocation_id(), request)
            .await
    }

    pub(super) fn bind(self: Arc<Self>, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        let model = self.clone();
        builder
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
