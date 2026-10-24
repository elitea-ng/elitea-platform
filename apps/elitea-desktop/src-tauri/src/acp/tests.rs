//! ACP mode against an in-process client (a duplex pipe) and the mock
//! platform (`testutil::serve`): the hand-written types against the
//! schema's own examples, the framing's bounds, and whole prompt turns on
//! the real agent host.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _, BufReader};

use super::agent::{self, AcpConfig, Defaults, EventRouter, Mapper};
use super::rpc::{self, Incoming, Line};
use super::types::{self, ToolKind};
use crate::d0::api::{ApiError, Bearer, Credentials, PlatformApi};
use crate::d0::remote_tools::RetryPolicy;
use crate::d0::turn::{AgentHost, HostDeps, PolicySource};
use crate::testutil::{MockServer, Req, Res, serve};
use crate::workspaces::WorkspaceStore;

const UUID: &str = "99999999-2222-4333-8444-555555555555";
const EXECUTION: &str = "0123456789abcdef0123456789abcdef";
const TOOLKIT_REF: &str = "tkr1_0123456789abcdef0123456789abcdef";

// ---- stage 1: types and framing ------------------------------------------

fn round_trip<T: DeserializeOwned + Serialize>(value: &Value) -> Value {
    let typed: T =
        serde_json::from_value(value.clone()).unwrap_or_else(|error| panic!("{error}: {value}"));
    serde_json::to_value(&typed).unwrap()
}

#[test]
fn types_round_trip_schema_examples() {
    let fixture: Value =
        serde_json::from_str(include_str!("fixtures/schema-v1-examples.json")).unwrap();
    // The fixture and the types were written from the same schema commit.
    assert_eq!(fixture["commit"], types::SCHEMA_COMMIT);
    let examples = fixture["examples"].as_array().unwrap();
    assert!(examples.len() > 30);
    for example in examples {
        let value = &example["value"];
        let back = match example["type"].as_str().unwrap() {
            "InitializeRequest" => round_trip::<types::InitializeRequest>(value),
            "InitializeResponse" => round_trip::<types::InitializeResponse>(value),
            "NewSessionRequest" => round_trip::<types::NewSessionRequest>(value),
            "NewSessionResponse" => round_trip::<types::NewSessionResponse>(value),
            "LoadSessionRequest" => round_trip::<types::LoadSessionRequest>(value),
            "LoadSessionResponse" => round_trip::<types::LoadSessionResponse>(value),
            "McpServer" => round_trip::<types::McpServer>(value),
            "SessionNotification" => round_trip::<types::SessionNotification>(value),
            "PromptRequest" => round_trip::<types::PromptRequest>(value),
            "PromptResponse" => round_trip::<types::PromptResponse>(value),
            "CancelNotification" => round_trip::<types::CancelNotification>(value),
            "SetSessionModeRequest" => round_trip::<types::SetSessionModeRequest>(value),
            "RequestPermissionRequest" => round_trip::<types::RequestPermissionRequest>(value),
            "RequestPermissionResponse" => round_trip::<types::RequestPermissionResponse>(value),
            "ContentBlock" => round_trip::<types::ContentBlock>(value),
            "ToolCallContent" => round_trip::<types::ToolCallContent>(value),
            "ToolCallLocation" => round_trip::<types::ToolCallLocation>(value),
            other => panic!("no type for {other}"),
        };
        assert_eq!(&back, value, "{}", example["source"]);
    }
}

#[test]
fn inbound_types_are_strict() {
    // A required field missing, a closed enum widened, a wrong type: refused.
    for (bad, why) in [
        (json!({"sessionId": "s"}), "prompt without blocks"),
        (
            json!({"sessionId": "s", "prompt": [{"type": "video", "data": "x"}]}),
            "unknown block",
        ),
        (
            json!({"sessionId": 7, "prompt": []}),
            "a numeric session id",
        ),
    ] {
        assert!(
            serde_json::from_value::<types::PromptRequest>(bad).is_err(),
            "{why}"
        );
    }
    assert!(
        serde_json::from_value::<types::RequestPermissionResponse>(
            json!({"outcome": {"outcome": "approved_everything"}})
        )
        .is_err()
    );
    // An MCP server of an unknown transport is not one.
    assert!(
        serde_json::from_value::<types::McpServer>(
            json!({"type": "websocket", "name": "x", "url": "wss://x", "headers": []})
        )
        .is_err()
    );
    // `_meta` and fields a newer client adds are ignored, as the schema allows.
    let request: types::InitializeRequest = serde_json::from_value(json!({
        "protocolVersion": 1, "_meta": {"x": 1},
        "clientCapabilities": {"fs": {"readTextFile": true}, "terminal": false, "auth": {"terminal": true}}
    }))
    .unwrap();
    assert!(request.client_capabilities.fs.read_text_file);
}

#[test]
fn messages_are_classified_and_malformed_ones_refused() {
    assert_eq!(
        rpc::parse(br#"{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/"}}"#),
        Ok(Incoming::Request {
            id: rpc::Id::Number(3),
            method: "session/new".into(),
            params: json!({"cwd": "/"}),
        })
    );
    assert!(matches!(
        rpc::parse(br#"{"jsonrpc":"2.0","method":"session/cancel","params":{}}"#),
        Ok(Incoming::Notification { .. })
    ));
    // `result: null` is an answer, not a missing one.
    assert_eq!(
        rpc::parse(br#"{"jsonrpc":"2.0","id":"a","result":null}"#),
        Ok(Incoming::Response {
            id: rpc::Id::Text("a".into()),
            result: Ok(Value::Null),
        })
    );
    for (line, code) in [
        (&b"not json"[..], rpc::PARSE_ERROR),
        (
            br#"{"jsonrpc":"1.0","id":1,"method":"x"}"#,
            rpc::INVALID_REQUEST,
        ),
        (
            br#"{"jsonrpc":"2.0","id":{"a":1},"method":"x"}"#,
            rpc::INVALID_REQUEST,
        ),
        (br#"{"jsonrpc":"2.0","id":1}"#, rpc::INVALID_REQUEST),
        (
            br#"{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":"m"}}"#,
            rpc::INVALID_REQUEST,
        ),
        (
            br#"{"jsonrpc":"2.0","id":1,"method":"x","result":{}}"#,
            rpc::INVALID_REQUEST,
        ),
    ] {
        assert_eq!(rpc::parse(line).unwrap_err().error.code, code, "{line:?}");
    }
    let long = format!(
        r#"{{"jsonrpc":"2.0","id":1,"method":"{}"}}"#,
        "m".repeat(200)
    );
    assert_eq!(
        rpc::parse(long.as_bytes()).unwrap_err().error.code,
        rpc::INVALID_REQUEST
    );
}

#[tokio::test]
async fn an_oversized_line_is_skipped_without_being_kept() {
    let mut input = Vec::new();
    input.extend_from_slice(&[b'x'; 100]);
    input.push(b'\n');
    input.extend_from_slice(b"{\"a\":1}\n{\"b\":2}");
    let mut reader = &input[..];
    assert_eq!(
        rpc::read_line(&mut reader, 64).await.unwrap(),
        Line::TooLong
    );
    assert_eq!(
        rpc::read_line(&mut reader, 64).await.unwrap(),
        Line::Message(b"{\"a\":1}".to_vec())
    );
    // A last line without its newline still counts; then the input ends.
    assert_eq!(
        rpc::read_line(&mut reader, 64).await.unwrap(),
        Line::Message(b"{\"b\":2}".to_vec())
    );
    assert_eq!(rpc::read_line(&mut reader, 64).await.unwrap(), Line::Eof);
}

#[test]
fn tool_kinds_and_plans_map_to_acp() {
    assert_eq!(agent::tool_kind("read_file", false), ToolKind::Read);
    assert_eq!(agent::tool_kind("apply_patch", false), ToolKind::Edit);
    assert_eq!(agent::tool_kind("run_command", false), ToolKind::Execute);
    assert_eq!(agent::tool_kind("search_files", false), ToolKind::Search);
    assert_eq!(
        agent::tool_kind("exit_plan_mode", false),
        ToolKind::SwitchMode
    );
    assert_eq!(agent::tool_kind("Jira_create_issue", true), ToolKind::Fetch);
    assert_eq!(agent::tool_kind("anything_else", false), ToolKind::Other);
    let plan = agent::plan_of(Some(
        &json!({"title": "t", "status": "in_progress", "steps": [
            {"n": 1, "step": "one", "status": "completed"},
            {"n": 2, "step": "two", "status": "cancelled"},
            {"n": 3, "step": "three", "status": "in_progress"},
        ]}),
    ));
    let entries: Vec<_> = plan.entries.iter().map(|e| e.content.as_str()).collect();
    assert_eq!(entries, ["one", "three"]);
    assert!(agent::plan_of(None).entries.is_empty());
    // A link outside the workspace is not a mention.
    let root = std::path::Path::new("/w/repo");
    assert_eq!(
        agent::workspace_relative("file:///w/repo/src/main.rs#L3", root, root).as_deref(),
        Some("src/main.rs")
    );
    assert_eq!(
        agent::workspace_relative("file:///etc/passwd", root, root),
        None
    );
    assert_eq!(
        agent::workspace_relative("https://x/w/repo/a", root, root),
        None
    );
    assert_eq!(
        agent::workspace_relative("file:///w/repo", root, root),
        None
    );
}

#[test]
fn a_failed_turn_keeps_its_code() {
    let mut mapper = Mapper::default();
    mapper.map(
        "error",
        &json!({"code": "model_gateway.forbidden", "message": "no"}),
    );
    mapper.map("status", &json!({"phase": "error"}));
    assert_eq!(
        mapper.end,
        agent::End::Failed {
            code: "model_gateway.forbidden".into(),
            message: "no".into()
        }
    );
}

#[test]
fn defaults_come_from_the_environment_and_are_checked() {
    let env = |pairs: &'static [(&'static str, &'static str)]| {
        move |name: &str| {
            pairs
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value).to_owned())
        }
    };
    assert_eq!(
        Defaults::from_env(env(&[
            ("ELITEA_PROJECT_ID", "3"),
            ("ELITEA_AGENT_ID", " 5 ")
        ])),
        Ok(Defaults {
            project_id: Some(3),
            application_id: Some(5),
            version_id: None
        })
    );
    for bad in ["0", "-1", "abc", "2147483648"] {
        let pairs: &'static [(&'static str, &'static str)] =
            Box::leak(Box::new([("ELITEA_AGENT_VERSION_ID", bad)]));
        assert!(Defaults::from_env(env(pairs)).is_err(), "{bad}");
    }
}

#[test]
fn the_identifier_is_the_bundles() {
    let config: Value = serde_json::from_str(include_str!("../../tauri.conf.json")).unwrap();
    assert_eq!(config["identifier"], super::APP_IDENTIFIER);
}

// ---- stage 2: the agent on the mock platform ------------------------------

struct StaticCredentials(String);

#[async_trait]
impl Credentials for StaticCredentials {
    async fn bearer(&self) -> Result<Bearer, ApiError> {
        Ok(Bearer {
            origin: self.0.clone(),
            token: "elnat_test_token".into(),
        })
    }

    async fn refreshed(&self) -> Result<Bearer, ApiError> {
        self.bearer().await
    }

    fn identity(&self) -> Option<String> {
        Some(format!("{}#1", self.0))
    }
}

struct Policy(Value);

impl PolicySource for Policy {
    fn local_work(&self) -> Option<Value> {
        Some(self.0.clone())
    }
}

fn allowed() -> Value {
    json!({"allowed": true, "shell": true, "max_sandbox_mode": "workspace-write"})
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
        raw: Vec::new(),
    }
}

fn tool_call(id: &str, name: &str, args: &Value) -> Res {
    crate::d0::tests::tool_call(id, name, args)
}

fn answer(text: &str) -> Res {
    sse(&[
        json!({"choices": [{"delta": {"content": text}}]}),
        json!({"choices": [{"delta": {}, "finish_reason": "stop"}]}),
    ])
}

type Model = Box<dyn Fn(usize) -> Res + Send + Sync>;

/// The platform: agent 5 version 9 of project 1 (with a Jira toolkit whose
/// call asks for confirmation), a new conversation [`UUID`], and `model`
/// answering the n-th model call.
async fn platform(model: Model) -> MockServer {
    let model_calls = AtomicUsize::new(0);
    serve(move |req: &Req| {
        let path = req.path.as_str();
        let ok = |body: Value| Res::json(200, &body);
        if path == "/api/v2/social/author" {
            return ok(json!({"id": 1, "name": "Me"}));
        }
        if path.starts_with("/api/v2/configurations/models/1?") {
            return ok(crate::d0::tests::catalogue(200_000));
        }
        if path == "/api/v2/elitea_core/default_version/prompt_lib/1/5" {
            return ok(json!({"id": 9, "name": "base"}));
        }
        if path == "/api/v2/elitea_core/application/prompt_lib/1/5" {
            return ok(json!({"id": 5, "name": "Coder", "versions": [{"id": 9, "agent_type": "openai"}]}));
        }
        if path == "/api/v2/elitea_core/conversations/prompt_lib/1" && req.method == "POST" {
            return ok(json!({"id": 42, "uuid": UUID, "source": "local_work"}));
        }
        if path == format!("/api/v2/elitea_core/participants/prompt_lib/1/{UUID}") {
            return ok(json!([]));
        }
        if path == "/api/v2/elitea_core/resolved_version/prompt_lib/1/5/9" {
            return ok(json!({
                "schema_version": "elitea.client.resolved-application-version.v1",
                "project_id": 1, "application_id": 5, "version_id": 9,
                "definition_sha256": "00", "withheld_secrets": [],
                "project_context_withheld": false,
                "version_details": {
                    "instructions": "Be brief.", "agent_type": "openai",
                    "llm_settings": {"model_name": "gpt-test", "max_tokens": 256},
                    "tools": [{
                        "kind": "remote_toolkit", "type": "jira", "toolkit_name": "Jira",
                        "selected_tools": ["create_issue"], "all_tools": false,
                        "toolkit_ref": {"toolkit_id": 3, "project_id": 1, "ref": TOOLKIT_REF}
                    }]
                },
            }));
        }
        if path.starts_with(&format!("/api/v2/elitea_core/conversation/prompt_lib/1/{UUID}")) {
            return ok(json!({"id": 42, "message_count": 0, "uuid": UUID, "participants": [
                {"id": 70, "entity_name": "user", "entity_meta": {"id": 1}},
                {"id": 77, "entity_name": "application", "entity_meta": {"id": 5, "project_id": 1},
                 "entity_settings": {"version_id": 9}}
            ]}));
        }
        if path == format!("/api/v2/elitea_core/local_turn/prompt_lib/1/{UUID}") {
            let body: Value = serde_json::from_str(&req.body).unwrap_or_default();
            return ok(json!({
                "execution_id": EXECUTION, "question_id": body["question_id"],
                "response_message_id": "11111111-2222-4333-8444-555555555555",
                "conversation_uuid": UUID, "participant_id": 77,
                "expires_at": "2026-10-10T00:00:00Z", "created": true,
                "memory_recall": {"text": "", "count": 0, "memory_ids": []}
            }));
        }
        if path == "/llm/v1/chat/completions" {
            return model(model_calls.fetch_add(1, Ordering::SeqCst));
        }
        if path == "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/1/3" {
            let body: Value = serde_json::from_str(&req.body).unwrap_or_default();
            if body.get("confirmation").is_none() {
                return Res::json(409, &json!({
                    "ok": false, "error": "confirmation_required", "toolkit_id": 3,
                    "message": "Creates an issue in Jira",
                    "hitl_interrupt": {"interrupt_id": "hitl_fedcba9876543210fedcba9876543210",
                        "guardrail_type": "sensitive_tool", "action_label": "Create a Jira issue",
                        "policy_message": "Creates an issue in Jira",
                        "available_actions": ["approve", "reject"], "tool_name": "create_issue"}
                }));
            }
            return ok(json!({"ok": true, "status": "ok", "toolkit_id": 3,
                "tool_name": "create_issue", "result": {"key": "J-1"}}));
        }
        if path == format!("/api/v2/elitea_core/local_turn_commit/prompt_lib/1/{EXECUTION}") {
            return ok(json!({"execution_id": EXECUTION, "created": true,
                "conversation_uuid": UUID, "question_message_id": "q",
                "response_message_id": "r", "memories_used": 0,
                "committed_at": "2026-10-09T00:00:00Z"}));
        }
        Res::json(404, &json!({"error": "not_found", "message": path}))
    })
    .await
}

/// The editor's end of the pipe.
struct Client {
    writer: tokio::io::WriteHalf<tokio::io::DuplexStream>,
    lines: tokio::io::Lines<BufReader<tokio::io::ReadHalf<tokio::io::DuplexStream>>>,
    next_id: i64,
}

/// What the agent sent while a call ran: notifications and its requests.
#[derive(Default)]
struct Seen {
    updates: Vec<Value>,
    permission_requests: Vec<Value>,
}

impl Seen {
    fn updates_of(&self, kind: &str) -> Vec<&Value> {
        self.updates
            .iter()
            .filter(|update| update["sessionUpdate"] == kind)
            .collect()
    }
}

impl Client {
    async fn send(&mut self, message: &Value) {
        let mut line = serde_json::to_vec(message).unwrap();
        line.push(b'\n');
        self.writer.write_all(&line).await.unwrap();
    }

    async fn next(&mut self) -> Value {
        let line = tokio::time::timeout(Duration::from_secs(30), self.lines.next_line())
            .await
            .expect("the agent answered in time")
            .unwrap()
            .expect("the agent kept the connection");
        serde_json::from_str(&line).unwrap()
    }

    async fn notify(&mut self, method: &str, params: Value) {
        self.send(&json!({"jsonrpc": "2.0", "method": method, "params": params}))
            .await;
    }

    async fn start(&mut self, method: &str, params: Value) -> i64 {
        self.next_id += 1;
        let id = self.next_id;
        self.send(&json!({"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
            .await;
        id
    }

    /// Until the answer to `id`; `permission` answers each permission
    /// request (its params → the chosen option id, `None`: cancelled).
    async fn until(
        &mut self,
        id: i64,
        seen: &mut Seen,
        permission: &dyn Fn(&Value) -> Option<String>,
    ) -> Value {
        loop {
            let message = self.next().await;
            if message["method"] == "session/update" {
                seen.updates.push(message["params"]["update"].clone());
            } else if message["method"] == "session/request_permission" {
                let outcome = permission(&message["params"]).map_or_else(
                    || json!({"outcome": "cancelled"}),
                    |option| json!({"outcome": "selected", "optionId": option}),
                );
                seen.permission_requests.push(message["params"].clone());
                self.send(
                    &json!({"jsonrpc": "2.0", "id": message["id"], "result": {"outcome": outcome}}),
                )
                .await;
            } else if message["id"] == id {
                return message;
            }
        }
    }

    async fn call(&mut self, method: &str, params: Value) -> (Value, Seen) {
        let id = self.start(method, params).await;
        let mut seen = Seen::default();
        let answer = self.until(id, &mut seen, &|_| None).await;
        (answer, seen)
    }
}

struct Acp {
    server: MockServer,
    client: Client,
    host: Arc<AgentHost>,
    folder: tempfile::TempDir,
    _app: tempfile::TempDir,
}

async fn acp(server: MockServer, policy: Value) -> Acp {
    let app = tempfile::tempdir().unwrap();
    let folder = tempfile::tempdir().unwrap();
    for dir in ["config", "logs"] {
        std::fs::create_dir_all(app.path().join(dir)).unwrap();
    }
    let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
    let policy = Arc::new(Policy(policy));
    let credentials: Arc<dyn Credentials> = Arc::new(StaticCredentials(server.origin.clone()));
    let router = Arc::new(EventRouter::default());
    let app_dirs = crate::d0::turn::app_dirs_for_sandbox([
        Some(app.path().join("config")),
        Some(app.path().to_owned()),
        Some(app.path().join("logs")),
        None,
    ]);
    let index = Arc::new(crate::index::IndexRegistry::new(
        workspaces.clone(),
        policy.clone(),
        Arc::new(elitea_local_index::service::NoEvents),
    ));
    let host = Arc::new(
        AgentHost::new(HostDeps {
            http: crate::net::SharedHttp::new("0.1.0"),
            credentials: credentials.clone(),
            client_version: "0.1.0".into(),
            policy,
            workspaces: workspaces.clone(),
            emitter: router.clone(),
            retry: RetryPolicy {
                attempts: 3,
                delay: Duration::from_millis(10),
            },
            history: Some(Arc::new(
                crate::history::HistoryStore::open(app.path()).unwrap(),
            )),
            sessions: Some(Arc::new(
                crate::sessions::DesktopStateStore::open(app.path()).unwrap(),
            )),
            index: Some(index),
            sandbox_deny: app_dirs.clone(),
            app_id: Some("com.example.elitea-test".into()),
            user_rules_dir: Some(app.path().join("config")),
            user_commands_dir: Some(app.path().join("config/commands")),
        })
        .unwrap(),
    );
    let (client_end, agent_end) = tokio::io::duplex(1 << 20);
    let (agent_read, agent_write) = tokio::io::split(agent_end);
    tokio::spawn(agent::serve(
        AcpConfig {
            agents: host.clone(),
            workspaces,
            api: Arc::new(PlatformApi::shared(
                crate::net::SharedHttp::new("0.1.0"),
                credentials,
                "0.1.0",
            )),
            router,
            defaults: Defaults {
                project_id: Some(1),
                application_id: Some(5),
                version_id: None,
            },
            app_dirs,
            client_version: "0.1.0".into(),
        },
        agent_read,
        agent_write,
    ));
    let (read, writer) = tokio::io::split(client_end);
    Acp {
        server,
        client: Client {
            writer,
            lines: BufReader::new(read).lines(),
            next_id: 0,
        },
        host,
        folder,
        _app: app,
    }
}

impl Acp {
    async fn initialize(&mut self) -> Value {
        let (answer, _) = self
            .client
            .call(
                "initialize",
                json!({"protocolVersion": 1, "clientCapabilities": {"fs": {"readTextFile": true, "writeTextFile": true}, "terminal": true},
                       "clientInfo": {"name": "zed", "version": "1.0"}}),
            )
            .await;
        answer
    }

    /// `initialize` and `session/new` in the test's folder.
    async fn session(&mut self) -> (String, Seen) {
        self.initialize().await;
        let cwd = self.folder.path().to_string_lossy().into_owned();
        let id = self
            .client
            .start("session/new", json!({"cwd": cwd, "mcpServers": [
                {"name": "planted", "command": "/bin/sh", "args": ["-c", "touch pwned"], "env": [{"name": "TOKEN", "value": "secret"}]}
            ]}))
            .await;
        let mut seen = Seen::default();
        let answer = self.client.until(id, &mut seen, &|_| None).await;
        let session_id = answer["result"]["sessionId"]
            .as_str()
            .unwrap_or_else(|| panic!("{answer}"))
            .to_owned();
        // The commands menu follows the answer.
        let update = self.client.next().await;
        assert_eq!(
            update["params"]["update"]["sessionUpdate"],
            "available_commands_update"
        );
        assert_eq!(update["params"]["sessionId"], session_id);
        (session_id, seen)
    }

    fn seen(&self, path_end: &str) -> Vec<Req> {
        self.server
            .seen()
            .into_iter()
            .filter(|req| req.path.ends_with(path_end))
            .collect()
    }

    async fn commits_settled(&self) {
        for _ in 0..3_000 {
            if self.host.unsaved_commits().0 == 0 {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("a commit did not settle");
    }
}

fn prompt(session_id: &str, text: &str) -> Value {
    json!({"sessionId": session_id, "prompt": [{"type": "text", "text": text}]})
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn initialize_advertises_v1() {
    let mut acp = acp(platform(Box::new(|_| answer("hi"))).await, allowed()).await;
    // Nothing before `initialize`.
    let (refused, _) = acp
        .client
        .call("session/new", json!({"cwd": "/tmp", "mcpServers": []}))
        .await;
    assert_eq!(refused["error"]["code"], rpc::INVALID_REQUEST);
    let answer = acp.initialize().await;
    let result = &answer["result"];
    assert_eq!(result["protocolVersion"], 1);
    assert_eq!(result["agentCapabilities"]["loadSession"], true);
    assert_eq!(
        result["agentCapabilities"]["promptCapabilities"]["image"],
        true
    );
    assert_eq!(
        result["agentCapabilities"]["promptCapabilities"]["embeddedContext"],
        false
    );
    assert_eq!(
        result["agentCapabilities"]["mcpCapabilities"],
        json!({"http": false, "sse": false})
    );
    assert_eq!(result["authMethods"], json!([]));
    assert_eq!(result["agentInfo"]["name"], "elitea");
    // A client asking for a newer version still gets the one spoken here.
    let (newer, _) = acp
        .client
        .call("initialize", json!({"protocolVersion": 7}))
        .await;
    assert_eq!(newer["result"]["protocolVersion"], 1);
    // Unknown methods and malformed params are refused, the connection kept.
    let (unknown, _) = acp.client.call("terminal/create", json!({})).await;
    assert_eq!(unknown["error"]["code"], rpc::METHOD_NOT_FOUND);
    let (malformed, _) = acp.client.call("session/new", json!({"cwd": 3})).await;
    assert_eq!(malformed["error"]["code"], rpc::INVALID_PARAMS);
    acp.client.send(&json!("not an object")).await;
    assert_eq!(acp.client.next().await["error"]["code"], rpc::PARSE_ERROR);
    // The app's own data is never a workspace, nor is a relative path.
    for cwd in [
        acp._app
            .path()
            .join("config")
            .to_string_lossy()
            .into_owned(),
        "relative".into(),
    ] {
        let (refused, _) = acp
            .client
            .call("session/new", json!({"cwd": cwd, "mcpServers": []}))
            .await;
        assert!(refused["error"].is_object(), "{refused}");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn prompt_maps_events_to_session_updates() {
    let model: Model = Box::new(|n| match n {
        0 => tool_call(
            "call-write",
            "write_file",
            &json!({"path": "notes.txt", "content": "hello\n"}),
        ),
        _ => answer("Done."),
    });
    let mut acp = acp(platform(model).await, allowed()).await;
    let (session_id, _) = acp.session().await;
    assert_eq!(session_id, UUID);
    // The conversation was created as Local work, with the folder's name only.
    let created = &acp.seen("/elitea_core/conversations/prompt_lib/1")[0];
    let body: Value = serde_json::from_str(&created.body).unwrap();
    assert_eq!(body["source"], "local_work");
    assert!(!created.body.contains(&*acp.folder.path().to_string_lossy()));
    let participants: Value =
        serde_json::from_str(&acp.seen(&format!("/participants/prompt_lib/1/{UUID}"))[0].body)
            .unwrap();
    assert_eq!(participants[1]["entity_settings"]["version_id"], 9);
    assert_eq!(participants[1]["entity_meta"]["name"], "Coder");

    let (answer, seen) = acp
        .client
        .call("session/prompt", prompt(&session_id, "Write notes"))
        .await;
    assert_eq!(answer["result"]["stopReason"], "end_turn", "{answer}");
    let calls = seen.updates_of("tool_call");
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0]["toolCallId"], "call-write");
    assert_eq!(calls[0]["kind"], "edit");
    assert_eq!(calls[0]["status"], "in_progress");
    let results = seen.updates_of("tool_call_update");
    assert_eq!(results.len(), 1);
    assert_eq!(results[0]["status"], "completed");
    let text: String = seen
        .updates_of("agent_message_chunk")
        .iter()
        .map(|update| update["content"]["text"].as_str().unwrap())
        .collect();
    assert_eq!(text, "Done.");
    assert!(seen.permission_requests.is_empty());
    assert_eq!(
        std::fs::read_to_string(acp.folder.path().join("notes.txt")).unwrap(),
        "hello\n"
    );
    // The client's MCP server was never started.
    assert!(!acp.folder.path().join("pwned").exists());
    // The turn is committed like a desktop turn, after its answer.
    acp.commits_settled().await;
    assert_eq!(
        acp.seen(&format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"))
            .len(),
        1
    );

    // A reopened session replays this computer's turns.
    let cwd = acp.folder.path().to_string_lossy().into_owned();
    let (loaded, replay) = acp
        .client
        .call(
            "session/load",
            json!({"sessionId": UUID.to_uppercase(), "cwd": cwd, "mcpServers": []}),
        )
        .await;
    assert_eq!(
        loaded["result"]["modes"]["currentModeId"], "default",
        "{loaded}"
    );
    let user = replay.updates_of("user_message_chunk");
    assert_eq!(user[0]["content"]["text"], "Write notes");
    assert_eq!(replay.updates_of("tool_call").len(), 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn ask_becomes_request_permission_and_deny_is_final() {
    // The policy denies `secret/`; the remote toolkit asks for confirmation.
    let model: Model = Box::new(|n| match n {
        0 => tool_call(
            "call-denied",
            "write_file",
            &json!({"path": "secret/key.txt", "content": "x"}),
        ),
        1 => tool_call(
            "call-remote",
            "Jira_create_issue",
            &json!({"summary": "Bug"}),
        ),
        _ => answer("Understood."),
    });
    let mut policy = allowed();
    policy["path_deny"] = json!(["secret/**"]);
    let mut acp = acp(platform(model).await, policy).await;
    let (session_id, _) = acp.session().await;
    let id = acp
        .client
        .start("session/prompt", prompt(&session_id, "Do both"))
        .await;
    let mut seen = Seen::default();
    let answer = acp
        .client
        .until(id, &mut seen, &|_| Some("reject_once".to_owned()))
        .await;
    assert_eq!(answer["result"]["stopReason"], "end_turn", "{answer}");
    // Only the ask reached the client; the policy's deny never did.
    assert_eq!(
        seen.permission_requests.len(),
        1,
        "{:?}",
        seen.permission_requests
    );
    let asked = &seen.permission_requests[0];
    assert_eq!(asked["toolCall"]["toolCallId"], "call-remote");
    let options: Vec<&str> = asked["options"]
        .as_array()
        .unwrap()
        .iter()
        .map(|o| o["optionId"].as_str().unwrap())
        .collect();
    assert_eq!(options, ["allow_once", "reject_once"]);
    // Both calls failed; the model went on and answered.
    let results = seen.updates_of("tool_call_update");
    assert_eq!(results.len(), 2);
    assert!(results.iter().all(|update| update["status"] == "failed"));
    assert!(!acp.folder.path().join("secret").exists());
    // The rejected call was never sent confirmed.
    let remote = acp.seen("/remote_toolkit_call/prompt_lib/1/3");
    assert_eq!(remote.len(), 1);
    assert!(!seen.updates_of("agent_message_chunk").is_empty());

    // An answer naming an option that was not offered is a rejection too.
    let id = acp
        .client
        .start("session/prompt", prompt(&session_id, "Again"))
        .await;
    let mut seen = Seen::default();
    acp.client
        .until(id, &mut seen, &|_| Some("allow_always".to_owned()))
        .await;
    let remote = acp.seen("/remote_toolkit_call/prompt_lib/1/3");
    assert!(
        remote.iter().all(|req| !req.body.contains("confirmation")),
        "{remote:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn plan_mode_is_selectable_and_offers_no_writes() {
    let model: Model = Box::new(|n| match n {
        0 => tool_call(
            "call-write",
            "write_file",
            &json!({"path": "notes.txt", "content": "x"}),
        ),
        _ => answer("Here is the plan."),
    });
    let mut acp = acp(platform(model).await, allowed()).await;
    let (session_id, _) = acp.session().await;
    let (refused, _) = acp
        .client
        .call(
            "session/set_mode",
            json!({"sessionId": session_id, "modeId": "yolo"}),
        )
        .await;
    assert_eq!(refused["error"]["code"], rpc::INVALID_PARAMS);
    let (set, _) = acp
        .client
        .call(
            "session/set_mode",
            json!({"sessionId": session_id, "modeId": "plan"}),
        )
        .await;
    assert_eq!(set["result"], json!({}));
    let (answer, seen) = acp
        .client
        .call("session/prompt", prompt(&session_id, "Plan it"))
        .await;
    assert_eq!(answer["result"]["stopReason"], "end_turn", "{answer}");
    assert!(seen.permission_requests.is_empty());
    assert!(!acp.folder.path().join("notes.txt").exists());
    let first: Value = serde_json::from_str(&acp.seen("/llm/v1/chat/completions")[0].body).unwrap();
    let tools: Vec<&str> = first["tools"]
        .as_array()
        .unwrap()
        .iter()
        .filter_map(|tool| tool["function"]["name"].as_str())
        .collect();
    assert!(
        tools.contains(&"read_file") && tools.contains(&"exit_plan_mode"),
        "{tools:?}"
    );
    assert!(
        !tools.contains(&"write_file") && !tools.contains(&"run_command"),
        "{tools:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn cancel_commits_nothing() {
    let slow = Arc::new(AtomicBool::new(true));
    // Set as the model is asked (the mock logs a request only once answered).
    let asked = Arc::new(AtomicBool::new(false));
    let model: Model = {
        let (slow, asked) = (slow.clone(), asked.clone());
        Box::new(move |_| {
            asked.store(true, Ordering::SeqCst);
            tokio::task::block_in_place(|| {
                for _ in 0..250 {
                    if !slow.load(Ordering::SeqCst) {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(20));
                }
            });
            answer("too late")
        })
    };
    let mut acp = acp(platform(model).await, allowed()).await;
    let (session_id, _) = acp.session().await;
    let id = acp
        .client
        .start("session/prompt", prompt(&session_id, "Long work"))
        .await;
    for _ in 0..3_000 {
        if asked.load(Ordering::SeqCst) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    // A second prompt on the session while one runs is refused.
    let (busy, _) = acp
        .client
        .call("session/prompt", prompt(&session_id, "More"))
        .await;
    assert_eq!(busy["error"]["data"]["code"], "busy", "{busy}");
    acp.client
        .notify("session/cancel", json!({"sessionId": session_id}))
        .await;
    // The model's answer may still arrive: the turn stays cancelled.
    slow.store(false, Ordering::SeqCst);
    let mut seen = Seen::default();
    let answer = acp.client.until(id, &mut seen, &|_| None).await;
    assert_eq!(answer["result"]["stopReason"], "cancelled", "{answer}");
    acp.commits_settled().await;
    assert!(
        acp.seen(&format!("/local_turn_commit/prompt_lib/1/{EXECUTION}"))
            .is_empty()
    );
    // The session is free for the next prompt.
    let (next, _) = acp
        .client
        .call("session/prompt", prompt(&session_id, "Again"))
        .await;
    assert_eq!(next["result"]["stopReason"], "end_turn", "{next}");
}

/// Every event the router parks is bounded: a start refused before any
/// prompt asked for its events does not grow memory without end.
#[test]
fn parked_events_are_bounded() {
    use crate::d0::events::{AgentEvent, EventEmitter as _};
    let router = EventRouter::default();
    for turn in 0..100 {
        for seq in 0..10 {
            router.emit(AgentEvent {
                turn_id: format!("t{turn}"),
                seq,
                kind: "status",
                payload: json!({}),
            });
        }
    }
    let mut sizes = Vec::new();
    for turn in [0, 99] {
        let mut events = router.route(&format!("t{turn}"));
        let mut count = 0;
        while events.try_recv().is_ok() {
            count += 1;
        }
        sizes.push(count);
    }
    // The oldest turns were dropped; the newest kept whole.
    assert_eq!(sizes, [0, 10]);
}
