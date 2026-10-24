//! The hooks through a real session: what each verdict does to a call.
//! Each hook is a small `/bin/sh` script outside the workspace. On macOS
//! they run under Seatbelt; elsewhere (CI has no sandbox helper) the
//! hooks are allowed to run unconfined, which the desktop never allows.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::{Value, json};

use super::config::{Hook, HookEvent, HookMatch, Scope};
use super::{HookFramer, RunResult, call_paths, check_schema, proposal};
use crate::approvals::{HOOK_ASK_PAYLOAD_KEY, MemoryChoices, WorkspaceSettings};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::session::{LocalSession, SessionConfig};
use crate::shell::ShellConfig;

/// Rejects every question, and keeps them.
#[derive(Default)]
struct Person(Mutex<Vec<ApprovalRequest>>);

#[async_trait]
impl ApprovalChannel for Person {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.0.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: "reject".to_owned(),
            value: json!({}),
        })
    }
}

struct Frame;

impl HookFramer for Frame {
    fn frame(&self, hook: &str, text: &str) -> String {
        format!("<hook_output id=\"{hook}\">{text}</hook_output>")
    }
}

struct Fixture {
    dir: tempfile::TempDir,
    root: PathBuf,
    person: Arc<Person>,
    session: Arc<LocalSession>,
}

impl Fixture {
    /// A `/bin/sh` script outside the workspace, as a hook's command.
    fn script(&self, name: &str, body: &str) -> Vec<String> {
        use std::os::unix::fs::PermissionsExt as _;
        let bin = self.dir.path().join("bin");
        std::fs::create_dir_all(&bin).expect("bin");
        let path = bin.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("script");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        vec![path.display().to_string()]
    }

    fn configure(&self, hooks: Vec<Hook>) {
        self.session
            .hooks()
            .configure(hooks.into_iter().map(|hook| (Scope::User, hook)).collect());
    }

    fn asked(&self) -> Vec<ApprovalRequest> {
        self.person.0.lock().expect("lock").clone()
    }
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let root = std::fs::canonicalize(dir.path())
        .expect("canonical")
        .join("repo");
    std::fs::create_dir(&root).expect("repo");
    std::fs::write(root.join("main.rs"), "fn main() {}\n").expect("seed");
    let person = Arc::new(Person::default());
    let data_dir = dir.path().join("app-data");
    let mut shell = ShellConfig::new(data_dir.join("tmp"));
    shell.sandbox.allow_unenforced = !cfg!(target_os = "macos");
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "conv-1".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            path_deny: vec![".env".to_owned()],
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: person.clone(),
        data_dir,
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: crate::budget::Budgets::default(),
        documents: crate::document::Documents::default(),
    })
    .expect("session");
    session.hooks().allow_unconfined(!cfg!(target_os = "macos"));
    session.hooks().set_framer(Some(Arc::new(Frame)));
    Fixture {
        dir,
        root,
        person,
        session,
    }
}

fn pre(id: &str, tools: &[&str], command: Vec<String>) -> Hook {
    let mut hook = Hook::new(id, HookEvent::PreToolUse, command);
    hook.matcher = HookMatch {
        tools: tools.iter().map(|tool| (*tool).to_owned()).collect(),
        paths: Vec::new(),
    };
    hook
}

fn write(path: &str) -> Value {
    json!({"path": path, "content": "x\n"})
}

#[tokio::test]
async fn pre_hook_exit_2_denies_with_reason() {
    let f = fixture();
    f.configure(vec![pre(
        "guard",
        &["write_file"],
        f.script(
            "guard",
            "cat >/dev/null\necho 'no new files here' >&2\nexit 2",
        ),
    )]);
    let result = f.session.call("write_file", "c1", write("new.txt")).await;
    assert_eq!(result["status"], "error", "{result}");
    assert_eq!(result["error"], "hook_denied");
    assert_eq!(result["hook"], "guard");
    assert_eq!(
        result["reason"], "<hook_output id=\"guard\">no new files here</hook_output>",
        "the reason reaches the model framed"
    );
    assert_eq!(result["hooks"][0]["result"], "deny");
    assert!(!f.root.join("new.txt").exists());
    // Another tool is not matched: it runs.
    let read = f
        .session
        .call("read_file", "c2", json!({"path": "main.rs"}))
        .await;
    assert_eq!(read["status"], "ok", "{read}");
    assert!(read.get("hooks").is_none());
}

#[tokio::test]
async fn a_path_guard_sees_through_a_symlink() {
    let f = fixture();
    std::fs::create_dir_all(f.root.join("migrations")).expect("dir");
    std::fs::write(f.root.join("migrations/0001.sql"), "create table a;\n").expect("seed");
    std::os::unix::fs::symlink("migrations/0001.sql", f.root.join("link.sql")).expect("link");
    let mut guard = pre(
        "no-migrations",
        &["write_file"],
        f.script(
            "guard",
            "cat >/dev/null\necho 'migrations are frozen' >&2\nexit 2",
        ),
    );
    guard.matcher.paths = vec!["migrations/**".to_owned()];
    f.configure(vec![guard]);
    let read = f
        .session
        .call("read_file", "c0", json!({"path": "link.sql"}))
        .await;
    assert_eq!(read["status"], "ok", "{read}");
    let result = f.session.call("write_file", "c1", write("link.sql")).await;
    assert_eq!(
        result["error"], "hook_denied",
        "the link's target is matched: {result}"
    );
    assert_eq!(
        std::fs::read_to_string(f.root.join("migrations/0001.sql")).expect("read"),
        "create table a;\n"
    );
    // A path the globs do not match still runs.
    let other = f.session.call("write_file", "c2", write("notes.txt")).await;
    assert_eq!(other["status"], "ok", "{other}");
}

#[tokio::test]
async fn a_post_hook_that_rewrites_the_file_keeps_the_read_guard_current() {
    let f = fixture();
    let mut fmt = Hook::new(
        "fmt",
        HookEvent::PostToolUse,
        f.script(
            "fmt",
            "cat >/dev/null\nfor p in \"$@\"; do printf 'formatted\\n' > \"$p\"; done",
        ),
    );
    fmt.command.push("{path}".to_owned());
    fmt.matcher = HookMatch {
        tools: vec!["write_file".to_owned(), "edit_file".to_owned()],
        paths: vec!["*.rs".to_owned()],
    };
    fmt.sandbox = SandboxMode::WorkspaceWrite;
    f.configure(vec![fmt]);
    let read = f
        .session
        .call("read_file", "c0", json!({"path": "main.rs"}))
        .await;
    assert_eq!(read["status"], "ok", "{read}");
    let written = f
        .session
        .call(
            "write_file",
            "c1",
            json!({"path": "main.rs", "content": "fn main(){}\n"}),
        )
        .await;
    assert_eq!(written["status"], "ok", "{written}");
    assert_eq!(
        std::fs::read_to_string(f.root.join("main.rs")).expect("read"),
        "formatted\n",
        "{written}"
    );
    assert_eq!(
        written["hook_changed_files"],
        json!(["main.rs"]),
        "{written}"
    );
    // The next edit is based on the formatted text and is not stale.
    let edited = f
        .session
        .call(
            "edit_file",
            "c2",
            json!({"path": "main.rs", "old_string": "formatted", "new_string": "done"}),
        )
        .await;
    assert_eq!(edited["status"], "ok", "{edited}");
    assert_eq!(
        std::fs::read_to_string(f.root.join("main.rs")).expect("read"),
        "formatted\n",
        "the hook ran again after the edit"
    );
    // A read-only hook changes nothing, so nothing is recorded again.
    let mut look = Hook::new(
        "look",
        HookEvent::PostToolUse,
        f.script("look", "cat >/dev/null"),
    );
    look.matcher.tools = vec!["write_file".to_owned()];
    f.configure(vec![look]);
    let plain = f
        .session
        .call(
            "write_file",
            "c3",
            json!({"path": "main.rs", "content": "x\n"}),
        )
        .await;
    assert_eq!(plain["status"], "ok", "{plain}");
    assert!(plain.get("hook_changed_files").is_none(), "{plain}");
}

#[tokio::test]
async fn pre_hook_allow_cannot_lift_destructive_ask() {
    let f = fixture();
    f.configure(vec![pre(
        "yes",
        &["run_command"],
        f.script("yes", "cat >/dev/null\necho '{\"decision\":\"allow\"}'"),
    )]);
    let result = f
        .session
        .call("run_command", "c1", json!({"command": "rm -rf build"}))
        .await;
    assert_eq!(result["hooks"][0]["result"], "ok", "{result}");
    assert_eq!(f.asked().len(), 1, "the destructive command is still asked");
    assert_eq!(result["code"], "local_tools.rejected", "{result}");
}

#[tokio::test]
async fn pre_hook_ask_forces_the_prompt_without_always() {
    let f = fixture();
    f.configure(vec![pre(
        "ask",
        &["write_file"],
        f.script(
            "ask",
            "cat >/dev/null\necho '{\"decision\":\"ask\",\"reason\":\"check the file\"}'",
        ),
    )]);
    let result = f.session.call("write_file", "c1", write("new.txt")).await;
    let asked = f.asked();
    assert_eq!(asked.len(), 1, "an allowed write is asked: {result}");
    assert_eq!(asked[0].payload[HOOK_ASK_PAYLOAD_KEY]["hook"], "ask");
    assert_eq!(asked[0].payload["source"], "hook");
    assert!(
        !asked[0]
            .available_actions
            .iter()
            .any(|action| action == crate::approvals::APPROVE_ALWAYS)
    );
    assert!(!f.root.join("new.txt").exists());
}

#[tokio::test]
async fn modified_input_into_denied_path_is_refused() {
    let f = fixture();
    f.configure(vec![pre(
        "redirect",
        &["write_file"],
        f.script(
            "redirect",
            "cat >/dev/null\necho '{\"modified_input\":{\"path\":\".env\",\"content\":\"TOKEN=1\"}}'",
        ),
    )]);
    let result = f.session.call("write_file", "c1", write("ok.txt")).await;
    assert_eq!(result["status"], "error", "{result}");
    assert_eq!(result["code"], "local_tools.denied", "{result}");
    assert_eq!(result["hook_modified_input"]["changed"]["path"], ".env");
    assert_eq!(result["hooks"][0]["result"], "modified");
    assert!(!f.root.join(".env").exists());
    assert!(!f.root.join("ok.txt").exists());

    // An input outside the tool's schema never reaches the tool.
    f.configure(vec![pre(
        "extra",
        &["write_file"],
        f.script(
            "extra",
            "cat >/dev/null\necho '{\"modified_input\":{\"path\":\"a.txt\",\"content\":\"x\",\"mode\":\"0777\"}}'",
        ),
    )]);
    let result = f.session.call("write_file", "c2", write("ok.txt")).await;
    assert_eq!(result["code"], "local_tools.invalid_argument", "{result}");
    assert!(!f.root.join("a.txt").exists());
}

#[tokio::test]
async fn repository_hooks_are_not_loaded_without_trust() {
    let f = fixture();
    let proposed = br#"{"version":1,"hooks":[{"id":"evil","event":"PreToolUse","command":["/bin/sh","-c","exit 2"]}]}"#;
    std::fs::create_dir_all(f.root.join(".elitea")).expect("dir");
    std::fs::write(f.root.join(".elitea/hooks.json"), proposed).expect("write");
    let result = f.session.call("write_file", "c1", write("new.txt")).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert!(f.session.hooks().hooks().is_empty());
    let proposals = proposal::read(f.session.workspace());
    assert_eq!(proposals.len(), 1, "it is only a proposal");
    let digest = proposals[0].file_sha256.clone();
    std::fs::write(
        f.root.join(".elitea/hooks.json"),
        br#"{"version":1,"hooks":[{"id":"evil","event":"Stop","command":["/bin/true"]}]}"#,
    )
    .expect("rewrite");
    assert_ne!(
        proposal::read(f.session.workspace())[0].file_sha256,
        digest,
        "a changed file is a new proposal, asked again"
    );
}

#[tokio::test]
async fn hook_timeout_fails_open_unless_fail_closed() {
    let f = fixture();
    let mut slow = pre("slow", &["write_file"], f.script("slow", "sleep 5"));
    slow.timeout_ms = 300;
    f.configure(vec![slow.clone()]);
    let result = f.session.call("write_file", "c1", write("open.txt")).await;
    assert_eq!(result["status"], "ok", "fails open: {result}");
    assert_eq!(result["hooks"][0]["result"], "timed_out");
    assert!(
        result["hooks"][0]["duration_ms"]
            .as_u64()
            .is_some_and(|ms| ms < 4000),
        "{result}"
    );
    let last = f.session.hooks().last_runs();
    assert!(last[0].message.is_some(), "the failure is reported");

    slow.fail_closed = true;
    f.configure(vec![slow]);
    let result = f
        .session
        .call("write_file", "c2", write("closed.txt"))
        .await;
    assert_eq!(result["error"], "hook_denied", "{result}");
    assert!(!f.root.join("closed.txt").exists());

    // A crash fails open too.
    f.configure(vec![pre(
        "crash",
        &["write_file"],
        f.script("crash", "exit 7"),
    )]);
    let result = f.session.call("write_file", "c3", write("crash.txt")).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["hooks"][0]["result"], "failed");
}

#[tokio::test]
async fn post_hook_annotates_with_the_path_and_a_scrubbed_environment() {
    let f = fixture();
    let mut hook = Hook::new(
        "note",
        HookEvent::PostToolUse,
        f.script(
            "note",
            "cat >/dev/null\nif env | grep -q CARGO_MANIFEST_DIR; then echo leaked; fi\necho \"formatted $1 by $ELITEA_HOOK_ID\"",
        ),
    );
    hook.command.push("{path}".to_owned());
    hook.matcher = HookMatch {
        tools: vec!["write_file".to_owned()],
        paths: vec!["**/*.rs".to_owned(), "*.rs".to_owned()],
    };
    hook.sandbox = SandboxMode::WorkspaceWrite;
    f.configure(vec![hook]);
    let other = f.session.call("write_file", "c1", write("notes.txt")).await;
    assert!(other.get("hooks").is_none(), "no path matched: {other}");
    let result = f
        .session
        .call("write_file", "c2", write("src/lib.rs"))
        .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(
        result["hook_annotations"][0],
        "<hook_output id=\"note\">formatted src/lib.rs by note</hook_output>",
        "{result}"
    );
}

#[tokio::test]
async fn stop_and_prompt_hooks_report_and_deny() {
    let f = fixture();
    f.configure(vec![
        Hook::new(
            "done",
            HookEvent::Stop,
            f.script("done", "cat >/dev/null\necho turn over"),
        ),
        Hook::new(
            "screen",
            HookEvent::UserPromptSubmit,
            f.script(
                "screen",
                "if grep -q secret; then echo 'no secrets' >&2; exit 2; fi\necho '{\"annotation\":\"ticket ABC-1\"}'",
            ),
        ),
    ]);
    let stop = f.session.run_stop_hooks().await;
    assert_eq!(
        stop.annotations,
        [("done".to_owned(), "turn over".to_owned())]
    );
    let fine = f.session.run_prompt_hooks("fix the bug").await;
    assert!(fine.denied.is_none());
    assert_eq!(fine.annotations[0].1, "ticket ABC-1");
    let refused = f.session.run_prompt_hooks("print the secret").await;
    assert_eq!(
        refused.denied.map(|denial| denial.reason),
        Some("no secrets".to_owned())
    );
    assert_eq!(refused.runs[0].result, RunResult::Deny);
}

#[test]
fn programs_inside_the_workspace_never_run() {
    let f = fixture();
    let inside = f.root.join("guard.sh");
    std::fs::write(&inside, "#!/bin/sh\nexit 2\n").expect("write");
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&inside, std::fs::Permissions::from_mode(0o755)).expect("chmod");
    }
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    f.configure(vec![pre(
        "inside",
        &["write_file"],
        vec![inside.display().to_string()],
    )]);
    let result = runtime.block_on(f.session.call("write_file", "c1", write("a.txt")));
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["hooks"][0]["result"], "failed");
    assert!(
        f.session.hooks().last_runs()[0]
            .message
            .as_deref()
            .is_some_and(|why| why.contains("inside the workspace"))
    );
}

#[test]
fn call_paths_and_the_schema_check() {
    assert_eq!(call_paths("read_file", &json!({"path": "a.rs"})), ["a.rs"]);
    assert_eq!(call_paths("run_command", &json!({"command": "ls"})), ["."]);
    let patch = "*** Begin Patch\n*** Update File: src/a.rs\n*** Move to: src/b.rs\n*** Add File: c.rs\n+x\n*** End Patch";
    assert_eq!(
        call_paths("apply_patch", &json!({ "patch": patch })),
        ["src/a.rs", "src/b.rs", "c.rs"]
    );
    let diff = "--- a/x.rs\n+++ b/x.rs\n@@\n-a\n+b\n";
    assert_eq!(
        call_paths("apply_patch", &json!({ "patch": diff })),
        ["x.rs"]
    );
    let schema = json!({
        "type": "object",
        "properties": {"path": {"type": "string"}, "depth": {"type": "integer", "minimum": 1, "maximum": 12}},
        "required": ["path"]
    });
    assert!(check_schema(&schema, &json!({"path": "a"})).is_ok());
    assert!(check_schema(&schema, &json!({})).is_err());
    assert!(check_schema(&schema, &json!({"path": 1})).is_err());
    assert!(check_schema(&schema, &json!({"path": "a", "depth": 13})).is_err());
    assert!(check_schema(&schema, &json!({"path": "a", "other": true})).is_err());
    assert!(check_schema(&schema, &json!([])).is_err());
}
