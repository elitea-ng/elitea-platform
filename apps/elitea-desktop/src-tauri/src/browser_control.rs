//! Who drives a thread's browser pane (ADR-0048 decision 4, #1591): the
//! control pill's state machine, the same rules as the cloud browser's
//! (`libs/rust/browser-session/src/control.rs`).
//!
//! - **Idle**: no agent turn drives the pane. The person browses freely;
//!   their input changes nothing here.
//! - **Agent**: a turn's browser call ran on the pane (from its first call to
//!   the end of the turn, [`PaneControl::agent_started`]).
//! - **You**: the person took over ([`PaneControl::take_over`], or any
//!   input in the page while the agent drives, [`PaneControl::person_input`]),
//!   or the agent asked them to (`browser_request_user`, [`PaneControl::ask`]).
//!
//! The agent's side ([`crate::browser_tools`]): while the person drives,
//! every call answers `user_in_control`, except `browser_snapshot` and
//! `browser_screenshot` when the person ticked **Let the agent watch**; a
//! call during which the person used the page answers `user_active` at once
//! ([`PaneControl::input_after`] wakes it, it does not wait for the call).
//!
//! **Hand back** ([`PaneControl::hand_back`]) returns the page to the agent,
//! with an optional note. The note answers a pending `browser_request_user`
//! (its tool result carries it); after a plain take-over it goes to the
//! turn's steering queue ([`NoteSink`]), so it reaches the conversation as
//! the person's message at the next safe point. A question that nobody
//! answers within [`ASK_WAIT`] (ADR-0045's wait for a local ask) is
//! declined, and the agent gets the page back.

use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use serde::Serialize;
use tokio::sync::watch;

/// How long `browser_request_user` waits for the person: the wait of a
/// local rule's ask (ADR-0045 decision 3). Past it the question is declined.
pub const ASK_WAIT: Duration = crate::d0::unattended::RULE_ASK_WAIT;

/// The most bytes of a hand-back note (the cloud's bound, ADR-0049).
pub const MAX_NOTE_BYTES: usize = 2048;

/// What the pill shows.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Pill {
    #[default]
    Idle,
    Agent,
    /// The person drives (`You`).
    User,
}

/// The control state, as the pane's UI receives it (`browser://event`
/// kind `control`).
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct ControlView {
    pub control: Pill,
    /// The agent's reason, while its `browser_request_user` waits.
    pub reason: Option<String>,
    /// The agent's question waits for the person (Hand back answers it,
    /// Decline declines it).
    pub asked: bool,
    /// **Let the agent watch**: snapshots and screenshots still run while
    /// the person drives.
    pub watch: bool,
}

/// The person's answer to `browser_request_user`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Answer {
    HandedBack(Option<String>),
    Declined,
}

/// Where a take-over's hand-back note goes: the running turn's steering
/// queue (the person's message of the conversation).
pub trait NoteSink: Send + Sync {
    /// `false` when the turn could not take it.
    fn note(&self, text: &str) -> bool;
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
struct State {
    /// An agent turn drives the pane (how many toolsets of the thread
    /// called it: one turn at a time, counted to be safe).
    agent: bool,
    /// The person drives.
    person: bool,
    reason: Option<String>,
    asked: bool,
    watch: bool,
    /// Bumped on every input of the person while the agent drives.
    input_epoch: u64,
    /// Bumped on every answer (hand back, decline, timeout, turn end).
    answer_epoch: u64,
}

impl State {
    fn view(&self) -> ControlView {
        let control = if self.person {
            Pill::User
        } else if self.agent {
            Pill::Agent
        } else {
            Pill::Idle
        };
        ControlView {
            control,
            reason: self.reason.clone(),
            asked: self.asked,
            watch: self.watch,
        }
    }
}

type Notify = Arc<dyn Fn(&ControlView) + Send + Sync>;

/// One pane's control.
pub struct PaneControl {
    state: watch::Sender<State>,
    answer: Mutex<Option<Answer>>,
    sink: Mutex<Option<Arc<dyn NoteSink>>>,
    notify: Mutex<Option<Notify>>,
    ask_wait: Duration,
}

impl Default for PaneControl {
    fn default() -> Self {
        Self::new(ASK_WAIT)
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// Cut `note` (trimmed) to [`MAX_NOTE_BYTES`] at a character boundary;
/// `None` when nothing is left.
#[must_use]
pub fn clean_note(note: Option<&str>) -> Option<String> {
    let note = note?.replace('\0', " ");
    let note = note.trim();
    if note.is_empty() {
        return None;
    }
    let mut end = note.len().min(MAX_NOTE_BYTES);
    while !note.is_char_boundary(end) {
        end -= 1;
    }
    Some(note[..end].to_owned())
}

impl PaneControl {
    #[must_use]
    pub fn new(ask_wait: Duration) -> Self {
        Self {
            state: watch::channel(State::default()).0,
            answer: Mutex::new(None),
            sink: Mutex::new(None),
            notify: Mutex::new(None),
            ask_wait,
        }
    }

    /// Who hears each change (the pane's UI).
    pub fn set_notify(&self, notify: Notify) {
        *lock(&self.notify) = Some(notify);
    }

    #[must_use]
    pub fn view(&self) -> ControlView {
        self.state.borrow().view()
    }

    /// Change the state; the UI hears it when the view changed.
    fn change(&self, work: impl FnOnce(&mut State)) {
        let before = self.state.borrow().view();
        self.state.send_modify(work);
        let after = self.state.borrow().view();
        if before != after {
            let notify = lock(&self.notify).clone();
            if let Some(notify) = notify {
                notify(&after);
            }
        }
    }

    /// The person drives the page now.
    #[must_use]
    pub fn person_drives(&self) -> bool {
        self.state.borrow().person
    }

    /// The person lets the agent watch (snapshots and screenshots).
    #[must_use]
    pub fn watching(&self) -> bool {
        self.state.borrow().watch
    }

    /// The person's input count, for [`Self::input_after`].
    #[must_use]
    pub fn input_epoch(&self) -> u64 {
        self.state.borrow().input_epoch
    }

    /// A turn's browser call is about to run: the agent drives (the pill
    /// says Agent unless the person has the page), and a take-over note
    /// goes to `sink`.
    pub fn agent_started(&self, sink: Option<Arc<dyn NoteSink>>) {
        if let Some(sink) = sink {
            *lock(&self.sink) = Some(sink);
        }
        self.change(|state| state.agent = true);
    }

    /// The turn ended: the pane is idle again, and the person's control,
    /// a pending question and the watch tick end with it.
    pub fn agent_ended(&self) {
        *lock(&self.sink) = None;
        *lock(&self.answer) = None;
        self.change(|state| {
            let epoch = state.answer_epoch + 1;
            *state = State {
                input_epoch: state.input_epoch,
                answer_epoch: epoch,
                ..State::default()
            };
        });
    }

    /// The person clicked, typed or scrolled in the page. While the agent
    /// drives this pauses it: the person drives, and the call in flight
    /// answers `user_active`. While idle it changes nothing.
    pub fn person_input(&self) {
        self.change(|state| {
            if state.agent {
                state.input_epoch += 1;
                state.person = true;
            }
        });
    }

    /// **Take over**: the person drives (only while an agent turn does).
    pub fn take_over(&self) {
        self.change(|state| {
            if state.agent && !state.person {
                state.person = true;
                state.input_epoch += 1;
            }
        });
    }

    /// **Let the agent watch** ticked or not.
    pub fn set_watch(&self, watch: bool) {
        self.change(|state| state.watch = watch);
    }

    /// **Hand back**, with an optional note. Answers a pending question
    /// (the note goes in its tool result); otherwise the note goes to the
    /// turn's steering queue. `true` when the note reached the agent.
    pub fn hand_back(&self, note: Option<&str>) -> bool {
        let note = clean_note(note);
        let asked = self.state.borrow().asked;
        let delivered = if asked {
            *lock(&self.answer) = Some(Answer::HandedBack(note.clone()));
            note.is_some()
        } else {
            let sink = lock(&self.sink).clone();
            match (&note, sink) {
                (Some(text), Some(sink)) => sink.note(text),
                _ => false,
            }
        };
        self.change(|state| {
            state.person = false;
            state.reason = None;
            state.asked = false;
            state.answer_epoch += 1;
        });
        delivered
    }

    /// **Decline** the agent's question: it is told the person did not take
    /// it, and gets the page back.
    pub fn decline(&self) {
        if !self.state.borrow().asked {
            return;
        }
        *lock(&self.answer) = Some(Answer::Declined);
        self.change(|state| {
            state.person = false;
            state.reason = None;
            state.asked = false;
            state.answer_epoch += 1;
        });
    }

    /// Resolves once the person's input count is past `epoch`.
    pub async fn input_after(&self, epoch: u64) {
        let mut changes = self.state.subscribe();
        let _ = changes.wait_for(|state| state.input_epoch > epoch).await;
    }

    /// `browser_request_user`: the person drives with `reason` shown, until
    /// they hand back (with an optional note) or decline, or [`ASK_WAIT`]
    /// passes (declined).
    pub async fn ask(&self, reason: &str) -> Answer {
        *lock(&self.answer) = None;
        let mut changes = self.state.subscribe();
        let mut asked_at = 0;
        let reason = reason.trim().chars().take(500).collect::<String>();
        self.change(|state| {
            state.person = true;
            state.asked = true;
            state.reason = Some(reason);
            asked_at = state.answer_epoch;
        });
        let answered = tokio::time::timeout(
            self.ask_wait,
            changes.wait_for(|state| state.answer_epoch > asked_at),
        )
        .await;
        if let Ok(Ok(_)) = answered {
            return lock(&self.answer).take().unwrap_or(Answer::Declined);
        }
        // Nobody answered in time: the agent gets the page back, declined.
        self.change(|state| {
            if state.asked {
                state.person = false;
                state.reason = None;
                state.asked = false;
                state.answer_epoch += 1;
            }
        });
        Answer::Declined
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Default)]
    struct Notes(Mutex<Vec<String>>);

    impl NoteSink for Notes {
        fn note(&self, text: &str) -> bool {
            lock(&self.0).push(text.to_owned());
            true
        }
    }

    #[test]
    fn idle_until_the_agent_drives_and_input_while_idle_changes_nothing() {
        let control = PaneControl::default();
        assert_eq!(control.view().control, Pill::Idle);
        control.person_input();
        control.take_over();
        assert_eq!(control.view().control, Pill::Idle);
        assert_eq!(control.input_epoch(), 0);
        control.agent_started(None);
        assert_eq!(control.view().control, Pill::Agent);
    }

    #[test]
    fn input_while_the_agent_drives_hands_the_page_to_the_person() {
        let control = PaneControl::default();
        control.agent_started(None);
        control.person_input();
        assert_eq!(control.view().control, Pill::User);
        assert!(control.person_drives());
        assert_eq!(control.input_epoch(), 1);
    }

    #[test]
    fn a_take_over_hand_back_note_goes_to_the_turn() {
        let notes = Arc::new(Notes::default());
        let control = PaneControl::default();
        control.agent_started(Some(notes.clone()));
        control.take_over();
        assert_eq!(control.view().control, Pill::User);
        assert!(control.hand_back(Some("  signed in, continue  ")));
        assert_eq!(control.view().control, Pill::Agent);
        assert_eq!(*lock(&notes.0), ["signed in, continue"]);
        // An empty note is no message.
        control.take_over();
        assert!(!control.hand_back(Some("   ")));
        assert_eq!(lock(&notes.0).len(), 1);
    }

    #[test]
    fn the_turn_end_resets_everything_but_the_input_count() {
        let control = PaneControl::default();
        control.agent_started(Some(Arc::new(Notes::default())));
        control.set_watch(true);
        control.take_over();
        control.agent_ended();
        assert_eq!(control.view(), ControlView::default());
        assert_eq!(control.input_epoch(), 1);
        // No turn: a note has nowhere to go.
        assert!(!control.hand_back(Some("late")));
    }

    #[test]
    fn notes_are_cut_on_a_character_boundary() {
        let long = "é".repeat(MAX_NOTE_BYTES);
        let cut = clean_note(Some(&long)).unwrap();
        assert!(cut.len() <= MAX_NOTE_BYTES);
        assert!(cut.chars().all(|c| c == 'é'));
        assert_eq!(clean_note(Some("a\0b")).as_deref(), Some("a b"));
        assert_eq!(clean_note(None), None);
    }

    #[test]
    fn every_change_of_the_view_is_heard_once() {
        let heard = Arc::new(Mutex::new(Vec::new()));
        let control = PaneControl::default();
        let sink = heard.clone();
        control.set_notify(Arc::new(move |view: &ControlView| {
            lock(&sink).push(view.control);
        }));
        control.agent_started(None);
        control.agent_started(None);
        control.person_input();
        control.person_input();
        control.hand_back(None);
        control.agent_ended();
        assert_eq!(
            *lock(&heard),
            [Pill::Agent, Pill::User, Pill::Agent, Pill::Idle]
        );
    }

    #[tokio::test]
    async fn a_question_is_answered_by_hand_back_with_its_note() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(None);
        let asking = control.clone();
        let question = tokio::spawn(async move { asking.ask("sign in to GitHub").await });
        while !control.view().asked {
            tokio::task::yield_now().await;
        }
        let view = control.view();
        assert_eq!(view.control, Pill::User);
        assert_eq!(view.reason.as_deref(), Some("sign in to GitHub"));
        assert!(control.hand_back(Some("done")));
        assert_eq!(
            question.await.unwrap(),
            Answer::HandedBack(Some("done".to_owned()))
        );
        assert_eq!(control.view().control, Pill::Agent);
        assert_eq!(control.view().reason, None);
    }

    #[tokio::test]
    async fn a_question_is_declined_by_the_person_or_by_its_wait() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(None);
        let asking = control.clone();
        let question = tokio::spawn(async move { asking.ask("captcha").await });
        while !control.view().asked {
            tokio::task::yield_now().await;
        }
        control.decline();
        assert_eq!(question.await.unwrap(), Answer::Declined);
        assert_eq!(control.view().control, Pill::Agent);

        let quick = PaneControl::new(Duration::from_millis(20));
        quick.agent_started(None);
        assert_eq!(quick.ask("mfa").await, Answer::Declined);
        assert_eq!(quick.view().control, Pill::Agent);
        assert!(!quick.view().asked);
        // Decline without a question does nothing.
        quick.decline();
        assert_eq!(quick.view().control, Pill::Agent);
    }

    #[tokio::test]
    async fn input_after_wakes_on_the_next_input_only() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(None);
        let epoch = control.input_epoch();
        let waiting = control.clone();
        let woken = tokio::spawn(async move { waiting.input_after(epoch).await });
        tokio::task::yield_now().await;
        assert!(!woken.is_finished());
        control.person_input();
        tokio::time::timeout(Duration::from_secs(1), woken)
            .await
            .unwrap()
            .unwrap();
    }
}
