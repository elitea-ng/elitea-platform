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
        // The user turn and the terminal event only. Since ADK 2.3.0 the
        // model-visible history skips partial deltas with or without refresh
        // (2.2.0 exposed all 64 of them, 66 items, when refresh was off); the
        // refresh path must still deliver every fragment and store none.
        assert_eq!(retained, 2);
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

/// Records the history the model would see at each step of the stream.
pub(super) struct RecordingStreamProbe {
    seen: Arc<Mutex<Vec<Value>>>,
}

impl RecordingStreamProbe {
    pub(super) fn new() -> Self {
        Self {
            seen: Arc::new(Mutex::new(Vec::new())),
        }
    }
}
#[async_trait]
impl adk_rust::Agent for RecordingStreamProbe {
    fn name(&self) -> &'static str {
        "recording-probe"
    }
    fn description(&self) -> &'static str {
        "Records the refreshed history"
    }
    fn sub_agents(&self) -> &[Arc<dyn adk_rust::Agent>] {
        &[]
    }
    async fn run(
        &self,
        ctx: Arc<dyn adk_rust::InvocationContext>,
    ) -> adk_rust::Result<adk_rust::EventStream> {
        let seen = self.seen.clone();
        Ok(Box::pin(adk_rust::futures::stream::unfold(
            Some((ctx, seen, 0)),
            |state| async move {
                let (ctx, seen, index) = state?;
                seen.lock()
                    .unwrap()
                    .push(serde_json::to_value(ctx.session().conversation_history()).unwrap());
                if index > 3 {
                    return None;
                }
                let mut event = Event::with_id(format!("step-{index}"), ctx.invocation_id());
                event.author = "recording-probe".into();
                event.llm_response.partial = index == 0;
                event.set_content(Content::new("model").with_text(format!("answer {index}")));
                Some((Ok(event), Some((ctx, seen, index + 1))))
            },
        )))
    }
}

/// A host hook which serves the refresh from a full read, and counts its calls.
struct FullReadRefresher {
    sessions: Arc<InMemorySessionService>,
    calls: AtomicUsize,
}
#[async_trait]
impl adk_runner::SessionEventRefresh for FullReadRefresher {
    async fn refreshed_events(
        &self,
        identity: &adk_rust::AdkIdentity,
    ) -> adk_rust::Result<Vec<Event>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let session = self
            .sessions
            .get(GetRequest {
                app_name: identity.app_name.to_string(),
                user_id: identity.user_id.to_string(),
                session_id: identity.session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await?;
        Ok(session.events().all())
    }
}

/// FU-287: without a refresher the Runner is exactly what it was; with one
/// that serves the same events, the model sees the same history and the same
/// events are stored. The hook is asked once per persisted non-partial event.
#[tokio::test]
async fn session_event_refresher_is_optional_and_replaces_only_the_full_read() {
    use adk_rust::futures::StreamExt;
    let mut outcomes = Vec::new();
    for mode in ["no_refresh", "full_read", "hook"] {
        let sessions = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "recording-probe".into(),
                user_id: "user".into(),
                session_id: Some("session".into()),
                state: HashMap::new(),
            })
            .await
            .unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let hook = Arc::new(FullReadRefresher {
            sessions: sessions.clone(),
            calls: AtomicUsize::new(0),
        });
        let mut runner = adk_rust::runner::Runner::builder()
            .app_name("recording-probe")
            .agent(Arc::new(RecordingStreamProbe { seen: seen.clone() }))
            .session_service(sessions.clone())
            .build()
            .unwrap()
            .with_session_event_refresh(mode != "no_refresh");
        if mode == "hook" {
            runner = runner.with_session_event_refresher(hook.clone());
        }
        let mut stream = runner
            .run(
                "user".try_into().unwrap(),
                "session".try_into().unwrap(),
                Content::new("user").with_text("Record the history"),
            )
            .await
            .unwrap();
        let mut delivered = Vec::new();
        while let Some(event) = stream.next().await {
            delivered.push(serde_json::to_value(event.unwrap()).unwrap());
        }
        let stored = sessions
            .get(GetRequest {
                app_name: "recording-probe".into(),
                user_id: "user".into(),
                session_id: "session".into(),
                num_recent_events: None,
                after: None,
            })
            .await
            .unwrap()
            .events()
            .all();
        // The user turn's id is a fresh uuid in every run.
        let stored_ids = stored
            .iter()
            .map(|event| {
                if event.id.starts_with("step-") {
                    event.id.clone()
                } else {
                    "user-turn".to_owned()
                }
            })
            .collect::<Vec<_>>();
        let calls = hook.calls.load(Ordering::SeqCst);
        let seen = seen.lock().unwrap().clone();
        outcomes.push((mode, delivered.len(), stored_ids, seen, calls));
    }
    let [none, full, hook] = <[_; 3]>::try_from(outcomes).unwrap();
    // Three non-partial events are persisted after the partial one: the hook is
    // asked once for each, and only the hook run asks it.
    assert_eq!((none.4, full.4, hook.4), (0, 0, 3));
    // Delivery and stored events never differ between the three runs, and the
    // history the model sees is the same with the refresh served by the hook as
    // with the runner's own full read.
    assert_eq!((none.1, none.2.clone()), (full.1, full.2.clone()));
    assert_eq!((full.1, full.2, full.3), (hook.1, hook.2, hook.3));
}

struct TransferChild;
#[async_trait]
impl adk_rust::Agent for TransferChild {
    fn name(&self) -> &'static str {
        "transfer-child"
    }
    fn description(&self) -> &'static str {
        "Answers after a transfer"
    }
    fn sub_agents(&self) -> &[Arc<dyn adk_rust::Agent>] {
        &[]
    }
    async fn run(
        &self,
        ctx: Arc<dyn adk_rust::InvocationContext>,
    ) -> adk_rust::Result<adk_rust::EventStream> {
        let mut event = Event::with_id("child-answer", ctx.invocation_id());
        event.author = "transfer-child".into();
        event.set_content(Content::new("model").with_text("child answer"));
        Ok(Box::pin(adk_rust::futures::stream::iter([Ok(event)])))
    }
}

struct TransferParent {
    children: Vec<Arc<dyn adk_rust::Agent>>,
}
#[async_trait]
impl adk_rust::Agent for TransferParent {
    fn name(&self) -> &'static str {
        "transfer-parent"
    }
    fn description(&self) -> &'static str {
        "Hands the turn to its child"
    }
    fn sub_agents(&self) -> &[Arc<dyn adk_rust::Agent>] {
        &self.children
    }
    async fn run(
        &self,
        ctx: Arc<dyn adk_rust::InvocationContext>,
    ) -> adk_rust::Result<adk_rust::EventStream> {
        let mut event = Event::with_id("parent-handoff", ctx.invocation_id());
        event.author = "transfer-parent".into();
        event.set_content(Content::new("model").with_text("handing off"));
        event.actions.transfer_to_agent = Some("transfer-child".into());
        Ok(Box::pin(adk_rust::futures::stream::iter([Ok(event)])))
    }
}

/// FU-287: the Runner asks the hook after the events of its transfer loop
/// too, not only after the events of the main loop.
#[tokio::test]
async fn session_event_refresher_is_asked_in_the_main_loop_and_the_transfer_loop() {
    use adk_rust::futures::StreamExt;
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "transfer-probe".into(),
            user_id: "user".into(),
            session_id: Some("session".into()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let hook = Arc::new(FullReadRefresher {
        sessions: sessions.clone(),
        calls: AtomicUsize::new(0),
    });
    let runner = adk_rust::runner::Runner::builder()
        .app_name("transfer-probe")
        .agent(Arc::new(TransferParent {
            children: vec![Arc::new(TransferChild)],
        }))
        .session_service(sessions.clone())
        .build()
        .unwrap()
        .with_session_event_refresh(true)
        .with_session_event_refresher(hook.clone());
    let mut stream = runner
        .run(
            "user".try_into().unwrap(),
            "session".try_into().unwrap(),
            Content::new("user").with_text("Hand it over"),
        )
        .await
        .unwrap();
    let mut authors = Vec::new();
    while let Some(event) = stream.next().await {
        authors.push(event.unwrap().author);
    }
    assert_eq!(authors, ["transfer-parent", "transfer-child"]);
    assert_eq!(hook.calls.load(Ordering::SeqCst), 2);
}
