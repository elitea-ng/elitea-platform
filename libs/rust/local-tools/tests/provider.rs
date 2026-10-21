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
use elitea_local_tools::approvals::{MemoryChoices, PermissionProfile, WorkspaceSettings};
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
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
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
        "multi_edit",
        "apply_patch",
        "search_files",
        "find_files",
        "read_output",
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

    fixture.session.set_profile(PermissionProfile::Plan);
    let plan = names(&tools(&fixture).await);
    assert!(plan.contains(&"read_file".to_owned()));
    assert!(!plan.contains(&"write_file".to_owned()));
    assert!(!plan.contains(&"run_command".to_owned()));
    // No commands in plan mode: `run_tests` goes with `run_command`, while
    // `read_output` (read-only) stays.
    assert!(!plan.contains(&"run_tests".to_owned()));
    assert!(plan.contains(&"read_output".to_owned()));
    assert_eq!(plan.last().map(String::as_str), Some("exit_plan_mode"));
    assert!(
        !all.contains(&"exit_plan_mode".to_owned()),
        "plan mode only"
    );
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
async fn a_turn_reads_checkpoints_changes_and_can_be_undone() {
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
    assert_eq!(
        fixture.person.asked(),
        0,
        "in-workspace edits are allowed by default; the checkpoint undoes them"
    );
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

/// `multi_edit` and a Codex envelope run through the provider with the
/// edit rows' defaults: allowed in the workspace, a `path_deny` file refused.
#[tokio::test]
async fn multi_edit_and_codex_envelopes_run_through_the_provider() {
    let fixture = fixture(open_policy());
    let root = fixture.session.workspace().root().to_path_buf();
    std::fs::write(root.join(".env"), "KEY=1\n").expect("env");
    fixture.session.begin_turn("edits");
    call(&fixture, "read_file", json!({ "path": "main.rs" })).await;
    let edited = call(
        &fixture,
        "multi_edit",
        json!({ "path": "main.rs", "edits": [
            { "old_string": "main", "new_string": "start" },
            { "old_string": "{}", "new_string": "{ run(); }" }
        ] }),
    )
    .await;
    assert_eq!(edited["status"], "ok", "{edited}");
    assert_eq!(edited["snippet"], "     1\tfn start() { run(); }\n");
    assert_eq!(fixture.person.asked(), 0);
    let patched = call(
        &fixture,
        "apply_patch",
        json!({ "patch": "*** Begin Patch\n*** Update File: main.rs\n@@\n-fn start() { run(); }\n+fn start() {}\n*** Add File: lib.rs\n+pub fn run() {}\n*** End Patch" }),
    )
    .await;
    assert_eq!(patched["status"], "ok", "{patched}");
    assert_eq!(patched["changed"], json!(["main.rs", "lib.rs"]));
    assert_eq!(
        std::fs::read_to_string(root.join("main.rs")).expect("main"),
        "fn start() {}\n"
    );
    let denied = call(
        &fixture,
        "apply_patch",
        json!({ "patch": "*** Begin Patch\n*** Add File: .env\n+KEY=2\n*** End Patch" }),
    )
    .await;
    assert_eq!(denied["code"], "local_tools.denied", "{denied}");
    assert_eq!(
        std::fs::read_to_string(root.join(".env")).expect("env"),
        "KEY=1\n"
    );
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
    // Confinement is decided per command: where a sandbox hid what the
    // command may not read (Seatbelt), it ran unasked; where it ran
    // without one (the fixture allows unenforced sandboxes), it was asked.
    let expected_asks = usize::from(ran["enforcement"] != "full");
    assert_eq!(
        fixture.person.asked(),
        expected_asks,
        "asked exactly when the command ran unconfined: {ran}"
    );

    let written = call(
        &fixture,
        "write_file",
        json!({ "path": "x.txt", "content": "x" }),
    )
    .await;
    assert_eq!(written["status"], "ok", "{written}");
    assert_eq!(
        fixture.person.asked(),
        expected_asks,
        "writes are not asked"
    );
    let untracked = fixture.session.workspace().root().join("x.txt");

    fixture.person.answer(ApprovalOutcome::Decided {
        action: "reject".to_owned(),
        value: json!({ "reason": "not now" }),
    });
    let rejected = call(
        &fixture,
        "run_command",
        json!({ "command": "git clean -fdx" }),
    )
    .await;
    assert_eq!(rejected["code"], "local_tools.rejected");
    assert!(untracked.exists());

    fixture.person.answer(ApprovalOutcome::Deferred);
    let pending = call(
        &fixture,
        "run_command",
        json!({ "command": "git clean -fdx" }),
    )
    .await;
    assert_eq!(pending["code"], "local_tools.approval_pending");
    assert!(untracked.exists());
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

/// Output over a budget: a large command and a large diff answer within
/// the budget with an `output_id`, `read_output` pages them, plan mode
/// keeps it, and the spill files outlive the session (a reopen pages them)
/// until their retention ends or the conversation is forgotten.
#[tokio::test]
async fn large_outputs_spill_and_read_output_pages_them() {
    let fixture = fixture(open_policy());
    let ran = call(
        &fixture,
        "run_command",
        json!({ "command": "seq 1 150000", "sandbox": "read-only" }),
    )
    .await;
    assert_eq!(ran["status"], "ok", "{ran}");
    let shown = ran["stdout"].as_str().expect("stdout").len()
        + ran["stderr"].as_str().expect("stderr").len();
    assert!(shown.div_ceil(4) <= 6_000, "{shown}");
    assert!(ran["stdout_bytes"].as_u64().expect("bytes") > 900_000);
    let id = ran["output_id"].as_str().expect("output_id").to_owned();
    let out_dir = fixture.session.spill_store().conversation_dir();
    let spilled = out_dir.join(format!("{id}.txt"));
    assert!(out_dir.starts_with(fixture.dir.path().join("app-data/sessions/conv-1/out")));
    assert!(spilled.is_file());
    assert!(!spilled.starts_with(fixture.session.workspace().root()));

    fixture.session.set_profile(PermissionProfile::Plan);
    let page = call(
        &fixture,
        "read_output",
        json!({ "output_id": id, "offset": 149_999 }),
    )
    .await;
    assert_eq!(page["status"], "ok", "{page}");
    assert_eq!(page["content"], "149999\t149999\n150000\t150000\n");
    let path = call(
        &fixture,
        "read_output",
        json!({ "output_id": spilled.display().to_string() }),
    )
    .await;
    assert_eq!(path["code"], "local_tools.invalid_argument", "{path}");
    fixture.session.set_profile(PermissionProfile::Build);

    std::fs::write(
        fixture.session.workspace().root().join("main.rs"),
        (0..20_000)
            .map(|n| "// line ".to_owned() + &n.to_string() + "\n")
            .collect::<String>(),
    )
    .expect("write");
    let diff = call(&fixture, "git_diff", json!({})).await;
    assert_eq!(diff["truncated"], true, "{diff}");
    assert!(diff["output"].as_str().expect("diff").len() <= 8_000 * 4);
    let diff_id = diff["output_id"]
        .as_str()
        .expect("diff output_id")
        .to_owned();
    let diff_page = call(&fixture, "read_output", json!({ "output_id": diff_id })).await;
    assert!(
        diff_page["content"]
            .as_str()
            .expect("content")
            .contains("diff --git")
    );

    let Fixture {
        dir,
        person,
        session,
        provider,
    } = fixture;
    drop((person, session, provider));
    assert!(spilled.is_file(), "the outputs outlive the session");
    let reopened =
        elitea_local_tools::budget::SpillStore::new(&dir.path().join("app-data"), "conv-1");
    assert_eq!(
        reopened.read(&id).expect("kept").lines().last(),
        Some("150000")
    );
    elitea_local_tools::budget::forget_conversation_outputs(
        &dir.path().join("app-data"),
        "conv-1",
        "unrelated",
    );
    assert!(
        spilled.is_file(),
        "another conversation's forget keeps them"
    );
    reopened.clear();
    assert!(!out_dir.exists());
    drop(dir);
}

/// #1225: `exit_plan_mode` asks the person (never a rule) with `accept` /
/// `decline` and the plan; a decline keeps plan mode and carries the
/// person's feedback, bounded; an accept leaves it, and the toolset offers
/// every tool again.
#[tokio::test]
async fn exit_plan_mode_asks_the_person_and_an_accept_leaves_plan_mode() {
    let fixture = fixture(open_policy());
    fixture.session.set_profile(PermissionProfile::Plan);
    let plan = "# Rename\n\n1. Find the callers\n2. Rename";
    fixture.person.answer(ApprovalOutcome::Decided {
        action: "decline".to_owned(),
        value: json!({ "feedback": format!("Keep the old name as an alias. {}", "x".repeat(4096)) }),
    });
    let declined = call(&fixture, "exit_plan_mode", json!({ "plan": plan })).await;
    assert_eq!(declined["accepted"], false, "{declined}");
    let feedback = declined["feedback"].as_str().expect("feedback");
    assert!(feedback.starts_with("Keep the old name as an alias."));
    assert!(feedback.len() <= elitea_local_tools::session::MAX_FEEDBACK_BYTES);
    assert_eq!(fixture.session.profile(), PermissionProfile::Plan);
    {
        let asked = fixture.person.asked.lock().expect("lock");
        let request = asked.last().expect("asked");
        assert_eq!(request.available_actions, ["accept", "decline"]);
        assert_eq!(request.payload["plan_proposal"]["plan"], plan);
    }
    // An approve (a tool card's answer) is not an accept.
    fixture.person.answer(ApprovalOutcome::Decided {
        action: "approve".to_owned(),
        value: Value::Null,
    });
    let not_accepted = call(&fixture, "exit_plan_mode", json!({ "plan": plan })).await;
    assert_eq!(not_accepted["accepted"], false, "{not_accepted}");
    assert_eq!(fixture.session.profile(), PermissionProfile::Plan);

    fixture.person.answer(ApprovalOutcome::Decided {
        action: "accept".to_owned(),
        value: Value::Null,
    });
    let accepted = call(&fixture, "exit_plan_mode", json!({ "plan": plan })).await;
    assert_eq!(accepted["accepted"], true, "{accepted}");
    assert_eq!(fixture.session.profile(), PermissionProfile::Build);
    let now = names(&tools(&fixture).await);
    assert!(now.contains(&"write_file".to_owned()) && now.contains(&"run_command".to_owned()));
    assert!(!now.contains(&"exit_plan_mode".to_owned()));
    // Out of plan mode it is not offered, and a call is refused unasked.
    let asked = fixture.person.asked();
    let refused = fixture
        .session
        .call("exit_plan_mode", "c", json!({ "plan": plan }))
        .await;
    assert_eq!(refused["code"], "local_tools.denied", "{refused}");
    assert_eq!(fixture.person.asked(), asked);
}

/// #1225: a call the person denies with feedback is a typed failure the
/// model reads: `declined_by_person` and the person's text, bounded. A
/// denial without feedback is typed too; a rule's refusal is not.
#[tokio::test]
async fn a_denial_with_feedback_is_a_typed_failure_with_the_persons_text() {
    let fixture = fixture(open_policy());
    fixture.person.answer(ApprovalOutcome::Decided {
        action: "reject".to_owned(),
        value: json!({ "reason": "the person rejected it", "by": "person",
                       "feedback": "Do not reset; stash nothing, just diff." }),
    });
    let denied = call(
        &fixture,
        "run_command",
        json!({ "command": "git reset --hard HEAD" }),
    )
    .await;
    assert_eq!(fixture.person.asked(), 1, "asked: {denied}");
    assert_eq!(denied["status"], "error");
    assert_eq!(denied["code"], "local_tools.rejected");
    assert_eq!(denied["ok"], false);
    assert_eq!(denied["error"]["code"], "declined_by_person");
    assert_eq!(
        denied["error"]["feedback"],
        "Do not reset; stash nothing, just diff."
    );

    fixture.person.answer(ApprovalOutcome::Decided {
        action: "reject".to_owned(),
        value: json!({ "reason": "the person rejected it", "by": "person" }),
    });
    let plain = call(
        &fixture,
        "run_command",
        json!({ "command": "git reset --hard HEAD" }),
    )
    .await;
    assert_eq!(plain["error"]["code"], "declined_by_person");
    assert!(plain["error"].get("feedback").is_none());

    // A rule's denial says which layer refused, never "the person".
    let ruled = call(&fixture, "run_command", json!({ "command": "rm -rf x" })).await;
    assert!(
        ruled["message"]
            .as_str()
            .is_some_and(|m| m.contains("denied by policy")),
        "{ruled}"
    );
    assert!(
        ruled.get("error").is_none() && ruled.get("ok").is_none(),
        "{ruled}"
    );
}

/// #1225 × #1224: a `run_tests` approval (a test run with the network)
/// declined with feedback answers the model the same typed failure, and the
/// tests do not run.
#[tokio::test]
async fn a_run_tests_denial_with_feedback_is_a_typed_failure() {
    let fixture = fixture(LocalWorkPolicy {
        network: true,
        ..open_policy()
    });
    let root = fixture.session.workspace().root().to_path_buf();
    std::fs::write(
        root.join("Cargo.toml"),
        "[package]\nname = \"declined\"\nversion = \"0.1.0\"\nedition = \"2024\"\n",
    )
    .expect("manifest");
    fixture.person.answer(ApprovalOutcome::Decided {
        action: "reject".to_owned(),
        value: json!({ "reason": "the person rejected it", "by": "person",
                       "feedback": "Run them offline." }),
    });
    let denied = call(&fixture, "run_tests", json!({ "network": true })).await;
    assert_eq!(fixture.person.asked(), 1, "asked: {denied}");
    assert_eq!(denied["code"], "local_tools.rejected", "{denied}");
    assert_eq!(denied["error"]["code"], "declined_by_person");
    assert_eq!(denied["error"]["feedback"], "Run them offline.");
    assert!(!root.join("target").exists(), "the tests did not run");
}
