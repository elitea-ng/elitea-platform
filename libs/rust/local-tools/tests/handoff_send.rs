//! Send (ADR-0034 decisions 5 and 6, Verification 1): the transfer set,
//! the exclusions on tracked files, the person's repository byte-identical
//! afterwards, the orphan base and upstream, the shadow repository, the
//! scan and the caps, and the segment read back.
#![cfg(feature = "handoff")]
#![allow(clippy::unwrap_used, clippy::expect_used)]

mod handoff_common;

use std::collections::{BTreeMap, BTreeSet};

use elitea_local_tools::handoff::caps::Caps;
use elitea_local_tools::handoff::exclude::Reason;
use elitea_local_tools::handoff::snapshot::{self, open_source};
use elitea_workspace_store::bundle::{self, Expect, Limits};
use elitea_workspace_store::objects::{self, ObjectDb};

use handoff_common::{CONVERSATION, Fixture, git, git_state_fingerprint, tree_fingerprint, write};

fn files_of(top: &std::path::Path, tree: &str) -> BTreeSet<String> {
    git(top, &["ls-tree", "-r", "--name-only", tree])
        .lines()
        .map(str::to_owned)
        .collect()
}

#[test]
fn the_transfer_set_is_the_checkpoint_set_minus_exclusions() {
    let fixture = Fixture::repo();
    write(&fixture.top, "private/plan.md", "denied by policy\n");
    write(&fixture.top, ".eliteaignore", "scratch/\n");
    write(&fixture.top, "scratch/tmp.txt", "x\n");
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
    assert!(!snap.blocked(), "{:?} {:?}", snap.findings, snap.caps);
    let files = files_of(&fixture.top, &snap.tree);
    let expected: BTreeSet<String> = [
        ".gitignore",
        ".eliteaignore",
        "README.md",
        "blob.bin",
        "src/lib.rs",
        "staged.txt",
        "untracked/new.md",
    ]
    .iter()
    .map(|s| (*s).to_owned())
    .collect();
    assert_eq!(
        files, expected,
        "deleted, ignored and excluded files are not sent"
    );
    // The edited README is sent as edited.
    assert_eq!(
        git(&fixture.top, &["show", &format!("{}:README.md", snap.tree)]),
        "hello, edited"
    );
    let excluded: BTreeMap<String, (Reason, bool)> = snap
        .excluded
        .iter()
        .map(|e| (e.path.clone(), (e.reason, e.tracked)))
        .collect();
    assert_eq!(excluded[".env"], (Reason::Mandatory, true));
    assert_eq!(excluded["keys/id_rsa"], (Reason::Mandatory, true));
    assert_eq!(excluded["private/plan.md"], (Reason::PathDeny, false));
    assert_eq!(excluded["scratch/tmp.txt"], (Reason::Eliteaignore, false));
    assert_eq!(snap.files, expected.len());
}

#[test]
fn a_send_leaves_index_head_stash_and_working_tree_byte_identical() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    // A stash of the person's own.
    write(top, "stashed.txt", "for the stash\n");
    git(top, &["add", "stashed.txt"]);
    git(
        top,
        &["stash", "push", "-q", "-m", "mine", "--", "stashed.txt"],
    );
    let state = git_state_fingerprint(top);
    let tree = tree_fingerprint(top);
    let status = git(top, &["status", "--porcelain=v2", "--untracked-files=all"]);
    let config = fixture.config(&[]);
    let source = open_source(
        top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    let snap = snapshot::take(&source, &config).unwrap();
    snapshot::bundle(&source, &snap, Limits::default()).unwrap();
    snapshot::keep_refs(&source, &snap, CONVERSATION).unwrap();
    assert_eq!(
        git_state_fingerprint(top),
        state,
        "index, HEAD, branches, stash, reflogs"
    );
    assert_eq!(tree_fingerprint(top), tree, "working tree");
    assert_eq!(
        git(top, &["status", "--porcelain=v2", "--untracked-files=all"]),
        status
    );
    // The twin is kept privately.
    assert_eq!(
        git(
            top,
            &[
                "rev-parse",
                &format!("refs/elitea/handoff-local/{CONVERSATION}/twin")
            ]
        ),
        snap.twin
    );
    // The fingerprints would have seen a change: the check can fail.
    git(top, &["add", "README.md"]);
    assert_ne!(git_state_fingerprint(top), state);
    write(top, "src/lib.rs", "pub fn b() {}\n");
    assert_ne!(tree_fingerprint(top), tree);
}

#[test]
fn the_base_is_an_orphan_with_heads_tree_and_the_twin_sits_on_head() {
    let fixture = Fixture::repo();
    let top = &fixture.top;
    let head = git(top, &["rev-parse", "HEAD"]);
    let config = fixture.config(&[]);
    let source = open_source(
        top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    let snap = snapshot::take(&source, &config).unwrap();
    assert_eq!(snap.head.as_deref(), Some(head.as_str()));
    assert_eq!(
        git(top, &["rev-parse", &format!("{}^{{tree}}", snap.base)]),
        git(top, &["rev-parse", "HEAD^{tree}"])
    );
    assert_eq!(
        git(top, &["rev-list", "--parents", "-n1", &snap.base]),
        snap.base,
        "B has no parent"
    );
    assert!(
        git(top, &["log", "-1", "--format=%B", &snap.base])
            .contains(&format!("Elitea-Base: {head}"))
    );
    assert_eq!(
        git(top, &["rev-parse", &format!("{}^", snap.handoff)]),
        snap.base
    );
    assert_eq!(git(top, &["rev-parse", &format!("{}^", snap.twin)]), head);
    assert_eq!(
        git(top, &["rev-parse", &format!("{}^{{tree}}", snap.twin)]),
        snap.tree
    );
    assert!(
        git(top, &["log", "-1", "--format=%B", &snap.handoff])
            .contains("Changed README; ran tests.")
    );
    assert!(snap.upstream.is_none(), "no upstream without a remote");
}

#[test]
fn the_upstream_is_an_orphan_with_the_upstream_tree_and_no_credentials() {
    let fixture = Fixture::new();
    let remote = fixture.base.join("remote.git");
    std::fs::create_dir(&remote).unwrap();
    git(&remote, &["init", "-q", "--bare"]);
    let seed = fixture.base.join("seed");
    std::fs::create_dir(&seed).unwrap();
    git(&seed, &["init", "-q"]);
    write(&seed, "a.txt", "upstream\n");
    git(&seed, &["add", "-A"]);
    git(&seed, &["commit", "-q", "-m", "up"]);
    git(&seed, &["push", "-q", remote.to_str().unwrap(), "main"]);
    std::fs::remove_dir(&fixture.top).unwrap();
    git(
        &fixture.base,
        &["clone", "-q", remote.to_str().unwrap(), "repo"],
    );
    let top = &fixture.top;
    // Local work ahead of the upstream, and a remote URL with credentials.
    write(top, "a.txt", "local commit\n");
    git(top, &["commit", "-q", "-am", "local"]);
    let upstream_sha = git(top, &["rev-parse", "origin/main"]);
    git(
        top,
        &[
            "remote",
            "set-url",
            "origin",
            "https://me:ghp_secret@github.example/org/repo.git",
        ],
    );
    let config = fixture.config(&[]);
    let source = open_source(
        top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    let snap = snapshot::take(&source, &config).unwrap();
    let upstream = snap.upstream.clone().expect("an upstream");
    assert_eq!(upstream.sha, upstream_sha);
    assert_eq!(upstream.branch, "main");
    assert_eq!(upstream.url, "https://github.example/org/repo.git");
    assert_eq!(
        git(
            top,
            &["rev-parse", &format!("{}^{{tree}}", upstream.commit)]
        ),
        git(top, &["rev-parse", "origin/main^{tree}"])
    );
    assert_eq!(
        git(top, &["rev-list", "--parents", "-n1", &upstream.commit]),
        upstream.commit
    );
    let message = git(top, &["log", "-1", "--format=%B", &upstream.commit]);
    assert!(
        message.contains(&format!(
            "Elitea-Upstream: {upstream_sha} https://github.example/org/repo.git main"
        )),
        "{message}"
    );
    assert!(!message.contains("ghp_secret"));
    let refs: Vec<String> = snapshot::published_refs(&snap)
        .into_iter()
        .map(|(name, _)| name)
        .collect();
    assert_eq!(
        refs,
        [
            "refs/elitea/base",
            "refs/elitea/upstream",
            "refs/elitea/handoff/1"
        ]
    );
}

#[test]
fn a_folder_that_is_not_a_repository_gets_a_shadow_in_app_data() {
    let fixture = Fixture::new();
    let top = &fixture.top;
    write(top, "notes.md", "plain folder\n");
    write(
        top,
        "node_modules/pkg/index.js",
        "ignored by the default set\n",
    );
    write(top, "target/debug/app", "ignored\n");
    write(top, "docs/a.txt", "a\n");
    write(top, ".gitignore", "*.tmp\n");
    write(top, "x.tmp", "ignored by the folder's own rules\n");
    let config = fixture.config(&[]);
    let source = open_source(
        top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    assert!(source.shadow);
    assert!(source.git_dir().starts_with(&fixture.data));
    let snap = snapshot::take(&source, &config).unwrap();
    snapshot::keep_refs(&source, &snap, CONVERSATION).unwrap();
    assert!(snap.head.is_none());
    let db = ObjectDb::open_git_dir(source.git_dir()).unwrap();
    let files: BTreeSet<String> = db
        .flatten(&objects::oid(&snap.tree).unwrap())
        .unwrap()
        .into_keys()
        .collect();
    let expected: BTreeSet<String> = [".gitignore", "docs/a.txt", "notes.md"]
        .iter()
        .map(|s| (*s).to_owned())
        .collect();
    assert_eq!(files, expected);
    assert_eq!(
        db.commit(&objects::oid(&snap.base).unwrap())
            .unwrap()
            .tree
            .to_string(),
        objects::EMPTY_TREE
    );
    assert!(
        !top.join(".git").exists(),
        "nothing is created in the folder"
    );
}

#[test]
fn a_planted_key_blocks_the_send_until_it_is_left_on_the_laptop() {
    let fixture = Fixture::repo();
    let key = ["AKIA", "Z7QWERTYUIOPASDF"].concat();
    write(
        &fixture.top,
        "config/app.ini",
        &format!("[aws]\nkey = {key}\n"),
    );
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
    assert!(snap.blocked());
    assert_eq!(snap.findings.len(), 1);
    assert_eq!(snap.findings[0].path, "config/app.ini");
    assert_eq!(snap.findings[0].line, 2);
    assert_eq!(snap.findings[0].rule, "aws-access-key-id");
    let leave = fixture.config(&["config/app.ini"]);
    let snap = snapshot::take(&source, &leave).unwrap();
    assert!(!snap.blocked());
    assert!(
        snap.excluded
            .iter()
            .any(|e| e.path == "config/app.ini" && e.reason == Reason::LeaveOnLaptop)
    );
    assert!(!files_of(&fixture.top, &snap.tree).contains("config/app.ini"));
}

#[test]
fn the_caps_refuse_with_the_largest_paths() {
    let fixture = Fixture::repo();
    write(&fixture.top, "big.dat", &"x".repeat(4096));
    let mut config = fixture.config(&[]);
    config.caps = Caps {
        max_file_bytes: 1024,
        max_files: 3,
        ..Caps::default()
    };
    let source = open_source(
        &fixture.top,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap();
    let snap = snapshot::take(&source, &config).unwrap();
    assert!(snap.blocked());
    let hit: Vec<&str> = snap
        .caps
        .violations
        .iter()
        .map(|v| v.cap.as_str())
        .collect();
    assert!(
        hit.contains(&"max_file_bytes") && hit.contains(&"max_files"),
        "{hit:?}"
    );
    assert_eq!(snap.caps.largest[0].path, "big.dat");
    assert!(
        !files_of(&fixture.top, &snap.tree).contains("big.dat"),
        "an oversize file is never hashed in"
    );
}

#[test]
fn the_segment_reads_back_with_the_published_refs_and_the_transfer_set() {
    let fixture = Fixture::repo();
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
    let written = snapshot::bundle(&source, &snap, Limits::default()).unwrap();
    let quarantine = fixture.base.join("q");
    bundle::init_quarantine(&quarantine).unwrap();
    let refs: BTreeMap<String, String> = snapshot::published_refs(&snap).into_iter().collect();
    let verified = bundle::verify_into(
        &written.bytes,
        &quarantine,
        &Expect {
            sha256: Some(&written.sha256),
            bytes: None,
            refs: Some(&refs),
        },
        Limits::default(),
    )
    .unwrap();
    let sent = verified
        .db
        .flatten(
            &verified
                .db
                .commit(&objects::oid(&snap.handoff).unwrap())
                .unwrap()
                .tree,
        )
        .unwrap();
    assert!(!sent.contains_key(".env") && !sent.contains_key("keys/id_rsa"));
    assert!(sent.contains_key("untracked/new.md"));
    // No history left the laptop: two commits (B, H_1), no first commit.
    let head = git(&fixture.top, &["rev-parse", "HEAD"]);
    assert!(!verified.db.exists(&objects::oid(&head).unwrap()));
    assert_eq!(written.commits, 2);
}

#[test]
fn a_subfolder_and_a_sha256_repository_are_refused() {
    let fixture = Fixture::repo();
    let config = fixture.config(&[]);
    let error = open_source(
        &fixture.top.join("src"),
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap_err();
    assert!(error.message().contains("top level"), "{}", error.message());
    let other = fixture.base.join("sha256");
    std::fs::create_dir(&other).unwrap();
    git(&other, &["init", "-q", "--object-format=sha256"]);
    let error = open_source(
        &other,
        &fixture.data,
        &config.sandbox,
        &config.deny,
        Some(&fixture.global),
    )
    .unwrap_err();
    assert!(error.message().contains("sha256"), "{}", error.message());
}
