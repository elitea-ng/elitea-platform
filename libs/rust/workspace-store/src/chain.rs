//! Segment chains (ADR-0034 decision 2, Phase 2): a `COMPACT` and the
//! `PUSH` segments published after it.
//!
//! * **Publish plan** ([`push_prerequisites`]): a writer's next `PUSH` names
//!   the last published head as its prerequisites (every published ref value
//!   the writer's object database holds) and carries only what those
//!   commits' closure does not ([`crate::bundle::write_push`]). Main checks
//!   that every prerequisite is the current value of one of the thread's
//!   refs, so a `PUSH` always extends the chain the store holds.
//! * **Fetch plan** ([`check_plan`]): Main answers the shortest chain to the
//!   head: the newest live `COMPACT`, then every live `PUSH` after it, in
//!   `seq` order. A reader checks the plan's shape before it fetches a byte.
//! * **Verification** ([`verify_chain_into`]): every segment is checked
//!   against its content address, its prerequisites are proved present in
//!   the quarantine (what the earlier segments carried) BEFORE its pack is
//!   indexed, its refs must be the plan's, and connectivity is walked from
//!   its refs over what the chain proved so far. Nothing outside the
//!   quarantine is touched, and nothing is usable unless every segment
//!   passed.

use std::collections::{BTreeMap, HashSet};
use std::path::Path;

use gix_hash::ObjectId;

use crate::bundle::{self, Header, Limits, VerifiedBundle};
use crate::connectivity;
use crate::digest::sha256_hex;
use crate::error::StoreError;
use crate::objects::{ObjectDb, oid};
use crate::transport::FetchPlan;
use crate::{KIND_COMPACT, KIND_PUSH};

/// The prerequisites of the writer's next `PUSH`: the distinct commits the
/// store's refs hold (`published`, the fetch plan's or the last publish's
/// refs) that `db` has, in a stable order. Empty when `db` holds none of
/// them: the writer publishes a `COMPACT`.
///
/// # Errors
///
/// A published value that is not an object id, or an unreadable object.
pub fn push_prerequisites(
    db: &ObjectDb,
    published: &BTreeMap<String, String>,
) -> Result<Vec<ObjectId>, StoreError> {
    let mut out: Vec<ObjectId> = Vec::new();
    for sha in published.values() {
        let id = oid(sha)?;
        if out.contains(&id) || !db.exists(&id) {
            continue;
        }
        if matches!(db.find(&id)?, Some((gix_object::Kind::Commit, _))) {
            out.push(id);
        }
    }
    out.sort();
    out.truncate(bundle::MAX_PREREQUISITES);
    Ok(out)
}

/// A fetch plan's shape: a non-empty chain in ascending `seq`, a `COMPACT`
/// with no prerequisites first, then only `PUSH` segments with 1 to
/// [`bundle::MAX_PREREQUISITES`] prerequisites; the last segment is the head
/// and holds the plan's refs.
///
/// # Errors
///
/// [`StoreError::Integrity`] naming what is wrong;
/// [`StoreError::Unsupported`] for a kind this client does not read.
pub fn check_plan(plan: &FetchPlan) -> Result<(), StoreError> {
    let Some(first) = plan.segments.first() else {
        return Err(StoreError::Integrity(
            "the store holds no segment for this thread".into(),
        ));
    };
    if first.kind != KIND_COMPACT || !first.prerequisites.is_empty() {
        return Err(StoreError::Integrity(
            "the fetch plan does not start with a COMPACT segment".into(),
        ));
    }
    let mut previous = 0;
    for segment in &plan.segments {
        if segment.seq <= previous {
            return Err(StoreError::Integrity(
                "the fetch plan is not in seq order".into(),
            ));
        }
        previous = segment.seq;
        match segment.kind.as_str() {
            KIND_COMPACT if segment.seq == first.seq => {}
            KIND_COMPACT => {
                return Err(StoreError::Integrity(
                    "a COMPACT segment after the chain's first".into(),
                ));
            }
            KIND_PUSH
                if !segment.prerequisites.is_empty()
                    && segment.prerequisites.len() <= bundle::MAX_PREREQUISITES => {}
            KIND_PUSH => {
                return Err(StoreError::Integrity(
                    "a PUSH segment without valid prerequisites".into(),
                ));
            }
            other => {
                return Err(StoreError::Unsupported(format!(
                    "a segment of kind `{other}`"
                )));
            }
        }
    }
    let last = &plan.segments[plan.segments.len() - 1];
    if last.seq != plan.head_seq || last.refs != plan.refs {
        return Err(StoreError::Integrity(
            "the fetch plan's last segment is not the head".into(),
        ));
    }
    Ok(())
}

/// One fetched segment of a chain, as its plan names it.
#[derive(Clone, Copy, Debug)]
pub struct ChainSegment<'a> {
    pub seq: u64,
    pub kind: &'a str,
    /// The content address the plan named.
    pub sha256: &'a str,
    pub prerequisites: &'a [String],
    /// The thread's refs after this segment (the plan's).
    pub refs: &'a BTreeMap<String, String>,
    pub bytes: &'a [u8],
}

/// The chain of a fetched plan: each of its segments with the bytes
/// fetched for it ([`crate::sync::fetch_chain`]'s answer), in plan order.
///
/// # Errors
///
/// [`StoreError::Integrity`] when there is not exactly one body per
/// segment: a chain short of its head must never verify as an older head.
pub fn plan_segments<'a>(
    plan: &'a FetchPlan,
    bodies: &'a [Vec<u8>],
) -> Result<Vec<ChainSegment<'a>>, StoreError> {
    if plan.segments.len() != bodies.len() {
        return Err(StoreError::Integrity(
            "the fetched chain does not match its plan".into(),
        ));
    }
    Ok(plan
        .segments
        .iter()
        .zip(bodies)
        .map(|(segment, bytes)| ChainSegment {
            seq: segment.seq,
            kind: &segment.kind,
            sha256: &segment.sha256,
            prerequisites: &segment.prerequisites,
            refs: &segment.refs,
            bytes,
        })
        .collect())
}

/// Verify a chain (a `COMPACT`, then `PUSH` segments in `seq` order) into
/// `quarantine` (an empty one, see [`bundle::init_quarantine`]). The result
/// holds the refs of the chain's last segment, the quarantine's objects and
/// each segment's pack offset (for importing them).
///
/// A `COMPACT`-only chain (one segment) reads exactly as
/// [`bundle::verify_into`] does.
///
/// # Errors
///
/// [`StoreError::Integrity`] for a wrong content address, a prerequisite
/// the chain does not hold (nothing of that segment is indexed), refs other
/// than the plan's, a broken pack, a missing object, or a chain out of
/// shape; as [`bundle::parse_header`].
pub fn verify_chain_into(
    segments: &[ChainSegment<'_>],
    quarantine: &Path,
    limits: Limits,
) -> Result<VerifiedBundle, StoreError> {
    let Some((first, rest)) = segments.split_first() else {
        return Err(StoreError::Integrity("the chain is empty".into()));
    };
    if first.kind != KIND_COMPACT || !first.prerequisites.is_empty() {
        return Err(StoreError::Integrity(
            "the chain does not start with a COMPACT segment".into(),
        ));
    }
    let compact = bundle::verify_into(
        first.bytes,
        quarantine,
        &bundle::Expect {
            sha256: Some(first.sha256),
            bytes: None,
            refs: Some(first.refs),
        },
        limits,
    )?;
    let mut known: HashSet<ObjectId> = connectivity::reachable(
        &compact.db,
        &compact
            .header
            .refs
            .iter()
            .map(|(_, id)| *id)
            .collect::<Vec<_>>(),
        limits.max_objects,
    )?
    .objects
    .into_iter()
    .collect();
    let mut pack_offsets = vec![compact.pack_offset];
    let mut previous = first.seq;
    let mut db = compact.db;
    for segment in rest {
        if segment.seq <= previous {
            return Err(StoreError::Integrity(
                "the chain is not in seq order".into(),
            ));
        }
        previous = segment.seq;
        if segment.kind != KIND_PUSH {
            return Err(StoreError::Integrity(
                "only PUSH segments follow the chain's COMPACT".into(),
            ));
        }
        let (offset, next) = verify_push(segment, quarantine, &db, &mut known, limits)?;
        pack_offsets.push(offset);
        db = next;
    }
    let last = segments[segments.len() - 1];
    let mut refs = Vec::with_capacity(last.refs.len());
    for (name, sha) in last.refs {
        let id = oid(sha).map_err(|_| StoreError::Integrity("a plan ref is not an id".into()))?;
        if !known.contains(&id) {
            return Err(StoreError::Integrity(format!(
                "the chain does not carry the commit of `{name}`"
            )));
        }
        refs.push((name.clone(), id));
    }
    let tips: Vec<ObjectId> = refs.iter().map(|(_, id)| *id).collect();
    let walk = connectivity::reachable(&db, &tips, limits.max_objects)?;
    if walk.blob_bytes > limits.max_unpacked_bytes {
        return Err(StoreError::Integrity(format!(
            "the chain unpacks to {} bytes; the limit is {}",
            walk.blob_bytes, limits.max_unpacked_bytes
        )));
    }
    Ok(VerifiedBundle {
        header: Header {
            prerequisites: Vec::new(),
            refs,
            pack_offset: compact.pack_offset,
        },
        db,
        quarantine: quarantine.to_path_buf(),
        pack_offset: compact.pack_offset,
        pack_offsets,
        objects: walk.objects.len(),
        unpacked_bytes: walk.blob_bytes,
    })
}

/// One `PUSH` of a chain: its pack's offset and the quarantine reopened over
/// it. `known` gains what it carried.
fn verify_push(
    segment: &ChainSegment<'_>,
    quarantine: &Path,
    db: &ObjectDb,
    known: &mut HashSet<ObjectId>,
    limits: Limits,
) -> Result<(usize, ObjectDb), StoreError> {
    if sha256_hex(segment.bytes) != segment.sha256 {
        return Err(StoreError::Integrity(format!(
            "segment {}'s SHA-256 is not its content address",
            segment.seq
        )));
    }
    let header = bundle::parse_header(segment.bytes)?;
    if header.refs.is_empty() || header.prerequisites.is_empty() {
        return Err(StoreError::Integrity(format!(
            "segment {} is not a PUSH bundle",
            segment.seq
        )));
    }
    let mut named: Vec<ObjectId> = Vec::with_capacity(segment.prerequisites.len());
    for sha in segment.prerequisites {
        named.push(oid(sha).map_err(|_| StoreError::Integrity("a bad prerequisite".into()))?);
    }
    let mut declared = header.prerequisites.clone();
    declared.sort();
    named.sort();
    if declared != named {
        return Err(StoreError::Integrity(format!(
            "segment {}'s prerequisites are not the plan's",
            segment.seq
        )));
    }
    // Before anything of it is indexed: every prerequisite is a commit the
    // chain already proved, with its whole closure.
    for prerequisite in &declared {
        if !known.contains(prerequisite) || !db.exists(prerequisite) {
            return Err(StoreError::Integrity(format!(
                "segment {} needs {prerequisite}, which the chain does not hold",
                segment.seq
            )));
        }
    }
    for (name, id) in &header.refs {
        if segment.refs.get(name) != Some(&id.to_string()) {
            return Err(StoreError::Integrity(format!(
                "segment {}'s refs are not the plan's",
                segment.seq
            )));
        }
    }
    bundle::index_pack(
        &segment.bytes[header.pack_offset..],
        &quarantine.join("objects/pack"),
    )?;
    let db = ObjectDb::open_objects(&quarantine.join("objects"))?;
    let tips: Vec<ObjectId> = header.refs.iter().map(|(_, id)| *id).collect();
    let walk = connectivity::reachable_excluding(&db, &tips, known, limits.max_objects)?;
    known.extend(walk.objects);
    Ok((header.pack_offset, db))
}
