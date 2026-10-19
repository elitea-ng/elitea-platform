//! The worker's pull-request diff: rendered from U to the workspace, the
//! limits checked before rendering, applied back by real git.

use std::fs;
use std::path::Path;

use elitea_agent_runtime::toolkits::workspace_pull_request::DiffUnavailable;
use elitea_workspace_store::bundle;
use elitea_workspace_store::objects::ObjectDb;
use gix_object::Write as _;

use super::*;

fn numbered(from: usize, to: usize) -> String {
    use std::fmt::Write as _;
    (from..=to).fold(String::new(), |mut out, index| {
        let _ = writeln!(out, "line {index}");
        out
    })
}

/// A quarantine holding U (a commit of `files`, with the trailer) and the
/// worktree checked out from it.
fn fixture(files: &[(&str, &str)], trailer: &str) -> (tempfile::TempDir, ObjectDb, String) {
    let scratch = tempfile::tempdir().expect("scratch");
    let git = scratch.path().join("q.git");
    bundle::init_quarantine(&git).expect("init");
    let source = scratch.path().join("u");
    for (path, content) in files {
        let target = source.join(path);
        fs::create_dir_all(target.parent().expect("parent")).expect("mkdir");
        fs::write(target, content).expect("write");
    }
    let loose = gix_odb::loose::Store::at(git.join("objects"), gix_hash::Kind::Sha1);
    let tree = super::super::gix_codec::write_tree(&loose, &source, 0)
        .expect("tree")
        .expect("files");
    let raw = format!(
        "tree {tree}\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nElitea upstream\n\n{trailer}\n"
    );
    let commit = loose
        .write_buf(gix_object::Kind::Commit, raw.as_bytes())
        .expect("commit");
    // The worktree starts as U's checkout.
    let work = scratch.path().join("work");
    for (path, content) in files {
        let target = work.join(path);
        fs::create_dir_all(target.parent().expect("parent")).expect("mkdir");
        fs::write(target, content).expect("write");
    }
    let db = ObjectDb::open_objects(&git.join("objects")).expect("db");
    (scratch, db, commit.to_string())
}

const TRAILER: &str = "Elitea-Upstream: 3333333333333333333333333333333333333333 https://user:token@github.com/org/repo.git main";

#[test]
fn the_diff_renders_create_modify_delete_and_git_applies_it() {
    let original = numbered(1, 30);
    let (scratch, db, upstream) = fixture(
        &[
            ("README.md", "hello\nworld\n"),
            ("gone.txt", "bye\n"),
            ("same.txt", "unchanged\n"),
            ("src/main.rs", &original),
        ],
        TRAILER,
    );
    let work = scratch.path().join("work");
    fs::write(work.join("README.md"), "hello\nthere\nworld").expect("w");
    fs::remove_file(work.join("gone.txt")).expect("rm");
    fs::create_dir_all(work.join("new")).expect("mkdir");
    fs::write(work.join("new/file.txt"), "a\nb\n").expect("w");
    let changed = original
        .replace("line 5\n", "line five\n")
        .replace("line 25\n", "line twenty-five\nline extra\n");
    fs::write(work.join("src/main.rs"), &changed).expect("w");
    // A `.git` the agent made is never part of the diff.
    fs::create_dir_all(work.join(".git")).expect("git");
    fs::write(work.join(".git/HEAD"), "x").expect("w");

    let diff = pull_request_diff(&db, &upstream, &work).expect("diff");
    assert_eq!(diff.upstream_sha, "3".repeat(40));
    assert_eq!(diff.upstream_branch, "main");
    assert_eq!(diff.remote_url, "https://github.com/org/repo.git");
    let text = &diff.text;
    for want in [
        "diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
        "diff --git a/new/file.txt b/new/file.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new/file.txt\n@@ -0,0 +1,2 @@\n+a\n+b\n",
        "+there\n",
        "\\ No newline at end of file\n",
        "-line 5\n+line five\n",
        "+line extra\n",
    ] {
        assert!(text.contains(want), "lacks {want:?}:\n{text}");
    }
    assert!(!text.contains("same.txt") && !text.contains(".git/"));
    let section = &text[text.find("diff --git a/src/main.rs").expect("main")..];
    assert_eq!(section.matches("@@ -").count(), 2, "{section}");
    assert_git_applies(scratch.path(), &db, &upstream, &work, text);
}

#[test]
fn many_edits_round_trip_through_git_apply() {
    let old = numbered(1, 200);
    let (scratch, db, upstream) = fixture(&[("f.txt", &old)], TRAILER);
    let mut updated = old.clone();
    for index in [1, 2, 50, 51, 52, 120, 199, 200] {
        updated = updated.replacen(
            &format!("line {index}\n"),
            &format!("edited {index}\nadded {index}\n"),
            1,
        );
    }
    updated = updated.replacen("line 80\nline 81\nline 82\n", "", 1);
    let work = scratch.path().join("work");
    fs::write(work.join("f.txt"), &updated).expect("w");
    let diff = pull_request_diff(&db, &upstream, &work).expect("diff");
    assert_git_applies(scratch.path(), &db, &upstream, &work, &diff.text);
}

#[test]
fn binary_large_empty_and_no_upstream_are_refused_before_rendering() {
    let (scratch, db, upstream) = fixture(&[("a.txt", "a\n")], TRAILER);
    let work = scratch.path().join("work");
    assert_eq!(
        pull_request_diff(&db, &upstream, &work).err(),
        Some(DiffUnavailable::Empty)
    );
    fs::write(work.join("blob.bin"), b"\0\x01\x02").expect("w");
    assert_eq!(
        pull_request_diff(&db, &upstream, &work).err(),
        Some(DiffUnavailable::Binary)
    );
    fs::remove_file(work.join("blob.bin")).expect("rm");
    std::os::unix::fs::symlink("a.txt", work.join("link")).expect("link");
    assert_eq!(
        pull_request_diff(&db, &upstream, &work).err(),
        Some(DiffUnavailable::Binary)
    );
    fs::remove_file(work.join("link")).expect("rm");
    for index in 0..=MAX_DIFF_FILES {
        fs::write(work.join(format!("f{index}.txt")), "x\n").expect("w");
    }
    assert_eq!(
        pull_request_diff(&db, &upstream, &work).err(),
        Some(DiffUnavailable::TooLarge)
    );
    for index in 0..=MAX_DIFF_FILES {
        fs::remove_file(work.join(format!("f{index}.txt"))).expect("rm");
    }
    fs::write(
        work.join("big.txt"),
        "0123456789abcdef\n".repeat(MAX_DIFF_BYTES / 17 + 10),
    )
    .expect("w");
    assert_eq!(
        pull_request_diff(&db, &upstream, &work).err(),
        Some(DiffUnavailable::TooLarge)
    );
    let (scratch, db, upstream) = fixture(&[("a.txt", "a\n")], "no trailer");
    fs::write(scratch.path().join("work/a.txt"), "b\n").expect("w");
    assert_eq!(
        pull_request_diff(&db, &upstream, &scratch.path().join("work")).err(),
        Some(DiffUnavailable::NoUpstream)
    );
}

#[test]
fn upstream_trailers_and_remote_urls_are_read_safely() {
    let upstream = parse_upstream_trailer(&format!(
        "Elitea upstream\n\nElitea-Upstream: {} https://x:y@github.com/o/r.git feature/one\n",
        "4".repeat(40)
    ))
    .expect("trailer");
    assert_eq!(upstream.branch, "feature/one");
    assert_eq!(upstream.remote_url, "https://github.com/o/r.git");
    assert!(parse_upstream_trailer("no trailer").is_none());
    assert_eq!(
        strip_url_credentials("git@github.com:o/r.git"),
        "github.com:o/r.git"
    );
    assert_eq!(
        strip_url_credentials("ssh://git@github.com/o/r.git"),
        "ssh://github.com/o/r.git"
    );
}

/// Apply the diff with real git (when installed) to U's files and compare
/// with the worktree.
fn assert_git_applies(scratch: &Path, db: &ObjectDb, upstream: &str, work: &Path, patch: &str) {
    let Some(git) = std::env::var_os("PATH").and_then(|paths| {
        std::env::split_paths(&paths)
            .map(|dir| dir.join("git"))
            .find(|candidate| candidate.is_file())
    }) else {
        eprintln!("git is not installed; the git apply round trip is skipped");
        return;
    };
    let target = scratch.join("applied");
    let commit = db
        .commit(&elitea_workspace_store::objects::oid(upstream).expect("oid"))
        .expect("commit");
    for (path, entry) in db.flatten(&commit.tree).expect("tree") {
        let file = target.join(&path);
        fs::create_dir_all(file.parent().expect("parent")).expect("mkdir");
        fs::write(file, db.blob(&entry.oid).expect("blob")).expect("write");
    }
    let patch_file = scratch.join("change.patch");
    fs::write(&patch_file, patch).expect("patch");
    let output = std::process::Command::new(git)
        .args(["apply", "--unsafe-paths", "--directory="])
        .arg(&patch_file)
        .current_dir(&target)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("HOME", scratch)
        .output()
        .expect("git runs");
    assert!(
        output.status.success(),
        "git apply: {}\n{patch}",
        String::from_utf8_lossy(&output.stderr)
    );
    let mut expected = std::collections::BTreeMap::new();
    super::walk(work, work, &mut expected).expect("walk");
    let mut applied = std::collections::BTreeMap::new();
    super::walk(&target, &target, &mut applied).expect("walk");
    assert_eq!(
        applied.keys().collect::<Vec<_>>(),
        expected.keys().collect::<Vec<_>>()
    );
    for (path, file) in expected {
        assert_eq!(
            fs::read(&applied[&path]).expect("applied"),
            fs::read(file).expect("work"),
            "{path}"
        );
    }
}
