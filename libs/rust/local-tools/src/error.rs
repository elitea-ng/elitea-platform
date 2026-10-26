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
    /// A spilled output `read_output` was asked for is no longer kept
    /// (past its retention, dropped by the cap, or another
    /// conversation's): re-run the tool that produced it.
    OutputExpired,
    /// A session already runs as many background processes as it may
    /// ([`crate::process::MAX_LIVE`]).
    TooManyProcesses,
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
            Self::OutputExpired => "local_tools.output_expired",
            Self::TooManyProcesses => "local_tools.too_many_processes",
            Self::Io => "local_tools.io",
        }
    }
}

/// A local tool failure: a code and a message for the model and the person.
#[derive(Clone, Eq, PartialEq)]
pub struct ToolError {
    code: ErrorCode,
    message: String,
    /// More fields of the tool result the model sees (a rule's refusal:
    /// `error`, `rule`, `justification`).
    details: Option<serde_json::Map<String, serde_json::Value>>,
    /// The person's own text, when it differs from the model's (full
    /// paths, uids, what to change): shown through the host's UI, never in
    /// a tool result.
    /// Boxed: the error stays small enough to return by value.
    person: Option<Box<str>>,
}

impl ToolError {
    #[must_use]
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            details: None,
            person: None,
        }
    }

    /// This error with a text of its own for the person (see
    /// [`Self::person_message`]); the model keeps [`Self::message`].
    #[must_use]
    pub fn with_person_message(mut self, text: impl Into<String>) -> Self {
        self.person = Some(text.into().into_boxed_str());
        self
    }

    /// What the person is told: their own text when there is one, else
    /// the model's message.
    #[must_use]
    pub fn person_message(&self) -> &str {
        self.person.as_deref().unwrap_or(&self.message)
    }

    /// This error with more fields in its tool result (never `status`,
    /// `code` or `message`).
    #[must_use]
    pub fn with_details(mut self, details: serde_json::Map<String, serde_json::Value>) -> Self {
        self.details = Some(details);
        self
    }

    /// The extra fields of its tool result.
    #[must_use]
    pub fn details(&self) -> Option<&serde_json::Map<String, serde_json::Value>> {
        self.details.as_ref()
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
        let mut result = serde_json::json!({
            "status": "error",
            "code": self.code.as_str(),
            "message": self.message,
        });
        if let (Some(details), serde_json::Value::Object(fields)) = (&self.details, &mut result) {
            for (key, value) in details {
                fields.entry(key.clone()).or_insert_with(|| value.clone());
            }
        }
        result
    }
}

impl fmt::Debug for ToolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolError")
            .field("code", &self.code)
            .field("message", &self.message)
            .field("details", &self.details)
            .field("person", &self.person)
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
