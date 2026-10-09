//! The macOS Seatbelt sandbox, proven on the real `sandbox-exec`: writes
//! outside the workspace and network connections (loopback included) are
//! blocked, and allowed again when the mode or the network switch says so.
//!
//! macOS only; elsewhere this file has no tests. Where `sandbox-exec` is
//! missing each test skips, unless `ELITEA_REQUIRE_SEATBELT=1` (CI's macOS
//! job) turns the skip into a failure.
#![cfg(target_os = "macos")]

use std::net::TcpListener;
use std::path::Path;

use elitea_local_tools::policy::SandboxMode;
use elitea_local_tools::sandbox::Enforcement;
use elitea_local_tools::shell::{CommandOutput, CommandSpec, ShellConfig, run};
use elitea_local_tools::workspace::{Workspace, WsPath};

fn available() -> bool {
    if Path::new("/usr/bin/sandbox-exec").is_file() {
        return true;
    }
    assert!(
        std::env::var_os("ELITEA_REQUIRE_SEATBELT").is_none(),
        "ELITEA_REQUIRE_SEATBELT is set but /usr/bin/sandbox-exec is missing"
    );
    eprintln!("skipping: /usr/bin/sandbox-exec is missing");
    false
}

struct Fixture {
    _dir: tempfile::TempDir,
    outside: std::path::PathBuf,
    workspace: Workspace,
    config: ShellConfig,
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("workspace");
    let outside = dir.path().join("outside");
    std::fs::create_dir_all(root.join(".git/hooks")).expect("workspace");
    std::fs::create_dir_all(&outside).expect("outside");
    let workspace = Workspace::open(&root, &[]).expect("open");
    let config = ShellConfig::new(dir.path().join("session-tmp"));
    Fixture {
        outside: std::fs::canonicalize(outside).expect("canonical"),
        _dir: dir,
        workspace,
        config,
    }
}

async fn sh(fixture: &Fixture, command: &str, mode: SandboxMode, network: bool) -> CommandOutput {
    let output = run(
        &fixture.workspace,
        &fixture.config,
        &CommandSpec {
            command: command.to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode,
            network,
        },
    )
    .await
    .expect("run");
    assert_eq!(output.enforcement, Enforcement::Full);
    output
}

fn quoted(path: &Path) -> String {
    format!("'{}'", path.display())
}

#[tokio::test]
async fn workspace_write_confines_writes_to_the_workspace_and_temp_dir() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let inside = fixture.workspace.root().join("made-inside");
    let outside = fixture.outside.join("made-outside");
    let tmp = sh(
        &fixture,
        "touch \"$TMPDIR/scratch\"",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(tmp.exit_code, Some(0), "{}", tmp.stderr);

    let ok = sh(
        &fixture,
        &format!("touch {}", quoted(&inside)),
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(ok.exit_code, Some(0), "{}", ok.stderr);
    assert!(inside.exists());

    let blocked = sh(
        &fixture,
        &format!("touch {}", quoted(&outside)),
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_ne!(blocked.exit_code, Some(0));
    assert!(
        blocked.stderr.contains("Operation not permitted"),
        "{}",
        blocked.stderr
    );
    assert!(
        !outside.exists(),
        "the sandbox let a write outside the workspace through"
    );

    let full = sh(
        &fixture,
        &format!("touch {}", quoted(&outside)),
        SandboxMode::FullAccess,
        false,
    )
    .await;
    assert_eq!(full.exit_code, Some(0), "{}", full.stderr);
    assert!(outside.exists());
}

#[tokio::test]
async fn read_only_blocks_every_write_but_reads_work() {
    if !available() {
        return;
    }
    let fixture = fixture();
    std::fs::write(fixture.workspace.root().join("readme"), "hello").expect("seed");
    let read = sh(&fixture, "cat readme", SandboxMode::ReadOnly, false).await;
    assert_eq!(read.stdout, "hello");
    let write = sh(&fixture, "touch new-file", SandboxMode::ReadOnly, false).await;
    assert_ne!(write.exit_code, Some(0));
    assert!(!fixture.workspace.root().join("new-file").exists());
    let devnull = sh(
        &fixture,
        "echo quiet > /dev/null",
        SandboxMode::ReadOnly,
        false,
    )
    .await;
    assert_eq!(devnull.exit_code, Some(0), "{}", devnull.stderr);
}

#[tokio::test]
async fn git_hooks_and_config_stay_read_only_inside_the_workspace() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let hook = sh(
        &fixture,
        "touch .git/hooks/pre-commit",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_ne!(hook.exit_code, Some(0));
    assert!(
        !fixture
            .workspace
            .root()
            .join(".git/hooks/pre-commit")
            .exists()
    );
    let config = sh(
        &fixture,
        "touch .git/config",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_ne!(config.exit_code, Some(0));
    let other = sh(
        &fixture,
        "touch .git/index",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(
        other.exit_code,
        Some(0),
        "the rest of .git stays writable for git itself: {}",
        other.stderr
    );
}

#[tokio::test]
async fn network_is_blocked_when_denied_and_open_when_allowed() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
    let port = listener.local_addr().expect("addr").port();
    let probe = format!("/usr/bin/nc -z -G 3 127.0.0.1 {port}");
    let denied = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, false).await;
    assert_ne!(
        denied.exit_code,
        Some(0),
        "a denied sandbox reached loopback: {}",
        denied.stdout
    );
    let allowed = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, true).await;
    assert_eq!(allowed.exit_code, Some(0), "{}", allowed.stderr);
    drop(listener);
}
