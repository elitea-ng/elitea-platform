//! Local consults (ADR-0061 decisions 3e/3f, #3065): a nested-agent call
//! of a local agent's turn, in a thread with other agents, runs the callee
//! here with the folder and the caller's tools; one the desktop may not run
//! goes to the cloud; the limits refuse as tool results; a thread member's
//! consult is recorded in the commit and counts toward the hops.

use super::hand_floor_tests::{
    Thread, UUID, bodies, event_of, hand_to, handed, platform, tool_names,
};
use super::*;

/// Agent 5's nested agents: the thread's Linter (application 7) and a
/// pipeline (application 8) the desktop never runs.
fn nested() -> Value {
    json!([
        {"kind": "application", "type": "application", "agent_type": "agent",
         "name": "Linter", "description": "Lints.", "application_id": 7, "application_version_id": 11},
        {"kind": "application", "type": "application", "agent_type": "agent",
         "name": "Flow", "application_id": 8, "application_version_id": 12}
    ])
}

fn ask(tool: &str, task: &str) -> Res {
    tool_call(&format!("call-{task}"), tool, &json!({"task": task}))
}

fn commit(h: &Harness) -> Value {
    bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn_commit/prompt_lib/1/{EXECUTION}"),
    )
    .remove(0)
}

/// A thread member answers locally: no cloud child turn, the reply comes
/// back framed as another agent's text, the consult is recorded in the
/// commit, and the callee gets no hand_floor and no nested agent.
#[tokio::test]
async fn a_thread_member_answers_a_consult_locally() {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 => ask("Linter", "Lint the patch"),
        1 => say("Lint clean."),
        _ => say("Done."),
    });
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;

    assert!(seen(&h.server, "/local_child_turn/prompt_lib/1/").is_empty());
    let asked = event_of(&h, "consult_started", Some(&started.turn_id)).await;
    assert_eq!(
        asked.payload,
        json!({"caller": 77, "callee": 99, "callee_name": "Linter",
               "question_excerpt": "Lint the patch", "local": true})
    );
    let answered = event_of(&h, "consult_finished", Some(&started.turn_id)).await;
    assert_eq!(answered.payload["ok"], true);
    assert_eq!(answered.payload["reply_excerpt"], "Lint clean.");

    let model = bodies(&h.server, "/llm/v1/chat/completions");
    assert_eq!(model.len(), 3);
    // The callee: the caller's local tools, never hand_floor or a nested
    // agent (depth 1).
    let callee_tools = tool_names(&model[1]);
    assert!(
        callee_tools.contains(&"read_file".to_owned()),
        "{callee_tools:?}"
    );
    assert!(!callee_tools.contains(&"hand_floor".to_owned()));
    assert!(
        !callee_tools
            .iter()
            .any(|tool| tool == "Linter" || tool == "Flow")
    );
    assert!(model[1].to_string().contains("Lint the patch"));
    // The caller read the reply as another agent's text.
    assert!(model[2].to_string().contains("agent_reply"));

    assert_eq!(
        commit(&h)["consults"],
        json!([{"participant_id": 99, "question": "Lint the patch", "reply": "Lint clean."}])
    );
}

/// A nested agent the desktop may not run (a pipeline) runs as a cloud
/// child turn: `subagent_started` says `executor: "cloud"`, the consult is
/// `local: false`, and nothing is recorded in the commit.
#[tokio::test]
async fn a_callee_the_desktop_may_not_run_goes_to_the_cloud() {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 => ask("Flow", "Run the flow"),
        _ => say("Done."),
    });
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;

    let child = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION}"),
    );
    assert_eq!(child.len(), 1);
    assert_eq!(
        (
            child[0]["application_id"].clone(),
            child[0]["version_id"].clone()
        ),
        (json!(8), json!(12))
    );
    let lane = event_of(&h, "subagent_started", Some(&started.turn_id)).await;
    assert_eq!(lane.payload["executor"], "cloud");
    let asked = event_of(&h, "consult_started", Some(&started.turn_id)).await;
    assert_eq!(asked.payload["local"], false);
    assert_eq!(asked.payload["callee"], Value::Null);
    let answered = event_of(&h, "consult_finished", Some(&started.turn_id)).await;
    assert_eq!(answered.payload["local"], false);
    assert_eq!(answered.payload["ok"], true);
    assert!(commit(&h).get("consults").is_none());
}

/// A turn runs at most three consults: the fourth is refused as a tool
/// result, and the commit records three.
#[tokio::test]
async fn the_fourth_consult_of_a_turn_is_refused() {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 | 2 | 4 | 6 => ask("Linter", &format!("Task {n}")),
        1 | 3 | 5 => say("ok"),
        _ => say("Done."),
    });
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;
    let model = bodies(&h.server, "/llm/v1/chat/completions");
    assert_eq!(model.len(), 8);
    assert!(model[7].to_string().contains("consult_limit"));
    assert_eq!(commit(&h)["consults"].as_array().unwrap().len(), 3);
}

/// A consult of a thread member counts toward the message's hops: the
/// next hand-off sends its pair.
#[tokio::test]
async fn a_member_consult_goes_with_the_next_hand_off() {
    let mut thread = Thread::new(
        handed(&[(88, "Reviewer", super::hand_floor_tests::HAND_1)]),
        |n| match n {
            0 => ask("Linter", "Lint it"),
            1 => say("Lint clean."),
            2 => hand_to(&[88]),
            _ => say("Handed."),
        },
    );
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;
    let hand = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn_hand_floor/prompt_lib/1/{EXECUTION}"),
    );
    assert_eq!(hand[0]["consults"], json!([{"from": 77, "to": 99}]));
}

/// Threat model (decision 5f): the person chose Bypass in a trusted folder;
/// the consulted agent's host command is ASKED (the taint gate closes
/// before a consult runs, and Bypass never carries into it).
#[tokio::test]
async fn a_consults_host_command_is_asked_even_under_bypass() {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 => ask("Linter", "List the files"),
        1 => tool_call("call-ls", "run_command", &json!({"command": "ls"})),
        2 => say("Could not list."),
        _ => say("Done."),
    });
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let policy = json!({"allowed": true, "shell": true, "network": true,
        "max_sandbox_mode": "full-access", "folder_trust": true});
    let h = harness(server, Some(policy), UiDecision::Deny).await;
    let store = crate::trust::TrustStore::new(
        h.workspaces.clone(),
        Some(h.app.path().join("config").join("rules.json")),
        vec![h.app.path().to_owned()],
    );
    h.host.set_trust_store(store.clone());
    store
        .grant(
            &h.workspace_id,
            &h.host.local_work_policy(),
            crate::trust::CONFIRMATION_VERSION,
            false,
        )
        .expect("trusted");
    let started = h
        .host
        .start(TurnRequest {
            bypass: true,
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    until_done_of(&h, &started.turn_id).await;
    let asked = event_of(&h, "approval_request", Some(&started.turn_id)).await;
    assert!(
        asked.payload.to_string().contains("bypass mode does not"),
        "{}",
        asked.payload
    );
    let _ = UUID;
}

/// A thread member that runs in the cloud is consulted in the cloud, never
/// with the folder, and nothing is recorded in the commit.
#[tokio::test]
async fn a_cloud_member_is_consulted_in_the_cloud() {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 => ask("Reviewer", "Review it"),
        _ => say("Done."),
    });
    thread.tools = json!([
        {"kind": "application", "type": "application", "agent_type": "agent",
         "name": "Reviewer", "application_id": 6, "application_version_id": 10}
    ]);
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;
    let child = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION}"),
    );
    assert_eq!(child.len(), 1);
    assert_eq!(child[0]["application_id"], 6);
    let asked = event_of(&h, "consult_started", Some(&started.turn_id)).await;
    assert_eq!(asked.payload["local"], false);
    assert_eq!(asked.payload["callee"], 88);
    // Only the caller asked the model: the callee never ran here.
    assert_eq!(bodies(&h.server, "/llm/v1/chat/completions").len(), 2);
    assert!(commit(&h).get("consults").is_none());
}

/// Issue 3130: a consulted thread member works in the caller's folder, so
/// its prompt carries the folder's AGENTS.md after its own instructions
/// when the thread respects it, and none of it when it does not (the
/// caller's neither).
async fn a_consult_follows_respect_agents_md(respect: bool) {
    let mut thread = Thread::new(handed(&[]), |n| match n {
        0 => ask("Linter", "Lint the patch"),
        1 => say("Lint clean."),
        _ => say("Done."),
    });
    thread.tools = nested();
    let server = serve(platform(thread)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    std::fs::write(h.folder.path().join("AGENTS.md"), "Run task test.").unwrap();
    h.host
        .set_respect_agents_md(&h.workspace_id, "42", respect)
        .await
        .unwrap();
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;

    let model = bodies(&h.server, "/llm/v1/chat/completions");
    assert_eq!(model.len(), 3);
    let system = |n: usize| {
        model[n]["messages"][0]["content"]
            .as_str()
            .unwrap_or_default()
            .to_owned()
    };
    let caller = system(0);
    assert_eq!(caller.contains("Run task test."), respect, "{caller}");
    // The callee (step 1).
    let callee = system(1);
    assert_eq!(callee.contains("Run task test."), respect, "{callee}");
    assert_eq!(
        callee.contains(crate::d0::turn::END_OF_PROJECT_INSTRUCTIONS),
        respect,
        "{callee}"
    );
}

#[tokio::test]
async fn a_consulted_member_follows_agents_md_when_the_thread_respects_it() {
    a_consult_follows_respect_agents_md(true).await;
}

#[tokio::test]
async fn a_consulted_member_ignores_agents_md_when_the_thread_does_not() {
    a_consult_follows_respect_agents_md(false).await;
}
