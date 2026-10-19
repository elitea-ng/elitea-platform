use serde_json::{Value, json};

use super::*;
use crate::agents::assembly_tests::ordinary_request;
use crate::agents::request::AgentExecutionKind;
use crate::agents::runtime::{PipelineNativeStart, pipeline_continuation_start};

const VALID: &str =
    include_str!("../../../../../libs/jsonschema/runtime/v1/fixtures/fanout-wake-v1.json");
const ZERO_REVISION: &str = include_str!(
    "../../../../../libs/jsonschema/runtime/v1/fixtures/fanout-wake-v1.invalid.zero-revision.json"
);
const DECISIONS_IN_CONTINUATION: &str = include_str!(
    "../../../../../libs/jsonschema/runtime/v1/fixtures/fanout-wake-v1.invalid.decisions-in-continuation.json"
);

/// The parked execution's frozen input with the wake fields of §8.1.
fn wake_payload(marker: Value) -> AgentExecutionPayload {
    let mut payload = ordinary_request(AgentExecutionKind::Application).payload;
    payload.user_input = UserInput::Text(String::new());
    payload.should_continue = false;
    payload.hitl_resume = false;
    payload.hitl_action = None;
    payload.hitl_value = None;
    payload.hitl_decisions.clear();
    payload.meta.insert(FANOUT_WAKE_META_KEY.to_owned(), marker);
    payload
}

fn fixture(raw: &str) -> Value {
    serde_json::from_str(raw).expect("fixture JSON")
}

#[test]
fn the_contract_fixture_is_admitted_as_a_wake_start() {
    let payload = wake_payload(fixture(VALID));
    let wake = FanoutWake::from_payload(&payload)
        .expect("valid wake")
        .expect("wake marker present");
    assert_eq!(
        wake.response_message_id(),
        "10000000-0000-4000-8000-000000000043"
    );
    assert_eq!(wake.decision_revision(), 2);
    let start = pipeline_continuation_start(&payload).expect("wake start");
    assert!(matches!(&start, PipelineNativeStart::FanoutWake(admitted) if *admitted == wake));
    assert!(start.is_resume(), "a wake is validated as a resume");
    assert!(!start.is_hitl_resume(), "a wake carries no decision");
}

#[test]
fn no_marker_is_no_wake() {
    let mut payload = wake_payload(json!(null));
    payload.meta.remove(FANOUT_WAKE_META_KEY);
    assert_eq!(FanoutWake::from_payload(&payload), Ok(None));
}

#[test]
fn each_invalid_contract_fixture_is_refused_typed() {
    for raw in [ZERO_REVISION, DECISIONS_IN_CONTINUATION] {
        let payload = wake_payload(fixture(raw));
        assert_eq!(
            FanoutWake::from_payload(&payload),
            Err(FanoutWakeError::MalformedMarker),
            "{raw}"
        );
        let Err(error) = pipeline_continuation_start(&payload) else {
            panic!("refused wake: {raw}");
        };
        assert_eq!(
            error
                .cause()
                .map(crate::agents::runtime::NativeAgentAssemblyCause::code),
            Some("graph.fanout.wake_malformed")
        );
    }
}

#[test]
fn a_marker_outside_the_schema_is_refused() {
    let valid = fixture(VALID);
    let mut cases = Vec::new();
    for (key, value) in [
        ("schema", json!("elitea.pipeline.fanout-wake.v2")),
        (
            "response_message_id",
            json!("00000000-0000-0000-0000-000000000000"),
        ),
        (
            "response_message_id",
            json!("10000000-0000-4000-8000-00000000004G"),
        ),
        (
            "response_message_id",
            json!("10000000-0000-4000-8000-0000000000AB"),
        ),
        (
            "response_message_id",
            json!("10000000000040008000000000000043"),
        ),
        ("decision_revision", json!(-1)),
        ("decision_revision", json!(2.5)),
        ("decision_revision", json!("2")),
        ("decision_revision", json!(9_223_372_036_854_775_808_u64)),
    ] {
        let mut marker = valid.clone();
        marker[key] = value;
        cases.push(marker);
    }
    for missing in ["schema", "response_message_id", "decision_revision"] {
        let mut marker = valid.clone();
        marker.as_object_mut().expect("object").remove(missing);
        cases.push(marker);
    }
    let mut extra = valid.clone();
    extra["interrupt_kind"] = json!("hitl");
    cases.push(extra);
    cases.push(json!([valid.clone()]));
    cases.push(json!("elitea.pipeline.fanout-wake.v1"));
    for marker in cases {
        assert_eq!(
            FanoutWake::from_payload(&wake_payload(marker.clone())),
            Err(FanoutWakeError::MalformedMarker),
            "{marker}"
        );
    }
    let mut largest = valid;
    largest["decision_revision"] = json!(i64::MAX);
    assert!(FanoutWake::from_payload(&wake_payload(largest)).is_ok());
}

#[test]
fn a_wake_marker_on_a_non_empty_input_is_refused() {
    type Mutation = fn(&mut AgentExecutionPayload);
    let mutations: [(&str, Mutation); 12] = [
        ("hitl_resume", |payload| payload.hitl_resume = true),
        ("should_continue", |payload| payload.should_continue = true),
        ("hitl_action", |payload| {
            payload.hitl_action = Some("approve".to_owned());
        }),
        ("hitl_value", |payload| {
            payload.hitl_value = Some("yes".to_owned());
        }),
        ("hitl_decisions", |payload| {
            payload
                .hitl_decisions
                .push(json!({"interrupt_id": "hitl:sensitive:0001", "action": "approve"}));
        }),
        ("user_input", |payload| {
            payload.user_input = UserInput::Text("continue".to_owned());
        }),
        ("content_blocks", |payload| {
            payload.user_input = UserInput::ContentBlocks(Vec::new());
        }),
        ("is_regenerate", |payload| payload.is_regenerate = true),
        ("truncated_content", |payload| {
            payload.truncated_content = Some("partial".to_owned());
        }),
        ("checkpoint_id", |payload| {
            payload.checkpoint_id = Some("checkpoint-1".to_owned());
        }),
        ("static_tool_resume", |payload| {
            payload.meta.insert(
                crate::agents::graph::static_tool_pause::STATIC_TOOL_RESUME_META_KEY.to_owned(),
                json!({}),
            );
        }),
        ("static_resume", |payload| {
            payload
                .meta
                .insert("pipeline_static_resume_v1".to_owned(), json!({}));
        }),
    ];
    for (name, mutate) in mutations {
        let mut payload = wake_payload(fixture(VALID));
        mutate(&mut payload);
        assert_eq!(
            FanoutWake::from_payload(&payload),
            Err(FanoutWakeError::NotAWake),
            "{name}"
        );
        let Err(error) = pipeline_continuation_start(&payload) else {
            panic!("refused wake: {name}");
        };
        assert_eq!(
            error
                .cause()
                .map(crate::agents::runtime::NativeAgentAssemblyCause::code),
            Some("graph.fanout.wake_not_empty"),
            "{name}"
        );
    }
}
