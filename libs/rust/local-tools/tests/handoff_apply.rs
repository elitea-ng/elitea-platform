//! Take-back, Phase 1 (ADR-0034 decision 13, Verification 4): the fast
//! path, a moved HEAD and local edits (worktree), discard, deletes,
//! renames, binaries and modes, excluded paths never deleted, a preflight
//! that leaves the folder and every ref byte-identical, the re-check before
//! apply, a tampered segment, and the checkpoint that undoes it all.
#![cfg(feature = "handoff")]
#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::too_many_lines,
    clippy::items_after_statements
)]

mod handoff_common;

use std::collections::BTreeMap;
use std::path::Path;

use elitea_local_tools::checkpoint::Checkpoints;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::handoff::SendRecord;
use elitea_local_tools::handoff::apply::{self, Mode, Outcome};
use elitea_local_tools::handoff::snapshot::{self, Source, open_source};
use elitea_local_tools::sandbox::SandboxConfig;
use elitea_local_tools::workspace::Workspace;
use elitea_workspace_store::bundle::{self, Limits};

use handoff_common::{
    CONVERSATION, Fixture, all_refs, git, git_state_fingerprint, tree_fingerprint, write,
};

struct Sent {
    source: Source,
    record: SendRecord,
    bytes: Vec<u8>,
}

fn send(fixture: &Fixture) -> Sent {
    send_leaving(fixture, &[])
}

fn send_leaving(fixture: &Fixture, leave: &[&str]) -> Sent {
    let config = fixture.config(leave);
    let source = open_source(
        &fixture.top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    let snap = snapshot::take(&source, &config).unwrap();
    assert!(!snap.blocked());
    let written = snapshot::bundle(&source, &snap, Limits::default()).unwrap();
    snapshot::keep_refs(&source, &snap, CONVERSATION).unwrap();
    Sent {
        record: snap.record(CONVERSATION),
        source,
        bytes: written.bytes,
    }
}

type Turn<'a> = &'a dyn Fn(&Path);

/// The cloud executor, played with the git CLI: hydrate `H_1`, one commit
/// per turn, a COMPACT bundle of every live ref plus `refs/elitea/cloud`.
fn cloud(
    fixture: &Fixture,
    sent: &Sent,
    turns: &[Turn<'_>],
) -> (Vec<u8>, BTreeMap<String, String>) {
    let dir = fixture.base.join(format!("cloud-{}", rand_suffix()));
    std::fs::create_dir(&dir).unwrap();
    git(&dir, &["init", "-q"]);
    let file = dir.join("../sent.bundle");
    std::fs::write(&file, &sent.bytes).unwrap();
    git(
        &dir,
        &[
            "fetch",
            "-q",
            file.to_str().unwrap(),
            "refs/elitea/*:refs/elitea/*",
        ],
    );
    git(
        &dir,
        &["checkout", "-q", "--detach", "refs/elitea/handoff/1"],
    );
    for (n, turn) in turns.iter().enumerate() {
        turn(&dir);
        git(&dir, &["add", "-A"]);
        git(
            &dir,
            &[
                "commit",
                "-q",
                "--allow-empty",
                "-m",
                &format!("cloud turn {}", n + 1),
            ],
        );
    }
    let mut names = vec!["refs/elitea/base", "refs/elitea/handoff/1"];
    if !turns.is_empty() {
        git(&dir, &["update-ref", "refs/elitea/cloud", "HEAD"]);
        names.push("refs/elitea/cloud");
    }
    let out = dir.join("../cloud.bundle");
    let mut args = vec!["bundle", "create", "-q", out.to_str().unwrap()];
    args.extend(names);
    git(&dir, &args);
    let bytes = std::fs::read(&out).unwrap();
    let refs = bundle::parse_header(&bytes).unwrap().ref_map();
    (bytes, refs)
}

fn rand_suffix() -> String {
    use std::sync::atomic::{AtomicU64, Ordering};
    static N: AtomicU64 = AtomicU64::new(0);
    N.fetch_add(1, Ordering::Relaxed).to_string()
}

fn checkpoints(fixture: &Fixture) -> Checkpoints {
    let workspace = Workspace::open(&fixture.top, &[]).unwrap();
    Checkpoints::open_with(&workspace, "ws1", &fixture.data, &SandboxConfig::default())
        .unwrap()
        .with_git_global_config(Some(fixture.global.clone()))
}

fn deny() -> Vec<String> {
    vec!["private/**".to_owned()]
}

fn cloud_edits(dir: &Path) {
    write(dir, "README.md", "hello from the cloud\n");
    write(dir, "src/new.rs", "pub fn new() {}\n");
    std::fs::remove_file(dir.join("src/lib.rs")).unwrap();
    std::fs::rename(dir.join("untracked/new.md"), dir.join("docs/moved.md")).unwrap_or_else(|_| {
        std::fs::create_dir_all(dir.join("docs")).unwrap();
        std::fs::rename(dir.join("untracked/new.md"), dir.join("docs/moved.md")).unwrap();
    });
    std::fs::write(dir.join("blob.bin"), [7u8, 0, 255, 1, 0]).unwrap();
    write(dir, "tool.sh", "#!/bin/sh\necho cloud\n");
    use std::os::unix::fs::PermissionsExt as _;
    std::fs::set_permissions(dir.join("tool.sh"), std::fs::Permissions::from_mode(0o755)).unwrap();
    // The cloud tries to write an excluded path: never applied.
    write(dir, ".env", "TOKEN=from-the-cloud\n");
}

#[test]
fn the_fast_path_lands_the_cloud_tree_as_uncommitted_changes_and_undo_restores() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(
        &fixture,
        &sent,
        &[&cloud_edits, &|dir| {
            write(dir, "README.md", "second turn\n");
        }],
    );
    let head = git(top, &["rev-parse", "HEAD"]);
    let index = std::fs::read(top.join(".git/index")).unwrap();
    let before_tree = tree_fingerprint(top);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    assert_eq!(preflight.cloud_turns(), 2);
    assert_eq!(preflight.conflict_paths, [] as [String; 0]);
    assert!(
        preflight.skipped_paths.contains(&".env".to_owned()),
        "{:?}",
        preflight.skipped_paths
    );
    for path in [
        "README.md",
        "src/new.rs",
        "src/lib.rs",
        "docs/moved.md",
        "untracked/new.md",
        "blob.bin",
        "tool.sh",
    ] {
        assert!(
            preflight.changed_paths.contains(&path.to_owned()),
            "{path}: {:?}",
            preflight.changed_paths
        );
    }
    let checkpoints = checkpoints(&fixture);
    let applied = apply::apply(
        &sent.source,
        &preflight,
        Mode::Apply,
        &deny(),
        Some(&checkpoints),
    )
    .unwrap();
    assert!(applied.applied);
    // The folder is the cloud tree.
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "second turn\n"
    );
    assert_eq!(
        std::fs::read_to_string(top.join("src/new.rs")).unwrap(),
        "pub fn new() {}\n"
    );
    assert!(!top.join("src/lib.rs").exists(), "deleted");
    assert!(
        !top.join("untracked/new.md").exists() && top.join("docs/moved.md").exists(),
        "renamed"
    );
    assert_eq!(
        std::fs::read(top.join("blob.bin")).unwrap(),
        [7u8, 0, 255, 1, 0]
    );
    {
        use std::os::unix::fs::PermissionsExt as _;
        assert_eq!(
            std::fs::metadata(top.join("tool.sh"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o755
        );
    }
    // Excluded paths: neither overwritten nor deleted.
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert!(top.join("keys/id_rsa").exists());
    // HEAD and the index are untouched; the cloud commits are on a branch
    // replayed onto the twin, whose parent is HEAD.
    assert_eq!(git(top, &["rev-parse", "HEAD"]), head);
    assert_eq!(std::fs::read(top.join(".git/index")).unwrap(), index);
    let branch = applied.branch.clone().unwrap();
    assert_eq!(branch, "elitea/cloud/c0ffee00");
    assert_eq!(
        git(top, &["rev-list", "--count", &format!("{head}..{branch}")]),
        "3",
        "twin + 2 turns"
    );
    // The twin sits on HEAD; the branch is the cloud tree with the
    // excluded paths as HEAD has them, so it never deletes the tracked
    // `.env` or key, nor carries the `.env` the cloud wrote.
    assert_eq!(git(top, &["rev-parse", &format!("{branch}~3")]), head);
    assert_eq!(
        git(top, &["diff", "--name-only", &preflight.cloud_tip, &branch]),
        ".env\nkeys/id_rsa"
    );
    let from_head = git(top, &["diff", "--name-status", &head, &branch]);
    assert!(
        !from_head.contains(".env") && !from_head.contains("id_rsa"),
        "{from_head}"
    );
    assert_eq!(
        git(top, &["log", "-1", "--format=%s", &branch]),
        "cloud turn 2"
    );
    // The per-commit changes for the history.
    let first = apply::Preflight::commit_changes(&preflight, &preflight.chain[0].sha).unwrap();
    assert!(
        first
            .iter()
            .any(|d| d.path == "src/lib.rs" && d.after.is_none())
    );
    // Undo: the checkpoint brings the folder back to before the take-back.
    // (Restores never remove directories a turn created; files are compared.)
    checkpoints.restore(applied.checkpoint.unwrap()).unwrap();
    let files = |tree: BTreeMap<String, String>| -> BTreeMap<String, String> {
        tree.into_iter()
            .filter(|(path, _)| !path.ends_with('/'))
            .collect()
    };
    assert_eq!(files(tree_fingerprint(top)), files(before_tree));
}

#[test]
fn a_preflight_writes_nothing_to_the_folder_or_any_ref() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    let tree = tree_fingerprint(top);
    let state = git_state_fingerprint(top);
    let all = all_refs(top);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    assert_eq!(tree_fingerprint(top), tree);
    assert_eq!(git_state_fingerprint(top), state);
    assert_eq!(all_refs(top), all, "not even a private ref");
    // Discard: still nothing.
    let applied = apply::apply(&sent.source, &preflight, Mode::Discard, &deny(), None).unwrap();
    assert!(!applied.applied);
    assert_eq!(tree_fingerprint(top), tree);
    assert_eq!(all_refs(top), all);
}

#[test]
fn a_moved_head_offers_a_worktree_and_leaves_the_folder() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    git(top, &["commit", "-q", "-m", "local commit after the send"]);
    let tree = tree_fingerprint(top);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::Conflicts);
    let refused = apply::apply(&sent.source, &preflight, Mode::Apply, &deny(), None).unwrap_err();
    assert_eq!(refused.code(), ErrorCode::InvalidArgument);
    let applied = apply::apply(&sent.source, &preflight, Mode::Worktree, &deny(), None).unwrap();
    let path = applied.worktree_path.clone().unwrap();
    assert_eq!(tree_fingerprint(top), tree, "the folder is untouched");
    assert_eq!(
        std::fs::read_to_string(path.join("README.md")).unwrap(),
        "hello from the cloud\n"
    );
    // The folder's own excluded files are copied (local → local); the
    // `.env` the cloud wrote never overwrites them.
    assert_eq!(
        std::fs::read_to_string(path.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert!(path.join("keys/id_rsa").exists());
    assert!(git(top, &["worktree", "list"]).contains(&path.to_string_lossy().into_owned()));
    // The worktree is the branch: clean.
    assert_eq!(git(&path, &["status", "--porcelain"]), "");
    assert_eq!(
        git(&path, &["rev-parse", "HEAD"]),
        git(top, &["rev-parse", applied.branch.as_deref().unwrap()])
    );
}

#[test]
fn local_edits_since_the_send_are_conflicts() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    write(top, "README.md", "edited locally meanwhile\n");
    write(top, "local-only.txt", "new locally\n");
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::Conflicts);
    assert_eq!(preflight.conflict_paths, ["README.md"]);
    let refused = apply::apply(&sent.source, &preflight, Mode::Apply, &deny(), None).unwrap_err();
    assert_eq!(refused.code(), ErrorCode::InvalidArgument);
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "edited locally meanwhile\n"
    );
}

#[test]
fn a_folder_changed_after_its_preflight_is_refused_untouched() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    write(top, "src/lib.rs", "pub fn changed_after_preview() {}\n");
    let tree = tree_fingerprint(top);
    let all = all_refs(top);
    let error = apply::apply(&sent.source, &preflight, Mode::Apply, &deny(), None).unwrap_err();
    assert_eq!(error.code(), ErrorCode::StaleRead);
    assert!(error.message().starts_with(apply::FOLDER_CHANGED));
    assert_eq!(tree_fingerprint(top), tree);
    assert_eq!(all_refs(top), all);
}

#[test]
fn a_tampered_segment_aborts_with_nothing_applied() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (mut bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    let offset = bundle::parse_header(&bytes).unwrap().pack_offset;
    bytes[offset + 30] ^= 0x40;
    let tree = tree_fingerprint(top);
    let all = all_refs(top);
    let error = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap_err();
    assert!(error.message().contains("integrity"), "{}", error.message());
    assert_eq!(tree_fingerprint(top), tree);
    assert_eq!(all_refs(top), all);
    // Refs other than the plan's: refused the same way.
    let (good, _) = cloud(&fixture, &sent, &[&cloud_edits]);
    let mut other = refs.clone();
    other.insert("refs/elitea/cloud".into(), sent.record.base.clone());
    assert!(apply::preflight(&sent.source, &sent.record, &good, &other, &deny()).is_err());
}

#[test]
fn a_thread_with_no_cloud_turn_takes_back_as_a_no_op() {
    let fixture = Fixture::repo();
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[]);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    assert_eq!(preflight.cloud_turns(), 0);
    assert_eq!(preflight.changed_paths, [] as [String; 0]);
}

#[test]
fn a_folder_without_a_repository_takes_back_through_its_shadow() {
    let fixture = Fixture::new();
    let top = &fixture.top;
    write(top, "notes.md", "plain\n");
    write(top, "docs/a.txt", "a\n");
    write(top, ".env", "LOCAL=1\n");
    let sent = send(&fixture);
    assert!(sent.record.shadow);
    let (bytes, refs) = cloud(
        &fixture,
        &sent,
        &[&|dir| {
            write(dir, "notes.md", "edited in the cloud\n");
            std::fs::remove_file(dir.join("docs/a.txt")).unwrap();
        }],
    );
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    let checkpoints = checkpoints(&fixture);
    let applied = apply::apply(
        &sent.source,
        &preflight,
        Mode::Apply,
        &deny(),
        Some(&checkpoints),
    )
    .unwrap();
    assert!(applied.checkpoint.is_some());
    assert_eq!(
        std::fs::read_to_string(top.join("notes.md")).unwrap(),
        "edited in the cloud\n"
    );
    assert!(!top.join("docs/a.txt").exists());
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "LOCAL=1\n"
    );
    assert!(!top.join(".git").exists());
}

/// Risk 2a: the take-back worktree opens as a desktop workspace when its
/// repository is one the person opened — host git and git checkpoints
/// work in it — and a `.git` file is still refused otherwise.
#[test]
fn a_take_back_worktree_opens_as_a_workspace_of_a_trusted_repository() {
    use elitea_local_tools::deny::DenyList;
    use elitea_local_tools::git::{Repo, forget_worktree_parent, trust_worktree_parent};
    use std::sync::Arc;
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(&fixture, &sent, &[&cloud_edits]);
    git(top, &["commit", "-q", "-m", "moved on"]);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    let applied = apply::apply(&sent.source, &preflight, Mode::Worktree, &deny(), None).unwrap();
    let path = applied.worktree_path.unwrap();
    let deny_list = || Arc::new(DenyList::new([fixture.data.join("secrets")]));
    // Not trusted yet: refused as before.
    let refused = Repo::discover(&path, &SandboxConfig::default(), deny_list()).unwrap_err();
    assert_eq!(refused.code(), ErrorCode::UnsafeRepository);
    trust_worktree_parent(top);
    let repo = Repo::discover(&path, &SandboxConfig::default(), deny_list())
        .unwrap()
        .unwrap();
    assert!(repo.is_linked_worktree());
    // Forgotten (the workspace was removed): refused again.
    forget_worktree_parent(top);
    let refused = Repo::discover(&path, &SandboxConfig::default(), deny_list()).unwrap_err();
    assert_eq!(refused.code(), ErrorCode::UnsafeRepository);
    trust_worktree_parent(top);
    assert_eq!(
        repo.common_dir(),
        std::fs::canonicalize(top.join(".git")).unwrap()
    );
    // Git checkpoints, not copies, and one round-trips.
    let workspace = Workspace::open(&path, &[]).unwrap();
    let checkpoints =
        Checkpoints::open_with(&workspace, "wt1", &fixture.data, &SandboxConfig::default())
            .unwrap()
            .with_git_global_config(Some(fixture.global.clone()));
    assert!(
        checkpoints.git_refusal().is_none(),
        "{:?}",
        checkpoints.git_refusal()
    );
    assert_eq!(checkpoints.kind(), "git");
    let info = checkpoints.create("before an edit").unwrap();
    write(&path, "README.md", "edited in the worktree\n");
    checkpoints.restore(info.seq).unwrap();
    assert_eq!(
        std::fs::read_to_string(path.join("README.md")).unwrap(),
        "hello from the cloud\n"
    );
    assert_eq!(git(&path, &["status", "--porcelain"]), "");
    // A `.git` file that points at the repository itself, or at a worktree
    // that registered another folder, is refused even for a trusted parent.
    let fake = fixture.base.join("fake");
    std::fs::create_dir(&fake).unwrap();
    std::fs::write(
        fake.join(".git"),
        format!("gitdir: {}\n", top.join(".git").display()),
    )
    .unwrap();
    assert_eq!(
        Repo::discover(&fake, &SandboxConfig::default(), deny_list())
            .unwrap_err()
            .code(),
        ErrorCode::UnsafeRepository
    );
    let pointer = std::fs::read_to_string(path.join(".git")).unwrap();
    std::fs::write(fake.join(".git"), pointer).unwrap();
    assert_eq!(
        Repo::discover(&fake, &SandboxConfig::default(), deny_list())
            .unwrap_err()
            .code(),
        ErrorCode::UnsafeRepository
    );
}

/// Risks 2b and 2c: a worktree gets the folder's excluded files (tracked,
/// untracked and ignored), and a file the cloud created that the exclusions
/// match is written only where no local file exists — in the worktree and
/// on the fast path.
#[test]
fn cloud_created_excluded_files_never_overwrite_local_ones() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    // An ignored `.env.local` the folder needs to build.
    write(top, ".gitignore", "ignored.log\nbuild/\n.env.local\n");
    git(top, &["add", ".gitignore"]);
    git(top, &["commit", "-q", "-m", "ignore .env.local"]);
    write(top, ".env.local", "LOCAL=1\n");
    let sent = send(&fixture);
    let created = |dir: &Path| {
        write(dir, ".env", "TOKEN=from-the-cloud\n");
        write(dir, "config/.env.cloud", "CLOUD=1\n");
        write(dir, "src/ok.rs", "pub fn ok() {}\n");
    };
    let (bytes, refs) = cloud(&fixture, &sent, &[&created]);

    // Fast path.
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.outcome, Outcome::FastPath);
    for path in ["config/.env.cloud", "src/ok.rs"] {
        assert!(preflight.changed_paths.contains(&path.to_owned()), "{path}");
    }
    assert!(preflight.skipped_paths.contains(&".env".to_owned()));
    let applied = apply::apply(&sent.source, &preflight, Mode::Apply, &deny(), None).unwrap();
    assert!(applied.written.contains(&"config/.env.cloud".to_owned()));
    assert_eq!(
        std::fs::read_to_string(top.join("config/.env.cloud")).unwrap(),
        "CLOUD=1\n"
    );
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert_eq!(
        std::fs::read_to_string(top.join(".env.local")).unwrap(),
        "LOCAL=1\n"
    );

    // Worktree (a second thread state: undo the fast path by hand first).
    std::fs::remove_file(top.join("config/.env.cloud")).unwrap();
    std::fs::remove_file(top.join("src/ok.rs")).unwrap();
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    let applied = apply::apply(&sent.source, &preflight, Mode::Worktree, &deny(), None).unwrap();
    let path = applied.worktree_path.unwrap();
    for local in [".env", ".env.local", "keys/id_rsa"] {
        assert!(
            applied.copied.contains(&local.to_owned()),
            "{local}: {:?}",
            applied.copied
        );
    }
    assert_eq!(
        std::fs::read_to_string(path.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert_eq!(
        std::fs::read_to_string(path.join(".env.local")).unwrap(),
        "LOCAL=1\n"
    );
    assert_eq!(
        std::fs::read_to_string(path.join("config/.env.cloud")).unwrap(),
        "CLOUD=1\n"
    );
    // Clean but for the cloud-created excluded file, which no commit holds.
    assert_eq!(
        git(&path, &["status", "--porcelain", "-uall"]),
        "?? config/.env.cloud"
    );
}

/// A path both left on the laptop and in `path_deny` is a hard deny: a file
/// the cloud creates there is never written (the leave-on-laptop reason
/// would let a created file in where none exists).
#[test]
fn a_path_in_both_leave_and_path_deny_is_never_written_by_take_back() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    write(top, "deploy/a.tf", "local\n");
    git(top, &["add", "deploy"]);
    git(top, &["commit", "-q", "-m", "deploy"]);
    let sent = send_leaving(&fixture, &["deploy"]);
    let created = |dir: &Path| {
        write(dir, "deploy/x.tf", "from the cloud\n");
        write(dir, "src/ok.rs", "pub fn ok() {}\n");
    };
    let (bytes, refs) = cloud(&fixture, &sent, &[&created]);
    let deny = vec!["deploy/**".to_owned()];
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny).unwrap();
    assert!(preflight.skipped_paths.contains(&"deploy/x.tf".to_owned()));
    let applied = apply::apply(&sent.source, &preflight, Mode::Apply, &deny, None).unwrap();
    assert!(!applied.written.contains(&"deploy/x.tf".to_owned()));
    assert!(!top.join("deploy/x.tf").exists(), "never written");
    assert_eq!(
        std::fs::read_to_string(top.join("deploy/a.tf")).unwrap(),
        "local\n"
    );
    assert!(top.join("src/ok.rs").exists());
}
