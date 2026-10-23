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
