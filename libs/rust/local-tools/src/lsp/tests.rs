//! Edit diagnostics end to end with a fake language server
//! (`tests/fixtures/lsp/fake_lsp.sh`: LSP framing in `sh`, scripted
//! `publishDiagnostics`) started by the session under its sandbox.
//!
//! A server starts only under a fully enforced sandbox: Seatbelt on macOS,
//! bubblewrap where it works on Linux. Where there is none (Linux CI
//! without a usable bubblewrap) the tests that need a running server
//! assert the other half of that rule instead: the edit succeeds and its
//! diagnostics say `unavailable`. The Seatbelt tests at the end run on the
//! real `sandbox-exec` (skipped where it is missing, unless
//! `ELITEA_REQUIRE_SEATBELT=1`).

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::{Value, json};

use super::*;
use crate::approvals::{MemoryChoices, WorkspaceSettings};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::session::{LocalSession, SessionConfig};
use crate::shell::ShellConfig;

struct Approve;

#[async_trait]
impl ApprovalChannel for Approve {
    async fn request(&self, _request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        Ok(ApprovalOutcome::Decided {
            action: "approve".to_owned(),
            value: Value::Null,
        })
    }
}

#[derive(Default)]
struct Heard(Mutex<Vec<LspStatus>>);

impl LspEvents for Heard {
    fn status(&self, status: &LspStatus) {
        self.0.lock().unwrap().push(status.clone());
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    base: PathBuf,
    root: PathBuf,
    data: PathBuf,
    session: Arc<LocalSession>,
    heard: Arc<Heard>,
}

fn policy() -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        shell: true,
        path_deny: vec!["*.secret.rs".to_owned()],
        ..LocalWorkPolicy::default()
    }
}

fn fixture_with(policy: LocalWorkPolicy, files: &[(&str, &str)]) -> Fixture {
    fixture_opts(policy, files, false)
}

/// With `unconfined_hooks`, the person's hooks may run without an OS
/// sandbox where there is none (as in the hooks tests; the desktop never
/// allows it). A language server still needs a full one.
fn fixture_opts(
    policy: LocalWorkPolicy,
    files: &[(&str, &str)],
    unconfined_hooks: bool,
) -> Fixture {
    let dir = tempfile::tempdir().unwrap();
    let base = std::fs::canonicalize(dir.path()).unwrap();
    let root = base.join("workspace");
    let home = base.join("home");
    let data = base.join("app-data");
    for (path, text) in files {
        let path = root.join(path);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }
    std::fs::create_dir_all(&home).unwrap();
    let mut shell = ShellConfig::new(data.join("tmp").join("session"));
    shell.home = Some(home);
    shell.sandbox.allow_unenforced = unconfined_hooks;
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "conv-1".to_owned(),
        policy,
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(Approve),
        data_dir: data.clone(),
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: crate::budget::Budgets::default(),
        documents: crate::document::Documents::default(),
    })
    .unwrap();
    session.hooks().allow_unconfined(unconfined_hooks);
    let heard = Arc::new(Heard::default());
    session.lsp().set_events(Some(heard.clone()));
    Fixture {
        _dir: dir,
        base,
        root,
        data,
        session,
        heard,
    }
}

fn rust_crate(lib: &str) -> Fixture {
    fixture_with(
        policy(),
        &[
            (
                "Cargo.toml",
                "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
            ),
            ("src/lib.rs", lib),
        ],
    )
}

/// The fake server, copied outside the workspace, run by `/bin/sh` with
/// `args`.
fn fake_server(fixture: &Fixture, args: &[&str]) -> ServerProgram {
    let bin = fixture.base.join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    let script = bin.join("fake_lsp.sh");
    std::fs::write(
        &script,
        include_str!("../../tests/fixtures/lsp/fake_lsp.sh"),
    )
    .unwrap();
    let mut all = vec![script.display().to_string()];
    all.extend(args.iter().map(|arg| (*arg).to_owned()));
    ServerProgram {
        program: PathBuf::from("/bin/sh"),
        args: all,
    }
}

fn enable(fixture: &Fixture, language: Language, program: ServerProgram, wait: Duration) {
    fixture.session.lsp().configure(LspSettings {
        enabled: true,
        waits: BTreeMap::from([(language, wait)]),
        programs: BTreeMap::from([(language, program)]),
        idle: IDLE,
    });
}

/// Whether a server can start here: the sandbox is fully enforced.
fn sandboxed(fixture: &Fixture) -> bool {
    fixture
        .session
        .command_confinement(SandboxMode::ReadOnly, false)
        .is_ok_and(|confinement| {
            matches!(
                confinement.enforcement,
                crate::sandbox::Enforcement::Full | crate::sandbox::Enforcement::Container
            ) && confinement.hides_denied
        })
}

/// What the fake server logged.
fn received(fixture: &Fixture, language: Language) -> String {
    let log = fixture
        .data
        .join("tmp")
        .join("session")
        .join("lsp")
        .join(language.as_str())
        .join("tmp")
        .join("received.log");
    std::fs::read_to_string(log).unwrap_or_default()
}

async fn call(fixture: &Fixture, tool: &str, args: Value) -> Value {
    fixture.session.call(tool, "call-1", args).await
}

async fn edit(fixture: &Fixture, path: &str, old: &str, new: &str) -> Value {
    let read = call(fixture, "read_file", json!({ "path": path })).await;
    assert_eq!(read["status"], "ok", "{read}");
    let result = call(
        fixture,
        "edit_file",
        json!({ "path": path, "old_string": old, "new_string": new }),
    )
    .await;
    assert_eq!(result["status"], "ok", "the edit never fails: {result}");
    result
}

/// Without a full sandbox no server starts: the edit still succeeds, and
/// its diagnostics say why once.
fn unavailable_here(result: &Value) -> bool {
    if result["diagnostics"]["status"] == "unavailable" {
        let message = result["diagnostics"]["message"]
            .as_str()
            .unwrap_or_default();
        assert!(message.contains("diagnostics off"), "{result}");
        return true;
    }
    false
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn new_errors_are_appended_after_edit() {
    let fixture = rust_crate("fn a() {}\n");
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    let result = edit(
        &fixture,
        "src/lib.rs",
        "fn a() {}\n",
        "fn a() {\n    let x = 1;\n    UNDEFINED_NAME;\n}\n",
    )
    .await;
    assert!(
        std::fs::read_to_string(fixture.root.join("src/lib.rs"))
            .unwrap()
            .contains("UNDEFINED_NAME")
    );
    if !sandboxed(&fixture) {
        assert!(unavailable_here(&result), "{result}");
        return;
    }
    let diagnostics = &result["diagnostics"];
    assert_eq!(diagnostics["status"], "ok", "{result}");
    assert_eq!(
        diagnostics["new_errors"],
        json!([{
            "path": "src/lib.rs",
            "line": 3,
            "col": 9,
            "severity": "error",
            "code": "E0425",
            "message": "cannot find value `foo` in this scope",
        }])
    );
    assert_eq!(diagnostics["total_errors_in_file"], 1);
    let states: Vec<LspState> = fixture
        .heard
        .0
        .lock()
        .unwrap()
        .iter()
        .map(|s| s.state)
        .collect();
    assert_eq!(states, vec![LspState::Starting, LspState::Ready]);
    // The workspace's text reached the server through the edit only.
    let log = received(&fixture, Language::Rust);
    assert!(log.contains("initialize"), "{log}");
    assert!(log.contains("textDocument/didOpen file://"), "{log}");
    assert!(log.contains("textDocument/didChange"), "{log}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn preexisting_errors_are_not_repeated() {
    let fixture = rust_crate("fn a() { OLD_BROKEN }\n");
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    let result = edit(
        &fixture,
        "src/lib.rs",
        "fn a() { OLD_BROKEN }\n",
        "fn a() { OLD_BROKEN }\n\nfn b() { UNDEFINED_NAME }\n",
    )
    .await;
    if !sandboxed(&fixture) {
        assert!(unavailable_here(&result), "{result}");
        return;
    }
    let diagnostics = &result["diagnostics"];
    assert_eq!(diagnostics["status"], "ok", "{result}");
    let codes: Vec<&str> = diagnostics["new_errors"]
        .as_array()
        .unwrap()
        .iter()
        .map(|error| error["code"].as_str().unwrap())
        .collect();
    assert_eq!(codes, vec!["E0425"], "the old error is not new: {result}");
    assert_eq!(diagnostics["total_errors_in_file"], 2);
    // Open now: the next edit compares with what the last one left.
    let again = edit(&fixture, "src/lib.rs", "fn b()", "fn c()").await;
    assert_eq!(again["diagnostics"]["status"], "ok", "{again}");
    assert_eq!(again["diagnostics"]["new_errors"], json!([]), "{again}");
    assert_eq!(again["diagnostics"]["total_errors_in_file"], 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn timeout_reports_status_and_edit_succeeds() {
    let fixture = rust_crate("fn a() {}\n");
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["silent"]),
        Duration::from_millis(300),
    );
    let result = edit(
        &fixture,
        "src/lib.rs",
        "fn a() {}",
        "fn a() { UNDEFINED_NAME }",
    )
    .await;
    assert!(
        std::fs::read_to_string(fixture.root.join("src/lib.rs"))
            .unwrap()
            .contains("UNDEFINED_NAME")
    );
    if !sandboxed(&fixture) {
        assert!(unavailable_here(&result), "{result}");
        return;
    }
    assert_eq!(result["diagnostics"]["status"], "timeout", "{result}");
    assert_eq!(result["diagnostics"]["new_errors"], json!([]));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn denied_path_is_never_opened_in_server() {
    let fixture = fixture_with(
        policy(),
        &[
            (
                "Cargo.toml",
                "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
            ),
            ("src/lib.rs", "fn a() {}\n"),
            (
                "src/keys.secret.rs",
                "const KEY: &str = \"UNDEFINED_NAME\";\n",
            ),
        ],
    );
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    // The tools refuse it outright …
    let refused = call(
        &fixture,
        "diagnostics",
        json!({ "path": "src/keys.secret.rs" }),
    )
    .await;
    assert_eq!(refused["status"], "error", "{refused}");
    assert_eq!(refused["code"], "local_tools.denied", "{refused}");
    // … and the sync below them never gives it to a server, even when an
    // edit names it beside an allowed file.
    let denied = WsPath::from_relative(Path::new("src/keys.secret.rs")).unwrap();
    let allowed = WsPath::from_relative(Path::new("src/lib.rs")).unwrap();
    let lsp = fixture.session.lsp();
    let before = lsp.before(
        fixture.session.workspace(),
        &[denied.clone(), allowed.clone()],
    );
    assert!(!before.0.contains_key(&denied));
    let result = lsp
        .after(
            &fixture.session,
            before,
            vec![
                Changed {
                    path: denied,
                    deleted: false,
                },
                Changed {
                    path: allowed,
                    deleted: false,
                },
            ],
        )
        .await;
    let log = received(&fixture, Language::Rust);
    assert!(!log.contains("secret"), "{log}");
    if sandboxed(&fixture) {
        assert!(log.contains("src/lib.rs"), "{log}");
        assert_eq!(result.unwrap()["status"], "ok");
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_missing_server_is_reported_once_and_the_edit_succeeds() {
    let fixture = fixture_with(
        policy(),
        &[("go.mod", "module x\n"), ("main.go", "package main\n")],
    );
    enable(
        &fixture,
        Language::Go,
        ServerProgram {
            program: fixture.base.join("bin/gopls"),
            args: Vec::new(),
        },
        Duration::from_millis(300),
    );
    let first = edit(
        &fixture,
        "main.go",
        "package main\n",
        "package main\n\nfunc a() {}\n",
    )
    .await;
    assert_eq!(first["diagnostics"]["status"], "unavailable", "{first}");
    assert!(
        first["diagnostics"]["message"]
            .as_str()
            .unwrap()
            .contains("does not exist"),
        "{first}"
    );
    let second = edit(&fixture, "main.go", "func a() {}", "func b() {}").await;
    assert!(
        second.get("diagnostics").is_none(),
        "reported once: {second}"
    );
    let heard = fixture.heard.0.lock().unwrap().clone();
    assert_eq!(heard.last().unwrap().state, LspState::Unavailable);
    // A file no server serves gets no field at all.
    let plain = fixture_with(policy(), &[("go.mod", "module x\n"), ("notes.md", "a\n")]);
    let result = edit(&plain, "notes.md", "a", "b").await;
    assert!(result.get("diagnostics").is_none(), "{result}");
}

/// A server found missing is tried again once the backoff has passed (the
/// person installed it meanwhile), not on every turn's `configure`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_unavailable_server_is_tried_again_after_the_backoff() {
    let fixture = fixture_with(
        policy(),
        &[("go.mod", "module x\n"), ("main.go", "package main\n")],
    );
    let program = fixture.base.join("bin/gopls");
    let settings = LspSettings {
        enabled: true,
        waits: BTreeMap::from([(Language::Go, Duration::from_millis(300))]),
        programs: BTreeMap::from([(
            Language::Go,
            ServerProgram {
                program: program.clone(),
                args: Vec::new(),
            },
        )]),
        idle: IDLE,
    };
    fixture.session.lsp().configure(settings.clone());
    let first = edit(
        &fixture,
        "main.go",
        "package main\n",
        "package main\n\nfunc a() {}\n",
    )
    .await;
    assert!(
        first["diagnostics"]["message"]
            .as_str()
            .unwrap()
            .contains("does not exist"),
        "{first}"
    );
    // The same settings at the next turn, within the backoff: still off.
    fixture.session.lsp().configure(settings.clone());
    assert_eq!(
        fixture.session.lsp().status()[0].state,
        LspState::Unavailable
    );
    // Installed meanwhile; once the backoff has passed it is tried again.
    std::fs::create_dir_all(program.parent().unwrap()).unwrap();
    std::fs::write(&program, "#!/bin/sh\nexit 0\n").unwrap();
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    fixture.session.lsp().retry_unavailable(Duration::ZERO);
    assert!(fixture.session.lsp().status().is_empty(), "forgotten");
    let second = edit(&fixture, "main.go", "func a() {}", "func b() {}").await;
    let message = second["diagnostics"]["message"]
        .as_str()
        .unwrap_or_default();
    assert!(
        !message.contains("does not exist"),
        "the program was looked for again: {second}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn no_server_starts_while_commands_are_off() {
    let fixture = fixture_with(
        LocalWorkPolicy {
            shell: false,
            ..policy()
        },
        &[
            (
                "Cargo.toml",
                "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
            ),
            ("src/lib.rs", "fn a() {}\n"),
        ],
    );
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    let tools: Vec<&str> = fixture
        .session
        .available_tools()
        .iter()
        .map(|tool| tool.name)
        .collect();
    assert!(!tools.contains(&"diagnostics"), "{tools:?}");
    let result = edit(
        &fixture,
        "src/lib.rs",
        "fn a() {}",
        "fn a() { UNDEFINED_NAME }",
    )
    .await;
    assert_eq!(result["diagnostics"]["status"], "unavailable", "{result}");
    assert_eq!(received(&fixture, Language::Rust), "");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn diagnostics_is_offered_bounded_and_allowed_by_default() {
    let fixture = rust_crate("fn a() { OLD_BROKEN UNDEFINED_NAME }\n");
    let tools: Vec<&str> = fixture
        .session
        .available_tools()
        .iter()
        .map(|tool| tool.name)
        .collect();
    assert!(
        !tools.contains(&"diagnostics"),
        "off until the host turns it on"
    );
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    let tools: Vec<&str> = fixture
        .session
        .available_tools()
        .iter()
        .map(|tool| tool.name)
        .collect();
    assert!(tools.contains(&"diagnostics"), "{tools:?}");
    let result = call(
        &fixture,
        "diagnostics",
        json!({ "path": "src/lib.rs", "max": 1 }),
    )
    .await;
    assert_eq!(result["status"], "ok", "no question asked: {result}");
    if !sandboxed(&fixture) {
        assert_eq!(result["diagnostics"]["status"], "unavailable", "{result}");
        return;
    }
    let diagnostics = &result["diagnostics"];
    assert_eq!(diagnostics["status"], "ok", "{result}");
    assert_eq!(diagnostics["total"], 2);
    assert_eq!(diagnostics["items"].as_array().unwrap().len(), 1);
    assert_eq!(diagnostics["truncated"], true);
    // Without a path: the files of this session.
    let all = call(&fixture, "diagnostics", json!({ "severity": "all" })).await;
    assert_eq!(all["diagnostics"]["total"], 2, "{all}");
    let bad = call(&fixture, "diagnostics", json!({ "max": 1, "extra": true })).await;
    assert_eq!(bad["status"], "error", "{bad}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_idle_server_is_stopped() {
    let fixture = rust_crate("fn a() {}\n");
    fixture.session.lsp().configure(LspSettings {
        enabled: true,
        waits: BTreeMap::from([(Language::Rust, Duration::from_secs(5))]),
        programs: BTreeMap::from([(Language::Rust, fake_server(&fixture, &["normal"]))]),
        idle: Duration::from_millis(100),
    });
    let result = edit(&fixture, "src/lib.rs", "fn a() {}", "fn b() {}").await;
    if !sandboxed(&fixture) {
        assert!(unavailable_here(&result), "{result}");
        return;
    }
    for _ in 0..100 {
        if fixture.session.lsp().status().is_empty() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(fixture.session.lsp().status(), Vec::<LspStatus>::new());
    let heard = fixture.heard.0.lock().unwrap().clone();
    assert_eq!(heard.last().unwrap().state, LspState::Stopped);
}

struct Frame;

impl crate::hooks::HookFramer for Frame {
    fn frame(&self, hook: &str, text: &str) -> String {
        format!("[{hook}] {text}")
    }
}

/// A `/bin/sh` hook script outside the workspace.
fn hook_script(fixture: &Fixture, name: &str, body: &str) -> Vec<String> {
    let bin = fixture.base.join("hooks");
    std::fs::create_dir_all(&bin).unwrap();
    let path = bin.join(name);
    std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
    vec!["/bin/sh".to_owned(), path.display().to_string()]
}

/// Hooks and diagnostics together (#1236, #1237): a `PreToolUse` hook's
/// modified input is what is written and what the language server is
/// given, and the `PostToolUse` hooks see the result with its
/// `diagnostics` (they run after the dispatch, which adds them).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_hook_modified_write_gets_diagnostics_and_post_hooks_see_them() {
    use crate::hooks::{Hook, HookEvent, HookMatch, Scope};
    let fixture = fixture_opts(
        policy(),
        &[
            (
                "Cargo.toml",
                "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
            ),
            ("src/lib.rs", "fn a() {}\n"),
        ],
        !cfg!(target_os = "macos"),
    );
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal"]),
        Duration::from_secs(5),
    );
    let only_writes = HookMatch {
        tools: vec!["write_file".to_owned()],
        paths: Vec::new(),
    };
    let mut pre = Hook::new(
        "inject",
        HookEvent::PreToolUse,
        hook_script(
            &fixture,
            "inject.sh",
            r#"cat >/dev/null
printf '%s\n' '{"modified_input":{"path":"src/lib.rs","content":"fn a() {\n    let x = 1;\n    UNDEFINED_NAME;\n}\n"}}'"#,
        ),
    );
    pre.matcher = only_writes.clone();
    let mut post = Hook::new(
        "look",
        HookEvent::PostToolUse,
        hook_script(
            &fixture,
            "look.sh",
            r#"input=$(cat)
case "$input" in
  *E0425*) echo "saw E0425" ;;
  *'"diagnostics"'*) echo "saw diagnostics" ;;
  *) echo "saw no diagnostics" ;;
esac"#,
        ),
    );
    post.matcher = only_writes;
    fixture.session.hooks().set_framer(Some(Arc::new(Frame)));
    fixture
        .session
        .hooks()
        .configure(vec![(Scope::User, pre), (Scope::User, post)]);

    let read = call(&fixture, "read_file", json!({ "path": "src/lib.rs" })).await;
    assert_eq!(read["status"], "ok", "{read}");
    let result = call(
        &fixture,
        "write_file",
        json!({ "path": "src/lib.rs", "content": "fn a() {}\nfn b() {}\n" }),
    )
    .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["hooks"][0]["result"], "modified", "{result}");
    let written = std::fs::read_to_string(fixture.root.join("src/lib.rs")).unwrap();
    assert!(
        written.contains("UNDEFINED_NAME"),
        "the hook's input was written: {written}"
    );
    if !sandboxed(&fixture) {
        assert!(unavailable_here(&result), "{result}");
        assert_eq!(
            result["hook_annotations"][0], "[look] saw diagnostics",
            "{result}"
        );
        return;
    }
    // The error is the modified content's, not the original call's.
    assert_eq!(result["diagnostics"]["status"], "ok", "{result}");
    assert_eq!(
        result["diagnostics"]["new_errors"][0]["code"], "E0425",
        "{result}"
    );
    assert_eq!(
        result["hook_annotations"][0], "[look] saw E0425",
        "{result}"
    );
}

#[test]
fn only_new_diagnostics_count_whatever_their_lines() {
    let diag = |line, message: &str| Diag {
        line,
        col: 1,
        severity: Severity::Error,
        code: None,
        message: message.to_owned(),
    };
    let before = vec![diag(1, "a"), diag(5, "b")];
    let after = vec![diag(3, "a"), diag(9, "b"), diag(10, "b"), diag(11, "c")];
    let new: Vec<(u32, &str)> = new_since(&before, &after)
        .iter()
        .map(|diag| (diag.line, diag.message.as_str()))
        .collect();
    assert_eq!(new, vec![(10, "b"), (11, "c")]);
}

#[test]
fn a_report_is_bounded_and_warnings_come_only_when_few() {
    let path = WsPath::from_relative(Path::new("src/lib.rs")).unwrap();
    let diag = |severity, index: usize| Diag {
        line: u32::try_from(index).unwrap(),
        col: 1,
        severity,
        code: Some("E1".to_owned()),
        message: format!("problem {index} {}", "x".repeat(300)),
    };
    let mut after: Vec<Diag> = (0..30).map(|index| diag(Severity::Error, index)).collect();
    after.extend((0..6).map(|index| diag(Severity::Warning, 100 + index)));
    let report = Report {
        files: vec![FileOutcome {
            path: path.clone(),
            after: Some(after),
            before: Vec::new(),
        }],
        unavailable: Vec::new(),
    }
    .render()
    .unwrap();
    let items = report["new_errors"].as_array().unwrap();
    assert!(items.len() <= MAX_REPORTED);
    assert!(serde_json::to_string(items).unwrap().len() <= MAX_REPORT_BYTES);
    assert!(items.iter().all(|item| item["severity"] == "error"));
    assert_eq!(report["truncated"], true);
    assert_eq!(report["total_errors_in_file"], 30);
    let few = Report {
        files: vec![FileOutcome {
            path,
            after: Some(vec![diag(Severity::Warning, 1), diag(Severity::Error, 2)]),
            before: Vec::new(),
        }],
        unavailable: Vec::new(),
    }
    .render()
    .unwrap();
    assert_eq!(few["new_errors"].as_array().unwrap().len(), 2);
    assert!(few.get("truncated").is_none());
}

/// Whether the live Seatbelt tests run here.
#[cfg(target_os = "macos")]
fn seatbelt() -> bool {
    let present = Path::new("/usr/bin/sandbox-exec").is_file();
    assert!(
        present || std::env::var_os("ELITEA_REQUIRE_SEATBELT").is_none(),
        "ELITEA_REQUIRE_SEATBELT is set but /usr/bin/sandbox-exec is missing"
    );
    present
}

#[cfg(target_os = "macos")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn language_server_cannot_write_workspace() {
    if !seatbelt() {
        return;
    }
    let fixture = rust_crate("fn a() {}\n");
    let credentials = fixture.data.join("credentials.json");
    std::fs::create_dir_all(&fixture.data).unwrap();
    std::fs::write(&credentials, "{\"token\":\"x\"}").unwrap();
    let probe = credentials.display().to_string();
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal", "probe", &probe]),
        Duration::from_secs(5),
    );
    let result = edit(&fixture, "src/lib.rs", "fn a() {}", "fn b() {}").await;
    assert_eq!(result["diagnostics"]["status"], "ok", "{result}");
    let log = received(&fixture, Language::Rust);
    assert!(log.contains("readdoc:ok"), "the control reads: {log}");
    assert!(log.contains("write:denied"), "{log}");
    assert!(
        log.contains("read:denied"),
        "credentials.json is hidden: {log}"
    );
    assert!(!fixture.root.join("src/lib.rs.written").exists());
}

#[cfg(target_os = "macos")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn language_server_has_no_network() {
    if !seatbelt() {
        return;
    }
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port().to_string();
    let accept = std::thread::spawn(move || {
        listener.set_nonblocking(true).unwrap();
        for _ in 0..300 {
            if listener.accept().is_ok() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        false
    });
    let fixture = rust_crate("fn a() {}\n");
    enable(
        &fixture,
        Language::Rust,
        fake_server(&fixture, &["normal", "probe", "", &port]),
        Duration::from_secs(5),
    );
    let result = edit(&fixture, "src/lib.rs", "fn a() {}", "fn b() {}").await;
    assert_eq!(result["diagnostics"]["status"], "ok", "{result}");
    let log = received(&fixture, Language::Rust);
    assert!(
        log.contains("connect:refused"),
        "loopback is off too: {log}"
    );
    assert!(!accept.join().unwrap(), "nothing reached the listener");
}
