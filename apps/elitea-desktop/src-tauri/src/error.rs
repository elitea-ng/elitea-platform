//! The one error type the host returns to the webview.
//!
//! Its `Display` text is what the connect screen shows, so each message is
//! written for a person. Nothing here carries a token, a code or a URL
//! query: a failed sign-in must not put a credential into the UI or a log.

use serde::{Serialize, Serializer};

#[derive(Debug, thiserror::Error)]
pub enum HostError {
    #[error("{0}")]
    InvalidAddress(String),
    #[error("{0}")]
    Unsupported(String),
    #[error("the deployment's discovery document points at another address, so it was refused")]
    OriginMismatch,
    #[error("could not reach the deployment; check the address and your connection")]
    Unreachable,
    #[error("the deployment is not available right now; try again shortly")]
    Unavailable,
    #[error("connect to a deployment first")]
    NotConnected,
    #[error("sign-in was cancelled or timed out")]
    SignInAborted,
    #[error("sign-in was denied")]
    SignInDenied,
    #[error("sign-in failed: the response did not match this attempt")]
    SignInRejected,
    #[error(
        "this deployment does not recognise the desktop client; ask an administrator to register client id \"{0}\""
    )]
    ClientNotRegistered(String),
    #[error("this version of Elitea is older than the deployment allows; update the app")]
    UpgradeRequired,
    #[error("the device session was revoked")]
    DeviceRevoked,
    #[error("could not use the stored sign-in: {0}")]
    Credentials(String),
    #[error("could not read or write the app's settings: {0}")]
    Storage(String),
    #[error("could not open the system browser")]
    Browser,
    #[error("an unexpected error occurred: {0}")]
    Internal(String),
}

impl Serialize for HostError {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&self.to_string())
    }
}
