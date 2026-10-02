use std::sync::{Arc, Mutex};

use adk_rust::agent::LlmAgentBuilder;
use adk_rust::futures::StreamExt as _;
use adk_rust::runner::Runner;
use adk_rust::session::{CreateRequest, GetRequest, InMemorySessionService, SessionService};
use adk_rust::tool::BasicToolset;
use adk_rust::{Content, Event, Part, ToolConfirmationDecision, ToolConfirmationRequest};
use serde_json::{Value, json};

use super::assembly_tests::ordinary_request;
use super::direct_hitl::{
    DirectHitlDecision, DirectHitlDecisionSet, DirectHitlError, DirectHitlErrorCode,
    ResolvedDirectHitlDecision, ResolvedDirectHitlStart, sensitive_call_identity,
};
use super::request::AgentExecutionKind;
use super::sensitive_tools::SensitiveToolCatalog;
use crate::toolkits::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DelegatedAuthorizationCatalog,
    DelegatedAuthorizationRequirement, ToolAdmissionPolicy,
    encode_delegated_authorization_requirement,
};

const AUTH_SERVER: &str = "https://mcp.example.invalid/v1/mcp";

fn pending_events(arguments: Value) -> Vec<Event> {
    let mut call = Event::with_id("call-event", "invocation-1");
    call.author = "elitea-agent".to_owned();
    call.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: "double".to_owned(),
            args: arguments.clone(),
            id: Some("call-1".to_owned()),
            thought_signature: None,
        }],
    });
    let mut confirmation = Event::with_id("confirmation-event", "invocation-1");
    confirmation.author = "elitea-agent".to_owned();
    confirmation.llm_response.interrupted = true;
    confirmation.llm_response.turn_complete = true;
    confirmation.actions.tool_confirmation = Some(ToolConfirmationRequest {
        tool_name: "double".to_owned(),
        function_call_id: Some("call-1".to_owned()),
        args: arguments,
    });
    vec![call, confirmation]
}

fn pending_authorization_events(arguments: Value) -> Vec<Event> {
    let mut events = pending_events(arguments);
    let requirement = authorization_requirement();
    events[1].provider_metadata.insert(
        DELEGATED_AUTHORIZATION_METADATA_KEY.to_owned(),
        encode_delegated_authorization_requirement(&requirement)
            .expect("authorization requirement metadata"),
    );
    events
}

fn authorization_requirement() -> DelegatedAuthorizationRequirement {
    DelegatedAuthorizationRequirement::new(
        "Remote MCP".to_owned(),
        "mcp".to_owned(),
        AUTH_SERVER.to_owned(),
        Some("https://mcp.example.invalid/.well-known/oauth-protected-resource".to_owned()),
        Some("Bearer".to_owned()),
    )
    .expect("authorization requirement")
}

#[test]
fn checkpointed_skip_survives_another_guard_without_leaking_into_a_fresh_run() {
    let arguments = json!({"value": 21});
    let mut events = pending_events(arguments.clone());
    let mut skipped = DelegatedAuthorizationCatalog::default();
    skipped
        .insert("read_first", authorization_requirement())
        .unwrap();
    skipped
        .insert("read_second", authorization_requirement())
        .unwrap();
    skipped.decline(&authorization_requirement());
    events[1].provider_metadata.insert(
        crate::toolkits::DELEGATED_AUTHORIZATION_SCOPE_KEY.to_owned(),
        skipped.encode_declined_scope().unwrap().unwrap(),
    );
    let (interrupt_id, _) =
        sensitive_call_identity("invocation-1", "call-1", "double", &arguments).unwrap();
    let decision = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .unwrap()
        .resolve(&session(events))
        .unwrap();
    let mut fresh = DelegatedAuthorizationCatalog::default();
    fresh
        .insert("read_first", authorization_requirement())
        .unwrap();
    fresh
        .insert("read_second", authorization_requirement())
        .unwrap();
    let mut resumed = fresh.clone();
    decision.restore_authorization_scope(&mut resumed);
    assert!(resumed.is_declined("read_first"));
    assert!(resumed.is_declined("read_second"));
    assert!(!fresh.is_declined("read_first"));
    assert!(!fresh.is_declined("read_second"));
}

fn direct_payload(
    action: &str,
    value: &str,
    interrupt_id: &str,
) -> super::request::AgentExecutionPayload {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some(action.to_owned());
    request.payload.hitl_value = Some(value.to_owned());
    request.payload.hitl_decisions = vec![json!({
        "interrupt_id": interrupt_id,
        "tool_call_id": "call-1",
        "action": action,
        "value": value,
    })];
    request.payload
}

fn authorization_payload(
    action: &str,
    interrupt_id: &str,
) -> super::request::AgentExecutionPayload {
    let mut payload = direct_payload(action, "", interrupt_id);
    payload.hitl_decisions[0]["guardrail_type"] = json!("mcp_auth");
    if action == "authorize" {
        payload.mcp_tokens.insert(
            AUTH_SERVER.to_owned(),
            json!({"access_token": "runtime-secret"}),
        );
    } else {
        payload
            .user_declined_mcp_servers
            .push(json!({"server_url": AUTH_SERVER}));
    }
    payload
}

fn session(events: Vec<Event>) -> FixtureSession {
    FixtureSession { events }
}

fn sensitive_catalog(read_only: bool) -> SensitiveToolCatalog {
    let runtime = json!({
        "toolkit_security": {
            "sensitive_tools": {"fixture": ["double"]},
            "sensitive_action_company_name": "Example Org"
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
            .expect("sensitive tool policy"),
        read_only,
    )
    .expect("sensitive catalog")
}

fn admission_error(result: Result<DirectHitlDecision, DirectHitlError>) -> DirectHitlError {
    match result {
        Ok(_) => panic!("decision admission unexpectedly succeeded"),
        Err(error) => error,
    }
}

fn decision_set_error(result: Result<DirectHitlDecisionSet, DirectHitlError>) -> DirectHitlError {
    match result {
        Ok(_) => panic!("decision-set admission unexpectedly succeeded"),
        Err(error) => error,
    }
}

fn resolution_error(
    result: Result<ResolvedDirectHitlDecision, DirectHitlError>,
) -> DirectHitlError {
    match result {
        Ok(_) => panic!("decision resolution unexpectedly succeeded"),
        Err(error) => error,
    }
}

struct FixtureSession {
    events: Vec<Event>,
}

impl adk_rust::session::Session for FixtureSession {
    fn id(&self) -> &'static str {
        "session-1"
    }

    fn app_name(&self) -> &'static str {
        "elitea-agent-v1"
    }

    fn user_id(&self) -> &'static str {
        "user-1"
    }

    fn state(&self) -> &dyn adk_rust::session::State {
        self
    }

    fn events(&self) -> &dyn adk_rust::session::Events {
        self
    }

    fn last_update_time(&self) -> chrono::DateTime<chrono::Utc> {
        chrono::Utc::now()
    }
}

impl adk_rust::session::State for FixtureSession {
    fn get(&self, _key: &str) -> Option<Value> {
        None
    }

    fn set(&mut self, _key: String, _value: Value) {}

    fn all(&self) -> std::collections::HashMap<String, Value> {
        std::collections::HashMap::new()
    }
}

impl adk_rust::session::Events for FixtureSession {
    fn all(&self) -> Vec<Event> {
        self.events.clone()
    }

    fn len(&self) -> usize {
        self.events.len()
    }

    fn at(&self, index: usize) -> Option<&Event> {
        self.events.get(index)
    }
}

#[test]
fn direct_decision_resolves_only_the_exact_latest_persisted_call() {
    let arguments = json!({"value": 21, "api_token": "runtime-secret"});
    let events = pending_events(arguments.clone());
    let (interrupt_id, call_digest) =
        sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
            .expect("call identity");
    let decision = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .expect("decision admission");
    let resolved = decision
        .resolve(&session(events))
        .expect("exact session call");

    assert_eq!(resolved.interrupt_id(), interrupt_id);
    assert_eq!(resolved.call_digest(), call_digest);
    assert_eq!(resolved.call_id(), "call-1");
    assert_eq!(resolved.tool_name(), "double");
    assert_eq!(resolved.arguments(), &arguments);
    assert_eq!(resolved.decision(), ToolConfirmationDecision::Approve);
    assert_eq!(resolved.denial_comment(), None);
    assert_eq!(
        resolved.fingerprint(),
        adk_rust::tool_call_fingerprint("double", &arguments)
    );
}

#[test]
fn delegated_authorization_accepts_only_the_frozen_configuration_token_key() {
    let arguments = json!({});
    let (interrupt_id, _) =
        sensitive_call_identity("invocation-1", "call-1", "double", &arguments).unwrap();
    let mut events = pending_authorization_events(arguments);
    let requirement = authorization_requirement()
        .with_configured_oauth(
            "https://login.example.test",
            json!({"client_id":"public-id", "configuration_uuid":"config-1"})
                .as_object()
                .unwrap(),
        )
        .unwrap();
    events[1].provider_metadata.insert(
        DELEGATED_AUTHORIZATION_METADATA_KEY.into(),
        encode_delegated_authorization_requirement(&requirement).unwrap(),
    );
    for (key, accepted) in [
        ("config-1:https://login.example.test", true),
        ("config-2:https://login.example.test", false),
    ] {
        let mut payload = authorization_payload("authorize", &interrupt_id);
        payload.mcp_tokens.clear();
        payload
            .mcp_tokens
            .insert(key.into(), json!({"access_token":"runtime-secret"}));
        let result = DirectHitlDecisionSet::from_payload(&payload)
            .unwrap()
            .resolve(&session(events.clone()));
        assert_eq!(result.is_ok(), accepted);
    }
}

#[test]
fn delegated_authorization_decision_is_bound_to_interrupt_action_and_server_authority() {
    let arguments = json!({});
    let events = pending_authorization_events(arguments.clone());
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
        .expect("authorization identity");

    for (action, expected) in [
        ("authorize", ToolConfirmationDecision::Approve),
        ("skip", ToolConfirmationDecision::Deny),
    ] {
        let resolved =
            DirectHitlDecisionSet::from_payload(&authorization_payload(action, &interrupt_id))
                .expect("authorization decision admission")
                .resolve(&session(events.clone()))
                .expect("checkpoint-bound authorization decision");
        let ResolvedDirectHitlStart::Direct(decision) = resolved else {
            panic!("root authorization must resolve as one direct decision");
        };
        assert!(decision.is_delegated_authorization());
        assert_eq!(decision.decision(), expected);

        let mut materialized = DelegatedAuthorizationCatalog::default();
        if action == "skip" {
            materialized
                .insert("double", authorization_requirement())
                .expect("materialized authorization catalog");
        }
        decision
            .into_delegated_authorization_replay(&mut materialized)
            .expect("authorization replay");
    }

    let mut missing_token = authorization_payload("authorize", &interrupt_id);
    missing_token.mcp_tokens.clear();
    let Err(error) = DirectHitlDecisionSet::from_payload(&missing_token)
        .expect("bounded authorization decision")
        .resolve(&session(events.clone()))
    else {
        panic!("authorization without exact server authority was admitted");
    };
    assert_eq!(error.code(), DirectHitlErrorCode::StaleDecision);

    let mut sensitive_action = authorization_payload("authorize", &interrupt_id);
    sensitive_action.hitl_action = Some("approve".to_owned());
    sensitive_action.hitl_decisions[0]["action"] = json!("approve");
    let Err(error) = DirectHitlDecisionSet::from_payload(&sensitive_action)
        .expect("bounded mismatched guardrail decision")
        .resolve(&session(events))
    else {
        panic!("sensitive action was admitted for authorization guardrail");
    };
    assert_eq!(error.code(), DirectHitlErrorCode::StaleDecision);
}

#[test]
fn direct_replay_rejects_effectful_tools_before_model_or_tool_execution() {
    let arguments = json!({"value": 21});
    let events = pending_events(arguments.clone());
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
        .expect("call identity");
    let resolved = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .expect("decision admission")
        .resolve(&session(events))
        .expect("exact session call");
    let Err(error) = resolved.into_direct_replay(&sensitive_catalog(false)) else {
        panic!("effectful direct tool was admitted for replay");
    };
    assert_eq!(error.code(), DirectHitlErrorCode::UnsupportedCapability);
}

#[test]
fn exact_partial_replay_suffix_is_restartable_but_completed_output_is_stale() {
    let arguments = json!({"value": 21});
    let mut events = pending_events(arguments.clone());
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
        .expect("call identity");
    let marker_text = format!(
        "[Elitea direct HITL {interrupt_id}] The pending tool call was approved. Continue the original request."
    );
    let mut marker = Event::with_id("resume-user", "invocation-2");
    marker.author = "user".to_owned();
    marker.llm_response.content = Some(Content::new("user").with_text(marker_text));
    events.push(marker);
    let pending = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .expect("pending decision")
        .resolve(&session(events.clone()))
        .expect("exact marker-only suffix");
    assert!(!pending.has_persisted_result());

    let mut call = Event::with_id("resume-call", "invocation-2");
    call.author = "elitea-agent".to_owned();
    call.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: "double".to_owned(),
            args: arguments.clone(),
            id: Some("call-1".to_owned()),
            thought_signature: None,
        }],
    });
    events.push(call);
    let pending = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .expect("pending call decision")
        .resolve(&session(events.clone()))
        .expect("exact call suffix");
    assert!(!pending.has_persisted_result());

    let mut result = Event::with_id("resume-result", "invocation-2");
    result.author = "elitea-agent".to_owned();
    result.actions.tool_confirmation_decision = Some(ToolConfirmationDecision::Approve);
    result.llm_response.content = Some(Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: adk_rust::FunctionResponseData::new("double", json!({"value": 42})),
            id: Some("call-1".to_owned()),
            annotations: None,
        }],
    });
    events.push(result);
    let completed = DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
        .expect("completed decision")
        .resolve(&session(events.clone()))
        .expect("exact persisted result");
    assert!(completed.has_persisted_result());

    let mut final_output = Event::with_id("resume-final", "invocation-2");
    final_output.author = "elitea-agent".to_owned();
    final_output.llm_response.content = Some(Content::new("model").with_text("already complete"));
    events.push(final_output);
    let error = resolution_error(
        DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
            .expect("terminal decision")
            .resolve(&session(events)),
    );
    assert_eq!(error.code(), DirectHitlErrorCode::StaleDecision);
}

#[test]
fn direct_block_comment_maps_to_deny_without_becoming_diagnostic_data() {
    let arguments = json!({"value": 21});
    let events = pending_events(arguments.clone());
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
        .expect("call identity");
    let decision = DirectHitlDecision::from_payload(&direct_payload(
        "block_with_comment",
        "retain this record",
        &interrupt_id,
    ))
    .expect("block decision");
    let resolved = decision
        .resolve(&session(events))
        .expect("exact session call");

    assert_eq!(resolved.decision(), ToolConfirmationDecision::Deny);
    assert_eq!(resolved.denial_comment(), Some("retain this record"));
}

#[test]
fn direct_decision_rejects_stale_tampered_or_already_advanced_sessions() {
    let arguments = json!({"value": 21});
    let events = pending_events(arguments.clone());
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &arguments)
        .expect("call identity");

    let stale = resolution_error(
        DirectHitlDecision::from_payload(&direct_payload("approve", "", "hitl_e1:stale"))
            .expect("bounded stale decision")
            .resolve(&session(events.clone())),
    );
    assert_eq!(stale.code(), DirectHitlErrorCode::StaleDecision);

    let mut tampered = direct_payload("approve", "", &interrupt_id);
    tampered.hitl_decisions[0]["tool_call_id"] = json!("call-other");
    let error = resolution_error(
        DirectHitlDecision::from_payload(&tampered)
            .expect("bounded mismatched call")
            .resolve(&session(events.clone())),
    );
    assert_eq!(error.code(), DirectHitlErrorCode::StaleDecision);

    let mut advanced = events;
    let mut completed = Event::with_id("decision-event", "invocation-2");
    completed.actions.tool_confirmation_decision = Some(ToolConfirmationDecision::Deny);
    advanced.push(completed);
    let error = resolution_error(
        DirectHitlDecision::from_payload(&direct_payload("approve", "", &interrupt_id))
            .expect("decision admission")
            .resolve(&session(advanced)),
    );
    assert_eq!(error.code(), DirectHitlErrorCode::StaleDecision);
}

#[test]
fn direct_decision_admission_is_strict_and_bounded() {
    let mut missing_continue = direct_payload("approve", "", "hitl_e1:one");
    missing_continue.should_continue = false;
    assert_eq!(
        admission_error(DirectHitlDecision::from_payload(&missing_continue)).code(),
        DirectHitlErrorCode::UnsupportedCapability
    );

    let mut inconsistent = direct_payload("reject", "", "hitl_e1:one");
    inconsistent.hitl_action = Some("approve".to_owned());
    assert_eq!(
        admission_error(DirectHitlDecision::from_payload(&inconsistent)).code(),
        DirectHitlErrorCode::InvalidInput
    );

    let mut missing_scalar_value = direct_payload("approve", "", "hitl_e1:one");
    missing_scalar_value.hitl_value = None;
    assert_eq!(
        admission_error(DirectHitlDecision::from_payload(&missing_scalar_value)).code(),
        DirectHitlErrorCode::InvalidInput
    );

    let mut unknown = direct_payload("approve", "", "hitl_e1:one");
    unknown.hitl_decisions[0]["extra"] = json!(true);
    assert_eq!(
        admission_error(DirectHitlDecision::from_payload(&unknown)).code(),
        DirectHitlErrorCode::InvalidInput
    );

    let oversized_comment = "x".repeat(2_001);
    assert_eq!(
        admission_error(DirectHitlDecision::from_payload(&direct_payload(
            "block_with_comment",
            &oversized_comment,
            "hitl_e1:one",
        )))
        .code(),
        DirectHitlErrorCode::InvalidInput
    );
}

#[test]
fn parallel_decision_set_is_bounded_unique_and_has_no_scalar_alias() {
    let mut payload = direct_payload("approve", "", "hitl_e1:one");
    payload.hitl_action = None;
    payload.hitl_value = None;
    payload.hitl_decisions.push(json!({
        "interrupt_id": "hitl_e1:two",
        "tool_call_id": "call-1",
        "action": "block_with_comment",
        "value": "retain this record",
    }));
    DirectHitlDecisionSet::from_payload(&payload).expect("parallel decision set");

    payload.hitl_action = Some("approve".to_owned());
    assert_eq!(
        decision_set_error(DirectHitlDecisionSet::from_payload(&payload)).code(),
        DirectHitlErrorCode::InvalidInput
    );
    payload.hitl_action = None;
    payload.hitl_decisions[1]["interrupt_id"] = json!("hitl_e1:one");
    assert_eq!(
        decision_set_error(DirectHitlDecisionSet::from_payload(&payload)).code(),
        DirectHitlErrorCode::InvalidInput
    );

    payload.hitl_decisions = (0..17)
        .map(|ordinal| {
            json!({
                "interrupt_id": format!("hitl_e1:{ordinal}"),
                "tool_call_id": "call-1",
                "action": "approve",
                "value": "",
            })
        })
        .collect();
    assert_eq!(
        decision_set_error(DirectHitlDecisionSet::from_payload(&payload)).code(),
        DirectHitlErrorCode::UnsupportedCapability
    );
}

// ─────────────────────────────────────────────────────────────────────────────
// A MESSAGE THAT MADE SEVERAL CALLS IS RESUMED AS A MESSAGE (#948, #949)
// ─────────────────────────────────────────────────────────────────────────────
//
// ADK's confirmation pre-check (`llm_agent.rs`) breaks out of its scan at the
// FIRST call that needs a decision and returns before any tool of that message
// is dispatched — so a pause abandons the whole assistant message, not just the
// sensitive call in it. The replay therefore has to re-emit the whole message.

/// One model event with `calls`, then a confirmation for `confirm`.
fn multi_call_events(calls: &[(&str, &str, Value)], confirm: usize) -> Vec<Event> {
    let mut call = Event::with_id("call-event", "invocation-1");
    call.author = "elitea-agent".to_owned();
    call.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: calls
            .iter()
            .map(|(call_id, tool_name, arguments)| Part::FunctionCall {
                name: (*tool_name).to_owned(),
                args: arguments.clone(),
                id: Some((*call_id).to_owned()),
                thought_signature: None,
            })
            .collect(),
    });
    let (call_id, tool_name, arguments) = &calls[confirm];
    let mut confirmation = Event::with_id("confirmation-event", "invocation-1");
    confirmation.author = "elitea-agent".to_owned();
    confirmation.llm_response.interrupted = true;
    confirmation.llm_response.turn_complete = true;
    confirmation.actions.tool_confirmation = Some(ToolConfirmationRequest {
        tool_name: (*tool_name).to_owned(),
        function_call_id: Some((*call_id).to_owned()),
        args: arguments.clone(),
    });
    vec![call, confirmation]
}

fn multi_call_payload(
    action: &str,
    value: &str,
    interrupt_id: &str,
    call_id: &str,
) -> super::request::AgentExecutionPayload {
    let mut payload = direct_payload(action, value, interrupt_id);
    payload.hitl_decisions[0]["tool_call_id"] = json!(call_id);
    payload
}

/// A catalogue naming every tool in `tool_names` sensitive.
fn sensitive_catalog_for(tool_names: &[&str], read_only: bool) -> SensitiveToolCatalog {
    let runtime = json!({
        "toolkit_security": {
            "sensitive_tools": {"fixture": tool_names},
            "sensitive_action_company_name": "Example Org"
        }
    });
    let policy = ToolAdmissionPolicy::from_runtime_config(
        runtime.as_object().expect("runtime configuration object"),
    )
    .expect("runtime policy");
    let mut catalog = SensitiveToolCatalog::default();
    for tool_name in tool_names {
        catalog
            .merge(
                SensitiveToolCatalog::fixture(
                    tool_name,
                    policy
                        .sensitive_tool("fixture", "Fixture Tools", tool_name)
                        .expect("sensitive tool policy"),
                    read_only,
                )
                .expect("sensitive catalog"),
            )
            .expect("merged catalog");
    }
    catalog
}

struct NeverCalledLlm;

#[async_trait::async_trait]
impl adk_rust::Llm for NeverCalledLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        _request: adk_rust::LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<adk_rust::LlmResponseStream> {
        Err(adk_rust::AdkError::agent(
            "the replay must not reach the provider on its first generation",
        ))
    }
}

/// The request ADK hands the replay model: the paused message still pending.
fn replay_request(calls: &[(&str, &str, Value)]) -> adk_rust::LlmRequest {
    adk_rust::LlmRequest {
        model: "fixture-model".to_owned(),
        contents: vec![Content {
            role: "model".to_owned(),
            parts: calls
                .iter()
                .map(|(call_id, tool_name, arguments)| Part::FunctionCall {
                    name: (*tool_name).to_owned(),
                    args: arguments.clone(),
                    id: Some((*call_id).to_owned()),
                    thought_signature: None,
                })
                .collect(),
        }],
        config: None,
        tools: calls
            .iter()
            .map(|(_, tool_name, _)| ((*tool_name).to_owned(), json!({"description": "fixture"})))
            .collect(),
        previous_response_id: None,
    }
}

/// The function calls one generation of `model` emits.
async fn emitted_calls(model: &dyn adk_rust::Llm, request: adk_rust::LlmRequest) -> Vec<String> {
    use adk_rust::futures::StreamExt as _;

    let mut stream = model
        .generate_content(request, false)
        .await
        .expect("replay generation");
    let mut emitted = Vec::new();
    while let Some(response) = stream.next().await {
        let response = response.expect("replay response");
        let Some(content) = response.content else {
            continue;
        };
        for part in content.parts {
            if let Part::FunctionCall { id: Some(id), .. } = part {
                emitted.push(id);
            }
        }
    }
    emitted
}

#[tokio::test]
async fn blocking_one_sensitive_call_replays_the_non_sensitive_call_beside_it() {
    // ELITEA-1001 (#949): the non-sensitive call of a two-call message was
    // never dispatched, because the replay re-emitted ONLY the decided call.
    let sensitive = json!({"value": 21});
    let ordinary = json!({"topic": "status"});
    let calls = [
        ("call-1", "status", ordinary.clone()),
        ("call-2", "double", sensitive.clone()),
    ];
    let events = multi_call_events(&calls, 1);
    let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-2", "double", &sensitive)
        .expect("call identity");
    let resolved = DirectHitlDecision::from_payload(&multi_call_payload(
        "block_with_comment",
        "not this one",
        &interrupt_id,
        "call-2",
    ))
    .expect("decision admission")
    .resolve(&session(events))
    .expect("exact session call");

    let replay = resolved
        .into_direct_replay(&sensitive_catalog_for(&["double"], false))
        .expect("blocked replay");
    assert_eq!(replay.replay_call_ids(), vec!["call-1", "call-2"]);
    // Only the declined call is answered locally; the other one still runs.
    assert_eq!(replay.blocked_calls(), vec![("call-2", "not this one")]);
    assert_eq!(replay.approved_call_ids(), vec!["call-2"]);

    let prepared = replay.bind(std::sync::Arc::new(NeverCalledLlm));
    assert_eq!(
        emitted_calls(prepared.model().as_ref(), replay_request(&calls)).await,
        vec!["call-1".to_owned(), "call-2".to_owned()],
        "the replay dropped the call the user never decided about"
    );
}

#[tokio::test]
async fn a_second_sensitive_call_keeps_the_first_decision_and_is_offered_on_its_own() {
    // ELITEA-1003 (#948): both calls are sensitive, so ADK pauses on the first
    // one, and the SECOND resume must still carry the first decision — an
    // omitted decision would raise the very same card again.
    let first = json!({"value": 21});
    let second = json!({"value": 42});
    let calls = [
        ("call-1", "double", first.clone()),
        ("call-2", "triple", second.clone()),
    ];
    let mut events = multi_call_events(&calls, 0);
    let (first_interrupt, _) = sensitive_call_identity("invocation-1", "call-1", "double", &first)
        .expect("first call identity");

    // ── Resume one: the first card is approved. ─────────────────────────────
    let replay = DirectHitlDecision::from_payload(&multi_call_payload(
        "approve",
        "",
        &first_interrupt,
        "call-1",
    ))
    .expect("first decision admission")
    .resolve(&session(events.clone()))
    .expect("first exact session call")
    .into_direct_replay(&sensitive_catalog_for(&["double", "triple"], true))
    .expect("first replay");
    assert_eq!(replay.replay_call_ids(), vec!["call-1", "call-2"]);
    assert!(replay.blocked_calls().is_empty());
    assert_eq!(
        replay.approved_call_ids(),
        vec!["call-1"],
        "the undecided sensitive call must stay undecided so ADK raises its own card"
    );

    // ── What that resume persists: the marker, the replayed message, and the
    //    second call's own confirmation. ─────────────────────────────────────
    let mut marker = Event::with_id("resume-user", "invocation-2");
    marker.author = "user".to_owned();
    marker.llm_response.content = Some(Content::new("user").with_text(format!(
        "[Elitea direct HITL {first_interrupt}] The pending tool call was approved. Continue the original request."
    )));
    events.push(marker);
    let mut replayed = multi_call_events(&calls, 1);
    for event in &mut replayed {
        event.invocation_id = "invocation-2".to_owned();
    }
    events.extend(replayed);

    // ── Resume two: the second card, decided on its own interrupt id. ───────
    let (second_interrupt, _) =
        sensitive_call_identity("invocation-2", "call-2", "triple", &second)
            .expect("second call identity");
    assert_ne!(
        first_interrupt, second_interrupt,
        "two pauses that share an interrupt id cannot be decided independently"
    );
    let replay = DirectHitlDecision::from_payload(&multi_call_payload(
        "reject",
        "",
        &second_interrupt,
        "call-2",
    ))
    .expect("second decision admission")
    .resolve(&session(events))
    .expect("second exact session call")
    .into_direct_replay(&sensitive_catalog_for(&["double", "triple"], true))
    .expect("second replay");
    assert_eq!(replay.replay_call_ids(), vec!["call-1", "call-2"]);
    assert_eq!(replay.blocked_calls(), vec![("call-2", "denied by user")]);
    let mut approved = replay.approved_call_ids();
    approved.sort_unstable();
    assert_eq!(
        approved,
        vec!["call-1", "call-2"],
        "the first card's approval was lost, so ADK would raise it a second time"
    );

    let prepared = replay.bind(std::sync::Arc::new(NeverCalledLlm));
    assert_eq!(prepared.confirmed_call_ids(), vec!["call-1", "call-2"]);
}

#[test]
fn same_tool_siblings_need_separate_decisions_even_with_identical_arguments() {
    let first = json!({"value": 21});
    for second in [first.clone(), json!({"value": 42})] {
        let calls = [
            ("call-1", "double", first.clone()),
            ("call-2", "double", second),
        ];
        let events = multi_call_events(&calls, 0);
        let (interrupt_id, _) = sensitive_call_identity("invocation-1", "call-1", "double", &first)
            .expect("call identity");
        for (action, comment) in [("approve", ""), ("block_with_comment", "stop")] {
            let replay = DirectHitlDecision::from_payload(&multi_call_payload(
                action,
                comment,
                &interrupt_id,
                "call-1",
            ))
            .expect("decision admission")
            .resolve(&session(events.clone()))
            .expect("exact session call")
            .into_direct_replay(&sensitive_catalog(true))
            .expect("exact call replay");
            assert_eq!(replay.replay_call_ids(), vec!["call-1", "call-2"]);
            assert_eq!(replay.approved_call_ids(), vec!["call-1"]);
            assert_eq!(
                replay.blocked_calls(),
                if action == "approve" {
                    Vec::new()
                } else {
                    vec![("call-1", "stop")]
                },
                "a denial must replace only the decided invocation"
            );
        }
    }
}

struct SameToolReplayFixture {
    sessions: Arc<InMemorySessionService>,
    calls: Arc<Mutex<Vec<(String, Value)>>>,
    requests: Arc<Mutex<Vec<adk_rust::LlmRequest>>>,
}

impl SameToolReplayFixture {
    async fn new(second_arguments: Value) -> Self {
        let sessions = Arc::new(InMemorySessionService::new());
        sessions
            .create(CreateRequest {
                app_name: "elitea-agent-v1".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: Some("session-1".to_owned()),
                state: std::collections::HashMap::new(),
            })
            .await
            .expect("fixture session");
        let calls = [
            ("call-1", "double", json!({"value": 21})),
            ("call-2", "double", second_arguments),
        ];
        for event in multi_call_events(&calls, 0) {
            sessions
                .append_event("session-1", event)
                .await
                .expect("persisted initial pause");
        }
        Self {
            sessions,
            calls: Arc::default(),
            requests: Arc::default(),
        }
    }

    async fn stored_session(&self) -> Box<dyn adk_rust::session::Session> {
        self.sessions
            .get(GetRequest {
                app_name: "elitea-agent-v1".to_owned(),
                user_id: "user-1".to_owned(),
                session_id: "session-1".to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("persisted replay session")
    }

    async fn resume(&self, call_id: &str, action: &str) -> (String, Vec<Event>) {
        let stored = self.stored_session().await;
        let events = stored.events().all();
        let confirmation = events.last().expect("pending confirmation");
        let request = confirmation
            .actions
            .tool_confirmation
            .as_ref()
            .expect("pending card");
        assert_eq!(request.function_call_id.as_deref(), Some(call_id));
        let (interrupt_id, _) = sensitive_call_identity(
            &confirmation.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )
        .expect("pending card identity");
        let replay = DirectHitlDecision::from_payload(&multi_call_payload(
            action,
            "",
            &interrupt_id,
            call_id,
        ))
        .expect("exact decision")
        .resolve(stored.as_ref())
        .expect("persisted decision resolution")
        .into_direct_replay(&sensitive_catalog(true))
        .expect("read-only replay");
        let tool: Arc<dyn adk_rust::Tool> = Arc::new(RecordedDoubleTool {
            calls: Arc::clone(&self.calls),
        });
        let prepared = replay.bind(Arc::new(ReplayFinalLlm {
            requests: Arc::clone(&self.requests),
        }));
        let (model, input, toolsets) = prepared.into_parts(vec![Arc::new(BasicToolset::new(
            "fixture-tools",
            vec![tool],
        ))]);
        let mut agent = LlmAgentBuilder::new("elitea-agent")
            .model(model)
            .require_tool_confirmation("double");
        for toolset in toolsets {
            agent = agent.toolset(toolset);
        }
        let (user_content, run_config) = input.into_parts();
        let runner = Runner::builder()
            .app_name("elitea-agent-v1")
            .agent(Arc::new(agent.build().expect("replay agent")))
            .session_service(self.sessions.clone())
            .run_config(run_config)
            .build()
            .expect("replay runner");
        let mut stream = runner
            .run(
                adk_rust::UserId::new("user-1").expect("user identity"),
                adk_rust::SessionId::new("session-1").expect("session identity"),
                user_content,
            )
            .await
            .expect("replay stream");
        let mut emitted = Vec::new();
        while let Some(event) = stream.next().await {
            emitted.push(event.expect("replay event"));
        }
        (interrupt_id, emitted)
    }
}

struct RecordedDoubleTool {
    calls: Arc<Mutex<Vec<(String, Value)>>>,
}

#[async_trait::async_trait]
impl adk_rust::Tool for RecordedDoubleTool {
    fn name(&self) -> &'static str {
        "double"
    }

    fn description(&self) -> &'static str {
        "Double one integer."
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(
        &self,
        context: Arc<dyn adk_rust::ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let value = arguments["value"]
            .as_i64()
            .ok_or_else(|| adk_rust::AdkError::agent("fixture integer missing"))?;
        self.calls
            .lock()
            .map_err(|_| adk_rust::AdkError::agent("fixture call lock failed"))?
            .push((context.function_call_id().to_owned(), arguments));
        Ok(json!({"value": value * 2}))
    }
}

struct ReplayFinalLlm {
    requests: Arc<Mutex<Vec<adk_rust::LlmRequest>>>,
}

#[async_trait::async_trait]
impl adk_rust::Llm for ReplayFinalLlm {
    fn name(&self) -> &'static str {
        "fixture-model"
    }

    async fn generate_content(
        &self,
        request: adk_rust::LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<adk_rust::LlmResponseStream> {
        self.requests
            .lock()
            .map_err(|_| adk_rust::AdkError::agent("fixture request lock failed"))?
            .push(request);
        Ok(Box::pin(adk_rust::futures::stream::once(async {
            Ok(adk_rust::LlmResponse::new(
                Content::new("model").with_text("Both calls completed."),
            ))
        })))
    }
}

async fn finish_same_tool_replay(
    fixture: &SameToolReplayFixture,
    first_action: &str,
    second_action: &str,
    second_arguments: &Value,
) -> Vec<Event> {
    let (first_interrupt, first_run) = fixture.resume("call-1", first_action).await;
    let cards = first_run
        .iter()
        .filter_map(|event| event.actions.tool_confirmation.as_ref())
        .collect::<Vec<_>>();
    assert_eq!(cards.len(), 1);
    assert_eq!(cards[0].function_call_id.as_deref(), Some("call-2"));
    assert_eq!(cards[0].args, *second_arguments);
    // ADK pre-checks the whole message before dispatching any tool.
    assert!(fixture.calls.lock().expect("recorded calls").is_empty());
    assert!(
        fixture
            .requests
            .lock()
            .expect("provider requests")
            .is_empty()
    );
    let (second_interrupt, second_run) = fixture.resume("call-2", second_action).await;
    assert_ne!(first_interrupt, second_interrupt);
    assert!(
        second_run
            .iter()
            .all(|event| event.actions.tool_confirmation.is_none())
    );
    assert_eq!(fixture.requests.lock().expect("provider requests").len(), 1);
    second_run
}

fn assert_same_tool_results(fixture: &SameToolReplayFixture, expected: &[(&str, Value)]) {
    let requests = fixture.requests.lock().expect("provider requests");
    let responses = requests[0]
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .filter_map(|part| match part {
            Part::FunctionResponse {
                function_response,
                id: Some(id),
                ..
            } => Some((id.as_str(), &function_response.response)),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(responses.len(), 2);
    for (call_id, response) in expected {
        assert_eq!(
            responses.iter().filter(|(id, _)| id == call_id).count(),
            1,
            "each call has exactly one result in provider history"
        );
        assert_eq!(
            responses.iter().find(|(id, _)| id == call_id).unwrap().1,
            response
        );
    }
}

#[tokio::test]
async fn same_tool_siblings_pause_separately_then_execute_once() {
    for second_arguments in [json!({"value": 21}), json!({"value": 42})] {
        let fixture = SameToolReplayFixture::new(second_arguments.clone()).await;
        let final_run =
            finish_same_tool_replay(&fixture, "approve", "approve", &second_arguments).await;
        assert_eq!(
            *fixture.calls.lock().expect("recorded calls"),
            vec![
                ("call-1".to_owned(), json!({"value": 21})),
                ("call-2".to_owned(), second_arguments.clone()),
            ],
            "each distinct approved invocation executes exactly once"
        );
        let second_result = second_arguments["value"].as_i64().unwrap() * 2;
        assert_same_tool_results(
            &fixture,
            &[
                ("call-1", json!({"value": 42})),
                ("call-2", json!({"value": second_result})),
            ],
        );
        assert!(final_run.iter().any(|event| {
            event.content().is_some_and(|content| {
                content.parts
                    == Content::new("model")
                        .with_text("Both calls completed.")
                        .parts
            })
        }));
        let stored = fixture.stored_session().await;
        assert_eq!(
            stored
                .events()
                .all()
                .iter()
                .flat_map(Event::tool_results)
                .count(),
            2,
            "results persist once across both resumes"
        );
    }
}

#[tokio::test]
async fn same_tool_sibling_denials_stay_bound_to_their_call_ids() {
    let second_arguments = json!({"value": 42});
    for (first_action, second_action, approved_id, approved_arguments, blocked_id) in [
        (
            "reject",
            "approve",
            "call-2",
            second_arguments.clone(),
            "call-1",
        ),
        (
            "approve",
            "reject",
            "call-1",
            json!({"value": 21}),
            "call-2",
        ),
    ] {
        let fixture = SameToolReplayFixture::new(second_arguments.clone()).await;
        let final_run =
            finish_same_tool_replay(&fixture, first_action, second_action, &second_arguments).await;
        assert_eq!(
            *fixture.calls.lock().expect("recorded calls"),
            vec![(approved_id.to_owned(), approved_arguments.clone())],
            "denial blocks only its invocation; the approved sibling runs once"
        );
        let blocked = super::direct_hitl::blocked_tool_result(
            "double",
            "Fixture Tools",
            "fixture",
            "Fixture Tools.double",
            None,
        );
        let approved_result = approved_arguments["value"].as_i64().unwrap() * 2;
        assert_same_tool_results(
            &fixture,
            &[
                (blocked_id, blocked),
                (approved_id, json!({"value": approved_result})),
            ],
        );
        let denied = final_run
            .iter()
            .filter(|event| {
                event.actions.tool_confirmation_decision == Some(ToolConfirmationDecision::Deny)
            })
            .flat_map(Event::tool_results)
            .collect::<Vec<_>>();
        assert_eq!(denied.len(), 1);
        assert_eq!(denied[0].call_id, Some(blocked_id));
    }
}

#[test]
fn a_denial_comment_survives_into_the_next_resume_of_the_same_message() {
    // The marker is the only place an earlier card's comment survives, so the
    // SECOND resume's reconstruction of the first blocked result reads it back.
    let first = json!({"value": 21});
    let second = json!({"value": 42});
    let calls = [
        ("call-1", "double", first.clone()),
        ("call-2", "triple", second.clone()),
    ];
    let mut events = multi_call_events(&calls, 0);
    let (first_interrupt, _) = sensitive_call_identity("invocation-1", "call-1", "double", &first)
        .expect("first call identity");
    let mut marker = Event::with_id("resume-user", "invocation-2");
    marker.author = "user".to_owned();
    marker.llm_response.content = Some(Content::new("user").with_text(format!(
        "[Elitea direct HITL {first_interrupt}] The pending tool call was rejected. Continue without executing it. Reviewer comment: not on my watch"
    )));
    events.push(marker);
    let mut replayed = multi_call_events(&calls, 1);
    for event in &mut replayed {
        event.invocation_id = "invocation-2".to_owned();
    }
    events.extend(replayed);

    let (second_interrupt, _) =
        sensitive_call_identity("invocation-2", "call-2", "triple", &second)
            .expect("second call identity");
    let replay = DirectHitlDecision::from_payload(&multi_call_payload(
        "approve",
        "",
        &second_interrupt,
        "call-2",
    ))
    .expect("second decision admission")
    .resolve(&session(events))
    .expect("second exact session call")
    .into_direct_replay(&sensitive_catalog_for(&["double", "triple"], true))
    .expect("second replay");
    assert_eq!(
        replay.blocked_calls(),
        vec![("call-1", "not on my watch")],
        "the first card's own words were lost on the way to the second resume"
    );
}
