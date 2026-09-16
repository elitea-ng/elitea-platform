use super::*;
use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::session::{
    AppendEventRequest, CreateRequest, DeleteRequest, GetRequest, InMemorySessionService,
    ListRequest, SessionService,
};
use adk_rust::{BeforeModelResult, LlmResponse, LlmResponseStream};
use async_trait::async_trait;
use serde_json::json;
use std::sync::atomic::{AtomicUsize, Ordering};

use crate::agents::context_budget::{RequestContextBudget, RequestContextUsage};
use crate::agents::model_checkpoint::{CHECKPOINT_KEY, ModelCheckpointWriter};
use crate::agents::request::ModelContextLimits;

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
struct Summary {
    requests: Mutex<Vec<LlmRequest>>,
    fail: bool,
}
#[async_trait]
impl Llm for Summary {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        assert!(!streaming);
        self.requests.lock().unwrap().push(request);
        if self.fail {
            return Err(AdkError::new(
                ErrorComponent::Model,
                ErrorCategory::Unavailable,
                "fixture_summary_interrupted",
                "Simulated interruption during summarization.",
            ));
        }
        Ok(Box::pin(stream::once(async {
            Ok(LlmResponse::new(
                Content::new("model").with_text(crate::agents::context_summary::fixture()),
            ))
        })))
    }
}

fn load_request() -> GetRequest {
    GetRequest {
        app_name: "elitea-agent-v1".into(),
        user_id: "user-1".into(),
        session_id: "session-1".into(),
        num_recent_events: None,
        after: None,
    }
}

async fn create(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .create(CreateRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: "user-1".into(),
            session_id: Some("session-1".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap()
}

fn plan() -> ContextCompactionPlan {
    ContextCompactionPlan {
        max_context_tokens: 8000,
        preserve_recent_messages: 2,
        preserve_system_messages: true,
        summary_instructions: "Preserve work and corrections. {messages}".into(),
    }
}

async fn checkpoint_ready() -> adk_rust::Result<()> {
    Ok(())
}

fn history() -> LlmRequest {
    serde_json::from_value(json!({"model":"fixture", "contents": [
        {"role":"system", "parts":[{"text":"EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"}]},
        {"role":"user", "parts":[{"text":"Original task, preserve this exactly."}]},
        {"role":"model", "parts":[{"text":"earlier draft ".repeat(1400)}]},
        {"role":"user", "parts":[{"text":"Latest correction: use project two."}]},
        {"role":"model", "parts":[{"name":"lookup", "id":"call-one", "args":{"project_id":2}}]},
        {"role":"function", "parts":[{"id":"call-one", "functionResponse":{"name":"lookup", "response":{"status":"ok","output":"evidence ".repeat(900)}}}]},
        {"role":"model", "parts":[{"text":"Use the confirmed evidence."}]},
        {"role":"model", "parts":[{"name":"lookup", "id":"call-two", "args":{"project_id":2}}]},
        {"role":"function", "parts":[{"id":"call-two", "functionResponse":{"name":"lookup", "response":{"status":"failed","error":"not found"}}}]}
    ]})).unwrap()
}

async fn store(sessions: &dyn SessionService, record: Option<&CompactionRecord>) {
    let session = sessions.get(load_request()).await.unwrap();
    let mut event = Event::new("persist-summary");
    event.actions.state_delta.insert(
        STATE_KEY.into(),
        DurableContextCompaction::state_value(record).unwrap(),
    );
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: session.try_identity().unwrap(),
            event,
        })
        .await
        .unwrap();
}

#[tokio::test]
async fn repeated_compaction_reuses_exact_coverage_and_preserves_tool_groups_and_authority() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let original = history();
    let (prepared, record) = compaction
        .prepare(original.clone(), checkpoint_ready)
        .await
        .unwrap();
    let usage = Budget.measure(&prepared).unwrap();
    assert!(usage.estimated_input <= usage.budget.compaction_target());
    assert_eq!(prepared.contents[0].parts, original.contents[0].parts);
    assert_eq!(prepared.contents[1].parts, original.contents[1].parts);
    assert_eq!(prepared.contents[3].parts, original.contents[3].parts);
    assert_eq!(
        prepared.contents[prepared.contents.len() - 2].parts,
        original.contents[7].parts
    );
    assert_eq!(
        prepared.contents.last().unwrap().parts,
        original.contents[8].parts
    );
    let summary_input = serde_json::to_string(&summary.requests.lock().unwrap()[0]).unwrap();
    assert!(summary_input.contains("call-one"));
    assert!(summary_input.contains("evidence"));
    assert!(summary_input.contains("Original task, preserve this exactly."));
    assert!(!summary_input.contains("call-two"));
    assert!(!summary_input.contains("EXACT_SKILL_REVISION"));
    store(&sessions, record.as_ref()).await;
    compaction.committed(record).unwrap();
    let session = sessions.get(load_request()).await.unwrap();
    let restored_summary = Arc::new(Summary::default());
    let restored = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        restored_summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    for request in [original.clone(), prepared.clone()] {
        let (after, _) = restored.prepare(request, checkpoint_ready).await.unwrap();
        assert_eq!(
            serde_json::to_value(after).unwrap(),
            serde_json::to_value(&prepared).unwrap()
        );
    }
    assert!(restored_summary.requests.lock().unwrap().is_empty());
    let mut extended = original.clone();
    extended
        .contents
        .push(Content::new("model").with_text("new work ".repeat(3100)));
    extended
        .contents
        .push(Content::new("user").with_text("Next correction: retain the failed lookup."));
    extended
        .contents
        .push(Content::new("model").with_text("review"));
    extended
        .contents
        .push(Content::new("user").with_text("continue"));
    let (twice, record) = restored
        .prepare(extended.clone(), checkpoint_ready)
        .await
        .unwrap();
    restored.committed(record).unwrap();
    let (repeated, _) = restored.prepare(extended, checkpoint_ready).await.unwrap();
    assert_eq!(
        serde_json::to_value(twice).unwrap(),
        serde_json::to_value(repeated).unwrap()
    );
    assert_eq!(restored_summary.requests.lock().unwrap().len(), 1);
    let mut edited = original;
    edited.contents[2] = Content::new("model").with_text("edited history ".repeat(1400));
    restored.prepare(edited, checkpoint_ready).await.unwrap();
    assert_eq!(restored_summary.requests.lock().unwrap().len(), 2);
}

#[test]
fn cutoff_never_splits_a_tool_group() {
    let history = history();
    let working = &history.contents[2..];
    assert_eq!(complete_prefix(working, 4).unwrap(), 2);
    assert_eq!(complete_prefix(working, 1).unwrap(), 5);
    assert!(complete_prefix(&working[3..], 1).is_err());
}

#[test]
fn coverage_digest_is_stable_across_json_object_key_order() {
    let left: Content = serde_json::from_str(r#"{"role":"model","parts":[{"name":"lookup","id":"one","args":{"z":1,"a":{"y":2,"b":3}}}]}"#).unwrap();
    let right: Content = serde_json::from_str(r#"{"role":"model","parts":[{"name":"lookup","id":"one","args":{"a":{"b":3,"y":2},"z":1}}]}"#).unwrap();
    assert_eq!(
        extend_digest([0; 32], &[left]).unwrap(),
        extend_digest([0; 32], &[right]).unwrap()
    );
}

#[tokio::test]
async fn oversized_protected_input_fails_without_spending_a_summary_call() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction = DurableContextCompaction::new(
        plan(),
        Arc::new(Budget),
        summary.clone(),
        [7; 32],
        session.as_ref(),
    )
    .unwrap();
    let mut request = history();
    request.contents[0] = Content::new("system").with_text("protected ".repeat(4000));
    let result = compaction
        .prepare(request, || async {
            panic!("must fail before summary preparation")
        })
        .await;
    assert!(matches!(result, Err(error) if error.code == "context_budget_exceeded"));
    assert!(summary.requests.lock().unwrap().is_empty());
}

#[tokio::test]
async fn repeated_tool_loop_compaction_does_not_pin_a_previous_summary_as_user_input() {
    let sessions = InMemorySessionService::new();
    let session = create(&sessions).await;
    let summary = Arc::new(Summary::default());
    let compaction =
        DurableContextCompaction::new(plan(), Arc::new(Budget), summary, [7; 32], session.as_ref())
            .unwrap();
    let mut original = history();
    original.contents.remove(3);
    let (_, record) = compaction
        .prepare(original.clone(), checkpoint_ready)
        .await
        .unwrap();
    assert_eq!(record.as_ref().unwrap().replacement.len(), 1);
    compaction.committed(record).unwrap();
    original
        .contents
        .push(Content::new("model").with_text("new work ".repeat(3100)));
    let mut pair = history().contents[7..].to_vec();
    for content in &mut pair {
        for part in &mut content.parts {
            match part {
                Part::FunctionCall { id, .. } | Part::FunctionResponse { id, .. } => {
                    *id = Some("call-three".into());
                }
                _ => {}
            }
        }
    }
    original.contents.extend(pair);
    let (prepared, record) = compaction
        .prepare(original, checkpoint_ready)
        .await
        .unwrap();
    assert_eq!(record.as_ref().unwrap().replacement.len(), 1);
    assert_eq!(
        prepared
            .contents
            .iter()
            .filter(|content| content.role == "user")
            .count(),
        2
    );
}

struct ModelAfterCheckpoint {
    sessions: Arc<dyn SessionService>,
    calls: AtomicUsize,
}
#[async_trait]
impl Llm for ModelAfterCheckpoint {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let session = self.sessions.get(load_request()).await.unwrap();
        assert!(
            session
                .state()
                .get(STATE_KEY)
                .is_some_and(|value| !value.is_null())
        );
        let checkpoint = session.state().get(CHECKPOINT_KEY).unwrap();
        assert_eq!(
            checkpoint["model"]["request"],
            serde_json::to_value(&request).unwrap()
        );
        let encoded = serde_json::to_string(&request).unwrap();
        assert!(encoded.contains("EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"));
        assert!(encoded.contains("Original task, preserve this exactly."));
        assert!(!encoded.contains(&"earlier draft ".repeat(30)));
        Err(AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Unavailable,
            "fixture_after_checkpoint",
            "Simulated process loss before a model result.",
        ))
    }
}

async fn run(sessions: Arc<dyn SessionService>, recover: bool) -> (usize, usize, Vec<String>) {
    run_with_summary_failure(sessions, recover, false).await
}

async fn run_with_summary_failure(
    sessions: Arc<dyn SessionService>,
    recover: bool,
    fail_summary: bool,
) -> (usize, usize, Vec<String>) {
    let session = sessions.get(load_request()).await.unwrap();
    let summary = Arc::new(Summary {
        fail: fail_summary,
        ..Summary::default()
    });
    let compaction = Arc::new(
        DurableContextCompaction::new(
            plan(),
            Arc::new(Budget),
            summary.clone(),
            [7; 32],
            session.as_ref(),
        )
        .unwrap(),
    );
    let mut writer = ModelCheckpointWriter::new(sessions.clone(), "execution".into(), 7, [7; 32])
        .with_request_budget(Some(Arc::new(Budget)))
        .with_context_compaction(Some(compaction));
    if recover {
        writer = writer.restore(session.as_ref()).unwrap();
    }
    let model = Arc::new(ModelAfterCheckpoint {
        sessions: sessions.clone(),
        calls: AtomicUsize::new(0),
    });
    let builder = adk_rust::agent::LlmAgentBuilder::new("agent")
        .model(model.clone())
        .before_model_callback(Box::new(|_, mut request| {
            Box::pin(async move {
                request.contents.insert(
                    0,
                    Content::new("system").with_text("EXACT_SKILL_REVISION_AND_PROJECT_CONTEXT"),
                );
                Ok(BeforeModelResult::Continue(request))
            })
        }));
    let runner = adk_rust::runner::Runner::builder()
        .app_name("elitea-agent-v1")
        .agent(Arc::new(writer.bind(builder).build().unwrap()))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut stream = runner
        .run(
            "user-1".try_into().unwrap(),
            "session-1".try_into().unwrap(),
            Content::new("user").with_text("Continue the actual task."),
        )
        .await
        .unwrap();
    let mut errors = Vec::new();
    while let Some(event) = stream.next().await {
        if let Err(error) = event {
            errors.push(error.code.to_string());
        }
    }
    let summaries = summary.requests.lock().unwrap().len();
    (summaries, model.calls.load(Ordering::SeqCst), errors)
}

async fn seed(sessions: &dyn SessionService) {
    let session = create(sessions).await;
    for content in history().contents.into_iter().skip(1) {
        let mut event = Event::new("prior-turn");
        event.author = if content.role == "user" {
            "user"
        } else {
            "agent"
        }
        .into();
        event.set_content(content);
        sessions
            .append_event_for_identity(AppendEventRequest {
                identity: session.try_identity().unwrap(),
                event,
            })
            .await
            .unwrap();
    }
}

#[tokio::test]
async fn runner_persists_summary_and_request_before_dispatch_and_restores_both() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    seed(sessions.as_ref()).await;
    assert_eq!(
        run(sessions.clone(), false).await,
        (1, 1, vec!["fixture_after_checkpoint".into()])
    );
    let stored = sessions.get(load_request()).await.unwrap();
    assert!(stored.events().all().iter().any(|event| {
        serde_json::to_string(event)
            .unwrap()
            .contains(&"earlier draft ".repeat(30))
    }));
    assert_eq!(
        run(sessions, true).await,
        (0, 1, vec!["fixture_after_checkpoint".into()])
    );
}

struct RejectCompaction(InMemorySessionService);
#[async_trait]
impl SessionService for RejectCompaction {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.0.create(req).await
    }
    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        self.0.get(req).await
    }
    async fn list(&self, req: ListRequest) -> adk_rust::Result<Vec<Box<dyn Session>>> {
        self.0.list(req).await
    }
    async fn delete(&self, req: DeleteRequest) -> adk_rust::Result<()> {
        self.0.delete(req).await
    }
    async fn append_event(&self, session: &str, event: Event) -> adk_rust::Result<()> {
        if event.actions.state_delta.contains_key(STATE_KEY) {
            return Err(invalid_compaction());
        }
        self.0.append_event(session, event).await
    }
}

#[tokio::test]
async fn failed_summary_persistence_prevents_model_dispatch() {
    let sessions: Arc<dyn SessionService> =
        Arc::new(RejectCompaction(InMemorySessionService::new()));
    seed(sessions.as_ref()).await;
    assert_eq!(
        run(sessions.clone(), false).await,
        (1, 0, vec!["context_compaction_invalid".into()])
    );
    let stored = sessions.get(load_request()).await.unwrap();
    assert!(stored.state().get(STATE_KEY).is_none());
    assert_eq!(
        stored.state().get(CHECKPOINT_KEY).unwrap()["phase"],
        "context_pending"
    );
    assert!(
        ModelCheckpointWriter::new(sessions, "execution".into(), 7, [7; 32])
            .restore(stored.as_ref())
            .is_err()
    );
}

#[tokio::test]
async fn postgres_summary_checkpoint_survives_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for durable summary process replacement proof"
        );
        return;
    };
    let db = crate::state::postgres_session_tests::IsolatedPostgres::create(&url).await;
    crate::state::postgres_session_tests::install_schema(&db.pool).await;
    for phase in ["write", "resume", "read"] {
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "agents::context_compaction::tests::postgres_summary_child",
                "--nocapture",
            ])
            .env("ELITEA_COMPACTION_TEST_DATABASE", &db.database_name)
            .env("ELITEA_COMPACTION_TEST_PHASE", phase)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{phase} process failed: {} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
    db.pool.close().await;
}

#[tokio::test]
async fn postgres_summary_child() {
    use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};
    use std::str::FromStr as _;
    let Ok(database) = std::env::var("ELITEA_COMPACTION_TEST_DATABASE") else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let phase = std::env::var("ELITEA_COMPACTION_TEST_PHASE").unwrap();
    let options = sqlx::postgres::PgConnectOptions::from_str(
        &std::env::var("ELITEA_TEST_DATABASE_URL").unwrap(),
    )
    .unwrap()
    .database(&database);
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
        .unwrap();
    let attempt = match phase.as_str() {
        "write" => 1,
        "resume" => 2,
        "read" => 3,
        _ => panic!("invalid phase"),
    };
    let sessions: Arc<dyn SessionService> = Arc::new(
        PostgresSessionService::activate(
            pool.clone(),
            crate::state::postgres_session_tests::authority_for(
                &format!("claim-{attempt}"),
                attempt,
                attempt,
                [u8::try_from(attempt).unwrap(); 32],
            ),
            SessionLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    if phase == "write" {
        seed(sessions.as_ref()).await;
        assert_eq!(
            run_with_summary_failure(sessions, false, true).await,
            (1, 0, vec!["fixture_summary_interrupted".into()])
        );
    } else {
        assert_eq!(
            run(sessions.clone(), true).await,
            (
                usize::from(phase == "resume"),
                1,
                vec!["fixture_after_checkpoint".into()]
            )
        );
        let stored = sessions.get(load_request()).await.unwrap();
        assert!(stored.events().all().iter().any(|event| {
            serde_json::to_string(event)
                .unwrap()
                .contains(&"earlier draft ".repeat(30))
        }));
    }
    pool.close().await;
}
