//! Authenticated multiplexing for all capabilities on the worker command stream.

use std::sync::Arc;

use async_trait::async_trait;

use super::agent_delivery_processor::AgentDeliveryProcessor;
use super::agent_invocation::AuthorizedAgentLifecycle;
use super::agent_lease::UnixMillisClock;
use super::agent_preparation::AgentInputMaterializer;
use super::command_delivery::{CommandDeliveryProcessor, verification_poison};
use super::invocation_supervisor::InvocationSupervisionError;
use super::output_delivery::{AgentProgressConnector, AgentTerminalReplay, ToolkitTerminalReplay};
use super::toolkit_delivery_processor::{ToolkitDeliveryProcessor, process_toolkit_verified};
use crate::protocol::command::{
    SignedCommandAuthenticator, VerifiedExecutionCommandKind, parse_and_verify_execution_command,
};
use crate::transport::ControlRpc;
use crate::transport::command_bus::{
    CommandDelivery, CommandRetirementClient, DeliveryVerdict, PoisonReason,
};

pub(super) struct ExecutionDeliveryProcessor<R, RC, T, K, D, I> {
    authenticator: Arc<dyn SignedCommandAuthenticator>,
    /// `ELITEA_WORKER_GOVERNED_CONTEXT` (ADR-0039, #1115).
    governed_security_context: bool,
    agent: AgentDeliveryProcessor<R, RC, T, K, D, I>,
    toolkit: ToolkitDeliveryProcessor<R, RC, T, K, I>,
}

impl<R, RC, T, K, D, I> ExecutionDeliveryProcessor<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: CommandRetirementClient + 'static,
    T: AgentTerminalReplay + ToolkitTerminalReplay + AgentProgressConnector + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    pub(super) fn new(
        authenticator: Arc<dyn SignedCommandAuthenticator>,
        agent: AgentDeliveryProcessor<R, RC, T, K, D, I>,
        toolkit: ToolkitDeliveryProcessor<R, RC, T, K, I>,
    ) -> Self {
        Self {
            authenticator,
            governed_security_context: false,
            agent,
            toolkit,
        }
    }

    /// Serve governed commands: the delivery must still come through the
    /// governed lane, and preparation verifies its context.
    pub(super) const fn with_governed_security_context(mut self, enabled: bool) -> Self {
        self.governed_security_context = enabled;
        self
    }

    pub(super) fn stop(&self) -> Result<(), InvocationSupervisionError> {
        self.agent.stop()
    }

    pub(super) async fn close(&self) -> Result<(), InvocationSupervisionError> {
        self.agent.close().await
    }
}

#[async_trait]
impl<R, RC, T, K, D, I> CommandDeliveryProcessor for ExecutionDeliveryProcessor<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: CommandRetirementClient + 'static,
    T: AgentTerminalReplay + ToolkitTerminalReplay + AgentProgressConnector + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    async fn process(&self, delivery: CommandDelivery) -> DeliveryVerdict {
        let verified = match parse_and_verify_execution_command(
            delivery.signed_envelope(),
            Some(self.authenticator.as_ref()),
        ) {
            Ok(verified) => verified,
            Err(error) => {
                tracing::warn!(
                    event = "execution_delivery_verification_failed",
                    error_code = protocol_error_code(&error),
                    retryable = false,
                );
                return DeliveryVerdict::poison(verification_poison(&error));
            }
        };
        let command = match &verified {
            VerifiedExecutionCommandKind::Agent(verified) => verified.command(),
            VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) => verified.command(),
        };
        if let Err(verdict) = governed_lane_verdict(
            self.governed_security_context,
            command.security_context_ref.is_some(),
            delivery.coordinates().governed(),
        ) {
            return verdict;
        }
        match verified {
            VerifiedExecutionCommandKind::Agent(verified) => {
                if let Err(verdict) =
                    delivery.bind_idempotency_key(&verified.command().idempotency_key)
                {
                    tracing::warn!(event = "execution_delivery_subject_mismatch");
                    return verdict;
                }
                self.agent
                    .process_verified_delivery(delivery, verified)
                    .await;
            }
            VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) => {
                if let Err(verdict) =
                    delivery.bind_idempotency_key(&verified.command().idempotency_key)
                {
                    tracing::warn!(event = "execution_delivery_subject_mismatch");
                    return verdict;
                }
                Box::pin(process_toolkit_verified(&self.toolkit, delivery, verified)).await;
            }
        }
        DeliveryVerdict::Processed
    }
}

/// The governed-lane rule before any claim (ADR-0039, #1115). A command
/// carrying a security context reference on a worker that does not serve it
/// is an unsupported command (never claimed, so never failed in the user's
/// name). Otherwise the lane and the command must agree: a governed command
/// rides only the governed durable, a legacy one only the legacy durable.
fn governed_lane_verdict(
    serves_governed: bool,
    carries_context: bool,
    governed_lane: bool,
) -> Result<(), DeliveryVerdict> {
    if carries_context && !serves_governed {
        tracing::warn!(
            event = "execution_delivery_governed_unsupported",
            error_code = "execution_delivery.unsupported_capability",
            retryable = false,
        );
        return Err(DeliveryVerdict::poison(PoisonReason::UnsupportedCommand));
    }
    if carries_context != governed_lane {
        tracing::warn!(
            event = "execution_delivery_governed_lane_mismatch",
            governed_lane,
            retryable = false,
        );
        return Err(DeliveryVerdict::poison(PoisonReason::SubjectMismatch));
    }
    Ok(())
}

const fn protocol_error_code(error: &crate::protocol::ProtocolError) -> &'static str {
    match error {
        crate::protocol::ProtocolError::InvalidInput(_) => "execution_delivery.invalid_input",
        crate::protocol::ProtocolError::ResourceExhausted(_)
        | crate::protocol::ProtocolError::InputFieldLimit { .. } => {
            "execution_delivery.resource_exhausted"
        }
        crate::protocol::ProtocolError::IncompatibleVersion(_) => {
            "execution_delivery.incompatible_version"
        }
        crate::protocol::ProtocolError::AuthorizationFailed(_) => {
            "execution_delivery.authorization_failed"
        }
        crate::protocol::ProtocolError::UnsupportedCapability(_) => {
            "execution_delivery.unsupported_capability"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::governed_lane_verdict;
    use crate::transport::command_bus::{DeliveryVerdict, PoisonReason};

    #[allow(clippy::needless_pass_by_value)] // Reads one verdict per call.
    fn reason(result: Result<(), DeliveryVerdict>) -> Option<PoisonReason> {
        match result {
            Ok(()) => None,
            Err(DeliveryVerdict::Poison { reason, .. }) => Some(reason),
            Err(DeliveryVerdict::Processed) => panic!("a governed-lane refusal is a poison"),
        }
    }

    #[test]
    fn a_governed_command_needs_the_opt_in_and_the_governed_lane() {
        // Off: legacy commands only, byte for byte.
        assert_eq!(reason(governed_lane_verdict(false, false, false)), None);
        assert_eq!(
            reason(governed_lane_verdict(false, true, false)),
            Some(PoisonReason::UnsupportedCommand)
        );
        assert_eq!(
            reason(governed_lane_verdict(false, true, true)),
            Some(PoisonReason::UnsupportedCommand)
        );
        // On: each lane carries its own kind of command only.
        assert_eq!(reason(governed_lane_verdict(true, false, false)), None);
        assert_eq!(reason(governed_lane_verdict(true, true, true)), None);
        assert_eq!(
            reason(governed_lane_verdict(true, true, false)),
            Some(PoisonReason::SubjectMismatch)
        );
        assert_eq!(
            reason(governed_lane_verdict(true, false, true)),
            Some(PoisonReason::SubjectMismatch)
        );
    }
}
