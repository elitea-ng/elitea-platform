//! End-to-end tests of the D0 assembler against an in-process mock of the
//! platform (`testutil::serve`): resolve → start → a model tool-call loop
//! with one local and one remote tool (the 409 confirmation path included)
//! → commit; the refusals; event sequencing; changes and undo.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Value, json};

use super::api::{ApiError, Bearer, Credentials, PlatformApi};
use super::approvals::UiDecision;
use super::definition::RemoteToolSpec;
use super::events::{AgentEvent, VecEmitter};
use super::remote_tools::{self, RemoteContext, RetryPolicy};
use super::turn::{AgentHost, HostDeps, PolicySource, TURNS_KEPT_PER_WORKSPACE, TurnRequest};
use crate::testutil::{MockServer, Req, Res, serve};
use crate::workspaces::WorkspaceStore;

const EXECUTION: &str = "0123456789abcdef0123456789abcdef";
const TOOLKIT_REF: &str = "tkr1_0123456789abcdef0123456789abcdef";
const INTERRUPT: &str = "hitl_fedcba9876543210fedcba9876543210";

/// The signed-in session; a test may sign out or switch it mid-way.
struct StaticCredentials {
    origin: String,
    identity: std::sync::Mutex<Option<String>>,
}

impl StaticCredentials {
    fn new(origin: String) -> Self {
        Self {
            identity: std::sync::Mutex::new(Some(format!("{origin}#1"))),
            origin,
        }
    }

    fn switch_to(&self, identity: Option<&str>) {
        *self.identity.lock().unwrap() = identity.map(str::to_owned);
    }
}

#[async_trait]
impl Credentials for StaticCredentials {
    async fn bearer(&self) -> Result<Bearer, ApiError> {
        Ok(Bearer {
            origin: self.origin.clone(),
            token: "elnat_test_token".into(),
        })
    }

    async fn refreshed(&self) -> Result<Bearer, ApiError> {
        self.bearer().await
    }

    fn identity(&self) -> Option<String> {
        self.identity.lock().unwrap().clone()
    }
}

/// The stored policy's `local_work`; a test may change it mid-way.
struct Policy(std::sync::Mutex<Option<Value>>);

impl Policy {
    fn set(&self, local_work: Option<Value>) {
        *self.0.lock().unwrap() = local_work;
    }
}

impl PolicySource for Policy {
    fn local_work(&self) -> Option<Value> {
        self.0.lock().unwrap().clone()
    }
}

fn allowed() -> Option<Value> {
    Some(json!({"allowed": true, "shell": true, "max_sandbox_mode": "workspace-write"}))
}

fn skill() -> Value {
    let content = "Write tersely.";
    json!({
        "id": "skill-style", "name": "Style", "description": "the house style.",
        "instructions": content,
        "revision": elitea_agent_runtime::instruction_authority::content_digest(content),
        "scope": "project"
    })
}

fn resolved(details: &Value, withheld: &[&str]) -> Value {
    json!({
        "schema_version": "elitea.client.resolved-application-version.v1",
        "project_id": 1, "application_id": 5, "version_id": 9,
        "definition_sha256": "00", "withheld_secrets": withheld,
        "project_context_withheld": false,
        "version_details": details,
    })
}

fn agent_details() -> Value {
    json!({
        "instructions": "Be brief.",
        "agent_type": "openai",
        "llm_settings": {"model_name": "gpt-test", "max_tokens": 256},
        "skills": [skill()],
        "tools": [{
            "kind": "remote_toolkit", "type": "jira", "toolkit_name": "Jira",
            "selected_tools": ["create_issue"], "all_tools": false,
            "toolkit_ref": {"toolkit_id": 3, "project_id": 1, "ref": TOOLKIT_REF}
        }]
    })
}

fn sse(chunks: &[Value]) -> Res {
    let mut body = String::new();
    for chunk in chunks {
        body.push_str(&format!("data: {chunk}\n\n"));
    }
    body.push_str("data: [DONE]\n\n");
    Res {
        status: 200,
        headers: vec![("Content-Type", "text/event-stream".into())],
        body,
    }
}

fn tool_call(id: &str, name: &str, args: &Value) -> Res {
    sse(&[
        json!({"choices": [{"delta": {"tool_calls": [{"index": 0, "id": id, "type": "function",
            "function": {"name": name, "arguments": args.to_string()}}]}}]}),
        json!({"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}),
    ])
}

/// The platform: one agent, one conversation, a model that writes a local
/// file, then calls the remote toolkit (sensitive: 409 first), then answers.
fn platform(details: Value, withheld: &'static [&'static str]) -> impl Fn(&Req) -> Res {
    let model_calls = AtomicUsize::new(0);
    let remote_calls = AtomicUsize::new(0);
    move |req: &Req| {
        let path = req.path.as_str();
        if path == "/api/v2/social/author" {
            return Res::json(200, &json!({"id": 1, "name": "Me"}));
        }
        if path == "/api/v2/elitea_core/resolved_version/prompt_lib/1/5/9" {
            return Res::json(200, &resolved(&details, withheld));
        }
        if path == "/api/v2/elitea_core/conversation/prompt_lib/1/42" {
            return Res::json(
                200,
                &json!({"id": 42, "uuid": "99999999-2222-4333-8444-555555555555", "participants": [
                    {"id": 70, "entity_name": "user", "entity_meta": {"id": 1}},
                    {"id": 77, "entity_name": "application", "entity_meta": {"id": 5, "project_id": 1},
                     "entity_settings": {"version_id": 9}}
                ]}),
            );
        }
        // The route takes the conversation UUID only (the server's validStart).
        if path
            == "/api/v2/elitea_core/local_turn/prompt_lib/1/99999999-2222-4333-8444-555555555555"
        {
            let body: Value = serde_json::from_str(&req.body).unwrap_or_default();
            return Res::json(
                200,
                &json!({
                    "execution_id": EXECUTION,
                    "question_id": body["question_id"],
                    "response_message_id": "11111111-2222-4333-8444-555555555555",
                    "conversation_uuid": "99999999-2222-4333-8444-555555555555",
                    "participant_id": 77, "expires_at": "2026-10-10T00:00:00Z", "created": true,
                    "memory_recall": {"text": "Memory: the user likes tea.", "count": 1, "memory_ids": ["3"]}
                }),
            );
        }
        if path == "/llm/v1/chat/completions" {
            return match model_calls.fetch_add(1, Ordering::SeqCst) {
                0 => tool_call(
                    "call-local",
                    "write_file",
                    &json!({"path": "notes.txt", "content": "hello\n"}),
                ),
                1 => tool_call(
                    "call-remote",
                    "Jira_create_issue",
                    &json!({"summary": "Bug"}),
                ),
                _ => sse(&[
                    json!({"choices": [{"delta": {"content": "Done"}}]}),
                    json!({"choices": [{"delta": {"content": "."}, "finish_reason": "stop"}]}),
                ]),
            };
        }
        if path == "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/1/3" {
            let body: Value = serde_json::from_str(&req.body).unwrap_or_default();
            remote_calls.fetch_add(1, Ordering::SeqCst);
            if body.get("confirmation").is_none() {
                return Res::json(
                    409,
                    &json!({
                        "ok": false, "error": "confirmation_required", "toolkit_id": 3,
                        "message": "Creates an issue in Jira",
                        "hitl_interrupt": {"interrupt_id": INTERRUPT, "guardrail_type": "sensitive_tool",
                            "action_label": "Create a Jira issue", "policy_message": "Creates an issue in Jira",
                            "available_actions": ["approve", "reject"], "tool_name": "create_issue"}
                    }),
                );
            }
            return Res::json(
                200,
                &json!({"ok": true, "status": "ok", "task_id": "t1",
                "toolkit_id": 3, "tool_name": "create_issue", "result": {"key": "J-1"}}),
            );
        }
        if path == format!("/api/v2/elitea_core/local_turn_commit/prompt_lib/1/{EXECUTION}") {
            return Res::json(
                200,
                &json!({"execution_id": EXECUTION, "created": true,
                "conversation_uuid": "99999999-2222-4333-8444-555555555555",
                "question_message_id": "q", "response_message_id": "r",
                "memories_used": 1, "committed_at": "2026-10-09T00:00:00Z"}),
            );
        }
        Res::json(404, &json!({"error": "not_found", "message": path}))
    }
}

struct Harness {
    server: MockServer,
    credentials: Arc<StaticCredentials>,
    host: Arc<AgentHost>,
    emitter: Arc<VecEmitter>,
    workspace_id: String,
    folder: tempfile::TempDir,
    policy: Arc<Policy>,
    app: tempfile::TempDir,
}

/// A host on the mock platform; approvals answered `decision` by the UI.
async fn harness(server: MockServer, policy: Option<Value>, decision: UiDecision) -> Harness {
    let app = tempfile::tempdir().unwrap();
    let folder = tempfile::tempdir().unwrap();
    let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
    let workspace_id = workspaces.add(folder.path()).unwrap().id;
    // The UI binds a folder before it offers a session.
    workspaces.bind_project(&workspace_id, 1).unwrap();
    let emitter = Arc::new(VecEmitter::default());
    let policy = Arc::new(Policy(std::sync::Mutex::new(policy)));
    let credentials = Arc::new(StaticCredentials::new(server.origin.clone()));
    let host = Arc::new(
        AgentHost::new(HostDeps {
            credentials: credentials.clone(),
            client_version: "0.1.0".into(),
            policy: policy.clone(),
            workspaces,
            emitter: emitter.clone(),
            retry: RetryPolicy {
                attempts: 3,
                delay: Duration::from_millis(10),
            },
            history: Some(Arc::new(
                crate::history::HistoryStore::open(app.path()).unwrap(),
            )),
        })
        .unwrap(),
    );
    // The UI: answer every approval request.
    let (answers, mut questions) = tokio::sync::mpsc::unbounded_channel::<String>();
    emitter
        .on_event
        .lock()
        .unwrap()
        .replace(Box::new(move |event: &AgentEvent| {
            if event.kind == "approval_request" {
                let id = event.payload["request_id"].as_str().unwrap().to_owned();
                let _ = answers.send(id);
            }
        }));
    tokio::spawn({
        let host = host.clone();
        async move {
            while let Some(id) = questions.recv().await {
                assert!(host.respond(&id, decision));
            }
        }
    });
    Harness {
        server,
        credentials,
        host,
        emitter,
        workspace_id,
        folder,
        policy,
        app,
    }
}

fn request(workspace_id: &str) -> TurnRequest {
    TurnRequest {
        workspace_id: workspace_id.to_owned(),
        project_id: 1,
        conversation_id: json!(42),
        application_id: 5,
        version_id: 9,
        prompt: "Write notes and file a bug".into(),
        plan_mode: false,
        mentions: Vec::new(),
    }
}

async fn until_done(emitter: &VecEmitter) -> Vec<AgentEvent> {
    for _ in 0..500 {
        let events = emitter.all();
        if events.iter().any(|event| event.kind == "done") {
            return events;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("the turn did not finish: {:?}", emitter.kinds());
}

async fn until_done_of(emitter: &VecEmitter, turn_id: &str) {
    for _ in 0..500 {
        if emitter
            .all()
            .iter()
            .any(|event| event.kind == "done" && event.turn_id == turn_id)
        {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("turn {turn_id} did not finish: {:?}", emitter.kinds());
}

/// The platform of [`platform`], with a model that stalls while `slow` is
/// set (for at most five seconds).
async fn stalling_platform(slow: Arc<AtomicBool>) -> MockServer {
    let inner = platform(agent_details(), &[]);
    serve(move |req: &Req| {
        if req.path == "/llm/v1/chat/completions" {
            // block_in_place: a plain blocking wait here starves the
            // runtime's timer, and the test's own sleeps with it.
            tokio::task::block_in_place(|| {
                for _ in 0..250 {
                    if !slow.load(Ordering::SeqCst) {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(20));
                }
            });
        }
        inner(req)
    })
    .await
}

fn seen(server: &MockServer, path_end: &str) -> Vec<Req> {
    server
        .seen()
        .into_iter()
        .filter(|req| req.path.ends_with(path_end))
        .collect()
}

#[tokio::test]
async fn one_local_turn_runs_end_to_end_and_commits() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    assert_eq!(started.execution_id, EXECUTION);
    let events = until_done(&h.emitter).await;

    // Event sequencing: one turn id, gapless sequence, phases in order.
    assert!(events.iter().all(|e| e.turn_id == started.turn_id));
    for (index, event) in events.iter().enumerate() {
        assert_eq!(event.seq, index as u64, "{:?}", h.emitter.kinds());
    }
    let compact: Vec<String> = events
        .iter()
        .map(|e| match e.kind {
            "status" => format!("status:{}", e.payload["phase"].as_str().unwrap()),
            "tool_call" | "tool_result" => {
                format!("{}:{}", e.kind, e.payload["call_id"].as_str().unwrap())
            }
            other => other.to_owned(),
        })
        .collect();
    assert_eq!(
        compact,
        [
            "status:resolving",
            "status:starting",
            "status:running",
            "tool_call:call-local",
            "tool_result:call-local",
            "tool_call:call-remote",
            "approval_request",
            "tool_result:call-remote",
            "text_delta",
            "text_delta",
            "status:committing",
            "status:done",
            "done",
        ]
    );
    let by_kind = |kind: &str| {
        events
            .iter()
            .filter(|e| e.kind == kind)
            .cloned()
            .collect::<Vec<_>>()
    };
    let calls = by_kind("tool_call");
    assert_eq!(calls[0].payload["tool"], "write_file");
    assert_eq!(calls[0].payload["remote"], false);
    assert_eq!(calls[0].payload["args_summary"], "notes.txt");
    assert_eq!(calls[1].payload["tool"], "Jira_create_issue");
    assert_eq!(calls[1].payload["remote"], true);
    assert!(
        by_kind("tool_result")
            .iter()
            .all(|e| e.payload["ok"] == true)
    );
    let approval = &by_kind("approval_request")[0].payload;
    assert_eq!(approval["title"], "Create a Jira issue");
    assert_eq!(approval["reason"], "Creates an issue in Jira");
    assert_eq!(approval["can_remember"], false);
    let done = &by_kind("done")[0].payload;
    assert_eq!(done["committed"], true);
    assert_eq!(done["conversation_id"], 42);
    assert_eq!(done["changed_files"], 1);
    assert_eq!(
        done["message_ids"][1],
        "11111111-2222-4333-8444-555555555555"
    );

    // The local tool really ran in the workspace.
    assert_eq!(
        std::fs::read_to_string(h.folder.path().join("notes.txt")).unwrap(),
        "hello\n"
    );

    // Start: the conversation's agent participant answers, addressed by the
    // conversation UUID although the UI passed the numeric id 42.
    let start = &seen(
        &h.server,
        "/local_turn/prompt_lib/1/99999999-2222-4333-8444-555555555555",
    )[0];
    let start_body: Value = serde_json::from_str(&start.body).unwrap();
    assert_eq!(start_body["participant_id"], 77);
    assert_eq!(start_body["user_input"], "Write notes and file a bug");
    assert_eq!(start.headers["authorization"], "Bearer elnat_test_token");
    assert_eq!(start.headers["x-client-version"], "0.1.0");

    // The model: the /llm contract headers, the memory splice, the skills.
    let llm = seen(&h.server, "/llm/v1/chat/completions");
    assert_eq!(llm.len(), 3);
    assert_eq!(llm[0].headers["x-project-id"], "1");
    assert_eq!(llm[0].headers["x-elitea-execution-id"], EXECUTION);
    assert_eq!(llm[0].headers["authorization"], "Bearer elnat_test_token");
    assert!(!llm[0].headers.contains_key("openai-project"));
    let first: Value = serde_json::from_str(&llm[0].body).unwrap();
    assert_eq!(first["model"], "gpt-test");
    let system = first["messages"][0]["content"].as_str().unwrap();
    assert!(
        system.starts_with("Be brief.\n\nMemory: the user likes tea."),
        "{system}"
    );
    assert!(system.contains("Available skill: Style."), "{system}");
    let tools: Vec<&str> = first["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["function"]["name"].as_str().unwrap())
        .collect();
    assert!(tools.contains(&"write_file") && tools.contains(&"Jira_create_issue"));
    assert!(tools.contains(&"load_skill"), "{tools:?}");
    let third: Value = serde_json::from_str(&llm[2].body).unwrap();
    let roles: Vec<&str> = third["messages"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["role"].as_str().unwrap())
        .collect();
    assert_eq!(
        roles,
        ["system", "user", "assistant", "tool", "assistant", "tool"]
    );

    // The remote call: 409, then the approved call with a new key.
    let remote = seen(&h.server, "/remote_toolkit_call/prompt_lib/1/3");
    assert_eq!(remote.len(), 2);
    let first_call: Value = serde_json::from_str(&remote[0].body).unwrap();
    assert_eq!(
        first_call,
        json!({"execution_id": EXECUTION, "application_id": 5, "version_id": 9,
            "toolkit_ref": TOOLKIT_REF, "tool_name": "create_issue", "arguments": {"summary": "Bug"}})
    );
    let second_call: Value = serde_json::from_str(&remote[1].body).unwrap();
    assert_eq!(second_call["confirmation"]["interrupt_id"], INTERRUPT);
    assert_eq!(second_call["confirmation"]["approved"], true);
    assert_ne!(
        remote[0].headers["idempotency-key"],
        remote[1].headers["idempotency-key"]
    );

    // The commit.
    let commit = &seen(
        &h.server,
        &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"),
    )[0];
    let body: Value = serde_json::from_str(&commit.body).unwrap();
    assert_eq!(
        body["user_message"]["content"],
        "Write notes and file a bug"
    );
    assert_eq!(body["assistant_message"], json!({"content": "Done."}));
    let tool_calls = body["tool_calls"].as_object().unwrap();
    assert_eq!(tool_calls.len(), 2);
    assert_eq!(tool_calls["call-local"]["tool_name"], "write_file");
    assert_eq!(tool_calls["call-remote"]["tool_name"], "Jira_create_issue");
    assert_eq!(
        tool_calls["call-remote"]["tool_inputs"],
        json!({"summary": "Bug"})
    );
    assert!(
        tool_calls["call-remote"]["tool_output"]
            .as_str()
            .unwrap()
            .contains("J-1")
    );
    assert_eq!(body["hitl_exchanges"][0]["kind"], "tool_confirmation");
    assert_eq!(body["hitl_exchanges"][0]["decision"], "approve");
    assert_eq!(body["hitl_exchanges"][0]["tool_run_id"], "call-remote");
    assert_eq!(body["local_work"]["sandbox_mode"], "workspace-write");
    assert_eq!(body["local_work"]["paths_touched"], json!(["notes.txt"]));
    assert_eq!(body["local_work"]["commands"], json!([]));

    // Changed files, then undo of the whole turn.
    let changes = h.host.changes(&started.turn_id).unwrap();
    assert_eq!(changes.len(), 1);
    assert_eq!(changes[0].path, "notes.txt");
    assert_eq!(changes[0].status, "added");
    assert_eq!((changes[0].added, changes[0].removed), (1, 0));
    assert!(changes[0].diff.contains("+hello"));
    let restored = h.host.restore(&started.turn_id, None).unwrap();
    assert_eq!(restored, ["notes.txt"]);
    assert!(!h.folder.path().join("notes.txt").exists());
    assert!(h.host.changes("unknown").is_err());
}

#[tokio::test]
async fn a_denied_remote_call_is_a_tool_failure_the_model_reads() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::Deny).await;
    h.host.start(request(&h.workspace_id)).await.unwrap();
    let events = until_done(&h.emitter).await;
    let results: Vec<_> = events.iter().filter(|e| e.kind == "tool_result").collect();
    assert_eq!(results[1].payload["ok"], false);
    assert!(
        results[1].payload["summary"]
            .as_str()
            .unwrap()
            .contains("rejected")
    );
    // Only the first, unconfirmed call reached the platform.
    assert_eq!(
        seen(&h.server, "/remote_toolkit_call/prompt_lib/1/3").len(),
        1
    );
    let commit = &seen(
        &h.server,
        &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"),
    )[0];
    let body: Value = serde_json::from_str(&commit.body).unwrap();
    assert_eq!(body["hitl_exchanges"][0]["decision"], "reject");
}

async fn refused(
    details: Value,
    withheld: &'static [&'static str],
    policy: Option<Value>,
) -> (String, Harness) {
    let server = serve(platform(details, withheld)).await;
    let h = harness(server, policy, UiDecision::AllowOnce).await;
    let error = h.host.start(request(&h.workspace_id)).await.unwrap_err();
    let events = h.emitter.all();
    assert_eq!(events.last().unwrap().payload["phase"], "error");
    assert_eq!(events[events.len() - 2].kind, "error");
    assert_eq!(
        events[events.len() - 2].payload["code"],
        error.code.as_str()
    );
    assert!(
        seen(
            &h.server,
            "/local_turn/prompt_lib/1/99999999-2222-4333-8444-555555555555"
        )
        .is_empty(),
        "a refused turn opens no execution"
    );
    (error.code, h)
}

#[tokio::test]
async fn an_agent_that_needs_secrets_is_refused_locally() {
    let (code, h) = refused(agent_details(), &["/instructions"], allowed()).await;
    assert_eq!(code, "secrets_withheld");
    let message = h.emitter.all()[1].payload["message"]
        .as_str()
        .unwrap()
        .to_owned();
    assert!(message.contains("run it in the cloud"), "{message}");
}

#[tokio::test]
async fn a_pipeline_and_nested_agents_are_refused_locally() {
    let (code, _) = refused(
        json!({"agent_type": "pipeline", "tools": []}),
        &[],
        allowed(),
    )
    .await;
    assert_eq!(code, "pipeline_unsupported");
    let nested = json!({
        "llm_settings": {"model_name": "m"},
        "tools": [{"kind": "application", "type": "application", "application_id": 2, "application_version_id": 3}]
    });
    let (code, _) = refused(nested, &[], allowed()).await;
    assert_eq!(code, "nested_agents_unsupported");
}

#[tokio::test]
async fn local_work_off_refuses_before_any_request() {
    for policy in [None, Some(json!({"allowed": false, "shell": true}))] {
        let (code, h) = refused(agent_details(), &[], policy).await;
        assert_eq!(code, "local_work_disabled");
        assert!(h.server.seen().is_empty());
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_cancelled_turn_commits_nothing() {
    // The model never answers: the turn waits until it is cancelled.
    let server = serve({
        let inner = platform(agent_details(), &[]);
        move |req: &Req| {
            if req.path == "/llm/v1/chat/completions" {
                std::thread::sleep(Duration::from_secs(2));
            }
            inner(req)
        }
    })
    .await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(h.host.status(&started.turn_id).unwrap().state, "running");
    assert_eq!(h.host.status("unknown").unwrap_err().code, "turn_unknown");
    h.host.cancel(&started.turn_id).unwrap();
    // A second cancel of a cancelled turn is still a cancel.
    h.host.cancel(&started.turn_id).unwrap();
    let events = until_done(&h.emitter).await;
    let kinds: Vec<_> = events.iter().map(|e| e.kind).collect();
    assert_eq!(kinds[kinds.len() - 2], "status");
    assert_eq!(events[events.len() - 2].payload["phase"], "cancelled");
    assert_eq!(events.last().unwrap().payload["committed"], false);
    assert!(
        seen(
            &h.server,
            &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}")
        )
        .is_empty()
    );
    assert_eq!(h.host.cancel("unknown").unwrap_err().code, "turn_unknown");
    // A cancelled turn stays cancelled: cancelling it again says so.
    h.host.cancel(&started.turn_id).unwrap();
    // The workspace is free again.
    let again = h.host.start(request(&h.workspace_id)).await;
    assert!(again.is_ok(), "{again:?}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_policy_change_does_not_let_an_undo_or_a_turn_past_a_running_turn() {
    let slow = Arc::new(AtomicBool::new(false));
    let server = stalling_platform(slow.clone()).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let first = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h.emitter, &first.turn_id).await;
    let notes = h.folder.path().join("notes.txt");
    assert!(notes.exists());

    // The policy changes, so the next turn runs on a rebuilt session.
    h.policy.set(Some(
        json!({"allowed": true, "shell": false, "max_sandbox_mode": "workspace-write"}),
    ));
    slow.store(true, Ordering::SeqCst);
    let second = h.host.start(request(&h.workspace_id)).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;

    // The busy flag is the workspace's, not the old session's: undoing
    // the first turn, or a third turn after one more policy change, waits.
    let undo = h.host.restore(&first.turn_id, None).unwrap_err();
    assert_eq!(undo.code, "workspace_busy");
    assert!(
        notes.exists(),
        "nothing was restored under the running turn"
    );
    h.policy.set(allowed());
    let third = h.host.start(request(&h.workspace_id)).await.unwrap_err();
    assert_eq!(third.code, "workspace_busy");

    h.host.cancel(&second.turn_id).unwrap();
    slow.store(false, Ordering::SeqCst);
    until_done_of(&h.emitter, &second.turn_id).await;
    assert_eq!(h.host.restore(&first.turn_id, None).unwrap(), ["notes.txt"]);
    assert!(!notes.exists());
}

/// The platform of [`platform`], with a model that answers at once.
async fn answering_platform() -> MockServer {
    let inner = platform(agent_details(), &[]);
    serve(move |req: &Req| {
        if req.path == "/llm/v1/chat/completions" {
            return sse(&[
                json!({"choices": [{"delta": {"content": "ok"}, "finish_reason": "stop"}]}),
            ]);
        }
        inner(req)
    })
    .await
}

#[tokio::test]
async fn only_the_last_turns_of_a_workspace_are_kept() {
    let h = harness(answering_platform().await, allowed(), UiDecision::AllowOnce).await;
    let mut ids = Vec::new();
    for _ in 0..=TURNS_KEPT_PER_WORKSPACE {
        let started = h.host.start(request(&h.workspace_id)).await.unwrap();
        until_done_of(&h.emitter, &started.turn_id).await;
        ids.push(started.turn_id);
    }
    // The oldest is forgotten, with a code that says why.
    assert_eq!(h.host.changes(&ids[0]).unwrap_err().code, "turn_expired");
    assert_eq!(
        h.host.restore(&ids[0], None).unwrap_err().code,
        "turn_expired"
    );
    assert_eq!(h.host.cancel(&ids[0]).unwrap_err().code, "turn_expired");
    for kept in &ids[1..] {
        assert!(h.host.changes(kept).is_ok());
    }
    assert_eq!(h.host.changes("nope").unwrap_err().code, "turn_unknown");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_workspace_with_a_running_turn_cannot_be_removed() {
    let slow = Arc::new(AtomicBool::new(true));
    let h = harness(
        stalling_platform(slow.clone()).await,
        allowed(),
        UiDecision::AllowOnce,
    )
    .await;
    let running = h.host.start(request(&h.workspace_id)).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let refused = h.host.remove_workspace(&h.workspace_id).unwrap_err();
    assert_eq!(refused.code, "workspace_busy");

    h.host.cancel(&running.turn_id).unwrap();
    slow.store(false, Ordering::SeqCst);
    until_done_of(&h.emitter, &running.turn_id).await;
    h.host.remove_workspace(&h.workspace_id).unwrap();
    // Its turns are gone from the host, and a new turn finds no workspace.
    assert_eq!(
        h.host.changes(&running.turn_id).unwrap_err().code,
        "turn_unknown"
    );
    assert_eq!(
        h.host
            .start(request(&h.workspace_id))
            .await
            .unwrap_err()
            .code,
        "workspace_unknown"
    );
    assert!(h.folder.path().exists(), "the folder itself is untouched");
}

#[tokio::test]
async fn a_model_refusal_is_committed_as_a_failed_turn() {
    let server = serve({
        let inner = platform(agent_details(), &[]);
        move |req: &Req| {
            if req.path == "/llm/v1/chat/completions" {
                return Res::json(
                    402,
                    &json!({"error": {"type": "budget_exceeded",
                    "code": "member_budget_exceeded", "scope": "member", "message": "no"}}),
                );
            }
            inner(req)
        }
    })
    .await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    h.host.start(request(&h.workspace_id)).await.unwrap();
    let events = until_done(&h.emitter).await;
    let error = events.iter().find(|e| e.kind == "error").unwrap();
    assert_eq!(
        error.payload["code"],
        "model_gateway.member_budget_exhausted"
    );
    let phases: Vec<_> = events
        .iter()
        .filter(|e| e.kind == "status")
        .map(|e| e.payload["phase"].as_str().unwrap().to_owned())
        .collect();
    assert_eq!(
        phases,
        ["resolving", "starting", "running", "committing", "error"]
    );
    assert_eq!(events.last().unwrap().payload["committed"], true);
    let commit = &seen(
        &h.server,
        &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"),
    )[0];
    let body: Value = serde_json::from_str(&commit.body).unwrap();
    assert_eq!(body["assistant_message"]["is_error"], true);
    assert_eq!(body["assistant_message"]["content"], "");
}

#[tokio::test]
async fn a_remote_retry_reuses_its_idempotency_key() {
    let attempts = Arc::new(AtomicUsize::new(0));
    let server = serve({
        let attempts = attempts.clone();
        move |_req: &Req| {
            if attempts.fetch_add(1, Ordering::SeqCst) == 0 {
                Res::json(
                    504,
                    &json!({"ok": false, "error": "remote_toolkit_timeout", "task_id": "t"}),
                )
            } else {
                Res::json(
                    200,
                    &json!({"ok": true, "status": "ok", "result": "fine", "truncated": false}),
                )
            }
        }
    })
    .await;
    let api = Arc::new(
        PlatformApi::new(
            Arc::new(StaticCredentials::new(server.origin.clone())),
            "0.1.0",
        )
        .unwrap(),
    );
    let context = RemoteContext {
        api,
        project_id: 1,
        execution_id: EXECUTION.into(),
        application_id: 5,
        version_id: 9,
        prompt: Arc::new(super::approvals::UiPrompt::new(Arc::default())),
        retry: RetryPolicy {
            attempts: 3,
            delay: Duration::from_millis(5),
        },
    };
    let spec = RemoteToolSpec {
        exposed_name: "Jira_call".into(),
        toolkit_id: 3,
        toolkit_ref: TOOLKIT_REF.into(),
        toolkit_name: "Jira".into(),
        description: String::new(),
        tool_name: None,
    };
    let result = remote_tools::call(&context, &spec, "c1", "search", json!({"q": 1})).await;
    assert_eq!(result, json!({"status": "ok", "result": "fine"}));
    let seen = server.seen();
    assert_eq!(seen.len(), 2);
    assert_eq!(
        seen[0].headers["idempotency-key"],
        seen[1].headers["idempotency-key"]
    );
    assert_eq!(seen[0].body, seen[1].body);
}

/// ADR-0029 decision 2: no crate in this binary may turn on serde_json's
/// `preserve_order` (it would change the runtime's digests). With it on,
/// `Map` keeps insertion order; without it, `Map` is a `BTreeMap`.
#[test]
fn serde_json_preserve_order_is_off() {
    let mut map = serde_json::Map::new();
    map.insert("b".into(), json!(1));
    map.insert("a".into(), json!(2));
    let keys: Vec<&String> = map.keys().collect();
    assert_eq!(
        keys,
        ["a", "b"],
        "serde_json/preserve_order is enabled in the desktop host's dependency graph"
    );
}

#[test]
fn turn_errors_keep_their_codes() {
    let error: super::turn::TurnError = ApiError::local("network", "down").into();
    assert_eq!(error.code, "network");
}

#[tokio::test]
async fn an_unbound_workspace_runs_no_turn() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let unbound = tempfile::tempdir().unwrap();
    let store = WorkspaceStore::new(h.app.path().to_owned());
    let id = store.add(unbound.path()).unwrap().id;
    let error = h.host.start(request(&id)).await.unwrap_err();
    assert_eq!(error.code, "workspace_unbound");
    assert!(h.server.seen().is_empty(), "refused before any request");
    // Bound to another project: refused too.
    h.host.bind_project(&id, 2).unwrap();
    let error = h.host.start(request(&id)).await.unwrap_err();
    assert_eq!(error.code, "workspace_project_mismatch");
    assert_eq!(
        h.host.bind_project("nope", 1).unwrap_err().code,
        "workspace_unknown"
    );
}

#[tokio::test]
async fn referenced_files_are_listed_under_the_prompt_never_inlined() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    std::fs::create_dir_all(h.folder.path().join("src")).unwrap();
    std::fs::write(h.folder.path().join("src/main.rs"), "SECRET_BODY").unwrap();
    let mut with_mentions = request(&h.workspace_id);
    with_mentions.mentions = vec!["src/main.rs".into(), "src".into(), "src/main.rs".into()];
    h.host.start(with_mentions).await.unwrap();
    until_done(&h.emitter).await;
    let expected =
        "Write notes and file a bug\n\nFiles the user referenced:\n- src/main.rs\n- src/";
    let start = &seen(
        &h.server,
        "/local_turn/prompt_lib/1/99999999-2222-4333-8444-555555555555",
    )[0];
    let start_body: Value = serde_json::from_str(&start.body).unwrap();
    assert_eq!(start_body["user_input"], expected);
    let commit = &seen(
        &h.server,
        &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"),
    )[0];
    let body: Value = serde_json::from_str(&commit.body).unwrap();
    assert_eq!(body["user_message"]["content"], expected);
    assert!(!commit.body.contains("SECRET_BODY") && !start.body.contains("SECRET_BODY"));
}

#[tokio::test]
async fn agents_md_is_applied_after_the_agents_instructions_and_reported() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    std::fs::write(h.folder.path().join("agents.md"), "Run task test.").unwrap();
    std::fs::create_dir_all(h.folder.path().join("apps/web")).unwrap();
    std::fs::write(h.folder.path().join("apps/web/AGENTS.md"), "Use pnpm.").unwrap();
    std::fs::write(h.folder.path().join("apps/web/main.ts"), "x").unwrap();
    let mut req = request(&h.workspace_id);
    req.mentions = vec!["apps/web/main.ts".into()];
    req.plan_mode = true;
    h.host.start(req).await.unwrap();
    let events = until_done(&h.emitter).await;
    let running = events
        .iter()
        .find(|e| e.kind == "status" && e.payload["phase"] == "running")
        .unwrap();
    assert_eq!(
        running.payload["project_instructions"],
        json!(["agents.md", "apps/web/AGENTS.md"])
    );
    let llm = seen(&h.server, "/llm/v1/chat/completions");
    let first: Value = serde_json::from_str(&llm[0].body).unwrap();
    let system = first["messages"][0]["content"].as_str().unwrap();
    let agent = system.find("Be brief.").unwrap();
    let root = system.find("Run task test.").unwrap();
    let nested = system.find("Use pnpm.").unwrap();
    assert!(agent < root && root < nested, "{system}");

    // Read fresh at the next start: an edit applies to the next turn.
    std::fs::remove_file(h.folder.path().join("agents.md")).unwrap();
    let before = h.emitter.all().len();
    h.host.start(request(&h.workspace_id)).await.unwrap();
    for _ in 0..500 {
        if h.emitter.all()[before..].iter().any(|e| e.kind == "done") {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    let second = h.emitter.all()[before..]
        .iter()
        .find(|e| e.kind == "status" && e.payload["phase"] == "running")
        .cloned()
        .unwrap();
    assert!(second.payload.get("project_instructions").is_none());
}

#[tokio::test]
async fn a_mention_outside_the_workspace_is_refused_before_any_request() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(
        server,
        Some(json!({"allowed": true, "shell": true, "max_sandbox_mode": "workspace-write", "path_deny": ["*.pem"]})),
        UiDecision::AllowOnce,
    )
    .await;
    std::fs::write(h.folder.path().join("key.pem"), "k").unwrap();
    for bad in ["../outside.txt", "/etc/passwd", "key.pem", "missing.rs"] {
        let mut req = request(&h.workspace_id);
        req.mentions = vec![bad.into()];
        let error = h.host.start(req).await.unwrap_err();
        assert_eq!(error.code, "invalid_request", "{bad}");
    }
    assert!(h.server.seen().is_empty(), "refused before any request");
}

#[tokio::test]
async fn the_file_picker_lists_the_workspace_under_its_policy() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(
        server,
        Some(json!({"allowed": true, "shell": true, "max_sandbox_mode": "workspace-write", "path_deny": ["*.pem"]})),
        UiDecision::AllowOnce,
    )
    .await;
    std::fs::write(h.folder.path().join(".gitignore"), "build/\n").unwrap();
    std::fs::create_dir_all(h.folder.path().join("build")).unwrap();
    std::fs::write(h.folder.path().join("build/out.txt"), "x").unwrap();
    std::fs::write(h.folder.path().join("notes.md"), "x").unwrap();
    std::fs::write(h.folder.path().join("key.pem"), "k").unwrap();
    let found = h.host.workspace_files(&h.workspace_id, "", None).unwrap();
    let paths: Vec<&str> = found.iter().map(|f| f.path.as_str()).collect();
    // Shallower, then shorter, first.
    assert_eq!(paths, ["notes.md", ".gitignore"]);
    assert_eq!(
        h.host.workspace_files("nope", "", None).unwrap_err().code,
        "workspace_unknown"
    );
    h.policy.set(None);
    assert_eq!(
        h.host
            .workspace_files(&h.workspace_id, "", None)
            .unwrap_err()
            .code,
        "local_work_disabled"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_running_turn_keeps_its_workspace_on_its_project() {
    let slow = Arc::new(AtomicBool::new(true));
    let h = harness(
        stalling_platform(slow.clone()).await,
        allowed(),
        UiDecision::AllowOnce,
    )
    .await;
    let running = h.host.start(request(&h.workspace_id)).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let refused = h.host.bind_project(&h.workspace_id, 2).unwrap_err();
    assert_eq!(refused.code, "workspace_busy");

    h.host.cancel(&running.turn_id).unwrap();
    slow.store(false, Ordering::SeqCst);
    until_done_of(&h.emitter, &running.turn_id).await;
    assert_eq!(
        h.host.bind_project(&h.workspace_id, 2).unwrap().project_id,
        Some(2)
    );
}

/// The platform of [`platform`], with the version read held while `hold`
/// is set (for at most five seconds); `entered` tells it arrived.
async fn holding_resolve(hold: Arc<AtomicBool>, entered: Arc<AtomicBool>) -> MockServer {
    let inner = platform(agent_details(), &[]);
    serve(move |req: &Req| {
        if req
            .path
            .starts_with("/api/v2/elitea_core/resolved_version/")
        {
            entered.store(true, Ordering::SeqCst);
            tokio::task::block_in_place(|| {
                for _ in 0..250 {
                    if !hold.load(Ordering::SeqCst) {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(20));
                }
            });
        }
        inner(req)
    })
    .await
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_workspace_removed_or_rebound_while_a_turn_prepares_is_not_used() {
    for rebind in [false, true] {
        let hold = Arc::new(AtomicBool::new(true));
        let entered = Arc::new(AtomicBool::new(false));
        let h = harness(
            holding_resolve(hold.clone(), entered.clone()).await,
            allowed(),
            UiDecision::AllowOnce,
        )
        .await;
        let starting = tokio::spawn({
            let host = h.host.clone();
            let request = request(&h.workspace_id);
            async move { host.start(request).await }
        });
        // The start is reading the version: it holds no claim yet.
        for _ in 0..250 {
            if entered.load(Ordering::SeqCst) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(
            entered.load(Ordering::SeqCst),
            "the start reads the version"
        );
        if rebind {
            h.host.bind_project(&h.workspace_id, 2).unwrap();
        } else {
            h.host.remove_workspace(&h.workspace_id).unwrap();
        }
        hold.store(false, Ordering::SeqCst);
        let error = starting.await.unwrap().unwrap_err();
        let expected = if rebind {
            "workspace_project_mismatch"
        } else {
            "workspace_unknown"
        };
        assert_eq!(error.code, expected);
        assert!(
            seen(
                &h.server,
                "/local_turn/prompt_lib/1/99999999-2222-4333-8444-555555555555"
            )
            .is_empty(),
            "no execution was opened for a workspace that changed"
        );
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_turn_past_its_run_is_not_cancellable() {
    // The commit is held: the run has ended, the turn is being saved.
    let hold = Arc::new(AtomicBool::new(true));
    let entered = Arc::new(AtomicBool::new(false));
    let server = serve({
        let inner = platform(agent_details(), &[]);
        let (hold, entered) = (hold.clone(), entered.clone());
        move |req: &Req| {
            if req.path.contains("/local_turn_commit/") {
                entered.store(true, Ordering::SeqCst);
                tokio::task::block_in_place(|| {
                    for _ in 0..250 {
                        if !hold.load(Ordering::SeqCst) {
                            break;
                        }
                        std::thread::sleep(Duration::from_millis(20));
                    }
                });
            }
            inner(req)
        }
    })
    .await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    for _ in 0..500 {
        if entered.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(
        entered.load(Ordering::SeqCst),
        "the turn reached its commit"
    );
    let refused = h.host.cancel(&started.turn_id).unwrap_err();
    assert_eq!(refused.code, "turn_not_cancellable");
    let status = h.host.status(&started.turn_id).unwrap();
    assert_eq!((status.state, status.done), ("committing", None));
    hold.store(false, Ordering::SeqCst);
    let events = until_done(&h.emitter).await;
    // The refusal was the truth: the turn was committed.
    let done = events.last().unwrap();
    assert_eq!(done.payload["committed"], true);
    // A UI that missed `done` gets the same payload from the status.
    let status = h.host.status(&started.turn_id).unwrap();
    assert_eq!(status.state, "done");
    assert_eq!(status.done.as_ref(), Some(&done.payload));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_turn_never_finishes_under_another_session() {
    for signed_out in [false, true] {
        let slow = Arc::new(AtomicBool::new(true));
        let h = harness(
            stalling_platform(slow.clone()).await,
            allowed(),
            UiDecision::AllowOnce,
        )
        .await;
        let started = h.host.start(request(&h.workspace_id)).await.unwrap();
        tokio::time::sleep(Duration::from_millis(100)).await;
        // Another account signs in (or no one is signed in) while it runs.
        h.credentials
            .switch_to((!signed_out).then_some("https://other.example#2"));
        slow.store(false, Ordering::SeqCst);
        until_done_of(&h.emitter, &started.turn_id).await;
        let events = h.emitter.all();
        assert!(
            events
                .iter()
                .any(|e| e.kind == "error" && e.payload["code"] == "identity_changed"),
            "{:?}",
            h.emitter.kinds()
        );
        assert_eq!(events.last().unwrap().payload["committed"], false);
        assert!(
            seen(
                &h.server,
                &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}")
            )
            .is_empty(),
            "nothing is committed under another session"
        );
        // And a new turn is refused before any request while signed out.
        if signed_out {
            let before = h.server.seen().len();
            let error = h.host.start(request(&h.workspace_id)).await.unwrap_err();
            assert_eq!(error.code, "not_signed_in");
            assert_eq!(h.server.seen().len(), before);
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn signing_out_cancels_running_turns_and_forgets_kept_ones() {
    let slow = Arc::new(AtomicBool::new(false));
    let h = harness(
        stalling_platform(slow.clone()).await,
        allowed(),
        UiDecision::AllowOnce,
    )
    .await;
    let finished = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h.emitter, &finished.turn_id).await;
    slow.store(true, Ordering::SeqCst);
    let running = h.host.start(request(&h.workspace_id)).await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;

    h.host.forget_identity();
    slow.store(false, Ordering::SeqCst);
    until_done_of(&h.emitter, &running.turn_id).await;
    let done = h
        .emitter
        .all()
        .into_iter()
        .rfind(|e| e.kind == "done" && e.turn_id == running.turn_id)
        .unwrap();
    assert_eq!(done.payload["committed"], false);
    assert_eq!(
        seen(
            &h.server,
            &format!("/local_turn_commit/prompt_lib/1/{EXECUTION}")
        )
        .len(),
        1,
        "only the turn that finished before the sign-out was committed"
    );
    // Neither turn is kept: no review or undo of the last session's work.
    for turn in [&finished.turn_id, &running.turn_id] {
        assert_eq!(h.host.changes(turn).unwrap_err().code, "turn_unknown");
    }
}

#[tokio::test]
async fn a_finished_turn_is_in_its_threads_history_as_it_was_emitted() {
    let server = serve(platform(agent_details(), &[])).await;
    let h = harness(server, allowed(), UiDecision::AllowOnce).await;
    // A refused start is not history.
    let mut refused = request(&h.workspace_id);
    refused.prompt = "  ".into();
    assert!(h.host.start(refused).await.is_err());

    let mut asked = request(&h.workspace_id);
    std::fs::write(h.folder.path().join("README.md"), "x").unwrap();
    asked.mentions = vec!["README.md".into()];
    let started = h.host.start(asked).await.unwrap();
    let emitted: Vec<AgentEvent> = until_done(&h.emitter)
        .await
        .into_iter()
        .filter(|e| e.turn_id == started.turn_id)
        .collect();

    let turns = h.host.thread_history(&h.workspace_id, "42").await.unwrap();
    assert_eq!(turns.len(), 1, "only the turn that started");
    let turn = &turns[0];
    assert_eq!(turn.turn_id, started.turn_id);
    assert_eq!(turn.prompt, "Write notes and file a bug", "as typed");
    assert_eq!(turn.mentions, ["README.md"]);
    assert_eq!(
        turn.conversation_uuid.as_deref(),
        Some("99999999-2222-4333-8444-555555555555")
    );
    assert_eq!(turn.state, "done");
    assert!(turn.live, "the host still keeps it for review and undo");
    assert_eq!(turn.changes.as_ref().unwrap()[0]["path"], "notes.txt");
    // Every event is stored in order, the text deltas merged into one row.
    let stored: Vec<(&str, u64)> = turn
        .events
        .iter()
        .map(|e| (e.kind.as_str(), e.seq))
        .collect();
    let mut expected: Vec<(&str, u64)> = Vec::new();
    for event in &emitted {
        if event.kind == "text_delta" && expected.last().is_some_and(|(k, _)| *k == "text_delta") {
            expected.pop();
        }
        expected.push((event.kind, event.seq));
    }
    assert_eq!(stored, expected);
    let text: String = turn
        .events
        .iter()
        .filter(|e| e.kind == "text_delta")
        .map(|e| e.payload["text"].as_str().unwrap())
        .collect();
    assert_eq!(text, "Done.");
    // The thread is found by its UUID too; another thread is empty.
    assert_eq!(
        h.host
            .thread_history(&h.workspace_id, "99999999-2222-4333-8444-555555555555")
            .await
            .unwrap()
            .len(),
        1
    );
    assert!(
        h.host
            .thread_history(&h.workspace_id, "43")
            .await
            .unwrap()
            .is_empty()
    );
    // The user was looked up once for the session; a new sign-in asks again.
    assert_eq!(seen(&h.server, "/social/author").len(), 1);
    h.host.forget_identity();
    let after = h.host.thread_history(&h.workspace_id, "42").await.unwrap();
    assert_eq!(after.len(), 1, "sign-out keeps the history (keyed by user)");
    assert!(!after[0].live, "but the turn is no longer reviewable");
    assert_eq!(seen(&h.server, "/social/author").len(), 2);

    assert_eq!(
        h.host
            .delete_thread_history(&h.workspace_id, "42")
            .await
            .unwrap(),
        1
    );
    assert!(
        h.host
            .thread_history(&h.workspace_id, "42")
            .await
            .unwrap()
            .is_empty()
    );
}

#[tokio::test]
async fn removing_a_workspace_forgets_its_threads() {
    let h = harness(answering_platform().await, allowed(), UiDecision::AllowOnce).await;
    let started = h.host.start(request(&h.workspace_id)).await.unwrap();
    until_done_of(&h.emitter, &started.turn_id).await;
    assert_eq!(
        h.host
            .thread_history(&h.workspace_id, "42")
            .await
            .unwrap()
            .len(),
        1
    );
    h.host.remove_workspace(&h.workspace_id).unwrap();
    assert!(
        h.host
            .thread_history(&h.workspace_id, "42")
            .await
            .unwrap()
            .is_empty()
    );
}
