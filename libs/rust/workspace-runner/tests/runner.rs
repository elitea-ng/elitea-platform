//! The runner end to end on a temporary workspace: unpack (with the git
//! baseline), tool calls over the line protocol, pack.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::fs;

use elitea_agent_runtime::workspace_archive::{self, ArchiveLimits};
use elitea_workspace_runner::{RunnerConfig, RunnerPolicy, open_session, pack, serve, unpack};
use serde_json::{Value, json};

const LIMITS: ArchiveLimits = ArchiveLimits {
    max_files: 100,
    max_file_bytes: 1 << 20,
    max_total_bytes: 4 << 20,
};

fn git_available() -> bool {
    std::process::Command::new("git").arg("--version").output().is_ok()
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
    let session = open_session(&RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: RunnerPolicy {
            shell: true,
            network: false,
            ..RunnerPolicy::default()
        },
    })
    .unwrap();
    let requests = [
        json!({"tool": "read_file", "call_id": "1", "args": {"path": "README.md"}}),
        json!({"tool": "edit_file", "call_id": "2", "args": {"path": "README.md", "old_string": "hello", "new_string": "hello cloud"}}),
        json!({"tool": "write_file", "call_id": "3", "args": {"path": "src/new.txt", "content": "new\n"}}),
        json!({"tool": "run_command", "call_id": "4", "args": {"command": "ls src", "sandbox": "workspace-write"}}),
        json!({"tool": "git_status", "call_id": "5", "args": {}}),
        json!({"tool": "git_commit", "call_id": "6", "args": {"message": "no"}}),
        json!("not a request"),
    ];
    let input: String = requests.iter().map(|request| format!("{request}\n")).collect();
    let mut output = Vec::new();
    serve(session, tokio::io::BufReader::new(input.as_bytes()), &mut output)
        .await
        .unwrap();
    let answers: Vec<Value> = String::from_utf8(output)
        .unwrap()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect();
    assert_eq!(answers.len(), requests.len());
    for answer in &answers[..5] {
        assert_eq!(answer["status"], "ok", "{answer}");
    }
    assert!(answers[3].to_string().contains("new.txt"), "{}", answers[3]);
    assert!(answers[4].to_string().contains("README.md"), "{}", answers[4]);
    assert_eq!(answers[5]["code"], "denied", "git_commit is the worker's");
    assert_eq!(answers[6]["status"], "error");

    // Back out: the changes, and never .git.
    let mut packed = Vec::new();
    pack(workspace.path(), &mut packed, LIMITS).unwrap();
    let out = tempfile::tempdir().unwrap();
    workspace_archive::read_tree(&mut packed.as_slice(), out.path(), LIMITS).unwrap();
    assert_eq!(fs::read_to_string(out.path().join("README.md")).unwrap(), "hello cloud\n");
    assert_eq!(fs::read_to_string(out.path().join("src/new.txt")).unwrap(), "new\n");
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
