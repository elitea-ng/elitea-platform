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
use crate::transport::command_bus::{CommandDelivery, CommandRetirementClient, DeliveryVerdict};

pub(super) struct ExecutionDeliveryProcessor<R, RC, T, K, D, I> {
    authenticator: Arc<dyn SignedCommandAuthenticator>,
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
            agent,
            toolkit,
        }
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
