//! How the agent runtime reports a cloud-executor workspace failure
//! (ADR-0034): a data-free assembly error whose cause is the workspace run's
//! stable code.

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::workspace_run::turn::WorkspaceRunError;

/// A cloud turn reached a worker that has no workspace run composed: it
/// fails rather than run the agent without its files.
pub(crate) const fn workspace_run_unavailable() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "this worker cannot run a handed-off workspace turn",
    )
    .with_cause("workspace_run.unavailable", None)
}

fn code_for(error: &WorkspaceRunError) -> NativeAgentAssemblyErrorCode {
    match error {
        WorkspaceRunError::Store(store) if store.retryable() => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
        WorkspaceRunError::Sandbox(crate::workspace_run::sandbox::SandboxRunError::Unavailable) => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
        WorkspaceRunError::SecretFound { .. } | WorkspaceRunError::StaleState => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        WorkspaceRunError::Codec(crate::workspace_run::codec::CodecError::TooLarge) => {
            NativeAgentAssemblyErrorCode::ResourceExhausted
        }
        // The runner failed inside the sandbox: a platform fault, not the
        // person's input.
        WorkspaceRunError::Sandbox(
            crate::workspace_run::sandbox::SandboxRunError::RunnerFailed,
        ) => NativeAgentAssemblyErrorCode::InvalidResult,
        _ => NativeAgentAssemblyErrorCode::InvalidInput,
    }
}

/// The workspace could not be hydrated: the turn fails before the model runs.
pub(crate) fn hydrate_error(error: &WorkspaceRunError) -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        code_for(error),
        "the handed-off workspace could not be prepared",
    )
    .with_cause(error.code(), None)
}

/// The turn's work could not be published; the turn fails and nothing moved.
pub(crate) fn publish_error(error: &WorkspaceRunError) -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        code_for(error),
        "the cloud turn's changes could not be published",
    )
    .with_cause(error.code(), None)
}

#[cfg(test)]
mod tests {
    use super::hydrate_error;
    use crate::workspace_run::sandbox::SandboxRunError;
    use crate::workspace_run::turn::WorkspaceRunError;
    use elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause;

    /// A runner that failed preparing the workspace (a git baseline that
    /// could not be made) is an internal fault with its own cause, not the
    /// person's invalid input nor an out-of-protocol answer.
    #[test]
    fn a_runner_failure_is_an_internal_fault_with_its_own_cause() {
        let error = hydrate_error(&WorkspaceRunError::Sandbox(SandboxRunError::RunnerFailed));
        assert_eq!(error.code().as_str(), "native_agent.invalid_result");
        assert_eq!(
            error.cause().map(NativeAgentAssemblyCause::code),
            Some("workspace_run.runner_failed")
        );
        assert_eq!(
            error.to_string(),
            "the handed-off workspace could not be prepared"
        );
    }
}
