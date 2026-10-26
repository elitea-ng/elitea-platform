//! An in-process store with Main's publish rules (ADR-0034 decisions 2 and
//! 7, contract §2.2–§2.5), for tests of both writers: the epoch fence, the
//! ref allowlist, the object checked (size and checksum) before the
//! transaction, all-or-nothing ref compare-and-set, an identical replay
//! answered with the original result, and plan URLs that expire. Phase 2:
//! a `PUSH` names prerequisites that are current ref values and needs a live
//! `COMPACT`; a `COMPACT` supersedes the chain; at most
//! [`crate::compaction::SERVER_MAX_PUSH_CHAIN`] live `PUSH` segments; the
//! fetch plan is the live chain.

use std::collections::BTreeMap;
use std::sync::Mutex;

use async_trait::async_trait;
use serde_json::json;

use crate::digest::sha256_hex;
use crate::error::StoreError;
use crate::refs::{self, Writer};
use crate::transport::{
    CompletedPart, FetchPlan, Multipart, PlanSegment, PublishRequest, Published, PutTarget,
    StoreTransport, UploadGrant, UploadRequest,
};

#[derive(Clone, Debug)]
struct Segment {
    seq: u64,
    kind: String,
    prerequisites: Vec<String>,
    superseded: bool,
    sha256: String,
    bytes: u64,
    refs: BTreeMap<String, String>,
    request: PublishRequest,
    result: Published,
}

#[derive(Debug)]
struct Inner {
    epoch: u64,
    writer: Writer,
    objects: BTreeMap<String, Vec<u8>>,
    parts: BTreeMap<(String, u32), Vec<u8>>,
    refs: BTreeMap<String, String>,
    segments: Vec<Segment>,
    /// Plan URLs carry this; bumping it expires every URL handed out.
    url_generation: u64,
    multipart_threshold: u64,
    part_size: u64,
    publishes: u64,
}

/// The in-process store of one thread.
#[derive(Debug)]
pub struct MemoryStore {
    inner: Mutex<Inner>,
}

impl Default for MemoryStore {
    fn default() -> Self {
        Self::new(1, Writer::Desktop)
    }
}

fn lock(store: &MemoryStore) -> std::sync::MutexGuard<'_, Inner> {
    store
        .inner
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl MemoryStore {
    #[must_use]
    pub fn new(epoch: u64, writer: Writer) -> Self {
        Self {
            inner: Mutex::new(Inner {
                epoch,
                writer,
                objects: BTreeMap::new(),
                parts: BTreeMap::new(),
                refs: BTreeMap::new(),
                segments: Vec::new(),
                url_generation: 0,
                multipart_threshold: 64 * 1024 * 1024,
                part_size: 8 * 1024 * 1024,
                publishes: 0,
            }),
        }
    }

    /// Segments larger than `threshold` get a multipart grant of `part_size`
    /// parts.
    #[must_use]
    pub fn with_multipart(self, threshold: u64, part_size: u64) -> Self {
        {
            let mut inner = lock(&self);
            inner.multipart_threshold = threshold;
            inner.part_size = part_size;
        }
        self
    }

    /// A state change: the epoch and who may publish.
    pub fn transition(&self, epoch: u64, writer: Writer) {
        let mut inner = lock(self);
        inner.epoch = epoch;
        inner.writer = writer;
    }

    /// Expire every URL handed out so far.
    pub fn expire_urls(&self) {
        lock(self).url_generation += 1;
    }

    /// Flip one byte of a stored object (a tampered segment).
    pub fn tamper(&self, sha256: &str, at: usize) {
        if let Some(bytes) = lock(self).objects.get_mut(sha256)
            && let Some(byte) = bytes.get_mut(at)
        {
            *byte ^= 0xff;
        }
    }

    /// Store `bytes` as an object and publish them as the executor would
    /// (tests of take-back).
    pub fn put_object(&self, bytes: Vec<u8>) -> String {
        let sha = sha256_hex(&bytes);
        lock(self).objects.insert(sha.clone(), bytes);
        sha
    }

    #[must_use]
    pub fn refs(&self) -> BTreeMap<String, String> {
        lock(self).refs.clone()
    }

    #[must_use]
    pub fn segment_count(&self) -> usize {
        lock(self).segments.len()
    }

    /// The live segments' kinds, in `seq` order (the chain).
    #[must_use]
    pub fn live_kinds(&self) -> Vec<String> {
        lock(self)
            .segments
            .iter()
            .filter(|segment| !segment.superseded)
            .map(|segment| segment.kind.clone())
            .collect()
    }

    #[must_use]
    pub fn object_count(&self) -> usize {
        lock(self).objects.len()
    }

    /// Publishes that reached the store (applied or refused).
    #[must_use]
    pub fn publishes(&self) -> u64 {
        lock(self).publishes
    }

    #[must_use]
    pub fn object(&self, sha256: &str) -> Option<Vec<u8>> {
        lock(self).objects.get(sha256).cloned()
    }

    /// Lose an object the store acknowledged (a dropped upload).
    pub fn drop_object(&self, sha256: &str) {
        lock(self).objects.remove(sha256);
    }
}

fn sha_of_url<'a>(url: &'a str, prefix: &str) -> Result<&'a str, StoreError> {
    url.strip_prefix(prefix)
        .map(|rest| rest.split('?').next().unwrap_or_default())
        .ok_or_else(|| StoreError::http(404, "not_found", "no such URL"))
}

/// Whether `request` is a `PUSH`; a `COMPACT` names no prerequisites, a
/// `PUSH` 1 to 8, and no other kind exists.
///
/// Main's rule is `workspaceruntime.ValidPrerequisites` (the bound is
/// [`crate::bundle::MAX_PREREQUISITES`], pinned against Main's by
/// `TestChainBoundsMatchTheRustStore`); keep the two in step.
fn is_push(request: &PublishRequest) -> Result<bool, StoreError> {
    match request.kind.as_str() {
        crate::KIND_COMPACT if request.prerequisites.is_empty() => Ok(false),
        crate::KIND_PUSH
            if !request.prerequisites.is_empty()
                && request.prerequisites.len() <= crate::bundle::MAX_PREREQUISITES =>
        {
            Ok(true)
        }
        _ => Err(StoreError::http(
            422,
            "invalid_request",
            "a COMPACT has no prerequisites; a PUSH names 1 to 8",
        )),
    }
}

/// Every expected old value holds, or `409 ref_conflict` with the current
/// values of the named refs.
fn ref_conflict(inner: &Inner, request: &PublishRequest) -> Result<(), StoreError> {
    let conflict = request
        .refs
        .iter()
        .any(|update| inner.refs.get(&update.name) != update.old.as_ref());
    if !conflict {
        return Ok(());
    }
    let current: serde_json::Map<String, serde_json::Value> = request
        .refs
        .iter()
        .map(|update| {
            (
                update.name.clone(),
                inner
                    .refs
                    .get(&update.name)
                    .map_or(serde_json::Value::Null, |sha| json!(sha)),
            )
        })
        .collect();
    Err(StoreError::Http {
        status: 409,
        code: "ref_conflict".into(),
        message: "a ref moved".into(),
        details: json!({ "current": current }),
    })
}

/// A `PUSH`'s rules inside the transaction: a live `COMPACT` to extend,
/// prerequisites that are current ref values, and room in the chain.
fn check_push(inner: &Inner, request: &PublishRequest) -> Result<(), StoreError> {
    let live_compact = inner
        .segments
        .iter()
        .any(|segment| !segment.superseded && segment.kind == crate::KIND_COMPACT);
    let known = request
        .prerequisites
        .iter()
        .all(|sha| inner.refs.values().any(|value| value == sha));
    if !live_compact || !known {
        return Err(StoreError::http(
            409,
            "prerequisite_unknown",
            "a prerequisite is not a commit the store's refs hold",
        ));
    }
    let pushes = inner
        .segments
        .iter()
        .filter(|segment| !segment.superseded && segment.kind == crate::KIND_PUSH)
        .count();
    if pushes >= crate::compaction::SERVER_MAX_PUSH_CHAIN {
        return Err(StoreError::http(
            409,
            "segment_chain_too_long",
            "publish a COMPACT",
        ));
    }
    Ok(())
}

#[async_trait]
impl StoreTransport for MemoryStore {
    async fn upload_grant(&self, request: &UploadRequest) -> Result<UploadGrant, StoreError> {
        let inner = lock(self);
        if request.epoch != inner.epoch {
            return Err(StoreError::http(409, "stale_epoch", "the epoch moved on"));
        }
        if !crate::digest::is_sha256_hex(&request.sha256) {
            return Err(StoreError::http(422, "invalid_request", "bad digest"));
        }
        if inner.objects.contains_key(&request.sha256) {
            return Ok(UploadGrant {
                exists: true,
                ..UploadGrant::default()
            });
        }
        if request.bytes > inner.multipart_threshold {
            let count = request.bytes.div_ceil(inner.part_size);
            return Ok(UploadGrant {
                multipart: Some(Multipart {
                    upload_id: request.sha256.clone(),
                    part_size: inner.part_size,
                    part_urls: (1..=count)
                        .map(|n| {
                            format!(
                                "mem://part/{}/{n}?g={}",
                                request.sha256, inner.url_generation
                            )
                        })
                        .collect(),
                    complete_url: format!("mem://complete/{}", request.sha256),
                }),
                ..UploadGrant::default()
            });
        }
        Ok(UploadGrant {
            put: Some(PutTarget {
                url: format!("mem://put/{}?g={}", request.sha256, inner.url_generation),
                headers: BTreeMap::from([(
                    "Content-Type".to_owned(),
                    "application/x-git-bundle".to_owned(),
                )]),
            }),
            ..UploadGrant::default()
        })
    }

    async fn put(&self, target: &PutTarget, bytes: &[u8]) -> Result<(), StoreError> {
        let sha = sha_of_url(&target.url, "mem://put/")?.to_owned();
        let mut inner = lock(self);
        if !target
            .url
            .ends_with(&format!("?g={}", inner.url_generation))
        {
            return Err(StoreError::http(403, "expired", "the URL expired"));
        }
        inner.objects.insert(sha, bytes.to_vec());
        Ok(())
    }

    async fn put_part(&self, url: &str, bytes: &[u8]) -> Result<String, StoreError> {
        let rest = sha_of_url(url, "mem://part/")?;
        let (sha, number) = rest
            .split_once('/')
            .ok_or_else(|| StoreError::http(404, "not_found", "no such part"))?;
        let number: u32 = number
            .parse()
            .map_err(|_| StoreError::http(404, "not_found", "no such part"))?;
        lock(self)
            .parts
            .insert((sha.to_owned(), number), bytes.to_vec());
        Ok(format!("etag-{number}"))
    }

    async fn complete_multipart(
        &self,
        multipart: &Multipart,
        parts: &[CompletedPart],
    ) -> Result<(), StoreError> {
        let sha = sha_of_url(&multipart.complete_url, "mem://complete/")?.to_owned();
        let mut inner = lock(self);
        let mut bytes = Vec::new();
        for part in parts {
            let chunk = inner
                .parts
                .remove(&(sha.clone(), part.part_number))
                .ok_or_else(|| StoreError::http(422, "invalid_part", "a part is missing"))?;
            bytes.extend_from_slice(&chunk);
        }
        inner.objects.insert(sha, bytes);
        Ok(())
    }

    async fn publish(&self, request: &PublishRequest) -> Result<Published, StoreError> {
        let mut inner = lock(self);
        inner.publishes += 1;
        // Before the transaction: the object exists with the declared size
        // and checksum.
        match inner.objects.get(&request.segment_sha256) {
            None => {
                return Err(StoreError::http(
                    422,
                    "segment_missing",
                    "the segment is not stored",
                ));
            }
            Some(bytes)
                if bytes.len() as u64 != request.bytes
                    || sha256_hex(bytes) != request.segment_sha256 =>
            {
                return Err(StoreError::http(
                    422,
                    "checksum_mismatch",
                    "the stored segment differs",
                ));
            }
            Some(_) => {}
        }
        if request.object_format != crate::OBJECT_FORMAT_SHA1 {
            return Err(StoreError::http(
                422,
                "unsupported_object_format",
                "sha1 only",
            ));
        }
        let push = is_push(request)?;
        // The transaction.
        if let Some(previous) = inner
            .segments
            .iter()
            .find(|segment| segment.request == *request)
            && request
                .refs
                .iter()
                .all(|update| inner.refs.get(&update.name) == Some(&update.new))
        {
            return Ok(previous.result.clone());
        }
        if request.epoch != inner.epoch {
            return Err(StoreError::http(409, "stale_epoch", "the epoch moved on"));
        }
        let names: Vec<&str> = request
            .refs
            .iter()
            .map(|update| update.name.as_str())
            .collect();
        if refs::check(inner.writer, None, &names).is_err() {
            return Err(StoreError::http(
                403,
                "ref_not_allowed",
                "a ref this writer may not write",
            ));
        }
        ref_conflict(&inner, request)?;
        if push {
            check_push(&inner, request)?;
        }
        let mut superseded = Vec::new();
        if !push {
            for segment in &mut inner.segments {
                if !segment.superseded {
                    segment.superseded = true;
                    superseded.push(json!(segment.seq));
                }
            }
        }
        let seq = inner.segments.last().map_or(1, |last| last.seq + 1);
        for update in &request.refs {
            inner.refs.insert(update.name.clone(), update.new.clone());
        }
        let result = Published {
            seq,
            refs: inner.refs.clone(),
            superseded,
        };
        let refs = inner.refs.clone();
        inner.segments.push(Segment {
            seq,
            kind: request.kind.clone(),
            prerequisites: request.prerequisites.clone(),
            superseded: false,
            sha256: request.segment_sha256.clone(),
            bytes: request.bytes,
            refs,
            request: request.clone(),
            result: result.clone(),
        });
        Ok(result)
    }

    async fn fetch_plan(&self, since_seq: u64) -> Result<FetchPlan, StoreError> {
        let inner = lock(self);
        let head_seq = inner.segments.last().map_or(0, |last| last.seq);
        // The live chain, as Main's `workspacestore.SelectChain` answers
        // it: the newest live COMPACT unless `since_seq` is past it, then
        // every live PUSH after both the COMPACT and `since_seq`. The
        // readers here always ask from 0 (see `chain::check_plan`).
        let live: Vec<&Segment> = inner
            .segments
            .iter()
            .filter(|segment| !segment.superseded)
            .collect();
        let compact_seq = live
            .iter()
            .rev()
            .find(|segment| segment.kind == crate::KIND_COMPACT)
            .map(|segment| segment.seq);
        let segments = match compact_seq {
            None => Vec::new(),
            Some(compact) => live
                .iter()
                .filter(|segment| {
                    if segment.seq == compact {
                        since_seq <= compact
                    } else {
                        segment.seq > compact && segment.seq > since_seq
                    }
                })
                .map(|segment| PlanSegment {
                    seq: segment.seq,
                    kind: segment.kind.clone(),
                    sha256: segment.sha256.clone(),
                    bytes: segment.bytes,
                    prerequisites: segment.prerequisites.clone(),
                    refs: segment.refs.clone(),
                    url: format!("mem://get/{}?g={}", segment.sha256, inner.url_generation),
                    expires_at: None,
                })
                .collect(),
        };
        Ok(FetchPlan {
            head_seq,
            refs: inner.refs.clone(),
            segments,
        })
    }

    async fn get(&self, url: &str) -> Result<Vec<u8>, StoreError> {
        let sha = sha_of_url(url, "mem://get/")?;
        let inner = lock(self);
        if !url.ends_with(&format!("?g={}", inner.url_generation)) {
            return Err(StoreError::http(403, "expired", "the URL expired"));
        }
        inner
            .objects
            .get(sha)
            .cloned()
            .ok_or_else(|| StoreError::http(404, "not_found", "no such object"))
    }
}
