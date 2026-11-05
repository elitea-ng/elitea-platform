//! The one error type of the trait.

/// Why a driver call failed. The variants are stable codes the computer
/// toolset maps to tool results (ADR-0047 decisions 5 and 6).
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum DesktopError {
    /// The ref names a snapshot this driver no longer holds, or an element
    /// that is gone or changed since that snapshot. Take a new snapshot.
    #[error("stale ref {element_ref}: {reason}")]
    StaleRef { element_ref: String, reason: String },
    /// The target is a secure text field; its value is never read or
    /// written (`secure_field_refused`).
    #[error("secure field refused")]
    SecureFieldRefused,
    /// The key chord is one the driver never sends (quit, force quit, log
    /// out, lock screen, ...).
    #[error("key chord refused: {0}")]
    ChordRefused(String),
    #[error("application not found: {0}")]
    AppNotFound(String),
    #[error("window not found: {0}")]
    WindowNotFound(String),
    /// The OS privacy grant the call needs is missing.
    #[error("permission denied: {0}")]
    PermissionDenied(String),
    /// The application or element does not support the action.
    #[error("action not supported: {0}")]
    ActionNotSupported(String),
    /// The action was sent and did not take effect, or could not be sent.
    #[error("action failed: {0}")]
    ActionFailed(String),
    #[error("timed out: {0}")]
    Timeout(String),
    #[error("invalid arguments: {0}")]
    InvalidArgs(String),
    /// This OS has no driver (the [`Unsupported`](crate::Unsupported) driver).
    #[error("desktop control is not supported on this platform")]
    PlatformNotSupported,
    #[error("internal error: {0}")]
    Internal(String),
}

impl DesktopError {
    /// The stable code of the error, for tool results and logs.
    #[must_use]
    pub fn code(&self) -> &'static str {
        match self {
            Self::StaleRef { .. } => "stale_ref",
            Self::SecureFieldRefused => "secure_field_refused",
            Self::ChordRefused(_) => "chord_refused",
            Self::AppNotFound(_) => "app_not_found",
            Self::WindowNotFound(_) => "window_not_found",
            Self::PermissionDenied(_) => "permission_denied",
            Self::ActionNotSupported(_) => "action_not_supported",
            Self::ActionFailed(_) => "action_failed",
            Self::Timeout(_) => "timeout",
            Self::InvalidArgs(_) => "invalid_args",
            Self::PlatformNotSupported => "platform_not_supported",
            Self::Internal(_) => "internal",
        }
    }

    pub(crate) fn stale(element_ref: &crate::ElementRef, reason: impl Into<String>) -> Self {
        Self::StaleRef {
            element_ref: element_ref.to_string(),
            reason: reason.into(),
        }
    }
}
