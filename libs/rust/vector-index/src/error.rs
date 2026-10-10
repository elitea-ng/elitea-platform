//! The crate's errors, and the retry classification of `elitea-vector`
//! status codes.

use tonic::Code;

use crate::filter::FilterError;

/// Whether repeating the same call can succeed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ErrorClass {
    /// The service or the network was unavailable or busy; the same call
    /// may succeed later.
    Retryable,
    /// Repeating the same call fails the same way: the request, the token or
    /// the caller's permission is wrong, or the failure is a defect.
    NonRetryable,
}

/// Classifies a gRPC status code returned by `elitea-vector`.
///
/// * `UNAVAILABLE` (Qdrant or elitea-main unreachable, which the facade
///   reports and fails closed on), `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`
///   and `ABORTED` are retryable.
/// * `UNAUTHENTICATED` is not: the token is expired, revoked or unknown, and
///   the same bearer stays refused. A claim that needs a fresh token has to
///   get it from Main.
/// * `PERMISSION_DENIED`, `INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS`,
///   `FAILED_PRECONDITION`, `OUT_OF_RANGE`, `UNIMPLEMENTED`, `CANCELLED`,
///   `DATA_LOSS`, `INTERNAL` and `UNKNOWN` are not.
#[must_use]
pub const fn classify(code: Code) -> ErrorClass {
    match code {
        Code::Unavailable | Code::DeadlineExceeded | Code::ResourceExhausted | Code::Aborted => {
            ErrorClass::Retryable
        }
        _ => ErrorClass::NonRetryable,
    }
}

/// Everything this crate can fail with.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// `elitea-vector` answered with a non-OK status.
    #[error("elitea-vector refused the call ({code:?}): {message}")]
    Rpc {
        /// The gRPC status code.
        code: Code,
        /// The status message. It never holds a token.
        message: String,
    },
    /// The channel to `elitea-vector` could not be built or used.
    #[error("cannot reach elitea-vector: {0}")]
    Transport(String),
    /// The caller's bearer is not usable as gRPC metadata.
    #[error("the vector token cannot be sent as a bearer: {0}")]
    InvalidToken(String),
    /// The SDK filter cannot be translated.
    #[error("{0}")]
    Filter(#[from] FilterError),
    /// A tool argument is out of range or malformed.
    #[error("{0}")]
    InvalidArgument(String),
    /// An index the caller named does not exist.
    #[error("{0}")]
    NotFound(String),
    /// A request the tools refuse on purpose.
    #[error("{0}")]
    Refused(String),
    /// The query could not be embedded.
    #[error("embedding the query failed: {0}")]
    Embedding(String),
    /// The step-back model call failed.
    #[error("the step-back model call failed: {0}")]
    Model(String),
}

impl Error {
    /// The retry class of this error. Only network and service-busy
    /// failures are retryable.
    #[must_use]
    pub const fn class(&self) -> ErrorClass {
        match self {
            Self::Rpc { code, .. } => classify(*code),
            Self::Transport(_) => ErrorClass::Retryable,
            _ => ErrorClass::NonRetryable,
        }
    }

    /// Whether repeating the same call can succeed.
    #[must_use]
    pub const fn is_retryable(&self) -> bool {
        matches!(self.class(), ErrorClass::Retryable)
    }
}

impl From<tonic::Status> for Error {
    fn from(status: tonic::Status) -> Self {
        Self::Rpc {
            code: status.code(),
            message: status.message().to_owned(),
        }
    }
}

/// The crate's result type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_busy_and_unreachable_codes_retry() {
        for code in [
            Code::Unavailable,
            Code::DeadlineExceeded,
            Code::ResourceExhausted,
            Code::Aborted,
        ] {
            assert_eq!(classify(code), ErrorClass::Retryable, "{code:?}");
        }
        for code in [
            Code::Unauthenticated,
            Code::PermissionDenied,
            Code::InvalidArgument,
            Code::NotFound,
            Code::AlreadyExists,
            Code::FailedPrecondition,
            Code::OutOfRange,
            Code::Unimplemented,
            Code::Cancelled,
            Code::DataLoss,
            Code::Internal,
            Code::Unknown,
        ] {
            assert_eq!(classify(code), ErrorClass::NonRetryable, "{code:?}");
        }
    }

    #[test]
    fn errors_carry_their_class() {
        assert!(Error::from(tonic::Status::unavailable("qdrant down")).is_retryable());
        assert!(Error::Transport("refused".into()).is_retryable());
        assert!(!Error::from(tonic::Status::unauthenticated("expired")).is_retryable());
        assert!(!Error::InvalidArgument("x".into()).is_retryable());
    }
}
