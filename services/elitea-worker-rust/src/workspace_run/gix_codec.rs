//! [`WorkspaceCodec`] over `elitea-workspace-store` (ADR-0034 decision 2).
//!
//! Hydrate verifies the segment into a quarantine object database with the
//! crate (digest, size, pack checksum, refs, connectivity) and only then
//! writes the checkout's files. The turn's commit is written as loose
//! objects into the same quarantine, and the COMPACT segment is the crate's
//! `write_compact` over it.
//!
//! No git CLI runs and no repository config is read: the agent-written tree
//! is only ever read as data (`symlink_metadata`, never followed), and
//! `.git` anywhere in it is ignored.

use std::cmp::Ordering;
use std::collections::BTreeMap;
use std::fs::{self, File};
use std::io::Write as _;
use std::os::unix::fs::{PermissionsExt as _, symlink};
use std::path::{Component, Path};

use elitea_workspace_store::StoreError;
use elitea_workspace_store::bundle::{self, Expect, Limits};
use elitea_workspace_store::objects::{ObjectDb, oid};
use gix_object::Write as _;

use super::codec::{
    BuiltSegment, CodecError, HydratedTree, SegmentExpectation, TurnCommit, WorkspaceCodec,
};
use super::store::REF_CLOUD;

const MODE_FILE: u16 = 0o100_644;
const MODE_EXECUTABLE: u16 = 0o100_755;
const MODE_SYMLINK: u16 = 0o120_000;
const MODE_GITLINK: u16 = 0o160_000;
const MODE_TREE: u32 = 0o40_000;
/// Objects a segment may carry besides its files (trees and commits).
const OBJECTS_PER_FILE: u64 = 3;
const MAX_TREE_DEPTH: usize = 256;

/// The production codec.
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct StoreCodec;

fn verify_error(error: &StoreError) -> CodecError {
    match error {
        StoreError::UnsupportedObjectFormat(_) => CodecError::UnsupportedObjectFormat,
        StoreError::Io(_) => CodecError::Io,
        _ => CodecError::Integrity,
    }
}

/// A path the checkout may write: relative, normal components only, and no
/// `.git` component in any case (APFS and NTFS fold case).
fn safe_relative(path: &str) -> bool {
    let path = Path::new(path);
    !path.as_os_str().is_empty()
        && path.components().all(|component| match component {
            Component::Normal(name) => !name.eq_ignore_ascii_case(".git"),
            _ => false,
        })
}

impl WorkspaceCodec for StoreCodec {
    fn hydrate(
        &self,
        segment: &[u8],
        expect: &SegmentExpectation,
        checkout: &str,
        git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError> {
        bundle::init_quarantine(git_dir).map_err(|error| verify_error(&error))?;
        let limits = Limits {
            max_objects: usize::try_from(
                expect
                    .max_files
                    .saturating_mul(OBJECTS_PER_FILE)
                    .saturating_add(10_000),
            )
            .unwrap_or(usize::MAX),
            max_unpacked_bytes: expect.max_unpacked_bytes,
        };
        let verified = bundle::verify_into(
            segment,
            git_dir,
            &Expect {
                sha256: Some(&expect.sha256),
                bytes: Some(expect.bytes),
                refs: Some(&expect.refs),
            },
            limits,
        )
        .map_err(|error| verify_error(&error))?;
        let commit = verified
            .db
            .commit(&oid(checkout).map_err(|_| CodecError::Integrity)?)
            .map_err(|error| verify_error(&error))?;
        let files = verified
            .db
            .flatten(&commit.tree)
            .map_err(|error| verify_error(&error))?;
        if files.len() as u64 > expect.max_files {
            return Err(CodecError::TooLarge);
        }
        // Every check passed: only now is anything written to the tree.
        let mut unpacked: u64 = 0;
        let mut written: u64 = 0;
        for (path, entry) in &files {
            if !safe_relative(path) {
                return Err(CodecError::Integrity);
            }
            if entry.mode == MODE_GITLINK {
                // A submodule commit has no content in the segment.
                continue;
            }
            let content = verified
                .db
                .blob(&entry.oid)
                .map_err(|error| verify_error(&error))?;
            if content.len() as u64 > expect.max_file_bytes {
                return Err(CodecError::TooLarge);
            }
            unpacked += content.len() as u64;
            let target = worktree.join(path);
            if let Some(parent) = target.parent() {
                fs::create_dir_all(parent).map_err(|_| CodecError::Io)?;
            }
            match entry.mode {
                MODE_SYMLINK => {
                    let link = String::from_utf8(content).map_err(|_| CodecError::Integrity)?;
                    symlink(link, &target).map_err(|_| CodecError::Io)?;
                }
                MODE_FILE | MODE_EXECUTABLE => {
                    let mut file = File::create(&target).map_err(|_| CodecError::Io)?;
                    file.write_all(&content).map_err(|_| CodecError::Io)?;
                    let mode = if entry.mode == MODE_EXECUTABLE { 0o755 } else { 0o644 };
                    fs::set_permissions(&target, fs::Permissions::from_mode(mode))
                        .map_err(|_| CodecError::Io)?;
                }
                _ => return Err(CodecError::Integrity),
            }
            written += 1;
        }
        // Contract 6.4: the explicit includes, from the handoff snapshot's
        // `Elitea-Included: <path>` trailers (one per path).
        let mut included_excluded = Vec::new();
        for (name, sha) in &expect.refs {
            if name.starts_with(super::store::REF_HANDOFF_PREFIX) {
                let snapshot = verified
                    .db
                    .commit(&oid(sha).map_err(|_| CodecError::Integrity)?)
                    .map_err(|error| verify_error(&error))?;
                included_excluded.extend(included_trailers(&snapshot.message));
            }
        }
        Ok(HydratedTree {
            files: written,
            unpacked_bytes: unpacked,
            included_excluded,
        })
    }

    fn commit_and_bundle(
        &self,
        git_dir: &Path,
        worktree: &Path,
        commit: &TurnCommit,
        refs: &BTreeMap<String, String>,
        max_segment_bytes: u64,
    ) -> Result<BuiltSegment, CodecError> {
        let objects = git_dir.join("objects");
        let loose = gix_odb::loose::Store::at(&objects, gix_hash::Kind::Sha1);
        let tree = match write_tree(&loose, worktree, 0)? {
            Some(tree) => tree,
            // An emptied workspace is the empty tree.
            None => loose
                .write_buf(gix_object::Kind::Tree, &[])
                .map_err(|_| CodecError::Io)?,
        };
        let parent = oid(&commit.parent).map_err(|_| CodecError::Integrity)?;
        let signature = format!(
            "{} <{}> {} +0000",
            commit.author_name, commit.author_email, commit.unix_seconds
        );
        let raw = format!(
            "tree {tree}\nparent {parent}\nauthor {signature}\ncommitter {signature}\n\n{}",
            commit.message
        );
        let new_commit = loose
            .write_buf(gix_object::Kind::Commit, raw.as_bytes())
            .map_err(|_| CodecError::Io)?;
        let mut tips = Vec::with_capacity(refs.len() + 1);
        for (name, sha) in refs {
            if name != REF_CLOUD {
                tips.push((name.clone(), oid(sha).map_err(|_| CodecError::Integrity)?));
            }
        }
        tips.push((REF_CLOUD.to_owned(), new_commit));
        let db = ObjectDb::open_objects(&objects).map_err(|error| verify_error(&error))?;
        let written = bundle::write_compact(&db, &tips, Limits::default()).map_err(|error| {
            match error {
                StoreError::Invalid(_) => CodecError::TooLarge,
                other => verify_error(&other),
            }
        })?;
        if written.bytes.len() as u64 > max_segment_bytes {
            return Err(CodecError::TooLarge);
        }
        Ok(BuiltSegment {
            commit: new_commit.to_string(),
            sha256: written.sha256,
            bytes: written.bytes,
        })
    }
}

/// The `Elitea-Included` trailer values of a commit message, as safe
/// relative paths (`./` stripped); anything else is ignored.
fn included_trailers(message: &str) -> Vec<String> {
    message
        .lines()
        .filter_map(|line| line.strip_prefix("Elitea-Included:"))
        .map(|value| value.trim().trim_start_matches("./").to_owned())
        .filter(|path| safe_relative(path))
        .collect()
}

struct TreeEntry {
    name: Vec<u8>,
    mode: u32,
    id: gix_hash::ObjectId,
}

/// Git's tree order: names compared as bytes, a tree's name as if it ended
/// in `/`.
fn git_order(left: &TreeEntry, right: &TreeEntry) -> Ordering {
    let key = |entry: &TreeEntry| {
        let mut name = entry.name.clone();
        if entry.mode == MODE_TREE {
            name.push(b'/');
        }
        name
    };
    key(left).cmp(&key(right))
}

/// Write `directory` as a tree; `None` for a directory with no files (git
/// stores no empty directory).
fn write_tree(
    loose: &gix_odb::loose::Store,
    directory: &Path,
    depth: usize,
) -> Result<Option<gix_hash::ObjectId>, CodecError> {
    if depth > MAX_TREE_DEPTH {
        return Err(CodecError::InvalidTree);
    }
    let mut entries = Vec::new();
    for entry in fs::read_dir(directory).map_err(|_| CodecError::Io)? {
        let entry = entry.map_err(|_| CodecError::Io)?;
        let name = entry.file_name();
        if name.eq_ignore_ascii_case(".git") {
            continue;
        }
        let name_bytes = name
            .to_str()
            .ok_or(CodecError::InvalidTree)?
            .as_bytes()
            .to_vec();
        let path = entry.path();
        // Never followed: the type is the entry's own.
        let metadata = fs::symlink_metadata(&path).map_err(|_| CodecError::Io)?;
        let kind = metadata.file_type();
        if kind.is_dir() {
            if let Some(id) = write_tree(loose, &path, depth + 1)? {
                entries.push(TreeEntry {
                    name: name_bytes,
                    mode: MODE_TREE,
                    id,
                });
            }
        } else if kind.is_symlink() {
            let target = fs::read_link(&path).map_err(|_| CodecError::Io)?;
            let target = target.to_str().ok_or(CodecError::InvalidTree)?;
            let id = loose
                .write_buf(gix_object::Kind::Blob, target.as_bytes())
                .map_err(|_| CodecError::Io)?;
            entries.push(TreeEntry {
                name: name_bytes,
                mode: u32::from(MODE_SYMLINK),
                id,
            });
        } else if kind.is_file() {
            let mut file = File::open(&path).map_err(|_| CodecError::Io)?;
            let id = loose
                .write_stream(gix_object::Kind::Blob, metadata.len(), &mut file)
                .map_err(|_| CodecError::Io)?;
            let mode = if metadata.permissions().mode() & 0o111 != 0 {
                MODE_EXECUTABLE
            } else {
                MODE_FILE
            };
            entries.push(TreeEntry {
                name: name_bytes,
                mode: u32::from(mode),
                id,
            });
        } else {
            // A socket, a fifo or a device cannot be committed.
            return Err(CodecError::InvalidTree);
        }
    }
    if entries.is_empty() {
        return Ok(None);
    }
    entries.sort_by(git_order);
    let mut raw = Vec::new();
    for entry in &entries {
        raw.extend_from_slice(format!("{:o} ", entry.mode).as_bytes());
        raw.extend_from_slice(&entry.name);
        raw.push(0);
        raw.extend_from_slice(entry.id.as_bytes());
    }
    loose
        .write_buf(gix_object::Kind::Tree, &raw)
        .map(Some)
        .map_err(|_| CodecError::Io)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::workspace_run::store::sha256_hex;

    const BASE_REF: &str = "refs/elitea/base";
    const HANDOFF_REF: &str = "refs/elitea/handoff/1";

    fn expectation(segment: &[u8], refs: &BTreeMap<String, String>) -> SegmentExpectation {
        SegmentExpectation {
            sha256: sha256_hex(segment),
            bytes: segment.len() as u64,
            refs: refs.clone(),
            max_unpacked_bytes: 1 << 30,
            max_files: 1_000,
            max_file_bytes: 1 << 20,
        }
    }

    fn turn_commit(parent: &str, seconds: i64) -> TurnCommit {
        TurnCommit {
            parent: parent.to_owned(),
            message: "Elitea cloud turn\n\nElitea-Execution-Id: e\nElitea-Epoch: 2\n".to_owned(),
            author_name: "Elitea cloud executor".to_owned(),
            author_email: "cloud-executor@elitea.invalid".to_owned(),
            unix_seconds: seconds,
        }
    }

    /// Build a first segment the way a desktop would: an orphan base and a
    /// snapshot commit, bundled with the crate, then round-trip a cloud turn.
    #[test]
    fn a_hydrated_tree_commits_and_bundles_back_into_a_verifiable_segment() {
        let scratch = tempfile::tempdir().expect("scratch");
        // The "desktop": a work tree committed twice into its own objects.
        let desktop = scratch.path().join("desktop.git");
        bundle::init_quarantine(&desktop).expect("init");
        let source = scratch.path().join("src-tree");
        fs::create_dir_all(source.join("src")).expect("mkdir");
        fs::write(source.join("README.md"), "hello\n").expect("w");
        fs::write(source.join("src/main.rs"), "fn main() {}\n").expect("w");
        fs::write(source.join("run.sh"), "#!/bin/sh\necho hi\n").expect("w");
        fs::set_permissions(source.join("run.sh"), fs::Permissions::from_mode(0o755)).expect("p");
        symlink("README.md", source.join("readme-link")).expect("link");
        let loose = gix_odb::loose::Store::at(desktop.join("objects"), gix_hash::Kind::Sha1);
        let tree = write_tree(&loose, &source, 0).expect("tree").expect("files");
        let base_raw = format!(
            "tree {tree}\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nElitea base\n"
        );
        let base = loose
            .write_buf(gix_object::Kind::Commit, base_raw.as_bytes())
            .expect("base");
        let handoff_raw = format!(
            "tree {tree}\nparent {base}\nauthor a <a@x> 2 +0000\ncommitter a <a@x> 2 +0000\n\nsnapshot\n"
        );
        let handoff = loose
            .write_buf(gix_object::Kind::Commit, handoff_raw.as_bytes())
            .expect("handoff");
        let db = ObjectDb::open_objects(&desktop.join("objects")).expect("db");
        let first = bundle::write_compact(
            &db,
            &[
                (BASE_REF.to_owned(), base),
                (HANDOFF_REF.to_owned(), handoff),
            ],
            Limits::default(),
        )
        .expect("segment");
        let refs: BTreeMap<String, String> = [
            (BASE_REF.to_owned(), base.to_string()),
            (HANDOFF_REF.to_owned(), handoff.to_string()),
        ]
        .into();

        // The executor hydrates it.
        let git_dir = scratch.path().join("q.git");
        let worktree = scratch.path().join("in");
        fs::create_dir_all(&worktree).expect("in");
        let hydrated = StoreCodec
            .hydrate(
                &first.bytes,
                &expectation(&first.bytes, &refs),
                &handoff.to_string(),
                &git_dir,
                &worktree,
            )
            .expect("hydrated");
        assert_eq!(hydrated.files, 4);
        assert_eq!(fs::read_to_string(worktree.join("src/main.rs")).expect("r"), "fn main() {}\n");
        assert_eq!(
            fs::metadata(worktree.join("run.sh")).expect("m").permissions().mode() & 0o111,
            0o111
        );
        assert_eq!(
            fs::read_link(worktree.join("readme-link")).expect("l"),
            Path::new("README.md")
        );

        // The turn edits a file, adds one and writes a `.git` that must be
        // ignored; the codec commits and bundles.
        fs::write(worktree.join("README.md"), "hello cloud\n").expect("w");
        fs::write(worktree.join("NEW.txt"), "new\n").expect("w");
        fs::create_dir_all(worktree.join(".git")).expect("git");
        fs::write(worktree.join(".git/config"), "[core]\n").expect("w");
        let built = StoreCodec
            .commit_and_bundle(&git_dir, &worktree, &turn_commit(&handoff.to_string(), 3), &refs, 1 << 30)
            .expect("built");
        assert_eq!(built.sha256, sha256_hex(&built.bytes));

        // The published segment verifies and carries the new cloud ref.
        let mut cloud_refs = refs.clone();
        cloud_refs.insert(REF_CLOUD.to_owned(), built.commit.clone());
        let check = scratch.path().join("check.git");
        let out = scratch.path().join("out");
        fs::create_dir_all(&out).expect("out");
        let mut expect = expectation(&built.bytes, &cloud_refs);
        expect.refs = cloud_refs;
        StoreCodec
            .hydrate(&built.bytes, &expect, &built.commit, &check, &out)
            .expect("the published segment verifies");
        assert_eq!(fs::read_to_string(out.join("README.md")).expect("r"), "hello cloud\n");
        assert_eq!(fs::read_to_string(out.join("NEW.txt")).expect("r"), "new\n");
        assert!(!out.join(".git").exists());
        let verified_db = ObjectDb::open_objects(&check.join("objects")).expect("db");
        let info = verified_db.commit(&oid(&built.commit).expect("oid")).expect("commit");
        assert_eq!(info.parents, vec![handoff]);
        assert!(info.message.contains("Elitea-Epoch: 2"));
        assert_real_git_accepts(scratch.path(), &built.bytes, &built.commit);
    }

    /// The segment is a real git bundle: git clones it and `fsck --strict`
    /// accepts every object the codec wrote (when git is installed).
    fn assert_real_git_accepts(scratch: &Path, segment: &[u8], cloud: &str) {
        let Ok(git) = which_git() else {
            eprintln!("git is not installed; the real-git check is skipped");
            return;
        };
        let file = scratch.join("turn.bundle");
        fs::write(&file, segment).expect("bundle");
        let mirror = scratch.join("mirror.git");
        let run = |args: &[&str]| {
            let output = std::process::Command::new(&git)
                .args(args)
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .env("GIT_CONFIG_GLOBAL", "/dev/null")
                .env("HOME", scratch)
                .output()
                .expect("git runs");
            assert!(
                output.status.success(),
                "git {args:?}: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            String::from_utf8_lossy(&output.stdout).into_owned()
        };
        run(&["clone", "--mirror", "-q", file.to_str().expect("utf8"), mirror.to_str().expect("utf8")]);
        run(&["-C", mirror.to_str().expect("utf8"), "fsck", "--strict", "--no-dangling"]);
        let heads = run(&["-C", mirror.to_str().expect("utf8"), "rev-parse", "refs/elitea/cloud"]);
        assert_eq!(heads.trim(), cloud);
    }

    fn which_git() -> Result<std::path::PathBuf, ()> {
        std::env::var_os("PATH")
            .and_then(|paths| {
                std::env::split_paths(&paths)
                    .map(|dir| dir.join("git"))
                    .find(|candidate| candidate.is_file())
            })
            .ok_or(())
    }

    #[test]
    fn a_tampered_segment_writes_nothing() {
        let scratch = tempfile::tempdir().expect("scratch");
        let worktree = scratch.path().join("in");
        fs::create_dir_all(&worktree).expect("in");
        let segment = b"# v2 git bundle\nnot a bundle".to_vec();
        let refs = BTreeMap::new();
        let mut expect = expectation(&segment, &refs);
        expect.sha256 = "0".repeat(64);
        assert_eq!(
            StoreCodec.hydrate(&segment, &expect, &"1".repeat(40), &scratch.path().join("q"), &worktree),
            Err(CodecError::Integrity)
        );
        assert_eq!(fs::read_dir(&worktree).expect("dir").count(), 0);
    }

    #[test]
    fn included_trailers_are_read_as_safe_paths() {
        let message = "snapshot\n\nElitea-Base: x\nElitea-Included: ./.env\nElitea-Included: config/dev.key\nElitea-Included: ../escape\n";
        assert_eq!(included_trailers(message), vec![".env", "config/dev.key"]);
    }

    #[test]
    fn checkout_paths_must_be_plain_and_never_git() {
        assert!(safe_relative("src/main.rs"));
        for bad in ["/etc/passwd", "../x", "a/../b", ".git/config", "a/.GIT/x", ""] {
            assert!(!safe_relative(bad), "{bad}");
        }
    }
}
