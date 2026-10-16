//! The worker's half of `WorkspaceStoreService` (ADR-0034 decision 9): the
//! four claim-bound gRPC calls on Main's control listener, and the strict
//! reading of their frames.
//!
//! [`WorkspaceStore`] is what the turn orchestrator uses; [`GrpcWorkspaceStore`]
//! is the production implementation over the control channel. Every call is
//! one attempt: a lost publish is retried by the orchestrator only as the
//! same request, which Main answers with the original result.

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
    WorkspaceEnvSecretRequestV1, WorkspaceFetchFrameV1, WorkspaceFetchPlanRequestV1,
    WorkspacePublishRequestV1, WorkspacePublishResponseV1, WorkspaceRefUpdateV1, WorkspaceRefV1,
    WorkspaceRefusalCodeV1, WorkspaceRefusalV1, WorkspaceUploadFrameV1, WorkspaceUploadHeaderV1,
    WorkspaceUploadResponseV1, workspace_env_secret_frame_v1, workspace_fetch_frame_v1,
    workspace_store_service_client::WorkspaceStoreServiceClient, workspace_upload_frame_v1,
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

/// The fetched thread: its plan; its one COMPACT segment was streamed to
/// the file the caller named, and checked against the plan on the way.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FetchedThread {
    pub(crate) head_seq: u64,
    pub(crate) refs: BTreeMap<String, String>,
    pub(crate) segment: PlannedSegment,
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

/// The store calls, as the orchestrator uses them.
#[async_trait]
pub(crate) trait WorkspaceStore: Send + Sync {
    /// Fetch the plan and stream its segment into the file `into`, never
    /// holding more than one chunk of it.
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

/// Reads a fetch stream frame by frame: exactly one plan, then exactly one
/// COMPACT segment's chunks (written straight to the segment file, hashed
/// as they pass) and its end frame, matched against the plan. At most one
/// chunk is in memory.
pub(crate) struct FetchSink {
    file: std::io::BufWriter<std::fs::File>,
    max_segment_bytes: u64,
    plan: Option<(u64, BTreeMap<String, String>, PlannedSegment)>,
    hasher: Sha256,
    written: u64,
    done: Option<FetchedThread>,
}

impl FetchSink {
    /// # Errors
    ///
    /// The segment file cannot be created.
    pub(crate) fn create(into: &Path, max_segment_bytes: u64) -> Result<Self, StoreCallError> {
        let file = std::fs::File::create(into)
            .map_err(|_| StoreCallError::Malformed("the segment file cannot be written"))?;
        Ok(Self {
            file: std::io::BufWriter::with_capacity(CHUNK_BYTES * 2, file),
            max_segment_bytes,
            plan: None,
            hasher: Sha256::new(),
            written: 0,
            done: None,
        })
    }

    /// Take one frame.
    ///
    /// # Errors
    ///
    /// A refusal or rejection frame, or any frame out of order.
    pub(crate) fn push(&mut self, frame: WorkspaceFetchFrameV1) -> Result<(), StoreCallError> {
        use std::io::Write as _;
        use workspace_fetch_frame_v1::Frame;
        if self.done.is_some() {
            return Err(StoreCallError::Malformed(
                "the fetch stream continues past its segment",
            ));
        }
        let Some((head_seq, refs, planned)) = &self.plan else {
            return match frame.frame {
                Some(Frame::Plan(plan)) => {
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
                        || segment.bytes > self.max_segment_bytes
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
                    self.plan = Some((plan.head_seq, refs, planned));
                    Ok(())
                }
                Some(Frame::Refusal(refusal)) => Err(refused(&refusal)),
                Some(Frame::Rejection(rejection)) => Err(rejected(&rejection)),
                _ => Err(StoreCallError::Malformed("the fetch stream has no plan")),
            };
        };
        match frame.frame {
            Some(Frame::SegmentChunk(chunk)) => {
                if chunk.is_empty()
                    || chunk.len() > CHUNK_BYTES
                    || self.written + chunk.len() as u64 > planned.bytes
                {
                    return Err(StoreCallError::Malformed("a segment chunk is malformed"));
                }
                self.file
                    .write_all(&chunk)
                    .map_err(|_| StoreCallError::Malformed("the segment file cannot be written"))?;
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
                    .flush()
                    .map_err(|_| StoreCallError::Malformed("the segment file cannot be written"))?;
                self.done = Some(FetchedThread {
                    head_seq: *head_seq,
                    refs: refs.clone(),
                    segment: planned.clone(),
                });
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
    /// It ended before the segment's end frame.
    pub(crate) fn finish(self) -> Result<FetchedThread, StoreCallError> {
        self.done
            .ok_or(StoreCallError::Malformed("the fetch stream ended early"))
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

/// Read a whole fetch stream into the segment file `into`.
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
        let mut client = self.client.clone();
        let wire = publish_request(claim, request);
        let response = timeout(self.deadline, client.workspace_publish(wire))
            .await
            .map_err(|_| StoreCallError::Transport { retryable: true })?
            .map_err(|status| status_error(&status))?
            .into_inner();
        read_publish(&response, request)
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
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        WorkspaceFetchPlanV1, WorkspaceSegmentEndV1, WorkspaceSegmentV1,
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
        frames.extend(
            segment
                .chunks(CHUNK_BYTES)
                .map(|chunk| WorkspaceFetchFrameV1 {
                    frame: Some(Frame::SegmentChunk(chunk.to_vec())),
                }),
        );
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
        let dir = tempfile::tempdir().expect("dir");
        let file = dir.path().join("segment");
        let segment = vec![7_u8; 100_000];
        let fetched =
            read_fetch_frames(plan_frames(&segment, false), 1 << 20, &file).expect("fetched");
        assert_eq!(std::fs::read(&file).expect("file"), segment);
        assert_eq!(fetched.refs.get(REF_CLOUD).map(String::as_str), Some(CLOUD));

        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, true), 1 << 20, &file),
            Err(StoreCallError::Refused {
                code: RefusalCode::ChecksumMismatch,
                ..
            })
        ));
        // Over the policy's segment cap: refused before any byte is used.
        assert!(matches!(
            read_fetch_frames(plan_frames(&segment, false), 1_000, &file),
            Err(StoreCallError::Malformed(_))
        ));
        // A truncated stream is never a usable segment.
        let mut truncated = plan_frames(&segment, false);
        truncated.pop();
        assert!(read_fetch_frames(truncated, 1 << 20, &file).is_err());
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
}
