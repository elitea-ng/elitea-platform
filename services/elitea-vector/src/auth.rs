//! Who is calling, and for which project. Deny by default.
//!
//! A request is admitted in one of two ways:
//!
//! * **Token.** `authorization: Bearer <token>` metadata. elitea-main
//!   verifies the token ([`Introspector`]); the answer is cached until the
//!   token expires, and never longer than the configured maximum (shorter
//!   still for a worker claim token, whose revocation its expiry does not
//!   show). A request that needs a memory decision (a personal-scope request
//!   or a project-scope write) is NOT served from the cache: it asks
//!   elitea-main afresh ([`Authenticator::fresh_token`]), because its permission
//!   depends on the conversation's participants and on whether the run has
//!   read personal memory. The token's project is the request's project, and the token's
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

/// The `owner` payload value of project-shared memory.
pub const PROJECT_OWNER: &str = "project";

/// The `owner` payload value of one user's personal memory.
#[must_use]
pub fn user_owner(user_id: i64) -> String {
    format!("user:{user_id}")
}

/// Who and what a memory request works on, decided from the token and the
/// worker's toolkit reference; see [`Caller::memory_binding`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MemoryBinding {
    /// `user:<id>` or `project`.
    pub owner: String,
    /// The memory toolkit.
    pub toolkit_id: i64,
}

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
    /// Whether the token may use the PERSONAL memory scope: elitea-main says
    /// so for a worker claim of a run whose conversation has one human
    /// participant. Never true for a callback token.
    pub personal_memory_allowed: bool,
    /// Set by elitea-main for a project-memory or shared-index WRITE asked by
    /// an execution bound to a conversation in which personal memory was read
    /// (by any turn): shared data must not be written from such a
    /// conversation.
    pub project_memory_write_refused: bool,
    /// The memory toolkits a worker claim was given (every one a toolkit of
    /// the token's project, frozen by elitea-main from the run's own
    /// toolkits). A memory request naming any other toolkit id is refused, so
    /// a token cannot reach the memory store of a toolkit its run was not
    /// given. Empty for a callback token.
    pub memory_toolkit_ids: Vec<i64>,
    /// Set when elitea-main established that the token's execution is bound
    /// to no conversation (an index ingest, a direct toolkit call). Only then
    /// is the shared-write question skipped; the default (false) asks.
    pub conversation_unbound: bool,
}

/// The refusal of a project-memory write in a conversation where personal
/// memory was read.
pub const PROJECT_WRITE_REFUSED: &str = "memory.project_write_after_personal_read: this conversation read personal memory, so it cannot write shared project memory";

/// The refusal of a toolkit-index write in a conversation where personal
/// memory was read.
pub const SHARED_INDEX_WRITE_REFUSED: &str = "memory.shared_write_after_personal_read: this conversation read personal memory, so it cannot write a shared index";

/// What a memory request is about to do, told to elitea-main with the
/// introspection so it decides the memory permissions at that moment.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum MemoryIntent {
    /// No memory decision wanted: every non-memory request and project-scope
    /// memory reads. Such an answer comes from the claim cache.
    #[default]
    None,
    /// A read in the personal scope.
    PersonalRead,
    /// A write in the personal scope.
    PersonalWrite,
    /// A write in the project scope.
    ProjectWrite,
    /// A toolkit-index upsert or delete by a worker claim (an index is shared
    /// by the project): refused in a conversation where personal memory was
    /// read.
    SharedIndexWrite,
}

impl MemoryIntent {
    /// The intent of a MEMORY operation in `scope` (`write` or read).
    #[must_use]
    pub fn of(memory_scope: i32, write: bool) -> Self {
        match (pb::MemoryScope::try_from(memory_scope), write) {
            (Ok(pb::MemoryScope::Personal), false) => Self::PersonalRead,
            (Ok(pb::MemoryScope::Personal), true) => Self::PersonalWrite,
            (Ok(pb::MemoryScope::Project), true) => Self::ProjectWrite,
            _ => Self::None,
        }
    }

    /// The intent of an operation on `source` (`write` or read) by `caller`:
    /// a memory operation by its scope, a toolkit-index write by a worker
    /// claim (the only token that runs in a conversation) as a shared-index
    /// write, anything else none.
    #[must_use]
    pub fn for_request(caller: &Caller, source: i32, memory_scope: i32, write: bool) -> Self {
        let Caller::Token(grant) = caller else {
            return Self::None;
        };
        if source == pb::Source::Memory as i32 {
            return Self::of(memory_scope, write);
        }
        // A toolkit-index write by a token whose execution can be in a
        // conversation. A callback token never names `toolkit_index` (its
        // source is its provider's), and an index ingest or a direct toolkit
        // call has no conversation, so neither costs an introspection.
        if write
            && source == pb::Source::ToolkitIndex as i32
            && grant.kind == pb::TokenKind::WorkerClaim
            && !grant.conversation_unbound
        {
            return Self::SharedIndexWrite;
        }
        Self::None
    }

    /// Whether the decision is a shared write (project memory or a shared
    /// index), which a tainted conversation refuses.
    #[must_use]
    pub const fn is_shared_write(self) -> bool {
        matches!(self, Self::ProjectWrite | Self::SharedIndexWrite)
    }

    const fn wire(self) -> pb::MemoryIntent {
        match self {
            Self::None => pb::MemoryIntent::Unspecified,
            Self::PersonalRead => pb::MemoryIntent::PersonalRead,
            Self::PersonalWrite => pb::MemoryIntent::PersonalWrite,
            Self::ProjectWrite => pb::MemoryIntent::ProjectWrite,
            Self::SharedIndexWrite => pb::MemoryIntent::SharedIndexWrite,
        }
    }
}

impl TokenGrant {
    /// The user the token was minted for: the id of a `user:<id>` principal
    /// of a WORKER CLAIM token. An engine callback token has no user identity
    /// for memory, whatever its principal says.
    #[must_use]
    pub fn memory_user(&self) -> Option<i64> {
        if self.kind != pb::TokenKind::WorkerClaim {
            return None;
        }
        self.principal
            .strip_prefix("user:")
            .filter(|id| !id.is_empty() && id.bytes().all(|byte| byte.is_ascii_digit()))
            .and_then(|id| id.parse::<i64>().ok())
            .filter(|id| *id > 0)
    }

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

    /// What a MEMORY request works under: the owner from the TOKEN and the
    /// toolkit the worker named.
    ///
    /// * `source` is not memory: `memory_scope` and `toolkit_id` must be unset,
    ///   and the answer is `None`.
    /// * `source` is memory: `memory_scope` and a positive `toolkit_id` are
    ///   required. PROJECT is the literal `project` owner. PERSONAL is
    ///   `user:<id>` of the token's user, refused when the token names no user
    ///   or elitea-main did not allow personal memory for the claim (it
    ///   re-checks on every introspection). An administrator caller has no
    ///   memory binding: it uses `DeleteMemoryUser` and `DeleteMemoryToolkit`.
    ///
    /// # Errors
    /// `INVALID_ARGUMENT` for a missing or misplaced scope or toolkit;
    /// `PERMISSION_DENIED` as above, with a stable `memory.` reason the client
    /// can word.
    pub fn memory_binding(
        &self,
        source: i32,
        memory_scope: i32,
        toolkit_id: i64,
        write: bool,
    ) -> Result<Option<MemoryBinding>, Status> {
        self.binding_checked(source, memory_scope, toolkit_id, write, true)
    }

    /// The checks of [`Self::memory_binding`] that a CACHED grant answers
    /// (the scope, the toolkit, the token's user): everything but the
    /// per-moment decisions (the personal permission and the write-after-read
    /// refusal), which only a fresh introspection makes. Run before asking
    /// elitea-main afresh, so a request refused for a fixed reason never
    /// reaches the introspection that would record a personal read.
    ///
    /// # Errors
    /// As [`Self::memory_binding`], without the per-moment refusals.
    pub fn precheck_memory(
        &self,
        source: i32,
        memory_scope: i32,
        toolkit_id: i64,
    ) -> Result<(), Status> {
        self.binding_checked(source, memory_scope, toolkit_id, false, false)
            .map(|_| ())
    }

    fn binding_checked(
        &self,
        source: i32,
        memory_scope: i32,
        toolkit_id: i64,
        write: bool,
        per_moment: bool,
    ) -> Result<Option<MemoryBinding>, Status> {
        let is_memory = source == pb::Source::Memory as i32;
        if !is_memory {
            return if memory_scope == pb::MemoryScope::Unspecified as i32 && toolkit_id == 0 {
                Ok(None)
            } else {
                Err(Status::invalid_argument(
                    "memory_scope and toolkit_id apply to the memory source only",
                ))
            };
        }
        let Self::Token(grant) = self else {
            return Err(Status::permission_denied(
                "memory.administrator: an administrator has no memory scope; use `DeleteMemoryUser` or `DeleteMemoryToolkit`",
            ));
        };
        if toolkit_id <= 0 {
            return Err(Status::invalid_argument(
                "toolkit_id is required for the memory source",
            ));
        }
        // The toolkit must be one the run was given: elitea-main froze them
        // from the run's own project, so this also binds the toolkit to the
        // token's project (the point's project_id and the scope of every
        // operation are the token's).
        if !grant.memory_toolkit_ids.contains(&toolkit_id) {
            return Err(Status::permission_denied(
                "memory.toolkit_not_in_run: this run was not given that memory toolkit",
            ));
        }
        let owner = match pb::MemoryScope::try_from(memory_scope) {
            Ok(pb::MemoryScope::Project) => {
                // Shared memory is not written from a run that has read
                // personal memory: what it saw could be copied into it.
                if per_moment && write && grant.project_memory_write_refused {
                    return Err(Status::permission_denied(PROJECT_WRITE_REFUSED));
                }
                PROJECT_OWNER.to_owned()
            }
            Ok(pb::MemoryScope::Personal) => {
                let user = grant.memory_user().ok_or_else(|| {
                    Status::permission_denied(
                        "memory.personal_unavailable: this token names no user, so there is no personal memory",
                    )
                })?;
                if per_moment && !grant.personal_memory_allowed {
                    return Err(Status::permission_denied(
                        "memory.personal_shared_conversation: personal memory is only available in a private conversation that only this person is in",
                    ));
                }
                user_owner(user)
            }
            Ok(pb::MemoryScope::Unspecified) | Err(_) => {
                return Err(Status::invalid_argument(
                    "memory_scope is required for the memory source",
                ));
            }
        };
        Ok(Some(MemoryBinding { owner, toolkit_id }))
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

    /// Like [`Self::introspect`], telling elitea-main what a memory request is
    /// about to do. An answer for an intent other than [`MemoryIntent::None`]
    /// is made afresh and is never cached. The default ignores the intent.
    ///
    /// # Errors
    /// As [`Self::introspect`].
    async fn introspect_for(
        &self,
        token: &str,
        intent: MemoryIntent,
    ) -> Result<Option<TokenGrant>, IntrospectError> {
        let _ = intent;
        self.introspect(token).await
    }
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
        self.introspect_for(token, MemoryIntent::None).await
    }

    async fn introspect_for(
        &self,
        token: &str,
        intent: MemoryIntent,
    ) -> Result<Option<TokenGrant>, IntrospectError> {
        let mut client = TokenIntrospectionServiceClient::clone(&self.client.load());
        let call = client.introspect_token(pb::IntrospectTokenRequest {
            token: token.to_owned(),
            memory_intent: intent.wire() as i32,
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
    // A source this build cannot name is IGNORED and the ones it can name are
    // kept: a newer elitea-main may add a source, and rejecting the whole
    // token for it would take down the sources this build does serve (a
    // rolling upgrade). Nothing is widened: an ignored source admits nothing,
    // and a token left with no known source admits nothing at all.
    let mut sources = Vec::with_capacity(response.allowed_sources.len());
    for raw in &response.allowed_sources {
        match pb::Source::try_from(*raw) {
            Ok(pb::Source::Unspecified) | Err(_) => {
                tracing::debug!(source = *raw, "ignoring an unknown source in a grant");
            }
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
        personal_memory_allowed: response.personal_memory_allowed
            && kind == pb::TokenKind::WorkerClaim,
        project_memory_write_refused: response.project_memory_write_refused
            && kind == pb::TokenKind::WorkerClaim,
        // Only a worker claim can be told it is unbound; any other kind is
        // never asked (it cannot name toolkit_index).
        conversation_unbound: response.conversation_unbound && kind == pb::TokenKind::WorkerClaim,
        memory_toolkit_ids: if kind == pb::TokenKind::WorkerClaim {
            response
                .memory_toolkit_ids
                .iter()
                .copied()
                .filter(|id| *id > 0)
                .collect()
        } else {
            Vec::new()
        },
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
    /// Allowed personal-read answers, reused for
    /// [`PERSONAL_READ_REUSE_SECONDS`] (valid-until, grant).
    personal_reads: Cache<[u8; 32], Arc<(i64, TokenGrant)>>,
}

/// How long an allowed personal-read answer is reused by the same token. A
/// conversation made shared (or a participant added) mid-turn is seen within
/// this window; the taint was recorded by the first answer.
pub const PERSONAL_READ_REUSE_SECONDS: i64 = 3;

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
        let personal_reads = Cache::builder()
            .max_capacity(policy.max_entries as u64)
            .time_to_live(std::time::Duration::from_secs(
                PERSONAL_READ_REUSE_SECONDS.unsigned_abs() + 1,
            ))
            .build();
        Self {
            inner,
            policy,
            clock,
            entries,
            personal_reads,
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

    /// The entry for `digest`, and whether this call loaded it (as opposed to
    /// finding or sharing one).
    async fn load(
        &self,
        digest: [u8; 32],
        token: &str,
    ) -> Result<(Arc<CacheEntry>, bool), IntrospectError> {
        let loaded = std::sync::atomic::AtomicBool::new(false);
        let entry = self
            .entries
            .try_get_with(digest, async {
                loaded.store(true, std::sync::atomic::Ordering::Relaxed);
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
            })?;
        Ok((entry, loaded.load(std::sync::atomic::Ordering::Relaxed)))
    }
}

#[async_trait]
impl Introspector for CachingIntrospector {
    async fn introspect_for(
        &self,
        token: &str,
        intent: MemoryIntent,
    ) -> Result<Option<TokenGrant>, IntrospectError> {
        if intent == MemoryIntent::None {
            return self.introspect(token).await;
        }
        let now = (self.clock)();
        // A personal READ answer may be reused for PERSONAL_READ_REUSE_SECONDS
        // by the same token: the first one already tainted the conversation
        // and decided the participants, so a burst of searches in one turn
        // costs elitea-main one call. A conversation made shared, or a
        // participant added, is therefore seen within that window. Every
        // other memory decision (a personal write, a shared write) is asked
        // afresh each time.
        let digest: [u8; 32] = Sha256::digest(token.as_bytes()).into();
        if intent == MemoryIntent::PersonalRead
            && let Some(hit) = self.personal_reads.get(&digest).await
            && now < hit.0
        {
            return Ok(Some(hit.1.clone()).filter(|grant| now < grant.expires_at_unix));
        }
        let answer = self
            .inner
            .introspect_for(token, intent)
            .await?
            .filter(|grant| now < grant.expires_at_unix);
        if intent == MemoryIntent::PersonalRead
            && let Some(grant) = answer
                .as_ref()
                .filter(|grant| grant.personal_memory_allowed)
        {
            self.personal_reads
                .insert(
                    digest,
                    Arc::new((
                        now.saturating_add(PERSONAL_READ_REUSE_SECONDS)
                            .min(grant.expires_at_unix),
                        grant.clone(),
                    )),
                )
                .await;
        }
        Ok(answer)
    }

    async fn introspect(&self, token: &str) -> Result<Option<TokenGrant>, IntrospectError> {
        let digest: [u8; 32] = Sha256::digest(token.as_bytes()).into();
        let now = (self.clock)();
        let (mut entry, fresh) = self.load(digest, token).await?;
        if !fresh && now >= entry.valid_until {
            // Past its deadline: drop it and ask once more (still shared by
            // every concurrent request for this token).
            self.entries.invalidate(&digest).await;
            entry = self.load(digest, token).await?.0;
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

    /// Verifies `token` afresh (never from the cache) for a memory request
    /// about to do `intent`. Only a token caller is asked: an administrator
    /// has no memory scope.
    ///
    /// # Errors
    /// As [`Self::verify_token`].
    pub async fn fresh_token(&self, token: &str, intent: MemoryIntent) -> Result<Caller, Status> {
        match self.introspector.introspect_for(token, intent).await {
            Ok(Some(grant)) => Ok(Caller::Token(grant)),
            Ok(None) => Err(Status::unauthenticated("the token is not accepted")),
            Err(IntrospectError::NotAuthorized) => {
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
            personal_memory_allowed: false,
            project_memory_write_refused: false,
            memory_toolkit_ids: vec![5, 9],
            conversation_unbound: false,
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

    /// Counts the plain and the intent-carrying introspections.
    struct Intents {
        plain: AtomicUsize,
        fresh: AtomicUsize,
    }

    #[async_trait]
    impl Introspector for Intents {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            self.plain.fetch_add(1, Ordering::SeqCst);
            Ok(Some(claim_grant(7, 100_000)))
        }

        async fn introspect_for(
            &self,
            _: &str,
            intent: MemoryIntent,
        ) -> Result<Option<TokenGrant>, IntrospectError> {
            if intent == MemoryIntent::None {
                return self.introspect("").await;
            }
            self.fresh.fetch_add(1, Ordering::SeqCst);
            Ok(Some(claim_grant(7, 100_000)))
        }
    }

    #[tokio::test]
    async fn only_a_memory_intent_asks_main_afresh_and_everything_else_uses_the_cache() {
        let inner = Arc::new(Intents {
            plain: AtomicUsize::new(0),
            fresh: AtomicUsize::new(0),
        });
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        // A toolkit-index request (no intent): one call, then the cache.
        for _ in 0..5 {
            cache.introspect("t").await.expect("ok");
            cache
                .introspect_for("t", MemoryIntent::None)
                .await
                .expect("ok");
        }
        assert_eq!(inner.plain.load(Ordering::SeqCst), 1);
        assert_eq!(inner.fresh.load(Ordering::SeqCst), 0);
        // Personal-scope requests and project writes ask every time and do not
        // disturb the cache.
        for intent in [
            MemoryIntent::PersonalRead,
            MemoryIntent::PersonalWrite,
            MemoryIntent::ProjectWrite,
            MemoryIntent::PersonalRead,
        ] {
            cache.introspect_for("t", intent).await.expect("ok");
        }
        assert_eq!(inner.fresh.load(Ordering::SeqCst), 4);
        cache.introspect("t").await.expect("ok");
        assert_eq!(inner.plain.load(Ordering::SeqCst), 1);
    }

    /// Answers an allowed personal read and counts the calls.
    struct AllowsPersonal(AtomicUsize);

    #[async_trait]
    impl Introspector for AllowsPersonal {
        async fn introspect(&self, _: &str) -> Result<Option<TokenGrant>, IntrospectError> {
            Ok(Some(claim_grant(7, 100_000)))
        }

        async fn introspect_for(
            &self,
            _: &str,
            intent: MemoryIntent,
        ) -> Result<Option<TokenGrant>, IntrospectError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Ok(Some(TokenGrant {
                personal_memory_allowed: intent == MemoryIntent::PersonalRead,
                ..claim_grant(7, 100_000)
            }))
        }
    }

    #[tokio::test]
    async fn an_allowed_personal_read_is_reused_for_a_few_seconds_and_nothing_else_is() {
        let inner = Arc::new(AllowsPersonal(AtomicUsize::new(0)));
        let now = Arc::new(AtomicI64::new(1_000));
        let cache = CachingIntrospector::new(inner.clone(), CachePolicy::default(), clock(&now));
        for _ in 0..5 {
            let grant = cache
                .introspect_for("t", MemoryIntent::PersonalRead)
                .await
                .expect("ok")
                .expect("grant");
            assert!(grant.personal_memory_allowed);
        }
        assert_eq!(
            inner.0.load(Ordering::SeqCst),
            1,
            "a burst of reads asks once"
        );
        // Writes are never reused.
        for intent in [
            MemoryIntent::PersonalWrite,
            MemoryIntent::ProjectWrite,
            MemoryIntent::SharedIndexWrite,
        ] {
            cache.introspect_for("t", intent).await.expect("ok");
        }
        assert_eq!(inner.0.load(Ordering::SeqCst), 4);
        // After the window the next read asks again (a conversation made
        // shared meanwhile is seen then).
        now.fetch_add(PERSONAL_READ_REUSE_SECONDS, Ordering::SeqCst);
        cache
            .introspect_for("t", MemoryIntent::PersonalRead)
            .await
            .expect("ok");
        assert_eq!(inner.0.load(Ordering::SeqCst), 5);
        // Another token is its own entry.
        cache
            .introspect_for("u", MemoryIntent::PersonalRead)
            .await
            .expect("ok");
        assert_eq!(inner.0.load(Ordering::SeqCst), 6);
    }

    #[test]
    fn conversation_unbound_defaults_to_asking_and_holds_only_for_a_claim() {
        let answer = |kind: pb::TokenKind, unbound| pb::IntrospectTokenResponse {
            active: true,
            project_id: 7,
            principal: "user:42".to_owned(),
            kind: kind as i32,
            expires_at_unix: 2_000,
            allowed_sources: vec![pb::Source::ToolkitIndex as i32],
            conversation_unbound: unbound,
            ..pb::IntrospectTokenResponse::default()
        };
        // The zero value (an answer that omits the field) asks.
        let omitted =
            grant_from_response(&answer(pb::TokenKind::WorkerClaim, false), 1_000).expect("grant");
        assert!(!omitted.conversation_unbound);
        assert_eq!(
            MemoryIntent::for_request(
                &Caller::Token(omitted),
                pb::Source::ToolkitIndex as i32,
                0,
                true
            ),
            MemoryIntent::SharedIndexWrite
        );
        assert!(
            grant_from_response(&answer(pb::TokenKind::WorkerClaim, true), 1_000)
                .expect("grant")
                .conversation_unbound
        );
        assert!(
            !grant_from_response(&answer(pb::TokenKind::EngineCallback, true), 1_000)
                .expect("grant")
                .conversation_unbound
        );
    }

    #[test]
    fn a_toolkit_index_write_by_a_worker_claim_is_a_shared_index_write() {
        // An index ingest's or a direct call's claim (no conversation) costs
        // no introspection.
        let unbound = Caller::Token(TokenGrant {
            conversation_unbound: true,
            ..claim_grant(7, 100_000)
        });
        assert_eq!(
            MemoryIntent::for_request(&unbound, pb::Source::ToolkitIndex as i32, 0, true),
            MemoryIntent::None
        );
        // A callback token is never asked: its only source is its provider's,
        // and `toolkit_index` is refused to it by require_source.
        let deepwiki = Caller::Token(grant(7, 100_000));
        assert!(
            deepwiki
                .require_source(pb::Source::ToolkitIndex as i32)
                .is_err()
        );
        let claim = Caller::Token(claim_grant(7, 100_000));
        let callback = Caller::Token(grant(7, 100_000));
        let index = pb::Source::ToolkitIndex as i32;
        assert_eq!(
            MemoryIntent::for_request(&claim, index, 0, true),
            MemoryIntent::SharedIndexWrite
        );
        // A read, or a callback token (no conversation), keeps the cache.
        assert_eq!(
            MemoryIntent::for_request(&claim, index, 0, false),
            MemoryIntent::None
        );
        assert_eq!(
            MemoryIntent::for_request(&callback, index, 0, true),
            MemoryIntent::None
        );
        assert_eq!(
            MemoryIntent::for_request(&callback, pb::Source::Deepwiki as i32, 0, true),
            MemoryIntent::None
        );
        assert_eq!(
            MemoryIntent::for_request(&claim, MEMORY, PROJECT, true),
            MemoryIntent::ProjectWrite
        );
        assert!(MemoryIntent::SharedIndexWrite.is_shared_write());
        assert!(MemoryIntent::ProjectWrite.is_shared_write());
        assert!(!MemoryIntent::PersonalWrite.is_shared_write());
    }

    #[test]
    fn the_intent_of_a_request_follows_its_scope_and_direction() {
        let personal = pb::MemoryScope::Personal as i32;
        let project = pb::MemoryScope::Project as i32;
        assert_eq!(
            MemoryIntent::of(personal, false),
            MemoryIntent::PersonalRead
        );
        assert_eq!(
            MemoryIntent::of(personal, true),
            MemoryIntent::PersonalWrite
        );
        assert_eq!(MemoryIntent::of(project, true), MemoryIntent::ProjectWrite);
        // A project read keeps the claim cache.
        assert_eq!(MemoryIntent::of(project, false), MemoryIntent::None);
        assert_eq!(MemoryIntent::of(0, true), MemoryIntent::None);
    }

    #[test]
    fn an_unknown_source_is_ignored_and_the_known_ones_are_kept() {
        let now = 1_000;
        let response = pb::IntrospectTokenResponse {
            active: true,
            project_id: 7,
            principal: "user:1".to_owned(),
            kind: pb::TokenKind::WorkerClaim as i32,
            expires_at_unix: now + 60,
            // toolkit_index, then a source a future elitea-main might add.
            allowed_sources: vec![pb::Source::ToolkitIndex as i32, 99],
            personal_memory_allowed: false,
            project_memory_write_refused: false,
            memory_toolkit_ids: Vec::new(),
            conversation_unbound: false,
        };
        let grant = grant_from_response(&response, now).expect("kept");
        assert_eq!(grant.sources, vec![pb::Source::ToolkitIndex]);
        assert!(grant.allows(pb::Source::ToolkitIndex));
        // Only unknown sources admit nothing.
        let only_unknown = pb::IntrospectTokenResponse {
            allowed_sources: vec![99, 0],
            ..response.clone()
        };
        assert!(grant_from_response(&only_unknown, now).is_none());
    }

    #[test]
    fn a_project_write_is_refused_after_a_personal_read_and_a_read_is_not() {
        let refused = Caller::Token(TokenGrant {
            sources: vec![pb::Source::Memory],
            project_memory_write_refused: true,
            ..claim_grant(7, 100_000)
        });
        let status = refused
            .memory_binding(MEMORY, PROJECT, 5, true)
            .expect_err("write after a personal read");
        assert_eq!(status.code(), tonic::Code::PermissionDenied);
        assert!(
            status
                .message()
                .starts_with("memory.project_write_after_personal_read")
        );
        assert!(refused.memory_binding(MEMORY, PROJECT, 5, false).is_ok());
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
            personal_memory_allowed: false,
            project_memory_write_refused: false,
            memory_toolkit_ids: Vec::new(),
            conversation_unbound: false,
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
            // Only sources this build cannot name: nothing is left to admit
            // (a known source beside an unknown one is kept; see
            // `an_unknown_source_is_ignored_and_the_known_ones_are_kept`).
            pb::IntrospectTokenResponse {
                allowed_sources: vec![99, 100],
                ..good.clone()
            },
        ] {
            assert!(grant_from_response(&bad, 1_000).is_none(), "{bad:?}");
        }
    }

    fn memory_grant(principal: &str, kind: pb::TokenKind, personal: bool) -> TokenGrant {
        TokenGrant {
            project_id: 7,
            principal: principal.to_owned(),
            kind,
            expires_at_unix: i64::MAX,
            sources: vec![pb::Source::Memory],
            personal_memory_allowed: personal,
            project_memory_write_refused: false,
            memory_toolkit_ids: vec![5, 9],
            conversation_unbound: false,
        }
    }

    /// The owner a binding names, with a fixed toolkit.
    trait OwnerOf {
        fn owner_of(&self, source: i32, scope: i32) -> Result<Option<String>, Status>;
    }

    impl OwnerOf for Caller {
        fn owner_of(&self, source: i32, scope: i32) -> Result<Option<String>, Status> {
            let toolkit = if source == pb::Source::Memory as i32 {
                5
            } else {
                0
            };
            self.memory_binding(source, scope, toolkit, false)
                .map(|binding| binding.map(|binding| binding.owner))
        }
    }

    const MEMORY: i32 = pb::Source::Memory as i32;
    const PERSONAL: i32 = pb::MemoryScope::Personal as i32;
    const PROJECT: i32 = pb::MemoryScope::Project as i32;

    #[test]
    fn the_memory_owner_comes_from_the_token_and_nowhere_else() {
        let claim = |principal: &str, personal| {
            Caller::Token(memory_grant(
                principal,
                pb::TokenKind::WorkerClaim,
                personal,
            ))
        };
        // A user's personal scope is their id; the project scope is the literal.
        assert_eq!(
            claim("user:42", true)
                .owner_of(MEMORY, PERSONAL)
                .expect("owner"),
            Some("user:42".to_owned())
        );
        assert_eq!(
            claim("user:42", true)
                .owner_of(MEMORY, PROJECT)
                .expect("owner"),
            Some("project".to_owned())
        );
        // Not a user id: no personal scope, whatever the principal says.
        for principal in [
            "service:scheduler",
            "user:",
            "user:0",
            "user:-3",
            "user:4x",
            "user:042 ",
            "42",
            "",
        ] {
            let status = claim(principal, true)
                .owner_of(MEMORY, PERSONAL)
                .expect_err(principal);
            assert_eq!(status.code(), tonic::Code::PermissionDenied, "{principal}");
            assert!(
                status.message().starts_with("memory.personal_unavailable"),
                "{principal}"
            );
            // Its project scope is unaffected.
            assert_eq!(
                claim(principal, true)
                    .owner_of(MEMORY, PROJECT)
                    .expect("project"),
                Some("project".to_owned())
            );
        }
        // A claim in a conversation with several people has no personal scope.
        let status = claim("user:42", false)
            .owner_of(MEMORY, PERSONAL)
            .expect_err("shared");
        assert!(
            status
                .message()
                .starts_with("memory.personal_shared_conversation")
        );
        // An engine callback token has no user identity for memory.
        let callback = Caller::Token(memory_grant("user:42", pb::TokenKind::EngineCallback, true));
        assert!(callback.owner_of(MEMORY, PERSONAL).is_err());
        // An administrator has no memory owner at all.
        let admin = Caller::Admin("dns:elitea-main".to_owned());
        for scope in [PERSONAL, PROJECT, 0] {
            assert_eq!(
                admin.owner_of(MEMORY, scope).expect_err("admin").code(),
                tonic::Code::PermissionDenied
            );
        }
        // The scope is required for memory and refused for every other source.
        assert_eq!(
            claim("user:42", true)
                .owner_of(MEMORY, 0)
                .expect_err("required")
                .code(),
            tonic::Code::InvalidArgument
        );
        assert_eq!(
            claim("user:42", true)
                .owner_of(MEMORY, 9)
                .expect_err("unknown")
                .code(),
            tonic::Code::InvalidArgument
        );
        for source in [pb::Source::ToolkitIndex as i32, pb::Source::Deepwiki as i32] {
            assert_eq!(
                claim("user:42", true).owner_of(source, 0).expect("none"),
                None
            );
            assert_eq!(
                claim("user:42", true)
                    .owner_of(source, PROJECT)
                    .expect_err("scope")
                    .code(),
                tonic::Code::InvalidArgument
            );
        }
    }

    #[test]
    fn a_memory_binding_names_the_toolkit_and_requires_it() {
        let claim = Caller::Token(memory_grant("user:42", pb::TokenKind::WorkerClaim, true));
        let binding = claim
            .memory_binding(MEMORY, PROJECT, 9, false)
            .expect("binding")
            .expect("some");
        assert_eq!(binding.toolkit_id, 9);
        assert_eq!(binding.owner, "project");
        for toolkit in [0, -1] {
            assert_eq!(
                claim
                    .memory_binding(MEMORY, PROJECT, toolkit, false)
                    .expect_err("toolkit")
                    .code(),
                tonic::Code::InvalidArgument
            );
        }
        // Another source takes no toolkit.
        assert_eq!(
            claim
                .memory_binding(pb::Source::ToolkitIndex as i32, 0, 9, false)
                .expect_err("toolkit")
                .code(),
            tonic::Code::InvalidArgument
        );
        // A toolkit the run was not given (another toolkit of the project, or
        // a toolkit id of another project) is refused, in both scopes.
        for scope in [PROJECT, PERSONAL] {
            let status = claim
                .memory_binding(MEMORY, scope, 77, false)
                .expect_err("not the run's toolkit");
            assert_eq!(status.code(), tonic::Code::PermissionDenied);
            assert!(
                status.message().starts_with("memory.toolkit_not_in_run"),
                "{}",
                status.message()
            );
        }
    }

    #[test]
    fn the_precheck_refuses_fixed_reasons_and_leaves_the_per_moment_ones_to_main() {
        // A cached grant never carries the personal permission or the
        // write-after-read refusal; the precheck does not judge them.
        let cached = Caller::Token(TokenGrant {
            project_memory_write_refused: true,
            ..memory_grant("user:42", pb::TokenKind::WorkerClaim, false)
        });
        assert!(cached.precheck_memory(MEMORY, PERSONAL, 5).is_ok());
        assert!(cached.precheck_memory(MEMORY, PROJECT, 5).is_ok());
        // Fixed reasons are refused before elitea-main is asked (and so
        // before a personal read could be recorded).
        for (toolkit, scope, code) in [
            (77, PERSONAL, tonic::Code::PermissionDenied),
            (0, PERSONAL, tonic::Code::InvalidArgument),
            (5, 0, tonic::Code::InvalidArgument),
        ] {
            assert_eq!(
                cached
                    .precheck_memory(MEMORY, scope, toolkit)
                    .expect_err("fixed")
                    .code(),
                code
            );
        }
        let no_user = Caller::Token(memory_grant("service", pb::TokenKind::WorkerClaim, true));
        assert_eq!(
            no_user
                .precheck_memory(MEMORY, PERSONAL, 5)
                .expect_err("no user")
                .code(),
            tonic::Code::PermissionDenied
        );
    }

    #[test]
    fn the_run_s_memory_toolkits_survive_introspection_only_for_a_worker_claim() {
        let answer = |kind: pb::TokenKind| pb::IntrospectTokenResponse {
            active: true,
            project_id: 7,
            principal: "user:42".to_owned(),
            kind: kind as i32,
            expires_at_unix: 2_000,
            allowed_sources: vec![pb::Source::Memory as i32],
            personal_memory_allowed: false,
            project_memory_write_refused: false,
            memory_toolkit_ids: vec![3, 0, -4, 8],
            conversation_unbound: false,
        };
        assert_eq!(
            grant_from_response(&answer(pb::TokenKind::WorkerClaim), 1_000)
                .expect("claim")
                .memory_toolkit_ids,
            vec![3, 8]
        );
        assert!(
            grant_from_response(&answer(pb::TokenKind::EngineCallback), 1_000)
                .expect("callback")
                .memory_toolkit_ids
                .is_empty()
        );
    }

    #[test]
    fn personal_memory_survives_introspection_only_for_a_worker_claim() {
        let answer = |kind: pb::TokenKind, personal| pb::IntrospectTokenResponse {
            active: true,
            project_id: 7,
            principal: "user:42".to_owned(),
            kind: kind as i32,
            expires_at_unix: 2_000,
            allowed_sources: vec![pb::Source::Memory as i32],
            personal_memory_allowed: personal,
            project_memory_write_refused: false,
            memory_toolkit_ids: Vec::new(),
            conversation_unbound: false,
        };
        assert!(
            grant_from_response(&answer(pb::TokenKind::WorkerClaim, true), 1_000)
                .expect("claim")
                .personal_memory_allowed
        );
        assert!(
            !grant_from_response(&answer(pb::TokenKind::WorkerClaim, false), 1_000)
                .expect("claim")
                .personal_memory_allowed
        );
        // A callback token is never given the personal scope, even if the
        // answer says so.
        assert!(
            !grant_from_response(&answer(pb::TokenKind::EngineCallback, true), 1_000)
                .expect("callback")
                .personal_memory_allowed
        );
    }
}
