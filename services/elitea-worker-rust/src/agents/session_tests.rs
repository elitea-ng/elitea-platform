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
    assemble_ordinary_native_with_sessions_and_runtime_catalogs,
};
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
    let regeneration_sessions: Arc<dyn SessionService> = sessions;
    let regenerated = assemble_ordinary_native_with_sessions(
        regeneration_model,
        regeneration_plan,
        Vec::new(),
        SensitiveToolCatalog::default(),
        regeneration_sessions,
    )
    .await
    .expect("regeneration assembly");
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
            test_session_authority_for("execution/two", 3),
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
    let model = FixtureBoundModel {
        model: Arc::new(SequencedLlm {
            responses: Mutex::new(VecDeque::from([
                parallel_application_call_response(),
                LlmResponse::new(Content::new("model").with_text("Both names were resolved.")),
            ])),
            calls: Arc::new(AtomicUsize::new(0)),
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

    assert_eq!(
        result_ids,
        ["application-call-1", "application-call-2"],
        "ADK must restore provider call order after concurrent completion"
    );
    let completion = completion.select().await.expect("selected completion");
    let _ = completion;
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
        internal_tools.toolsets(),
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
    let resume_runtime = OrdinaryRuntimeBindings::new(
        internal_tools.toolsets(),
        SensitiveToolCatalog::default(),
        DelegatedAuthorizationCatalog::default(),
        ApplicationRuntimeProjection::default(),
    )
    .with_internal_tools(internal_tools);
    let resumed = assemble_direct_hitl_resume_with_sessions_and_applications(
        resumed_model,
        resume_plan,
        resume_runtime,
        DirectHitlDecisionSet::single(decision),
        sessions,
    )
    .await
    .expect("ask_user replay assembly");
    let (mut resumed_run, _projector, completion) = resumed.start().expect("ask_user replay start");
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
    assert_eq!((calls, results), (2, 1));
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
    // `swarm` and `planner`: two platform-catalogue names this runtime does
    // not implement, plus `ask_user`, which IS implemented and must produce
    // no notice of its own.
    request.payload.internal_tools = vec![
        "swarm".to_owned(),
        ASK_USER_TOOL_NAME.to_owned(),
        "planner".to_owned(),
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
        internal_tools.toolsets(),
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
        ["internal tool 'planner' is not available on this worker\n\
             internal tool 'swarm' is not available on this worker"],
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
        internal_tools.toolsets(),
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

#[tokio::test]
async fn block_actions_emit_one_correlated_structured_result_without_executing_the_tool() {
    assert_blocked_direct_replay("reject", "", "denied by user").await;
    assert_blocked_direct_replay(
        "block_with_comment",
        "retain this record",
        "retain this record",
    )
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
        assert!(event.tool_results().is_empty());
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
    assert_eq!(count_call_parts(&requests[0], "call-1"), (3, 1));
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
        "message": "You declined THIS specific call to 'Fixture Tools.double'; it was not executed. The block is for THIS invocation only, not the tool itself. This is NOT a stop signal — do not end your turn or summarize yet. Do not retry this same call with the same arguments, but DO continue: if more items remain, call the tool again for the NEXT item now; otherwise use another available tool to keep making progress. Only stop and ask the user when nothing remains that can be done without this exact declined call."
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
    assert_eq!(replayed_calls, 2);
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
            .contains("before it produced visible output")
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
    let Err(error) =
        super::events::CompletedAgentBrowserOutput::ordinary(String::new(), "thread-1".to_owned())
    else {
        panic!("empty model result was accepted");
    };
    assert_eq!(error.code(), AgentEventProjectionErrorCode::InvalidOutput);
}
