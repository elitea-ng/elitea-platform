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

/// How the mock Main routes a person's message (`turntaking.Route`).
#[derive(Clone, Copy, PartialEq)]
enum Routing {
    /// Policy `addressed`: the addressees (the composer's agent first when
    /// it is one of them), else the composer's agent.
    Addressed,
    /// Policy `lead`, with the lead's participant id: the lead answers an
    /// unaddressed message; an address still wins.
    Lead(i64),
    /// A broken Main that routes every start to the other agent: the host
    /// must never run an agent Main did not answer on.
    Flip,
}

/// Main's routed order for a first start (`turntaking.Route`).
fn route(routing: Routing, participant: i64, addressed: &[i64]) -> Vec<i64> {
    if routing == Routing::Flip {
        return vec![if participant == 77 { 88 } else { 77 }];
    }
    if !addressed.is_empty() {
        let mut targets: Vec<i64> = Vec::new();
        if addressed.contains(&participant) {
            targets.push(participant);
        }
        targets.extend(addressed.iter().filter(|id| **id != participant));
        return targets;
    }
    match routing {
        Routing::Lead(lead) => vec![lead],
        _ => vec![participant],
    }
}

/// The execution, response message and executor Main opens for each agent.
fn opened(participant: i64) -> (&'static str, &'static str, &'static str) {
    if participant == 88 {
        (EXECUTION_2, "22222222-2222-4333-8444-555555555555", "cloud")
    } else {
        (EXECUTION, "11111111-2222-4333-8444-555555555555", "local")
    }
}

fn floor_platform(later: Later) -> impl Fn(&Req) -> Res {
    main_platform(later, Routing::Addressed)
}

fn main_platform(later: Later, routing: Routing) -> impl Fn(&Req) -> Res {
    let first_question = Mutex::new(FIRST_QUESTION.to_owned());
    let first_targets = Mutex::new(Vec::<i64>::new());
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
            if body.get("floor_turn").is_none() {
                // A person's message: Main routes it, and answers on the
                // first target (a replay of the same question answers the
                // same execution).
                let participant = body["participant_id"].as_i64().unwrap_or_default();
                let addressed: Vec<i64> = body
                    .get("addressed_to")
                    .and_then(Value::as_array)
                    .map(|ids| ids.iter().filter_map(Value::as_i64).collect())
                    .unwrap_or_default();
                let targets = route(routing, participant, &addressed);
                first_question.lock().unwrap().clone_from(&question);
                first_targets.lock().unwrap().clone_from(&targets);
                let (execution, response, executor) = opened(targets[0]);
                return Res::json(
                    200,
                    &json!({
                        "execution_id": execution, "question_id": question,
                        "response_message_id": response,
                        "conversation_uuid": UUID, "participant_id": targets[0], "created": true,
                        "expires_at": "2026-10-10T00:00:00Z",
                        "memory_recall": {"text": ""}, "executor": executor,
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
            let targets = first_targets.lock().unwrap().clone();
            let participant = body["participant_id"].as_i64().unwrap_or_default();
            let (execution, response, executor) = opened(participant);
            return Res::json(
                200,
                &json!({
                    "execution_id": execution, "question_id": question,
                    "response_message_id": response,
                    "conversation_uuid": UUID, "participant_id": participant, "created": true,
                    "expires_at": "2026-10-10T00:00:00Z",
                    "memory_recall": {"text": ""}, "executor": executor,
                    "floor": {"question_id": first, "position": 1, "targets": targets, "agents": agents()}
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

fn starts_of(h: &Harness) -> Vec<Value> {
    bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_turn/prompt_lib/1/{UUID}"),
    )
}

/// The floor fields of the turn's `status` `running`.
fn running_floor(h: &Harness, turn_id: &str) -> Value {
    h.emitter
        .all()
        .into_iter()
        .find(|event| {
            event.turn_id == turn_id
                && event.kind == "status"
                && event.payload.get("floor").is_some()
        })
        .map(|event| event.payload["floor"].clone())
        .unwrap_or_else(|| panic!("no floor status: {:?}", h.emitter.kinds()))
}

/// A composer pick is not an address (ADR-0050 decision 7c, #1917): a
/// message that names no agent goes out with no `addressed_to`, and under
/// `lead` Main routes it to the lead. The host follows: it starts the same
/// question again as the lead (here a cloud agent), and the turn runs as it,
/// never as the composer's agent.
#[tokio::test]
async fn an_unaddressed_message_goes_to_the_lead_not_the_composers_agent() {
    let server = serve(main_platform(Later::Started, Routing::Lead(88))).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h
        .host
        .start(TurnRequest {
            prompt: "Review the change".into(),
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    assert_eq!(started.execution_id, EXECUTION_2);
    until_done_of(&h, &started.turn_id).await;

    let starts = starts_of(&h);
    assert_eq!(starts.len(), 2, "{starts:?}");
    // The composer's agent is the sticky target, never an address.
    assert_eq!(starts[0]["participant_id"], 77);
    assert!(starts[0].get("addressed_to").is_none(), "{}", starts[0]);
    // The same message, as the lead.
    assert_eq!(starts[1]["participant_id"], 88);
    assert_eq!(starts[1]["question_id"], starts[0]["question_id"]);
    assert!(starts[1].get("addressed_to").is_none());
    assert!(starts[1].get("floor_turn").is_none());

    // The lead answered, in the cloud: the composer's agent never ran.
    assert!(seen(&h.server, "/llm/v1/chat/completions").is_empty());
    let child = bodies(
        &h.server,
        &format!("/api/v2/elitea_core/local_child_turn/prompt_lib/1/{EXECUTION_2}"),
    );
    assert_eq!(child.len(), 1);
    assert_eq!(child[0]["application_id"], 6);
    let floor = running_floor(&h, &started.turn_id);
    assert_eq!(floor["agent_name"], "Reviewer");
    assert_eq!(floor["participant_id"], 88);
    assert_eq!(floor["executor"], "cloud");
    assert_eq!(floor["followed"], true);
    assert_eq!(settled_done(&h, &started.turn_id)["commit"], "committed");
    assert!(
        seen(
            &h.server,
            &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}")
        )
        .is_empty()
    );
}

/// The lead is a local agent and the composer shows a cloud one: the host
/// follows to the lead and runs it here, with the workspace.
#[tokio::test]
async fn a_local_lead_answers_a_message_sent_from_a_cloud_agents_composer() {
    let server = serve(main_platform(Later::Started, Routing::Lead(77))).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h
        .host
        .start(TurnRequest {
            prompt: "Fix the flaky test".into(),
            application_id: 6,
            version_id: 10,
            cloud: true,
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    assert_eq!(started.execution_id, EXECUTION);
    until_done_of(&h, &started.turn_id).await;
    let starts = starts_of(&h);
    assert_eq!(starts.len(), 2, "{starts:?}");
    assert_eq!(starts[0]["participant_id"], 88);
    assert_eq!(starts[1]["participant_id"], 77);
    // Coder ran here; no cloud relay.
    assert_eq!(seen(&h.server, "/llm/v1/chat/completions").len(), 1);
    assert!(
        h.server
            .seen()
            .iter()
            .all(|req| !req.path.contains("/local_child_turn/"))
    );
    let floor = running_floor(&h, &started.turn_id);
    assert_eq!(floor["agent_name"], "Coder");
    assert_eq!(floor["executor"], "local");
    assert_eq!(settled_done(&h, &started.turn_id)["commit"], "committed");
}

/// An address beats the lead: a message naming one agent goes to it, and
/// the host follows nothing.
#[tokio::test]
async fn an_addressed_message_goes_to_the_addressee_under_a_lead() {
    let server = serve(main_platform(Later::Started, Routing::Lead(88))).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h
        .host
        .start(TurnRequest {
            prompt: "@Coder fix the flaky test".into(),
            addressed_to: vec![77],
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    until_done_of(&h, &started.turn_id).await;
    let starts = starts_of(&h);
    assert_eq!(starts.len(), 1, "{starts:?}");
    assert_eq!(starts[0]["addressed_to"], json!([77]));
    assert_eq!(seen(&h.server, "/llm/v1/chat/completions").len(), 1);
    let floor = running_floor(&h, &started.turn_id);
    assert_eq!(floor["agent_name"], "Coder");
    assert!(floor.get("followed").is_none());
}

/// The host follows once, and only to the agent Main answered on: a Main
/// that routes the followed start elsewhere again is refused, and no agent
/// runs.
#[tokio::test]
async fn a_start_routed_elsewhere_again_is_refused_and_runs_no_agent() {
    let server = serve(main_platform(Later::Started, Routing::Flip)).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let refused = h
        .host
        .start(TurnRequest {
            prompt: "Fix the flaky test".into(),
            ..request(&h.workspace_id)
        })
        .await
        .unwrap_err();
    assert_eq!(refused.code, "floor_routed_elsewhere");
    assert_eq!(starts_of(&h).len(), 2);
    assert!(seen(&h.server, "/llm/v1/chat/completions").is_empty());
    assert!(h.server.seen().iter().all(|req| {
        !req.path.contains("/local_child_turn/") && !req.path.contains("/local_turn_commit/")
    }));
}

/// A followed cloud agent takes none of the composer's sticky modes, and
/// keeps the message's own picks (which its turn then refuses with its
/// reason).
#[test]
fn a_followed_cloud_agent_drops_the_sticky_modes() {
    let target = super::super::api::FloorTarget {
        participant_id: 88,
        application_id: 6,
        version_id: 10,
        cloud: true,
    };
    let base = TurnRequest {
        plan_mode: true,
        bypass: true,
        mentions: vec!["a.txt".into()],
        ..request("w")
    };
    let followed = super::super::floor::follow(&base, &target);
    assert_eq!((followed.application_id, followed.version_id), (6, 10));
    assert!(followed.cloud && !followed.plan_mode && !followed.bypass);
    assert!(followed.model.is_none() && followed.unattended.is_none());
    assert_eq!(followed.mentions, vec!["a.txt".to_owned()]);
    assert_eq!(followed.prompt, base.prompt);
    // A local agent keeps them.
    let local = super::super::floor::follow(
        &base,
        &super::super::api::FloorTarget {
            cloud: false,
            ..target
        },
    );
    assert!(local.plan_mode && local.bypass && !local.cloud);
}

/// Main opened a "Run to completion" chain for the composer's grant, and the
/// followed cloud agent runs without it: its commit ends the chain.
#[test]
fn a_chain_the_followed_agent_runs_without_ends_with_its_turn() {
    use super::super::floor::closing_outcome;
    use super::super::unattended::ChainOutcome;
    let started = |fields: Value| -> super::super::api::LocalTurnStarted {
        let mut body = json!({"execution_id": "e", "question_id": "q",
            "response_message_id": "r", "memory_recall": {"text": ""}});
        if let (Value::Object(body), Value::Object(fields)) = (&mut body, fields) {
            body.extend(fields);
        }
        serde_json::from_value(body).unwrap()
    };
    let chain = started(json!({"unattended": {"grant_id": "g"}}));
    let request = request("w");
    assert_eq!(
        closing_outcome(&request, &chain, false),
        Some(ChainOutcome::Completed)
    );
    assert_eq!(
        closing_outcome(&request, &chain, true),
        Some(ChainOutcome::Failed)
    );
    // No chain was opened: nothing to close.
    assert_eq!(closing_outcome(&request, &started(json!({})), false), None);
}
