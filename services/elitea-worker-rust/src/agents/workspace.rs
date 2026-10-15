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
