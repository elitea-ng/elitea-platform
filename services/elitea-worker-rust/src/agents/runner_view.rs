//! FU-287: the Runner's refreshed model view, read incrementally.
//!
//! After every persisted event the vendored Runner replaces its in-memory
//! history with the session's complete model view. A full read parses every
//! stored payload (superseded recovery markers included) in every refresh,
//! so a run's refresh cost grew with the whole stored history.
//!
//! [`IncrementalRunnerView`] keeps the *active* rows of one root session in
//! memory, together with the cursor of the last committed ordinal it saw. A
//! refresh reads only the rows after the cursor, in one short transaction
//! under the shared writer lock (`PostgresSessionService::events_since`), and
//! applies the active-history rules to them. The ordinals are gap-free and
//! committed in order, so the head proves the read was complete.
//!
//! The row-local classification (recovery marker, branch, snapshot agent,
//! control, author) is computed by the same SQL as the full read and is never
//! recomputed here. Only the global retirement rules of
//! `state/postgres_session_active_events.sql` are mirrored, including SQL's
//! three-valued `NOT IN` for a NULL author. `runner_view_pg_tests` proves the
//! two agree on generated event sequences.
//!
//! The view re-reads in full (a *fallback*) whenever a new row could bring
//! back a row it already dropped, when the rows do not continue the cursor,
//! or when the cached view would pass the full read's bounds. The cache is
//! per run and in memory; a new process starts from a full read.

use std::collections::HashMap;
use std::sync::Arc;
#[cfg(test)]
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};

use adk_runner::SessionEventRefresh;
use adk_rust::session::GetRequest;
use adk_rust::{AdkIdentity, Event};
use async_trait::async_trait;
use serde_json::Value;

use crate::state::{
    ClassifiedEvent, PostgresSessionError, PostgresSessionService, refresh_span, session_read_error,
};

/// Counters of one view, read by tests. Production reads the span fields.
#[cfg(test)]
#[derive(Debug, Default)]
pub(crate) struct RefreshStats {
    /// Refreshes served.
    pub(crate) refreshes: AtomicU64,
    /// Full reads which started a cache (the first read, and each fallback).
    pub(crate) seeds: AtomicU64,
    /// Full reads forced by a row which could bring back a dropped one, a gap,
    /// or the bounds. The first read is not a fallback.
    pub(crate) fallbacks: AtomicU64,
    /// Rows decoded from storage, by refreshes and seeds.
    pub(crate) rows_read: AtomicU64,
}

#[cfg(test)]
impl RefreshStats {
    pub(crate) fn snapshot(&self) -> RefreshTally {
        RefreshTally {
            refreshes: self.refreshes.load(Ordering::Relaxed),
            seeds: self.seeds.load(Ordering::Relaxed),
            fallbacks: self.fallbacks.load(Ordering::Relaxed),
            rows_read: self.rows_read.load(Ordering::Relaxed),
        }
    }
}

#[cfg(test)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct RefreshTally {
    pub(crate) refreshes: u64,
    pub(crate) seeds: u64,
    pub(crate) fallbacks: u64,
    pub(crate) rows_read: u64,
}

/// Test-only: the stats of every view built over a database, by its name, so a
/// run reads its own views and no other test's.
#[cfg(test)]
static VIEWS_BY_DATABASE: std::sync::Mutex<Vec<(String, Arc<RefreshStats>)>> =
    std::sync::Mutex::new(Vec::new());

/// Test-only: the summed stats of the views built over `database`.
#[cfg(test)]
pub(crate) fn stats_for_database(database: &str) -> RefreshTally {
    let views = VIEWS_BY_DATABASE.lock().expect("view registry");
    views
        .iter()
        .filter(|(name, _)| name == database)
        .map(|(_, stats)| stats.snapshot())
        .fold(RefreshTally::default(), |sum, tally| RefreshTally {
            refreshes: sum.refreshes + tally.refreshes,
            seeds: sum.seeds + tally.seeds,
            fallbacks: sum.fallbacks + tally.fallbacks,
            rows_read: sum.rows_read + tally.rows_read,
        })
}

/// The global facts the retirement rules read, mirrored from
/// `postgres_session_active_events.sql`: the latest history snapshot, whether
/// any row has a branch, and the latest recovery marker of each branch.
#[derive(Clone, Default)]
struct ViewFacts {
    /// The latest history snapshot marker: its ordinal and agent.
    snapshot: Option<(i64, String)>,
    /// Any stored row has a branch.
    has_branches: bool,
    /// The latest recovery marker ordinal of each branch.
    markers: HashMap<String, i64>,
}

impl ViewFacts {
    /// Take in one row, in ascending ordinal order or not.
    fn absorb(&mut self, row: &ClassifiedEvent) {
        self.has_branches |= !row.branch.is_empty();
        if row.recovery_marker {
            let latest = self
                .markers
                .entry(row.branch.clone())
                .or_insert(row.ordinal);
            *latest = (*latest).max(row.ordinal);
            if let Some(agent) = &row.snapshot_agent
                && self
                    .snapshot
                    .as_ref()
                    .is_none_or(|(ordinal, _)| *ordinal < row.ordinal)
            {
                self.snapshot = Some((row.ordinal, agent.clone()));
            }
        }
    }

    /// The facts of a set of rows. A seed (the active view) gives the facts of
    /// every stored row: its snapshot marker is always active, each branch's
    /// latest marker is active, and when any row has a branch the view holds a
    /// branch row (one is either active itself or superseded by an active
    /// marker of its branch).
    fn from_rows(rows: &[ClassifiedEvent]) -> Self {
        let mut facts = Self::default();
        for row in rows {
            facts.absorb(row);
        }
        facts
    }

    /// The `active_events` rule for one row (read path: no candidate).
    fn is_active(&self, row: &ClassifiedEvent) -> bool {
        let snapshot = self.snapshot.as_ref();
        if row.recovery_marker {
            return self.markers.get(&row.branch) == Some(&row.ordinal)
                || snapshot.is_some_and(|(ordinal, _)| *ordinal == row.ordinal);
        }
        // `author NOT IN ('user', snapshot_agent)` is NULL, not true, for a
        // NULL author: such a row is kept only by the other terms.
        self.has_branches
            || snapshot.is_none()
            || row.has_control
            || snapshot.is_some_and(|(ordinal, agent)| {
                row.ordinal > *ordinal
                    || row
                        .author
                        .as_deref()
                        .is_some_and(|author| author != "user" && author != agent)
            })
    }
}

/// What the cached view is made of: the active rows and the global facts the
/// retirement rules read.
struct ViewCache {
    /// The highest committed ordinal this cache has applied.
    cursor: i64,
    /// Active rows in ascending ordinal order.
    rows: Vec<ClassifiedEvent>,
    facts: ViewFacts,
    /// The session state under the compaction key.
    compaction_record: Option<Value>,
    /// Payload bytes of the cached rows.
    bytes: usize,
}

enum Extension {
    Applied,
    /// The new rows may bring back a row the cache dropped, or do not
    /// continue the cursor: only a full read is exact.
    FullRead,
}

impl ViewCache {
    fn from_seed(head: i64, rows: Vec<ClassifiedEvent>, record: Option<Value>) -> Self {
        let bytes = rows
            .iter()
            .fold(0_usize, |sum, row| sum.saturating_add(row.payload_bytes));
        Self {
            cursor: head - 1,
            facts: ViewFacts::from_rows(&rows),
            rows,
            compaction_record: record,
            bytes,
        }
    }

    /// Apply rows committed after the cursor. On `FullRead` the cache is
    /// unchanged.
    fn extend(
        &mut self,
        head: i64,
        new_rows: Vec<ClassifiedEvent>,
        compaction_key: &str,
    ) -> Extension {
        let continues = i64::try_from(new_rows.len())
            .is_ok_and(|count| self.cursor.checked_add(count) == head.checked_sub(1))
            && new_rows
                .iter()
                .zip(self.cursor + 1..)
                .all(|(row, expected)| row.ordinal == expected);
        if !continues {
            return Extension::FullRead;
        }
        let mut facts = self.facts.clone();
        for row in &new_rows {
            facts.absorb(row);
        }
        // A row dropped behind a snapshot cut returns when a branch appears
        // or when a snapshot of another agent moves the cut. Dropped rows are
        // not kept, so only a full read can bring them back.
        let brings_back = self.facts.snapshot.as_ref().is_some_and(|(_, agent)| {
            (!self.facts.has_branches && facts.has_branches)
                || facts
                    .snapshot
                    .as_ref()
                    .is_none_or(|(_, latest)| latest != agent)
        });
        if brings_back {
            return Extension::FullRead;
        }
        for row in &new_rows {
            if let Some(value) = row.event.actions.state_delta.get(compaction_key) {
                self.compaction_record = Some(value.clone());
            }
        }
        self.rows.extend(new_rows);
        self.facts = facts;
        let facts = &self.facts;
        self.rows.retain(|row| facts.is_active(row));
        self.bytes = self
            .rows
            .iter()
            .fold(0_usize, |sum, row| sum.saturating_add(row.payload_bytes));
        self.cursor = head - 1;
        Extension::Applied
    }
}

/// The cache of one run, and whether the view gave up caching.
#[derive(Default)]
struct ViewState {
    cache: Option<ViewCache>,
    /// The active view passed the retained-byte bound, so nothing stays
    /// resident and the refresh reads in full, as before the cache. The next
    /// refresh looks at the active bytes again (without decoding them while
    /// they are still over) and caches the view once it fits.
    oversized: bool,
}

/// The refreshed model view of one root session, read incrementally.
pub(crate) struct IncrementalRunnerView {
    sessions: Arc<PostgresSessionService>,
    identity: AdkIdentity,
    /// The history scope of the root agent, as `RunnerSessionService` has it.
    scope: Option<([u8; 32], String)>,
    state: tokio::sync::Mutex<ViewState>,
    #[cfg(test)]
    stats: Arc<RefreshStats>,
}

impl IncrementalRunnerView {
    pub(crate) fn new(
        sessions: Arc<PostgresSessionService>,
        identity: AdkIdentity,
        scope: Option<([u8; 32], String)>,
    ) -> Self {
        #[cfg(test)]
        let stats = Arc::new(RefreshStats::default());
        #[cfg(test)]
        if let Some(database) = sessions.database_name() {
            VIEWS_BY_DATABASE
                .lock()
                .expect("view registry")
                .push((database, Arc::clone(&stats)));
        }
        Self {
            sessions,
            identity,
            scope,
            state: tokio::sync::Mutex::new(ViewState::default()),
            #[cfg(test)]
            stats,
        }
    }

    fn scope(&self) -> Option<([u8; 32], &str)> {
        self.scope
            .as_ref()
            .map(|(digest, agent)| (*digest, agent.as_str()))
    }

    fn project(&self, cache: &ViewCache) -> adk_rust::Result<Vec<Event>> {
        let mut events = cache
            .rows
            .iter()
            .map(|row| row.event.clone())
            .collect::<Vec<_>>();
        super::runner_history::project_events(
            &mut events,
            cache.compaction_record.clone(),
            self.scope(),
        )?;
        Ok(events)
    }

    /// The view exactly as `RunnerSessionService::get` serves it.
    async fn full_read(&self) -> adk_rust::Result<Vec<Event>> {
        let session = super::runner_history::read(
            self.sessions.as_ref(),
            GetRequest {
                app_name: self.identity.app_name.to_string(),
                user_id: self.identity.user_id.to_string(),
                session_id: self.identity.session_id.to_string(),
                num_recent_events: None,
                after: None,
            },
            self.scope(),
        )
        .await?;
        Ok(session.events().all())
    }

    async fn refresh(&self, span: &tracing::Span) -> adk_rust::Result<Vec<Event>> {
        let mut state = self.state.lock().await;
        let mut rows_read = 0_u64;
        let mut fallback = false;
        let result = self
            .refresh_locked(&mut state, span, &mut rows_read, &mut fallback)
            .await;
        span.record("rows_read", rows_read);
        span.record("fallback", fallback);
        #[cfg(test)]
        {
            self.stats.refreshes.fetch_add(1, Ordering::Relaxed);
            self.stats.rows_read.fetch_add(rows_read, Ordering::Relaxed);
            if fallback {
                self.stats.fallbacks.fetch_add(1, Ordering::Relaxed);
            }
        }
        drop(state);
        result
    }

    async fn refresh_locked(
        &self,
        state: &mut ViewState,
        span: &tracing::Span,
        rows_read: &mut u64,
        fallback: &mut bool,
    ) -> adk_rust::Result<Vec<Event>> {
        let limits = self.sessions.limits();
        if let Some(cache) = state.cache.as_mut() {
            let tail = self.sessions.events_since(span, cache.cursor).await?;
            *rows_read += count(tail.rows.len());
            if !tail.too_large && tail.head - 1 == cache.cursor && tail.rows.is_empty() {
                return self.project(cache);
            }
            if !tail.too_large
                && matches!(
                    cache.extend(tail.head, tail.rows, super::context_compaction::STATE_KEY),
                    Extension::Applied
                )
            {
                if cache.rows.len() <= limits.max_events
                    && cache.bytes <= limits.max_retained_event_bytes
                {
                    return self.project(cache);
                }
                // The cache passed the bounds: read in full at once. The full
                // read has the typed count limit and no byte bound, and the
                // next refresh looks again at whether the view fits.
                state.oversized = cache.bytes > limits.max_retained_event_bytes;
                state.cache = None;
                *fallback = true;
                return self.full_read().await;
            }
            // Dropping the cache before the read keeps a failed read from
            // leaving a stale cache behind.
            state.cache = None;
            *fallback = true;
        }
        *fallback |= state.oversized;
        let seed = self
            .sessions
            .seed_active(span, super::context_compaction::STATE_KEY)
            .await?;
        *rows_read += count(seed.rows.len());
        #[cfg(test)]
        self.stats.seeds.fetch_add(1, Ordering::Relaxed);
        if seed.too_large {
            // The full read has no byte bound; keep nothing resident.
            state.oversized = true;
            *fallback = true;
            return self.full_read().await;
        }
        state.oversized = false;
        let cache = ViewCache::from_seed(seed.head, seed.rows, seed.state_value);
        let events = self.project(&cache)?;
        state.cache = Some(cache);
        Ok(events)
    }
}

fn count(rows: usize) -> u64 {
    u64::try_from(rows).unwrap_or(u64::MAX)
}

/// A failure of the test-only comparison, never of a run.
#[cfg(test)]
fn cross_check_error(message: &'static str) -> adk_rust::AdkError {
    adk_rust::AdkError::new(
        adk_rust::ErrorComponent::Session,
        adk_rust::ErrorCategory::Internal,
        "session.refresh_cross_check",
        message,
    )
}

#[async_trait]
impl SessionEventRefresh for IncrementalRunnerView {
    async fn refreshed_events(&self, identity: &AdkIdentity) -> adk_rust::Result<Vec<Event>> {
        // One view serves one session. Any other identity fails closed.
        if *identity != self.identity {
            return Err(session_read_error(PostgresSessionError::InvalidScope));
        }
        let span = refresh_span();
        let events = self.refresh(&span).await?;
        #[cfg(test)]
        if cross_check_enabled() {
            self.cross_check(&events).await?;
        }
        Ok(events)
    }
}

#[cfg(test)]
static CROSS_CHECK: AtomicBool = AtomicBool::new(true);

/// Test-only: whether every refresh also runs the full read and compares.
/// On by default for the whole test build, so every PostgreSQL suite which
/// drives a Runner proves the incremental view equals the full one.
#[cfg(test)]
pub(crate) fn cross_check_enabled() -> bool {
    CROSS_CHECK.load(Ordering::Relaxed)
}

/// Test-only: a timing measurement turns the extra full reads off. The
/// returned guard turns them back on when it drops.
#[cfg(test)]
pub(crate) fn disable_cross_check() -> CrossCheckOff {
    CrossCheckOff(CROSS_CHECK.swap(false, Ordering::Relaxed))
}

/// Restores the cross-check flag it found.
#[cfg(test)]
pub(crate) struct CrossCheckOff(bool);

#[cfg(test)]
impl Drop for CrossCheckOff {
    fn drop(&mut self) {
        CROSS_CHECK.store(self.0, Ordering::Relaxed);
    }
}

#[cfg(test)]
impl IncrementalRunnerView {
    pub(crate) fn stats(&self) -> Arc<RefreshStats> {
        Arc::clone(&self.stats)
    }

    /// Whether the view has given up caching for the byte bound.
    pub(crate) async fn is_oversized(&self) -> bool {
        self.state.lock().await.oversized
    }

    /// The compaction record the cache holds.
    pub(crate) async fn compaction_record(&self) -> Option<Value> {
        let state = self.state.lock().await;
        state
            .cache
            .as_ref()
            .and_then(|cache| cache.compaction_record.clone())
    }

    /// The view as `RunnerSessionService::get` serves it.
    pub(crate) async fn full_read_for_tests(&self) -> adk_rust::Result<Vec<Event>> {
        self.full_read().await
    }

    /// The cached active rows after the last refresh, for equivalence tests.
    pub(crate) async fn cached_events(&self) -> Option<Vec<Event>> {
        let state = self.state.lock().await;
        state
            .cache
            .as_ref()
            .map(|cache| cache.rows.iter().map(|row| row.event.clone()).collect())
    }

    /// Run one refresh outside a Runner.
    pub(crate) async fn refresh_for_tests(&self) -> adk_rust::Result<Vec<Event>> {
        self.refreshed_events(&self.identity.clone()).await
    }

    async fn committed_head(&self, span: &tracing::Span) -> adk_rust::Result<i64> {
        self.sessions
            .events_since(span, i64::MAX)
            .await
            .map(|tail| tail.head)
    }

    /// Compare a refreshed view with a full read, unless a writer appended
    /// between the two. Another writer's append is not a divergence.
    async fn cross_check(&self, refreshed: &[Event]) -> adk_rust::Result<()> {
        // Not a refresh: the head reads carry no refresh span.
        let span = tracing::Span::none();
        let before = self.committed_head(&span).await?;
        let full = self.full_read().await?;
        let after = self.committed_head(&span).await?;
        let cursor = {
            let state = self.state.lock().await;
            state.cache.as_ref().map(|cache| cache.cursor)
        };
        if before != after || cursor != Some(before - 1) {
            return Ok(());
        }
        let encode = |events: &[Event]| {
            events
                .iter()
                .map(|event| {
                    serde_json::to_value(event)
                        .map_err(|_| cross_check_error("an event cannot be encoded"))
                })
                .collect::<Result<Vec<_>, _>>()
        };
        if encode(refreshed)? != encode(&full)? {
            eprintln!(
                "FU-287 cross-check: the incremental view ({} events) differs from the full read ({} events)",
                refreshed.len(),
                full.len()
            );
            return Err(cross_check_error(
                "the incremental session view differs from the full read",
            ));
        }
        Ok(())
    }
}

/// Test-only: the ordinals the mirrored retirement rules keep, given every
/// stored row. The SQL `active_events` rules are the oracle.
#[cfg(test)]
pub(crate) fn mirrored_active_ordinals(all: &[ClassifiedEvent]) -> Vec<i64> {
    let facts = ViewFacts::from_rows(all);
    all.iter()
        .filter(|row| facts.is_active(row))
        .map(|row| row.ordinal)
        .collect()
}
