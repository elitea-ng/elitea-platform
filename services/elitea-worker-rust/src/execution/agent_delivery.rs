use std::fmt;
use std::sync::Arc;

use crate::agents::AgentExecutionKind;
use crate::protocol::ProtocolError;
use crate::protocol::command::{
    SignedCommandAuthenticator, VerifiedAgentCommand, parse_and_verify_agent_command,
};
use crate::protocol::control::{
    AcceptedAgentClaim, AcceptedTerminalClaimRecovery, AgentClaimDecision, AgentControlClient,
    AgentControlError, AgentOutputRecovery, AgentOutputRecoveryKind, RecoveredSettlement,
    TerminalRedeliveryKind,
};
use crate::protocol::elitea::runtime::v1::{ExecutionOutcomeV1, execution_output_frame_v1};
use crate::protocol::output::{
    ValidatedAgentOutputFrameKind, validate_restored_agent_output_frame,
};
use crate::spool::ExecutionSpoolIdentity;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandError, RedisCommandRetirer, RedisRetirementClient,
};

/// Stable failure categories for the shared application/ad-hoc delivery route.
#[derive(Debug)]
pub enum AgentDeliveryError {
    Protocol(ProtocolError),
    Control(AgentControlError),
    Retirement(RedisCommandError),
}

impl AgentDeliveryError {
    /// Stable low-cardinality category for delivery diagnostics.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::Protocol(ProtocolError::InvalidInput(_)) => "agent_delivery.invalid_input",
            Self::Protocol(ProtocolError::ResourceExhausted(_)) => {
                "agent_delivery.resource_exhausted"
            }
            Self::Protocol(ProtocolError::IncompatibleVersion(_)) => {
                "agent_delivery.incompatible_version"
            }
            Self::Protocol(ProtocolError::AuthorizationFailed(_)) => {
                "agent_delivery.authorization_failed"
            }
            Self::Protocol(ProtocolError::UnsupportedCapability(_)) => {
                "agent_delivery.unsupported_capability"
            }
            Self::Control(error) if error.retryable() => "agent_delivery.control_unavailable",
            Self::Control(_) => "agent_delivery.control_rejected",
            Self::Retirement(error) => error.code(),
        }
    }

    /// Whether redelivery can make progress without changing immutable input.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        match self {
            Self::Control(error) => error.retryable(),
            Self::Retirement(error) => error.retryable(),
            Self::Protocol(_) => false,
        }
    }
}

impl fmt::Display for AgentDeliveryError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Protocol(error) => error.fmt(formatter),
            Self::Control(error) => error.fmt(formatter),
            Self::Retirement(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for AgentDeliveryError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Protocol(error) => Some(error),
            Self::Control(error) => Some(error),
            Self::Retirement(error) => Some(error),
        }
    }
}

impl From<ProtocolError> for AgentDeliveryError {
    fn from(value: ProtocolError) -> Self {
        Self::Protocol(value)
    }
}

impl From<AgentControlError> for AgentDeliveryError {
    fn from(value: AgentControlError) -> Self {
        Self::Control(value)
    }
}

impl From<RedisCommandError> for AgentDeliveryError {
    fn from(value: RedisCommandError) -> Self {
        Self::Retirement(value)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentDeliveryRouteKind {
    Fresh,
    OutputRecovery,
    RetryLaterNoAck,
    Completed,
}

/// An exact verified delivery and accepted claim retained for the future
/// `BeginExecution` stage.
///
/// Its fields remain private so later execution stages cannot bypass the
/// shared router or manufacture fresh business-input authority.
pub struct FreshAgentDelivery {
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    claim_handoff_watermark: u64,
    claim: AcceptedAgentClaim,
}

impl FreshAgentDelivery {
    #[must_use]
    pub const fn execution_kind(&self) -> AgentExecutionKind {
        self.verified.kind()
    }

    #[must_use]
    pub const fn claim_handoff_watermark(&self) -> u64 {
        self.claim_handoff_watermark
    }

    pub(crate) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedAgentCommand,
        AcceptedAgentClaim,
    ) {
        (self.delivery, self.verified, self.claim)
    }

    pub(crate) fn into_terminal_recovery_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedAgentCommand,
        AcceptedTerminalClaimRecovery,
    ) {
        (
            self.delivery,
            self.verified,
            self.claim.into_terminal_recovery(),
        )
    }

    #[must_use]
    pub(crate) fn spool_identity(&self) -> ExecutionSpoolIdentity {
        let command = self.verified.command();
        ExecutionSpoolIdentity {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            producer_id: self.claim.producer_id().to_owned(),
        }
    }

    #[must_use]
    pub(crate) fn matches_output_transport(
        &self,
        workload_session_id: &str,
        producer_id: &str,
    ) -> bool {
        self.claim
            .matches_output_transport(workload_session_id, producer_id)
    }

    #[must_use]
    pub(crate) fn matches_output_binding(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1,
    ) -> bool {
        self.claim.matches_output_binding(
            frame.identity.as_ref(),
            frame.fence.as_ref(),
            frame.claim_handoff_watermark,
        )
    }

    #[must_use]
    pub(crate) fn matches_output_identity(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1,
    ) -> bool {
        self.claim.matches_output_identity(frame.identity.as_ref())
    }

    pub(crate) fn validate_output_frame(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1,
    ) -> Result<ValidatedAgentOutputFrameKind, ProtocolError> {
        let kind = validate_restored_agent_output_frame(&self.verified, frame)?;
        if let Some(execution_output_frame_v1::Payload::AgentExecution(result)) =
            frame.payload.as_ref()
            && !self.claim.matches_agent_result_binding(result)
        {
            return Err(ProtocolError::InvalidInput(
                "the restored agent output frame is malformed",
            ));
        }
        Ok(kind)
    }
}

#[cfg(test)]
pub(crate) fn test_fresh_agent_delivery(
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    claim: AcceptedAgentClaim,
) -> FreshAgentDelivery {
    FreshAgentDelivery {
        delivery,
        claim_handoff_watermark: claim.claim_handoff_watermark(),
        verified,
        claim,
    }
}

/// An input-free redelivery which may only inspect and recover durable output.
pub struct OutputRecoveryAgentDelivery {
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    recovery: AgentOutputRecovery,
}

impl OutputRecoveryAgentDelivery {
    #[must_use]
    pub const fn execution_kind(&self) -> AgentExecutionKind {
        self.verified.kind()
    }

    #[must_use]
    pub const fn recovery_kind(&self) -> AgentOutputRecoveryKind {
        self.recovery.kind()
    }

    #[must_use]
    pub const fn desired_state(&self) -> crate::protocol::control::DesiredExecutionState {
        self.recovery.desired_state()
    }

    pub(crate) fn matches_output_transport(
        &self,
        workload_session_id: &str,
        producer_id: &str,
    ) -> bool {
        self.recovery
            .matches_output_transport(workload_session_id, producer_id)
    }

    pub(crate) fn spool_identity(&self) -> ExecutionSpoolIdentity {
        let command = self.verified.command();
        ExecutionSpoolIdentity {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            producer_id: self.recovery.producer_id().to_owned(),
        }
    }

    pub(crate) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedAgentCommand,
        AgentOutputRecovery,
    ) {
        (self.delivery, self.verified, self.recovery)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentDeliveryCompletionKind {
    TerminalRedelivery(TerminalRedeliveryKind),
    RecoveredTerminalSettlement,
    RecoveredSettlement,
}

/// Non-authoritative summary emitted only after atomic Redis retirement.
#[derive(Debug, Eq, PartialEq)]
pub struct AgentDeliveryCompletion {
    kind: AgentDeliveryCompletionKind,
    settlement_receipt_id: Option<String>,
    settlement_outcome: Option<ExecutionOutcomeV1>,
}

impl AgentDeliveryCompletion {
    #[must_use]
    pub const fn kind(&self) -> AgentDeliveryCompletionKind {
        self.kind
    }

    #[must_use]
    pub fn settlement_receipt_id(&self) -> Option<&str> {
        self.settlement_receipt_id.as_deref()
    }

    #[must_use]
    pub const fn settlement_outcome(&self) -> Option<ExecutionOutcomeV1> {
        self.settlement_outcome
    }
}

/// Closed result of routing all ten authenticated agent claim dispositions.
pub enum AgentDeliveryRoute {
    Fresh(Box<FreshAgentDelivery>),
    OutputRecovery(Box<OutputRecoveryAgentDelivery>),
    RetryLaterNoAck,
    Completed(AgentDeliveryCompletion),
}

impl AgentDeliveryRoute {
    #[must_use]
    pub const fn kind(&self) -> AgentDeliveryRouteKind {
        match self {
            Self::Fresh(_) => AgentDeliveryRouteKind::Fresh,
            Self::OutputRecovery(_) => AgentDeliveryRouteKind::OutputRecovery,
            Self::RetryLaterNoAck => AgentDeliveryRouteKind::RetryLaterNoAck,
            Self::Completed(_) => AgentDeliveryRouteKind::Completed,
        }
    }
}

/// Keeps checkpoint recovery separate from fresh invocation and output replay.
#[allow(dead_code)]
pub(crate) enum CheckpointDeliveryRoute {
    Ordinary(AgentDeliveryRoute),
    Inspect(Box<CheckpointAgentDelivery>),
}

#[allow(dead_code)]
pub(crate) struct CheckpointAgentDelivery {
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    inspection: crate::protocol::control::ModelCheckpointInspection,
}

#[allow(dead_code)]
impl CheckpointAgentDelivery {
    pub(crate) fn matches_output_transport(&self, session: &str, producer: &str) -> bool {
        self.inspection.matches_output_transport(session, producer)
    }
    pub(crate) fn spool_identity(&self) -> ExecutionSpoolIdentity {
        let command = self.verified.command();
        ExecutionSpoolIdentity {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
            producer_id: self.inspection.producer_id().to_owned(),
        }
    }
    pub(crate) fn covered_progress(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1,
    ) -> Result<bool, ProtocolError> {
        Ok(
            self.validate_output(frame)? == ValidatedAgentOutputFrameKind::Progress
                && frame.sequence <= self.inspection.output_watermark(),
        )
    }

    pub(crate) fn validate_output(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::ExecutionOutputFrameV1,
    ) -> Result<ValidatedAgentOutputFrameKind, ProtocolError> {
        if !self.inspection.matches_output_identity(frame) {
            return Err(ProtocolError::AuthorizationFailed(
                "the recovery output identity is invalid",
            ));
        }
        validate_restored_agent_output_frame(&self.verified, frame)
    }
    pub(crate) fn output_watermark(&self) -> u64 {
        self.inspection.output_watermark()
    }

    pub(crate) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedAgentCommand,
        crate::protocol::control::ModelCheckpointInspection,
    ) {
        (self.delivery, self.verified, self.inspection)
    }
}

#[cfg(test)]
pub(crate) fn test_checkpoint_delivery(
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    inspection: crate::protocol::control::ModelCheckpointInspection,
) -> CheckpointAgentDelivery {
    CheckpointAgentDelivery {
        delivery,
        verified,
        inspection,
    }
}

/// Shared application/ad-hoc pre-execution lifecycle owner.
///
/// This is not whole-delivery or invocation admission. It intentionally stops
/// before capacity reservation, input materialization, output-spool recovery,
/// `BeginExecution`, and ADK invocation. It does fully own terminal redelivery
/// ordering: Claim -> optional `PrepareSettlement` -> atomic Redis retirement.
/// No terminal route can return success before retirement.
pub struct AgentDeliveryRouter<R, C> {
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<C>>,
}

impl<R, C> AgentDeliveryRouter<R, C> {
    #[must_use]
    pub fn new(control: AgentControlClient<R>, retirer: RedisCommandRetirer<C>) -> Self {
        Self {
            control: Arc::new(control),
            retirer: Arc::new(retirer),
        }
    }

    /// Share the exact control and retirement owners with the later invocation
    /// coordinator. This prevents route and lifecycle wiring from drifting to
    /// different worker identities or Redis generations.
    #[must_use]
    pub(crate) const fn from_shared(
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<C>>,
    ) -> Self {
        Self { control, retirer }
    }
}

impl<R, C> AgentDeliveryRouter<R, C>
where
    R: ControlRpc,
    C: RedisRetirementClient,
{
    /// Verify, claim, and route one agent delivery without conflating fresh
    /// execution authority with recovery or terminal retirement authority.
    ///
    /// # Errors
    ///
    /// Returns a typed protocol, control, or retirement failure. A failure or
    /// no-ACK route never calls a generic Redis ACK/delete operation.
    pub async fn route(
        &self,
        delivery: RedisCommandDelivery,
        authenticator: &dyn SignedCommandAuthenticator,
        now_unix_millis: i64,
    ) -> Result<AgentDeliveryRoute, AgentDeliveryError> {
        let verified =
            parse_and_verify_agent_command(delivery.signed_envelope(), Some(authenticator))?;
        self.route_verified(delivery, verified, now_unix_millis)
            .await
    }

    /// Claim and route a command authenticated by the whole-delivery owner.
    /// Keeping this entrypoint crate-private lets that owner establish the
    /// remote-parent execution span before the first control-plane call while
    /// preserving the public verify-and-route contract above.
    pub(crate) async fn route_verified(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedAgentCommand,
        now_unix_millis: i64,
    ) -> Result<AgentDeliveryRoute, AgentDeliveryError> {
        tracing::info!(event = "agent_claim_started");
        let decision = self
            .control
            .claim_agent_delivery(&verified, now_unix_millis)
            .await?;
        self.route_claim_decision(delivery, verified, decision)
            .await
    }

    /// The recovery coordinator alone opts in after it can own the full lifecycle.
    #[allow(dead_code)]
    pub(crate) async fn route_checkpoint_verified(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedAgentCommand,
        now_unix_millis: i64,
    ) -> Result<CheckpointDeliveryRoute, AgentDeliveryError> {
        use crate::protocol::control::CheckpointClaimDecision;
        match self
            .control
            .claim_agent_checkpoint_delivery(&verified, now_unix_millis)
            .await?
        {
            CheckpointClaimDecision::Inspect(inspection) => Ok(CheckpointDeliveryRoute::Inspect(
                Box::new(CheckpointAgentDelivery {
                    delivery,
                    verified,
                    inspection: *inspection,
                }),
            )),
            CheckpointClaimDecision::Ordinary(decision) => self
                .route_claim_decision(delivery, verified, *decision)
                .await
                .map(CheckpointDeliveryRoute::Ordinary),
        }
    }

    async fn route_claim_decision(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedAgentCommand,
        decision: AgentClaimDecision,
    ) -> Result<AgentDeliveryRoute, AgentDeliveryError> {
        match decision {
            AgentClaimDecision::Accepted(claim) => {
                tracing::info!(
                    event = "agent_claim_completed",
                    claim_disposition = "accepted",
                );
                Ok(AgentDeliveryRoute::Fresh(Box::new(FreshAgentDelivery {
                    delivery,
                    verified,
                    claim_handoff_watermark: claim.claim_handoff_watermark(),
                    claim: *claim,
                })))
            }
            AgentClaimDecision::ActiveLeaseNoAck(recovery)
            | AgentClaimDecision::RecoverRunningNoAck(recovery)
            | AgentClaimDecision::RecoverAmbiguousInvocationNoAck(recovery) => {
                tracing::warn!(
                    event = "agent_claim_completed",
                    claim_disposition = "output_recovery",
                    retryable = true,
                );
                Ok(AgentDeliveryRoute::OutputRecovery(Box::new(
                    OutputRecoveryAgentDelivery {
                        delivery,
                        verified,
                        recovery,
                    },
                )))
            }
            AgentClaimDecision::RetryLaterNoAck(_) => {
                tracing::warn!(
                    event = "agent_claim_completed",
                    claim_disposition = "retry_later",
                    retryable = true,
                );
                Ok(AgentDeliveryRoute::RetryLaterNoAck)
            }
            AgentClaimDecision::SettledAck(authority)
            | AgentClaimDecision::ObsoleteAck(authority)
            | AgentClaimDecision::RetiredAck(authority) => {
                tracing::info!(
                    event = "agent_claim_completed",
                    claim_disposition = "terminal_redelivery",
                );
                let kind = authority.kind();
                self.retirer
                    .retire_agent_command(delivery, &verified, authority.into())
                    .await?;
                Ok(AgentDeliveryRoute::Completed(AgentDeliveryCompletion {
                    kind: AgentDeliveryCompletionKind::TerminalRedelivery(kind),
                    settlement_receipt_id: None,
                    settlement_outcome: None,
                }))
            }
            AgentClaimDecision::RecoverTerminalAck(recovery) => {
                tracing::info!(
                    event = "agent_claim_completed",
                    claim_disposition = "terminal_recovery",
                );
                let receipt = self
                    .control
                    .prepare_recovered_agent_settlement(recovery)
                    .await?;
                let completion = settlement_completion(
                    AgentDeliveryCompletionKind::RecoveredTerminalSettlement,
                    &receipt,
                );
                self.retirer
                    .retire_agent_command(delivery, &verified, receipt.into())
                    .await?;
                Ok(AgentDeliveryRoute::Completed(completion))
            }
            AgentClaimDecision::RecoverSettlement(receipt) => {
                tracing::info!(
                    event = "agent_claim_completed",
                    claim_disposition = "settlement_recovery",
                );
                let completion = recovered_settlement_completion(&receipt);
                self.retirer
                    .retire_agent_command(delivery, &verified, receipt.into())
                    .await?;
                Ok(AgentDeliveryRoute::Completed(completion))
            }
        }
    }
}

fn settlement_completion(
    kind: AgentDeliveryCompletionKind,
    receipt: &crate::protocol::control::SettlementReceipt,
) -> AgentDeliveryCompletion {
    AgentDeliveryCompletion {
        kind,
        settlement_receipt_id: Some(receipt.receipt_id().to_owned()),
        settlement_outcome: Some(receipt.outcome()),
    }
}

fn recovered_settlement_completion(receipt: &RecoveredSettlement) -> AgentDeliveryCompletion {
    AgentDeliveryCompletion {
        kind: AgentDeliveryCompletionKind::RecoveredSettlement,
        settlement_receipt_id: Some(receipt.receipt_id().to_owned()),
        settlement_outcome: Some(receipt.outcome()),
    }
}
