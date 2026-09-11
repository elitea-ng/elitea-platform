use std::fmt;
use std::num::NonZeroU64;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use prost::Message;
use ring::digest;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc, oneshot, watch};
use tokio::task::JoinHandle;
use tokio::time::timeout;
use tokio_stream::wrappers::ReceiverStream;
use tonic::metadata::MetadataValue;
use tonic::transport::Channel;
use tonic::{Request, Streaming};

use crate::protocol::command::{VerifiedAgentCommand, VerifiedExecutionCommand};
use crate::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, ExecutionFenceV1, ExecutionIdentityV1, ExecutionOutcomeV1,
    ExecutionOutputAckV1, ExecutionOutputFrameV1, RuntimeErrorCodeV1, SettlementProposalV1,
    execution_output_frame_v1, execution_output_service_client::ExecutionOutputServiceClient,
};
use crate::protocol::output::MAX_OUTPUT_FRAME_BYTES;
use crate::spool::{EncryptedOutputSpool, SpoolError};

use super::output_session::{
    DurableOutputFrame, OutputSessionError, OutputSessionLimits, OutputSessionState,
};

const MAX_METADATA_BYTES: usize = 256;
const MAX_ACK_BYTES: usize = 80 * 1024;
const MAX_QUEUED_FRAMES: usize = 128;
const MAX_QUEUED_BYTES: usize = 64 * 1024 * 1024;
const MAX_ACK_TIMEOUT: Duration = Duration::from_mins(5);
const MAX_STREAM_DEADLINE: Duration = Duration::from_hours(1);

/// Bounded output-stream settings supplied by deployment composition.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct OutputGrpcConfig {
    pub max_queued_frames: usize,
    pub max_queued_bytes: usize,
    pub max_frame_bytes: usize,
    pub max_server_credit_frames: u32,
    pub max_server_credit_bytes: u64,
    pub stream_deadline: Duration,
    pub ack_timeout: Duration,
    pub workload_session_id: String,
    pub producer_id: String,
}

impl OutputGrpcConfig {
    fn state_limits(&self) -> OutputSessionLimits {
        OutputSessionLimits {
            queued_frame_capacity: self.max_queued_frames,
            queued_byte_capacity: self.max_queued_bytes,
            frame_byte_limit: self.max_frame_bytes,
            server_credit_frame_limit: self.max_server_credit_frames,
            server_credit_byte_limit: self.max_server_credit_bytes,
        }
    }

    fn validate(&self) -> Result<(), OutputGrpcError> {
        self.state_limits()
            .validate()
            .map_err(OutputGrpcError::Protocol)?;
        if self.stream_deadline.is_zero()
            || self.ack_timeout.is_zero()
            || self.max_queued_frames > MAX_QUEUED_FRAMES
            || self.max_queued_bytes > MAX_QUEUED_BYTES
            || self.max_frame_bytes > MAX_OUTPUT_FRAME_BYTES
            || usize::try_from(self.max_server_credit_frames)
                .map_or(true, |frames| frames > MAX_QUEUED_FRAMES)
            || self.max_server_credit_bytes > MAX_QUEUED_BYTES as u64
            || self.stream_deadline > MAX_STREAM_DEADLINE
            || self.ack_timeout > MAX_ACK_TIMEOUT
            || !valid_metadata_value(&self.workload_session_id)
            || !valid_metadata_value(&self.producer_id)
        {
            return Err(OutputGrpcError::InvalidConfiguration(
                "the output gRPC configuration is malformed",
            ));
        }
        Ok(())
    }
}

/// Stable, data-free output delivery failures.
#[derive(Debug)]
pub enum OutputGrpcError {
    InvalidConfiguration(&'static str),
    Protocol(OutputSessionError),
    Spool(SpoolError),
    Unavailable(&'static str),
}

impl fmt::Display for OutputGrpcError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message) | Self::Unavailable(message) => {
                formatter.write_str(message)
            }
            Self::Protocol(error) => error.fmt(formatter),
            Self::Spool(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for OutputGrpcError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Protocol(error) => Some(error),
            Self::Spool(error) => Some(error),
            Self::InvalidConfiguration(_) | Self::Unavailable(_) => None,
        }
    }
}

impl From<OutputSessionError> for OutputGrpcError {
    fn from(value: OutputSessionError) -> Self {
        Self::Protocol(value)
    }
}

impl From<SpoolError> for OutputGrpcError {
    fn from(value: SpoolError) -> Self {
        Self::Spool(value)
    }
}

#[async_trait]
trait OutputStreamIo: Send {
    async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<(), OutputGrpcError>;
    async fn receive(&mut self) -> Result<Option<ExecutionOutputAckV1>, OutputGrpcError>;
    async fn close(&mut self) -> Result<(), OutputGrpcError>;
}

struct TonicOutputStream {
    outbound: Option<mpsc::Sender<ExecutionOutputFrameV1>>,
    inbound: Streaming<ExecutionOutputAckV1>,
}

impl TonicOutputStream {
    async fn open(channel: Channel, config: &OutputGrpcConfig) -> Result<Self, OutputGrpcError> {
        config.validate()?;
        let (outbound, receiver) = mpsc::channel(config.max_queued_frames);
        let mut request = Request::new(ReceiverStream::new(receiver));
        request.set_timeout(config.stream_deadline);
        let workload =
            MetadataValue::try_from(config.workload_session_id.as_str()).map_err(|_| {
                OutputGrpcError::InvalidConfiguration("the output gRPC metadata is malformed")
            })?;
        let producer = MetadataValue::try_from(config.producer_id.as_str()).map_err(|_| {
            OutputGrpcError::InvalidConfiguration("the output gRPC metadata is malformed")
        })?;
        request
            .metadata_mut()
            .insert("x-elitea-workload-session", workload);
        request
            .metadata_mut()
            .insert("x-elitea-producer-id", producer);

        let mut client = ExecutionOutputServiceClient::new(channel)
            .max_encoding_message_size(config.max_frame_bytes)
            .max_decoding_message_size(MAX_ACK_BYTES);
        let response = client
            .publish(request)
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output gRPC stream is unavailable"))?;
        Ok(Self {
            outbound: Some(outbound),
            inbound: response.into_inner(),
        })
    }
}

#[async_trait]
impl OutputStreamIo for TonicOutputStream {
    async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<(), OutputGrpcError> {
        self.outbound
            .as_ref()
            .ok_or(OutputGrpcError::Unavailable(
                "the output gRPC stream is closed",
            ))?
            .send(frame)
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output gRPC stream is unavailable"))
    }

    async fn receive(&mut self) -> Result<Option<ExecutionOutputAckV1>, OutputGrpcError> {
        self.inbound
            .message()
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output gRPC stream is unavailable"))
    }

    async fn close(&mut self) -> Result<(), OutputGrpcError> {
        self.outbound.take();
        Ok(())
    }
}

struct OutputSession<S> {
    io: S,
    spool: Option<EncryptedOutputSpool>,
    state: OutputSessionState,
    config: OutputGrpcConfig,
    terminal_committed: bool,
    failed: bool,
}

/// Validated durable state before any output endpoint is contacted.
///
/// Delivery coordination can inspect, reconcile, exact-replace, or append to
/// the encrypted spool before choosing whether a stream replay is authorized.
pub struct PreparedOutputSpool {
    spool: EncryptedOutputSpool,
    state: OutputSessionState,
    pending: Vec<DurableOutputFrame>,
    config: OutputGrpcConfig,
    reconciled: bool,
}

impl PreparedOutputSpool {
    /// Validate the complete durable spool without opening a network stream.
    ///
    /// This synchronous recovery boundary is intended to run on the worker's
    /// bounded blocking executor.
    ///
    /// # Errors
    ///
    /// Returns a stable error for invalid limits, corrupt/noncanonical frames,
    /// sequence gaps, identity changes, or filesystem failures.
    pub fn prepare(
        mut spool: EncryptedOutputSpool,
        config: OutputGrpcConfig,
    ) -> Result<Self, OutputGrpcError> {
        config.validate()?;
        let pending = spool.pending()?;
        let (state, pending) = OutputSessionState::restore(config.state_limits(), pending)?;
        Ok(Self {
            spool,
            state,
            pending,
            config,
            reconciled: false,
        })
    }

    /// Return a defensive copy of the last restored frame.
    #[must_use]
    pub fn pending_replay_frame(&self) -> Option<ExecutionOutputFrameV1> {
        self.pending.last().map(|frame| frame.message().clone())
    }

    /// Return the bounded number of restored, unacknowledged frames.
    #[must_use]
    pub const fn pending_frame_count(&self) -> usize {
        self.pending.len()
    }

    /// Report whether the spool contains these exact deterministic bytes.
    #[must_use]
    pub fn replays(&self, frame: &ExecutionOutputFrameV1) -> bool {
        let encoded = frame.encode_to_vec();
        self.pending.iter().any(|candidate| {
            candidate.sequence() == frame.sequence && candidate.encoded() == encoded
        })
    }

    /// Persist a new contiguous frame before endpoint availability is assumed.
    ///
    /// # Errors
    ///
    /// Returns a typed protocol, capacity, or spool error without mutating the
    /// in-memory admission state when durable publication fails.
    pub fn persist(&mut self, frame: ExecutionOutputFrameV1) -> Result<u64, OutputGrpcError> {
        self.require_unreconciled()?;
        let mut next_state = self.state.clone();
        let frame = next_state.prepare_new_frame(frame)?;
        let sequence = NonZeroU64::new(frame.sequence()).ok_or(OutputGrpcError::Protocol(
            OutputSessionError::InvalidInput("the output sequence is malformed"),
        ))?;
        self.require_pending_capacity(frame.encoded().len())?;
        self.spool.put(sequence, frame.encoded())?;
        next_state.commit_durable_admission(&frame)?;
        self.state = next_state;
        self.pending.push(frame);
        Ok(sequence.get())
    }

    /// Retire a spool only when an authenticated control receipt covers its
    /// complete durable prefix.
    ///
    /// # Errors
    ///
    /// Returns authorization failure when the watermark does not cover every
    /// pending frame.
    pub fn reconcile_pending_through(&mut self, sequence: u64) -> Result<(), OutputGrpcError> {
        self.require_unreconciled()?;
        let sequence = NonZeroU64::new(sequence)
            .filter(|sequence| i64::try_from(sequence.get()).is_ok())
            .ok_or(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "the output reconciliation watermark is malformed",
            )))?;
        if self.pending.is_empty() {
            return Ok(());
        }
        if self
            .pending
            .last()
            .is_some_and(|frame| sequence.get() < frame.sequence())
        {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the output reconciliation watermark does not cover the durable spool",
                ),
            ));
        }
        self.spool.acknowledge_through(sequence)?;
        self.pending.clear();
        self.reconciled = true;
        Ok(())
    }

    /// CAS one exact pending frame without changing its stream binding.
    ///
    /// # Errors
    ///
    /// Returns authorization failure unless exactly one byte-identical frame is
    /// pending and the replacement retains its sequence and complete binding.
    pub fn replace_pending_exact(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_single_expected(expected)?;
        if replacement.sequence != expected.sequence {
            return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "the terminal replacement changed its output sequence",
            )));
        }
        self.state.validate_replacement_binding(replacement)?;
        let (state, pending) = self.restore_replacement(replacement)?;
        self.replace_pending_bytes(expected, replacement)?;
        self.state = state;
        self.pending = pending;
        Ok(())
    }

    /// CAS an exact old-fence frame to the canonical fresh-fence cancellation.
    ///
    /// # Errors
    ///
    /// Returns authorization failure unless every recovery binding and
    /// canonical cancellation field matches the current contract.
    pub fn replace_pending_cancelled_recovery(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_single_expected(expected)?;
        validate_recovery_rebind(expected, replacement, RecoveryKind::Cancelled)?;
        self.replace_rebound_pending(expected, replacement)
    }

    /// CAS an exact old-fence terminal to the canonical ambiguous failure.
    ///
    /// # Errors
    ///
    /// Returns authorization failure unless every recovery binding and
    /// canonical failure field matches the current contract.
    pub fn replace_pending_ambiguous_recovery(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_single_expected(expected)?;
        validate_recovery_rebind(expected, replacement, RecoveryKind::Ambiguous)?;
        self.replace_rebound_pending(expected, replacement)
    }

    /// Rebind an admitted read-tool terminal without changing its payload.
    /// A claim created after its deadline can instead seal a deadline failure.
    pub(crate) fn replace_pending_toolkit_terminal_recovery(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_single_expected(expected)?;
        let mut rebound = expected.clone();
        rebound.fence.clone_from(&replacement.fence);
        rebound.claim_handoff_watermark = replacement.claim_handoff_watermark;
        let kind = if &rebound == replacement {
            RecoveryKind::Terminal
        } else {
            RecoveryKind::Deadline
        };
        validate_recovery_rebind(expected, replacement, kind)?;
        self.replace_rebound_pending(expected, replacement)
    }

    /// Connect a fresh session only while the encrypted spool is empty.
    ///
    /// Restored progress requires an owned replay coordinator which can retain
    /// its exact ACK proof across caller cancellation. Restored terminals use
    /// the separate settlement-authority replay path. This generic entry point
    /// therefore never transmits pending durable output.
    ///
    /// # Errors
    ///
    /// Returns a stable authorization error for any pending frame before
    /// opening the endpoint, or a typed transport error for a fresh session.
    pub async fn connect(self, channel: Channel) -> Result<OutputGrpcSession, OutputGrpcError> {
        self.require_empty_session_start()?;
        let max_frame_bytes = self.config.max_frame_bytes;
        let io = TonicOutputStream::open(channel, &self.config).await?;
        let inner = OutputSession::from_prepared(io, self).await?;
        Ok(spawn_output_actor(inner, max_frame_bytes))
    }

    /// Start one owned replay of the sole exact nonterminal frame.
    ///
    /// Endpoint opening completes before the replay task starts, so cancelling
    /// this constructor cannot abandon a transmitted frame. Once returned, the
    /// session owns replay through a bound ACK, rejection, timeout, or explicit
    /// close; cancelling a result waiter does not cancel that task. Dropping
    /// the owner abandons local continuation and requires Main claim/handoff
    /// recovery, so the future publisher must keep session and cursor
    /// inseparable and use explicit close on its normal paths.
    ///
    /// # Errors
    ///
    /// Returns a stable binding error before network access unless the spool
    /// contains exactly the expected nonterminal frame, or a typed endpoint
    /// error before replay starts.
    #[allow(dead_code)] // Consumed by the capability-disabled progress publisher.
    pub(crate) async fn start_progress_replay(
        self,
        channel: Channel,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<ProgressReplaySession, OutputGrpcError> {
        self.require_progress_replay(expected)?;
        let io = TonicOutputStream::open(channel, &self.config).await?;
        Ok(spawn_progress_replay(self, io, expected))
    }

    #[cfg(test)]
    fn start_progress_replay_over<S: OutputStreamIo + 'static>(
        self,
        io: S,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<ProgressReplaySession, OutputGrpcError> {
        self.require_progress_replay(expected)?;
        Ok(spawn_progress_replay(self, io, expected))
    }

    fn require_progress_replay(
        &self,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_single_expected(expected)?;
        if expected.terminal {
            return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "terminal output does not produce progress replay authority",
            )));
        }
        Ok(())
    }

    /// Replay the sole exact terminal and return settlement authority only
    /// after Main's bound ACK removes it from the durable spool.
    ///
    /// This crate-private operation is consumed only by the owned delivery
    /// coordinator. Callers must not regenerate or append a terminal while a
    /// replay attempt is in flight.
    ///
    /// # Errors
    ///
    /// Returns a stable binding, transport, ACK, or spool error. Before a
    /// bound ACK, the exact terminal remains durable for a later reopener.
    #[allow(dead_code)] // Consumed by the next accepted-output coordinator slice.
    pub(crate) async fn replay_terminal(
        self,
        channel: Channel,
        verified: &impl VerifiedExecutionCommand,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        self.require_single_expected(expected)?;
        let pending = PendingTerminalSettlement::new(verified, expected)?;
        let io = TonicOutputStream::open(channel, &self.config).await?;
        self.replay_terminal_over(io, pending, expected.sequence)
            .await
    }

    #[allow(dead_code)] // Production caller lands with replay_terminal above.
    async fn replay_terminal_over<S: OutputStreamIo>(
        self,
        io: S,
        pending: PendingTerminalSettlement,
        expected_sequence: u64,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        let mut session = OutputSession::from_prepared(io, self).await?;
        if !session.terminal_committed || session.state.acknowledged_sequence() != expected_sequence
        {
            return Err(OutputGrpcError::Unavailable(
                "the terminal replay did not reach its bound durable ACK",
            ));
        }
        // A close failure cannot revoke the already applied durable ACK.
        let _ignored = session.io.close().await;
        Ok(pending.into_acked())
    }

    fn require_single_expected(
        &self,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        self.require_unreconciled()?;
        if self.pending.len() != 1 || !self.replays(expected) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the durable output spool changed before recovery",
                ),
            ));
        }
        Ok(())
    }

    fn require_empty_session_start(&self) -> Result<(), OutputGrpcError> {
        self.require_unreconciled()?;
        if !self.pending.is_empty() {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "pending output requires the owned recovery coordinator",
                ),
            ));
        }
        Ok(())
    }

    fn require_unreconciled(&self) -> Result<(), OutputGrpcError> {
        if self.reconciled {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the durable output spool was already reconciled",
                ),
            ));
        }
        Ok(())
    }

    fn require_pending_capacity(&self, encoded_bytes: usize) -> Result<(), OutputGrpcError> {
        let pending_bytes = self.pending.iter().try_fold(0usize, |total, frame| {
            total.checked_add(frame.encoded().len())
        });
        if self.pending.len() >= self.config.max_queued_frames
            || pending_bytes
                .and_then(|total| total.checked_add(encoded_bytes))
                .is_none_or(|total| total > self.config.max_queued_bytes)
        {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::ResourceExhausted(
                    "the durable output queue capacity was exceeded",
                ),
            ));
        }
        Ok(())
    }

    fn replace_pending_bytes(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        let sequence = NonZeroU64::new(expected.sequence).ok_or(OutputGrpcError::Protocol(
            OutputSessionError::InvalidInput("the replacement output sequence is malformed"),
        ))?;
        self.spool.replace_exact(
            sequence,
            &expected.encode_to_vec(),
            &replacement.encode_to_vec(),
        )?;
        Ok(())
    }

    fn restore_replacement(
        &self,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(OutputSessionState, Vec<DurableOutputFrame>), OutputGrpcError> {
        let restored = vec![crate::spool::SpooledFrame {
            sequence: NonZeroU64::new(replacement.sequence).ok_or(OutputGrpcError::Protocol(
                OutputSessionError::InvalidInput("the replacement output sequence is malformed"),
            ))?,
            payload: replacement.encode_to_vec(),
        }];
        OutputSessionState::restore(self.config.state_limits(), restored)
            .map_err(OutputGrpcError::Protocol)
    }

    fn replace_rebound_pending(
        &mut self,
        expected: &ExecutionOutputFrameV1,
        replacement: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputGrpcError> {
        if replacement.encoded_len() > self.config.max_frame_bytes {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::ResourceExhausted(
                    "the replacement output frame exceeds the transport limit",
                ),
            ));
        }
        let (state, pending) = self.restore_replacement(replacement)?;
        self.replace_pending_bytes(expected, replacement)?;
        self.state = state;
        self.pending = pending;
        Ok(())
    }
}

impl<S: OutputStreamIo> OutputSession<S> {
    #[cfg(test)]
    async fn open(
        io: S,
        spool: EncryptedOutputSpool,
        config: OutputGrpcConfig,
    ) -> Result<Self, OutputGrpcError> {
        let prepared = PreparedOutputSpool::prepare(spool, config)?;
        Self::from_prepared(io, prepared).await
    }

    async fn from_prepared(io: S, prepared: PreparedOutputSpool) -> Result<Self, OutputGrpcError> {
        let (_shutdown, mut shutdown) = watch::channel(false);
        Self::from_prepared_with_shutdown(io, prepared, &mut shutdown).await
    }

    async fn from_prepared_with_shutdown(
        io: S,
        prepared: PreparedOutputSpool,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<Self, OutputGrpcError> {
        prepared.require_unreconciled()?;
        let PreparedOutputSpool {
            spool,
            state,
            pending: restored,
            config,
            reconciled: _,
        } = prepared;
        let mut session = Self {
            io,
            spool: Some(spool),
            state,
            config,
            terminal_committed: false,
            failed: false,
        };
        let bootstrap = match timeout(
            session.config.ack_timeout,
            session.receive_ack_or_shutdown(shutdown),
        )
        .await
        {
            Ok(result) => result?,
            Err(_) => {
                return Err(OutputGrpcError::Unavailable(
                    "the output bootstrap ACK deadline was exceeded",
                ));
            }
        };
        let plan = session.state.validate_bootstrap_ack(&bootstrap)?;
        session.state.commit_ack(plan);
        for frame in restored {
            session.state.queue_replay(&frame)?;
            session.transmit_and_commit(&frame, shutdown).await?;
            if frame.message().terminal {
                session.terminal_committed = true;
            }
        }
        Ok(session)
    }

    #[cfg(test)]
    async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<u64, OutputGrpcError> {
        let (_shutdown, mut receiver) = watch::channel(false);
        self.send_with_shutdown(frame, &mut receiver).await
    }

    async fn send_with_shutdown(
        &mut self,
        frame: ExecutionOutputFrameV1,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<u64, OutputGrpcError> {
        if self.failed {
            return Err(OutputGrpcError::Unavailable(
                "the output gRPC session is unavailable",
            ));
        }
        let result = self.send_once(frame, shutdown).await;
        if result.is_err() {
            self.failed = true;
        }
        result
    }

    async fn send_once(
        &mut self,
        frame: ExecutionOutputFrameV1,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<u64, OutputGrpcError> {
        if self.terminal_committed {
            return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "the logical output stream already committed a terminal frame",
            )));
        }
        let mut next_state = self.state.clone();
        let frame = next_state.prepare_new_frame(frame)?;
        let sequence = frame.sequence();
        let nonzero_sequence = NonZeroU64::new(sequence).ok_or(OutputGrpcError::Protocol(
            OutputSessionError::InvalidInput("the output sequence is malformed"),
        ))?;
        let encoded = frame.encoded().to_vec();
        self.with_spool(move |spool| spool.put(nonzero_sequence, &encoded))
            .await?;
        next_state.commit_persisted_admission(&frame)?;
        self.state = next_state;
        self.transmit_and_commit(&frame, shutdown).await?;
        if frame.message().terminal {
            self.terminal_committed = true;
        }
        Ok(sequence)
    }

    async fn transmit_and_commit(
        &mut self,
        frame: &DurableOutputFrame,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<(), OutputGrpcError> {
        let ack_timeout = self.config.ack_timeout;
        match timeout(
            ack_timeout,
            self.transmit_and_commit_within_deadline(frame, shutdown),
        )
        .await
        {
            Ok(result) => result,
            Err(_) => Err(OutputGrpcError::Unavailable(
                "the output ACK deadline was exceeded",
            )),
        }
    }

    async fn transmit_and_commit_within_deadline(
        &mut self,
        frame: &DurableOutputFrame,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<(), OutputGrpcError> {
        while !self.state.has_transmission_credit(frame) {
            let ack = self.receive_ack_or_shutdown(shutdown).await?;
            let plan = self.state.validate_ack(&ack)?;
            self.apply_ack(plan).await?;
        }
        self.state.reserve_transmission(frame)?;
        tokio::select! {
            biased;
            _ = shutdown.changed() => {
                return Err(OutputGrpcError::Unavailable(
                    "the output gRPC session is closing",
                ));
            }
            result = self.io.send(frame.message().clone()) => result?,
        }
        self.state.complete_transmission(frame)?;
        loop {
            let ack = self.receive_ack_or_shutdown(shutdown).await?;
            let plan = self.state.validate_ack(&ack)?;
            let acknowledged = plan.acknowledged_sequence();
            self.apply_ack(plan).await?;
            if acknowledged == frame.sequence() {
                return Ok(());
            }
        }
    }

    async fn apply_ack(
        &mut self,
        plan: super::output_session::AckPlan,
    ) -> Result<(), OutputGrpcError> {
        if let Some(sequence) = plan.retire_spool_through() {
            let nonzero_sequence = NonZeroU64::new(sequence).ok_or(OutputGrpcError::Protocol(
                OutputSessionError::InvalidInput("the output ACK sequence is malformed"),
            ))?;
            self.with_spool(move |spool| spool.acknowledge_through(nonzero_sequence))
                .await?;
        }
        self.state.commit_ack(plan);
        Ok(())
    }

    async fn receive_ack_or_shutdown(
        &mut self,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<ExecutionOutputAckV1, OutputGrpcError> {
        if *shutdown.borrow() {
            return Err(OutputGrpcError::Unavailable(
                "the output gRPC session is closing",
            ));
        }
        tokio::select! {
            biased;
            _ = shutdown.changed() => {
                Err(OutputGrpcError::Unavailable("the output gRPC session is closing"))
            }
            result = self.io.receive() => match result? {
                Some(ack) => Ok(ack),
                None => Err(OutputGrpcError::Unavailable(
                    "the output gRPC stream closed before its bound ACK",
                )),
            }
        }
    }

    async fn with_spool<T, F>(&mut self, operation: F) -> Result<T, OutputGrpcError>
    where
        T: Send + 'static,
        F: FnOnce(&mut EncryptedOutputSpool) -> Result<T, SpoolError> + Send + 'static,
    {
        let spool = self.spool.take().ok_or(OutputGrpcError::Unavailable(
            "the output spool ownership is unavailable",
        ))?;
        let (spool, result) = run_spool(spool, operation).await?;
        self.spool = Some(spool);
        result.map_err(OutputGrpcError::Spool)
    }
}

enum SessionCommand {
    Send {
        frame: Box<ExecutionOutputFrameV1>,
        permit: OwnedSemaphorePermit,
        response: oneshot::Sender<Result<u64, OutputGrpcError>>,
    },
}

/// One ordered, execution-bound output stream over a caller-verified channel.
///
/// An owned task serializes durable work after command admission. Cancelling a
/// caller's wait cannot cancel a spool write, steal a preceding frame's ACK, or
/// strand the session halfway through ACK deletion.
pub struct OutputGrpcSession {
    commands: mpsc::Sender<SessionCommand>,
    admission: Arc<Semaphore>,
    acknowledged_sequence: Arc<AtomicU64>,
    completed_progress_sequence: Arc<AtomicU64>,
    completed_progress: Arc<Mutex<Option<CompletedProgressOutcome>>>,
    max_frame_bytes: usize,
    shutdown: watch::Sender<bool>,
    task: Option<JoinHandle<()>>,
}

/// Proof that Main durably acknowledged one exact terminal frame.
///
/// The type has no public constructor, accessors, `Clone`, or `Debug`. It can
/// only be produced by [`OutputGrpcSession::send_terminal`] after the bound ACK
/// is applied and is consumed by the semantic settlement operation.
pub struct DurablyAckedTerminal {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    proposal: SettlementProposalV1,
    stable_delivery_id: String,
    exact_signed_envelope: Box<[u8]>,
}

/// Proof that Main durably acknowledged one exact nonterminal frame.
///
/// The type has no public constructor, accessors, `Clone`, or `Debug`. It can
/// only be produced by a live send or owned restored replay after the bound
/// ACK is applied. The execution output cursor consumes it to advance exactly
/// the frame whose canonical protobuf bytes were sent.
pub struct DurablyAckedProgress {
    sequence: u64,
    frame_sha256: [u8; 32],
    _permit: Option<OwnedSemaphorePermit>,
}

/// Closed result of one owned exact progress replay.
///
/// Neither variant is constructible outside this transport. The future output
/// publisher consumes the ACK proof to advance its cursor or the rejection
/// proof to authorize one same-sequence terminal replacement.
#[allow(dead_code)] // Consumed by the capability-disabled progress publisher.
pub(crate) enum ProgressReplayDecision {
    Acknowledged(DurablyAckedProgress),
    Rejected(FrameBoundProgressRejection),
}

/// Cancellation-safe owner for one restored progress replay attempt.
///
/// The replay task stores its exact decision before notifying waiters. A
/// cancelled `wait` can be retried on the same value. Callers commit the proof
/// synchronously, then close the session before reopening the spool. Dropping
/// this owner is intentionally not a local proof-recovery operation: the
/// capability-disabled publisher must discard its cursor with the session and
/// rely on Main's next claim/handoff recovery.
#[allow(dead_code)] // Consumed by the capability-disabled progress publisher.
#[must_use = "the replay session must be observed and explicitly closed"]
pub(crate) struct ProgressReplaySession {
    completion: watch::Receiver<bool>,
    result: Arc<Mutex<Option<Result<ProgressReplayDecision, OutputGrpcError>>>>,
    shutdown: watch::Sender<bool>,
    task: Option<JoinHandle<()>>,
}

#[allow(dead_code)] // Consumed by the capability-disabled progress publisher.
impl ProgressReplaySession {
    /// Wait for the owned replay without gaining cancellation authority over it.
    ///
    /// # Errors
    ///
    /// Returns the replay's stable transport/protocol failure, or a stable
    /// ownership error if the result was already consumed or its task failed.
    pub(crate) async fn wait(&mut self) -> Result<ProgressReplayDecision, OutputGrpcError> {
        while !*self.completion.borrow() {
            self.completion.changed().await.map_err(|_| {
                OutputGrpcError::Unavailable("the progress replay task ended without a result")
            })?;
        }
        self.result
            .lock()
            .map_err(|_| OutputGrpcError::Unavailable("the progress replay result is unavailable"))?
            .take()
            .ok_or(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the progress replay result was already consumed",
                ),
            ))?
    }

    /// Signal shutdown and observe the owned replay task.
    ///
    /// A bound decision already stored for the caller is not revoked by a later
    /// stream-close error. Cancelling this wait retains the join handle so a
    /// later call can finish the explicit close.
    pub(crate) async fn close(&mut self) -> Result<(), OutputGrpcError> {
        let _ignored = self.shutdown.send(true);
        let outcome = match self.task.as_mut() {
            Some(task) => Some(task.await),
            None => None,
        };
        if let Some(outcome) = outcome {
            self.task = None;
            outcome.map_err(|_| {
                OutputGrpcError::Unavailable("the progress replay task is unavailable")
            })?;
        }
        Ok(())
    }
}

impl Drop for ProgressReplaySession {
    fn drop(&mut self) {
        let _ignored = self.shutdown.send(true);
    }
}

/// Main-owned winner which may replace one exact rejected progress frame.
///
/// This transport-local value records only the authenticated decision. The
/// execution lifecycle remains responsible for final lease priority, business
/// error mapping, and sampling the terminal occurrence time.
#[derive(Clone, Copy, Eq, PartialEq)]
pub(crate) enum ProgressRejectionWinner {
    Cancelled,
    DeadlineExceeded,
}

/// Exact frame-bound Main decision authorizing a same-sequence failure.
///
/// The type has no public constructor, accessors, `Clone`, or `Debug`. A plain
/// transport error is never replacement authority; only the output actor can
/// mint this proof after validating Main's complete rejection ACK.
pub(crate) struct FrameBoundProgressRejection {
    sequence: u64,
    frame_sha256: [u8; 32],
    winner: ProgressRejectionWinner,
    _permit: Option<OwnedSemaphorePermit>,
}

impl FrameBoundProgressRejection {
    pub(crate) fn into_binding(self) -> (u64, [u8; 32], ProgressRejectionWinner) {
        (self.sequence, self.frame_sha256, self.winner)
    }
}

struct PendingProgress {
    sequence: u64,
    frame_sha256: [u8; 32],
}

impl PendingProgress {
    fn new(frame: &ExecutionOutputFrameV1) -> Self {
        Self {
            sequence: frame.sequence,
            frame_sha256: progress_frame_sha256(frame),
        }
    }

    fn into_completed(self, permit: OwnedSemaphorePermit) -> CompletedProgressOutcome {
        CompletedProgressOutcome::Acknowledged(DurablyAckedProgress {
            sequence: self.sequence,
            frame_sha256: self.frame_sha256,
            _permit: Some(permit),
        })
    }

    fn into_rejected(
        self,
        permit: Option<OwnedSemaphorePermit>,
        winner: ProgressRejectionWinner,
    ) -> FrameBoundProgressRejection {
        FrameBoundProgressRejection {
            sequence: self.sequence,
            frame_sha256: self.frame_sha256,
            winner,
            _permit: permit,
        }
    }
}

enum CompletedProgressOutcome {
    Acknowledged(DurablyAckedProgress),
    Rejected(FrameBoundProgressRejection),
}

impl CompletedProgressOutcome {
    fn matches(&self, sequence: u64, frame_sha256: &[u8; 32]) -> bool {
        match self {
            Self::Acknowledged(acknowledged) => {
                acknowledged.sequence == sequence && acknowledged.frame_sha256 == *frame_sha256
            }
            Self::Rejected(rejected) => {
                rejected.sequence == sequence && rejected.frame_sha256 == *frame_sha256
            }
        }
    }

    const fn sequence(&self) -> u64 {
        match self {
            Self::Acknowledged(acknowledged) => acknowledged.sequence,
            Self::Rejected(rejected) => rejected.sequence,
        }
    }
}

fn progress_rejection_winner(error: &OutputGrpcError) -> Option<ProgressRejectionWinner> {
    match error {
        OutputGrpcError::Protocol(OutputSessionError::CancellationWon) => {
            Some(ProgressRejectionWinner::Cancelled)
        }
        OutputGrpcError::Protocol(OutputSessionError::DeadlineWon) => {
            Some(ProgressRejectionWinner::DeadlineExceeded)
        }
        _ => None,
    }
}

impl DurablyAckedProgress {
    pub(crate) fn into_binding(self) -> (u64, [u8; 32]) {
        (self.sequence, self.frame_sha256)
    }
}

pub(crate) fn progress_frame_sha256(frame: &ExecutionOutputFrameV1) -> [u8; 32] {
    let value = digest::digest(&digest::SHA256, &frame.encode_to_vec());
    let mut binding = [0_u8; 32];
    binding.copy_from_slice(value.as_ref());
    binding
}

/// Validated terminal settlement material which is not authority until the
/// corresponding frame receives its bound durable ACK.
struct PendingTerminalSettlement {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    proposal: SettlementProposalV1,
    stable_delivery_id: String,
    exact_signed_envelope: Box<[u8]>,
}

impl PendingTerminalSettlement {
    fn new(
        verified: &impl VerifiedExecutionCommand,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<Self, OutputGrpcError> {
        validate_terminal_settlement_binding(frame)?;
        let identity = frame.identity.clone().ok_or(OutputGrpcError::Protocol(
            OutputSessionError::InvalidInput(
                "the terminal output identity is missing before settlement",
            ),
        ))?;
        let fence = frame.fence.clone().ok_or(OutputGrpcError::Protocol(
            OutputSessionError::InvalidInput(
                "the terminal output fence is missing before settlement",
            ),
        ))?;
        let proposal = frame
            .settlement_proposal
            .clone()
            .ok_or(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "the terminal settlement proposal is missing",
            )))?;
        if !terminal_identity_matches_command(&identity, verified) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the terminal output identity does not match its verified command",
                ),
            ));
        }
        Ok(Self {
            identity,
            fence,
            proposal,
            stable_delivery_id: verified.command().idempotency_key.clone(),
            exact_signed_envelope: verified.exact_signed_envelope().into(),
        })
    }

    fn into_acked(self) -> DurablyAckedTerminal {
        DurablyAckedTerminal {
            identity: self.identity,
            fence: self.fence,
            proposal: self.proposal,
            stable_delivery_id: self.stable_delivery_id,
            exact_signed_envelope: self.exact_signed_envelope,
        }
    }
}

impl DurablyAckedTerminal {
    pub(crate) fn into_settlement_parts(
        self,
    ) -> (
        ExecutionIdentityV1,
        ExecutionFenceV1,
        SettlementProposalV1,
        String,
        Box<[u8]>,
    ) {
        (
            self.identity,
            self.fence,
            self.proposal,
            self.stable_delivery_id,
            self.exact_signed_envelope,
        )
    }
}

#[cfg(test)]
pub(crate) fn test_acknowledged_terminal(
    verified: &VerifiedAgentCommand,
    frame: &ExecutionOutputFrameV1,
) -> Result<DurablyAckedTerminal, OutputGrpcError> {
    PendingTerminalSettlement::new(verified, frame).map(PendingTerminalSettlement::into_acked)
}

#[cfg(test)]
pub(crate) fn test_acknowledged_progress(
    frame: &ExecutionOutputFrameV1,
) -> Result<DurablyAckedProgress, OutputGrpcError> {
    if frame.terminal {
        return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
            "terminal output does not produce progress ACK authority",
        )));
    }
    Ok(DurablyAckedProgress {
        sequence: frame.sequence,
        frame_sha256: progress_frame_sha256(frame),
        _permit: None,
    })
}

#[cfg(test)]
pub(crate) fn test_rejected_progress(
    frame: &ExecutionOutputFrameV1,
    winner: ProgressRejectionWinner,
) -> Result<FrameBoundProgressRejection, OutputGrpcError> {
    if frame.terminal {
        return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
            "terminal output does not produce progress rejection authority",
        )));
    }
    Ok(PendingProgress::new(frame).into_rejected(None, winner))
}

impl OutputGrpcSession {
    /// Open a bounded fresh stream and validate bootstrap credit before
    /// returning admission to the caller.
    ///
    /// A nonempty encrypted spool is rejected before network access. Restored
    /// output must enter its typed owned recovery coordinator so a durable ACK
    /// cannot lose its continuation authority to caller cancellation.
    ///
    /// The supplied channel must already enforce the deployment's mTLS trust
    /// policy. TLS material loading and endpoint construction are owned by the
    /// deployment-composition slice.
    ///
    /// # Errors
    ///
    /// Returns a stable [`OutputGrpcError`] when configuration, restored state,
    /// bootstrap credit, transport, or spool recovery fails.
    pub async fn open(
        channel: Channel,
        spool: EncryptedOutputSpool,
        config: OutputGrpcConfig,
    ) -> Result<Self, OutputGrpcError> {
        let prepared =
            tokio::task::spawn_blocking(move || PreparedOutputSpool::prepare(spool, config))
                .await
                .map_err(|_| {
                    OutputGrpcError::Unavailable("the output spool task is unavailable")
                })??;
        prepared.connect(channel).await
    }

    /// Persist, transmit, authenticate the bound ACK, then retire the durable
    /// spool prefix. Calls are serialized by exclusive ownership.
    ///
    /// # Errors
    ///
    /// Returns a typed protocol, spool, or transport error. A failure never
    /// deletes an unacknowledged frame. When Main's exact cancellation or
    /// deadline decision wins, the typed error remains descriptive while the
    /// separate opaque proof is retained for the owned delivery coordinator.
    pub async fn send(
        &mut self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedProgress, OutputGrpcError> {
        if frame.terminal {
            return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                "terminal output requires the settlement-authority send path",
            )));
        }
        self.validate_frame_size(frame)?;
        let pending = PendingProgress::new(frame);
        let sequence = self.send_frame(frame.clone()).await?;
        if sequence != pending.sequence {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the progress ACK does not cover the transmitted frame",
                ),
            ));
        }
        self.take_completed_acknowledgement(pending.sequence, &pending.frame_sha256)?
            .ok_or(OutputGrpcError::Unavailable(
                "the durable progress ACK proof is unavailable",
            ))
    }

    /// Consume a durable ACK proof retained after a cancelled send waiter.
    ///
    /// The supplied frame must be byte-identical to the frame whose ACK was
    /// applied. A mismatched frame cannot consume the retained proof, and no
    /// proof exists when cancellation happened before durable completion.
    #[allow(dead_code)] // Consumed by the capability-disabled progress coordinator.
    pub(crate) fn take_acknowledged_progress(
        &self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<Option<DurablyAckedProgress>, OutputGrpcError> {
        let pending = PendingProgress::new(frame);
        self.take_completed_acknowledgement(pending.sequence, &pending.frame_sha256)
    }

    fn take_completed_acknowledgement(
        &self,
        sequence: u64,
        frame_sha256: &[u8; 32],
    ) -> Result<Option<DurablyAckedProgress>, OutputGrpcError> {
        let mut completed = self.completed_progress.lock().map_err(|_| {
            OutputGrpcError::Unavailable("the durable progress ACK proof is unavailable")
        })?;
        let Some(current) = completed.as_ref() else {
            return Ok(None);
        };
        if !current.matches(sequence, frame_sha256) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the retained progress ACK does not match the expected frame",
                ),
            ));
        }
        if !matches!(current, CompletedProgressOutcome::Acknowledged(_)) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the retained progress outcome is not a durable ACK",
                ),
            ));
        }
        let Some(CompletedProgressOutcome::Acknowledged(acknowledged)) = completed.take() else {
            return Err(OutputGrpcError::Unavailable(
                "the durable progress ACK proof is unavailable",
            ));
        };
        Ok(Some(acknowledged))
    }

    /// Consume an exact frame-bound cancellation or deadline decision retained
    /// after the send waiter completed or was cancelled.
    ///
    /// A plain transport error cannot authorize replacement. The supplied
    /// frame must be byte-identical to the frame rejected by Main.
    #[allow(dead_code)] // Consumed by the capability-disabled progress coordinator.
    pub(crate) fn take_rejected_progress(
        &self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<Option<FrameBoundProgressRejection>, OutputGrpcError> {
        let pending = PendingProgress::new(frame);
        let mut completed = self.completed_progress.lock().map_err(|_| {
            OutputGrpcError::Unavailable("the durable progress decision proof is unavailable")
        })?;
        let Some(current) = completed.as_ref() else {
            return Ok(None);
        };
        if !current.matches(pending.sequence, &pending.frame_sha256) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the retained progress decision does not match the expected frame",
                ),
            ));
        }
        if !matches!(current, CompletedProgressOutcome::Rejected(_)) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the retained progress outcome is not a rejection",
                ),
            ));
        }
        let Some(CompletedProgressOutcome::Rejected(rejected)) = completed.take() else {
            return Err(OutputGrpcError::Unavailable(
                "the durable progress decision proof is unavailable",
            ));
        };
        Ok(Some(rejected))
    }

    /// Consume whichever exact bound progress decision the actor retained.
    ///
    /// The fresh progress publisher uses this after a response-channel loss so
    /// a durable ACK and a Main cancellation/deadline winner remain distinct
    /// from an ordinary uncertain transport failure.
    #[allow(dead_code)] // Consumed by the capability-disabled progress publisher.
    pub(crate) fn take_progress_decision(
        &self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<Option<ProgressReplayDecision>, OutputGrpcError> {
        let pending = PendingProgress::new(frame);
        let mut completed = self.completed_progress.lock().map_err(|_| {
            OutputGrpcError::Unavailable("the durable progress decision proof is unavailable")
        })?;
        let Some(current) = completed.as_ref() else {
            return Ok(None);
        };
        if !current.matches(pending.sequence, &pending.frame_sha256) {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(
                    "the retained progress decision does not match the expected frame",
                ),
            ));
        }
        let Some(current) = completed.take() else {
            return Err(OutputGrpcError::Unavailable(
                "the durable progress decision proof is unavailable",
            ));
        };
        Ok(Some(match current {
            CompletedProgressOutcome::Acknowledged(acknowledged) => {
                ProgressReplayDecision::Acknowledged(acknowledged)
            }
            CompletedProgressOutcome::Rejected(rejected) => {
                ProgressReplayDecision::Rejected(rejected)
            }
        }))
    }

    async fn send_frame(&mut self, frame: ExecutionOutputFrameV1) -> Result<u64, OutputGrpcError> {
        self.validate_frame_size(&frame)?;
        let permit = Arc::clone(&self.admission)
            .acquire_owned()
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output session task is unavailable"))?;
        let (response, result) = oneshot::channel();
        self.commands
            .send(SessionCommand::Send {
                frame: Box::new(frame),
                permit,
                response,
            })
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output session task is unavailable"))?;
        result
            .await
            .map_err(|_| OutputGrpcError::Unavailable("the output session task is unavailable"))?
    }

    fn validate_frame_size(&self, frame: &ExecutionOutputFrameV1) -> Result<(), OutputGrpcError> {
        if frame.encoded_len() > self.max_frame_bytes {
            return Err(OutputGrpcError::Protocol(
                OutputSessionError::ResourceExhausted(
                    "the output frame exceeds the transport limit",
                ),
            ));
        }
        Ok(())
    }

    /// Persist and publish one terminal frame, returning settlement authority
    /// only after Main's bound durable ACK retires the local spool entry.
    ///
    /// # Errors
    ///
    /// Returns a typed protocol error for a nonterminal/incomplete frame, or
    /// the same spool/transport failures as [`Self::send`].
    pub async fn send_terminal(
        &mut self,
        verified: &VerifiedAgentCommand,
        frame: ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        let pending = PendingTerminalSettlement::new(verified, &frame)?;
        self.send_frame(frame).await?;
        Ok(pending.into_acked())
    }

    /// Return the highest sequence whose bound Main ACK was durably applied to
    /// the local spool.
    #[must_use]
    pub fn acknowledged_sequence(&self) -> u64 {
        self.acknowledged_sequence.load(Ordering::Acquire)
    }

    /// Return the sequence whose exact ACK or bound rejection proof is ready.
    ///
    /// The actor publishes this value only after storing the corresponding
    /// one-shot proof. It is a cancellation-recovery signal, not authority by
    /// itself.
    #[allow(dead_code)] // Consumed by the capability-disabled progress coordinator.
    pub(crate) fn completed_progress_sequence(&self) -> u64 {
        self.completed_progress_sequence.load(Ordering::Acquire)
    }

    /// Close the outbound half without changing any unacknowledged spool data.
    ///
    /// # Errors
    ///
    /// Returns a stable transport error if the output driver cannot close.
    pub async fn close(&mut self) -> Result<(), OutputGrpcError> {
        let _ignored = self.shutdown.send(true);
        let outcome = match self.task.as_mut() {
            Some(task) => Some(task.await),
            None => None,
        };
        if let Some(outcome) = outcome {
            self.task = None;
            outcome.map_err(|_| {
                OutputGrpcError::Unavailable("the output session task is unavailable")
            })?;
        }
        Ok(())
    }
}

impl Drop for OutputGrpcSession {
    fn drop(&mut self) {
        let _ignored = self.shutdown.send(true);
    }
}

fn spawn_output_actor<S>(mut inner: OutputSession<S>, max_frame_bytes: usize) -> OutputGrpcSession
where
    S: OutputStreamIo + 'static,
{
    let (commands, mut receiver) = mpsc::channel(1);
    let admission = Arc::new(Semaphore::new(1));
    let (shutdown, mut shutdown_receiver) = watch::channel(false);
    let acknowledged_sequence = Arc::new(AtomicU64::new(inner.state.acknowledged_sequence()));
    let actor_acknowledged = Arc::clone(&acknowledged_sequence);
    let completed_progress_sequence = Arc::new(AtomicU64::new(0));
    let actor_completed_progress_sequence = Arc::clone(&completed_progress_sequence);
    let completed_progress = Arc::new(Mutex::new(None));
    let actor_completed_progress = Arc::clone(&completed_progress);
    let task = tokio::spawn(async move {
        loop {
            if *shutdown_receiver.borrow() {
                break;
            }
            let command = tokio::select! {
                biased;
                _ = shutdown_receiver.changed() => {
                    break;
                }
                command = receiver.recv() => match command {
                    Some(command) => command,
                    None => break,
                }
            };
            match command {
                SessionCommand::Send {
                    frame,
                    permit,
                    response,
                } => {
                    let pending_progress = (!frame.terminal).then(|| PendingProgress::new(&frame));
                    let result = inner
                        .send_with_shutdown(*frame, &mut shutdown_receiver)
                        .await;
                    let completed = pending_progress.and_then(|pending| match &result {
                        Ok(_) => Some(pending.into_completed(permit)),
                        Err(error) => progress_rejection_winner(error).map(|winner| {
                            CompletedProgressOutcome::Rejected(
                                pending.into_rejected(Some(permit), winner),
                            )
                        }),
                    });
                    if let Some(completed) = completed {
                        let completed_sequence = completed.sequence();
                        let stored = actor_completed_progress.lock().is_ok_and(|mut slot| {
                            if slot.is_some() {
                                false
                            } else {
                                *slot = Some(completed);
                                true
                            }
                        });
                        if !stored {
                            let _ignored = response.send(Err(OutputGrpcError::Unavailable(
                                "the durable progress ACK proof is unavailable",
                            )));
                            break;
                        }
                        actor_acknowledged
                            .store(inner.state.acknowledged_sequence(), Ordering::Release);
                        actor_completed_progress_sequence
                            .store(completed_sequence, Ordering::Release);
                        let _ignored = response.send(result);
                        if *shutdown_receiver.borrow() {
                            break;
                        }
                        continue;
                    }
                    actor_acknowledged
                        .store(inner.state.acknowledged_sequence(), Ordering::Release);
                    let _ignored = response.send(result);
                    if *shutdown_receiver.borrow() {
                        break;
                    }
                }
            }
        }
        let _ignored = inner.io.close().await;
    });
    OutputGrpcSession {
        commands,
        admission,
        acknowledged_sequence,
        completed_progress_sequence,
        completed_progress,
        max_frame_bytes,
        shutdown,
        task: Some(task),
    }
}

fn spawn_progress_replay<S>(
    prepared: PreparedOutputSpool,
    io: S,
    expected: &ExecutionOutputFrameV1,
) -> ProgressReplaySession
where
    S: OutputStreamIo + 'static,
{
    let pending = PendingProgress::new(expected);
    let expected_sequence = expected.sequence;
    let result = Arc::new(Mutex::new(None));
    let actor_result = Arc::clone(&result);
    let (completion_sender, completion) = watch::channel(false);
    let (shutdown, mut shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(async move {
        let replayed =
            OutputSession::from_prepared_with_shutdown(io, prepared, &mut shutdown_receiver).await;
        match replayed {
            Ok(mut session) => {
                let decision = if session.terminal_committed
                    || session.state.acknowledged_sequence() != expected_sequence
                {
                    Err(OutputGrpcError::Unavailable(
                        "the progress replay did not reach its bound durable ACK",
                    ))
                } else {
                    Ok(ProgressReplayDecision::Acknowledged(DurablyAckedProgress {
                        sequence: pending.sequence,
                        frame_sha256: pending.frame_sha256,
                        _permit: None,
                    }))
                };
                store_progress_replay_result(&actor_result, decision, &completion_sender);
                let _ignored = session.io.close().await;
            }
            Err(error) => {
                let decision = match progress_rejection_winner(&error) {
                    Some(winner) => Ok(ProgressReplayDecision::Rejected(
                        pending.into_rejected(None, winner),
                    )),
                    None => Err(error),
                };
                store_progress_replay_result(&actor_result, decision, &completion_sender);
            }
        }
    });
    ProgressReplaySession {
        completion,
        result,
        shutdown,
        task: Some(task),
    }
}

fn store_progress_replay_result(
    slot: &Mutex<Option<Result<ProgressReplayDecision, OutputGrpcError>>>,
    result: Result<ProgressReplayDecision, OutputGrpcError>,
    completion: &watch::Sender<bool>,
) {
    let stored = slot.lock().is_ok_and(|mut current| {
        if current.is_some() {
            false
        } else {
            *current = Some(result);
            true
        }
    });
    if stored {
        let _ignored = completion.send(true);
    }
}

async fn run_spool<T, F>(
    mut spool: EncryptedOutputSpool,
    operation: F,
) -> Result<(EncryptedOutputSpool, Result<T, SpoolError>), OutputGrpcError>
where
    T: Send + 'static,
    F: FnOnce(&mut EncryptedOutputSpool) -> Result<T, SpoolError> + Send + 'static,
{
    tokio::task::spawn_blocking(move || {
        let result = operation(&mut spool);
        (spool, result)
    })
    .await
    .map_err(|_| OutputGrpcError::Unavailable("the output spool task is unavailable"))
}

fn valid_metadata_value(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_METADATA_BYTES
        && value.is_ascii()
        && !value.as_bytes().contains(&b'\0')
        && !value.as_bytes().contains(&b'\r')
        && !value.as_bytes().contains(&b'\n')
}

fn validate_terminal_settlement_binding(
    frame: &ExecutionOutputFrameV1,
) -> Result<(), OutputGrpcError> {
    let identity = frame.identity.as_ref().ok_or(OutputGrpcError::Protocol(
        OutputSessionError::InvalidInput(
            "the terminal output identity is missing before settlement",
        ),
    ))?;
    let proposal = frame
        .settlement_proposal
        .as_ref()
        .ok_or(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
            "the terminal settlement proposal is missing",
        )))?;
    let valid_outcome = ExecutionOutcomeV1::try_from(proposal.requested_outcome)
        .is_ok_and(|outcome| !matches!(outcome, ExecutionOutcomeV1::Unspecified));
    let valid_digest = frame.payload_digest.as_ref().is_some_and(|value| {
        value.algorithm == DigestAlgorithmV1::Sha256 as i32
            && value.value.len() == 32
            && value.value.iter().any(|byte| *byte != 0)
    });
    if !frame.terminal
        || !valid_outcome
        || !valid_digest
        || proposal.proposal_id != format!("{}:settlement", identity.command_id)
        || proposal.terminal_logical_output_id != frame.logical_output_id
        || proposal.terminal_event_id != frame.event_id
        || proposal.terminal_sequence != frame.sequence
        || proposal.terminal_payload_digest != frame.payload_digest
        || proposal.prepare_idempotency_key != format!("{}:prepare-settlement", identity.command_id)
    {
        return Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
            "the terminal settlement binding is malformed",
        )));
    }
    Ok(())
}

fn terminal_identity_matches_command(
    identity: &ExecutionIdentityV1,
    verified: &impl VerifiedExecutionCommand,
) -> bool {
    let command = verified.command();
    identity.tenant_id == command.tenant_id
        && identity.resource_project_id == command.resource_project_id
        && identity.projection_project_id == command.projection_project_id
        && identity.command_id == command.command_id
        && identity.execution_id == command.execution_id
        && identity.generation == command.generation
}

#[derive(Clone, Copy)]
enum RecoveryKind {
    Cancelled,
    Ambiguous,
    Deadline,
    Terminal,
}

fn validate_recovery_rebind(
    expected: &ExecutionOutputFrameV1,
    replacement: &ExecutionOutputFrameV1,
    kind: RecoveryKind,
) -> Result<(), OutputGrpcError> {
    let Some(expected_identity) = expected.identity.as_ref() else {
        return Err(recovery_binding_error(kind));
    };
    let Some(expected_fence) = expected.fence.as_ref() else {
        return Err(recovery_binding_error(kind));
    };
    let Some(replacement_identity) = replacement.identity.as_ref() else {
        return Err(recovery_binding_error(kind));
    };
    let Some(replacement_fence) = replacement.fence.as_ref() else {
        return Err(recovery_binding_error(kind));
    };
    let Some(settlement) = replacement.settlement_proposal.as_ref() else {
        return Err(recovery_binding_error(kind));
    };
    let expected_predecessor = replacement.sequence.checked_sub(1);
    if !replacement.terminal
        || replacement.sequence != expected.sequence
        || replacement.stream_id != expected.stream_id
        || replacement_identity != expected_identity
        || replacement_fence.claim_attempt <= expected_fence.claim_attempt
        || replacement_fence.lease_epoch <= expected_fence.lease_epoch
        || replacement_fence.fence_token == expected_fence.fence_token
        || replacement.claim_handoff_watermark < expected.claim_handoff_watermark
        || expected_predecessor != Some(replacement.claim_handoff_watermark)
    {
        return Err(recovery_binding_error(kind));
    }

    let valid_payload = match (kind, replacement.payload.as_ref()) {
        (RecoveryKind::Terminal, _) => {
            expected.terminal && expected_fence.producer_id == replacement_fence.producer_id
        }
        (
            RecoveryKind::Cancelled,
            Some(execution_output_frame_v1::Payload::RuntimeError(error)),
        ) => {
            error.code == RuntimeErrorCodeV1::Cancelled as i32
                && settlement.requested_outcome == ExecutionOutcomeV1::Cancelled as i32
        }
        (
            RecoveryKind::Ambiguous,
            Some(execution_output_frame_v1::Payload::RuntimeError(error)),
        ) => {
            expected.terminal
                && expected.logical_output_id == replacement.logical_output_id
                && expected.event_id == replacement.event_id
                && error.code == RuntimeErrorCodeV1::Internal as i32
                && error.safe_message == "The runtime operation failed."
                && !error.retryable
                && settlement.requested_outcome == ExecutionOutcomeV1::Failed as i32
        }
        (RecoveryKind::Deadline, Some(execution_output_frame_v1::Payload::RuntimeError(error))) => {
            expected.terminal
                && expected.logical_output_id == replacement.logical_output_id
                && expected.event_id == replacement.event_id
                && expected_fence.producer_id == replacement_fence.producer_id
                && error.code == RuntimeErrorCodeV1::DeadlineExceeded as i32
                && error.safe_message == "The execution deadline was exceeded."
                && error.retryable
                && settlement.requested_outcome == ExecutionOutcomeV1::Failed as i32
        }
        _ => false,
    };
    if !valid_payload {
        return Err(recovery_binding_error(kind));
    }
    Ok(())
}

fn recovery_binding_error(kind: RecoveryKind) -> OutputGrpcError {
    let message = match kind {
        RecoveryKind::Cancelled => "the cancellation recovery replacement is not exactly bound",
        RecoveryKind::Ambiguous => "the ambiguous recovery replacement is not exactly bound",
        RecoveryKind::Deadline => "the deadline recovery replacement is not exactly bound",
        RecoveryKind::Terminal => "the terminal recovery replacement is not exactly bound",
    };
    OutputGrpcError::Protocol(OutputSessionError::AuthorizationFailed(message))
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;
    use std::fs;
    use std::os::unix::fs::PermissionsExt;
    use std::path::Path;
    use std::sync::Mutex;

    use prost::Message;
    use ring::digest;
    use tokio::sync::Notify;
    use tonic::{Response, Status};

    use crate::protocol::command::{
        SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
        parse_and_verify_agent_command,
    };
    use crate::protocol::control::AgentControlClient;
    use crate::protocol::elitea::runtime::v1::{
        AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1, BeginExecutionRequestV1,
        BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1,
        DesiredExecutionStateV1, DigestV1, ExecutionFenceV1, ExecutionIdentityV1,
        ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
        PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1, RuntimeErrorV1,
        SettlementProposalV1,
    };
    use crate::spool::{
        ExecutionSpoolBinding, ExecutionSpoolIdentity, SpoolLimits, SpoolMasterKey,
    };

    use super::*;

    fn verified_agent_command(name: &str) -> crate::protocol::command::VerifiedAgentCommand {
        let prefix = format!("{name}=");
        let line = include_str!("../../tests/fixtures/agent_control_vectors.txt")
            .lines()
            .find(|line| line.starts_with(&prefix))
            .expect("signed command vector");
        let (_, encoded) = line.split_once('=').expect("named vector");
        let bytes = encoded
            .as_bytes()
            .chunks_exact(2)
            .map(|pair| {
                u8::from_str_radix(std::str::from_utf8(pair).expect("ASCII hex"), 16)
                    .expect("fixture hex")
            })
            .collect::<Vec<_>>();
        let authenticator = TestOnlyConformanceHmacAuthenticator;
        parse_and_verify_agent_command(
            &bytes,
            Some(&authenticator as &dyn SignedCommandAuthenticator),
        )
        .expect("verified agent command")
    }

    struct FakeStream {
        acknowledgements: VecDeque<Result<Option<ExecutionOutputAckV1>, OutputGrpcError>>,
        writes: Vec<ExecutionOutputFrameV1>,
        closed: bool,
    }

    struct GatedStream {
        acknowledgements: mpsc::Receiver<ExecutionOutputAckV1>,
        writes: Arc<Mutex<Vec<ExecutionOutputFrameV1>>>,
        wrote: Arc<Notify>,
    }

    struct CloseGatedStream {
        acknowledgements: mpsc::Receiver<ExecutionOutputAckV1>,
        writes: Arc<Mutex<Vec<ExecutionOutputFrameV1>>>,
        wrote: Arc<Notify>,
        close_started: Arc<Notify>,
        release_close: Arc<Notify>,
    }

    struct SettlementRpc {
        request: Arc<Mutex<Option<PrepareSettlementRequestV1>>>,
    }

    #[async_trait]
    impl OutputStreamIo for FakeStream {
        async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<(), OutputGrpcError> {
            self.writes.push(frame);
            Ok(())
        }

        async fn receive(&mut self) -> Result<Option<ExecutionOutputAckV1>, OutputGrpcError> {
            self.acknowledgements.pop_front().unwrap_or(Ok(None))
        }

        async fn close(&mut self) -> Result<(), OutputGrpcError> {
            self.closed = true;
            Ok(())
        }
    }

    #[async_trait]
    impl OutputStreamIo for GatedStream {
        async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<(), OutputGrpcError> {
            self.writes.lock().expect("writes lock").push(frame);
            self.wrote.notify_one();
            Ok(())
        }

        async fn receive(&mut self) -> Result<Option<ExecutionOutputAckV1>, OutputGrpcError> {
            Ok(self.acknowledgements.recv().await)
        }

        async fn close(&mut self) -> Result<(), OutputGrpcError> {
            Ok(())
        }
    }

    #[async_trait]
    impl OutputStreamIo for CloseGatedStream {
        async fn send(&mut self, frame: ExecutionOutputFrameV1) -> Result<(), OutputGrpcError> {
            self.writes.lock().expect("writes lock").push(frame);
            self.wrote.notify_one();
            Ok(())
        }

        async fn receive(&mut self) -> Result<Option<ExecutionOutputAckV1>, OutputGrpcError> {
            Ok(self.acknowledgements.recv().await)
        }

        async fn close(&mut self) -> Result<(), OutputGrpcError> {
            self.close_started.notify_one();
            self.release_close.notified().await;
            Ok(())
        }
    }

    #[async_trait]
    impl crate::transport::ControlRpc for SettlementRpc {
        async fn claim_command(
            &self,
            _request: Request<ClaimCommandRequestV1>,
        ) -> Result<Response<ClaimCommandResponseV1>, Status> {
            Err(Status::unimplemented("test-only"))
        }

        async fn begin_execution(
            &self,
            _request: Request<BeginExecutionRequestV1>,
        ) -> Result<Response<BeginExecutionResponseV1>, Status> {
            Err(Status::unimplemented("test-only"))
        }

        async fn authorize_invocation(
            &self,
            _request: Request<AuthorizeInvocationRequestV1>,
        ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
            Err(Status::unimplemented("test-only"))
        }

        async fn renew_lease(
            &self,
            _request: Request<RenewLeaseRequestV1>,
        ) -> Result<Response<RenewLeaseResponseV1>, Status> {
            Err(Status::unimplemented("test-only"))
        }

        async fn observe_desired_state(
            &self,
            _request: Request<ObserveDesiredStateRequestV1>,
        ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
            Err(Status::unimplemented("test-only"))
        }

        async fn prepare_settlement(
            &self,
            request: Request<PrepareSettlementRequestV1>,
        ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
            *self.request.lock().expect("settlement request") = Some(request.into_inner());
            Ok(Response::new(PrepareSettlementResponseV1 {
                settlement_receipt_id: "settlement-receipt-1".to_owned(),
                outcome: ExecutionOutcomeV1::Succeeded as i32,
                rejection: None,
            }))
        }
    }

    fn config() -> OutputGrpcConfig {
        OutputGrpcConfig {
            max_queued_frames: 2,
            max_queued_bytes: 2_048,
            max_frame_bytes: 1_024,
            max_server_credit_frames: 2,
            max_server_credit_bytes: 2_048,
            stream_deadline: Duration::from_mins(5),
            ack_timeout: Duration::from_secs(1),
            workload_session_id: "session-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        }
    }

    fn frame(sequence: u64, terminal: bool) -> ExecutionOutputFrameV1 {
        ExecutionOutputFrameV1 {
            output_schema_revision: "elitea.runtime.execution-output.v1".to_owned(),
            stream_id: "execution-1:1".to_owned(),
            identity: Some(ExecutionIdentityV1 {
                tenant_id: "tenant-1".to_owned(),
                resource_project_id: "resource-1".to_owned(),
                projection_project_id: "projection-1".to_owned(),
                command_id: "command-1".to_owned(),
                execution_id: "execution-1".to_owned(),
                generation: 1,
            }),
            fence: Some(ExecutionFenceV1 {
                workload_session_id: "session-1".to_owned(),
                producer_id: "worker-1".to_owned(),
                claim_attempt: 1,
                lease_epoch: 1,
                fence_token: vec![b'f'; 32],
            }),
            logical_output_id: format!("logical-{sequence}"),
            event_id: format!("event-{sequence}"),
            sequence,
            claim_handoff_watermark: 0,
            event_type: 0,
            occurred_at_unix_millis: 1_700_000_000_000,
            payload_digest: None,
            terminal,
            settlement_proposal: None,
            payload: None,
        }
    }

    fn terminal_frame() -> ExecutionOutputFrameV1 {
        let mut terminal = frame(1, true);
        terminal.payload_digest = Some(DigestV1 {
            algorithm: DigestAlgorithmV1::Sha256 as i32,
            value: vec![b'p'; 32],
        });
        terminal.settlement_proposal = Some(SettlementProposalV1 {
            proposal_id: "command-1:settlement".to_owned(),
            requested_outcome: ExecutionOutcomeV1::Succeeded as i32,
            terminal_logical_output_id: terminal.logical_output_id.clone(),
            terminal_event_id: terminal.event_id.clone(),
            terminal_sequence: terminal.sequence,
            terminal_payload_digest: terminal.payload_digest.clone(),
            prepare_idempotency_key: "command-1:prepare-settlement".to_owned(),
        });
        terminal
    }

    fn bootstrap() -> ExecutionOutputAckV1 {
        ExecutionOutputAckV1 {
            credit_frames: 2,
            credit_bytes: 2_048,
            desired_state: DesiredExecutionStateV1::Running as i32,
            ..ExecutionOutputAckV1::default()
        }
    }

    fn bound_ack(frame: &ExecutionOutputFrameV1) -> ExecutionOutputAckV1 {
        ExecutionOutputAckV1 {
            stream_id: frame.stream_id.clone(),
            identity: frame.identity.clone(),
            fence: frame.fence.clone(),
            committed_contiguous_sequence: frame.sequence,
            claim_handoff_watermark: frame.claim_handoff_watermark,
            credit_frames: 2,
            credit_bytes: 2_048,
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        }
    }

    fn rejection(
        frame: &ExecutionOutputFrameV1,
        code: RuntimeErrorCodeV1,
        retryable: bool,
    ) -> ExecutionOutputAckV1 {
        let mut rejection = bound_ack(frame);
        rejection.committed_contiguous_sequence = 0;
        rejection.credit_frames = 0;
        rejection.credit_bytes = 0;
        rejection.desired_state = DesiredExecutionStateV1::Unspecified as i32;
        rejection.rejection = Some(RuntimeErrorV1 {
            code: code as i32,
            safe_message: String::new(),
            retryable,
        });
        rejection
    }

    fn progress_winner(
        frame: &ExecutionOutputFrameV1,
        winner: ProgressRejectionWinner,
    ) -> ExecutionOutputAckV1 {
        let mut rejection = bound_ack(frame);
        rejection.committed_contiguous_sequence = frame.sequence.saturating_sub(1);
        rejection.credit_frames = 0;
        rejection.credit_bytes = 0;
        let (desired_state, code, safe_message, retryable) = match winner {
            ProgressRejectionWinner::Cancelled => (
                DesiredExecutionStateV1::Cancelled,
                RuntimeErrorCodeV1::Cancelled,
                "Execution cancellation won before this output became durable.",
                false,
            ),
            ProgressRejectionWinner::DeadlineExceeded => (
                DesiredExecutionStateV1::Running,
                RuntimeErrorCodeV1::DeadlineExceeded,
                "The execution deadline was exceeded.",
                true,
            ),
        };
        rejection.desired_state = desired_state as i32;
        rejection.rejection = Some(RuntimeErrorV1 {
            code: code as i32,
            safe_message: safe_message.to_owned(),
            retryable,
        });
        rejection
    }

    fn recovery_frame(
        expected: &ExecutionOutputFrameV1,
        kind: RecoveryKind,
    ) -> ExecutionOutputFrameV1 {
        let mut replacement = expected.clone();
        let fence = replacement.fence.as_mut().expect("replacement fence");
        fence.claim_attempt += 1;
        fence.lease_epoch += 1;
        fence.fence_token = if fence.fence_token == vec![b'g'; 32] {
            vec![b'h'; 32]
        } else {
            vec![b'g'; 32]
        };
        replacement.claim_handoff_watermark = replacement.sequence - 1;
        replacement.terminal = true;
        let (code, safe_message, outcome) = match kind {
            RecoveryKind::Cancelled => (
                RuntimeErrorCodeV1::Cancelled,
                "Execution was cancelled.",
                ExecutionOutcomeV1::Cancelled,
            ),
            RecoveryKind::Ambiguous => (
                RuntimeErrorCodeV1::Internal,
                "The runtime operation failed.",
                ExecutionOutcomeV1::Failed,
            ),
            RecoveryKind::Deadline => (
                RuntimeErrorCodeV1::DeadlineExceeded,
                "The execution deadline was exceeded.",
                ExecutionOutcomeV1::Failed,
            ),
            RecoveryKind::Terminal => return replacement,
        };
        replacement.payload = Some(execution_output_frame_v1::Payload::RuntimeError(
            RuntimeErrorV1 {
                code: code as i32,
                safe_message: safe_message.to_owned(),
                retryable: matches!(kind, RecoveryKind::Deadline),
            },
        ));
        replacement.settlement_proposal = Some(SettlementProposalV1 {
            requested_outcome: outcome as i32,
            ..SettlementProposalV1::default()
        });
        replacement
    }

    fn try_spool(root: &Path) -> Result<EncryptedOutputSpool, SpoolError> {
        if !root.exists() {
            fs::create_dir(root).expect("spool root");
            fs::set_permissions(root, fs::Permissions::from_mode(0o700)).expect("private root");
        }
        let root = fs::canonicalize(root).expect("canonical spool root");
        let binding = ExecutionSpoolBinding::new(&ExecutionSpoolIdentity {
            tenant_id: "tenant-1".to_owned(),
            resource_project_id: "resource-1".to_owned(),
            projection_project_id: "projection-1".to_owned(),
            command_id: "command-1".to_owned(),
            execution_id: "execution-1".to_owned(),
            generation: 1,
            producer_id: "worker-1".to_owned(),
        })
        .expect("binding");
        EncryptedOutputSpool::open(
            &root,
            &SpoolMasterKey::new([7; 32]),
            &binding,
            SpoolLimits {
                max_frames: 2,
                max_encrypted_bytes: 2_176,
                max_frame_bytes: 1_024,
            },
        )
    }

    fn spool(root: &Path) -> EncryptedOutputSpool {
        try_spool(root).expect("spool")
    }

    #[test]
    fn prepared_spool_persists_and_restores_exact_bytes_without_a_network() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let first = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        assert!(prepared.pending_replay_frame().is_none());
        assert_eq!(prepared.persist(first.clone()).expect("durable frame"), 1);
        assert!(prepared.replays(&first));
        drop(prepared);

        let recovered =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored spool");
        assert_eq!(recovered.pending_replay_frame(), Some(first.clone()));
        assert!(recovered.replays(&first));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn offline_persisted_frame_replays_once_and_restores_capacity_one() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, false);
        let second = frame(2, false);
        let mut bounded = config();
        bounded.max_queued_frames = 1;
        bounded.max_server_credit_frames = 1;
        let mut bootstrap = bootstrap();
        bootstrap.credit_frames = 1;
        let mut first_ack = bound_ack(&first);
        first_ack.credit_frames = 1;
        let mut second_ack = bound_ack(&second);
        second_ack.credit_frames = 1;

        let mut prepared = PreparedOutputSpool::prepare(spool(&temp.path().join("root")), bounded)
            .expect("prepared spool");
        prepared
            .persist(first.clone())
            .expect("offline persisted frame");
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap)),
                Ok(Some(first_ack)),
                Ok(Some(second_ack)),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let mut session = OutputSession::from_prepared(io, prepared)
            .await
            .expect("exact replay");
        assert_eq!(session.io.writes, vec![first]);
        assert_eq!(
            session
                .send(second.clone())
                .await
                .expect("capacity restored"),
            2
        );
        assert_eq!(session.io.writes, vec![frame(1, false), second]);
        assert!(
            session
                .spool
                .as_mut()
                .expect("spool")
                .pending()
                .expect("pending")
                .is_empty()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn generic_session_open_cannot_transmit_or_delete_restored_progress() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-generic-restored-progress");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let channel = tonic::transport::Endpoint::from_static("https://127.0.0.1:9").connect_lazy();
        assert!(matches!(
            restored.connect(channel).await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        let mut recovered = spool(&root);
        let pending = recovered.pending().expect("preserved progress spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, progress.encode_to_vec());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn cancelled_replay_wait_retains_the_exact_ack_proof() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-owned-progress-replay");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let (acknowledgements, receiver) = mpsc::channel(2);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let writes = Arc::new(Mutex::new(Vec::new()));
        let wrote = Arc::new(Notify::new());
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::clone(&writes),
            wrote: Arc::clone(&wrote),
        };
        let mut replay = restored
            .start_progress_replay_over(io, &progress)
            .expect("owned replay");
        {
            let wait = replay.wait();
            tokio::pin!(wait);
            tokio::select! {
                _result = &mut wait => panic!("replay returned before its ACK"),
                () = wrote.notified() => {}
            }
        }

        acknowledgements
            .send(bound_ack(&progress))
            .await
            .expect("bound progress ACK");
        let proof = match replay.wait().await.expect("stored replay decision") {
            ProgressReplayDecision::Acknowledged(proof) => proof,
            ProgressReplayDecision::Rejected(_) => panic!("ACK became a rejection"),
        };
        assert_eq!(
            proof.into_binding(),
            (progress.sequence, progress_frame_sha256(&progress))
        );
        assert!(matches!(
            replay.wait().await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        replay.close().await.expect("joined replay task");
        assert_eq!(*writes.lock().expect("writes lock"), vec![progress]);
        assert!(
            spool(&root)
                .pending()
                .expect("empty acknowledged spool")
                .is_empty()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn cancelled_replay_close_retains_the_join_owner() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-cancelled-replay-close");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let (acknowledgements, receiver) = mpsc::channel(2);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let wrote = Arc::new(Notify::new());
        let close_started = Arc::new(Notify::new());
        let release_close = Arc::new(Notify::new());
        let io = CloseGatedStream {
            acknowledgements: receiver,
            writes: Arc::new(Mutex::new(Vec::new())),
            wrote: Arc::clone(&wrote),
            close_started: Arc::clone(&close_started),
            release_close: Arc::clone(&release_close),
        };
        let mut replay = restored
            .start_progress_replay_over(io, &progress)
            .expect("owned replay");
        wrote.notified().await;
        acknowledgements
            .send(bound_ack(&progress))
            .await
            .expect("bound progress ACK");
        assert!(matches!(
            replay.wait().await.expect("stored replay decision"),
            ProgressReplayDecision::Acknowledged(_)
        ));

        {
            let close = replay.close();
            tokio::pin!(close);
            tokio::select! {
                result = &mut close => panic!("close returned before release: {result:?}"),
                () = close_started.notified() => {}
            }
        }
        release_close.notify_one();
        replay.close().await.expect("retryable replay close");
        assert!(
            spool(&root)
                .pending()
                .expect("empty acknowledged spool")
                .is_empty()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn owned_progress_replay_retains_exact_rejection_and_spool() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-rejected-progress-replay");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap())),
                Ok(Some(progress_winner(
                    &progress,
                    ProgressRejectionWinner::DeadlineExceeded,
                ))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let mut replay = restored
            .start_progress_replay_over(io, &progress)
            .expect("owned replay");
        let rejected = match replay.wait().await.expect("stored replay decision") {
            ProgressReplayDecision::Rejected(rejected) => rejected,
            ProgressReplayDecision::Acknowledged(_) => panic!("rejection became an ACK"),
        };
        let (sequence, frame_sha256, winner) = rejected.into_binding();
        assert_eq!(sequence, progress.sequence);
        assert_eq!(frame_sha256, progress_frame_sha256(&progress));
        assert!(winner == ProgressRejectionWinner::DeadlineExceeded);
        replay.close().await.expect("joined replay task");

        let pending = spool(&root).pending().expect("preserved progress spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, progress.encode_to_vec());
    }

    #[tokio::test(flavor = "current_thread")]
    async fn pretransmit_bound_winner_cannot_mint_progress_rejection_authority() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-pretransmit-progress-winner");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(progress_winner(
                &progress,
                ProgressRejectionWinner::Cancelled,
            )))]),
            writes: Vec::new(),
            closed: false,
        };
        let mut replay = restored
            .start_progress_replay_over(io, &progress)
            .expect("owned replay");
        assert!(matches!(
            replay.wait().await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        replay.close().await.expect("joined replay task");

        let pending = spool(&root).pending().expect("preserved progress spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, progress.encode_to_vec());
    }

    #[tokio::test(flavor = "current_thread")]
    async fn closing_progress_replay_before_ack_preserves_the_exact_frame() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-closed-progress-replay");
        let progress = frame(1, false);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(progress.clone())
            .expect("durable progress");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored progress");
        let (acknowledgements, receiver) = mpsc::channel(1);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let wrote = Arc::new(Notify::new());
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::new(Mutex::new(Vec::new())),
            wrote: Arc::clone(&wrote),
        };
        let mut replay = restored
            .start_progress_replay_over(io, &progress)
            .expect("owned replay");
        wrote.notified().await;
        replay.close().await.expect("bounded replay close");
        assert!(matches!(
            replay.wait().await,
            Err(OutputGrpcError::Unavailable(_))
        ));

        let pending = spool(&root).pending().expect("preserved progress spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, progress.encode_to_vec());
    }

    #[test]
    fn progress_replay_rejects_a_terminal_before_starting_a_task() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-terminal-progress-replay");
        let terminal = terminal_frame();
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(terminal.clone())
            .expect("durable terminal");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored terminal");
        let io = FakeStream {
            acknowledgements: VecDeque::new(),
            writes: Vec::new(),
            closed: false,
        };
        assert!(matches!(
            restored.start_progress_replay_over(io, &terminal),
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
        let pending = spool(&root).pending().expect("preserved terminal spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, terminal.encode_to_vec());
    }

    #[tokio::test(flavor = "current_thread")]
    async fn restored_terminal_ack_mints_the_same_settlement_authority() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-terminal-proof");
        let verified = verified_agent_command("signed_command_output_session");
        let terminal = terminal_frame();
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(terminal.clone())
            .expect("durable terminal");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored terminal");
        let pending =
            PendingTerminalSettlement::new(&verified, &terminal).expect("terminal binding");
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap())),
                Ok(Some(bound_ack(&terminal))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let acknowledged = restored
            .replay_terminal_over(io, pending, terminal.sequence)
            .await
            .expect("durably acknowledged restored terminal");
        let (identity, fence, proposal, stable_delivery_id, exact_envelope) =
            acknowledged.into_settlement_parts();
        assert_eq!(identity, terminal.identity.expect("terminal identity"));
        assert_eq!(fence, terminal.fence.expect("terminal fence"));
        assert_eq!(
            proposal,
            terminal
                .settlement_proposal
                .expect("terminal settlement proposal")
        );
        assert_eq!(stable_delivery_id, verified.command().idempotency_key);
        assert_eq!(exact_envelope.as_ref(), verified.exact_signed_envelope());
        assert!(
            spool(&root)
                .pending()
                .expect("empty acknowledged spool")
                .is_empty()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn restored_terminal_without_a_bound_ack_retains_the_exact_spool() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root-terminal-no-ack");
        let verified = verified_agent_command("signed_command_output_session");
        let terminal = terminal_frame();
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(terminal.clone())
            .expect("durable terminal");
        drop(prepared);

        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored terminal");
        let pending =
            PendingTerminalSettlement::new(&verified, &terminal).expect("terminal binding");
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(None)]),
            writes: Vec::new(),
            closed: false,
        };
        assert!(matches!(
            restored
                .replay_terminal_over(io, pending, terminal.sequence)
                .await,
            Err(OutputGrpcError::Unavailable(_))
        ));
        let mut recovered = spool(&root);
        let pending = recovered.pending().expect("preserved terminal spool");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, terminal.encode_to_vec());
    }

    #[test]
    fn terminal_admission_ends_both_live_and_restored_durable_streams() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let terminal = frame(1, true);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(terminal.clone())
            .expect("durable terminal");
        assert!(matches!(
            prepared.persist(frame(2, false)),
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
        drop(prepared);

        let mut invalid_spool = spool(&root);
        invalid_spool
            .put(
                NonZeroU64::new(2).expect("nonzero"),
                &frame(2, false).encode_to_vec(),
            )
            .expect("corrupt continuation fixture");
        drop(invalid_spool);
        assert!(matches!(
            PreparedOutputSpool::prepare(spool(&root), config()),
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
    }

    #[test]
    fn reconciliation_requires_the_complete_prefix_and_finalizes_the_spool() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let mut prepared = PreparedOutputSpool::prepare(spool(&temp.path().join("root")), config())
            .expect("prepared spool");
        prepared
            .reconcile_pending_through(1)
            .expect("empty spool reconciliation is a no-op");
        prepared.persist(frame(1, false)).expect("first frame");
        prepared.persist(frame(2, false)).expect("second frame");
        assert!(matches!(
            prepared.reconcile_pending_through(1),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        prepared
            .reconcile_pending_through(2)
            .expect("complete receipt");
        assert!(matches!(
            prepared.persist(frame(3, false)),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
    }

    #[test]
    fn reconciliation_rejects_watermarks_outside_the_durable_integer_domain() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let mut prepared = PreparedOutputSpool::prepare(spool(&temp.path().join("root")), config())
            .expect("prepared spool");
        assert!(matches!(
            prepared.reconcile_pending_through(i64::MAX as u64 + 1),
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
    }

    #[test]
    fn exact_replacement_recomputes_terminal_state_before_durable_cas() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, false);
        let mut replacement = first.clone();
        replacement.terminal = true;
        let mut prepared = PreparedOutputSpool::prepare(spool(&temp.path().join("root")), config())
            .expect("prepared spool");
        prepared.persist(first.clone()).expect("first frame");
        prepared
            .replace_pending_exact(&first, &replacement)
            .expect("terminal CAS");
        assert!(prepared.replays(&replacement));
        assert!(matches!(
            prepared.persist(frame(2, false)),
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
    }

    #[test]
    fn recovery_rebinds_are_canonical_and_cas_the_exact_old_fence() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let old_progress = frame(1, false);
        let cancelled = recovery_frame(&old_progress, RecoveryKind::Cancelled);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared
            .persist(old_progress.clone())
            .expect("old progress");
        prepared
            .replace_pending_cancelled_recovery(&old_progress, &cancelled)
            .expect("cancelled recovery");
        assert!(prepared.replays(&cancelled));
        drop(prepared);

        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored cancellation");
        let ambiguous = recovery_frame(&cancelled, RecoveryKind::Ambiguous);
        let mut malformed = ambiguous.clone();
        if let Some(execution_output_frame_v1::Payload::RuntimeError(error)) =
            malformed.payload.as_mut()
        {
            error.safe_message = "different".to_owned();
        }
        assert!(matches!(
            prepared.replace_pending_ambiguous_recovery(&cancelled, &malformed),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        assert!(prepared.replays(&cancelled));
        prepared
            .replace_pending_ambiguous_recovery(&cancelled, &ambiguous)
            .expect("ambiguous recovery");
        assert!(prepared.replays(&ambiguous));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn toolkit_terminal_rebind_rejects_old_ack_then_mints_new_fence_settlement() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let verified = verified_agent_command("signed_command_output_session");
        let old = terminal_frame();
        let replacement = recovery_frame(&old, RecoveryKind::Terminal);
        let mut prepared =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
        prepared.persist(old.clone()).expect("old terminal");
        prepared
            .replace_pending_toolkit_terminal_recovery(&old, &replacement)
            .expect("terminal replacement");
        let pending = PendingTerminalSettlement::new(&verified, &replacement)
            .expect("replacement settlement binding");
        let stale = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(Some(bound_ack(&old)))]),
            writes: Vec::new(),
            closed: false,
        };
        assert!(matches!(
            prepared
                .replay_terminal_over(stale, pending, replacement.sequence)
                .await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        let restored =
            PreparedOutputSpool::prepare(spool(&root), config()).expect("restored replacement");
        assert!(restored.replays(&replacement));
        let pending = PendingTerminalSettlement::new(&verified, &replacement)
            .expect("replacement settlement binding");
        let current = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap())),
                Ok(Some(bound_ack(&replacement))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let acknowledged = restored
            .replay_terminal_over(current, pending, replacement.sequence)
            .await
            .expect("new fence acknowledgment");
        let (identity, fence, proposal, _, _) = acknowledged.into_settlement_parts();
        assert_eq!(Some(identity), replacement.identity);
        assert_eq!(Some(fence), replacement.fence);
        assert_eq!(Some(proposal), replacement.settlement_proposal);
        assert!(
            spool(&root)
                .pending()
                .expect("acknowledged spool")
                .is_empty()
        );
    }

    #[test]
    fn toolkit_terminal_rebind_refuses_result_changes_and_stale_compare_exchange() {
        let mutations: &[fn(&mut ExecutionOutputFrameV1)] = &[
            |frame| frame.occurred_at_unix_millis += 1,
            |frame| frame.logical_output_id.push('x'),
            |frame| frame.payload_digest.as_mut().unwrap().value[0] ^= 1,
            |frame| {
                frame
                    .settlement_proposal
                    .as_mut()
                    .unwrap()
                    .requested_outcome = ExecutionOutcomeV1::Failed as i32;
            },
        ];
        for mutate in mutations {
            let temp = tempfile::tempdir().expect("temporary directory");
            let root = temp.path().join("root");
            let old = terminal_frame();
            let replacement = recovery_frame(&old, RecoveryKind::Terminal);
            let mut prepared =
                PreparedOutputSpool::prepare(spool(&root), config()).expect("prepared spool");
            prepared.persist(old.clone()).expect("old terminal");
            let mut changed = replacement.clone();
            mutate(&mut changed);
            assert!(
                prepared
                    .replace_pending_toolkit_terminal_recovery(&old, &changed)
                    .is_err()
            );
            assert!(prepared.replays(&old));
            prepared
                .replace_pending_toolkit_terminal_recovery(&old, &replacement)
                .expect("exact replacement");
            assert!(
                prepared
                    .replace_pending_toolkit_terminal_recovery(&old, &replacement)
                    .is_err()
            );
            assert!(prepared.replays(&replacement));
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn frame_is_spooled_before_send_and_deleted_only_after_bound_ack() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, false);
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(Some(bound_ack(&first)))]),
            writes: Vec::new(),
            closed: false,
        };
        let mut session = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        assert_eq!(session.send(first.clone()).await.expect("committed"), 1);
        assert_eq!(session.state.acknowledged_sequence(), 1);
        assert_eq!(session.io.writes, vec![first]);
        assert!(
            session
                .spool
                .as_mut()
                .expect("spool")
                .pending()
                .unwrap()
                .is_empty()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn lost_ack_preserves_exact_spool_for_next_session_replay() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, true);
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(None)]),
            writes: Vec::new(),
            closed: false,
        };
        let mut failed = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        assert!(matches!(
            failed.send(first.clone()).await,
            Err(OutputGrpcError::Unavailable(_))
        ));
        let spool = failed.spool.take().expect("preserved spool");
        let replay_io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(Some(bound_ack(&first)))]),
            writes: Vec::new(),
            closed: false,
        };
        let recovered = OutputSession::open(replay_io, spool, config())
            .await
            .expect("replay");
        assert_eq!(recovered.io.writes, vec![first]);
        assert_eq!(recovered.state.acknowledged_sequence(), 1);
        assert!(recovered.terminal_committed);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_failed_stream_cannot_be_reused_after_preserving_its_spool() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, false);
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(None)]),
            writes: Vec::new(),
            closed: false,
        };
        let mut failed = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        assert!(failed.send(first).await.is_err());
        assert!(matches!(
            failed.send(frame(2, false)).await,
            Err(OutputGrpcError::Unavailable(_))
        ));
        assert_eq!(
            failed
                .spool
                .as_mut()
                .expect("spool")
                .pending()
                .expect("pending")
                .len(),
            1
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn retryable_and_authorization_rejections_preserve_the_exact_spool() {
        for (ordinal, code, retryable, expected_dependency) in [
            (1, RuntimeErrorCodeV1::DependencyUnavailable, true, true),
            (2, RuntimeErrorCodeV1::Internal, true, true),
            (3, RuntimeErrorCodeV1::AuthenticationFailed, true, false),
            (4, RuntimeErrorCodeV1::StaleFence, true, false),
        ] {
            let temp = tempfile::tempdir().expect("temporary directory");
            let current = frame(1, false);
            let io = FakeStream {
                acknowledgements: VecDeque::from([
                    Ok(Some(bootstrap())),
                    Ok(Some(rejection(&current, code, retryable))),
                ]),
                writes: Vec::new(),
                closed: false,
            };
            let mut session = OutputSession::open(
                io,
                spool(&temp.path().join(format!("root-{ordinal}"))),
                config(),
            )
            .await
            .expect("session");
            let result = session.send(current.clone()).await;
            if expected_dependency {
                assert!(matches!(
                    result,
                    Err(OutputGrpcError::Protocol(
                        OutputSessionError::DependencyUnavailable
                    ))
                ));
            } else {
                assert!(matches!(
                    result,
                    Err(OutputGrpcError::Protocol(
                        OutputSessionError::AuthorizationFailed(_)
                    ))
                ));
            }
            let pending = session
                .spool
                .as_mut()
                .expect("spool")
                .pending()
                .expect("preserved rejection spool");
            assert_eq!(pending.len(), 1);
            assert_eq!(pending[0].payload, current.encode_to_vec());
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn only_a_bound_terminal_ack_can_mint_and_settle_retirement_authority() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let verified = verified_agent_command("signed_command_output_session");
        let mismatched = verified_agent_command("signed_command");
        let terminal = terminal_frame();
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap())),
                Ok(Some(bound_ack(&terminal))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let settings = config();
        let inner = OutputSession::open(io, spool(&temp.path().join("root")), settings.clone())
            .await
            .expect("output session");
        let mut output = spawn_output_actor(inner, settings.max_frame_bytes);
        assert!(matches!(
            output.send_terminal(&verified, frame(1, false)).await,
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
        assert!(matches!(
            output.send_terminal(&mismatched, terminal.clone()).await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        let acknowledged = output
            .send_terminal(&verified, terminal.clone())
            .await
            .expect("durably acknowledged terminal");

        let captured = Arc::new(Mutex::new(None));
        let control = AgentControlClient::new(
            SettlementRpc {
                request: Arc::clone(&captured),
            },
            crate::transport::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "session-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("semantic control");
        let receipt = control
            .prepare_agent_settlement(acknowledged)
            .await
            .expect("settled terminal");
        assert_eq!(receipt.receipt_id(), "settlement-receipt-1");
        assert_eq!(receipt.outcome(), ExecutionOutcomeV1::Succeeded);

        let request = captured
            .lock()
            .expect("settlement request")
            .clone()
            .expect("captured settlement");
        assert_eq!(request.identity, terminal.identity);
        assert_eq!(request.fence, terminal.fence);
        assert_eq!(request.proposal, terminal.settlement_proposal);
        assert_eq!(request.idempotency_key, "command-1:prepare-settlement");
        let proposal_bytes = request.proposal.as_ref().expect("proposal").encode_to_vec();
        let expected = digest::digest(&digest::SHA256, &proposal_bytes);
        assert_eq!(
            request
                .proposal_digest
                .as_ref()
                .expect("proposal digest")
                .value,
            expected.as_ref()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn prior_watermark_ack_is_applied_but_waits_for_the_current_commit() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let first = frame(1, false);
        let second = frame(2, true);
        let mut prior = bound_ack(&second);
        prior.committed_contiguous_sequence = 1;
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(bootstrap())),
                Ok(Some(bound_ack(&first))),
                Ok(Some(prior)),
                Ok(Some(bound_ack(&second))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let mut session = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        assert_eq!(session.send(first).await.expect("first commit"), 1);
        assert_eq!(session.send(second).await.expect("second commit"), 2);
        let pending = session
            .spool
            .as_mut()
            .expect("spool")
            .pending()
            .expect("pending terminal");
        assert!(pending.is_empty());
        assert!(session.terminal_committed);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn zero_bootstrap_credit_waits_for_a_bound_absolute_replenishment() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let current = frame(1, false);
        let mut zero = bootstrap();
        zero.credit_frames = 0;
        zero.credit_bytes = 0;
        let mut replenished = bound_ack(&current);
        replenished.committed_contiguous_sequence = 0;
        let io = FakeStream {
            acknowledgements: VecDeque::from([
                Ok(Some(zero)),
                Ok(Some(replenished)),
                Ok(Some(bound_ack(&current))),
            ]),
            writes: Vec::new(),
            closed: false,
        };
        let mut session = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        assert_eq!(session.send(current.clone()).await.expect("commit"), 1);
        assert_eq!(session.io.writes, vec![current]);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn cancelling_a_caller_wait_cannot_reassign_its_ack_to_the_next_frame() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let (acknowledgements, receiver) = mpsc::channel(4);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let writes = Arc::new(Mutex::new(Vec::new()));
        let wrote = Arc::new(Notify::new());
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::clone(&writes),
            wrote: Arc::clone(&wrote),
        };
        let inner = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        let mut session = spawn_output_actor(inner, config().max_frame_bytes);
        let first = frame(1, false);
        {
            let first_wait = session.send(&first);
            tokio::pin!(first_wait);
            tokio::select! {
                _result = &mut first_wait => panic!("send returned before its ACK"),
                () = wrote.notified() => {}
            }
        }
        acknowledgements
            .send(bound_ack(&first))
            .await
            .expect("first bound ACK");
        timeout(Duration::from_secs(1), async {
            while session.acknowledged_sequence() != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("first ACK applied");

        let second = frame(2, false);
        {
            let second_wait = session.send(&second);
            tokio::pin!(second_wait);
            assert!(
                timeout(Duration::from_millis(10), &mut second_wait)
                    .await
                    .is_err(),
                "the next command bypassed the first command's actor permit"
            );
            assert_eq!(writes.lock().expect("writes lock").len(), 1);
        }
        assert!(matches!(
            session.take_acknowledged_progress(&second),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        assert!(matches!(
            session.take_rejected_progress(&first),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        let first_proof = session
            .take_acknowledged_progress(&first)
            .expect("proof lookup")
            .expect("cancelled wait retained its proof");
        assert_eq!(first_proof.into_binding().0, 1);
        assert!(
            session
                .take_acknowledged_progress(&first)
                .expect("consumed proof lookup")
                .is_none()
        );
        assert!(
            session
                .take_acknowledged_progress(&second)
                .expect("cancelled-before-admission lookup")
                .is_none()
        );

        let second_result = {
            let second_wait = session.send(&second);
            tokio::pin!(second_wait);
            tokio::select! {
                _result = &mut second_wait => panic!("second send returned before its ACK"),
                () = wrote.notified() => {}
            }
            acknowledgements
                .send(bound_ack(&second))
                .await
                .expect("second bound ACK");
            second_wait.await
        };
        assert_eq!(second_result.expect("second commit").into_binding().0, 2);
        assert_eq!(session.acknowledged_sequence(), 2);
        assert_eq!(*writes.lock().expect("writes lock"), vec![first, second]);
        session.close().await.expect("close actor");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn cancelled_wait_retains_the_exact_bound_rejection_before_signalling_completion() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let (acknowledgements, receiver) = mpsc::channel(2);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let wrote = Arc::new(Notify::new());
        let writes = Arc::new(Mutex::new(Vec::new()));
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::clone(&writes),
            wrote: Arc::clone(&wrote),
        };
        let inner = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        let mut session = spawn_output_actor(inner, config().max_frame_bytes);
        let progress = frame(1, false);
        {
            let wait = session.send(&progress);
            tokio::pin!(wait);
            tokio::select! {
                _result = &mut wait => panic!("send returned before rejection"),
                () = wrote.notified() => {}
            }
        }

        acknowledgements
            .send(progress_winner(
                &progress,
                ProgressRejectionWinner::Cancelled,
            ))
            .await
            .expect("bound cancellation winner");
        timeout(Duration::from_secs(1), async {
            while session.completed_progress_sequence() != progress.sequence {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("rejection proof stored before completion signal");
        assert_eq!(session.acknowledged_sequence(), 0);
        assert!(matches!(
            session.take_acknowledged_progress(&progress),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));

        let mut substituted = progress.clone();
        substituted.occurred_at_unix_millis += 1;
        assert!(matches!(
            session.take_rejected_progress(&substituted),
            Err(OutputGrpcError::Protocol(
                OutputSessionError::AuthorizationFailed(_)
            ))
        ));
        let rejected = session
            .take_rejected_progress(&progress)
            .expect("rejection lookup")
            .expect("cancelled wait retained its exact rejection proof");
        let (sequence, frame_sha256, winner) = rejected.into_binding();
        assert_eq!(sequence, progress.sequence);
        assert_eq!(frame_sha256, progress_frame_sha256(&progress));
        assert!(winner == ProgressRejectionWinner::Cancelled);
        assert!(
            session
                .take_rejected_progress(&progress)
                .expect("consumed rejection lookup")
                .is_none()
        );
        assert_eq!(*writes.lock().expect("writes lock"), vec![progress]);
        session.close().await.expect("close actor");
    }

    #[tokio::test(flavor = "current_thread")]
    async fn close_interrupts_an_ack_blocked_actor_and_releases_the_spool_lock() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let (acknowledgements, receiver) = mpsc::channel(1);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let wrote = Arc::new(Notify::new());
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::new(Mutex::new(Vec::new())),
            wrote: Arc::clone(&wrote),
        };
        let inner = OutputSession::open(io, spool(&root), config())
            .await
            .expect("session");
        let mut session = spawn_output_actor(inner, config().max_frame_bytes);
        {
            let current = frame(1, false);
            let wait = session.send(&current);
            tokio::pin!(wait);
            tokio::select! {
                _result = &mut wait => panic!("send returned before close"),
                () = wrote.notified() => {}
            }
        }
        timeout(Duration::from_secs(1), session.close())
            .await
            .expect("bounded close")
            .expect("closed actor");
        let mut reopened = spool(&root);
        assert_eq!(reopened.pending().expect("pending frame").len(), 1);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn drop_interrupts_an_ack_blocked_actor_and_eventually_releases_the_spool_lock() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let (acknowledgements, receiver) = mpsc::channel(1);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let wrote = Arc::new(Notify::new());
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::new(Mutex::new(Vec::new())),
            wrote: Arc::clone(&wrote),
        };
        let inner = OutputSession::open(io, spool(&root), config())
            .await
            .expect("session");
        let mut session = spawn_output_actor(inner, config().max_frame_bytes);
        {
            let current = frame(1, false);
            let wait = session.send(&current);
            tokio::pin!(wait);
            tokio::select! {
                _result = &mut wait => panic!("send returned before drop"),
                () = wrote.notified() => {}
            }
        }
        drop(session);

        let mut reopened = timeout(Duration::from_secs(1), async {
            loop {
                match try_spool(&root) {
                    Ok(spool) => break spool,
                    Err(SpoolError::OwnershipUnavailable(_)) => tokio::task::yield_now().await,
                    Err(error) => panic!("unexpected reopen failure: {error}"),
                }
            }
        })
        .await
        .expect("bounded drop cleanup");
        assert_eq!(reopened.pending().expect("pending frame").len(), 1);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn oversized_frame_is_rejected_before_actor_queue_admission() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let (acknowledgements, receiver) = mpsc::channel(1);
        acknowledgements
            .send(bootstrap())
            .await
            .expect("bootstrap ACK");
        let writes = Arc::new(Mutex::new(Vec::new()));
        let io = GatedStream {
            acknowledgements: receiver,
            writes: Arc::clone(&writes),
            wrote: Arc::new(Notify::new()),
        };
        let inner = OutputSession::open(io, spool(&temp.path().join("root")), config())
            .await
            .expect("session");
        let mut session = spawn_output_actor(inner, config().max_frame_bytes);
        let mut oversized = frame(1, false);
        oversized.logical_output_id = "x".repeat(2_048);
        assert!(matches!(
            session.send(&oversized).await,
            Err(OutputGrpcError::Protocol(
                OutputSessionError::ResourceExhausted(_)
            ))
        ));
        assert!(writes.lock().expect("writes lock").is_empty());
        session.close().await.expect("close actor");
    }

    #[tokio::test(flavor = "current_thread")]
    async fn malformed_bootstrap_and_early_eof_leave_spool_intact() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let mut invalid = bootstrap();
        invalid.credit_frames = 3;
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(invalid))]),
            writes: Vec::new(),
            closed: false,
        };
        assert!(matches!(
            OutputSession::open(io, spool(&temp.path().join("root")), config()).await,
            Err(OutputGrpcError::Protocol(OutputSessionError::InvalidInput(
                _
            )))
        ));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn early_eof_during_replay_preserves_the_durable_frame() {
        let temp = tempfile::tempdir().expect("temporary directory");
        let root = temp.path().join("root");
        let pending_frame = frame(1, false);
        let mut initial_spool = spool(&root);
        initial_spool
            .put(
                NonZeroU64::new(1).expect("nonzero"),
                &pending_frame.encode_to_vec(),
            )
            .expect("pending frame");
        let io = FakeStream {
            acknowledgements: VecDeque::from([Ok(Some(bootstrap())), Ok(None)]),
            writes: Vec::new(),
            closed: false,
        };
        assert!(matches!(
            OutputSession::open(io, initial_spool, config()).await,
            Err(OutputGrpcError::Unavailable(_))
        ));
        let mut reopened = spool(&root);
        let pending = reopened.pending().expect("preserved pending frame");
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].payload, pending_frame.encode_to_vec());
    }

    #[test]
    fn metadata_is_exact_ascii_bounded_and_control_free() {
        assert!(valid_metadata_value("session-1"));
        assert!(!valid_metadata_value(""));
        assert!(!valid_metadata_value("line\nfeed"));
        assert!(!valid_metadata_value("nul\0byte"));
        assert!(!valid_metadata_value("é"));
        assert!(!valid_metadata_value(&"x".repeat(257)));
    }

    #[test]
    fn deployment_limits_are_bounded_to_protocol_v1() {
        let mut invalid = config();
        invalid.max_queued_frames = 129;
        assert!(invalid.validate().is_err());
        let mut invalid = config();
        invalid.max_frame_bytes = MAX_OUTPUT_FRAME_BYTES + 1;
        assert!(invalid.validate().is_err());
        let mut invalid = config();
        invalid.ack_timeout = Duration::from_secs(301);
        assert!(invalid.validate().is_err());
        let mut invalid = config();
        invalid.stream_deadline = Duration::from_secs(3_601);
        assert!(invalid.validate().is_err());
    }
}
