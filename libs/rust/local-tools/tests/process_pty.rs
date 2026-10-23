//! Background processes and terminal sessions over real runs (#1223):
//! an interactive Python on a terminal answers what is typed into it, a
//! piped background process reads its stdin, ending the session stops
//! what is still running, and on macOS a background process is confined
//! by Seatbelt like any other command.
//!
//! The Python test skips without `python3` on `PATH`, unless
//! `ELITEA_REQUIRE_PTY=1`.

use std::path::PathBuf;
use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{MemoryChoices, WorkspaceSettings};
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

struct Approving;

#[async_trait]
impl ApprovalChannel for Approving {
    async fn request(&self, _request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        Ok(ApprovalOutcome::Decided {
            action: "approve".to_owned(),
            value: Value::Null,
        })
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    home: PathBuf,
    session: Arc<LocalSession>,
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("ws");
    let home = base.join("home");
    std::fs::create_dir_all(&root).expect("ws");
    std::fs::create_dir_all(&home).expect("home");
    let data_dir = base.join("data");
    let mut shell = ShellConfig::new(data_dir.join("tmp"));
    shell.home = Some(home.clone());
    // Linux CI has no sandbox helper binary; on macOS Seatbelt confines
    // the run either way.
    shell.sandbox.allow_unenforced = true;
    let session = LocalSession::open(SessionConfig {
        root,
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(Approving),
        data_dir,
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
        documents: elitea_local_tools::document::Documents::default(),
    })
    .expect("session");
    Fixture {
        _dir: dir,
        home,
        session,
    }
}

fn has_python() -> bool {
    let found = std::process::Command::new("python3")
        .arg("--version")
        .output()
        .is_ok_and(|output| output.status.success());
    assert!(
        found || std::env::var_os("ELITEA_REQUIRE_PTY").is_none(),
        "ELITEA_REQUIRE_PTY is set but python3 is not on PATH"
    );
    if !found {
        eprintln!("skipped: python3 is not on PATH");
    }
    found
}

async fn start(session: &Arc<LocalSession>, args: Value) -> String {
    let started = session.call("run_command", "c1", args).await;
    assert_eq!(started["status"], "running", "{started}");
    started["process_id"]
        .as_str()
        .expect("process id")
        .to_owned()
}

#[tokio::test]
async fn interactive_python_round_trip() {
    if !has_python() {
        return;
    }
    let fixture = fixture();
    let session = &fixture.session;
    let id = start(
        session,
        json!({ "command": "python3 -q -i", "tty": true, "yield_ms": 2000 }),
    )
    .await;

    let answer = session
        .call(
            "process_write",
            "c2",
            json!({ "process_id": id, "input": "print(6 * 7)\n", "yield_ms": 2000 }),
        )
        .await;
    assert!(
        answer["output"]
            .as_str()
            .is_some_and(|out| out.contains("42")),
        "{answer}"
    );
    assert!(
        answer["screen"].as_str().is_some_and(|s| s.contains("42")),
        "{answer}"
    );

    session
        .call(
            "process_write",
            "c3",
            json!({ "process_id": id, "input": "exit()\n" }),
        )
        .await;
    let ended = session
        .call(
            "process_wait",
            "c4",
            json!({ "process_id": id, "timeout_seconds": 20 }),
        )
        .await;
    assert_eq!(ended["status"], "exited", "{ended}");
    assert_eq!(ended["exit_code"], 0, "{ended}");
}

#[tokio::test]
async fn a_piped_background_process_reads_what_is_written() {
    let fixture = fixture();
    let session = &fixture.session;
    let id = start(
        session,
        json!({
            "command": "echo ready; read line; echo \"got:$line\"",
            "background": true,
        }),
    )
    .await;
    session
        .call(
            "process_write",
            "c2",
            json!({ "process_id": id, "input": "hello\n" }),
        )
        .await;
    let ended = session
        .call(
            "process_wait",
            "c3",
            json!({ "process_id": id, "timeout_seconds": 20 }),
        )
        .await;
    assert_eq!(ended["status"], "exited", "{ended}");
    let output = ended["output"].as_str().expect("output");
    assert!(
        output.contains("ready") && output.contains("got:hello"),
        "{ended}"
    );
}

#[tokio::test]
async fn ending_the_session_stops_what_is_running() {
    let fixture = fixture();
    let session = &fixture.session;
    let id = start(
        session,
        json!({ "command": "sleep 600", "background": true, "keep_alive": "session" }),
    )
    .await;
    session.end_turn_processes().await;
    assert!(session.process(&id).expect("listed").is_running());
    session.stop_processes().await;
    let listed = session.call("process_list", "c2", json!({})).await;
    assert_eq!(listed["processes"][0]["status"], "killed", "{listed}");
}

/// A background process gets the same Seatbelt profile as a foreground
/// command: the home credentials stay unreadable.
#[cfg(target_os = "macos")]
#[tokio::test]
async fn background_process_cannot_read_credentials() {
    if !std::path::Path::new("/usr/bin/sandbox-exec").is_file() {
        assert!(
            std::env::var_os("ELITEA_REQUIRE_SEATBELT").is_none(),
            "ELITEA_REQUIRE_SEATBELT is set but /usr/bin/sandbox-exec is missing"
        );
        return;
    }
    let fixture = fixture();
    let key = fixture.home.join(".ssh/id_ed25519");
    std::fs::create_dir_all(key.parent().expect("parent")).expect("ssh");
    std::fs::write(&key, "SECRET-KEY").expect("key");
    let session = &fixture.session;
    let id = start(
        session,
        json!({ "command": "cat \"$HOME/.ssh/id_ed25519\"", "background": true, "yield_ms": 0 }),
    )
    .await;
    let ended = session
        .call(
            "process_wait",
            "c2",
            json!({ "process_id": id, "timeout_seconds": 20 }),
        )
        .await;
    assert_ne!(ended["exit_code"], 0, "{ended}");
    let output = ended["output"].as_str().expect("output");
    assert!(!output.contains("SECRET-KEY"), "{ended}");
    assert!(output.contains("Operation not permitted"), "{ended}");
}
