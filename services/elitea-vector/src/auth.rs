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
//! * **Administrator.** No token, and a client certificate whose identity
//!   ([`crate::identity`]) is in the administrator list (elitea-main). This
//!   caller names the project in the request.
//!
//! Anything else is `UNAUTHENTICATED`. When elitea-main cannot answer, the
//! request is refused with `UNAVAILABLE`: the service fails closed.

use std::collections::{HashMap, HashSet};
use std::fmt;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use sha2::{Digest as _, Sha256};
use tonic::transport::Channel;
use tonic::{Request, Status};

use crate::identity::certificate_identity;
use crate::pb;
use crate::pb::token_introspection_service_client::TokenIntrospectionServiceClient;

/// The longest bearer token accepted.
const MAX_TOKEN_BYTES: usize = 4096;

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
    client: TokenIntrospectionServiceClient<Channel>,
    timeout: Duration,
}

impl GrpcIntrospector {
    /// A client over `channel`; each call waits at most `timeout`.
    #[must_use]
    pub fn new(channel: Channel, timeout: Duration) -> Self {
        Self {
            client: TokenIntrospectionServiceClient::new(channel),
            timeout,
        }
    }
}

#[async_trait]
impl Introspector for GrpcIntrospector {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        let mut client = self.client.clone();
        let call = client.introspect_token(pb::IntrospectTokenRequest {
            token: token.to_owned(),
        });
        let response = tokio::time::timeout(self.timeout, call)
            .await
            .map_err(|_| IntrospectError::Unavailable("timed out".to_owned()))?
            .map_err(|status| IntrospectError::Unavailable(status.code().to_string()))?
            .into_inner();
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

#[derive(Clone)]
struct CacheEntry {
    grant: Option<TokenGrant>,
    valid_until: i64,
}

/// Cache settings.
#[derive(Clone, Copy, Debug)]
pub struct CachePolicy {
    /// The longest an admitted answer is kept, whatever the token's expiry.
    pub max_ttl_seconds: i64,
    /// The longest an admitted worker claim token is kept. A claim token is
    /// revoked by its claim settling, being cancelled or being lost, which
    /// its expiry does not show, so this bounds how long a revoked one still
    /// works here. It never exceeds `max_ttl_seconds`.
    pub worker_claim_max_ttl_seconds: i64,
    /// How long a refusal is kept.
    pub negative_ttl_seconds: i64,
    /// The most entries kept; a full cache drops its expired entries, then
    /// everything.
    pub max_entries: usize,
}

impl Default for CachePolicy {
    fn default() -> Self {
        Self {
            max_ttl_seconds: 300,
            worker_claim_max_ttl_seconds: 30,
            negative_ttl_seconds: 10,
            max_entries: 10_000,
        }
    }
}

/// The clock the cache reads, in Unix seconds. Tests replace it.
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>;

/// Caches introspection answers, keyed by the token's SHA-256 (the token
/// itself is never kept).
pub struct CachingIntrospector {
    inner: Arc<dyn Introspector>,
    policy: CachePolicy,
    clock: Clock,
    entries: Mutex<HashMap<[u8; 32], CacheEntry>>,
}

impl CachingIntrospector {
    /// A cache in front of `inner`.
    #[must_use]
    pub fn new(inner: Arc<dyn Introspector>, policy: CachePolicy, clock: Clock) -> Self {
        Self {
            inner,
            policy,
            clock,
            entries: Mutex::new(HashMap::new()),
        }
    }

    /// A live cache entry, if any.
    fn lookup(&self, digest: &[u8; 32], now: i64) -> Option<CacheEntry> {
        let entries = self.entries.lock().ok()?;
        let entry = entries.get(digest)?;
        (now < entry.valid_until).then(|| entry.clone())
    }

    fn store(&self, digest: [u8; 32], grant: Option<TokenGrant>, now: i64) {
        let valid_until = match &grant {
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
        };
        let Ok(mut entries) = self.entries.lock() else {
            return;
        };
        if entries.len() >= self.policy.max_entries {
            entries.retain(|_, entry| now < entry.valid_until);
            if entries.len() >= self.policy.max_entries {
                entries.clear();
            }
        }
        entries.insert(digest, CacheEntry { grant, valid_until });
    }
}

#[async_trait]
impl Introspector for CachingIntrospector {
    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        let digest: [u8; 32] = Sha256::digest(token.as_bytes()).into();
        let now = (self.clock)();
        let grant = if let Some(cached) = self.lookup(&digest, now) {
            cached.grant
        } else {
            let fresh = self.inner.introspect(token).await?;
            self.store(digest, fresh.clone(), now);
            fresh
        };
        // Expiry is checked on every use, cached or not.
        Ok(grant.filter(|grant| now < grant.expires_at_unix))
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

    /// Admits the caller of `request`.
    ///
    /// # Errors
    /// `UNAUTHENTICATED` for a missing, malformed or unusable credential;
    /// `UNAVAILABLE` when elitea-main cannot verify a token.
    pub async fn authenticate<T>(&self, request: &Request<T>) -> Result<Caller, Status> {
        if let Some(value) = request.metadata().get("authorization") {
            let token = bearer(value.to_str().ok())
                .ok_or_else(|| Status::unauthenticated("malformed authorization metadata"))?;
            return match self.introspector.introspect(token).await {
                Ok(Some(grant)) => Ok(Caller::Token(grant)),
                Ok(None) => Err(Status::unauthenticated("the token is not accepted")),
                Err(error) => {
                    tracing::warn!(%error, "token introspection failed; request refused");
                    Err(Status::unavailable("token verification is unavailable"))
                }
            };
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
        now.store(1_029, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 1);
        // Past the worker claim cap (30 s), well inside the general one.
        now.store(1_030, Ordering::SeqCst);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
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
