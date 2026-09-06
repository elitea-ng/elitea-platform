//! Encrypted terminal preflight and bounded replay for direct toolkit reads.

use std::fmt;
use std::sync::Arc;

use prost::Message;

use super::output_delivery::{
    AgentOutputPreflight, AgentOutputPreflightError, AgentOutputSpoolFactory,
    AgentOutputSpoolReopener, PreparedAgentOutput, ToolkitTerminalReplay,
};
use super::toolkit_delivery::{FreshToolkitDelivery, ToolkitOutputRecoveryDelivery};
use crate::protocol::ProtocolError;
use crate::protocol::command::VerifiedToolkitExecuteReadCommand;
use crate::protocol::control::{AcceptedTerminalClaimRecovery, AgentOutputRecovery};
use crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1;
use crate::protocol::output::{
    RuntimeFailureKind, ToolkitExecuteReadTerminalOutput,
    build_toolkit_execute_read_terminal_output_frame,
};
use crate::spool::SpoolError;
use crate::transport::redis_commands::RedisCommandDelivery;
use crate::transport::{
    DurablyAckedTerminal, OutputGrpcError, OutputProtocolError, PreparedOutputSpool,
};

pub(super) enum ToolkitOutputPreflightOutcome {
    Empty(Box<EmptyToolkitOutput>),
    Terminal(Box<ToolkitTerminalRecovery>),
}

pub(super) struct EmptyToolkitOutput {
    fresh: FreshToolkitDelivery,
    output: PreparedAgentOutput,
}

impl EmptyToolkitOutput {
    pub(super) fn into_parts(self) -> (FreshToolkitDelivery, PreparedAgentOutput) {
        (self.fresh, self.output)
    }
}

pub(super) struct EmptyToolkitOutputRecovery {
    recovery: ToolkitOutputRecoveryDelivery,
    output: PreparedAgentOutput,
}

impl EmptyToolkitOutputRecovery {
    pub(super) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedToolkitExecuteReadCommand,
        AgentOutputRecovery,
        PreparedAgentOutput,
    ) {
        let (delivery, verified, recovery) = self.recovery.into_parts();
        (delivery, verified, recovery, self.output)
    }
}

pub(super) struct ToolkitTerminalRecovery {
    pub(super) delivery: RedisCommandDelivery,
    pub(super) verified: VerifiedToolkitExecuteReadCommand,
    pub(super) claim: AcceptedTerminalClaimRecovery,
    pub(super) spool: PreparedOutputSpool,
    pub(super) frame: ExecutionOutputFrameV1,
    pub(super) reopener: AgentOutputSpoolReopener,
}

pub(super) async fn prepare_toolkit_output(
    output: &AgentOutputPreflight,
    fresh: FreshToolkitDelivery,
) -> Result<ToolkitOutputPreflightOutcome, AgentOutputPreflightError> {
    let policy = output.shared_policy();
    if !fresh.matches_output_transport(
        &policy.output_config.workload_session_id,
        &policy.output_config.producer_id,
    ) {
        return Err(AgentOutputPreflightError::InvalidConfiguration(
            "the output transport identity does not match the accepted direct toolkit claim",
        ));
    }
    let factory = AgentOutputSpoolFactory::new(policy, Arc::new(fresh.spool_identity()));
    let prepared = factory.reopen().await?;
    let Some(frame) = prepared.pending_replay_frame() else {
        return Ok(ToolkitOutputPreflightOutcome::Empty(Box::new(
            EmptyToolkitOutput {
                fresh,
                output: PreparedAgentOutput::new(prepared, factory),
            },
        )));
    };
    if prepared.pending_frame_count() != 1
        || !fresh.matches_output_identity(&frame)
        || !fresh.matches_output_binding(&frame)
        || fresh.validate_output_frame(&frame).is_err()
    {
        return Err(AgentOutputPreflightError::InvalidDurableState(
            "the pending direct toolkit output does not match the accepted claim",
        ));
    }
    let expected = frame.encode_to_vec();
    let (delivery, verified, claim) = fresh.into_terminal_parts();
    Ok(ToolkitOutputPreflightOutcome::Terminal(Box::new(
        ToolkitTerminalRecovery {
            delivery,
            verified,
            claim,
            spool: prepared,
            frame,
            reopener: factory.seal_terminal(expected),
        },
    )))
}

pub(super) async fn prepare_empty_toolkit_recovery(
    output: &AgentOutputPreflight,
    recovery: ToolkitOutputRecoveryDelivery,
) -> Result<Option<EmptyToolkitOutputRecovery>, AgentOutputPreflightError> {
    let policy = output.shared_policy();
    if !recovery.matches_output_transport(
        &policy.output_config.workload_session_id,
        &policy.output_config.producer_id,
    ) {
        return Err(AgentOutputPreflightError::InvalidConfiguration(
            "the output transport identity does not match the direct toolkit recovery claim",
        ));
    }
    let factory = AgentOutputSpoolFactory::new(policy, Arc::new(recovery.spool_identity()));
    let prepared = factory.reopen().await?;
    if prepared.pending_frame_count() != 0 {
        return Ok(None);
    }
    Ok(Some(EmptyToolkitOutputRecovery {
        recovery,
        output: PreparedAgentOutput::new(prepared, factory),
    }))
}

#[derive(Debug)]
pub(super) enum ToolkitTerminalError {
    Protocol(ProtocolError),
    Preflight(AgentOutputPreflightError),
    Output(OutputGrpcError),
}

impl ToolkitTerminalError {
    pub(super) fn code(&self) -> &'static str {
        match self {
            Self::Protocol(_) => "toolkit_terminal.invalid_state",
            Self::Preflight(error) => error.code(),
            Self::Output(error) if reconnectable(error) => "toolkit_terminal.output_unavailable",
            Self::Output(_) => "toolkit_terminal.output_rejected",
        }
    }

    pub(super) fn retryable(&self) -> bool {
        match self {
            Self::Preflight(error) => error.retryable(),
            Self::Output(error) => reconnectable(error),
            Self::Protocol(_) => false,
        }
    }
}

impl fmt::Display for ToolkitTerminalError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Protocol(error) => {
                write!(formatter, "direct toolkit terminal is invalid: {error}")
            }
            Self::Preflight(error) => {
                write!(formatter, "direct toolkit spool recovery failed: {error}")
            }
            Self::Output(error) => {
                write!(formatter, "direct toolkit output delivery failed: {error}")
            }
        }
    }
}

impl std::error::Error for ToolkitTerminalError {}

pub(super) async fn replay_toolkit_terminal_with_replacement<T: ToolkitTerminalReplay>(
    replay: &T,
    verified: &VerifiedToolkitExecuteReadCommand,
    mut frame: ExecutionOutputFrameV1,
    spool: PreparedOutputSpool,
    mut reopener: AgentOutputSpoolReopener,
    max_output_sessions: usize,
) -> Result<(DurablyAckedTerminal, ExecutionOutputFrameV1), ToolkitTerminalError> {
    match replay_exact(
        replay,
        verified,
        &frame,
        spool,
        &reopener,
        max_output_sessions,
    )
    .await
    {
        Ok(acknowledged) => Ok((acknowledged, frame)),
        Err(error) => {
            let Some(winner) = output_winner(&error) else {
                return Err(ToolkitTerminalError::Output(error));
            };
            let fence = frame.fence.as_ref().ok_or({
                ToolkitTerminalError::Output(OutputGrpcError::Protocol(
                    OutputProtocolError::AuthorizationFailed(
                        "the direct toolkit terminal fence is unavailable for replacement",
                    ),
                ))
            })?;
            let replacement = build_toolkit_execute_read_terminal_output_frame(
                verified,
                fence,
                ToolkitExecuteReadTerminalOutput::Failure(winner),
                frame.sequence,
                frame.occurred_at_unix_millis,
                frame.claim_handoff_watermark,
            )
            .map_err(ToolkitTerminalError::Protocol)?;
            let spool = reopener
                .replace_expected_terminal(&frame, &replacement)
                .await
                .map_err(ToolkitTerminalError::Preflight)?;
            frame = replacement;
            let acknowledged = replay_exact(
                replay,
                verified,
                &frame,
                spool,
                &reopener,
                max_output_sessions,
            )
            .await
            .map_err(ToolkitTerminalError::Output)?;
            Ok((acknowledged, frame))
        }
    }
}

async fn replay_exact<T: ToolkitTerminalReplay>(
    replay: &T,
    verified: &VerifiedToolkitExecuteReadCommand,
    frame: &ExecutionOutputFrameV1,
    first: PreparedOutputSpool,
    reopener: &AgentOutputSpoolReopener,
    max_output_sessions: usize,
) -> Result<DurablyAckedTerminal, OutputGrpcError> {
    let mut first = Some(first);
    for attempt in 0..max_output_sessions {
        let spool = match first.take() {
            Some(spool) => spool,
            None => reopener.reopen().await.map_err(preflight_as_output)?,
        };
        match replay.replay_toolkit_terminal(spool, verified, frame).await {
            Ok(acknowledged) => return Ok(acknowledged),
            Err(error) if attempt + 1 < max_output_sessions && reconnectable(&error) => {}
            Err(error) => return Err(error),
        }
    }
    Err(OutputGrpcError::Unavailable(
        "the bounded direct toolkit output attempts were exhausted",
    ))
}

fn output_winner(error: &OutputGrpcError) -> Option<RuntimeFailureKind> {
    match error {
        OutputGrpcError::Protocol(OutputProtocolError::CancellationWon) => {
            Some(RuntimeFailureKind::Cancelled)
        }
        OutputGrpcError::Protocol(OutputProtocolError::DeadlineWon) => {
            Some(RuntimeFailureKind::DeadlineExceeded)
        }
        _ => None,
    }
}

fn reconnectable(error: &OutputGrpcError) -> bool {
    matches!(
        error,
        OutputGrpcError::Unavailable(_)
            | OutputGrpcError::Protocol(OutputProtocolError::DependencyUnavailable)
            | OutputGrpcError::Spool(
                SpoolError::OwnershipUnavailable(_) | SpoolError::Unavailable { .. }
            )
    )
}

fn preflight_as_output(error: AgentOutputPreflightError) -> OutputGrpcError {
    match error {
        AgentOutputPreflightError::Output(error) => error,
        AgentOutputPreflightError::InvalidConfiguration(message) => {
            OutputGrpcError::InvalidConfiguration(message)
        }
        AgentOutputPreflightError::InvalidDurableState(message) => {
            OutputGrpcError::Protocol(OutputProtocolError::AuthorizationFailed(message))
        }
        AgentOutputPreflightError::Unavailable(message) => OutputGrpcError::Unavailable(message),
    }
}
