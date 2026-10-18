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
        self.call_approved(tool, call_id, args, false).await
    }

    async fn call_approved(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
        approved: bool,
    ) -> Result<Value, HostError> {
        self.sandbox
            .call_tool_approved(tool, call_id, args, approved)
            .await
            .map_err(|error| {
                match error {
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
                .with_reason(error.code())
            })
    }
}

/// The pre-dispatch question of the cloud workspace's `run_command`
/// (ADR-0034 decision 9, ADR-0029 addendum item 3): a destructive or
/// escaping command is a server HITL interrupt before anything runs; the
/// turn then publishes its work so far and pauses (decision 7), and an
/// approval resumes it in a new turn that replays the call approved.
/// Network use is not asked about: the project setting decided it.
pub(crate) struct WorkspaceCommandGate {
    sandbox: Arc<dyn WorkspaceSandbox>,
}

impl WorkspaceCommandGate {
    #[must_use]
    pub(crate) fn new(sandbox: Arc<dyn WorkspaceSandbox>) -> Self {
        Self { sandbox }
    }
}

#[async_trait]
impl adk_rust::agent::ToolCallConfirmation for WorkspaceCommandGate {
    async fn requires_confirmation(&self, tool_name: &str, args: &Value) -> bool {
        if tool_name != "run_command" {
            return false;
        }
        let Some(command) = args.get("command").and_then(Value::as_str) else {
            // Malformed: the tool refuses it without running anything.
            return false;
        };
        let cwd = args.get("cwd").and_then(Value::as_str);
        // A classification that cannot be made asks.
        self.sandbox
            .classify_command(command, cwd)
            .await
            .unwrap_or(true)
    }
}

/// The provider for one turn: `run_command` only when the project's shell
/// switch is on.
#[must_use]
pub(crate) fn provider(sandbox: Arc<dyn WorkspaceSandbox>, shell: bool) -> WorkspaceToolProvider {
    WorkspaceToolProvider::new(Arc::new(SandboxWorkspaceRunner::new(sandbox)), shell)
}

/// `open_pull_request`'s diff (ADR-0034 decision 10): computed by the
/// worker from the turn's own repository, outside the sandbox
/// ([`super::turn::WorkspaceTurn::pull_request_diff`]). Handed to the GitHub
/// family in process; never a model argument, never an artifact.
pub(crate) struct TurnDiffSource {
    turn: Arc<super::turn::WorkspaceTurn>,
}

impl TurnDiffSource {
    #[must_use]
    pub(crate) fn new(turn: Arc<super::turn::WorkspaceTurn>) -> Self {
        Self { turn }
    }
}

#[async_trait]
impl elitea_agent_runtime::toolkits::workspace_pull_request::WorkspaceDiffSource
    for TurnDiffSource
{
    async fn pull_request_diff(
        &self,
    ) -> Result<
        elitea_agent_runtime::toolkits::workspace_pull_request::WorkspaceDiff,
        elitea_agent_runtime::toolkits::workspace_pull_request::DiffUnavailable,
    > {
        self.turn.pull_request_diff().await
    }
}

/// `pull_request.opened` (decision 12): identifiers and counts, never file
/// contents. Main records the audit row from this event.
pub(crate) struct LogPullRequestAudit {
    execution_id: String,
}

impl LogPullRequestAudit {
    #[must_use]
    pub(crate) fn new(execution_id: &str) -> Self {
        Self {
            execution_id: execution_id.to_owned(),
        }
    }
}

impl elitea_agent_runtime::toolkits::workspace_pull_request::PullRequestAudit
    for LogPullRequestAudit
{
    fn opened(
        &self,
        event: &elitea_agent_runtime::toolkits::workspace_pull_request::PullRequestOpened,
    ) {
        tracing::info!(
            event = "pull_request.opened",
            audit = "pull_request.opened",
            entity_type = "workspace_handoff",
            execution_id = self.execution_id.as_str(),
            toolkit = event.toolkit.as_str(),
            repository = event.repository.as_str(),
            number = event.number,
            files = event.files,
            upstream_sha = event.upstream_sha.as_str(),
        );
    }
}
