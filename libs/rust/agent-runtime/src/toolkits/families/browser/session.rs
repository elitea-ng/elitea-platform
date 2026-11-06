//! What the host lends the `browser` family: one cloud browser session for
//! the run, reached through the supervisor's `BrowserSessionService` over
//! gRPC and mTLS (ADR-0049 decision 4). The family never speaks CDP and
//! never sees a grant, a token or the proxy: it calls [`BrowserSession`].

use std::fmt;

use async_trait::async_trait;
use serde_json::Value;

/// One call's answer, as the session agent gave it.
#[derive(Clone, Debug, PartialEq)]
pub struct SessionAnswer {
    /// The contract's JSON result, failures included
    /// (`{"status":"error","code":...}`).
    pub result: Value,
    /// `browser_screenshot` only: the image's media type and bytes.
    pub image: Option<SessionImage>,
}

/// A screenshot the session took.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SessionImage {
    pub media_type: String,
    pub data: Vec<u8>,
}

/// Why a session call did not reach the page. The message of each is fixed
/// text the family writes; no transport error text crosses into a result.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SessionError {
    /// The platform's browser capacity is full (`browser_capacity`), or the
    /// run holds its most sessions.
    Capacity,
    /// The session ended (its wall, its idle time, or the run's end).
    Ended,
    /// The supervisor refused the call (a grant, a fence, a ceiling).
    Refused,
    /// The supervisor or the session agent could not be reached.
    Unavailable,
}

impl fmt::Display for SessionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Capacity => "the cloud browser is at its capacity",
            Self::Ended => "the cloud browser session ended",
            Self::Refused => "the cloud browser refused the call",
            Self::Unavailable => "the cloud browser is unavailable",
        })
    }
}

impl std::error::Error for SessionError {}

/// The run's cloud browser session. The host starts it at the first call
/// and keeps it for the run; it ends it when the run ends
/// ([`BrowserSession::end`]), except at a pause for a person, where it is
/// kept so the person can take over (ADR-0049 decision 7).
#[async_trait]
pub trait BrowserSession: Send + Sync {
    /// Run one tool call of the contract. `call_id` is the model's tool call
    /// id.
    async fn call(
        &self,
        call_id: &str,
        name: &str,
        arguments: &Value,
    ) -> Result<SessionAnswer, SessionError>;

    /// End the session. `now`: destroy at once (a failed or cancelled run);
    /// else the run ended and the session lingers for a viewer. Idempotent;
    /// a session that never started does nothing.
    async fn end(&self, now: bool);
}
