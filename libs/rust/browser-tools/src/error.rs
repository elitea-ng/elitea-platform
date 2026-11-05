//! The typed errors of the browser tools.
//!
//! A browser tool's failure goes back to the model as a tool result, so
//! the agent can react (snapshot again, ask the person, give up). The codes
//! are the ADR's own words and are the same on both hosts; a driver never
//! invents one. Messages never carry page text, typed text or cookies.

use std::fmt;

use serde_json::{Value, json};

/// Stable classification of a browser tool failure.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum ErrorCode {
    /// A malformed, missing or unknown argument, or a ref that names a
    /// control the call cannot act on (typing into a button).
    InvalidArgument,
    /// No browser tool has this name.
    UnknownTool,
    /// The ref is not in the current snapshot: the page changed since, the
    /// ref names another snapshot, or no such ref was given out. The agent
    /// snapshots again.
    StaleRef,
    /// Typing or pressing keys into a password, new-password or payment
    /// card field (ADR-0048 decision 5). Never lifted.
    SecureFieldRefused,
    /// A navigation to a refused scheme: `file:`, `chrome:`, `about:` other
    /// than `about:blank`, `javascript:`, `data:`, or anything that is not
    /// `http` or `https`.
    SchemeRefused,
    /// The person used the page while the call ran; the agent is paused.
    UserActive,
    /// The person drives the page; the agent's call was not run.
    UserInControl,
    /// The host is outside the run's egress allowlist or the pane's domain
    /// policy (ADR-0049 decision 5, ADR-0048 decision 8).
    EgressRefused,
    /// `browser_request_user` in a run with no person (ADR-0049 decision 7).
    NoPerson,
    /// The person declined, or did not answer before the question's timeout.
    Declined,
    /// No browser session could be started: the platform's cap is reached
    /// (ADR-0049 decision 10).
    BrowserCapacity,
    /// More than the pane's `BrowserAct` rate ([`crate::rate`]).
    RateLimited,
    /// The page did not load (DNS, TLS, connection, an HTTP-level failure
    /// before a document).
    NavigationFailed,
    /// `browser_wait` (or a navigation) did not finish before its timeout.
    Timeout,
    /// The driver cannot do this on this platform.
    Unsupported,
    /// The driver failed; the message is for the operator's logs as much as
    /// for the model.
    Internal,
}

impl ErrorCode {
    /// Every code, in declaration order.
    pub const ALL: [Self; 16] = [
        Self::InvalidArgument,
        Self::UnknownTool,
        Self::StaleRef,
        Self::SecureFieldRefused,
        Self::SchemeRefused,
        Self::UserActive,
        Self::UserInControl,
        Self::EgressRefused,
        Self::NoPerson,
        Self::Declined,
        Self::BrowserCapacity,
        Self::RateLimited,
        Self::NavigationFailed,
        Self::Timeout,
        Self::Unsupported,
        Self::Internal,
    ];

    /// The code as the model sees it (`stale_ref`).
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidArgument => "invalid_argument",
            Self::UnknownTool => "unknown_tool",
            Self::StaleRef => "stale_ref",
            Self::SecureFieldRefused => "secure_field_refused",
            Self::SchemeRefused => "scheme_refused",
            Self::UserActive => "user_active",
            Self::UserInControl => "user_in_control",
            Self::EgressRefused => "egress_refused",
            Self::NoPerson => "no_person",
            Self::Declined => "declined",
            Self::BrowserCapacity => "browser_capacity",
            Self::RateLimited => "rate_limited",
            Self::NavigationFailed => "navigation_failed",
            Self::Timeout => "timeout",
            Self::Unsupported => "unsupported",
            Self::Internal => "internal",
        }
    }

    /// The code named by `text`, if any.
    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|code| code.as_str() == text)
    }
}

impl fmt::Display for ErrorCode {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// A browser tool failure: a code and a message for the model.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BrowserError {
    code: ErrorCode,
    message: String,
}

impl BrowserError {
    #[must_use]
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    #[must_use]
    pub fn invalid(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::InvalidArgument, message)
    }

    #[must_use]
    pub fn stale_ref(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::StaleRef, message)
    }

    /// The refusal for a secure field, worded the same on both hosts.
    #[must_use]
    pub fn secure_field() -> Self {
        Self::new(
            ErrorCode::SecureFieldRefused,
            "this is a password or payment card field; the agent never types into it. Ask the person with browser_request_user.",
        )
    }

    #[must_use]
    pub const fn code(&self) -> ErrorCode {
        self.code
    }

    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }

    /// The tool result the model sees for this failure.
    #[must_use]
    pub fn to_result(&self) -> Value {
        json!({
            "status": "error",
            "code": self.code.as_str(),
            "message": self.message,
        })
    }
}

impl fmt::Display for BrowserError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for BrowserError {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_round_trip_and_are_unique() {
        let mut seen = std::collections::HashSet::new();
        for code in ErrorCode::ALL {
            assert!(seen.insert(code.as_str()), "{code} twice");
            assert_eq!(ErrorCode::parse(code.as_str()), Some(code));
        }
        assert_eq!(ErrorCode::parse("nope"), None);
    }

    #[test]
    fn result_shape() {
        let result = BrowserError::stale_ref("snapshot again").to_result();
        assert_eq!(
            result,
            json!({"status": "error", "code": "stale_ref", "message": "snapshot again"})
        );
    }
}
