//! Persist the model/tool boundary before ADK starts the corresponding operation.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::session::{AppendEventRequest, Session, SessionService};
use adk_rust::{
    AdkError, AdkIdentity, BeforeModelResult, ErrorCategory, ErrorComponent, Event, LlmRequest,
};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

pub(super) const CHECKPOINT_KEY: &str = "elitea.agent.recovery.v1";
pub(super) const HISTORY_SNAPSHOT_KEY: &str = "elitea.agent.history_snapshot.v1";

#[derive(Clone, Copy, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
enum Phase {
    ContextPending,
    ModelPending,
    ToolMayHaveStarted,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Checkpoint {
    version: u8,
    execution_id: String,
    generation: u64,
    definition_digest: [u8; 32],
    invocation_id: String,
    phase: Phase,
    model: Option<ModelSnapshot>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ModelSnapshot {
    request: LlmRequest,
    tools: HashMap<String, Value>,
}

/// Read model history without granting permission to replay an operation.
pub(super) fn history_contents(
    event: &Event,
    definition: [u8; 32],
    agent_name: &str,
) -> adk_rust::Result<Vec<adk_rust::Content>> {
    let descriptor = event.actions.state_delta.get(HISTORY_SNAPSHOT_KEY);
    let checkpoint = event
        .actions
        .state_delta
        .get(CHECKPOINT_KEY)
        .ok_or_else(invalid_checkpoint)?;
    let checkpoint: Checkpoint =
        serde_json::from_value(checkpoint.clone()).map_err(|_| invalid_checkpoint())?;
    if descriptor.is_none_or(|value| {
        value["version"] != 1 || value["agent_name"].as_str() != Some(agent_name)
    }) || checkpoint.version != 1
        || checkpoint.definition_digest != definition
        || checkpoint.invocation_id != event.invocation_id
        || !matches!(checkpoint.phase, Phase::ModelPending)
    {
        return Err(invalid_checkpoint());
    }
    let model = checkpoint.model.ok_or_else(invalid_checkpoint)?;
    if model.request.contents.is_empty() {
        return Err(invalid_checkpoint());
    }
    // Current instructions are bound separately. A saved request cannot restore
    // obsolete system instructions through the conversation-history channel.
    Ok(model
        .request
        .contents
        .into_iter()
        .filter(|content| content.role != "system")
        .collect())
}

/// Opaque evidence produced only after durable checkpoint validation.
/// It carries no model input, credential, tool result, or session contents.
#[derive(Clone)]
pub(crate) struct ValidatedModelCheckpoint {
    execution_id: String,
    generation: u64,
    digest: [u8; 32],
}

impl ValidatedModelCheckpoint {
    pub(crate) fn matches_checkpoint(&self, other: &Self) -> bool {
        self.execution_id == other.execution_id
            && self.generation == other.generation
            && self.digest == other.digest
    }

    #[cfg(test)]
    pub(crate) fn test_evidence(execution_id: String, generation: u64) -> Self {
        Self {
            execution_id,
            generation,
            digest: [42; 32],
        }
    }

    pub(crate) fn matches_execution(&self, execution_id: &str, generation: u64) -> bool {
        self.execution_id == execution_id && self.generation == generation
    }

    pub(crate) const fn digest(&self) -> [u8; 32] {
        self.digest
    }
}

#[derive(Clone)]
pub(super) struct ModelCheckpointWriter {
    sessions: Arc<dyn SessionService>,
    execution_id: String,
    generation: u64,
    definition_digest: [u8; 32],
    replay: Option<Arc<Mutex<Option<LlmRequest>>>>,
    validated_digest: Option<[u8; 32]>,
    request_budget: Option<Arc<dyn super::context_budget::ModelRequestBudget>>,
    context_compaction: Option<Arc<super::context_compaction::DurableContextCompaction>>,
    context_events: Option<super::graph::PipelineNodeEventSender>,
}

impl ModelCheckpointWriter {
    pub(super) fn new(
        sessions: Arc<dyn SessionService>,
        execution_id: String,
        generation: u64,
        definition_digest: [u8; 32],
    ) -> Self {
        Self {
            sessions,
            execution_id,
            generation,
            definition_digest,
            replay: None,
            validated_digest: None,
            request_budget: None,
            context_compaction: None,
            context_events: None,
        }
    }

    pub(super) fn with_context_events(
        mut self,
        events: super::graph::PipelineNodeEventSender,
    ) -> Self {
        self.context_events = Some(events);
        self
    }

    async fn emit_context_status(
        &self,
        phase: super::context_status::ContextPhase,
        usage: super::context_budget::RequestContextUsage,
    ) -> adk_rust::Result<()> {
        if let Some(events) = &self.context_events {
            events
                .send_context_status(
                    super::context_status::ModelContextStatus::new(phase, usage).event()?,
                )
                .await?;
        }
        Ok(())
    }

    pub(super) fn with_context_compaction(
        mut self,
        compaction: Option<Arc<super::context_compaction::DurableContextCompaction>>,
    ) -> Self {
        self.context_compaction = compaction;
        self
    }

    pub(super) fn with_request_budget(
        mut self,
        budget: Option<Arc<dyn super::context_budget::ModelRequestBudget>>,
    ) -> Self {
        self.request_budget = budget;
        self
    }

    /// Restore only an unfinished model step from this exact execution.
    /// A tool boundary and a persisted model result are not replay permission.
    pub(super) fn restore(self, session: &dyn Session) -> adk_rust::Result<Self> {
        let allow_context_preparation = self.context_compaction.is_some();
        self.restore_validated(session, allow_context_preparation)
    }

    /// Validate evidence before credential redemption, without constructing a model.
    /// This returns no executable writer; restoration still requires a compactor.
    pub(super) fn inspect(
        self,
        session: &dyn Session,
        allow_context_preparation: bool,
    ) -> adk_rust::Result<Option<ValidatedModelCheckpoint>> {
        Ok(self
            .restore_validated(session, allow_context_preparation)?
            .validated_checkpoint())
    }

    fn restore_validated(
        mut self,
        session: &dyn Session,
        allow_context_preparation: bool,
    ) -> adk_rust::Result<Self> {
        let Some(value) = session.state().get(CHECKPOINT_KEY) else {
            return Ok(self);
        };
        // A new user turn shares a session but owns a different execution.
        if value.get("execution_id").and_then(Value::as_str) != Some(self.execution_id.as_str()) {
            return Ok(self);
        }
        let checkpoint: Checkpoint =
            serde_json::from_value(value.clone()).map_err(|_| invalid_checkpoint())?;
        if checkpoint.version != 1
            || checkpoint.execution_id != self.execution_id
            || checkpoint.generation != self.generation
            || checkpoint.definition_digest != self.definition_digest
            || checkpoint.invocation_id.is_empty()
            || checkpoint.invocation_id.len() > 256
        {
            return Err(invalid_checkpoint());
        }
        if !matches!(
            checkpoint.phase,
            Phase::ModelPending | Phase::ContextPending
        ) || matches!(checkpoint.phase, Phase::ContextPending) && !allow_context_preparation
        {
            return Err(invalid_checkpoint());
        }
        // The marker and state commit together. Refuse a missing marker or a
        // completed model event after it; those cannot prove an unfinished step.
        let events = session.events().all();
        let marker = events
            .iter()
            .rposition(|event| {
                event.author == "elitea-recovery"
                    && event.invocation_id == checkpoint.invocation_id
                    && event.actions.state_delta.get(CHECKPOINT_KEY) == Some(&value)
            })
            .ok_or_else(invalid_checkpoint)?;
        if events[marker + 1..].iter().any(|event| {
            event.invocation_id == checkpoint.invocation_id
                && !event.llm_response.partial
                && event
                    .llm_response
                    .content
                    .as_ref()
                    .is_some_and(|content| !content.parts.is_empty())
        }) {
            return Err(invalid_checkpoint());
        }
        let Some(mut model) = checkpoint.model else {
            return Err(invalid_checkpoint());
        };
        if model.request.contents.is_empty() {
            return Err(invalid_checkpoint());
        }
        let encoded = serde_json::to_vec(&value).map_err(|_| invalid_checkpoint())?;
        self.validated_digest = Some(
            digest::digest(&digest::SHA256, &encoded)
                .as_ref()
                .try_into()
                .map_err(|_| invalid_checkpoint())?,
        );
        model.request.tools = model.tools;
        self.replay = Some(Arc::new(Mutex::new(Some(model.request))));
        Ok(self)
    }

    #[allow(dead_code)] // Consumed by the recovery coordinator integration.
    pub(super) fn validated_checkpoint(&self) -> Option<ValidatedModelCheckpoint> {
        self.validated_digest
            .map(|digest| ValidatedModelCheckpoint {
                execution_id: self.execution_id.clone(),
                generation: self.generation,
                digest,
            })
    }

    pub(super) fn is_recovery(&self) -> bool {
        self.replay.is_some()
    }

    fn prepare_request(&self, mut request: LlmRequest) -> adk_rust::Result<LlmRequest> {
        if let Some(replay) = &self.replay {
            let mut pending = replay.lock().map_err(|_| invalid_checkpoint())?;
            if let Some(saved) = pending.as_ref() {
                // Restored tools must still exist with the same declarations.
                // Credentials and runtime tool objects come from fresh binding.
                if saved
                    .tools
                    .iter()
                    .any(|(name, schema)| request.tools.get(name) != Some(schema))
                {
                    return Err(invalid_checkpoint());
                }
                let mut saved = pending.take().ok_or_else(invalid_checkpoint)?;
                // Older checkpoints were written before instruction rehydration.
                // Only the authority callback creates system contents here;
                // client history rejects them and static instructions are bound
                // separately by the provider adapter. Keep a prepared snapshot
                // exact; repair the old shape from the restored authority state.
                if !saved
                    .contents
                    .iter()
                    .any(|content| content.role == "system")
                {
                    saved.contents.splice(
                        0..0,
                        request
                            .contents
                            .into_iter()
                            .filter(|content| content.role == "system"),
                    );
                }
                return Ok(saved);
            }
            request
                .contents
                .retain(|content| content.role != "user" || !content.parts.is_empty());
        }
        Ok(request)
    }

    pub(super) fn bind(self, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        let model = self.clone();
        builder
            .retain_prepared_history(self.context_compaction.is_some())
            .before_model_callback(Box::new(move |context, request| {
                let writer = model.clone();
                Box::pin(async move {
                    writer
                        .before_model(
                            context.try_identity()?,
                            context.invocation_id(),
                            context.agent_name(),
                            request,
                        )
                        .await
                })
            }))
            .before_tool_callback(Box::new(move |context| {
                let writer = self.clone();
                Box::pin(async move {
                    writer
                        .before_tool(context.try_identity()?, context.invocation_id())
                        .await?;
                    Ok(None)
                })
            }))
    }

    pub(super) async fn before_model(
        &self,
        identity: AdkIdentity,
        invocation_id: &str,
        agent_name: &str,
        request: LlmRequest,
    ) -> adk_rust::Result<BeforeModelResult> {
        let request = self.prepare_request(request)?;
        let mut compacted = false;
        let (request, record) = if let Some(compaction) = &self.context_compaction {
            let request = super::replay_history::model_history(request)?;
            let source = request.clone();
            let pending_identity = identity.clone();
            let was_compacted = &mut compacted;
            compaction
                .prepare(request, |usage| async move {
                    self.persist(
                        pending_identity,
                        invocation_id,
                        Phase::ContextPending,
                        Some(&source),
                        None,
                        None,
                    )
                    .await?;
                    *was_compacted = true;
                    self.emit_context_status(super::context_status::ContextPhase::Compacting, usage)
                        .await
                })
                .await?
        } else {
            (request, None)
        };
        let usage = if let Some(budget) = &self.request_budget {
            let provider_request = super::replay_history::model_history(request.clone())?;
            let usage = budget.measure(&provider_request)?;
            usage.check()?;
            Some(usage)
        } else {
            None
        };
        self.persist(
            identity,
            invocation_id,
            Phase::ModelPending,
            Some(&request),
            self.context_compaction
                .as_ref()
                .map(|_| {
                    super::context_compaction::DurableContextCompaction::state_value(
                        record.as_ref(),
                    )
                })
                .transpose()?,
            self.context_compaction.as_ref().map(|_| agent_name),
        )
        .await?;
        if let Some(compaction) = &self.context_compaction {
            compaction.committed(record)?;
        }
        if let Some(usage) = usage {
            self.emit_context_status(
                if compacted {
                    super::context_status::ContextPhase::Compacted
                } else {
                    super::context_status::ContextPhase::Measured
                },
                usage,
            )
            .await?;
        }
        Ok(BeforeModelResult::Continue(request))
    }

    pub(super) async fn before_tool(
        &self,
        identity: AdkIdentity,
        invocation_id: &str,
    ) -> adk_rust::Result<()> {
        // Stop authorizing model replay before any tool crosses its boundary.
        self.persist(
            identity,
            invocation_id,
            Phase::ToolMayHaveStarted,
            None,
            None,
            None,
        )
        .await
    }

    async fn persist(
        &self,
        identity: AdkIdentity,
        invocation_id: &str,
        phase: Phase,
        request: Option<&LlmRequest>,
        context_state: Option<Value>,
        history_agent: Option<&str>,
    ) -> adk_rust::Result<()> {
        let mut event = Event::new(invocation_id);
        "elitea-recovery".clone_into(&mut event.author);
        // ADK deliberately omits `tools` from LlmRequest serialization.
        // Preserve declarations explicitly so a recovered request retains them.
        let model = request.map(|request| json!({"request": request, "tools": request.tools}));
        let checkpoint: Value = json!({
            "version": 1,
            "execution_id": self.execution_id,
            "generation": self.generation,
            "definition_digest": self.definition_digest,
            "invocation_id": invocation_id,
            "phase": phase,
            "model": model,
        });
        event
            .actions
            .state_delta
            .insert(CHECKPOINT_KEY.to_owned(), checkpoint);
        if let Some(agent_name) = history_agent {
            event.actions.state_delta.insert(
                HISTORY_SNAPSHOT_KEY.to_owned(),
                json!({"version": 1, "agent_name": agent_name}),
            );
        }
        if let Some(value) = context_state {
            event
                .actions
                .state_delta
                .insert(super::context_compaction::STATE_KEY.to_owned(), value);
        }
        // The existing session implementation enforces claim fencing and size
        // limits. A failed write stops ADK before the provider/tool call.
        self.sessions
            .append_event_for_identity(AppendEventRequest { identity, event })
            .await
    }
}

fn invalid_checkpoint() -> AdkError {
    AdkError::new(
        ErrorComponent::Session,
        ErrorCategory::InvalidInput,
        "agent_recovery.invalid_checkpoint",
        "the checkpoint does not authorize model continuation",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_rust::futures::StreamExt as _;
    use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService};
    use adk_rust::{Content, Llm, LlmResponse, LlmResponseStream, Part, Tool, ToolContext};
    use async_trait::async_trait;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[tokio::test]
    async fn prepared_and_legacy_checkpoints_restore_one_authoritative_instruction_block() {
        for prepared in [false, true] {
            let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
            let session = sessions
                .create(CreateRequest {
                    app_name: "checkpoint-test".into(),
                    user_id: "user".into(),
                    session_id: Some("session".into()),
                    state: HashMap::new(),
                })
                .await
                .expect("session");
            let writer =
                ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32]);
            let mut saved = LlmRequest::new(
                "model",
                vec![Content::new("user").with_text("original task")],
            );
            if prepared {
                saved
                    .contents
                    .insert(0, Content::new("system").with_text("checkpoint authority"));
            }
            writer
                .persist(
                    session.try_identity().expect("identity"),
                    "invocation",
                    Phase::ModelPending,
                    Some(&saved),
                    None,
                    None,
                )
                .await
                .expect("persist");
            let stored = sessions
                .get(GetRequest {
                    app_name: "checkpoint-test".into(),
                    user_id: "user".into(),
                    session_id: "session".into(),
                    num_recent_events: None,
                    after: None,
                })
                .await
                .expect("stored");
            let replay = writer.restore(stored.as_ref()).expect("restore");
            let current = LlmRequest::new(
                "model",
                vec![
                    Content::new("system").with_text("restored session authority"),
                    Content::new("user").with_text("fresh ADK history"),
                ],
            );
            let restored = replay.prepare_request(current).expect("prepare replay");
            let expected = if prepared {
                "checkpoint authority"
            } else {
                "restored session authority"
            };
            assert_eq!(
                serde_json::to_value(&restored.contents).expect("JSON"),
                json!([
                    Content::new("system").with_text(expected),
                    Content::new("user").with_text("original task"),
                ])
            );
        }
    }

    async fn checkpoint(sessions: &dyn SessionService) -> Value {
        sessions
            .get(GetRequest {
                app_name: "checkpoint-test".into(),
                user_id: "user".into(),
                session_id: "session".into(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("session")
            .state()
            .get(CHECKPOINT_KEY)
            .expect("checkpoint")
    }

    struct Probe {
        sessions: Arc<dyn SessionService>,
        models: AtomicUsize,
        tools: AtomicUsize,
    }

    #[async_trait]
    impl Llm for Probe {
        fn name(&self) -> &'static str {
            "checkpoint-probe"
        }
        async fn generate_content(
            &self,
            request: LlmRequest,
            _: bool,
        ) -> adk_rust::Result<LlmResponseStream> {
            let stored = checkpoint(self.sessions.as_ref()).await;
            assert_eq!(stored["phase"], "model_pending");
            assert_eq!(stored["execution_id"], "execution");
            assert_eq!(stored["generation"], 7);
            assert_eq!(stored["definition_digest"], json!(vec![7; 32]));
            assert_eq!(stored["model"]["tools"], json!(request.tools));
            assert!(!request.tools.is_empty());
            let response = if self.models.fetch_add(1, Ordering::SeqCst) == 0 {
                LlmResponse::new(Content {
                    role: "model".into(),
                    parts: vec![Part::FunctionCall {
                        name: "probe".into(),
                        args: json!({}),
                        id: Some("call-1".into()),
                        thought_signature: None,
                    }],
                })
            } else {
                assert!(request.contents.iter().any(|c| {
                    c.parts
                        .iter()
                        .any(|p| matches!(p, Part::FunctionResponse { .. }))
                }));
                LlmResponse::new(Content::new("model").with_text("done"))
            };
            Ok(Box::pin(adk_rust::futures::stream::once(async {
                Ok(response)
            })))
        }
    }

    #[async_trait]
    impl Tool for Probe {
        fn name(&self) -> &'static str {
            "probe"
        }
        fn description(&self) -> &'static str {
            "Inspect checkpoint ordering"
        }
        async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
            let stored = checkpoint(self.sessions.as_ref()).await;
            assert_eq!(stored["phase"], "tool_may_have_started");
            assert!(stored["model"].is_null());
            self.tools.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"result": "committed"}))
        }
    }

    #[tokio::test]
    async fn runner_persists_each_boundary_before_model_and_tool_invocation() {
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "checkpoint-test".into(),
                user_id: "user".into(),
                session_id: Some("session".into()),
                state: std::collections::HashMap::default(),
            })
            .await
            .expect("create");
        let probe = Arc::new(Probe {
            sessions: sessions.clone(),
            models: AtomicUsize::new(0),
            tools: AtomicUsize::new(0),
        });
        let agent = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
            .bind(
                LlmAgentBuilder::new("agent")
                    .model(probe.clone())
                    .tool(probe.clone()),
            )
            .build()
            .expect("agent");
        let runner = adk_rust::runner::Runner::builder()
            .app_name("checkpoint-test")
            .agent(Arc::new(agent))
            .session_service(sessions.clone())
            .build()
            .expect("runner");
        let mut events = runner
            .run(
                "user".try_into().expect("user"),
                "session".try_into().expect("session"),
                Content::new("user").with_text("run probe"),
            )
            .await
            .expect("run");
        while let Some(event) = events.next().await {
            event.expect("event");
        }
        assert_eq!(probe.models.load(Ordering::SeqCst), 2);
        assert_eq!(probe.tools.load(Ordering::SeqCst), 1);
    }
    #[tokio::test]
    async fn missing_checkpoint_store_prevents_model_dispatch() {
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "checkpoint-test".into(),
                user_id: "user".into(),
                session_id: Some("session".into()),
                state: std::collections::HashMap::default(),
            })
            .await
            .expect("create");
        let probe = Arc::new(Probe {
            sessions: sessions.clone(),
            models: AtomicUsize::new(0),
            tools: AtomicUsize::new(0),
        });
        let unavailable: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        let agent = ModelCheckpointWriter::new(unavailable, "execution".into(), 7, [7; 32])
            .bind(LlmAgentBuilder::new("agent").model(probe.clone()))
            .build()
            .expect("agent");
        let runner = adk_rust::runner::Runner::builder()
            .app_name("checkpoint-test")
            .agent(Arc::new(agent))
            .session_service(sessions)
            .build()
            .expect("runner");
        let mut events = runner
            .run(
                "user".try_into().expect("user"),
                "session".try_into().expect("session"),
                Content::new("user").with_text("run"),
            )
            .await
            .expect("run");
        let mut failed = false;
        while let Some(event) = events.next().await {
            failed |= event.is_err();
        }
        assert!(failed);
        assert_eq!(probe.models.load(Ordering::SeqCst), 0);
        assert_eq!(probe.tools.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn oversized_request_never_becomes_a_recoverable_model_checkpoint() {
        use super::super::context_budget::{
            ModelRequestBudget, RequestContextBudget, RequestContextUsage,
        };
        use super::super::request::ModelContextLimits;

        struct Measurement {
            byte_limit: usize,
        }
        impl ModelRequestBudget for Measurement {
            fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage> {
                let budget = RequestContextBudget::resolve(
                    Some(ModelContextLimits {
                        context_window_tokens: 8_000,
                        max_output_tokens: 4_000,
                        context_window_fallback: false,
                        max_output_fallback: false,
                        max_input_tokens: None,
                    }),
                    &serde_json::Map::new(),
                    Some(4_000),
                )
                .unwrap()
                .unwrap();
                budget.measure_provider_request(
                    &serde_json::to_vec(request).unwrap(),
                    self.byte_limit,
                )
            }
        }

        for (text, byte_limit, code) in [
            (
                "private checkpoint fixture ".repeat(1_000),
                1_048_576,
                "context_budget_exceeded",
            ),
            (
                "small request".to_owned(),
                1,
                "model_request_bytes_exceeded",
            ),
        ] {
            let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
            sessions
                .create(CreateRequest {
                    app_name: "checkpoint-test".into(),
                    user_id: "user".into(),
                    session_id: Some("session".into()),
                    state: HashMap::default(),
                })
                .await
                .unwrap();
            let probe = Arc::new(Probe {
                sessions: sessions.clone(),
                models: AtomicUsize::new(0),
                tools: AtomicUsize::new(0),
            });
            let agent =
                ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
                    .with_request_budget(Some(Arc::new(Measurement { byte_limit })))
                    .bind(LlmAgentBuilder::new("agent").model(probe.clone()))
                    .build()
                    .unwrap();
            let runner = adk_rust::runner::Runner::builder()
                .app_name("checkpoint-test")
                .agent(Arc::new(agent))
                .session_service(sessions.clone())
                .build()
                .unwrap();
            let mut events = runner
                .run(
                    "user".try_into().unwrap(),
                    "session".try_into().unwrap(),
                    Content::new("user").with_text(text),
                )
                .await
                .unwrap();
            let mut errors = Vec::new();
            while let Some(event) = events.next().await {
                if let Err(error) = event {
                    errors.push(error);
                }
            }
            assert_eq!(errors.len(), 1);
            assert_eq!(errors[0].code, code);
            assert!(!errors[0].to_string().contains("private checkpoint fixture"));
            assert_eq!(probe.models.load(Ordering::SeqCst), 0);
            let stored = sessions
                .get(GetRequest {
                    app_name: "checkpoint-test".into(),
                    user_id: "user".into(),
                    session_id: "session".into(),
                    num_recent_events: None,
                    after: None,
                })
                .await
                .unwrap();
            assert!(stored.state().get(CHECKPOINT_KEY).is_none());
        }
    }
    #[tokio::test]
    #[allow(clippy::too_many_lines)] // One ordered story checks replay permission across lifecycle transitions.
    async fn recovery_rejects_foreign_generation_definition_tools_and_completed_steps() {
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        let session = sessions
            .create(CreateRequest {
                app_name: "checkpoint-test".into(),
                user_id: "user".into(),
                session_id: Some("session".into()),
                state: HashMap::new(),
            })
            .await
            .expect("create");
        let identity = session.try_identity().expect("identity");
        let request: LlmRequest = serde_json::from_value(json!({
            "model": "model", "contents": [{"role": "user", "parts": [{"text": "original"}]}], "config": null,
        })).expect("request");
        let mut request = request;
        request
            .tools
            .insert("probe".into(), json!({"type": "object"}));
        let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32]);
        writer
            .persist(
                identity.clone(),
                "inv-1",
                Phase::ModelPending,
                Some(&request),
                None,
                None,
            )
            .await
            .expect("persist");
        let load = || {
            sessions.get(GetRequest {
                app_name: "checkpoint-test".into(),
                user_id: "user".into(),
                session_id: "session".into(),
                num_recent_events: None,
                after: None,
            })
        };
        let stored = load().await.expect("load");
        assert!(
            ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 8, [7; 32])
                .restore(stored.as_ref())
                .is_err()
        );
        assert!(
            ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [8; 32])
                .restore(stored.as_ref())
                .is_err()
        );
        assert!(writer.validated_checkpoint().is_none());
        let replay = writer.clone().restore(stored.as_ref()).expect("restore");
        let evidence = replay.validated_checkpoint().expect("validated evidence");
        assert!(evidence.matches_execution("execution", 7));
        assert!(!evidence.matches_execution("other", 7));
        assert!(!evidence.matches_execution("execution", 8));
        let persisted = stored.state().get(CHECKPOINT_KEY).expect("checkpoint");
        let encoded = serde_json::to_vec(&persisted).expect("checkpoint encoding");
        assert_eq!(
            evidence.digest().as_slice(),
            digest::digest(&digest::SHA256, &encoded).as_ref()
        );
        let mut changed = request.clone();
        changed.tools.clear();
        assert!(replay.prepare_request(changed.clone()).is_err());
        assert!(replay.prepare_request(changed).is_err());
        assert_eq!(
            replay
                .prepare_request(request.clone())
                .expect("exact")
                .tools,
            request.tools
        );
        let mut terminal = Event::new("inv-1");
        terminal.author = "agent".into();
        terminal.llm_response = LlmResponse::new(Content::new("model").with_text("done"));
        sessions
            .append_event_for_identity(AppendEventRequest {
                identity: identity.clone(),
                event: terminal,
            })
            .await
            .expect("terminal");
        assert!(
            writer
                .clone()
                .restore(load().await.expect("load").as_ref())
                .is_err()
        );
        writer
            .persist(
                identity,
                "inv-1",
                Phase::ToolMayHaveStarted,
                None,
                None,
                None,
            )
            .await
            .expect("tool marker");
        assert!(
            writer
                .restore(load().await.expect("load").as_ref())
                .is_err()
        );
    }
}
