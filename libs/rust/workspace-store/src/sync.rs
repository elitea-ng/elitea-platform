//! The flows over a [`StoreTransport`], with retries:
//!
//! * [`upload`]: the grant, then one PUT or the parts of a multipart upload;
//!   nothing when the digest is already stored. A PUT refused as expired
//!   (403, 404, 410) gets a fresh grant once.
//! * [`publish`]: the same request retried after no answer or a 5xx. The
//!   server answers an identical replay with the original result; should a
//!   retry still meet `409 ref_conflict` whose current refs ARE the refs
//!   this publish moved to, the first attempt landed and its answer was
//!   lost: that is success.
//! * [`upload_and_publish`]: both; a publish that finds the segment missing
//!   (`422 segment_missing`: the upload was dropped) uploads it again once.
//! * [`fetch_latest`]: the plan, the newest `COMPACT` and its bytes,
//!   checked against the plan's size and SHA-256. A URL refused as expired
//!   or gone, or bytes that do not match, fetch the plan again (a stale
//!   plan); after the last round, an integrity failure.

use std::time::Duration;

use crate::digest::sha256_hex;
use crate::error::StoreError;
use crate::transport::{
    CompletedPart, FetchPlan, PlanSegment, PublishRequest, Published, StoreTransport, UploadRequest,
};

/// How often and how patiently an operation is retried.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Backoff {
    /// Attempts in all (at least 1).
    pub attempts: u32,
    pub base: Duration,
    pub max: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self {
            attempts: 5,
            base: Duration::from_millis(250),
            max: Duration::from_secs(8),
        }
    }
}

impl Backoff {
    /// No waiting between attempts (tests).
    #[must_use]
    pub const fn immediate(attempts: u32) -> Self {
        Self {
            attempts,
            base: Duration::ZERO,
            max: Duration::ZERO,
        }
    }

    fn delay(&self, attempt: u32) -> Duration {
        let factor = 1u32.checked_shl(attempt.min(16)).unwrap_or(u32::MAX);
        self.base.saturating_mul(factor).min(self.max)
    }
}

/// Run `op` until it succeeds, fails for good, or the attempts run out.
async fn retry<T, F, Fut>(backoff: Backoff, mut op: F) -> Result<T, StoreError>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<T, StoreError>>,
{
    let attempts = backoff.attempts.max(1);
    let mut attempt = 0;
    loop {
        match op().await {
            Err(error) if error.is_retryable() && attempt + 1 < attempts => {
                tracing::debug!(code = error.code(), attempt, "store call retried");
                tokio::time::sleep(backoff.delay(attempt)).await;
                attempt += 1;
            }
            other => return other,
        }
    }
}

fn expired(error: &StoreError) -> bool {
    matches!(
        error,
        StoreError::Http {
            status: 403 | 404 | 410,
            ..
        }
    )
}

/// Upload a segment unless the store has it.
///
/// # Errors
///
/// The grant was refused (`stale_epoch`, `segment_too_large`,
/// `workspace_quota_exceeded`), or the bytes could not be sent.
pub async fn upload(
    transport: &dyn StoreTransport,
    epoch: u64,
    bytes: &[u8],
    backoff: Backoff,
) -> Result<(), StoreError> {
    let request = UploadRequest {
        epoch,
        sha256: sha256_hex(bytes),
        bytes: bytes.len() as u64,
    };
    let mut regranted = false;
    loop {
        let grant = retry(backoff, || transport.upload_grant(&request)).await?;
        if grant.exists {
            return Ok(());
        }
        let sent = if let Some(put) = &grant.put {
            retry(backoff, || transport.put(put, bytes)).await
        } else if let Some(multipart) = &grant.multipart {
            send_parts(transport, multipart, bytes, backoff).await
        } else {
            return Err(StoreError::Invalid(
                "the upload grant names no upload".into(),
            ));
        };
        match sent {
            Err(error) if expired(&error) && !regranted => regranted = true,
            other => return other,
        }
    }
}

async fn send_parts(
    transport: &dyn StoreTransport,
    multipart: &crate::transport::Multipart,
    bytes: &[u8],
    backoff: Backoff,
) -> Result<(), StoreError> {
    let size = usize::try_from(multipart.part_size)
        .ok()
        .filter(|size| *size > 0)
        .ok_or_else(|| StoreError::Invalid("the multipart grant has no part size".into()))?;
    let chunks: Vec<&[u8]> = bytes.chunks(size).collect();
    if chunks.len() != multipart.part_urls.len() {
        return Err(StoreError::Invalid(format!(
            "the multipart grant has {} part URLs for {} parts",
            multipart.part_urls.len(),
            chunks.len()
        )));
    }
    let mut parts = Vec::with_capacity(chunks.len());
    for (index, (chunk, url)) in chunks.iter().zip(&multipart.part_urls).enumerate() {
        let etag = retry(backoff, || transport.put_part(url, chunk)).await?;
        parts.push(CompletedPart {
            part_number: u32::try_from(index + 1).unwrap_or(u32::MAX),
            etag,
        });
    }
    retry(backoff, || transport.complete_multipart(multipart, &parts)).await
}

/// Whether a `ref_conflict`'s current refs are where `request` moved them.
fn conflict_is_our_own(error: &StoreError, request: &PublishRequest) -> bool {
    let StoreError::Http { code, details, .. } = error else {
        return false;
    };
    if code != "ref_conflict" {
        return false;
    }
    let current = details.get("current").unwrap_or(details);
    request.refs.iter().all(|update| {
        current.get(&update.name).and_then(|v| v.as_str()) == Some(update.new.as_str())
    })
}

/// Publish a segment, retried until the answer arrives.
///
/// # Errors
///
/// A refusal (`ref_conflict` that is someone else's, `stale_epoch`,
/// `ref_not_allowed`, `segment_missing`, `checksum_mismatch`), or no answer
/// after every attempt.
pub async fn publish(
    transport: &dyn StoreTransport,
    request: &PublishRequest,
    backoff: Backoff,
) -> Result<Published, StoreError> {
    let mut retried = false;
    let attempts = backoff.attempts.max(1);
    let mut attempt = 0;
    loop {
        match transport.publish(request).await {
            Ok(published) => return Ok(published),
            Err(error) if retried && conflict_is_our_own(&error, request) => {
                // The first attempt landed; its answer was lost. The seq is
                // the plan's head.
                let plan = retry(backoff, || transport.fetch_plan(0)).await?;
                let refs = request
                    .refs
                    .iter()
                    .map(|update| (update.name.clone(), update.new.clone()))
                    .collect();
                return Ok(Published {
                    seq: plan.head_seq,
                    refs,
                    superseded: Vec::new(),
                });
            }
            Err(error) if error.is_retryable() && attempt + 1 < attempts => {
                retried = true;
                tokio::time::sleep(backoff.delay(attempt)).await;
                attempt += 1;
            }
            Err(error) => return Err(error),
        }
    }
}

/// Upload, then publish; a dropped upload is sent again once.
///
/// # Errors
///
/// As [`upload`] and [`publish`].
pub async fn upload_and_publish(
    transport: &dyn StoreTransport,
    bytes: &[u8],
    request: &PublishRequest,
    backoff: Backoff,
) -> Result<Published, StoreError> {
    if request.segment_sha256 != sha256_hex(bytes) || request.bytes != bytes.len() as u64 {
        return Err(StoreError::Invalid(
            "the publish does not describe these bytes".into(),
        ));
    }
    upload(transport, request.epoch, bytes, backoff).await?;
    match publish(transport, request, backoff).await {
        Err(StoreError::Http { code, .. }) if code == "segment_missing" => {
            tracing::warn!("the store lost the uploaded segment; sending it again");
            upload(transport, request.epoch, bytes, backoff).await?;
            publish(transport, request, backoff).await
        }
        other => other,
    }
}

/// The plan rounds a fetch tries before it gives up.
const FETCH_ROUNDS: u32 = 3;

/// The newest `COMPACT` of the thread after `since_seq`, its bytes checked
/// against the plan.
///
/// # Errors
///
/// [`StoreError::Unsupported`] for an incremental segment,
/// [`StoreError::Integrity`] when the bytes never match the plan, or the
/// transport's errors.
pub async fn fetch_latest(
    transport: &dyn StoreTransport,
    since_seq: u64,
    backoff: Backoff,
) -> Result<(FetchPlan, PlanSegment, Vec<u8>), StoreError> {
    let mut last = StoreError::Integrity("the store holds no segment".into());
    for _round in 0..FETCH_ROUNDS {
        let plan = retry(backoff, || transport.fetch_plan(since_seq)).await?;
        let Some(segment) = plan
            .segments
            .iter()
            .max_by_key(|segment| segment.seq)
            .cloned()
        else {
            return Err(StoreError::Integrity(
                "the store holds no segment for this thread".into(),
            ));
        };
        if segment.kind != crate::KIND_COMPACT || !segment.prerequisites.is_empty() {
            return Err(StoreError::Unsupported(format!(
                "a {} segment with prerequisites",
                segment.kind
            )));
        }
        match retry(backoff, || transport.get(&segment.url)).await {
            Ok(bytes)
                if bytes.len() as u64 == segment.bytes && sha256_hex(&bytes) == segment.sha256 =>
            {
                return Ok((plan, segment, bytes));
            }
            Ok(_) => {
                last = StoreError::Integrity(
                    "the segment's bytes do not match the plan's SHA-256 or size".into(),
                );
            }
            Err(error) if expired(&error) => last = error,
            Err(error) => return Err(error),
        }
        tracing::debug!(code = last.code(), "fetch plan stale; fetching it again");
    }
    Err(match last {
        StoreError::Integrity(message) => StoreError::Integrity(message),
        other => StoreError::Integrity(format!("the plan's segment could not be fetched: {other}")),
    })
}
