//! The change set of a cloud turn, over a real quarantine object database.

use std::fs;
use std::path::Path;

use elitea_workspace_store::bundle;
use elitea_workspace_store::objects::ObjectDb;
use gix_object::Write as _;
use prost::Message as _;

use super::*;
use crate::protocol::elitea::runtime::v1::{RuntimeErrorV1, WorkspaceRefusalV1};

/// A quarantine to write commits of trees into.
struct Repo {
    scratch: tempfile::TempDir,
    git: std::path::PathBuf,
    count: usize,
}

enum Entry<'a> {
    Text(&'a str),
    Bytes(Vec<u8>),
    Link(&'a str),
}

impl Repo {
    fn new() -> Self {
        let scratch = tempfile::tempdir().expect("scratch");
        let git = scratch.path().join("q.git");
        bundle::init_quarantine(&git).expect("init");
        Self {
            scratch,
            git,
            count: 0,
        }
    }

    /// Commit the tree of `files` (child of `parent`), answering its sha.
    fn commit(&mut self, files: &[(&str, Entry<'_>)], parent: Option<&str>) -> String {
        self.count += 1;
        let source = self.scratch.path().join(format!("t{}", self.count));
        fs::create_dir_all(&source).expect("mkdir");
        for (path, entry) in files {
            let target = source.join(path);
            fs::create_dir_all(target.parent().expect("parent")).expect("mkdir");
            match entry {
                Entry::Text(text) => fs::write(target, text).expect("write"),
                Entry::Bytes(bytes) => fs::write(target, bytes).expect("write"),
                Entry::Link(to) => std::os::unix::fs::symlink(to, target).expect("link"),
            }
        }
        let loose = gix_odb::loose::Store::at(self.git.join("objects"), gix_hash::Kind::Sha1);
        let tree = super::super::gix_codec::write_tree(&loose, &source, 0)
            .expect("tree")
            .unwrap_or_else(|| loose.write_buf(gix_object::Kind::Tree, &[]).expect("empty"));
        let parent = parent
            .map(|sha| format!("parent {sha}\n"))
            .unwrap_or_default();
        let raw = format!(
            "tree {tree}\n{parent}author a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nc\n"
        );
        loose
            .write_buf(gix_object::Kind::Commit, raw.as_bytes())
            .expect("commit")
            .to_string()
    }

    fn db(&self) -> ObjectDb {
        ObjectDb::open_objects(&self.git.join("objects")).expect("db")
    }

    fn git_dir(&self) -> &Path {
        &self.git
    }
}

fn numbered(from: usize, to: usize) -> String {
    use std::fmt::Write as _;
    (from..=to).fold(String::new(), |mut out, index| {
        let _ = writeln!(out, "line {index}");
        out
    })
}

fn find<'a>(files: &'a [ChangedFile], path: &str) -> &'a ChangedFile {
    files
        .iter()
        .find(|file| file.path == path)
        .unwrap_or_else(|| panic!("{path} listed"))
}

#[test]
fn a_turn_lists_added_modified_deleted_and_renamed_files_with_line_counts() {
    let mut repo = Repo::new();
    let long = numbered(1, 40);
    let base = repo.commit(
        &[
            ("src/main.rs", Entry::Text(&long)),
            ("gone.txt", Entry::Text("bye\n")),
            ("old/name.txt", Entry::Text("moved content\n")),
            ("same.txt", Entry::Text("same\n")),
        ],
        None,
    );
    let edited = long
        .replace("line 5\n", "line five\n")
        .replace("line 30\n", "line thirty\nline extra\n");
    let commit = repo.commit(
        &[
            ("src/main.rs", Entry::Text(&edited)),
            ("new/name.txt", Entry::Text("moved content\n")),
            ("same.txt", Entry::Text("same\n")),
            ("added.md", Entry::Text("a\nb\n")),
        ],
        Some(&base),
    );
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    let paths: Vec<&str> = set.turn.iter().map(|file| file.path.as_str()).collect();
    assert_eq!(
        paths,
        ["added.md", "gone.txt", "new/name.txt", "src/main.rs"]
    );
    let main = find(&set.turn, "src/main.rs");
    assert_eq!(main.status, ChangeStatus::Modified);
    assert_eq!((main.additions, main.deletions), (Some(3), Some(2)));
    assert!(main.diff.starts_with("diff --git a/src/main.rs b/src/main.rs\n--- a/src/main.rs\n+++ b/src/main.rs\n@@ -2,7 +2,7 @@\n"));
    assert!(main.diff.contains("\n-line 5\n+line five\n"));
    let renamed = find(&set.turn, "new/name.txt");
    assert_eq!(renamed.status, ChangeStatus::Renamed);
    assert_eq!(renamed.from_path.as_deref(), Some("old/name.txt"));
    assert_eq!((renamed.additions, renamed.deletions), (Some(0), Some(0)));
    assert_eq!(renamed.diff, "");
    let added = find(&set.turn, "added.md");
    assert_eq!(added.status, ChangeStatus::Added);
    assert_eq!((added.additions, added.deletions), (Some(2), Some(0)));
    assert_eq!(
        added.diff,
        "diff --git a/added.md b/added.md\nnew file mode 100644\n--- /dev/null\n+++ b/added.md\n@@ -0,0 +1,2 @@\n+a\n+b\n"
    );
    let gone = find(&set.turn, "gone.txt");
    assert_eq!(gone.status, ChangeStatus::Deleted);
    assert_eq!((gone.additions, gone.deletions), (Some(0), Some(1)));
    assert_eq!(set.turn_omitted, 0);
    // The turn starts from the snapshot: both scopes are the same.
    assert_eq!(set.cumulative, set.turn);
}

#[test]
fn the_cumulative_scope_is_measured_from_the_handoff_snapshot() {
    let mut repo = Repo::new();
    let snapshot = repo.commit(&[("a.txt", Entry::Text("one\n"))], None);
    let first = repo.commit(
        &[
            ("a.txt", Entry::Text("two\n")),
            ("b.txt", Entry::Text("b\n")),
        ],
        Some(&snapshot),
    );
    let second = repo.commit(
        &[
            ("a.txt", Entry::Text("two\n")),
            ("b.txt", Entry::Text("bee\n")),
        ],
        Some(&first),
    );
    let set = compute(&repo.db(), &snapshot, &first, &second).expect("changes");
    let turn: Vec<&str> = set.turn.iter().map(|file| file.path.as_str()).collect();
    assert_eq!(turn, ["b.txt"]);
    assert_eq!(find(&set.turn, "b.txt").status, ChangeStatus::Modified);
    let cumulative: Vec<&str> = set.cumulative.iter().map(|f| f.path.as_str()).collect();
    assert_eq!(cumulative, ["a.txt", "b.txt"]);
    assert_eq!(find(&set.cumulative, "b.txt").status, ChangeStatus::Added);
    assert_eq!(
        (set.base.as_str(), set.parent.as_str()),
        (snapshot.as_str(), first.as_str())
    );
}

#[test]
fn binary_files_and_symlinks_are_listed_without_counts_or_diff() {
    let mut repo = Repo::new();
    let base = repo.commit(
        &[
            ("logo.png", Entry::Bytes(vec![0x89, b'P', 0, 1])),
            ("link", Entry::Link("a.txt")),
        ],
        None,
    );
    let commit = repo.commit(
        &[
            ("logo.png", Entry::Bytes(vec![0x89, b'P', 0, 2])),
            ("link", Entry::Link("b.txt")),
        ],
        Some(&base),
    );
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    for path in ["logo.png", "link"] {
        let file = find(&set.turn, path);
        assert!(file.binary, "{path}");
        assert_eq!((file.additions, file.deletions), (None, None));
        assert_eq!(file.diff, "");
    }
}

#[test]
fn a_blob_over_the_limit_is_too_large() {
    let mut repo = Repo::new();
    let big = "x\n".repeat(MAX_BLOB_BYTES / 2 + 1);
    let base = repo.commit(&[("small.txt", Entry::Text("s\n"))], None);
    let commit = repo.commit(
        &[
            ("small.txt", Entry::Text("s\n")),
            ("big.txt", Entry::Text(&big)),
        ],
        Some(&base),
    );
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    let file = find(&set.turn, "big.txt");
    assert!(file.too_large && !file.binary);
    assert_eq!((file.additions, file.deletions), (None, None));
    assert_eq!(file.diff, "");
}

#[test]
fn a_rewrite_past_the_middle_bound_is_too_large_and_an_edit_in_a_long_file_is_counted() {
    let mut repo = Repo::new();
    let long = numbered(1, 5_000);
    let rewritten = long.replace("line", "row");
    let edited = long.replace("line 2500\n", "line 2500 edited\n");
    let base = repo.commit(
        &[
            ("rewrite.txt", Entry::Text(&long)),
            ("edit.txt", Entry::Text(&long)),
        ],
        None,
    );
    let commit = repo.commit(
        &[
            ("rewrite.txt", Entry::Text(&rewritten)),
            ("edit.txt", Entry::Text(&edited)),
        ],
        Some(&base),
    );
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    let rewrite = find(&set.turn, "rewrite.txt");
    assert!(rewrite.too_large);
    assert_eq!(rewrite.additions, None);
    let edit = find(&set.turn, "edit.txt");
    assert!(!edit.too_large);
    assert_eq!((edit.additions, edit.deletions), (Some(1), Some(1)));
    assert!(edit.diff.contains("@@ -2497,7 +2497,7 @@\n"));
}

#[test]
fn files_past_the_listing_bound_are_counted_as_omitted() {
    let mut repo = Repo::new();
    let base = repo.commit(&[], None);
    let names: Vec<String> = (0..=MAX_FILES)
        .map(|index| format!("f/{index:05}.txt"))
        .collect();
    let contents: Vec<String> = (0..=MAX_FILES).map(|index| format!("{index}\n")).collect();
    let files: Vec<(&str, Entry<'_>)> = names
        .iter()
        .zip(&contents)
        .map(|(name, content)| (name.as_str(), Entry::Text(content)))
        .collect();
    let commit = repo.commit(&files, Some(&base));
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    assert_eq!(set.turn.len(), MAX_FILES);
    assert_eq!(set.turn_omitted, 1);
    assert_eq!(
        set.turn.last().map(|f| f.path.as_str()),
        Some("f/00999.txt")
    );
}

#[test]
fn the_scope_diff_budget_keeps_counts_and_drops_later_diffs() {
    let mut repo = Repo::new();
    // Each added file's diff is about 40 KiB: six fit in 256 KiB.
    let body = "y".repeat(99) + "\n";
    let content = body.repeat(400);
    let names: Vec<String> = (0..8).map(|index| format!("d{index}.txt")).collect();
    let base = repo.commit(&[], None);
    let files: Vec<(&str, Entry<'_>)> = names
        .iter()
        .map(|name| (name.as_str(), Entry::Text(&content)))
        .collect();
    let commit = repo.commit(&files, Some(&base));
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    let kept: Vec<bool> = set.turn.iter().map(|file| !file.diff.is_empty()).collect();
    assert_eq!(kept, [true, true, true, true, true, true, false, false]);
    assert!(set.turn.iter().all(|file| file.additions == Some(400)));
    let total: usize = set.turn.iter().map(|file| file.diff.len()).sum();
    assert!(total <= MAX_SCOPE_DIFF_BYTES);
}

#[test]
fn a_diff_over_the_per_file_bound_is_not_kept() {
    let mut repo = Repo::new();
    let content = "z".repeat(99) + "\n";
    let content = content.repeat(600);
    let base = repo.commit(&[], None);
    let commit = repo.commit(&[("wide.txt", Entry::Text(&content))], Some(&base));
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    let file = find(&set.turn, "wide.txt");
    assert_eq!(file.additions, Some(600));
    assert!(file.diff.is_empty() && !file.too_large);
}

#[test]
fn the_codec_reads_the_change_set_from_the_quarantine() {
    let mut repo = Repo::new();
    let base = repo.commit(&[("a.txt", Entry::Text("a\n"))], None);
    let commit = repo.commit(&[("a.txt", Entry::Text("b\n"))], Some(&base));
    let codec = super::super::gix_codec::StoreCodec;
    let set =
        super::super::codec::WorkspaceCodec::changes(&codec, repo.git_dir(), &base, &base, &commit)
            .expect("changes");
    assert_eq!(set.turn.len(), 1);
    assert!(
        super::super::codec::WorkspaceCodec::changes(
            &codec,
            repo.git_dir(),
            &base,
            &base,
            &"0".repeat(40)
        )
        .is_err()
    );
}

#[test]
fn record_frames_are_a_header_then_turn_then_cumulative_files_under_the_request_bound() {
    let claim = WorkspaceClaimV1 {
        thread_uuid: "00000000-0000-4000-8000-000000000001".to_owned(),
        epoch: 3,
        claim_id: "c".to_owned(),
        ..WorkspaceClaimV1::default()
    };
    let largest = ChangedFile {
        path: "p".repeat(MAX_PATH_BYTES),
        from_path: Some("q".repeat(MAX_PATH_BYTES)),
        status: ChangeStatus::Renamed,
        additions: Some(u32::MAX),
        deletions: Some(u32::MAX),
        binary: false,
        too_large: false,
        diff: "d".repeat(MAX_FILE_DIFF_BYTES),
    };
    let set = ChangeSet {
        commit: "1".repeat(40),
        parent: "2".repeat(40),
        base: "3".repeat(40),
        depth: 6,
        turn: vec![largest.clone()],
        turn_omitted: 4,
        cumulative: vec![ChangedFile {
            status: ChangeStatus::Added,
            from_path: None,
            ..largest
        }],
        cumulative_omitted: 5,
    };
    let frames = frames(&claim, &set);
    assert_eq!(frames.len(), 3);
    let Some(workspace_changes_frame_v1::Frame::Header(header)) = &frames[0].frame else {
        panic!("header first");
    };
    assert_eq!(header.claim.as_ref(), Some(&claim));
    assert_eq!((header.turn_omitted, header.cumulative_omitted), (4, 5));
    assert_eq!(header.depth, 6);
    assert_eq!(header.commit, set.commit);
    let Some(workspace_changes_frame_v1::Frame::File(turn)) = &frames[1].frame else {
        panic!("a file");
    };
    assert_eq!(turn.scope, WorkspaceChangeScopeV1::Turn as i32);
    assert_eq!(turn.status, WorkspaceChangeStatusV1::Renamed as i32);
    assert_eq!(
        turn.from_path,
        set.turn[0].from_path.clone().unwrap_or_default()
    );
    let Some(workspace_changes_frame_v1::Frame::File(cumulative)) = &frames[2].frame else {
        panic!("a file");
    };
    assert_eq!(cumulative.scope, WorkspaceChangeScopeV1::Cumulative as i32);
    assert_eq!(cumulative.from_path, "");
    for frame in &frames {
        assert!(frame.encoded_len() < 64 * 1_024);
    }
}

#[test]
fn a_record_response_is_recorded_or_exactly_one_refusal_or_rejection() {
    let recorded = WorkspaceRecordChangesResponseV1 {
        recorded: true,
        ..Default::default()
    };
    assert!(read_record_changes(&recorded).is_ok());
    let refused = WorkspaceRecordChangesResponseV1 {
        refusal: Some(WorkspaceRefusalV1 {
            code: 3,
            ..Default::default()
        }),
        ..Default::default()
    };
    assert!(matches!(
        read_record_changes(&refused),
        Err(StoreCallError::Refused { .. })
    ));
    let rejected = WorkspaceRecordChangesResponseV1 {
        rejection: Some(RuntimeErrorV1::default()),
        ..Default::default()
    };
    assert!(matches!(
        read_record_changes(&rejected),
        Err(StoreCallError::Rejected { .. })
    ));
    let both = WorkspaceRecordChangesResponseV1 {
        recorded: true,
        rejection: Some(RuntimeErrorV1::default()),
        ..Default::default()
    };
    assert!(matches!(
        read_record_changes(&both),
        Err(StoreCallError::Malformed(_))
    ));
}

#[test]
fn the_handoff_snapshot_is_the_single_handoff_ref() {
    let refs: BTreeMap<String, String> = [
        ("refs/elitea/base".to_owned(), "b".repeat(40)),
        ("refs/elitea/handoff/1".to_owned(), "a".repeat(40)),
    ]
    .into();
    assert_eq!(handoff_snapshot(&refs), Some("a".repeat(40)));
    let mut two = refs.clone();
    two.insert("refs/elitea/handoff/2".to_owned(), "c".repeat(40));
    assert_eq!(handoff_snapshot(&two), None);
}

#[test]
fn empty_files_are_never_paired_as_a_rename() {
    let mut repo = Repo::new();
    let base = repo.commit(
        &[
            ("pkg/__init__.py", Entry::Text("")),
            ("keep.txt", Entry::Text("k\n")),
        ],
        None,
    );
    let commit = repo.commit(
        &[
            ("lib/__init__.py", Entry::Text("")),
            ("keep.txt", Entry::Text("k\n")),
        ],
        Some(&base),
    );
    let set = compute(&repo.db(), &base, &base, &commit).expect("changes");
    assert_eq!(set.turn.len(), 2);
    assert_eq!(
        find(&set.turn, "lib/__init__.py").status,
        ChangeStatus::Added
    );
    assert_eq!(
        find(&set.turn, "pkg/__init__.py").status,
        ChangeStatus::Deleted
    );
    assert!(set.turn.iter().all(|file| file.from_path.is_none()));
}

#[test]
fn a_path_main_would_refuse_is_omitted_alone() {
    for path in [
        "a/.GIT/config",
        ".git",
        "a//b",
        "a/./b",
        "a/../b",
        "/abs",
        "trailing/",
        "nul\0byte",
        "",
    ] {
        assert!(!valid_path(path), "{path:?} is refused");
    }
    assert!(valid_path("src/.gitignore"));
    assert!(valid_path(&"a".repeat(MAX_PATH_BYTES)));
    assert!(!valid_path(&"a".repeat(MAX_PATH_BYTES + 1)));

    let mut repo = Repo::new();
    let base = repo.commit(&[("x.txt", Entry::Text("x\n"))], None);
    let db = repo.db();
    let blob = db
        .flatten(&db.commit(&oid(&base).expect("oid")).expect("commit").tree)
        .expect("tree")
        .remove("x.txt")
        .expect("blob");
    let after: BTreeMap<String, FileEntry> = [
        ("ok.txt".to_owned(), blob),
        ("vendor/.Git/HEAD".to_owned(), blob),
        ("a/../escape".to_owned(), blob),
    ]
    .into_iter()
    .collect();
    let (files, omitted) = scope(&db, &BTreeMap::new(), &after).expect("scope");
    assert_eq!(omitted, 2);
    assert_eq!(files.len(), 1);
    assert_eq!(files[0].path, "ok.txt");
    assert_eq!(files[0].additions, Some(1));
}

#[test]
fn the_depth_is_the_turn_number_from_the_snapshot() {
    let mut repo = Repo::new();
    let snapshot = repo.commit(&[("a.txt", Entry::Text("one\n"))], None);
    let first = repo.commit(&[("a.txt", Entry::Text("two\n"))], Some(&snapshot));
    let second = repo.commit(&[("a.txt", Entry::Text("three\n"))], Some(&first));
    // A regenerate of the first turn: its parent is the snapshot again.
    let regenerated = repo.commit(&[("a.txt", Entry::Text("deux\n"))], Some(&snapshot));
    let db = repo.db();
    let depth_of = |parent: &str, commit: &str| {
        compute(&db, &snapshot, parent, commit)
            .expect("changes")
            .depth
    };
    assert_eq!(depth_of(&snapshot, &first), 1);
    assert_eq!(depth_of(&first, &second), 2);
    assert_eq!(depth_of(&snapshot, &regenerated), 1);
    // The snapshot not on the commit's first-parent chain: unknown.
    let unrelated = repo.commit(&[("z.txt", Entry::Text("z\n"))], None);
    let set = compute(&repo.db(), &snapshot, &unrelated, &unrelated).expect("changes");
    assert_eq!(set.depth, 0);
}
