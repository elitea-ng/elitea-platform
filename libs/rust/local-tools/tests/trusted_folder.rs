//! A trusted folder's commands (ADR-0061): they run on the host with the
//! person's environment and redacted output, on every platform (Windows,
//! with no sandbox, included); a turn that read outside content is asked
//! before one runs, under bypass too; revoking trust stops what runs on
//! the host.

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{
    FolderTrust, MemoryChoices, PermissionProfile, WorkspaceSettings,
};
use elitea_local_tools::host_env::HostEnvironment;
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

/// Rejects every question it is asked, and keeps them.
#[derive(Default)]
struct RecordingPrompt {
    seen: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for RecordingPrompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.seen.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: "reject".to_owned(),
            value: json!({ "reason": "the person rejected it" }),
        })
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    session: Arc<LocalSession>,
    prompt: Arc<RecordingPrompt>,
}

const TOKEN: &str = "ghp_abcdefghijklmnopqrstuvwxyz0123456789";
const APP_TOKEN: &str = "the-apps-own-access-token-value";

fn fixture() -> Fixture {
    fixture_with(false)
}

fn fixture_with(env_fallback: bool) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    std::fs::create_dir_all(&root).expect("ws");
    let prompt = Arc::new(RecordingPrompt::default());
    let mut shell = ShellConfig::new(dir.path().join("tmp"));
    std::fs::create_dir_all(dir.path().join("tmp")).expect("tmp");
    let mut vars: Vec<(String, String)> = std::env::vars()
        .filter(|(name, _)| ["PATH", "SystemRoot", "ComSpec", "PATHEXT"].contains(&name.as_str()))
        .collect();
    vars.push(("ELITEA_TRUST_PROBE".to_owned(), "from-login".to_owned()));
    vars.push(("CORP_TOOL".to_owned(), "from-login".to_owned()));
    vars.push(("KUBECONFIG".to_owned(), "/home/me/.kube/config".to_owned()));
    // Never passed on: the app's own variables and its tokens.
    vars.push(("ELITEA_TEST_TOKEN".to_owned(), "app-variable".to_owned()));
    vars.push(("LEAKED".to_owned(), APP_TOKEN.to_owned()));
    shell.host_environment = Some(Arc::new(if env_fallback {
        HostEnvironment::failed()
    } else {
        HostEnvironment::captured(vars)
    }));
    shell
        .held_secrets
        .set(vec![elitea_local_tools::credentials::Secret::held(
            APP_TOKEN,
        )]);
    let session = LocalSession::open(SessionConfig {
        spill_dir: None,
        root,
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            network: true,
            max_sandbox_mode: SandboxMode::FullAccess,
            folder_trust: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: dir.path().join("data"),
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
        documents: elitea_local_tools::document::Documents::default(),
    })
    .expect("session");
    Fixture {
        _dir: dir,
        session,
        prompt,
    }
}

async fn run(fixture: &Fixture, command: &str) -> Value {
    fixture
        .session
        .call("run_command", "c1", json!({ "command": command }))
        .await
}

const PRINT_ENV: &str = if cfg!(windows) {
    "cmd /c echo %CORP_TOOL%"
} else {
    "printenv CORP_TOOL"
};

/// Trusted, a routine command runs on the host without a question, with
/// the person's environment, on every platform.
#[tokio::test]
async fn a_trusted_folder_runs_routine_commands_on_the_host() {
    let fixture = fixture();
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    let result = run(&fixture, PRINT_ENV).await;
    assert_eq!(result["exit_code"], 0, "{result}");
    assert_eq!(result["execution"], "host", "{result}");
    assert_eq!(result["enforcement"], "none", "{result}");
    assert!(
        result["stdout"]
            .as_str()
            .unwrap_or_default()
            .contains("from-login"),
        "{result}"
    );
    assert!(fixture.prompt.seen.lock().expect("lock").is_empty());
}

/// A token in a host command's output is removed (the CLI-login formats).
#[cfg(unix)]
#[tokio::test]
async fn host_output_is_redacted() {
    let fixture = fixture();
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    let result = run(&fixture, &format!("echo {TOKEN}")).await;
    let stdout = result["stdout"].as_str().unwrap_or_default();
    assert!(!stdout.contains(TOKEN), "{result}");
    assert!(stdout.contains("[REDACTED]"), "{result}");
}

/// The captured environment reaches a host command (`KUBECONFIG`, the real
/// `PATH`); the app's own variables and a value equal to its token do not.
#[cfg(unix)]
#[tokio::test]
async fn host_commands_get_the_login_environment_without_the_apps_secrets() {
    let fixture = fixture();
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    let result = run(&fixture, "env").await;
    let stdout = result["stdout"].as_str().unwrap_or_default();
    assert!(
        stdout.contains("KUBECONFIG=/home/me/.kube/config"),
        "{result}"
    );
    let path = std::env::var("PATH").unwrap_or_default();
    assert!(stdout.contains(&format!("PATH={path}")), "{result}");
    assert!(!stdout.contains("ELITEA_TEST_TOKEN"), "{result}");
    assert!(!stdout.contains("LEAKED"), "{result}");
    assert!(!stdout.contains(APP_TOKEN), "{result}");
    assert!(result.get("env_fallback").is_none(), "{result}");
}

/// The login shell could not be read: the fallback, and the result says so.
#[cfg(unix)]
#[tokio::test]
async fn a_failed_capture_reports_the_fallback() {
    let fixture = fixture_with(true);
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    let result = run(&fixture, "true").await;
    assert_eq!(result["env_fallback"], true, "{result}");
}

/// Untrusted on Windows: there is no sandbox, so a command is refused
/// (`sandbox_unavailable`) as before; trust is the way to run it.
#[cfg(windows)]
#[tokio::test]
async fn untrusted_on_windows_is_refused() {
    let fixture = fixture();
    let result = run(&fixture, "cmd /c echo hi").await;
    assert_eq!(result["status"], "error", "{result}");
    assert_eq!(
        result["code"], "local_tools.sandbox_unavailable",
        "{result}"
    );
}

/// Untrusted on macOS: the command keeps its sandbox.
#[cfg(target_os = "macos")]
#[tokio::test]
async fn untrusted_on_macos_stays_sandboxed() {
    let fixture = fixture();
    let result = run(&fixture, "echo hi").await;
    assert_eq!(result["exit_code"], 0, "{result}");
    assert!(result.get("execution").is_none(), "{result}");
    assert_ne!(result["enforcement"], "none", "{result}");
}

/// Prompt injection (decision 5f, 5g): after the turn read outside
/// content, a host command is asked, and bypass does not answer it.
#[tokio::test]
async fn after_an_untrusted_read_a_host_command_is_asked_under_bypass() {
    let fixture = fixture();
    let engine = fixture.session.rules();
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    engine.set_profile(PermissionProfile::Bypass);
    engine.reset_untrusted_reads(false);
    // A web page was read.
    engine.record_untrusted_read();
    let result = run(&fixture, PRINT_ENV).await;
    assert_eq!(result["status"], "error", "{result}");
    let seen = fixture.prompt.seen.lock().expect("lock");
    assert_eq!(seen.len(), 1, "the person was asked");
    assert_eq!(seen[0].payload["source"], "taint", "{:?}", seen[0].payload);
}

/// Revoking trust stops every process that runs on the host.
#[cfg(unix)]
#[tokio::test]
async fn revoking_trust_stops_host_processes() {
    let fixture = fixture();
    fixture
        .session
        .set_folder_trust(Some(FolderTrust::default()))
        .await;
    let result = fixture
        .session
        .call(
            "run_command",
            "c1",
            json!({ "command": "sleep 30", "background": true, "yield_ms": 0 }),
        )
        .await;
    assert_eq!(result["execution"], "host", "{result}");
    assert!(
        fixture
            .session
            .processes()
            .iter()
            .any(|p| p.status == elitea_local_tools::process::ProcessState::Running)
    );
    fixture.session.set_folder_trust(None).await;
    assert!(
        !fixture
            .session
            .processes()
            .iter()
            .any(|p| p.status == elitea_local_tools::process::ProcessState::Running),
        "{:?}",
        fixture.session.processes()
    );
}
