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
use std::sync::atomic::AtomicBool;
use std::sync::atomic::{AtomicU64, Ordering};

use adk_runner::SessionEventRefresh;
use adk_rust::session::{GetRequest, SessionService as _};
use adk_rust::{AdkError, AdkIdentity, ErrorCategory, ErrorComponent, Event};
use async_trait::async_trait;
use serde_json::Value;

use crate::state::{ClassifiedEvent, PostgresSessionService, refresh_span};

/// Counters of one view, readable by tests and by the run's owner.
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

/// What the cached view is made of: the active rows and the global facts the
/// retirement rules read.
struct ViewCache {
    /// The highest committed ordinal this cache has applied.
    cursor: i64,
    /// Active rows in ascending ordinal order.
    rows: Vec<ClassifiedEvent>,
    /// The latest history snapshot marker: its ordinal and agent.
    snapshot: Option<(i64, String)>,
    /// Any stored row has a branch.
    has_branches: bool,
    /// The latest recovery marker ordinal of each branch.
    markers: HashMap<String, i64>,
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
        // The seed is the active view. Its snapshot marker is always active,
        // each branch's latest marker is active, and when any row has a
        // branch the view holds a branch row (a branch row is either active
        // itself or superseded by an active marker of its branch).
        let mut snapshot: Option<(i64, String)> = None;
        let mut markers: HashMap<String, i64> = HashMap::new();
        let mut has_branches = false;
        let mut bytes = 0_usize;
        for row in &rows {
            bytes = bytes.saturating_add(row.payload_bytes);
            has_branches |= !row.branch.is_empty();
            if row.recovery_marker {
                let latest = markers.entry(row.branch.clone()).or_insert(row.ordinal);
                *latest = (*latest).max(row.ordinal);
                if let Some(agent) = &row.snapshot_agent
                    && snapshot
                        .as_ref()
                        .is_none_or(|(ordinal, _)| *ordinal < row.ordinal)
                {
                    snapshot = Some((row.ordinal, agent.clone()));
                }
            }
        }
        Self {
            cursor: head - 1,
            rows,
            snapshot,
            has_branches,
            markers,
            compaction_record: record,
            bytes,
        }
    }

    /// The `active_events` rule for one row (read path: no candidate).
    fn is_active(
        row: &ClassifiedEvent,
        snapshot: Option<&(i64, String)>,
        has_branches: bool,
        markers: &HashMap<String, i64>,
    ) -> bool {
        if row.recovery_marker {
            return markers.get(&row.branch) == Some(&row.ordinal)
                || snapshot.is_some_and(|(ordinal, _)| *ordinal == row.ordinal);
        }
        // `author NOT IN ('user', snapshot_agent)` is NULL, not true, for a
        // NULL author: such a row is kept only by the other terms.
        has_branches
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
        let mut snapshot = self.snapshot.clone();
        let mut has_branches = self.has_branches;
        let mut markers = self.markers.clone();
        for row in &new_rows {
            has_branches |= !row.branch.is_empty();
            if row.recovery_marker {
                markers.insert(row.branch.clone(), row.ordinal);
                if let Some(agent) = &row.snapshot_agent {
                    snapshot = Some((row.ordinal, agent.clone()));
                }
            }
        }
        // A row dropped behind a snapshot cut returns when a branch appears
        // or when a snapshot of another agent moves the cut. Dropped rows are
        // not kept, so only a full read can bring them back.
        let brings_back = self.snapshot.as_ref().is_some_and(|(_, agent)| {
            (!self.has_branches && has_branches)
                || snapshot.as_ref().is_none_or(|(_, latest)| latest != agent)
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
        self.snapshot = snapshot;
        self.has_branches = has_branches;
        self.markers = markers;
        let (snapshot, has_branches, markers) = (&self.snapshot, self.has_branches, &self.markers);
        self.rows
            .retain(|row| Self::is_active(row, snapshot.as_ref(), has_branches, markers));
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
    /// The active view passed the retained-byte bound: every refresh reads in
    /// full, as it did before the cache, and nothing stays resident.
    oversized: bool,
}

/// The refreshed model view of one root session, read incrementally.
pub(crate) struct IncrementalRunnerView {
    sessions: Arc<PostgresSessionService>,
    identity: AdkIdentity,
    /// The history scope of the root agent, as `RunnerSessionService` has it.
    scope: Option<([u8; 32], String)>,
    state: tokio::sync::Mutex<ViewState>,
    stats: Arc<RefreshStats>,
}

impl IncrementalRunnerView {
    pub(crate) fn new(
        sessions: Arc<PostgresSessionService>,
        identity: AdkIdentity,
        scope: Option<([u8; 32], String)>,
    ) -> Self {
        Self {
            sessions,
            identity,
            scope,
            state: tokio::sync::Mutex::new(ViewState::default()),
            stats: Arc::new(RefreshStats::default()),
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
        let session = self
            .sessions
            .get(GetRequest {
                app_name: self.identity.app_name.to_string(),
                user_id: self.identity.user_id.to_string(),
                session_id: self.identity.session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await?;
        let session = super::runner_history::project(session, self.scope())?;
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
        self.stats.refreshes.fetch_add(1, Ordering::Relaxed);
        self.stats.rows_read.fetch_add(rows_read, Ordering::Relaxed);
        if fallback {
            self.stats.fallbacks.fetch_add(1, Ordering::Relaxed);
        }
        #[cfg(test)]
        tally::record(rows_read, fallback);
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
        if state.oversized {
            *fallback = true;
            return self.full_read().await;
        }
        if let Some(cache) = state.cache.as_mut() {
            let tail = self
                .sessions
                .events_since(span, cache.cursor, limits.max_events + 1)
                .await?;
            *rows_read += count(tail.rows.len());
            if tail.head - 1 == cache.cursor && tail.rows.is_empty() {
                return self.project(cache);
            }
            match cache.extend(tail.head, tail.rows, super::context_compaction::STATE_KEY) {
                Extension::Applied => {
                    if cache.rows.len() <= limits.max_events
                        && cache.bytes <= limits.max_retained_event_bytes
                    {
                        return self.project(cache);
                    }
                }
                Extension::FullRead => {}
            }
            // Dropping the cache before the read keeps a failed read from
            // leaving a stale cache behind.
            state.cache = None;
            *fallback = true;
        }
        let seed = self
            .sessions
            .seed_active(span, super::context_compaction::STATE_KEY)
            .await?;
        *rows_read += count(seed.rows.len());
        self.stats.seeds.fetch_add(1, Ordering::Relaxed);
        let cache = ViewCache::from_seed(seed.head, seed.rows, seed.state_value);
        if cache.bytes > limits.max_retained_event_bytes {
            // The full read has no byte bound; keep nothing resident.
            state.oversized = true;
            *fallback = true;
            return self.full_read().await;
        }
        let events = self.project(&cache)?;
        state.cache = Some(cache);
        Ok(events)
    }
}

fn count(rows: usize) -> u64 {
    u64::try_from(rows).unwrap_or(u64::MAX)
}

fn identity_refused() -> AdkError {
    AdkError::new(
        ErrorComponent::Session,
        ErrorCategory::NotFound,
        "session.not_found",
        "the durable session was not found",
    )
}

#[async_trait]
impl SessionEventRefresh for IncrementalRunnerView {
    async fn refreshed_events(&self, identity: &AdkIdentity) -> adk_rust::Result<Vec<Event>> {
        // One view serves one session. Any other identity fails closed.
        if *identity != self.identity {
            return Err(identity_refused());
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

/// Test-only: a timing measurement turns the extra full reads off.
#[cfg(test)]
pub(crate) fn set_cross_check(enabled: bool) {
    CROSS_CHECK.store(enabled, Ordering::Relaxed);
}

#[cfg(test)]
impl IncrementalRunnerView {
    pub(crate) fn stats(&self) -> Arc<RefreshStats> {
        Arc::clone(&self.stats)
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
            .events_since(span, i64::MAX, 0)
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
            tally::skipped();
            return Ok(());
        }
        let encode = |events: &[Event]| {
            events
                .iter()
                .map(|event| serde_json::to_value(event).map_err(|_| identity_refused()))
                .collect::<Result<Vec<_>, _>>()
        };
        if encode(refreshed)? != encode(&full)? {
            tally::mismatch();
            eprintln!(
                "FU-287 cross-check: the incremental view ({} events) differs from the full read ({} events)",
                refreshed.len(),
                full.len()
            );
            return Err(AdkError::new(
                ErrorComponent::Session,
                ErrorCategory::Internal,
                "session.refresh_cross_check",
                "the incremental session view differs from the full read",
            ));
        }
        tally::checked();
        Ok(())
    }
}

/// Test-only: the ordinals the mirrored retirement rules keep, given every
/// stored row. The SQL `active_events` rules are the oracle.
#[cfg(test)]
pub(crate) fn mirrored_active_ordinals(all: &[ClassifiedEvent]) -> Vec<i64> {
    let mut snapshot: Option<(i64, String)> = None;
    let mut markers: HashMap<String, i64> = HashMap::new();
    let mut has_branches = false;
    for row in all {
        has_branches |= !row.branch.is_empty();
        if row.recovery_marker {
            let latest = markers.entry(row.branch.clone()).or_insert(row.ordinal);
            *latest = (*latest).max(row.ordinal);
            if let Some(agent) = &row.snapshot_agent
                && snapshot
                    .as_ref()
                    .is_none_or(|(ordinal, _)| *ordinal < row.ordinal)
            {
                snapshot = Some((row.ordinal, agent.clone()));
            }
        }
    }
    all.iter()
        .filter(|row| ViewCache::is_active(row, snapshot.as_ref(), has_branches, &markers))
        .map(|row| row.ordinal)
        .collect()
}

/// Test-only process-wide counts, for runs whose views no test holds.
#[cfg(test)]
pub(crate) mod tally {
    use std::sync::atomic::{AtomicU64, Ordering};

    static REFRESHES: AtomicU64 = AtomicU64::new(0);
    static FALLBACKS: AtomicU64 = AtomicU64::new(0);
    static ROWS_READ: AtomicU64 = AtomicU64::new(0);
    static CHECKED: AtomicU64 = AtomicU64::new(0);
    static SKIPPED: AtomicU64 = AtomicU64::new(0);
    static MISMATCHES: AtomicU64 = AtomicU64::new(0);

    pub(super) fn record(rows_read: u64, fallback: bool) {
        REFRESHES.fetch_add(1, Ordering::Relaxed);
        ROWS_READ.fetch_add(rows_read, Ordering::Relaxed);
        if fallback {
            FALLBACKS.fetch_add(1, Ordering::Relaxed);
        }
    }

    pub(super) fn checked() {
        CHECKED.fetch_add(1, Ordering::Relaxed);
    }

    pub(super) fn skipped() {
        SKIPPED.fetch_add(1, Ordering::Relaxed);
    }

    pub(super) fn mismatch() {
        MISMATCHES.fetch_add(1, Ordering::Relaxed);
    }

    /// `(refreshes, fallbacks, rows_read, cross_checked, cross_check_skipped, mismatches)`
    pub(crate) fn totals() -> (u64, u64, u64, u64, u64, u64) {
        (
            REFRESHES.load(Ordering::Relaxed),
            FALLBACKS.load(Ordering::Relaxed),
            ROWS_READ.load(Ordering::Relaxed),
            CHECKED.load(Ordering::Relaxed),
            SKIPPED.load(Ordering::Relaxed),
            MISMATCHES.load(Ordering::Relaxed),
        )
    }
}
