//! Shared durable execution coordination.

mod agent_coordinator;
pub mod agent_delivery;
mod agent_delivery_processor;
mod agent_invocation;
pub mod agent_lease;
pub mod agent_preparation;
mod execution_delivery_processor;
pub mod invocation_admission;
mod invocation_supervisor;
mod native_agent_lifecycle;
pub mod output_delivery;
pub(crate) mod production;
mod redis_delivery;
mod toolkit_delivery;
mod toolkit_delivery_processor;
mod toolkit_output;

#[cfg(test)]
mod agent_lease_tests;
#[cfg(test)]
mod agent_preparation_tests;
#[cfg(test)]
mod invocation_admission_tests;
#[cfg(test)]
mod invocation_supervisor_tests;
#[cfg(test)]
mod output_delivery_tests;
#[cfg(test)]
mod redis_delivery_tests;

pub use agent_delivery::{
    AgentDeliveryCompletion, AgentDeliveryCompletionKind, AgentDeliveryError, AgentDeliveryRoute,
    AgentDeliveryRouteKind, AgentDeliveryRouter, FreshAgentDelivery, OutputRecoveryAgentDelivery,
};
pub use agent_lease::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseErrorCode, ClaimLeaseMonitor,
    ClaimLeaseMonitorConfig, SystemUnixMillisClock, UnixMillisClock,
};
pub use agent_preparation::{
    AgentPreparationConfig, AgentPreparationError, AgentPreparationErrorCode,
    AgentPreparationOutcome, PreInvocationTerminalCause, PreparedAgentInvocation,
    prepare_fresh_agent_invocation,
};
pub use invocation_admission::{
    InvocationAdmission, InvocationAdmissionConfig, InvocationAdmissionError,
    InvocationAdmissionErrorCode, InvocationReservation,
};
pub use output_delivery::{
    AcceptedTerminalOutputRecovery, AcceptedTerminalRecoveryCompletion, AgentOutputPreflight,
    AgentOutputPreflightError, AgentOutputPreflightKind, AgentOutputPreflightOutcome,
    AgentOutputRecoveryRequiredKind, AgentOutputRecoveryRequiredNoAck, AgentTerminalRecoveryConfig,
    AgentTerminalRecoveryError, EmptyAgentOutput,
};
