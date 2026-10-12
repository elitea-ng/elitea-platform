//! Who is calling, and for which project. Deny by default.
//!
//! A request is admitted in one of two ways:
//!
//! * **Token.** `authorization: Bearer <token>` metadata. elitea-main
//!   verifies the token ([`Introspector`]); the answer is cached until the
//!   token expires, and never longer than the configured maximum (shorter
//!   still for a worker claim token, whose revocation its expiry does not
//!   show). The token's project is the request's project, and the token's
//!   sources are the only sources the request may name
//!   ([`Caller::require_source`]).
//!
//!   **Bounded revocation lag.** elitea-main reads revocation on every
//!   introspection call, but this service answers repeat calls from the
//!   cache, so a revoked token keeps working here for at most the cache
//!   cap: [`WORKER_CLAIM_CAP_SECONDS`] (5 s) for a worker claim token,
//!   [`DEFAULT_MAX_TTL_SECONDS`] (60 s) for an engine callback token by
//!   default, and never more than [`HARD_MAX_TTL_SECONDS`] (300 s) whatever
//!   is configured. A refusal is cached for at most
//!   [`NEGATIVE_TTL_SECONDS`] (5 s).
//! * **Administrator.** No token, and a client certificate whose identity
//!   ([`crate::identity`]) is in the administrator list (elitea-main). This
//!   caller names the project in the request.
//!
//! Anything else is `UNAUTHENTICATED`. When elitea-main cannot answer, the
//! request is refused with `UNAVAILABLE`: the service fails closed.

use std::collections::HashSet;
use std::fmt;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use arc_swap::ArcSwap;
use async_trait::async_trait;
use moka::future::Cache;
use sha2::{Digest as _, Sha256};
use tonic::transport::Channel;
use tonic::{Request, Status};

use crate::identity::certificate_identity;
use crate::pb;
use crate::pb::token_introspection_service_client::TokenIntrospectionServiceClient;

/// The longest bearer token accepted.
const MAX_TOKEN_BYTES: usize = 4096;

/// How long a worker claim token's answer is cached at most: the bound on
/// how long a revoked claim token still works here.
pub const WORKER_CLAIM_CAP_SECONDS: i64 = 5;
/// The default longest an engine callback token's answer is cached.
pub const DEFAULT_MAX_TTL_SECONDS: i64 = 60;
/// The most the configured cache cap may be; a larger value is clamped.
pub const HARD_MAX_TTL_SECONDS: i64 = 300;
/// How long a refusal is cached at most.
pub const NEGATIVE_TTL_SECONDS: i64 = 5;
/// `/readyz` fails while an introspection attempt within this many seconds
/// was refused by elitea-main as not authorized.
pub const NOT_AUTHORIZED_WINDOW_SECONDS: i64 = 60;
/// The token the startup self-check presents: no token is ever minted with
/// it, so an authorized caller is answered "inactive".
const SELF_CHECK_TOKEN: &str = "elitea-vector-introspection-self-check";

/// A verified token.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TokenGrant {
    /// The project the token is bound to.
    pub project_id: i64,
    /// The token owner, as elitea-main names it.
    pub principal: String,
    /// What minted the token.
    pub kind: pb::TokenKind,
    /// Expiry, in seconds since the Unix epoch.
    pub expires_at_unix: i64,
    /// The sources the token may read and write; never empty. A worker claim
    /// token names `ToolkitIndex`; a callback token names the source of the
    /// provider that minted it.
    pub sources: Vec<pb::Source>,
}

impl TokenGrant {
    /// Whether the token may use `source`.
    #[must_use]
    pub fn allows(&self, source: pb::Source) -> bool {
        source != pb::Source::Unspecified && self.sources.contains(&source)
    }
}

/// An admitted caller.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Caller {
    /// A project-bound token.
    Token(TokenGrant),
    /// An administrator mTLS identity.
    Admin(String),
}

impl Caller {
    /// The project a request works in.
    ///
    /// A token caller works in its token's project: a request may leave
    /// `requested` at 0 or repeat that project, and any other value is
    /// `PERMISSION_DENIED`. An administrator must name a project.
    ///
    /// # Errors
    /// As above.
    pub fn project(&self, requested: i64) -> Result<i64, Status> {
        match self {
            Self::Token(grant) => {
                if requested == 0 || requested == grant.project_id {
                    Ok(grant.project_id)
                } else {
                    Err(Status::permission_denied(
                        "the request names a project its token is not bound to",
                    ))
                }
            }
            Self::Admin(_) => {
                if requested > 0 {
                    Ok(requested)
                } else {
                    Err(Status::invalid_argument(
                        "an administrator request must name project_id",
                    ))
                }
            }
        }
    }

    /// Refuses an administrator caller: the operation needs a token.
    ///
    /// # Errors
    /// `PERMISSION_DENIED` for an administrator.
    pub fn require_token(&self) -> Result<&TokenGrant, Status> {
        match self {
            Self::Token(grant) => Ok(grant),
            Self::Admin(_) => Err(Status::permission_denied(
                "this operation requires a project token",
            )),
        }
    }

    /// Refuses a token caller whose token may not use `source` (the raw wire
    /// value of a namespace or scope). An administrator may use every
    /// source. An unset or unknown source is left to the request's own
    /// validation, which refuses it.
    ///
    /// # Errors
    /// `PERMISSION_DENIED` for a token not admitted for `source`.
    pub fn require_source(&self, source: i32) -> Result<(), Status> {
        let Self::Token(grant) = self else {
            return Ok(());
        };
        match pb::Source::try_from(source) {
            Ok(pb::Source::Unspecified) | Err(_) => Ok(()),
            Ok(source) if grant.allows(source) => Ok(()),
            Ok(_) => Err(Status::permission_denied(
                "the token may not use this source",
            )),
        }
    }

    /// The sources of `requested` this caller may use: all of them for an
    /// administrator, the token's own for a token.
    #[must_use]
    pub fn permitted_sources(&self, requested: &[pb::Source]) -> Vec<pb::Source> {
        requested
            .iter()
            .copied()
            .filter(|source| match self {
                Self::Admin(_) => true,
                Self::Token(grant) => grant.allows(*source),
            })
            .collect()
    }

    /// Refuses a token caller: the operation is for administrators.
    ///
    /// # Errors
    /// `PERMISSION_DENIED` for a token.
    pub fn require_admin(&self) -> Result<&str, Status> {
        match self {
            Self::Admin(identity) => Ok(identity),
            Self::Token(_) => Err(Status::permission_denied(
                "this operation is for administrators",
            )),
        }
    }
}

/// Why a token could not be verified.
#[derive(Debug, thiserror::Error)]
pub enum IntrospectError {
    /// elitea-main did not answer in time, or answered with an error.
    #[error("token introspection is unavailable: {0}")]
    Unavailable(String),
    /// elitea-main refused this service as a caller (`PERMISSION_DENIED` or
    /// `UNAUTHENTICATED`): a misconfiguration, not an outage, and not
    /// something a retry fixes.
    #[error("elitea-main does not authorize this service to introspect tokens")]
    NotAuthorized,
}

/// The message of the `FAILED_PRECONDITION` a caller gets when
/// [`IntrospectError::NotAuthorized`].
pub const NOT_AUTHORIZED_MESSAGE: &str = "vector service is not authorized to introspect tokens; \
check ELITEA_VECTOR_INTROSPECTION_CLIENTS and the client certificate";

/// What the last attempts to reach elitea-main showed; read by `/readyz`.
#[derive(Debug, Default)]
pub struct IntrospectionHealth {
    /// `(unix seconds, refused as not authorized)` of the last attempt that
    /// told us anything: an answer (authorized) or a refusal. An outage
    /// (`Unavailable`) tells nothing and leaves it as it was.
    last: std::sync::Mutex<Option<(i64, bool)>>,
}

impl IntrospectionHealth {
    /// Records an attempt that elitea-main answered (`not_authorized` false)
    /// or refused as not authorized (true) at `now`.
    pub fn record(&self, now: i64, not_authorized: bool) {
        if let Ok(mut last) = self.last.lock() {
            *last = Some((now, not_authorized));
        }
    }

    /// Whether the last informative attempt, made within
    /// [`NOT_AUTHORIZED_WINDOW_SECONDS`] of `now`, was refused as not
    /// authorized.
    #[must_use]
    pub fn not_authorized(&self, now: i64) -> bool {
        self.last.lock().is_ok_and(|last| {
            matches!(*last, Some((at, true)) if now.saturating_sub(at) < NOT_AUTHORIZED_WINDOW_SECONDS)
        })
    }
}

/// The [`IntrospectError`] of a failed call to elitea-main.
#[must_use]
pub fn error_from_status(status: &Status) -> IntrospectError {
    match status.code() {
        tonic::Code::PermissionDenied | tonic::Code::Unauthenticated => {
            IntrospectError::NotAuthorized
        }
        code => IntrospectError::Unavailable(code.to_string()),
    }
}

/// Verifies a token with elitea-main.
#[async_trait]
pub trait Introspector: Send + Sync {
    /// `Ok(None)` for a token that is not usable.
    ///
    /// # Errors
    /// [`IntrospectError::Unavailable`] when no answer could be had.
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError>;
}

/// The seconds since the Unix epoch.
#[must_use]
pub fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |elapsed| {
            i64::try_from(elapsed.as_secs()).unwrap_or(i64::MAX)
        })
}

/// elitea-main's `TokenIntrospectionService` over mTLS.
pub struct GrpcIntrospector {
    /// Swapped when the client certificate rotates ([`crate::tls`]).
    client: ArcSwap<TokenIntrospectionServiceClient<Channel>>,
    timeout: Duration,
    health: Arc<IntrospectionHealth>,
}

impl GrpcIntrospector {
    /// A client over `channel`; each call waits at most `timeout`.
    #[must_use]
    pub fn new(channel: Channel, timeout: Duration) -> Self {
        Self {
            client: ArcSwap::from_pointee(TokenIntrospectionServiceClient::new(channel)),
            timeout,
            health: Arc::new(IntrospectionHealth::default()),
        }
    }

    /// What the attempts so far showed, for `/readyz`.
    #[must_use]
    pub fn health(&self) -> Arc<IntrospectionHealth> {
        Arc::clone(&self.health)
    }

    /// Asks elitea-main about a token nobody holds. An authorized caller is
    /// answered "inactive"; one that is not authorized is refused with
    /// `PERMISSION_DENIED`. The outcome lands in [`Self::health`]; an outage
    /// is only logged (elitea-main may still be starting).
    pub async fn self_check(&self) {
        match self.introspect(SELF_CHECK_TOKEN).await {
            Ok(_) => tracing::debug!("introspection self-check passed"),
            Err(IntrospectError::NotAuthorized) => tracing::error!(
                "introspection self-check: elitea-main refused this service; \
                 check ELITEA_VECTOR_INTROSPECTION_CLIENTS and the client certificate"
            ),
            Err(error) => {
                tracing::warn!(%error, "introspection self-check could not reach elitea-main");
            }
        }
    }

    /// Runs [`Self::self_check`] now and every `interval` until the task is
    /// dropped, so `/readyz` follows a fixed (or newly broken) grant.
    pub fn spawn_self_check(self: &Arc<Self>, interval: Duration) -> tokio::task::JoinHandle<()> {
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                ticker.tick().await;
                this.self_check().await;
            }
        })
    }

    /// Uses `channel` for every call from now on; calls already in flight
    /// finish on the old one.
    pub fn replace_channel(&self, channel: Channel) {
        self.client
            .store(Arc::new(TokenIntrospectionServiceClient::new(channel)));
    }
}

#[async_trait]
impl Introspector for GrpcIntrospector {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        let mut client = TokenIntrospectionServiceClient::clone(&self.client.load());
        let call = client.introspect_token(pb::IntrospectTokenRequest {
            token: token.to_owned(),
        });
        let outcome = tokio::time::timeout(self.timeout, call)
            .await
            .map_err(|_| IntrospectError::Unavailable("timed out".to_owned()))?;
        let response = match outcome {
            Ok(response) => response.into_inner(),
            Err(status) => {
                let error = error_from_status(&status);
                if matches!(error, IntrospectError::NotAuthorized) {
                    self.health.record(unix_now(), true);
                }
                return Err(error);
            }
        };
        self.health.record(unix_now(), false);
        Ok(grant_from_response(&response, unix_now()))
    }
}

/// The grant an introspection answer gives, or `None` when the answer does
/// not admit the token. Anything incomplete is refused.
#[must_use]
pub fn grant_from_response(response: &pb::IntrospectTokenResponse, now: i64) -> Option<TokenGrant> {
    let kind = pb::TokenKind::try_from(response.kind).ok()?;
    if !response.active
        || response.project_id <= 0
        || response.expires_at_unix <= now
        || response.principal.is_empty()
        || !matches!(
            kind,
            pb::TokenKind::WorkerClaim | pb::TokenKind::EngineCallback
        )
    {
        return None;
    }
    // No source, or one this build cannot name, admits nothing: a token is
    // never widened to "every source" by an answer it cannot read.
    let mut sources = Vec::with_capacity(response.allowed_sources.len());
    for raw in &response.allowed_sources {
        match pb::Source::try_from(*raw) {
            Ok(pb::Source::Unspecified) | Err(_) => return None,
            Ok(source) => {
                if !sources.contains(&source) {
                    sources.push(source);
                }
            }
        }
    }
    if sources.is_empty() {
        return None;
    }
    Some(TokenGrant {
        project_id: response.project_id,
        principal: response.principal.clone(),
        kind,
        expires_at_unix: response.expires_at_unix,
        sources,
    })
}

struct CacheEntry {
    grant: Option<TokenGrant>,
    valid_until: i64,
}

/// Cache settings.
#[derive(Clone, Copy, Debug)]
pub struct CachePolicy {
    /// The longest an admitted answer is kept, whatever the token's expiry.
    /// Never more than [`HARD_MAX_TTL_SECONDS`].
    pub max_ttl_seconds: i64,
    /// The longest an admitted worker claim token is kept. A claim token is
    /// revoked by its claim settling, being cancelled or being lost, which
    /// its expiry does not show, so this bounds how long a revoked one still
    /// works here. It never exceeds `max_ttl_seconds` or
    /// [`WORKER_CLAIM_CAP_SECONDS`].
    pub worker_claim_max_ttl_seconds: i64,
    /// How long a refusal is kept. Refusals are cached only briefly (at most
    /// a few seconds): long enough that a flood of one bad token costs
    /// elitea-main one call, short enough that a token minted a moment after
    /// its first (premature) use is not refused for long.
    pub negative_ttl_seconds: i64,
    /// The most entries kept; beyond it the least recently used go.
    pub max_entries: usize,
}

impl Default for CachePolicy {
    fn default() -> Self {
        Self {
            max_ttl_seconds: DEFAULT_MAX_TTL_SECONDS,
            worker_claim_max_ttl_seconds: WORKER_CLAIM_CAP_SECONDS,
            negative_ttl_seconds: NEGATIVE_TTL_SECONDS,
            max_entries: 10_000,
        }
    }
}

impl CachePolicy {
    /// This policy held to the hard bounds: `max_ttl_seconds` at most
    /// [`HARD_MAX_TTL_SECONDS`], the worker claim cap at most
    /// [`WORKER_CLAIM_CAP_SECONDS`] and `max_ttl_seconds`, refusals at most
    /// [`NEGATIVE_TTL_SECONDS`]; nothing below zero. A value that had to
    /// change is logged.
    #[must_use]
    pub fn bounded(self) -> Self {
        let max_ttl_seconds = self.max_ttl_seconds.clamp(0, HARD_MAX_TTL_SECONDS);
        let bounded = Self {
            max_ttl_seconds,
            worker_claim_max_ttl_seconds: self
                .worker_claim_max_ttl_seconds
                .clamp(0, WORKER_CLAIM_CAP_SECONDS)
                .min(max_ttl_seconds),
            negative_ttl_seconds: self.negative_ttl_seconds.clamp(0, NEGATIVE_TTL_SECONDS),
            max_entries: self.max_entries,
        };
        if bounded.max_ttl_seconds != self.max_ttl_seconds
            || bounded.worker_claim_max_ttl_seconds != self.worker_claim_max_ttl_seconds
            || bounded.negative_ttl_seconds != self.negative_ttl_seconds
        {
            tracing::warn!(
                requested = ?self,
                applied = ?bounded,
                "introspection cache lifetimes clamped to their hard maximums"
            );
        }
        bounded
    }
}

/// The clock the cache reads, in Unix seconds. Tests replace it.
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>;

/// Caches introspection answers, keyed by the token's SHA-256 (the token
/// itself is never kept).
///
/// The cache is a bounded LRU with single-flight misses: any number of
/// concurrent requests for one uncached token cause one call to elitea-main,
/// and all of them share its answer. A failed call is not cached.
pub struct CachingIntrospector {
    inner: Arc<dyn Introspector>,
    policy: CachePolicy,
    clock: Clock,
    entries: Cache<[u8; 32], Arc<CacheEntry>>,
}

impl CachingIntrospector {
    /// A cache in front of `inner`.
    #[must_use]
    pub fn new(inner: Arc<dyn Introspector>, policy: CachePolicy, clock: Clock) -> Self {
        let policy = policy.bounded();
        let backstop = policy.max_ttl_seconds.max(policy.negative_ttl_seconds);
        let entries = Cache::builder()
            .max_capacity(policy.max_entries as u64)
            // The entries carry their own deadline (checked against `clock`);
            // this only lets memory go once nothing could still be valid.
            .time_to_live(Duration::from_secs(u64::try_from(backstop).unwrap_or(1)))
            .build();
        Self {
            inner,
            policy,
            clock,
            entries,
        }
    }

    fn valid_until(&self, grant: Option<&TokenGrant>, now: i64) -> i64 {
        match grant {
            Some(grant) => {
                let ttl = if grant.kind == pb::TokenKind::WorkerClaim {
                    self.policy
                        .worker_claim_max_ttl_seconds
                        .min(self.policy.max_ttl_seconds)
                } else {
                    self.policy.max_ttl_seconds
                };
                grant.expires_at_unix.min(now.saturating_add(ttl))
            }
            None => now.saturating_add(self.policy.negative_ttl_seconds),
        }
    }

    async fn load(
        &self,
        digest: [u8; 32],
        token: &str,
    ) -> Result<Arc<CacheEntry>, IntrospectError> {
        self.entries
            .try_get_with(digest, async {
                let grant = self.inner.introspect(token).await?;
                let valid_until = self.valid_until(grant.as_ref(), (self.clock)());
                Ok::<_, IntrospectError>(Arc::new(CacheEntry { grant, valid_until }))
            })
            .await
            .map_err(|error: Arc<IntrospectError>| match *error {
                IntrospectError::NotAuthorized => IntrospectError::NotAuthorized,
                IntrospectError::Unavailable(ref reason) => {
                    IntrospectError::Unavailable(reason.clone())
                }
            })
    }
}

#[async_trait]
impl Introspector for CachingIntrospector {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        let digest: [u8; 32] = Sha256::digest(token.as_bytes()).into();
        let now = (self.clock)();
        let mut entry = self.load(digest, token).await?;
        if now >= entry.valid_until {
            // Past its deadline: drop it and ask once more (still shared by
            // every concurrent request for this token).
            self.entries.invalidate(&digest).await;
            entry = self.load(digest, token).await?;
        }
        // Expiry is checked on every use, cached or not.
        Ok(entry
            .grant
            .clone()
            .filter(|grant| now < grant.expires_at_unix))
    }
}

/// Admits callers.
#[derive(Clone)]
pub struct Authenticator {
    introspector: Arc<dyn Introspector>,
    admins: Arc<HashSet<String>>,
}

impl fmt::Debug for Authenticator {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Authenticator")
            .field("admins", &self.admins)
            .finish_non_exhaustive()
    }
}

impl Authenticator {
    /// An authenticator over `introspector`, with the administrator
    /// identities `admins`.
    #[must_use]
    pub fn new(introspector: Arc<dyn Introspector>, admins: HashSet<String>) -> Self {
        Self {
            introspector,
            admins: Arc::new(admins),
        }
    }

    /// The bearer token of `request`, when it carries a well-formed one.
    #[must_use]
    pub fn bearer_token<T>(request: &Request<T>) -> Option<String> {
        let value = request.metadata().get("authorization")?;
        bearer(value.to_str().ok()).map(str::to_owned)
    }

    /// Verifies `token` (through the cache) and returns its caller.
    ///
    /// # Errors
    /// `UNAUTHENTICATED` for a token that is not accepted; `UNAVAILABLE`
    /// when elitea-main cannot verify it; `FAILED_PRECONDITION` (not
    /// retryable) when elitea-main does not authorize this service to
    /// introspect at all.
    pub async fn verify_token(&self, token: &str) -> Result<Caller, Status> {
        match self.introspector.introspect(token).await {
            Ok(Some(grant)) => Ok(Caller::Token(grant)),
            Ok(None) => Err(Status::unauthenticated("the token is not accepted")),
            Err(IntrospectError::NotAuthorized) => {
                tracing::error!(
                    "elitea-main refuses this service's token introspection; \
                     check ELITEA_VECTOR_INTROSPECTION_CLIENTS and the client certificate"
                );
                Err(Status::failed_precondition(NOT_AUTHORIZED_MESSAGE))
            }
            Err(error) => {
                tracing::warn!(%error, "token introspection failed; request refused");
                Err(Status::unavailable("token verification is unavailable"))
            }
        }
    }

    /// Admits the caller of `request`.
    ///
    /// # Errors
    /// `UNAUTHENTICATED` for a missing, malformed or unusable credential;
    /// `UNAVAILABLE` when elitea-main cannot verify a token.
    pub async fn authenticate<T>(&self, request: &Request<T>) -> Result<Caller, Status> {
        if let Some(value) = request.metadata().get("authorization") {
            let token = bearer(value.to_str().ok())
                .ok_or_else(|| Status::unauthenticated("malformed authorization metadata"))?;
            return self.verify_token(token).await;
        }
        let identity = request.peer_certs().and_then(|certificates| {
            certificates
                .first()
                .and_then(|der| certificate_identity(der))
        });
        match identity {
            Some(identity) if self.admins.contains(&identity) => Ok(Caller::Admin(identity)),
            _ => Err(Status::unauthenticated("a project token is required")),
        }
    }
}

fn bearer(value: Option<&str>) -> Option<&str> {
    let value = value?;
    let (scheme, token) = value.split_once(' ')?;
    let token = token.trim();
    (scheme.eq_ignore_ascii_case("bearer")
        && !token.is_empty()
        && token.len() <= MAX_TOKEN_BYTES
        && !token.contains(char::is_whitespace))
    .then_some(token)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};

    struct Counting {
        calls: AtomicUsize,
        answer: Option<TokenGrant>,
    }

    #[async_trait]
    impl Introspector for Counting {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(self.answer.clone())
        }
    }

    struct Down;

    #[async_trait]
    impl Introspector for Down {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            Err(IntrospectError::Unavailable("down".to_owned()))
        }
    }

    fn grant(project_id: i64, expires_at_unix: i64) -> TokenGrant {
        TokenGrant {
            project_id,
            principal: "user:1".to_owned(),
            kind: pb::TokenKind::EngineCallback,
            expires_at_unix,
            sources: vec![pb::Source::Deepwiki],
        }
    }

    fn claim_grant(project_id: i64, expires_at_unix: i64) -> TokenGrant {
        TokenGrant {
            kind: pb::TokenKind::WorkerClaim,
            sources: vec![pb::Source::ToolkitIndex],
            ..grant(project_id, expires_at_unix)
        }
    }

    #[tokio::test]
    async fn a_worker_claim_token_is_cached_for_less_time() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(claim_grant(7, 100_000)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        cache.introspect("t").await.expect("ok");
        now.store(1_004, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        // Past the worker claim cap (5 s), well inside the general one.
        now.store(1_005, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    }

    /// Slow enough that every concurrent request arrives while the first
    /// call is still in flight.
    struct Slow {
        calls: AtomicUsize,
    }

    #[async_trait]
    impl Introspector for Slow {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            tokio::time::sleep(Duration::from_millis(100)).await;
            Ok(Some(grant(7, 100_000)))
        }
    }

    #[tokio::test]
    async fn concurrent_requests_for_one_uncached_token_cause_one_call() {
        let inner = Arc::new(Slow {
            calls: AtomicUsize::new(0),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = Arc::new(CachingIntrospector::new(
            inner.clone(),
            CachePolicy::default(),
            clock(&now),
        ));
        let mut tasks = Vec::new();
        for _ in 0..50 {
            let cache = Arc::clone(&cache);
            tasks.push(tokio::spawn(async move { cache.introspect("one").await }));
        }
        for task in tasks {
            assert_eq!(
                task.await.expect("joined").expect("answered"),
                Some(grant(7, 100_000))
            );
        }
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn the_default_caps_are_short() {
        let policy = CachePolicy::default();
        assert_eq!(policy.worker_claim_max_ttl_seconds, 5);
        assert_eq!(policy.max_ttl_seconds, 60);
        assert!(policy.negative_ttl_seconds <= 5);
    }

    #[test]
    fn configured_caps_are_clamped_to_the_hard_maximums() {
        let policy = CachePolicy {
            max_ttl_seconds: 86_400,
            worker_claim_max_ttl_seconds: 600,
            negative_ttl_seconds: 120,
            max_entries: 10,
        }
        .bounded();
        assert_eq!(policy.max_ttl_seconds, 300);
        assert_eq!(policy.worker_claim_max_ttl_seconds, 5);
        assert_eq!(policy.negative_ttl_seconds, 5);
        // The worker cap never exceeds the general one.
        let tight = CachePolicy {
            max_ttl_seconds: 2,
            ..CachePolicy::default()
        }
        .bounded();
        assert_eq!(tight.worker_claim_max_ttl_seconds, 2);
        // A configured value inside the bounds is kept.
        let kept = CachePolicy {
            max_ttl_seconds: 120,
            ..CachePolicy::default()
        }
        .bounded();
        assert_eq!(kept.max_ttl_seconds, 120);
    }

    #[tokio::test]
    async fn an_over_long_configured_cap_still_cannot_exceed_the_hard_maximum() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, 1_000_000)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let policy = CachePolicy {
            max_ttl_seconds: 86_400,
            ..CachePolicy::default()
        };
        let cache = CachingIntrospector::new(inner.clone(), policy, clock(&now));
        cache.introspect("t").await.expect("ok");
        now.store(1_299, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        now.store(1_300, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn a_callback_token_is_cached_for_the_default_minute() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, 100_000)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        cache.introspect("t").await.expect("ok");
        now.store(1_059, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        now.store(1_060, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    }

    struct Unauthorized;

    #[async_trait]
    impl Introspector for Unauthorized {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            Err(IntrospectError::NotAuthorized)
        }
    }

    #[test]
    fn a_permission_refusal_from_main_is_not_authorized() {
        for code in [tonic::Code::PermissionDenied, tonic::Code::Unauthenticated] {
            assert!(matches!(
                error_from_status(&Status::new(code, "no")),
                IntrospectError::NotAuthorized
            ));
        }
        for code in [
            tonic::Code::Unavailable,
            tonic::Code::DeadlineExceeded,
            tonic::Code::Internal,
        ] {
            assert!(matches!(
                error_from_status(&Status::new(code, "x")),
                IntrospectError::Unavailable(_)
            ));
        }
    }

    #[tokio::test]
    async fn not_authorized_reaches_the_caller_as_a_failed_precondition() {
        let now = Arc::new(AtomicI64::new(1_000));
        // Through the cache too: the distinction survives single-flight.
        let cache =
            CachingIntrospector::new(Arc::new(Unauthorized), CachePolicy::default(), clock(&now));
        let auth = Authenticator::new(Arc::new(cache), HashSet::new());
        let mut request = Request::new(());
        request
            .metadata_mut()
            .insert("authorization", "Bearer t".parse().expect("metadata"));
        let status = auth.authenticate(&request).await.expect_err("refused");
        assert_eq!(status.code(), tonic::Code::FailedPrecondition);
        assert_eq!(
            status.message(),
            "vector service is not authorized to introspect tokens; check \
             ELITEA_VECTOR_INTROSPECTION_CLIENTS and the client certificate"
        );
    }

    #[test]
    fn readiness_follows_the_last_introspection_attempt_for_a_minute() {
        let health = IntrospectionHealth::default();
        assert!(!health.not_authorized(1_000), "nothing attempted yet");
        health.record(1_000, true);
        assert!(health.not_authorized(1_000));
        assert!(health.not_authorized(1_059));
        assert!(!health.not_authorized(1_060), "the refusal aged out");
        // An answered attempt clears it.
        health.record(1_010, false);
        assert!(!health.not_authorized(1_011));
        health.record(1_020, true);
        assert!(health.not_authorized(1_021));
    }

    #[tokio::test]
    async fn a_refused_attempt_records_not_authorized_and_an_answer_clears_it() {
        // A real tonic server standing in for elitea-main (plaintext over
        // loopback), answering as configured.
        use crate::pb::token_introspection_service_server::{
            TokenIntrospectionService, TokenIntrospectionServiceServer,
        };
        type Answer = Arc<std::sync::Mutex<Result<(), tonic::Code>>>;
        struct Main(Answer);
        #[tonic::async_trait]
        impl TokenIntrospectionService for Main {
            async fn introspect_token(
                &self,
                _: Request<pb::IntrospectTokenRequest>,
            ) -> Result<tonic::Response<pb::IntrospectTokenResponse>, Status> {
                match *self.0.lock().expect("lock") {
                    Ok(()) => Ok(tonic::Response::new(pb::IntrospectTokenResponse::default())),
                    Err(code) => Err(Status::new(code, "denied")),
                }
            }
        }
        let answer: Answer = Arc::new(std::sync::Mutex::new(Err(tonic::Code::PermissionDenied)));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind");
        let port = listener.local_addr().expect("addr").port();
        tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(TokenIntrospectionServiceServer::new(Main(Arc::clone(
                    &answer,
                ))))
                .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener)),
        );
        let channel = tonic::transport::Endpoint::from_shared(format!("http://127.0.0.1:{port}"))
            .expect("endpoint")
            .connect_lazy();
        let introspector = Arc::new(GrpcIntrospector::new(channel, Duration::from_secs(2)));
        let health = introspector.health();

        // Not authorized: the self-check records it.
        introspector.self_check().await;
        assert!(health.not_authorized(unix_now()));
        assert!(matches!(
            introspector.introspect("t").await,
            Err(IntrospectError::NotAuthorized)
        ));

        // Authorized (main answers "inactive"): the next check clears it.
        *answer.lock().expect("lock") = Ok(());
        introspector.self_check().await;
        assert!(!health.not_authorized(unix_now()));
        assert!(
            introspector
                .introspect("t")
                .await
                .expect("answered")
                .is_none()
        );
    }

    #[tokio::test]
    async fn a_refusal_is_cached_only_briefly() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: None,
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        assert_eq!(cache.introspect("bad").await.expect("ok"), None);
        assert_eq!(cache.introspect("bad").await.expect("ok"), None);
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        now.store(1_005, Ordering::SeqCst);
        assert_eq!(cache.introspect("bad").await.expect("ok"), None);
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn a_failed_call_is_not_cached() {
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(Arc::new(Down), CachePolicy::default(), clock(&now));
        assert!(cache.introspect("t").await.is_err());
        assert!(cache.introspect("t").await.is_err());
    }

    #[tokio::test]
    async fn the_cache_is_bounded() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, 100_000)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let policy = CachePolicy {
            max_entries: 10,
            ..CachePolicy::default()
        };
        let cache = CachingIntrospector::new(inner, policy, clock(&now));
        for index in 0..200 {
            cache.introspect(&format!("t{index}")).await.expect("ok");
        }
        cache.entries.run_pending_tasks().await;
        assert!(cache.entries.entry_count() <= 10);
    }

    #[test]
    fn a_token_caller_may_use_only_its_sources() {
        let worker = Caller::Token(claim_grant(7, i64::MAX));
        assert!(
            worker
                .require_source(pb::Source::ToolkitIndex as i32)
                .is_ok()
        );
        for denied in [pb::Source::Deepwiki, pb::Source::Inventory] {
            assert_eq!(
                worker
                    .require_source(denied as i32)
                    .expect_err("denied")
                    .code(),
                tonic::Code::PermissionDenied
            );
        }
        let callback = Caller::Token(grant(7, i64::MAX));
        assert!(callback.require_source(pb::Source::Deepwiki as i32).is_ok());
        assert!(
            callback
                .require_source(pb::Source::ToolkitIndex as i32)
                .is_err()
        );
        let admin = Caller::Admin("dns:elitea-main".to_owned());
        assert!(admin.require_source(pb::Source::Deepwiki as i32).is_ok());
        let all = [pb::Source::ToolkitIndex, pb::Source::Deepwiki];
        assert_eq!(
            worker.permitted_sources(&all),
            vec![pb::Source::ToolkitIndex]
        );
        assert_eq!(callback.permitted_sources(&all), vec![pb::Source::Deepwiki]);
        assert_eq!(admin.permitted_sources(&all), all.to_vec());
    }

    fn clock(now: &Arc<AtomicI64>) -> Clock {
        let now = Arc::clone(now);
        Arc::new(move || now.load(Ordering::SeqCst))
    }

    #[tokio::test]
    async fn answers_are_cached_until_expiry_and_expiry_is_rechecked() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, 1_100)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        assert_eq!(
            cache.introspect("t").await.expect("ok"),
            Some(grant(7, 1_100))
        );
        assert_eq!(
            cache.introspect("t").await.expect("ok"),
            Some(grant(7, 1_100))
        );
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        now.store(1_100, Ordering::SeqCst);
        // Expired: refused even though the old answer said active.
        assert_eq!(cache.introspect("t").await.expect("ok"), None);
    }

    #[tokio::test]
    async fn cache_lifetime_is_capped() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, 100_000)),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let policy = CachePolicy {
            max_ttl_seconds: 60,
            ..CachePolicy::default()
        };
        let cache = CachingIntrospector::new(inner.clone(), policy, clock(&now));
        cache.introspect("t").await.expect("ok");
        now.store(1_061, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn unavailable_main_fails_closed() {
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(Arc::new(Down), CachePolicy::default(), clock(&now));
        let auth = Authenticator::new(Arc::new(cache), HashSet::new());
        let mut request = Request::new(());
        request
            .metadata_mut()
            .insert("authorization", "Bearer t".parse().expect("metadata"));
        let status = auth.authenticate(&request).await.expect_err("refused");
        assert_eq!(status.code(), tonic::Code::Unavailable);
    }

    #[tokio::test]
    async fn missing_and_malformed_credentials_are_refused() {
        let inner = Arc::new(Counting {
            calls: AtomicUsize::new(0),
            answer: Some(grant(7, i64::MAX)),
        });
        let auth = Authenticator::new(inner, HashSet::from(["dns:elitea-main".to_owned()]));
        let status = auth
            .authenticate(&Request::new(()))
            .await
            .expect_err("no token, no certificate");
        assert_eq!(status.code(), tonic::Code::Unauthenticated);
        for value in ["t", "Basic t", "Bearer ", "Bearer a b"] {
            let mut request = Request::new(());
            request
                .metadata_mut()
                .insert("authorization", value.parse().expect("metadata"));
            let status = auth.authenticate(&request).await.expect_err(value);
            assert_eq!(status.code(), tonic::Code::Unauthenticated, "{value}");
        }
    }

    #[test]
    fn a_token_caller_cannot_name_another_project() {
        let caller = Caller::Token(grant(7, i64::MAX));
        assert_eq!(caller.project(0).expect("own"), 7);
        assert_eq!(caller.project(7).expect("own"), 7);
        assert_eq!(
            caller.project(8).expect_err("other").code(),
            tonic::Code::PermissionDenied
        );
        assert!(caller.require_admin().is_err());
        let admin = Caller::Admin("dns:elitea-main".to_owned());
        assert_eq!(admin.project(8).expect("named"), 8);
        assert!(admin.project(0).is_err());
        assert!(admin.require_token().is_err());
    }

    #[test]
    fn incomplete_answers_admit_nothing() {
        let good = pb::IntrospectTokenResponse {
            active: true,
            project_id: 7,
            principal: "user:1".to_owned(),
            kind: pb::TokenKind::EngineCallback as i32,
            expires_at_unix: 2_000,
            allowed_sources: vec![pb::Source::Deepwiki as i32],
        };
        assert_eq!(
            grant_from_response(&good, 1_000).expect("good").sources,
            vec![pb::Source::Deepwiki]
        );
        for bad in [
            pb::IntrospectTokenResponse {
                active: false,
                ..good.clone()
            },
            pb::IntrospectTokenResponse {
                project_id: 0,
                ..good.clone()
            },
            pb::IntrospectTokenResponse {
                expires_at_unix: 1_000,
                ..good.clone()
            },
            pb::IntrospectTokenResponse {
                kind: 0,
                ..good.clone()
            },
            pb::IntrospectTokenResponse {
                principal: String::new(),
                ..good.clone()
            },
            // No source is not every source.
            pb::IntrospectTokenResponse {
                allowed_sources: Vec::new(),
                ..good.clone()
            },
            pb::IntrospectTokenResponse {
                allowed_sources: vec![pb::Source::Unspecified as i32],
                ..good.clone()
            },
            // A source this build cannot name.
            pb::IntrospectTokenResponse {
                allowed_sources: vec![pb::Source::Deepwiki as i32, 99],
                ..good.clone()
            },
        ] {
            assert!(grant_from_response(&bad, 1_000).is_none(), "{bad:?}");
        }
    }
}
