//! Watching and stepping into an exchange (ADR-0061 decision 2c, #3058).
//!
//! An exchange is one person's message and every agent turn that follows
//! from it. Main decides the order (the routed `floor.targets` of the
//! message's first start); the host runs it one turn at a time
//! ([`super::floor::next`], `AgentHost::continue_floor`) and keeps here what
//! the person did to it: **Stop exchange** (no later turn of the message
//! starts, and the running one is cancelled) and **Skip** (one queued agent
//! does not answer). The webview never computes the order: it reads the
//! queue from `status` `running`'s `floor.exchange` and the `exchange`
//! event this module's callers send.
//!
//! The queue is a list of [`Queued`] items so the local hand-offs of
//! ADR-0061 decision 3 (#3064) can add the turns Main queues for a
//! `hand_floor` call: Stop and Skip then act on them the same way (Main also
//! records a stopped hand-off as skipped with `stopped_by_person`).

use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex};

use super::api::{FloorAgent, FloorStarted};
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
    // #3064 adds `Handed { hand_turn_id, participant_id }`: a turn Main
    // queued for a local `hand_floor` call, after the routed ones.
}

impl Queued {
    #[must_use]
    pub const fn participant_id(&self) -> i64 {
        match self {
            Self::Routed { participant_id, .. } => *participant_id,
        }
    }
}

struct Exchange {
    targets: Vec<i64>,
    agents: Vec<FloorAgent>,
    /// The position of the turn that runs, or ran last.
    position: usize,
    skipped: HashSet<i64>,
    stopped: bool,
    /// `floor_stopped` went out for the stop.
    announced: bool,
    /// The turn that runs, or ran last, and its events.
    current_turn: String,
    events: Option<Arc<TurnEvents>>,
}

impl Exchange {
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
            .collect()
    }

    fn remaining(&self) -> Vec<i64> {
        self.queue().iter().map(Queued::participant_id).collect()
    }
}

#[derive(Default)]
struct Inner {
    by_question: HashMap<String, Exchange>,
    by_turn: HashMap<String, String>,
    order: VecDeque<String>,
}

/// What a Stop found to cancel.
pub struct Stopped {
    /// The exchange's turn that runs now (or ran last): cancel it.
    pub turn_id: String,
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
        if !inner.by_question.contains_key(&question) {
            inner.order.push_back(question.clone());
            while inner.order.len() > KEPT {
                if let Some(old) = inner.order.pop_front() {
                    inner.by_question.remove(&old);
                    inner.by_turn.retain(|_, q| *q != old);
                }
            }
        }
        inner.by_turn.insert(turn_id.to_owned(), question.clone());
        let exchange = inner
            .by_question
            .entry(question)
            .or_insert_with(|| Exchange {
                targets: Vec::new(),
                agents: Vec::new(),
                position: 0,
                skipped: HashSet::new(),
                stopped: false,
                announced: false,
                current_turn: String::new(),
                events: None,
            });
        exchange.targets.clone_from(&floor.targets);
        exchange.agents.clone_from(&floor.agents);
        exchange.position = floor.position;
        turn_id.clone_into(&mut exchange.current_turn);
        exchange.events = Some(events);
        exchange.remaining()
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
    /// rather than accepted with no effect.
    #[must_use]
    pub fn advance(&self, next: FloorNext) -> Option<FloorNext> {
        let mut inner = self.inner();
        let Some(exchange) = inner.by_question.get_mut(&next.question_id) else {
            return Some(next);
        };
        if exchange.stopped {
            return None;
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
        let agent = exchange
            .agents
            .iter()
            .find(|agent| agent.participant_id == participant_id);
        Some(FloorNext {
            position,
            participant_id,
            name: agent.map(|agent| agent.name.clone()).unwrap_or_default(),
            cloud: agent.is_some_and(|agent| agent.executor == super::api::EXECUTOR_CLOUD),
            ..next
        })
    }

    /// The person stopped the exchange of `turn_id`: no later turn of it
    /// starts. The turn to cancel; `None` when `turn_id` is in no exchange.
    pub fn stop(&self, turn_id: &str) -> Option<Stopped> {
        let mut inner = self.inner();
        let question = inner.by_turn.get(turn_id)?.clone();
        let exchange = inner.by_question.get_mut(&question)?;
        exchange.stopped = true;
        Some(Stopped {
            turn_id: exchange.current_turn.clone(),
        })
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

    /// The person skipped `participant_id` in the exchange of `turn_id`:
    /// the ids still queued and the events of the exchange's current turn
    /// (to send the `exchange` event on).
    ///
    /// # Errors
    ///
    /// `exchange_unknown` when `turn_id` is in no exchange,
    /// `exchange_not_queued` when the agent is not queued in it.
    pub fn skip(
        &self,
        turn_id: &str,
        participant_id: i64,
    ) -> Result<(Vec<i64>, Option<Arc<TurnEvents>>), super::turn::TurnError> {
        let mut inner = self.inner();
        let exchange = inner
            .by_turn
            .get(turn_id)
            .cloned()
            .and_then(|question| inner.by_question.get_mut(&question))
            .ok_or_else(|| {
                super::turn::TurnError::new(
                    "exchange_unknown",
                    "This turn is not part of an exchange between agents.",
                )
            })?;
        if !exchange.remaining().contains(&participant_id) {
            return Err(super::turn::TurnError::new(
                "exchange_not_queued",
                "That agent is not waiting to answer in this exchange.",
            ));
        }
        exchange.skipped.insert(participant_id);
        Ok((exchange.remaining(), exchange.events.clone()))
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
            plan_mode: false,
            defaults: crate::d0::turn::DefaultVersions::default(),
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
        let (remaining, _) = exchanges.skip("t1", 88).expect("queued");
        assert_eq!(remaining, vec![99]);
        let advanced = exchanges.advance(next(1, 88)).expect("the third");
        assert_eq!((advanced.position, advanced.participant_id), (2, 99));
        assert_eq!(advanced.name, "Writer");
        assert!(advanced.cloud);
        // Skipping one not queued (the running agent, or twice) is refused.
        assert_eq!(
            exchanges.skip("t1", 77).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        assert_eq!(
            exchanges.skip("t1", 88).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        assert_eq!(
            exchanges.skip("nope", 99).err().map(|e| e.code),
            Some("exchange_unknown".into())
        );
    }

    /// Every agent left skipped: nobody answers.
    #[test]
    fn skipping_everyone_left_ends_the_exchange() {
        let exchanges = Exchanges::default();
        exchanges.begin("t1", &floor(0), events());
        exchanges.skip("t1", 88).expect("queued");
        exchanges.skip("t1", 99).expect("queued");
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
            exchanges.skip("t1", 88).err().map(|e| e.code),
            Some("exchange_not_queued".into())
        );
        // The one after it is still queued.
        let (remaining, _) = exchanges.skip("t1", 99).expect("queued");
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

    #[test]
    fn an_unknown_exchange_leaves_the_next_turn_alone() {
        let exchanges = Exchanges::default();
        let unchanged = exchanges.advance(next(1, 88)).expect("unchanged");
        assert_eq!(unchanged.participant_id, 88);
    }
}
