use super::*;
use crate::agents::context_budget::{RequestContextBudget, RequestContextUsage};
use crate::agents::model_checkpoint::CHECKPOINT_KEY;
use crate::agents::request::ModelContextLimits;
use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};
use adk_rust::futures::stream;
use adk_rust::session::{InMemorySessionService, Session};
use adk_rust::{LlmRequest, LlmResponse, LlmResponseStream};
use std::sync::atomic::AtomicUsize;

struct Context {
    session: &'static str,
    invocation: &'static str,
    input: Content,
}

impl Context {
    fn child(session: &'static str) -> Self {
        Self {
            session,
            invocation: "attempt-one",
            input: Content::new("user").with_text("Child task"),
        }
    }
}

#[async_trait]
impl ReadonlyContext for Context {
    fn invocation_id(&self) -> &str {
        self.invocation
    }
    fn agent_name(&self) -> &'static str {
        "child-agent"
    }
    fn user_id(&self) -> &'static str {
        "user-1"
    }
    fn app_name(&self) -> &'static str {
        "elitea-agent-v1"
    }
    fn session_id(&self) -> &str {
        self.session
    }
    fn branch(&self) -> &'static str {
        "child"
    }
    fn user_content(&self) -> &Content {
        &self.input
    }
}

struct Budget;
impl ModelRequestBudget for Budget {
    fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage> {
        RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 8000,
                max_output_tokens: 1000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: None,
            }),
            &serde_json::Map::new(),
            Some(1000),
        )
        .unwrap()
        .unwrap()
        .measure_provider_request(&serde_json::to_vec(request).unwrap(), 1_048_576)
    }
}

#[derive(Default)]
struct Summary(AtomicUsize);
#[async_trait]
impl Llm for Summary {
    fn name(&self) -> &'static str {
        "summary-fixture"
    }
    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(Box::pin(stream::once(async {
            Ok(LlmResponse::new(
                Content::new("model").with_text(crate::agents::context_summary::fixture()),
            ))
        })))
    }
}

fn storage(backend: ModelScopeBackend) -> ModelScopeSessions {
    ModelScopeSessions::new(backend, "execution-1".into(), 1, [0x11; 32])
}

fn checkpoint(storage: &ModelScopeSessions, summary: Arc<Summary>) -> Arc<ScopedModelCheckpoint> {
    storage.checkpoint(
        Some(ContextCompactionPlan {
            max_context_tokens: 8000,
            preserve_recent_messages: 2,
            preserve_system_messages: true,
            summary_instructions: "Preserve work. {messages}".into(),
        }),
        Some(Arc::new(Budget)),
        summary,
        None,
        None,
    )
}

fn history() -> LlmRequest {
    serde_json::from_value(serde_json::json!({"model":"fixture", "contents":[
        {"role":"system", "parts":[{"text":"Exact active instruction revision"}]},
        {"role":"user", "parts":[{"text":"Child task"}]},
        {"role":"model", "parts":[{"text":"call-one child evidence ".repeat(1400)}]},
        {"role":"user", "parts":[{"text":"Continue with the correction"}]},
        {"role":"model", "parts":[{"text":"Recent exact answer"}]}
    ]}))
    .unwrap()
}

async fn prepare(scope: &ScopedModelCheckpoint, context: &Context) -> LlmRequest {
    let writer = scope.writer(context).await.unwrap();
    assert!(
        !writer.checkpoint.is_recovery(),
        "loading notes grants no retry permission"
    );
    let BeforeModelResult::Continue(request) = writer
        .checkpoint
        .before_model(
            writer.identity.clone(),
            context.invocation_id(),
            context.agent_name(),
            history(),
        )
        .await
        .unwrap()
    else {
        panic!("prepared request")
    };
    request
}

async fn stored(scope: &ScopedModelCheckpoint, context: &Context) -> Box<dyn Session> {
    let writer = scope.writer(context).await.unwrap();
    writer
        .sessions
        .get(GetRequest {
            app_name: writer.identity.app_name.to_string(),
            user_id: writer.identity.user_id.to_string(),
            session_id: writer.identity.session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap()
}

#[tokio::test]
async fn scope_reloads_summary_without_sharing_sibling_history_or_retry_permission() {
    let sessions = Arc::new(InMemorySessionService::new());
    let storage = storage(ModelScopeBackend::Local(sessions));
    let child = Context::child("parent-one-call-one");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let prepared = prepare(&first, &child).await;
    let writer = first.writer(&child).await.unwrap();
    writer
        .checkpoint
        .before_tool(writer.identity.clone(), child.invocation_id())
        .await
        .unwrap();
    assert!(
        ModelCheckpointWriter::new(writer.sessions.clone(), "execution-1".into(), 1, [0x11; 32])
            .inspect(stored(&first, &child).await.as_ref(), true)
            .is_err()
    );
    let mut replacement_context = Context::child("parent-one-call-one");
    replacement_context.invocation = "replacement-attempt";
    let replacement = checkpoint(&storage, summary.clone());
    assert_eq!(
        serde_json::to_value(prepare(&replacement, &replacement_context).await).unwrap(),
        serde_json::to_value(prepared).unwrap()
    );
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    let sibling = Context::child("parent-one-call-two");
    assert!(
        first.writer(&sibling).await.is_err(),
        "a writer is bound to one child"
    );
    let other = checkpoint(&storage, summary.clone());
    prepare(&other, &sibling).await;
    assert_eq!(summary.0.load(Ordering::SeqCst), 2);
    assert_ne!(
        storage.identity(&child).unwrap(),
        storage.identity(&sibling).unwrap()
    );
    let mut another_execution = storage.clone();
    another_execution.execution_id = "execution-2".into();
    assert_ne!(
        storage.identity(&child).unwrap(),
        another_execution.identity(&child).unwrap()
    );
    another_execution = storage.clone();
    another_execution.generation += 1;
    assert_ne!(
        storage.identity(&child).unwrap(),
        another_execution.identity(&child).unwrap()
    );
}

#[tokio::test]
async fn authorized_child_recovery_restores_exact_pending_request_without_resummarizing() {
    let sessions = Arc::new(InMemorySessionService::new());
    let storage = storage(ModelScopeBackend::Local(sessions));
    let child = Context::child("parent-call-recovery");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let prepared = prepare(&first, &child).await;
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary.clone());
    let mut context = Context::child("parent-call-recovery");
    context.invocation = "replacement-attempt";
    let mut fresh = history();
    fresh.contents = vec![Content::new("user").with_text("Child task")];
    let BeforeModelResult::Continue(restored) =
        replacement.before_model(&context, fresh).await.unwrap()
    else {
        panic!("restored provider request")
    };
    assert_eq!(
        serde_json::to_value(restored).unwrap(),
        serde_json::to_value(prepared).unwrap()
    );
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    assert_eq!(
        stored(&replacement, &context)
            .await
            .state()
            .get(CHECKPOINT_KEY)
            .unwrap()["invocation_id"],
        "replacement-attempt"
    );
}

#[tokio::test]
async fn authorized_child_recovery_still_refuses_an_unfinished_tool() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("parent-call-tool-boundary");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    prepare(&first, &child).await;
    let writer = first.writer(&child).await.unwrap();
    writer
        .checkpoint
        .before_tool(writer.identity.clone(), child.invocation_id())
        .await
        .unwrap();
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary.clone());
    assert!(replacement.before_model(&child, history()).await.is_err());
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn replay_control_input_stays_outside_compaction_and_pending_provider_request() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let summary = Arc::new(Summary::default());
    let base = checkpoint(&storage, summary.clone());
    let marker = Content::new("user").with_text("PRIVATE_REPLAY_CONTROL");
    let scope = storage.checkpoint(
        base.plan.clone(),
        Some(Arc::new(Budget)),
        summary.clone(),
        Some(marker.clone()),
        None,
    );
    let context = Context::child("parent-call-one");
    let mut request = history();
    request.contents.push(marker);
    let BeforeModelResult::Continue(first) =
        scope.before_model(&context, request.clone()).await.unwrap()
    else {
        panic!("replay request")
    };
    assert_eq!(
        serde_json::to_value(&first).unwrap(),
        serde_json::to_value(&request).unwrap()
    );
    assert_eq!(summary.0.load(Ordering::SeqCst), 0);
    let replay_boundary = stored(&scope, &context)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(replay_boundary["phase"], "tool_may_have_started");
    assert!(replay_boundary["model"].is_null());
    assert!(
        !replay_boundary
            .to_string()
            .contains("PRIVATE_REPLAY_CONTROL")
    );
    let BeforeModelResult::Continue(next) = scope.before_model(&context, request).await.unwrap()
    else {
        panic!("provider request")
    };
    let encoded = serde_json::to_string(&next).unwrap();
    assert!(!encoded.contains("PRIVATE_REPLAY_CONTROL"));
    assert!(encoded.contains("Child task"));
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    let saved = stored(&scope, &context)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert!(!saved.to_string().contains("PRIVATE_REPLAY_CONTROL"));
}

#[tokio::test]
async fn completed_guard_replay_prepares_the_first_real_provider_request() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let summary = Arc::new(Summary::default());
    let base = checkpoint(&storage, summary.clone());
    let marker = Content::new("user").with_text("PRIVATE_REPLAY_CONTROL");
    let scope = storage
        .checkpoint(
            base.plan.clone(),
            Some(Arc::new(Budget)),
            summary.clone(),
            Some(marker.clone()),
            None,
        )
        .with_replay_pending(false);
    let context = Context::child("parent-call-one");
    let mut request = history();
    request.contents.push(marker);
    let BeforeModelResult::Continue(prepared) =
        scope.before_model(&context, request).await.unwrap()
    else {
        panic!("provider request")
    };
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    assert!(
        !serde_json::to_string(&prepared)
            .unwrap()
            .contains("PRIVATE_REPLAY_CONTROL")
    );
    assert_eq!(
        stored(&scope, &context)
            .await
            .state()
            .get(CHECKPOINT_KEY)
            .unwrap()["phase"],
        "model_pending"
    );
}

struct Completion;

#[test]
fn terminal_tool_call_is_not_a_completed_child_even_with_skip_summarization() {
    let mut event = Event::new("child-attempt");
    event.author = "child-agent".into();
    event.llm_response.turn_complete = true;
    event.actions.skip_summarization = true;
    event.set_content(Content {
        role: "model".into(),
        parts: vec![adk_rust::Part::FunctionCall {
            name: "unfinished_tool".into(),
            args: serde_json::json!({}),
            id: Some("pending-call".into()),
            thought_signature: None,
        }],
    });
    assert!(!successful_terminal(&event, "child-agent"));
}
impl crate::agents::session::DurableModelCompletion for Completion {
    fn snapshot(&self) -> adk_rust::Result<Option<String>> {
        Ok(Some("Complete streamed child result".into()))
    }
}

struct UnexpectedChildRun(Arc<AtomicUsize>);

#[async_trait]
impl Agent for UnexpectedChildRun {
    fn name(&self) -> &'static str {
        "child-agent"
    }
    fn description(&self) -> &'static str {
        "Detect repeated child execution"
    }
    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }
    async fn run(&self, _: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Err(AdkError::session(
            "A completed child must not execute again.",
        ))
    }
}

#[tokio::test]
async fn authorized_recovery_delivers_completed_child_without_another_model_request() {
    let sessions = Arc::new(InMemorySessionService::new());
    let storage = storage(ModelScopeBackend::Local(sessions.clone()));
    let child = Context::child("completed-call");
    let summary = Arc::new(Summary::default());
    let base = checkpoint(&storage, summary.clone());
    let first = storage.checkpoint(
        base.plan.clone(),
        Some(Arc::new(Budget)),
        summary.clone(),
        None,
        Some(Arc::new(Completion)),
    );
    prepare(&first, &child).await;
    append_streamed_terminal(&first, &child).await;
    let fresh = checkpoint(&storage, summary.clone());
    assert!(fresh.completed_event(&child).await.unwrap().is_none());
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary.clone());
    let mut context = Context::child("completed-call");
    context.invocation = "replacement-attempt";
    let event = replacement
        .completed_event(&context)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(event.invocation_id, "replacement-attempt");
    assert_eq!(event.branch, "child");
    assert_eq!(event.author, "child-agent");
    assert!(event.llm_response.turn_complete);
    assert_eq!(
        serde_json::to_value(&event.llm_response.content).unwrap(),
        serde_json::to_value(Content::new("model").with_text("Complete streamed child result"))
            .unwrap()
    );
    assert!(event.actions.state_delta.is_empty());
    assert!(event.llm_response.usage_metadata.is_none());
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    sessions
        .create(CreateRequest {
            app_name: child.app_name().into(),
            user_id: child.user_id().into(),
            session_id: Some(child.session_id().into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let calls = Arc::new(AtomicUsize::new(0));
    let runner = adk_rust::runner::Runner::builder()
        .app_name(child.app_name())
        .agent(replacement.wrap(Arc::new(UnexpectedChildRun(calls.clone()))))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut invocation = crate::agents::runtime::NativeAgentInvocation::new(
        runner,
        child.user_id().try_into().unwrap(),
        child.session_id().try_into().unwrap(),
        child.input.clone(),
    )
    .start()
    .unwrap();
    let delivered = invocation.next_event().await.unwrap().unwrap();
    assert_eq!(
        serde_json::to_value(delivered.llm_response.content).unwrap(),
        serde_json::to_value(event.llm_response.content).unwrap()
    );
    assert!(invocation.next_event().await.unwrap().is_none());
    assert_eq!(calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn later_pending_request_supersedes_child_completion() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("later-call");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    prepare(&first, &child).await;
    let mut event = Event::new(child.invocation_id());
    event.author = child.agent_name().into();
    event.set_content(Content::new("model").with_text("Earlier answer"));
    event.llm_response.turn_complete = true;
    first.append(&child, event).await.unwrap();
    let pending = prepare(&first, &child).await;
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary);
    assert!(replacement.completed_event(&child).await.unwrap().is_none());
    let BeforeModelResult::Continue(request) =
        replacement.before_model(&child, history()).await.unwrap()
    else {
        panic!("pending request");
    };
    assert_eq!(
        serde_json::to_value(request).unwrap(),
        serde_json::to_value(pending).unwrap()
    );
}

#[tokio::test]
async fn interrupted_child_does_not_receive_a_completion_receipt() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("interrupted-call");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    prepare(&first, &child).await;
    let mut event = Event::new(child.invocation_id());
    event.author = child.agent_name().into();
    event.set_content(Content::new("model").with_text("Waiting for approval"));
    event.llm_response.turn_complete = true;
    event.llm_response.interrupted = true;
    first.append(&child, event).await.unwrap();
    assert!(
        stored(&first, &child)
            .await
            .state()
            .get(COMPLETION_KEY)
            .is_none()
    );
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary);
    assert!(replacement.completed_event(&child).await.is_err());
}

#[tokio::test]
async fn completed_child_requires_matching_definition_and_atomic_event_receipt() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("receipt-validation");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    prepare(&first, &child).await;
    let mut event = Event::new(child.invocation_id());
    event.author = child.agent_name().into();
    event.set_content(Content::new("model").with_text("Saved answer"));
    event.llm_response.turn_complete = true;
    first.append(&child, event).await.unwrap();
    let mut changed = storage.clone().with_pending_model_recovery();
    changed.definition_digest = [0x22; 32];
    assert!(
        checkpoint(&changed, summary.clone())
            .completed_event(&child)
            .await
            .is_err()
    );
    let mut receipt = stored(&first, &child)
        .await
        .state()
        .get(COMPLETION_KEY)
        .unwrap();
    receipt["event_id"] = serde_json::json!("missing-event");
    let writer = first.writer(&child).await.unwrap();
    let mut detached = Event::new(child.invocation_id());
    detached
        .actions
        .state_delta
        .insert(COMPLETION_KEY.into(), receipt);
    writer
        .sessions
        .append_event_for_identity(AppendEventRequest {
            identity: writer.identity.clone(),
            event: detached,
        })
        .await
        .unwrap();
    assert!(
        checkpoint(&storage.with_pending_model_recovery(), summary)
            .completed_event(&child)
            .await
            .is_err()
    );
}

async fn append_streamed_terminal(scope: &ScopedModelCheckpoint, child: &Context) {
    let mut event = Event::new(child.invocation_id());
    event.author = "child-agent".into();
    event.llm_response.partial = true;
    event.set_content(Content::new("model").with_text("Complete "));
    scope.append(child, event.clone()).await.unwrap();
    event.set_content(Content::new("model").with_text("streamed child result"));
    scope.append(child, event.clone()).await.unwrap();
    event.llm_response.partial = false;
    event.llm_response.turn_complete = true;
    event.llm_response.content = None;
    scope.append(child, event.clone()).await.unwrap();
    let stored = stored(scope, child).await;
    let events = stored.events().all();
    let matching: Vec<_> = events.iter().filter(|saved| saved.id == event.id).collect();
    assert_eq!(matching.len(), 1);
    assert!(
        serde_json::to_string(matching[0])
            .unwrap()
            .contains("Complete streamed child result")
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered story verifies parent takeover and child history recovery.
async fn postgres_child_scope_is_fenced_by_root_takeover_and_reuses_its_own_summary() {
    use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping child model PostgreSQL test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let database = IsolatedPostgres::create(&url).await;
    install_schema(&database.pool).await;
    let root = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-1", 1, 1, [1; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let child = Context::child("parent-one-call-one");
    let storage = storage(ModelScopeBackend::Postgres(root.clone()));
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let first = storage.checkpoint(
        first.plan.clone(),
        Some(Arc::new(Budget)),
        summary.clone(),
        None,
        Some(Arc::new(Completion)),
    );
    let prepared = prepare(&first, &child).await;
    append_streamed_terminal(&first, &child).await;
    let mut event = Event::new(child.invocation_id());
    event.author = "child-agent".into();
    event.set_content(Content::new("model").with_text("Durable child result"));
    event.llm_response.turn_complete = true;
    first.append(&child, event).await.unwrap();
    assert!(
        stored(&first, &child)
            .await
            .state()
            .get(CHECKPOINT_KEY)
            .is_some()
    );
    assert!(
        root.get(GetRequest {
            app_name: child.app_name().into(),
            user_id: child.user_id().into(),
            session_id: storage.identity(&child).unwrap().session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .is_err(),
        "root service must not gain arbitrary session access"
    );
    let replacement = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-2", 2, 2, [2; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let mut stale_event = Event::new("stale-attempt");
    stale_event.author = "child-agent".into();
    stale_event.set_content(Content::new("model").with_text("Stale child result"));
    let error = first.append(&child, stale_event).await.unwrap_err();
    assert_eq!(error.code, "session.writer_not_current");
    assert!(
        root.model_scope(&storage.identity(&Context::child("new-child")).unwrap())
            .await
            .is_err()
    );
    let replacement_storage = ModelScopeSessions::new(
        ModelScopeBackend::Postgres(replacement),
        "execution-1".into(),
        1,
        [0x11; 32],
    );
    let recovered = checkpoint(
        &replacement_storage.clone().with_pending_model_recovery(),
        summary.clone(),
    );
    let completed = recovered.completed_event(&child).await.unwrap().unwrap();
    assert_eq!(
        serde_json::to_value(completed.llm_response.content).unwrap(),
        serde_json::to_value(Content::new("model").with_text("Durable child result")).unwrap()
    );
    let reloaded = checkpoint(&replacement_storage, summary.clone());
    assert_eq!(
        serde_json::to_value(prepare(&reloaded, &child).await).unwrap(),
        serde_json::to_value(prepared).unwrap()
    );
    assert_eq!(summary.0.load(Ordering::SeqCst), 1);
    let retained: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM elitea_runtime.agent_session_events WHERE strpos(event_payload, $1) > 0)",
    ).bind("Durable child result").fetch_one(&database.pool).await.unwrap();
    assert!(
        retained,
        "the immutable ledger retains the completed child result"
    );
    let active = stored(&reloaded, &child).await.events().all();
    assert!(
        active
            .iter()
            .filter_map(|event| event.llm_response.content.as_ref())
            .any(|content| serde_json::to_string(content)
                .unwrap()
                .contains("Child task")),
        "the child restores model history from its prepared request snapshot"
    );
    database.pool.close().await;
}

#[tokio::test]
async fn output_limited_child_does_not_receive_a_completion_receipt() {
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("output-limited-call");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary);
    prepare(&first, &child).await;
    let mut event = Event::new(child.invocation_id());
    event.author = child.agent_name().into();
    event.set_content(Content::new("model").with_text("Unfinished child answer"));
    event.llm_response.turn_complete = true;
    event.llm_response.finish_reason = Some(adk_rust::FinishReason::MaxTokens);
    first.append(&child, event).await.unwrap();
    let session = stored(&first, &child).await;
    assert!(
        session.state().get(COMPLETION_KEY).is_none(),
        "Truncated output is not completed work"
    );
    assert!(
        first
            .completed_content(session.as_ref(), child.agent_name())
            .unwrap()
            .is_none()
    );
}

struct OutputModel {
    requests: std::sync::Mutex<Vec<LlmRequest>>,
    replies: std::sync::Mutex<std::collections::VecDeque<(&'static str, adk_rust::FinishReason)>>,
}
impl OutputModel {
    fn new(replies: Vec<(&'static str, adk_rust::FinishReason)>) -> Arc<Self> {
        Arc::new(Self {
            requests: std::sync::Mutex::new(Vec::new()),
            replies: std::sync::Mutex::new(replies.into()),
        })
    }
}
#[async_trait]
impl Llm for OutputModel {
    fn name(&self) -> &'static str {
        "output-fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.requests.lock().unwrap().push(request);
        let (text, finish_reason) = self
            .replies
            .lock()
            .unwrap()
            .pop_front()
            .expect("unexpected model request");
        Ok(Box::pin(stream::iter(vec![
            Ok(LlmResponse {
                content: Some(Content::new("model").with_text(text)),
                partial: true,
                ..LlmResponse::default()
            }),
            Ok(LlmResponse {
                finish_reason: Some(finish_reason),
                turn_complete: true,
                ..LlmResponse::default()
            }),
        ])))
    }
}
async fn simple_request(scope: &ScopedModelCheckpoint, child: &Context) -> LlmRequest {
    let request = LlmRequest::new("output-fixture", vec![child.input.clone()]);
    let BeforeModelResult::Continue(request) = scope.before_model(child, request).await.unwrap()
    else {
        panic!("request")
    };
    request
}
async fn collect_output(mut output: LlmResponseStream) -> adk_rust::Result<String> {
    let mut text = String::new();
    let mut complete = false;
    while let Some(chunk) = output.next().await {
        let chunk = chunk?;
        if let Some(content) = chunk.content {
            for part in content.parts {
                if let adk_rust::Part::Text { text: value } = part {
                    text.push_str(&value);
                }
            }
        }
        complete |=
            chunk.turn_complete && chunk.finish_reason == Some(adk_rust::FinishReason::Stop);
    }
    assert!(complete, "child must finish before returning to parent");
    Ok(text)
}

#[tokio::test]
async fn child_output_continues_without_repeating_the_anchor() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("output-call");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("First part", MaxTokens),
        ("First part and ending", Stop),
    ]);
    let output = scope
        .clone()
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "First part and ending"
    );
    let requests = model.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    assert!(
        serde_json::to_string(&requests[1])
            .unwrap()
            .contains("First part")
    );
}

#[tokio::test]
async fn child_output_recovery_restores_prefix_and_exact_pending_request() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("output-recovery");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let request = simple_request(&first, &child).await;
    let first_model = OutputModel::new(vec![("First part", MaxTokens)]);
    let mut output = first
        .clone()
        .delegation_model(first_model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    output.next().await.unwrap().unwrap();
    // The next item is emitted only after the continuation checkpoint commits.
    output.next().await.unwrap().unwrap();
    drop(output);
    assert_eq!(first_model.requests.lock().unwrap().len(), 1);
    let saved = stored(&first, &child)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(saved["output_continuation"]["prefix"], "First part");
    assert_eq!(saved["output_continuation"]["round"], 1);
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary);
    let request = simple_request(&replacement, &child).await;
    assert_eq!(
        serde_json::to_value(&request).unwrap(),
        saved["model"]["request"]
    );
    let model = OutputModel::new(vec![("First part and ending", Stop)]);
    let output = replacement
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "First part and ending"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn child_output_refuses_an_unverified_continuation_seam() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("invalid-output-seam");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("First part", MaxTokens),
        ("Different answer", Stop),
        ("Still different", Stop),
    ]);
    let output = scope
        .clone()
        .delegation_model(model)
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap_err().code,
        "model.output_continuation_failed"
    );
    assert!(
        stored(&scope, &child)
            .await
            .state()
            .get(COMPLETION_KEY)
            .is_none()
    );
}

#[tokio::test]
async fn child_output_reasoning_only_exhaustion_retries_for_visible_answer() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("reasoning-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![("", MaxTokens), ("Complete answer", Stop)]);
    let output = scope
        .delegation_model(model)
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(collect_output(output).await.unwrap(), "Complete answer");
}

#[tokio::test]
async fn adk_child_returns_one_complete_answer_after_multiple_output_continuations() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let sessions = Arc::new(InMemorySessionService::new());
    let storage = storage(ModelScopeBackend::Local(sessions.clone()));
    let child = Context::child("adk-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let model = OutputModel::new(vec![
        ("First", MaxTokens),
        ("First second", MaxTokens),
        ("First second final", Stop),
    ]);
    let agent = scope
        .clone()
        .bind(
            LlmAgentBuilder::new(child.agent_name())
                .model(scope.clone().delegation_model(model.clone())),
        )
        .build()
        .unwrap();
    sessions
        .create(CreateRequest {
            app_name: child.app_name().into(),
            user_id: child.user_id().into(),
            session_id: Some(child.session_id().into()),
            state: std::collections::HashMap::new(),
        })
        .await
        .unwrap();
    let runner = adk_rust::runner::Runner::builder()
        .app_name(child.app_name())
        .agent(scope.clone().wrap(Arc::new(agent)))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut invocation = crate::agents::runtime::NativeAgentInvocation::new(
        runner,
        child.user_id().try_into().unwrap(),
        child.session_id().try_into().unwrap(),
        child.input.clone(),
    )
    .start()
    .unwrap();
    let mut finals = Vec::new();
    while let Some(event) = invocation.next_event().await.unwrap() {
        if event.is_final_response() && event.llm_response.turn_complete {
            finals.push(event.llm_response.content.unwrap());
        }
    }
    assert_eq!(finals.len(), 1);
    let text: String = finals[0]
        .parts
        .iter()
        .map(|part| match part {
            adk_rust::Part::Text { text } => text.as_str(),
            _ => panic!("unexpected non-text result"),
        })
        .collect();
    assert_eq!(text, "First second final");
    assert_eq!(model.requests.lock().unwrap().len(), 3);
    let writer = scope.writer.get().unwrap();
    let saved = writer
        .sessions
        .get(GetRequest {
            app_name: writer.identity.app_name.to_string(),
            user_id: writer.identity.user_id.to_string(),
            session_id: writer.identity.session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap();
    let completed = scope
        .completed_content(saved.as_ref(), child.agent_name())
        .unwrap()
        .unwrap();
    assert_eq!(
        serde_json::to_value(completed).unwrap(),
        serde_json::to_value(Content::new("model").with_text("First second final")).unwrap()
    );
}

#[tokio::test]
async fn postgres_child_output_continuation_survives_claim_takeover() {
    postgres_output_takeover(true).await;
}

#[tokio::test]
async fn postgres_child_without_compaction_continues_after_claim_takeover() {
    postgres_output_takeover(false).await;
}

async fn postgres_output_takeover(compaction_enabled: bool) {
    use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping output continuation PostgreSQL test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let database = IsolatedPostgres::create(&url).await;
    install_schema(&database.pool).await;
    let root = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-1", 1, 1, [1; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let first_storage = storage(ModelScopeBackend::Postgres(root));
    let child = Context::child("postgres-output");
    let summary = Arc::new(Summary::default());
    let make_checkpoint = |storage: &ModelScopeSessions| {
        if compaction_enabled {
            checkpoint(storage, summary.clone())
        } else {
            storage.checkpoint(None, Some(Arc::new(Budget)), summary.clone(), None, None)
        }
    };
    let first = make_checkpoint(&first_storage);
    let request = simple_request(&first, &child).await;
    let model = OutputModel::new(vec![("Accepted prefix", MaxTokens)]);
    let mut output = first
        .clone()
        .delegation_model(model)
        .generate_content(request, true)
        .await
        .unwrap();
    output.next().await.unwrap().unwrap();
    output.next().await.unwrap().unwrap();
    drop(output);
    let replacement_root = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-2", 2, 2, [2; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let old_writer = first.writer.get().unwrap();
    let error = old_writer
        .checkpoint
        .before_model(
            old_writer.identity.clone(),
            &old_writer.invocation_id,
            &old_writer.agent_name,
            LlmRequest::new("output-fixture", vec![child.input.clone()]),
        )
        .await
        .unwrap_err();
    assert_eq!(error.code, "session.writer_not_current");
    let replacement_storage =
        storage(ModelScopeBackend::Postgres(replacement_root)).with_pending_model_recovery();
    let replacement = make_checkpoint(&replacement_storage);
    let request = simple_request(&replacement, &child).await;
    let model = OutputModel::new(vec![("Accepted prefix and complete ending", Stop)]);
    let output = replacement
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "Accepted prefix and complete ending"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 1);
    assert_eq!(summary.0.load(Ordering::SeqCst), 0);
    database.pool.close().await;
}

#[tokio::test]
async fn child_output_accepts_completion_on_fourth_continuation() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("long-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("a", MaxTokens),
        ("ab", MaxTokens),
        ("abc", MaxTokens),
        ("abcd", MaxTokens),
        ("abcde", Stop),
    ]);
    let output = scope
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(collect_output(output).await.unwrap(), "abcde");
    assert_eq!(model.requests.lock().unwrap().len(), 5);
}

#[tokio::test]
async fn child_output_never_dispatches_a_fifth_continuation() {
    use adk_rust::FinishReason::MaxTokens;
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("bounded-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("a", MaxTokens),
        ("ab", MaxTokens),
        ("abc", MaxTokens),
        ("abcd", MaxTokens),
        ("abcde", MaxTokens),
    ]);
    let output = scope
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap_err().code,
        "model.output_continuation_failed"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 5);
}

#[tokio::test]
async fn child_output_repair_consumes_one_of_four_continuations() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("repair-counts-toward-cap");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("a", MaxTokens),
        ("ab", MaxTokens),
        ("INVALID", Stop),
        ("abc", MaxTokens),
        ("abcd", MaxTokens),
    ]);
    let output = scope
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap_err().code,
        "model.output_continuation_failed"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 5);
}

#[tokio::test]
async fn child_output_recovery_keeps_only_the_remaining_continuation_allowance() {
    use adk_rust::FinishReason::MaxTokens;
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("fourth-continuation-recovery");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let request = simple_request(&first, &child).await;
    let model = OutputModel::new(vec![
        ("a", MaxTokens),
        ("ab", MaxTokens),
        ("abc", MaxTokens),
        ("abcd", MaxTokens),
    ]);
    let mut output = first
        .clone()
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    // Each fixture call emits its text and the persisted continuation boundary.
    for _ in 0..8 {
        output.next().await.unwrap().unwrap();
    }
    drop(output);
    let saved = stored(&first, &child)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(saved["output_continuation"]["round"], 4);
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary);
    let request = simple_request(&replacement, &child).await;
    let model = OutputModel::new(vec![("abcde", MaxTokens)]);
    let output = replacement
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap_err().code,
        "model.output_continuation_failed"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn child_output_accepts_verified_suffix_without_changing_accepted_prefix() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("verified-output-suffix");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let first = "First accepted record.\nRECORD 081: Cedar archive verification remains complete.";
    let model = OutputModel::new(vec![
        (first, MaxTokens),
        (
            "RECORD 081: Cedar archive verification remains complete.\nRECORD 082: final",
            Stop,
        ),
    ]);
    let output = scope
        .delegation_model(model)
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        format!("{first}\nRECORD 082: final")
    );
}

#[tokio::test]
async fn child_output_repairs_one_invalid_boundary_without_accepting_rejected_text() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("repair-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("First part", MaxTokens),
        ("UNACCEPTED REPLACEMENT", Stop),
        ("First part and ending", Stop),
    ]);
    let output = scope
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "First part and ending"
    );
    let requests = model.requests.lock().unwrap();
    assert_eq!(requests.len(), 3);
    let repair = serde_json::to_string(&requests[2]).unwrap();
    assert!(!repair.contains("UNACCEPTED REPLACEMENT"));
    assert!(repair.contains("previous continuation was rejected"));
    assert_eq!(requests[2].contents.len(), 3);
}

#[tokio::test]
async fn child_output_recovery_does_not_reset_boundary_repair_allowance() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("repair-recovery");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&storage, summary.clone());
    let request = simple_request(&first, &child).await;
    let model = OutputModel::new(vec![
        ("First part", MaxTokens),
        ("UNACCEPTED REPLACEMENT", Stop),
    ]);
    let mut output = first
        .clone()
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    for _ in 0..3 {
        output.next().await.unwrap().unwrap();
    }
    drop(output);
    let saved = stored(&first, &child)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(saved["output_continuation"]["repair_used"], true);
    assert_eq!(saved["output_continuation"]["round"], 2);
    assert_eq!(saved["output_continuation"]["prefix"], "First part");
    assert!(!saved.to_string().contains("UNACCEPTED REPLACEMENT"));
    let replacement = checkpoint(&storage.with_pending_model_recovery(), summary);
    let request = simple_request(&replacement, &child).await;
    assert_eq!(
        serde_json::to_value(&request).unwrap(),
        saved["model"]["request"]
    );
    let model = OutputModel::new(vec![("STILL INVALID", Stop)]);
    let output = replacement
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap_err().code,
        "model.output_continuation_failed"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn postgres_boundary_repair_survives_claim_takeover() {
    use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping output continuation PostgreSQL test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let database = IsolatedPostgres::create(&url).await;
    install_schema(&database.pool).await;
    let root = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-1", 1, 1, [1; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let first_storage = storage(ModelScopeBackend::Postgres(root));
    let child = Context::child("postgres-output");
    let summary = Arc::new(Summary::default());
    let first = checkpoint(&first_storage, summary.clone());
    let request = simple_request(&first, &child).await;
    let model = OutputModel::new(vec![
        ("Accepted prefix", MaxTokens),
        ("UNACCEPTED REPLACEMENT", Stop),
    ]);
    let mut output = first
        .clone()
        .delegation_model(model)
        .generate_content(request, true)
        .await
        .unwrap();
    output.next().await.unwrap().unwrap();
    output.next().await.unwrap().unwrap();
    output.next().await.unwrap().unwrap();
    drop(output);
    let saved = stored(&first, &child)
        .await
        .state()
        .get(CHECKPOINT_KEY)
        .unwrap();
    assert_eq!(saved["output_continuation"]["repair_used"], true);
    assert_eq!(saved["output_continuation"]["round"], 2);
    assert!(!saved.to_string().contains("UNACCEPTED REPLACEMENT"));
    let replacement_root = Arc::new(
        PostgresSessionService::activate(
            database.pool.clone(),
            authority_for("claim-2", 2, 2, [2; 32]),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let old_writer = first.writer.get().unwrap();
    let error = old_writer
        .checkpoint
        .before_model(
            old_writer.identity.clone(),
            &old_writer.invocation_id,
            &old_writer.agent_name,
            LlmRequest::new("output-fixture", vec![child.input.clone()]),
        )
        .await
        .unwrap_err();
    assert_eq!(error.code, "session.writer_not_current");
    let replacement_storage =
        storage(ModelScopeBackend::Postgres(replacement_root)).with_pending_model_recovery();
    let replacement = checkpoint(&replacement_storage, summary);
    let request = simple_request(&replacement, &child).await;
    assert_eq!(
        serde_json::to_value(&request).unwrap(),
        saved["model"]["request"]
    );
    let model = OutputModel::new(vec![("Accepted prefix and complete ending", Stop)]);
    let output = replacement
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "Accepted prefix and complete ending"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 1);
    database.pool.close().await;
}

#[tokio::test]
async fn non_streaming_child_emits_incomplete_evidence_before_failure_without_receipt() {
    use adk_rust::FinishReason::MaxTokens;
    let sessions = Arc::new(InMemorySessionService::new());
    let storage = storage(ModelScopeBackend::Local(sessions.clone()));
    let child = Context::child("adk-output");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let model = OutputModel::new(vec![
        ("First", MaxTokens),
        ("First second", MaxTokens),
        ("First second third", MaxTokens),
        ("First second third fourth", MaxTokens),
        ("First second third fourth fifth", MaxTokens),
    ]);
    let agent = scope
        .clone()
        .bind(
            LlmAgentBuilder::new(child.agent_name())
                .model(scope.clone().delegation_model(model.clone())),
        )
        .build()
        .unwrap();
    sessions
        .create(CreateRequest {
            app_name: child.app_name().into(),
            user_id: child.user_id().into(),
            session_id: Some(child.session_id().into()),
            state: std::collections::HashMap::new(),
        })
        .await
        .unwrap();
    let runner = adk_rust::runner::Runner::builder()
        .app_name(child.app_name())
        .agent(scope.clone().wrap(Arc::new(agent)))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut invocation = crate::agents::runtime::NativeAgentInvocation::new_with_run_config(
        runner,
        child.user_id().try_into().unwrap(),
        child.session_id().try_into().unwrap(),
        child.input.clone(),
        adk_rust::RunConfig::builder()
            .streaming_mode(adk_rust::StreamingMode::None)
            .build(),
    )
    .start()
    .unwrap();
    let mut partials = Vec::new();
    loop {
        match invocation.next_event().await {
            Ok(Some(event)) => {
                assert!(!event.llm_response.turn_complete);
                if event.llm_response.partial
                    && let Some(content) = event.llm_response.content
                {
                    partials.push(content);
                }
            }
            Ok(None) => panic!("Expected continuation failure"),
            Err(error) => {
                assert_eq!(
                    error.upstream_code(),
                    Some("model.output_continuation_failed")
                );
                break;
            }
        }
    }
    assert_eq!(partials.len(), 1);
    assert_eq!(
        partials[0].parts[0],
        adk_rust::Part::Text {
            text: "First second third fourth fifth".into()
        }
    );
    assert_eq!(model.requests.lock().unwrap().len(), 5);
    let writer = scope.writer.get().unwrap();
    let saved = writer
        .sessions
        .get(GetRequest {
            app_name: writer.identity.app_name.to_string(),
            user_id: writer.identity.user_id.to_string(),
            session_id: writer.identity.session_id.to_string(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap();
    assert!(
        scope
            .completed_content(saved.as_ref(), child.agent_name())
            .unwrap()
            .is_none()
    );
}

#[tokio::test]
async fn child_without_compaction_continues_and_stops_after_first_complete_response() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let summary = Arc::new(Summary::default());
    let scope = storage.checkpoint(None, Some(Arc::new(Budget)), summary.clone(), None, None);
    let child = Context::child("continuation-without-compaction");
    let request = simple_request(&scope, &child).await;
    let model = OutputModel::new(vec![
        ("Accepted prefix", MaxTokens),
        ("Accepted prefix and complete ending", Stop),
    ]);
    let output = scope
        .clone()
        .delegation_model(model.clone())
        .generate_content(request, true)
        .await
        .unwrap();
    assert_eq!(
        collect_output(output).await.unwrap(),
        "Accepted prefix and complete ending"
    );
    assert_eq!(model.requests.lock().unwrap().len(), 2);
    assert_eq!(summary.0.load(Ordering::SeqCst), 0);
    assert!(
        scope
            .writer
            .get()
            .unwrap()
            .checkpoint
            .output_continuation()
            .unwrap()
            .is_none()
    );
}

#[tokio::test]
async fn structured_continuation_terminal_receipt_contains_joined_answer() {
    use adk_rust::FinishReason::{MaxTokens, Stop};
    let storage = storage(ModelScopeBackend::Local(Arc::new(
        InMemorySessionService::new(),
    )));
    let child = Context::child("structured-receipt");
    let scope = checkpoint(&storage, Arc::new(Summary::default()));
    let mut request = simple_request(&scope, &child).await;
    request.config = Some(adk_rust::GenerateContentConfig {
        response_schema: Some(
            serde_json::json!({"type":"object","properties":{"answer":{"type":"string"}}}),
        ),
        ..Default::default()
    });
    let model = OutputModel::new(vec![
        (r#"{"answer":"First"#, MaxTokens),
        (r#"{"answer":"First complete"}"#, Stop),
    ]);
    let mut stream = output::generate(scope.clone(), model, request, true).unwrap();
    let mut visible = String::new();
    while let Some(response) = stream.next().await {
        let response = response.unwrap();
        if let Some(content) = &response.content {
            for part in &content.parts {
                if let adk_rust::Part::Text { text } = part {
                    visible.push_str(text);
                }
            }
        }
        let mut event = Event::with_id("structured-turn", child.invocation_id());
        event.author = child.agent_name().to_owned();
        event.llm_response = response;
        scope.append(&child, event).await.unwrap();
    }
    let saved = stored(&scope, &child).await;
    let completed = scope
        .completed_content(saved.as_ref(), child.agent_name())
        .unwrap()
        .unwrap();
    let durable: String = completed
        .parts
        .iter()
        .filter_map(adk_rust::Part::text)
        .collect();
    assert_eq!(visible, r#"{"answer":"First complete"}"#);
    assert_eq!(durable, visible);
}
