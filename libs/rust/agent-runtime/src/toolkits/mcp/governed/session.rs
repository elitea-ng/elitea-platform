//! Governed MCP session reuse (#1474).
//!
//! Each governed tool call still redeems `mcp.tool.call` on its own: Main
//! decides and audits every call. The session that carries the call is
//! kept for reuse, under three rules:
//!
//! - One slot belongs to one governed server of one run build. A session in
//!   a slot is never used for another server, and its token is never sent to
//!   another server.
//! - A session is reused only for the SAME token generation: the SHA-256 of
//!   the access token the call redeemed must equal the digest of the token
//!   the session was opened with. A new token opens a new session and drops
//!   the old one.
//! - A session never outlives its redeemed token: it is used only while the
//!   latest redemption of its token is not expired, and a timer drops it at
//!   that expiry. A call that fails drops the session too, so the next call
//!   dials afresh.
//!
//! A call already in flight keeps the session until it returns.
//!
//! No lock is held across a dial. The slot holds one cell per token
//! generation; the first call of a generation dials and lists in that cell,
//! and calls of the same generation wait for that one dial only. A call
//! with another token replaces the cell at once and dials in parallel, so a
//! slow dial never blocks it (#1474 review).

use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex, PoisonError, Weak};
use std::time::Duration;

use adk_core::Tool;
use ring::digest;

use super::super::{McpSession, RemoteMcpConfig};
use crate::toolkits::governed::client::Clock;

/// The SHA-256 of one access token: the session's token generation.
pub(super) type TokenGeneration = [u8; 32];

/// The token generation of `token`.
pub(super) fn token_generation(token: &str) -> TokenGeneration {
    let mut out = [0_u8; 32];
    out.copy_from_slice(digest::digest(&digest::SHA256, token.as_bytes()).as_ref());
    out
}

/// One open session, its token generation and the tools listed on it.
pub(super) struct CachedSession {
    generation: TokenGeneration,
    /// The expiry of the latest redemption of this session's token.
    not_after_unix_millis: AtomicI64,
    tools: Vec<Arc<dyn Tool>>,
    /// Held so the connection stays open; dropped with the entry.
    _session: McpSession,
    /// Holds the token in zeroizing memory; dropped with the entry.
    config: RemoteMcpConfig,
}

impl CachedSession {
    pub(super) fn new(
        generation: TokenGeneration,
        not_after_unix_millis: i64,
        tools: Vec<Arc<dyn Tool>>,
        session: McpSession,
        config: RemoteMcpConfig,
    ) -> Self {
        Self {
            generation,
            not_after_unix_millis: AtomicI64::new(not_after_unix_millis),
            tools,
            _session: session,
            config,
        }
    }

    pub(super) fn tool(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.tools.iter().find(|tool| tool.name() == name).cloned()
    }

    pub(super) const fn config(&self) -> &RemoteMcpConfig {
        &self.config
    }

    fn usable(&self, generation: &TokenGeneration, now_unix_millis: i64) -> bool {
        self.generation == *generation
            && self.not_after_unix_millis.load(Ordering::Acquire) > now_unix_millis
    }
}

/// The cell of one token generation: empty while its session is dialled,
/// set once it is open.
struct GenerationCell {
    generation: TokenGeneration,
    cell: Arc<tokio::sync::OnceCell<Arc<CachedSession>>>,
}

/// The reusable session of one governed server.
#[derive(Default)]
pub(super) struct SessionSlot {
    /// Held only to read or swap the cell; never across an `.await`.
    entry: Mutex<Option<GenerationCell>>,
}

impl SessionSlot {
    fn lock(&self) -> std::sync::MutexGuard<'_, Option<GenerationCell>> {
        self.entry.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The cell for `generation`: the current one when it is for the same
    /// generation and its session (if open) is still usable, else a new
    /// empty cell. Replacing a cell drops the slot's hold on the old session.
    fn cell_for(
        &self,
        generation: &TokenGeneration,
        now_unix_millis: i64,
    ) -> Arc<tokio::sync::OnceCell<Arc<CachedSession>>> {
        let mut entry = self.lock();
        if let Some(current) = entry.as_ref()
            && current.generation == *generation
            && current
                .cell
                .get()
                .is_none_or(|session| session.usable(generation, now_unix_millis))
        {
            return Arc::clone(&current.cell);
        }
        let cell = Arc::new(tokio::sync::OnceCell::new());
        *entry = Some(GenerationCell {
            generation: *generation,
            cell: Arc::clone(&cell),
        });
        cell
    }

    /// Drop `cell` when it is still the slot's and holds no session (its
    /// dial failed).
    fn forget_empty(&self, cell: &Arc<tokio::sync::OnceCell<Arc<CachedSession>>>) {
        let mut entry = self.lock();
        if entry
            .as_ref()
            .is_some_and(|current| Arc::ptr_eq(&current.cell, cell) && current.cell.get().is_none())
        {
            *entry = None;
        }
    }

    /// The session for `generation`: the cached one when it is still usable,
    /// else the one `open` builds. A cached session of another generation, or
    /// an expired one, is dropped first. Calls of one generation share one
    /// dial; calls of another generation do not wait for it. A failed `open`
    /// leaves the slot empty.
    ///
    /// # Errors
    ///
    /// The error of `open`.
    pub(super) async fn session<E, F, Fut>(
        self: &Arc<Self>,
        generation: TokenGeneration,
        not_after_unix_millis: i64,
        clock: Clock,
        open: F,
    ) -> Result<Arc<CachedSession>, E>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<CachedSession, E>>,
    {
        let cell = self.cell_for(&generation, clock());
        let mut opened_here = false;
        let outcome = cell
            .get_or_try_init(|| {
                opened_here = true;
                async move { open().await.map(Arc::new) }
            })
            .await;
        let session = match outcome {
            Ok(session) => Arc::clone(session),
            Err(error) => {
                self.forget_empty(&cell);
                return Err(error);
            }
        };
        if opened_here {
            self.expire_at_not_after(&session, clock);
        } else {
            // A session of the same token: this call's redemption is the
            // latest one, so it sets the expiry.
            session
                .not_after_unix_millis
                .fetch_max(not_after_unix_millis, Ordering::AcqRel);
        }
        Ok(session)
    }

    /// Drop `session` when it is still the cached one (a failed call).
    pub(super) fn evict(&self, session: &Arc<CachedSession>) {
        let mut entry = self.lock();
        if entry.as_ref().is_some_and(|current| {
            current
                .cell
                .get()
                .is_some_and(|cached| Arc::ptr_eq(cached, session))
        }) {
            *entry = None;
        }
    }

    /// A timer that drops `session` when its latest redemption expires. It
    /// holds no strong reference: a slot or session dropped first ends it.
    fn expire_at_not_after(self: &Arc<Self>, session: &Arc<CachedSession>, clock: Clock) {
        let Ok(runtime) = tokio::runtime::Handle::try_current() else {
            return;
        };
        let slot: Weak<Self> = Arc::downgrade(self);
        let watched: Weak<CachedSession> = Arc::downgrade(session);
        runtime.spawn(async move {
            loop {
                let Some(session) = watched.upgrade() else {
                    return;
                };
                let remaining = session
                    .not_after_unix_millis
                    .load(Ordering::Acquire)
                    .saturating_sub(clock());
                if remaining <= 0 {
                    if let Some(slot) = slot.upgrade() {
                        slot.evict(&session);
                    }
                    return;
                }
                drop(session);
                let wait = u64::try_from(remaining).unwrap_or(u64::MAX);
                tokio::time::sleep(Duration::from_millis(wait)).await;
            }
        });
    }
}
