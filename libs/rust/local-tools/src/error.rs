//! The one error type every local tool returns.
//!
//! A local tool's failure goes back to the model as a tool result (it can
//! read the message and try something else), so messages name the workspace
//! path involved. They never carry file contents or environment values.

use std::fmt;

/// Stable classification of a local tool failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ErrorCode {
    /// A malformed or missing argument.
    InvalidArgument,
    /// The path resolves outside the workspace (`..`, an absolute path, a
    /// symlink).
    OutsideWorkspace,
    /// A rule refused it: policy `path_deny`, a protected path, an approval
    /// rule, plan mode.
    Denied,
    /// The person (or a rule asking them) rejected the call.
    Rejected,
    /// The decision was deferred to a later resume.
    ApprovalPending,
    NotFound,
    /// The file changed since the agent last read it, or it was never read.
    StaleRead,
    /// An edit's anchor did not match exactly once, or a patch hunk did not
    /// apply.
    Conflict,
    TooLarge,
    /// The file is not text.
    Binary,
    Timeout,
    /// The requested sandbox cannot be enforced on this machine.
    SandboxUnavailable,
    Unsupported,
    /// git failed or the folder is not what the operation needs.
    Git,
    /// The folder's git repository could run its own code in the host's
    /// git (filter drivers, gpg.program, core.worktree, includes, a
    /// relocated `.git`, submodules): the host does not run git there.
    UnsafeRepository,
    Io,
}

impl ErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidArgument => "local_tools.invalid_argument",
            Self::OutsideWorkspace => "local_tools.outside_workspace",
            Self::Denied => "local_tools.denied",
            Self::Rejected => "local_tools.rejected",
            Self::ApprovalPending => "local_tools.approval_pending",
            Self::NotFound => "local_tools.not_found",
            Self::StaleRead => "local_tools.stale_read",
            Self::Conflict => "local_tools.conflict",
            Self::TooLarge => "local_tools.too_large",
            Self::Binary => "local_tools.binary",
            Self::Timeout => "local_tools.timeout",
            Self::SandboxUnavailable => "local_tools.sandbox_unavailable",
            Self::Unsupported => "local_tools.unsupported",
            Self::Git => "local_tools.git",
            Self::UnsafeRepository => "local_tools.unsafe_repository",
            Self::Io => "local_tools.io",
        }
    }
}

/// A local tool failure: a code and a message for the model and the person.
#[derive(Clone, Eq, PartialEq)]
pub struct ToolError {
    code: ErrorCode,
    message: String,
}

impl ToolError {
    #[must_use]
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    #[must_use]
    pub const fn code(&self) -> ErrorCode {
        self.code
    }

    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }

    pub(crate) fn invalid(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::InvalidArgument, message)
    }

    pub(crate) fn io(what: &str, error: &std::io::Error) -> Self {
        let code = match error.kind() {
            std::io::ErrorKind::NotFound => ErrorCode::NotFound,
            _ => ErrorCode::Io,
        };
        Self::new(code, format!("{what}: {}", error.kind()))
    }

    /// The tool result the model sees for this failure.
    #[must_use]
    pub fn to_result(&self) -> serde_json::Value {
        serde_json::json!({
            "status": "error",
            "code": self.code.as_str(),
            "message": self.message,
        })
    }
}

impl fmt::Debug for ToolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolError")
            .field("code", &self.code)
            .field("message", &self.message)
            .finish()
    }
}

impl fmt::Display for ToolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl std::error::Error for ToolError {}

impl From<rustix::io::Errno> for ToolError {
    fn from(errno: rustix::io::Errno) -> Self {
        Self::io("file system", &std::io::Error::from(errno))
    }
}

/// Result alias for this crate.
pub type ToolResult<T> = Result<T, ToolError>;
