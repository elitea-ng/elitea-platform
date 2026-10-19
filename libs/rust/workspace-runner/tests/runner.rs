//! The runner end to end on a temporary workspace: unpack (with the git
//! baseline), tool calls over the line protocol, pack.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::fs;

use elitea_agent_runtime::workspace_archive::{self, ArchiveLimits};
use elitea_workspace_runner::{RunnerConfig, RunnerPolicy, open_session, pack, serve, unpack};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt};

const LIMITS: ArchiveLimits = ArchiveLimits {
    max_files: 100,
    max_file_bytes: 1 << 20,
    max_total_bytes: 4 << 20,
};

fn git_available() -> bool {
    std::process::Command::new("git")
        .arg("--version")
        .output()
        .is_ok()
}

/// The worker's side of `serve`: one request line, then wait for its answer.
async fn serve_one_by_one(config: &RunnerConfig, requests: &[Value]) -> Vec<Value> {
    let session = open_session(config).unwrap();
    let (mut to_runner, runner_in) = tokio::io::duplex(1 << 16);
    let (runner_out, from_runner) = tokio::io::duplex(1 << 16);
    let runner = tokio::spawn(serve(
        session,
        tokio::io::BufReader::new(runner_in),
        runner_out,
    ));
    let mut answers = tokio::io::BufReader::new(from_runner).lines();
    let mut out = Vec::new();
    for request in requests {
        to_runner
            .write_all(format!("{request}\n").as_bytes())
            .await
            .unwrap();
        let line = answers.next_line().await.unwrap().expect("an answer");
        out.push(serde_json::from_str(&line).unwrap());
    }
    drop(to_runner);
    runner.await.unwrap().unwrap();
    out
}

#[tokio::test(flavor = "multi_thread")]
async fn unpack_serve_and_pack_a_workspace() {
    if !git_available() {
        eprintln!("git is not installed; the runner test is skipped");
        return;
    }
    // The worker side: a checked-out tree, archived.
    let checkout = tempfile::tempdir().unwrap();
    fs::write(checkout.path().join("README.md"), "hello\n").unwrap();
    let mut archive = Vec::new();
    workspace_archive::write_tree(checkout.path(), &mut archive, LIMITS).unwrap();

    // Inside the container.
    let workspace = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    unpack(&mut archive.as_slice(), workspace.path(), LIMITS).unwrap();
    assert!(workspace.path().join(".git").is_dir());
    let config = RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: RunnerPolicy {
            shell: true,
            network: false,
            ..RunnerPolicy::default()
        },
        env: Vec::new(),
    };
    let requests = [
        json!({"tool": "read_file", "call_id": "1", "args": {"path": "README.md"}}),
        json!({"tool": "edit_file", "call_id": "2", "args": {"path": "README.md", "old_string": "hello", "new_string": "hello cloud"}}),
        json!({"tool": "write_file", "call_id": "3", "args": {"path": "src/new.txt", "content": "new\n"}}),
        json!({"tool": "run_command", "call_id": "4", "args": {"command": "ls src", "sandbox": "workspace-write"}}),
        json!({"tool": "git_status", "call_id": "5", "args": {}}),
        json!({"tool": "git_commit", "call_id": "6", "args": {"message": "no"}}),
        json!("not a request"),
    ];
    let answers = serve_one_by_one(&config, &requests).await;
    assert_eq!(answers.len(), requests.len());
    for answer in &answers[..5] {
        assert_eq!(answer["status"], "ok", "{answer}");
    }
    assert!(answers[3].to_string().contains("new.txt"), "{}", answers[3]);
    assert!(
        answers[4].to_string().contains("README.md"),
        "{}",
        answers[4]
    );
    assert_eq!(answers[5]["code"], "denied", "git_commit is the worker's");
    assert_eq!(answers[6]["status"], "error");

    // Back out: the changes, and never .git.
    let mut packed = Vec::new();
    pack(workspace.path(), &mut packed, LIMITS).unwrap();
    let out = tempfile::tempdir().unwrap();
    workspace_archive::read_tree(&mut packed.as_slice(), out.path(), LIMITS).unwrap();
    assert_eq!(
        fs::read_to_string(out.path().join("README.md")).unwrap(),
        "hello cloud\n"
    );
    assert_eq!(
        fs::read_to_string(out.path().join("src/new.txt")).unwrap(),
        "new\n"
    );
    assert!(!out.path().join(".git").exists());
}

/// Decision 9: with the project's network on, a networked command is not an
/// escaping action; a destructive one still waits for a person; with the
/// network off a networked command is refused by policy.
#[tokio::test(flavor = "multi_thread")]
async fn cloud_approvals_follow_the_project_policy() {
    if !git_available() {
        return;
    }
    let ask = |network: bool, command: &str| {
        let command = command.to_owned();
        async move {
            let workspace = tempfile::tempdir().unwrap();
            let data = tempfile::tempdir().unwrap();
            let mut archive = Vec::new();
            let source = tempfile::tempdir().unwrap();
            fs::write(source.path().join("a.txt"), "a\n").unwrap();
            workspace_archive::write_tree(source.path(), &mut archive, LIMITS).unwrap();
            unpack(&mut archive.as_slice(), workspace.path(), LIMITS).unwrap();
            let session = open_session(&RunnerConfig {
                root: workspace.path().to_path_buf(),
                data_dir: data.path().to_path_buf(),
                policy: RunnerPolicy {
                    shell: true,
                    network,
                    ..RunnerPolicy::default()
                },
                env: Vec::new(),
            })
            .unwrap();
            let line = json!({"tool": "run_command", "call_id": "1",
                "args": {"command": command, "sandbox": "workspace-write", "network": true}})
            .to_string();
            elitea_workspace_runner::handle_line(&session, &line).await
        }
    };
    assert_eq!(ask(true, "echo fetched").await["status"], "ok");
    let destructive = ask(true, "rm -rf a.txt").await;
    assert_eq!(destructive["status"], "error", "{destructive}");
    let off = ask(false, "echo fetched").await;
    assert_eq!(off["status"], "error", "{off}");
}

/// Contract 6.4: the harvested env is in `run_command`'s environment, only
/// there, and only when the worker sent it.
#[tokio::test(flavor = "multi_thread")]
async fn the_harvested_env_reaches_run_command_only_when_sent() {
    if !git_available() {
        return;
    }
    let run = |env: Vec<(String, String)>| async move {
        let source = tempfile::tempdir().unwrap();
        fs::write(source.path().join("a.txt"), "a\n").unwrap();
        let mut archive = Vec::new();
        workspace_archive::write_tree(source.path(), &mut archive, LIMITS).unwrap();
        let workspace = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        unpack(&mut archive.as_slice(), workspace.path(), LIMITS).unwrap();
        let session = open_session(&RunnerConfig {
            root: workspace.path().to_path_buf(),
            data_dir: data.path().to_path_buf(),
            policy: RunnerPolicy {
                shell: true,
                ..RunnerPolicy::default()
            },
            env,
        })
        .unwrap();
        let line = json!({"tool": "run_command", "call_id": "1",
            "args": {"command": "printenv HANDOFF_SECRET_VALUE", "sandbox": "workspace-write"}})
        .to_string();
        elitea_workspace_runner::handle_line(&session, &line).await
    };
    let with = run(elitea_workspace_runner::parse_env_line(
        r#"{"env":{"HANDOFF_SECRET_VALUE":"s3cr3t-value"}}"#,
    )
    .unwrap())
    .await;
    assert!(with.to_string().contains("s3cr3t-value"), "{with}");
    let without = run(Vec::new()).await;
    assert!(!without.to_string().contains("s3cr3t-value"), "{without}");
    assert!(elitea_workspace_runner::parse_env_line(r#"{"env":{},"x":1}"#).is_err());
}

/// Item 2: the worker's pre-dispatch question, and a HITL-approved call.
/// Destructive and escaping commands (ADR-0029 addendum item 3) are asked;
/// network use is not; an approved call runs, the same call unapproved is
/// still deferred.
#[tokio::test(flavor = "multi_thread")]
async fn destructive_commands_are_classified_and_run_only_when_approved() {
    if !git_available() {
        return;
    }
    let source = tempfile::tempdir().unwrap();
    fs::create_dir_all(source.path().join("dir")).unwrap();
    fs::write(source.path().join("dir/a.txt"), "a\n").unwrap();
    let mut archive = Vec::new();
    workspace_archive::write_tree(source.path(), &mut archive, LIMITS).unwrap();
    let workspace = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    unpack(&mut archive.as_slice(), workspace.path(), LIMITS).unwrap();
    let session = open_session(&RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: RunnerPolicy {
            shell: true,
            network: true,
            ..RunnerPolicy::default()
        },
        env: Vec::new(),
    })
    .unwrap();
    let classify = |command: &str| {
        let line = json!({"classify": {"command": command}}).to_string();
        let session = std::sync::Arc::clone(&session);
        async move { elitea_workspace_runner::handle_line(&session, &line).await }
    };
    for asked in [
        "rm -rf dir",
        "git push origin main",
        "git push --force",
        "git reset --hard HEAD~1",
        "npm publish",
        "sudo apt install x",
        "echo hi > /etc/hosts",
    ] {
        assert_eq!(classify(asked).await["ask"], true, "{asked}");
    }
    for routine in [
        "ls -la",
        "cargo test",
        "curl https://example.com",
        "rm dir/a.txt",
    ] {
        assert_eq!(classify(routine).await["ask"], false, "{routine}");
    }
    let run = |approved: bool, call_id: &str| {
        let line = json!({"tool": "run_command", "call_id": call_id, "approved": approved,
            "args": {"command": "rm -rf dir", "sandbox": "workspace-write"}})
        .to_string();
        let session = std::sync::Arc::clone(&session);
        async move { elitea_workspace_runner::handle_line(&session, &line).await }
    };
    let deferred = run(false, "c-1").await;
    assert_eq!(deferred["status"], "error", "{deferred}");
    assert!(workspace.path().join("dir/a.txt").exists());
    let ran = run(true, "c-2").await;
    assert_eq!(ran["status"], "ok", "{ran}");
    assert!(!workspace.path().join("dir").exists());
}

/// A take back's wrap-up: the worker stops waiting for a long command and
/// closes stdin. The command is killed with its whole process group, so it
/// never writes to the workspace the worker then packs.
#[tokio::test(flavor = "multi_thread")]
async fn closing_stdin_mid_command_kills_the_command() {
    if !git_available() {
        return;
    }
    let workspace = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let session = open_session(&RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: RunnerPolicy {
            shell: true,
            network: false,
            ..RunnerPolicy::default()
        },
        env: Vec::new(),
    })
    .unwrap();
    let (mut to_runner, runner_in) = tokio::io::duplex(1 << 16);
    let (runner_out, _from_runner) = tokio::io::duplex(1 << 16);
    let runner = tokio::spawn(serve(
        session,
        tokio::io::BufReader::new(runner_in),
        runner_out,
    ));
    let request = json!({"tool": "run_command", "call_id": "1", "args": {
        "command": "sh -c 'sleep 2; echo late > late.txt' & sleep 2; echo late > leader.txt",
        "sandbox": "workspace-write"}});
    to_runner
        .write_all(format!("{request}\n").as_bytes())
        .await
        .unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    drop(to_runner);
    tokio::time::timeout(std::time::Duration::from_secs(5), runner)
        .await
        .expect("serve ends when stdin closes")
        .unwrap()
        .unwrap();
    tokio::time::sleep(std::time::Duration::from_secs(3)).await;
    assert!(
        !workspace.path().join("leader.txt").exists(),
        "the command was not stopped"
    );
    assert!(
        !workspace.path().join("late.txt").exists(),
        "a child of the command survived"
    );
}
