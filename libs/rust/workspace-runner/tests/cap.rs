//! The workspace cap the runner enforces where the backend cannot size the
//! volume (Docker's local driver): checked after every tool that can grow
//! the tree, growth refused while over, and `pack` refused.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::fs;

use elitea_agent_runtime::workspace_archive::ArchiveLimits;
use elitea_workspace_runner::{
    RunnerConfig, RunnerPolicy, WorkspaceCap, open_session, pack_capped, serve_capped,
    workspace_usage,
};
use serde_json::{Value, json};

const LIMITS: ArchiveLimits = ArchiveLimits {
    max_files: 100,
    max_file_bytes: 4 << 20,
    max_total_bytes: 16 << 20,
};

#[tokio::test(flavor = "multi_thread")]
async fn over_the_cap_growth_is_refused_and_nothing_is_packed() {
    let workspace = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    fs::write(workspace.path().join("README.md"), "hello\n").unwrap();
    let cap = WorkspaceCap {
        root: workspace.path().to_path_buf(),
        max_bytes: workspace_usage(workspace.path()) + 1024,
    };
    assert!(cap.check().is_ok());
    let session = open_session(&RunnerConfig {
        root: workspace.path().to_path_buf(),
        data_dir: data.path().to_path_buf(),
        policy: RunnerPolicy::default(),
        env: Vec::new(),
    })
    .unwrap();
    let big = "x".repeat(4096);
    let requests = [
        json!({"tool": "write_file", "call_id": "1", "args": {"path": "small.txt", "content": "ok\n"}}),
        json!({"tool": "write_file", "call_id": "2", "args": {"path": "big.txt", "content": big}}),
        json!({"tool": "write_file", "call_id": "3", "args": {"path": "more.txt", "content": "more\n"}}),
        json!({"tool": "read_file", "call_id": "4", "args": {"path": "README.md"}}),
    ];
    let mut input = String::new();
    for request in &requests {
        input.push_str(&request.to_string());
        input.push('\n');
    }
    let mut output = Vec::new();
    serve_capped(
        session,
        Some(cap.clone()),
        tokio::io::BufReader::new(input.as_bytes()),
        &mut output,
    )
    .await
    .unwrap();
    let answers: Vec<Value> = String::from_utf8(output)
        .unwrap()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect();
    assert!(
        answers[0].get("workspace_over_cap").is_none(),
        "{}",
        answers[0]
    );
    // The write that crossed the cap ran, and says so.
    assert!(answers[1]["workspace_over_cap"]["bytes"].as_u64().unwrap() > cap.max_bytes);
    assert!(
        answers[1]["notice"]
            .as_str()
            .unwrap()
            .contains("over its cap")
    );
    // Further growth is refused while over; reading is not.
    assert_eq!(answers[2]["code"], "workspace_too_large", "{}", answers[2]);
    assert!(!workspace.path().join("more.txt").exists());
    assert_ne!(answers[3]["status"], "error", "{}", answers[3]);

    // Pack refuses an oversized workspace, and packs once it fits again.
    let mut archive = Vec::new();
    assert_eq!(
        pack_capped(workspace.path(), &mut archive, LIMITS, Some(&cap)).unwrap_err(),
        "runner.workspace_too_large"
    );
    assert!(archive.is_empty());
    fs::remove_file(workspace.path().join("big.txt")).unwrap();
    pack_capped(workspace.path(), &mut archive, LIMITS, Some(&cap)).unwrap();
    assert!(!archive.is_empty());
}
