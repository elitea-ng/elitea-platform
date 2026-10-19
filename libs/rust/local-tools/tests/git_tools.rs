//! The git tools respect `path_deny`: `git_diff` never shows the content of
//! a denied file, under any spelling of its name.

use std::path::Path;
use std::process::Command;
use std::sync::Arc;

use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use serde_json::json;

fn git(dir: &Path, args: &[&str]) {
    let output = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "user.name=T",
            "-c",
            "user.email=t@example.com",
            "-c",
            "commit.gpgsign=false",
            "-c",
            "init.defaultBranch=main",
        ])
        .args(args)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .output()
        .expect("git");
    assert!(output.status.success(), "git {args:?}");
}

#[tokio::test]
async fn git_diff_leaves_out_path_deny_files() {
    let dir = tempfile::tempdir().expect("dir");
    let root = std::fs::canonicalize(dir.path())
        .expect("canonical")
        .join("repo");
    std::fs::create_dir_all(root.join("secrets")).expect("secrets");
    std::fs::create_dir_all(root.join("app/config")).expect("app");
    git(&root, &["init", "-q"]);
    let files = [
        ".env",
        "app/config/.env",
        "secrets/key.txt",
        "app/id.pem",
        "visible.txt",
    ];
    for file in files {
        std::fs::write(root.join(file), "before\n").expect("seed");
    }
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    for file in files {
        std::fs::write(root.join(file), format!("SECRET-AFTER {file}\n")).expect("change");
    }
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            path_deny: vec![
                ".env".to_owned(),
                "secrets/**".to_owned(),
                "*.PEM".to_owned(),
            ],
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(DeferringPrompt),
        data_dir: dir.path().join("data"),
        shell: None,
        deny_read: Vec::new(),
        app_id: None,
    })
    .expect("session");
    for args in [
        json!({}),
        json!({ "revision": "HEAD" }),
        json!({ "stat": true }),
    ] {
        let result = session.call("git_diff", "c", args.clone()).await;
        assert_eq!(result["status"], "ok", "{result}");
        let output = result["output"].as_str().expect("output");
        assert!(output.contains("visible.txt"), "{args}: {output}");
        for hidden in [".env", "secrets", "id.pem"] {
            assert!(!output.contains(hidden), "{args} shows {hidden}: {output}");
        }
    }
    let explicit = session
        .call("git_diff", "c", json!({ "paths": ["visible.txt", "app"] }))
        .await;
    let output = explicit["output"].as_str().expect("output");
    assert!(output.contains("visible.txt"));
    assert!(!output.contains("SECRET-AFTER app/config/.env"), "{output}");
    assert!(!output.contains("id.pem"), "{output}");
}

/// The host's summary (for its note to the model) is the branch and a
/// count: no path, and no approval asked (the prompt here defers all).
#[tokio::test]
async fn the_git_summary_is_the_branch_and_a_count() {
    let dir = tempfile::tempdir().expect("dir");
    let root = std::fs::canonicalize(dir.path())
        .expect("canonical")
        .join("repo");
    std::fs::create_dir_all(&root).expect("repo");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("a.txt"), "a\n").expect("a");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    std::fs::write(root.join("a.txt"), "changed\n").expect("change");
    std::fs::write(root.join("new.txt"), "new\n").expect("new");
    let open = |root: std::path::PathBuf| {
        LocalSession::open(SessionConfig {
            root,
            session_id: "s".to_owned(),
            policy: LocalWorkPolicy {
                allowed: true,
                ..LocalWorkPolicy::default()
            },
            settings: WorkspaceSettings::default(),
            choices: Arc::new(MemoryChoices::default()),
            prompt: Arc::new(DeferringPrompt),
            data_dir: dir.path().join("data"),
            shell: None,
            deny_read: Vec::new(),
            app_id: None,
        })
        .expect("session")
    };
    let summary = open(root).git_summary().await.expect("in git");
    assert_eq!(summary.branch.as_deref(), Some("main"));
    assert_eq!(summary.changed, 2);
    let plain = dir.path().join("plain");
    std::fs::create_dir_all(&plain).expect("plain");
    assert!(
        open(std::fs::canonicalize(plain).expect("canonical"))
            .git_summary()
            .await
            .is_none()
    );
}
