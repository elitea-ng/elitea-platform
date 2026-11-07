//! The person is always in charge of a computer session (ADR-0047
//! decision 6, #1584): the session panel's state machine.
//!
//! It is the browser control pill's machine (`browser_control`) adapted to
//! the whole desktop, on the same mechanism (`crate::control_state`: one
//! step per change, the UI told only when the view changed, calls woken
//! when the person acts):
//!
//! - **Idle**: no turn holds a computer session. Input changes nothing.
//! - **Active**: a turn's `computer` call ran ([`ComputerControl::begin`],
//!   until the turn ends, [`ComputerControl::end`]). The panel shows the app
//!   and the last action ([`ComputerControl::acted`]) with **Pause** and
//!   **Stop**, and cannot be hidden.
//! - **Paused**: the person pressed **Pause**, or used the keyboard or the
//!   pointer ([`ComputerControl::person_input`], the user-active guard). A
//!   call in flight answers `user_active` at once; every later call answers
//!   `user_active` too, until the person presses **Resume** — only Resume
//!   lifts it, never time or the agent. The panel says "Paused, you are
//!   using the computer" for input, "Paused" for the button.
//! - **Stopped**: **Stop**, or the stop shortcut (Control-Option-Escape).
//!   The pending call and every later call of the turn answer
//!   `stopped_by_user`: the session is over and the turn goes on without the
//!   tools. The next turn starts Idle.
//!
//! **One desktop, one session.** Two threads may use the computer at once:
//! each turn joins the session ([`ComputerControl::begin`]) and leaves it
//! when it ends; the session is Idle again only when the last one left. What
//! the person pressed holds for all of them — no turn's call undoes a
//! pause, a pause gates every turn, and Stop ends every turn using the
//! computer at that moment. A turn starting after the stop has it again.
//!
//! Where the browser pane hands the page to the person and back (take over
//! / hand back, with a note), the computer has no hand-back: the person is
//! in charge of the whole machine, so input only pauses, and the agent
//! learns why from the tool result. `computer_request_user` stays the
//! approvals' ordinary question (#1583), so there is no `ask` here.

use serde::Serialize;

use crate::control_state::{Notify, Observed, TurnId, Viewed};

/// What the panel shows.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    #[default]
    Idle,
    Active,
    Paused,
    Stopped,
}

/// Why a session is paused.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PauseReason {
    /// The person pressed **Pause**.
    Button,
    /// The person used the keyboard or the pointer.
    UserActive,
}

/// The most characters of the app name and the action the panel shows.
const MAX_SHOWN_CHARS: usize = 120;

/// The session panel's state, as the UI receives it
/// (`computer://event`, `computer_session`).
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct SessionView {
    pub phase: Phase,
    pub paused_by: Option<PauseReason>,
    /// The app the last call was about (its name, else its bundle id).
    pub app: Option<String>,
    /// The last action ("press", "type 12 characters", ...).
    pub action: Option<String>,
}

impl SessionView {
    /// The panel must be on screen (it cannot be hidden while active).
    #[must_use]
    pub fn shown(&self) -> bool {
        matches!(self.phase, Phase::Active | Phase::Paused)
    }
}

/// What a call may do now.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Gate {
    Go,
    /// Paused: `user_active`.
    Paused,
    /// Stopped: `stopped_by_user`.
    Stopped,
}

/// One session: every turn using the computer while it runs (two threads
/// of the desktop may at once), and what the person pressed, which holds
/// for all of them until the last one ends.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
struct State {
    /// The turns in the session (it is Idle without one). The first one's
    /// app and action are the panel's until it ends; then the next one's.
    turns: Vec<TurnId>,
    paused: Option<PauseReason>,
    /// The turns **Stop** ended: they have no tools left. A turn that
    /// starts using the computer after the stop is not one of them.
    stopped: Vec<TurnId>,
    app: Option<String>,
    action: Option<String>,
    /// Bumped on every pause or stop while a session runs: a call in flight
    /// waits on it.
    epoch: u64,
}

impl State {
    fn running(&self) -> bool {
        !self.turns.is_empty()
    }

    /// Every turn of the session was stopped.
    fn all_stopped(&self) -> bool {
        self.running() && self.turns.iter().all(|turn| self.stopped.contains(turn))
    }
}

impl Viewed for State {
    type View = SessionView;

    fn view(&self) -> SessionView {
        let phase = if !self.running() {
            Phase::Idle
        } else if self.all_stopped() {
            Phase::Stopped
        } else if self.paused.is_some() {
            Phase::Paused
        } else {
            Phase::Active
        };
        SessionView {
            phase,
            paused_by: if phase == Phase::Paused {
                self.paused
            } else {
                None
            },
            app: self.app.clone(),
            action: self.action.clone(),
        }
    }
}

/// An app's name or an action as the panel and the menu-bar item show it:
/// controls, bidi and invisible format characters dropped
/// (`notices::sanitize`, so a name cannot read as another), cut.
fn shown(text: &str) -> String {
    crate::notices::sanitize(text)
        .chars()
        .take(MAX_SHOWN_CHARS)
        .collect()
}

/// The app's one computer session control (one desktop).
pub struct ComputerControl {
    state: Observed<State>,
}

impl Default for ComputerControl {
    fn default() -> Self {
        Self {
            state: Observed::new(State::default()),
        }
    }
}

impl ComputerControl {
    /// Who hears each change of the view (the panel, the main window, the
    /// menu-bar item).
    pub fn set_notify(&self, notify: Notify<SessionView>) {
        self.state.set_notify(notify);
    }

    #[must_use]
    pub fn view(&self) -> SessionView {
        self.state.view()
    }

    /// Turn `turn`'s `computer` call is about to run: it joins the session
    /// (a new one when none runs). Joining never undoes what the person
    /// pressed: a paused session stays paused for every turn, the joiner
    /// included, until **Resume**. The same turn joining again is nothing.
    pub fn begin(&self, turn: TurnId) {
        self.state.change(|state| {
            if !state.running() {
                *state = State {
                    epoch: state.epoch,
                    ..State::default()
                };
            }
            if !state.turns.contains(&turn) {
                state.turns.push(turn);
            }
        });
    }

    /// The turn's call acted on `app`: the panel shows it, for the turn the
    /// panel follows (the session's first one still using it).
    pub fn acted(&self, turn: TurnId, app: Option<&str>, action: &str) {
        self.state.change(|state| {
            if state.turns.first() == Some(&turn) {
                state.app = app.map(shown);
                state.action = Some(shown(action));
            }
        });
    }

    /// Turn `turn` ended: it leaves the session. With the last turn gone,
    /// the session ends and the panel goes away; until then, the pause and
    /// the stop stay as they are.
    pub fn end(&self, turn: TurnId) {
        self.state.change(|state| {
            let Some(at) = state.turns.iter().position(|t| *t == turn) else {
                return;
            };
            state.turns.remove(at);
            if !state.running() {
                *state = State {
                    epoch: state.epoch,
                    ..State::default()
                };
            } else if at == 0 {
                // The panel follows the next turn from its next call.
                state.app = None;
                state.action = None;
            }
        });
    }

    /// What a call of `turn` may do now. A pause holds for every turn,
    /// in the session or not; a stop for the turns it ended.
    #[must_use]
    pub fn gate(&self, turn: TurnId) -> Gate {
        self.gate_at(turn).0
    }

    /// The pause-and-stop count and the gate of `turn`, read together:
    /// a pause or a stop after this read bumps the count past it.
    #[must_use]
    pub fn gate_at(&self, turn: TurnId) -> (Gate, u64) {
        self.state.read(|state| {
            let gate = if state.stopped.contains(&turn) {
                Gate::Stopped
            } else if state.paused.is_some() {
                Gate::Paused
            } else {
                Gate::Go
            };
            (gate, state.epoch)
        })
    }

    /// The pause-and-stop count, for [`Self::interrupted_after`].
    #[must_use]
    pub fn epoch(&self) -> u64 {
        self.state.read(|state| state.epoch)
    }

    /// Resolves once the session is paused or stopped after `epoch`.
    pub async fn interrupted_after(&self, epoch: u64) {
        self.state.wait_for(|state| state.epoch > epoch).await;
    }

    fn pause(&self, reason: PauseReason) {
        self.state.change(|state| {
            if state.running() && !state.all_stopped() {
                if state.paused.is_none() {
                    state.epoch += 1;
                }
                // Input while paused by the button says why it stays paused.
                state.paused = Some(match (state.paused, reason) {
                    (Some(PauseReason::UserActive), _) => PauseReason::UserActive,
                    (_, reason) => reason,
                });
            }
        });
    }

    /// The person used the keyboard or the pointer (the user-active
    /// guard): while a session runs, it pauses. While idle, nothing.
    pub fn person_input(&self) {
        self.pause(PauseReason::UserActive);
    }

    /// **Pause**.
    pub fn pause_pressed(&self) {
        self.pause(PauseReason::Button);
    }

    /// **Resume**: the only way out of a pause.
    pub fn resume(&self) {
        self.state.change(|state| {
            if !state.all_stopped() {
                state.paused = None;
            }
        });
    }

    /// **Stop**, or the stop shortcut: the session is over for every turn
    /// using the computer now.
    pub fn stop(&self) {
        self.state.change(|state| {
            if state.running() && !state.all_stopped() {
                for turn in state.turns.clone() {
                    if !state.stopped.contains(&turn) {
                        state.stopped.push(turn);
                    }
                }
                state.paused = None;
                state.epoch += 1;
            }
        });
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use super::*;
    use crate::control_state::lock;

    #[test]
    fn idle_until_a_turn_holds_a_session_and_input_while_idle_changes_nothing() {
        let control = ComputerControl::default();
        control.person_input();
        control.pause_pressed();
        control.stop();
        assert_eq!(control.view(), SessionView::default());
        assert_eq!(control.epoch(), 0);
        assert!(!control.view().shown());
        control.begin(1);
        assert_eq!(control.view().phase, Phase::Active);
        assert!(control.view().shown());
        assert_eq!(control.gate(1), Gate::Go);
    }

    #[test]
    fn input_pauses_at_once_and_only_resume_lifts_it() {
        let control = ComputerControl::default();
        control.begin(1);
        control.person_input();
        let view = control.view();
        assert_eq!(view.phase, Phase::Paused);
        assert_eq!(view.paused_by, Some(PauseReason::UserActive));
        assert_eq!(control.gate(1), Gate::Paused);
        // More input, a new call, the same turn's next call: still paused.
        control.person_input();
        control.begin(1);
        assert_eq!(control.gate(1), Gate::Paused);
        assert_eq!(control.epoch(), 1);
        control.resume();
        assert_eq!(control.view().phase, Phase::Active);
        assert_eq!(control.gate(1), Gate::Go);
    }

    #[test]
    fn pause_button_then_input_says_the_person_is_using_the_computer() {
        let control = ComputerControl::default();
        control.begin(1);
        control.pause_pressed();
        assert_eq!(control.view().paused_by, Some(PauseReason::Button));
        control.person_input();
        assert_eq!(control.view().paused_by, Some(PauseReason::UserActive));
        assert_eq!(control.epoch(), 1, "one pause, one wake");
    }

    #[test]
    fn stop_ends_the_session_for_the_turn_and_resume_does_not_undo_it() {
        let control = ComputerControl::default();
        control.begin(1);
        control.person_input();
        control.stop();
        assert_eq!(control.view().phase, Phase::Stopped);
        assert!(!control.view().shown());
        assert_eq!(control.gate(1), Gate::Stopped);
        control.resume();
        control.person_input();
        control.pause_pressed();
        control.begin(1);
        assert_eq!(control.gate(1), Gate::Stopped);
        assert_eq!(control.epoch(), 2);
        control.stop();
        assert_eq!(control.epoch(), 2, "a second stop is nothing");
        // The next turn starts afresh.
        control.begin(2);
        assert_eq!(control.view().phase, Phase::Active);
        assert_eq!(control.gate(2), Gate::Go);
    }

    #[test]
    fn another_turns_call_never_undoes_a_pause_and_is_paused_too() {
        let control = ComputerControl::default();
        control.begin(1);
        control.pause_pressed();
        // A second thread's turn starts using the computer meanwhile.
        control.begin(2);
        assert_eq!(control.view().phase, Phase::Paused);
        assert_eq!(control.gate(1), Gate::Paused);
        assert_eq!(
            control.gate(2),
            Gate::Paused,
            "the pause holds for every turn"
        );
        // A turn that has not joined is paused as well.
        assert_eq!(control.gate(3), Gate::Paused);
        // Turn 1 ends: the session goes on, still paused, for turn 2.
        control.end(1);
        assert_eq!(control.view().phase, Phase::Paused);
        assert_eq!(control.gate(2), Gate::Paused);
        control.resume();
        assert_eq!(control.gate(2), Gate::Go);
        control.end(2);
        assert_eq!(control.view().phase, Phase::Idle);
    }

    #[test]
    fn another_turns_call_never_undoes_a_stop() {
        let control = ComputerControl::default();
        control.begin(1);
        control.begin(2);
        control.stop();
        assert_eq!(control.view().phase, Phase::Stopped);
        // Both turns were using it: both are stopped, and joining again or
        // a new call of either changes nothing.
        control.begin(2);
        control.begin(1);
        assert_eq!(control.gate(1), Gate::Stopped);
        assert_eq!(control.gate(2), Gate::Stopped);
        assert_eq!(control.view().phase, Phase::Stopped);
        // Turn 1's late end leaves turn 2 stopped.
        control.end(1);
        assert_eq!(control.gate(2), Gate::Stopped);
        assert_eq!(control.view().phase, Phase::Stopped);
        // A turn that starts after the stop has the computer.
        control.begin(3);
        assert_eq!(control.gate(3), Gate::Go);
        assert_eq!(control.gate(2), Gate::Stopped);
        assert_eq!(control.view().phase, Phase::Active);
        control.end(2);
        control.end(3);
        assert_eq!(control.view().phase, Phase::Idle);
    }

    #[test]
    fn the_panel_follows_the_first_turn_and_ignores_turns_not_in_the_session() {
        let control = ComputerControl::default();
        control.begin(1);
        control.begin(2);
        control.acted(2, Some("Notes"), "press");
        assert_eq!(control.view().app, None);
        control.acted(1, Some("TextEdit"), "type");
        assert_eq!(control.view().app.as_deref(), Some("TextEdit"));
        control.end(9);
        assert_eq!(control.view().app.as_deref(), Some("TextEdit"));
        control.end(1);
        assert_eq!(control.view().app, None);
        control.acted(2, Some("Notes"), "press");
        assert_eq!(control.view().app.as_deref(), Some("Notes"));
    }

    #[test]
    fn the_gate_and_its_epoch_are_one_read() {
        let control = ComputerControl::default();
        control.begin(1);
        assert_eq!(control.gate_at(1), (Gate::Go, 0));
        control.person_input();
        assert_eq!(control.gate_at(1), (Gate::Paused, 1));
    }

    #[test]
    fn app_names_cannot_hide_or_reorder_characters() {
        let control = ComputerControl::default();
        control.begin(1);
        // A right-to-left override and zero-width marks would make the name
        // read as another app's.
        control.acted(
            1,
            Some("Bank\u{202E}gnp.\u{200F}\u{200B}App"),
            "press\u{2066}",
        );
        let view = control.view();
        assert_eq!(view.app.as_deref(), Some("Bankgnp.App"));
        assert_eq!(view.action.as_deref(), Some("press"));
    }

    #[test]
    fn the_panel_shows_the_app_and_the_last_action_cleaned_and_cut() {
        let control = ComputerControl::default();
        control.begin(1);
        control.acted(1, Some("Text\u{7}Edit\n"), &"x".repeat(500));
        let view = control.view();
        assert_eq!(view.app.as_deref(), Some("TextEdit"));
        assert_eq!(view.action.unwrap().chars().count(), MAX_SHOWN_CHARS);
        control.end(1);
        assert_eq!(control.view(), SessionView::default());
    }

    #[test]
    fn every_change_of_the_view_is_heard_once() {
        let heard = Arc::new(Mutex::new(Vec::new()));
        let control = ComputerControl::default();
        let sink = heard.clone();
        control.set_notify(Arc::new(move |view: &SessionView| {
            lock(&sink).push(view.phase);
        }));
        control.begin(1);
        control.begin(1);
        control.person_input();
        control.person_input();
        control.resume();
        control.stop();
        control.end(1);
        assert_eq!(
            *lock(&heard),
            [
                Phase::Active,
                Phase::Paused,
                Phase::Active,
                Phase::Stopped,
                Phase::Idle
            ]
        );
    }

    #[tokio::test]
    async fn a_call_in_flight_wakes_on_a_pause_or_a_stop() {
        let control = Arc::new(ComputerControl::default());
        control.begin(1);
        let epoch = control.epoch();
        let waiting = control.clone();
        let woken = tokio::spawn(async move { waiting.interrupted_after(epoch).await });
        tokio::task::yield_now().await;
        assert!(!woken.is_finished());
        control.person_input();
        tokio::time::timeout(Duration::from_millis(100), woken)
            .await
            .expect("woken within 100 ms")
            .unwrap();
        let epoch = control.epoch();
        control.resume();
        let waiting = control.clone();
        let woken = tokio::spawn(async move { waiting.interrupted_after(epoch).await });
        control.stop();
        tokio::time::timeout(Duration::from_millis(100), woken)
            .await
            .expect("woken within 100 ms")
            .unwrap();
    }
}
