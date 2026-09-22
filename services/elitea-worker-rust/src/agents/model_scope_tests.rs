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
        ContextCompactionPlan {
            max_context_tokens: 8000,
            preserve_recent_messages: 2,
            preserve_system_messages: true,
            summary_instructions: "Preserve work. {messages}".into(),
        },
        Arc::new(Budget),
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
        Arc::new(Budget),
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
    assert!(
        stored(&scope, &context)
            .await
            .state()
            .get(CHECKPOINT_KEY)
            .is_none()
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
            Arc::new(Budget),
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
        Arc::new(Budget),
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
        Arc::new(Budget),
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
