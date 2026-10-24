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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    assert_eq!(preflight.cloud_turns(), 2);
    assert_eq!(preflight.report.conflict_paths, [] as [String; 0]);
    assert!(
        preflight.report.skipped().contains(&".env".to_owned()),
        "{:?}",
        preflight.report.skipped_paths
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
            preflight.report.touched().contains(&path.to_owned()),
            "{path}: {:?}",
            preflight.report.changed_paths
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
    assert_eq!(applied.rest_branch, None, "every turn taken back");
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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
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
    // Tree-based (Phase 2): a commit that left the folder's files as they
    // were is still the fast path; a worktree stays on offer.
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    assert!(preflight.modes().contains(&Mode::Worktree));
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
    assert_eq!(preflight.report.outcome, Outcome::Conflicts);
    assert_eq!(preflight.report.conflict_paths, ["README.md"]);
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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    assert_eq!(preflight.cloud_turns(), 0);
    assert_eq!(preflight.report.changed_paths, []);
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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
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
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    for path in ["config/.env.cloud", "src/ok.rs"] {
        assert!(
            preflight.report.touched().contains(&path.to_owned()),
            "{path}"
        );
    }
    assert!(preflight.report.skipped().contains(&".env".to_owned()));
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
    assert!(
        preflight
            .report
            .skipped()
            .contains(&"deploy/x.tf".to_owned())
    );
    let applied = apply::apply(&sent.source, &preflight, Mode::Apply, &deny, None).unwrap();
    assert!(!applied.written.contains(&"deploy/x.tf".to_owned()));
    assert!(!top.join("deploy/x.tf").exists(), "never written");
    assert_eq!(
        std::fs::read_to_string(top.join("deploy/a.tf")).unwrap(),
        "local\n"
    );
    assert!(top.join("src/ok.rs").exists());
}

// ---- Phase 2: clean merge, conflict markers, up to turn k ----

use elitea_local_tools::handoff::apply::{SkipReason, SkippedPath};

fn skipped_for(preflight: &apply::Preflight, path: &str) -> Option<SkipReason> {
    preflight
        .report
        .skipped_paths
        .iter()
        .find(|skip| skip.path == path)
        .map(|skip| skip.reason)
}

/// Both sides changed other files: a clean merge lands both, excluded and
/// denied paths are never written or deleted, the preflight wrote nothing,
/// `apply_with_markers` is refused (nothing conflicts), and undo restores.
#[test]
fn a_clean_merge_lands_both_sides_and_undo_restores() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(
        &fixture,
        &sent,
        &[&cloud_edits, &|dir| {
            write(dir, "private/plan.md", "the cloud wrote a denied path\n");
        }],
    );
    write(top, "staged.txt", "staged, then edited locally\n");
    write(top, "local-only.txt", "new locally\n");
    let tree = tree_fingerprint(top);
    let state = git_state_fingerprint(top);
    let all = all_refs(top);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.report.outcome, Outcome::CleanMerge);
    assert_eq!(preflight.report.conflict_paths, [] as [String; 0]);
    assert!(!preflight.report.applied);
    assert_eq!(preflight.report.base, sent.record.twin);
    assert!(preflight.report.result_tree.is_some());
    assert_eq!(
        preflight.modes(),
        [Mode::Apply, Mode::Worktree, Mode::Discard]
    );
    // The record's exclusions and the deny list, each with its reason.
    assert_eq!(skipped_for(&preflight, ".env"), Some(SkipReason::Excluded));
    assert_eq!(
        skipped_for(&preflight, "keys/id_rsa"),
        Some(SkipReason::Excluded)
    );
    assert_eq!(
        skipped_for(&preflight, "private/plan.md"),
        Some(SkipReason::PathDeny)
    );
    let renamed = preflight
        .report
        .changed_paths
        .iter()
        .find(|change| change.path == "docs/moved.md")
        .unwrap();
    assert_eq!(renamed.change, apply::Change::Renamed);
    assert_eq!(renamed.from.as_deref(), Some("untracked/new.md"));
    // The preflight wrote nothing anyone sees.
    assert_eq!(tree_fingerprint(top), tree);
    assert_eq!(git_state_fingerprint(top), state);
    assert_eq!(all_refs(top), all);

    let refused = apply::apply(
        &sent.source,
        &preflight,
        Mode::ApplyWithMarkers,
        &deny(),
        None,
    )
    .unwrap_err();
    assert_eq!(refused.code(), ErrorCode::InvalidArgument);
    let checkpoints = checkpoints(&fixture);
    let applied = apply::apply(
        &sent.source,
        &preflight,
        Mode::Apply,
        &deny(),
        Some(&checkpoints),
    )
    .unwrap();
    assert!(applied.applied && applied.report.applied);
    // Both sides.
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "hello from the cloud\n"
    );
    assert_eq!(
        std::fs::read_to_string(top.join("staged.txt")).unwrap(),
        "staged, then edited locally\n"
    );
    assert!(top.join("local-only.txt").exists());
    assert!(top.join("src/new.rs").exists() && !top.join("src/lib.rs").exists());
    // Never written or deleted.
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert!(top.join("keys/id_rsa").exists());
    assert!(!top.join("private/plan.md").exists());
    checkpoints.restore(applied.checkpoint.unwrap()).unwrap();
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "hello, edited\n"
    );
    assert!(top.join("src/lib.rs").exists());
}

/// Both sides changed the same lines of a text file and a binary file:
/// `apply` is refused, a folder changed since the preflight is refused,
/// `apply_with_markers` writes markers into the text file and never into
/// the binary one, the rules still hold, and undo restores.
#[test]
fn conflict_markers_go_into_text_files_never_binary_ones() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(
        &fixture,
        &sent,
        &[&|dir| {
            write(dir, "README.md", "hello from the cloud\n");
            std::fs::write(dir.join("blob.bin"), [1u8, 0, 2, 0]).unwrap();
            write(dir, "src/new.rs", "pub fn new() {}\n");
            write(dir, "private/plan.md", "denied\n");
            write(dir, ".env", "TOKEN=from-the-cloud\n");
        }],
    );
    write(top, "README.md", "hello from the laptop\n");
    std::fs::write(top.join("blob.bin"), [9u8, 0, 9, 0]).unwrap();
    let tree = tree_fingerprint(top);
    let all = all_refs(top);
    let preflight = apply::preflight(&sent.source, &sent.record, &bytes, &refs, &deny()).unwrap();
    assert_eq!(preflight.report.outcome, Outcome::Conflicts);
    assert_eq!(preflight.report.conflict_paths, ["README.md", "blob.bin"]);
    assert_eq!(preflight.report.result_tree, None);
    assert_eq!(
        preflight.modes(),
        [Mode::ApplyWithMarkers, Mode::Worktree, Mode::Discard]
    );
    assert!(
        preflight.report.skipped_paths.contains(&SkippedPath {
            path: "blob.bin".into(),
            reason: SkipReason::BinaryConflict,
        }),
        "{:?}",
        preflight.report.skipped_paths
    );
    assert_eq!(
        skipped_for(&preflight, "private/plan.md"),
        Some(SkipReason::PathDeny)
    );
    assert_eq!(skipped_for(&preflight, ".env"), Some(SkipReason::Excluded));
    assert!(!preflight.report.touched().contains(&"blob.bin".to_owned()));
    assert_eq!(tree_fingerprint(top), tree, "the preflight wrote nothing");
    assert_eq!(all_refs(top), all);

    let refused = apply::apply(&sent.source, &preflight, Mode::Apply, &deny(), None).unwrap_err();
    assert_eq!(refused.code(), ErrorCode::InvalidArgument);
    assert!(refused.message().starts_with("merge_conflicts"));
    // Changed since the preflight: refused, untouched.
    write(top, "late.txt", "late\n");
    let stale = apply::apply(
        &sent.source,
        &preflight,
        Mode::ApplyWithMarkers,
        &deny(),
        None,
    )
    .unwrap_err();
    assert_eq!(stale.code(), ErrorCode::StaleRead);
    assert!(stale.message().starts_with(apply::FOLDER_CHANGED));
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "hello from the laptop\n"
    );
    std::fs::remove_file(top.join("late.txt")).unwrap();

    let checkpoints = checkpoints(&fixture);
    let applied = apply::apply(
        &sent.source,
        &preflight,
        Mode::ApplyWithMarkers,
        &deny(),
        Some(&checkpoints),
    )
    .unwrap();
    let readme = std::fs::read_to_string(top.join("README.md")).unwrap();
    assert!(
        readme.contains("<<<<<<<")
            && readme.contains("hello from the laptop")
            && readme.contains("=======")
            && readme.contains("hello from the cloud")
            && readme.contains(">>>>>>>"),
        "{readme}"
    );
    assert_eq!(std::fs::read(top.join("blob.bin")).unwrap(), [9u8, 0, 9, 0]);
    assert!(!applied.written.contains(&"blob.bin".to_owned()));
    assert!(top.join("src/new.rs").exists());
    assert!(!top.join("private/plan.md").exists());
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "TOKEN=local-only\n"
    );
    assert!(top.join("keys/id_rsa").exists());
    // The branch holds the cloud's commits, not the markers.
    let branch = applied.branch.clone().unwrap();
    assert_eq!(
        git(top, &["show", &format!("{branch}:README.md")]),
        "hello from the cloud"
    );
    checkpoints.restore(applied.checkpoint.unwrap()).unwrap();
    assert_eq!(
        std::fs::read_to_string(top.join("README.md")).unwrap(),
        "hello from the laptop\n"
    );
    assert!(!top.join("src/new.rs").exists());
}

/// A simple clean repository with a tracked `.env` (excluded).
fn plain_repo() -> Fixture {
    let fixture = Fixture::new();
    let top = &fixture.top;
    git(top, &["init", "-q"]);
    write(top, "README.md", "sent\n");
    write(top, "src/lib.rs", "pub fn a() {}\n");
    write(top, ".env", "TOKEN=local\n");
    git(top, &["add", "-A"]);
    git(top, &["commit", "-q", "-m", "first"]);
    fixture
}

/// Taking back up to turn `k` writes exactly `C_k`'s tree (the excluded
/// `.env` aside), replays only the first `k` turns, and a turn the chain
/// does not have is refused before anything is written.
#[test]
fn taking_back_up_to_turn_k_applies_exactly_that_turns_tree() {
    let fixture = plain_repo();
    let top = &fixture.top;
    let sent = send(&fixture);
    let (bytes, refs) = cloud(
        &fixture,
        &sent,
        &[
            &|dir| {
                write(dir, "README.md", "turn 1\n");
                write(dir, "t1.txt", "one\n");
            },
            &|dir| {
                write(dir, "README.md", "turn 2\n");
                write(dir, "t2.txt", "two\n");
                std::fs::remove_file(dir.join("src/lib.rs")).unwrap();
            },
            &|dir| {
                write(dir, "README.md", "turn 3\n");
                write(dir, "t3.txt", "three\n");
            },
        ],
    );
    let tree = tree_fingerprint(top);
    for bad in [0, 4] {
        let error = apply::preflight_up_to(
            &sent.source,
            &sent.record,
            &bytes,
            &refs,
            &deny(),
            Some(bad),
        )
        .unwrap_err();
        assert_eq!(error.code(), ErrorCode::InvalidArgument, "{bad}");
        assert!(error.message().starts_with("turn_out_of_range"), "{bad}");
    }
    // A tampered segment aborts the same way for any turn.
    let mut tampered = bytes.clone();
    let offset = bundle::parse_header(&tampered).unwrap().pack_offset;
    tampered[offset + 30] ^= 0x40;
    assert!(
        apply::preflight_up_to(
            &sent.source,
            &sent.record,
            &tampered,
            &refs,
            &deny(),
            Some(2)
        )
        .is_err()
    );
    assert_eq!(tree_fingerprint(top), tree);

    let preflight =
        apply::preflight_up_to(&sent.source, &sent.record, &bytes, &refs, &deny(), Some(2))
            .unwrap();
    assert_eq!(preflight.cloud_turns(), 3);
    assert_eq!(preflight.up_to_turn, 2);
    assert_eq!(preflight.target, preflight.chain[1].sha);
    assert_ne!(preflight.target, preflight.cloud_tip);
    assert_eq!(preflight.report.outcome, Outcome::FastPath);
    assert!(!preflight.report.touched().contains(&"t3.txt".to_owned()));
    let checkpoints = checkpoints(&fixture);
    let applied = apply::apply(
        &sent.source,
        &preflight,
        Mode::Apply,
        &deny(),
        Some(&checkpoints),
    )
    .unwrap();
    assert_eq!(applied.replayed.len(), 2, "only turns 1 and 2");
    // Turn 3 is not taken back, but its commit is kept on a branch of its
    // own, on top of turn 2.
    let rest = applied.rest_branch.clone().unwrap();
    assert_eq!(rest, "elitea/cloud/c0ffee00-after-turn-2");
    assert_eq!(
        git(top, &["log", "-1", "--format=%s", &rest]),
        "cloud turn 3"
    );
    assert_eq!(
        git(
            top,
            &[
                "rev-list",
                "--count",
                &format!("{}..{rest}", applied.branch.as_deref().unwrap())
            ]
        ),
        "1"
    );
    assert!(!top.join("t3.txt").exists());
    // The folder is exactly C_2's tree, plus the excluded local `.env`.
    let expected: BTreeMap<String, String> = git(
        top,
        &[
            "ls-tree",
            "-r",
            "--format=%(path) %(objectname)",
            &preflight.target,
        ],
    )
    .lines()
    .map(|line| {
        let (path, oid) = line.split_once(' ').unwrap();
        (path.to_owned(), oid.to_owned())
    })
    .collect();
    let mut actual = BTreeMap::new();
    for path in tree_fingerprint(top).keys().filter(|p| !p.ends_with('/')) {
        if path == ".env" {
            continue;
        }
        actual.insert(path.clone(), git(top, &["hash-object", path]));
    }
    assert_eq!(actual, expected);
    assert_eq!(
        std::fs::read_to_string(top.join(".env")).unwrap(),
        "TOKEN=local\n"
    );
    let branch = applied.branch.unwrap();
    assert_eq!(
        git(top, &["log", "-1", "--format=%s", &branch]),
        "cloud turn 2"
    );
}

/// The shared engine's fixture: the same `(base, ours, theirs)` cases
/// through both callers, the worktree merge-back (#1235) and the handoff
/// take-back (#1187), with the same outcomes and the same folders.
mod shared_engine {
    use std::path::Path;
    use std::sync::Arc;

    use elitea_local_tools::apply as merge_back;
    use elitea_local_tools::apply::{Outcome, SkipReason};
    use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
    use elitea_local_tools::deny::DenyList;
    use elitea_local_tools::git::Repo;
    use elitea_local_tools::handoff::apply::{self as take_back, Mode};
    use elitea_local_tools::policy::LocalWorkPolicy;
    use elitea_local_tools::sandbox::SandboxConfig;
    use elitea_local_tools::session::{LocalSession, SessionConfig};

    use super::handoff_common::{Fixture, git, write};
    use super::{checkpoints, cloud, send};

    type Edit = fn(&Path);

    struct Case {
        name: &'static str,
        ours: Edit,
        theirs: Edit,
        outcome: Outcome,
        conflicts: &'static [&'static str],
        binary_conflict: Option<&'static str>,
        /// The folder after the apply (with markers for conflicts):
        /// `(path, Some(text) | None for absent)`; `a.txt` is checked for
        /// markers when it conflicts.
        expect: &'static [(&'static str, Option<&'static str>)],
    }

    const BASE_A: &str = "1\n2\n3\n4\n5\n6\n7\n";

    fn base(top: &Path) {
        write(top, "a.txt", BASE_A);
        write(top, "b.txt", "b\n");
        write(top, "c.txt", "c\n");
        std::fs::write(top.join("bin.dat"), [0u8, 1, 2, 0]).unwrap();
    }

    fn nothing(_: &Path) {}

    const CASES: &[Case] = &[
        Case {
            name: "fast_path",
            ours: nothing,
            theirs: |dir| {
                write(dir, "a.txt", "1\ntwo\n3\n4\n5\n6\n7\n");
                write(dir, "d.txt", "d\n");
            },
            outcome: Outcome::FastPath,
            conflicts: &[],
            binary_conflict: None,
            expect: &[
                ("a.txt", Some("1\ntwo\n3\n4\n5\n6\n7\n")),
                ("d.txt", Some("d\n")),
            ],
        },
        Case {
            name: "clean_merge",
            ours: |dir| {
                write(dir, "a.txt", "1\n2\n3\n4\n5\n6\nSEVEN\n");
                write(dir, "b.txt", "b, local\n");
            },
            theirs: |dir| {
                write(dir, "a.txt", "1\ntwo\n3\n4\n5\n6\n7\n");
                std::fs::remove_file(dir.join("c.txt")).unwrap();
            },
            outcome: Outcome::CleanMerge,
            conflicts: &[],
            binary_conflict: None,
            expect: &[
                ("a.txt", Some("1\ntwo\n3\n4\n5\n6\nSEVEN\n")),
                ("b.txt", Some("b, local\n")),
                ("c.txt", None),
            ],
        },
        Case {
            name: "text_conflict",
            ours: |dir| write(dir, "a.txt", "1\nTWO\n3\n4\n5\n6\n7\n"),
            theirs: |dir| {
                write(dir, "a.txt", "1\ntwo\n3\n4\n5\n6\n7\n");
                write(dir, "d.txt", "d\n");
            },
            outcome: Outcome::Conflicts,
            conflicts: &["a.txt"],
            binary_conflict: None,
            expect: &[("d.txt", Some("d\n"))],
        },
        Case {
            name: "binary_conflict",
            ours: |dir| std::fs::write(dir.join("bin.dat"), [0u8, 9]).unwrap(),
            theirs: |dir| {
                std::fs::write(dir.join("bin.dat"), [0u8, 8]).unwrap();
                write(dir, "d.txt", "d\n");
            },
            outcome: Outcome::Conflicts,
            conflicts: &["bin.dat"],
            binary_conflict: Some("bin.dat"),
            expect: &[("d.txt", Some("d\n")), ("a.txt", Some(BASE_A))],
        },
    ];

    fn check(case: &Case, top: &Path, caller: &str) {
        for (path, text) in case.expect {
            assert_eq!(
                std::fs::read_to_string(top.join(path)).ok().as_deref(),
                *text,
                "{caller}/{}: {path}",
                case.name
            );
        }
        if case.conflicts.contains(&"a.txt") {
            let a = std::fs::read_to_string(top.join("a.txt")).unwrap();
            assert!(
                a.contains("<<<<<<<") && a.contains("TWO") && a.contains("two"),
                "{caller}/{}: {a}",
                case.name
            );
        }
        if let Some(path) = case.binary_conflict {
            assert_eq!(
                std::fs::read(top.join(path)).unwrap(),
                [0u8, 9],
                "{caller}/{}: the folder's binary file is kept",
                case.name
            );
        }
    }

    fn session(root: &Path, data: &Path, id: &str) -> Arc<LocalSession> {
        LocalSession::open(SessionConfig {
            root: root.to_path_buf(),
            session_id: id.to_owned(),
            policy: LocalWorkPolicy {
                allowed: true,
                ..LocalWorkPolicy::default()
            },
            settings: WorkspaceSettings::default(),
            choices: Arc::new(MemoryChoices::default()),
            prompt: Arc::new(DeferringPrompt),
            data_dir: data.join(id),
            shell: None,
            deny_read: Vec::new(),
            app_id: None,
            budgets: elitea_local_tools::budget::Budgets::default(),
            documents: elitea_local_tools::document::Documents::default(),
        })
        .unwrap()
    }

    fn base_repo() -> Fixture {
        let fixture = Fixture::new();
        git(&fixture.top, &["init", "-q"]);
        base(&fixture.top);
        git(&fixture.top, &["add", "-A"]);
        git(&fixture.top, &["commit", "-q", "-m", "base"]);
        fixture
    }

    fn through_worktree_merge_back(case: &Case) {
        let fixture = base_repo();
        let top = &fixture.top;
        let main = Repo::discover(top, &SandboxConfig::default(), Arc::new(DenyList::new([])))
            .unwrap()
            .unwrap();
        std::fs::create_dir_all(fixture.data.join("worktrees")).unwrap();
        let created = elitea_local_tools::worktree::create(
            &main,
            &fixture.data.join("worktrees/abcd1234"),
            "abcd1234",
            "elitea/abcd1234",
            "HEAD",
        )
        .unwrap();
        let tree = created.entry.worktree.clone();
        (case.theirs)(&tree);
        let theirs = session(&tree, &fixture.data, "wt")
            .checkpoints()
            .snapshot_commit()
            .unwrap();
        (case.ours)(top);
        let target = session(top, &fixture.data, "main");
        let preview = merge_back::preflight(&target, &theirs, &created.base_sha).unwrap();
        assert_eq!(
            preview.report.outcome, case.outcome,
            "worktree/{}",
            case.name
        );
        assert_eq!(
            preview.report.conflict_paths, case.conflicts,
            "worktree/{}",
            case.name
        );
        if let Some(path) = case.binary_conflict {
            assert!(
                preview
                    .report
                    .skipped_paths
                    .iter()
                    .any(|s| s.path == path && s.reason == SkipReason::BinaryConflict),
                "worktree/{}",
                case.name
            );
        }
        let applied = if case.outcome == Outcome::Conflicts {
            assert!(merge_back::apply(&target, &preview, "merge").is_err());
            merge_back::apply_with_markers(&target, &preview, "merge").unwrap()
        } else {
            merge_back::apply(&target, &preview, "merge").unwrap()
        };
        check(case, top, "worktree");
        let seq = applied.checkpoint.unwrap();
        target.restore_checkpoint(seq).unwrap();
        assert_eq!(
            std::fs::read_to_string(top.join("d.txt")).ok(),
            None,
            "worktree/{}: undone",
            case.name
        );
    }

    fn through_handoff_take_back(case: &Case) {
        let fixture = base_repo();
        let top = &fixture.top;
        let sent = send(&fixture);
        let theirs = case.theirs;
        let (bytes, refs) = cloud(&fixture, &sent, &[&|dir: &Path| theirs(dir)]);
        (case.ours)(top);
        let preflight =
            take_back::preflight(&sent.source, &sent.record, &bytes, &refs, &[]).unwrap();
        assert_eq!(
            preflight.report.outcome, case.outcome,
            "handoff/{}",
            case.name
        );
        assert_eq!(
            preflight.report.conflict_paths, case.conflicts,
            "handoff/{}",
            case.name
        );
        if let Some(path) = case.binary_conflict {
            assert!(
                preflight
                    .report
                    .skipped_paths
                    .iter()
                    .any(|s| s.path == path && s.reason == SkipReason::BinaryConflict),
                "handoff/{}",
                case.name
            );
        }
        let mode = if case.outcome == Outcome::Conflicts {
            Mode::ApplyWithMarkers
        } else {
            Mode::Apply
        };
        let checkpoints = checkpoints(&fixture);
        let applied =
            take_back::apply(&sent.source, &preflight, mode, &[], Some(&checkpoints)).unwrap();
        check(case, top, "handoff");
        checkpoints.restore(applied.checkpoint.unwrap()).unwrap();
        assert_eq!(
            std::fs::read_to_string(top.join("d.txt")).ok(),
            None,
            "handoff/{}: undone",
            case.name
        );
    }

    #[test]
    fn both_callers_agree_on_every_case() {
        for case in CASES {
            through_worktree_merge_back(case);
            through_handoff_take_back(case);
        }
    }
}
