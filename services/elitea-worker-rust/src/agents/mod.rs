//! Language-neutral agent execution contracts.

pub(crate) mod application_tools;
pub(crate) mod assembly;
pub(crate) mod attachments;
pub(crate) mod context_management;
pub(crate) mod direct_hitl;
pub(crate) mod events;
pub mod graph;
pub(crate) mod internal_tools;
pub(crate) mod native_runtime;
pub(crate) mod ordinary;
pub(crate) mod pipeline;
pub mod protocol;
mod replay_history;
pub mod request;
pub mod result;
mod runner_history;
pub(crate) mod runtime;
pub(crate) mod sensitive_tools;
pub(crate) mod session;
pub(crate) mod variables;

#[cfg(test)]
mod assembly_tests;
#[cfg(test)]
mod context_management_tests;
#[cfg(test)]
mod direct_hitl_tests;
#[cfg(test)]
mod events_tests;
#[cfg(test)]
mod native_runtime_tests;
#[cfg(test)]
mod ordinary_tests;
#[cfg(test)]
mod pipeline_tests;
#[cfg(test)]
mod runtime_tests;
#[cfg(test)]
mod session_tests;

pub use crate::protocol::ProtocolError as AgentProtocolError;
pub use protocol::{AGENT_INPUT_SCHEMA_REVISION, parse_agent_execution_input, request_from};
pub use request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    NextInputSuggestionPolicy, UserInput,
};
pub use result::{
    AGENT_RESULT_CLASSIFICATION, AGENT_RESULT_MEDIA_TYPE, AgentResultArtifact, AgentTerminalState,
    BoundAgentExecutionResult, bind_result_artifact,
};
