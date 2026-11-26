//! Watching and stepping into an exchange (ADR-0061 decision 2c, #3058).
//!
//! An exchange is one person's message and every agent turn that follows
//! from it. Main decides the order (the routed `floor.targets` of the
//! message's first start, then the hand-offs Main queued for local
//! `hand_floor` calls, #3064); the host runs it one turn at a time
//! ([`super::floor::next`], `AgentHost::continue_floor`) and keeps here what
//! the person did to it: **Stop exchange** (no later turn of the message
//! starts, and the running one is cancelled) and **Skip** (one queued agent
//! does not answer). The webview never computes the order: it reads the
//! queue from `status` `running`'s `floor.exchange` and the `exchange`
//! event this module's callers send.
//!
//! The queue is a list of [`Queued`] items: the routed agents still to
//! answer, then the hand-offs Main queued, in Main's order. Stop and Skip
//! act on both; a stopped or skipped hand-off is also recorded by Main as
//! skipped (`skipLocalFloorHands` with `stopped_by_person` or
//! `skipped_by_person`): the host's caller sends that.
//!
//! The exchange's wall clock (ADR-0061 decision 3f): an exchange runs at
//! most [`EXCHANGE_WALL_CLOCK`] from its first turn; past it no later turn
//! starts ([`Exchanges::expired`]).

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use super::api::{FloorAgent, FloorStarted, HandedTo};
use super::events::TurnEvents;
use super::floor::FloorNext;

/// `floor_stopped`'s code when the person stopped the exchange.
pub const STOPPED_BY_PERSON: &str = "stopped_by_person";

/// `floor_stopped`'s message when the person stopped the exchange.
pub const STOPPED_MESSAGE: &str = "You stopped the exchange.";

/// `floor_stopped`'s code when the person skipped every agent left.
pub const SKIPPED_BY_PERSON: &str = "skipped_by_person";

/// `floor_stopped`'s message when the person skipped every agent left.
pub const ALL_SKIPPED_MESSAGE: &str = "You skipped the remaining agents.";

/// `floor_stopped`'s code when the exchange ran past its wall clock.
pub const EXCHANGE_TIMED_OUT: &str = "exchange_timed_out";

/// `floor_stopped`'s message (a system line) for [`EXCHANGE_TIMED_OUT`].
pub const TIMED_OUT_MESSAGE: &str =
    "The exchange stopped after 30 minutes. Send a new message to go on.";

/// One exchange's wall clock (ADR-0061 decision 3f).
pub const EXCHANGE_WALL_CLOCK: Duration = Duration::from_secs(30 * 60);

/// The exchanges kept (one per routed message); older ones are forgotten.
const KEPT: usize = 32;

/// One turn still to run in an exchange.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Queued {
    /// The agent at `position` of the message's routed order.
    Routed {
        position: usize,
        participant_id: i64,
    },
    /// A turn Main queued for a local `hand_floor` call (#3064), after the
    /// routed ones.
    Handed {
        hand_id: String,
        participant_id: i64,
    },
}

impl Queued {
    #[must_use]
    pub const fn participant_id(&self) -> i64 {
        match self {
            Self::Routed { participant_id, .. } | Self::Handed { participant_id, .. } => {
                *participant_id
            }
        }
    }
}

/// A hand-off Main queued and the host has not started yet.
#[derive(Clone, Debug)]
struct Hand {
    hand_id: String,
    participant_id: i64,
    name: String,
    from: i64,
}

struct Exchange {
    targets: Vec<i64>,
    agents: Vec<FloorAgent>,
    /// The position of the turn that runs, or ran last.
    position: usize,
    skipped: HashSet<i64>,
    /// The hand-offs still queued, in Main's order.
    hands: Vec<Hand>,
    stopped: bool,
    /// `floor_stopped` went out for the stop.
    announced: bool,
    /// The turn that runs, or ran last, and its events.
    current_turn: String,
    events: Option<Arc<TurnEvents>>,
    /// The consults the message ran so far, as (caller, callee) pairs
    /// (ADR-0061 decision 3f: they count toward the message's hops).
    consults: Vec<(i64, i64)>,
    /// Every consult of the message (thread members or not), for the
    /// per-message limit.
    consult_count: usize,
    started: Instant,
    /// The project of the queued hand-offs (Main's skip route).
    project_id: i64,
}

impl Exchange {
    fn new() -> Self {
        Self {
            targets: Vec::new(),
            agents: Vec::new(),
            position: 0,
            skipped: HashSet::new(),
            hands: Vec::new(),
            stopped: false,
            announced: false,
            current_turn: String::new(),
            events: None,
            consults: Vec::new(),
            consult_count: 0,
            started: Instant::now(),
            project_id: 0,
        }
    }

    fn queue(&self) -> Vec<Queued> {
        if self.stopped {
            return Vec::new();
        }
        self.targets
            .iter()
            .enumerate()
            .skip(self.position + 1)
            .filter(|(_, id)| !self.skipped.contains(id))
            .map(|(position, id)| Queued::Routed {
                position,
                participant_id: *id,
            })
            .chain(self.hands.iter().map(|hand| Queued::Handed {
                hand_id: hand.hand_id.clone(),
                participant_id: hand.participant_id,
            }))
            .collect()
    }

    fn remaining(&self) -> Vec<i64> {
        self.queue().iter().map(Queued::participant_id).collect()
    }

    /// The queue as the UI reads it: each item's agent, and its hand-off.
    fn queue_wire(&self) -> serde_json::Value {
        self.queue()
            .iter()
            .map(|item| match item {
                Queued::Routed { participant_id, .. } => {
                    serde_json::json!({ "participant_id": participant_id })
                }
                Queued::Handed {
                    hand_id,
                    participant_id,
                } => serde_json::json!({ "participant_id": participant_id, "hand_id": hand_id }),
            })
            .collect()
    }

    fn agent(&self, participant_id: i64) -> Option<&FloorAgent> {
        self.agents
            .iter()
            .find(|agent| agent.participant_id == participant_id)
    }
}

#[derive(Default)]
struct Inner {
    by_question: HashMap<String, Exchange>,
    by_turn: HashMap<String, String>,
    order: VecDeque<String>,
}

impl Inner {
    fn exchange(&mut self, question: &str) -> &mut Exchange {
        if !self.by_question.contains_key(question) {
            self.order.push_back(question.to_owned());
            while self.order.len() > KEPT {
                if let Some(old) = self.order.pop_front() {
                    self.by_question.remove(&old);
                    self.by_turn.retain(|_, q| *q != old);
                }
            }
        }
        self.by_question
            .entry(question.to_owned())
            .or_insert_with(Exchange::new)
    }
}

/// What a Stop found to cancel.
pub struct Stopped {
    /// The exchange's turn that runs now (or ran last): cancel it.
    pub turn_id: String,
    /// The person's message.
    pub question_id: String,
    /// The hand-offs Main queued that will not start now: Main records them
    /// `stopped_by_person`.
    pub hands: Vec<String>,
    /// Their project.
    pub project_id: i64,
}

/// What a Skip did.
pub struct Skipped {
    /// The agents still queued.
    pub remaining: Vec<i64>,
    /// The exchange's current turn's events (the `exchange` event goes there).
    pub events: Option<Arc<TurnEvents>>,
    /// The hand-off skipped, when the agent was a queued hand-off: Main
    /// records it `skipped_by_person`. With its message.
    pub hand: Option<(String, String)>,
    /// The hand-off's project.
    pub project_id: i64,
}

/// The person's controls over the host's exchanges.
#[derive(Default)]
pub struct Exchanges {
    inner: Mutex<Inner>,
}

impl Exchanges {
    fn inner(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The turn `turn_id` of a routed message runs at `floor.position`: it
    /// becomes the exchange's current turn. The ids still queued after it.
    pub fn begin(&self, turn_id: &str, floor: &FloorStarted, events: Arc<TurnEvents>) -> Vec<i64> {
        let mut inner = self.inner();
        let question = floor.question_id.clone();
        inner.by_turn.insert(turn_id.to_owned(), question.clone());
        let exchange = inner.exchange(&question);
        exchange.targets.clone_from(&floor.targets);
        exchange.agents.clone_from(&floor.agents);
        exchange.position = floor.position;
        turn_id.clone_into(&mut exchange.current_turn);
        exchange.events = Some(events);
        exchange.remaining()
    }

    /// A handed turn `turn_id` of `question` runs: it becomes the
    /// exchange's current turn. The ids still queued after it.
    pub fn begin_hand(
        &self,
        turn_id: &str,
        question: &str,
        agents: &[FloorAgent],
        events: Arc<TurnEvents>,
    ) -> Vec<i64> {
        let mut inner = self.inner();
        inner
            .by_turn
            .insert(turn_id.to_owned(), question.to_owned());
        let exchange = inner.exchange(question);
        if exchange.agents.is_empty() {
            exchange.agents = agents.to_vec();
        }
        turn_id.clone_into(&mut exchange.current_turn);
        exchange.events = Some(events);
        exchange.remaining()
    }

    /// Main queued `handed` for a `hand_floor` call of `from` in the
    /// exchange of `question` (in Main's order). The ids now queued; `None`
    /// when the exchange was stopped (the caller tells Main they are
    /// skipped).
    pub fn push_hands(
        &self,
        question: &str,
        from: i64,
        project_id: i64,
        handed: &[HandedTo],
    ) -> Option<Vec<i64>> {
        let mut inner = self.inner();
        let exchange = inner.exchange(question);
        if exchange.stopped {
            return None;
        }
        exchange.project_id = project_id;
        exchange.hands.extend(handed.iter().map(|hand| Hand {
            hand_id: hand.hand_id.clone(),
            participant_id: hand.participant_id,
            name: hand.name.clone(),
            from,
        }));
        Some(exchange.remaining())
    }

    /// Hand-offs are queued for the exchange of `question` (#3080: an
    /// unattended turn that handed the floor ends its chain for them).
    #[must_use]
    pub fn has_hands(&self, question: &str) -> bool {
        self.inner()
            .by_question
            .get(question)
            .is_some_and(|exchange| !exchange.stopped && !exchange.hands.is_empty())
    }

    /// The next hand-off of the exchange `template` belongs to, reserved
    /// (it leaves the queue, as [`Self::advance`] reserves a routed agent);
    /// `None` when the exchange was stopped or none is queued.
    #[must_use]
    pub fn next_hand(&self, template: FloorNext) -> Option<FloorNext> {
        let mut inner = self.inner();
        let exchange = inner.by_question.get_mut(&template.question_id)?;
        if exchange.stopped || exchange.hands.is_empty() {
            return None;
        }
        let hand = exchange.hands.remove(0);
        let agent = exchange.agent(hand.participant_id);
        let name = if hand.name.is_empty() {
            agent.map(|agent| agent.name.clone()).unwrap_or_default()
        } else {
            hand.name.clone()
        };
        let cloud = agent.is_some_and(|agent| agent.executor == super::api::EXECUTOR_CLOUD);
        Some(FloorNext {
            position: 0,
            participant_id: hand.participant_id,
            name,
            cloud,
            hand_id: Some(hand.hand_id),
            from: Some(hand.from),
            ..template
        })
    }

    /// The queued hand-offs of the exchange `turn_id` belongs to, as the UI
    /// reads them (`[{participant_id, hand_id}]`).
    #[must_use]
    pub fn hands_of(&self, turn_id: &str) -> serde_json::Value {
        let inner = self.inner();
        inner
            .by_turn
            .get(turn_id)
            .and_then(|question| inner.by_question.get(question))
            .filter(|exchange| !exchange.stopped)
            .map(|exchange| {
                exchange
                    .hands
                    .iter()
                    .map(|hand| {
                        serde_json::json!({
                            "participant_id": hand.participant_id,
                            "hand_id": hand.hand_id,
                        })
                    })
                    .collect()
            })
            .unwrap_or_else(|| serde_json::json!([]))
    }

    /// The hand-offs still queued for `question`, taken from the queue (the
    /// caller tells Main they will not start).
    pub fn drain_hands(&self, question: &str) -> Vec<String> {
        self.inner()
            .by_question
            .get_mut(question)
            .map(|exchange| exchange.hands.drain(..).map(|hand| hand.hand_id).collect())
            .unwrap_or_default()
    }

    /// The participant ids still queued in the exchange `turn_id` belongs to.
    #[must_use]
    pub fn remaining_of(&self, turn_id: &str) -> Vec<i64> {
        let inner = self.inner();
        inner
            .by_turn
            .get(turn_id)
            .and_then(|question| inner.by_question.get(question))
            .map(Exchange::remaining)
            .unwrap_or_default()
    }

    /// The queue of the exchange `turn_id` belongs to, as the UI reads it
    /// (`[{participant_id, hand_id?}]`); `null` when it is in none.
    #[must_use]
    pub fn queue_of(&self, turn_id: &str) -> serde_json::Value {
        let inner = self.inner();
        inner
            .by_turn
            .get(turn_id)
            .and_then(|question| inner.by_question.get(question))
            .map_or(serde_json::Value::Null, Exchange::queue_wire)
    }

    /// Whether the exchange of `question` ran past its wall clock.
    #[must_use]
    pub fn expired(&self, question: &str) -> bool {
        self.inner()
            .by_question
            .get(question)
            .is_some_and(|exchange| exchange.started.elapsed() >= EXCHANGE_WALL_CLOCK)
    }

    /// Age the exchange of `question` by `by` (tests of the wall clock).
    #[cfg(test)]
    pub fn age(&self, question: &str, by: Duration) {
        if let Some(exchange) = self.inner().by_question.get_mut(question)
            && let Some(started) = exchange.started.checked_sub(by)
        {
            exchange.started = started;
        }
    }

    /// Record one consult of the message `question` by `caller` of `callee`
    /// (`None`: a nested agent that is not a thread member). Refused (the
    /// count reached `per_message`) answers false and records nothing.
    pub fn record_consult(
        &self,
        question: &str,
        caller: i64,
        callee: Option<i64>,
        per_message: usize,
    ) -> bool {
        let mut inner = self.inner();
        let exchange = inner.exchange(question);
        if exchange.consult_count >= per_message {
            return false;
        }
        exchange.consult_count += 1;
        if let Some(callee) = callee {
            exchange.consults.push((caller, callee));
        }
        true
    }

    /// The consult pairs the message `question` ran so far.
    #[must_use]
    pub fn consults(&self, question: &str) -> Vec<(i64, i64)> {
        self.inner()
            .by_question
            .get(question)
            .map(|exchange| exchange.consults.clone())
            .unwrap_or_default()
    }

    /// A later turn of `question` started as `turn_id` (before its run
    /// reports): later Stops cancel it. Whether the exchange is stopped, in
    /// which case the caller cancels it (the Stop raced the start).
    pub fn adopt(&self, question: &str, turn_id: &str) -> bool {
        let mut inner = self.inner();
        inner
            .by_turn
            .insert(turn_id.to_owned(), question.to_owned());
        inner.by_question.get_mut(question).is_some_and(|exchange| {
            turn_id.clone_into(&mut exchange.current_turn);
            exchange.stopped
        })
    }

    /// `next` as the person left it: the first agent at or after its
    /// position that was not skipped; `None` when the exchange was stopped
    /// or nobody is left. An exchange the host does not know is unchanged.
    /// The agent picked is reserved (it leaves the queue), so a Skip of it
    /// that arrives before its turn begins is refused (`exchange_not_queued`)
    /// rather than accepted with no effect. A hand-off (`next.hand_id`) was
    /// reserved by [`Self::next_hand`]: it goes on unless the exchange was
    /// stopped since.
    #[must_use]
    pub fn advance(&self, next: FloorNext) -> Option<FloorNext> {
        let mut inner = self.inner();
        let Some(exchange) = inner.by_question.get_mut(&next.question_id) else {
            return Some(next);
        };
        if exchange.stopped {
            return None;
        }
        if next.hand_id.is_some() {
            return Some(next);
        }
        let (position, participant_id) = exchange
            .targets
            .iter()
            .enumerate()
            .skip(next.position)
            .find(|(_, id)| !exchange.skipped.contains(id))
            .map(|(position, id)| (position, *id))?;
        // Reserved: the queue now starts after it.
        exchange.position = position;
        let agent = exchange.agent(participant_id);
        Some(FloorNext {
            position,
            participant_id,
            name: agent.map(|agent| agent.name.clone()).unwrap_or_default(),
            cloud: agent.is_some_and(|agent| agent.executor == super::api::EXECUTOR_CLOUD),
            ..next
        })
    }

    /// The person stopped the exchange of `turn_id`: no later turn of it
    /// starts. The turn to cancel and the hand-offs dropped; `None` when
    /// `turn_id` is in no exchange.
    pub fn stop(&self, turn_id: &str) -> Option<Stopped> {
        let mut inner = self.inner();
        let question = inner.by_turn.get(turn_id)?.clone();
        let exchange = inner.by_question.get_mut(&question)?;
        exchange.stopped = true;
        Some(Stopped {
            turn_id: exchange.current_turn.clone(),
            question_id: question,
            hands: exchange.hands.drain(..).map(|hand| hand.hand_id).collect(),
            project_id: exchange.project_id,
        })
    }

    /// Stop the exchange of `question` for the host's own reason (its wall
    /// clock): no later turn starts. The hand-offs dropped.
    pub fn stop_question(&self, question: &str) -> Vec<String> {
        let mut inner = self.inner();
        inner
            .by_question
            .get_mut(question)
            .map(|exchange| {
                exchange.stopped = true;
                exchange.announced = true;
                exchange.hands.drain(..).map(|hand| hand.hand_id).collect()
            })
            .unwrap_or_default()
    }

    /// Whether the exchange of `question` was stopped.
    #[must_use]
    pub fn stopped(&self, question: &str) -> bool {
        self.inner()
            .by_question
            .get(question)
            .is_some_and(|exchange| exchange.stopped)
    }

    /// Whether `floor_stopped` (`stopped_by_person`) is still owed for the
    /// exchange `turn_id` belongs to: true once, after a Stop.
    pub fn take_announcement(&self, turn_id: &str) -> bool {
        let mut inner = self.inner();
        let Some(question) = inner.by_turn.get(turn_id).cloned() else {
            return false;
        };
        inner
            .by_question
            .get_mut(&question)
            .is_some_and(|exchange| {
                let owed = exchange.stopped && !exchange.announced;
                exchange.announced |= owed;
                owed
            })
    }

    /// The person skipped `participant_id` in the exchange of `turn_id`: a
    /// routed agent still to answer, else its first queued hand-off.
    ///
    /// # Errors
    ///
    /// `exchange_unknown` when `turn_id` is in no exchange,
    /// `exchange_not_queued` when the agent is not queued in it.
    pub fn skip(
        &self,
        turn_id: &str,
        participant_id: i64,
        hand_id: Option<&str>,
    ) -> Result<Skipped, super::turn::TurnError> {
        let mut inner = self.inner();
        let question = inner.by_turn.get(turn_id).cloned();
        let exchange = question
            .as_ref()
            .and_then(|question| inner.by_question.get_mut(question))
            .ok_or_else(|| {
                super::turn::TurnError::new(
                    "exchange_unknown",
                    "This turn is not part of an exchange between agents.",
                )
            })?;
        let routed = !exchange.stopped
            && hand_id.is_none()
            && exchange
                .targets
                .iter()
                .skip(exchange.position + 1)
                .any(|id| *id == participant_id && !exchange.skipped.contains(id));
        let mut hand = None;
        if routed {
            exchange.skipped.insert(participant_id);
        } else if let Some(at) = exchange
            .hands
            .iter()
            .position(|hand| {
                hand.participant_id == participant_id && hand_id.is_none_or(|id| id == hand.hand_id)
            })
            .filter(|_| !exchange.stopped)
        {
            let removed = exchange.hands.remove(at);
            hand = question.map(|question| (removed.hand_id, question));
        } else {
            return Err(super::turn::TurnError::new(
                "exchange_not_queued",
                "That agent is not waiting to answer in this exchange.",
            ));
        }
        Ok(Skipped {
            remaining: exchange.remaining(),
            events: exchange.events.clone(),
            hand,
            project_id: exchange.project_id,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::{TurnEvents, VecEmitter};

    fn floor(position: usize) -> FloorStarted {
        FloorStarted {
            question_id: "q1".into(),
            position,
            targets: vec![77, 88, 99],
            agents: [
                (77, "Coder", "local"),
                (88, "Reviewer", "cloud"),
                (99, "Writer", "cloud"),
            ]
            .into_iter()
            .map(|(participant_id, name, executor)| FloorAgent {
                participant_id,
                name: name.into(),
                executor: executor.into(),
                application_id: None,
            })
            .collect(),
        }
    }

    fn next(position: usize, participant_id: i64) -> FloorNext {
        FloorNext {
            question_id: "q1".into(),
            position,
            participant_id,
            name: String::new(),
            cloud: false,
            workspace_id: "w1".into(),
            project_id: 1,
            conversation_id: serde_json::json!(42),
            conversation_uuid: "u".into(),
            bypass: false,
            full_auto: false,
            plan_mode: false,
            defaults: crate::d0::turn::DefaultVersions::default(),
            hand_id: None,
            from: None,
        }
    }

    fn events() -> Arc<TurnEvents> {
        Arc::new(TurnEvents::new(
            "t1".into(),
            Arc::new(VecEmitter::default()),
        ))
    }

    #[test]
    fn a_skip_drops_one_agent_and_the_next_one_answers() {
        let exchanges = Exchanges::default();
        assert_eq!(exchanges.begin("t1", &floor(0), events()), vec![88, 99]);
        let remaining = exchanges.skip("t1", 88, None).expect("queued").remaining;
        assert_eq!(remaining, vec![99]);
        let advanced = exchanges.advance(next(1, 88)).expect("the third");
        assert_eq!((advanced.position, advanced.participant_id), (2, 99));
        assert_eq!(advanced.name, "Writer");
        assert!(advanced.cloud);
        // Skipping one not queued (the running agent, or twice) is refused.
        assert_eq!(
            exchanges.skip("t1", 77, None).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        assert_eq!(
            exchanges.skip("t1", 88, None).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        assert_eq!(
            exchanges.skip("nope", 99, None).err().map(|e| e.code),
            Some("exchange_unknown".into())
        );
    }

    /// Every agent left skipped: nobody answers.
    #[test]
    fn skipping_everyone_left_ends_the_exchange() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(0), events());
        exchanges.skip("t1", 88, None).expect("queued");
        exchanges.skip("t1", 99, None).expect("queued");
        assert!(exchanges.advance(next(1, 88)).is_none());
    }

    /// The agent `advance` picked is reserved: a Skip of it before its turn
    /// begins is refused, not silently ignored.
    #[test]
    fn the_picked_agent_cannot_be_skipped_before_it_begins() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(0), events());
        let advanced = exchanges.advance(next(1, 88)).expect("the second");
        assert_eq!(advanced.participant_id, 88);
        assert_eq!(
            exchanges.skip("t1", 88, None).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        // The one after it is still queued.
        let remaining = exchanges.skip("t1", 99, None).expect("queued").remaining;
        assert!(remaining.is_empty());
    }

    #[test]
    fn a_stop_ends_the_queue_and_is_announced_once() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(0), events());
        assert!(!exchanges.take_announcement("t1"));
        assert_eq!(
            exchanges.stop("t1").map(|s| s.turn_id).as_deref(),
            Some("t1")
        );
        assert!(exchanges.stopped("q1"));
        assert!(exchanges.advance(next(1, 88)).is_none());
        assert!(exchanges.take_announcement("t1"));
        assert!(!exchanges.take_announcement("t1"));
        // A later turn that raced the stop is cancelled by its starter.
        assert!(exchanges.adopt("q1", "t2"));
        assert!(exchanges.stop("unknown").is_none());
    }

    fn handed(id: &str, participant_id: i64) -> HandedTo {
        HandedTo {
            participant_id,
            name: String::new(),
            hand_id: id.into(),
        }
    }

    /// Main's queued hand-offs come after the routed agents, run in Main's
    /// order, and a Skip of one names its id for Main's skip route.
    #[test]
    fn hand_offs_queue_after_the_routed_agents_and_can_be_skipped() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(1), events());
        let remaining = exchanges
            .push_hands("q1", 77, 1, &[handed("h1", 88), handed("h2", 99)])
            .expect("open");
        assert_eq!(remaining, vec![99, 88, 99]);
        // Skipping 99 skips the routed agent first, then its hand-off.
        assert!(
            exchanges
                .skip("t1", 99, None)
                .expect("routed")
                .hand
                .is_none()
        );
        let skipped = exchanges.skip("t1", 99, None).expect("handed");
        assert_eq!(skipped.hand, Some(("h2".to_owned(), "q1".to_owned())));
        assert_eq!(skipped.remaining, vec![88]);
        let next = exchanges.next_hand(next(0, 0)).expect("the hand-off");
        assert_eq!(
            (
                next.participant_id,
                next.hand_id.as_deref(),
                next.from,
                next.cloud
            ),
            (88, Some("h1"), Some(77), true)
        );
        assert_eq!(next.name, "Reviewer");
        // Reserved: it is gone from the queue, and a Stop does not name it.
        assert!(exchanges.next_hand(next.clone()).is_none());
        assert!(exchanges.advance(next).is_some());
    }

    /// A Stop drops the queued hand-offs (Main records them
    /// `stopped_by_person`), and no later hand-off is queued or started.
    #[test]
    fn a_stop_names_the_queued_hand_offs_and_starts_none() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(2), events());
        exchanges.push_hands("q1", 99, 1, &[handed("h1", 77)]);
        let stopped = exchanges.stop("t1").expect("known");
        assert_eq!(stopped.hands, vec!["h1".to_owned()]);
        assert_eq!(stopped.question_id, "q1");
        assert!(
            exchanges
                .push_hands("q1", 99, 1, &[handed("h2", 88)])
                .is_none()
        );
        assert!(exchanges.next_hand(next(0, 0)).is_none());
        let mut hand = next(0, 77);
        hand.hand_id = Some("h1".into());
        assert!(exchanges.advance(hand).is_none());
    }

    #[test]
    fn consults_count_per_message_and_the_wall_clock_runs_out() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(0), events());
        assert!(exchanges.record_consult("q1", 77, Some(88), 2));
        assert!(exchanges.record_consult("q1", 77, None, 2));
        assert!(!exchanges.record_consult("q1", 77, Some(99), 2));
        assert_eq!(exchanges.consults("q1"), vec![(77, 88)]);
        assert!(!exchanges.expired("q1"));
        exchanges.age("q1", EXCHANGE_WALL_CLOCK);
        assert!(exchanges.expired("q1"));
    }

    #[test]
    fn an_unknown_exchange_leaves_the_next_turn_alone() {
        let exchanges = Exchanges::default();
        let unchanged = exchanges.advance(next(1, 88)).expect("unchanged");
        assert_eq!(unchanged.participant_id, 88);
    }
}
