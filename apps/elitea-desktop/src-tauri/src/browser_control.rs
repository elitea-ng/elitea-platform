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
//! answers within [`ASK_WAIT`] is declined, and the agent gets the page
//! back. Whether a hand back answers a question is decided in the same
//! state change that ends the question, so a question asked while the
//! person hands back is never lost: it either takes the note or stays on
//! screen.
//!
//! **One turn owns the control** (the mechanism is `crate::control_state`,
//! shared with the computer session). Each turn's toolset has a [`TurnId`];
//! the latest turn to call the pane owns the pill and the note sink, and
//! only the owner's end ([`PaneControl::agent_ended`]) resets them. An
//! older turn of the thread finishing late changes nothing.

use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::Serialize;

use crate::control_state::{Observed, Viewed, lock};
pub use crate::control_state::{TurnId, next_turn_id};

/// How long `browser_request_user` waits for the person. ADR-0048 says only
/// "ADR-0045 timeout and default", and ADR-0045's table has no row for it;
/// the agent's turn is blocked meanwhile, so it gets a wait of its own,
/// 10 minutes (the step-limit question's), not a rule ask's 30. Past it the
/// question is declined. The pane shows the deadline.
pub const ASK_WAIT: Duration = Duration::from_mins(10);

/// Answers kept for `ask`s that have not taken theirs yet.
const MAX_ANSWERS: usize = 8;

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
    /// When the agent's question is declined unless answered (Unix ms).
    pub deadline_ms: Option<u64>,
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
    /// The turn that drives the pane (the latest to call it); `None`: idle.
    owner: Option<TurnId>,
    /// The person drives.
    person: bool,
    reason: Option<String>,
    deadline_ms: Option<u64>,
    /// The pending question's number; `None` when none waits.
    asked: Option<u64>,
    watch: bool,
    /// Bumped on every input of the person while the agent drives.
    input_epoch: u64,
    /// The last question's number (kept across turns).
    questions: u64,
    /// Answers by question number, until their `ask` takes them.
    answers: Vec<(u64, Answer)>,
}

impl State {
    /// The pending question ends with `answer`; the agent drives again.
    fn answer(&mut self, answer: Answer) {
        if let Some(question) = self.asked.take() {
            self.answers.push((question, answer));
            if self.answers.len() > MAX_ANSWERS {
                self.answers.remove(0);
            }
        }
        self.person = false;
        self.reason = None;
        self.deadline_ms = None;
    }

    /// Idle: only the counters and untaken answers stay; a pending
    /// question is declined.
    fn reset(&mut self) {
        self.answer(Answer::Declined);
        *self = State {
            input_epoch: self.input_epoch,
            questions: self.questions,
            answers: std::mem::take(&mut self.answers),
            ..State::default()
        };
    }
}

impl Viewed for State {
    type View = ControlView;

    fn view(&self) -> ControlView {
        let control = if self.person {
            Pill::User
        } else if self.owner.is_some() {
            Pill::Agent
        } else {
            Pill::Idle
        };
        ControlView {
            control,
            reason: self.reason.clone(),
            asked: self.asked.is_some(),
            watch: self.watch,
            deadline_ms: self.deadline_ms,
        }
    }
}

type Notify = crate::control_state::Notify<ControlView>;

/// One pane's control.
pub struct PaneControl {
    state: Observed<State>,
    /// The owner turn's note sink.
    sink: Mutex<Option<(TurnId, Arc<dyn NoteSink>)>>,
    ask_wait: Duration,
}

impl Default for PaneControl {
    fn default() -> Self {
        Self::new(ASK_WAIT)
    }
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
            state: Observed::new(State::default()),
            sink: Mutex::new(None),
            ask_wait,
        }
    }

    /// Who hears each change (the pane's UI).
    pub fn set_notify(&self, notify: Notify) {
        self.state.set_notify(notify);
    }

    #[must_use]
    pub fn view(&self) -> ControlView {
        self.state.view()
    }

    fn change<R>(&self, work: impl FnOnce(&mut State) -> R) -> R {
        self.state.change(work)
    }

    /// The person drives the page now.
    #[must_use]
    pub fn person_drives(&self) -> bool {
        self.state.read(|state| state.person)
    }

    /// The person lets the agent watch (snapshots and screenshots).
    #[must_use]
    pub fn watching(&self) -> bool {
        self.state.read(|state| state.watch)
    }

    /// The person's input count, for [`Self::input_after`].
    #[must_use]
    pub fn input_epoch(&self) -> u64 {
        self.state.read(|state| state.input_epoch)
    }

    /// Turn `turn`'s browser call is about to run: it owns the control
    /// now (the pill says Agent unless the person has the page), and a
    /// take-over note goes to its `sink`.
    pub fn agent_started(&self, turn: TurnId, sink: Option<Arc<dyn NoteSink>>) {
        *lock(&self.sink) = sink.map(|sink| (turn, sink));
        self.change(|state| state.owner = Some(turn));
    }

    /// Turn `turn` ended: when it owns the control, the pane is idle again,
    /// and the person's control, a pending question (declined) and the
    /// watch tick end with it. Another turn's end changes nothing.
    pub fn agent_ended(&self, turn: TurnId) {
        {
            let mut sink = lock(&self.sink);
            if sink.as_ref().is_some_and(|(owner, _)| *owner == turn) {
                *sink = None;
            }
        }
        self.change(|state| {
            if state.owner == Some(turn) {
                state.reset();
            }
        });
    }

    /// Every turn ends (sign-out): the pane is idle.
    pub fn reset(&self) {
        *lock(&self.sink) = None;
        self.change(State::reset);
    }

    /// The person clicked, typed or scrolled in the page. While the agent
    /// drives this pauses it: the person drives, and the call in flight
    /// answers `user_active`. While idle it changes nothing.
    pub fn person_input(&self) {
        self.change(|state| {
            if state.owner.is_some() {
                state.input_epoch += 1;
                state.person = true;
            }
        });
    }

    /// **Take over**: the person drives (only while an agent turn does).
    pub fn take_over(&self) {
        self.change(|state| {
            if state.owner.is_some() && !state.person {
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
        // One step decides whether a question waits and answers it, so a
        // question asked meanwhile is either answered by this note or
        // still on screen afterwards, never declined unseen.
        let answered = self.change(|state| {
            if state.asked.is_some() {
                state.answer(Answer::HandedBack(note.clone()));
                true
            } else {
                state.person = false;
                false
            }
        });
        if answered {
            return note.is_some();
        }
        let sink = lock(&self.sink).as_ref().map(|(_, sink)| sink.clone());
        match (&note, sink) {
            (Some(text), Some(sink)) => sink.note(text),
            _ => false,
        }
    }

    /// **Decline** the agent's question: it is told the person did not take
    /// it, and gets the page back.
    pub fn decline(&self) {
        self.change(|state| {
            if state.asked.is_some() {
                state.answer(Answer::Declined);
            }
        });
    }

    /// Resolves once the person's input count is past `epoch`.
    pub async fn input_after(&self, epoch: u64) {
        self.state.wait_for(|state| state.input_epoch > epoch).await;
    }

    /// `browser_request_user`: the person drives with `reason` shown, until
    /// they hand back (with an optional note) or decline, or [`ASK_WAIT`]
    /// passes (declined).
    pub async fn ask(&self, reason: &str) -> Answer {
        let reason = reason.trim().chars().take(500).collect::<String>();
        let deadline = SystemTime::now()
            .checked_add(self.ask_wait)
            .and_then(|at| at.duration_since(UNIX_EPOCH).ok())
            .and_then(|at| u64::try_from(at.as_millis()).ok());
        let question = self.change(|state| {
            // A question still waiting is superseded: declined.
            state.answer(Answer::Declined);
            state.questions += 1;
            state.person = true;
            state.asked = Some(state.questions);
            state.reason = Some(reason);
            state.deadline_ms = deadline;
            state.questions
        });
        let answered = |state: &State| state.asked != Some(question);
        let _ = tokio::time::timeout(self.ask_wait, self.state.wait_for(answered)).await;
        // Nobody answered in time: the agent gets the page back, declined.
        self.change(|state| {
            if state.asked == Some(question) {
                state.answer(Answer::Declined);
            }
            let at = state
                .answers
                .iter()
                .position(|(number, _)| *number == question);
            at.map_or(Answer::Declined, |at| state.answers.remove(at).1)
        })
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
        control.agent_started(1, None);
        assert_eq!(control.view().control, Pill::Agent);
    }

    #[test]
    fn input_while_the_agent_drives_hands_the_page_to_the_person() {
        let control = PaneControl::default();
        control.agent_started(1, None);
        control.person_input();
        assert_eq!(control.view().control, Pill::User);
        assert!(control.person_drives());
        assert_eq!(control.input_epoch(), 1);
    }

    #[test]
    fn a_take_over_hand_back_note_goes_to_the_turn() {
        let notes = Arc::new(Notes::default());
        let control = PaneControl::default();
        control.agent_started(1, Some(notes.clone()));
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
        control.agent_started(1, Some(Arc::new(Notes::default())));
        control.set_watch(true);
        control.take_over();
        control.agent_ended(1);
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
        control.agent_started(1, None);
        control.agent_started(1, None);
        control.person_input();
        control.person_input();
        control.hand_back(None);
        control.agent_ended(1);
        assert_eq!(
            *lock(&heard),
            [Pill::Agent, Pill::User, Pill::Agent, Pill::Idle]
        );
    }

    #[tokio::test]
    async fn a_question_is_answered_by_hand_back_with_its_note() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(1, None);
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
        control.agent_started(1, None);
        let asking = control.clone();
        let question = tokio::spawn(async move { asking.ask("captcha").await });
        while !control.view().asked {
            tokio::task::yield_now().await;
        }
        control.decline();
        assert_eq!(question.await.unwrap(), Answer::Declined);
        assert_eq!(control.view().control, Pill::Agent);

        let quick = PaneControl::new(Duration::from_millis(20));
        quick.agent_started(1, None);
        assert_eq!(quick.ask("mfa").await, Answer::Declined);
        assert_eq!(quick.view().control, Pill::Agent);
        assert!(!quick.view().asked);
        // Decline without a question does nothing.
        quick.decline();
        assert_eq!(quick.view().control, Pill::Agent);
    }

    /// A note sink that starts the agent's next question while the hand
    /// back is under way (the race of review finding 1): the question's
    /// future is polled once, so its state change lands in the middle.
    struct AskingSink {
        control: Arc<PaneControl>,
        question:
            Mutex<Option<std::pin::Pin<Box<dyn std::future::Future<Output = Answer> + Send>>>>,
    }

    impl NoteSink for AskingSink {
        fn note(&self, _text: &str) -> bool {
            let control = self.control.clone();
            let mut question: std::pin::Pin<Box<dyn std::future::Future<Output = Answer> + Send>> =
                Box::pin(async move { control.ask("enter the MFA code").await });
            let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
            assert!(question.as_mut().poll(&mut cx).is_pending());
            *lock(&self.question) = Some(question);
            true
        }
    }

    #[tokio::test]
    async fn a_question_asked_during_a_hand_back_stays_on_screen_and_takes_its_own_answer() {
        let control = Arc::new(PaneControl::default());
        let sink = Arc::new(AskingSink {
            control: control.clone(),
            question: Mutex::new(None),
        });
        control.agent_started(1, Some(sink.clone()));
        control.take_over();
        assert!(control.hand_back(Some("added it, go on")));
        let view = control.view();
        assert!(view.asked, "the new question was swallowed: {view:?}");
        assert_eq!(view.control, Pill::User);
        assert_eq!(view.reason.as_deref(), Some("enter the MFA code"));
        let question = lock(&sink.question).take().unwrap();
        control.hand_back(Some("123456 entered"));
        assert_eq!(
            question.await,
            Answer::HandedBack(Some("123456 entered".to_owned()))
        );
    }

    #[test]
    fn only_the_turn_that_owns_the_control_ends_it() {
        let older = Arc::new(Notes::default());
        let newer = Arc::new(Notes::default());
        let control = PaneControl::default();
        control.agent_started(1, Some(older.clone()));
        control.agent_started(2, Some(newer.clone()));
        control.take_over();
        // The older turn's tools go away late: the newer turn keeps its
        // pill, its person's control and its note sink.
        control.agent_ended(1);
        assert_eq!(control.view().control, Pill::User);
        assert!(control.hand_back(Some("over to you")));
        assert!(lock(&older.0).is_empty());
        assert_eq!(*lock(&newer.0), ["over to you"]);
        control.agent_ended(2);
        assert_eq!(control.view().control, Pill::Idle);
    }

    #[tokio::test]
    async fn the_turn_end_declines_its_pending_question() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(7, None);
        let asking = control.clone();
        let question = tokio::spawn(async move { asking.ask("sign in").await });
        while !control.view().asked {
            tokio::task::yield_now().await;
        }
        control.agent_ended(7);
        assert_eq!(question.await.unwrap(), Answer::Declined);
        assert_eq!(control.view(), ControlView::default());
    }

    #[tokio::test]
    async fn a_question_waits_ten_minutes_and_shows_its_deadline() {
        assert_eq!(ASK_WAIT, Duration::from_mins(10));
        let control = Arc::new(PaneControl::default());
        control.agent_started(1, None);
        let before = SystemTime::now().duration_since(UNIX_EPOCH).unwrap();
        let asking = control.clone();
        let question = tokio::spawn(async move { asking.ask("captcha").await });
        while !control.view().asked {
            tokio::task::yield_now().await;
        }
        let deadline = control.view().deadline_ms.expect("a deadline while asked");
        let wait = u64::try_from(ASK_WAIT.as_millis()).unwrap();
        let from = u64::try_from(before.as_millis()).unwrap();
        assert!(
            (from + wait..from + wait + 5_000).contains(&deadline),
            "{deadline}"
        );
        control.decline();
        assert_eq!(question.await.unwrap(), Answer::Declined);
        assert_eq!(control.view().deadline_ms, None);
    }

    #[tokio::test]
    async fn input_after_wakes_on_the_next_input_only() {
        let control = Arc::new(PaneControl::default());
        control.agent_started(1, None);
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
