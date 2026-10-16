//! A read-only git object database, opened at an `objects/` directory
//! only: no repository config, hooks, attributes, filters or helpers are
//! read or run, so opening a folder someone else controls runs none of its
//! code. Alternates are refused (they would read objects from elsewhere).
//!
//! Plus the two walks take-back needs: a tree flattened to its files, and
//! the difference of two flattened trees.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub use gix_hash::ObjectId;
use gix_object::{Exists as _, Find as _, Kind};

use crate::error::StoreError;

/// The empty tree's id (sha1).
pub const EMPTY_TREE: &str = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";

/// The deepest tree a walk follows.
const MAX_TREE_DEPTH: usize = 256;

/// Parse a full sha1 object id.
///
/// # Errors
///
/// [`StoreError::Invalid`] when `hex` is not 40 hex digits.
pub fn oid(hex: &str) -> Result<ObjectId, StoreError> {
    if !crate::digest::is_sha1_hex(hex) {
        return Err(StoreError::Invalid(format!(
            "`{hex}` is not a sha1 object id"
        )));
    }
    ObjectId::from_hex(hex.as_bytes())
        .map_err(|_| StoreError::Invalid(format!("`{hex}` is not an object id")))
}

/// The object format a git directory declares (`extensions.objectFormat`
/// in its `config`), lowercased; `sha1` when it declares none.
///
/// # Errors
///
/// The config exists but cannot be read.
pub fn object_format(git_dir: &Path) -> Result<String, StoreError> {
    let text = match std::fs::read_to_string(git_dir.join("config")) {
        Ok(text) => text,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok("sha1".into()),
        Err(error) => return Err(StoreError::io("cannot read the repository config", &error)),
    };
    let mut in_extensions = false;
    for line in text.lines() {
        let line = line.trim();
        if let Some(header) = line.strip_prefix('[') {
            in_extensions = header
                .trim_end_matches(']')
                .trim()
                .eq_ignore_ascii_case("extensions");
            continue;
        }
        if !in_extensions {
            continue;
        }
        if let Some((key, value)) = line.split_once('=')
            && key.trim().eq_ignore_ascii_case("objectformat")
        {
            let value = value.split(['#', ';']).next().unwrap_or_default();
            return Ok(value.trim().trim_matches('"').to_ascii_lowercase());
        }
    }
    Ok("sha1".into())
}

/// One entry of a tree.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TreeEntry {
    pub name: Vec<u8>,
    /// The git mode (`0o100644`, `0o100755`, `0o120000`, `0o40000`,
    /// `0o160000`).
    pub mode: u16,
    pub oid: ObjectId,
}

impl TreeEntry {
    #[must_use]
    pub fn is_tree(&self) -> bool {
        self.mode & 0o170_000 == 0o040_000
    }
}

/// A file of a flattened tree.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct FileEntry {
    pub oid: ObjectId,
    pub mode: u16,
}

impl FileEntry {
    #[must_use]
    pub fn is_link(&self) -> bool {
        self.mode & 0o170_000 == 0o120_000
    }

    #[must_use]
    pub fn is_gitlink(&self) -> bool {
        self.mode & 0o170_000 == 0o160_000
    }

    #[must_use]
    pub fn is_executable(&self) -> bool {
        self.mode == 0o100_755
    }
}

/// A commit as far as the store reads one.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommitInfo {
    pub tree: ObjectId,
    pub parents: Vec<ObjectId>,
    pub message: String,
}

/// A read-only object database.
pub struct ObjectDb {
    handle: gix_odb::HandleArc,
    dir: PathBuf,
}

impl std::fmt::Debug for ObjectDb {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ObjectDb")
            .field("dir", &self.dir)
            .finish_non_exhaustive()
    }
}

impl ObjectDb {
    /// The objects of the git directory `git_dir`.
    ///
    /// # Errors
    ///
    /// [`StoreError::UnsupportedObjectFormat`] for a sha256 (or any non
    /// sha1) repository, or as [`Self::open_objects`].
    pub fn open_git_dir(git_dir: &Path) -> Result<Self, StoreError> {
        let format = object_format(git_dir)?;
        if format != crate::OBJECT_FORMAT_SHA1 {
            return Err(StoreError::UnsupportedObjectFormat(format!(
                "the repository uses {format} object ids; only sha1 repositories can be handed off"
            )));
        }
        Self::open_objects(&git_dir.join("objects"))
    }

    /// The objects under `objects_dir`.
    ///
    /// # Errors
    ///
    /// The directory cannot be opened, or it borrows objects from elsewhere
    /// (`info/alternates`).
    pub fn open_objects(objects_dir: &Path) -> Result<Self, StoreError> {
        if std::fs::symlink_metadata(objects_dir.join("info/alternates")).is_ok() {
            return Err(StoreError::Invalid(
                "the object database borrows objects from elsewhere (alternates)".into(),
            ));
        }
        if !objects_dir.is_dir() {
            return Err(StoreError::Invalid("the object database is missing".into()));
        }
        let store = gix_odb::Store::at_opts(
            objects_dir.to_path_buf(),
            gix_hash::Kind::Sha1,
            &mut std::iter::empty(),
            gix_odb::store::init::Options::default(),
        )
        .map_err(|error| StoreError::io("cannot open the object database", &error))?;
        Ok(Self {
            handle: Arc::new(store).to_cache_arc(),
            dir: objects_dir.to_path_buf(),
        })
    }

    /// The `objects/` directory.
    #[must_use]
    pub fn dir(&self) -> &Path {
        &self.dir
    }

    #[must_use]
    pub fn exists(&self, id: &ObjectId) -> bool {
        self.handle.exists(id)
    }

    /// The object's kind and bytes; `None` when it is absent.
    ///
    /// # Errors
    ///
    /// The object database cannot be read (a broken pack or loose file).
    pub fn find(&self, id: &ObjectId) -> Result<Option<(Kind, Vec<u8>)>, StoreError> {
        let mut buffer = Vec::new();
        match self.handle.try_find(id, &mut buffer) {
            Ok(Some(data)) => Ok(Some((data.kind, data.data.to_vec()))),
            Ok(None) => Ok(None),
            Err(error) => Err(StoreError::Integrity(format!(
                "cannot read object {id}: {error}"
            ))),
        }
    }

    fn require(&self, id: &ObjectId, kind: Kind) -> Result<Vec<u8>, StoreError> {
        match self.find(id)? {
            Some((found, data)) if found == kind => Ok(data),
            Some((found, _)) => Err(StoreError::Integrity(format!(
                "object {id} is a {found}, not a {kind}"
            ))),
            None => Err(StoreError::Integrity(format!("object {id} is missing"))),
        }
    }

    /// # Errors
    ///
    /// The object is missing, not a commit, or cannot be parsed.
    pub fn commit(&self, id: &ObjectId) -> Result<CommitInfo, StoreError> {
        let data = self.require(id, Kind::Commit)?;
        let commit = gix_object::CommitRef::from_bytes(&data, gix_hash::Kind::Sha1)
            .map_err(|_| StoreError::Integrity(format!("commit {id} cannot be parsed")))?;
        Ok(CommitInfo {
            tree: commit.tree(),
            parents: commit.parents().collect(),
            message: String::from_utf8_lossy(commit.message).into_owned(),
        })
    }

    /// # Errors
    ///
    /// The object is missing, not a tree, or cannot be parsed.
    pub fn tree(&self, id: &ObjectId) -> Result<Vec<TreeEntry>, StoreError> {
        let data = self.require(id, Kind::Tree)?;
        let tree = gix_object::TreeRef::from_bytes(&data, gix_hash::Kind::Sha1)
            .map_err(|_| StoreError::Integrity(format!("tree {id} cannot be parsed")))?;
        Ok(tree
            .entries
            .iter()
            .map(|entry| TreeEntry {
                name: entry.filename.to_vec(),
                mode: entry.mode.value(),
                oid: entry.oid.to_owned(),
            })
            .collect())
    }

    /// # Errors
    ///
    /// The object is missing or not a blob.
    pub fn blob(&self, id: &ObjectId) -> Result<Vec<u8>, StoreError> {
        self.require(id, Kind::Blob)
    }

    /// Every non-tree entry under tree `id`, by `/`-joined path. Names that
    /// are not UTF-8, empty, `.`, `..` or hold a `/` are refused: such a
    /// tree is not something this client writes to a folder.
    ///
    /// # Errors
    ///
    /// A missing or broken object, or a refused name.
    pub fn flatten(&self, id: &ObjectId) -> Result<BTreeMap<String, FileEntry>, StoreError> {
        let mut out = BTreeMap::new();
        let mut stack = vec![(String::new(), *id, 0usize)];
        while let Some((prefix, tree, depth)) = stack.pop() {
            if depth > MAX_TREE_DEPTH {
                return Err(StoreError::Integrity("a tree is nested too deeply".into()));
            }
            for entry in self.tree(&tree)? {
                let name = std::str::from_utf8(&entry.name)
                    .ok()
                    .filter(|name| {
                        !name.is_empty()
                            && *name != "."
                            && *name != ".."
                            && !name.contains(['/', '\0'])
                    })
                    .ok_or_else(|| {
                        StoreError::Integrity(
                            "a tree holds a path that is not a usable file name".into(),
                        )
                    })?;
                let path = if prefix.is_empty() {
                    name.to_owned()
                } else {
                    format!("{prefix}/{name}")
                };
                if entry.is_tree() {
                    stack.push((path, entry.oid, depth + 1));
                } else {
                    out.insert(
                        path,
                        FileEntry {
                            oid: entry.oid,
                            mode: entry.mode,
                        },
                    );
                }
            }
        }
        Ok(out)
    }

    /// Commits from `tip` back to (not including) `stop`, newest first,
    /// following first parents; `None` when `stop` is not reached.
    ///
    /// # Errors
    ///
    /// A missing or broken commit.
    pub fn first_parent_chain(
        &self,
        tip: &ObjectId,
        stop: &ObjectId,
        max: usize,
    ) -> Result<Option<Vec<ObjectId>>, StoreError> {
        let mut out = Vec::new();
        let mut current = *tip;
        while current != *stop {
            if out.len() >= max {
                return Err(StoreError::Integrity(format!(
                    "more than {max} commits after the handoff"
                )));
            }
            out.push(current);
            match self.commit(&current)?.parents.first() {
                Some(parent) => current = *parent,
                None => return Ok(None),
            }
        }
        Ok(Some(out))
    }
}

/// How a path changed between two trees.
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ChangeKind {
    Added,
    Modified,
    Deleted,
}

/// One changed path.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Change {
    pub path: String,
    pub kind: ChangeKind,
    pub old: Option<FileEntry>,
    pub new: Option<FileEntry>,
}

/// What changed from `old` to `new` (content or mode), by path. A rename
/// is a deletion and an addition.
#[must_use]
pub fn diff(old: &BTreeMap<String, FileEntry>, new: &BTreeMap<String, FileEntry>) -> Vec<Change> {
    let mut out = Vec::new();
    for (path, before) in old {
        match new.get(path) {
            None => out.push(Change {
                path: path.clone(),
                kind: ChangeKind::Deleted,
                old: Some(*before),
                new: None,
            }),
            Some(after) if after != before => out.push(Change {
                path: path.clone(),
                kind: ChangeKind::Modified,
                old: Some(*before),
                new: Some(*after),
            }),
            Some(_) => {}
        }
    }
    for (path, after) in new {
        if !old.contains_key(path) {
            out.push(Change {
                path: path.clone(),
                kind: ChangeKind::Added,
                old: None,
                new: Some(*after),
            });
        }
    }
    out.sort_by(|a, b| a.path.cmp(&b.path));
    out
}
