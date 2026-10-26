use std::path::Path;
use std::process::Command;

use elitea_agent_runtime::toolkits::workspace_pull_request::PushUnavailable;
use gix_hash::ObjectId;
use gix_object::Write as _;

use super::prepare_push;

const SIGNATURE: &str = "Elitea cloud executor <cloud-executor@elitea.invalid> 1700000000 +0000";
const PERSON: &str = "Dev <dev@example.com> 1600000000 +0000";

struct Repo {
    loose: gix_odb::loose::Store,
}

impl Repo {
    fn at(git_dir: &Path) -> Self {
        elitea_workspace_store::bundle::init_quarantine(git_dir).expect("quarantine");
        Self {
            loose: gix_odb::loose::Store::at(git_dir.join("objects"), gix_hash::Kind::Sha1),
        }
    }

    fn blob(&self, bytes: &[u8]) -> ObjectId {
        self.loose
            .write_buf(gix_object::Kind::Blob, bytes)
            .expect("blob")
    }

    /// A flat tree of `(name, blob)`, names in git order.
    fn tree(&self, entries: &[(&str, ObjectId)]) -> ObjectId {
        let entries: Vec<(&str, &str, ObjectId)> = entries
            .iter()
            .map(|(name, id)| ("100644", *name, *id))
            .collect();
        self.tree_of(&entries)
    }

    /// A tree of `(mode, name, id)`, names in git order.
    fn tree_of(&self, entries: &[(&str, &str, ObjectId)]) -> ObjectId {
        let mut raw = Vec::new();
        for (mode, name, id) in entries {
            raw.extend_from_slice(format!("{mode} {name}\0").as_bytes());
            raw.extend_from_slice(id.as_bytes());
        }
        self.loose
            .write_buf(gix_object::Kind::Tree, &raw)
            .expect("tree")
    }

    fn commit(&self, tree: ObjectId, parent: Option<ObjectId>, message: &str) -> ObjectId {
        let parent = parent
            .map(|id| format!("parent {id}\n"))
            .unwrap_or_default();
        let raw = format!("tree {tree}\n{parent}author {PERSON}\ncommitter {PERSON}\n\n{message}");
        self.loose
            .write_buf(gix_object::Kind::Commit, raw.as_bytes())
            .expect("commit")
    }
}

/// The store history of a handed-off thread: U (R's tree, the upstream
/// trailer), B (the desktop's HEAD = R's tree), H (an uncommitted edit),
/// one cloud turn (a binary file).
struct Thread {
    _scratch: tempfile::TempDir,
    git_dir: std::path::PathBuf,
    worktree: std::path::PathBuf,
    upstream_commit: ObjectId,
    head: ObjectId,
    r: ObjectId,
}

fn thread(r: ObjectId, r_file: &[u8]) -> Thread {
    let scratch = tempfile::tempdir().expect("scratch");
    let git_dir = scratch.path().join("quarantine.git");
    let repo = Repo::at(&git_dir);
    let r_tree = repo.tree(&[("a.txt", repo.blob(r_file))]);
    let upstream = repo.commit(
        r_tree,
        None,
        &format!("Elitea upstream\n\nElitea-Upstream: {r} https://github.com/o/r.git main\n"),
    );
    let base = repo.commit(r_tree, None, &format!("Elitea base\n\nElitea-Base: {r}\n"));
    let h_tree = repo.tree(&[("a.txt", repo.blob(b"two\n"))]);
    let handoff = repo.commit(h_tree, Some(base), "Elitea handoff\n\nElitea-Handoff: 1\n");
    let binary = repo.blob(b"\0\x01binary\xff");
    let c_tree = repo.tree(&[("a.txt", repo.blob(b"two\n")), ("bin.dat", binary)]);
    let turn = repo.commit(
        c_tree,
        Some(handoff),
        "Elitea cloud turn\n\nElitea-Execution-Id: e-1\nElitea-Epoch: 1\n",
    );
    let worktree = scratch.path().join("work");
    std::fs::create_dir_all(&worktree).expect("work");
    std::fs::write(worktree.join("a.txt"), b"three\n").expect("a.txt");
    std::fs::write(worktree.join("bin.dat"), b"\0\x01binary\xff").expect("bin.dat");
    Thread {
        _scratch: scratch,
        git_dir,
        worktree,
        upstream_commit: upstream,
        head: turn,
        r,
    }
}

fn git() -> Option<std::path::PathBuf> {
    [
        "/usr/bin/git",
        "/opt/homebrew/bin/git",
        "/usr/local/bin/git",
    ]
    .into_iter()
    .map(std::path::PathBuf::from)
    .find(|path| path.is_file())
}

fn run(git: &Path, dir: &Path, args: &[&str]) -> String {
    let output = Command::new(git)
        .current_dir(dir)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("HOME", dir)
        .args(args)
        .output()
        .expect("git runs");
    assert!(output.status.success(), "git {args:?}: {output:?}");
    String::from_utf8(output.stdout).expect("utf-8")
}

/// The pack lands on a real git repository that holds only R, its history
/// connects, the binary is there, and no store trailer leaks.
#[test]
fn the_thread_is_re_parented_onto_the_upstream_commit_and_a_real_git_accepts_it() {
    let Some(git) = git() else {
        eprintln!("no git binary: the pack is checked without it");
        return;
    };
    let origin = tempfile::tempdir().expect("origin");
    let dir = origin.path();
    run(&git, dir, &["init", "-q", "-b", "main"]);
    std::fs::write(dir.join("a.txt"), b"one\n").expect("a.txt");
    run(&git, dir, &["add", "a.txt"]);
    run(
        &git,
        dir,
        &[
            "-c",
            "user.name=Dev",
            "-c",
            "user.email=dev@example.com",
            "commit",
            "-qm",
            "R",
        ],
    );
    let r: ObjectId = run(&git, dir, &["rev-parse", "HEAD"])
        .trim()
        .parse()
        .expect("R");
    let thread = thread(r, b"one\n");
    let prepared = prepare_push(
        &thread.git_dir,
        &thread.upstream_commit.to_string(),
        &thread.head.to_string(),
        &thread.worktree,
        1 << 20,
        SIGNATURE,
    )
    .expect("prepared");
    // B has R's tree: H, the cloud turn and the workspace are new commits.
    assert_eq!(prepared.commits, 3);
    assert_eq!(prepared.files, 2);
    assert_eq!(prepared.upstream.sha, thread.r.to_string());
    assert_eq!(prepared.upstream.branch, "main");

    let mut child = Command::new(&git)
        .current_dir(dir)
        .args(["index-pack", "--stdin"])
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::null())
        .spawn()
        .expect("index-pack");
    std::io::Write::write_all(
        child.stdin.as_mut().expect("stdin"),
        prepared.pack.as_slice(),
    )
    .expect("pack written");
    drop(child.stdin.take());
    assert!(child.wait().expect("index-pack").success());
    run(
        &git,
        dir,
        &["update-ref", "refs/heads/cloud", &prepared.tip],
    );
    run(&git, dir, &["fsck", "--connectivity-only", "--no-dangling"]);
    let log = run(&git, dir, &["log", "--format=%s|%an", "cloud"]);
    assert_eq!(
        log.lines().collect::<Vec<_>>(),
        [
            "Work in progress from the Elitea cloud thread|Elitea cloud executor",
            "Elitea cloud turn|Dev",
            "Elitea handoff|Dev",
            "R|Dev"
        ]
    );
    assert_eq!(run(&git, dir, &["show", "cloud:a.txt"]), "three\n");
    let raw = run(&git, dir, &["log", "--format=%B", "cloud"]);
    assert!(
        !raw.contains("Elitea-"),
        "no store trailer reaches the remote"
    );
    let binary = Command::new(&git)
        .current_dir(dir)
        .args(["cat-file", "blob", "cloud:bin.dat"])
        .output()
        .expect("cat-file");
    assert_eq!(binary.stdout, b"\0\x01binary\xff");
}

#[test]
fn nothing_changed_is_nothing_to_push_and_a_large_pack_is_refused() {
    let r: ObjectId = "e".repeat(40).parse().expect("id");
    let large = thread(r, b"one\n");
    assert!(matches!(
        prepare_push(
            &large.git_dir,
            &large.upstream_commit.to_string(),
            &large.head.to_string(),
            &large.worktree,
            8,
            SIGNATURE,
        ),
        Err(PushUnavailable::TooLarge)
    ));
    // The upstream commit as the head and its own files: no change.
    let same = thread(r, b"two\n");
    std::fs::remove_file(same.worktree.join("bin.dat")).expect("rm");
    std::fs::write(same.worktree.join("a.txt"), b"two\n").expect("a.txt");
    assert_eq!(
        prepare_push(
            &same.git_dir,
            &same.upstream_commit.to_string(),
            &same.upstream_commit.to_string(),
            &same.worktree,
            1 << 20,
            SIGNATURE,
        )
        .err(),
        Some(PushUnavailable::Empty)
    );
    assert!(matches!(
        prepare_push(
            &same.git_dir,
            &"0".repeat(40),
            &same.head.to_string(),
            &same.worktree,
            1 << 20,
            SIGNATURE,
        ),
        Err(PushUnavailable::NoUpstream)
    ));
}

/// Index `pack` into the repository at `dir` and point `refs/heads/cloud`
/// at `tip`.
fn land(git: &Path, dir: &Path, pack: &[u8], tip: &str) {
    let mut child = Command::new(git)
        .current_dir(dir)
        .args(["index-pack", "--stdin"])
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::null())
        .spawn()
        .expect("index-pack");
    std::io::Write::write_all(child.stdin.as_mut().expect("stdin"), pack).expect("pack written");
    drop(child.stdin.take());
    assert!(child.wait().expect("index-pack").success());
    run(git, dir, &["update-ref", "refs/heads/cloud", tip]);
    run(git, dir, &["fsck", "--connectivity-only", "--no-dangling"]);
}

fn commit_all(git: &Path, dir: &Path, message: &str) -> ObjectId {
    run(git, dir, &["add", "-A"]);
    run(
        git,
        dir,
        &[
            "-c",
            "user.name=Dev",
            "-c",
            "user.email=dev@example.com",
            "commit",
            "-qm",
            message,
        ],
    );
    run(git, dir, &["rev-parse", "HEAD"])
        .trim()
        .parse()
        .expect("a commit")
}

/// The handoff left tracked files out (an excluded secret, an oversize
/// file, a secret committed only locally): the pushed branch neither deletes
/// what the remote has at those paths nor uploads the local content the
/// remote lacks, in any commit, B's included.
#[test]
fn files_the_handoff_left_out_keep_the_remotes_state_and_are_never_uploaded() {
    let Some(git) = git() else {
        eprintln!("no git binary: the left-out paths are checked without it");
        return;
    };
    let origin = tempfile::tempdir().expect("origin");
    let dir = origin.path();
    run(&git, dir, &["init", "-q", "-b", "main"]);
    std::fs::write(dir.join("a.txt"), b"one\n").expect("a.txt");
    std::fs::write(dir.join(".env"), b"SECRET=upstream\n").expect(".env");
    std::fs::write(dir.join("big.bin"), b"big-upstream\n").expect("big.bin");
    let r = commit_all(&git, dir, "R");

    let scratch = tempfile::tempdir().expect("scratch");
    let git_dir = scratch.path().join("quarantine.git");
    let repo = Repo::at(&git_dir);
    let one = repo.blob(b"one\n");
    let env_upstream = repo.blob(b"SECRET=upstream\n");
    let big = repo.blob(b"big-upstream\n");
    let r_tree = repo.tree(&[(".env", env_upstream), ("a.txt", one), ("big.bin", big)]);
    let upstream = repo.commit(
        r_tree,
        None,
        &format!("Elitea upstream\n\nElitea-Upstream: {r} https://github.com/o/r.git main\n"),
    );
    // HEAD: a local commit changed the secret and added a key; big.bin is
    // the remote's. The send left all three out of H.
    let env_local = repo.blob(b"SECRET=local-commit\n");
    let key_local = repo.blob(b"LOCALKEY\n");
    let b_tree = repo.tree(&[
        (".env", env_local),
        ("a.txt", one),
        ("big.bin", big),
        ("local.key", key_local),
    ]);
    let base = repo.commit(b_tree, None, &format!("Elitea base\n\nElitea-Base: {r}\n"));
    let two = repo.blob(b"two\n");
    let h_tree = repo.tree(&[("a.txt", two)]);
    let handoff = repo.commit(h_tree, Some(base), "Elitea handoff\n\nElitea-Handoff: 1\n");
    let binary = repo.blob(b"\0\x01binary\xff");
    let c_tree = repo.tree(&[("a.txt", two), ("bin.dat", binary)]);
    let turn = repo.commit(
        c_tree,
        Some(handoff),
        "Elitea cloud turn\n\nElitea-Execution-Id: e-1\nElitea-Epoch: 1\n",
    );
    let worktree = scratch.path().join("work");
    std::fs::create_dir_all(&worktree).expect("work");
    std::fs::write(worktree.join("a.txt"), b"three\n").expect("a.txt");
    std::fs::write(worktree.join("bin.dat"), b"\0\x01binary\xff").expect("bin.dat");

    let prepared = prepare_push(
        &git_dir,
        &upstream.to_string(),
        &turn.to_string(),
        &worktree,
        1 << 20,
        SIGNATURE,
    )
    .expect("prepared");
    // a.txt and bin.dat differ from R; nothing else.
    assert_eq!(prepared.files, 2);
    land(&git, dir, prepared.pack.as_slice(), &prepared.tip);
    let changed = run(
        &git,
        dir,
        &["diff", "--name-status", &r.to_string(), "cloud"],
    );
    assert_eq!(
        changed.lines().collect::<Vec<_>>(),
        ["M\ta.txt", "A\tbin.dat"]
    );
    // No commit of the branch touches a left-out path: nothing is deleted
    // on the way and B publishes nothing of HEAD's secrets.
    let touched = run(
        &git,
        dir,
        &["log", "--name-only", "--format=", &format!("{r}..cloud")],
    );
    for path in [".env", "big.bin", "local.key"] {
        assert!(
            !touched.lines().any(|line| line == path),
            "{path} was touched: {touched}"
        );
    }
    assert_eq!(run(&git, dir, &["show", "cloud:.env"]), "SECRET=upstream\n");
    assert_eq!(run(&git, dir, &["show", "cloud:big.bin"]), "big-upstream\n");
    // The local content never reached the remote.
    for local in [env_local, key_local] {
        let present = Command::new(&git)
            .current_dir(dir)
            .args(["cat-file", "-e", &local.to_string()])
            .status()
            .expect("cat-file");
        assert!(!present.success(), "{local} was uploaded");
    }
}

/// A cloud turn made a directory where a left-out file is: keeping the
/// file is impossible, so nothing is pushed.
#[test]
fn a_left_out_file_the_cloud_replaced_by_a_directory_refuses_the_push() {
    let r: ObjectId = "e".repeat(40).parse().expect("id");
    let scratch = tempfile::tempdir().expect("scratch");
    let git_dir = scratch.path().join("quarantine.git");
    let repo = Repo::at(&git_dir);
    let one = repo.blob(b"one\n");
    let secret = repo.blob(b"SECRET=upstream\n");
    let r_tree = repo.tree(&[(".env", secret), ("a.txt", one)]);
    let upstream = repo.commit(
        r_tree,
        None,
        &format!("Elitea upstream\n\nElitea-Upstream: {r} https://github.com/o/r.git main\n"),
    );
    let base = repo.commit(r_tree, None, &format!("Elitea base\n\nElitea-Base: {r}\n"));
    let h_tree = repo.tree(&[("a.txt", one)]);
    let handoff = repo.commit(h_tree, Some(base), "Elitea handoff\n\nElitea-Handoff: 1\n");
    let worktree = scratch.path().join("work");
    std::fs::create_dir_all(worktree.join(".env")).expect(".env/");
    std::fs::write(worktree.join(".env/inner"), b"x\n").expect("inner");
    std::fs::write(worktree.join("a.txt"), b"one\n").expect("a.txt");
    assert_eq!(
        prepare_push(
            &git_dir,
            &upstream.to_string(),
            &handoff.to_string(),
            &worktree,
            1 << 20,
            SIGNATURE,
        )
        .err(),
        Some(PushUnavailable::ExcludedTracked)
    );
}

/// A missing store object is a damaged store, not a size: it is told apart
/// from the size cap.
#[test]
fn a_missing_object_is_a_store_integrity_error_not_too_large() {
    let r: ObjectId = "e".repeat(40).parse().expect("id");
    let thread = thread(r, b"one\n");
    // bin.dat's blob lives only in the cloud turn's tree once the
    // workspace no longer has it.
    std::fs::remove_file(thread.worktree.join("bin.dat")).expect("rm");
    let binary = gix_odb::loose::Store::at(thread.git_dir.join("objects"), gix_hash::Kind::Sha1)
        .write_buf(gix_object::Kind::Blob, b"\0\x01binary\xff")
        .expect("id");
    let hex = binary.to_string();
    std::fs::remove_file(
        thread
            .git_dir
            .join("objects")
            .join(&hex[..2])
            .join(&hex[2..]),
    )
    .expect("the loose blob");
    assert_eq!(
        prepare_push(
            &thread.git_dir,
            &thread.upstream_commit.to_string(),
            &thread.head.to_string(),
            &thread.worktree,
            1 << 20,
            SIGNATURE,
        )
        .err(),
        Some(PushUnavailable::StoreIntegrity)
    );
}
