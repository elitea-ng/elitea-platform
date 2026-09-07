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
