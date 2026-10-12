//! The `local_work` group of the native client policy (ADR-0029 decision
//! 6), as far as local tools read it.
//!
//! The desktop receives the policy with every token response and enforces
//! it here; the server cannot verify what a client did. Fields this crate
//! does not use (`local_mcp`, `memory_write`, `cloud_sync`) are ignored when
//! deserialising. Every default fails closed (`local_index` too: the server
//! sends `true` by default, a policy without the field turns it off).

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
// The server's wire format: one switch per capability.
#[allow(clippy::struct_excessive_bools)]
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
    /// [`crate::command::CommandPattern`]). A ceiling, not an approval:
    /// an admissible command still goes through the rules below (a
    /// destructive one is asked, see [`crate::classify`]).
    pub command_allow: Vec<String>,
    /// Commands refused when the model names them (in any segment of a
    /// compound command, and inside `sh -c`, `env`, `sudo` and other
    /// wrappers). **Advisory, not a security boundary:** a program started
    /// by a script, a build tool or an interpreter is not seen; the sandbox
    /// and the approval are what confine a command (see
    /// [`crate::command`]).
    pub command_deny: Vec<String>,
    /// Paths no local tool reads or writes (globs, see
    /// [`crate::workspace::Workspace::open`]).
    pub path_deny: Vec<String>,
    /// Whether the desktop may build and use a local index of a workspace
    /// (ADR-0029 decision 7). Only with `allowed`: the host gates on both.
    pub local_index: bool,
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
            local_index: false,
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
        assert!(!policy.local_index, "sent as false");
        assert!(!LocalWorkPolicy::default().allowed);
        assert!(
            !LocalWorkPolicy::default().local_index,
            "absent is off, like every other field"
        );
        let on: LocalWorkPolicy =
            serde_json::from_value(serde_json::json!({"allowed": true, "local_index": true}))
                .expect("parse");
        assert!(on.local_index);
        assert!(SandboxMode::ReadOnly < SandboxMode::WorkspaceWrite);
        assert!(SandboxMode::WorkspaceWrite < SandboxMode::FullAccess);
    }
}
