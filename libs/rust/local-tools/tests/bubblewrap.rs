//! The loopback switch under Linux bubblewrap (#1223): the command keeps
//! its private network namespace, so it serves and connects on its own
//! `lo` and reaches nothing outside it.
//!
//! Linux only; elsewhere this file has no tests. Where `bwrap` does not
//! confine the command each test skips, unless `ELITEA_REQUIRE_BWRAP=1`.
#![cfg(target_os = "linux")]

use elitea_local_tools::policy::SandboxMode;
use elitea_local_tools::sandbox::Enforcement;
use elitea_local_tools::shell::{CommandOutput, CommandSpec, ShellConfig, run};
use elitea_local_tools::workspace::{Workspace, WsPath};

async fn sh(dir: &tempfile::TempDir, command: &str, loopback: bool) -> Option<CommandOutput> {
    let root = dir.path().join("workspace");
    std::fs::create_dir_all(&root).expect("workspace");
    let workspace = Workspace::open(&root, &[]).expect("open");
    let config = ShellConfig::new(dir.path().join("data/tmp"));
    let output = run(
        &workspace,
        &config,
        &CommandSpec {
            command: command.to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode: SandboxMode::WorkspaceWrite,
            network: false,
            loopback,
        },
    )
    .await;
    match output {
        Ok(output) if output.enforcement == Enforcement::Full => Some(output),
        _ => {
            assert!(
                std::env::var_os("ELITEA_REQUIRE_BWRAP").is_none(),
                "ELITEA_REQUIRE_BWRAP is set but bwrap does not confine commands here"
            );
            eprintln!("skipping: bwrap does not confine commands here");
            None
        }
    }
}

const SERVE: &str = "python3 -c 'import socket; s = socket.socket(); \
     s.bind((\"127.0.0.1\", 0)); s.listen(1); \
     c = socket.create_connection(s.getsockname()); print(\"served\")'";

#[tokio::test]
async fn loopback_serves_inside_the_command_and_reaches_nothing_outside() {
    let dir = tempfile::tempdir().expect("dir");
    let Some(served) = sh(&dir, SERVE, true).await else {
        return;
    };
    assert_eq!(served.exit_code, Some(0), "{}", served.stderr);
    assert!(served.stdout.contains("served"), "{}", served.stdout);

    let external = sh(
        &dir,
        "python3 -c 'import socket; socket.create_connection((\"1.1.1.1\", 443), 3)'",
        true,
    )
    .await
    .expect("confined");
    assert_ne!(external.exit_code, Some(0), "loopback reached the network");
}

/// #1420: a workspace inside a denied directory (a task worktree in the
/// host's data) is bound again over the mask; what is denied inside it (a
/// `path_deny` match, a deny-list file) is masked again over that bind.
#[tokio::test]
async fn denied_files_inside_a_reopened_workspace_stay_hidden() {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let data = base.join("data");
    let root = data.join("worktrees/a");
    std::fs::create_dir_all(&root).expect("workspace");
    std::fs::write(root.join("a.txt"), "visible\n").expect("a");
    std::fs::write(root.join(".env"), "ENV-SECRET-3").expect("env");
    std::fs::write(root.join("secret.txt"), "LISTED-SECRET-4").expect("secret");
    let workspace = Workspace::open(&root, &[".env".to_owned()]).expect("open");
    let mut config = ShellConfig::new(data.join("tmp"));
    config.deny_read.push(data.clone());
    config.deny_read.push(root.join("secret.txt"));
    let output = run(
        &workspace,
        &config,
        &CommandSpec {
            command: "cat a.txt; cat .env; cat secret.txt".to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode: SandboxMode::WorkspaceWrite,
            network: false,
            loopback: false,
        },
    )
    .await;
    let output = match output {
        Ok(output) if output.enforcement == Enforcement::Full => output,
        _ => {
            assert!(
                std::env::var_os("ELITEA_REQUIRE_BWRAP").is_none(),
                "ELITEA_REQUIRE_BWRAP is set but bwrap does not confine commands here"
            );
            eprintln!("skipping: bwrap does not confine commands here");
            return;
        }
    };
    assert!(output.stdout.contains("visible"), "{output:?}");
    assert!(!output.stdout.contains("ENV-SECRET-3"), "{output:?}");
    assert!(!output.stdout.contains("LISTED-SECRET-4"), "{output:?}");
}
