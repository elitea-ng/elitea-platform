//! `git_commit`: `.git` is read-only for sandboxed commands, so the agent
//! commits through the host's hardened git. The commit is allowed by
//! default (a workspace rule may ask; the fixture's does), never
//! remembered, runs no hooks, is not signed, carries the person's
//! global identity (not the repository's), and an unsafe repository is
//! refused.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{
    APPROVE_ALWAYS, ChoiceStore, MemoryChoices, RememberedChoice, ToolKind, Verdict, WorkspaceRule,
    WorkspaceSettings,
};
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

fn git(dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "user.name=Setup",
            "-c",
            "user.email=setup@example.com",
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
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

/// Answers every question with `action` and records it.
struct Prompt {
    action: &'static str,
    seen: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for Prompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.seen.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: self.action.to_owned(),
            value: json!({}),
        })
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    root: PathBuf,
    marker: PathBuf,
    session: Arc<LocalSession>,
    prompt: Arc<Prompt>,
    choices: Arc<MemoryChoices>,
}

/// A session whose workspace rules ask before every commit.
fn fixture(action: &'static str, prepare: impl FnOnce(&Path)) -> Fixture {
    let ask_commits = WorkspaceSettings {
        rules: vec![WorkspaceRule {
            tools: vec![ToolKind::GitCommit],
            command: None,
            path: None,
            verdict: Verdict::Ask,
        }],
    };
    fixture_with(action, ask_commits, prepare)
}

fn fixture_with(
    action: &'static str,
    settings: WorkspaceSettings,
    prepare: impl FnOnce(&Path),
) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("repo");
    std::fs::create_dir_all(&root).expect("repo");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("a.txt"), "a\n").expect("a");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    // The repository's identity must not be used; its hook must not run.
    std::fs::write(
        root.join(".git/config"),
        format!(
            "{}[user]\n\tname = Repo Name\n\temail = repo@example.com\n",
            std::fs::read_to_string(root.join(".git/config")).expect("config")
        ),
    )
    .expect("config");
    let marker = base.join("hook-ran");
    let hook = root.join(".git/hooks/pre-commit");
    std::fs::write(&hook, format!("#!/bin/sh\ntouch {}\n", marker.display())).expect("hook");
    Command::new("chmod")
        .arg("+x")
        .arg(&hook)
        .status()
        .expect("chmod");
    prepare(&root);
    let global = base.join("gitconfig");
    std::fs::write(
        &global,
        "[user]\n\tname = Global Person\n\temail = person@example.com\n",
    )
    .expect("global");
    let mut shell = ShellConfig::new(base.join("tmp"));
    shell.git_global_config = Some(global);
    let prompt = Arc::new(Prompt {
        action,
        seen: Mutex::new(Vec::new()),
    });
    let choices = Arc::new(MemoryChoices::default());
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            path_deny: vec![".env".to_owned()],
            ..LocalWorkPolicy::default()
        },
        settings,
        choices: choices.clone(),
        prompt: prompt.clone(),
        data_dir: base.join("data"),
        shell: Some(shell),
        deny_read: Vec::new(),
    })
    .expect("session");
    Fixture {
        _dir: dir,
        root,
        marker,
        session,
        prompt,
        choices,
    }
}

async fn commit(fixture: &Fixture, args: Value) -> Value {
    fixture.session.call("git_commit", "c", args).await
}

#[tokio::test]
async fn commits_selected_files_with_the_global_identity_and_no_hooks() {
    let fixture = fixture("approve", |_| {});
    std::fs::write(fixture.root.join("a.txt"), "changed\n").expect("edit");
    std::fs::write(fixture.root.join("new.txt"), "new\n").expect("new");
    std::fs::write(fixture.root.join("other.txt"), "not selected\n").expect("other");
    let result = commit(
        &fixture,
        json!({ "message": "Agent change\n\nBody", "paths": ["a.txt", "new.txt"] }),
    )
    .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["subject"], "Agent change");
    let head = git(&fixture.root, &["rev-parse", "HEAD"]);
    assert_eq!(result["commit"], head);
    assert_eq!(
        git(
            &fixture.root,
            &["log", "-1", "--format=%an <%ae>|%cn <%ce>|%s"]
        ),
        "Global Person <person@example.com>|Global Person <person@example.com>|Agent change"
    );
    assert_eq!(
        git(&fixture.root, &["show", "--name-only", "--format=", "HEAD"]),
        "a.txt\nnew.txt"
    );
    assert!(!git(&fixture.root, &["cat-file", "commit", "HEAD"]).contains("gpgsig"));
    assert!(!fixture.marker.exists(), "the repository's hook ran");
    assert_eq!(
        git(&fixture.root, &["status", "--porcelain"]),
        "?? other.txt",
        "only the selected files were committed"
    );
    let seen = fixture.prompt.seen.lock().expect("lock").clone();
    assert_eq!(seen.len(), 1, "the commit was asked");
    assert!(
        seen[0].message.contains("Agent change"),
        "{}",
        seen[0].message
    );
    assert!(
        !seen[0]
            .available_actions
            .contains(&APPROVE_ALWAYS.to_owned()),
        "a commit is never remembered"
    );
}

/// The owner's default: a commit through the host's git is not asked.
#[tokio::test]
async fn commits_run_without_a_question_by_default() {
    let fixture = fixture_with("reject", WorkspaceSettings::default(), |_| {});
    std::fs::write(fixture.root.join("a.txt"), "changed\n").expect("edit");
    let result = commit(&fixture, json!({ "message": "quiet", "paths": ["a.txt"] })).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert!(fixture.prompt.seen.lock().expect("lock").is_empty());
    assert_eq!(git(&fixture.root, &["log", "-1", "--format=%s"]), "quiet");
}

#[tokio::test]
async fn commits_what_is_staged_and_is_refused_when_rejected() {
    let rejected = fixture("reject", |_| {});
    std::fs::write(rejected.root.join("a.txt"), "changed\n").expect("edit");
    git(&rejected.root, &["add", "a.txt"]);
    let before = git(&rejected.root, &["rev-parse", "HEAD"]);
    let result = commit(&rejected, json!({ "message": "nope" })).await;
    assert_eq!(result["code"], "local_tools.rejected", "{result}");
    assert_eq!(git(&rejected.root, &["rev-parse", "HEAD"]), before);

    let approved = fixture("approve", |_| {});
    std::fs::write(approved.root.join("a.txt"), "staged\n").expect("edit");
    git(&approved.root, &["add", "a.txt"]);
    let result = commit(&approved, json!({ "message": "staged change" })).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(
        git(&approved.root, &["log", "-1", "--format=%s"]),
        "staged change"
    );
}

#[tokio::test]
async fn remembered_shell_choices_and_approve_always_never_allow_a_commit() {
    let fixture = fixture("approve_always", |_| {});
    for command in ["git commit", "git"] {
        fixture
            .choices
            .remember(RememberedChoice {
                tool: ToolKind::RunCommand,
                command: Some(command.split(' ').map(str::to_owned).collect()),
                paths: None,
                glob: None,
                sandbox: Some(SandboxMode::FullAccess),
                network: true,
            })
            .expect("remember");
    }
    for round in 0..2 {
        std::fs::write(fixture.root.join("a.txt"), format!("round {round}\n")).expect("edit");
        let result = commit(&fixture, json!({ "message": "m", "paths": ["a.txt"] })).await;
        assert_eq!(result["status"], "ok", "{result}");
    }
    assert_eq!(
        fixture.prompt.seen.lock().expect("lock").len(),
        2,
        "asked every time"
    );
    assert!(
        fixture
            .choices
            .list()
            .iter()
            .all(|choice| choice.tool != ToolKind::GitCommit)
    );
}

#[tokio::test]
async fn unsafe_repositories_and_denied_paths_are_refused() {
    let unsafe_repo = fixture("approve", |root| {
        let config = root.join(".git/config");
        let text = std::fs::read_to_string(&config).expect("config");
        std::fs::write(
            config,
            format!("{text}[core]\n\tfsmonitor = touch /tmp/x\n"),
        )
        .expect("config");
    });
    std::fs::write(unsafe_repo.root.join("a.txt"), "x\n").expect("edit");
    let result = commit(&unsafe_repo, json!({ "message": "m", "paths": ["a.txt"] })).await;
    assert_eq!(result["code"], "local_tools.unsafe_repository", "{result}");

    let denied = fixture("approve", |_| {});
    std::fs::write(denied.root.join(".env"), "TOKEN=1\n").expect("env");
    let result = commit(&denied, json!({ "message": "m", "paths": [".env"] })).await;
    assert_eq!(result["code"], "local_tools.denied", "{result}");
    assert!(denied.prompt.seen.lock().expect("lock").is_empty());
    let empty = commit(&denied, json!({ "message": "  " })).await;
    assert_eq!(empty["code"], "local_tools.invalid_argument");
}

/// A directory never stages what `path_deny` hides: committing `.` takes
/// everything else and leaves `.env` (at any depth) untouched.
#[tokio::test]
async fn committing_a_directory_leaves_denied_files_out() {
    let fixture = fixture("approve", |_| {});
    std::fs::create_dir_all(fixture.root.join("sub")).expect("sub");
    std::fs::write(fixture.root.join(".env"), "TOKEN=1\n").expect("env");
    std::fs::write(fixture.root.join("sub/.env"), "TOKEN=2\n").expect("env");
    std::fs::write(fixture.root.join("b.txt"), "b\n").expect("b");
    std::fs::write(fixture.root.join("sub/c.txt"), "c\n").expect("c");
    let result = commit(&fixture, json!({ "message": "all", "paths": ["."] })).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(
        git(&fixture.root, &["show", "--name-only", "--format=", "HEAD"]),
        "b.txt\nsub/c.txt"
    );
    assert_eq!(git(&fixture.root, &["diff", "--cached", "--name-only"]), "");
    assert_eq!(
        git(
            &fixture.root,
            &["status", "--porcelain", "--untracked-files=all"]
        ),
        "?? .env\n?? sub/.env"
    );
}

/// A commit of what is staged takes the whole index: a denied file staged
/// there (by the person, outside the session) is never committed. The
/// commit is refused, naming it, and the index is left as it was.
#[tokio::test]
async fn committing_what_is_staged_refuses_a_staged_denied_file() {
    let fixture = fixture_with("approve", WorkspaceSettings::default(), |_| {});
    std::fs::write(fixture.root.join("a.txt"), "changed\n").expect("edit");
    std::fs::create_dir_all(fixture.root.join("sub")).expect("sub");
    std::fs::write(fixture.root.join("sub/.env"), "TOKEN=1\n").expect("env");
    git(&fixture.root, &["add", "a.txt", "sub/.env"]);
    let before = git(&fixture.root, &["rev-parse", "HEAD"]);
    let result = commit(&fixture, json!({ "message": "staged" })).await;
    assert_eq!(result["code"], "local_tools.denied", "{result}");
    let message = result["message"].as_str().unwrap_or_default();
    assert!(message.contains("sub/.env"), "{message}");
    assert!(message.contains("nothing was committed"), "{message}");
    assert_eq!(git(&fixture.root, &["rev-parse", "HEAD"]), before);
    assert_eq!(
        git(&fixture.root, &["diff", "--cached", "--name-only"]),
        "a.txt\nsub/.env",
        "the index is the person's: left as it was"
    );

    // Once the denied file is unstaged, the same commit goes through.
    git(&fixture.root, &["restore", "--staged", "sub/.env"]);
    let result = commit(&fixture, json!({ "message": "staged" })).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(
        git(&fixture.root, &["show", "--name-only", "--format=", "HEAD"]),
        "a.txt"
    );
}
