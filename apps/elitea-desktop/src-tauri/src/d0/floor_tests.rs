//! Several agents in one desktop thread (ADR-0050 decision 7f, #1806): the
//! host follows the floor Main routed, one agent at a time, and relays a
//! cloud agent's turn as text only.

use super::*;

const UUID: &str = "99999999-2222-4333-8444-555555555555";
const FIRST_QUESTION: &str = "unset";

/// The thread: the person (70), a local agent (77: application 5 on
/// version 9) and a cloud agent (88: application 6 on version 10).
fn two_agent_detail() -> Value {
    json!({"id": 42, "message_count": 0, "uuid": UUID, "participants": [
        {"id": 70, "entity_name": "user", "entity_meta": {"id": 1}},
        {"id": 77, "entity_name": "application", "entity_meta": {"id": 5, "project_id": 1, "name": "Coder"},
         "entity_settings": {"version_id": 9}},
        {"id": 88, "entity_name": "application", "entity_meta": {"id": 6, "project_id": 1, "name": "Reviewer"},
         "entity_settings": {"version_id": 10, "executor": "cloud"}}
    ]})
}

fn floor_details() -> Value {
    json!({
        "instructions": "Be brief.",
        "agent_type": "openai",
        "llm_settings": {"model_name": "gpt-test", "max_tokens": 256},
        "tools": []
    })
}

fn resolved_for(application_id: i64, version_id: i64) -> Value {
    json!({
        "schema_version": "elitea.client.resolved-application-version.v1",
        "project_id": 1, "application_id": application_id, "version_id": version_id,
        "definition_sha256": "00", "withheld_secrets": [],
        "project_context_withheld": false,
        "version_details": floor_details(),
    })
}

fn agents() -> Value {
    json!([
        {"participant_id": 77, "name": "Coder", "executor": "local"},
        {"participant_id": 88, "name": "Reviewer", "executor": "cloud"}
    ])
}

/// What the later agent's start answers.
#[derive(Clone, Copy, PartialEq)]
enum Later {
    Started,
    Superseded,
}

fn floor_platform(later: Later) -> impl Fn(&Req) -> Res {
    let first_question = Mutex::new(FIRST_QUESTION.to_owned());
    move |req: &Req| {
        let path = req.path.as_str();
        if path == "/api/v2/social/author" {
            return Res::json(200, &json!({"id": 1, "name": "Me"}));
        }
        if path.starts_with("/api/v2/configurations/models/1?") {
            return Res::json(200, &catalogue(200_000));
        }
        if path == "/api/v2/elitea_core/resolved_version/prompt_lib/1/5/9" {
            return Res::json(200, &resolved_for(5, 9));
        }
        if path == "/api/v2/elitea_core/resolved_version/prompt_lib/1/6/10" {
            return Res::json(200, &resolved_for(6, 10));
        }
        if path.starts_with(&format!(
            "/api/v2/elitea_core/conversation/prompt_lib/1/{UUID}?messages_limit="
        )) {
            let mut detail = two_agent_detail();
            detail["message_groups"] = json!([
                {"id": 1, "author_participant_id": 70, "message_items": [{"item_type": "text_message", "item_details": {"content": "Review the change"}}]},
                {"id": 2, "author_participant_id": 77, "reply_to_id": 1, "message_items": [{"item_type": "text_message", "item_details": {"content": "Local answer."}}]}
            ]);
            return Res::json(200, &detail);
        }
        if path == "/api/v2/elitea_core/conversation/prompt_lib/1/42"
            || path == format!("/api/v2/elitea_core/conversation/prompt_lib/1/{UUID}")
        {
            return Res::json(200, &two_agent_detail());
        }
        if path == format!("/api/v2/elitea_core/local_turn/prompt_lib/1/{UUID}") {
            let body: Value = serde_json::from_str(&req.body).unwrap_or_default();
            let question = body["question_id"].as_str().unwrap_or_default().to_owned();
            if body["participant_id"] == 77 {
                first_question.lock().unwrap().clone_from(&question);
                // Main routes by the address: the composer's pick alone is one target.
                let targets = body
                    .get("addressed_to")
                    .cloned()
                    .unwrap_or_else(|| json!([77, 88]));
                return Res::json(
                    200,
                    &json!({
                        "execution_id": EXECUTION, "question_id": question,
                        "response_message_id": "11111111-2222-4333-8444-555555555555",
                        "conversation_uuid": UUID, "participant_id": 77, "created": true,
                        "expires_at": "2026-10-10T00:00:00Z",
                        "memory_recall": {"text": ""}, "executor": "local",
                        "floor": {"question_id": question, "position": 0, "targets": targets, "agents": agents()}
                    }),
                );
            }
            if later == Later::Superseded {
                return Res::json(
                    409,
                    &json!({"error": "floor_turn_superseded",
                    "message": "A newer message superseded this one."}),
                );
            }
            let first = first_question.lock().unwrap().clone();
            return Res::json(
                200,
                &json!({
                    "execution_id": EXECUTION_2, "question_id": question,
                    "response_message_id": "22222222-2222-4333-8444-555555555555",
                    "conversation_uuid": UUID, "participant_id": 88, "created": true,
                    "expires_at": "2026-10-10T00:00:00Z",
                    "memory_recall": {"text": ""}, "executor": "cloud",
                    "floor": {"question_id": first, "position": 1, "targets": [77, 88], "agents": agents()}
                }),
            );
        }
        if path == format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION_2}") {
            return Res::json(
                202,
                &json!({"child_execution_id": "child-1",
                "response_message_id": "33333333-2222-4333-8444-555555555555",
                "conversation_uuid": "44444444-2222-4333-8444-555555555555"}),
            );
        }
        if path
            == format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION_2}/child-1")
        {
            return Res::json(
                200,
                &json!({"status": "completed", "output": "Cloud answer."}),
            );
        }
        if path == "/llm/v1/chat/completions" {
            return say("Local answer.");
        }
        for execution in [EXECUTION, EXECUTION_2] {
            if path == format!("/api/v2/elitea_core/local_turn_commit/prompt_lib/1/{execution}") {
                return Res::json(
                    200,
                    &json!({"execution_id": execution, "created": true,
                    "conversation_uuid": UUID, "question_message_id": "q", "response_message_id": "r",
                    "memories_used": 0, "committed_at": "2026-10-09T00:00:00Z"}),
                );
            }
        }
        Res::json(404, &json!({"error": "not_found", "message": path}))
    }
}

fn addressed(h: &Harness) -> TurnRequest {
    TurnRequest {
        prompt: "Review the change".into(),
        addressed_to: vec![77, 88],
        ..request(&h.workspace_id)
    }
}

async fn first_event(h: &Harness, kind: &str) -> AgentEvent {
    for _ in 0..SETTLE_POLLS {
        if let Some(event) = h.emitter.all().into_iter().find(|event| event.kind == kind) {
            return event;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("no {kind}: {:?}", h.emitter.kinds());
}

fn bodies(server: &MockServer, path: &str) -> Vec<Value> {
    server
        .seen()
        .into_iter()
        .filter(|req| req.path == path)
        .map(|req| serde_json::from_str(&req.body).unwrap_or_default())
        .collect()
}

/// A message addressed to a local and a cloud agent: the local agent answers
/// first with the workspace; after its commit the host starts the cloud
/// agent's turn itself, which relays only the conversation's text to the
/// cloud and commits the answer.
#[tokio::test]
async fn a_local_then_a_cloud_agent_answer_one_message_in_turn() {
    let server = serve(floor_platform(Later::Started)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    std::fs::write(h.folder.path().join("secret.txt"), "WORKSPACE-ONLY").unwrap();
    let started = h.host.start(addressed(&h)).await.unwrap();
    assert_eq!(started.execution_id, EXECUTION);
    until_done_of(&h, &started.turn_id).await;

    // The first turn says the message goes on, and to whom.
    let done = settled_done(&h, &started.turn_id);
    assert_eq!(done["continuing"], true);
    assert_eq!(done["floor_next"]["participant_id"], 88);
    assert_eq!(done["floor_next"]["agent_name"], "Reviewer");

    let continued = first_event(&h, "turn_continued").await;
    assert_eq!(continued.turn_id, started.turn_id);
    assert_eq!(continued.payload["kind"], "floor");
    assert_eq!(continued.payload["executor"], "cloud");
    assert_eq!(continued.payload["execution_id"], EXECUTION_2);
    let next = continued.payload["next_turn_id"]
        .as_str()
        .unwrap()
        .to_owned();
    until_done_of(&h, &next).await;
    assert_eq!(settled_done(&h, &next)["commit"], "committed");

    // The starts: the person's address, then the later turn of the message.
    let starts = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn/prompt_lib/1/{UUID}"),
    );
    assert_eq!(starts.len(), 2, "{starts:?}");
    assert_eq!(starts[0]["addressed_to"], json!([77, 88]));
    assert_eq!(starts[0]["participant_id"], 77);
    assert!(starts[0].get("floor_turn").is_none());
    assert_eq!(starts[1]["participant_id"], 88);
    assert_eq!(starts[1]["user_input"], super::super::floor::FLOOR_NOTE);
    assert_eq!(
        starts[1]["floor_turn"]["question_id"],
        starts[0]["question_id"]
    );
    assert_eq!(starts[1]["floor_turn"]["position"], 1);
    assert!(starts[1].get("addressed_to").is_none());

    // Only the local agent called the model here.
    assert_eq!(seen(&h.server, "/llm/v1/chat/completions").len(), 1);

    // The cloud agent ran its own version, given the conversation's text
    // only: never the workspace's files.
    let child = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION_2}"),
    );
    assert_eq!(child.len(), 1);
    assert_eq!(
        (
            child[0]["application_id"].clone(),
            child[0]["version_id"].clone()
        ),
        (json!(6), json!(10))
    );
    let input = child[0]["input"].as_str().unwrap();
    assert!(input.contains("[Person]: Review the change"), "{input}");
    assert!(input.contains("[Coder]: Local answer."), "{input}");
    assert!(!input.contains("WORKSPACE-ONLY"));

    // Its commit carries the cloud's answer and no workspace work.
    let commit = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn_commit/prompt_lib/1/{EXECUTION_2}"),
    );
    assert_eq!(commit[0]["assistant_message"]["content"], "Cloud answer.");
    assert_eq!(
        commit[0]["user_message"]["content"],
        super::super::floor::FLOOR_NOTE
    );
    assert_eq!(commit[0]["local_work"]["paths_touched"], json!([]));
}

/// A newer message took the floor: the older message's remaining agent does
/// not answer, and the first turn says so.
#[tokio::test]
async fn a_superseded_message_stops_its_remaining_agents() {
    let server = serve(floor_platform(Later::Superseded)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(addressed(&h)).await.unwrap();
    until_done_of(&h, &started.turn_id).await;
    let stopped = first_event(&h, "floor_stopped").await;
    assert_eq!(stopped.turn_id, started.turn_id);
    assert_eq!(stopped.payload["code"], "floor_turn_superseded");
    assert!(
        h.emitter
            .all()
            .iter()
            .all(|event| event.kind != "turn_continued")
    );
    assert!(
        seen(
            &h.server,
            &format!("/local_child_turn/prompt_lib/1/{EXECUTION_2}")
        )
        .is_empty()
    );
}

/// A cloud agent's turn takes only text: the host refuses what only a
/// workspace turn uses before any request.
#[tokio::test]
async fn a_cloud_agents_turn_refuses_workspace_inputs() {
    let server = serve(floor_platform(Later::Started)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let refused = h
        .host
        .start(TurnRequest {
            application_id: 6,
            version_id: 10,
            cloud: true,
            mentions: vec!["secret.txt".into()],
            ..request(&h.workspace_id)
        })
        .await
        .unwrap_err();
    assert_eq!(refused.code, "cloud_agent_text_only");
    assert!(seen(&h.server, &format!("/local_turn/prompt_lib/1/{UUID}")).is_empty());
}

/// A message that names no agent, in a thread of several: the composer's
/// pick is sent as the address (ADR-0050 decision 7c), so a lead
/// conversation cannot route it past the agent the person chose. Only that
/// agent answers.
#[tokio::test]
async fn the_composers_pick_is_the_address_when_the_message_names_no_one() {
    let server = serve(floor_platform(Later::Started)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h
        .host
        .start(TurnRequest {
            prompt: "Fix the flaky test".into(),
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    until_done_of(&h, &started.turn_id).await;
    let starts = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn/prompt_lib/1/{UUID}"),
    );
    assert_eq!(starts.len(), 1, "{starts:?}");
    assert_eq!(starts[0]["participant_id"], 77);
    assert_eq!(starts[0]["addressed_to"], json!([77]));
    let done = settled_done(&h, &started.turn_id);
    assert!(done.get("floor_next").is_none(), "{done}");
    assert!(
        h.emitter
            .all()
            .iter()
            .all(|event| event.kind != "turn_continued" && event.kind != "error")
    );
}
