//! Full auto (issue 3079): it answers the taint gate's ask that Bypass
//! leaves to the person, keeps every deny, never answers the hard-deny
//! list (asked when attended, declined when unattended), and decides as
//! Bypass when the policy forbids it.

use std::sync::Arc;

use super::super::{
    FolderTrust, MemoryChoices, PermissionProfile, RulesEngine, Source, TAINT_REASON, ToolCall,
    ToolKind, Unattended, Verdict, WorkspaceSettings,
};
use super::{Hold, hold};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::workspace::Workspace;

fn policy(full_auto: bool) -> LocalWorkPolicy {
    LocalWorkPolicy {
        allowed: true,
        shell: true,
        network: true,
        max_sandbox_mode: SandboxMode::FullAccess,
        folder_trust: true,
        full_auto,
        command_deny: vec!["terraform apply".to_owned()],
        ..LocalWorkPolicy::default()
    }
}

struct Fixture {
    dir: tempfile::TempDir,
    workspace: Workspace,
    engine: RulesEngine,
}

fn fixture(full_auto: bool) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
    let engine = RulesEngine::new(
        policy(full_auto),
        &workspace,
        WorkspaceSettings::default(),
        Arc::new(MemoryChoices::default()),
    )
    .expect("engine");
    engine.set_folder_trust(Some(FolderTrust::default()));
    Fixture {
        dir,
        workspace,
        engine,
    }
}

fn host(command: &str) -> ToolCall {
    ToolCall {
        command: Some(command.to_owned()),
        sandbox: Some(SandboxMode::WorkspaceWrite),
        unconfined: true,
        host: true,
        ..ToolCall::new(ToolKind::RunCommand)
    }
}

fn decided(engine: &RulesEngine, command: &str) -> (Verdict, Source) {
    let decision = engine.decide(&host(command));
    (decision.verdict, decision.source)
}

/// The issue's case: after an untrusted read, `gh` on the host is asked by
/// the taint gate. Bypass leaves it to the person; Full auto answers it,
/// with the reason it would have asked.
#[test]
fn full_auto_answers_the_taint_gate_bypass_does_not() {
    let fixture = fixture(true);
    fixture.engine.record_untrusted_read();
    let command = "gh pr view 12";
    assert_eq!(
        decided(&fixture.engine, command),
        (Verdict::Ask, Source::Taint)
    );

    fixture.engine.set_profile(PermissionProfile::Bypass);
    assert_eq!(
        decided(&fixture.engine, command),
        (Verdict::Ask, Source::Taint),
        "Bypass keeps the taint gate"
    );

    fixture.engine.set_profile(PermissionProfile::FullAuto);
    assert_eq!(fixture.engine.profile(), PermissionProfile::FullAuto);
    let answered = fixture.engine.decide(&host(command));
    assert_eq!(
        (answered.verdict, answered.source),
        (Verdict::Allow, Source::FullAutoTaint)
    );
    assert_eq!(answered.reason, TAINT_REASON);

    // Any other ask is answered as Bypass would, marked as Full auto's.
    fixture.engine.reset_untrusted_reads(false);
    assert_eq!(
        decided(&fixture.engine, "git fetch origin"),
        (Verdict::Allow, Source::FullAuto)
    );
    // A deny stays a deny.
    assert_eq!(
        decided(&fixture.engine, "terraform apply"),
        (Verdict::Deny, Source::Policy)
    );
    // An unattended turn in Full auto is answered the same way (ADR-0045
    // runs may start in it).
    fixture.engine.set_unattended(Some(Unattended::default()));
    fixture.engine.record_untrusted_read();
    assert_eq!(
        decided(&fixture.engine, command),
        (Verdict::Allow, Source::FullAutoTaint)
    );
}

/// The hard-deny list is asked in an attended turn and declined at once in
/// an unattended one, after an untrusted read or not.
#[test]
fn the_hard_deny_list_still_asks_or_fails_closed() {
    let fixture = fixture(true);
    fixture.engine.set_profile(PermissionProfile::FullAuto);
    fixture.engine.record_untrusted_read();
    let held = [
        "rm -rf /usr/local/share/thing",
        "rm -rf ~/projects",
        "rm -r ../sibling",
        "find /var/tmp -name '*.log' -delete",
        "cat ~/.ssh/id_ed25519",
        "cp key ~/.aws/credentials",
        "security find-generic-password -s github -w",
        "echo 'export PATH=/x:$PATH' >> ~/.zshrc",
        "tee -a /etc/profile",
        "rm -rf ~/Library/Application\\ Support/elitea-desktop",
        "csrutil disable",
        "sudo setenforce 0",
    ];
    for command in held {
        assert_eq!(
            decided(&fixture.engine, command),
            (Verdict::Ask, Source::FullAutoHold),
            "{command}"
        );
    }
    fixture.engine.set_unattended(Some(Unattended::default()));
    for command in held {
        assert_eq!(
            decided(&fixture.engine, command),
            (Verdict::Deny, Source::FullAutoHold),
            "{command}"
        );
    }
}

/// What the list names, and what it leaves to Full auto.
#[test]
fn hold_names_what_outlives_the_folder() {
    let fixture = fixture(true);
    let root = fixture.dir.path().to_string_lossy().into_owned();
    let cases: &[(&str, Option<Hold>)] = &[
        ("rm -rf build target", None),
        ("rm -rf ./dist", None),
        ("find . -name '*.tmp' -delete", None),
        ("rm build/out.txt /tmp/one-file", None),
        ("rm -rf /", Some(Hold::RecursiveDeleteOutside)),
        ("rm -rf $HOME/x", Some(Hold::RecursiveDeleteOutside)),
        ("rm -fr -- ../../x", Some(Hold::RecursiveDeleteOutside)),
        (
            "Remove-Item -Recurse C:\\Users\\me",
            Some(Hold::RecursiveDeleteOutside),
        ),
        // After a `cd`, a relative target may be anywhere.
        (
            "cd /usr/local && rm -rf lib",
            Some(Hold::RecursiveDeleteOutside),
        ),
        ("cd .. && rm -rf proj", Some(Hold::RecursiveDeleteOutside)),
        // `find`'s leading options do not hide its paths.
        (
            "find -L / -name '*.log' -delete",
            Some(Hold::RecursiveDeleteOutside),
        ),
        ("find -H . -delete", None),
        // A substitution elsewhere in the command does not make an inside delete opaque.
        ("rm -rf dist && echo \"built $(date)\"", None),
        ("rm -rf $(cat list)", Some(Hold::RecursiveDeleteOutside)),
        // `-Force` is not `-Recurse`.
        ("Remove-Item -Force C:\\proj\\out.txt", None),
        (
            "Remove-Item -rec C:\\proj",
            Some(Hold::RecursiveDeleteOutside),
        ),
        ("cat .npmrc", None),
        ("cd ~ && cat .npmrc", Some(Hold::CredentialStore)),
        (
            "ssh-keygen -t ed25519 -f ~/.ssh/new",
            Some(Hold::CredentialStore),
        ),
        (
            "kubectl --kubeconfig=/home/me/.kube/config get pods",
            Some(Hold::CredentialStore),
        ),
        ("cat docs/.profile", None),
        ("vim ~/.bash_profile", Some(Hold::ShellProfile)),
        (
            "cat ~/.config/elitea/rules.json",
            Some(Hold::SandboxControl),
        ),
        ("spctl --master-disable", Some(Hold::SandboxControl)),
        ("gh pr view 12", None),
        ("cargo test", None),
    ];
    for (command, expected) in cases {
        assert_eq!(
            hold(&host(command), &fixture.workspace),
            *expected,
            "{command}"
        );
    }
    // Absolute inside the folder is inside, a protected path included;
    // escaping it with `..` is not.
    for inside in [
        format!("rm -rf {root}/build"),
        format!("rm -rf {root}/.git/hooks"),
    ] {
        assert_eq!(hold(&host(&inside), &fixture.workspace), None, "{inside}");
    }
    let escape = format!("rm -rf {root}/../other");
    assert_eq!(
        hold(&host(&escape), &fixture.workspace),
        Some(Hold::RecursiveDeleteOutside),
        "{escape}"
    );
    // A call without a command (a file tool) is never on the list.
    assert_eq!(
        hold(&ToolCall::new(ToolKind::WriteFile), &fixture.workspace),
        None
    );
}

/// A policy that forbids Full auto: the engine decides as Bypass, so the
/// taint gate's ask stays the person's.
#[test]
fn a_forbidding_policy_decides_as_bypass() {
    let fixture = fixture(false);
    fixture.engine.set_profile(PermissionProfile::FullAuto);
    fixture.engine.record_untrusted_read();
    assert_eq!(
        decided(&fixture.engine, "gh pr view 12"),
        (Verdict::Ask, Source::Taint)
    );
    fixture.engine.reset_untrusted_reads(false);
    assert_eq!(
        decided(&fixture.engine, "git fetch origin"),
        (Verdict::Allow, Source::Bypass)
    );
}

/// What Bypass leaves to the person stays theirs in Full auto: a
/// `computer` act is still asked.
#[test]
fn full_auto_keeps_the_computer_exception() {
    let fixture = fixture(true);
    fixture.engine.set_profile(PermissionProfile::FullAuto);
    let act = fixture.engine.decide(&ToolCall::new(ToolKind::ComputerAct));
    assert_ne!(act.source, Source::FullAuto);
    assert_ne!(act.verdict, Verdict::Allow);
}
