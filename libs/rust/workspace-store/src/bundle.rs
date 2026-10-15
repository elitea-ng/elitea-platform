//! adapted from floe (MIT): `floe-git`'s `bundle_header` and
//! `write_bundle_gix` (a v2 header rendered from refs and prerequisites,
//! then the pack). The reading side, the quarantine and the checks are
//! this crate's.
//!
//! A segment is a git bundle v2 file:
//!
//! ```text
//! # v2 git bundle
//! -<oid> <comment>        prerequisites (none in a COMPACT)
//! <oid> <refname>         one line per ref
//!                         an empty line
//! PACK...                 the pack
//! ```
//!
//! A v3 header (`# v3 git bundle`, `@capability` lines) is read when its
//! only capability is `@object-format=sha1`; sha256 is refused as
//! unsupported, a filter as unsupported.
//!
//! **Reading verifies before anything is touched.** [`verify_into`] checks
//! the content address, parses the header, indexes the pack into a
//! quarantine object database of its own (a pack whose checksum or objects
//! are broken fails here), and runs [`crate::connectivity::reachable`] from
//! every ref. Only a [`VerifiedBundle`] is something a caller may apply.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicBool;

use gix_hash::ObjectId;

use crate::connectivity;
use crate::digest::sha256_hex;
use crate::error::StoreError;
use crate::objects::{ObjectDb, oid};

const V2: &[u8] = b"# v2 git bundle\n";
const V3: &[u8] = b"# v3 git bundle\n";
/// The largest header read.
const MAX_HEADER_BYTES: usize = 1024 * 1024;
/// The most refs one bundle may carry.
const MAX_REFS: usize = 1000;

/// What a reader or a writer may handle at most.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Objects reachable from the refs.
    pub max_objects: usize,
    /// The bytes of the blobs reachable from the refs (the unpacked tree).
    pub max_unpacked_bytes: u64,
}

impl Default for Limits {
    /// ADR-0034 decision 5: 20,000 files, 2 GB unpacked; trees and commits
    /// on top of the files.
    fn default() -> Self {
        Self {
            max_objects: 200_000,
            max_unpacked_bytes: 2 * 1024 * 1024 * 1024,
        }
    }
}

/// A parsed header.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Header {
    pub prerequisites: Vec<ObjectId>,
    /// `(name, id)` in the order written.
    pub refs: Vec<(String, ObjectId)>,
    /// Where the pack starts.
    pub pack_offset: usize,
}

impl Header {
    /// The refs by name.
    #[must_use]
    pub fn ref_map(&self) -> BTreeMap<String, String> {
        self.refs
            .iter()
            .map(|(name, id)| (name.clone(), id.to_string()))
            .collect()
    }
}

/// A ref name a bundle may carry: `refs/…`, no whitespace, no control
/// characters, no `..`.
fn valid_ref(name: &str) -> bool {
    name.starts_with("refs/")
        && name.len() <= 255
        && !name.contains("..")
        && !name.ends_with('/')
        && name
            .chars()
            .all(|c| !c.is_control() && !c.is_whitespace() && !"~^:?*[\\".contains(c))
}

/// Render a v2 header.
fn render_header(refs: &[(String, ObjectId)], prerequisites: &[ObjectId]) -> Vec<u8> {
    let mut out = V2.to_vec();
    for prerequisite in prerequisites {
        out.extend_from_slice(format!("-{prerequisite} \n").as_bytes());
    }
    for (name, id) in refs {
        out.extend_from_slice(format!("{id} {name}\n").as_bytes());
    }
    out.push(b'\n');
    out
}

/// Parse a bundle's header.
///
/// # Errors
///
/// Not a bundle ([`StoreError::Integrity`]), another object format
/// ([`StoreError::UnsupportedObjectFormat`]), a capability this client does
/// not read ([`StoreError::Unsupported`]).
pub fn parse_header(bytes: &[u8]) -> Result<Header, StoreError> {
    let not_a_bundle =
        |why: &str| StoreError::Integrity(format!("the segment is not a git bundle: {why}"));
    let v3 = if bytes.starts_with(V2) {
        false
    } else if bytes.starts_with(V3) {
        true
    } else {
        return Err(not_a_bundle("no bundle signature"));
    };
    let mut offset = V2.len();
    let mut header = Header {
        prerequisites: Vec::new(),
        refs: Vec::new(),
        pack_offset: 0,
    };
    loop {
        if offset > MAX_HEADER_BYTES {
            return Err(not_a_bundle("the header is too long"));
        }
        let Some(end) = bytes[offset..].iter().position(|b| *b == b'\n') else {
            return Err(not_a_bundle("the header does not end"));
        };
        let line = std::str::from_utf8(&bytes[offset..offset + end])
            .map_err(|_| not_a_bundle("a header line is not text"))?;
        offset += end + 1;
        if line.is_empty() {
            break;
        }
        if let Some(capability) = line.strip_prefix('@') {
            if !v3 {
                return Err(not_a_bundle("a capability in a v2 header"));
            }
            match capability.split_once('=') {
                Some(("object-format", "sha1")) => {}
                Some(("object-format", other)) => {
                    return Err(StoreError::UnsupportedObjectFormat(format!(
                        "the bundle uses {other} object ids; only sha1 is supported"
                    )));
                }
                _ => {
                    return Err(StoreError::Unsupported(format!(
                        "the bundle capability `{capability}`"
                    )));
                }
            }
            continue;
        }
        if let Some(rest) = line.strip_prefix('-') {
            let id = rest.split(' ').next().unwrap_or_default();
            header
                .prerequisites
                .push(oid(id).map_err(|_| not_a_bundle("a bad prerequisite"))?);
            continue;
        }
        let (id, name) = line
            .split_once(' ')
            .ok_or_else(|| not_a_bundle("a bad ref line"))?;
        if id.len() == 64 {
            return Err(StoreError::UnsupportedObjectFormat(
                "the bundle uses sha256 object ids".into(),
            ));
        }
        let id = oid(id).map_err(|_| not_a_bundle("a bad ref id"))?;
        if !valid_ref(name) {
            return Err(not_a_bundle("a bad ref name"));
        }
        if header.refs.iter().any(|(seen, _)| seen == name) {
            return Err(not_a_bundle("a ref is named twice"));
        }
        header.refs.push((name.to_owned(), id));
        if header.refs.len() > MAX_REFS {
            return Err(not_a_bundle("too many refs"));
        }
    }
    if !bytes[offset..].starts_with(b"PACK") {
        return Err(not_a_bundle("no pack after the header"));
    }
    header.pack_offset = offset;
    Ok(header)
}

/// A written segment.
#[derive(Clone, Debug)]
pub struct Written {
    pub bytes: Vec<u8>,
    /// The content address.
    pub sha256: String,
    pub objects: usize,
    pub commits: usize,
    /// Files (blobs) and their bytes: the unpacked size.
    pub blobs: usize,
    pub unpacked_bytes: u64,
}

/// Write a `COMPACT` bundle: every object reachable from `refs`, no
/// prerequisites.
///
/// # Errors
///
/// A missing object, a ref name a bundle cannot carry, or `limits`
/// exceeded ([`StoreError::Invalid`] naming the limit).
pub fn write_compact(
    db: &ObjectDb,
    refs: &[(String, ObjectId)],
    limits: Limits,
) -> Result<Written, StoreError> {
    if refs.is_empty() {
        return Err(StoreError::Invalid(
            "a bundle needs at least one ref".into(),
        ));
    }
    for (name, _) in refs {
        if !valid_ref(name) {
            return Err(StoreError::Invalid(format!(
                "`{name}` cannot be a bundle ref"
            )));
        }
    }
    let tips: Vec<ObjectId> = refs.iter().map(|(_, id)| *id).collect();
    let walk = connectivity::reachable(db, &tips, limits.max_objects)?;
    if walk.blob_bytes > limits.max_unpacked_bytes {
        return Err(StoreError::Invalid(format!(
            "the files hold {} bytes; the limit is {}",
            walk.blob_bytes, limits.max_unpacked_bytes
        )));
    }
    let mut bytes = render_header(refs, &[]);
    crate::pack::write(db, &walk.objects, &mut bytes)?;
    Ok(Written {
        sha256: sha256_hex(&bytes),
        bytes,
        objects: walk.objects.len(),
        commits: walk.commits,
        blobs: walk.blobs,
        unpacked_bytes: walk.blob_bytes,
    })
}

/// An empty git directory to read a bundle into, outside any folder the
/// person or the agent works in.
///
/// # Errors
///
/// It cannot be created, or `dir` exists and is not empty.
pub fn init_quarantine(dir: &Path) -> Result<(), StoreError> {
    let io = |error: std::io::Error| StoreError::io("cannot create the quarantine", &error);
    if dir.exists() && std::fs::read_dir(dir).map_err(io)?.next().is_some() {
        return Err(StoreError::Invalid(
            "the quarantine directory is not empty".into(),
        ));
    }
    for sub in ["objects/pack", "objects/info", "refs"] {
        std::fs::create_dir_all(dir.join(sub)).map_err(io)?;
    }
    std::fs::write(dir.join("HEAD"), "ref: refs/heads/main\n").map_err(io)?;
    Ok(())
}

/// A bundle that passed every check, indexed into its quarantine.
#[derive(Debug)]
pub struct VerifiedBundle {
    pub header: Header,
    /// The quarantine's objects.
    pub db: ObjectDb,
    pub quarantine: PathBuf,
    /// Where the pack starts in the segment's bytes (for importing it).
    pub pack_offset: usize,
    pub objects: usize,
    pub unpacked_bytes: u64,
}

impl VerifiedBundle {
    /// A ref's id; `None` when the bundle does not carry it.
    #[must_use]
    pub fn ref_id(&self, name: &str) -> Option<ObjectId> {
        self.header
            .refs
            .iter()
            .find(|(seen, _)| seen == name)
            .map(|(_, id)| *id)
    }
}

/// What [`verify_into`] checks besides the bundle itself.
#[derive(Clone, Debug, Default)]
pub struct Expect<'a> {
    /// The content address the plan named.
    pub sha256: Option<&'a str>,
    /// The size the plan named.
    pub bytes: Option<u64>,
    /// The refs the plan says the segment holds, exactly.
    pub refs: Option<&'a BTreeMap<String, String>>,
}

/// Verify a `COMPACT` segment and index it into `quarantine` (see
/// [`init_quarantine`]). Nothing outside the quarantine is touched, and
/// nothing is usable unless every check passed.
///
/// # Errors
///
/// [`StoreError::Integrity`] for a wrong digest or size, a broken pack, a
/// missing object, or refs other than expected; [`StoreError::Unsupported`]
/// for prerequisites (an incremental segment); as [`parse_header`].
pub fn verify_into(
    bytes: &[u8],
    quarantine: &Path,
    expect: &Expect<'_>,
    limits: Limits,
) -> Result<VerifiedBundle, StoreError> {
    if let Some(size) = expect.bytes
        && size != bytes.len() as u64
    {
        return Err(StoreError::Integrity(format!(
            "the segment is {} bytes, the plan says {size}",
            bytes.len()
        )));
    }
    if let Some(expected) = expect.sha256
        && sha256_hex(bytes) != expected
    {
        return Err(StoreError::Integrity(
            "the segment's SHA-256 is not its content address".into(),
        ));
    }
    let header = parse_header(bytes)?;
    if !header.prerequisites.is_empty() {
        return Err(StoreError::Unsupported(
            "an incremental segment (it has prerequisites)".into(),
        ));
    }
    if header.refs.is_empty() {
        return Err(StoreError::Integrity("the segment carries no ref".into()));
    }
    if let Some(expected) = expect.refs
        && &header.ref_map() != expected
    {
        return Err(StoreError::Integrity(
            "the segment's refs are not the plan's".into(),
        ));
    }
    index_pack(
        &bytes[header.pack_offset..],
        &quarantine.join("objects/pack"),
    )?;
    let db = ObjectDb::open_objects(&quarantine.join("objects"))?;
    let tips: Vec<ObjectId> = header.refs.iter().map(|(_, id)| *id).collect();
    let walk = connectivity::reachable(&db, &tips, limits.max_objects)?;
    if walk.blob_bytes > limits.max_unpacked_bytes {
        return Err(StoreError::Integrity(format!(
            "the segment unpacks to {} bytes; the limit is {}",
            walk.blob_bytes, limits.max_unpacked_bytes
        )));
    }
    Ok(VerifiedBundle {
        pack_offset: header.pack_offset,
        header,
        db,
        quarantine: quarantine.to_path_buf(),
        objects: walk.objects.len(),
        unpacked_bytes: walk.blob_bytes,
    })
}

/// Index a pack into `pack_dir` (`.pack` + `.idx`), verifying its checksum
/// and every object.
fn index_pack(pack: &[u8], pack_dir: &Path) -> Result<(), StoreError> {
    let interrupt = AtomicBool::new(false);
    let mut reader: &[u8] = pack;
    let outcome = gix_pack::Bundle::write_to_directory(
        &mut reader,
        Some(pack_dir),
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
    .map_err(|error| StoreError::Integrity(format!("the pack is broken: {error}")))?;
    if let Some(keep) = outcome.keep_path {
        let _ = std::fs::remove_file(keep);
    }
    Ok(())
}
