use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::model::MockLlm;
use adk_rust::session::{GetRequest, InMemorySessionService, SessionService};
use adk_rust::tool::BasicToolset;
use adk_rust::{
    Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part, Tool,
    ToolConfirmationDecision, ToolContext, Toolset,
};
use async_trait::async_trait;
use chrono::Utc;
use serde_json::{Value, json};
use tokio::sync::Barrier;

use super::application_tools::SkippedApplicationChild;
use super::assembly::OrdinaryNoToolProfile;
use super::assembly_tests::{current_text_history, ordinary_request};
use super::direct_hitl::{DirectHitlDecision, DirectHitlDecisionSet};
use super::events::{AgentEventProjectionErrorCode, ApplicationToolPresentationCatalog};
use super::internal_tools::{ASK_USER_GUARDRAIL_TYPE, ASK_USER_TOOL_NAME, InternalToolCatalog};
use super::request::{AgentExecutionKind, UserInput};
use super::runtime::{NativeAgentCompletionSelector, NativeAgentRuntimeErrorCode};
use super::sensitive_tools::SensitiveToolCatalog;
use super::session::{
    ApplicationRuntimeProjection, AuthorizedNativeCommandBinding, BoundOrdinaryAgentModel,
    DurableModelCompletion, NativeSessionBackend, NativeToolExecutionMode, OrdinaryNativeAgentPlan,
    OrdinaryRuntimeBindings, assemble_direct_hitl_resume_with_sessions,
    assemble_direct_hitl_resume_with_sessions_and_applications, assemble_ordinary_native,
    assemble_ordinary_native_with_sessions, assemble_ordinary_native_with_sessions_and_options,
    assemble_ordinary_native_with_sessions_and_runtime_catalogs, reset_session_for_regeneration,
};
use super::unattended::UnattendedGrant;
use crate::protocol::control::test_session_authority_for;
use crate::protocol::node_event::encode_current_node_event_json;
use crate::toolkits::{DelegatedAuthorizationCatalog, ToolAdmissionPolicy};

struct FixtureBoundModel {
    model: Arc<dyn Llm>,
    completed: String,
}

impl BoundOrdinaryAgentModel for FixtureBoundModel {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self) -> Result<String, super::runtime::NativeAgentAssemblyError> {
        Ok(self.completed)
    }
}

fn bound_model(response: &str) -> FixtureBoundModel {
    FixtureBoundModel {
        model: Arc::new(
            MockLlm::new("fixture-model").with_response(LlmResponse::new(
                Content::new("assistant").with_text(response),
            )),
        ),
        completed: response.to_owned(),
    }
}

struct StreamingFixtureLlm {
    response: String,
}

#[async_trait]
impl Llm for StreamingFixtureLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let partial = LlmResponse {
            content: Some(Content::new("model").with_text(&self.response)),
            partial: true,
            ..LlmResponse::default()
        };
        let terminal = LlmResponse {
            finish_reason: Some(FinishReason::Stop),
            turn_complete: true,
            ..LlmResponse::default()
        };
        Ok(Box::pin(adk_rust::futures::stream::iter([
            Ok(partial),
            Ok(terminal),
        ])))
    }
}

struct FixtureDurableCompletion {
    value: String,
}

impl DurableModelCompletion for FixtureDurableCompletion {
    fn snapshot(&self) -> adk_rust::Result<Option<String>> {
        Ok(Some(self.value.clone()))
    }
}

struct StreamingBoundModel {
    model: Arc<dyn Llm>,
    completed: String,
    durable: Arc<dyn DurableModelCompletion>,
}

impl BoundOrdinaryAgentModel for StreamingBoundModel {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self) -> Result<String, super::runtime::NativeAgentAssemblyError> {
        Ok(self.completed)
    }

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
        Some(self.durable.clone())
    }
}

fn streaming_bound_model(response: &str) -> StreamingBoundModel {
    StreamingBoundModel {
        model: Arc::new(StreamingFixtureLlm {
            response: response.to_owned(),
        }),
        completed: response.to_owned(),
        durable: Arc::new(FixtureDurableCompletion {
            value: response.to_owned(),
        }),
    }
}

struct SequencedLlm {
    responses: Mutex<VecDeque<LlmResponse>>,
    calls: Arc<AtomicUsize>,
}

struct CapturingFinalLlm {
    requests: Arc<Mutex<Vec<LlmRequest>>>,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Llm for CapturingFinalLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.requests
            .lock()
            .map_err(|_| adk_rust::AdkError::agent("fixture request lock failed"))?
            .push(request);
        Ok(Box::pin(adk_rust::futures::stream::once(async {
            Ok(LlmResponse::new(
                Content::new("model").with_text("The resumed answer is 42."),
            ))
        })))
    }
}

#[async_trait]
impl Llm for SequencedLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let response = self
            .responses
            .lock()
            .map_err(|_| adk_rust::AdkError::agent("fixture model lock failed"))?
            .pop_front()
            .ok_or_else(|| adk_rust::AdkError::agent("fixture model response missing"))?;
        Ok(Box::pin(adk_rust::futures::stream::once(async move {
            Ok(response)
        })))
    }
}

/// Records each provider request, then answers with `inner`.
struct RequestCapturingLlm {
    inner: Arc<dyn Llm>,
    requests: Arc<Mutex<Vec<LlmRequest>>>,
}

#[async_trait]
impl Llm for RequestCapturingLlm {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.requests
            .lock()
            .map_err(|_| adk_rust::AdkError::agent("fixture request lock failed"))?
            .push(request.clone());
        self.inner.generate_content(request, stream).await
    }
}

struct CountingTool {
    calls: Arc<AtomicUsize>,
    read_only: bool,
}

struct ParallelApplicationProbeTool {
    barrier: Arc<Barrier>,
}

#[async_trait]
impl Tool for ParallelApplicationProbeTool {
    fn name(&self) -> &'static str {
        "elitea_agent_17_v_9"
    }

    fn description(&self) -> &'static str {
        "Fixture saved Application participant."
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        tokio::time::timeout(Duration::from_secs(1), self.barrier.wait())
            .await
            .map_err(|_| adk_rust::AdkError::agent("fixture calls were not concurrent"))?;
        Ok(json!({"task": arguments["task"]}))
    }
}

#[async_trait]
impl Tool for CountingTool {
    fn name(&self) -> &'static str {
        "double"
    }

    fn description(&self) -> &'static str {
        "Double one integer."
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let value = arguments
            .get("value")
            .and_then(Value::as_i64)
            .ok_or_else(|| adk_rust::AdkError::agent("fixture value missing"))?;
        Ok(json!({"value": value * 2}))
    }
}

fn tool_call_response() -> LlmResponse {
    tool_call_response_with(json!({"value": 21}))
}

fn tool_call_response_with(args: Value) -> LlmResponse {
    LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "double".to_owned(),
                args,
                id: Some("call-1".to_owned()),
                thought_signature: None,
            }],
        }),
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    }
}

fn ask_user_call_response() -> LlmResponse {
    LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: ASK_USER_TOOL_NAME.to_owned(),
                args: json!({
                    "questions": [{
                        "question": "Which environment should I use?",
                        "header": "Environment",
                        "options": [
                            {"label": "Staging", "description": "Use the staging project."},
                            {"label": "Production", "description": "Use the production project."}
                        ]
                    }]
                }),
                id: Some("ask-call-1".to_owned()),
                thought_signature: None,
            }],
        }),
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    }
}

fn parallel_application_call_response() -> LlmResponse {
    LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![
                Part::FunctionCall {
                    name: "elitea_agent_17_v_9".to_owned(),
                    args: json!({"task": "Resolve Olivia Lovelace"}),
                    id: Some("application-call-1".to_owned()),
                    thought_signature: None,
                },
                Part::FunctionCall {
                    name: "elitea_agent_17_v_9".to_owned(),
                    args: json!({"task": "Resolve Sasha Grey"}),
                    id: Some("application-call-2".to_owned()),
                    thought_signature: None,
                },
            ],
        }),
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    }
}

fn sensitive_catalog() -> SensitiveToolCatalog {
    sensitive_catalog_with_read_only(true)
}

fn sensitive_catalog_with_read_only(read_only: bool) -> SensitiveToolCatalog {
    let runtime = json!({
        "toolkit_security": {
            "sensitive_tools": {"fixture": ["double"]},
            "sensitive_action_company_name": "Example Org",
            "sensitive_action_message_template":
                "{company_name} must approve {action_name}."
        }
    });
    let policy = ToolAdmissionPolicy::from_runtime_config(
        runtime.as_object().expect("runtime configuration object"),
    )
    .expect("runtime policy");
    SensitiveToolCatalog::fixture(
        "double",
        policy
            .sensitive_tool("fixture", "Fixture Tools", "double")
            .expect("sensitive double policy"),
        read_only,
    )
    .expect("sensitive catalog")
}

#[tokio::test]
async fn invocation_local_session_runs_one_real_adk_turn_and_projects_the_exact_completion() {
    let request = ordinary_request(AgentExecutionKind::Application);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let mut assembled = assemble_ordinary_native(
        bound_model("native response"),
        plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
    )
    .await
    .expect("native assembly");
    let start = assembled
        .project_start(Utc::now())
        .expect("projected start");
    assert_eq!(start.len(), 1);

    let (mut native, mut projector, completion) = assembled.start().expect("native start");
    let mut projected = 0_usize;
    while let Some(event) = native.next_event().await.expect("native event") {
        projected += projector.project(&event).expect("projected event").len();
    }
    assert!(projected >= 3);
    let completion = completion.select().await.expect("selected completion");
    let finish = projector
        .finish_after_eos(completion, Utc::now())
        .expect("projected completion");
    let mut event_types = Vec::new();
    for event in finish {
        let value: serde_json::Value = serde_json::from_slice(
            &encode_current_node_event_json(&event).expect("canonical browser event"),
        )
        .expect("browser event JSON");
        event_types.push(value["type"].as_str().unwrap_or_default().to_owned());
        if value["type"] == "full_message" {
            assert_eq!(value["content"], "native response");
            assert_eq!(value["stream_id"], "conversation-1");
            assert_eq!(value["message_id"], "message-1");
            assert_eq!(value["response_metadata"]["project_id"], 17);
            assert_eq!(value["response_metadata"]["chat_project_id"], 9);
        }
    }
    assert_eq!(
        event_types,
        ["pipeline_finish", "agent_response", "full_message"]
    );
    assert_eq!(
        native
            .next_event()
            .await
            .expect_err("one-use stream")
            .code(),
        NativeAgentRuntimeErrorCode::InvalidState
    );
}

#[tokio::test]
async fn injected_session_restores_existing_history_without_reseeding_the_frozen_snapshot() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload.chat_history = current_text_history();
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let first_plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("first native plan");
    let user_id = first_plan.user_id().to_owned();
    let session_id = first_plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let first_sessions: Arc<dyn SessionService> = sessions.clone();
    let first = assemble_ordinary_native_with_sessions(
        bound_model("first response"),
        first_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        first_sessions,
    )
    .await
    .expect("first assembly");
    drop(first);

    let second_plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("second native plan");
    let second_sessions: Arc<dyn SessionService> = sessions.clone();
    let second = assemble_ordinary_native_with_sessions(
        bound_model("second response"),
        second_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        second_sessions,
    )
    .await
    .expect("restored assembly");
    drop(second);

    let restored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("restored injected session");
    assert_eq!(restored.events().len(), 2);
    let first_id = restored
        .events()
        .at(0)
        .expect("first history event")
        .id
        .clone();
    let second_id = restored
        .events()
        .at(1)
        .expect("second history event")
        .id
        .clone();
    assert!(first_id.starts_with("fh-"));
    assert!(second_id.starts_with("fh-"));
    assert_ne!(first_id, second_id);
}

#[tokio::test]
async fn durable_session_projects_the_previous_assistant_turn_into_the_next_model_request() {
    let mut first_request = ordinary_request(AgentExecutionKind::Adhoc);
    first_request.payload.chat_history.clear();
    first_request.payload.user_input = UserInput::Text("first question".to_owned());
    let first_profile =
        OrdinaryNoToolProfile::validate(&first_request).expect("first ordinary profile");
    let first_plan = OrdinaryNativeAgentPlan::from_authorized(
        &first_request,
        &first_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &first_request.payload.input_attachments,
    )
    .expect("first native plan");
    let sessions = Arc::new(InMemorySessionService::new());
    let first_sessions: Arc<dyn SessionService> = sessions.clone();
    let first = assemble_ordinary_native_with_sessions(
        streaming_bound_model("first answer"),
        first_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        first_sessions,
    )
    .await
    .expect("first assembly");
    let (mut first_run, _projector, first_completion) = first.start().expect("first run");
    while first_run.next_event().await.expect("first event").is_some() {}
    first_completion.select().await.expect("first completion");

    let mut second_request = first_request;
    second_request.payload.user_input = UserInput::Text("second question".to_owned());
    let second_profile =
        OrdinaryNoToolProfile::validate(&second_request).expect("second ordinary profile");
    let second_plan = OrdinaryNativeAgentPlan::from_authorized(
        &second_request,
        &second_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &second_request.payload.input_attachments,
    )
    .expect("second native plan");
    let captured = Arc::new(Mutex::new(Vec::new()));
    let calls = Arc::new(AtomicUsize::new(0));
    let second_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&calls),
        }),
        completed: "second answer".to_owned(),
    };
    let second_sessions: Arc<dyn SessionService> = sessions;
    let second = assemble_ordinary_native_with_sessions(
        second_model,
        second_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        second_sessions,
    )
    .await
    .expect("second assembly");
    let (mut second_run, _projector, second_completion) = second.start().expect("second run");
    while second_run
        .next_event()
        .await
        .expect("second event")
        .is_some()
    {}
    second_completion.select().await.expect("second completion");

    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let requests = captured.lock().expect("captured second request");
    let [request] = requests.as_slice() else {
        panic!("exactly one second-turn request expected");
    };
    assert_eq!(
        request
            .contents
            .iter()
            .map(|content| content.role.as_str())
            .collect::<Vec<_>>(),
        ["user", "model", "user"]
    );
    let visible = request
        .contents
        .iter()
        .map(|content| {
            content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect::<String>()
        })
        .collect::<Vec<_>>();
    assert_eq!(
        visible,
        ["first question", "first answer", "second question"]
    );
}

/// A session store that counts the reads, deletes and creates it serves.
struct CountingSessions {
    inner: Arc<dyn SessionService>,
    gets: AtomicUsize,
    deletes: AtomicUsize,
    creates: AtomicUsize,
}

#[async_trait]
impl SessionService for CountingSessions {
    async fn create(
        &self,
        req: adk_rust::session::CreateRequest,
    ) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
        self.creates.fetch_add(1, Ordering::SeqCst);
        self.inner.create(req).await
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
        self.gets.fetch_add(1, Ordering::SeqCst);
        self.inner.get(req).await
    }

    async fn list(
        &self,
        req: adk_rust::session::ListRequest,
    ) -> adk_rust::Result<Vec<Box<dyn adk_rust::session::Session>>> {
        self.inner.list(req).await
    }

    async fn delete(&self, req: adk_rust::session::DeleteRequest) -> adk_rust::Result<()> {
        self.deletes.fetch_add(1, Ordering::SeqCst);
        self.inner.delete(req).await
    }

    async fn append_event(&self, session_id: &str, event: adk_rust::Event) -> adk_rust::Result<()> {
        self.inner.append_event(session_id, event).await
    }

    async fn append_event_for_identity(
        &self,
        req: adk_rust::session::AppendEventRequest,
    ) -> adk_rust::Result<()> {
        self.inner.append_event_for_identity(req).await
    }
}

/// A session store whose deletes fail while `failing` is set.
struct FailingDeletes {
    inner: Arc<dyn SessionService>,
    failing: std::sync::atomic::AtomicBool,
}

#[async_trait]
impl SessionService for FailingDeletes {
    async fn create(
        &self,
        req: adk_rust::session::CreateRequest,
    ) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
        self.inner.create(req).await
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn adk_rust::session::Session>> {
        self.inner.get(req).await
    }

    async fn list(
        &self,
        req: adk_rust::session::ListRequest,
    ) -> adk_rust::Result<Vec<Box<dyn adk_rust::session::Session>>> {
        self.inner.list(req).await
    }

    async fn delete(&self, req: adk_rust::session::DeleteRequest) -> adk_rust::Result<()> {
        if self.failing.load(Ordering::SeqCst) {
            return Err(adk_rust::AdkError::session("delete failed"));
        }
        self.inner.delete(req).await
    }

    async fn append_event(&self, session_id: &str, event: adk_rust::Event) -> adk_rust::Result<()> {
        self.inner.append_event(session_id, event).await
    }

    async fn append_event_for_identity(
        &self,
        req: adk_rust::session::AppendEventRequest,
    ) -> adk_rust::Result<()> {
        self.inner.append_event_for_identity(req).await
    }
}

/// A pipeline regeneration resets its session right after the frontier: a
/// failed delete is an error that leaves the session, and the same command
/// retried resets it (an absent session is not an error either).
#[tokio::test]
async fn a_pipeline_regeneration_session_reset_is_safe_to_retry() {
    let store = Arc::new(FailingDeletes {
        inner: Arc::new(InMemorySessionService::new()),
        failing: std::sync::atomic::AtomicBool::new(true),
    });
    let sessions: Arc<dyn SessionService> = store.clone();
    let user = adk_rust::UserId::new("user-1").expect("user");
    let session = adk_rust::SessionId::new("session-1").expect("session");
    sessions
        .create(adk_rust::session::CreateRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: "user-1".into(),
            session_id: Some("session-1".into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .expect("session");
    let present = || async {
        sessions
            .get(GetRequest {
                app_name: "elitea-agent-v1".into(),
                user_id: "user-1".into(),
                session_id: "session-1".into(),
                num_recent_events: Some(0),
                after: None,
            })
            .await
            .is_ok()
    };
    assert!(
        reset_session_for_regeneration(sessions.as_ref(), &user, &session)
            .await
            .is_err()
    );
    assert!(present().await, "a failed delete leaves the session");
    store.failing.store(false, Ordering::SeqCst);
    reset_session_for_regeneration(sessions.as_ref(), &user, &session)
        .await
        .expect("retry");
    assert!(!present().await, "the retry reset it");
    reset_session_for_regeneration(sessions.as_ref(), &user, &session)
        .await
        .expect("nothing to reset is not an error");
}

#[tokio::test]
async fn regeneration_rebuilds_the_session_without_the_replaced_assistant_turn() {
    let mut first_request = ordinary_request(AgentExecutionKind::Adhoc);
    first_request.payload.chat_history.clear();
    first_request.payload.user_input = UserInput::Text("question to regenerate".to_owned());
    let first_profile =
        OrdinaryNoToolProfile::validate(&first_request).expect("first ordinary profile");
    let first_plan = OrdinaryNativeAgentPlan::from_authorized(
        &first_request,
        &first_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &first_request.payload.input_attachments,
    )
    .expect("first native plan");
    let sessions = Arc::new(InMemorySessionService::new());
    let first_sessions: Arc<dyn SessionService> = sessions.clone();
    let first = assemble_ordinary_native_with_sessions(
        streaming_bound_model("answer being replaced"),
        first_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        first_sessions,
    )
    .await
    .expect("first assembly");
    let (mut first_run, _projector, first_completion) = first.start().expect("first run");
    while first_run.next_event().await.expect("first event").is_some() {}
    first_completion.select().await.expect("first completion");

    let mut regeneration = first_request;
    regeneration.payload.is_regenerate = true;
    let regeneration_profile =
        OrdinaryNoToolProfile::validate_regeneration(&regeneration).expect("regeneration profile");
    let regeneration_plan = OrdinaryNativeAgentPlan::from_authorized(
        &regeneration,
        &regeneration_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &regeneration.payload.input_attachments,
    )
    .expect("regeneration native plan");
    let captured = Arc::new(Mutex::new(Vec::new()));
    let calls = Arc::new(AtomicUsize::new(0));
    let regeneration_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&calls),
        }),
        completed: "replacement answer".to_owned(),
    };
    let counting = Arc::new(CountingSessions {
        inner: sessions,
        gets: AtomicUsize::new(0),
        deletes: AtomicUsize::new(0),
        creates: AtomicUsize::new(0),
    });
    let regeneration_sessions: Arc<dyn SessionService> = counting.clone();
    let regenerated = assemble_ordinary_native_with_sessions(
        regeneration_model,
        regeneration_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        regeneration_sessions,
    )
    .await
    .expect("regeneration assembly");
    // #1943: one read, one delete and one create.
    assert_eq!(counting.gets.load(Ordering::SeqCst), 1, "one read");
    assert_eq!(counting.deletes.load(Ordering::SeqCst), 1, "one delete");
    assert_eq!(counting.creates.load(Ordering::SeqCst), 1, "one create");
    let (mut regeneration_run, _projector, regeneration_completion) =
        regenerated.start().expect("regeneration run");
    while regeneration_run
        .next_event()
        .await
        .expect("regeneration event")
        .is_some()
    {}
    regeneration_completion
        .select()
        .await
        .expect("regeneration completion");

    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let requests = captured.lock().expect("captured regeneration request");
    let [request] = requests.as_slice() else {
        panic!("exactly one regeneration request expected");
    };
    assert_eq!(request.contents.len(), 1);
    assert_eq!(request.contents[0].role, "user");
    assert_eq!(
        request.contents[0]
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect::<String>(),
        "question to regenerate"
    );
}

#[tokio::test]
async fn session_backend_rejects_a_claim_from_another_execution_before_storage() {
    let request = ordinary_request(AgentExecutionKind::Application);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let result = NativeSessionBackend::invocation_local()
        .open(
            test_session_authority_for("execution-two", 3),
            Arc::new(crate::state::TestStateWriterLease::current()),
            &plan,
        )
        .await;
    let Err(error) = result else {
        panic!("cross-execution session grant was accepted");
    };
    assert_eq!(
        error.code(),
        super::runtime::NativeAgentAssemblyErrorCode::AuthorizationFailed
    );
}

#[tokio::test]
async fn direct_llm_agent_executes_a_bound_toolset_before_the_final_model_turn() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("agent profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let model_calls = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([
                tool_call_response(),
                LlmResponse::new(Content::new("model").with_text("The answer is 42.")),
            ])),
            calls: Arc::clone(&model_calls),
        }),
        completed: "The answer is 42.".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&tool_calls),
        read_only: true,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let assembled =
        assemble_ordinary_native(model, plan, vec![toolset], SensitiveToolCatalog::default())
            .await
            .expect("native assembly");
    let (mut native, _projector, completion) = assembled.start().expect("native start");

    let mut saw_call = false;
    let mut saw_result = false;
    while let Some(event) = native.next_event().await.expect("native event") {
        saw_call |= !event.tool_calls().is_empty();
        saw_result |= !event.tool_results().is_empty();
    }

    assert!(saw_call);
    assert!(saw_result);
    assert_eq!(model_calls.load(Ordering::SeqCst), 2);
    assert_eq!(tool_calls.load(Ordering::SeqCst), 1);
    let completion = completion.select().await.expect("selected completion");
    let _ = completion;
}

#[tokio::test]
async fn application_only_agent_dispatches_repeated_participant_calls_concurrently_in_call_order() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("agent profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let requests = Arc::new(Mutex::new(Vec::new()));
    let model = FixtureBoundModel {
        model: Arc::new(RequestCapturingLlm {
            inner: Arc::new(SequencedLlm {
                responses: Mutex::new(VecDeque::from([
                    parallel_application_call_response(),
                    LlmResponse::new(Content::new("model").with_text("Both names were resolved.")),
                ])),
                calls: Arc::new(AtomicUsize::new(0)),
            }),
            requests: Arc::clone(&requests),
        }),
        completed: "Both names were resolved.".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(ParallelApplicationProbeTool {
        barrier: Arc::new(Barrier::new(2)),
    });
    let toolset: Arc<dyn Toolset> =
        Arc::new(BasicToolset::new("elitea_nested_applications", vec![tool]));
    let mut applications = ApplicationToolPresentationCatalog::default();
    applications
        .insert(
            "elitea_agent_17_v_9".to_owned(),
            "Full Name Resolver".to_owned(),
            "agent".to_owned(),
        )
        .expect("application presentation");
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let assembled = assemble_ordinary_native_with_sessions_and_options(
        model,
        plan,
        vec![toolset],
        SensitiveToolCatalog::default(),
        ApplicationRuntimeProjection::presentations(applications),
        NativeToolExecutionMode::ParallelApplications,
        sessions,
    )
    .await
    .expect("parallel native assembly");
    let (mut native, _projector, completion) = assembled.start().expect("native start");

    let result_ids = tokio::time::timeout(Duration::from_secs(3), async {
        let mut result_ids = Vec::new();
        while let Some(event) = native.next_event().await.expect("native event") {
            result_ids.extend(
                event
                    .tool_results()
                    .into_iter()
                    .filter_map(|result| result.call_id.map(str::to_owned)),
            );
        }
        result_ids
    })
    .await
    .expect("parallel calls completed without serial barrier timeout");

    // ADK 2.3.0 emits (and the Runner commits) each result as its call
    // completes, so the event order is the completion order; each result is
    // emitted exactly once.
    let mut emitted = result_ids;
    emitted.sort();
    assert_eq!(emitted, ["application-call-1", "application-call-2"]);
    let completion = completion.select().await.expect("selected completion");
    let _ = completion;
    // The provider still receives the results in call order.
    let requests = requests.lock().expect("captured provider requests");
    let [_, continuation] = requests.as_slice() else {
        panic!("one call request and one continuation request expected");
    };
    let response_ids: Vec<&str> = continuation
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter_map(|part| match part {
            Part::FunctionResponse { id: Some(id), .. } => Some(id.as_str()),
            _ => None,
        })
        .collect();
    assert_eq!(
        response_ids,
        ["application-call-1", "application-call-2"],
        "ADK must restore provider call order after concurrent completion"
    );
}

#[tokio::test]
async fn sensitive_direct_tool_pauses_before_execution_and_projects_masked_call_identity() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("agent profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let model_calls = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([tool_call_response_with(json!({
                "value": 21,
                "api_token": "must-not-be-published"
            }))])),
            calls: Arc::clone(&model_calls),
        }),
        completed: "must not be selected".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&tool_calls),
        read_only: true,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let mut assembled = assemble_ordinary_native_with_sessions(
        model,
        plan,
        vec![toolset],
        sensitive_catalog(),
        injected_sessions,
    )
    .await
    .expect("native assembly");
    assembled
        .project_start(Utc::now())
        .expect("projected start");
    let (mut native, mut projector, _completion) = assembled.start().expect("native start");

    let mut interrupt = None;
    let mut durable_confirmation_seen = false;
    let mut public_events = Vec::new();
    while let Some(event) = native.next_event().await.expect("native event") {
        let confirmation = event.actions.tool_confirmation.as_ref();
        if confirmation.is_some() {
            assert_confirmation_persisted(&sessions, &user_id, &session_id, &event).await;
            durable_confirmation_seen = true;
        }
        let summary = format!(
            "author={} interrupted={} complete={} calls={} confirmation={} confirmation_id={}",
            event.author,
            event.llm_response.interrupted,
            event.llm_response.turn_complete,
            event.tool_calls().len(),
            confirmation.map_or("none", |value| value.tool_name.as_str()),
            confirmation
                .and_then(|value| value.function_call_id.as_deref())
                .unwrap_or("none"),
        );
        for projected in projector
            .project(&event)
            .unwrap_or_else(|error| panic!("projected event failed ({summary}): {error}"))
        {
            let value: Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("canonical browser event"),
            )
            .expect("browser event JSON");
            if value["type"] == "agent_hitl_interrupt" {
                interrupt = Some(value.clone());
            }
            public_events.push(value);
        }
    }

    assert_sensitive_interrupt_projection(
        &interrupt.expect("sensitive-tool interrupt"),
        &public_events,
    );
    resolve_persisted_sensitive_decision(&sessions, &user_id, &session_id, &public_events).await;
    assert!(projector.is_paused());
    assert!(durable_confirmation_seen);
    assert_eq!(model_calls.load(Ordering::SeqCst), 1);
    assert_eq!(tool_calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep the end-to-end correlated tool-result proof together.
async fn ask_user_pauses_and_resumes_as_the_original_correlated_tool_result() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.internal_tools = vec![ASK_USER_TOOL_NAME.to_owned()];
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ask_user profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("ask_user native plan");
    let internal_tools =
        InternalToolCatalog::from_names(&request.payload.internal_tools).expect("ask_user catalog");
    let model_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([ask_user_call_response()])),
            calls: Arc::clone(&model_calls),
        }),
        completed: "must pause before completion".to_owned(),
    };
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let runtime = OrdinaryRuntimeBindings::new(
        internal_tools.toolsets(None),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_internal_tools(internal_tools);
    let mut assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        model,
        plan,
        runtime,
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("ask_user assembly");
    assembled
        .project_start(Utc::now())
        .expect("ask_user projected start");
    let (mut run, mut projector, _completion) = assembled.start().expect("ask_user start");
    let mut interrupt = None;
    while let Some(event) = run.next_event().await.expect("ask_user event") {
        for projected in projector.project(&event).expect("ask_user projection") {
            let value: Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("ask_user browser event"),
            )
            .expect("ask_user event JSON");
            if value["type"] == "agent_hitl_interrupt" {
                interrupt = Some(value);
            }
        }
    }
    let interrupt = interrupt.expect("ask_user interrupt");
    let pending = &interrupt["response_metadata"]["hitl_interrupts"][0];
    assert_eq!(pending["guardrail_type"], ASK_USER_GUARDRAIL_TYPE);
    assert_eq!(pending["tool_call_id"], "ask-call-1");
    assert_eq!(pending["available_actions"], json!(["answer"]));
    assert_eq!(pending["questions"][0]["id"], "q1");
    assert_eq!(model_calls.load(Ordering::SeqCst), 1);

    let interrupt_id = pending["interrupt_id"]
        .as_str()
        .expect("ask_user public interrupt identity");
    let encoded_answer = r#"{"q1":"Staging"}"#;
    let mut resume_payload = ordinary_request(AgentExecutionKind::Adhoc).payload;
    resume_payload.should_continue = true;
    resume_payload.hitl_resume = true;
    resume_payload.hitl_action = Some("answer".to_owned());
    resume_payload.hitl_value = Some(encoded_answer.to_owned());
    resume_payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "ask-call-1",
        "action": "answer",
        "value": encoded_answer,
    })];
    let decision = DirectHitlDecision::from_payload(&resume_payload).expect("ask_user decision");
    let resume_plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("ask_user resume plan");
    let provider_calls = Arc::new(AtomicUsize::new(0));
    let captured = Arc::new(Mutex::new(Vec::new()));
    let resumed_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&provider_calls),
        }),
        completed: "continued after clarification".to_owned(),
    };
    // The resumed turn also skips `swarm`: its final message must still carry
    // the notice list (the pause event persisted it once; the resume's final
    // message replaces it, and a resume that forgot it would erase the notice).
    let resume_tools =
        InternalToolCatalog::from_values(Some(&serde_json::json!([ASK_USER_TOOL_NAME, "swarm"])))
            .expect("resume internal tools");
    let resume_runtime = OrdinaryRuntimeBindings::new(
        internal_tools.toolsets(None),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_internal_tools(resume_tools);
    let resumed = assemble_direct_hitl_resume_with_sessions_and_applications(
        resumed_model,
        resume_plan,
        resume_runtime,
        DirectHitlDecisionSet::single(decision),
        sessions,
    )
    .await
    .expect("ask_user replay assembly");
    let (mut resumed_run, mut resumed_projector, completion) =
        resumed.start().expect("ask_user replay start");
    let resumed_start = resumed_projector
        .start(chrono::Utc::now())
        .expect("resumed agent start");
    let resumed_start: serde_json::Value = serde_json::from_slice(
        &crate::protocol::node_event::encode_current_node_event_json(
            resumed_start.into_iter().next().as_ref().expect("start"),
        )
        .expect("encode"),
    )
    .expect("start json");
    assert_eq!(
        resumed_start["response_metadata"]["skipped_capabilities"]["items"][0]["name"],
        "swarm"
    );
    let expected =
        Value::String("User answered:\n- Which environment should I use?: Staging".to_owned());
    let mut saw_answer = false;
    while let Some(event) = resumed_run
        .next_event()
        .await
        .expect("ask_user replay event")
    {
        saw_answer |= event.actions.tool_confirmation_decision
            == Some(ToolConfirmationDecision::Approve)
            && event.tool_results().iter().any(|result| {
                result.call_id == Some("ask-call-1")
                    && result.name == ASK_USER_TOOL_NAME
                    && result.response == &expected
            });
    }
    assert!(saw_answer);
    assert_eq!(provider_calls.load(Ordering::SeqCst), 1);
    completion.select().await.expect("ask_user completion");
    let requests = captured.lock().expect("ask_user provider requests");
    assert_eq!(requests.len(), 1);
    let (calls, results) = count_call_parts(&requests[0], "ask-call-1");
    assert_eq!((calls, results), (1, 1));
    assert!(
        requests[0]
            .contents
            .iter()
            .flat_map(|content| &content.parts)
            .any(|part| matches!(
                part,
                Part::FunctionResponse { function_response, id: Some(id), .. }
                    if id == "ask-call-1"
                        && function_response.name == ASK_USER_TOOL_NAME
                        && function_response.response == expected
            ))
    );
}

/// #866: an agent configured with an internal tool this runtime does not
/// implement gets a visible notice IN THE SESSION, not only the
/// `agent_internal_tool_skipped` log line `InternalToolCatalog::from_values`
/// already wrote. Assembly alone triggers it
/// (`seed_skipped_internal_tools_notice` runs before the model's first
/// turn), so this reads the session straight back rather than driving a
/// full model turn.
#[tokio::test]
async fn a_fresh_session_is_seeded_with_a_notice_for_every_skipped_internal_tool() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    // `swarm` and `image_generation`: two platform-catalogue names this
    // runtime does not implement, plus `ask_user`, which IS implemented and
    // must produce no notice of its own.
    request.payload.internal_tools = vec![
        "swarm".to_owned(),
        ASK_USER_TOOL_NAME.to_owned(),
        "image_generation".to_owned(),
    ];
    let profile = OrdinaryNoToolProfile::validate(&request).expect("skipped-tool profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("skipped-tool native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let internal_tools = InternalToolCatalog::from_names(&request.payload.internal_tools)
        .expect("skipped-tool catalog");
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let runtime = OrdinaryRuntimeBindings::new(
        internal_tools.toolsets(None),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_internal_tools(internal_tools);
    let assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        bound_model("unused — assembly alone is under test"),
        plan,
        runtime,
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("skipped-tool assembly");
    drop(assembled);

    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("skipped-tool session");

    let all_events = session.events().all();
    let notice_texts = all_events
        .iter()
        .filter_map(|event| {
            let content = event.content()?;
            if content.role != "tool" {
                return None;
            }
            content.parts.iter().find_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
        })
        .collect::<Vec<_>>();

    assert_eq!(
        notice_texts,
        [
            "internal tool 'image_generation' is not available on this worker\n\
             internal tool 'swarm' is not available on this worker"
        ],
        "expected exactly one role=\"tool\" notice event naming both skipped tools, \
         in PLATFORM_INTERNAL_TOOLS order, and none for ask_user"
    );
}

/// The companion negative case: nothing skipped (`ask_user` alone) means no
/// notice event at all — the fix must not add noise to the common path.
#[tokio::test]
async fn a_fresh_session_with_no_skipped_internal_tools_gets_no_notice() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.internal_tools = vec![ASK_USER_TOOL_NAME.to_owned()];
    let profile = OrdinaryNoToolProfile::validate(&request).expect("no-skip profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("no-skip native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let internal_tools =
        InternalToolCatalog::from_names(&request.payload.internal_tools).expect("no-skip catalog");
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let runtime = OrdinaryRuntimeBindings::new(
        internal_tools.toolsets(None),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_internal_tools(internal_tools);
    let assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        bound_model("unused — assembly alone is under test"),
        plan,
        runtime,
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("no-skip assembly");
    drop(assembled);

    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("no-skip session");

    assert!(
        session
            .events()
            .all()
            .iter()
            .all(|event| event.content().is_none_or(|content| content.role != "tool")),
        "no internal tool was skipped, so no role=\"tool\" notice event should exist"
    );
}

#[tokio::test]
async fn persisted_read_only_sensitive_call_replays_through_native_adk_without_model_replanning() {
    let fixture = pause_read_only_sensitive_call().await;
    let profile = OrdinaryNoToolProfile::validate(&fixture.request).expect("agent profile");
    let decision =
        DirectHitlDecision::from_payload(&approved_resume_payload(&fixture.interrupt_id))
            .expect("approved decision");

    let second_plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("resume native plan");
    let provider_calls = Arc::new(AtomicUsize::new(0));
    let captured = Arc::new(Mutex::new(Vec::new()));
    let second_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&provider_calls),
        }),
        completed: "The resumed answer is 42.".to_owned(),
    };
    let second_tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: true,
    });
    let second_toolset: Arc<dyn Toolset> =
        Arc::new(BasicToolset::new("fixture-tools", vec![second_tool]));
    let second_sessions: Arc<dyn SessionService> = fixture.sessions.clone();
    let resumed = assemble_direct_hitl_resume_with_sessions(
        second_model,
        second_plan,
        vec![second_toolset],
        sensitive_catalog(),
        decision,
        second_sessions,
    )
    .await
    .expect("read-only replay assembly");
    let (mut resumed_run, _projector, completion) = resumed.start().expect("resumed native run");
    let mut saw_approved_result = false;
    while let Some(event) = resumed_run.next_event().await.expect("resumed event") {
        saw_approved_result |= event.actions.tool_confirmation_decision
            == Some(ToolConfirmationDecision::Approve)
            && !event.tool_results().is_empty();
    }

    assert!(saw_approved_result);
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1);
    assert_eq!(provider_calls.load(Ordering::SeqCst), 1);
    {
        let requests = captured.lock().expect("captured provider requests");
        assert_eq!(requests.len(), 1);
        assert_replay_provider_transcript(&requests[0]);
    }
    completion.select().await.expect("resumed completion");

    assert_next_turn_after_resume(&fixture, &captured, &provider_calls).await;

    let advanced = fixture
        .sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: fixture.user_id,
            session_id: fixture.session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("advanced resumed session");
    let replayed =
        DirectHitlDecision::from_payload(&approved_resume_payload(&fixture.interrupt_id))
            .expect("same bounded decision")
            .resolve(advanced.as_ref());
    let Err(replayed) = replayed else {
        panic!("completed decision was replayed again");
    };
    assert_eq!(
        replayed.code(),
        super::direct_hitl::DirectHitlErrorCode::StaleDecision
    );
}

async fn assert_next_turn_after_resume(
    fixture: &PendingDirectReplay,
    captured: &Arc<Mutex<Vec<LlmRequest>>>,
    provider_calls: &Arc<AtomicUsize>,
) {
    // A later user turn restores the raw session, not the replay model.
    let mut next_request = ordinary_request(fixture.request.kind);
    next_request.payload.user_input = UserInput::Text("next question".to_owned());
    let next_profile = OrdinaryNoToolProfile::validate(&next_request).expect("next profile");
    let next_plan = OrdinaryNativeAgentPlan::from_authorized(
        &next_request,
        &next_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &next_request.payload.input_attachments,
    )
    .expect("next plan");
    let next = assemble_ordinary_native_with_sessions(
        FixtureBoundModel {
            model: Arc::new(CapturingFinalLlm {
                requests: Arc::clone(captured),
                calls: Arc::clone(provider_calls),
            }),
            completed: "next answer".into(),
        },
        next_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        fixture.sessions.clone(),
    )
    .await
    .expect("next assembly");
    let (mut next_run, _, next_completion) = next.start().expect("next start");
    while next_run.next_event().await.expect("next event").is_some() {}
    next_completion.select().await.expect("next completion");
    {
        let requests = captured.lock().expect("next request capture");
        assert_eq!(requests.len(), 2);
        assert_eq!(count_call_parts(&requests[1], "call-1"), (1, 1));
        assert!(!requests[1].contents.iter().flat_map(|content| &content.parts).any(|part| {
            matches!(part, Part::Text { text } if text.starts_with("Tool confirmation required for"))
        }), "runtime confirmation instructions must not enter provider history");
        let last = requests[1].contents.last().unwrap();
        assert_eq!(last.role, "user");
        assert_eq!(
            last.parts,
            Content::new("user").with_text("next question").parts
        );
    }
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn block_actions_emit_one_correlated_structured_result_without_executing_the_tool() {
    Box::pin(assert_blocked_direct_replay("reject", "", "denied by user")).await;
    Box::pin(assert_blocked_direct_replay(
        "block_with_comment",
        "retain this record",
        "retain this record",
    ))
    .await;
}

async fn assert_blocked_direct_replay(action: &str, value: &str, expected_reason: &str) {
    let fixture = pause_sensitive_call(false).await;
    let profile = OrdinaryNoToolProfile::validate(&fixture.request).expect("agent profile");
    let expected = expected_blocked_result(expected_reason);
    persist_blocked_result_before_provider(&fixture, &profile, action, value, &expected).await;
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("blocked replay plan");
    let provider_calls = Arc::new(AtomicUsize::new(0));
    let captured = Arc::new(Mutex::new(Vec::new()));
    let model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&provider_calls),
        }),
        completed: "continued after blocked tool".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: false,
    });
    let decision = DirectHitlDecision::from_payload(&blocked_resume_payload(
        &fixture.interrupt_id,
        action,
        value,
    ))
    .expect("blocked replay decision");
    let assembled = assemble_direct_hitl_resume_with_sessions(
        model,
        plan,
        vec![Arc::new(BasicToolset::new("fixture-tools", vec![tool]))],
        sensitive_catalog_with_read_only(false),
        decision,
        fixture.sessions.clone(),
    )
    .await
    .expect("blocked replay assembly");
    let (mut run, _projector, completion) = assembled.start().expect("blocked replay run");
    while let Some(event) = run.next_event().await.expect("blocked replay event") {
        assert_eq!(event.tool_results(), []);
    }
    completion
        .select()
        .await
        .expect("blocked replay completion");
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 0);
    assert_eq!(provider_calls.load(Ordering::SeqCst), 1);
    let requests = captured.lock().expect("blocked provider request");
    assert_eq!(requests.len(), 1);
    assert_blocked_provider_transcript(&requests[0], &expected);
}

async fn persist_blocked_result_before_provider(
    fixture: &PendingDirectReplay,
    profile: &OrdinaryNoToolProfile,
    action: &str,
    value: &str,
    expected: &Value,
) {
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("blocked result plan");
    let provider_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::new(Mutex::new(Vec::new())),
            calls: Arc::clone(&provider_calls),
        }),
        completed: "not reached".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: false,
    });
    let decision = DirectHitlDecision::from_payload(&blocked_resume_payload(
        &fixture.interrupt_id,
        action,
        value,
    ))
    .expect("blocked result decision");
    let assembled = assemble_direct_hitl_resume_with_sessions(
        model,
        plan,
        vec![Arc::new(BasicToolset::new("fixture-tools", vec![tool]))],
        sensitive_catalog_with_read_only(false),
        decision,
        fixture.sessions.clone(),
    )
    .await
    .expect("blocked result assembly");
    let (mut run, _projector, _completion) = assembled.start().expect("blocked result run");
    loop {
        let event = run
            .next_event()
            .await
            .expect("blocked result event")
            .expect("blocked result before EOS");
        if event.actions.tool_confirmation_decision == Some(ToolConfirmationDecision::Deny) {
            let results = event.tool_results();
            assert_eq!(results.len(), 1);
            assert_eq!(results[0].call_id, Some("call-1"));
            assert_eq!(results[0].name, "double");
            assert_eq!(results[0].response, expected);
            break;
        }
    }
    drop(run);
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 0);
    assert_eq!(provider_calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn persisted_read_only_result_continues_after_restart_without_tool_reexecution() {
    let fixture = pause_read_only_sensitive_call().await;
    let profile = OrdinaryNoToolProfile::validate(&fixture.request).expect("agent profile");
    interrupt_replay_before_tool_result(&fixture, &profile).await;
    let first_plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("first resume plan");
    let first_provider_calls = Arc::new(AtomicUsize::new(0));
    let first_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::new(Mutex::new(Vec::new())),
            calls: Arc::clone(&first_provider_calls),
        }),
        completed: "not reached".to_owned(),
    };
    let first_tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: true,
    });
    let first = assemble_direct_hitl_resume_with_sessions(
        first_model,
        first_plan,
        vec![Arc::new(BasicToolset::new(
            "fixture-tools",
            vec![first_tool],
        ))],
        sensitive_catalog(),
        approved_decision(&fixture),
        fixture.sessions.clone(),
    )
    .await
    .expect("first replay assembly");
    let (mut first_run, _projector, _completion) = first.start().expect("first replay run");
    loop {
        let event = first_run
            .next_event()
            .await
            .expect("first replay event")
            .expect("persisted result before EOS");
        if !event.tool_results().is_empty() {
            break;
        }
    }
    drop(first_run);
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1);
    assert_eq!(first_provider_calls.load(Ordering::SeqCst), 0);

    let second_plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("second resume plan");
    let second_provider_calls = Arc::new(AtomicUsize::new(0));
    let captured = Arc::new(Mutex::new(Vec::new()));
    let second_model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::clone(&second_provider_calls),
        }),
        completed: "resumed after persisted result".to_owned(),
    };
    let second_tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: true,
    });
    let second = assemble_direct_hitl_resume_with_sessions(
        second_model,
        second_plan,
        vec![Arc::new(BasicToolset::new(
            "fixture-tools",
            vec![second_tool],
        ))],
        sensitive_catalog(),
        approved_decision(&fixture),
        fixture.sessions.clone(),
    )
    .await
    .expect("second replay assembly");
    let (mut second_run, _projector, completion) = second.start().expect("second replay run");
    while second_run
        .next_event()
        .await
        .expect("second replay event")
        .is_some()
    {}
    completion.select().await.expect("second completion");
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1);
    assert_eq!(second_provider_calls.load(Ordering::SeqCst), 1);
    let requests = captured.lock().expect("captured resumed request");
    assert_eq!(requests.len(), 1);
    assert_eq!(count_call_parts(&requests[0], "call-1"), (1, 1));
}

async fn interrupt_replay_before_tool_result(
    fixture: &PendingDirectReplay,
    profile: &OrdinaryNoToolProfile,
) {
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("interrupted resume plan");
    let provider_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::new(Mutex::new(Vec::new())),
            calls: Arc::clone(&provider_calls),
        }),
        completed: "not reached".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: true,
    });
    let assembled = assemble_direct_hitl_resume_with_sessions(
        model,
        plan,
        vec![Arc::new(BasicToolset::new("fixture-tools", vec![tool]))],
        sensitive_catalog(),
        approved_decision(fixture),
        fixture.sessions.clone(),
    )
    .await
    .expect("interrupted replay assembly");
    let (mut run, _projector, _completion) = assembled.start().expect("interrupted replay run");
    loop {
        let event = run
            .next_event()
            .await
            .expect("interrupted replay event")
            .expect("replayed call before EOS");
        if !event.tool_calls().is_empty() {
            break;
        }
    }
    drop(run);
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 0);
    assert_eq!(provider_calls.load(Ordering::SeqCst), 0);
}

struct PendingDirectReplay {
    request: super::request::AgentExecutionRequest,
    sessions: Arc<InMemorySessionService>,
    user_id: String,
    session_id: String,
    interrupt_id: String,
    tool_calls: Arc<AtomicUsize>,
}

async fn pause_read_only_sensitive_call() -> PendingDirectReplay {
    pause_sensitive_call(true).await
}

async fn pause_sensitive_call(read_only: bool) -> PendingDirectReplay {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("agent profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("first native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&tool_calls),
        read_only,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([tool_call_response()])),
            calls: Arc::new(AtomicUsize::new(0)),
        }),
        completed: "not completed".to_owned(),
    };
    let injected: Arc<dyn SessionService> = sessions.clone();
    let first = assemble_ordinary_native_with_sessions(
        model,
        plan,
        vec![toolset],
        sensitive_catalog_with_read_only(read_only),
        injected,
    )
    .await
    .expect("initial sensitive assembly");
    let (mut run, _projector, _completion) = first.start().expect("initial native run");
    while run
        .next_event()
        .await
        .expect("initial sensitive event")
        .is_some()
    {}
    assert_eq!(tool_calls.load(Ordering::SeqCst), 0);
    let interrupt_id = persisted_interrupt_id(&sessions, &user_id, &session_id).await;
    PendingDirectReplay {
        request,
        sessions,
        user_id,
        session_id,
        interrupt_id,
        tool_calls,
    }
}

async fn persisted_interrupt_id(
    sessions: &InMemorySessionService,
    user_id: &str,
    session_id: &str,
) -> String {
    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: user_id.to_owned(),
            session_id: session_id.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted pending session");
    let confirmation = stored
        .events()
        .all()
        .into_iter()
        .find(|event| event.actions.tool_confirmation.is_some())
        .expect("persisted confirmation");
    let pending = confirmation
        .actions
        .tool_confirmation
        .as_ref()
        .expect("confirmation request");
    super::direct_hitl::sensitive_call_identity(
        &confirmation.invocation_id,
        pending
            .function_call_id
            .as_deref()
            .expect("function call identity"),
        &pending.tool_name,
        &pending.args,
    )
    .expect("public interrupt identity")
    .0
}

fn approved_resume_payload(interrupt_id: &str) -> super::request::AgentExecutionPayload {
    let mut resume = ordinary_request(AgentExecutionKind::Adhoc);
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = Some("approve".to_owned());
    resume.payload.hitl_value = Some(String::new());
    resume.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call-1",
        "action": "approve",
        "value": "",
    })];
    resume.payload
}

fn blocked_resume_payload(
    interrupt_id: &str,
    action: &str,
    value: &str,
) -> super::request::AgentExecutionPayload {
    let mut resume = ordinary_request(AgentExecutionKind::Adhoc);
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = Some(action.to_owned());
    resume.payload.hitl_value = Some(value.to_owned());
    resume.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call-1",
        "action": action,
        "value": value,
    })];
    resume.payload
}

fn expected_blocked_result(reason: &str) -> Value {
    json!({
        "type": "sensitive_tool_blocked",
        "blocked_tool_name": "double",
        "blocked_toolkit_name": "Fixture Tools",
        "blocked_toolkit_type": "fixture",
        "denial_reason": reason,
        "message": "You declined THIS specific call to Fixture Tools.double. It was not executed. The block is for THIS invocation only, not the tool itself. This is NOT a stop signal — do not end your turn or summarize yet. Do not retry this same call with the same arguments, but DO continue: if more items remain, call the tool again for the NEXT item now; otherwise use another available tool to keep making progress. Only stop and ask the user when nothing remains that can be done without this exact declined call."
    })
}

fn approved_decision(fixture: &PendingDirectReplay) -> DirectHitlDecision {
    DirectHitlDecision::from_payload(&approved_resume_payload(&fixture.interrupt_id))
        .expect("approved replay decision")
}

fn count_call_parts(request: &LlmRequest, call_id: &str) -> (usize, usize) {
    request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .fold((0, 0), |(calls, results), part| match part {
            Part::FunctionCall { id: Some(id), .. } if id == call_id => (calls + 1, results),
            Part::FunctionResponse { id: Some(id), .. } if id == call_id => (calls, results + 1),
            _ => (calls, results),
        })
}

fn assert_replay_provider_transcript(request: &LlmRequest) {
    let approval_messages = request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter(|part| {
            matches!(
                part,
                Part::Text { text }
                    if text.starts_with("[Elitea direct HITL hitl_e1:")
                        && text.ends_with(
                            "] The pending tool call was approved. Continue the original request."
                        )
            )
        })
        .count();
    let replayed_calls = request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter(|part| {
            matches!(
                part,
                Part::FunctionCall { name, id: Some(id), args, .. }
                    if name == "double" && id == "call-1" && args == &json!({"value": 21})
            )
        })
        .count();
    let results = request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter(|part| {
            matches!(
                part,
                Part::FunctionResponse { function_response, id: Some(id), .. }
                    if function_response.name == "double"
                        && id == "call-1"
                        && function_response.response == json!({"value": 42})
            )
        })
        .count();
    assert_eq!(approval_messages, 0);
    assert_eq!(replayed_calls, 1);
    assert_eq!(results, 1);
}

fn assert_blocked_provider_transcript(request: &LlmRequest, expected: &Value) {
    let replay_markers = request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter(
            |part| matches!(part, Part::Text { text } if text.starts_with("[Elitea direct HITL ")),
        )
        .count();
    let results = request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter(|part| {
            matches!(
                part,
                Part::FunctionResponse { function_response, id: Some(id), .. }
                    if function_response.name == "double"
                        && id == "call-1"
                        && &function_response.response == expected
            )
        })
        .count();
    assert_eq!(replay_markers, 0);
    assert_eq!(results, 1);
}

async fn resolve_persisted_sensitive_decision(
    sessions: &InMemorySessionService,
    user_id: &str,
    session_id: &str,
    public_events: &[Value],
) {
    let interrupt_id = public_events
        .iter()
        .find(|event| event["type"] == "agent_hitl_interrupt")
        .and_then(|event| event["response_metadata"]["hitl_interrupts"][0]["interrupt_id"].as_str())
        .expect("public interrupt identity");
    let mut resume = ordinary_request(AgentExecutionKind::Adhoc);
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = Some("approve".to_owned());
    resume.payload.hitl_value = Some(String::new());
    resume.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call-1",
        "action": "approve",
        "value": "",
    })];
    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: user_id.to_owned(),
            session_id: session_id.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("persisted pending session");
    let resolved = DirectHitlDecision::from_payload(&resume.payload)
        .expect("authorized continuation shape")
        .resolve(stored.as_ref())
        .expect("exact persisted call");
    assert_eq!(resolved.call_id(), "call-1");
    assert_eq!(resolved.arguments()["api_token"], "must-not-be-published");
}

async fn assert_confirmation_persisted(
    sessions: &InMemorySessionService,
    user_id: &str,
    session_id: &str,
    event: &adk_rust::Event,
) {
    let stored = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: user_id.to_owned(),
            session_id: session_id.to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("confirmation event persisted before Runner yield");
    let stored_events = stored.events().all();
    assert_eq!(
        stored_events.last().map(|stored| stored.id.as_str()),
        Some(event.id.as_str())
    );
    assert!(stored_events.iter().any(|stored| {
        stored
            .tool_calls()
            .iter()
            .any(|call| call.call_id == Some("call-1"))
    }));
}

fn assert_sensitive_interrupt_projection(interrupt: &Value, public_events: &[Value]) {
    let pending = &interrupt["response_metadata"]["hitl_interrupts"][0];
    assert_eq!(pending["guardrail_type"], "sensitive_tool");
    assert_eq!(pending["tool_call_id"], "call-1");
    assert_eq!(pending["toolkit_type"], "fixture");
    assert_eq!(pending["toolkit_name"], "Fixture Tools");
    assert_eq!(pending["tool_args"]["value"], 21);
    assert_eq!(pending["tool_args"]["api_token"], "***");
    assert!(pending.get("tool_args_raw").is_none());
    let encoded_public_events = serde_json::to_string(public_events).expect("public event JSON");
    assert!(!encoded_public_events.contains("must-not-be-published"));
    let tool_start = public_events
        .iter()
        .find(|event| event["type"] == "agent_tool_start")
        .expect("sensitive tool start");
    assert_eq!(
        tool_start["response_metadata"]["tool_inputs"]["api_token"],
        "***"
    );
    assert!(
        pending["interrupt_id"]
            .as_str()
            .is_some_and(|value| value.starts_with("hitl_e1:"))
    );
    assert!(
        pending["call_digest"]
            .as_str()
            .is_some_and(|value| value.starts_with("sha256:"))
    );
}

#[test]
fn pseudonymous_session_identity_is_stable_per_thread_and_separated_between_threads() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let first = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("first plan");
    let same = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("same plan");
    assert_eq!(first.session_id(), same.session_id());
    assert!(!first.session_id().contains("tenant-1"));
    assert!(!first.session_id().contains("user:42"));

    let mut other_request = ordinary_request(AgentExecutionKind::Adhoc);
    other_request.payload.thread_id = Some("thread-2".to_owned());
    let other_profile =
        OrdinaryNoToolProfile::validate(&other_request).expect("other ordinary profile");
    let other = OrdinaryNativeAgentPlan::from_authorized(
        &other_request,
        &other_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &other_request.payload.input_attachments,
    )
    .expect("other plan");
    assert_ne!(first.session_id(), other.session_id());
}

#[test]
fn output_continuation_keeps_the_session_and_replaces_the_user_turn_with_a_bounded_instruction() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let fresh = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("fresh plan");

    let mut continuation = request;
    continuation.payload.should_continue = true;
    continuation.payload.truncated_content =
        Some("A durable worker stores progress before it acknowledges each command.".to_owned());
    let continuation_profile = OrdinaryNoToolProfile::validate_output_continuation(&continuation)
        .expect("output continuation profile");
    let continued = OrdinaryNativeAgentPlan::from_authorized_output_continuation(
        &continuation,
        &continuation_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &continuation.payload.input_attachments,
    )
    .expect("output continuation plan");

    assert_eq!(fresh.session_id(), continued.session_id());
    let prompt = continued.user_text();
    assert!(prompt.contains("Continue exactly where it stopped."));
    assert!(prompt.contains("Original request:\ncurrent"));
    assert!(prompt.contains(
        "<visible-assistant-output>\nA durable worker stores progress before it acknowledges each command.\n</visible-assistant-output>"
    ));

    continuation.payload.truncated_content = Some(String::new());
    let reasoning_only_profile = OrdinaryNoToolProfile::validate_output_continuation(&continuation)
        .expect("reasoning-only continuation profile");
    let reasoning_only = OrdinaryNativeAgentPlan::from_authorized_output_continuation(
        &continuation,
        &reasoning_only_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &continuation.payload.input_attachments,
    )
    .expect("reasoning-only continuation plan");
    assert!(
        reasoning_only
            .user_text()
            .contains("produced no visible output. Answer now, directly.")
    );
}

#[test]
fn input_attachments_are_spliced_into_the_human_message_after_the_user_text() {
    // #606: an attached file's header chunk reaches the model AFTER the user's
    // own text. `user_text()` concatenates every text part, so a header spliced
    // in after the prompt shows up in it. The document is flagged
    // `needs_content_extraction` and carries no text beside it — the native
    // runtime cannot read it — so the file is ANNOUNCED (its name reaches the
    // model) while its content is not, which is exactly what the streaming e2e
    // asserts for the rust leg.
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.input_attachments = vec![json!({
        "type": "text",
        "text": "Bucket: chat-attachments\nFilename: conv/report.txt\nfilepath: /chat-attachments/conv/report.txt",
        "elitea_attachment": {
            "needs_content_extraction": true,
            "bucket": "chat-attachments",
            "name": "conv/report.txt",
            "filepath": "/chat-attachments/conv/report.txt",
            "item_id": "11111111-1111-1111-1111-111111111111"
        }
    })];
    let profile = OrdinaryNoToolProfile::validate(&request).expect("attachment profile admitted");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("attachment plan");

    let composed = plan.user_text();
    let user_offset = composed.find("current").expect("user text is present");
    let header_offset = composed
        .find("report.txt")
        .expect("the file is announced to the model");
    assert!(
        user_offset < header_offset,
        "the attachment header must follow the user's own text"
    );
    assert!(
        !composed.contains("elitea_attachment"),
        "the extraction marker must not reach the model"
    );
}

/// One stored attachment as main projects it into `chat_history` and into
/// `input_attachments`: the header chunk that keeps its `elitea_attachment`
/// marker, then the document text.
fn stored_attachment_chunks(file: &str, text: &str) -> Vec<Value> {
    vec![
        json!({
            "type": "text",
            "text": format!("Bucket: chat-attachments\nFilename: conv/{file}\nfilepath: /chat-attachments/conv/{file}"),
            "elitea_attachment": {
                "needs_content_extraction": true,
                "bucket": "chat-attachments",
                "name": format!("conv/{file}"),
                "filepath": format!("/chat-attachments/conv/{file}"),
                "item_id": "11111111-1111-1111-1111-111111111111"
            }
        }),
        json!({"type": "text", "text": text}),
    ]
}

fn visible_texts(request: &LlmRequest) -> Vec<String> {
    request
        .contents
        .iter()
        .map(|content| {
            content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect::<String>()
        })
        .collect()
}

/// Demo issue 2: the second and third questions in a chat that already holds
/// a file failed with `INVALID_INPUT`, because main's history projection keeps
/// the stored header chunk's `elitea_attachment` marker and history admission
/// refused the third key. Each turn here gets the history main would send,
/// with and without a new attachment, over one durable session.
#[tokio::test]
async fn follow_up_turns_in_a_chat_with_attachments_are_admitted_and_answered() {
    let sessions = Arc::new(InMemorySessionService::new());
    let first_file = stored_attachment_chunks("msa.pdf", "Notice period: 90 days.");
    let second_file = stored_attachment_chunks("sla.pdf", "Service credit: 10%.");
    let user_message = |question: &str, files: &[Vec<Value>]| {
        let mut content = vec![json!({"type": "text", "text": question})];
        for file in files {
            content.extend(file.iter().cloned());
        }
        json!({"role": "user", "content": content, "additional_kwargs": {}})
    };
    let answer = |text: &str| json!({"role": "assistant", "content": [{"type": "text", "text": text}], "additional_kwargs": {}});
    let turns = [
        ("What is the notice period?", Vec::new(), first_file.clone()),
        (
            "And the early termination fee?",
            vec![
                user_message(
                    "What is the notice period?",
                    std::slice::from_ref(&first_file),
                ),
                answer("The resumed answer is 42."),
            ],
            Vec::new(),
        ),
        (
            "Compare it with the SLA credit.",
            vec![
                user_message(
                    "What is the notice period?",
                    std::slice::from_ref(&first_file),
                ),
                answer("The resumed answer is 42."),
                user_message("And the early termination fee?", &[]),
                answer("The resumed answer is 42."),
            ],
            second_file.clone(),
        ),
    ];
    for (turn, (question, history, attachments)) in turns.into_iter().enumerate() {
        let mut request = ordinary_request(AgentExecutionKind::Adhoc);
        request.payload.chat_history = history;
        request.payload.input_attachments = attachments;
        request.payload.user_input = UserInput::Text(question.to_owned());
        let profile = OrdinaryNoToolProfile::validate(&request)
            .unwrap_or_else(|error| panic!("turn {} must be admitted: {error:?}", turn + 1));
        let plan = OrdinaryNativeAgentPlan::from_authorized(
            &request,
            &profile,
            &AuthorizedNativeCommandBinding::fixture(),
            &request.payload.input_attachments,
        )
        .expect("native plan");
        let captured = Arc::new(Mutex::new(Vec::new()));
        let model = FixtureBoundModel {
            model: Arc::new(CapturingFinalLlm {
                requests: Arc::clone(&captured),
                calls: Arc::new(AtomicUsize::new(0)),
            }),
            completed: "The resumed answer is 42.".to_owned(),
        };
        let session_service: Arc<dyn SessionService> = sessions.clone();
        let assembled = assemble_ordinary_native_with_sessions(
            model,
            plan,
            Vec::new(),
            SensitiveToolCatalog::default(),
            session_service,
        )
        .await
        .expect("assembly");
        let (mut run, _projector, completion) = assembled.start().expect("run");
        while run.next_event().await.expect("event").is_some() {}
        completion.select().await.expect("completion");

        let requests = captured.lock().expect("captured requests");
        let [model_request] = requests.as_slice() else {
            panic!("exactly one model request per turn");
        };
        let texts = visible_texts(model_request);
        let all = texts.join("\n");
        assert!(texts.last().is_some_and(|last| last.starts_with(question)));
        // The first file's text stays in front of the model on every turn.
        assert!(
            all.contains("Notice period: 90 days."),
            "turn {}: {all}",
            turn + 1
        );
        assert_eq!(turn == 2, all.contains("Service credit: 10%."));
        assert!(!all.contains("elitea_attachment"));
        assert!(!all.contains("needs_content_extraction"));
    }
}

/// A fresh session is seeded from `chat_history`. A history attachment chunk
/// reaches the seed as its text, without the marker. A malformed marker is
/// still refused at admission.
#[test]
fn history_attachment_markers_are_admitted_stripped_and_still_validated() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    let mut content = vec![json!({"type": "text", "text": "earlier question"})];
    content.extend(stored_attachment_chunks(
        "msa.pdf",
        "Notice period: 90 days.",
    ));
    request.payload.chat_history = vec![
        json!({"role": "user", "content": content, "additional_kwargs": {}}),
        json!({"role": "assistant", "content": [{"type": "text", "text": "90 days"}], "additional_kwargs": {}}),
    ];
    let profile = OrdinaryNoToolProfile::validate(&request).expect("history with a file");
    let history = profile.chat_history();
    assert_eq!(history.len(), 2);
    assert_eq!(history[0].parts.len(), 3);
    let seeded = format!("{:?}", history[0]);
    assert!(seeded.contains("Filename: conv/msa.pdf"));
    assert!(seeded.contains("Notice period: 90 days."));
    assert!(!seeded.contains("elitea_attachment"));

    for marker in [
        json!("not an object"),
        json!({"needs_content_extraction": "yes"}),
        json!({"needs_content_extraction": true, "bucket": "chat-attachments"}),
        json!({"bucket": "x".repeat(257)}),
    ] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        request.payload.chat_history = vec![json!({
            "role": "user",
            "content": [{"type": "text", "text": "header", "elitea_attachment": marker}],
            "additional_kwargs": {}
        })];
        assert_eq!(
            OrdinaryNoToolProfile::validate(&request)
                .expect_err("malformed history marker")
                .code(),
            super::runtime::NativeAgentAssemblyErrorCode::InvalidInput
        );
    }
    // Any other extra key is still outside the frozen history shape.
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.payload.chat_history = vec![json!({
        "role": "user",
        "content": [{"type": "text", "text": "header", "cache_control": {}}],
        "additional_kwargs": {}
    })];
    assert_eq!(
        OrdinaryNoToolProfile::validate(&request)
            .expect_err("unknown history key")
            .code(),
        super::runtime::NativeAgentAssemblyErrorCode::InvalidInput
    );
}

#[test]
fn session_definition_lineage_is_request_independent_and_application_version_scoped() {
    let mut first_request = ordinary_request(AgentExecutionKind::Application);
    let first_profile =
        OrdinaryNoToolProfile::validate(&first_request).expect("first application profile");
    let first = OrdinaryNativeAgentPlan::from_authorized(
        &first_request,
        &first_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &first_request.payload.input_attachments,
    )
    .expect("first application plan");

    first_request.binding.request_content_digest = [9; 32];
    first_request.payload.user_input = UserInput::Text("continued input".to_owned());
    first_request.payload.chat_history = current_text_history();
    let continued_profile =
        OrdinaryNoToolProfile::validate(&first_request).expect("continued application profile");
    let continued = OrdinaryNativeAgentPlan::from_authorized(
        &first_request,
        &continued_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &first_request.payload.input_attachments,
    )
    .expect("continued application plan");
    assert_eq!(first.definition_digest(), continued.definition_digest());

    first_request
        .payload
        .application
        .insert("version_id".to_owned(), json!(23));
    let changed_profile =
        OrdinaryNoToolProfile::validate(&first_request).expect("changed application profile");
    let changed = OrdinaryNativeAgentPlan::from_authorized(
        &first_request,
        &changed_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &first_request.payload.input_attachments,
    )
    .expect("changed application plan");
    assert_ne!(first.definition_digest(), changed.definition_digest());

    let mut adhoc_request = ordinary_request(AgentExecutionKind::Adhoc);
    let adhoc_profile =
        OrdinaryNoToolProfile::validate(&adhoc_request).expect("first ad-hoc profile");
    let adhoc = OrdinaryNativeAgentPlan::from_authorized(
        &adhoc_request,
        &adhoc_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &adhoc_request.payload.input_attachments,
    )
    .expect("first ad-hoc plan");
    adhoc_request.binding.request_content_digest = [7; 32];
    adhoc_request.payload.user_input = UserInput::Text("next ad-hoc turn".to_owned());
    adhoc_request.payload.llm["kwargs"]["model"] = json!("another-model");
    let next_adhoc_profile =
        OrdinaryNoToolProfile::validate(&adhoc_request).expect("next ad-hoc profile");
    let next_adhoc = OrdinaryNativeAgentPlan::from_authorized(
        &adhoc_request,
        &next_adhoc_profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &adhoc_request.payload.input_attachments,
    )
    .expect("next ad-hoc plan");
    assert_eq!(adhoc.definition_digest(), next_adhoc.definition_digest());
    assert_ne!(first.definition_digest(), adhoc.definition_digest());
}

#[test]
fn invalid_completed_content_never_becomes_a_browser_terminal() {
    let Err(error) = super::events::CompletedAgentBrowserOutput::ordinary(
        "nul\0byte".to_owned(),
        "thread-1".to_owned(),
    ) else {
        panic!("a NUL model result was accepted");
    };
    assert_eq!(error.code(), AgentEventProjectionErrorCode::InvalidOutput);
}

struct InterruptedAfterToolLlm {
    calls: AtomicUsize,
}

#[async_trait]
impl Llm for InterruptedAfterToolLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }
    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if self.calls.fetch_add(1, Ordering::SeqCst) == 0 {
            return Ok(Box::pin(adk_rust::futures::stream::once(async {
                Ok(tool_call_response())
            })));
        }
        Ok(Box::pin(async_stream::stream! {
            let mut response = LlmResponse::new(Content::new("model").with_text("partial"));
            response.partial = true;
            response.turn_complete = false;
            yield Ok(response);
            yield Err(adk_rust::AdkError::agent("fixture transport interruption"));
        }))
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One interrupted turn and its recovery share the fixture.
async fn interrupted_model_restores_saved_request_without_repeating_tool_or_user_turn() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.chat_history.clear();
    let profile = OrdinaryNoToolProfile::validate(&request).expect("profile");
    let plan = || {
        OrdinaryNativeAgentPlan::from_authorized(
            &request,
            &profile,
            &AuthorizedNativeCommandBinding::fixture(),
            &request.payload.input_attachments,
        )
        .expect("plan")
    };
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let tools = || -> Vec<Arc<dyn Toolset>> {
        vec![Arc::new(BasicToolset::new(
            "recovery-tools",
            vec![Arc::new(CountingTool {
                calls: tool_calls.clone(),
                read_only: true,
            })],
        ))]
    };
    let first = assemble_ordinary_native_with_sessions(
        FixtureBoundModel {
            model: Arc::new(InterruptedAfterToolLlm {
                calls: AtomicUsize::new(0),
            }),
            completed: String::new(),
        },
        plan(),
        tools(),
        SensitiveToolCatalog::default(),
        sessions.clone(),
    )
    .await
    .expect("first assembly");
    let (mut run, _, _) = first.start().expect("first start");
    let mut failed = false;
    loop {
        match run.next_event().await {
            Ok(Some(_)) => {}
            Ok(None) => break,
            Err(_) => {
                failed = true;
                break;
            }
        }
    }
    assert!(failed);
    drop(run);
    assert_eq!(tool_calls.load(Ordering::SeqCst), 1);
    let captured = Arc::new(Mutex::new(Vec::new()));
    let model_calls = Arc::new(AtomicUsize::new(0));
    let restored_plan = plan();
    let user_id = restored_plan.user_id().to_string();
    let session_id = restored_plan.session_id().to_string();
    let restore = || {
        super::session::assemble_ordinary_native_from_checkpoint(
            FixtureBoundModel {
                model: Arc::new(CapturingFinalLlm {
                    requests: captured.clone(),
                    calls: model_calls.clone(),
                }),
                completed: "The resumed answer is 42.".into(),
            },
            plan(),
            OrdinaryRuntimeBindings::new(
                tools(),
                SensitiveToolCatalog::default(),
                DelegatedAuthorizationCatalog::default(),
                ApplicationRuntimeProjection::default(),
            ),
            NativeToolExecutionMode::Sequential,
            sessions.clone(),
        )
    };
    let denied = restore().await.expect("recovery assembly");
    let mismatched =
        super::session::ValidatedModelCheckpoint::test_evidence("execution-one".into(), 3);
    let (claim, control) = crate::protocol::control::test_checkpoint_authorizer(
        "execution-one",
        3,
        mismatched.digest(),
    );
    let wrong_authority = claim
        .authorize(&control, mismatched)
        .await
        .unwrap_or_else(|_| panic!("fixture authorization"));
    assert!(denied.authorize(wrong_authority).is_err());
    assert_eq!(model_calls.load(Ordering::SeqCst), 0);
    let evidence = NativeSessionBackend::injected(sessions.clone())
        .inspect_model_checkpoint(
            test_session_authority_for("execution-one", 3),
            Arc::new(crate::state::TestStateWriterLease::current()),
            &plan(),
        )
        .await
        .expect("checkpoint inspection");
    assert_eq!(model_calls.load(Ordering::SeqCst), 0);
    let (claim, control) =
        crate::protocol::control::test_checkpoint_authorizer("execution-one", 3, evidence.digest());
    let authority = claim
        .authorize(&control, evidence)
        .await
        .unwrap_or_else(|_| panic!("checkpoint authorization failed"));
    let restored = restore().await.expect("recovery assembly");
    let (restored, _authority) = restored.authorize(authority).expect("matching checkpoint");
    let (mut run, _, completion) = restored.start().expect("recovery start");
    while run.next_event().await.expect("recovery event").is_some() {}
    completion.select().await.expect("completion");
    assert_eq!(tool_calls.load(Ordering::SeqCst), 1);
    assert_eq!(model_calls.load(Ordering::SeqCst), 1);
    {
        let requests = captured.lock().expect("requests");
        assert!(requests[0].contents.iter().any(|c| {
            c.parts
                .iter()
                .any(|p| matches!(p, Part::FunctionResponse { id: Some(id), .. } if id == "call-1"))
        }));
    }
    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".into(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("session");
    assert_eq!(
        session
            .events()
            .all()
            .iter()
            .filter(|event| event.author == "user")
            .count(),
        1
    );
}

#[tokio::test]
async fn checkpoint_inspection_admits_summary_preparation_before_model_binding() {
    use adk_rust::session::{AppendEventRequest, CreateRequest};
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.context_settings = json!({"enabled":true}).as_object().unwrap().clone();
    request.payload.model_context_limits = Some(super::request::ModelContextLimits {
        context_window_tokens: 32_000,
        max_output_tokens: 8_000,
        context_window_fallback: false,
        max_output_fallback: false,
        max_input_tokens: None,
    });
    let profile = OrdinaryNoToolProfile::validate(&request).unwrap();
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &[],
    )
    .unwrap();
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let session = sessions
        .create(CreateRequest {
            app_name: "elitea-agent-v1".into(),
            user_id: plan.user_id().into(),
            session_id: Some(plan.session_id().into()),
            state: std::collections::HashMap::default(),
        })
        .await
        .unwrap();
    let pending = LlmRequest::new(
        "fixture-model",
        vec![Content::new("user").with_text("Exact input awaiting summary.")],
    );
    let checkpoint = json!({
        "version":1, "execution_id":"execution-one", "generation":3,
        "definition_digest":plan.definition_digest(), "invocation_id":"interrupted-summary",
        "phase":"context_pending", "model":{"request":pending,"tools":{}},
    });
    let mut marker = adk_rust::Event::new("interrupted-summary");
    marker.author = "elitea-recovery".into();
    marker
        .actions
        .state_delta
        .insert(super::model_checkpoint::CHECKPOINT_KEY.into(), checkpoint);
    sessions
        .append_event_for_identity(AppendEventRequest {
            identity: session.try_identity().unwrap(),
            event: marker,
        })
        .await
        .unwrap();
    let evidence = NativeSessionBackend::injected(sessions)
        .inspect_model_checkpoint(
            test_session_authority_for("execution-one", 3),
            Arc::new(crate::state::TestStateWriterLease::current()),
            &plan,
        )
        .await
        .expect("inspection needs no model or credential");
    assert!(evidence.matches_execution("execution-one", 3));
}

/// #973: an attached application child this worker cannot build binds no tool,
/// and the run says which one. Before this, the child ended the whole assembly
/// with `native_agent.unsupported_capability` and the user was told nothing
/// about the attachment that had just broken every turn of their agent.
///
/// A PIPELINE is no longer one of them — `application_pipeline` compiles it
/// and offers it as a tool — so the children named here are the ones the
/// ordinary assembler still skips: `predict`, and any `agent_type` a future
/// version invents that this worker does not execute.
///
/// The same seeding point and the same role-`"tool"` shape as the skipped
/// internal tools one function above, and asserted the same way: assembly
/// alone triggers it, so the session is read straight back.
#[tokio::test]
async fn a_fresh_session_is_seeded_with_a_notice_for_every_skipped_application_child() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("skipped-child profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("skipped-child native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    // Listed out of alphabetical order on purpose: the notice is built from
    // the SET, so the text must not depend on the order the version stored
    // the references in.
    let runtime = OrdinaryRuntimeBindings::new(
        Vec::new(),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_skipped_application_children(vec![
        SkippedApplicationChild {
            agent_type: "predict".to_owned(),
            name: "classifier".to_owned(),
            too_large: false,
        },
        SkippedApplicationChild {
            agent_type: "future_type".to_owned(),
            name: "something-new".to_owned(),
            too_large: false,
        },
    ]);
    let assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        bound_model("unused — assembly alone is under test"),
        plan,
        runtime,
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("skipped-child assembly");
    drop(assembled);

    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("skipped-child session");

    let notice_texts = session
        .events()
        .all()
        .iter()
        .filter_map(|event| {
            let content = event.content()?;
            if content.role != "tool" {
                return None;
            }
            content.parts.iter().find_map(|part| match part {
                Part::Text { text } => Some(text.clone()),
                _ => None,
            })
        })
        .collect::<Vec<_>>();

    assert_eq!(
        notice_texts,
        [
            "attached future_type 'something-new' is not available on this worker\n\
          attached predict 'classifier' is not available on this worker"
        ],
        "expected exactly one role=\"tool\" notice event naming both skipped children"
    );
}

/// #1207 review round 2: a configured toolkit refused for a setting the Rust
/// worker does not honour is named in the run, with the reason — before, it
/// was skipped with one log line while the toolkit stayed listed as enabled.
/// The refusal comes from the real materializer, so the sentence asserted
/// here is the one a user's agent receives.
#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered fixture: materialize, assemble, then both notices.
async fn a_fresh_session_names_a_toolkit_refused_for_its_settings() {
    let version = serde_json::json!({"tools": [{
        "id": 7,
        "type": "jira",
        "toolkit_name": "tracker",
        "settings": {
            "jira_configuration": {
                "base_url": "https://jira.example.test",
                "hosting": "Server",
                "username": "svc",
                "api_key": "refused-toolkit-secret"
            },
            "verify_ssl": false
        }
    }]});
    let policy = Arc::new(
        ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"),
    );
    let snapshot = crate::toolkits::FrozenToolSnapshot::from_version_details(
        version.as_object().expect("version"),
    )
    .expect("snapshot")
    .apply_policy(policy.as_ref());
    let materialized = crate::toolkits::materialize_configured_toolsets_reporting_refusals(
        &snapshot,
        &policy,
        &serde_json::Map::new(),
        None,
        crate::toolkits::IndexToolSupport::Withheld(
            crate::toolkits::IndexToolsWithheld::NoVectorStore,
        ),
    )
    .await
    .expect("a refused toolkit does not fail the agent");
    assert!(materialized.toolsets.is_empty());

    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("refused-toolkit profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("refused-toolkit native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let runtime = OrdinaryRuntimeBindings::new(
        Vec::new(),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_left_out_toolkits(materialized.left_out);
    let assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        bound_model("unused — assembly alone is under test"),
        plan,
        runtime,
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("refused-toolkit assembly");
    // The user-visible half: the turn's own `agent_start` names the toolkit.
    let (_native, mut projector, _completion) = assembled.start().expect("native start");
    let start = projector
        .start(chrono::Utc::now())
        .expect("agent start")
        .into_iter()
        .next()
        .expect("one start event");
    let start: serde_json::Value = serde_json::from_slice(
        &crate::protocol::node_event::encode_current_node_event_json(&start).expect("encode"),
    )
    .expect("start json");
    let skipped = &start["response_metadata"]["skipped_capabilities"]["items"];
    assert_eq!(skipped[0]["kind"], "toolkit");
    assert_eq!(skipped[0]["name"], "tracker");
    assert_eq!(skipped[0]["type"], "jira");
    assert_eq!(skipped[0]["reason_code"], "unsupported_setting");
    assert!(!skipped.to_string().contains("refused-toolkit-secret"));

    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("refused-toolkit session");
    let notice_texts = session
        .events()
        .all()
        .iter()
        .filter_map(|event| {
            let content = event.content()?;
            if content.role != "tool" {
                return None;
            }
            content.parts.iter().find_map(|part| match part {
                Part::Text { text } => Some(text.clone()),
                _ => None,
            })
        })
        .collect::<Vec<_>>();
    assert_eq!(
        notice_texts,
        [
            "toolkit 'tracker' (jira) is not enabled on this worker: verify_ssl=false is not \
             supported by the Rust worker; TLS certificates are always verified"
        ]
    );
    assert!(!notice_texts[0].contains("refused-toolkit-secret"));
}

/// The companion negative case: an agent whose children are all agents gets no
/// notice — the fix must add nothing to the common path.
#[tokio::test]
async fn a_fresh_session_with_no_skipped_application_children_gets_no_notice() {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("no-skip profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("no-skip native plan");
    let user_id = plan.user_id().to_owned();
    let session_id = plan.session_id().to_owned();
    let sessions = Arc::new(InMemorySessionService::new());
    let injected_sessions: Arc<dyn SessionService> = sessions.clone();
    let assembled = assemble_ordinary_native_with_sessions_and_runtime_catalogs(
        bound_model("unused — assembly alone is under test"),
        plan,
        OrdinaryRuntimeBindings::new(
            Vec::new(),
            SensitiveToolCatalog::default(),
            DelegatedAuthorizationCatalog::default(),
            ApplicationRuntimeProjection::default(),
        ),
        NativeToolExecutionMode::Sequential,
        injected_sessions,
    )
    .await
    .expect("no-skip assembly");
    drop(assembled);

    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id,
            session_id,
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("no-skip session");
    assert!(
        session
            .events()
            .all()
            .iter()
            .filter_map(adk_rust::Event::content)
            .all(|content| content.role != "tool"),
        "a run with nothing skipped seeded a notice"
    );
}

/// A provider that streams one word per partial chunk and then closes the
/// turn with an EMPTY terminal response — exactly what the OpenAI-compatible
/// facade yields for a stream whose last chunk is `{"delta":{},
/// "finish_reason":"stop"}` (deploy/mock-llm does this; so does Bifrost).
struct WordStreamLlm {
    words: &'static [&'static str],
}

#[async_trait]
impl Llm for WordStreamLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let mut responses: Vec<adk_rust::Result<LlmResponse>> = self
            .words
            .iter()
            .map(|word| {
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text(*word)),
                    partial: true,
                    ..LlmResponse::default()
                })
            })
            .collect();
        responses.push(Ok(LlmResponse {
            finish_reason: Some(FinishReason::Stop),
            turn_complete: true,
            ..LlmResponse::default()
        }));
        Ok(Box::pin(adk_rust::futures::stream::iter(responses)))
    }
}

const STREAMED_WORDS: &[&str] = &["tool ", "result ", "1 ", "said ", "ok "];
const STREAMED_ANSWER: &str = "tool result 1 said ok ";

/// Run one native invocation to EOS and return what the browser and the
/// provisional store receive: the concatenated `agent_llm_chunk` text, the
/// completed step text, and the `full_message` content.
async fn project_streamed_run<C: NativeAgentCompletionSelector>(
    mut assembled: super::runtime::AssembledNativeAgentInvocation<C>,
) -> (String, Vec<String>, String) {
    assembled
        .project_start(Utc::now())
        .expect("projected start");
    let (mut native, mut projector, completion) = assembled.start().expect("native start");
    let mut streamed = String::new();
    let mut steps = Vec::new();
    while let Some(event) = native.next_event().await.expect("native event") {
        for projected in projector.project(&event).expect("projected event") {
            let value: Value = serde_json::from_slice(
                &encode_current_node_event_json(&projected).expect("canonical browser event"),
            )
            .expect("browser event JSON");
            if value["type"] == "agent_llm_chunk" {
                streamed.push_str(value["content"].as_str().unwrap_or_default());
            }
            if value["type"] == "agent_llm_end"
                && let Some(text) = value["response_metadata"]["thinking_steps"][0]["text"].as_str()
                && !text.is_empty()
            {
                steps.push(text.to_owned());
            }
        }
    }
    let completion = completion.select().await.expect("selected completion");
    let full = projector
        .finish_after_eos(completion, Utc::now())
        .expect("projected completion")
        .into_iter()
        .map(|event| {
            serde_json::from_slice::<Value>(
                &encode_current_node_event_json(&event).expect("canonical browser event"),
            )
            .expect("browser event JSON")
        })
        .find(|value| value["type"] == "full_message")
        .and_then(|value| value["content"].as_str().map(ToOwned::to_owned))
        .expect("full message");
    (streamed, steps, full)
}

/// #1082: ADK's Runner fills a content-less terminal event with every part
/// the turn streamed. The projector appended that restatement as one more
/// delta, so the provisional answer elitea-main stores held the reply twice
/// until the terminal replaced it — the window the chat journeys read.
/// Both durable-session shapes: with and without the completion enrichment.
#[tokio::test]
async fn a_streamed_turn_ending_in_an_empty_terminal_is_streamed_exactly_once() {
    for durable in [true, false] {
        let request = ordinary_request(AgentExecutionKind::Application);
        let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
        let plan = OrdinaryNativeAgentPlan::from_authorized(
            &request,
            &profile,
            &AuthorizedNativeCommandBinding::fixture(),
            &request.payload.input_attachments,
        )
        .expect("native plan");
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        let model: Arc<dyn Llm> = Arc::new(WordStreamLlm {
            words: STREAMED_WORDS,
        });
        let (streamed, steps, full) = if durable {
            project_streamed_run(
                assemble_ordinary_native_with_sessions(
                    StreamingBoundModel {
                        model,
                        completed: STREAMED_ANSWER.to_owned(),
                        durable: Arc::new(FixtureDurableCompletion {
                            value: STREAMED_ANSWER.to_owned(),
                        }),
                    },
                    plan,
                    Vec::new(),
                    SensitiveToolCatalog::default(),
                    sessions,
                )
                .await
                .expect("assembly"),
            )
            .await
        } else {
            project_streamed_run(
                assemble_ordinary_native_with_sessions(
                    FixtureBoundModel {
                        model,
                        completed: STREAMED_ANSWER.to_owned(),
                    },
                    plan,
                    Vec::new(),
                    SensitiveToolCatalog::default(),
                    sessions,
                )
                .await
                .expect("assembly"),
            )
            .await
        };
        assert_eq!(streamed, STREAMED_ANSWER, "durable enrichment: {durable}");
        assert_eq!(steps, [STREAMED_ANSWER], "durable enrichment: {durable}");
        assert_eq!(full, STREAMED_ANSWER, "durable enrichment: {durable}");
    }
}

/// #1082, the journey that exposed it: the continuation after a sensitive-tool
/// approval streams its answer once, not once plus a restatement.
#[tokio::test]
async fn an_approved_sensitive_call_continuation_streams_its_answer_exactly_once() {
    let fixture = pause_read_only_sensitive_call().await;
    let profile = OrdinaryNoToolProfile::validate(&fixture.request).expect("agent profile");
    let decision =
        DirectHitlDecision::from_payload(&approved_resume_payload(&fixture.interrupt_id))
            .expect("approved decision");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &fixture.request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &fixture.request.payload.input_attachments,
    )
    .expect("resume native plan");
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&fixture.tool_calls),
        read_only: true,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let sessions: Arc<dyn SessionService> = fixture.sessions.clone();
    let resumed = assemble_direct_hitl_resume_with_sessions(
        StreamingBoundModel {
            model: Arc::new(WordStreamLlm {
                words: STREAMED_WORDS,
            }),
            completed: STREAMED_ANSWER.to_owned(),
            durable: Arc::new(FixtureDurableCompletion {
                value: STREAMED_ANSWER.to_owned(),
            }),
        },
        plan,
        vec![toolset],
        sensitive_catalog(),
        decision,
        sessions,
    )
    .await
    .expect("read-only replay assembly");
    let (streamed, steps, full) = project_streamed_run(resumed).await;
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1);
    assert_eq!(streamed, STREAMED_ANSWER);
    assert_eq!(steps.last().map(String::as_str), Some(STREAMED_ANSWER));
    assert_eq!(full, STREAMED_ANSWER);
}

/// FU-226 at the conversation root: an approved sensitive call that changes data
/// runs exactly once, and the same decision sent again runs nothing.
#[tokio::test]
async fn an_approved_effectful_sensitive_call_runs_once_at_the_root() {
    let fixture = pause_sensitive_call(false).await;
    let profile = OrdinaryNoToolProfile::validate(&fixture.request).expect("agent profile");
    let resume = |words: &'static [&'static str], completed: &'static str| {
        let plan = OrdinaryNativeAgentPlan::from_authorized(
            &fixture.request,
            &profile,
            &AuthorizedNativeCommandBinding::fixture(),
            &fixture.request.payload.input_attachments,
        )
        .expect("resume native plan");
        let tool: Arc<dyn Tool> = Arc::new(CountingTool {
            calls: Arc::clone(&fixture.tool_calls),
            read_only: false,
        });
        let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
        let decision =
            DirectHitlDecision::from_payload(&approved_resume_payload(&fixture.interrupt_id))
                .expect("approved decision");
        assemble_direct_hitl_resume_with_sessions(
            StreamingBoundModel {
                model: Arc::new(WordStreamLlm { words }),
                completed: completed.to_owned(),
                durable: Arc::new(FixtureDurableCompletion {
                    value: completed.to_owned(),
                }),
            },
            plan,
            vec![toolset],
            sensitive_catalog_with_read_only(false),
            decision,
            fixture.sessions.clone() as Arc<dyn SessionService>,
        )
    };
    let resumed = resume(STREAMED_WORDS, STREAMED_ANSWER)
        .await
        .expect("an approved, never-dispatched effectful call is assembled");
    let (_, _, full) = project_streamed_run(resumed).await;
    assert_eq!(full, STREAMED_ANSWER);
    assert_eq!(fixture.tool_calls.load(Ordering::SeqCst), 1, "runs once");
    // The same decision again: the call already has its outcome, so nothing reruns it.
    if let Ok(again) = resume(STREAMED_WORDS, STREAMED_ANSWER).await {
        let _ = tokio::time::timeout(
            std::time::Duration::from_secs(10),
            project_streamed_run(again),
        )
        .await;
    }
    assert_eq!(
        fixture.tool_calls.load(Ordering::SeqCst),
        1,
        "a repeated approval never runs the effect again"
    );
}

/// Sub-agents (#1226) are bound last, over every toolset bound before them,
/// and a child is never offered a tool that needs a confirmation (the
/// workspace command's conditional entry included): it has no way to ask.
/// The nested applications and Smart Tools Selection's meta-tools are not
/// offered either.
#[test]
fn subagents_see_the_bound_toolsets_less_what_needs_a_person() {
    let named = |name: &str| -> Arc<dyn Toolset> { Arc::new(BasicToolset::new(name, Vec::new())) };
    let sensitive = SensitiveToolCatalog::default()
        .with_workspace_command()
        .expect("workspace command");
    let seen = Arc::new(Mutex::new(None));
    let record = Arc::clone(&seen);
    let _runtime = OrdinaryRuntimeBindings::new(
        vec![
            named("github"),
            named("elitea_nested_applications"),
            named(elitea_agent_runtime::lazy_tools::TOOLSET_NAME),
        ],
        sensitive,
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_workspace_toolset(named("local"))
    .with_subagents(move |toolsets, excluded| {
        let offered: Vec<String> = super::subagents::WorkerSubAgents::child_toolsets(toolsets)
            .iter()
            .map(|toolset| toolset.name().to_owned())
            .collect();
        *record.lock().expect("record") = Some((offered, excluded));
        named(elitea_agent_runtime::subagents::TOOLSET_NAME)
    });
    let (offered, excluded) = seen.lock().expect("seen").take().expect("bound");
    assert_eq!(offered, ["github", "local"]);
    assert_eq!(excluded, ["run_command"]);
}

/// A bound model that has no final text, as at a step-limit stop.
struct NoTextBoundModel {
    model: Arc<dyn Llm>,
}

impl BoundOrdinaryAgentModel for NoTextBoundModel {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self) -> Result<String, super::runtime::NativeAgentAssemblyError> {
        Err(super::runtime::NativeAgentAssemblyError::new(
            super::runtime::NativeAgentAssemblyErrorCode::InvalidResult,
            "the fixture model has no final text",
        ))
    }
}

fn granted_plan(
    grant: Option<UnattendedGrant>,
    steps_limit: Option<u32>,
) -> OrdinaryNativeAgentPlan {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.steps_limit = steps_limit;
    let profile = OrdinaryNoToolProfile::validate(&request).expect("agent profile");
    OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture().with_unattended_grant(grant),
        &request.payload.input_attachments,
    )
    .expect("native plan")
}

/// Run one sensitive `double` call under `grant` and return the public
/// events and how often the model and the tool ran.
async fn run_sensitive_call_under_grant(
    grant: Option<UnattendedGrant>,
) -> (Vec<Value>, Vec<String>, usize, usize) {
    let plan = granted_plan(None, None);
    let model_calls = Arc::new(AtomicUsize::new(0));
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([
                tool_call_response_with(json!({"value": 21, "api_token": "secret-token"})),
                LlmResponse::new(Content::new("model").with_text("The answer is 42.")),
            ])),
            calls: Arc::clone(&model_calls),
        }),
        completed: "The answer is 42.".to_owned(),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::clone(&tool_calls),
        read_only: false,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let catalog = sensitive_catalog_with_read_only(false).with_unattended_grant(grant.as_ref());
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let mut assembled =
        assemble_ordinary_native_with_sessions(model, plan, vec![toolset], catalog, sessions)
            .await
            .expect("native assembly");
    assembled
        .project_start(Utc::now())
        .expect("projected start");
    let (mut native, mut projector, _completion) = assembled.start().expect("native start");
    let mut public_events = Vec::new();
    let mut invocation_ids = Vec::new();
    while let Some(event) = native.next_event().await.expect("native event") {
        if !event.tool_calls().is_empty() {
            invocation_ids.push(event.invocation_id.clone());
        }
        for projected in projector.project(&event).expect("projected event") {
            public_events.push(
                serde_json::from_slice(
                    &encode_current_node_event_json(&projected).expect("canonical browser event"),
                )
                .expect("browser event JSON"),
            );
        }
    }
    (
        public_events,
        invocation_ids,
        model_calls.load(Ordering::SeqCst),
        tool_calls.load(Ordering::SeqCst),
    )
}

#[tokio::test]
async fn a_granted_sensitive_call_emits_one_approval_event_and_runs_the_tool() {
    let grant = UnattendedGrant::fixture(&[("fixture", "double")], false);
    let (events, invocation_ids, model_calls, tool_calls) =
        run_sensitive_call_under_grant(Some(grant)).await;

    assert_eq!(tool_calls, 1, "the granted tool runs without a pause");
    assert_eq!(model_calls, 2);
    assert!(
        !events
            .iter()
            .any(|event| event["type"] == "agent_hitl_interrupt")
    );
    let approvals: Vec<usize> = events
        .iter()
        .enumerate()
        .filter(|(_, event)| event["type"] == "agent_unattended_approval")
        .map(|(index, _)| index)
        .collect();
    let [approval_index] = approvals.as_slice() else {
        panic!("exactly one approval event expected: {events:?}");
    };
    let start_index = events
        .iter()
        .position(|event| event["type"] == "agent_tool_start")
        .expect("tool start");
    assert!(*approval_index < start_index, "the approval goes first");
    let approval = &events[*approval_index];
    assert_eq!(approval["content"], Value::Null);
    assert_eq!(approval["stream_id"], "conversation-1");
    assert_eq!(approval["message_id"], "message-1");
    assert!(approval["created_at"].is_string());
    assert!(approval["execution_generation"].is_string());
    let [invocation_id] = invocation_ids.as_slice() else {
        panic!("one tool-call event expected");
    };
    let args = json!({"value": 21, "api_token": "secret-token"});
    let (interrupt_id, _) =
        super::direct_hitl::sensitive_call_identity(invocation_id, "call-1", "double", &args)
            .expect("pause identity");
    let digest = super::events::unmasked_argument_digest(&args).expect("argument digest");
    assert!(digest.starts_with("sha256:") && digest.len() == 7 + 64);
    assert_eq!(
        approval["response_metadata"],
        json!({"unattended_approval_v1": {
            "grant_id": "0b9e3f4c-1d2a-4b5c-8d6e-7f8091a2b3c4",
            "interrupt_id": interrupt_id,
            "toolkit_name": "fixture",
            "toolkit_type": "fixture",
            "tool_name": "double",
            "argument_digest": digest,
        }})
    );
    let encoded = serde_json::to_string(&events).expect("public event JSON");
    assert!(!encoded.contains("secret-token"), "arguments stay masked");
}

#[tokio::test]
async fn a_sensitive_call_whose_pair_the_grant_does_not_name_still_pauses() {
    for grant in [
        None,
        Some(UnattendedGrant::fixture(&[("fixture", "triple")], true)),
        Some(UnattendedGrant::fixture(&[("other", "double")], true)),
    ] {
        let (events, _, model_calls, tool_calls) = run_sensitive_call_under_grant(grant).await;
        assert_eq!(tool_calls, 0);
        assert_eq!(model_calls, 1);
        assert!(
            events
                .iter()
                .any(|event| event["type"] == "agent_hitl_interrupt")
        );
        assert!(
            !events
                .iter()
                .any(|event| event["type"] == "agent_unattended_approval")
        );
    }
}

#[test]
fn the_legacy_auto_approve_flag_stays_refused_with_a_grant() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.auto_approve_sensitive_actions = true;
    let command = AuthorizedNativeCommandBinding::fixture().with_unattended_grant(Some(
        UnattendedGrant::fixture(&[("fixture", "double")], true),
    ));
    assert!(super::runtime::admit_ordinary_plan(&request, &command, &[]).is_err());
    request.payload.auto_approve_sensitive_actions = false;
    let (profile, _, _) =
        super::runtime::admit_ordinary_plan(&request, &command, &[]).expect("granted plan");
    assert!(profile.unattended_grant().is_some());
}

/// Drive a run that calls a tool on every step until its step limit of 1.
async fn run_to_step_limit(
    grant: Option<UnattendedGrant>,
) -> (
    super::runtime::NativeAgentRuntimeError,
    super::events::AgentEventProjector,
    super::session::OrdinaryAgentCompletion<NoTextBoundModel>,
) {
    let plan = granted_plan(grant, Some(1));
    let model = NoTextBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([tool_call_response(), tool_call_response()])),
            calls: Arc::new(AtomicUsize::new(0)),
        }),
    };
    let tool: Arc<dyn Tool> = Arc::new(CountingTool {
        calls: Arc::new(AtomicUsize::new(0)),
        read_only: true,
    });
    let toolset: Arc<dyn Toolset> = Arc::new(BasicToolset::new("fixture-tools", vec![tool]));
    let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
    let mut assembled = assemble_ordinary_native_with_sessions(
        model,
        plan,
        vec![toolset],
        SensitiveToolCatalog::default(),
        sessions,
    )
    .await
    .expect("native assembly");
    assembled
        .project_start(Utc::now())
        .expect("projected start");
    let (mut native, mut projector, completion) = assembled.start().expect("native start");
    let error = loop {
        match native.next_event().await {
            Ok(Some(event)) => {
                projector.project(&event).expect("projected event");
            }
            Ok(None) => panic!("the run must stop at its step limit"),
            Err(error) => break error,
        }
    };
    (error, *projector, completion)
}

#[tokio::test]
async fn a_granted_run_at_its_step_limit_completes_with_the_step_limit_flag() {
    let grant = UnattendedGrant::fixture(&[], true);
    let (error, mut projector, completion) = run_to_step_limit(Some(grant)).await;

    assert!(error.step_limit_reached());
    let stop = completion
        .step_limit_stop()
        .expect("granted step-limit stop");
    // The lifecycle marks the stop and closes the projector.
    stop.mark();
    let _closed = projector
        .close_for_step_limit(Utc::now())
        .expect("closed at the step limit");
    let selected = completion
        .select()
        .await
        .expect("selected without final text");
    let batch = projector
        .finish_after_eos(selected, Utc::now())
        .expect("step-limit result");
    let full_message = batch
        .into_iter()
        .find(|event| event.r#type == "full_message")
        .expect("full message");
    let metadata: Value =
        serde_json::from_slice(&full_message.response_metadata).expect("metadata JSON");
    assert_eq!(metadata["step_limit_reached"], true);
    assert_eq!(metadata["output_limit_reached"], false);
}

#[tokio::test]
async fn a_run_without_the_grant_at_its_step_limit_still_fails() {
    for grant in [None, Some(UnattendedGrant::fixture(&[], false))] {
        let (error, _projector, completion) = run_to_step_limit(grant).await;
        assert!(error.step_limit_reached());
        assert!(completion.step_limit_stop().is_none());
        assert!(completion.select().await.is_err());
    }
}

/// One turn of `reader` with `history` (and its references), capturing the
/// model's request.
async fn reconciled_turn(
    sessions: &Arc<InMemorySessionService>,
    question: &str,
    history: Vec<Value>,
    refs: Vec<super::request::ChatHistoryRef>,
) -> Vec<adk_rust::Content> {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.chat_history = history;
    request.payload.chat_history_refs = refs;
    request.payload.user_input = UserInput::Text(question.to_owned());
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let captured = Arc::new(Mutex::new(Vec::new()));
    let model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::new(AtomicUsize::new(0)),
        }),
        completed: format!("answer to {question}"),
    };
    let shared: Arc<dyn SessionService> = sessions.clone();
    let assembled = assemble_ordinary_native_with_sessions(
        model,
        plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        shared,
    )
    .await
    .expect("assembly");
    let (mut run, _projector, completion) = assembled.start().expect("run");
    while run.next_event().await.expect("event").is_some() {}
    completion.select().await.expect("completion");
    let requests = captured.lock().expect("captured request");
    requests
        .first()
        .expect("one model request")
        .contents
        .clone()
}

fn history_ref(id: &str, readers_own: bool) -> super::request::ChatHistoryRef {
    super::request::ChatHistoryRef {
        message_id: id.to_owned(),
        readers_own,
    }
}

fn role_texts(contents: &[adk_rust::Content]) -> Vec<(String, String)> {
    contents
        .iter()
        .map(|content| {
            let text = content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect::<String>();
            (content.role.clone(), text)
        })
        .collect()
}

/// ADR-0050 decision 9c (#1603): a restored session sees what another agent
/// wrote after the reader's last turn, labelled, once.
#[tokio::test]
async fn a_restored_session_reconciles_a_peers_later_message_once() {
    const Q1: &str = "00000000-0000-4000-8000-000000000001";
    const A1: &str = "00000000-0000-4000-8000-000000000002";
    const PEER: &str = "00000000-0000-4000-8000-000000000003";
    const Q2: &str = "00000000-0000-4000-8000-000000000004";
    const A2: &str = "00000000-0000-4000-8000-000000000005";
    let sessions = Arc::new(InMemorySessionService::new());
    reconciled_turn(&sessions, "first question", Vec::new(), Vec::new()).await;

    let peer = json!({
        "role": "user",
        "content": [{"type": "text", "text": "the peer's finding"}],
        "additional_kwargs": {"elitea_author": {"kind": "agent", "name": "CI Doctor"}}
    });
    let mut history = vec![
        json!({"role": "user", "content": [{"type": "text", "text": "first question"}], "additional_kwargs": {}}),
        json!({"role": "assistant", "content": [{"type": "text", "text": "answer to first question"}], "additional_kwargs": {}}),
        peer,
    ];
    let mut refs = vec![
        history_ref(Q1, true),
        history_ref(A1, true),
        history_ref(PEER, false),
    ];
    let second = role_texts(
        &reconciled_turn(&sessions, "second question", history.clone(), refs.clone()).await,
    );
    assert_eq!(second.len(), 4, "{second:?}");
    assert_eq!(second[0], ("user".to_owned(), "first question".to_owned()));
    assert_eq!(second[1].0, "model");
    assert_eq!(second[2].0, "user");
    assert!(
        second[2].1.contains("from=\"CI Doctor\"") && second[2].1.contains("the peer's finding"),
        "{second:?}"
    );
    assert_eq!(second[3], ("user".to_owned(), "second question".to_owned()));

    // The next turn matches the peer's message by id: it is not repeated.
    history.push(json!({"role": "user", "content": [{"type": "text", "text": "second question"}], "additional_kwargs": {}}));
    history.push(json!({"role": "assistant", "content": [{"type": "text", "text": "answer to second question"}], "additional_kwargs": {}}));
    refs.push(history_ref(Q2, true));
    refs.push(history_ref(A2, true));
    let third = role_texts(&reconciled_turn(&sessions, "third question", history, refs).await);
    let peers = third
        .iter()
        .filter(|(_, text)| text.contains("the peer's finding"))
        .count();
    assert_eq!(peers, 1, "{third:?}");
    assert_eq!(third.len(), 6, "{third:?}");
}

/// A session seeded before field 71 (ordinal ids) is reconciled once from its
/// last own entry; the seeded peer message is never appended again.
#[tokio::test]
async fn a_session_seeded_without_references_is_reconciled_without_duplicates() {
    let ids = (1..=8)
        .map(|n| format!("00000000-0000-4000-8000-00000000001{n}"))
        .collect::<Vec<_>>();
    let text = |role: &str, text: &str| json!({"role": role, "content": [{"type": "text", "text": text}], "additional_kwargs": {}});
    let peer = |text: &str| {
        json!({
            "role": "user",
            "content": [{"type": "text", "text": text}],
            "additional_kwargs": {"elitea_author": {"kind": "agent", "name": "CI Doctor"}}
        })
    };
    let sessions = Arc::new(InMemorySessionService::new());
    let mut history = vec![text("user", "earlier question"), peer("old finding")];
    // The old worker: no references, ordinal seed.
    reconciled_turn(&sessions, "first question", history.clone(), Vec::new()).await;

    history.extend([
        text("user", "first question"),
        text("assistant", "answer to first question"),
        peer("new finding"),
    ]);
    let mut refs = vec![
        history_ref(&ids[0], true),
        history_ref(&ids[1], false),
        history_ref(&ids[2], true),
        history_ref(&ids[3], true),
        history_ref(&ids[4], false),
    ];
    let second = role_texts(
        &reconciled_turn(&sessions, "second question", history.clone(), refs.clone()).await,
    );
    history.extend([
        text("user", "second question"),
        text("assistant", "answer to second question"),
    ]);
    refs.extend([history_ref(&ids[5], true), history_ref(&ids[6], true)]);
    let third = role_texts(&reconciled_turn(&sessions, "third question", history, refs).await);
    for turn in [&second, &third] {
        for finding in ["old finding", "new finding"] {
            let seen = turn
                .iter()
                .filter(|(_, text)| text.contains(finding))
                .count();
            assert_eq!(seen, 1, "{finding}: {turn:?}");
        }
    }
}

/// One turn of a reader whose history Main built under the context view
/// `view` (#1672), the sub-agent tools bound when `tagging` (#1865).
struct ViewTurn<'a> {
    question: &'a str,
    question_id: Option<&'a str>,
    view: Option<&'a str>,
    tagging: bool,
    regenerate: bool,
}

impl<'a> ViewTurn<'a> {
    const fn new(question: &'a str, view: Option<&'a str>) -> Self {
        Self {
            question,
            question_id: None,
            view,
            tagging: false,
            regenerate: false,
        }
    }
}

async fn view_turn(
    sessions: &Arc<InMemorySessionService>,
    turn: &ViewTurn<'_>,
    history: &[Value],
    refs: &[super::request::ChatHistoryRef],
) -> Vec<(String, String)> {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.chat_history = history.to_vec();
    request.payload.chat_history_refs = refs.to_vec();
    request.payload.context_view_history = turn.view.map(str::to_owned);
    request.payload.current_message_id = turn.question_id.map(str::to_owned);
    request.payload.is_regenerate = turn.regenerate;
    request.payload.user_input = UserInput::Text(turn.question.to_owned());
    let profile = if turn.regenerate {
        OrdinaryNoToolProfile::validate_regeneration(&request)
    } else {
        OrdinaryNoToolProfile::validate(&request)
    }
    .expect("ordinary profile");
    let mut plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    if turn.tagging {
        let _ = plan.tag_message_refs();
    }
    let captured = Arc::new(Mutex::new(Vec::new()));
    let model = FixtureBoundModel {
        model: Arc::new(CapturingFinalLlm {
            requests: Arc::clone(&captured),
            calls: Arc::new(AtomicUsize::new(0)),
        }),
        completed: format!("answer to {}", turn.question),
    };
    let shared: Arc<dyn SessionService> = sessions.clone();
    let assembled = assemble_ordinary_native_with_sessions(
        model,
        plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        shared,
    )
    .await
    .expect("assembly");
    let (mut run, _projector, completion) = assembled.start().expect("run");
    while run.next_event().await.expect("event").is_some() {}
    completion.select().await.expect("completion");
    let requests = captured.lock().expect("captured request");
    role_texts(&requests.first().expect("one model request").contents)
}

/// The three-agent fixture of #1672: the person, the reader and two peers.
struct ThreeAgents {
    entries: Vec<(&'static str, Value, super::request::ChatHistoryRef)>,
}

impl ThreeAgents {
    const fn new() -> Self {
        Self {
            entries: Vec::new(),
        }
    }

    fn id(n: usize) -> String {
        format!("00000000-0000-4000-8000-0000000002{n:02}")
    }

    fn push(
        &mut self,
        label: &'static str,
        role: &str,
        author: Option<&str>,
        text: &str,
        own: bool,
    ) {
        let kwargs = author.map_or_else(
            || json!({}),
            |name| json!({"elitea_author": {"kind": "agent", "name": name}}),
        );
        let entry = json!({"role": role, "content": [{"type": "text", "text": text}], "additional_kwargs": kwargs});
        let reference = history_ref(&Self::id(self.entries.len() + 1), own);
        self.entries.push((label, entry, reference));
    }

    /// The view of the entries whose labels `keep` names, in transcript order.
    fn view(&self, keep: &[&str]) -> (Vec<Value>, Vec<super::request::ChatHistoryRef>) {
        self.entries
            .iter()
            .filter(|(label, ..)| keep.contains(label))
            .map(|(_, entry, reference)| (entry.clone(), reference.clone()))
            .unzip()
    }
}

/// The id of the reader's session event in which the user asked `question`:
/// a history event (`hm-…`) once a re-seed rebuilt it from the view, the
/// turn's own event while the session lineage is kept.
async fn question_event_id(sessions: &Arc<InMemorySessionService>, question: &str) -> String {
    let request = ordinary_request(AgentExecutionKind::Adhoc);
    let profile = OrdinaryNoToolProfile::validate(&request).expect("ordinary profile");
    let plan = OrdinaryNativeAgentPlan::from_authorized(
        &request,
        &profile,
        &AuthorizedNativeCommandBinding::fixture(),
        &request.payload.input_attachments,
    )
    .expect("native plan");
    let session = sessions
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: plan.user_id().to_owned(),
            session_id: plan.session_id().to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("reader session");
    let ids = session
        .events()
        .all()
        .into_iter()
        .filter(|event| {
            event.author == "user"
                && event
                    .llm_response
                    .content
                    .as_ref()
                    .is_some_and(|content| format!("{content:?}").contains(question))
        })
        .map(|event| event.id.clone())
        .collect::<Vec<_>>();
    assert_eq!(ids.len(), 1, "{question}: {ids:?}");
    ids[0].clone()
}

fn texts_in(turn: &[(String, String)]) -> Vec<String> {
    turn.iter().map(|(_, text)| text.clone()).collect()
}

/// #1672: after a mode change the reader's next model context is exactly
/// its current view plus its own turns, in transcript order; a turn that
/// changes nothing sends the same prefix; widening the mode never appends
/// older messages after newer ones.
#[tokio::test]
async fn a_context_view_change_reseeds_the_readers_session_in_transcript_order() {
    let mut chat = ThreeAgents::new();
    chat.push(
        "kickoff",
        "user",
        None,
        "kickoff before the reader joined",
        false,
    );
    chat.push(
        "early",
        "user",
        Some("Planner"),
        "early planner note",
        false,
    );
    let sessions = Arc::new(InMemorySessionService::new());
    let (history, refs) = chat.view(&["kickoff", "early"]);
    let first = view_turn(
        &sessions,
        &ViewTurn::new("first question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(first.len(), 3, "{first:?}");

    chat.push("q1", "user", None, "first question", true);
    chat.push("a1", "assistant", None, "answer to first question", true);
    chat.push(
        "late",
        "user",
        Some("Reviewer"),
        "late reviewer finding",
        false,
    );
    // Narrowed to `since_joined`: the pre-join messages leave the session.
    let (history, refs) = chat.view(&["q1", "a1", "late"]);
    let second = view_turn(
        &sessions,
        &ViewTurn::new("second question", Some("since_joined")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(second.len(), 4, "{second:?}");
    assert_eq!(second[0], ("user".to_owned(), "first question".to_owned()));
    assert_eq!(
        second[1],
        ("model".to_owned(), "answer to first question".to_owned())
    );
    assert!(second[2].1.contains("late reviewer finding"), "{second:?}");
    assert_eq!(second[3], ("user".to_owned(), "second question".to_owned()));
    for gone in ["kickoff", "early planner note"] {
        assert!(
            !texts_in(&second).iter().any(|text| text.contains(gone)),
            "{gone}: {second:?}"
        );
    }

    chat.push("q2", "user", None, "second question", true);
    chat.push("a2", "assistant", None, "answer to second question", true);
    // The same mode: no re-seed, the earlier request is the prefix.
    let (history, refs) = chat.view(&["q1", "a1", "late", "q2", "a2"]);
    let third = view_turn(
        &sessions,
        &ViewTurn::new("third question", Some("since_joined")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(third.len(), 6, "{third:?}");
    assert_eq!(third[..4], second[..], "{third:?}");

    chat.push("q3", "user", None, "third question", true);
    chat.push("a3", "assistant", None, "answer to third question", true);
    // Widened to `full`: the older messages come back BEFORE the later ones.
    let all = [
        "kickoff", "early", "q1", "a1", "late", "q2", "a2", "q3", "a3",
    ];
    let (history, refs) = chat.view(&all);
    let fourth = view_turn(
        &sessions,
        &ViewTurn::new("fourth question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(fourth.len(), 10, "{fourth:?}");
    let positions = user_positions(
        &fourth,
        &[
            "kickoff",
            "early planner note",
            "first question",
            "late reviewer finding",
            "second question",
            "third question",
            "fourth question",
        ],
    );
    assert!(
        positions.windows(2).all(|pair| pair[0] < pair[1]),
        "{positions:?} {fourth:?}"
    );
    assert_eq!(positions[3], 4, "{fourth:?}");
}

/// Where each of `needles` is among the user-role entries of `turn`.
fn user_positions(turn: &[(String, String)], needles: &[&str]) -> Vec<usize> {
    needles
        .iter()
        .map(|needle| {
            turn.iter()
                .position(|(role, text)| role == "user" && text.contains(needle))
                .unwrap_or_else(|| panic!("{needle}: {turn:?}"))
        })
        .collect()
}

/// #1672: a session seeded before the view was recorded (#1609) and holding
/// side talk between two other agents is re-seeded on its first turn after
/// the upgrade, and only then.
#[tokio::test]
async fn a_session_from_before_the_view_is_reseeded_once() {
    let mut chat = ThreeAgents::new();
    chat.push(
        "side",
        "user",
        Some("Planner"),
        "planner to reviewer side talk",
        false,
    );
    chat.push("kickoff", "user", None, "kickoff", false);
    let sessions = Arc::new(InMemorySessionService::new());
    // The old worker: references, but no view.
    let (history, refs) = chat.view(&["side", "kickoff"]);
    let first = view_turn(
        &sessions,
        &ViewTurn::new("first question", None),
        &history,
        &refs,
    )
    .await;
    assert!(
        texts_in(&first)
            .iter()
            .any(|text| text.contains("side talk")),
        "{first:?}"
    );

    chat.push("q1", "user", None, "first question", true);
    chat.push("a1", "assistant", None, "answer to first question", true);
    // Main now leaves out side talk the reader is no party to.
    let (history, refs) = chat.view(&["kickoff", "q1", "a1"]);
    let second = view_turn(
        &sessions,
        &ViewTurn::new("second question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert!(
        !texts_in(&second)
            .iter()
            .any(|text| text.contains("side talk")),
        "{second:?}"
    );
    assert_eq!(second.len(), 4, "{second:?}");
    assert!(
        question_event_id(&sessions, "first question")
            .await
            .starts_with("hm-"),
        "re-seeded from the view"
    );

    chat.push("q2", "user", None, "second question", true);
    chat.push("a2", "assistant", None, "answer to second question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1", "q2", "a2"]);
    let third = view_turn(
        &sessions,
        &ViewTurn::new("third question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(third[..4], second[..], "re-seeded once: {third:?}");
}

/// #1905: a session from before the view that holds nothing the view leaves
/// out is rebound, not re-seeded: its own turns, tool results and cached
/// prefix stay, and the next turn keeps it.
#[tokio::test]
async fn a_session_from_before_the_view_holding_only_the_view_is_rebound() {
    let mut chat = ThreeAgents::new();
    chat.push("kickoff", "user", None, "kickoff", false);
    let sessions = Arc::new(InMemorySessionService::new());
    let (history, refs) = chat.view(&["kickoff"]);
    view_turn(
        &sessions,
        &ViewTurn::new("first question", None),
        &history,
        &refs,
    )
    .await;
    let own = question_event_id(&sessions, "first question").await;
    assert!(!own.starts_with("hm-"), "{own}");

    chat.push("q1", "user", None, "first question", true);
    chat.push("a1", "assistant", None, "answer to first question", true);
    chat.push(
        "late",
        "user",
        Some("Reviewer"),
        "late reviewer finding",
        false,
    );
    let (history, refs) = chat.view(&["kickoff", "q1", "a1", "late"]);
    let second = view_turn(
        &sessions,
        &ViewTurn::new("second question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(second.len(), 5, "{second:?}");
    assert!(second[3].1.contains("late reviewer finding"), "{second:?}");
    assert_eq!(
        question_event_id(&sessions, "first question").await,
        own,
        "rebound, not re-seeded"
    );

    chat.push("q2", "user", None, "second question", true);
    chat.push("a2", "assistant", None, "answer to second question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1", "late", "q2", "a2"]);
    let third = view_turn(
        &sessions,
        &ViewTurn::new("third question", Some("full")),
        &history,
        &refs,
    )
    .await;
    assert_eq!(third[..5], second[..], "bound after the rebind: {third:?}");
    assert_eq!(question_event_id(&sessions, "first question").await, own);
}

/// #1903: a regeneration of a turn that wrote its question carries that
/// question's id, so the regenerated session stays tagged and the next
/// tagged turn keeps it instead of re-seeding.
#[tokio::test]
async fn a_regeneration_naming_its_question_keeps_the_session_tagged() {
    let mut chat = ThreeAgents::new();
    chat.push("kickoff", "user", None, "kickoff", false);
    let sessions = Arc::new(InMemorySessionService::new());
    let (history, refs) = chat.view(&["kickoff"]);
    let q1 = ThreeAgents::id(2);
    let mut turn = ViewTurn::new("first question", Some("full"));
    turn.question_id = Some(&q1);
    turn.tagging = true;
    view_turn(&sessions, &turn, &history, &refs).await;

    // Regenerate the first answer: the history ends before its question.
    let mut turn = ViewTurn::new("first question", Some("full"));
    turn.question_id = Some(&q1);
    turn.tagging = true;
    turn.regenerate = true;
    let regenerated = view_turn(&sessions, &turn, &history, &refs).await;
    assert!(
        regenerated.last().is_some_and(
            |(_, text)| text.contains("first question") && text.starts_with("[message ")
        ),
        "{regenerated:?}"
    );
    let own = question_event_id(&sessions, "first question").await;

    chat.push("q1", "user", None, "first question", true);
    chat.push("a1", "assistant", None, "answer to first question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1"]);
    let q2 = ThreeAgents::id(4);
    let mut turn = ViewTurn::new("second question", Some("full"));
    turn.question_id = Some(&q2);
    turn.tagging = true;
    let second = view_turn(&sessions, &turn, &history, &refs).await;
    assert_eq!(second[..2], regenerated[..], "{second:?}");
    assert_eq!(
        question_event_id(&sessions, "first question").await,
        own,
        "kept, not re-seeded"
    );
}

/// #1865: a session holding messages without their `[message m-…]` ids is
/// re-seeded on the first turn the sub-agent tools are bound, and once it
/// holds ids it keeps them without another re-seed; a regeneration, which
/// asks its question again without an id, makes the next turn re-seed.
#[tokio::test]
async fn a_session_with_untagged_messages_is_reseeded_when_subagents_are_bound() {
    use elitea_agent_runtime::subagents::message_ref::message_ref;
    let tagged = |turn: &[(String, String)], id: &str| {
        let line = format!("[message {}]", message_ref(id));
        turn.iter()
            .filter(|(_, text)| text.starts_with(&line))
            .count()
    };
    let mut chat = ThreeAgents::new();
    chat.push("kickoff", "user", None, "kickoff", false);
    let sessions = Arc::new(InMemorySessionService::new());
    let (history, refs) = chat.view(&["kickoff"]);
    let q1 = ThreeAgents::id(2);
    let mut turn = ViewTurn::new("first question", Some("full"));
    turn.question_id = Some(&q1);
    view_turn(&sessions, &turn, &history, &refs).await;

    chat.push("q1", "user", None, "first question", true);
    chat.push("a1", "assistant", None, "answer to first question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1"]);
    let q2 = ThreeAgents::id(4);
    let mut turn = ViewTurn::new("second question", Some("full"));
    turn.question_id = Some(&q2);
    turn.tagging = true;
    let second = view_turn(&sessions, &turn, &history, &refs).await;
    for id in [ThreeAgents::id(1), q1.clone(), q2.clone()] {
        assert_eq!(tagged(&second, &id), 1, "{id}: {second:?}");
    }

    chat.push("q2", "user", None, "second question", true);
    chat.push("a2", "assistant", None, "answer to second question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1", "q2", "a2"]);
    let mut turn = ViewTurn::new("third question", Some("full"));
    turn.tagging = true;
    turn.regenerate = true;
    let third = view_turn(&sessions, &turn, &history, &refs).await;
    assert_eq!(tagged(&third, &q2), 1, "{third:?}");
    assert!(
        third
            .last()
            .is_some_and(|(_, text)| text == "third question"),
        "{third:?}"
    );

    // The regenerated question was stored without an id: the next tagged
    // turn re-seeds from the history, where it has one.
    let q3 = ThreeAgents::id(6);
    chat.push("q3", "user", None, "third question", true);
    chat.push("a3", "assistant", None, "answer to third question", true);
    let (history, refs) = chat.view(&["kickoff", "q1", "a1", "q2", "a2", "q3", "a3"]);
    let q4 = ThreeAgents::id(8);
    let mut turn = ViewTurn::new("fourth question", Some("full"));
    turn.question_id = Some(&q4);
    turn.tagging = true;
    let fourth = view_turn(&sessions, &turn, &history, &refs).await;
    for id in [&q3, &q4] {
        assert_eq!(tagged(&fourth, id), 1, "{id}: {fourth:?}");
    }
    assert_eq!(fourth.len(), 8, "{fourth:?}");

    // Nothing changed: the same bytes, no re-seed.
    chat.push("q4", "user", None, "fourth question", true);
    chat.push("a4", "assistant", None, "answer to fourth question", true);
    let all = ["kickoff", "q1", "a1", "q2", "a2", "q3", "a3", "q4", "a4"];
    let (history, refs) = chat.view(&all);
    let q5 = ThreeAgents::id(10);
    let mut turn = ViewTurn::new("fifth question", Some("full"));
    turn.question_id = Some(&q5);
    turn.tagging = true;
    let fifth = view_turn(&sessions, &turn, &history, &refs).await;
    assert_eq!(fifth[..8], fourth[..], "{fifth:?}");
}

/// A browser session that records its ends (ADR-0049 decision 8).
#[derive(Default)]
struct EndRecordingBrowser {
    ended: Mutex<Vec<bool>>,
}

#[async_trait]
impl elitea_agent_runtime::toolkits::BrowserSession for EndRecordingBrowser {
    async fn call(
        &self,
        _call_id: &str,
        _name: &str,
        _arguments: &Value,
    ) -> Result<
        elitea_agent_runtime::toolkits::SessionAnswer,
        elitea_agent_runtime::toolkits::SessionError,
    > {
        Err(elitea_agent_runtime::toolkits::SessionError::Unavailable)
    }

    async fn end(&self, now: bool) {
        self.ended.lock().expect("ended").push(now);
    }
}

impl EndRecordingBrowser {
    async fn ends(&self) -> Vec<bool> {
        for _ in 0..200 {
            if !self.ended.lock().expect("ended").is_empty() {
                break;
            }
            tokio::task::yield_now().await;
        }
        self.ended.lock().expect("ended").clone()
    }
}

/// A failed run ends its browser session at once.
#[tokio::test]
async fn a_failed_run_ends_its_browser_session_now() {
    let browser = Arc::new(EndRecordingBrowser::default());
    let (_error, _projector, completion) = run_to_step_limit(None).await;
    let completion = completion.with_browser(Some(Arc::clone(&browser) as _));
    assert!(completion.select().await.is_err());
    assert_eq!(browser.ends().await, [true], "ended once, at once");
}

/// A capped, cancelled or lease-lost run never reaches `select`: the
/// lifecycle drops the completion, and the drop ends the session at once.
#[tokio::test]
async fn a_capped_run_that_drops_its_completion_ends_its_browser_session() {
    let browser = Arc::new(EndRecordingBrowser::default());
    let (_error, _projector, completion) = run_to_step_limit(None).await;
    drop(completion.with_browser(Some(Arc::clone(&browser) as _)));
    assert_eq!(browser.ends().await, [true]);
}

/// A completed run ends it as a finished run (a viewer may still look), and
/// only once; a paused run keeps it for the resume.
#[tokio::test]
async fn a_completed_run_ends_and_a_paused_run_keeps_its_browser_session() {
    let browser = Arc::new(EndRecordingBrowser::default());
    let grant = UnattendedGrant::fixture(&[], true);
    let (_error, _projector, completion) = run_to_step_limit(Some(grant)).await;
    completion.step_limit_stop().expect("granted stop").mark();
    let completion = completion.with_browser(Some(Arc::clone(&browser) as _));
    completion.select().await.expect("completed");
    tokio::task::yield_now().await;
    assert_eq!(browser.ends().await, [false]);

    let kept = Arc::new(EndRecordingBrowser::default());
    let (_error, _projector, completion) = run_to_step_limit(None).await;
    let mut completion = completion.with_browser(Some(Arc::clone(&kept) as _));
    completion.paused().await.expect("paused");
    drop(completion);
    for _ in 0..10 {
        tokio::task::yield_now().await;
    }
    assert!(
        kept.ended.lock().expect("ended").is_empty(),
        "the pause keeps it"
    );
}
