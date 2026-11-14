//! Live progress frames of one index run (#2041).
//!
//! The terminal of a direct toolkit command is a single durable frame. An
//! `index.ingest.v1` run can last minutes, so it also publishes nonterminal
//! `NodeEvent` frames (stages and documents written) which Main projects to the
//! index activity and the execution stream.
//!
//! Progress never weakens the terminal's exactly-once contract:
//!
//! - frames are strictly sequential: one frame is bound, persisted and
//!   acknowledged by Main before the next is bound, so the spool holds at most
//!   one unacknowledged progress frame and the terminal replay, which requires
//!   exactly its own frame, stays valid;
//! - the terminal takes the sequence after the last acknowledged frame, from
//!   the same cursor, and is never written over or after an unresolved frame;
//! - progress is best effort. A frame Main rejects (Stop, the deadline) ends
//!   progress and its sequence is taken by the matching failure terminal, as an
//!   agent run does. A frame whose delivery stays uncertain after the bounded
//!   session attempts leaves the run without a terminal here: the command is
//!   not acknowledged, and the redelivered command's recovery claim reconciles
//!   the spool from Main's watermark.

use std::fmt;
use std::sync::Arc;

use super::output_delivery::{
    AgentOutputPreflightError, AgentOutputSpoolFactory, AgentProgressConnector,
    AgentProgressPublisherConfig, AgentProgressReplaySession, AgentProgressSession,
    PreparedAgentOutput, ReopenedAgentProgress, reconnectable_output,
};
use crate::indexing::progress::{ProgressNote, ProgressSink, ProgressStage};
use crate::protocol::ProtocolError;
use crate::protocol::command::VerifiedToolkitExecuteReadCommand;
use crate::protocol::control::{AgentProgressCommit, ToolkitOutputCursor};
use crate::protocol::elitea::runtime::v1::{ExecutionOutputFrameV1, NodeEventV1};
use crate::protocol::output::RuntimeFailureKind;
use crate::transport::output_grpc::{
    DurablyAckedProgress, FrameBoundProgressRejection, ProgressReplayDecision,
};
use crate::transport::{OutputGrpcError, PreparedOutputSpool};

/// How progress ended, when it can hand the spool back to the terminal.
pub(super) struct ToolkitProgressSettled {
    /// The spool, empty (or holding only a rejected frame the terminal
    /// replaces at its own sequence).
    pub(super) output: PreparedAgentOutput,
    /// The sequence the terminal takes.
    pub(super) terminal_sequence: u64,
    /// A failure Main's rejection decided: the terminal is this failure.
    pub(super) forced_failure: Option<RuntimeFailureKind>,
}

/// Stable, data-free reasons progress could not be settled for a terminal.
#[derive(Debug)]
pub(super) enum ToolkitProgressError {
    /// A frame may or may not be durable in Main; recovery decides.
    RecoveryRequired(&'static str),
    Protocol(ProtocolError),
    Preflight(AgentOutputPreflightError),
    Output(OutputGrpcError),
}

impl fmt::Display for ToolkitProgressError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::RecoveryRequired(message) => formatter.write_str(message),
            Self::Protocol(error) => write!(formatter, "the progress frame is invalid: {error}"),
            Self::Preflight(error) => write!(formatter, "progress spool recovery failed: {error}"),
            Self::Output(error) => write!(formatter, "progress output delivery failed: {error}"),
        }
    }
}

impl ToolkitProgressError {
    pub(super) const fn code(&self) -> &'static str {
        match self {
            Self::RecoveryRequired(_) => "toolkit_progress.recovery_required",
            Self::Protocol(_) => "toolkit_progress.invalid_state",
            Self::Preflight(_) => "toolkit_progress.output_unavailable",
            Self::Output(_) => "toolkit_progress.output_rejected",
        }
    }
}

struct PendingFrame {
    frame: ExecutionOutputFrameV1,
    attempts: usize,
}

/// Claim-bound, sequential progress publisher of one index run.
pub(super) struct ToolkitProgressPublisher<C: AgentProgressConnector> {
    cursor: ToolkitOutputCursor,
    first: Option<PreparedOutputSpool>,
    factory: AgentOutputSpoolFactory,
    connector: Arc<C>,
    live: Option<C::Session>,
    pending: Option<PendingFrame>,
    rejection: Option<FrameBoundProgressRejection>,
    /// No further frame is published (a rejection, or an unresolved failure).
    stopped: bool,
    max_output_sessions: usize,
}

impl<C: AgentProgressConnector> ToolkitProgressPublisher<C> {
    pub(super) fn new(
        cursor: ToolkitOutputCursor,
        output: PreparedAgentOutput,
        connector: Arc<C>,
        config: AgentProgressPublisherConfig,
    ) -> Self {
        let (prepared, factory) = output.into_parts();
        Self {
            cursor,
            first: Some(prepared),
            factory,
            connector,
            live: None,
            pending: None,
            rejection: None,
            stopped: false,
            max_output_sessions: config.max_output_sessions(),
        }
    }

    /// Publish one event and wait for Main's decision. Never fails the run: a
    /// problem stops further progress and is logged; whatever it leaves
    /// unresolved is dealt with by [`Self::finish`].
    pub(super) async fn publish(
        &mut self,
        verified: &VerifiedToolkitExecuteReadCommand,
        event: NodeEventV1,
        occurred_at_unix_millis: i64,
    ) {
        if self.stopped {
            return;
        }
        if self.pending.is_some() {
            self.stopped = true;
            return;
        }
        let Ok(progress) = self
            .cursor
            .bind_progress(verified, event, occurred_at_unix_millis)
        else {
            // Nothing was bound (the cursor is unchanged): only this event is
            // lost.
            tracing::warn!(
                event = "index_progress_event_dropped",
                error_code = "toolkit_progress.invalid_event",
            );
            return;
        };
        let frame = progress.into_frame();
        self.pending = Some(PendingFrame { frame, attempts: 0 });
        if let Err(error) = self.deliver().await {
            self.stopped = true;
            tracing::warn!(
                event = "index_progress_publication_failed",
                error_code = error.code(),
            );
        }
    }

    async fn deliver(&mut self) -> Result<(), ToolkitProgressError> {
        loop {
            let Some(pending) = self.pending.as_ref() else {
                return Ok(());
            };
            let (frame, attempts) = (pending.frame.clone(), pending.attempts);
            if let Some(session) = self.live.as_mut() {
                let decision = session.publish_progress(&frame).await;
                match decision {
                    Ok(ProgressReplayDecision::Acknowledged(acknowledged)) => {
                        return self.commit(acknowledged);
                    }
                    Ok(ProgressReplayDecision::Rejected(rejected)) => {
                        self.reject(rejected).await;
                        return Ok(());
                    }
                    Err(error) => {
                        self.close_live().await;
                        self.require_retry(error, attempts)?;
                        continue;
                    }
                }
            }
            if attempts >= self.max_output_sessions {
                return Err(ToolkitProgressError::Output(OutputGrpcError::Unavailable(
                    "the bounded progress output attempts were exhausted",
                )));
            }
            if let Some(pending) = self.pending.as_mut() {
                pending.attempts += 1;
            }
            let reopened = match self.first.take() {
                Some(prepared) => ReopenedAgentProgress::Empty(prepared),
                None => self
                    .factory
                    .reopen_progress(&frame)
                    .await
                    .map_err(ToolkitProgressError::Preflight)?,
            };
            match reopened {
                ReopenedAgentProgress::Empty(prepared) => {
                    match self.connector.connect_progress(prepared).await {
                        Ok(session) => self.live = Some(session),
                        Err(error) => self.require_retry(error, attempts + 1)?,
                    }
                }
                ReopenedAgentProgress::Pending(prepared) => {
                    match self.connector.start_progress_replay(prepared, &frame).await {
                        Ok(mut replay) => {
                            let waited = replay.wait().await;
                            if replay.close().await.is_err() {
                                tracing::warn!(
                                    "progress replay stream close failed after a bound Main decision"
                                );
                            }
                            match waited {
                                Ok(ProgressReplayDecision::Acknowledged(acknowledged)) => {
                                    return self.commit(acknowledged);
                                }
                                Ok(ProgressReplayDecision::Rejected(rejected)) => {
                                    self.reject(rejected).await;
                                    return Ok(());
                                }
                                Err(error) => self.require_retry(error, attempts + 1)?,
                            }
                        }
                        Err(error) => self.require_retry(error, attempts + 1)?,
                    }
                }
            }
        }
    }

    /// Continue only after a reconnectable failure with attempts left.
    fn require_retry(
        &self,
        error: OutputGrpcError,
        attempts: usize,
    ) -> Result<(), ToolkitProgressError> {
        if reconnectable_output(&error) && attempts < self.max_output_sessions {
            Ok(())
        } else {
            Err(ToolkitProgressError::Output(error))
        }
    }

    fn commit(&mut self, acknowledged: DurablyAckedProgress) -> Result<(), ToolkitProgressError> {
        match self.cursor.commit_progress_durable(acknowledged) {
            Ok(AgentProgressCommit::Advanced) => {
                self.pending = None;
                Ok(())
            }
            Ok(AgentProgressCommit::Exhausted(error)) => {
                self.pending = None;
                self.stopped = true;
                Err(ToolkitProgressError::Protocol(error))
            }
            Err(error) => Err(ToolkitProgressError::Protocol(error)),
        }
    }

    async fn reject(&mut self, rejected: FrameBoundProgressRejection) {
        self.rejection = Some(rejected);
        self.stopped = true;
        self.close_live().await;
    }

    async fn close_live(&mut self) {
        if let Some(mut session) = self.live.take()
            && session.close().await.is_err()
        {
            tracing::warn!("progress output session close failed after delivery uncertainty");
        }
    }

    /// Close the stream and settle the spool for the terminal.
    ///
    /// # Errors
    ///
    /// [`ToolkitProgressError::RecoveryRequired`] while a frame is
    /// unacknowledged and unrejected: the caller must not publish a terminal.
    pub(super) async fn finish(
        mut self,
        verified: &VerifiedToolkitExecuteReadCommand,
    ) -> Result<ToolkitProgressSettled, ToolkitProgressError> {
        self.close_live().await;
        let Self {
            mut cursor,
            first,
            factory,
            pending,
            rejection,
            ..
        } = self;
        match (pending, rejection) {
            (None, _) => {
                let terminal_sequence = cursor
                    .terminal_sequence()
                    .map_err(ToolkitProgressError::Protocol)?;
                let prepared = match first {
                    Some(prepared) => prepared,
                    None => {
                        let prepared = factory
                            .reopen()
                            .await
                            .map_err(ToolkitProgressError::Preflight)?;
                        if prepared.pending_frame_count() != 0 {
                            return Err(ToolkitProgressError::RecoveryRequired(
                                "the output spool is not empty after progress",
                            ));
                        }
                        prepared
                    }
                };
                Ok(ToolkitProgressSettled {
                    output: PreparedAgentOutput::new(prepared, factory),
                    terminal_sequence,
                    forced_failure: None,
                })
            }
            (Some(pending), Some(rejected)) => {
                let (terminal_sequence, failure) = cursor
                    .bind_rejected_progress(verified, rejected)
                    .map_err(ToolkitProgressError::Protocol)?;
                let reopened = factory
                    .reopen_progress(&pending.frame)
                    .await
                    .map_err(ToolkitProgressError::Preflight)?;
                let ReopenedAgentProgress::Pending(prepared) = reopened else {
                    return Err(ToolkitProgressError::RecoveryRequired(
                        "the rejected progress frame is not durably recoverable",
                    ));
                };
                Ok(ToolkitProgressSettled {
                    output: PreparedAgentOutput::new(prepared, factory),
                    terminal_sequence,
                    forced_failure: Some(failure),
                })
            }
            (Some(_), None) => Err(ToolkitProgressError::RecoveryRequired(
                "progress remains unacknowledged before terminal publication",
            )),
        }
    }
}

// ---------------------------------------------------------------------------
// The sink: an index run's notes become a few browser-visible tool steps.
// ---------------------------------------------------------------------------

/// Fewest documents between two document updates.
const MIN_DOCUMENT_STEP: u64 = 5;
/// Most document updates one run publishes, however large it is. With the
/// stage frames this bounds a run's progress at a few dozen frames.
const MAX_DOCUMENT_UPDATES: u32 = 20;
/// Shortest time between two document updates.
const MIN_DOCUMENT_INTERVAL_MILLIS: i64 = 1_000;

/// Which document counts are worth a frame.
#[derive(Debug, Default)]
pub(super) struct DocumentThrottle {
    updates: u32,
    last_processed: u64,
    last_at_unix_millis: i64,
}

impl DocumentThrottle {
    /// The writing started at `now`; the first update waits one interval.
    fn start(&mut self, now_unix_millis: i64) {
        self.last_at_unix_millis = now_unix_millis;
    }

    /// Whether `processed` of `total` documents is worth a frame now. The last
    /// document never is: the stage's end reports it.
    fn admit(&mut self, processed: u64, total: u64, now_unix_millis: i64) -> bool {
        if processed >= total || self.updates >= MAX_DOCUMENT_UPDATES {
            return false;
        }
        let step = MIN_DOCUMENT_STEP.max(total.div_ceil(u64::from(MAX_DOCUMENT_UPDATES)));
        if processed.saturating_sub(self.last_processed) < step
            || now_unix_millis.saturating_sub(self.last_at_unix_millis)
                < MIN_DOCUMENT_INTERVAL_MILLIS
        {
            return false;
        }
        self.updates += 1;
        self.last_processed = processed;
        self.last_at_unix_millis = now_unix_millis;
        true
    }
}

fn rfc3339(unix_millis: i64) -> Option<String> {
    chrono::DateTime::from_timestamp_millis(unix_millis)
        .map(|time| time.to_rfc3339_opts(chrono::SecondsFormat::Millis, true))
}

/// One tool step frame in the shape the index view already renders:
/// `agent_tool_start` opens a step named `name`, `agent_tool_end` closes it
/// with `text`. Timestamps are RFC 3339 strings, as Main's projection reads
/// them.
fn step_event(
    start: bool,
    message_id: &str,
    run_id: &str,
    name: &str,
    text: Option<&str>,
    unix_millis: i64,
) -> NodeEventV1 {
    let timestamp = rfc3339(unix_millis);
    let mut metadata = serde_json::Map::new();
    metadata.insert("tool_run_id".to_owned(), run_id.into());
    metadata.insert("tool_name".to_owned(), name.into());
    if let Some(timestamp) = timestamp.as_deref() {
        metadata.insert(
            if start {
                "timestamp_start"
            } else {
                "timestamp_finish"
            }
            .to_owned(),
            timestamp.into(),
        );
    }
    NodeEventV1 {
        r#type: if start {
            "agent_tool_start"
        } else {
            "agent_tool_end"
        }
        .to_owned(),
        message_id: Some(message_id.to_owned()),
        content: text
            .and_then(|text| serde_json::to_vec(text).ok())
            .unwrap_or_default(),
        response_metadata: serde_json::to_vec(&serde_json::Value::Object(metadata))
            .unwrap_or_default(),
        created_at: timestamp,
        ..NodeEventV1::default()
    }
}

/// Turns notes into frames, and decides which are worth sending.
#[derive(Debug)]
pub(super) struct Narrator {
    message_id: String,
    throttle: DocumentThrottle,
    /// Whether the plan left documents to write (otherwise the write stage is
    /// not worth a step).
    writes: bool,
}

impl Narrator {
    pub(super) fn new(execution_id: &str) -> Self {
        Self {
            message_id: format!("index-run:{execution_id}"),
            throttle: DocumentThrottle::default(),
            writes: true,
        }
    }

    pub(super) fn events(&mut self, note: ProgressNote, now: i64) -> Vec<NodeEventV1> {
        let id = self.message_id.as_str();
        match note {
            ProgressNote::Started(ProgressStage::Plan) => {
                vec![step_event(true, id, "plan", PLAN_STEP, None, now)]
            }
            ProgressNote::Planned {
                to_write,
                unchanged,
                vanished,
            } => {
                self.writes = to_write > 0;
                let text =
                    format!("{to_write} to index, {unchanged} unchanged, {vanished} to remove");
                vec![step_event(false, id, "plan", PLAN_STEP, Some(&text), now)]
            }
            ProgressNote::Started(ProgressStage::Write) => {
                if !self.writes {
                    return Vec::new();
                }
                self.throttle.start(now);
                vec![step_event(true, id, "write", WRITE_STEP, None, now)]
            }
            ProgressNote::Documents {
                processed,
                total,
                chunks,
            } => {
                if !self.writes || !self.throttle.admit(processed, total, now) {
                    return Vec::new();
                }
                let text = format!("Indexed {processed} of {total} documents ({chunks} chunks)");
                let run_id = format!("documents-{processed}");
                vec![
                    step_event(true, id, &run_id, &text, None, now),
                    step_event(false, id, &run_id, &text, Some(&text), now),
                ]
            }
            ProgressNote::Written { documents, chunks } => {
                if !self.writes {
                    return Vec::new();
                }
                let text = format!("{documents} documents indexed ({chunks} chunks)");
                vec![step_event(false, id, "write", WRITE_STEP, Some(&text), now)]
            }
        }
    }
}

const PLAN_STEP: &str = "Reading the source and the index";
const WRITE_STEP: &str = "Indexing documents";

/// Turns an index run's notes into progress frames.
pub(super) struct IndexProgressSink<'a, C: AgentProgressConnector> {
    verified: &'a VerifiedToolkitExecuteReadCommand,
    now: &'a (dyn Fn() -> i64 + Send + Sync),
    state: tokio::sync::Mutex<(Narrator, ToolkitProgressPublisher<C>)>,
}

impl<'a, C: AgentProgressConnector> IndexProgressSink<'a, C> {
    pub(super) fn new(
        verified: &'a VerifiedToolkitExecuteReadCommand,
        now: &'a (dyn Fn() -> i64 + Send + Sync),
        publisher: ToolkitProgressPublisher<C>,
    ) -> Self {
        Self {
            verified,
            now,
            state: tokio::sync::Mutex::new((
                Narrator::new(&verified.command().execution_id),
                publisher,
            )),
        }
    }

    /// End progress and hand the spool back to the terminal.
    pub(super) async fn finish(self) -> Result<ToolkitProgressSettled, ToolkitProgressError> {
        let (_, publisher) = self.state.into_inner();
        publisher.finish(self.verified).await
    }
}

#[async_trait::async_trait]
impl<C: AgentProgressConnector + 'static> ProgressSink for IndexProgressSink<'_, C> {
    async fn note(&self, note: ProgressNote) {
        let now = (self.now)();
        let mut state = self.state.lock().await;
        let (narrator, publisher) = &mut *state;
        for event in narrator.events(note, now) {
            publisher.publish(self.verified, event, now).await;
        }
    }
}
