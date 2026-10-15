//! The worker's half of `WorkspaceStoreService` (ADR-0034 decision 9): the
//! four claim-bound gRPC calls on Main's control listener, and the strict
//! reading of their frames.
//!
//! [`WorkspaceStore`] is what the turn orchestrator uses; [`GrpcWorkspaceStore`]
//! is the production implementation over the control channel. Every call is
//! one attempt: a lost publish is retried by the orchestrator only as the
//! same request, which Main answers with the original result.

use std::collections::BTreeMap;
use std::time::Duration;

use async_trait::async_trait;
use sha2::{Digest as _, Sha256};
use tokio::time::timeout;
use tokio_stream::StreamExt as _;
use tonic::transport::Channel;

use crate::protocol::elitea::runtime::v1::{
    RuntimeErrorCodeV1, RuntimeErrorV1, WorkspaceClaimV1, WorkspaceFetchFrameV1,
    WorkspaceFetchPlanRequestV1, WorkspacePublishRequestV1, WorkspacePublishResponseV1,
    WorkspacePullRequestDiffFrameV1, WorkspacePullRequestDiffRequestV1, WorkspaceRefUpdateV1,
    WorkspaceRefV1, WorkspaceRefusalCodeV1, WorkspaceRefusalV1, WorkspaceUploadFrameV1,
    WorkspaceUploadHeaderV1, WorkspaceUploadResponseV1, workspace_fetch_frame_v1,
    workspace_pull_request_diff_frame_v1, workspace_store_service_client::WorkspaceStoreServiceClient,
    workspace_upload_frame_v1,
};

use super::context::sha1_hex;

/// Bytes per stream frame, both directions: under the control listener's
/// 64 KiB request and 80 KiB response bounds.
pub(crate) const CHUNK_BYTES: usize = 32 * 1_024;
/// The control listener's message bounds.
const MAX_REQUEST_BYTES: usize = 64 * 1_024;
const MAX_RESPONSE_BYTES: usize = 80 * 1_024;
/// The pull-request diff bound (decision 10).
pub(crate) const MAX_DIFF_BYTES: u64 = 1 << 20;

pub(crate) const REF_CLOUD: &str = "refs/elitea/cloud";
pub(crate) const REF_HANDOFF_PREFIX: &str = "refs/elitea/handoff/";
pub(crate) const KIND_COMPACT: &str = "COMPACT";
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
    NoUpstream,
    DiffTooLarge,
    DiffBinary,
    DiffEmpty,
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
            Self::NoUpstream => "no_upstream",
            Self::DiffTooLarge => "diff_too_large",
            Self::DiffBinary => "diff_binary",
            Self::DiffEmpty => "diff_empty",
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
            WorkspaceRefusalCodeV1::NoUpstream => Self::NoUpstream,
            WorkspaceRefusalCodeV1::DiffTooLarge => Self::DiffTooLarge,
            WorkspaceRefusalCodeV1::DiffBinary => Self::DiffBinary,
            WorkspaceRefusalCodeV1::DiffEmpty => Self::DiffEmpty,
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
            Self::Malformed(_) => "workspace_store.malformed_response",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        match self {
            Self::Rejected { retryable, .. } | Self::Transport { retryable } => *retryable,
            Self::Refused { .. } | Self::Malformed(_) => false,
        }
    }
}

/// One planned segment.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PlannedSegment {
    pub(crate) seq: u64,
    pub(crate) sha256: String,
    pub(crate) bytes: u64,
    pub(crate) refs: BTreeMap<String, String>,
}

/// The fetched thread: its plan and its one COMPACT segment's bytes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FetchedThread {
    pub(crate) head_seq: u64,
    pub(crate) refs: BTreeMap<String, String>,
    pub(crate) segment: PlannedSegment,
    pub(crate) bytes: Vec<u8>,
}

/// The executor's publish: one COMPACT segment, one move of the cloud ref.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PublishSegment {
    pub(crate) segment_sha256: String,
    pub(crate) bytes: u64,
    pub(crate) old_cloud: Option<String>,
    pub(crate) new_cloud: String,
}

/// The applied publish.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PublishedSegment {
    pub(crate) seq: u64,
    pub(crate) refs: BTreeMap<String, String>,
}

/// The pull-request diff Main computed (U → cloud tip) and what it is
/// opened against.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PullRequestDiff {
    pub(crate) upstream_sha: String,
    pub(crate) upstream_branch: String,
    pub(crate) remote_url: String,
    pub(crate) cloud_sha: String,
    pub(crate) files: u32,
    pub(crate) text: String,
}

/// The four calls, as the orchestrator and the pull-request tool use them.
#[async_trait]
pub(crate) trait WorkspaceStore: Send + Sync {
    async fn fetch(
        &self,
        claim: &WorkspaceClaimV1,
        since_seq: u64,
        max_segment_bytes: u64,
    ) -> Result<FetchedThread, StoreCallError>;

    /// Upload one segment; `true` when Main already had that digest.
    async fn upload(
        &self,
        claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: &[u8],
    ) -> Result<bool, StoreCallError>;

    async fn publish(
        &self,
        claim: &WorkspaceClaimV1,
        request: &PublishSegment,
    ) -> Result<PublishedSegment, StoreCallError>;

    async fn pull_request_diff(
        &self,
        claim: &WorkspaceClaimV1,
    ) -> Result<PullRequestDiff, StoreCallError>;
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

fn refused(refusal: &WorkspaceRefusalV1) -> StoreCallError {
    match RefusalCode::from_wire(refusal.code) {
        Some(code) => StoreCallError::Refused {
            code,
            current: ref_map(&refusal.current).unwrap_or_default(),
        },
        None => StoreCallError::Malformed("the workspace refusal is unknown"),
    }
}

fn rejected(rejection: &RuntimeErrorV1) -> StoreCallError {
    let code = RuntimeErrorCodeV1::try_from(rejection.code)
        .map_or(RuntimeErrorCodeV1::Unspecified as i32, |code| code as i32);
    StoreCallError::Rejected {
        code,
        retryable: rejection.retryable,
    }
}

/// Read a whole fetch stream: exactly one plan, then exactly one COMPACT
/// segment's chunks and its end frame, matched against the plan.
///
/// # Errors
///
/// A refusal or rejection frame, or any frame out of order.
pub(crate) fn read_fetch_frames(
    frames: impl IntoIterator<Item = WorkspaceFetchFrameV1>,
    max_segment_bytes: u64,
) -> Result<FetchedThread, StoreCallError> {
    use workspace_fetch_frame_v1::Frame;
    let mut frames = frames.into_iter();
    let plan = match frames.next().and_then(|frame| frame.frame) {
        Some(Frame::Plan(plan)) => plan,
        Some(Frame::Refusal(refusal)) => return Err(refused(&refusal)),
        Some(Frame::Rejection(rejection)) => return Err(rejected(&rejection)),
        _ => return Err(StoreCallError::Malformed("the fetch stream has no plan")),
    };
    let refs = ref_map(&plan.refs)?;
    let [segment] = plan.segments.as_slice() else {
        return Err(StoreCallError::Malformed(
            "the fetch plan is not one COMPACT segment",
        ));
    };
    if segment.kind != KIND_COMPACT
        || !segment.prerequisites.is_empty()
        || !sha256_valid(&segment.sha256)
        || segment.bytes == 0
        || segment.bytes > max_segment_bytes
    {
        return Err(StoreCallError::Malformed(
            "the fetch plan is not one COMPACT segment",
        ));
    }
    let planned = PlannedSegment {
        seq: segment.seq,
        sha256: segment.sha256.clone(),
        bytes: segment.bytes,
        refs: ref_map(&segment.refs)?,
    };
    let mut bytes = Vec::with_capacity(usize::try_from(segment.bytes).unwrap_or(0));
    for frame in frames.by_ref() {
        match frame.frame {
            Some(Frame::SegmentChunk(chunk)) => {
                if chunk.is_empty()
                    || chunk.len() > CHUNK_BYTES
                    || (bytes.len() + chunk.len()) as u64 > planned.bytes
                {
                    return Err(StoreCallError::Malformed("a segment chunk is malformed"));
                }
                bytes.extend_from_slice(&chunk);
            }
            Some(Frame::SegmentEnd(end)) => {
                if end.sha256 != planned.sha256
                    || end.bytes != planned.bytes
                    || bytes.len() as u64 != planned.bytes
                    || sha256_hex(&bytes) != planned.sha256
                {
                    return Err(StoreCallError::Refused {
                        code: RefusalCode::ChecksumMismatch,
                        current: BTreeMap::new(),
                    });
                }
                if frames.next().is_some() {
                    return Err(StoreCallError::Malformed(
                        "the fetch stream continues past its segment",
                    ));
                }
                return Ok(FetchedThread {
                    head_seq: plan.head_seq,
                    refs,
                    segment: planned,
                    bytes,
                });
            }
            Some(Frame::Refusal(refusal)) => return Err(refused(&refusal)),
            Some(Frame::Rejection(rejection)) => return Err(rejected(&rejection)),
            Some(Frame::Plan(_)) | None => {
                return Err(StoreCallError::Malformed("the fetch stream is out of order"));
            }
        }
    }
    Err(StoreCallError::Malformed("the fetch stream ended early"))
}

/// The upload stream for one segment: a header, then 32 KiB chunks.
#[must_use]
pub(crate) fn upload_frames(
    claim: &WorkspaceClaimV1,
    sha256: &str,
    bytes: &[u8],
) -> Vec<WorkspaceUploadFrameV1> {
    use workspace_upload_frame_v1::Frame;
    let mut frames = vec![WorkspaceUploadFrameV1 {
        frame: Some(Frame::Header(WorkspaceUploadHeaderV1 {
            claim: Some(claim.clone()),
            sha256: sha256.to_owned(),
            bytes: bytes.len() as u64,
        })),
    }];
    frames.extend(bytes.chunks(CHUNK_BYTES).map(|chunk| WorkspaceUploadFrameV1 {
        frame: Some(Frame::Chunk(chunk.to_vec())),
    }));
    frames
}

fn read_upload(response: &WorkspaceUploadResponseV1) -> Result<bool, StoreCallError> {
    match (&response.refusal, &response.rejection) {
        (Some(refusal), None) if !response.exists => Err(refused(refusal)),
        (None, Some(rejection)) if !response.exists => Err(rejected(rejection)),
        (None, None) => Ok(response.exists),
        _ => Err(StoreCallError::Malformed("the upload response is ambiguous")),
    }
}

#[must_use]
pub(crate) fn publish_request(
    claim: &WorkspaceClaimV1,
    request: &PublishSegment,
) -> WorkspacePublishRequestV1 {
    WorkspacePublishRequestV1 {
        claim: Some(claim.clone()),
        kind: KIND_COMPACT.to_owned(),
        segment_sha256: request.segment_sha256.clone(),
        bytes: request.bytes,
        prerequisites: Vec::new(),
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
        _ => return Err(StoreCallError::Malformed("the publish response is ambiguous")),
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

/// Read a pull-request diff stream: one header, then chunks matching it.
///
/// # Errors
///
/// A refusal or rejection frame, or a diff that does not match its header.
pub(crate) fn read_diff_frames(
    frames: impl IntoIterator<Item = WorkspacePullRequestDiffFrameV1>,
) -> Result<PullRequestDiff, StoreCallError> {
    use workspace_pull_request_diff_frame_v1::Frame;
    let mut frames = frames.into_iter();
    let header = match frames.next().and_then(|frame| frame.frame) {
        Some(Frame::Header(header)) => header,
        Some(Frame::Refusal(refusal)) => return Err(refused(&refusal)),
        Some(Frame::Rejection(rejection)) => return Err(rejected(&rejection)),
        _ => return Err(StoreCallError::Malformed("the diff stream has no header")),
    };
    if !sha1_hex(&header.upstream_sha)
        || !sha1_hex(&header.cloud_sha)
        || header.upstream_branch.is_empty()
        || header.upstream_branch.len() > 255
        || header.bytes == 0
        || header.bytes > MAX_DIFF_BYTES
        || header.files == 0
        || header.files > 100
        || !sha256_valid(&header.diff_sha256)
    {
        return Err(StoreCallError::Malformed("the diff header is malformed"));
    }
    let mut text = Vec::with_capacity(usize::try_from(header.bytes).unwrap_or(0));
    for frame in frames {
        match frame.frame {
            Some(Frame::Chunk(chunk))
                if !chunk.is_empty()
                    && chunk.len() <= CHUNK_BYTES
                    && (text.len() + chunk.len()) as u64 <= header.bytes =>
            {
                text.extend_from_slice(&chunk);
            }
            _ => return Err(StoreCallError::Malformed("the diff stream is malformed")),
        }
    }
    if text.len() as u64 != header.bytes || sha256_hex(&text) != header.diff_sha256 {
        return Err(StoreCallError::Malformed("the diff does not match its header"));
    }
    let text = String::from_utf8(text)
        .map_err(|_| StoreCallError::Malformed("the diff is not UTF-8 text"))?;
    Ok(PullRequestDiff {
        upstream_sha: header.upstream_sha,
        upstream_branch: header.upstream_branch,
        remote_url: header.remote_url,
        cloud_sha: header.cloud_sha,
        files: header.files,
        text,
    })
}

/// The production store over the control channel.
pub(crate) struct GrpcWorkspaceStore {
    client: WorkspaceStoreServiceClient<Channel>,
    deadline: Duration,
}

impl GrpcWorkspaceStore {
    /// `deadline` bounds each whole call, segment transfer included.
    #[must_use]
    pub(crate) fn new(channel: Channel, deadline: Duration) -> Self {
        let client = WorkspaceStoreServiceClient::new(channel)
            .max_encoding_message_size(MAX_REQUEST_BYTES)
            .max_decoding_message_size(MAX_RESPONSE_BYTES);
        Self { client, deadline }
    }
}

fn status_error(status: &tonic::Status) -> StoreCallError {
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
    ) -> Result<FetchedThread, StoreCallError> {
        let mut client = self.client.clone();
        let request = WorkspaceFetchPlanRequestV1 {
            claim: Some(claim.clone()),
            since_seq,
        };
        let operation = async move {
            let mut stream = client
                .workspace_fetch_plan(request)
                .await
                .map_err(|status| status_error(&status))?
                .into_inner();
            // Frames are collected bounded: the plan's size cap is checked
            // while reading, and chunks past it are refused.
            let mut frames = Vec::new();
            let mut received: u64 = 0;
            while let Some(frame) = stream.next().await {
                let frame = frame.map_err(|status| status_error(&status))?;
                if let Some(workspace_fetch_frame_v1::Frame::SegmentChunk(chunk)) = &frame.frame {
                    received += chunk.len() as u64;
                    if received > max_segment_bytes {
                        return Err(StoreCallError::Malformed(
                            "the segment exceeds the policy's size",
                        ));
                    }
                }
                frames.push(frame);
            }
            read_fetch_frames(frames, max_segment_bytes)
        };
        timeout(self.deadline, operation)
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
    }

    async fn upload(
        &self,
        claim: &WorkspaceClaimV1,
        sha256: &str,
        bytes: &[u8],
    ) -> Result<bool, StoreCallError> {
        let mut client = self.client.clone();
        let frames = upload_frames(claim, sha256, bytes);
        let operation = async move {
            let response = client
                .workspace_upload(tokio_stream::iter(frames))
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
        let mut client = self.client.clone();
        let wire = publish_request(claim, request);
        let response = timeout(self.deadline, client.workspace_publish(wire))
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
            .map_err(|status| status_error(&status))?
            .into_inner();
        read_publish(&response, request)
    }

    async fn pull_request_diff(
        &self,
        claim: &WorkspaceClaimV1,
    ) -> Result<PullRequestDiff, StoreCallError> {
        let mut client = self.client.clone();
        let request = WorkspacePullRequestDiffRequestV1 {
            claim: Some(claim.clone()),
        };
        let operation = async move {
            let mut stream = client
                .workspace_pull_request_diff(request)
                .await
                .map_err(|status| status_error(&status))?
                .into_inner();
            let mut frames = Vec::new();
            let mut received: u64 = 0;
            while let Some(frame) = stream.next().await {
                let frame = frame.map_err(|status| status_error(&status))?;
                if let Some(workspace_pull_request_diff_frame_v1::Frame::Chunk(chunk)) =
                    &frame.frame
                {
                    received += chunk.len() as u64;
                    if received > MAX_DIFF_BYTES {
                        return Err(StoreCallError::Malformed("the diff is over 1 MiB"));
                    }
                }
                frames.push(frame);
            }
            read_diff_frames(frames)
        };
        timeout(self.deadline, operation)
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        WorkspaceFetchPlanV1, WorkspacePullRequestDiffHeaderV1, WorkspaceSegmentEndV1,
        WorkspaceSegmentV1,
    };

    const CLOUD: &str = "1111111111111111111111111111111111111111";

    fn plan_frames(segment: &[u8], corrupt_end: bool) -> Vec<WorkspaceFetchFrameV1> {
        use workspace_fetch_frame_v1::Frame;
        let refs = vec![WorkspaceRefV1 {
            name: REF_CLOUD.to_owned(),
            sha: CLOUD.to_owned(),
        }];
        let sha = sha256_hex(segment);
        let mut frames = vec![WorkspaceFetchFrameV1 {
            frame: Some(Frame::Plan(WorkspaceFetchPlanV1 {
                head_seq: 3,
                refs: refs.clone(),
                segments: vec![WorkspaceSegmentV1 {
                    seq: 3,
                    kind: KIND_COMPACT.to_owned(),
                    sha256: sha.clone(),
                    bytes: segment.len() as u64,
                    prerequisites: Vec::new(),
                    refs,
                }],
            })),
        }];
        frames.extend(segment.chunks(CHUNK_BYTES).map(|chunk| WorkspaceFetchFrameV1 {
            frame: Some(Frame::SegmentChunk(chunk.to_vec())),
        }));
        frames.push(WorkspaceFetchFrameV1 {
            frame: Some(Frame::SegmentEnd(WorkspaceSegmentEndV1 {
                sha256: if corrupt_end { "0".repeat(64) } else { sha },
                bytes: segment.len() as u64,
            })),
        });
        frames
    }

    #[test]
    fn a_fetch_stream_is_read_and_checked_against_its_plan() {
        let segment = vec![7_u8; 100_000];
        let fetched = read_fetch_frames(plan_frames(&segment, false), 1 << 20).expect("fetched");
        assert_eq!(fetched.bytes, segment);
        assert_eq!(fetched.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));

        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, true), 1 << 20),
            Err(StoreCallError::Refused {
                code: RefusalCode::ChecksumMismatch,
                ..
            })
        ));
        // Over the policy's segment cap: refused before any byte is used.
        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, false), 1_000),
            Err(StoreCallError::Malformed(_))
        ));
        // A truncated stream is never a usable segment.
        let mut truncated = plan_frames(&segment, false);
        truncated.pop();
        assert!(read_fetch_frames(truncated, 1 << 20).is_err());
    }

    #[test]
    fn a_refused_fetch_reads_its_code() {
        let frame = WorkspaceFetchFrameV1 {
            frame: Some(workspace_fetch_frame_v1::Frame::Refusal(WorkspaceRefusalV1 {
                code: WorkspaceRefusalCodeV1::StaleEpoch as i32,
                current: Vec::new(),
            })),
        };
        assert!(matches!(
            read_fetch_frames([frame], 1 << 20),
            Err(StoreCallError::Refused {
                code: RefusalCode::StaleEpoch,
                ..
            })
        ));
    }

    #[test]
    fn upload_frames_fit_the_control_listener_bounds() {
        let claim = WorkspaceClaimV1::default();
        let bytes = vec![1_u8; 3 * CHUNK_BYTES + 5];
        let frames = upload_frames(&claim, &sha256_hex(&bytes), &bytes);
        assert_eq!(frames.len(), 5);
        for frame in &frames {
            assert!(prost::Message::encoded_len(frame) < MAX_REQUEST_BYTES);
        }
    }

    #[test]
    fn a_publish_must_move_the_cloud_ref_to_the_requested_commit() {
        let request = PublishSegment {
            segment_sha256: "a".repeat(64),
            bytes: 10,
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
    fn a_diff_stream_must_match_its_header() {
        use workspace_pull_request_diff_frame_v1::Frame;
        let text = "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-a\n+b\n";
        let header = |digest: String| WorkspacePullRequestDiffFrameV1 {
            frame: Some(Frame::Header(WorkspacePullRequestDiffHeaderV1 {
                upstream_sha: "3".repeat(40),
                upstream_branch: "main".to_owned(),
                remote_url: "https://github.com/o/r.git".to_owned(),
                cloud_sha: "4".repeat(40),
                files: 1,
                bytes: text.len() as u64,
                diff_sha256: digest,
            })),
        };
        let chunk = WorkspacePullRequestDiffFrameV1 {
            frame: Some(Frame::Chunk(text.as_bytes().to_vec())),
        };
        let diff =
            read_diff_frames([header(sha256_hex(text.as_bytes())), chunk.clone()]).expect("diff");
        assert_eq!(diff.text, text);
        assert_eq!(diff.upstream_branch, "main");
        assert!(read_diff_frames([header("0".repeat(64)), chunk]).is_err());
    }
}
