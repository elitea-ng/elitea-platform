//! The worker's half of `WorkspaceStoreService` (ADR-0034 decision 9): the
//! four claim-bound gRPC calls on Main's control listener, and the strict
//! reading of their frames.
//!
//! [`WorkspaceStore`] is what the turn orchestrator uses; [`GrpcWorkspaceStore`]
//! is the production implementation over the control channel. Every call is
//! one attempt, but for one answer: a publish Main answers `ABORTED`
//! (`thread_busy`: another change to the thread was being saved, nothing was
//! compared) is resent, the SAME request, with a bounded backoff. A lost
//! publish is retried by the orchestrator only as the same request too, which
//! Main answers with the original result.

use std::collections::BTreeMap;
use std::path::Path;
use std::time::Duration;

use async_trait::async_trait;
use sha2::{Digest as _, Sha256};
use tokio::time::timeout;
use tokio_stream::StreamExt as _;
use tonic::transport::Channel;

use crate::protocol::elitea::runtime::v1::{
    RuntimeErrorCodeV1, RuntimeErrorV1, WorkspaceClaimV1, WorkspaceEnvSecretFrameV1,
    WorkspaceEnvSecretRequestV1, WorkspaceExecutorEventKindV1, WorkspaceFetchFrameV1,
    WorkspaceFetchPlanRequestV1, WorkspaceForgeCredentialRequestV1,
    WorkspaceForgeCredentialResponseV1, WorkspacePublishRequestV1, WorkspacePublishResponseV1,
    WorkspaceRecordEventRequestV1, WorkspaceRecordEventResponseV1, WorkspaceRefUpdateV1,
    WorkspaceRefV1, WorkspaceRefusalCodeV1, WorkspaceRefusalV1, WorkspaceUploadFrameV1,
    WorkspaceUploadHeaderV1, WorkspaceUploadResponseV1, workspace_env_secret_frame_v1,
    workspace_fetch_frame_v1, workspace_store_service_client::WorkspaceStoreServiceClient,
    workspace_upload_frame_v1,
};

use super::context::sha1_hex;
use super::redact::SecretEnv;

/// Bytes per stream frame, both directions: under the control listener's
/// 64 KiB request and 80 KiB response bounds.
pub(crate) const CHUNK_BYTES: usize = 32 * 1_024;
/// The control listener's message bounds.
const MAX_REQUEST_BYTES: usize = 64 * 1_024;
const MAX_RESPONSE_BYTES: usize = 80 * 1_024;

pub(crate) const REF_CLOUD: &str = "refs/elitea/cloud";
pub(crate) const REF_HANDOFF_PREFIX: &str = "refs/elitea/handoff/";
/// The desktop's upstream commit U (decision 6), a pull request's base.
pub(crate) const REF_UPSTREAM: &str = "refs/elitea/upstream";
pub(crate) const KIND_COMPACT: &str = elitea_workspace_store::KIND_COMPACT;
pub(crate) const KIND_PUSH: &str = elitea_workspace_store::KIND_PUSH;
/// The most segments a fetch plan may hold: a `COMPACT` and Main's bound on
/// the `PUSH` segments after it.
pub(crate) const MAX_PLAN_SEGMENTS: usize =
    1 + elitea_workspace_store::compaction::SERVER_MAX_PUSH_CHAIN;
/// Main's absolute segment bound (`workspaceruntime.MaxSegmentBytes`, the
/// transport's). A fetch checks the stored chain against it, never against
/// the claim's current `max_segment_bytes`: a policy lowered after segments
/// were published must not refuse them for good (the thread would never run
/// again). The turn then compacts, since its PUSH budget is what the lower
/// bound leaves past the chain's PUSH bytes (`turn.rs`).
pub(crate) const MAIN_MAX_SEGMENT_BYTES: u64 = 2 << 30;
/// The most prerequisites one `PUSH` names.
pub(crate) const MAX_PREREQUISITES: usize = elitea_workspace_store::bundle::MAX_PREREQUISITES;
pub(crate) const OBJECT_FORMAT_SHA1: &str = "sha1";

/// A typed business refusal from Main.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RefusalCode {
    NotWorkspaceTurn,
    StaleEpoch,
    RefConflict,
    RefNotAllowed,
    SegmentMissing,
    ChecksumMismatch,
    UnsupportedObjectFormat,
    SegmentTooLarge,
    QuotaExceeded,
    /// `WorkspaceForgeCredential`: the thread has no pull-request credential.
    NoForgeCredential,
    /// `WorkspaceForgeCredential`: the policy no longer allows pull requests.
    PullRequestsDisabled,
}

impl RefusalCode {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::NotWorkspaceTurn => "not_a_workspace_turn",
            Self::StaleEpoch => "stale_epoch",
            Self::RefConflict => "ref_conflict",
            Self::RefNotAllowed => "ref_not_allowed",
            Self::SegmentMissing => "segment_missing",
            Self::ChecksumMismatch => "checksum_mismatch",
            Self::UnsupportedObjectFormat => "unsupported_object_format",
            Self::SegmentTooLarge => "segment_too_large",
            Self::QuotaExceeded => "workspace_quota_exceeded",
            Self::NoForgeCredential => "no_forge_credential",
            Self::PullRequestsDisabled => "pull_requests_disabled",
        }
    }

    fn from_wire(code: i32) -> Option<Self> {
        Some(match WorkspaceRefusalCodeV1::try_from(code).ok()? {
            WorkspaceRefusalCodeV1::NotAWorkspaceTurn => Self::NotWorkspaceTurn,
            WorkspaceRefusalCodeV1::StaleEpoch => Self::StaleEpoch,
            WorkspaceRefusalCodeV1::RefConflict => Self::RefConflict,
            WorkspaceRefusalCodeV1::RefNotAllowed => Self::RefNotAllowed,
            WorkspaceRefusalCodeV1::SegmentMissing => Self::SegmentMissing,
            WorkspaceRefusalCodeV1::ChecksumMismatch => Self::ChecksumMismatch,
            WorkspaceRefusalCodeV1::UnsupportedObjectFormat => Self::UnsupportedObjectFormat,
            WorkspaceRefusalCodeV1::SegmentTooLarge => Self::SegmentTooLarge,
            WorkspaceRefusalCodeV1::QuotaExceeded => Self::QuotaExceeded,
            WorkspaceRefusalCodeV1::NoForgeCredential => Self::NoForgeCredential,
            WorkspaceRefusalCodeV1::PullRequestsDisabled => Self::PullRequestsDisabled,
            WorkspaceRefusalCodeV1::Unspecified => return None,
        })
    }
}

/// Why a store call did not give a result. Data-free: no body, sha or
/// path is carried into a message.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum StoreCallError {
    /// Main refused the request on its content.
    Refused {
        code: RefusalCode,
        current: BTreeMap<String, String>,
    },
    /// Main rejected the call (authentication, fence, availability).
    Rejected { code: i32, retryable: bool },
    /// The call did not complete (connection, deadline).
    Transport { retryable: bool },
    /// Main answered `ABORTED` (`thread_busy`): another change to the thread
    /// was being saved. Not a ref conflict; the same publish is resent.
    Busy,
    /// Main answered something this worker cannot accept.
    Malformed(&'static str),
}

impl StoreCallError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::Refused { code, .. } => code.as_str(),
            Self::Rejected { .. } => "workspace_store.rejected",
            Self::Transport { .. } => "workspace_store.unavailable",
            Self::Busy => "workspace_store.thread_busy",
            Self::Malformed(_) => "workspace_store.malformed_response",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        match self {
            Self::Rejected { retryable, .. } | Self::Transport { retryable } => *retryable,
            Self::Busy => true,
            Self::Refused { .. } | Self::Malformed(_) => false,
        }
    }
}

/// One planned segment.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PlannedSegment {
    pub(crate) seq: u64,
    /// [`KIND_COMPACT`] or [`KIND_PUSH`].
    pub(crate) kind: String,
    pub(crate) sha256: String,
    pub(crate) bytes: u64,
    /// A `PUSH`'s prerequisite commits; empty for a `COMPACT`.
    pub(crate) prerequisites: Vec<String>,
    /// The thread's refs after this segment.
    pub(crate) refs: BTreeMap<String, String>,
}

/// One fetched segment: its plan entry and the file it was streamed to.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FetchedSegment {
    pub(crate) planned: PlannedSegment,
    pub(crate) path: std::path::PathBuf,
}

/// The fetched thread: its plan, the live chain (a `COMPACT`, then the
/// `PUSH` segments after it, ADR-0034 Phase 2); each segment was streamed
/// to its own file in the directory the caller named, and checked against
/// the plan on the way.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FetchedThread {
    pub(crate) head_seq: u64,
    pub(crate) refs: BTreeMap<String, String>,
    pub(crate) segments: Vec<FetchedSegment>,
}

/// The executor's publish: one segment (a `COMPACT`, or a `PUSH` over the
/// published head), one move of the cloud ref.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PublishSegment {
    /// [`KIND_COMPACT`] or [`KIND_PUSH`].
    pub(crate) kind: &'static str,
    pub(crate) segment_sha256: String,
    pub(crate) bytes: u64,
    /// A `PUSH`'s prerequisites (published commits); empty for a `COMPACT`.
    pub(crate) prerequisites: Vec<String>,
    pub(crate) old_cloud: Option<String>,
    pub(crate) new_cloud: String,
}

/// The applied publish.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PublishedSegment {
    pub(crate) seq: u64,
    pub(crate) refs: BTreeMap<String, String>,
}

/// The store calls, as the orchestrator uses them.
#[async_trait]
pub(crate) trait WorkspaceStore: Send + Sync {
    /// Fetch the plan and stream its chain into files in the directory
    /// `into`, one per segment, never holding more than one chunk.
    ///
    /// A turn hydrates from nothing, so it always asks with `since_seq` 0
    /// and the plan starts with the chain's `COMPACT`. Main answers a
    /// `since_seq` at or past that `COMPACT`'s seq with only the later
    /// `PUSH` segments (`workspacestore.SelectChain`), which the plan check
    /// here refuses: there is no incremental reader.
    async fn fetch(
        &self,
        claim: &WorkspaceClaimV1,
        since_seq: u64,
        max_segment_bytes: u64,
        into: &Path,
    ) -> Result<FetchedThread, StoreCallError>;

    /// Upload the segment file `segment` (`bytes` long), read in chunks;
    /// `true` when Main already had that digest.
    async fn upload(
        &self,
        claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: u64,
        segment: &Path,
    ) -> Result<bool, StoreCallError>;

    async fn publish(
        &self,
        claim: &WorkspaceClaimV1,
        request: &PublishSegment,
    ) -> Result<PublishedSegment, StoreCallError>;

    /// The thread's harvested env (contract 6.4); empty when it has none.
    async fn env_secret(&self, claim: &WorkspaceClaimV1) -> Result<SecretEnv, StoreCallError>;

    /// The thread's pull-request credential (ADR-0034 decision 10,
    /// amendment 2026-10-26): the GitHub configuration's data as JSON text,
    /// zeroized on drop. Redeemed only when `open_pull_request` runs.
    async fn forge_credential(
        &self,
        _claim: &WorkspaceClaimV1,
    ) -> Result<zeroize::Zeroizing<String>, StoreCallError> {
        Err(StoreCallError::Malformed(
            "this store serves no forge credential",
        ))
    }

    /// Record one executor audit event of the running turn (decision 12).
    async fn record_event(
        &self,
        claim: &WorkspaceClaimV1,
        event: &super::audit::ExecutorEvent,
    ) -> Result<(), StoreCallError>;

    /// Record the files the turn's published commit changed (the web
    /// "Files changed" view). Best effort for the caller; a store that
    /// keeps no record answers `Ok`.
    async fn record_changes(
        &self,
        _claim: &WorkspaceClaimV1,
        _changes: &super::changes::ChangeSet,
    ) -> Result<(), StoreCallError> {
        Ok(())
    }
}

/// The wire request of an executor audit event: counts, rule ids, the file
/// count; no path field exists.
#[must_use]
pub(crate) fn record_event_request(
    claim: &WorkspaceClaimV1,
    event: &super::audit::ExecutorEvent,
) -> WorkspaceRecordEventRequestV1 {
    use super::audit::ExecutorEvent;
    let mut request = WorkspaceRecordEventRequestV1 {
        claim: Some(claim.clone()),
        ..WorkspaceRecordEventRequestV1::default()
    };
    match event {
        ExecutorEvent::ScanBlocked { findings, rules } => {
            request.kind = WorkspaceExecutorEventKindV1::ScanBlocked as i32;
            request.findings = *findings;
            request.rules.clone_from(rules);
        }
        ExecutorEvent::PullRequestOpened {
            toolkit,
            branch,
            base,
            number,
            url,
            files,
        } => {
            request.kind = WorkspaceExecutorEventKindV1::PullRequestOpened as i32;
            request.toolkit.clone_from(toolkit);
            request.branch.clone_from(branch);
            request.base.clone_from(base);
            request.number = *number;
            request.url = url.clone().unwrap_or_default();
            request.files = *files;
        }
    }
    request
}

/// Read a record-event response.
///
/// # Errors
///
/// A refusal or rejection, or a response that records nothing.
pub(crate) fn read_record_event(
    response: &WorkspaceRecordEventResponseV1,
) -> Result<(), StoreCallError> {
    match (&response.refusal, &response.rejection, response.recorded) {
        (Some(refusal), None, false) => Err(refused(refusal)),
        (None, Some(rejection), false) => Err(rejected(rejection)),
        (None, None, true) => Ok(()),
        _ => Err(StoreCallError::Malformed(
            "the record-event response is ambiguous",
        )),
    }
}

/// The contract's env limits.
pub(crate) const MAX_ENV_VARS: usize = 200;
pub(crate) const MAX_ENV_BYTES: usize = 64 * 1_024;

/// Read an env stream: frames of variables, then the end count.
///
/// # Errors
///
/// A refusal or rejection, a malformed name, or the limits exceeded.
pub(crate) fn read_env_frames(
    frames: impl IntoIterator<Item = WorkspaceEnvSecretFrameV1>,
) -> Result<SecretEnv, StoreCallError> {
    use workspace_env_secret_frame_v1::Frame;
    let mut vars: Vec<(String, String)> = Vec::new();
    let mut bytes = 0_usize;
    let mut frames = frames.into_iter();
    for frame in frames.by_ref() {
        match frame.frame {
            Some(Frame::Vars(batch)) => {
                for entry in batch.vars {
                    bytes += entry.name.len() + entry.value.len();
                    let valid_name = entry
                        .name
                        .bytes()
                        .next()
                        .is_some_and(|first| first.is_ascii_alphabetic() || first == b'_')
                        && entry
                            .name
                            .bytes()
                            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_');
                    if !valid_name
                        || entry.value.contains('\0')
                        || vars.iter().any(|(name, _)| *name == entry.name)
                    {
                        return Err(StoreCallError::Malformed("an env variable is malformed"));
                    }
                    vars.push((entry.name, entry.value));
                    if vars.len() > MAX_ENV_VARS || bytes > MAX_ENV_BYTES {
                        return Err(StoreCallError::Malformed("the env is over its limits"));
                    }
                }
            }
            Some(Frame::EndCount(count)) => {
                if count as usize != vars.len() || frames.next().is_some() {
                    return Err(StoreCallError::Malformed(
                        "the env stream does not match its count",
                    ));
                }
                return Ok(SecretEnv::new(vars));
            }
            Some(Frame::Refusal(refusal)) if vars.is_empty() => return Err(refused(&refusal)),
            Some(Frame::Rejection(rejection)) if vars.is_empty() => {
                return Err(rejected(&rejection));
            }
            _ => return Err(StoreCallError::Malformed("the env stream is malformed")),
        }
    }
    Err(StoreCallError::Malformed("the env stream ended early"))
}

pub(crate) fn sha256_hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    let digest = Sha256::digest(bytes);
    let mut out = String::with_capacity(64);
    for byte in digest {
        let _ = write!(out, "{byte:02x}");
    }
    out
}

fn sha256_valid(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn ref_map(refs: &[WorkspaceRefV1]) -> Result<BTreeMap<String, String>, StoreCallError> {
    let mut map = BTreeMap::new();
    for reference in refs {
        if !reference.name.starts_with("refs/elitea/")
            || reference.name.len() > 255
            || reference.name.contains("..")
            || !sha1_hex(&reference.sha)
            || map
                .insert(reference.name.clone(), reference.sha.clone())
                .is_some()
        {
            return Err(StoreCallError::Malformed("a workspace ref is malformed"));
        }
    }
    Ok(map)
}

pub(crate) fn refused(refusal: &WorkspaceRefusalV1) -> StoreCallError {
    match RefusalCode::from_wire(refusal.code) {
        Some(code) => StoreCallError::Refused {
            code,
            current: ref_map(&refusal.current).unwrap_or_default(),
        },
        None => StoreCallError::Malformed("the workspace refusal is unknown"),
    }
}

/// The most bytes of a forge credential (Main's
/// `workspaceruntime.MaxForgeCredentialBytes`).
pub(crate) const MAX_FORGE_CREDENTIAL_BYTES: usize = 32 * 1_024;

/// Reads a `WorkspaceForgeCredential` answer: exactly one of a JSON object,
/// a refusal or a rejection. The text is moved into zeroizing storage; the
/// response's own copy is wiped by the caller dropping it.
pub(crate) fn read_forge_credential(
    response: &mut WorkspaceForgeCredentialResponseV1,
) -> Result<zeroize::Zeroizing<String>, StoreCallError> {
    let text = zeroize::Zeroizing::new(std::mem::take(&mut response.github_configuration_json));
    match (&response.refusal, &response.rejection, text.is_empty()) {
        (Some(refusal), None, true) => Err(refused(refusal)),
        (None, Some(rejection), true) => Err(rejected(rejection)),
        (None, None, false)
            if text.len() <= MAX_FORGE_CREDENTIAL_BYTES && text.starts_with('{') =>
        {
            Ok(text)
        }
        _ => Err(StoreCallError::Malformed(
            "the forge credential answer is malformed",
        )),
    }
}

pub(crate) fn rejected(rejection: &RuntimeErrorV1) -> StoreCallError {
    let code = RuntimeErrorCodeV1::try_from(rejection.code)
        .map_or(RuntimeErrorCodeV1::Unspecified as i32, |code| code as i32);
    StoreCallError::Rejected {
        code,
        retryable: rejection.retryable,
    }
}

/// Reads a fetch stream frame by frame: exactly one plan (a chain: a
/// `COMPACT`, then up to [`MAX_PLAN_SEGMENTS`] − 1 `PUSH` segments), then
/// each segment's chunks in plan order (written straight to that segment's
/// file, hashed as they pass) and its end frame, matched against the plan.
/// At most one chunk is in memory.
pub(crate) struct FetchSink {
    dir: std::path::PathBuf,
    max_segment_bytes: u64,
    plan: Option<(u64, BTreeMap<String, String>, Vec<PlannedSegment>)>,
    file: Option<std::io::BufWriter<std::fs::File>>,
    hasher: Sha256,
    written: u64,
    fetched: Vec<FetchedSegment>,
}

/// The plan's segments, checked for shape: ascending `seq`, a `COMPACT`
/// with no prerequisites first, then `PUSH` segments with 1 to
/// [`MAX_PREREQUISITES`] prerequisites; the last is the head and holds the
/// plan's refs. Each segment is within `max_segment_bytes` (a turn passes
/// [`MAIN_MAX_SEGMENT_BYTES`]), and the chain within twice that.
fn planned_chain(
    plan: &crate::protocol::elitea::runtime::v1::WorkspaceFetchPlanV1,
    refs: &BTreeMap<String, String>,
    max_segment_bytes: u64,
) -> Result<Vec<PlannedSegment>, StoreCallError> {
    let shape = StoreCallError::Malformed("the fetch plan is not a segment chain");
    if plan.segments.is_empty() || plan.segments.len() > MAX_PLAN_SEGMENTS {
        return Err(shape);
    }
    let mut chain = Vec::with_capacity(plan.segments.len());
    let mut previous = 0;
    let mut total: u64 = 0;
    for (index, segment) in plan.segments.iter().enumerate() {
        let kind_ok = if index == 0 {
            segment.kind == KIND_COMPACT && segment.prerequisites.is_empty()
        } else {
            segment.kind == KIND_PUSH
                && !segment.prerequisites.is_empty()
                && segment.prerequisites.len() <= MAX_PREREQUISITES
                && segment.prerequisites.iter().all(|sha| sha1_hex(sha))
        };
        total = total.saturating_add(segment.bytes);
        if !kind_ok
            || segment.seq <= previous
            || !sha256_valid(&segment.sha256)
            || segment.bytes == 0
            || segment.bytes > max_segment_bytes
            || total > max_segment_bytes.saturating_mul(2)
        {
            return Err(shape);
        }
        previous = segment.seq;
        chain.push(PlannedSegment {
            seq: segment.seq,
            kind: segment.kind.clone(),
            sha256: segment.sha256.clone(),
            bytes: segment.bytes,
            prerequisites: segment.prerequisites.clone(),
            refs: ref_map(&segment.refs)?,
        });
    }
    let last = &chain[chain.len() - 1];
    if last.seq != plan.head_seq || &last.refs != refs {
        return Err(shape);
    }
    Ok(chain)
}

impl FetchSink {
    /// Segment `index`'s file in `dir`.
    #[must_use]
    pub(crate) fn segment_path(dir: &Path, index: usize) -> std::path::PathBuf {
        dir.join(format!("segment-{index:02}.bundle"))
    }

    /// # Errors
    ///
    /// The directory cannot be created.
    pub(crate) fn create(into: &Path, max_segment_bytes: u64) -> Result<Self, StoreCallError> {
        std::fs::create_dir_all(into)
            .map_err(|_| StoreCallError::Malformed("the segment file cannot be written"))?;
        Ok(Self {
            dir: into.to_path_buf(),
            max_segment_bytes,
            plan: None,
            file: None,
            hasher: Sha256::new(),
            written: 0,
            fetched: Vec::new(),
        })
    }

    fn open_next(&mut self) -> Result<(), StoreCallError> {
        let path = Self::segment_path(&self.dir, self.fetched.len());
        let file = std::fs::File::create(&path)
            .map_err(|_| StoreCallError::Malformed("the segment file cannot be written"))?;
        self.file = Some(std::io::BufWriter::with_capacity(CHUNK_BYTES * 2, file));
        self.hasher = Sha256::new();
        self.written = 0;
        Ok(())
    }

    /// Take one frame.
    ///
    /// # Errors
    ///
    /// A refusal or rejection frame, or any frame out of order.
    pub(crate) fn push(&mut self, frame: WorkspaceFetchFrameV1) -> Result<(), StoreCallError> {
        use std::io::Write as _;
        use workspace_fetch_frame_v1::Frame;
        let Some(segments) = self.plan.as_ref().map(|(_, _, chain)| chain.len()) else {
            return match frame.frame {
                Some(Frame::Plan(plan)) => {
                    let refs = ref_map(&plan.refs)?;
                    let chain = planned_chain(&plan, &refs, self.max_segment_bytes)?;
                    self.plan = Some((plan.head_seq, refs, chain));
                    self.open_next()
                }
                Some(Frame::Refusal(refusal)) => Err(refused(&refusal)),
                Some(Frame::Rejection(rejection)) => Err(rejected(&rejection)),
                _ => Err(StoreCallError::Malformed("the fetch stream has no plan")),
            };
        };
        let Some(planned) = self
            .plan
            .as_ref()
            .and_then(|(_, _, chain)| chain.get(self.fetched.len()).cloned())
        else {
            return Err(StoreCallError::Malformed(
                "the fetch stream continues past its segments",
            ));
        };
        let broken = || StoreCallError::Malformed("the segment file cannot be written");
        match frame.frame {
            Some(Frame::SegmentChunk(chunk)) => {
                if chunk.is_empty()
                    || chunk.len() > CHUNK_BYTES
                    || self.written + chunk.len() as u64 > planned.bytes
                {
                    return Err(StoreCallError::Malformed("a segment chunk is malformed"));
                }
                self.file
                    .as_mut()
                    .ok_or_else(broken)?
                    .write_all(&chunk)
                    .map_err(|_| broken())?;
                self.hasher.update(&chunk);
                self.written += chunk.len() as u64;
                Ok(())
            }
            Some(Frame::SegmentEnd(end)) => {
                let digest = hex_digest(std::mem::take(&mut self.hasher));
                if end.sha256 != planned.sha256
                    || end.bytes != planned.bytes
                    || self.written != planned.bytes
                    || digest != planned.sha256
                {
                    return Err(StoreCallError::Refused {
                        code: RefusalCode::ChecksumMismatch,
                        current: BTreeMap::new(),
                    });
                }
                self.file
                    .take()
                    .ok_or_else(broken)?
                    .flush()
                    .map_err(|_| broken())?;
                let path = Self::segment_path(&self.dir, self.fetched.len());
                self.fetched.push(FetchedSegment { planned, path });
                if self.fetched.len() < segments {
                    self.open_next()?;
                }
                Ok(())
            }
            Some(Frame::Refusal(refusal)) => Err(refused(&refusal)),
            Some(Frame::Rejection(rejection)) => Err(rejected(&rejection)),
            Some(Frame::Plan(_)) | None => Err(StoreCallError::Malformed(
                "the fetch stream is out of order",
            )),
        }
    }

    /// The stream ended.
    ///
    /// # Errors
    ///
    /// It ended before the last segment's end frame.
    pub(crate) fn finish(self) -> Result<FetchedThread, StoreCallError> {
        match self.plan {
            Some((head_seq, refs, chain)) if chain.len() == self.fetched.len() => {
                Ok(FetchedThread {
                    head_seq,
                    refs,
                    segments: self.fetched,
                })
            }
            _ => Err(StoreCallError::Malformed("the fetch stream ended early")),
        }
    }
}

fn hex_digest(hasher: Sha256) -> String {
    use std::fmt::Write as _;
    let mut out = String::with_capacity(64);
    for byte in hasher.finalize() {
        let _ = write!(out, "{byte:02x}");
    }
    out
}

/// Read a whole fetch stream into segment files in the directory `into`.
#[cfg(test)]
///
/// # Errors
///
/// As [`FetchSink::push`].
pub(crate) fn read_fetch_frames(
    frames: impl IntoIterator<Item = WorkspaceFetchFrameV1>,
    max_segment_bytes: u64,
    into: &Path,
) -> Result<FetchedThread, StoreCallError> {
    let mut sink = FetchSink::create(into, max_segment_bytes)?;
    for frame in frames {
        sink.push(frame)?;
    }
    sink.finish()
}

/// The upload stream of one segment file: a header, then 32 KiB chunks read
/// from `source` one at a time, as the stream is polled. A read error or a
/// file that is not `bytes` long ends the stream early, which Main refuses
/// (its byte count does not match the header).
pub(crate) fn upload_stream<R>(
    claim: WorkspaceClaimV1,
    sha256: String,
    bytes: u64,
    mut source: R,
) -> impl tokio_stream::Stream<Item = WorkspaceUploadFrameV1> + Send
where
    R: tokio::io::AsyncRead + Unpin + Send + 'static,
{
    use tokio::io::AsyncReadExt as _;
    use workspace_upload_frame_v1::Frame;
    async_stream::stream! {
        yield WorkspaceUploadFrameV1 {
            frame: Some(Frame::Header(WorkspaceUploadHeaderV1 {
                claim: Some(claim),
                sha256,
                bytes,
            })),
        };
        let mut sent: u64 = 0;
        let mut buffer = vec![0_u8; CHUNK_BYTES];
        while sent < bytes {
            let mut filled = 0;
            while filled < CHUNK_BYTES {
                match source.read(&mut buffer[filled..]).await {
                    Ok(0) | Err(_) => break,
                    Ok(count) => filled += count,
                }
            }
            if filled == 0 {
                break;
            }
            sent += filled as u64;
            yield WorkspaceUploadFrameV1 {
                frame: Some(Frame::Chunk(buffer[..filled].to_vec())),
            };
        }
    }
}

fn read_upload(response: &WorkspaceUploadResponseV1) -> Result<bool, StoreCallError> {
    match (&response.refusal, &response.rejection) {
        (Some(refusal), None) if !response.exists => Err(refused(refusal)),
        (None, Some(rejection)) if !response.exists => Err(rejected(rejection)),
        (None, None) => Ok(response.exists),
        _ => Err(StoreCallError::Malformed(
            "the upload response is ambiguous",
        )),
    }
}

#[must_use]
pub(crate) fn publish_request(
    claim: &WorkspaceClaimV1,
    request: &PublishSegment,
) -> WorkspacePublishRequestV1 {
    WorkspacePublishRequestV1 {
        claim: Some(claim.clone()),
        kind: request.kind.to_owned(),
        segment_sha256: request.segment_sha256.clone(),
        bytes: request.bytes,
        prerequisites: request.prerequisites.clone(),
        refs: vec![WorkspaceRefUpdateV1 {
            name: REF_CLOUD.to_owned(),
            old: request.old_cloud.clone(),
            new: request.new_cloud.clone(),
        }],
        object_format: OBJECT_FORMAT_SHA1.to_owned(),
    }
}

/// Read a publish response: an applied publish must move the cloud ref to
/// exactly the requested commit.
///
/// # Errors
///
/// A refusal, a rejection, or an applied result that is not the request's.
pub(crate) fn read_publish(
    response: &WorkspacePublishResponseV1,
    request: &PublishSegment,
) -> Result<PublishedSegment, StoreCallError> {
    match (&response.refusal, &response.rejection) {
        (Some(refusal), None) => return Err(refused(refusal)),
        (None, Some(rejection)) => return Err(rejected(rejection)),
        (None, None) => {}
        _ => {
            return Err(StoreCallError::Malformed(
                "the publish response is ambiguous",
            ));
        }
    }
    let refs = ref_map(&response.refs)?;
    if response.seq == 0 || refs.get(REF_CLOUD) != Some(&request.new_cloud) {
        return Err(StoreCallError::Malformed(
            "the publish response does not move the cloud ref",
        ));
    }
    Ok(PublishedSegment {
        seq: response.seq,
        refs,
    })
}

/// How a publish that met `thread_busy` is resent: the first wait, the cap
/// of the doubling wait, and the number of attempts in all.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct BusyRetry {
    pub(crate) first: Duration,
    pub(crate) cap: Duration,
    pub(crate) attempts: u32,
}

impl Default for BusyRetry {
    /// Main's thread lock wait is 50 ms and a busy publish holds no
    /// connection, so short waits converge; nine attempts span about 8 s.
    fn default() -> Self {
        Self {
            first: Duration::from_millis(100),
            cap: Duration::from_secs(2),
            attempts: 9,
        }
    }
}

/// Resend `call` while it answers [`StoreCallError::Busy`], waiting `retry`'s
/// doubling backoff between attempts. Any other answer is returned as it is.
pub(crate) async fn retry_busy<T, F, Fut>(retry: BusyRetry, call: F) -> Result<T, StoreCallError>
where
    F: Fn() -> Fut,
    Fut: std::future::Future<Output = Result<T, StoreCallError>>,
{
    let mut wait = retry.first;
    let mut attempt = 1;
    loop {
        match call().await {
            Err(StoreCallError::Busy) if attempt < retry.attempts.max(1) => {
                tracing::debug!(
                    event = "workspace.publish_busy",
                    attempt,
                    "the thread was busy; the same publish is resent"
                );
                tokio::time::sleep(wait).await;
                wait = (wait * 2).min(retry.cap);
                attempt += 1;
            }
            other => return other,
        }
    }
}

/// The production store over the control channel.
pub(crate) struct GrpcWorkspaceStore {
    client: WorkspaceStoreServiceClient<Channel>,
    deadline: Duration,
    busy: BusyRetry,
}

impl GrpcWorkspaceStore {
    /// `deadline` bounds each whole call, segment transfer included.
    #[must_use]
    pub(crate) fn new(channel: Channel, deadline: Duration) -> Self {
        let client = WorkspaceStoreServiceClient::new(channel)
            .max_encoding_message_size(MAX_REQUEST_BYTES)
            .max_decoding_message_size(MAX_RESPONSE_BYTES);
        Self {
            client,
            deadline,
            busy: BusyRetry::default(),
        }
    }

    /// Replace the `thread_busy` backoff (tests).
    #[cfg(test)]
    #[must_use]
    pub(crate) const fn with_busy_retry(mut self, busy: BusyRetry) -> Self {
        self.busy = busy;
        self
    }
}

/// The status message of Main's `thread_busy` (`runtimegrpc/workspace`
/// `ThreadBusyMessage`).
const THREAD_BUSY_MESSAGE: &str = "thread_busy";

fn status_error(status: &tonic::Status) -> StoreCallError {
    if status.code() == tonic::Code::Aborted && status.message() == THREAD_BUSY_MESSAGE {
        // Main's thread_busy (a publish that met another holder of the
        // thread). Any other ABORTED is a transport error.
        return StoreCallError::Busy;
    }
    StoreCallError::Transport {
        retryable: matches!(
            status.code(),
            tonic::Code::Unavailable | tonic::Code::DeadlineExceeded | tonic::Code::Aborted
        ),
    }
}

#[async_trait]
impl WorkspaceStore for GrpcWorkspaceStore {
    async fn fetch(
        &self,
        claim: &WorkspaceClaimV1,
        since_seq: u64,
        max_segment_bytes: u64,
        into: &Path,
    ) -> Result<FetchedThread, StoreCallError> {
        let mut client = self.client.clone();
        let request = WorkspaceFetchPlanRequestV1 {
            claim: Some(claim.clone()),
            since_seq,
        };
        let mut sink = FetchSink::create(into, max_segment_bytes)?;
        let operation = async move {
            let mut stream = client
                .workspace_fetch_plan(request)
                .await
                .map_err(|status| status_error(&status))?
                .into_inner();
            // Each frame goes to disk as it arrives (the sink bounds the
            // segment by its plan, the plan by the policy's cap).
            while let Some(frame) = stream.next().await {
                sink.push(frame.map_err(|status| status_error(&status))?)?;
            }
            sink.finish()
        };
        timeout(self.deadline, operation)
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
    }

    async fn upload(
        &self,
        claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: u64,
        segment: &Path,
    ) -> Result<bool, StoreCallError> {
        let mut client = self.client.clone();
        let file = tokio::fs::File::open(segment)
            .await
            .map_err(|_| StoreCallError::Malformed("the segment file cannot be read"))?;
        let frames = upload_stream(claim.clone(), sha256.to_owned(), bytes, file);
        let operation = async move {
            let response = client
                .workspace_upload(frames)
                .await
                .map_err(|status| status_error(&status))?
                .into_inner();
            read_upload(&response)
        };
        timeout(self.deadline, operation)
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
    }

    async fn publish(
        &self,
        claim: &WorkspaceClaimV1,
        request: &PublishSegment,
    ) -> Result<PublishedSegment, StoreCallError> {
        let wire = publish_request(claim, request);
        retry_busy(self.busy, || {
            let mut client = self.client.clone();
            let wire = wire.clone();
            async move {
                let response = timeout(self.deadline, client.workspace_publish(wire))
                    .await
                    .map_err(|_| StoreCallError::Transport { retryable: true })?
                    .map_err(|status| status_error(&status))?
                    .into_inner();
                read_publish(&response, request)
            }
        })
        .await
    }

    async fn env_secret(&self, claim: &WorkspaceClaimV1) -> Result<SecretEnv, StoreCallError> {
        let mut client = self.client.clone();
        let request = WorkspaceEnvSecretRequestV1 {
            claim: Some(claim.clone()),
        };
        let operation = async move {
            let mut stream = client
                .workspace_env_secret(request)
                .await
                .map_err(|status| status_error(&status))?
                .into_inner();
            let mut frames = Vec::new();
            while let Some(frame) = stream.next().await {
                frames.push(frame.map_err(|status| status_error(&status))?);
                if frames.len() > MAX_ENV_VARS + 2 {
                    return Err(StoreCallError::Malformed("the env stream is too long"));
                }
            }
            read_env_frames(frames)
        };
        timeout(self.deadline, operation)
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
    }

    async fn forge_credential(
        &self,
        claim: &WorkspaceClaimV1,
    ) -> Result<zeroize::Zeroizing<String>, StoreCallError> {
        let mut client = self.client.clone();
        let request = WorkspaceForgeCredentialRequestV1 {
            claim: Some(claim.clone()),
        };
        let mut response = timeout(self.deadline, client.workspace_forge_credential(request))
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
            .map_err(|status| status_error(&status))?
            .into_inner();
        read_forge_credential(&mut response)
    }

    async fn record_event(
        &self,
        claim: &WorkspaceClaimV1,
        event: &super::audit::ExecutorEvent,
    ) -> Result<(), StoreCallError> {
        let mut client = self.client.clone();
        let response = timeout(
            self.deadline,
            client.workspace_record_event(record_event_request(claim, event)),
        )
        .await
        .map_err(|_| StoreCallError::Transport { retryable: true })?
        .map_err(|status| status_error(&status))?
        .into_inner();
        read_record_event(&response)
    }

    async fn record_changes(
        &self,
        claim: &WorkspaceClaimV1,
        changes: &super::changes::ChangeSet,
    ) -> Result<(), StoreCallError> {
        let mut client = self.client.clone();
        let frames = tokio_stream::iter(super::changes::frames(claim, changes));
        let response = timeout(self.deadline, client.workspace_record_changes(frames))
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
            .map_err(|status| status_error(&status))?
            .into_inner();
        super::changes::read_record_changes(&response)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        WorkspaceFetchPlanV1, WorkspaceSegmentEndV1, WorkspaceSegmentV1,
    };

    const CLOUD: &str = "1111111111111111111111111111111111111111";

    fn cloud_refs(sha: &str) -> Vec<WorkspaceRefV1> {
        vec![WorkspaceRefV1 {
            name: REF_CLOUD.to_owned(),
            sha: sha.to_owned(),
        }]
    }

    /// One planned segment of a chain test: (seq, kind, bytes, prereqs).
    fn wire_segment(
        seq: u64,
        kind: &str,
        bytes: &[u8],
        prerequisites: &[&str],
        refs: Vec<WorkspaceRefV1>,
    ) -> WorkspaceSegmentV1 {
        WorkspaceSegmentV1 {
            seq,
            kind: kind.to_owned(),
            sha256: sha256_hex(bytes),
            bytes: bytes.len() as u64,
            prerequisites: prerequisites.iter().map(|p| (*p).to_owned()).collect(),
            refs,
        }
    }

    /// The frames of a plan and its segments' bodies, in plan order.
    fn chain_frames(
        plan: WorkspaceFetchPlanV1,
        bodies: &[&[u8]],
        corrupt_end: bool,
    ) -> Vec<WorkspaceFetchFrameV1> {
        use workspace_fetch_frame_v1::Frame;
        let mut frames = vec![WorkspaceFetchFrameV1 {
            frame: Some(Frame::Plan(plan)),
        }];
        for body in bodies {
            frames.extend(body.chunks(CHUNK_BYTES).map(|chunk| WorkspaceFetchFrameV1 {
                frame: Some(Frame::SegmentChunk(chunk.to_vec())),
            }));
            frames.push(WorkspaceFetchFrameV1 {
                frame: Some(Frame::SegmentEnd(WorkspaceSegmentEndV1 {
                    sha256: if corrupt_end {
                        "0".repeat(64)
                    } else {
                        sha256_hex(body)
                    },
                    bytes: body.len() as u64,
                })),
            });
        }
        frames
    }

    fn plan_frames(segment: &[u8], corrupt_end: bool) -> Vec<WorkspaceFetchFrameV1> {
        let plan = WorkspaceFetchPlanV1 {
            head_seq: 3,
            refs: cloud_refs(CLOUD),
            segments: vec![wire_segment(
                3,
                KIND_COMPACT,
                segment,
                &[],
                cloud_refs(CLOUD),
            )],
        };
        chain_frames(plan, &[segment], corrupt_end)
    }

    #[test]
    fn a_fetch_stream_is_read_and_checked_against_its_plan() {
        let dir = tempfile::tempdir().expect("dir");
        let into = dir.path().join("fetched");
        let segment = vec![7_u8; 100_000];
        let fetched =
            read_fetch_frames(plan_frames(&segment, false), 1 << 20, &into).expect("fetched");
        assert_eq!(fetched.segments.len(), 1);
        assert_eq!(
            std::fs::read(&fetched.segments[0].path).expect("file"),
            segment
        );
        assert_eq!(fetched.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));

        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, true), 1 << 20, &into),
            Err(StoreCallError::Refused {
                code: RefusalCode::ChecksumMismatch,
                ..
            })
        ));
        // Over the policy's segment cap: refused before any byte is used.
        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, false), 1_000, &into),
            Err(StoreCallError::Malformed(_))
        ));
        // A truncated stream is never a usable segment.
        let mut truncated = plan_frames(&segment, false);
        truncated.pop();
        assert!(read_fetch_frames(truncated, 1 << 20, &into).is_err());
    }

    /// Phase 2: a chain (a COMPACT, then PUSH segments) streams to one file
    /// per segment; a plan out of shape is refused before any byte is used.
    #[test]
    #[allow(
        clippy::too_many_lines,
        reason = "one chain and every shape it must not have"
    )]
    fn a_chain_is_fetched_segment_by_segment_and_its_shape_is_checked() {
        let dir = tempfile::tempdir().expect("dir");
        let into = dir.path().join("fetched");
        let compact = vec![1_u8; 40_000];
        let push = vec![2_u8; 50_000];
        let next = "2222222222222222222222222222222222222222";
        let plan = |segments: Vec<WorkspaceSegmentV1>, head_seq: u64| WorkspaceFetchPlanV1 {
            head_seq,
            refs: cloud_refs(next),
            segments,
        };
        let good = vec![
            wire_segment(3, KIND_COMPACT, &compact, &[], cloud_refs(CLOUD)),
            wire_segment(5, KIND_PUSH, &push, &[CLOUD], cloud_refs(next)),
        ];
        let fetched = read_fetch_frames(
            chain_frames(plan(good.clone(), 5), &[&compact, &push], false),
            1 << 20,
            &into,
        )
        .expect("fetched");
        assert_eq!(fetched.head_seq, 5);
        assert_eq!(fetched.segments.len(), 2);
        assert_eq!(fetched.segments[1].planned.kind, KIND_PUSH);
        assert_eq!(fetched.segments[1].planned.prerequisites, vec![CLOUD]);
        assert_eq!(
            std::fs::read(&fetched.segments[0].path).expect("c"),
            compact
        );
        assert_eq!(std::fs::read(&fetched.segments[1].path).expect("p"), push);

        // The stream ends after the COMPACT: never a usable chain.
        let mut short = chain_frames(plan(good.clone(), 5), &[&compact, &push], false);
        short.truncate(short.len() - 3);
        assert!(read_fetch_frames(short, 1 << 20, &into).is_err());

        let shapes: Vec<(&str, Vec<WorkspaceSegmentV1>, u64)> = vec![
            (
                "a PUSH first",
                vec![wire_segment(
                    5,
                    KIND_PUSH,
                    &push,
                    &[CLOUD],
                    cloud_refs(next),
                )],
                5,
            ),
            (
                "a PUSH without prerequisites",
                vec![
                    good[0].clone(),
                    wire_segment(5, KIND_PUSH, &push, &[], cloud_refs(next)),
                ],
                5,
            ),
            (
                "a malformed prerequisite",
                vec![
                    good[0].clone(),
                    wire_segment(5, KIND_PUSH, &push, &["zz"], cloud_refs(next)),
                ],
                5,
            ),
            (
                "out of seq order",
                vec![
                    good[0].clone(),
                    wire_segment(2, KIND_PUSH, &push, &[CLOUD], cloud_refs(next)),
                ],
                2,
            ),
            (
                "a second COMPACT",
                vec![
                    good[0].clone(),
                    wire_segment(5, KIND_COMPACT, &push, &[], cloud_refs(next)),
                ],
                5,
            ),
            (
                "an unknown kind",
                vec![
                    good[0].clone(),
                    wire_segment(5, "DELTA", &push, &[CLOUD], cloud_refs(next)),
                ],
                5,
            ),
            ("the head is not the last segment", good.clone(), 6),
            (
                "the last segment's refs are not the plan's",
                vec![
                    good[0].clone(),
                    wire_segment(5, KIND_PUSH, &push, &[CLOUD], cloud_refs(CLOUD)),
                ],
                5,
            ),
        ];
        for (name, segments, head) in shapes {
            assert!(
                matches!(
                    read_fetch_frames(
                        chain_frames(plan(segments, head), &[&compact, &push], false),
                        1 << 20,
                        &into,
                    ),
                    Err(StoreCallError::Malformed(_))
                ),
                "{name}"
            );
        }
        // Past Main's chain bound, or past twice the segment cap in all.
        let mut long = vec![good[0].clone()];
        for seq in 0..MAX_PLAN_SEGMENTS as u64 {
            long.push(wire_segment(
                10 + seq,
                KIND_PUSH,
                &push,
                &[CLOUD],
                cloud_refs(next),
            ));
        }
        let head = 10 + MAX_PLAN_SEGMENTS as u64 - 1;
        assert!(
            read_fetch_frames(chain_frames(plan(long, head), &[], false), 1 << 20, &into).is_err()
        );
        assert!(
            read_fetch_frames(
                chain_frames(plan(good, 5), &[&compact, &push], false),
                45_000,
                &into
            )
            .is_err(),
            "each segment fits 45 000 bytes, the chain does not fit 90 000"
        );
    }

    #[test]
    fn a_refused_fetch_reads_its_code() {
        let frame = WorkspaceFetchFrameV1 {
            frame: Some(workspace_fetch_frame_v1::Frame::Refusal(
                WorkspaceRefusalV1 {
                    code: WorkspaceRefusalCodeV1::StaleEpoch as i32,
                    current: Vec::new(),
                },
            )),
        };
        assert!(matches!(
            read_fetch_frames(
                [frame],
                1 << 20,
                &tempfile::tempdir().expect("dir").path().join("s")
            ),
            Err(StoreCallError::Refused {
                code: RefusalCode::StaleEpoch,
                ..
            })
        ));
    }

    /// A reader that panics on a read asking for more than one chunk, and
    /// counts how far it was read.
    struct ChunkedReads {
        data: Vec<u8>,
        offset: std::sync::Arc<std::sync::atomic::AtomicUsize>,
    }

    impl tokio::io::AsyncRead for ChunkedReads {
        fn poll_read(
            self: std::pin::Pin<&mut Self>,
            _: &mut std::task::Context<'_>,
            buf: &mut tokio::io::ReadBuf<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            assert!(buf.remaining() <= CHUNK_BYTES, "a whole-file read");
            let offset = self.offset.load(std::sync::atomic::Ordering::SeqCst);
            let count = buf.remaining().min(self.data.len() - offset);
            buf.put_slice(&self.data[offset..offset + count]);
            self.offset
                .store(offset + count, std::sync::atomic::Ordering::SeqCst);
            std::task::Poll::Ready(Ok(()))
        }
    }

    /// Item 4: the upload reads its file one chunk per frame, lazily, and
    /// every frame fits the control listener's bound.
    #[tokio::test]
    async fn the_upload_streams_its_file_in_bounded_chunks() {
        let bytes = vec![1_u8; 3 * CHUNK_BYTES + 5];
        let offset = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let source = ChunkedReads {
            data: bytes.clone(),
            offset: std::sync::Arc::clone(&offset),
        };
        let stream = upload_stream(
            WorkspaceClaimV1::default(),
            sha256_hex(&bytes),
            bytes.len() as u64,
            source,
        );
        tokio::pin!(stream);
        let header = stream.next().await.expect("header");
        assert!(prost::Message::encoded_len(&header) < MAX_REQUEST_BYTES);
        assert_eq!(
            offset.load(std::sync::atomic::Ordering::SeqCst),
            0,
            "nothing is read before a chunk is asked for"
        );
        let first = stream.next().await.expect("chunk");
        assert_eq!(
            offset.load(std::sync::atomic::Ordering::SeqCst),
            CHUNK_BYTES
        );
        let mut frames = vec![header, first];
        while let Some(frame) = stream.next().await {
            frames.push(frame);
        }
        assert_eq!(frames.len(), 5);
        let mut sent = Vec::new();
        for frame in &frames {
            assert!(prost::Message::encoded_len(frame) < MAX_REQUEST_BYTES);
            if let Some(workspace_upload_frame_v1::Frame::Chunk(chunk)) = &frame.frame {
                sent.extend_from_slice(chunk);
            }
        }
        assert_eq!(sent, bytes);
    }

    #[test]
    fn a_publish_must_move_the_cloud_ref_to_the_requested_commit() {
        let push = PublishSegment {
            kind: KIND_PUSH,
            segment_sha256: "a".repeat(64),
            bytes: 10,
            prerequisites: vec![CLOUD.to_owned()],
            old_cloud: Some(CLOUD.to_owned()),
            new_cloud: "2".repeat(40),
        };
        let wire = publish_request(&WorkspaceClaimV1::default(), &push);
        assert_eq!(wire.kind, KIND_PUSH);
        assert_eq!(wire.prerequisites, vec![CLOUD.to_owned()]);
        let request = PublishSegment {
            kind: KIND_COMPACT,
            segment_sha256: "a".repeat(64),
            bytes: 10,
            prerequisites: Vec::new(),
            old_cloud: Some(CLOUD.to_owned()),
            new_cloud: "2".repeat(40),
        };
        let wire = publish_request(&WorkspaceClaimV1::default(), &request);
        assert_eq!(wire.refs.len(), 1);
        assert_eq!(wire.refs[0].name, REF_CLOUD);
        assert_eq!(wire.refs[0].old.as_deref(), Some(CLOUD));
        let applied = WorkspacePublishResponseV1 {
            seq: 4,
            refs: vec![WorkspaceRefV1 {
                name: REF_CLOUD.to_owned(),
                sha: "2".repeat(40),
            }],
            ..Default::default()
        };
        assert_eq!(read_publish(&applied, &request).expect("applied").seq, 4);
        let wrong = WorkspacePublishResponseV1 {
            seq: 4,
            refs: vec![WorkspaceRefV1 {
                name: REF_CLOUD.to_owned(),
                sha: CLOUD.to_owned(),
            }],
            ..Default::default()
        };
        assert!(read_publish(&wrong, &request).is_err());
    }

    #[test]
    fn an_env_stream_is_read_strictly() {
        use crate::protocol::elitea::runtime::v1::{WorkspaceEnvVarV1, WorkspaceEnvVarsV1};
        use workspace_env_secret_frame_v1::Frame;
        let vars = |pairs: &[(&str, &str)]| WorkspaceEnvSecretFrameV1 {
            frame: Some(Frame::Vars(WorkspaceEnvVarsV1 {
                vars: pairs
                    .iter()
                    .map(|(name, value)| WorkspaceEnvVarV1 {
                        name: (*name).to_owned(),
                        value: (*value).to_owned(),
                    })
                    .collect(),
            })),
        };
        let end = |count| WorkspaceEnvSecretFrameV1 {
            frame: Some(Frame::EndCount(count)),
        };
        let env = read_env_frames([vars(&[("A_B", "value-1")]), vars(&[("C", "x")]), end(2)])
            .expect("env");
        assert_eq!(env.len(), 2);
        assert!(read_env_frames([end(0)]).expect("none").is_empty());
        assert!(read_env_frames([vars(&[("A", "1")]), end(2)]).is_err());
        assert!(read_env_frames([vars(&[("bad name", "1")]), end(1)]).is_err());
        assert!(read_env_frames([vars(&[("A", "1"), ("A", "2")]), end(2)]).is_err());
        assert!(read_env_frames([vars(&[("A", "1")])]).is_err());
    }

    const QUICK: BusyRetry = BusyRetry {
        first: Duration::from_millis(1),
        cap: Duration::from_millis(4),
        attempts: 5,
    };

    #[test]
    fn a_forge_credential_answer_is_exactly_one_of_its_three_forms() {
        use crate::protocol::elitea::runtime::v1::WorkspaceForgeCredentialResponseV1;
        let mut ok = WorkspaceForgeCredentialResponseV1 {
            github_configuration_json:
                r#"{"base_url":"https://api.github.com","access_token":"t"}"#.to_owned(),
            ..Default::default()
        };
        let text = read_forge_credential(&mut ok).expect("a credential");
        assert!(text.contains("access_token"));
        assert!(
            ok.github_configuration_json.is_empty(),
            "moved out of the response"
        );
        let mut none = WorkspaceForgeCredentialResponseV1 {
            refusal: Some(WorkspaceRefusalV1 {
                code: WorkspaceRefusalCodeV1::NoForgeCredential as i32,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(matches!(
            read_forge_credential(&mut none),
            Err(StoreCallError::Refused {
                code: RefusalCode::NoForgeCredential,
                ..
            })
        ));
        let mut off = WorkspaceForgeCredentialResponseV1 {
            refusal: Some(WorkspaceRefusalV1 {
                code: WorkspaceRefusalCodeV1::PullRequestsDisabled as i32,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert_eq!(
            read_forge_credential(&mut off)
                .err()
                .map(|error| error.code()),
            Some("pull_requests_disabled")
        );
        for mut malformed in [
            WorkspaceForgeCredentialResponseV1::default(),
            WorkspaceForgeCredentialResponseV1 {
                github_configuration_json: "[]".to_owned(),
                ..Default::default()
            },
            WorkspaceForgeCredentialResponseV1 {
                github_configuration_json: format!(
                    "{{{}}}",
                    " ".repeat(MAX_FORGE_CREDENTIAL_BYTES)
                ),
                ..Default::default()
            },
            WorkspaceForgeCredentialResponseV1 {
                github_configuration_json: "{}".to_owned(),
                refusal: Some(WorkspaceRefusalV1 {
                    code: WorkspaceRefusalCodeV1::NoForgeCredential as i32,
                    ..Default::default()
                }),
                ..Default::default()
            },
        ] {
            assert!(matches!(
                read_forge_credential(&mut malformed),
                Err(StoreCallError::Malformed(_))
            ));
        }
    }

    #[test]
    fn aborted_is_thread_busy_and_retryable_never_a_conflict() {
        let busy = status_error(&tonic::Status::aborted("thread_busy"));
        assert_eq!(busy, StoreCallError::Busy);
        assert!(busy.retryable());
        assert_eq!(busy.code(), "workspace_store.thread_busy");
        assert!(matches!(
            status_error(&tonic::Status::unavailable("down")),
            StoreCallError::Transport { retryable: true }
        ));
        // Another layer's ABORTED is not thread contention.
        assert!(matches!(
            status_error(&tonic::Status::aborted("stream reset by the proxy")),
            StoreCallError::Transport { retryable: true }
        ));
    }

    #[tokio::test]
    async fn busy_then_success_resends_the_same_call_until_it_applies() {
        let calls = std::sync::atomic::AtomicU32::new(0);
        let result = retry_busy(QUICK, || async {
            match calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst) {
                0 | 1 => Err(StoreCallError::Busy),
                _ => Ok(7_u64),
            }
        })
        .await;
        assert_eq!(result, Ok(7));
        assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 3);

        // A conflict is answered at once, never resent.
        let calls = std::sync::atomic::AtomicU32::new(0);
        let conflict = retry_busy(QUICK, || async {
            calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Err::<u64, _>(StoreCallError::Refused {
                code: RefusalCode::RefConflict,
                current: BTreeMap::new(),
            })
        })
        .await;
        assert!(matches!(conflict, Err(StoreCallError::Refused { .. })));
        assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 1);

        // A thread busy past the budget gives up with Busy.
        let calls = std::sync::atomic::AtomicU32::new(0);
        let exhausted = retry_busy(QUICK, || async {
            calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Err::<u64, _>(StoreCallError::Busy)
        })
        .await;
        assert_eq!(exhausted, Err(StoreCallError::Busy));
        assert_eq!(
            calls.load(std::sync::atomic::Ordering::SeqCst),
            QUICK.attempts
        );
    }
}

#[cfg(test)]
#[path = "store_busy_tests.rs"]
mod busy_tests;
