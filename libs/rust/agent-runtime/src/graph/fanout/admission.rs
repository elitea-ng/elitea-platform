//! Worker-wide admission for fan-out children.
//!
//! - [`ChildPermits`]: at most [`WORKER_CHILD_PERMITS`] children run at once in
//!   one Worker process, across every activation. A permit is held only while
//!   the child runs: a paused child releases it, so parked work never starves
//!   live work.
//! - [`ModelStreamLimiter`]: at most [`MODEL_STREAMS_PER_WORKER`] concurrent
//!   model streams per model, bounded to [`MAX_TRACKED_MODELS`] models. An
//!   unknown model past the bound shares one overflow lane rather than growing
//!   the table.
//!
//! - [`CodeSlots`]: at most `code_slots` Code attempts at once on one sandbox
//!   Supervisor target, per Worker process (W3-1). One slot is held for one
//!   whole attempt (preparation, submission, backoff, observation).
//! - [`required_window`]: the time a Code workload needs before the run's
//!   deadline, one rule for a root Code node (`items = 1`) and a fan-out of
//!   Code children.
//!
//! - [`code_attempt_scope`]: R1 (W3-2). One Code attempt never waits for a
//!   second slot while it holds one: an acquire inside an attempt that
//!   already holds a slot is refused at once, typed
//!   ([`NESTED_CODE_SLOT_CODE`]). The marker is task-local: it covers only the
//!   future it wraps and does not follow `tokio::spawn`.
//!
//! Waiting is cancellation-safe: dropping a pending acquire leaves nothing
//! held.

use std::cell::Cell;
use std::collections::HashMap;
use std::future::Future;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::time::Instant;

use crate::graph::fanout_budget::{
    MAX_TRACKED_MODELS, MODEL_STREAMS_PER_WORKER, WORKER_CHILD_PERMITS,
};

/// The process-wide child slots.
#[derive(Clone)]
pub struct ChildPermits {
    slots: Arc<Semaphore>,
}

/// One running child's slot. Dropping it (child settled or paused) frees it.
#[derive(Debug)]
pub struct ChildPermit {
    _permit: OwnedSemaphorePermit,
}

impl Default for ChildPermits {
    fn default() -> Self {
        Self::with_capacity(WORKER_CHILD_PERMITS)
    }
}

impl ChildPermits {
    /// A pool of `capacity` slots (`WORKER_CHILD_PERMITS` in production).
    #[must_use]
    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            slots: Arc::new(Semaphore::new(capacity)),
        }
    }

    /// Wait for a slot. The pool is never closed, so this only waits.
    pub async fn acquire(&self) -> ChildPermit {
        let permit = Arc::clone(&self.slots)
            .acquire_owned()
            .await
            .unwrap_or_else(|_| unreachable!("the child permit pool is never closed"));
        ChildPermit { _permit: permit }
    }

    /// Slots free right now.
    #[must_use]
    pub fn available(&self) -> usize {
        self.slots.available_permits()
    }
}

/// Per-(Worker, model) stream lanes.
#[derive(Clone, Default)]
pub struct ModelStreamLimiter {
    lanes: Arc<Mutex<Lanes>>,
}

#[derive(Default)]
struct Lanes {
    by_model: HashMap<String, Arc<Semaphore>>,
    overflow: Option<Arc<Semaphore>>,
}

/// One model stream's lane slot.
#[derive(Debug)]
pub struct ModelStreamPermit {
    _permit: OwnedSemaphorePermit,
}

impl ModelStreamLimiter {
    fn lane(&self, model: &str) -> Arc<Semaphore> {
        let mut lanes = self
            .lanes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(lane) = lanes.by_model.get(model) {
            return Arc::clone(lane);
        }
        if lanes.by_model.len() < MAX_TRACKED_MODELS {
            let lane = Arc::new(Semaphore::new(MODEL_STREAMS_PER_WORKER));
            lanes.by_model.insert(model.to_owned(), Arc::clone(&lane));
            return lane;
        }
        Arc::clone(
            lanes
                .overflow
                .get_or_insert_with(|| Arc::new(Semaphore::new(MODEL_STREAMS_PER_WORKER))),
        )
    }

    /// Wait for a stream slot on `model`'s lane.
    pub async fn acquire(&self, model: &str) -> ModelStreamPermit {
        let permit = self
            .lane(model)
            .acquire_owned()
            .await
            .unwrap_or_else(|_| unreachable!("model lanes are never closed"));
        ModelStreamPermit { _permit: permit }
    }

    /// Models with their own lane.
    #[must_use]
    pub fn tracked_models(&self) -> usize {
        self.lanes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .by_model
            .len()
    }
}

/// Code slots per Supervisor target when a profile does not set `code_slots`:
/// the shipped Supervisor `concurrency`.
pub const DEFAULT_CODE_SLOTS: u32 = 4;

/// Most Code slots per target: the Supervisor's own `concurrency` bound.
pub const MAX_CODE_SLOTS: u32 = 1024;

/// Allowance after a Code job's timeout: the Supervisor lease (60 s), one
/// heartbeat (20 s) and delivery slack. An attempt that cannot fit its
/// timeout plus this before the run's deadline would end unobservable.
pub const CODE_OBSERVATION_MARGIN: Duration = Duration::from_secs(90);

/// The time `items` Code workloads need before the run's deadline:
/// `ceil(items / min(concurrency, slots)) × item_timeout + CODE_OBSERVATION_MARGIN`.
///
/// A zero `concurrency` or `slots` counts as one lane. The result saturates
/// at `Duration::MAX`, which no deadline admits.
#[must_use]
pub fn required_window(
    items: u32,
    concurrency: u32,
    slots: u32,
    item_timeout: Duration,
) -> Duration {
    let lanes = concurrency.min(slots).max(1);
    item_timeout
        .saturating_mul(items.div_ceil(lanes))
        .saturating_add(CODE_OBSERVATION_MARGIN)
}

/// A fan-out activation whose Code items cannot finish before the run's
/// deadline (W3-2 raises it before freezing the activation).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct FanoutDeadlineInsufficient {
    pub required: Duration,
    pub remaining: Duration,
}

impl FanoutDeadlineInsufficient {
    /// The registered fault code (`graph.fanout.deadline_insufficient`).
    pub const CODE: &'static str = "graph.fanout.deadline_insufficient";
}

impl std::fmt::Display for FanoutDeadlineInsufficient {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(Self::CODE)
    }
}

impl std::error::Error for FanoutDeadlineInsufficient {}

/// Admit a fan-out of `items` Code children at `now`: the remaining time
/// must be at least [`required_window`]; exactly equal is admitted.
///
/// # Errors
///
/// [`FanoutDeadlineInsufficient`] when the window does not fit.
pub fn admit_fanout_window(
    items: u32,
    concurrency: u32,
    slots: u32,
    item_timeout: Duration,
    execution_deadline: Instant,
    now: Instant,
) -> Result<(), FanoutDeadlineInsufficient> {
    let required = required_window(items, concurrency, slots, item_timeout);
    let remaining = execution_deadline.saturating_duration_since(now);
    if remaining >= required {
        Ok(())
    } else {
        Err(FanoutDeadlineInsufficient {
            required,
            remaining,
        })
    }
}

/// One Supervisor target's Code slots, shared by every Code runtime of this
/// Worker process that submits to it.
#[derive(Clone, Debug)]
pub struct CodeSlots {
    semaphore: Arc<Semaphore>,
    capacity: u32,
}

/// One Code attempt's slot. Dropping it (terminal receipt or typed failure)
/// frees it.
#[derive(Debug)]
pub struct CodeSlot {
    _permit: OwnedSemaphorePermit,
    waited: Duration,
    /// This slot set the attempt's R1 marker: dropping it clears the marker,
    /// so the attempt may admit again once it no longer holds a slot.
    marks_attempt: bool,
}

impl Drop for CodeSlot {
    fn drop(&mut self) {
        if self.marks_attempt {
            // Outside the attempt's task (or scope) this is a no-op.
            let _ = CODE_SLOT_HELD.try_with(|held| held.set(false));
        }
    }
}

impl CodeSlot {
    /// How long the attempt waited for this slot.
    #[must_use]
    pub const fn waited(&self) -> Duration {
        self.waited
    }
}

/// The wait for a slot reached its bound; nothing is held.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SlotWaitExpired {
    pub waited: Duration,
}

/// What used up a fresh attempt's window.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CodeRefusalCause {
    /// The run had too little time left when the attempt arrived.
    Clock,
    /// The window was there on arrival; waiting for a slot used it up.
    SlotWait,
    /// The window was there at admission; preparation and hydration used it
    /// up (W3-2, N3). Nothing was submitted.
    Preparation,
    /// The attempt already holds a Code slot (R1): a second acquire would
    /// wait on itself. Refused at once; nothing more is held.
    NestedSlot,
}

/// The operator code of a nested Code slot acquire (R1).
pub const NESTED_CODE_SLOT_CODE: &str = "pipeline.code.nested_slot";

/// An attempt that may already have started could not get a slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StartedSlotRefusal {
    /// The run's deadline passed first; nothing is held.
    Expired(SlotWaitExpired),
    /// The attempt already holds a Code slot (R1); nothing more is held.
    NestedSlot,
}

tokio::task_local! {
    /// Set to `true` once the wrapped Code attempt holds its slot (R1).
    static CODE_SLOT_HELD: Cell<bool>;
}

/// Run one Code attempt under the R1 marker. Inside it, a second
/// [`CodeSlots`] admission after the first succeeded is refused at once
/// (`NestedSlot`) instead of waiting for a slot this attempt itself holds.
///
/// Wrap the Code attempt future only, never a fan-out child or activation
/// future: siblings polled by one task then never see each other's slot.
/// A nested attempt in the same task keeps the outer marker. A future moved
/// to another task (`tokio::spawn`) does not carry it.
pub async fn code_attempt_scope<F: Future>(attempt: F) -> F::Output {
    if CODE_SLOT_HELD.try_with(|_| ()).is_ok() {
        return attempt.await;
    }
    CODE_SLOT_HELD.scope(Cell::new(false), attempt).await
}

fn code_slot_held() -> bool {
    CODE_SLOT_HELD.try_with(Cell::get).unwrap_or(false)
}

/// Mark the attempt as holding `slot`; the slot clears the mark when it is
/// dropped (M1, #1896).
fn mark_code_slot_held(mut slot: CodeSlot) -> CodeSlot {
    // Outside a scope there is no attempt to mark.
    slot.marks_attempt = CODE_SLOT_HELD.try_with(|held| held.set(true)).is_ok();
    slot
}

/// N3 (W3-2): re-admit a fresh attempt after its preparation and hydration,
/// before its first submission: at least `required` must still be left.
/// Exactly equal is admitted.
///
/// # Errors
///
/// [`CodeDeadlineRefusal`] with cause `Preparation`; the caller releases its
/// slot and submits nothing.
pub fn recheck_after_preparation(
    execution_deadline: Instant,
    required: Duration,
    now: Instant,
) -> Result<(), CodeDeadlineRefusal> {
    let remaining = execution_deadline.saturating_duration_since(now);
    if remaining >= required {
        Ok(())
    } else {
        Err(CodeDeadlineRefusal {
            required,
            remaining,
            slot_wait: Duration::ZERO,
            cause: CodeRefusalCause::Preparation,
        })
    }
}

/// A fresh Code attempt refused before dispatch: nothing is held, nothing
/// was started.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CodeDeadlineRefusal {
    pub required: Duration,
    /// Remaining time when the attempt was refused.
    pub remaining: Duration,
    pub slot_wait: Duration,
    pub cause: CodeRefusalCause,
}

impl CodeSlots {
    /// `capacity` slots, clamped to `1..=MAX_CODE_SLOTS` (the Worker's config
    /// refuses anything outside it before this is built).
    #[must_use]
    pub fn new(capacity: u32) -> Self {
        let capacity = capacity.clamp(1, MAX_CODE_SLOTS);
        Self {
            // At most MAX_CODE_SLOTS, which fits every usize.
            semaphore: Arc::new(Semaphore::new(usize::try_from(capacity).unwrap_or(1))),
            capacity,
        }
    }

    /// The slots this target was built with.
    #[must_use]
    pub const fn capacity(&self) -> u32 {
        self.capacity
    }

    /// Slots free right now.
    #[must_use]
    pub fn available(&self) -> usize {
        self.semaphore.available_permits()
    }

    /// Wait for a slot until `deadline`. A slot already free is taken even
    /// at the deadline; the caller decides whether the time left suffices.
    ///
    /// # Errors
    ///
    /// [`SlotWaitExpired`] when `deadline` passes first; nothing is held.
    pub async fn acquire_until(&self, deadline: Instant) -> Result<CodeSlot, SlotWaitExpired> {
        let started = Instant::now();
        match tokio::time::timeout_at(deadline, Arc::clone(&self.semaphore).acquire_owned()).await {
            Ok(Ok(permit)) => Ok(CodeSlot {
                _permit: permit,
                waited: started.elapsed(),
                marks_attempt: false,
            }),
            Ok(Err(_)) => unreachable!("Code slots are never closed"),
            Err(_) => Err(SlotWaitExpired {
                waited: started.elapsed(),
            }),
        }
    }

    /// Admit a fresh Code attempt (nothing of it started yet): a slot taken
    /// while `execution_deadline − now ≥ required`. Exactly equal is
    /// admitted. The wait ends at `execution_deadline − required`.
    ///
    /// # Errors
    ///
    /// [`CodeDeadlineRefusal`] before any effect; nothing is held.
    pub async fn admit_fresh(
        &self,
        execution_deadline: Instant,
        required: Duration,
    ) -> Result<CodeSlot, CodeDeadlineRefusal> {
        let arrived = Instant::now();
        let refuse = |cause, slot_wait| CodeDeadlineRefusal {
            required,
            remaining: execution_deadline.saturating_duration_since(Instant::now()),
            slot_wait,
            cause,
        };
        if code_slot_held() {
            return Err(refuse(CodeRefusalCause::NestedSlot, Duration::ZERO));
        }
        let Some(bound) = execution_deadline.checked_sub(required) else {
            return Err(refuse(CodeRefusalCause::Clock, Duration::ZERO));
        };
        if arrived > bound {
            return Err(refuse(CodeRefusalCause::Clock, Duration::ZERO));
        }
        let slot = self
            .acquire_until(bound)
            .await
            .map_err(|expired| refuse(CodeRefusalCause::SlotWait, expired.waited))?;
        if Instant::now() > bound {
            let waited = slot.waited;
            drop(slot);
            return Err(refuse(CodeRefusalCause::SlotWait, waited));
        }
        Ok(mark_code_slot_held(slot))
    }

    /// A slot for an attempt that may already have started (recovery
    /// reconciles it, never refuses it): no deadline admission, the wait is
    /// bounded by the run's deadline only.
    ///
    /// # Errors
    ///
    /// [`StartedSlotRefusal::Expired`] when the run's deadline passes first;
    /// [`StartedSlotRefusal::NestedSlot`] at once inside an attempt that
    /// already holds a slot (R1).
    pub async fn admit_started(
        &self,
        execution_deadline: Instant,
    ) -> Result<CodeSlot, StartedSlotRefusal> {
        if code_slot_held() {
            return Err(StartedSlotRefusal::NestedSlot);
        }
        let slot = self
            .acquire_until(execution_deadline)
            .await
            .map_err(StartedSlotRefusal::Expired)?;
        Ok(mark_code_slot_held(slot))
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    const MINUTE: Duration = Duration::from_secs(60);

    #[test]
    fn required_window_matches_the_formula() {
        // (items, concurrency, slots, timeout, waves)
        for (items, concurrency, slots, waves) in [
            (1, 1, 4, 1),
            (1, 8, 1, 1),
            (8, 8, 4, 2),
            (8, 8, 1, 8),
            (9, 8, 4, 3),
            (9, 8, 8, 2),
            (64, 8, 4, 16),
            (64, 8, 1, 64),
            (4, 8, 4, 1),
            (5, 8, 4, 2),
            (0, 8, 4, 0),
            // A zero lane count is one lane, never a division by zero.
            (3, 0, 4, 3),
            (3, 8, 0, 3),
        ] {
            assert_eq!(
                required_window(items, concurrency, slots, MINUTE),
                MINUTE * waves + CODE_OBSERVATION_MARGIN,
                "items {items}, concurrency {concurrency}, slots {slots}"
            );
        }
        // Overflow-safe at the largest timeouts and counts.
        assert_eq!(
            required_window(u32::MAX, 1, 1, Duration::from_secs(u64::from(u32::MAX))),
            Duration::from_secs(u64::from(u32::MAX)) * u32::MAX + CODE_OBSERVATION_MARGIN
        );
        assert_eq!(
            required_window(u32::MAX, 1, 1, Duration::MAX),
            Duration::MAX
        );
        assert_eq!(required_window(1, 1, 1, Duration::MAX), Duration::MAX);
    }

    #[tokio::test(start_paused = true)]
    async fn fanout_window_admits_exactly_equal_and_refuses_one_ms_less() {
        let now = Instant::now();
        let required = required_window(9, 8, 4, MINUTE);
        assert!(admit_fanout_window(9, 8, 4, MINUTE, now + required, now).is_ok());
        let refused = admit_fanout_window(
            9,
            8,
            4,
            MINUTE,
            now + required - Duration::from_millis(1),
            now,
        )
        .unwrap_err();
        assert_eq!(refused.required, required);
        assert_eq!(
            refused.remaining,
            required.saturating_sub(Duration::from_millis(1))
        );
        assert_eq!(refused.to_string(), "graph.fanout.deadline_insufficient");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn code_slots_limit_and_limit_plus_one_and_release_on_drop() {
        let slots = CodeSlots::new(DEFAULT_CODE_SLOTS);
        let far = Instant::now() + Duration::from_secs(60);
        let mut held = Vec::new();
        for _ in 0..DEFAULT_CODE_SLOTS {
            held.push(slots.acquire_until(far).await.expect("within capacity"));
        }
        assert_eq!(slots.available(), 0);
        let waiting = slots.clone();
        let next = tokio::spawn(async move { waiting.acquire_until(far).await });
        tokio::task::yield_now().await;
        assert!(!next.is_finished(), "the fifth attempt ran without a slot");
        drop(held.pop());
        let slot = tokio::time::timeout(Duration::from_secs(5), next)
            .await
            .expect("a released slot admitted the next attempt")
            .expect("join")
            .expect("slot");
        drop(slot);
        drop(held);
        assert_eq!(
            slots.available(),
            usize::try_from(DEFAULT_CODE_SLOTS).unwrap()
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_cancelled_slot_wait_holds_nothing() {
        let slots = CodeSlots::new(1);
        let held = slots.acquire_until(Instant::now() + MINUTE).await.unwrap();
        let pending = tokio::time::timeout(
            Duration::from_millis(10),
            slots.acquire_until(Instant::now() + MINUTE),
        )
        .await;
        assert!(pending.is_err(), "the dropped wait got a slot");
        drop(held);
        assert_eq!(slots.available(), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn slot_wait_ends_at_the_bound_typed() {
        let slots = CodeSlots::new(1);
        let _held = slots.acquire_until(Instant::now() + MINUTE).await.unwrap();
        let bound = Instant::now() + Duration::from_secs(5);
        let expired = slots.acquire_until(bound).await.unwrap_err();
        assert_eq!(expired.waited, Duration::from_secs(5));
        assert_eq!(Instant::now(), bound);
        assert_eq!(slots.available(), 0, "the expired wait holds nothing");
    }

    #[tokio::test(start_paused = true)]
    async fn a_fresh_attempt_is_admitted_at_exactly_required_and_refused_one_ms_less() {
        let slots = CodeSlots::new(1);
        let required = required_window(1, 1, 1, MINUTE);
        let now = Instant::now();
        let slot = slots.admit_fresh(now + required, required).await.unwrap();
        assert_eq!(slot.waited(), Duration::ZERO);
        drop(slot);
        let refused = slots
            .admit_fresh(now + required - Duration::from_millis(1), required)
            .await
            .unwrap_err();
        assert_eq!(refused.cause, CodeRefusalCause::Clock);
        assert_eq!(refused.required, required);
        assert_eq!(
            refused.remaining,
            required.saturating_sub(Duration::from_millis(1))
        );
        assert_eq!(slots.available(), 1, "a refusal holds nothing");
        // A deadline nearer than the window itself cannot underflow.
        let refused = slots
            .admit_fresh(now + Duration::from_secs(1), Duration::MAX)
            .await
            .unwrap_err();
        assert_eq!(refused.cause, CodeRefusalCause::Clock);
    }

    #[tokio::test(start_paused = true)]
    async fn a_slot_wait_that_uses_up_the_margin_is_refused_as_slot_wait() {
        let slots = CodeSlots::new(1);
        let held = slots.acquire_until(Instant::now() + MINUTE).await.unwrap();
        let required = required_window(1, 1, 1, MINUTE);
        let arrived = Instant::now();
        let deadline = arrived + required + Duration::from_secs(3);
        let refused = slots.admit_fresh(deadline, required).await.unwrap_err();
        assert_eq!(refused.cause, CodeRefusalCause::SlotWait);
        assert_eq!(refused.slot_wait, Duration::from_secs(3));
        assert_eq!(refused.remaining, required, "the wait ended at the bound");
        assert_eq!(Instant::now(), deadline - required);
        drop(held);
        assert_eq!(slots.available(), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn a_started_attempt_takes_a_slot_without_the_deadline_check() {
        let slots = CodeSlots::new(1);
        let deadline = Instant::now() + Duration::from_secs(1);
        // Far less than timeout + margin: a started attempt is still served.
        let slot = slots.admit_started(deadline).await.unwrap();
        assert_eq!(slots.available(), 0);
        let StartedSlotRefusal::Expired(expired) = slots.admit_started(deadline).await.unwrap_err()
        else {
            panic!("outside an attempt scope a full target only waits");
        };
        assert_eq!(expired.waited, Duration::from_secs(1));
        drop(slot);
        assert_eq!(slots.available(), 1);
    }

    /// R1: inside one attempt that holds its slot, a second admission is
    /// refused at once (paused clock: no time passes), for a fresh and for a
    /// started attempt, and nothing more is held.
    #[tokio::test(start_paused = true)]
    async fn a_nested_acquire_inside_a_held_attempt_is_refused_at_once() {
        let slots = CodeSlots::new(2);
        let deadline = Instant::now() + MINUTE * 10;
        let required = required_window(1, 1, 2, MINUTE);
        code_attempt_scope(async {
            let held = slots.admit_fresh(deadline, required).await.unwrap();
            let before = Instant::now();
            let nested = slots.admit_fresh(deadline, required).await.unwrap_err();
            assert_eq!(nested.cause, CodeRefusalCause::NestedSlot);
            assert_eq!(
                slots.admit_started(deadline).await.unwrap_err(),
                StartedSlotRefusal::NestedSlot
            );
            // A nested attempt in the same task keeps the outer marker.
            let inner = code_attempt_scope(slots.admit_fresh(deadline, required))
                .await
                .unwrap_err();
            assert_eq!(inner.cause, CodeRefusalCause::NestedSlot);
            assert_eq!(Instant::now(), before, "the refusal never waited");
            assert_eq!(slots.available(), 1, "the refusals hold nothing");
            drop(held);
        })
        .await;
        assert_eq!(slots.available(), 2);
        // Outside any attempt there is no marker: an acquire only waits.
        drop(slots.admit_fresh(deadline, required).await.unwrap());
    }

    /// M1 (#1896): the marker follows the slot. Inside one attempt with one
    /// slot, a slot dropped and then admitted again is admitted (fresh and
    /// started); admitting while still holding it is refused as nested.
    #[tokio::test(start_paused = true)]
    async fn a_slot_dropped_inside_an_attempt_can_be_admitted_again() {
        let slots = CodeSlots::new(1);
        let deadline = Instant::now() + MINUTE * 10;
        let required = required_window(1, 1, 1, MINUTE);
        code_attempt_scope(async {
            let first = slots.admit_fresh(deadline, required).await.unwrap();
            assert_eq!(
                slots
                    .admit_fresh(deadline, required)
                    .await
                    .unwrap_err()
                    .cause,
                CodeRefusalCause::NestedSlot,
                "held: nested"
            );
            drop(first);
            let second = slots
                .admit_fresh(deadline, required)
                .await
                .expect("released: admitted again");
            drop(second);
            let started = slots
                .admit_started(deadline)
                .await
                .expect("released: a started attempt is admitted again");
            assert_eq!(
                slots.admit_started(deadline).await.unwrap_err(),
                StartedSlotRefusal::NestedSlot,
                "held: nested"
            );
            drop(started);
        })
        .await;
        assert_eq!(slots.available(), 1);
    }

    /// C3: two sibling attempts polled by one task, one slot. The marker is
    /// per wrapped attempt future, so the second waits for the first slot and
    /// is never refused as nested.
    #[tokio::test(start_paused = true)]
    async fn sibling_attempts_in_one_task_wait_for_each_other_never_nested() {
        let slots = CodeSlots::new(1);
        let deadline = Instant::now() + MINUTE * 10;
        let required = required_window(1, 1, 1, MINUTE);
        let attempt = || {
            code_attempt_scope(async {
                let slot = slots.admit_fresh(deadline, required).await?;
                tokio::time::sleep(Duration::from_secs(1)).await;
                drop(slot);
                Ok::<_, CodeDeadlineRefusal>(())
            })
        };
        let (first, second) = tokio::join!(attempt(), attempt());
        assert_eq!(first, Ok(()));
        assert_eq!(second, Ok(()));
        assert_eq!(slots.available(), 1);
    }

    /// The marker's documented limit: a future moved to another task does not
    /// carry it, so a spawned acquire waits instead of being refused. The
    /// structural test in the Worker (no activation path calls `admit_*`) is
    /// the backstop.
    #[tokio::test(start_paused = true)]
    async fn the_marker_does_not_follow_a_spawned_task() {
        let slots = CodeSlots::new(1);
        let deadline = Instant::now() + MINUTE * 10;
        let required = required_window(1, 1, 1, MINUTE);
        code_attempt_scope(async {
            let held = slots.admit_fresh(deadline, required).await.unwrap();
            let spawned = slots.clone();
            let waiting =
                tokio::spawn(async move { spawned.admit_fresh(deadline, required).await });
            tokio::time::sleep(Duration::from_secs(1)).await;
            assert!(!waiting.is_finished(), "the spawned acquire waits");
            drop(held);
            let slot = waiting.await.unwrap().unwrap();
            drop(slot);
        })
        .await;
    }

    /// N3: exactly `required` left after preparation is admitted; one
    /// millisecond less is refused with cause `Preparation`.
    #[tokio::test(start_paused = true)]
    async fn the_post_preparation_check_admits_exactly_required_and_refuses_one_ms_less() {
        let required = required_window(1, 1, 1, MINUTE);
        let now = Instant::now();
        assert!(recheck_after_preparation(now + required, required, now).is_ok());
        let refused =
            recheck_after_preparation(now + required - Duration::from_millis(1), required, now)
                .unwrap_err();
        assert_eq!(refused.cause, CodeRefusalCause::Preparation);
        assert_eq!(refused.required, required);
        assert_eq!(
            refused.remaining,
            required.saturating_sub(Duration::from_millis(1))
        );
        assert_eq!(refused.slot_wait, Duration::ZERO);
    }

    #[test]
    fn code_slot_capacity_is_bounded() {
        assert_eq!(CodeSlots::new(0).capacity(), 1);
        assert_eq!(CodeSlots::new(1).capacity(), 1);
        assert_eq!(CodeSlots::new(MAX_CODE_SLOTS).capacity(), MAX_CODE_SLOTS);
        assert_eq!(
            CodeSlots::new(MAX_CODE_SLOTS + 1).capacity(),
            MAX_CODE_SLOTS
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn child_permits_limit_and_limit_plus_one_and_release_on_drop() {
        let permits = ChildPermits::default();
        let mut held = Vec::new();
        for _ in 0..WORKER_CHILD_PERMITS {
            held.push(permits.acquire().await);
        }
        assert_eq!(permits.available(), 0);
        let waiting = permits.clone();
        let next = tokio::spawn(async move { waiting.acquire().await });
        tokio::task::yield_now().await;
        assert!(!next.is_finished(), "the 65th child ran without a permit");
        // A child that pauses drops its permit; the waiting child starts.
        drop(held.pop());
        let permit = tokio::time::timeout(Duration::from_secs(5), next)
            .await
            .expect("released permit admitted the next child")
            .expect("join");
        drop(permit);
        assert_eq!(permits.available(), 1);
    }

    #[tokio::test]
    async fn a_cancelled_acquire_holds_nothing() {
        let permits = ChildPermits::with_capacity(1);
        let held = permits.acquire().await;
        let pending = tokio::time::timeout(Duration::from_millis(10), permits.acquire()).await;
        assert!(pending.is_err());
        drop(held);
        assert_eq!(permits.available(), 1);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn model_lanes_limit_per_model_not_across_models() {
        let limiter = ModelStreamLimiter::default();
        let mut held = Vec::new();
        for _ in 0..MODEL_STREAMS_PER_WORKER {
            held.push(limiter.acquire("model-a").await);
        }
        // Another model is unaffected by model-a's full lane.
        let other = tokio::time::timeout(Duration::from_secs(5), limiter.acquire("model-b"))
            .await
            .expect("model-b has its own lane");
        drop(other);
        let blocked =
            tokio::time::timeout(Duration::from_millis(20), limiter.acquire("model-a")).await;
        assert!(blocked.is_err(), "the 17th stream on one model ran");
        drop(held.pop());
        tokio::time::timeout(Duration::from_secs(5), limiter.acquire("model-a"))
            .await
            .expect("released stream admitted");
    }

    #[tokio::test]
    async fn the_model_table_is_bounded() {
        let limiter = ModelStreamLimiter::default();
        for index in 0..=MAX_TRACKED_MODELS + 3 {
            drop(limiter.acquire(&format!("model-{index}")).await);
        }
        assert_eq!(limiter.tracked_models(), MAX_TRACKED_MODELS);
    }
}
