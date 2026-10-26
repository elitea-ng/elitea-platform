//! The one error type of the store client.

use serde_json::Value;

/// A refused or failed store operation. Messages never carry file
/// contents, tokens or presigned URLs.
#[derive(Clone, Debug, thiserror::Error, PartialEq)]
pub enum StoreError {
    /// The server answered with an error (`code` is its machine code, such
    /// as `ref_conflict` or `stale_epoch`). `details` is the error's detail
    /// object (`{"current": {...}}` for a ref conflict), `Null` without one.
    #[error("{message} ({status} {code})")]
    Http {
        status: u16,
        code: String,
        message: String,
        details: Value,
    },
    /// No answer arrived (connection, timeout, a cut-off body).
    #[error("network: {0}")]
    Network(String),
    /// The bytes are not what they claim: a digest or size mismatch, a
    /// broken pack, a missing object, a bundle whose refs differ from the
    /// plan's. Nothing was applied.
    #[error("integrity: {0}")]
    Integrity(String),
    /// A repository or bundle in another object format than sha1.
    #[error("unsupported object format: {0}")]
    UnsupportedObjectFormat(String),
    /// Not something this client handles (an incremental segment read on
    /// its own, a filtered bundle, an unknown segment kind).
    #[error("unsupported: {0}")]
    Unsupported(String),
    /// A ref the writer may not write.
    #[error("ref not allowed: {0}")]
    RefNotAllowed(String),
    /// Malformed input or a local I/O failure.
    #[error("{0}")]
    Invalid(String),
    #[error("io: {0}")]
    Io(String),
}

impl StoreError {
    /// Worth another attempt: no answer, a 5xx, or 429.
    #[must_use]
    pub fn is_retryable(&self) -> bool {
        match self {
            Self::Network(_) => true,
            Self::Http { status, .. } => *status >= 500 || *status == 429,
            _ => false,
        }
    }

    /// The server's (or this crate's) machine code.
    #[must_use]
    pub fn code(&self) -> &str {
        match self {
            Self::Http { code, .. } => code,
            Self::Network(_) => "network",
            Self::Integrity(_) => "integrity_failed",
            Self::UnsupportedObjectFormat(_) => "unsupported_object_format",
            Self::Unsupported(_) => "unsupported",
            Self::RefNotAllowed(_) => "ref_not_allowed",
            Self::Invalid(_) => "invalid",
            Self::Io(_) => "io",
        }
    }

    #[must_use]
    pub fn http(status: u16, code: &str, message: impl Into<String>) -> Self {
        Self::Http {
            status,
            code: code.to_owned(),
            message: message.into(),
            details: Value::Null,
        }
    }

    pub(crate) fn io(what: &str, error: &std::io::Error) -> Self {
        Self::Io(format!("{what}: {}", error.kind()))
    }
}
