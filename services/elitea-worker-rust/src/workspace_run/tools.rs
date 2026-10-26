//! The cloud turn's tools: `elitea-agent-runtime`'s `WorkspaceToolProvider`
//! over the turn's sandbox (ADR-0034 decision 9).

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::workspace_tools::{WorkspaceRunner, WorkspaceToolProvider};
use serde_json::Value;

use super::sandbox::{SandboxRunError, WorkspaceSandbox};
use super::wrap_up::WrapUp;

/// Sends each tool call to the sandbox's workspace runner.
pub(crate) struct SandboxWorkspaceRunner {
    sandbox: Arc<dyn WorkspaceSandbox>,
    /// The turn's take-back wrap-up (ADR-0034); `None` outside a turn.
    wrap_up: Option<Arc<WrapUp>>,
}

impl SandboxWorkspaceRunner {
    #[must_use]
    pub(crate) fn new(sandbox: Arc<dyn WorkspaceSandbox>) -> Self {
        Self {
            sandbox,
            wrap_up: None,
        }
    }

    /// Refuse new calls, and stop a long one, once the thread is taken back.
    #[must_use]
    pub(crate) fn with_wrap_up(mut self, wrap_up: Arc<WrapUp>) -> Self {
        self.wrap_up = Some(wrap_up);
        self
    }
}

fn taken_back() -> HostError {
    HostError::new(HostErrorCode::ExecutionEnded, super::wrap_up::REFUSED)
        .with_reason("workspace_run.taken_back")
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
        let call = self
            .sandbox
            .call_tool_approved(tool, call_id, args, approved);
        let result = match &self.wrap_up {
            Some(wrap_up) => super::wrap_up::guarded(wrap_up, super::wrap_up::COMMAND_GRACE, call)
                .await
                .ok_or_else(taken_back)?,
            None => call.await,
        };
        result.map_err(|error| {
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
/// switch is on. Production turns use [`turn_provider`].
#[cfg(test)]
#[must_use]
pub(crate) fn provider(sandbox: Arc<dyn WorkspaceSandbox>, shell: bool) -> WorkspaceToolProvider {
    WorkspaceToolProvider::new(Arc::new(SandboxWorkspaceRunner::new(sandbox)), shell)
}

/// The provider of a cloud turn, under its take-back wrap-up.
#[must_use]
pub(crate) fn turn_provider(
    turn: &super::turn::WorkspaceTurn,
    shell: bool,
) -> WorkspaceToolProvider {
    WorkspaceToolProvider::new(
        Arc::new(SandboxWorkspaceRunner::new(turn.sandbox()).with_wrap_up(turn.wrap_up())),
        shell,
    )
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

/// The thread's pull-request credential (amendment 2026-10-26): redeemed
/// from Main per `open_pull_request` call, outside the sandbox, and handed to
/// the GitHub family in process. A refusal is mapped to a data-free reason.
pub(crate) struct TurnCredentialSource {
    turn: Arc<super::turn::WorkspaceTurn>,
}

impl TurnCredentialSource {
    #[must_use]
    pub(crate) fn new(turn: Arc<super::turn::WorkspaceTurn>) -> Self {
        Self { turn }
    }
}

/// How a store refusal reads to the tool. Main answers a reference that no
/// longer resolves as `no_forge_credential` too: the owner chooses again.
pub(crate) fn credential_unavailable(
    error: &super::store::StoreCallError,
) -> elitea_agent_runtime::toolkits::workspace_pull_request::CredentialUnavailable {
    use super::store::{RefusalCode, StoreCallError};
    use elitea_agent_runtime::toolkits::workspace_pull_request::CredentialUnavailable;
    match error {
        StoreCallError::Refused {
            code: RefusalCode::NoForgeCredential,
            ..
        } => CredentialUnavailable::NotChosen,
        StoreCallError::Refused {
            code: RefusalCode::PullRequestsDisabled,
            ..
        } => CredentialUnavailable::Disabled,
        _ => CredentialUnavailable::Failed,
    }
}

#[async_trait]
impl elitea_agent_runtime::toolkits::workspace_pull_request::ThreadCredentialSource
    for TurnCredentialSource
{
    async fn github_configuration(
        &self,
    ) -> Result<
        zeroize::Zeroizing<String>,
        elitea_agent_runtime::toolkits::workspace_pull_request::CredentialUnavailable,
    > {
        self.turn.forge_credential().await.map_err(|error| {
            tracing::warn!(
                event = "pull_request.credential_unavailable",
                reason = error.code(),
            );
            credential_unavailable(&error)
        })
    }
}

/// `pull_request.opened` (decision 12): identifiers and counts, never file
/// contents. Main records the audit row from this event.
pub(crate) struct LogPullRequestAudit {
    execution_id: String,
    /// The turn's audit queue: Main records the row (decision 12).
    audit: Option<Arc<super::audit::ExecutorAudit>>,
}

impl LogPullRequestAudit {
    #[must_use]
    pub(crate) fn new(execution_id: &str) -> Self {
        Self {
            execution_id: execution_id.to_owned(),
            audit: None,
        }
    }

    /// Also send the event to Main's audit trail.
    #[must_use]
    pub(crate) fn with_audit(mut self, audit: Arc<super::audit::ExecutorAudit>) -> Self {
        self.audit = Some(audit);
        self
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
        if let Some(audit) = &self.audit {
            audit.record(super::audit::ExecutorEvent::PullRequestOpened {
                toolkit: event.toolkit.clone(),
                branch: event.branch.clone(),
                base: event.base.clone(),
                number: event.number,
                url: Some(event.url.clone()),
                files: u32::try_from(event.files).unwrap_or(u32::MAX),
            });
        }
    }
}

#[cfg(test)]
mod wrap_up_tests {
    use super::*;
    use crate::workspace_run::wrap_up::WrapUp;
    use std::path::Path;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[derive(Default)]
    struct CountingSandbox(AtomicUsize);

    impl CountingSandbox {
        fn calls(&self) -> usize {
            self.0.load(Ordering::SeqCst)
        }
    }

    #[async_trait]
    impl WorkspaceSandbox for CountingSandbox {
        async fn push_tree(&self, _: &Path) -> Result<(), SandboxRunError> {
            Ok(())
        }
        async fn call_tool(&self, _: &str, _: &str, _: Value) -> Result<Value, SandboxRunError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(serde_json::json!({"ok": true}))
        }
        async fn pull_tree(&self, _: &Path) -> Result<(), SandboxRunError> {
            Ok(())
        }
        async fn destroy(&self) -> Result<(), SandboxRunError> {
            Ok(())
        }
    }

    /// After a take back, a workspace tool call does not reach the sandbox:
    /// the model is told the work is being taken back.
    #[tokio::test]
    async fn a_taken_back_turn_runs_no_further_workspace_command() {
        let sandbox = Arc::new(CountingSandbox::default());
        let wrap_up = Arc::new(WrapUp::default());
        let runner = SandboxWorkspaceRunner::new(Arc::clone(&sandbox) as Arc<dyn WorkspaceSandbox>)
            .with_wrap_up(Arc::clone(&wrap_up));
        runner
            .call("read_file", "call-1", serde_json::json!({"path": "a.txt"}))
            .await
            .expect("before the take back");
        assert_eq!(sandbox.calls(), 1);
        wrap_up.request();
        let refused = runner
            .call(
                "run_command",
                "call-2",
                serde_json::json!({"command": "make"}),
            )
            .await
            .expect_err("refused");
        assert_eq!(refused.code(), HostErrorCode::ExecutionEnded);
        assert_eq!(sandbox.calls(), 1, "nothing reached the sandbox");
    }
}
