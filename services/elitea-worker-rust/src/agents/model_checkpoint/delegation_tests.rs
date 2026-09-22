use super::*;
use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::StreamExt as _;
use adk_rust::session::{CreateRequest, InMemorySessionService, Session, SessionService};
use adk_rust::{Tool, ToolContext};
use serde_json::Value;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

struct Probe {
    models: AtomicUsize,
    calls: AtomicUsize,
    block: AtomicBool,
    entered: tokio::sync::Notify,
}

fn calls() -> Content {
    Content {
        role: "model".into(),
        parts: vec![Part::FunctionCall {
            name: "saved_agent".into(),
            args: json!({"task":"Inspect Cedar"}),
            id: Some("stable-child-call".into()),
            thought_signature: None,
        }],
    }
}

#[async_trait]
impl Llm for Probe {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let count = self.models.fetch_add(1, Ordering::SeqCst);
        let content = if count == 0 {
            calls()
        } else {
            assert_eq!(count, 1, "recovery must not select another child");
            assert!(
                !serde_json::to_string(&request)
                    .unwrap()
                    .contains("RECOVERY_INPUT")
            );
            let results = request.contents.iter().flat_map(|content| &content.parts).filter(|part| {
                matches!(part, Part::FunctionResponse { id: Some(id), .. } if id == "stable-child-call")
            }).count();
            assert_eq!(results, 1);
            Content::new("model").with_text("Parent received the child result")
        };
        Ok(Box::pin(adk_rust::futures::stream::once(async {
            Ok(LlmResponse::new(content))
        })))
    }
}

#[async_trait]
impl Tool for Probe {
    fn name(&self) -> &'static str {
        "saved_agent"
    }
    fn description(&self) -> &'static str {
        "Saved child fixture"
    }
    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_rust::Result<Value> {
        assert_eq!(ctx.function_call_id(), "stable-child-call");
        assert_eq!(args, json!({"task":"Inspect Cedar"}));
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.entered.notify_one();
        if self.block.load(Ordering::SeqCst) {
            std::future::pending::<()>().await;
        }
        Ok(json!({"response":"Saved child result"}))
    }
}

async fn session(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .get(GetRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: "parent".into(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap()
}

fn runner(
    writer: ModelCheckpointWriter,
    probe: Arc<Probe>,
    sessions: Arc<dyn SessionService>,
) -> adk_rust::runner::Runner {
    let model = writer.delegation_model(probe.clone());
    let agent = writer
        .bind(LlmAgentBuilder::new("parent").model(model).tool(probe))
        .build()
        .unwrap();
    adk_rust::runner::Runner::builder()
        .app_name("delegation-test")
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .unwrap()
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One interruption story checks authority and exact call recovery.
async fn interrupted_parent_replays_exact_saved_child_call_before_requesting_final_answer() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "delegation-test".into(),
            user_id: "user".into(),
            session_id: Some("parent".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let probe = Arc::new(Probe {
        models: AtomicUsize::new(0),
        calls: AtomicUsize::new(0),
        block: AtomicBool::new(true),
        entered: tokio::sync::Notify::new(),
    });
    let writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 1, [1; 32])
        .with_application_tools(["saved_agent"].into_iter());
    let first = runner(writer.clone(), probe.clone(), sessions.clone());
    let task = tokio::spawn(async move {
        let mut events = first
            .run(
                "user".try_into().unwrap(),
                "parent".try_into().unwrap(),
                Content::new("user").with_text("Delegate the inspection"),
            )
            .await
            .unwrap();
        while let Some(event) = events.next().await {
            event.unwrap();
        }
    });
    tokio::time::timeout(std::time::Duration::from_secs(5), probe.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    let stored = session(sessions.as_ref()).await;
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["phase"],
        "delegation_pending"
    );
    let value = stored.state().get(CHECKPOINT_KEY).unwrap();
    let snapshot: Checkpoint = serde_json::from_value(value).unwrap();
    let mut saved = snapshot.model.unwrap();
    saved.request.tools = saved.tools;
    let removed = writer
        .clone()
        .with_application_tools(std::iter::empty())
        .restore(stored.as_ref())
        .unwrap();
    assert!(
        removed
            .before_model(
                stored.try_identity().unwrap(),
                "replacement",
                "parent",
                saved.request.clone()
            )
            .await
            .is_err()
    );
    let changed = writer.clone().restore(stored.as_ref()).unwrap();
    let mut changed_request = saved.request;
    changed_request.tools.clear();
    assert!(
        changed
            .before_model(
                stored.try_identity().unwrap(),
                "replacement",
                "parent",
                changed_request
            )
            .await
            .is_err()
    );
    let restored = writer.restore(stored.as_ref()).unwrap();
    assert!(restored.validated_checkpoint().is_some());
    probe.block.store(false, Ordering::SeqCst);
    let replacement = runner(restored, probe.clone(), sessions);
    let mut events = replacement
        .run(
            "user".try_into().unwrap(),
            "parent".try_into().unwrap(),
            Content::new("user").with_text("RECOVERY_INPUT"),
        )
        .await
        .unwrap();
    let mut final_text = false;
    while let Some(event) = events.next().await {
        final_text |= serde_json::to_string(&event.unwrap())
            .unwrap()
            .contains("Parent received the child result");
    }
    assert!(final_text);
    assert_eq!(probe.models.load(Ordering::SeqCst), 2);
    assert_eq!(
        probe.calls.load(Ordering::SeqCst),
        2,
        "dispatch resumes the same child identity"
    );
}

#[test]
fn delegation_validation_rejects_mixed_tools_and_duplicate_call_ids() {
    let mut content = calls();
    assert!(validate_calls(&content, |name| name == "saved_agent").is_ok());
    content.parts.push(content.parts[0].clone());
    assert!(validate_calls(&content, |_| true).is_err());
    assert!(validate_calls(&calls(), |_| false).is_err());
    let mut mixed = calls();
    mixed.parts.push(Part::FunctionCall {
        name: "ordinary_effect".into(),
        args: json!({}),
        id: Some("different-call".into()),
        thought_signature: None,
    });
    assert!(validate_calls(&mixed, |name| name == "saved_agent").is_err());
}
