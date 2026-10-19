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
use std::io::{Read, Write};
use std::os::unix::fs::{PermissionsExt as _, symlink};
use std::path::{Component, Path};

use elitea_workspace_store::StoreError;
use elitea_workspace_store::bundle::{self, Limits};
use elitea_workspace_store::objects::{ObjectDb, oid};
use gix_object::Write as _;
use sha2::Digest as _;

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
        segment: &Path,
        expect: &SegmentExpectation,
        checkout: &str,
        git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError> {
        let verified = verify_segment_file(segment, git_dir, expect, plain_reader)?;
        let (written, unpacked) = checkout_into(&verified.db, checkout, expect, worktree)?;
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
        out: &Path,
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
        let (sha256, bytes) =
            write_compact_file(&db, &tips, Limits::default(), max_segment_bytes, out)?;
        Ok(BuiltSegment {
            commit: new_commit.to_string(),
            path: out.to_path_buf(),
            sha256,
            bytes,
        })
    }

    fn parent_files(
        &self,
        git_dir: &Path,
        parent: &str,
    ) -> Result<Box<dyn super::file_redact::ParentTree + Send>, CodecError> {
        Self::quarantine_parent(git_dir, parent)
    }

    fn rewind(
        &self,
        git_dir: &Path,
        expect: &SegmentExpectation,
        cloud: &str,
        worktree: &Path,
        max_segment_bytes: u64,
        out: &Path,
    ) -> Result<(String, BuiltSegment), CodecError> {
        let objects = git_dir.join("objects");
        let db = ObjectDb::open_objects(&objects).map_err(|error| verify_error(&error))?;
        let info = db
            .commit(&oid(cloud).map_err(|_| CodecError::Integrity)?)
            .map_err(|error| verify_error(&error))?;
        let parent = *info.parents.first().ok_or(CodecError::Integrity)?;
        let parent_hex = parent.to_string();
        checkout_into(&db, &parent_hex, expect, worktree)?;
        let mut tips = Vec::with_capacity(expect.refs.len());
        for (name, sha) in &expect.refs {
            let id = if name == REF_CLOUD {
                parent
            } else {
                oid(sha).map_err(|_| CodecError::Integrity)?
            };
            tips.push((name.clone(), id));
        }
        let (sha256, bytes) =
            write_compact_file(&db, &tips, Limits::default(), max_segment_bytes, out)?;
        Ok((
            parent_hex.clone(),
            BuiltSegment {
                commit: parent_hex,
                path: out.to_path_buf(),
                sha256,
                bytes,
            },
        ))
    }
}

impl StoreCodec {
    fn quarantine_parent(
        git_dir: &Path,
        parent: &str,
    ) -> Result<Box<dyn super::file_redact::ParentTree + Send>, CodecError> {
        super::file_redact::QuarantineParent::open(git_dir, parent)
            .map(|parent| Box::new(parent) as Box<dyn super::file_redact::ParentTree + Send>)
            .map_err(|_| CodecError::Integrity)
    }
}

/// Write commit `checkout`'s tree from `db` into `worktree` (an empty
/// directory), within `expect`'s file caps. Returns (files, bytes).
fn checkout_into(
    db: &ObjectDb,
    checkout: &str,
    expect: &SegmentExpectation,
    worktree: &Path,
) -> Result<(u64, u64), CodecError> {
    let commit = db
        .commit(&oid(checkout).map_err(|_| CodecError::Integrity)?)
        .map_err(|error| verify_error(&error))?;
    let files = db
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
        let content = db.blob(&entry.oid).map_err(|error| verify_error(&error))?;
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
                let mode = if entry.mode == MODE_EXECUTABLE {
                    0o755
                } else {
                    0o644
                };
                fs::set_permissions(&target, fs::Permissions::from_mode(mode))
                    .map_err(|_| CodecError::Io)?;
            }
            _ => return Err(CodecError::Integrity),
        }
        written += 1;
    }
    Ok((written, unpacked))
}

/// Reads of a segment file go through this; tests wrap it to prove no read
/// asks for the whole file.
pub(crate) type SegmentReader = fn(File) -> Box<dyn Read + Send>;

pub(crate) fn plain_reader(file: File) -> Box<dyn Read + Send> {
    Box::new(file)
}

/// The largest bundle header read (the store crate's bound).
const MAX_HEADER_BYTES: u64 = 1024 * 1024;
/// Every read of a segment file is at most this.
pub(crate) const READ_CHUNK_BYTES: usize = 64 * 1024;

/// A verified quarantine.
pub(crate) struct VerifiedSegment {
    pub(crate) db: ObjectDb,
}

/// [`bundle::verify_into`] over a FILE: the same checks (size, content
/// address, header, refs, pack checksum and objects, connectivity, the
/// unpacked cap), with every read bounded to [`READ_CHUNK_BYTES`]; the pack
/// is indexed from a buffered stream, so the segment is never held whole.
pub(crate) fn verify_segment_file(
    segment: &Path,
    git_dir: &Path,
    expect: &SegmentExpectation,
    reader: SegmentReader,
) -> Result<VerifiedSegment, CodecError> {
    bundle::init_quarantine(git_dir).map_err(|error| verify_error(&error))?;
    let open = || File::open(segment).map(reader).map_err(|_| CodecError::Io);
    let length = fs::metadata(segment).map_err(|_| CodecError::Io)?.len();
    if length != expect.bytes {
        return Err(CodecError::Integrity);
    }
    // The content address, streamed.
    let mut hasher = sha2::Sha256::new();
    let mut input = open()?;
    let mut buffer = vec![0_u8; READ_CHUNK_BYTES];
    loop {
        let count = input.read(&mut buffer).map_err(|_| CodecError::Io)?;
        if count == 0 {
            break;
        }
        sha2::Digest::update(&mut hasher, &buffer[..count]);
    }
    if hex(&sha2::Digest::finalize(hasher)) != expect.sha256 {
        return Err(CodecError::Integrity);
    }
    // The header: at most 1 MiB plus the pack signature.
    let mut head = Vec::new();
    let mut input = open()?.take(MAX_HEADER_BYTES + 4);
    loop {
        let count = input.read(&mut buffer).map_err(|_| CodecError::Io)?;
        if count == 0 {
            break;
        }
        head.extend_from_slice(&buffer[..count]);
    }
    let header = bundle::parse_header(&head).map_err(|error| verify_error(&error))?;
    drop(head);
    if !header.prerequisites.is_empty() || header.refs.is_empty() || header.ref_map() != expect.refs
    {
        return Err(CodecError::Integrity);
    }
    // The pack, streamed into the quarantine's pack directory.
    let mut input = open()?;
    let offset = header.pack_offset as u64;
    std::io::copy(&mut (&mut input).take(offset), &mut std::io::sink())
        .map_err(|_| CodecError::Io)?;
    let mut pack = std::io::BufReader::with_capacity(READ_CHUNK_BYTES, input.take(length - offset));
    let interrupt = std::sync::atomic::AtomicBool::new(false);
    let outcome = gix_pack::Bundle::write_to_directory(
        &mut pack,
        Some(&git_dir.join("objects/pack")),
        &mut gix_features::progress::Discard,
        &interrupt,
        None::<gix_object::find::Never>,
        gix_hash::Kind::Sha1,
        gix_pack::bundle::write::Options {
            thread_limit: Some(1),
            iteration_mode: gix_pack::data::input::Mode::Verify,
            alloc_limit_bytes: Some(512 * 1024 * 1024),
            ..Default::default()
        },
    )
    .map_err(|_| CodecError::Integrity)?;
    if let Some(keep) = outcome.keep_path {
        let _ = fs::remove_file(keep);
    }
    let db =
        ObjectDb::open_objects(&git_dir.join("objects")).map_err(|error| verify_error(&error))?;
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
    let tips: Vec<gix_hash::ObjectId> = header.refs.iter().map(|(_, id)| *id).collect();
    let walk = elitea_workspace_store::connectivity::reachable(&db, &tips, limits.max_objects)
        .map_err(|error| verify_error(&error))?;
    if walk.blob_bytes > limits.max_unpacked_bytes {
        return Err(CodecError::TooLarge);
    }
    Ok(VerifiedSegment { db })
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

/// Counts and hashes what passes through, and refuses past `max`.
struct CountingWriter<W> {
    inner: W,
    sha256: sha2::Sha256,
    written: u64,
    max: u64,
    over: bool,
}

impl<W: Write> Write for CountingWriter<W> {
    fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
        if self.written + data.len() as u64 > self.max {
            self.over = true;
            return Err(std::io::Error::other("the segment is over its cap"));
        }
        let count = self.inner.write(data)?;
        sha2::Digest::update(&mut self.sha256, &data[..count]);
        self.written += count as u64;
        Ok(count)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

/// Feeds the pack's SHA-1 trailer.
struct PackWriter<'a, W> {
    inner: &'a mut W,
    sha1: gix_hash::Hasher,
}

impl<W: Write> Write for PackWriter<'_, W> {
    fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
        let count = self.inner.write(data)?;
        self.sha1.update(&data[..count]);
        Ok(count)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

/// The pack entry header: type and size, little-endian 7-bit groups.
fn entry_header(kind: gix_object::Kind, size: usize) -> Vec<u8> {
    let number: u8 = match kind {
        gix_object::Kind::Commit => 1,
        gix_object::Kind::Tree => 2,
        gix_object::Kind::Blob => 3,
        gix_object::Kind::Tag => 4,
    };
    let mut out = Vec::with_capacity(10);
    let mut size = size as u64;
    let mut byte = (number << 4) | u8::try_from(size & 0x0f).unwrap_or(0);
    size >>= 4;
    while size > 0 {
        out.push(byte | 0x80);
        byte = u8::try_from(size & 0x7f).unwrap_or(0);
        size >>= 7;
    }
    out.push(byte);
    out
}

/// [`bundle::write_compact`] into a FILE: the v2 header, then a pack of
/// whole objects written one object at a time (each object is bounded by
/// the file cap); the bytes and their SHA-256 are counted as they are
/// written, and a segment over `max_segment_bytes` stops the write.
fn write_compact_file(
    db: &ObjectDb,
    refs: &[(String, gix_hash::ObjectId)],
    limits: Limits,
    max_segment_bytes: u64,
    out: &Path,
) -> Result<(String, u64), CodecError> {
    let walk = elitea_workspace_store::connectivity::reachable(
        db,
        &refs.iter().map(|(_, id)| *id).collect::<Vec<_>>(),
        limits.max_objects,
    )
    .map_err(|error| verify_error(&error))?;
    if walk.blob_bytes > limits.max_unpacked_bytes {
        return Err(CodecError::TooLarge);
    }
    let file = File::create(out).map_err(|_| CodecError::Io)?;
    let mut writer = CountingWriter {
        inner: std::io::BufWriter::with_capacity(READ_CHUNK_BYTES, file),
        sha256: sha2::Sha256::new(),
        written: 0,
        max: max_segment_bytes,
        over: false,
    };
    let fail = |writer: &CountingWriter<_>| {
        if writer.over {
            CodecError::TooLarge
        } else {
            CodecError::Io
        }
    };
    let mut header = b"# v2 git bundle\n".to_vec();
    for (name, id) in refs {
        header.extend_from_slice(format!("{id} {name}\n").as_bytes());
    }
    header.push(b'\n');
    writer.write_all(&header).map_err(|_| fail(&writer))?;
    let count = u32::try_from(walk.objects.len()).map_err(|_| CodecError::TooLarge)?;
    let mut pack = PackWriter {
        inner: &mut writer,
        sha1: gix_hash::hasher(gix_hash::Kind::Sha1),
    };
    let mut start = b"PACK".to_vec();
    start.extend_from_slice(&2u32.to_be_bytes());
    start.extend_from_slice(&count.to_be_bytes());
    let mut result = pack.write_all(&start);
    for id in &walk.objects {
        if result.is_err() {
            break;
        }
        let Some((kind, data)) = db.find(id).map_err(|error| verify_error(&error))? else {
            return Err(CodecError::Integrity);
        };
        result = pack
            .write_all(&entry_header(kind, data.len()))
            .and_then(|()| {
                let mut encoder =
                    flate2::write::ZlibEncoder::new(&mut pack, flate2::Compression::default());
                encoder.write_all(&data)?;
                encoder.finish().map(|_| ())
            });
    }
    let PackWriter { sha1, .. } = pack;
    let trailer = sha1.try_finalize().map_err(|_| CodecError::Integrity)?;
    let result = result
        .and_then(|()| writer.write_all(trailer.as_slice()))
        .and_then(|()| writer.flush());
    if result.is_err() {
        return Err(fail(&writer));
    }
    Ok((hex(&sha2::Digest::finalize(writer.sha256)), writer.written))
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
pub(crate) fn write_tree(
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
    #[allow(clippy::too_many_lines, reason = "one end-to-end round trip")]
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
        let tree = write_tree(&loose, &source, 0)
            .expect("tree")
            .expect("files");
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
        let first_file = scratch.path().join("first.bundle");
        fs::write(&first_file, &first.bytes).expect("segment file");
        let hydrated = StoreCodec
            .hydrate(
                &first_file,
                &expectation(&first.bytes, &refs),
                &handoff.to_string(),
                &git_dir,
                &worktree,
            )
            .expect("hydrated");
        assert_eq!(hydrated.files, 4);
        assert_eq!(
            fs::read_to_string(worktree.join("src/main.rs")).expect("r"),
            "fn main() {}\n"
        );
        assert_eq!(
            fs::metadata(worktree.join("run.sh"))
                .expect("m")
                .permissions()
                .mode()
                & 0o111,
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
            .commit_and_bundle(
                &git_dir,
                &worktree,
                &turn_commit(&handoff.to_string(), 3),
                &refs,
                1 << 30,
                &scratch.path().join("turn.segment"),
            )
            .expect("built");
        let built_bytes = fs::read(&built.path).expect("built file");
        assert_eq!(built.sha256, sha256_hex(&built_bytes));
        assert_eq!(built.bytes, built_bytes.len() as u64);

        // The published segment verifies and carries the new cloud ref.
        let mut cloud_refs = refs.clone();
        cloud_refs.insert(REF_CLOUD.to_owned(), built.commit.clone());
        let check = scratch.path().join("check.git");
        let out = scratch.path().join("out");
        fs::create_dir_all(&out).expect("out");
        let mut expect = expectation(&built_bytes, &cloud_refs);
        expect.refs = cloud_refs;
        StoreCodec
            .hydrate(&built.path, &expect, &built.commit, &check, &out)
            .expect("the published segment verifies");
        assert_eq!(
            fs::read_to_string(out.join("README.md")).expect("r"),
            "hello cloud\n"
        );
        assert_eq!(fs::read_to_string(out.join("NEW.txt")).expect("r"), "new\n");
        assert!(!out.join(".git").exists());
        let verified_db = ObjectDb::open_objects(&check.join("objects")).expect("db");
        let info = verified_db
            .commit(&oid(&built.commit).expect("oid"))
            .expect("commit");
        assert_eq!(info.parents, vec![handoff]);
        assert!(info.message.contains("Elitea-Epoch: 2"));
        assert_real_git_accepts(scratch.path(), &built_bytes, &built.commit);
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
        run(&[
            "clone",
            "--mirror",
            "-q",
            file.to_str().expect("utf8"),
            mirror.to_str().expect("utf8"),
        ]);
        run(&[
            "-C",
            mirror.to_str().expect("utf8"),
            "fsck",
            "--strict",
            "--no-dangling",
        ]);
        let heads = run(&[
            "-C",
            mirror.to_str().expect("utf8"),
            "rev-parse",
            "refs/elitea/cloud",
        ]);
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
        let file = scratch.path().join("bad.bundle");
        fs::write(&file, &segment).expect("file");
        assert_eq!(
            StoreCodec.hydrate(
                &file,
                &expect,
                &"1".repeat(40),
                &scratch.path().join("q"),
                &worktree
            ),
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
        for bad in [
            "/etc/passwd",
            "../x",
            "a/../b",
            ".git/config",
            "a/.GIT/x",
            "",
        ] {
            assert!(!safe_relative(bad), "{bad}");
        }
    }

    /// A reader that refuses any single read larger than the codec's chunk:
    /// a `read_to_end` of the segment, or a buffer sized to the file, panics.
    struct BoundedReads(File);

    impl Read for BoundedReads {
        fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
            assert!(
                buffer.len() <= READ_CHUNK_BYTES,
                "a segment read asked for {} bytes at once",
                buffer.len()
            );
            self.0.read(buffer)
        }
    }

    fn bounded(file: File) -> Box<dyn Read + Send> {
        Box::new(BoundedReads(file))
    }

    /// Item 4: a segment many times the read chunk is written to disk,
    /// verified and indexed with every read bounded, and the turn's segment
    /// is streamed to a file under the cap (and refused over it).
    #[test]
    fn segments_are_verified_and_written_from_disk_in_bounded_reads() {
        let scratch = tempfile::tempdir().expect("scratch");
        let source = scratch.path().join("src");
        fs::create_dir_all(&source).expect("mkdir");
        // ~3 MiB of incompressible bytes: 48 chunks of the read bound.
        let mut state: u64 = 0x9e37_79b9_7f4a_7c15;
        let noise: Vec<u8> = (0..3 * 1024 * 1024)
            .map(|_| {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                state.to_be_bytes()[4]
            })
            .collect();
        fs::write(source.join("blob.bin"), &noise).expect("w");
        fs::write(source.join("a.txt"), "a\n").expect("w");
        let desktop = scratch.path().join("desktop.git");
        bundle::init_quarantine(&desktop).expect("init");
        let loose = gix_odb::loose::Store::at(desktop.join("objects"), gix_hash::Kind::Sha1);
        let tree = write_tree(&loose, &source, 0)
            .expect("tree")
            .expect("files");
        let raw =
            format!("tree {tree}\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nsnapshot\n");
        let handoff = loose
            .write_buf(gix_object::Kind::Commit, raw.as_bytes())
            .expect("commit");
        let db = ObjectDb::open_objects(&desktop.join("objects")).expect("db");
        let segment = scratch.path().join("first.bundle");
        let tips = [(HANDOFF_REF.to_owned(), handoff)];
        // Over its cap the write stops and says so.
        assert_eq!(
            write_compact_file(&db, &tips, Limits::default(), 1024 * 1024, &segment),
            Err(CodecError::TooLarge)
        );
        let (sha256, bytes) =
            write_compact_file(&db, &tips, Limits::default(), 1 << 30, &segment).expect("written");
        assert!(bytes > 3 * 1024 * 1024);
        // The streamed writer produces exactly the store crate's bundle.
        let reference = bundle::write_compact(&db, &tips, Limits::default()).expect("reference");
        assert_eq!(reference.sha256, sha256);
        assert_eq!(fs::read(&segment).expect("file"), reference.bytes);

        let refs: BTreeMap<String, String> = [(HANDOFF_REF.to_owned(), handoff.to_string())].into();
        let expect = SegmentExpectation {
            sha256,
            bytes,
            refs,
            max_unpacked_bytes: 1 << 30,
            max_files: 10,
            max_file_bytes: 8 << 20,
        };
        let verified =
            verify_segment_file(&segment, &scratch.path().join("q.git"), &expect, bounded)
                .expect("verified in bounded reads");
        let commit = verified.db.commit(&handoff).expect("commit");
        assert_eq!(verified.db.flatten(&commit.tree).expect("tree").len(), 2);

        // A size the plan does not name is refused before any read.
        let mut wrong = expect.clone();
        wrong.bytes += 1;
        assert!(matches!(
            verify_segment_file(&segment, &scratch.path().join("q2.git"), &wrong, bounded),
            Err(CodecError::Integrity)
        ));
    }
}
