//! A direct agent's tool call that PAUSES for the user (zefir-ca/agent-zefir#21).
//!
//! Client contract 1.2 ends such a call with `agent_tool_paused` — `error:
//! null`, a pause `finish_reason` and `pause: {interrupt_id, guardrail_type}` —
//! and describes the pending interrupt as an OBJECT in
//! `response_metadata.hitl_interrupt` (`ClientFrameHitlInterruptDetail`). The
//! Python worker has done both since #1066. This runtime emitted
//! `agent_tool_start` and then the interrupt card with `hitl_interrupt: true`,
//! so a client kept the call spinning forever, elitea-main stored its trace row
//! with no finish reason, and a client reading the documented detail object got
//! a boolean.
//!
//! A pipeline LLM node's call that pauses (a sensitive tool or `ask_user`)
//! ends the same way; `pipeline_tests.rs` (`assert_llm_node_call_paused`)
//! proves it for a root node and for a nested pipeline, whose paused frame is
//! rewritten to the public interrupt id with its card. A delegated MCP
//! authorization pause deliberately sends no tool frame: it is answered by
//! `mcp_authorization_required`, matching the Python worker, which records the
//! call as `action_required` without emitting a tool event.

use adk_rust::{Content, Event, FinishReason, Part, ToolConfirmationRequest};
use chrono::{TimeZone, Utc};
use serde_json::{Value, json};

use super::events::{AgentEventProjectionContext, AgentEventProjector};
use super::internal_tools::{
    ASK_USER_METADATA_KEY, ASK_USER_TOOL_NAME, AskUserRequest, encode_ask_user_request,
};
use super::sensitive_tools::SensitiveToolCatalog;
use crate::protocol::node_event::encode_current_node_event_json;
use crate::toolkits::ToolAdmissionPolicy;

fn sensitive_catalog() -> SensitiveToolCatalog {
    let runtime = json!({"toolkit_security": {
        "sensitive_tools": {"fixture": ["delete_branch"]},
        "sensitive_action_company_name": "Example Org"
    }});
    let policy = ToolAdmissionPolicy::from_runtime_config(
        runtime.as_object().expect("runtime security dictionary"),
    )
    .expect("runtime policy");
    SensitiveToolCatalog::fixture(
        "delete_branch",
        policy
            .sensitive_tool("fixture", "Fixture Tools", "delete_branch")
            .expect("sensitive fixture policy"),
        false,
    )
    .expect("sensitive fixture catalog")
}

fn at(second: u32) -> chrono::DateTime<Utc> {
    Utc.with_ymd_and_hms(2026, 10, 7, 12, 0, second)
        .single()
        .expect("fixture timestamp")
}

fn call_event(name: &str, args: &Value) -> Event {
    let mut event = Event::with_id("llm-call", "invocation-1");
    event.timestamp = at(1);
    event.author = "root-agent".to_owned();
    event.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: name.to_owned(),
            args: args.clone(),
            id: Some("call-1".to_owned()),
            thought_signature: None,
        }],
    });
    event.llm_response.turn_complete = true;
    event.llm_response.finish_reason = Some(FinishReason::Stop);
    event
}

fn confirmation_event(name: &str, args: &Value) -> Event {
    let mut event = Event::with_id("confirmation", "invocation-1");
    event.timestamp = at(2);
    event.author = "root-agent".to_owned();
    event.llm_response.interrupted = true;
    event.llm_response.turn_complete = true;
    event.actions.tool_confirmation = Some(ToolConfirmationRequest {
        tool_name: name.to_owned(),
        function_call_id: Some("call-1".to_owned()),
        args: args.clone(),
    });
    event
}

/// Every browser frame of a call that pauses, in emission order.
fn paused_frames(
    projector: &mut AgentEventProjector,
    name: &str,
    args: &Value,
    ask: bool,
) -> Vec<Value> {
    projector.start(at(0)).expect("start");
    let mut confirmation = confirmation_event(name, args);
    if ask {
        let request = AskUserRequest::from_arguments(args).expect("ask_user arguments");
        confirmation.provider_metadata.insert(
            ASK_USER_METADATA_KEY.to_owned(),
            encode_ask_user_request(&request).expect("ask_user metadata"),
        );
    }
    let mut frames = Vec::new();
    for event in [call_event(name, args), confirmation] {
        for projected in projector.project(&event).expect("projection") {
            frames.push(
                serde_json::from_slice(
                    &encode_current_node_event_json(&projected).expect("browser frame"),
                )
                .expect("frame JSON"),
            );
        }
    }
    frames
}

fn types(frames: &[Value]) -> Vec<&str> {
    frames
        .iter()
        .map(|frame| frame["type"].as_str().unwrap_or_default())
        .collect()
}

fn only<'a>(frames: &'a [Value], frame_type: &str) -> &'a Value {
    let matching: Vec<&Value> = frames.iter().filter(|f| f["type"] == frame_type).collect();
    assert_eq!(
        matching.len(),
        1,
        "exactly one {frame_type} in {:?}",
        types(frames)
    );
    matching[0]
}

fn assert_paused_call(frames: &[Value], reason: &str, guardrail: &str, tool: &str) {
    let order = types(frames);
    let start = order
        .iter()
        .position(|t| *t == "agent_tool_start")
        .expect("tool start");
    let paused = order
        .iter()
        .position(|t| *t == "agent_tool_paused")
        .unwrap_or_else(|| panic!("a paused call must end with agent_tool_paused, got {order:?}"));
    let interrupt = order
        .iter()
        .position(|t| *t == "agent_hitl_interrupt")
        .expect("interrupt");
    assert!(
        start < paused && paused < interrupt,
        "frame order {order:?}"
    );
    assert!(
        !order.contains(&"agent_tool_error"),
        "a pause is not a failure: {order:?}"
    );

    let card = &only(frames, "agent_hitl_interrupt")["response_metadata"];
    let detail = &card["hitl_interrupt"];
    assert!(
        detail.is_object(),
        "hitl_interrupt must be the pending interrupt object, got {detail}"
    );
    assert_eq!(
        detail, &card["hitl_interrupts"][0],
        "the singular is the one pending interrupt"
    );
    let interrupt_id = detail["interrupt_id"].as_str().expect("interrupt id");
    assert_eq!(detail["guardrail_type"], guardrail);
    assert_eq!(detail["tool_call_id"], "call-1");

    let metadata = &only(frames, "agent_tool_paused")["response_metadata"];
    assert_eq!(metadata["tool_run_id"], "call-1");
    assert_eq!(metadata["tool_name"], tool);
    assert_eq!(metadata["finish_reason"], reason);
    assert_eq!(metadata["error"], Value::Null);
    assert!(metadata["timestamp_finish"].is_string(), "{metadata}");
    assert_eq!(
        metadata["pause"],
        json!({"interrupt_id": interrupt_id, "guardrail_type": guardrail})
    );

    // elitea-main stores trace rows from partial_message tool_calls, so the
    // stored call must carry the same pause (agent_trace.go
    // currentAgentToolCallOutcome / currentAgentToolCallPauseAttrs).
    let stored = frames
        .iter()
        .rev()
        .filter(|f| f["type"] == "partial_message")
        .find_map(|f| f["response_metadata"]["tool_calls"].get("call-1"))
        .expect("a partial_message for the call");
    assert_eq!(stored["finish_reason"], reason, "{stored}");
    assert_eq!(stored["pause"]["interrupt_id"], interrupt_id);
}

#[test]
fn a_sensitive_tool_pause_ends_the_call_with_agent_tool_paused() {
    let mut projector = AgentEventProjector::with_sensitive_tools(
        AgentEventProjectionContext::fixture(json!({})),
        sensitive_catalog(),
    )
    .expect("projector");
    let frames = paused_frames(
        &mut projector,
        "delete_branch",
        &json!({"branch": "old"}),
        false,
    );
    assert_paused_call(
        &frames,
        "awaiting_approval",
        "sensitive_tool",
        "delete_branch",
    );
}

#[test]
fn a_clarifying_question_ends_the_call_with_agent_tool_paused() {
    let mut projector = AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
        .expect("projector");
    let args = json!({"questions": [{
        "question": "Which environment should I use?",
        "header": "Environment",
        "options": [
            {"label": "Staging", "description": "Use the staging project."},
            {"label": "Production", "description": "Use the production project."}
        ]
    }]});
    let frames = paused_frames(&mut projector, ASK_USER_TOOL_NAME, &args, true);
    assert_paused_call(
        &frames,
        "awaiting_input",
        "clarifying_question",
        ASK_USER_TOOL_NAME,
    );
}

/// The golden captures of a sensitive-tool pause — the paused call and its
/// approval card; `client_frames_tests.rs` records both into
/// `testdata/client-frames/rust.json`, where elitea-main's catalogue test
/// validates `hitl_interrupt` against `ClientFrameHitlInterruptDetail`.
pub(super) fn sensitive_pause_frames() -> (Value, Value) {
    let mut projector = AgentEventProjector::with_sensitive_tools(
        AgentEventProjectionContext::fixture(json!({})),
        sensitive_catalog(),
    )
    .expect("projector");
    let frames = paused_frames(
        &mut projector,
        "delete_branch",
        &json!({"branch": "old"}),
        false,
    );
    (
        only(&frames, "agent_tool_paused").clone(),
        only(&frames, "agent_hitl_interrupt").clone(),
    )
}
