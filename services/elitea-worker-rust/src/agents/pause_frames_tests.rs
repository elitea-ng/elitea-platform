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

/// The pending interrupt travels TWICE in one frame (`hitl_interrupt` and
/// `hitl_interrupts[0]`, which elitea-main requires to be equal), and an
/// `ask_user` pending object carries its questions twice more (`questions`
/// and `tool_args`). A request at `AskUserRequest`'s own 16 KiB bound used to
/// fit the 60 KiB frame with the object sent once; sent twice it did not, the
/// interrupt failed to encode and the turn failed instead of asking.
#[test]
fn a_clarifying_question_at_the_request_bound_still_pauses() {
    let question = |index: usize, description: &str| {
        json!({
            "question": format!("Question {index}: {}", "q".repeat(400)),
            "header": format!("Header {index}"),
            "options": (0..8).map(|option| json!({
                "label": format!("Option {index}.{option}"),
                "description": description,
            })).collect::<Vec<_>>(),
        })
    };
    // The largest uniform description AskUserRequest still admits.
    let args = (1..=1_000)
        .rev()
        .map(|length| {
            let description = "d".repeat(length);
            json!({"questions": (0..4).map(|index| question(index, &description)).collect::<Vec<_>>()})
        })
        .find(|args| AskUserRequest::from_arguments(args).is_ok())
        .expect("an admitted ask_user request");
    let request = AskUserRequest::from_arguments(&args).expect("admitted");
    assert!(
        encode_ask_user_request(&request).expect("encoded").len() > 15 * 1_024,
        "the fixture must sit at the request bound"
    );

    let mut projector = AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
        .expect("projector");
    let frames = paused_frames(&mut projector, ASK_USER_TOOL_NAME, &args, true);
    assert_paused_call(
        &frames,
        "awaiting_input",
        "clarifying_question",
        ASK_USER_TOOL_NAME,
    );
    let card = &only(&frames, "agent_hitl_interrupt")["response_metadata"];
    // The questions are the card's controls: they must arrive whole.
    assert_eq!(
        card["hitl_interrupt"]["questions"],
        request.questions_value()
    );
}

/// A sensitive call's masked arguments were admitted up to
/// `MAX_TOOL_EVENT_VALUE_BYTES` (40 KiB) and paused for approval with them
/// sent once; sent twice they overflow the frame. The card keeps every
/// identity field and shows the arguments bounded, flagged as truncated —
/// the full arguments already reached the client on `agent_tool_start`.
#[test]
fn a_sensitive_tool_pause_with_large_arguments_still_pauses() {
    let mut projector = AgentEventProjector::with_sensitive_tools(
        AgentEventProjectionContext::fixture(json!({})),
        sensitive_catalog(),
    )
    .expect("projector");
    let args = json!({"branch": "b".repeat(36 * 1_024)});
    let frames = paused_frames(&mut projector, "delete_branch", &args, false);
    assert_paused_call(
        &frames,
        "awaiting_approval",
        "sensitive_tool",
        "delete_branch",
    );
    let detail = &only(&frames, "agent_hitl_interrupt")["response_metadata"]["hitl_interrupt"];
    assert_eq!(detail["tool_args_truncated"], true, "{detail}");
    let shown = detail["tool_args"]["branch"]
        .as_str()
        .expect("bounded branch");
    assert!(shown.len() < 8 * 1_024, "{} bytes shown", shown.len());
    assert!(shown.starts_with("bbbb"));
    // The paused call itself still carries the real arguments.
    assert_eq!(
        only(&frames, "agent_tool_paused")["response_metadata"]["tool_inputs"],
        args
    );
}

/// A small card is sent exactly as built: no truncation flag.
#[test]
fn a_small_sensitive_card_is_not_marked_truncated() {
    let (_, card) = sensitive_pause_frames();
    let detail = &card["response_metadata"]["hitl_interrupt"];
    assert!(detail.get("tool_args_truncated").is_none(), "{detail}");
    assert_eq!(detail["tool_args"], json!({"branch": "old"}));
}

/// FU-246: a fixed Parallel member's own pause card, built where the
/// confirmation is born, reads back as the exact card the fan-out runner
/// raises: kind, identity, actions, display and the pending-payload digest.
#[test]
fn a_member_pause_card_round_trips_for_a_sensitive_call_and_an_ask() {
    use super::events::{fanout_member_card, fanout_member_card_parts};
    use super::graph::fanout_frames::CardKind;
    let catalog = sensitive_catalog();
    let args = json!({"branch": "release/1.2"});
    let card = fanout_member_card(&confirmation_event("delete_branch", &args), &catalog)
        .expect("a sensitive-tool card");
    assert_eq!(
        card,
        fanout_member_card(&confirmation_event("delete_branch", &args), &catalog)
            .expect("the same card"),
        "byte-stable for the same confirmation"
    );
    let (kind, interrupt_id, tool_call_id, parts) =
        fanout_member_card_parts(&card).expect("the card reads back");
    assert_eq!(kind, CardKind::ToolGuard);
    assert_ne!(interrupt_id, "");
    assert_eq!(tool_call_id, "call-1");
    assert_eq!(
        parts.available_actions,
        ["approve", "reject", "block_with_comment"]
    );
    assert_eq!(parts.display.tool_name.as_deref(), Some("delete_branch"));
    assert_eq!(parts.display.guardrail_type, "sensitive_tool");

    let ask_args = json!({"questions": [{"question": "Which environment?", "header": "Env",
        "options": [{"label": "Staging"}, {"label": "Production"}]}]});
    let mut ask = confirmation_event(ASK_USER_TOOL_NAME, &ask_args);
    let request = AskUserRequest::from_arguments(&ask_args).expect("ask_user arguments");
    ask.provider_metadata.insert(
        ASK_USER_METADATA_KEY.to_owned(),
        encode_ask_user_request(&request).expect("encoded ask_user request"),
    );
    let card = fanout_member_card(&ask, &catalog).expect("an ask card");
    let (kind, _, _, parts) = fanout_member_card_parts(&card).expect("the ask card reads back");
    assert_eq!(kind, CardKind::AskUser);
    assert_eq!(parts.available_actions, ["answer"]);
    assert_eq!(parts.display.guardrail_type, "clarifying_question");
}

/// A pause no member card can express is refused, and a descriptor that is
/// not exactly a member card never reads back as one.
#[test]
fn a_member_pause_card_refuses_what_it_cannot_express() {
    use super::events::{fanout_member_card, fanout_member_card_parts};
    let catalog = sensitive_catalog();
    let args = json!({"branch": "release/1.2"});
    // A tool outside the child's sensitive catalog, and a delegated sign-in.
    assert!(fanout_member_card(&confirmation_event("read_branch", &args), &catalog).is_err());
    let mut sign_in = confirmation_event("delete_branch", &args);
    sign_in.provider_metadata.insert(
        crate::toolkits::DELEGATED_AUTHORIZATION_METADATA_KEY.to_owned(),
        "{}".to_owned(),
    );
    assert!(fanout_member_card(&sign_in, &catalog).is_err());
    let card = fanout_member_card(&confirmation_event("delete_branch", &args), &catalog)
        .expect("a sensitive-tool card");
    for tampered in [
        {
            let mut card = card.clone();
            card["extra"] = json!(true);
            card
        },
        {
            let mut card = card.clone();
            card["display"]["guardrail_type"] = json!("clarifying_question");
            card
        },
        {
            let mut card = card.clone();
            card["payload_sha256"] = json!("not-a-digest");
            card
        },
        {
            let mut card = card.clone();
            card["available_actions"] = json!(["launch"]);
            card
        },
        {
            let mut card = card.clone();
            card["kind"] = json!("delegated_auth");
            card
        },
    ] {
        assert!(fanout_member_card_parts(&tampered).is_none(), "{tampered}");
    }
}

/// The approval ledger's argument digest (ADR-0041 decision 5) covers the
/// arguments BEFORE masking: elitea-main cannot digest the masked (or cut)
/// `tool_args` the card shows, so the worker sends `argument_digest` with the
/// root pause. The digest is `sha256:` over the canonical JSON (keys sorted,
/// compact) of the real arguments.
#[test]
fn a_sensitive_tool_pause_digests_the_unmasked_arguments() {
    use ring::digest;
    let mut projector = AgentEventProjector::with_sensitive_tools(
        AgentEventProjectionContext::fixture(json!({})),
        sensitive_catalog(),
    )
    .expect("projector");
    let args = json!({"token": "s3cr3t-value", "branch": "old"});
    let frames = paused_frames(&mut projector, "delete_branch", &args, false);
    let detail = &only(&frames, "agent_hitl_interrupt")["response_metadata"]["hitl_interrupt"];
    assert_eq!(detail["tool_args"]["token"], "***", "{detail}");
    let canonical = br#"{"branch":"old","token":"s3cr3t-value"}"#;
    let hex = |bytes: &[u8]| {
        bytes
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>()
    };
    let unmasked = format!(
        "sha256:{}",
        hex(digest::digest(&digest::SHA256, canonical).as_ref())
    );
    let masked = format!(
        "sha256:{}",
        hex(digest::digest(&digest::SHA256, br#"{"branch":"old","token":"***"}"#).as_ref())
    );
    assert_eq!(detail["argument_digest"], unmasked, "{detail}");
    assert_ne!(detail["argument_digest"], masked);
    assert!(!detail.to_string().contains("s3cr3t-value"), "{detail}");
}

#[test]
fn a_clarifying_question_pause_carries_its_argument_digest() {
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
    let detail = &only(&frames, "agent_hitl_interrupt")["response_metadata"]["hitl_interrupt"];
    assert!(
        detail["argument_digest"]
            .as_str()
            .is_some_and(|value| value.len() == 71 && value.starts_with("sha256:")),
        "{detail}"
    );
}
