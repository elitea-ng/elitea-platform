//! Durable claim routing for direct external-MCP toolkit reads.

use std::fmt;
use std::sync::Arc;

use crate::protocol::ProtocolError;
use crate::protocol::command::VerifiedToolkitExecuteReadCommand;
use crate::protocol::control::{
    AcceptedAgentClaim, AcceptedTerminalClaimRecovery, AgentClaimDecision, AgentControlClient,
    AgentControlError, AgentOutputRecovery,
};
use crate::protocol::elitea::runtime::v1::{ExecutionOutputFrameV1, execution_output_frame_v1};
use crate::protocol::output::validate_restored_toolkit_execute_read_output_frame;
use crate::spool::ExecutionSpoolIdentity;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandError, RedisCommandRetirer, RedisRetirementClient,
};

#[derive(Debug)]
pub(super) enum ToolkitDeliveryError {
    Control(AgentControlError),
    Retirement(RedisCommandError),
}

impl ToolkitDeliveryError {
    pub(super) const fn code(&self) -> &'static str {
        match self {
            Self::Control(error) if error.retryable() => "toolkit_delivery.control_unavailable",
            Self::Control(_) => "toolkit_delivery.control_rejected",
            Self::Retirement(error) => error.code(),
        }
    }

    pub(super) const fn retryable(&self) -> bool {
        match self {
            Self::Control(error) => error.retryable(),
            Self::Retirement(error) => error.retryable(),
        }
    }
}

impl fmt::Display for ToolkitDeliveryError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Control(error) => error.fmt(formatter),
            Self::Retirement(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for ToolkitDeliveryError {}

impl From<AgentControlError> for ToolkitDeliveryError {
    fn from(value: AgentControlError) -> Self {
        Self::Control(value)
    }
}

impl From<RedisCommandError> for ToolkitDeliveryError {
    fn from(value: RedisCommandError) -> Self {
        Self::Retirement(value)
    }
}

pub(super) struct FreshToolkitDelivery {
    delivery: RedisCommandDelivery,
    verified: VerifiedToolkitExecuteReadCommand,
    claim: AcceptedAgentClaim,
}

impl FreshToolkitDelivery {
    pub(super) fn spool_identity(&self) -> ExecutionSpoolIdentity {
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

    pub(super) fn matches_output_transport(&self, workload: &str, producer: &str) -> bool {
        self.claim.matches_output_transport(workload, producer)
    }

    pub(super) fn matches_output_identity(&self, frame: &ExecutionOutputFrameV1) -> bool {
        self.claim.matches_output_identity(frame.identity.as_ref())
    }

    pub(super) fn matches_output_binding(&self, frame: &ExecutionOutputFrameV1) -> bool {
        self.claim.matches_output_binding(
            frame.identity.as_ref(),
            frame.fence.as_ref(),
            frame.claim_handoff_watermark,
        )
    }

    pub(super) fn validate_output_frame(
        &self,
        frame: &ExecutionOutputFrameV1,
    ) -> Result<(), ProtocolError> {
        validate_restored_toolkit_execute_read_output_frame(&self.verified, frame)?;
        if let Some(execution_output_frame_v1::Payload::ToolkitExecuteRead(result)) =
            frame.payload.as_ref()
            && !self
                .claim
                .matches_toolkit_execute_read_result_binding(result)
        {
            return Err(ProtocolError::InvalidInput(
                "the restored direct toolkit output is malformed",
            ));
        }
        Ok(())
    }

    pub(super) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedToolkitExecuteReadCommand,
        AcceptedAgentClaim,
    ) {
        (self.delivery, self.verified, self.claim)
    }

    pub(super) fn into_terminal_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedToolkitExecuteReadCommand,
        AcceptedTerminalClaimRecovery,
    ) {
        (
            self.delivery,
            self.verified,
            self.claim.into_terminal_recovery(),
        )
    }
}

pub(super) struct ToolkitOutputRecoveryDelivery {
    delivery: RedisCommandDelivery,
    verified: VerifiedToolkitExecuteReadCommand,
    recovery: AgentOutputRecovery,
}

impl ToolkitOutputRecoveryDelivery {
    pub(super) const fn recovery(&self) -> &AgentOutputRecovery {
        &self.recovery
    }

    pub(super) fn matches_output_transport(&self, workload: &str, producer: &str) -> bool {
        self.recovery.matches_output_transport(workload, producer)
    }

    pub(super) fn spool_identity(&self) -> ExecutionSpoolIdentity {
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

    pub(super) fn into_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedToolkitExecuteReadCommand,
        AgentOutputRecovery,
    ) {
        (self.delivery, self.verified, self.recovery)
    }
}

pub(super) enum ToolkitDeliveryRoute {
    Fresh(Box<FreshToolkitDelivery>),
    OutputRecovery(Box<ToolkitOutputRecoveryDelivery>),
    RetryLaterNoAck,
    Completed,
}

pub(super) struct ToolkitDeliveryRouter<R, C> {
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<C>>,
}

impl<R, C> ToolkitDeliveryRouter<R, C> {
    pub(super) const fn new(
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<C>>,
    ) -> Self {
        Self { control, retirer }
    }
}

impl<R, C> ToolkitDeliveryRouter<R, C>
where
    R: ControlRpc,
    C: RedisRetirementClient,
{
    pub(super) async fn route(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        now_unix_millis: i64,
    ) -> Result<ToolkitDeliveryRoute, ToolkitDeliveryError> {
        let decision = self
            .control
            .claim_toolkit_execute_read_delivery(&verified, now_unix_millis)
            .await?;
        match decision {
            AgentClaimDecision::Accepted(claim) => Ok(ToolkitDeliveryRoute::Fresh(Box::new(
                FreshToolkitDelivery {
                    delivery,
                    verified,
                    claim: *claim,
                },
            ))),
            AgentClaimDecision::ActiveLeaseNoAck(recovery)
            | AgentClaimDecision::RecoverRunningNoAck(recovery)
            | AgentClaimDecision::RecoverAmbiguousInvocationNoAck(recovery) => Ok(
                ToolkitDeliveryRoute::OutputRecovery(Box::new(ToolkitOutputRecoveryDelivery {
                    delivery,
                    verified,
                    recovery,
                })),
            ),
            AgentClaimDecision::RetryLaterNoAck(_) => Ok(ToolkitDeliveryRoute::RetryLaterNoAck),
            AgentClaimDecision::SettledAck(authority)
            | AgentClaimDecision::ObsoleteAck(authority)
            | AgentClaimDecision::RetiredAck(authority) => {
                self.retirer
                    .retire_toolkit_execute_read_command(delivery, &verified, authority.into())
                    .await?;
                Ok(ToolkitDeliveryRoute::Completed)
            }
            AgentClaimDecision::RecoverTerminalAck(recovery) => {
                let receipt = self
                    .control
                    .prepare_recovered_agent_settlement(recovery)
                    .await?;
                self.retirer
                    .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                    .await?;
                Ok(ToolkitDeliveryRoute::Completed)
            }
            AgentClaimDecision::RecoverSettlement(receipt) => {
                self.retirer
                    .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                    .await?;
                Ok(ToolkitDeliveryRoute::Completed)
            }
        }
    }
}
