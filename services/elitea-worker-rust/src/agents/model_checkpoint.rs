//! Persist the model/tool boundary before ADK starts the corresponding operation.

use std::sync::Arc;

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::session::{AppendEventRequest, SessionService};
use adk_rust::{AdkIdentity, BeforeModelResult, Event, LlmRequest};
use serde::Serialize;
use serde_json::{Value, json};

pub(super) const CHECKPOINT_KEY: &str = "elitea.agent.recovery.v1";

#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "snake_case")]
enum Phase {
    ModelPending,
    ToolMayHaveStarted,
}

#[derive(Clone)]
pub(super) struct ModelCheckpointWriter {
    sessions: Arc<dyn SessionService>,
    execution_id: String,
    generation: u64,
    definition_digest: [u8; 32],
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
        }
    }

    pub(super) fn bind(self, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        let model = self.clone();
        builder
            .before_model_callback(Box::new(move |context, request| {
                let writer = model.clone();
                Box::pin(async move {
                    writer
                        .persist(
                            context.try_identity()?,
                            context.invocation_id(),
                            Phase::ModelPending,
                            Some(&request),
                        )
                        .await?;
                    Ok(BeforeModelResult::Continue(request))
                })
            }))
            .before_tool_callback(Box::new(move |context| {
                let writer = self.clone();
                Box::pin(async move {
                    // A pending model checkpoint must stop authorizing replay
                    // before any tool can cross its invocation boundary.
                    writer
                        .persist(
                            context.try_identity()?,
                            context.invocation_id(),
                            Phase::ToolMayHaveStarted,
                            None,
                        )
                        .await?;
                    Ok(None)
                })
            }))
    }

    async fn persist(
        &self,
        identity: AdkIdentity,
        invocation_id: &str,
        phase: Phase,
        request: Option<&LlmRequest>,
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
        // The existing session implementation enforces claim fencing and size
        // limits. A failed write stops ADK before the provider/tool call.
        self.sessions
            .append_event_for_identity(AppendEventRequest { identity, event })
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_rust::futures::StreamExt as _;
    use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService};
    use adk_rust::{Content, Llm, LlmResponse, LlmResponseStream, Part, Tool, ToolContext};
    use async_trait::async_trait;
    use std::sync::atomic::{AtomicUsize, Ordering};

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
}
