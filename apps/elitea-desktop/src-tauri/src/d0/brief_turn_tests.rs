//! A workspace turn's `spawn_agent` profile and brief (#1827), end to end:
//! the model names the id it was shown, the child gets the soul, the brief
//! and the name, and a brief naming what the agent did not see is refused.

use std::sync::atomic::{AtomicUsize, Ordering};

use serde_json::{Value, json};

use super::*;

/// The `[message m-…]` id the person's message reached the model under.
fn shown_id(body: &Value) -> String {
    let user = body["messages"]
        .as_array()
        .unwrap()
        .iter()
        .find(|m| m["role"] == "user" && m["name"] != crate::d0::model::HOST_CONTEXT_NAME)
        .unwrap();
    let text = user["content"].as_str().unwrap();
    text.strip_prefix("[message ")
        .and_then(|rest| rest.get(..12))
        .unwrap()
        .to_owned()
}

const SOUL: &str = "You are a careful reviewer.";

/// A request of the profiled child: its system prompt opens with the soul.
fn is_profiled_child(body: &Value) -> bool {
    body["messages"][0]["content"]
        .as_str()
        .is_some_and(|system| system.starts_with(SOUL))
}

/// The parent spawns with `context(id)` (the id it read), then waits and
/// answers; the child answers at once.
async fn brief_platform(context: fn(&str) -> Value) -> MockServer {
    let inner = platform(agent_details(), &[]);
    let parent_calls = AtomicUsize::new(0);
    serve(move |req: &Req| {
        if req.path == "/llm/v1/chat/completions" {
            let body: Value = serde_json::from_str(&req.body).unwrap();
            if is_profiled_child(&body) {
                return say("Reviewed.");
            }
            return match parent_calls.fetch_add(1, Ordering::SeqCst) {
                0 => tool_call(
                    "call-spawn",
                    "spawn_agent",
                    &json!({
                        "task": "Review the plan.",
                        "profile": {
                            "display_name": "Risk reviewer",
                            "soul": SOUL,
                            "model": "inherit",
                            "avatar": "default:owl"
                        },
                        "context": context(&shown_id(&body)),
                    }),
                ),
                1 if spawned_ids(&body).is_empty() => say("Refused."),
                1 => tool_call(
                    "call-wait",
                    "wait_agent",
                    &json!({"agent_ids": spawned_ids(&body)}),
                ),
                _ => say("Done."),
            };
        }
        inner(req)
    })
    .await
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_brief_quoting_the_shown_message_reaches_a_profiled_child() {
    let h = harness(
        brief_platform(|id| {
            json!({
                "excerpts": [{"message_id": id, "quote": "file a bug"}],
                "facts": ["The release is on Friday."]
            })
        })
        .await,
        allowed(),
        UiDecision::AllowOnce,
    )
    .await;
    h.host.start(request(&h.workspace_id)).await.unwrap();
    let events = until_done(&h).await;

    let bodies = llm_bodies(&h.server);
    let child = bodies.iter().find(|body| is_profiled_child(body)).unwrap();
    // The soul replaces the role text; the safety sentence stays; the model
    // is the turn's (`inherit`).
    let system = child["messages"][0]["content"].as_str().unwrap();
    assert!(system.starts_with(SOUL), "{system}");
    assert!(
        system.contains(elitea_agent_runtime::subagents::SUBAGENT_SAFETY),
        "{system}"
    );
    assert_eq!(child["model"], bodies[0]["model"]);
    // The checked brief, before the task.
    let task = child["messages"].to_string();
    assert!(task.contains("file a bug"), "{task}");
    assert!(task.contains("The release is on Friday."), "{task}");
    assert!(task.contains("Review the plan."), "{task}");

    // The lane: the display name is the label; the record's digests.
    let started = of_kind(&events, "subagent_started");
    assert_eq!(started.len(), 1, "{events:?}");
    let lane = &started[0];
    assert_eq!(lane["label"], "Risk reviewer");
    assert_eq!(lane["display_name"], "Risk reviewer");
    assert_eq!(lane["avatar"], "default:owl");
    assert_eq!(lane["profile_digest"].as_str().unwrap().len(), 64);
    assert_eq!(lane["brief_digest"].as_str().unwrap().len(), 64);
    // The record names the chat message the model's id stands for, the
    // turn's question, as a cloud spawn's does (#1908).
    let id = shown_id(&bodies[0]);
    let question = h
        .server
        .seen()
        .into_iter()
        .find(|req| req.path.contains("/local_turn/prompt_lib/"))
        .map(|req| serde_json::from_str::<Value>(&req.body).unwrap()["question_id"].clone())
        .unwrap();
    assert_eq!(
        crate::d0::brief::short_message_ref(question.as_str().unwrap()),
        id
    );
    assert_eq!(lane["brief_refs"]["message_ids"], json!([question]));
    assert_eq!(lane["brief_refs"]["facts"], 1);
    // Never the soul, a quote or a fact.
    let text = lane.to_string();
    for secret in ["careful reviewer", "file a bug", "Friday"] {
        assert!(!text.contains(secret), "{text}");
    }
    let finished = of_kind(&events, "subagent_finished");
    assert_eq!(finished[0]["status"], "completed");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_brief_naming_what_the_agent_did_not_see_starts_nothing() {
    for (context, code) in [
        (
            (|id: &str| json!({"excerpts": [{"message_id": id, "quote": "ship it on Monday"}]}))
                as fn(&str) -> Value,
            "BRIEF_EXCERPT_NOT_VISIBLE",
        ),
        (
            |_: &str| json!({"excerpts": [{"message_id": "m-0123456789", "quote": "file a bug"}]}),
            "BRIEF_EXCERPT_NOT_VISIBLE",
        ),
        (
            |_: &str| json!({"attachments": ["att-0011aabb"]}),
            "BRIEF_ATTACHMENT_NOT_READABLE",
        ),
        (
            |_: &str| json!({"memory_entries": ["3"]}),
            "BRIEF_MEMORY_NOT_RECALLED",
        ),
    ] {
        let h = harness(
            brief_platform(context).await,
            allowed(),
            UiDecision::AllowOnce,
        )
        .await;
        h.host.start(request(&h.workspace_id)).await.unwrap();
        let events = until_done(&h).await;
        assert!(of_kind(&events, "subagent_started").is_empty(), "{code}");
        let bodies = llm_bodies(&h.server);
        assert!(!bodies.iter().any(is_profiled_child), "{code}");
        // The refusal is the spawn's result the model reads.
        let second = bodies[1]["messages"].to_string();
        assert!(second.contains(code), "{code}: {second}");
    }
}

/// #1827 review: only the person's message is quotable. A floor turn's note
/// and an unattended continuation's prompt are the host's text: no id.
#[test]
fn the_hosts_own_turn_text_carries_no_message_id() {
    use super::super::turn::quotable_question_id;
    let person = request("w");
    assert_eq!(quotable_question_id(&person, "q-1"), "q-1");
    let floor = TurnRequest {
        participant_id: None,
        floor_turn: Some(super::super::turn::FloorTurn {
            question_id: "q-0".into(),
            position: 1,
            defaults: Default::default(),
        }),
        ..request("w")
    };
    assert_eq!(quotable_question_id(&floor, "q-1"), "");
    let grant: super::super::unattended::UnattendedGrant =
        serde_json::from_value(json!({"continue_step_limit": true})).unwrap();
    let root = super::super::unattended::UnattendedTurn::root(grant.admit().unwrap());
    let first = TurnRequest {
        unattended: Some(root.clone()),
        ..request("w")
    };
    // The chain's first turn is the person's message.
    assert_eq!(quotable_question_id(&first, "q-1"), "q-1");
    let continued = TurnRequest {
        unattended: Some(root.next("exec-1")),
        ..request("w")
    };
    assert_eq!(quotable_question_id(&continued, "q-2"), "");
}
