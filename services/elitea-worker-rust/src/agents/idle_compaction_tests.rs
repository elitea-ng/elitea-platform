use std::sync::Mutex;

use adk_rust::futures::stream;
use adk_rust::session::{CreateRequest, InMemorySessionService};
use adk_rust::{LlmResponse, LlmResponseStream};
use serde_json::json;

use super::*;
use crate::agents::assembly_tests::ordinary_request;
use crate::agents::context_budget::{RequestContextBudget, RequestContextUsage};
use crate::agents::context_management::ContextCompactionPlan;
use crate::agents::request::{AgentExecutionKind, ModelContextLimits};

const DIGEST: [u8; 32] = [7; 32];
const AGENT: &str = "elitea-agent";

fn marker() -> Value {
    json!({"cache_ttl_seconds":300,"request_started_unix_ms":1_762_500_000_000_i64,
        "schema":"elitea.agent.idle-compaction.v1",
        "source_execution_id":"0123456789abcdef0123456789abcdef","source_generation":3})
}

/// The source turn's frozen input with Main's idle compaction fields.
fn idle_payload(marker: Value) -> AgentExecutionPayload {
    let mut payload = ordinary_request(AgentExecutionKind::Application).payload;
    payload.user_input = UserInput::Text(String::new());
    payload.should_continue = false;
    payload.hitl_resume = false;
    payload.hitl_action = None;
    payload.hitl_value = None;
    payload.hitl_decisions.clear();
    payload.checkpoint_id = None;
    payload.is_regenerate = false;
    payload
        .meta
        .insert(IDLE_COMPACTION_META_KEY.to_owned(), marker);
    payload
}

#[test]
fn the_contract_marker_is_admitted() {
    let idle = IdleCompaction::from_payload(&idle_payload(marker()))
        .unwrap()
        .expect("an idle compaction");
    assert_eq!(idle.source(), ("0123456789abcdef0123456789abcdef", 3));
    let mut plain = idle_payload(marker());
    plain.meta.clear();
    assert_eq!(IdleCompaction::from_payload(&plain), Ok(None));
}

#[test]
fn an_unknown_key_or_a_wrong_schema_is_refused() {
    let mut unknown = marker();
    unknown["extra"] = json!(true);
    let mut schema = marker();
    schema["schema"] = json!("elitea.agent.idle-compaction.v2");
    let mut missing = marker();
    missing.as_object_mut().unwrap().remove("source_generation");
    let mut execution = marker();
    execution["source_execution_id"] = json!("0123456789ABCDEF0123456789ABCDEF");
    let mut generation = marker();
    generation["source_generation"] = json!(0);
    for value in [unknown, schema, missing, execution, generation] {
        assert_eq!(
            IdleCompaction::from_payload(&idle_payload(value)),
            Err(IdleCompactionError::MalformedMarker)
        );
    }
}

#[test]
fn a_marker_on_a_turn_with_input_or_a_decision_is_refused() {
    let mut text = idle_payload(marker());
    text.user_input = UserInput::Text("hello".into());
    let mut resume = idle_payload(marker());
    resume.hitl_resume = true;
    let mut regenerate = idle_payload(marker());
    regenerate.is_regenerate = true;
    let mut other_meta = idle_payload(marker());
    other_meta.meta.insert("fanout_wake_v1".into(), json!({}));
    for payload in [text, resume, regenerate, other_meta] {
        assert_eq!(
            IdleCompaction::from_payload(&payload),
            Err(IdleCompactionError::NotIdle)
        );
    }
}

#[test]
fn a_marker_on_a_pipeline_is_refused_before_graph_admission() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload = idle_payload(marker());
    let error =
        super::super::runtime::idle_compaction_admission_error(IdleCompactionError::Pipeline);
    assert_eq!(
        error.code(),
        crate::agents::runtime::NativeAgentAssemblyErrorCode::InvalidInput
    );
    assert_eq!(
        error
            .cause()
            .map(crate::agents::runtime::NativeAgentAssemblyCause::code),
        Some("agent.idle_compaction.pipeline")
    );
    // The route admits it before every other start kind of an ordinary agent.
    assert!(requested(&request.payload));
}

/// Measures the encoded request: 4 bytes per token, 400k window.
struct Budget;
impl ModelRequestBudget for Budget {
    fn measure(&self, request: &LlmRequest) -> adk_rust::Result<RequestContextUsage> {
        RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 400_000,
                max_output_tokens: 8_000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: None,
            }),
            &serde_json::Map::new(),
            Some(8_000),
        )
        .unwrap()
        .unwrap()
        .measure_provider_request(&serde_json::to_vec(request).unwrap(), 64 * 1_048_576)
    }
}

/// Answers each request with the next scripted text; records the requests.
struct Model {
    requests: Mutex<Vec<LlmRequest>>,
    answers: Mutex<Vec<Content>>,
}
impl Model {
    fn answering(answers: Vec<Content>) -> Self {
        Self {
            requests: Mutex::default(),
            answers: Mutex::new(answers),
        }
    }
}
#[async_trait]
impl Llm for Model {
    fn name(&self) -> &'static str {
        "model-1"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        assert!(streaming);
        self.requests.lock().unwrap().push(request);
        let content = self.answers.lock().unwrap().remove(0);
        Ok(Box::pin(stream::once(async move {
            Ok(LlmResponse {
                content: Some(content),
                turn_complete: true,
                finish_reason: Some(adk_rust::FinishReason::Stop),
                ..Default::default()
            })
        })))
    }
}

fn identity() -> AdkIdentity {
    AdkIdentity::new(
        adk_rust::AppName::new("elitea-agent-v1").unwrap(),
        "user-1".try_into().unwrap(),
        "session-1".try_into().unwrap(),
    )
}

/// A session whose last turn sent `request` and was answered with `answer`.
async fn session_with_turn(request: &LlmRequest, answer: &str) -> Arc<dyn SessionService> {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: "user-1".into(),
            session_id: Some("session-1".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let mut user = Event::new("inv-1");
    "user".clone_into(&mut user.author);
    user.set_content(request.contents.last().unwrap().clone());
    let mut marker = Event::new("inv-1");
    "elitea-recovery".clone_into(&mut marker.author);
    marker.actions.state_delta.insert(
        CHECKPOINT_KEY.into(),
        json!({"version":1,"execution_id":"0123456789abcdef0123456789abcdef","generation":3,
            "definition_digest":DIGEST,"invocation_id":"inv-1","phase":"model_pending",
            "model":{"request":request,"tools":request.tools}}),
    );
    marker.actions.state_delta.insert(
        HISTORY_SNAPSHOT_KEY.into(),
        json!({"version":1,"agent_name":AGENT}),
    );
    let mut reply = Event::new("inv-1");
    AGENT.clone_into(&mut reply.author);
    reply.set_content(Content::new("model").with_text(answer));
    for event in [user, marker, reply] {
        sessions
            .append_event_for_identity(AppendEventRequest {
                identity: identity(),
                event,
            })
            .await
            .unwrap();
    }
    sessions
}

/// A request of about `tokens` estimated input tokens.
fn last_request(tokens: usize) -> LlmRequest {
    let mut request = LlmRequest::new(
        "model-1",
        vec![
            Content::new("system").with_text("You are the agent."),
            Content::new("user").with_text("Find call-one and report."),
            Content::new("model").with_text("Working on it."),
            Content::new("user").with_text(format!("Bulk: {}", "x".repeat(tokens * 4))),
        ],
    );
    request
        .tools
        .insert("lookup".into(), json!({"name":"lookup","parameters":{}}));
    request
}

fn run(sessions: Arc<dyn SessionService>, model: Arc<Model>) -> IdleCompactionRun {
    IdleCompactionRun {
        sessions,
        identity: identity(),
        model,
        budget: Arc::new(Budget),
        definition_digest: DIGEST,
        agent_name: AGENT.into(),
        guidance: "Keep corrections.".into(),
        completion: None,
    }
}

async fn session(sessions: &dyn SessionService) -> Box<dyn Session> {
    sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: "user-1".into(),
            session_id: "session-1".into(),
            num_recent_events: None,
            after: None,
        })
        .await
        .unwrap()
}

fn summary() -> Content {
    Content::new("model").with_text(crate::agents::context_summary::fixture())
}

#[tokio::test]
async fn one_cached_call_writes_a_record_the_next_turn_accepts() {
    let request = last_request(70_000);
    let sessions = session_with_turn(&request, "Found call-one.").await;
    let model = Arc::new(Model::answering(vec![summary()]));
    let mut statuses = Vec::new();
    let outcome = run(Arc::clone(&sessions), Arc::clone(&model))
        .run("inv-idle", |event| statuses.push(event))
        .await
        .unwrap();
    assert!(matches!(outcome, IdleOutcome::Compacted { .. }));

    // One request: the previous request exactly, the answer, the prompt.
    let sent = model.requests.lock().unwrap().clone();
    assert_eq!(sent.len(), 1);
    let sent = &sent[0];
    assert_eq!(
        serde_json::to_value(&sent.contents[..request.contents.len()]).unwrap(),
        serde_json::to_value(&request.contents).unwrap()
    );
    assert_eq!(sent.tools, request.tools);
    assert_eq!(sent.config.is_some(), request.config.is_some());
    assert_eq!(
        serde_json::to_value(&sent.contents[request.contents.len()]).unwrap(),
        serde_json::to_value(Content::new("model").with_text("Found call-one.")).unwrap()
    );
    assert_eq!(
        serde_json::to_value(sent.contents.last().unwrap()).unwrap(),
        serde_json::to_value(Content::new("user").with_text(
            crate::agents::context_summary::continuation_prompt("Keep corrections.")
        ))
        .unwrap()
    );

    // Exactly one status, compacted; no transcript event was written.
    assert_eq!(statuses.len(), 1);
    let status = crate::agents::context_status::ModelContextStatus::from_event(&statuses[0])
        .unwrap()
        .unwrap();
    assert_eq!(
        status.phase,
        crate::agents::context_status::ContextPhase::Compacted
    );
    assert!(status.estimated_input_tokens() < 1_000);
    let stored = session(sessions.as_ref()).await;
    let events = stored.events().all();
    assert_eq!(events.len(), 4);
    assert_eq!(events[3].author, IDLE_AUTHOR);
    assert!(events[3].llm_response.content.is_none());
    assert!(!events[3].actions.state_delta.contains_key(CHECKPOINT_KEY));

    // The next ordinary turn's `prepare` splices the record in.
    let compaction = super::super::context_compaction::DurableContextCompaction::new(
        ContextCompactionPlan {
            max_context_tokens: 400_000,
            preserve_recent_messages: 2,
            preserve_system_messages: true,
            summary_instructions: String::new(),
        },
        Arc::new(Budget),
        Arc::new(Model::answering(Vec::new())),
        DIGEST,
        stored.as_ref(),
    )
    .unwrap();
    let mut next = request.clone();
    next.contents
        .push(Content::new("model").with_text("Found call-one."));
    next.contents
        .push(Content::new("user").with_text("And now the next question."));
    let (prepared, record) = compaction
        .prepare(next, |_| async { panic!("no new summary") })
        .await
        .unwrap();
    assert!(record.is_some());
    let texts: Vec<String> = prepared
        .contents
        .iter()
        .map(|content| serde_json::to_string(content).unwrap())
        .collect();
    assert_eq!(prepared.contents.len(), 4);
    assert!(texts[2].contains("Platform context-compaction checkpoint"));
    assert!(texts[3].contains("And now the next question."));
    assert!(!texts.iter().any(|text| text.contains("Bulk: ")));

    // A second idle job over the same history does nothing.
    let again = Arc::new(Model::answering(Vec::new()));
    let outcome = run(sessions, again)
        .run("inv-idle-2", |_| panic!("no status"))
        .await
        .unwrap();
    assert_eq!(outcome, IdleOutcome::Skipped("already_compacted"));
}

#[tokio::test]
async fn a_template_miss_is_asked_once_more() {
    let request = last_request(70_000);
    let sessions = session_with_turn(&request, "Found call-one.").await;
    let model = Arc::new(Model::answering(vec![
        Content::new("model").with_text("Here is a summary in prose."),
        summary(),
    ]));
    run(sessions, Arc::clone(&model))
        .run("inv-idle", |_| {})
        .await
        .unwrap();
    let sent = model.requests.lock().unwrap();
    assert_eq!(sent.len(), 2);
    assert_eq!(
        serde_json::to_value(sent[1].contents.last().unwrap()).unwrap(),
        serde_json::to_value(
            Content::new("user").with_text(crate::agents::context_management::TEMPLATE_RETRY)
        )
        .unwrap()
    );
}

#[tokio::test]
async fn an_answer_that_calls_a_tool_is_refused() {
    let request = last_request(70_000);
    let sessions = session_with_turn(&request, "Found call-one.").await;
    let mut call = Content::new("model");
    call.parts.push(Part::FunctionCall {
        name: "lookup".into(),
        args: json!({}),
        id: Some("call-two".into()),
        thought_signature: None,
    });
    // Called twice: the retry answers the call with a refusal, and a
    // second call refuses the run without running anything.
    let model = Arc::new(Model::answering(vec![call.clone(), call]));
    let error = run(Arc::clone(&sessions), Arc::clone(&model))
        .run("inv-idle", |_| panic!("no status"))
        .await
        .unwrap_err();
    assert_eq!(error.code, "agent.idle_compaction.tool_call");
    assert_eq!(session(sessions.as_ref()).await.events().len(), 3);
    let sent = model.requests.lock().unwrap();
    assert_eq!(sent.len(), 2);
    let retry = sent[1].contents.last().unwrap();
    assert!(matches!(
        &retry.parts[0],
        Part::FunctionResponse { id: Some(id), .. } if id == "call-two"
    ));
    assert!(matches!(retry.parts.last(), Some(Part::Text { .. })));
}

#[tokio::test]
async fn a_request_below_the_threshold_does_nothing() {
    let request = last_request(1_000);
    let sessions = session_with_turn(&request, "Found call-one.").await;
    let model = Arc::new(Model::answering(Vec::new()));
    let outcome = run(Arc::clone(&sessions), Arc::clone(&model))
        .run("inv-idle", |_| panic!("no status"))
        .await
        .unwrap();
    assert_eq!(outcome, IdleOutcome::Skipped("below_threshold"));
    assert!(model.requests.lock().unwrap().is_empty());
    assert_eq!(session(sessions.as_ref()).await.events().len(), 3);
}

#[tokio::test]
async fn a_session_without_an_answered_request_does_nothing() {
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let model = Arc::new(Model::answering(Vec::new()));
    let outcome = run(sessions, Arc::clone(&model))
        .run("inv-idle", |_| panic!("no status"))
        .await
        .unwrap();
    assert_eq!(outcome, IdleOutcome::Skipped("no_session"));
    assert!(model.requests.lock().unwrap().is_empty());
}

#[tokio::test]
async fn a_tool_call_then_a_summary_compacts() {
    let request = last_request(70_000);
    let sessions = session_with_turn(&request, "Found call-one.").await;
    let mut call = Content::new("model");
    call.parts.push(Part::FunctionCall {
        name: "lookup".into(),
        args: json!({}),
        id: Some("call-two".into()),
        thought_signature: None,
    });
    let model = Arc::new(Model::answering(vec![call, summary()]));
    let outcome = run(sessions, Arc::clone(&model))
        .run("inv-idle", |_| {})
        .await
        .unwrap();
    assert!(matches!(outcome, IdleOutcome::Compacted { .. }));
    assert_eq!(model.requests.lock().unwrap().len(), 2);
}
