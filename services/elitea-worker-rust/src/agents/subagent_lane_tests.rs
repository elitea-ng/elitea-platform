use adk_rust::{Content, Event, FunctionResponseData, Part};
use elitea_agent_runtime::subagents::profile::{Brief, Profile, SpawnRecord};
use elitea_agent_runtime::subagents::{ChildIdentity, ChildReport, ChildStatus};
use serde_json::{Value, json};

use super::{MAX_EVENTS_PER_CHILD, SUBAGENT_EVENT, SUBAGENT_METADATA_KEY, SubAgentLane};
use crate::agents::events::{AgentEventProjectionContext, AgentEventProjector};
use crate::protocol::node_event::encode_current_node_event_json;

fn child() -> ChildIdentity {
    ChildIdentity {
        agent_id: "a_0000beef".to_owned(),
        parent_call_id: "call-spawn".to_owned(),
        label: Some("callers".to_owned()),
        ordinal: 1,
        depth: 1,
        parent_agent_id: None,
        record: SpawnRecord::new(
            &Profile {
                display_name: Some("Owl".to_owned()),
                avatar: Some("default:owl".to_owned()),
                ..Profile::default()
            },
            &Brief::default(),
        ),
    }
}

fn model(parts: Vec<Part>) -> Event {
    let mut event = Event::new("child-invocation");
    event.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts,
    });
    event
}

fn tool_call() -> Event {
    model(vec![Part::FunctionCall {
        name: "read_file".to_owned(),
        args: json!({"path": "src/main.rs", "api_key": "sk-secret"}),
        id: Some("call-1".to_owned()),
        thought_signature: None,
    }])
}

fn tool_result() -> Event {
    let mut event = Event::new("child-invocation");
    event.llm_response.content = Some(Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new(
                "read_file",
                json!({"content": "fn main() {}\n".repeat(200)}),
            ),
            id: Some("call-1".to_owned()),
            annotations: None,
        }],
    });
    event
}

fn report(status: ChildStatus) -> ChildReport {
    ChildReport {
        agent_id: "a_0000beef".to_owned(),
        status,
        result: Some("Two callers.".to_owned()),
        steps: 2,
        input_tokens: 100,
        output_tokens: 20,
        error: None,
    }
}

/// The hooks put one bounded entry each on the lane, in order: the start
/// with the profile's name and avatar, the tool call (its secret masked),
/// the result (summarised), the text and the end.
#[test]
fn the_hooks_queue_the_child_lifecycle() {
    let lane = SubAgentLane::default();
    let child = child();
    lane.started(&child);
    lane.event(&child, &tool_call());
    lane.event(&child, &tool_result());
    lane.event(
        &child,
        &model(vec![Part::Text {
            text: "Two callers.".to_owned(),
        }]),
    );
    lane.finished(&child, &report(ChildStatus::Completed));
    let entries = lane.drain();
    let kinds: Vec<&str> = entries
        .iter()
        .map(|e| e["kind"].as_str().unwrap())
        .collect();
    assert_eq!(
        kinds,
        ["started", "tool_start", "tool_end", "text", "finished"]
    );
    assert_eq!(entries[0]["display_name"], "Owl");
    assert_eq!(entries[0]["avatar"], "default:owl");
    assert_eq!(entries[0]["parent_call_id"], "call-spawn");
    assert_eq!(entries[1]["args_summary"], "src/main.rs");
    assert!(!entries[1].to_string().contains("sk-secret"));
    assert_eq!(entries[2]["ok"], true);
    assert!(entries[2]["summary"].as_str().unwrap().chars().count() <= 240);
    assert_eq!(entries[4]["status"], "completed");
    assert_eq!(entries[4]["result"], "Two callers.");
    assert_eq!(lane.drain(), Vec::<Value>::new());
}

/// One child's run puts at most `MAX_EVENTS_PER_CHILD` tool and text
/// entries on the lane; its start and end always go.
#[test]
fn the_lane_is_bounded_per_child() {
    let lane = SubAgentLane::default();
    let child = child();
    lane.started(&child);
    for _ in 0..(MAX_EVENTS_PER_CHILD + 20) {
        lane.event(&child, &tool_call());
    }
    lane.finished(&child, &report(ChildStatus::Completed));
    let mut all = Vec::new();
    loop {
        let batch = lane.drain();
        if batch.is_empty() {
            break;
        }
        all.extend(batch);
    }
    assert_eq!(all.len(), MAX_EVENTS_PER_CHILD + 2);
    assert_eq!(all.last().unwrap()["kind"], "finished");
}

/// A child still running when the run ends is stopped with it, once.
#[test]
fn closing_the_run_ends_each_running_child_once() {
    let lane = SubAgentLane::default();
    let child = child();
    lane.started(&child);
    lane.close_running();
    lane.close_running();
    // An event after its end opens nothing.
    lane.event(&child, &tool_call());
    let entries = lane.drain();
    assert_eq!(entries.len(), 2);
    assert_eq!(entries[1]["status"], "cancelled");
    assert_eq!(entries[1]["error"], "subagent_cancelled");
}

/// The registry finds the ledger (and its lane) the host made for the
/// execution generation, and only that one.
#[test]
fn the_lifecycle_finds_the_hosts_ledger() {
    let registry = crate::protocol::ephemeral_agents::EphemeralAgentRegistry::default();
    assert!(registry.existing("e", 1).is_none());
    let ledger = registry.ledger("e", 1);
    let found = registry.existing("e", 1).expect("the host's ledger");
    assert!(std::sync::Arc::ptr_eq(&ledger, &found));
    ledger.lane().started(&child());
    ledger
        .lane()
        .finished(&child(), &report(ChildStatus::Failed));
    let entries = found.lane().drain();
    assert_eq!(entries.len(), 2);
    assert_eq!(entries[1]["status"], "failed");
    assert!(registry.existing("e", 2).is_none());
}

/// Each entry is one `agent_subagent` node event the browser codec accepts.
#[test]
fn entries_project_as_agent_subagent_frames() {
    let lane = SubAgentLane::default();
    let child = child();
    lane.started(&child);
    lane.event(&child, &tool_call());
    let projector = AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
        .expect("projector");
    let batch = projector
        .project_subagent_entries(lane.drain(), chrono::Utc::now())
        .expect("projected");
    let frames: Vec<Value> = batch
        .into_iter()
        .map(|event| {
            serde_json::from_slice(&encode_current_node_event_json(&event).expect("valid"))
                .expect("json")
        })
        .collect();
    assert_eq!(frames.len(), 2);
    assert!(frames.iter().all(|frame| frame["type"] == SUBAGENT_EVENT));
    assert_eq!(
        frames[0]["response_metadata"][SUBAGENT_METADATA_KEY]["agent_id"],
        "a_0000beef"
    );
    assert_eq!(
        frames[1]["response_metadata"][SUBAGENT_METADATA_KEY]["tool"],
        "read_file"
    );
}

/// A wake is never lost: a push before the wait leaves a permit.
#[tokio::test]
async fn a_push_wakes_the_waiting_loop() {
    let lane = SubAgentLane::default();
    lane.started(&child());
    tokio::time::timeout(std::time::Duration::from_secs(1), lane.notified())
        .await
        .expect("woken");
}
