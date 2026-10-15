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
    let config = fixture.config(&[]);
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
    assert!(preflight.conflict_paths.is_empty());
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
    assert_eq!(
        git(top, &["rev-parse", &format!("{branch}~2")]),
        sent.record.twin
    );
    assert_eq!(
        git(top, &["rev-parse", &format!("{branch}^{{tree}}")]),
        git(
            top,
            &["rev-parse", &format!("{}^{{tree}}", preflight.cloud_tip)]
        )
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
    assert!(
        !path.join(".env").exists(),
        "an excluded path is not in the worktree"
    );
    assert!(git(top, &["worktree", "list"]).contains(&path.to_string_lossy().into_owned()));
    // The worktree is the branch, but for the excluded path the cloud
    // wrote, which is never written to disk.
    assert_eq!(git(&path, &["status", "--porcelain"]), "D .env");
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
    assert!(preflight.changed_paths.is_empty());
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
