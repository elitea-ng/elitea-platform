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
    use crate::workspace_run::codec::CodecError;
    use crate::workspace_run::sandbox::SandboxRunError;
    match error {
        WorkspaceRunError::Store(store) if store.retryable() => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
        WorkspaceRunError::Store(_) => NativeAgentAssemblyErrorCode::InvalidInput,
        WorkspaceRunError::SecretFound { .. } | WorkspaceRunError::StaleState => {
            NativeAgentAssemblyErrorCode::AuthorizationFailed
        }
        WorkspaceRunError::Sandbox(sandbox) => match sandbox {
            SandboxRunError::Unavailable => NativeAgentAssemblyErrorCode::DependencyUnavailable,
            // The deployment cannot enforce what the project's policy asks.
            SandboxRunError::PolicyUnenforceable | SandboxRunError::EgressProxyUnavailable => {
                NativeAgentAssemblyErrorCode::UnsupportedCapability
            }
            // A cap, the wall time, or the memory limit.
            SandboxRunError::TooLarge
            | SandboxRunError::WorkspaceOverCap
            | SandboxRunError::Timeout
            | SandboxRunError::RunnerKilled => NativeAgentAssemblyErrorCode::ResourceExhausted,
            // The platform's own sandbox failed: not the person's input.
            SandboxRunError::Protocol
            | SandboxRunError::ArchiveRefused
            | SandboxRunError::GitBaselineFailed
            | SandboxRunError::RunnerFailed => NativeAgentAssemblyErrorCode::InvalidResult,
        },
        WorkspaceRunError::Codec(codec) => match codec {
            CodecError::TooLarge => NativeAgentAssemblyErrorCode::ResourceExhausted,
            CodecError::UnsupportedObjectFormat => {
                NativeAgentAssemblyErrorCode::UnsupportedCapability
            }
            // The person's tree holds what a commit cannot carry.
            CodecError::InvalidTree => NativeAgentAssemblyErrorCode::InvalidInput,
            CodecError::Integrity | CodecError::Io => NativeAgentAssemblyErrorCode::InvalidResult,
        },
        WorkspaceRunError::ScanFailed
        | WorkspaceRunError::RedactionFailed
        | WorkspaceRunError::Internal => NativeAgentAssemblyErrorCode::InvalidResult,
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
    /// Each sandbox failure is reported as what it is, not as the person's
    /// invalid input.
    #[test]
    fn sandbox_failures_are_labelled_by_their_cause() {
        let code = |error| {
            hydrate_error(&WorkspaceRunError::Sandbox(error))
                .code()
                .as_str()
        };
        for (error, expected) in [
            (
                SandboxRunError::Unavailable,
                "native_agent.dependency_unavailable",
            ),
            (
                SandboxRunError::PolicyUnenforceable,
                "native_agent.unsupported_capability",
            ),
            (
                SandboxRunError::EgressProxyUnavailable,
                "native_agent.unsupported_capability",
            ),
            (SandboxRunError::TooLarge, "native_agent.resource_exhausted"),
            (
                SandboxRunError::WorkspaceOverCap,
                "native_agent.resource_exhausted",
            ),
            (SandboxRunError::Timeout, "native_agent.resource_exhausted"),
            (
                SandboxRunError::RunnerKilled,
                "native_agent.resource_exhausted",
            ),
            (SandboxRunError::Protocol, "native_agent.invalid_result"),
            (
                SandboxRunError::ArchiveRefused,
                "native_agent.invalid_result",
            ),
            (
                SandboxRunError::GitBaselineFailed,
                "native_agent.invalid_result",
            ),
            (SandboxRunError::RunnerFailed, "native_agent.invalid_result"),
        ] {
            assert_eq!(code(error), expected, "{error:?}");
        }
    }

    #[test]
    fn a_runner_failure_is_an_internal_fault_with_its_own_cause() {
        let error = hydrate_error(&WorkspaceRunError::Sandbox(
            SandboxRunError::GitBaselineFailed,
        ));
        assert_eq!(error.code().as_str(), "native_agent.invalid_result");
        assert_eq!(
            error.cause().map(NativeAgentAssemblyCause::code),
            Some("workspace_run.git_baseline_failed")
        );
        assert_eq!(
            error.to_string(),
            "the handed-off workspace could not be prepared"
        );
    }
}
