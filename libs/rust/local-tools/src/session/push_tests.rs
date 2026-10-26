//! `git_push` in bypass mode (the owner's report: the agent could not push
//! and handed `git push` to the person, "the sandbox still blocks .git").
//! The push goes through the host's hardened git and lands; the person is
//! not asked; the policy's deny list still refuses it; and a sandboxed
//! command still cannot write `.git/hooks`.
//!
//! A unit test, not an integration one: it pushes to a local bare
//! repository, a transport only the crate's own tests allow.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::{Value, json};

use super::{LocalSession, SessionConfig};
use crate::approvals::{MemoryChoices, PermissionProfile, WorkspaceSettings};
use crate::policy::LocalWorkPolicy;
use crate::shell::ShellConfig;

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
    remote: PathBuf,
    session: Arc<LocalSession>,
    prompt: Arc<Prompt>,
}

/// A repository with one commit ahead of its bare `origin`, a session
/// whose prompt answers `action`, and `policy` on top of local work with
/// the shell and the network on.
fn fixture(action: &'static str, policy: LocalWorkPolicy) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let remote = base.join("origin.git");
    std::fs::create_dir_all(&remote).expect("remote");
    git(&remote, &["init", "-q", "--bare"]);
    let root = base.join("repo");
    std::fs::create_dir_all(&root).expect("repo");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("a.txt"), "a\n").expect("a");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    git(
        &root,
        &["remote", "add", "origin", &remote.display().to_string()],
    );
    git(&root, &["push", "-q", "origin", "main"]);
    std::fs::write(root.join("a.txt"), "b\n").expect("b");
    git(&root, &["commit", "-q", "-am", "ahead"]);
    let global = base.join("gitconfig");
    std::fs::write(&global, "[user]\n\tname = P\n\temail = p@example.com\n").expect("global");
    let mut shell = ShellConfig::new(base.join("tmp"));
    shell.git_global_config = Some(global);
    let prompt = Arc::new(Prompt {
        action,
        seen: Mutex::new(Vec::new()),
    });
    let session = LocalSession::open(SessionConfig {
        spill_dir: None,
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            network: true,
            ..policy
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: base.join("data"),
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: crate::budget::Budgets::default(),
        documents: crate::document::Documents::default(),
    })
    .expect("session");
    Fixture {
        _dir: dir,
        root,
        remote,
        session,
        prompt,
    }
}

fn remote_head(fixture: &Fixture) -> String {
    git(&fixture.remote, &["rev-parse", "refs/heads/main"])
}

async fn push(fixture: &Fixture, args: Value) -> Value {
    fixture.session.call("git_push", "p", args).await
}

#[tokio::test]
async fn bypass_mode_pushes_through_git_push_without_asking() {
    // A prompt that would refuse: bypass mode must not reach it.
    let fixture = fixture("reject", LocalWorkPolicy::default());
    let before = remote_head(&fixture);
    fixture.session.set_profile(PermissionProfile::Bypass);
    let result = push(&fixture, json!({})).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["remote"], "origin");
    assert_eq!(result["branch"], "main");
    let head = git(&fixture.root, &["rev-parse", "HEAD"]);
    assert_ne!(before, head);
    assert_eq!(remote_head(&fixture), head, "the push landed");
    assert_eq!(
        git(&fixture.root, &["rev-parse", "refs/remotes/origin/main"]),
        head,
        "the remote-tracking ref moved"
    );
    assert!(
        fixture.prompt.seen.lock().expect("lock").is_empty(),
        "bypass mode asked"
    );
}

#[tokio::test]
async fn a_push_is_asked_outside_bypass_mode() {
    let fixture = fixture("reject", LocalWorkPolicy::default());
    let before = remote_head(&fixture);
    let result = push(&fixture, json!({ "remote": "origin", "branch": "main" })).await;
    assert_eq!(result["code"], "local_tools.rejected", "{result}");
    assert_eq!(fixture.prompt.seen.lock().expect("lock").len(), 1);
    assert_eq!(remote_head(&fixture), before);
}

#[tokio::test]
async fn the_policy_deny_list_refuses_a_push_in_bypass_mode() {
    let fixture = fixture(
        "approve",
        LocalWorkPolicy {
            command_deny: vec!["git push".to_owned()],
            ..LocalWorkPolicy::default()
        },
    );
    let before = remote_head(&fixture);
    fixture.session.set_profile(PermissionProfile::Bypass);
    let result = push(&fixture, json!({})).await;
    assert_eq!(result["code"], "local_tools.rejected", "{result}");
    assert!(
        result["message"]
            .as_str()
            .is_some_and(|message| message.contains("denied by policy")),
        "{result}"
    );
    assert_eq!(remote_head(&fixture), before);
    assert!(fixture.prompt.seen.lock().expect("lock").is_empty());
}

#[tokio::test]
async fn only_a_configured_remote_and_a_local_branch_are_pushed() {
    let fixture = fixture("approve", LocalWorkPolicy::default());
    fixture.session.set_profile(PermissionProfile::Bypass);
    for args in [
        json!({ "remote": "/tmp/elsewhere.git" }),
        json!({ "remote": "--receive-pack=touch x" }),
        json!({ "branch": "--force" }),
        json!({ "branch": "nope" }),
        json!({ "branch": "main:refs/heads/other" }),
        json!({ "force": true }),
    ] {
        let result = push(&fixture, args.clone()).await;
        assert_ne!(result["status"], "ok", "{args} → {result}");
    }
}

/// The repository's own config cannot point a push at a proxy (with TLS
/// checks off, it would see the person's token).
#[tokio::test]
async fn a_repository_http_setting_refuses_the_push() {
    let fixture = fixture("approve", LocalWorkPolicy::default());
    git(
        &fixture.root,
        &["config", "http.proxy", "http://127.0.0.1:9"],
    );
    let before = remote_head(&fixture);
    fixture.session.set_profile(PermissionProfile::Bypass);
    let result = push(&fixture, json!({})).await;
    assert_eq!(result["code"], "local_tools.unsafe_repository", "{result}");
    assert_eq!(remote_head(&fixture), before);
}

/// `git push` in `run_command` names the tool that pushes, and a
/// sandboxed command still cannot plant a hook, bypass mode or not.
#[tokio::test]
async fn the_shell_points_at_git_push_and_still_cannot_write_hooks() {
    let fixture = fixture("approve", LocalWorkPolicy::default());
    fixture.session.set_profile(PermissionProfile::Bypass);
    let result = fixture
        .session
        .call(
            "run_command",
            "r",
            json!({ "command": "git push origin main", "network": true }),
        )
        .await;
    assert_ne!(result["status"], "ok", "{result}");
    assert!(
        result.to_string().contains("git_push"),
        "the refusal names git_push: {result}"
    );
    let hook = fixture.root.join(".git/hooks/pre-push");
    let config = fixture.root.join(".git/config");
    let config_before = std::fs::read_to_string(&config).expect("config");
    for command in [
        "/bin/sh -c 'printf \"#!/bin/sh\\ntouch /tmp/pwned\\n\" > .git/hooks/pre-push'",
        "/bin/sh -c 'printf \"[core]\\n\\thooksPath = /tmp\\n\" >> .git/config'",
    ] {
        let _ = fixture
            .session
            .call("run_command", "w", json!({ "command": command }))
            .await;
    }
    assert!(!hook.exists(), "a sandboxed command wrote .git/hooks");
    assert_eq!(
        std::fs::read_to_string(&config).expect("config"),
        config_before,
        "a sandboxed command wrote .git/config"
    );
}
