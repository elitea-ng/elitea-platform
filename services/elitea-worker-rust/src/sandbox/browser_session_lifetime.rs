//! The lifetime of the `browser` profile's sessions (ADR-0049 decisions 1
//! and 10), as a state machine without I/O: every operation takes `now`, a
//! monotonic offset from the supervisor's [`Clock`], so the tests drive it
//! with an injected clock.
//!
//! ```text
//!            admit                confirm                run_ended
//!   (none) ─────────▶ Starting ─────────────▶ Live ─────────────────▶ Lingering
//!                        │ abandon             │ idle 15 min            │ linger 10 min
//!                        ▼                     │ wall                   │ wall
//!                     (none)                   ▼                        ▼
//!                                           Retired ◀─────────────── Retired
//! ```
//!
//! * A session starts at the run's first browser call (the worker's
//!   `StartBrowserSession`) and holds a slot of the profile's concurrency
//!   from `admit` until it is retired: a Pod that exists counts.
//! * It is destroyed at the run's end plus the linger (10 minutes, capped by
//!   its wall), after 15 minutes without a call or viewer input, or at its
//!   wall limit, whichever is first.
//! * A retired name is never admitted again: a session is never reused,
//!   across runs or threads. Retired names are kept for a day (longer than
//!   any wall a profile admits).
//! * A start while the profile is full waits (the service does that, up to
//!   [`LifetimeLimits::capacity_wait`]) and then fails `browser_capacity`.
//! * A run holds at most [`LifetimeLimits::sessions_per_run`] sessions at
//!   once; past it a start fails `browser_session_limit` at once (waiting
//!   cannot help: only the run itself frees them).
//! * Every call is bound to the session's claim (`owner`) and fenced by the
//!   claim's generation: a call under an older generation than the newest
//!   the session has seen is refused.

use std::collections::{BTreeMap, VecDeque};
use std::time::Duration;

/// The linger after a run ends, so a person can look at the final page.
pub const LINGER: Duration = Duration::from_mins(10);
/// A session without a call or viewer input for this long is destroyed.
pub const IDLE: Duration = Duration::from_mins(15);
/// How long a start waits for a free slot before `browser_capacity`.
pub const CAPACITY_WAIT: Duration = Duration::from_secs(30);
/// How long a retired name is remembered.
const RETIRED_FOR: Duration = Duration::from_hours(24);
/// At most this many retired names are remembered (a full table forgets
/// the oldest; a name is a fresh 96-bit value per run and index).
const MAX_RETIRED: usize = 65_536;

/// A monotonic clock: the offset from an origin the supervisor fixes.
pub trait Clock: Send + Sync + 'static {
    fn now(&self) -> Duration;
}

/// The supervisor's clock: Tokio's, so paused-time tests drive it too.
pub struct TokioClock(tokio::time::Instant);

impl Default for TokioClock {
    fn default() -> Self {
        Self(tokio::time::Instant::now())
    }
}

impl Clock for TokioClock {
    fn now(&self) -> Duration {
        self.0.elapsed()
    }
}

/// The profile's lifetime limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LifetimeLimits {
    /// Sessions alive at once (starting, live and lingering).
    pub concurrency: usize,
    pub sessions_per_run: usize,
    pub linger: Duration,
    pub idle: Duration,
    pub capacity_wait: Duration,
}

impl LifetimeLimits {
    /// The ADR's limits with the profile's concurrency and per-run ceiling.
    #[must_use]
    pub fn new(concurrency: usize, sessions_per_run: usize) -> Self {
        Self {
            concurrency,
            sessions_per_run,
            linger: LINGER,
            idle: IDLE,
            capacity_wait: CAPACITY_WAIT,
        }
    }
}

/// The claim a session is bound to.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SessionClaim {
    /// The session's spec digest, 64 hex.
    pub request: String,
    /// The claim scope, 64 hex.
    pub owner: String,
    /// The run, 64 hex.
    pub run: String,
    pub generation: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Starting,
    Live,
    Lingering { until: Duration },
}

#[derive(Clone, Debug)]
struct Session {
    claim: SessionClaim,
    phase: Phase,
    wall_deadline: Duration,
    last_activity: Duration,
}

/// Why a session goes away.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EndReason {
    Idle,
    Wall,
    LingerElapsed,
    Destroyed,
    /// The session agent exited or broke the protocol.
    AgentFailed,
}

impl EndReason {
    /// The view stream's end code.
    #[must_use]
    pub fn code(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Wall => "wall",
            Self::LingerElapsed => "linger_elapsed",
            Self::Destroyed => "session_destroyed",
            Self::AgentFailed => "agent_failed",
        }
    }
}

/// What `admit` decided.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Admission {
    /// A new session: start it, then `confirm` or `abandon`.
    New,
    /// The same claim's live session (a retried start).
    Existing,
}

/// A refusal, by the stable code the service reports.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum Refusal {
    /// The profile is at its concurrency: wait, then `browser_capacity`.
    #[error("browser_capacity")]
    Full,
    #[error("browser_session_limit")]
    SessionLimit,
    /// Retired, or lingering after its run ended.
    #[error("browser_session_ended")]
    Ended,
    #[error("browser_session_not_started")]
    NotStarted,
    /// Another claim's session, or a start still in progress.
    #[error("browser_session_conflict")]
    Conflict,
    /// A call under an older claim generation.
    #[error("browser_session_fenced")]
    Fenced,
}

/// The sessions of one profile.
pub struct SessionTable {
    limits: LifetimeLimits,
    sessions: BTreeMap<String, Session>,
    retired: BTreeMap<String, Duration>,
    retired_order: VecDeque<(Duration, String)>,
}

impl SessionTable {
    #[must_use]
    pub fn new(limits: LifetimeLimits) -> Self {
        Self {
            limits,
            sessions: BTreeMap::new(),
            retired: BTreeMap::new(),
            retired_order: VecDeque::new(),
        }
    }

    #[must_use]
    pub fn limits(&self) -> LifetimeLimits {
        self.limits
    }

    /// Sessions holding a slot.
    #[must_use]
    pub fn len(&self) -> usize {
        self.sessions.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.sessions.is_empty()
    }

    #[must_use]
    pub fn phase(&self, name: &str) -> Option<Phase> {
        self.sessions.get(name).map(|session| session.phase)
    }

    #[must_use]
    pub fn contains(&self, name: &str) -> bool {
        self.sessions.contains_key(name)
    }

    #[must_use]
    pub fn is_retired(&self, name: &str) -> bool {
        self.retired.contains_key(name)
    }

    /// Admit a start of `name` for `claim` with a wall of `wall`.
    ///
    /// # Errors
    ///
    /// [`Refusal::Ended`] for a retired name, [`Refusal::Conflict`] for
    /// another claim's session (or one still starting),
    /// [`Refusal::SessionLimit`] past the run's ceiling, [`Refusal::Full`]
    /// at the profile's concurrency.
    pub fn admit(
        &mut self,
        name: &str,
        claim: &SessionClaim,
        wall: Duration,
        now: Duration,
    ) -> Result<Admission, Refusal> {
        self.forget_retired(now);
        if self.retired.contains_key(name) {
            return Err(Refusal::Ended);
        }
        if let Some(session) = self.sessions.get_mut(name) {
            if session.claim.request != claim.request || session.claim.owner != claim.owner {
                return Err(Refusal::Conflict);
            }
            return match session.phase {
                Phase::Starting => Err(Refusal::Conflict),
                Phase::Lingering { .. } => Err(Refusal::Ended),
                Phase::Live => {
                    Self::fence(session, claim.generation)?;
                    session.last_activity = now;
                    Ok(Admission::Existing)
                }
            };
        }
        let held = self
            .sessions
            .values()
            .filter(|session| session.claim.run == claim.run)
            .count();
        if held >= self.limits.sessions_per_run {
            return Err(Refusal::SessionLimit);
        }
        if self.sessions.len() >= self.limits.concurrency {
            return Err(Refusal::Full);
        }
        self.sessions.insert(
            name.to_owned(),
            Session {
                claim: claim.clone(),
                phase: Phase::Starting,
                wall_deadline: now + wall,
                last_activity: now,
            },
        );
        Ok(Admission::New)
    }

    /// The started session is live.
    pub fn confirm(&mut self, name: &str, now: Duration) {
        if let Some(session) = self.sessions.get_mut(name)
            && session.phase == Phase::Starting
        {
            session.phase = Phase::Live;
            session.last_activity = now;
        }
    }

    /// The start failed before the session served anything: free the slot
    /// without retiring the name, so the worker may retry it.
    pub fn abandon(&mut self, name: &str) {
        if self
            .sessions
            .get(name)
            .is_some_and(|session| session.phase == Phase::Starting)
        {
            self.sessions.remove(name);
        }
    }

    fn fence(session: &mut Session, generation: u64) -> Result<(), Refusal> {
        if generation < session.claim.generation {
            return Err(Refusal::Fenced);
        }
        session.claim.generation = generation;
        Ok(())
    }

    fn owned(&mut self, name: &str, claim: &SessionClaim) -> Result<&mut Session, Refusal> {
        if self.retired.contains_key(name) {
            return Err(Refusal::Ended);
        }
        let session = self.sessions.get_mut(name).ok_or(Refusal::NotStarted)?;
        if session.claim.request != claim.request || session.claim.owner != claim.owner {
            return Err(Refusal::Conflict);
        }
        Ok(session)
    }

    /// A tool call (or a driving viewer's input): the session must be live
    /// and the claim's; it is no longer idle.
    ///
    /// # Errors
    ///
    /// As [`Refusal`]; a lingering session is [`Refusal::Ended`].
    pub fn touch(
        &mut self,
        name: &str,
        claim: &SessionClaim,
        now: Duration,
    ) -> Result<(), Refusal> {
        let session = self.owned(name, claim)?;
        match session.phase {
            Phase::Starting => Err(Refusal::NotStarted),
            Phase::Lingering { .. } => Err(Refusal::Ended),
            Phase::Live => {
                Self::fence(session, claim.generation)?;
                session.last_activity = now;
                Ok(())
            }
        }
    }

    /// A viewer may attach to a live or lingering session of this claim.
    /// Returns whether the session lingers (its input is refused).
    ///
    /// # Errors
    ///
    /// As [`Refusal`].
    pub fn viewable(&mut self, name: &str, claim: &SessionClaim) -> Result<bool, Refusal> {
        let session = self.owned(name, claim)?;
        match session.phase {
            Phase::Starting => Err(Refusal::NotStarted),
            Phase::Live => Ok(false),
            Phase::Lingering { .. } => Ok(true),
        }
    }

    /// The run ended: refuse further calls; destroy after the linger,
    /// capped by the wall. Returns when the linger ends. Idempotent.
    ///
    /// # Errors
    ///
    /// As [`Refusal`].
    pub fn run_ended(
        &mut self,
        name: &str,
        claim: &SessionClaim,
        now: Duration,
    ) -> Result<Duration, Refusal> {
        let linger = self.limits.linger;
        let session = self.owned(name, claim)?;
        Self::fence(session, claim.generation)?;
        let until = match session.phase {
            Phase::Lingering { until } => until,
            Phase::Starting | Phase::Live => {
                let until = (now + linger).min(session.wall_deadline);
                session.phase = Phase::Lingering { until };
                until
            }
        };
        Ok(until)
    }

    /// The session's claim, for a destroy that must prove ownership.
    ///
    /// # Errors
    ///
    /// As [`Refusal`].
    pub fn check_owner(&mut self, name: &str, claim: &SessionClaim) -> Result<(), Refusal> {
        let session = self.owned(name, claim)?;
        Self::fence(session, claim.generation)
    }

    /// Remove `name` and remember it so it is never admitted again.
    /// Returns whether it held a slot.
    pub fn retire(&mut self, name: &str, now: Duration) -> bool {
        let held = self.sessions.remove(name).is_some();
        if self.retired.insert(name.to_owned(), now).is_none() {
            self.retired_order.push_back((now, name.to_owned()));
        }
        while self.retired_order.len() > MAX_RETIRED {
            if let Some((_, oldest)) = self.retired_order.pop_front() {
                self.retired.remove(&oldest);
            }
        }
        held
    }

    fn forget_retired(&mut self, now: Duration) {
        while let Some((at, _)) = self.retired_order.front() {
            if now.saturating_sub(*at) < RETIRED_FOR {
                break;
            }
            if let Some((_, name)) = self.retired_order.pop_front() {
                self.retired.remove(&name);
            }
        }
    }

    /// Every session past its idle limit, its wall or its linger, with why.
    /// A starting session is bounded by its wall only.
    #[must_use]
    pub fn expired(&self, now: Duration) -> Vec<(String, EndReason)> {
        self.sessions
            .iter()
            .filter_map(|(name, session)| {
                let reason = if now >= session.wall_deadline {
                    EndReason::Wall
                } else {
                    match session.phase {
                        Phase::Live if now >= session.last_activity + self.limits.idle => {
                            EndReason::Idle
                        }
                        Phase::Lingering { until } if now >= until => EndReason::LingerElapsed,
                        Phase::Starting | Phase::Live | Phase::Lingering { .. } => return None,
                    }
                };
                Some((name.clone(), reason))
            })
            .collect()
    }

    /// The names holding a slot.
    #[must_use]
    pub fn names(&self) -> Vec<String> {
        self.sessions.keys().cloned().collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const MIN: Duration = Duration::from_mins(1);

    fn claim(run: &str, generation: u64) -> SessionClaim {
        SessionClaim {
            request: format!("req-{run}"),
            owner: format!("owner-{run}"),
            run: run.to_owned(),
            generation,
        }
    }

    fn table(concurrency: usize) -> SessionTable {
        SessionTable::new(LifetimeLimits::new(concurrency, 3))
    }

    fn start(table: &mut SessionTable, name: &str, claim: &SessionClaim, now: Duration) {
        assert_eq!(
            table.admit(name, claim, 60 * MIN, now),
            Ok(Admission::New),
            "{name}"
        );
        table.confirm(name, now);
    }

    #[test]
    fn a_session_is_destroyed_after_fifteen_idle_minutes_and_calls_keep_it() {
        let mut t = table(4);
        let c = claim("r1", 1);
        start(&mut t, "s1", &c, Duration::ZERO);
        assert_eq!(t.expired(14 * MIN).len(), 0);
        t.touch("s1", &c, 14 * MIN).expect("call");
        assert!(t.expired(28 * MIN).is_empty(), "the call reset idle");
        assert_eq!(
            t.expired(29 * MIN),
            vec![("s1".to_owned(), EndReason::Idle)]
        );
    }

    #[test]
    fn the_wall_limit_ends_a_busy_session() {
        let mut t = table(4);
        let c = claim("r1", 1);
        start(&mut t, "s1", &c, Duration::ZERO);
        for minute in (5..60).step_by(5) {
            t.touch("s1", &c, minute * MIN).expect("call");
            assert_eq!(t.expired(minute * MIN).len(), 0);
        }
        assert_eq!(
            t.expired(60 * MIN),
            vec![("s1".to_owned(), EndReason::Wall)]
        );
    }

    #[test]
    fn run_end_lingers_ten_minutes_refusing_calls_but_admitting_viewers() {
        let mut t = table(4);
        let c = claim("r1", 1);
        start(&mut t, "s1", &c, Duration::ZERO);
        let until = t.run_ended("s1", &c, 5 * MIN).expect("ended");
        assert_eq!(until, 15 * MIN);
        assert_eq!(t.run_ended("s1", &c, 7 * MIN), Ok(until), "idempotent");
        assert_eq!(t.touch("s1", &c, 6 * MIN), Err(Refusal::Ended));
        assert_eq!(t.viewable("s1", &c), Ok(true));
        assert_eq!(t.expired(14 * MIN).len(), 0);
        assert_eq!(
            t.expired(15 * MIN),
            vec![("s1".to_owned(), EndReason::LingerElapsed)]
        );
        // The linger never outlives the wall.
        let mut t = table(4);
        start(&mut t, "s2", &c, Duration::ZERO);
        assert_eq!(t.run_ended("s2", &c, 55 * MIN), Ok(60 * MIN));
    }

    #[test]
    fn a_retired_name_is_never_admitted_again() {
        let mut t = table(4);
        let c = claim("r1", 1);
        start(&mut t, "s1", &c, Duration::ZERO);
        assert!(t.retire("s1", MIN));
        assert_eq!(t.admit("s1", &c, 60 * MIN, 2 * MIN), Err(Refusal::Ended));
        assert_eq!(t.touch("s1", &c, 2 * MIN), Err(Refusal::Ended));
        // Another claim cannot take the name either.
        assert_eq!(
            t.admit("s1", &claim("r2", 1), 60 * MIN, 2 * MIN),
            Err(Refusal::Ended)
        );
        assert!(t.is_empty());
    }

    #[test]
    fn concurrency_counts_starting_live_and_lingering_sessions() {
        let mut t = table(2);
        let (a, b, c) = (claim("a", 1), claim("b", 1), claim("c", 1));
        assert_eq!(
            t.admit("s1", &a, 60 * MIN, Duration::ZERO),
            Ok(Admission::New)
        );
        start(&mut t, "s2", &b, Duration::ZERO);
        t.run_ended("s2", &b, MIN).expect("lingers");
        assert_eq!(t.admit("s3", &c, 60 * MIN, MIN), Err(Refusal::Full));
        t.abandon("s1");
        assert!(!t.is_retired("s1"), "a failed start may be retried");
        assert_eq!(t.admit("s3", &c, 60 * MIN, MIN), Ok(Admission::New));
    }

    #[test]
    fn a_run_holds_at_most_three_sessions() {
        let mut t = table(16);
        let c = claim("r1", 1);
        for name in ["s1", "s2", "s3"] {
            start(&mut t, name, &c, Duration::ZERO);
        }
        assert_eq!(
            t.admit("s4", &c, 60 * MIN, Duration::ZERO),
            Err(Refusal::SessionLimit)
        );
        // Another run is not limited by this one.
        start(&mut t, "t1", &claim("r2", 1), Duration::ZERO);
        t.retire("s1", MIN);
        start(&mut t, "s4", &c, MIN);
    }

    #[test]
    fn a_retried_start_observes_the_live_session_and_another_claim_conflicts() {
        let mut t = table(4);
        let c = claim("r1", 1);
        assert_eq!(
            t.admit("s1", &c, 60 * MIN, Duration::ZERO),
            Ok(Admission::New)
        );
        assert_eq!(
            t.admit("s1", &c, 60 * MIN, Duration::ZERO),
            Err(Refusal::Conflict),
            "still starting"
        );
        t.confirm("s1", Duration::ZERO);
        assert_eq!(t.admit("s1", &c, 60 * MIN, MIN), Ok(Admission::Existing));
        assert_eq!(
            t.admit("s1", &claim("r2", 1), 60 * MIN, MIN),
            Err(Refusal::Conflict)
        );
        assert_eq!(t.touch("s1", &claim("r2", 1), MIN), Err(Refusal::Conflict));
        assert_eq!(t.touch("nope", &c, MIN), Err(Refusal::NotStarted));
    }

    #[test]
    fn an_older_claim_generation_is_fenced() {
        let mut t = table(4);
        start(&mut t, "s1", &claim("r1", 2), Duration::ZERO);
        assert_eq!(t.touch("s1", &claim("r1", 1), MIN), Err(Refusal::Fenced));
        t.touch("s1", &claim("r1", 3), MIN).expect("a newer lease");
        assert_eq!(t.touch("s1", &claim("r1", 2), MIN), Err(Refusal::Fenced));
        assert_eq!(
            t.run_ended("s1", &claim("r1", 2), MIN),
            Err(Refusal::Fenced)
        );
    }

    #[test]
    fn retired_names_are_forgotten_after_a_day() {
        let mut t = table(4);
        let c = claim("r1", 1);
        start(&mut t, "s1", &c, Duration::ZERO);
        t.retire("s1", Duration::ZERO);
        assert!(t.is_retired("s1"));
        assert_eq!(t.admit("s2", &c, 60 * MIN, RETIRED_FOR), Ok(Admission::New));
        assert!(!t.is_retired("s1"));
    }

    #[test]
    fn the_tokio_clock_follows_paused_time() {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .start_paused(true)
            .build()
            .expect("runtime");
        runtime.block_on(async {
            let clock = TokioClock::default();
            tokio::time::advance(IDLE).await;
            assert!(clock.now() >= IDLE);
        });
    }
}
