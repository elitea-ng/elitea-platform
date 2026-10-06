//! Shared error vocabulary required by the saved-family authority port.
//! This module contains no HTTP runtime or node.
use std::fmt;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[allow(
    dead_code,
    reason = "Retain required protocol foundations without enabling deferred execution paths."
)]
pub(crate) enum HttpActionError {
    InvalidInput,
    ResourceExhausted,
    Authentication,
    Authorization,
    RedirectRefused,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    UnexpectedStatus(u16),
    PolicyDenied,
    ReconciliationRequired,
}

impl fmt::Display for HttpActionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidInput => "the HTTP action input is invalid",
            Self::ResourceExhausted => "the HTTP action exceeds its approved bounds",
            Self::Authentication => "HTTP endpoint authentication failed",
            Self::Authorization => "the HTTP endpoint refused authorization",
            Self::RedirectRefused => "the HTTP action refuses redirects",
            Self::RateLimited => "the HTTP endpoint rate limited the request",
            Self::Timeout => "the HTTP action timed out",
            Self::DependencyUnavailable => "the HTTP endpoint is unavailable",
            Self::InvalidResponse => "the HTTP endpoint returned an invalid response",
            Self::UnexpectedStatus(_) => "the HTTP status does not meet the response contract",
            Self::PolicyDenied => "the HTTP action is refused by platform policy",
            Self::ReconciliationRequired => {
                "HTTP completion is uncertain; reconcile the existing effect before continuing"
            }
        })
    }
}

impl std::error::Error for HttpActionError {}
