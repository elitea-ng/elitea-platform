//! Native ADK history-retention contracts.
use adk_rust::agent::LlmAgentBuilder;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::{
    BeforeModelResult, Content, Event, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part, Tool,
    ToolContext,
};
use async_trait::async_trait;
use serde_json::{Value, json};
use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

struct HistoryProbe {
    calls: AtomicUsize,
}
#[async_trait]
impl Llm for HistoryProbe {
    fn name(&self) -> &'static str {
        "history-probe"
    }
    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let index = self.calls.fetch_add(1, Ordering::SeqCst);
        let content = if index < 4 {
            Content {
                role: "model".into(),
                parts: vec![Part::FunctionCall {
                    name: "history-probe".into(),
                    args: json!({}),
                    id: Some(format!("call-{index}")),
                    thought_signature: None,
                }],
            }
        } else {
            Content::new("model").with_text("done")
        };
        Ok(Box::pin(adk_rust::futures::stream::once(async move {
            Ok(LlmResponse::new(content))
        })))
    }
}
#[async_trait]
impl Tool for HistoryProbe {
    fn name(&self) -> &'static str {
        "history-probe"
    }
    fn description(&self) -> &'static str {
        "History retention probe"
    }
    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
        Ok(json!({"output":"x".repeat(1024)}))
    }
}
#[tokio::test]
async fn native_loop_retains_prepared_history() {
    use adk_rust::futures::StreamExt;
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "history-probe".into(),
            user_id: "user".into(),
            session_id: Some("session".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let probe = Arc::new(HistoryProbe {
        calls: AtomicUsize::new(0),
    });
    let lengths = Arc::new(Mutex::new(Vec::new()));
    let observed = lengths.clone();
    let agent = LlmAgentBuilder::new("agent")
        .retain_prepared_history(true)
        .model(probe.clone())
        .tool(probe.clone())
        .before_model_callback(Box::new(move |_, mut request| {
            let observed = observed.clone();
            Box::pin(async move {
                observed.lock().unwrap().push(request.contents.len());
                if request.contents.len() > 3 {
                    let tail = request.contents.split_off(request.contents.len() - 2);
                    request.contents.truncate(1);
                    request.contents.extend(tail);
                }
                Ok(BeforeModelResult::Continue(request))
            })
        }))
        .build()
        .unwrap();
    let runner = adk_rust::runner::Runner::builder()
        .app_name("history-probe")
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut stream = runner
        .run(
            "user".try_into().unwrap(),
            "session".try_into().unwrap(),
            Content::new("user").with_text("Run four lookups"),
        )
        .await
        .unwrap();
    while let Some(event) = stream.next().await {
        event.unwrap();
    }
    let observed = lengths.lock().unwrap();
    println!("callback input lengths: {observed:?}");
    assert_eq!(probe.calls.load(Ordering::SeqCst), 5);
    assert!(
        observed.iter().all(|count| *count <= 5),
        "discarded history grew inside the native model loop"
    );
}

struct StreamingHistoryProbe {
    maximum: Arc<AtomicUsize>,
}
#[async_trait]
impl adk_rust::Agent for StreamingHistoryProbe {
    fn name(&self) -> &'static str {
        "stream-probe"
    }
    fn description(&self) -> &'static str {
        "Stream retention probe"
    }
    fn sub_agents(&self) -> &[Arc<dyn adk_rust::Agent>] {
        &[]
    }
    async fn run(
        &self,
        ctx: Arc<dyn adk_rust::InvocationContext>,
    ) -> adk_rust::Result<adk_rust::EventStream> {
        let maximum = self.maximum.clone();
        Ok(Box::pin(adk_rust::futures::stream::unfold(
            Some((ctx, maximum, 0)),
            |state| async move {
                let (ctx, maximum, index) = state?;
                maximum.fetch_max(ctx.session().conversation_history().len(), Ordering::SeqCst);
                if index > 64 {
                    return None;
                }
                let mut event = Event::with_id("stream-result", ctx.invocation_id());
                event.author = "stream-probe".into();
                event.llm_response.partial = index < 64;
                event.set_content(Content::new("model").with_text(if index < 64 {
                    "chunk"
                } else {
                    "complete"
                }));
                Some((Ok(event), Some((ctx, maximum, index + 1))))
            },
        )))
    }
}
#[tokio::test]
async fn native_runner_stream_refresh_preserves_delivery_without_retaining_partials() {
    use adk_rust::futures::StreamExt;
    for refresh in [false, true] {
        let sessions = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "stream-probe".into(),
                user_id: "user".into(),
                session_id: Some("session".into()),
                state: HashMap::new(),
            })
            .await
            .unwrap();
        let maximum = Arc::new(AtomicUsize::new(0));
        let runner = adk_rust::runner::Runner::builder()
            .app_name("stream-probe")
            .agent(Arc::new(StreamingHistoryProbe {
                maximum: maximum.clone(),
            }))
            .session_service(sessions.clone())
            .build()
            .unwrap()
            .with_session_event_refresh(refresh);
        let mut stream = runner
            .run(
                "user".try_into().unwrap(),
                "session".try_into().unwrap(),
                Content::new("user").with_text("Stream the answer"),
            )
            .await
            .unwrap();
        let mut partials = 0;
        let mut finals = 0;
        while let Some(event) = stream.next().await {
            let event = event.unwrap();
            if event.llm_response.partial {
                partials += 1;
            } else {
                finals += 1;
            }
        }
        assert_eq!((partials, finals), (64, 1));
        let retained = maximum.load(Ordering::SeqCst);
        println!("refresh={refresh} maximum model-visible session items={retained}");
        if refresh {
            assert_eq!(retained, 2);
        } else {
            assert_eq!(retained, 66);
        }
        let stored = sessions
            .get(GetRequest {
                app_name: "stream-probe".into(),
                user_id: "user".into(),
                session_id: "session".into(),
                num_recent_events: None,
                after: None,
            })
            .await
            .unwrap();
        assert_eq!(stored.events().len(), 2);
    }
}
