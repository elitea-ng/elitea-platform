//! Delegation assertions of remote calls (ADR-0044 decision 4, #1501).
//!
//! Before the agent calls a remote MCP server, the host asks Main for a
//! short-lived assertion signed with the platform's agent assertion key: its
//! audience is the endpoint, and it names the run, the agent, the delegating
//! user (or mandate) and a digest of the run's scope. The call carries it in
//! the [`HEADER_NAME`] header, beside the server's own credential. The
//! receiver verifies it against the issuer's JWKS
//! (`/.well-known/elitea-agent-jwks.json`).
//!
//! One [`DelegationAssertions`] serves one run. The worker sends the
//! endpoint URL as configured; Main alone normalizes it (with the function
//! it applies to the run's admitted toolkit URLs) and signs the normalized
//! form, so the worker never compares audiences. The cache is keyed by the
//! raw endpoint:
//!
//! - an assertion is kept until shortly before it expires;
//! - "disabled" (no assertion goes to this endpoint) is kept for the run;
//! - a failed mint is not asked again for [`FAILURE_BACKOFF`].
//!
//! A run that requires assertions (the governed profile) refuses a call it
//! cannot sign; any other run logs the failure and calls without one.

use std::collections::HashMap;
use std::fmt;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;

/// The header a remote call carries the assertion in.
pub const HEADER_NAME: &str = "Elitea-Agent-Assertion";

/// The reason code of a refused remote call (shown to the model and logged).
pub const ASSERTION_UNAVAILABLE: &str = "agent_assertion_unavailable";

/// An assertion is used until this long before it expires; then a new one
/// is minted.
pub const REFRESH_MARGIN: Duration = Duration::from_mins(1);

/// After a failed mint, the same endpoint is not asked again for this long:
/// the failure is answered from the cache.
pub const FAILURE_BACKOFF: Duration = Duration::from_secs(30);

/// The largest assertion accepted from Main (Main's own limit).
const MAX_ASSERTION_BYTES: usize = 8 * 1_024;

/// The longest endpoint URL that can be an audience (Main's own limit).
const MAX_AUDIENCE_BYTES: usize = 2_048;

/// What Main answered to one mint.
#[derive(Clone, Eq, PartialEq)]
pub enum MintedAssertion {
    /// A signed assertion and when it expires (Unix milliseconds).
    Issued {
        token: String,
        expires_at_unix_millis: i64,
    },
    /// No assertion goes to this endpoint: outbound assertions are off, the
    /// endpoint is not on the allowlist, or the deployment has no agent
    /// assertion key (never for a governed run).
    Disabled,
}

impl fmt::Debug for MintedAssertion {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Issued {
                expires_at_unix_millis,
                ..
            } => formatter
                .debug_struct("Issued")
                .field("expires_at_unix_millis", expires_at_unix_millis)
                .finish_non_exhaustive(),
            Self::Disabled => formatter.write_str("Disabled"),
        }
    }
}

/// A stable, data-free failure to get an assertion.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum DelegationAssertionError {
    /// The endpoint URL cannot be an audience (empty or longer than Main
    /// accepts; Main refuses the rest).
    #[error("the endpoint cannot be the audience of an agent assertion")]
    UnusableAudience,
    /// Main refused, did not answer, or answered something malformed.
    #[error("the agent assertion could not be minted")]
    Unavailable,
    /// The deployment turned outbound assertions off, but this run
    /// requires them.
    #[error("outbound agent assertions are off, but this run requires them")]
    Disabled,
}

/// Mints one assertion for an audience (the cloud worker's claim lease asks
/// Main over the runtime control service).
#[async_trait]
pub trait DelegationAssertionSource: Send + Sync {
    /// Mint an assertion for `endpoint`, the URL as configured (Main
    /// normalizes it).
    ///
    /// # Errors
    ///
    /// [`DelegationAssertionError::Unavailable`] when Main refuses or does
    /// not answer.
    async fn mint(&self, endpoint: &str) -> Result<MintedAssertion, DelegationAssertionError>;
}

enum Cached {
    Issued {
        token: String,
        expires_at_unix_millis: i64,
    },
    /// No assertion goes to this endpoint for the rest of the run.
    Disabled,
    /// The last mint failed; answered from the cache until then.
    Failed {
        error: DelegationAssertionError,
        until_unix_millis: i64,
    },
}

/// A cache answer.
enum Hit {
    Token(String),
    Disabled,
    Failed(DelegationAssertionError),
}

/// The delegation assertions of one run.
pub struct DelegationAssertions {
    source: Arc<dyn DelegationAssertionSource>,
    required: bool,
    cache: Mutex<HashMap<String, Cached>>,
    now: fn() -> i64,
}

impl fmt::Debug for DelegationAssertions {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DelegationAssertions")
            .field("required", &self.required)
            .finish_non_exhaustive()
    }
}

fn system_now_unix_millis() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|elapsed| i64::try_from(elapsed.as_millis()).ok())
        .unwrap_or(0)
}

fn millis(duration: Duration) -> i64 {
    i64::try_from(duration.as_millis()).unwrap_or(i64::MAX)
}

impl DelegationAssertions {
    /// The assertions of one run. `required` is the governed profile: a
    /// call that cannot be signed is refused.
    #[must_use]
    pub fn new(source: Arc<dyn DelegationAssertionSource>, required: bool) -> Arc<Self> {
        Self::with_clock(source, required, system_now_unix_millis)
    }

    /// [`Self::new`] with an injected clock (Unix milliseconds).
    #[must_use]
    pub fn with_clock(
        source: Arc<dyn DelegationAssertionSource>,
        required: bool,
        now: fn() -> i64,
    ) -> Arc<Self> {
        Arc::new(Self {
            source,
            required,
            cache: Mutex::new(HashMap::new()),
            now,
        })
    }

    /// Whether a call that cannot be signed is refused.
    #[must_use]
    pub const fn required(&self) -> bool {
        self.required
    }

    /// The assertion for a call to `endpoint`, or `None` when the call goes
    /// without one (assertions off, or a failure in a run that does not
    /// require them; the failure is logged).
    ///
    /// # Errors
    ///
    /// A [`DelegationAssertionError`] when this run requires an assertion
    /// and none can be had.
    pub async fn assertion_for(
        &self,
        endpoint: &str,
    ) -> Result<Option<String>, DelegationAssertionError> {
        if endpoint.is_empty() || endpoint.len() > MAX_AUDIENCE_BYTES {
            return self.without(DelegationAssertionError::UnusableAudience, true);
        }
        let now = (self.now)();
        match self.cached(endpoint, now) {
            Some(Hit::Token(token)) => return Ok(Some(token)),
            Some(Hit::Disabled) => return Ok(None),
            Some(Hit::Failed(error)) => return self.without(error, false),
            None => {}
        }
        let audience = endpoint.to_owned();
        match self.source.mint(endpoint).await {
            Ok(MintedAssertion::Issued {
                token,
                expires_at_unix_millis,
            }) if valid_token(&token) && expires_at_unix_millis > now => {
                self.store(
                    audience,
                    Cached::Issued {
                        token: token.clone(),
                        expires_at_unix_millis,
                    },
                );
                Ok(Some(token))
            }
            Ok(MintedAssertion::Issued { .. }) => {
                self.failed(audience, DelegationAssertionError::Unavailable, now)
            }
            Ok(MintedAssertion::Disabled) if self.required => {
                self.failed(audience, DelegationAssertionError::Disabled, now)
            }
            Ok(MintedAssertion::Disabled) => {
                self.store(audience, Cached::Disabled);
                Ok(None)
            }
            Err(error) => self.failed(audience, error, now),
        }
    }

    /// Remember a failed mint for [`FAILURE_BACKOFF`] and answer it.
    fn failed(
        &self,
        endpoint: String,
        error: DelegationAssertionError,
        now: i64,
    ) -> Result<Option<String>, DelegationAssertionError> {
        self.store(
            endpoint,
            Cached::Failed {
                error,
                until_unix_millis: now.saturating_add(millis(FAILURE_BACKOFF)),
            },
        );
        self.without(error, true)
    }

    /// The answer without an assertion: the error for a run that requires
    /// one, else `None`. `log` is false for a failure answered from the
    /// cache (it was logged when it happened).
    fn without(
        &self,
        error: DelegationAssertionError,
        log: bool,
    ) -> Result<Option<String>, DelegationAssertionError> {
        if self.required {
            return Err(error);
        }
        if log {
            tracing::warn!(
                event = "agent_assertion_skipped",
                reason_code = ASSERTION_UNAVAILABLE,
                error = %error,
                "a remote MCP call goes without an agent assertion"
            );
        }
        Ok(None)
    }

    /// What the cache answers for `audience` at `now`; `None` to mint.
    fn cached(&self, audience: &str, now: i64) -> Option<Hit> {
        let cache = self
            .cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        match cache.get(audience)? {
            Cached::Issued {
                token,
                expires_at_unix_millis,
            } if expires_at_unix_millis.saturating_sub(now) > millis(REFRESH_MARGIN) => {
                Some(Hit::Token(token.clone()))
            }
            Cached::Disabled => Some(Hit::Disabled),
            Cached::Failed {
                error,
                until_unix_millis,
            } if *until_unix_millis > now => Some(Hit::Failed(*error)),
            _ => None,
        }
    }

    fn store(&self, audience: String, cached: Cached) {
        self.cache
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .insert(audience, cached);
    }
}

/// A compact JWS: three base64url segments, bounded.
fn valid_token(token: &str) -> bool {
    !token.is_empty()
        && token.len() <= MAX_ASSERTION_BYTES
        && token.split('.').count() == 3
        && token
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

#[cfg(test)]
#[path = "remote_assertion_tests.rs"]
mod tests;
