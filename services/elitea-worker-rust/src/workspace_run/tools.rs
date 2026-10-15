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

/// The provider for one turn: `run_command` only when the project's shell
/// switch is on.
#[must_use]
pub(crate) fn provider(sandbox: Arc<dyn WorkspaceSandbox>, shell: bool) -> WorkspaceToolProvider {
    WorkspaceToolProvider::new(Arc::new(SandboxWorkspaceRunner::new(sandbox)), shell)
}

/// `open_pull_request`'s diff: Main's `WorkspacePullRequestDiff` under the
/// turn's claim (ADR-0034 decision 10). Handed to the GitHub family in
/// process; never a model argument, never an artifact.
pub(crate) struct StoreDiffSource {
    store: Arc<dyn super::store::WorkspaceStore>,
    claim: crate::protocol::elitea::runtime::v1::WorkspaceClaimV1,
}

impl StoreDiffSource {
    #[must_use]
    pub(crate) fn new(
        store: Arc<dyn super::store::WorkspaceStore>,
        claim: crate::protocol::elitea::runtime::v1::WorkspaceClaimV1,
    ) -> Self {
        Self { store, claim }
    }
}

#[async_trait]
impl elitea_agent_runtime::toolkits::workspace_pull_request::WorkspaceDiffSource
    for StoreDiffSource
{
    async fn pull_request_diff(
        &self,
    ) -> Result<
        elitea_agent_runtime::toolkits::workspace_pull_request::WorkspaceDiff,
        elitea_agent_runtime::toolkits::workspace_pull_request::DiffUnavailable,
    > {
        use super::store::{RefusalCode, StoreCallError};
        use elitea_agent_runtime::toolkits::workspace_pull_request::{
            DiffUnavailable, WorkspaceDiff,
        };
        match self.store.pull_request_diff(&self.claim).await {
            Ok(diff) => Ok(WorkspaceDiff {
                upstream_sha: diff.upstream_sha,
                upstream_branch: diff.upstream_branch,
                remote_url: diff.remote_url,
                text: diff.text,
            }),
            Err(StoreCallError::Refused { code, .. }) => Err(match code {
                RefusalCode::NoUpstream => DiffUnavailable::NoUpstream,
                RefusalCode::DiffTooLarge => DiffUnavailable::TooLarge,
                RefusalCode::DiffBinary => DiffUnavailable::Binary,
                RefusalCode::DiffEmpty => DiffUnavailable::Empty,
                _ => DiffUnavailable::Failed(
                    "The workspace store refused the change; no pull request was opened.",
                ),
            }),
            Err(_) => Err(DiffUnavailable::Failed(
                "The workspace store is unavailable; no pull request was opened.",
            )),
        }
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
