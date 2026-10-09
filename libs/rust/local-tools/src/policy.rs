//! The `local_work` group of the native client policy (ADR-0029 decision
//! 6), as far as local tools read it.
//!
//! The desktop receives the policy with every token response and enforces
//! it here; the server cannot verify what a client did. Fields this crate
//! does not use (`local_mcp`, `local_index`, `memory_write`, `cloud_sync`)
//! are ignored when deserialising. Every default fails closed.

use serde::{Deserialize, Serialize};

/// How far a command may reach, ordered from the most confined.
#[derive(
    Clone, Copy, Debug, Default, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize,
)]
#[serde(rename_all = "kebab-case")]
pub enum SandboxMode {
    /// Reads anywhere, writes nowhere (but `/dev/null` and the like).
    #[default]
    ReadOnly,
    /// Reads anywhere, writes only inside the workspace and the session's
    /// temporary directory.
    WorkspaceWrite,
    /// No file system confinement.
    FullAccess,
}

impl SandboxMode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ReadOnly => "read-only",
            Self::WorkspaceWrite => "workspace-write",
            Self::FullAccess => "full-access",
        }
    }
}

/// `native_client_policy.local_work`.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct LocalWorkPolicy {
    /// Local work is off unless the policy enables it.
    pub allowed: bool,
    /// Whether the shell tool exists at all.
    pub shell: bool,
    /// The widest sandbox a command may ask for.
    pub max_sandbox_mode: SandboxMode,
    /// Whether a command may use the network.
    pub network: bool,
    /// When non-empty, the only commands that may run (argv prefixes, see
    /// [`crate::approvals::CommandPattern`]). A ceiling, not an approval:
    /// an admissible command is still asked unless another rule allows it.
    pub command_allow: Vec<String>,
    /// Commands that never run.
    pub command_deny: Vec<String>,
    /// Paths no local tool reads or writes (globs, see
    /// [`crate::workspace::Workspace::open`]).
    pub path_deny: Vec<String>,
}

impl Default for LocalWorkPolicy {
    fn default() -> Self {
        Self {
            allowed: false,
            shell: false,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            network: false,
            command_allow: Vec::new(),
            command_deny: Vec::new(),
            path_deny: Vec::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{LocalWorkPolicy, SandboxMode};

    #[test]
    fn defaults_fail_closed_and_unknown_fields_are_ignored() {
        let policy: LocalWorkPolicy = serde_json::from_value(serde_json::json!({
            "allowed": true,
            "max_sandbox_mode": "full-access",
            "memory_write": true,
            "local_index": false
        }))
        .expect("parse");
        assert!(policy.allowed);
        assert!(!policy.shell);
        assert!(!policy.network);
        assert_eq!(policy.max_sandbox_mode, SandboxMode::FullAccess);
        assert!(!LocalWorkPolicy::default().allowed);
        assert!(SandboxMode::ReadOnly < SandboxMode::WorkspaceWrite);
        assert!(SandboxMode::WorkspaceWrite < SandboxMode::FullAccess);
    }
}
