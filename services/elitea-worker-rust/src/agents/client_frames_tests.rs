//! The client frame catalogue's golden frames, captured from THIS worker.
//!
//! Client contract 1.1 documents the `execution.node_event` frames a native
//! client renders tool activity and pauses from (`x-elitea-client-frames` in
//! services/elitea-main/api/openapi/v2.yaml). This test projects each of them
//! through the real `AgentEventProjector` and compares the result with
//! testdata/client-frames/rust.json; elitea-main's
//! `TestClientFrameCatalogueAcceptsWorkerFrames` validates that file against
//! the catalogue. A change to what this worker emits fails HERE until the file
//! is regenerated, and the regenerated file fails THERE if it no longer
//! satisfies the contract.
//!
//! Regenerate with `ELITEA_UPDATE_CLIENT_FRAMES=1`.

use std::collections::BTreeMap;

use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::{Content, Event, FinishReason, Part, ToolConfirmationDecision};
use chrono::{TimeZone, Utc};
use serde_json::{Value, json};

use super::events::{AgentEventProjectionContext, AgentEventProjector};
use crate::protocol::elitea::runtime::v1::NodeEventV1;
use crate::protocol::node_event::encode_current_node_event_json;

const GOLDEN: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../testdata/client-frames/rust.json"
);

fn timestamp(second: u32) -> chrono::DateTime<Utc> {
    Utc.with_ymd_and_hms(2026, 8, 14, 12, 0, second)
        .single()
        .expect("fixture timestamp")
}

fn current(event: &NodeEventV1) -> Value {
    serde_json::from_slice(
        &encode_current_node_event_json(event).expect("valid projected browser event"),
    )
    .expect("projected JSON")
}

fn model_event(id: &str, second: u32, parts: Vec<Part>) -> Event {
    let mut event = Event::with_id(id, "invocation-1");
    event.timestamp = timestamp(second);
    event.author = "root-agent".to_owned();
    event.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts,
    });
    event.llm_response.turn_complete = true;
    event.llm_response.finish_reason = Some(FinishReason::Stop);
    event
}

fn graph_interrupt_event(id: &str, checkpoint_id: &str, data: Value) -> Event {
    let message = data
        .get("message")
        .and_then(Value::as_str)
        .expect("interrupt fixture message")
        .to_owned();
    let payload = GraphInterruptPayload {
        kind: "dynamic".to_owned(),
        node: None,
        message: Some(message.clone()),
        data: Some(data),
        thread_id: "thread-1".to_owned(),
        checkpoint_id: checkpoint_id.to_owned(),
    };
    let mut event = Event::with_id(id, "invocation-1");
    event.timestamp = timestamp(1);
    event.author = "root-agent".to_owned();
    event.llm_response.content = Some(Content {
        role: "assistant".to_owned(),
        parts: vec![Part::Text {
            text: format!("Dynamic interrupt: {message}"),
        }],
    });
    event.provider_metadata.insert(
        INTERRUPT_METADATA_KEY.to_owned(),
        payload.to_metadata_value(),
    );
    event
}

fn first_of(frames: &[Value], frame_type: &str) -> Value {
    frames
        .iter()
        .find(|frame| frame["type"] == frame_type)
        .unwrap_or_else(|| panic!("no {frame_type} frame in {frames:?}"))
        .clone()
}

// One linear script of four fixtures reads better than four helpers that each
// rebuild a projector.
#[allow(clippy::too_many_lines)]
fn capture() -> BTreeMap<&'static str, Value> {
    let mut captured = BTreeMap::new();

    // A tool call, start to end.
    let mut projector = AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
        .expect("projector");
    projector.start(timestamp(0)).expect("start");
    let start: Vec<Value> = projector
        .project(&model_event(
            "llm-tool",
            1,
            vec![Part::FunctionCall {
                name: "lookup_issue".to_owned(),
                args: json!({"issue_number": 42}),
                id: Some("call-1".to_owned()),
                thought_signature: None,
            }],
        ))
        .expect("tool start")
        .into_iter()
        .map(|event| current(&event))
        .collect();
    captured.insert("tool_start", first_of(&start, "agent_tool_start"));
    let mut result = Event::with_id("tool-result", "invocation-1");
    result.timestamp = timestamp(2);
    result.author = "root-agent".to_owned();
    result.llm_response.content = Some(Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: adk_rust::FunctionResponseData::new(
                "lookup_issue",
                json!({"title": "Bounded result"}),
            ),
            id: Some("call-1".to_owned()),
            annotations: None,
        }],
    });
    result.actions.tool_confirmation_decision = Some(ToolConfirmationDecision::Approve);
    let finish: Vec<Value> = projector
        .project(&result)
        .expect("tool result")
        .into_iter()
        .map(|event| current(&event))
        .collect();
    captured.insert("tool_end", first_of(&finish, "agent_tool_end"));

    // A sensitive tool call that paused for approval (agent-zefir#21).
    let (paused, card) = super::pause_frames_tests::sensitive_pause_frames();
    captured.insert("tool_paused", paused);
    captured.insert("sensitive_hitl_interrupt", card);

    // The answer stopped at the model's output limit.
    let mut projector = AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
        .expect("projector");
    projector.start(timestamp(0)).expect("start");
    let mut limited = model_event(
        "llm-limited",
        1,
        vec![Part::Text {
            text: "partial answer".to_owned(),
        }],
    );
    limited.llm_response.finish_reason = Some(FinishReason::MaxTokens);
    let projected: Vec<Value> = projector
        .project(&limited)
        .expect("max-token completion")
        .into_iter()
        .map(|event| current(&event))
        .collect();
    captured.insert(
        "requires_confirmation",
        first_of(&projected, "agent_requires_confirmation"),
    );

    // A pipeline HITL pause.
    let mut projector =
        AgentEventProjector::new(AgentEventProjectionContext::pipeline_fixture(json!({})))
            .expect("projector");
    projector.start(timestamp(0)).expect("start");
    let projected: Vec<Value> = projector
        .project(&graph_interrupt_event(
            "graph-interrupt",
            "checkpoint-7",
            json!({
                "schema_revision": "elitea.graph.hitl-interrupt.v1",
                "type": "hitl",
                "interaction_type": "pipeline_hitl_node",
                "history_contract_version": 1,
                "guardrail_type": "pipeline_hitl",
                "node_name": "review",
                "message": "Review the generated answer.",
                "available_actions": ["approve", "reject", "edit"],
                "routes": {"approve": "publish", "reject": "END", "edit": "revise"},
                "edit_state_key": "answer",
                "definition_digest": format!("sha256:{}", "1".repeat(64)),
            }),
        ))
        .expect("pipeline HITL projection")
        .into_iter()
        .map(|event| current(&event))
        .collect();
    captured.insert(
        "hitl_interrupt",
        first_of(&projected, "agent_hitl_interrupt"),
    );

    // A pipeline toolkit that needs sign-in.
    let mut projector =
        AgentEventProjector::new(AgentEventProjectionContext::pipeline_fixture(json!({})))
            .expect("projector");
    projector.start(timestamp(0)).expect("start");
    let projected: Vec<Value> = projector
        .project(&graph_interrupt_event(
            "graph-mcp-interrupt",
            "checkpoint-mcp-1",
            json!({
                "schema_revision": "elitea.graph.mcp-authorization.v1",
                "type": "hitl",
                "guardrail_type": "mcp_auth",
                "node_name": "lookup",
                "message": "Authorization is required to use the Customer Support MCP toolkit. Choose Authorize to sign in, or Skip to stop this pipeline safely.",
                "available_actions": ["authorize", "skip"],
                "routes": {},
                "definition_digest": format!("sha256:{}", "1".repeat(64)),
                "tool_call_id": "pipeline:lookup:4",
                "tool_name": "search_records",
                "toolkit_name": "Customer Support",
                "toolkit_type": "mcp",
                "tool_args": {},
                "argument_digest": format!("sha256:{}", "2".repeat(64)),
                "server_url": "https://mcp.example.invalid/v1/mcp",
                "resource_metadata_url": "https://mcp.example.invalid/.well-known/oauth-protected-resource",
                "www_authenticate": "Bearer resource_metadata=\"https://mcp.example.invalid/.well-known/oauth-protected-resource\"",
                "resource_metadata": {
                    "authorization_servers": ["https://login.example.invalid"],
                    "oauth_authorization_server": {
                        "issuer": "https://login.example.invalid",
                        "authorization_endpoint": "https://login.example.invalid/authorize",
                        "token_endpoint": "https://login.example.invalid/token",
                        "registration_endpoint": "https://login.example.invalid/register",
                        "grant_types_supported": ["authorization_code", "refresh_token"],
                        "code_challenge_methods_supported": ["S256"]
                    },
                    "scopes_supported": ["mcp:read"]
                },
            }),
        ))
        .expect("MCP authorization projection")
        .into_iter()
        .map(|event| current(&event))
        .collect();
    captured.insert(
        "mcp_authorization",
        first_of(&projected, "mcp_authorization_required"),
    );
    captured
}

#[test]
fn client_frames_match_the_golden_file() {
    let captured = serde_json::to_value(capture()).expect("captured frames");
    if std::env::var("ELITEA_UPDATE_CLIENT_FRAMES").as_deref() == Ok("1") {
        let mut rendered = serde_json::to_string_pretty(&captured).expect("render");
        rendered.push('\n');
        std::fs::write(GOLDEN, rendered).expect("write the golden frames");
    }
    let golden: Value =
        serde_json::from_str(&std::fs::read_to_string(GOLDEN).unwrap_or_else(|error| {
            panic!("{GOLDEN}: {error}; regenerate with ELITEA_UPDATE_CLIENT_FRAMES=1")
        }))
        .expect("golden frames are JSON");
    assert_eq!(
        golden, captured,
        "a client-rendered frame changed; regenerate testdata/client-frames/rust.json with \
         ELITEA_UPDATE_CLIENT_FRAMES=1 and make sure elitea-main's \
         TestClientFrameCatalogueAcceptsWorkerFrames still passes"
    );
}
