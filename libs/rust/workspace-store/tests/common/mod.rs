//! Test repositories built with the git CLI (tests only; the crate itself
//! never runs git).
#![allow(
    dead_code,
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::missing_panics_doc
)]

use std::path::{Path, PathBuf};
use std::process::Command;

pub fn git(dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "init.defaultBranch=main",
            "-c",
            "user.name=T",
            "-c",
            "user.email=t@example.invalid",
            "-c",
            "commit.gpgSign=false",
        ])
        .args(args)
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .output()
        .expect("git");
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

/// A repository with a few files, a binary, an executable and a nested
/// directory, one commit; `(tempdir, work tree, HEAD)`.
pub fn repo() -> (tempfile::TempDir, PathBuf, String) {
    let dir = tempfile::tempdir().unwrap();
    let top = dir.path().join("repo");
    std::fs::create_dir_all(top.join("src/deep")).unwrap();
    git(&top, &["init", "-q"]);
    std::fs::write(top.join("README.md"), "hello\n").unwrap();
    std::fs::write(top.join("src/main.rs"), "fn main() {}\n").unwrap();
    std::fs::write(top.join("src/deep/data.bin"), [0u8, 1, 2, 255, 0, 7]).unwrap();
    std::fs::write(top.join("run.sh"), "#!/bin/sh\necho hi\n").unwrap();
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(top.join("run.sh"), std::fs::Permissions::from_mode(0o755))
            .unwrap();
    }
    git(&top, &["add", "-A"]);
    git(&top, &["commit", "-q", "-m", "first"]);
    let head = git(&top, &["rev-parse", "HEAD"]);
    (dir, top, head)
}

/// A new commit on top of HEAD with `path` set to `text`.
pub fn commit_file(top: &Path, path: &str, text: &str) -> String {
    if let Some(parent) = Path::new(path).parent() {
        std::fs::create_dir_all(top.join(parent)).unwrap();
    }
    std::fs::write(top.join(path), text).unwrap();
    git(top, &["add", "-A"]);
    git(top, &["commit", "-q", "-m", &format!("edit {path}")]);
    git(top, &["rev-parse", "HEAD"])
}
