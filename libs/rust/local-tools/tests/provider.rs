//! The local tools end to end through the runtime's interfaces: the
//! `ToolProvider` hands over an adk `Toolset`, its tools execute with an adk
//! `ToolContext`, approvals go through the `ApprovalChannel`, and a turn's
//! first change takes a checkpoint the person can restore.

use std::process::Command;
use std::sync::{Arc, Mutex};

use adk_core::{CallbackContext, Content, EventActions, ReadonlyContext, Tool, ToolContext};
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, ToolProvider, ToolsetRequest,
};
use elitea_local_tools::approvals::{MemoryChoices, WorkspaceSettings};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::provider::LocalToolProvider;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

struct Context {
    content: Content,
    actions: Mutex<EventActions>,
}

impl Context {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            content: Content::new("user"),
            actions: Mutex::new(EventActions::default()),
        })
    }
}

#[async_trait]
impl ReadonlyContext for Context {
    fn invocation_id(&self) -> &'static str {
        "invocation"
    }
    fn agent_name(&self) -> &'static str {
        "agent"
    }
    fn user_id(&self) -> &'static str {
        "user"
    }
    fn app_name(&self) -> &'static str {
        "desktop"
    }
    fn session_id(&self) -> &'static str {
        "session"
    }
    fn branch(&self) -> &'static str {
        ""
    }
    fn user_content(&self) -> &Content {
        &self.content
    }
}

#[async_trait]
impl CallbackContext for Context {
    fn artifacts(&self) -> Option<Arc<dyn adk_core::Artifacts>> {
        None
    }
}

#[async_trait]
impl ToolContext for Context {
    fn function_call_id(&self) -> &'static str {
        "call-1"
    }
    fn actions(&self) -> EventActions {
        self.actions.lock().expect("lock").clone()
    }
    fn set_actions(&self, actions: EventActions) {
        *self.actions.lock().expect("lock") = actions;
    }
    async fn search_memory(&self, _query: &str) -> adk_core::Result<Vec<adk_core::MemoryEntry>> {
        Ok(Vec::new())
    }
}

/// The person: answers every question with `answer`, and counts them.
struct Person {
    answer: Mutex<ApprovalOutcome>,
    asked: Mutex<Vec<ApprovalRequest>>,
}

impl Person {
    fn approving() -> Arc<Self> {
        Arc::new(Self {
            answer: Mutex::new(ApprovalOutcome::Decided {
                action: "approve".to_owned(),
                value: Value::Null,
            }),
            asked: Mutex::new(Vec::new()),
        })
    }

    fn asked(&self) -> usize {
        self.asked.lock().expect("lock").len()
    }

    fn answer(&self, outcome: ApprovalOutcome) {
        *self.answer.lock().expect("lock") = outcome;
    }
}

#[async_trait]
impl ApprovalChannel for Person {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.asked.lock().expect("lock").push(request);
        Ok(self.answer.lock().expect("lock").clone())
    }
}

fn git(dir: &std::path::Path, args: &[&str]) {
    let status = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "user.name=T",
            "-c",
            "user.email=t@example.com",
            "-c",
            "commit.gpgsign=false",
        ])
        .args(args)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("HOME", dir)
        .status()
        .expect("git");
    assert!(status.success(), "git {args:?}");
}

struct Fixture {
    dir: tempfile::TempDir,
    person: Arc<Person>,
    session: Arc<LocalSession>,
    provider: LocalToolProvider,
}

fn fixture(policy: LocalWorkPolicy) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("repo");
    std::fs::create_dir(&root).expect("repo");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("main.rs"), "fn main() {}\n").expect("seed");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    let person = Person::approving();
    let data_dir = dir.path().join("app-data");
    let mut shell = ShellConfig::new(data_dir.join("tmp"));
    // Linux CI has no sandbox helper binary; the Seatbelt tests cover
    // enforcement on macOS.
    shell.sandbox.allow_unenforced = true;
    let session = LocalSession::open(SessionConfig {
        root,
        session_id: "conv-1".to_owned(),
        policy,
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: person.clone(),
        data_dir,
        shell: Some(shell),
    })
    .expect("session");
    let provider = LocalToolProvider::new(session.clone());
    Fixture {
        dir,
        person,
        session,
        provider,
    }
}

fn open_policy() -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        shell: true,
        command_deny: vec!["rm".to_owned()],
        path_deny: vec![".env".to_owned()],
        ..LocalWorkPolicy::default()
    }
}

async fn tools(fixture: &Fixture) -> Vec<Arc<dyn Tool>> {
    let request = ToolsetRequest {
        toolkits: Vec::new(),
        context_label: "agent".to_owned(),
    };
    let toolsets = fixture.provider.toolsets(&request).await.expect("toolsets");
    assert_eq!(toolsets.len(), 1);
    assert_eq!(toolsets[0].name(), "local");
    toolsets[0].tools(Context::new()).await.expect("tools")
}

async fn call(fixture: &Fixture, name: &str, args: Value) -> Value {
    let tool = tools(fixture)
        .await
        .into_iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("{name} is not offered"));
    tool.execute(Context::new(), args).await.expect("execute")
}

fn names(tools: &[Arc<dyn Tool>]) -> Vec<String> {
    tools.iter().map(|tool| tool.name().to_owned()).collect()
}

#[tokio::test]
async fn the_toolset_follows_policy_and_plan_mode() {
    let fixture = fixture(open_policy());
    let all = names(&tools(&fixture).await);
    for expected in [
        "read_file",
        "write_file",
        "edit_file",
        "apply_patch",
        "search_files",
        "list_tree",
        "read_document",
        "run_command",
        "git_status",
        "git_diff",
        "git_log",
        "git_branches",
    ] {
        assert!(
            all.contains(&expected.to_owned()),
            "{expected} missing from {all:?}"
        );
    }
    assert!(
        tools(&fixture)
            .await
            .iter()
            .all(|tool| tool.parameters_schema().is_some())
    );

    fixture.session.set_plan_mode(true);
    let plan = names(&tools(&fixture).await);
    assert!(plan.contains(&"read_file".to_owned()));
    assert!(!plan.contains(&"write_file".to_owned()));
    assert!(!plan.contains(&"run_command".to_owned()));
    let refused = fixture
        .session
        .call("write_file", "c", json!({ "path": "x", "content": "y" }))
        .await;
    assert_eq!(refused["code"], "local_tools.denied");

    let no_shell = fixture_names(LocalWorkPolicy {
        shell: false,
        ..open_policy()
    })
    .await;
    assert!(!no_shell.contains(&"run_command".to_owned()));
    let off = fixture_names(LocalWorkPolicy::default()).await;
    assert!(off.is_empty(), "local work off: no tools at all");
}

async fn fixture_names(policy: LocalWorkPolicy) -> Vec<String> {
    names(&tools(&fixture(policy)).await)
}

#[tokio::test]
async fn a_turn_reads_asks_checkpoints_changes_and_can_be_undone() {
    let fixture = fixture(open_policy());
    let root = fixture.session.workspace().root().to_path_buf();
    fixture.session.begin_turn("make main print");

    let unread = call(
        &fixture,
        "edit_file",
        json!({ "path": "main.rs", "old_string": "{}", "new_string": "{ println!(\"hi\"); }" }),
    )
    .await;
    assert_eq!(unread["code"], "local_tools.stale_read");
    assert_eq!(
        fixture.person.asked(),
        0,
        "a write that would be refused is never asked about"
    );

    let read = call(&fixture, "read_file", json!({ "path": "main.rs" })).await;
    assert_eq!(read["status"], "ok");
    assert_eq!(fixture.person.asked(), 0, "reads are allowed by default");

    let edited = call(
        &fixture,
        "edit_file",
        json!({ "path": "main.rs", "old_string": "{}", "new_string": "{ println!(\"hi\"); }" }),
    )
    .await;
    assert_eq!(edited["status"], "ok", "{edited}");
    assert_eq!(fixture.person.asked(), 1);
    assert_eq!(fixture.session.turn_checkpoint(), Some(1));
    let created = call(
        &fixture,
        "write_file",
        json!({ "path": "notes.md", "content": "todo\n" }),
    )
    .await;
    assert_eq!(created["status"], "ok");
    assert_eq!(
        fixture.session.list_checkpoints().expect("list").len(),
        1,
        "one checkpoint per turn"
    );

    let diff = call(&fixture, "git_diff", json!({})).await;
    assert!(diff["output"].as_str().expect("diff").contains("println"));

    let report = fixture.session.restore_checkpoint(1).expect("restore");
    assert_eq!(report.deleted, ["notes.md"]);
    assert_eq!(
        std::fs::read_to_string(root.join("main.rs")).expect("main"),
        "fn main() {}\n"
    );
    assert!(!root.join("notes.md").exists());
    let after_restore = call(
        &fixture,
        "edit_file",
        json!({ "path": "main.rs", "old_string": "{}", "new_string": "{ }" }),
    )
    .await;
    assert_eq!(
        after_restore["code"], "local_tools.stale_read",
        "a restore drops every read stamp"
    );
    drop(fixture.dir);
}

#[tokio::test]
async fn denials_rejections_and_deferrals_reach_the_model_as_results() {
    let fixture = fixture(open_policy());
    let denied = call(
        &fixture,
        "run_command",
        json!({ "command": "rm -rf build" }),
    )
    .await;
    assert_eq!(denied["code"], "local_tools.rejected");
    assert!(
        denied["message"]
            .as_str()
            .expect("message")
            .contains("denied by policy")
    );
    assert_eq!(fixture.person.asked(), 0);

    let denied_path = call(&fixture, "read_file", json!({ "path": ".env" })).await;
    assert_eq!(denied_path["code"], "local_tools.denied");
    let escape = call(&fixture, "read_file", json!({ "path": "../../etc/passwd" })).await;
    assert_eq!(escape["code"], "local_tools.outside_workspace");

    let ran = call(
        &fixture,
        "run_command",
        json!({ "command": "git status --short" }),
    )
    .await;
    assert_eq!(ran["status"], "ok", "{ran}");
    assert_eq!(ran["exit_code"], 0);
    assert_eq!(fixture.person.asked(), 1);

    fixture.person.answer(ApprovalOutcome::Decided {
        action: "reject".to_owned(),
        value: json!({ "reason": "not now" }),
    });
    let rejected = call(
        &fixture,
        "write_file",
        json!({ "path": "x.txt", "content": "x" }),
    )
    .await;
    assert_eq!(rejected["code"], "local_tools.rejected");
    assert!(!fixture.session.workspace().root().join("x.txt").exists());

    fixture.person.answer(ApprovalOutcome::Deferred);
    let pending = call(
        &fixture,
        "write_file",
        json!({ "path": "x.txt", "content": "x" }),
    )
    .await;
    assert_eq!(pending["code"], "local_tools.approval_pending");
    assert!(!fixture.session.workspace().root().join("x.txt").exists());
}

#[tokio::test]
async fn the_session_channel_is_the_hosts_approval_channel() {
    let fixture = fixture(open_policy());
    let channel = fixture.session.approvals();
    let hitl = ApprovalRequest {
        subject: "node-1".to_owned(),
        message: "approve the plan?".to_owned(),
        available_actions: vec!["approve".to_owned(), "reject".to_owned()],
        payload: json!({ "node": "review" }),
    };
    let outcome = channel.request(hitl).await.expect("hitl");
    assert!(matches!(outcome, ApprovalOutcome::Decided { ref action, .. } if action == "approve"));
    assert_eq!(
        fixture.person.asked(),
        1,
        "HITL requests go straight to the person"
    );
}
