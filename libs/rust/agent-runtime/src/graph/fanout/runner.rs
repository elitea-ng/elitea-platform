//! The single-owner fan-out runner shared by Parallel and Map (design §2).
//!
//! One task owns the activation. Children are futures in one bounded
//! `FuturesUnordered`; the loop is one `select!` over:
//! - a child settling (or pausing);
//! - the decision tick, armed **only while a card is open**: 1 s, doubling to
//!   5 s while fetches bring nothing new, back to 1 s when a card opens;
//! - the activation deadline and the stop latch.
//!
//! It never sleeps to poll. It writes nothing to the parent: the planner
//! freezes before and joins after. Per-interrupt decisions are applied through
//! [`super::apply::classify`] against the child's own checkpoint, and acknowledged
//! only after the resumed child saved again.
//!
//! Rules:
//! - A child paused with several cards resumes once all of its cards are
//!   decided (one resume carries that child's whole decision set); siblings
//!   never wait for each other.
//! - The runner parks as soon as nothing can run and at least one card is
//!   open, after one final fetch that brought nothing new.
//! - A restore (any child already paused or settled) fetches once at once,
//!   so a decision applied before a crash is acknowledged even when no card
//!   is open any more. A fresh activation never fetches without a card.
//! - Lease loss is a control stop (`LeaseLost`); the replacement claim
//!   restores everything from checkpoints.
//! - The deadline parks if only paused children remain, otherwise it stops the
//!   running children and fails.
//! - `on_error: stop` closes admission on the first failure; the planner then
//!   fails the node after the running children drain.
//! - Delegated auth (`authorize`, `skip`) applies only at the park boundary
//!   (the delegated-auth spike fallback, design §5.6): the credential reaches
//!   a child through the continuation's command, not under the live claim.
//!   Only an execution's restore fetch delivers these
//!   decisions; live ticks and the final fetch leave them `DECIDED`, so Main
//!   admits the `fanout_wake` continuation that applies them.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use futures::future::BoxFuture;
use futures::stream::{FuturesUnordered, StreamExt};
use tokio::sync::Notify;
use tokio::time::Instant;

use super::admission::ChildPermits;
use super::apply::{ApplyOutcome, CardIdentity, ChildCheckpointView, classify};
use super::plan::{ChildTerminal, OnError, SettledChild};
use crate::graph::fanout_budget::{
    CANCEL_CLEANUP, DECISION_TICK_INITIAL, DECISION_TICK_MAX, MAX_FANOUT_CHILDREN,
    MAX_FANOUT_CONCURRENCY, MAX_FETCH_DECISIONS, MAX_OPEN_INTERRUPTS,
};

/// The owner's stop latch (durable Stop, lease probe, root cancel). Children
/// read [`FanoutStop::is_fired`]; the runner awaits [`FanoutStop::fired`].
#[derive(Default)]
pub struct FanoutStop {
    fired: AtomicBool,
    notify: Notify,
}

impl FanoutStop {
    pub fn fire(&self) {
        self.fired.store(true, Ordering::Release);
        self.notify.notify_waiters();
    }

    #[must_use]
    pub fn is_fired(&self) -> bool {
        self.fired.load(Ordering::Acquire)
    }

    pub async fn fired(&self) {
        let notified = self.notify.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        if self.is_fired() {
            return;
        }
        notified.await;
    }
}

/// One decision as the private fetch returned it (contract §7). The runner
/// never inspects `action`, `value` or `credential_ref`; the child adapter
/// turns them into resume controls.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FetchedDecision {
    pub interrupt_key: String,
    pub request_id: String,
    pub revision: u64,
    pub decision_sha256: String,
    pub action: String,
    pub value: Option<String>,
    pub credential_ref: Option<String>,
}

/// One fetch result.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct DecisionBatch {
    pub decisions: Vec<FetchedDecision>,
    pub decision_revision: u64,
}

/// The ACK outcome (contract §7).
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AckResult {
    Applied { child_checkpoint_id: String },
    Stale,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum DecisionError {
    /// One bounded attempt failed (network, Main busy). Retried by the next
    /// tick or the next fetch; never fatal on its own.
    #[error("graph.fanout.decisions_unavailable")]
    Unavailable,
    /// Main refused the claim or fence: this execution lost its lease.
    #[error("graph.fanout.decisions_lease_lost")]
    LeaseLost,
}

/// Main's decision ledger as the Worker sees it (claim-fenced fetch/ACK).
#[async_trait]
pub trait DecisionSource: Send + Sync {
    async fn fetch(&self) -> Result<DecisionBatch, DecisionError>;
    async fn ack(
        &self,
        decision: &FetchedDecision,
        result: &AckResult,
    ) -> Result<(), DecisionError>;
}

/// A child as its own checkpoint restores it.
#[derive(Clone, Debug, PartialEq)]
pub enum RestoredChild {
    /// Not started, or found mid-run: it must run (the adapter applies the
    /// D2 "may have started" rule for an effectful tool in flight).
    Pending,
    Paused {
        checkpoint_id: String,
        cards: Vec<CardIdentity>,
    },
    Settled(ChildTerminal),
}

/// What one child invocation ended with.
#[derive(Clone, Debug, PartialEq)]
pub enum ChildRun {
    Settled {
        terminal: ChildTerminal,
        checkpoint_id: String,
    },
    Paused {
        checkpoint_id: String,
        cards: Vec<CardIdentity>,
    },
    LeaseLost,
    Cancelled,
}

/// Fan-out member lifecycle (contract §9 `fanout-member-status`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MemberStatus {
    Start,
    End,
    Paused { open_interrupts: usize },
    Failed,
    Cancelled,
}

/// The planner's children (Parallel branches or Map items), in declared order.
#[async_trait]
pub trait FanoutChildren: Send + Sync {
    /// `(key, node)` per child in declared order.
    fn members(&self) -> &[(String, String)];
    /// Restore every child from its own checkpoint (batched by the store).
    async fn restore(&self) -> Result<Vec<RestoredChild>, FanoutRunError>;
    /// The child's latest checkpoint, for the apply proof.
    async fn latest(&self, child: usize) -> Result<ChildCheckpointView, FanoutRunError>;
    /// Run the child: fresh or continued when `decisions` is empty, resumed
    /// with exactly that child's decisions otherwise.
    fn run(&self, child: usize, decisions: Vec<FetchedDecision>) -> BoxFuture<'static, ChildRun>;
    /// The child and its latest checkpoint for a key that is not an open card
    /// but belongs to an earlier pause of one of the children (a decision
    /// applied before a crash whose ACK was lost). `None`: a foreign key.
    async fn answered_pause(
        &self,
        _interrupt_key: &str,
    ) -> Result<Option<(usize, ChildCheckpointView)>, FanoutRunError> {
        Ok(None)
    }
}

/// Frames the runner raises, in order, before it goes on. Card frames are
/// never coalesced. A frame the host cannot deliver stops the run
/// ([`FanoutRunError::FrameUndelivered`]): Main raises a card only from its
/// frame, so an undelivered card could never be decided.
#[async_trait]
pub trait FanoutEvents: Send + Sync {
    async fn card_opened(&self, _child: usize, _card: &CardIdentity) -> Result<(), FanoutRunError> {
        Ok(())
    }
    async fn member(&self, _child: usize, _status: MemberStatus) -> Result<(), FanoutRunError> {
        Ok(())
    }
}

/// No frames (tests, hosts without a stream).
pub struct NoEvents;
impl FanoutEvents for NoEvents {}

/// A ledger with nothing decided: every fetch is empty and every ACK is a
/// no-op. Hosts use it until a claim-fenced decision client is wired, so a
/// paused child parks the activation instead of running without a decision.
pub struct NoDecisions;

#[async_trait]
impl DecisionSource for NoDecisions {
    async fn fetch(&self) -> Result<DecisionBatch, DecisionError> {
        Ok(DecisionBatch::default())
    }

    async fn ack(
        &self,
        _decision: &FetchedDecision,
        _result: &AckResult,
    ) -> Result<(), DecisionError> {
        Ok(())
    }
}

#[derive(Clone, Debug)]
pub struct FanoutRunnerConfig {
    pub max_concurrency: usize,
    pub on_error: OnError,
    pub deadline: Option<Instant>,
}

#[derive(Clone, Debug, PartialEq)]
pub enum RunnerOutcome {
    /// Every admitted child is terminal. Under `on_error: stop` after a
    /// failure, unstarted children are absent; the planner fails the node.
    Settled(Vec<SettledChild>),
    /// Nothing can run; these cards are open. The execution parks.
    Parked {
        open: Vec<CardIdentity>,
        decision_revision_seen: u64,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum FanoutRunError {
    #[error("graph.fanout.lease_lost")]
    LeaseLost,
    #[error("graph.fanout.cancelled")]
    Cancelled,
    #[error("graph.fanout.cancellation_cleanup_failed")]
    CancelCleanupExceeded,
    #[error("graph.fanout.deadline_exceeded")]
    DeadlineExceeded,
    #[error("graph.fanout.too_many_open_interrupts")]
    TooManyOpenInterrupts,
    #[error("graph.fanout.invalid_configuration")]
    InvalidConfiguration,
    #[error("graph.fanout.corrupt_child")]
    CorruptChild,
    #[error("graph.fanout.frame_undelivered")]
    FrameUndelivered,
}

enum Slot {
    Queued(Vec<FetchedDecision>),
    Running,
    Paused {
        cards: Vec<CardIdentity>,
        /// Decisions proven against this pause, waiting for the rest of the
        /// child's cards.
        ready: BTreeMap<String, FetchedDecision>,
    },
    Done(ChildTerminal),
}

/// Backoff of the decision tick (1, 2, 4, 5, 5 … seconds).
struct Tick {
    delay: Duration,
    next: Instant,
}

impl Tick {
    fn reset(now: Instant) -> Self {
        Self {
            delay: DECISION_TICK_INITIAL,
            next: now + DECISION_TICK_INITIAL,
        }
    }

    fn after_fetch(&mut self, now: Instant, brought_news: bool) {
        self.delay = if brought_news {
            DECISION_TICK_INITIAL
        } else {
            (self.delay * 2).min(DECISION_TICK_MAX)
        };
        self.next = now + self.delay;
    }
}

pub struct FanoutRunner {
    children: Arc<dyn FanoutChildren>,
    decisions: Arc<dyn DecisionSource>,
    events: Arc<dyn FanoutEvents>,
    permits: ChildPermits,
    stop: Arc<FanoutStop>,
    config: FanoutRunnerConfig,
}

type Running = FuturesUnordered<BoxFuture<'static, (usize, Vec<FetchedDecision>, ChildRun)>>;

struct Loop {
    slots: Vec<Slot>,
    /// Decisions delivered to a running child, acknowledged when it saves again.
    delivered: BTreeSet<String>,
    /// Decisions for a child that is running: re-examined when it pauses.
    held: BTreeMap<String, FetchedDecision>,
    revision_seen: u64,
    admission_open: bool,
    tick: Option<Tick>,
}

impl FanoutRunner {
    pub fn new(
        children: Arc<dyn FanoutChildren>,
        decisions: Arc<dyn DecisionSource>,
        events: Arc<dyn FanoutEvents>,
        permits: ChildPermits,
        stop: Arc<FanoutStop>,
        config: FanoutRunnerConfig,
    ) -> Result<Self, FanoutRunError> {
        let count = children.members().len();
        if count == 0
            || count > MAX_FANOUT_CHILDREN
            || !(1..=MAX_FANOUT_CONCURRENCY).contains(&config.max_concurrency)
        {
            return Err(FanoutRunError::InvalidConfiguration);
        }
        Ok(Self {
            children,
            decisions,
            events,
            permits,
            stop,
            config,
        })
    }

    /// Run the activation to a join or a park.
    pub async fn run(self) -> Result<RunnerOutcome, FanoutRunError> {
        let restored = self.children.restore().await?;
        if restored.len() != self.children.members().len() {
            return Err(FanoutRunError::CorruptChild);
        }
        // A re-entered occurrence (any child already paused or settled) may
        // hold a decision applied before a crash whose ACK was lost; a fresh
        // activation cannot hold one.
        let reentered = restored
            .iter()
            .any(|child| !matches!(child, RestoredChild::Pending));
        let mut state = Loop {
            slots: Vec::with_capacity(restored.len()),
            delivered: BTreeSet::new(),
            held: BTreeMap::new(),
            revision_seen: 0,
            admission_open: true,
            tick: None,
        };
        for (child, restored) in restored.into_iter().enumerate() {
            state.slots.push(match restored {
                RestoredChild::Pending => Slot::Queued(Vec::new()),
                RestoredChild::Paused { cards, .. } => {
                    // Re-raise: card ingestion is idempotent on the key.
                    for card in &cards {
                        self.events.card_opened(child, card).await?;
                    }
                    Slot::Paused {
                        cards,
                        ready: BTreeMap::new(),
                    }
                }
                RestoredChild::Settled(terminal) => Slot::Done(terminal),
            });
        }
        Self::check_open_cards(&state)?;
        let mut running: Running = FuturesUnordered::new();
        // A restore fetches once, immediately: it applies decisions for open
        // cards (park-boundary auth included) and acknowledges ones applied
        // before a crash.
        if reentered {
            self.fetch_and_apply(&mut state, true).await?;
        }
        if open_cards(&state) > 0 {
            state.tick = Some(Tick::reset(Instant::now()));
        }
        loop {
            if let Err(error) = self.admit(&mut state, &mut running).await {
                return Err(self.drain_after_stop(running, error).await);
            }
            if running.is_empty() && !has_queued(&state) {
                if open_cards(&state) == 0 || !state.admission_open {
                    return Ok(RunnerOutcome::Settled(self.settled(state)));
                }
                // Final fetch before parking; continue only if it made a
                // child runnable (an ACK alone never spins this loop).
                self.fetch_and_apply(&mut state, false).await?;
                if has_queued(&state) {
                    continue;
                }
                return Ok(RunnerOutcome::Parked {
                    open: open_card_list(&state),
                    decision_revision_seen: state.revision_seen,
                });
            }
            let tick_at = state.tick.as_ref().map(|tick| tick.next);
            let armed = tick_at.is_some() && open_cards(&state) > 0;
            let deadline = self.config.deadline;
            tokio::select! {
                biased;
                () = self.stop.fired() => {
                    return Err(self.drain_after_stop(running, FanoutRunError::Cancelled).await);
                }
                () = sleep_until_opt(deadline), if deadline.is_some() => {
                    if running.is_empty() && !has_queued(&state) {
                        continue;
                    }
                    self.stop.fire();
                    return Err(self.drain_after_stop(running, FanoutRunError::DeadlineExceeded).await);
                }
                Some((child, delivered, run)) = running.next() => {
                    if let Some(error) = self.settle(&mut state, child, delivered, run).await? {
                        return Err(self.drain_after_stop(running, error).await);
                    }
                }
                () = sleep_until_opt(tick_at), if armed => {
                    let news = self.fetch_and_apply(&mut state, false).await?;
                    if let Some(tick) = state.tick.as_mut() {
                        tick.after_fetch(Instant::now(), news);
                    }
                }
            }
        }
    }

    fn check_open_cards(state: &Loop) -> Result<(), FanoutRunError> {
        if open_cards(state) > MAX_OPEN_INTERRUPTS {
            return Err(FanoutRunError::TooManyOpenInterrupts);
        }
        Ok(())
    }

    async fn admit(&self, state: &mut Loop, running: &mut Running) -> Result<(), FanoutRunError> {
        let mut in_flight = state
            .slots
            .iter()
            .filter(|slot| matches!(slot, Slot::Running))
            .count();
        for child in 0..state.slots.len() {
            if in_flight >= self.config.max_concurrency {
                break;
            }
            let Slot::Queued(decisions) = &state.slots[child] else {
                continue;
            };
            // `on_error: stop` closes all admission, resumes included.
            if !state.admission_open {
                break;
            }
            let decisions = decisions.clone();
            state.slots[child] = Slot::Running;
            in_flight += 1;
            self.events.member(child, MemberStatus::Start).await?;
            let permits = self.permits.clone();
            let run = self.children.run(child, decisions.clone());
            running.push(Box::pin(async move {
                // The permit covers only the run; a pause releases it.
                let _permit = permits.acquire().await;
                (child, decisions, run.await)
            }));
        }
        Ok(())
    }

    /// Apply one child's result. `Some(error)` is a control stop.
    async fn settle(
        &self,
        state: &mut Loop,
        child: usize,
        delivered: Vec<FetchedDecision>,
        run: ChildRun,
    ) -> Result<Option<FanoutRunError>, FanoutRunError> {
        let checkpoint = match &run {
            ChildRun::Settled { checkpoint_id, .. } | ChildRun::Paused { checkpoint_id, .. } => {
                Some(checkpoint_id.clone())
            }
            ChildRun::LeaseLost | ChildRun::Cancelled => None,
        };
        // The resumed child saved again: ACK what it was resumed with.
        if let Some(checkpoint) = checkpoint {
            for decision in &delivered {
                state.delivered.remove(&decision.interrupt_key);
                self.ack(
                    decision,
                    &AckResult::Applied {
                        child_checkpoint_id: checkpoint.clone(),
                    },
                )
                .await?;
            }
        }
        match run {
            ChildRun::Settled { terminal, .. } => {
                let failed = matches!(terminal, ChildTerminal::Failed(_));
                let status = if failed {
                    MemberStatus::Failed
                } else {
                    MemberStatus::End
                };
                if let Err(error) = self.events.member(child, status).await {
                    return Ok(Some(error));
                }
                if failed && self.config.on_error == OnError::Stop {
                    state.admission_open = false;
                }
                state.slots[child] = Slot::Done(terminal);
                let held = std::mem::take(&mut state.held);
                self.apply_decisions(state, held.into_values().collect())
                    .await?;
                Ok(None)
            }
            ChildRun::Paused { cards, .. } => {
                if cards.is_empty() {
                    return Err(FanoutRunError::CorruptChild);
                }
                for card in &cards {
                    if let Err(error) = self.events.card_opened(child, card).await {
                        return Ok(Some(error));
                    }
                }
                let status = MemberStatus::Paused {
                    open_interrupts: cards.len(),
                };
                if let Err(error) = self.events.member(child, status).await {
                    return Ok(Some(error));
                }
                state.slots[child] = Slot::Paused {
                    cards,
                    ready: BTreeMap::new(),
                };
                Self::check_open_cards(state)?;
                // A new card resets the tick to 1 s.
                state.tick = Some(Tick::reset(Instant::now()));
                // Decisions held while it ran may answer the new pause.
                let held = std::mem::take(&mut state.held);
                self.apply_decisions(state, held.into_values().collect())
                    .await?;
                Ok(None)
            }
            ChildRun::LeaseLost => Ok(Some(FanoutRunError::LeaseLost)),
            ChildRun::Cancelled => {
                // The run stops as cancelled either way.
                let _ = self.events.member(child, MemberStatus::Cancelled).await;
                Ok(Some(FanoutRunError::Cancelled))
            }
        }
    }

    async fn ack(
        &self,
        decision: &FetchedDecision,
        result: &AckResult,
    ) -> Result<(), FanoutRunError> {
        match self.decisions.ack(decision, result).await {
            // A failed ACK is retried by the next fetch: the decision is still
            // DECIDED in Main and classifies as already applied or stale.
            Ok(()) | Err(DecisionError::Unavailable) => Ok(()),
            Err(DecisionError::LeaseLost) => Err(FanoutRunError::LeaseLost),
        }
    }

    /// One claim-fenced fetch and apply. `true` when it changed what can run.
    /// `restore`: the immediate fetch of an execution that restored open
    /// cards, the only fetch that delivers park-boundary (auth) decisions.
    async fn fetch_and_apply(
        &self,
        state: &mut Loop,
        restore: bool,
    ) -> Result<bool, FanoutRunError> {
        let batch = match self.decisions.fetch().await {
            Ok(batch) => batch,
            Err(DecisionError::Unavailable) => return Ok(false),
            Err(DecisionError::LeaseLost) => return Err(FanoutRunError::LeaseLost),
        };
        state.revision_seen = state.revision_seen.max(batch.decision_revision);
        let mut decisions = batch.decisions;
        decisions.truncate(MAX_FETCH_DECISIONS);
        if !restore {
            decisions.retain(|decision| !is_park_boundary_action(&decision.action));
        }
        self.apply_decisions(state, decisions).await
    }

    async fn apply_decisions(
        &self,
        state: &mut Loop,
        decisions: Vec<FetchedDecision>,
    ) -> Result<bool, FanoutRunError> {
        let mut news = false;
        for decision in decisions {
            let key = decision.interrupt_key.clone();
            if state.delivered.contains(&key) {
                // Delivered to a running child; ACKed when it saves.
                continue;
            }
            let Some((child, card)) = find_card(state, &key) else {
                // Not an open card: an earlier, already answered pause of a
                // child (ACK lost in a crash), or a foreign key.
                match self.children.answered_pause(&key).await? {
                    Some((_, ChildCheckpointView::Running { .. })) => {
                        news |= state.held.insert(key, decision).is_none();
                    }
                    Some((_, latest)) => {
                        news |= self.ack_classified(&decision, None, &latest).await?;
                    }
                    None => {
                        self.ack(&decision, &AckResult::Stale).await?;
                        news = true;
                    }
                }
                continue;
            };
            let latest = self.children.latest(child).await?;
            match classify(Some(&card), &latest) {
                ApplyOutcome::Apply => {
                    if let Slot::Paused { ready, .. } = &mut state.slots[child]
                        && ready.insert(key, decision).is_none()
                    {
                        news = true;
                    }
                }
                ApplyOutcome::Held => {
                    news |= state.held.insert(key, decision).is_none();
                }
                ApplyOutcome::Stale => {
                    // The child's checkpoint contradicts its open card: it can
                    // never be answered. Fail typed instead of parking forever.
                    self.ack(&decision, &AckResult::Stale).await?;
                    return Err(FanoutRunError::CorruptChild);
                }
                outcome @ ApplyOutcome::AlreadyApplied { .. } => {
                    news |= self
                        .ack_classified(&decision, Some(outcome), &latest)
                        .await?;
                    // Already past this pause: continue from its checkpoint.
                    state.slots[child] = Slot::Queued(Vec::new());
                }
            }
        }
        // Resume every paused child whose cards are all decided.
        for child in 0..state.slots.len() {
            let Slot::Paused { cards, ready } = &mut state.slots[child] else {
                continue;
            };
            if cards
                .iter()
                .all(|card| ready.contains_key(&card.interrupt_key))
            {
                let decisions = cards
                    .iter()
                    .filter_map(|card| ready.remove(&card.interrupt_key))
                    .collect::<Vec<_>>();
                for decision in &decisions {
                    state.delivered.insert(decision.interrupt_key.clone());
                }
                state.slots[child] = Slot::Queued(decisions);
                news = true;
            }
        }
        Ok(news)
    }

    async fn ack_classified(
        &self,
        decision: &FetchedDecision,
        outcome: Option<ApplyOutcome>,
        latest: &ChildCheckpointView,
    ) -> Result<bool, FanoutRunError> {
        let outcome = outcome.unwrap_or_else(|| match latest {
            ChildCheckpointView::Paused { checkpoint_id, .. }
            | ChildCheckpointView::Terminal { checkpoint_id } => ApplyOutcome::AlreadyApplied {
                child_checkpoint_id: checkpoint_id.clone(),
            },
            ChildCheckpointView::Running { .. } => ApplyOutcome::Held,
        });
        match outcome {
            ApplyOutcome::AlreadyApplied {
                child_checkpoint_id,
            } => {
                self.ack(
                    decision,
                    &AckResult::Applied {
                        child_checkpoint_id,
                    },
                )
                .await?;
                Ok(true)
            }
            ApplyOutcome::Stale => {
                self.ack(decision, &AckResult::Stale).await?;
                Ok(true)
            }
            ApplyOutcome::Apply | ApplyOutcome::Held => Ok(false),
        }
    }

    /// Stop running children and bound their cleanup.
    async fn drain_after_stop(
        &self,
        mut running: Running,
        error: FanoutRunError,
    ) -> FanoutRunError {
        self.stop.fire();
        let drained = tokio::time::timeout(CANCEL_CLEANUP, async {
            while running.next().await.is_some() {}
        })
        .await;
        if drained.is_err() && error == FanoutRunError::Cancelled {
            return FanoutRunError::CancelCleanupExceeded;
        }
        error
    }

    fn settled(&self, state: Loop) -> Vec<SettledChild> {
        let members = self.children.members();
        state
            .slots
            .into_iter()
            .zip(members)
            .filter_map(|(slot, (key, node))| match slot {
                Slot::Done(terminal) => Some(SettledChild {
                    key: key.clone(),
                    node: node.clone(),
                    terminal,
                }),
                _ => None,
            })
            .collect()
    }
}

/// Decisions applied only when an execution restores (delegated auth).
fn is_park_boundary_action(action: &str) -> bool {
    matches!(action, "authorize" | "skip")
}

fn find_card(state: &Loop, key: &str) -> Option<(usize, CardIdentity)> {
    state
        .slots
        .iter()
        .enumerate()
        .find_map(|(child, slot)| match slot {
            Slot::Paused { cards, .. } => cards
                .iter()
                .find(|card| card.interrupt_key == key)
                .map(|card| (child, card.clone())),
            _ => None,
        })
}

fn open_cards(state: &Loop) -> usize {
    state
        .slots
        .iter()
        .map(|slot| match slot {
            Slot::Paused { cards, .. } => cards.len(),
            _ => 0,
        })
        .sum()
}

fn open_card_list(state: &Loop) -> Vec<CardIdentity> {
    state
        .slots
        .iter()
        .flat_map(|slot| match slot {
            Slot::Paused { cards, .. } => cards.clone(),
            _ => Vec::new(),
        })
        .collect()
}

fn has_queued(state: &Loop) -> bool {
    state.admission_open
        && state
            .slots
            .iter()
            .any(|slot| matches!(slot, Slot::Queued(_)))
}

async fn sleep_until_opt(at: Option<Instant>) {
    match at {
        Some(at) => tokio::time::sleep_until(at).await,
        None => std::future::pending().await,
    }
}

#[cfg(test)]
#[path = "runner_tests.rs"]
mod tests;
