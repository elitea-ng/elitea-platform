//! adapted from floe (MIT): `floe-git`'s `check_connectivity` (tips peeled
//! through tags, a commit walk, a breadth-first tree walk with a seen-set
//! so a shared subtree is verified once, the first missing object named).
//! Rewritten over [`ObjectDb`] without gix's rev-walk or traversal crates.
//!
//! The consumer of a bundle runs this on its quarantine before any ref
//! moves or any file is written (ADR-0034 decision 2): every object
//! reachable from the bundle's refs must be present. Submodule commits
//! (gitlinks) are not followed, as git does not follow them.

use std::collections::{HashSet, VecDeque};

use gix_hash::ObjectId;
use gix_object::Kind;

use crate::error::StoreError;
use crate::objects::ObjectDb;

/// The deepest tag chain followed.
const MAX_TAG_DEPTH: usize = 64;

/// What a walk found: every object reachable from the tips, in a stable
/// order (commits and tags first as met, then trees and blobs as met), and
/// the bytes of the blobs among them.
#[derive(Debug, Default)]
pub struct Reachable {
    pub objects: Vec<ObjectId>,
    pub commits: usize,
    pub blob_bytes: u64,
    pub blobs: usize,
}

/// Every object reachable from `tips`, refusing at the first missing one.
/// `max_objects` bounds the walk.
///
/// # Errors
///
/// [`StoreError::Integrity`] naming a missing or mistyped object, or a walk
/// over `max_objects`.
pub fn reachable(
    db: &ObjectDb,
    tips: &[ObjectId],
    max_objects: usize,
) -> Result<Reachable, StoreError> {
    let mut walk = Walk {
        db,
        max_objects,
        seen: HashSet::new(),
        out: Reachable::default(),
        commits: VecDeque::new(),
        trees: VecDeque::new(),
    };
    for tip in tips {
        walk.seed(*tip)?;
    }
    while let Some(id) = walk.commits.pop_front() {
        if !walk.push(id)? {
            continue;
        }
        walk.out.commits += 1;
        let commit = db.commit(&id)?;
        if walk.push(commit.tree)? {
            walk.trees.push_back(commit.tree);
        }
        for parent in commit.parents {
            if !walk.seen.contains(&parent) {
                walk.commits.push_back(parent);
            }
        }
    }
    while let Some(id) = walk.trees.pop_front() {
        for entry in db.tree(&id)? {
            if entry.mode & 0o170_000 == 0o160_000 {
                continue;
            }
            if entry.is_tree() {
                if walk.push(entry.oid)? {
                    walk.trees.push_back(entry.oid);
                }
                continue;
            }
            if walk.push(entry.oid)? {
                walk.blob(&entry.oid)?;
            }
        }
    }
    Ok(walk.out)
}

struct Walk<'a> {
    db: &'a ObjectDb,
    max_objects: usize,
    seen: HashSet<ObjectId>,
    out: Reachable,
    commits: VecDeque<ObjectId>,
    trees: VecDeque<ObjectId>,
}

impl Walk<'_> {
    /// Record `id`; `false` when it was seen already.
    fn push(&mut self, id: ObjectId) -> Result<bool, StoreError> {
        if !self.seen.insert(id) {
            return Ok(false);
        }
        if self.seen.len() > self.max_objects {
            return Err(StoreError::Integrity(format!(
                "more than {} objects are reachable",
                self.max_objects
            )));
        }
        self.out.objects.push(id);
        Ok(true)
    }

    /// A blob met in a tree: present, and a blob.
    fn blob(&mut self, id: &ObjectId) -> Result<(), StoreError> {
        let Some((kind, data)) = self.db.find(id)? else {
            return Err(StoreError::Integrity(format!("object {id} is missing")));
        };
        if kind != Kind::Blob {
            return Err(StoreError::Integrity(format!(
                "object {id} is a {kind}, not a blob"
            )));
        }
        self.out.blobs += 1;
        self.out.blob_bytes += data.len() as u64;
        Ok(())
    }

    /// A tip, peeled through tags.
    fn seed(&mut self, tip: ObjectId) -> Result<(), StoreError> {
        let mut id = tip;
        for _ in 0..=MAX_TAG_DEPTH {
            let Some((kind, data)) = self.db.find(&id)? else {
                return Err(StoreError::Integrity(format!("object {id} is missing")));
            };
            match kind {
                Kind::Tag => {
                    self.push(id)?;
                    let tag = gix_object::TagRef::from_bytes(&data, gix_hash::Kind::Sha1)
                        .map_err(|_| StoreError::Integrity(format!("tag {id} cannot be parsed")))?;
                    id = tag.target();
                }
                Kind::Commit => {
                    if !self.seen.contains(&id) {
                        self.commits.push_back(id);
                    }
                    return Ok(());
                }
                Kind::Tree => {
                    if self.push(id)? {
                        self.trees.push_back(id);
                    }
                    return Ok(());
                }
                Kind::Blob => {
                    if self.push(id)? {
                        self.out.blobs += 1;
                        self.out.blob_bytes += data.len() as u64;
                    }
                    return Ok(());
                }
            }
        }
        Err(StoreError::Integrity("a tag chain is too deep".into()))
    }
}
