//! Governed redemption on the direct toolkit lane (ADR-0039, #1474).
//!
//! A governed `toolkit.call_tool.v1` or `toolkit.execute.read.v1` command
//! runs its one toolkit through the command's governed seam, the same path
//! an agent uses:
//!
//! - a credentialed configured toolkit redeems per invocation through its
//!   adapter, and a toolkit no adapter governs is unsupported;
//! - an MCP connection redeems `mcp.tools.list` once and `mcp.tool.call`
//!   for the call; a connection the run cannot govern is unsupported.
//!
//! No claim-time credential reaches the toolkit. Main decides each
//! redemption: a command whose security context binds no agent identity,
//! or froze no reference for the toolkit, is denied by the broker.
//!
//! The other kinds on this lane stay refused on a governed command: tool
//! discovery and configuration validation read claim-time settings and have
//! no governed operation. `index.ingest.v1` has its own governed path.

use std::sync::Arc;

use crate::protocol::command::ToolkitCommandKind;
use crate::protocol::control::AuthorizedToolkitExecution;
use crate::toolkits::DirectToolkitRuntime;
use crate::transport::credential_redeemer::governed_client;
use crate::transport::platform_client::PlatformClient;

/// Whether a governed command of `kind` runs on this lane through the
/// governed seam. `false` refuses it, fail closed.
#[must_use]
pub(super) const fn runs_governed(kind: ToolkitCommandKind) -> bool {
    matches!(
        kind,
        ToolkitCommandKind::CallTool | ToolkitCommandKind::ExecuteRead
    )
}

/// The direct runtime for one authorized command: with the command's
/// governed seam on a governed command, unchanged on a legacy one.
#[must_use]
pub(super) fn runtime_for(
    base: &DirectToolkitRuntime,
    platform: &Arc<PlatformClient>,
    authorized: &AuthorizedToolkitExecution,
) -> DirectToolkitRuntime {
    base.for_command(governed_client(
        platform,
        &authorized.shared_runtime_context(),
    ))
}

#[cfg(test)]
mod tests {
    use super::{ToolkitCommandKind, runs_governed};

    /// Only the two tool-call kinds redeem on this lane; discovery,
    /// validation and index ingest stay refused here, fail closed.
    #[test]
    fn only_tool_calls_run_governed_on_the_direct_lane() {
        assert!(runs_governed(ToolkitCommandKind::CallTool));
        assert!(runs_governed(ToolkitCommandKind::ExecuteRead));
        assert!(!runs_governed(ToolkitCommandKind::AvailableTools));
        assert!(!runs_governed(ToolkitCommandKind::ConfigurationValidate));
        assert!(!runs_governed(ToolkitCommandKind::IndexIngest));
    }
}
