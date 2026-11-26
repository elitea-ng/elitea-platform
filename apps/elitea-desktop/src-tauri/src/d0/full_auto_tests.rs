//! Full auto (ADR-0061 amendment of 2026-11-26, #3079): a turn answers the
//! asks Bypass answers and records each answer in `local_work.full_auto`;
//! its hard-deny list still reaches the person; an organisation that
//! forbids it, or plan mode, refuses the start; the folder's session
//! leaves it with the turn.

use super::*;

fn full_auto_allowed() -> Option<Value> {
    Some(
        json!({"allowed": true, "shell": true, "max_sandbox_mode": "workspace-write",
        "full_auto": true}),
    )
}

/// A model that runs `command`, then answers.
async fn command_platform(command: &'static str) -> MockServer {
    let inner = platform(agent_details(), &[]);
    let calls = AtomicUsize::new(0);
    serve(move |req: &Req| {
        if req.path == "/llm/v1/chat/completions" {
            return match calls.fetch_add(1, Ordering::SeqCst) {
                0 => tool_call("call-1", "run_command", &json!({"command": command})),
                _ => say("Done."),
            };
        }
        inner(req)
    })
    .await
}

fn full_auto_request(h: &Harness) -> TurnRequest {
    TurnRequest {
        bypass: true,
        full_auto: true,
        ..request(&h.workspace_id)
    }
}

fn prompt_rule(h: &Harness, program: &str) {
    write_rules(
        &h.workspaces.data_dir(&h.workspace_id).join("rules.json"),
        &format!(r#"{{"version":1,"rules":[{{"pattern":{program},"decision":"prompt"}}]}}"#),
    );
}

#[tokio::test]
async fn a_full_auto_turn_answers_and_records_each_answer() {
    let server = command_platform("git reset --hard").await;
    let h = harness(server, full_auto_allowed(), UiDecision::Deny).await;
    prompt_rule(&h, r#"["git","reset"]"#);
    h.host.start(full_auto_request(&h)).await.unwrap();
    let events = until_done(&h).await;
    assert!(
        of_kind(&events, "approval_request").is_empty(),
        "{:?}",
        h.emitter.kinds()
    );
    let commit = &commit_bodies(&h.server)[0];
    assert_eq!(commit["hitl_exchanges"][0]["source"], "bypass");
    assert_eq!(commit["hitl_exchanges"][0]["decision"], "approve");
    let full_auto = &commit["local_work"]["full_auto"];
    assert_eq!(full_auto["answered"], 1, "{commit}");
    assert_eq!(
        full_auto["asks"][0]["command"],
        "run_command: git reset --hard"
    );
    assert_eq!(full_auto["asks"][0]["tainted"], false);
    assert_eq!(full_auto["asks"][0]["held"], false);
    // The folder's session leaves Full auto with the turn.
    let (session, _) = h.host.process_session(&h.workspace_id).expect("session");
    assert_eq!(
        session.profile(),
        elitea_local_tools::approvals::PermissionProfile::Build
    );
}

/// A command on the hard-deny list (a shell profile) is not answered: the
/// person is asked, and their answer is used; the hold is audited.
#[tokio::test]
async fn the_hard_deny_list_still_reaches_the_person() {
    let server = command_platform("cat ~/.zshrc").await;
    let h = harness(server, full_auto_allowed(), UiDecision::Deny).await;
    prompt_rule(&h, r#"["cat"]"#);
    h.host.start(full_auto_request(&h)).await.unwrap();
    let events = until_done(&h).await;
    assert_eq!(
        of_kind(&events, "approval_request").len(),
        1,
        "{:?}",
        h.emitter.kinds()
    );
    let commit = &commit_bodies(&h.server)[0];
    assert_eq!(commit["hitl_exchanges"][0]["source"], "person");
    assert_eq!(commit["hitl_exchanges"][0]["decision"], "reject");
    let full_auto = &commit["local_work"]["full_auto"];
    assert_eq!(full_auto["answered"], 0, "{commit}");
    assert_eq!(full_auto["asks"][0]["held"], true);
}

/// An organisation without `full_auto` refuses the start; so does plan mode.
#[tokio::test]
async fn a_forbidden_or_planning_full_auto_start_is_refused() {
    let server = command_platform("git status").await;
    let h = harness(server, allowed(), UiDecision::Deny).await;
    let error = h.host.start(full_auto_request(&h)).await.unwrap_err();
    assert_eq!(error.code, super::super::turn::FULL_AUTO_FORBIDDEN);

    let server = command_platform("git status").await;
    let h = harness(server, full_auto_allowed(), UiDecision::Deny).await;
    let error = h
        .host
        .start(TurnRequest {
            plan_mode: true,
            ..full_auto_request(&h)
        })
        .await
        .unwrap_err();
    assert_eq!(error.code, "invalid_request");
}

/// A Bypass turn commits no `full_auto` audit.
#[tokio::test]
async fn a_bypass_turn_commits_no_full_auto_audit() {
    let server = command_platform("git reset --hard").await;
    let h = harness(server, full_auto_allowed(), UiDecision::Deny).await;
    prompt_rule(&h, r#"["git","reset"]"#);
    h.host
        .start(TurnRequest {
            bypass: true,
            ..request(&h.workspace_id)
        })
        .await
        .unwrap();
    until_done(&h).await;
    let commit = &commit_bodies(&h.server)[0];
    assert!(commit["local_work"].get("full_auto").is_none(), "{commit}");
}
