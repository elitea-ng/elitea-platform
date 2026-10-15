//! Fixtures of the handoff tests: repositories built with the git CLI in
//! temporary directories (never this checkout), and a fingerprint of
//! everything a send must leave byte-identical.
#![allow(
    dead_code,
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::missing_panics_doc
)]

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;

use elitea_local_tools::deny::DenyList;
use elitea_local_tools::handoff::caps::Caps;
use elitea_local_tools::handoff::snapshot::SendConfig;
use elitea_local_tools::sandbox::SandboxConfig;
use sha2::{Digest as _, Sha256};

pub const CONVERSATION: &str = "c0ffee00-0000-4000-8000-000000000001";

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
            "-c",
            "protocol.file.allow=always",
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

pub fn write(top: &Path, path: &str, text: &str) {
    let full = top.join(path);
    if let Some(parent) = full.parent() {
        std::fs::create_dir_all(parent).unwrap();
    }
    std::fs::write(full, text).unwrap();
}

pub struct Fixture {
    pub dir: tempfile::TempDir,
    /// Canonical.
    pub base: PathBuf,
    pub top: PathBuf,
    pub data: PathBuf,
    pub global: PathBuf,
}

impl Fixture {
    pub fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let base = std::fs::canonicalize(dir.path()).unwrap();
        let top = base.join("repo");
        let data = base.join("app-data");
        std::fs::create_dir_all(&top).unwrap();
        std::fs::create_dir_all(&data).unwrap();
        let global = base.join("gitconfig");
        std::fs::write(&global, "").unwrap();
        Self {
            dir,
            base,
            top,
            data,
            global,
        }
    }

    /// A repository with a commit, then staged, unstaged, untracked and
    /// ignored changes, a tracked `.env` and a tracked `id_rsa`.
    pub fn repo() -> Self {
        let fixture = Self::new();
        let top = &fixture.top;
        git(top, &["init", "-q"]);
        write(top, ".gitignore", "ignored.log\nbuild/\n");
        write(top, "README.md", "hello\n");
        write(top, "src/lib.rs", "pub fn a() {}\n");
        write(top, "src/gone.rs", "// deleted later\n");
        write(top, ".env", "TOKEN=local-only\n");
        write(top, "keys/id_rsa", "not really a key\n");
        std::fs::write(top.join("blob.bin"), [0u8, 159, 146, 150, 0, 1]).unwrap();
        git(top, &["add", "-A"]);
        git(top, &["commit", "-q", "-m", "first"]);
        // Staged, unstaged, untracked, ignored; a deletion.
        write(top, "staged.txt", "staged\n");
        git(top, &["add", "staged.txt"]);
        write(top, "README.md", "hello, edited\n");
        write(top, "untracked/new.md", "new\n");
        write(top, "ignored.log", "noise\n");
        write(top, "build/out.o", "obj\n");
        std::fs::remove_file(top.join("src/gone.rs")).unwrap();
        fixture
    }

    pub fn config(&self, leave: &[&str]) -> SendConfig {
        SendConfig {
            root: self.top.clone(),
            data_dir: self.data.clone(),
            sandbox: SandboxConfig::default(),
            deny: Arc::new(DenyList::new([self.data.join("secrets")])),
            path_deny: vec!["private/**".to_owned()],
            leave_on_laptop: leave.iter().map(|s| (*s).to_owned()).collect(),
            patterns: elitea_local_tools::handoff::exclude::Patterns::default(),
            include: Vec::new(),
            caps: Caps::default(),
            conversation_uuid: CONVERSATION.to_owned(),
            note: "Changed README; ran tests.".to_owned(),
            global_config: Some(self.global.clone()),
        }
    }
}

fn hash_file(path: &Path) -> Option<String> {
    std::fs::read(path)
        .ok()
        .map(|bytes| format!("{:x}", Sha256::digest(bytes)))
}

fn walk(root: &Path, dir: &Path, skip_git: bool, out: &mut BTreeMap<String, String>) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let rel = path
            .strip_prefix(root)
            .unwrap()
            .to_string_lossy()
            .into_owned();
        if skip_git && rel == ".git" {
            continue;
        }
        let meta = std::fs::symlink_metadata(&path).unwrap();
        if meta.is_dir() {
            out.insert(format!("{rel}/"), String::new());
            walk(root, &path, skip_git, out);
        } else {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = meta.permissions().mode();
            out.insert(
                rel,
                format!("{mode:o}:{}", hash_file(&path).unwrap_or_default()),
            );
        }
    }
}

/// The working tree (every file, its mode and bytes).
pub fn tree_fingerprint(top: &Path) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    walk(top, top, true, &mut out);
    out
}

/// The index, `HEAD`, branches, tags, stash and the reflogs.
pub fn git_state_fingerprint(top: &Path) -> BTreeMap<String, String> {
    let git_dir = top.join(".git");
    let mut out = BTreeMap::new();
    for name in ["index", "HEAD", "packed-refs", "ORIG_HEAD", "config"] {
        out.insert(
            name.to_owned(),
            hash_file(&git_dir.join(name)).unwrap_or_default(),
        );
    }
    for dir in [
        "refs/heads",
        "refs/tags",
        "refs/stash",
        "refs/remotes",
        "logs",
    ] {
        let mut sub = BTreeMap::new();
        walk(&git_dir, &git_dir.join(dir), false, &mut sub);
        out.extend(sub);
    }
    if let Ok(stash) = std::fs::read(git_dir.join("refs/stash")) {
        out.insert(
            "refs/stash".to_owned(),
            String::from_utf8_lossy(&stash).into_owned(),
        );
    }
    out
}

/// Every ref of the repository (`for-each-ref`).
pub fn all_refs(top: &Path) -> String {
    git(top, &["for-each-ref", "--format=%(refname) %(objectname)"])
}
