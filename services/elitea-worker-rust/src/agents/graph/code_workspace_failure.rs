//! Typed workspace failures carry no provider messages or transport cause.
use crate::{
    agents::graph::node_recovery::NodeFailureClass,
    sandbox::client::SandboxCallError,
    transport::{InputContentError, control_grpc::ControlGrpcError},
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::agents::graph::code_remote) struct CodeWorkspaceFailure {
    class: NodeFailureClass,
}
impl CodeWorkspaceFailure {
    pub(in crate::agents::graph::code_remote) const fn class(self) -> NodeFailureClass {
        self.class
    }
    pub(in crate::agents::graph::code_remote) const fn invalid_input() -> Self {
        Self {
            class: NodeFailureClass::InvalidInput,
        }
    }
    pub(in crate::agents::graph::code_remote) const fn invalid_configuration() -> Self {
        Self {
            class: NodeFailureClass::InvalidConfiguration,
        }
    }
    pub(in crate::agents::graph::code_remote) const fn authorization() -> Self {
        Self {
            class: NodeFailureClass::AuthorizationDenied,
        }
    }
    pub(in crate::agents::graph::code_remote) const fn deadline() -> Self {
        Self {
            class: NodeFailureClass::AttemptTimeout,
        }
    }
    pub(in crate::agents::graph::code_remote) fn input(error: &InputContentError) -> Self {
        let class = match error {
            InputContentError::AuthorizationFailed(_) => NodeFailureClass::AuthorizationDenied,
            InputContentError::InvalidConfiguration(_) => NodeFailureClass::InvalidConfiguration,
            InputContentError::InvalidInput(_) | InputContentError::ResourceExhausted(_) => {
                NodeFailureClass::InvalidInput
            }
            InputContentError::Timeout(_) => NodeFailureClass::AttemptTimeout,
            InputContentError::DependencyUnavailable(_) | InputContentError::Transport(_) => {
                NodeFailureClass::DependencyUnavailable
            }
        };
        Self { class }
    }
    pub(in crate::agents::graph::code_remote) fn sandbox(error: &SandboxCallError) -> Self {
        let class = match error {
            SandboxCallError::Invalid => NodeFailureClass::InvalidInput,
            SandboxCallError::InvalidReceipt => NodeFailureClass::InvalidResult,
            SandboxCallError::Rejected => NodeFailureClass::AuthorizationDenied,
            SandboxCallError::Authorization(ControlGrpcError::InvalidConfiguration(_)) => {
                NodeFailureClass::InvalidConfiguration
            }
            SandboxCallError::Authorization(ControlGrpcError::ResourceExhausted(_)) => {
                NodeFailureClass::InvalidInput
            }
            SandboxCallError::Authorization(ControlGrpcError::Unavailable(_)) => {
                NodeFailureClass::DependencyUnavailable
            }
            SandboxCallError::Submission { code } => match code {
                tonic::Code::Unauthenticated => NodeFailureClass::AuthenticationDenied,
                tonic::Code::PermissionDenied => NodeFailureClass::AuthorizationDenied,
                tonic::Code::Cancelled => NodeFailureClass::Cancelled,
                tonic::Code::InvalidArgument => NodeFailureClass::InvalidInput,
                tonic::Code::DataLoss => NodeFailureClass::InvalidResult,
                tonic::Code::DeadlineExceeded => NodeFailureClass::AttemptTimeout,
                tonic::Code::Unavailable | tonic::Code::Aborted => {
                    NodeFailureClass::DependencyUnavailable
                }
                tonic::Code::ResourceExhausted => NodeFailureClass::RateLimited,
                _ => NodeFailureClass::Unknown,
            },
        };
        Self { class }
    }
}
impl std::fmt::Display for CodeWorkspaceFailure {
    fn fmt(&self, output: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        output.write_str("Code repository preparation is refused or unconfirmed.")
    }
}
impl std::error::Error for CodeWorkspaceFailure {}

#[cfg(test)]
#[path = "code_workspace_failure_tests.rs"]
mod tests;
