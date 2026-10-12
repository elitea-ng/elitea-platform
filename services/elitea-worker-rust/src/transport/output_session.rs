use std::fmt;

use prost::Message;

use crate::protocol::elitea::runtime::v1::{
    DesiredExecutionStateV1, ExecutionOutputAckV1, ExecutionOutputFrameV1, RuntimeErrorCodeV1,
};
use crate::spool::SpooledFrame;

use crate::protocol::output::OUTPUT_SCHEMA_REVISION;

const DEADLINE_EXCEEDED_SAFE_MESSAGE: &str = "The execution deadline was exceeded.";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct OutputSessionLimits {
    pub(crate) queued_frame_capacity: usize,
    pub(crate) queued_byte_capacity: usize,
    pub(crate) frame_byte_limit: usize,
    pub(crate) server_credit_frame_limit: u32,
    pub(crate) server_credit_byte_limit: u64,
}

impl OutputSessionLimits {
    pub(crate) fn validate(self) -> Result<Self, OutputSessionError> {
        if self.queued_frame_capacity == 0
            || self.queued_byte_capacity == 0
            || self.frame_byte_limit == 0
            || self.server_credit_frame_limit == 0
            || self.server_credit_byte_limit == 0
            || self.frame_byte_limit > self.queued_byte_capacity
        {
            return Err(OutputSessionError::InvalidInput(
                "the output session limits are malformed",
            ));
        }
        Ok(self)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OutputSessionError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    AuthorizationFailed(&'static str),
    Cancelled,
    CancellationWon,
    DeadlineWon,
    /// The output plane refused the terminal result on its content
    /// (`RESULT_REFUSED`): the worker replaces it with a FAILED terminal.
    ResultRefused,
    DependencyUnavailable,
    Draining,
}

impl fmt::Display for OutputSessionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::AuthorizationFailed(message) => formatter.write_str(message),
            Self::Cancelled => formatter.write_str("execution cancellation was observed"),
            Self::CancellationWon => {
                formatter.write_str("execution cancellation won before durable output")
            }
            Self::DeadlineWon => {
                formatter.write_str("the execution deadline won before durable output")
            }
            Self::ResultRefused => {
                formatter.write_str("the output plane refused the terminal result")
            }
            Self::DependencyUnavailable => {
                formatter.write_str("the output dependency is unavailable")
            }
            Self::Draining => formatter.write_str("the output stream is draining"),
        }
    }
}

impl std::error::Error for OutputSessionError {}

#[derive(Clone, Debug, Eq, PartialEq)]
struct StreamBinding {
    stream_id: String,
    identity: Vec<u8>,
    fence: Vec<u8>,
    claim_handoff_watermark: u64,
}

#[derive(Debug)]
pub(crate) struct DurableOutputFrame {
    message: ExecutionOutputFrameV1,
    encoded: Vec<u8>,
}

impl DurableOutputFrame {
    pub(crate) fn from_message_and_bytes(
        message: ExecutionOutputFrameV1,
        encoded: Vec<u8>,
    ) -> Self {
        Self { message, encoded }
    }

    pub(crate) fn message(&self) -> &ExecutionOutputFrameV1 {
        &self.message
    }

    pub(crate) fn encoded(&self) -> &[u8] {
        &self.encoded
    }

    pub(crate) fn sequence(&self) -> u64 {
        self.message.sequence
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct AckPlan {
    acknowledged_sequence: u64,
    credit_frames: u32,
    credit_bytes: u64,
    draining: bool,
    bootstrap: bool,
    retire_spool_through: Option<u64>,
}

impl AckPlan {
    pub(crate) fn acknowledged_sequence(self) -> u64 {
        self.acknowledged_sequence
    }

    pub(crate) fn retire_spool_through(self) -> Option<u64> {
        self.retire_spool_through
    }
}

#[derive(Clone, Debug)]
pub(crate) struct OutputSessionState {
    limits: OutputSessionLimits,
    binding: Option<StreamBinding>,
    queued_frames: usize,
    queued_bytes: usize,
    credit_frames: u32,
    credit_bytes: u64,
    acknowledged_sequence: u64,
    highest_admitted_sequence: u64,
    highest_transmitted_sequence: u64,
    bootstrap_credit_received: bool,
    draining: bool,
    terminal_admitted: bool,
}

impl OutputSessionState {
    pub(crate) fn empty(limits: OutputSessionLimits) -> Result<Self, OutputSessionError> {
        Ok(Self {
            limits: limits.validate()?,
            binding: None,
            queued_frames: 0,
            queued_bytes: 0,
            credit_frames: 0,
            credit_bytes: 0,
            acknowledged_sequence: 0,
            highest_admitted_sequence: 0,
            highest_transmitted_sequence: 0,
            bootstrap_credit_received: false,
            draining: false,
            terminal_admitted: false,
        })
    }

    pub(crate) fn restore(
        limits: OutputSessionLimits,
        pending: Vec<SpooledFrame>,
    ) -> Result<(Self, Vec<DurableOutputFrame>), OutputSessionError> {
        let mut state = Self::empty(limits)?;
        let mut restored = Vec::with_capacity(pending.len());
        let mut restored_bytes = 0usize;
        let mut previous = None;
        for spooled in pending {
            if state.terminal_admitted {
                return Err(OutputSessionError::InvalidInput(
                    "a terminal output frame must end its durable stream",
                ));
            }
            let sequence = spooled.sequence.get();
            if previous.is_some_and(|value| sequence != value + 1) {
                return Err(OutputSessionError::InvalidInput(
                    "spooled output sequences are not contiguous",
                ));
            }
            let frame = decode_spooled_frame(&spooled.payload, state.limits.frame_byte_limit)?;
            if frame.sequence != sequence {
                return Err(OutputSessionError::InvalidInput(
                    "a spooled sequence does not match its output frame",
                ));
            }
            state.bind_frame(&frame)?;
            restored_bytes = restored_bytes.checked_add(spooled.payload.len()).ok_or(
                OutputSessionError::ResourceExhausted(
                    "the restored output queue capacity was exceeded",
                ),
            )?;
            if restored.len() >= state.limits.queued_frame_capacity
                || restored_bytes > state.limits.queued_byte_capacity
            {
                return Err(OutputSessionError::ResourceExhausted(
                    "the restored output queue capacity was exceeded",
                ));
            }
            state.terminal_admitted = frame.terminal;
            restored.push(DurableOutputFrame::from_message_and_bytes(
                frame,
                spooled.payload,
            ));
            previous = Some(sequence);
        }
        if let Some(last) = previous {
            state.acknowledged_sequence = restored[0].sequence() - 1;
            state.highest_admitted_sequence = last;
        }
        Ok((state, restored))
    }

    pub(crate) fn prepare_new_frame(
        &mut self,
        frame: ExecutionOutputFrameV1,
    ) -> Result<DurableOutputFrame, OutputSessionError> {
        if self.draining {
            return Err(OutputSessionError::Draining);
        }
        if self.terminal_admitted {
            return Err(OutputSessionError::InvalidInput(
                "the logical output stream already admitted a terminal frame",
            ));
        }
        let sequence = frame.sequence;
        if sequence == 0 || sequence > i64::MAX as u64 {
            return Err(OutputSessionError::InvalidInput(
                "the output sequence is malformed",
            ));
        }
        let encoded = frame.encode_to_vec();
        if encoded.len() > self.limits.frame_byte_limit {
            return Err(OutputSessionError::ResourceExhausted(
                "the output frame exceeds the transport limit",
            ));
        }
        self.bind_frame(&frame)?;
        if self.highest_admitted_sequence == 0 {
            let handoff = self
                .binding
                .as_ref()
                .map_or(0, |binding| binding.claim_handoff_watermark);
            if sequence <= handoff {
                return Err(OutputSessionError::InvalidInput(
                    "the output sequence does not advance its claim watermark",
                ));
            }
        } else if sequence != self.highest_admitted_sequence.saturating_add(1) {
            return Err(OutputSessionError::InvalidInput(
                "output sequences are not contiguous",
            ));
        }
        self.require_queue_capacity(encoded.len())?;
        Ok(DurableOutputFrame::from_message_and_bytes(frame, encoded))
    }

    pub(crate) fn commit_persisted_admission(
        &mut self,
        frame: &DurableOutputFrame,
    ) -> Result<(), OutputSessionError> {
        self.require_queue_capacity(frame.encoded.len())?;
        self.commit_durable_admission(frame)?;
        self.queued_frames += 1;
        self.queued_bytes += frame.encoded.len();
        Ok(())
    }

    pub(crate) fn commit_durable_admission(
        &mut self,
        frame: &DurableOutputFrame,
    ) -> Result<(), OutputSessionError> {
        let expected = if self.highest_admitted_sequence == 0 {
            frame.sequence()
        } else {
            self.highest_admitted_sequence.saturating_add(1)
        };
        if frame.sequence() != expected {
            return Err(OutputSessionError::InvalidInput(
                "the persisted output admission is stale",
            ));
        }
        self.highest_admitted_sequence = frame.sequence();
        self.terminal_admitted = frame.message.terminal;
        Ok(())
    }

    pub(crate) fn queue_replay(
        &mut self,
        frame: &DurableOutputFrame,
    ) -> Result<(), OutputSessionError> {
        if frame.sequence() > self.highest_admitted_sequence {
            return Err(OutputSessionError::InvalidInput(
                "the replay frame was not restored from the durable spool",
            ));
        }
        self.require_queue_capacity(frame.encoded.len())?;
        self.queued_frames += 1;
        self.queued_bytes += frame.encoded.len();
        Ok(())
    }

    pub(crate) fn reserve_transmission(
        &mut self,
        frame: &DurableOutputFrame,
    ) -> Result<(), OutputSessionError> {
        if self.draining {
            return Err(OutputSessionError::Draining);
        }
        let encoded_bytes = u64::try_from(frame.encoded.len()).map_err(|_| {
            OutputSessionError::ResourceExhausted("the output frame size is malformed")
        })?;
        if self.credit_frames == 0 || self.credit_bytes < encoded_bytes {
            return Err(OutputSessionError::ResourceExhausted(
                "the output stream has insufficient server credit",
            ));
        }
        self.credit_frames -= 1;
        self.credit_bytes -= encoded_bytes;
        self.highest_transmitted_sequence = frame.sequence();
        Ok(())
    }

    pub(crate) fn has_transmission_credit(&self, frame: &DurableOutputFrame) -> bool {
        u64::try_from(frame.encoded.len()).is_ok_and(|encoded_bytes| {
            self.credit_frames >= 1 && self.credit_bytes >= encoded_bytes
        })
    }

    pub(crate) fn complete_transmission(
        &mut self,
        frame: &DurableOutputFrame,
    ) -> Result<(), OutputSessionError> {
        if self.queued_frames == 0 || self.queued_bytes < frame.encoded.len() {
            return Err(OutputSessionError::InvalidInput(
                "the output queue accounting is malformed",
            ));
        }
        self.queued_frames -= 1;
        self.queued_bytes -= frame.encoded.len();
        Ok(())
    }

    pub(crate) fn validate_ack(
        &self,
        ack: &ExecutionOutputAckV1,
    ) -> Result<AckPlan, OutputSessionError> {
        let rejection = RuntimeErrorCodeV1::try_from(
            ack.rejection.as_ref().map_or(0, |rejection| rejection.code),
        )
        .map_err(|_| OutputSessionError::InvalidInput("the output rejection code is malformed"))?;
        if rejection != RuntimeErrorCodeV1::Unspecified {
            return self.validate_rejection(ack, rejection);
        }
        let desired = DesiredExecutionStateV1::try_from(ack.desired_state).map_err(|_| {
            OutputSessionError::InvalidInput("the output desired state is malformed")
        })?;
        if desired == DesiredExecutionStateV1::Cancelled {
            return Err(OutputSessionError::Cancelled);
        }
        if !matches!(
            desired,
            DesiredExecutionStateV1::Running | DesiredExecutionStateV1::Draining
        ) {
            return Err(OutputSessionError::InvalidInput(
                "the output desired state is malformed",
            ));
        }
        self.validate_credit(ack)?;

        if !has_ack_binding(ack) {
            if ack.committed_contiguous_sequence != 0
                || ack.claim_handoff_watermark != 0
                || self.bootstrap_credit_received
                || desired != DesiredExecutionStateV1::Running
            {
                return Err(OutputSessionError::AuthorizationFailed(
                    "the output ACK identity does not match its stream",
                ));
            }
            return Ok(AckPlan {
                acknowledged_sequence: self.acknowledged_sequence,
                credit_frames: ack.credit_frames,
                credit_bytes: ack.credit_bytes,
                draining: false,
                bootstrap: true,
                retire_spool_through: None,
            });
        }

        self.validate_ack_binding(ack)?;
        if ack.committed_contiguous_sequence < self.acknowledged_sequence {
            return Err(OutputSessionError::InvalidInput(
                "the output ACK watermark moved backwards",
            ));
        }
        if ack.committed_contiguous_sequence > self.highest_transmitted_sequence {
            return Err(OutputSessionError::InvalidInput(
                "the output ACK exceeds the highest transmitted sequence",
            ));
        }
        Ok(AckPlan {
            acknowledged_sequence: ack.committed_contiguous_sequence,
            credit_frames: ack.credit_frames,
            credit_bytes: ack.credit_bytes,
            draining: desired == DesiredExecutionStateV1::Draining,
            bootstrap: false,
            retire_spool_through: (ack.committed_contiguous_sequence > self.acknowledged_sequence)
                .then_some(ack.committed_contiguous_sequence),
        })
    }

    pub(crate) fn validate_bootstrap_ack(
        &self,
        ack: &ExecutionOutputAckV1,
    ) -> Result<AckPlan, OutputSessionError> {
        if has_ack_binding(ack) || ack.rejection.is_some() {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output bootstrap ACK is malformed",
            ));
        }
        let plan = self.validate_ack(ack)?;
        if !plan.bootstrap {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output bootstrap ACK is malformed",
            ));
        }
        Ok(plan)
    }

    pub(crate) fn commit_ack(&mut self, plan: AckPlan) {
        if plan.bootstrap {
            self.bootstrap_credit_received = true;
        }
        self.acknowledged_sequence = plan.acknowledged_sequence;
        self.credit_frames = plan.credit_frames;
        self.credit_bytes = plan.credit_bytes;
        self.draining = plan.draining;
    }

    pub(crate) fn acknowledged_sequence(&self) -> u64 {
        self.acknowledged_sequence
    }

    pub(crate) fn validate_replacement_binding(
        &self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<(), OutputSessionError> {
        let replacement = frame_binding(frame)?;
        if self.binding.as_ref() != Some(&replacement) {
            return Err(OutputSessionError::AuthorizationFailed(
                "the replacement output identity does not match its stream",
            ));
        }
        if frame.encoded_len() > self.limits.frame_byte_limit {
            return Err(OutputSessionError::ResourceExhausted(
                "the replacement output frame exceeds the transport limit",
            ));
        }
        Ok(())
    }

    fn require_queue_capacity(&self, encoded_bytes: usize) -> Result<(), OutputSessionError> {
        let next_bytes = self.queued_bytes.checked_add(encoded_bytes).ok_or(
            OutputSessionError::ResourceExhausted("the output queue capacity was exceeded"),
        )?;
        if self.queued_frames >= self.limits.queued_frame_capacity
            || next_bytes > self.limits.queued_byte_capacity
        {
            return Err(OutputSessionError::ResourceExhausted(
                "the output queue capacity was exceeded",
            ));
        }
        Ok(())
    }

    fn bind_frame(&mut self, frame: &ExecutionOutputFrameV1) -> Result<(), OutputSessionError> {
        let binding = frame_binding(frame)?;
        if self
            .binding
            .as_ref()
            .is_some_and(|current| current != &binding)
        {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output frame identity changed within one stream",
            ));
        }
        self.binding.get_or_insert(binding);
        Ok(())
    }

    fn validate_credit(&self, ack: &ExecutionOutputAckV1) -> Result<(), OutputSessionError> {
        if ack.credit_frames > self.limits.server_credit_frame_limit
            || ack.credit_bytes > self.limits.server_credit_byte_limit
        {
            return Err(OutputSessionError::InvalidInput(
                "output control credit exceeds the negotiated limit",
            ));
        }
        Ok(())
    }

    fn validate_ack_binding(&self, ack: &ExecutionOutputAckV1) -> Result<(), OutputSessionError> {
        let Some(binding) = self.binding.as_ref() else {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output ACK identity does not match its stream",
            ));
        };
        let identity = ack.identity.as_ref().map(Message::encode_to_vec);
        let fence = ack.fence.as_ref().map(Message::encode_to_vec);
        if ack.stream_id != binding.stream_id
            || identity.as_deref() != Some(binding.identity.as_slice())
            || fence.as_deref() != Some(binding.fence.as_slice())
            || ack.claim_handoff_watermark != binding.claim_handoff_watermark
        {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output ACK identity does not match its stream",
            ));
        }
        Ok(())
    }

    fn validate_rejection(
        &self,
        ack: &ExecutionOutputAckV1,
        rejection: RuntimeErrorCodeV1,
    ) -> Result<AckPlan, OutputSessionError> {
        match rejection {
            RuntimeErrorCodeV1::Cancelled => {
                self.validate_bound_winner(ack, DesiredExecutionStateV1::Cancelled)?;
                let retryable = ack
                    .rejection
                    .as_ref()
                    .is_some_and(|rejection| rejection.retryable);
                if retryable {
                    return Err(OutputSessionError::AuthorizationFailed(
                        "the output cancellation winner is malformed",
                    ));
                }
                Err(OutputSessionError::CancellationWon)
            }
            RuntimeErrorCodeV1::DeadlineExceeded => {
                self.validate_bound_winner(ack, DesiredExecutionStateV1::Running)?;
                let Some(error) = ack.rejection.as_ref() else {
                    return Err(OutputSessionError::AuthorizationFailed(
                        "the output deadline winner is malformed",
                    ));
                };
                if error.safe_message != DEADLINE_EXCEEDED_SAFE_MESSAGE || !error.retryable {
                    return Err(OutputSessionError::AuthorizationFailed(
                        "the output deadline winner is malformed",
                    ));
                }
                Err(OutputSessionError::DeadlineWon)
            }
            RuntimeErrorCodeV1::ResultRefused => {
                self.validate_ack_binding(ack)?;
                if ack
                    .rejection
                    .as_ref()
                    .is_none_or(|rejection| rejection.retryable)
                {
                    return Err(OutputSessionError::AuthorizationFailed(
                        "the output refusal is malformed",
                    ));
                }
                Err(OutputSessionError::ResultRefused)
            }
            RuntimeErrorCodeV1::AuthenticationFailed
            | RuntimeErrorCodeV1::AuthorizationFailed
            | RuntimeErrorCodeV1::StaleFence => Err(OutputSessionError::AuthorizationFailed(
                "the output stream was rejected",
            )),
            RuntimeErrorCodeV1::DependencyUnavailable | RuntimeErrorCodeV1::Internal
                if ack
                    .rejection
                    .as_ref()
                    .is_some_and(|rejection| rejection.retryable) =>
            {
                self.validate_bound_winner(ack, DesiredExecutionStateV1::Unspecified)?;
                Err(OutputSessionError::DependencyUnavailable)
            }
            _ => Err(OutputSessionError::InvalidInput(
                "the output service rejected a frame or stream",
            )),
        }
    }

    fn validate_bound_winner(
        &self,
        ack: &ExecutionOutputAckV1,
        desired: DesiredExecutionStateV1,
    ) -> Result<(), OutputSessionError> {
        self.validate_ack_binding(ack)?;
        if ack.desired_state != desired as i32
            || ack.committed_contiguous_sequence != 0
            || ack.credit_frames != 0
            || ack.credit_bytes != 0
        {
            return Err(OutputSessionError::AuthorizationFailed(
                "the output rejection winner is not exactly bound",
            ));
        }
        Ok(())
    }
}

fn decode_spooled_frame(
    raw: &[u8],
    max_frame_bytes: usize,
) -> Result<ExecutionOutputFrameV1, OutputSessionError> {
    if raw.is_empty() || raw.len() > max_frame_bytes {
        return Err(OutputSessionError::ResourceExhausted(
            "a spooled output frame exceeds the transport limit",
        ));
    }
    let frame = ExecutionOutputFrameV1::decode(raw)
        .map_err(|_| OutputSessionError::InvalidInput("a spooled output frame is malformed"))?;
    if frame.encode_to_vec() != raw {
        return Err(OutputSessionError::InvalidInput(
            "a spooled output frame is not canonical protocol v1",
        ));
    }
    Ok(frame)
}

fn frame_binding(frame: &ExecutionOutputFrameV1) -> Result<StreamBinding, OutputSessionError> {
    if frame.output_schema_revision != OUTPUT_SCHEMA_REVISION || frame.stream_id.is_empty() {
        return Err(OutputSessionError::InvalidInput(
            "the output frame identity is incomplete",
        ));
    }
    let identity = frame
        .identity
        .as_ref()
        .ok_or(OutputSessionError::InvalidInput(
            "the output frame identity is incomplete",
        ))?;
    let fence = frame
        .fence
        .as_ref()
        .ok_or(OutputSessionError::InvalidInput(
            "the output frame identity is incomplete",
        ))?;
    if frame.stream_id != format!("{}:{}", identity.execution_id, identity.generation)
        || frame.sequence == 0
        || frame.sequence > i64::MAX as u64
        || frame.claim_handoff_watermark >= frame.sequence
        || !valid_identity(identity)
        || !valid_fence(fence)
        || identity.generation == 0
        || identity.generation > i64::MAX as u64
        || fence.claim_attempt == 0
        || fence.claim_attempt > i64::MAX as u64
        || fence.lease_epoch == 0
        || fence.lease_epoch > i64::MAX as u64
        || frame.claim_handoff_watermark > i64::MAX as u64
    {
        return Err(OutputSessionError::InvalidInput(
            "the output frame identity exceeds the durable integer domain",
        ));
    }
    Ok(StreamBinding {
        stream_id: frame.stream_id.clone(),
        identity: identity.encode_to_vec(),
        fence: fence.encode_to_vec(),
        claim_handoff_watermark: frame.claim_handoff_watermark,
    })
}

fn has_ack_binding(ack: &ExecutionOutputAckV1) -> bool {
    !ack.stream_id.is_empty() || ack.identity.is_some() || ack.fence.is_some()
}

fn valid_identity(identity: &crate::protocol::elitea::runtime::v1::ExecutionIdentityV1) -> bool {
    [
        identity.tenant_id.as_str(),
        identity.resource_project_id.as_str(),
        identity.projection_project_id.as_str(),
        identity.command_id.as_str(),
        identity.execution_id.as_str(),
    ]
    .into_iter()
    .all(|value| !value.is_empty() && value.len() <= 256)
}

fn valid_fence(fence: &crate::protocol::elitea::runtime::v1::ExecutionFenceV1) -> bool {
    !fence.workload_session_id.is_empty()
        && fence.workload_session_id.len() <= 256
        && !fence.producer_id.is_empty()
        && fence.producer_id.len() <= 256
        && fence.fence_token.len() == 32
        && fence.fence_token.iter().any(|byte| *byte != 0)
}

#[cfg(test)]
mod tests {
    use std::num::NonZeroU64;

    use super::*;
    use crate::protocol::elitea::runtime::v1::{
        ExecutionFenceV1, ExecutionIdentityV1, RuntimeErrorV1,
    };

    fn limits() -> OutputSessionLimits {
        OutputSessionLimits {
            queued_frame_capacity: 2,
            queued_byte_capacity: 2_048,
            frame_byte_limit: 1_024,
            server_credit_frame_limit: 2,
            server_credit_byte_limit: 2_048,
        }
    }

    fn frame(sequence: u64) -> ExecutionOutputFrameV1 {
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
            terminal: false,
            settlement_proposal: None,
            payload: None,
        }
    }

    fn bootstrap_ack() -> ExecutionOutputAckV1 {
        ExecutionOutputAckV1 {
            credit_frames: 2,
            credit_bytes: 2_048,
            desired_state: DesiredExecutionStateV1::Running as i32,
            ..ExecutionOutputAckV1::default()
        }
    }

    fn bound_ack(frame: &ExecutionOutputFrameV1, sequence: u64) -> ExecutionOutputAckV1 {
        ExecutionOutputAckV1 {
            stream_id: frame.stream_id.clone(),
            identity: frame.identity.clone(),
            fence: frame.fence.clone(),
            committed_contiguous_sequence: sequence,
            claim_handoff_watermark: frame.claim_handoff_watermark,
            credit_frames: 2,
            credit_bytes: 2_048,
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        }
    }

    fn commit_bootstrap(state: &mut OutputSessionState) {
        let plan = state
            .validate_ack(&bootstrap_ack())
            .expect("bootstrap credit");
        state.commit_ack(plan);
    }

    #[test]
    fn bootstrap_credit_is_identity_free_exactly_once() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        commit_bootstrap(&mut state);
        assert_eq!(
            state.validate_ack(&bootstrap_ack()),
            Err(OutputSessionError::AuthorizationFailed(
                "the output ACK identity does not match its stream"
            ))
        );
    }

    #[test]
    fn restored_binding_does_not_make_a_second_bootstrap_legal() {
        let restored_frame = frame(1);
        let pending = vec![SpooledFrame {
            sequence: NonZeroU64::new(1).expect("nonzero"),
            payload: restored_frame.encode_to_vec(),
        }];
        let (mut state, _) =
            OutputSessionState::restore(limits(), pending).expect("restored state");
        commit_bootstrap(&mut state);
        assert!(matches!(
            state.validate_ack(&bootstrap_ack()),
            Err(OutputSessionError::AuthorizationFailed(_))
        ));
    }

    #[test]
    fn admission_is_contiguous_and_advances_the_handoff_watermark() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let mut first = frame(4);
        first.claim_handoff_watermark = 3;
        let first = state.prepare_new_frame(first).expect("first frame");
        state
            .commit_persisted_admission(&first)
            .expect("persisted frame");

        let mut skipped = frame(6);
        skipped.claim_handoff_watermark = 3;
        assert_eq!(
            state.prepare_new_frame(skipped).unwrap_err(),
            OutputSessionError::InvalidInput("output sequences are not contiguous")
        );
    }

    #[test]
    fn queue_frame_and_byte_limits_are_enforced_before_admission() {
        let one_frame_limits = OutputSessionLimits {
            queued_frame_capacity: 1,
            ..limits()
        };
        let mut state = OutputSessionState::empty(one_frame_limits).expect("valid limits");
        let first = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&first)
            .expect("persisted frame");
        assert!(matches!(
            state.prepare_new_frame(frame(2)),
            Err(OutputSessionError::ResourceExhausted(_))
        ));
    }

    #[test]
    fn transmission_consumes_credit_and_ack_retires_only_durable_spool() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        commit_bootstrap(&mut state);
        let frame = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        state.reserve_transmission(&frame).expect("server credit");
        state
            .complete_transmission(&frame)
            .expect("queue accounting");

        let plan = state
            .validate_ack(&bound_ack(frame.message(), 1))
            .expect("bound ACK");
        assert_eq!(plan.retire_spool_through(), Some(1));
        assert_eq!(state.acknowledged_sequence(), 0);
        state.commit_ack(plan);
        assert_eq!(state.acknowledged_sequence(), 1);
    }

    #[test]
    fn ack_cannot_advance_past_the_highest_transmitted_frame() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let frame = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        assert_eq!(
            state.validate_ack(&bound_ack(frame.message(), 1)),
            Err(OutputSessionError::InvalidInput(
                "the output ACK exceeds the highest transmitted sequence"
            ))
        );
    }

    #[test]
    fn ack_binding_must_match_the_exact_stream_identity_and_fence() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        commit_bootstrap(&mut state);
        let frame = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        state.reserve_transmission(&frame).expect("server credit");
        let mut ack = bound_ack(frame.message(), 1);
        ack.fence.as_mut().expect("fence").fence_token[0] ^= 1;
        assert!(matches!(
            state.validate_ack(&ack),
            Err(OutputSessionError::AuthorizationFailed(_))
        ));
    }

    #[test]
    fn backwards_ack_and_excess_credit_are_rejected() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        commit_bootstrap(&mut state);
        let frame = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        state.reserve_transmission(&frame).expect("server credit");
        let first = state
            .validate_ack(&bound_ack(frame.message(), 1))
            .expect("first ACK");
        state.commit_ack(first);

        let mut backwards = bound_ack(frame.message(), 0);
        backwards.credit_frames = 1;
        assert!(matches!(
            state.validate_ack(&backwards),
            Err(OutputSessionError::InvalidInput(_))
        ));
        let mut excess = bound_ack(frame.message(), 1);
        excess.credit_frames = 3;
        assert!(matches!(
            state.validate_ack(&excess),
            Err(OutputSessionError::InvalidInput(_))
        ));
    }

    #[test]
    fn draining_ack_closes_new_admission() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        commit_bootstrap(&mut state);
        let frame = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        state.reserve_transmission(&frame).expect("server credit");
        let mut ack = bound_ack(frame.message(), 1);
        ack.desired_state = DesiredExecutionStateV1::Draining as i32;
        let plan = state.validate_ack(&ack).expect("draining ACK");
        state.commit_ack(plan);
        assert_eq!(
            state.prepare_new_frame(super::tests::frame(2)).unwrap_err(),
            OutputSessionError::Draining
        );
    }

    #[test]
    fn exact_cancellation_and_deadline_winners_are_typed() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let frame = state.prepare_new_frame(frame(1)).expect("bound frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        let mut cancellation = bound_ack(frame.message(), 0);
        cancellation.credit_frames = 0;
        cancellation.credit_bytes = 0;
        cancellation.desired_state = DesiredExecutionStateV1::Cancelled as i32;
        cancellation.rejection = Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Cancelled as i32,
            safe_message: "Execution cancellation won before this output became durable."
                .to_owned(),
            retryable: false,
        });
        assert_eq!(
            state.validate_ack(&cancellation),
            Err(OutputSessionError::CancellationWon)
        );

        let mut deadline = cancellation;
        deadline.desired_state = DesiredExecutionStateV1::Running as i32;
        deadline.rejection = Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::DeadlineExceeded as i32,
            safe_message: DEADLINE_EXCEEDED_SAFE_MESSAGE.to_owned(),
            retryable: true,
        });
        assert_eq!(
            state.validate_ack(&deadline),
            Err(OutputSessionError::DeadlineWon)
        );
    }

    #[test]
    fn malformed_winner_cannot_authorize_terminal_replacement() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let frame = state.prepare_new_frame(frame(1)).expect("bound frame");
        state
            .commit_persisted_admission(&frame)
            .expect("persisted frame");
        let mut ack = bound_ack(frame.message(), 0);
        ack.credit_frames = 0;
        ack.credit_bytes = 0;
        ack.desired_state = DesiredExecutionStateV1::Cancelled as i32;
        ack.rejection = Some(RuntimeErrorV1 {
            code: RuntimeErrorCodeV1::Cancelled as i32,
            safe_message: "forged".to_owned(),
            retryable: true,
        });
        assert!(matches!(
            state.validate_ack(&ack),
            Err(OutputSessionError::AuthorizationFailed(_))
        ));
    }

    #[test]
    fn restored_frames_are_canonical_contiguous_and_identity_bound() {
        let first = frame(4);
        let second = frame(5);
        let pending = vec![
            SpooledFrame {
                sequence: NonZeroU64::new(4).expect("nonzero"),
                payload: first.encode_to_vec(),
            },
            SpooledFrame {
                sequence: NonZeroU64::new(5).expect("nonzero"),
                payload: second.encode_to_vec(),
            },
        ];
        let (state, restored) =
            OutputSessionState::restore(limits(), pending).expect("restored frames");
        assert_eq!(state.acknowledged_sequence(), 3);
        assert_eq!(restored.len(), 2);
        assert_eq!(restored[1].sequence(), 5);
    }

    #[test]
    fn restored_gap_sequence_mismatch_and_unknown_wire_fail_closed() {
        let first = frame(1);
        let second = frame(3);
        let gap = vec![
            SpooledFrame {
                sequence: NonZeroU64::new(1).expect("nonzero"),
                payload: first.encode_to_vec(),
            },
            SpooledFrame {
                sequence: NonZeroU64::new(3).expect("nonzero"),
                payload: second.encode_to_vec(),
            },
        ];
        assert!(matches!(
            OutputSessionState::restore(limits(), gap),
            Err(OutputSessionError::InvalidInput(_))
        ));

        let mismatch = vec![SpooledFrame {
            sequence: NonZeroU64::new(2).expect("nonzero"),
            payload: frame(1).encode_to_vec(),
        }];
        assert!(matches!(
            OutputSessionState::restore(limits(), mismatch),
            Err(OutputSessionError::InvalidInput(_))
        ));

        let mut unknown = frame(1).encode_to_vec();
        unknown.extend_from_slice(&[0xd8, 0x07, 0x01]);
        let unknown = vec![SpooledFrame {
            sequence: NonZeroU64::new(1).expect("nonzero"),
            payload: unknown,
        }];
        assert!(matches!(
            OutputSessionState::restore(limits(), unknown),
            Err(OutputSessionError::InvalidInput(_))
        ));
    }

    #[test]
    fn restored_sequence_above_postgres_bigint_fails_closed() {
        let oversized = i64::MAX as u64 + 1;
        let mut frame = frame(oversized);
        frame.claim_handoff_watermark = i64::MAX as u64;
        let pending = vec![SpooledFrame {
            sequence: NonZeroU64::new(oversized).expect("nonzero"),
            payload: frame.encode_to_vec(),
        }];
        assert!(matches!(
            OutputSessionState::restore(limits(), pending),
            Err(OutputSessionError::InvalidInput(_))
        ));
    }

    #[test]
    fn changed_identity_inside_one_stream_is_rejected() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let first = state.prepare_new_frame(frame(1)).expect("first frame");
        state
            .commit_persisted_admission(&first)
            .expect("persisted frame");
        let mut changed = frame(2);
        changed.identity.as_mut().expect("identity").command_id = "other".to_owned();
        assert!(matches!(
            state.prepare_new_frame(changed),
            Err(OutputSessionError::AuthorizationFailed(_))
        ));
    }

    #[test]
    fn durable_integer_domain_rejects_values_above_postgres_bigint() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let mut oversized = frame(i64::MAX as u64 + 1);
        oversized.claim_handoff_watermark = i64::MAX as u64;
        assert!(matches!(
            state.prepare_new_frame(oversized),
            Err(OutputSessionError::InvalidInput(_))
        ));

        let mut oversized_fence = frame(1);
        oversized_fence.fence.as_mut().expect("fence").lease_epoch = i64::MAX as u64 + 1;
        assert!(matches!(
            state.prepare_new_frame(oversized_fence),
            Err(OutputSessionError::InvalidInput(_))
        ));
    }

    #[test]
    fn schema_stream_and_nonzero_fence_binding_are_exact() {
        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let mut wrong_stream = frame(1);
        wrong_stream.stream_id = "different".to_owned();
        assert!(matches!(
            state.prepare_new_frame(wrong_stream),
            Err(OutputSessionError::InvalidInput(_))
        ));

        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let mut wrong_revision = frame(1);
        wrong_revision.output_schema_revision = "future".to_owned();
        assert!(matches!(
            state.prepare_new_frame(wrong_revision),
            Err(OutputSessionError::InvalidInput(_))
        ));

        let mut state = OutputSessionState::empty(limits()).expect("valid limits");
        let mut zero_fence = frame(1);
        zero_fence.fence.as_mut().expect("fence").fence_token = vec![0; 32];
        assert!(matches!(
            state.prepare_new_frame(zero_fence),
            Err(OutputSessionError::InvalidInput(_))
        ));
    }
}
