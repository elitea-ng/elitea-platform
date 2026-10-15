//! The cloud turn's tools: `elitea-agent-runtime`'s `WorkspaceToolProvider`
//! over the turn's sandbox (ADR-0034 decision 9).

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::workspace_tools::{WorkspaceRunner, WorkspaceToolProvider};
use serde_json::Value;

use super::sandbox::{SandboxRunError, WorkspaceSandbox};

/// Sends each tool call to the sandbox's workspace runner.
pub(crate) struct SandboxWorkspaceRunner {
    sandbox: Arc<dyn WorkspaceSandbox>,
}

impl SandboxWorkspaceRunner {
    #[must_use]
    pub(crate) fn new(sandbox: Arc<dyn WorkspaceSandbox>) -> Self {
        Self { sandbox }
    }
}

#[async_trait]
impl WorkspaceRunner for SandboxWorkspaceRunner {
    async fn call(&self, tool: &str, call_id: &str, args: Value) -> Result<Value, HostError> {
        self.sandbox
            .call_tool(tool, call_id, args)
            .await
            .map_err(|error| match error {
                SandboxRunError::Timeout => HostError::new(
                    HostErrorCode::ResourceExhausted,
                    "the workspace tool ran out of time",
                ),
                SandboxRunError::Protocol => HostError::new(
                    HostErrorCode::InvalidResponse,
                    "the workspace runner answered out of protocol",
                ),
                _ => HostError::new(
                    HostErrorCode::DependencyUnavailable,
                    "the workspace sandbox is unavailable",
                ),
            }
            .with_reason(error.code()))
    }
}

/// The provider for one turn: `run_command` only when the project's shell
/// switch is on.
#[must_use]
pub(crate) fn provider(sandbox: Arc<dyn WorkspaceSandbox>, shell: bool) -> WorkspaceToolProvider {
    WorkspaceToolProvider::new(Arc::new(SandboxWorkspaceRunner::new(sandbox)), shell)
}
