//! The host runs git in folders the agent (or whoever wrote the repository)
//! controls. Each test plants one way repository-controlled code used to
//! run outside the sandbox through the host's own git calls — filter
//! drivers, gpg.program, core.worktree, includes, a relocated `.git`,
//! commondir, submodules — and proves none of it runs: the repository is
//! refused with `local_tools.unsafe_repository`, checkpoints fall back to
//! copies, and the marker the payload would write never appears.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;

use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::checkpoint::Checkpoints;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::workspace::Workspace;
use serde_json::{Value, json};

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
            "-c",
            "core.hooksPath=/dev/null",
        ])
        .args(args)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .output()
        .expect("git");
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
}

struct Planted {
    _dir: tempfile::TempDir,
    root: PathBuf,
    outside: PathBuf,
    marker: PathBuf,
    data: PathBuf,
}

/// A committed repository, an outside directory, and the marker path a
/// payload would create.
fn repository() -> Planted {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("repo");
    let outside = base.join("outside");
    std::fs::create_dir_all(&root).expect("repo");
    std::fs::create_dir_all(&outside).expect("outside");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("a.txt"), "a\n").expect("a");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    std::fs::write(root.join("a.txt"), "changed\n").expect("change");
    Planted {
        marker: outside.join("pwned"),
        data: base.join("data"),
        _dir: dir,
        root,
        outside,
    }
}

fn payload(marker: &Path) -> String {
    // Config values: `;` and `#` start comments, quotes are stripped.
    format!("touch {}", marker.display())
}

fn append(path: &Path, text: &str) {
    let mut current = std::fs::read_to_string(path).unwrap_or_default();
    current.push_str(text);
    std::fs::write(path, current).expect("append");
}

/// Every host path that runs git: opening checkpoints, taking one,
/// restoring one, and the git tools. None may run the payload.
async fn exercise(planted: &Planted) -> (Checkpoints, Value) {
    let workspace = Workspace::open(&planted.root, &[]).expect("workspace");
    let checkpoints = Checkpoints::open(&workspace, "s1", &planted.data).expect("open");
    if let Ok(info) = checkpoints.create("turn") {
        let _ = checkpoints.restore(info.seq);
    }
    let session = LocalSession::open(SessionConfig {
        root: planted.root.clone(),
        session_id: "s1".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(DeferringPrompt),
        data_dir: planted.data.clone(),
        shell: None,
        deny_read: Vec::new(),
    })
    .expect("session");
    let mut results = Vec::new();
    for tool in ["git_status", "git_diff", "git_log", "git_branches"] {
        results.push(session.call(tool, "call-1", json!({})).await);
    }
    assert!(
        !planted.marker.exists(),
        "repository-controlled code ran on the host"
    );
    (checkpoints, Value::Array(results))
}

fn assert_refused(checkpoints: &Checkpoints, results: &Value) {
    assert_eq!(
        checkpoints.kind(),
        "copy",
        "an unsafe repository is not used"
    );
    let refusal = checkpoints.git_refusal().expect("a refusal");
    assert_eq!(refusal.code(), ErrorCode::UnsafeRepository);
    for result in results.as_array().expect("results") {
        assert_eq!(result["code"], "local_tools.unsafe_repository", "{result}");
    }
}

#[tokio::test]
async fn a_safe_repository_still_checkpoints_through_git() {
    let planted = repository();
    let (checkpoints, results) = exercise(&planted).await;
    assert_eq!(checkpoints.kind(), "git");
    assert!(checkpoints.git_refusal().is_none());
    for result in results.as_array().expect("results") {
        assert_eq!(result["status"], "ok", "{result}");
    }
}

#[tokio::test]
async fn filter_drivers_in_the_repository_config_never_run() {
    let planted = repository();
    let command = payload(&planted.marker);
    append(
        &planted.root.join(".git/config"),
        &format!("[filter \"pwn\"]\n\tclean = {command} && cat\n\tsmudge = {command} && cat\n"),
    );
    std::fs::write(planted.root.join(".gitattributes"), "* filter=pwn\n").expect("attrs");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn gpg_program_and_signing_never_run() {
    let planted = repository();
    let script = planted.outside.join("gpg.sh");
    std::fs::write(
        &script,
        format!("#!/bin/sh\n{}\n", payload(&planted.marker)),
    )
    .expect("gpg");
    Command::new("chmod")
        .arg("+x")
        .arg(&script)
        .status()
        .expect("chmod");
    append(
        &planted.root.join(".git/config"),
        &format!(
            "[commit]\n\tgpgSign = true\n[log]\n\tshowSignature = true\n[gpg]\n\tprogram = {}\n",
            script.display()
        ),
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn a_core_worktree_elsewhere_is_refused() {
    let planted = repository();
    append(
        &planted.root.join(".git/config"),
        &format!("[core]\n\tworktree = {}\n", planted.outside.display()),
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
    assert_eq!(
        std::fs::read_dir(&planted.outside)
            .expect("outside")
            .count(),
        0,
        "nothing was written to the other work tree"
    );
}

#[tokio::test]
async fn includes_aliases_hooks_and_fsmonitor_in_the_config_are_refused() {
    for snippet in [
        "[include]\n\tpath = ../evil.cfg\n",
        "[includeIf \"gitdir:/\"]\n\tpath = ../evil.cfg\n",
        "[alias]\n\tst = !touch x\n",
        "[core]\n\tfsmonitor = touch x\n",
        "[core]\n\thooksPath = hooks\n",
        "[core]\n\tsshCommand = touch x\n",
        "[diff \"x\"]\n\ttextconv = touch x\n",
        "[merge \"x\"]\n\tdriver = touch x\n",
        "[credential]\n\thelper = touch x\n",
        "[submodule \"m\"]\n\turl = ../m\n",
        "[url \"x\"]\n\tinsteadOf = y\n",
        "[core]\n\tpager = touch x\n",
        "[this is not a config line\n",
    ] {
        let planted = repository();
        std::fs::write(
            planted.root.join("evil.cfg"),
            format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
        )
        .expect("evil");
        append(&planted.root.join(".git/config"), snippet);
        let (checkpoints, results) = exercise(&planted).await;
        assert_refused(&checkpoints, &results);
    }
}

#[tokio::test]
async fn a_relocated_git_dir_is_refused() {
    let planted = repository();
    let elsewhere = planted.outside.join("real.git");
    std::fs::rename(planted.root.join(".git"), &elsewhere).expect("move");
    append(
        &elsewhere.join("config"),
        &format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    );
    std::fs::write(
        planted.root.join(".git"),
        format!("gitdir: {}\n", elsewhere.display()),
    )
    .expect("gitdir file");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn commondir_and_submodule_git_dirs_are_refused() {
    let planted = repository();
    std::fs::write(
        planted.root.join(".git/commondir"),
        planted.outside.display().to_string(),
    )
    .expect("commondir");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);

    let planted = repository();
    let module = planted.root.join(".git/modules/m");
    std::fs::create_dir_all(&module).expect("module");
    std::fs::write(
        module.join("config"),
        format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    )
    .expect("module config");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}
