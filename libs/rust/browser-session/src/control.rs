//! Who drives the page (ADR-0048 decision 4, ADR-0049 decision 6).
//!
//! The view relay (#1597) feeds the person's side in: their input
//! ([`Control::person_input`]), **Take over**, **Hand back** with a note,
//! and their answer to `browser_request_user`. The driver reads the agent's
//! side: a call while the person drives is `user_in_control`, and a call
//! during which the person used the page is `user_active`.

use std::sync::{Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use tokio::sync::watch;

/// Who drives.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub enum Driver {
    #[default]
    Agent,
    /// The person took over (or was asked to, with `reason`).
    Person { reason: Option<String> },
}

/// The control state, as the supervisor and the view see it.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ControlState {
    pub driver: Driver,
    /// Bumped on every input the person makes.
    pub input_epoch: u64,
    /// Bumped on every hand back or decline.
    pub handback_epoch: u64,
}

/// The person's answer to the agent's question.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Answer {
    /// They handed the page back, with an optional note.
    HandedBack(Option<String>),
    /// They declined, or did not answer in time.
    Declined,
}

#[derive(Default)]
struct Inner {
    /// The latest hand back's note; `None` until one (or after a decline).
    answer: Option<Answer>,
}

/// The shared control state of one pane.
pub struct Control {
    /// There is a person who can be asked (a run with a viewer; ADR-0049
    /// decision 7: a schedule or webhook run has nobody).
    pub attended: bool,
    /// How long `browser_request_user` waits before it counts as declined.
    pub request_timeout: Duration,
    state: watch::Sender<ControlState>,
    inner: Mutex<Inner>,
}

impl Control {
    #[must_use]
    pub fn new(attended: bool, request_timeout: Duration) -> Self {
        Self {
            attended,
            request_timeout,
            state: watch::channel(ControlState::default()).0,
            inner: Mutex::new(Inner::default()),
        }
    }

    fn inner(&self) -> MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(PoisonError::into_inner)
    }

    #[must_use]
    pub fn state(&self) -> ControlState {
        self.state.borrow().clone()
    }

    /// Changes, for the supervisor's event stream.
    #[must_use]
    pub fn subscribe(&self) -> watch::Receiver<ControlState> {
        self.state.subscribe()
    }

    /// The person used the page: the agent is paused and the person drives.
    pub fn person_input(&self) {
        self.state.send_modify(|state| {
            state.input_epoch += 1;
            if state.driver == Driver::Agent {
                state.driver = Driver::Person { reason: None };
            }
        });
    }

    /// The person pressed **Take over**.
    pub fn take_over(&self) {
        self.state.send_if_modified(|state| {
            if state.driver == Driver::Agent {
                state.driver = Driver::Person { reason: None };
                true
            } else {
                false
            }
        });
    }

    /// The person handed the page back, with an optional note.
    pub fn hand_back(&self, note: Option<String>) {
        self.inner().answer = Some(Answer::HandedBack(note));
        self.state.send_modify(|state| {
            state.driver = Driver::Agent;
            state.handback_epoch += 1;
        });
    }

    /// The person declined the agent's question.
    pub fn decline(&self) {
        self.inner().answer = Some(Answer::Declined);
        self.state.send_modify(|state| {
            state.driver = Driver::Agent;
            state.handback_epoch += 1;
        });
    }

    /// The agent asks the person to drive and waits for their answer; a
    /// question that times out is [`Answer::Declined`].
    ///
    /// The caller has checked [`Self::attended`].
    pub async fn ask(&self, reason: &str) -> Answer {
        self.inner().answer = None;
        let mut changes = self.state.subscribe();
        let asked_at = {
            let mut epoch = 0;
            self.state.send_modify(|state| {
                state.driver = Driver::Person {
                    reason: Some(reason.to_owned()),
                };
                epoch = state.handback_epoch;
            });
            epoch
        };
        let answered = tokio::time::timeout(
            self.request_timeout,
            changes.wait_for(|state| state.handback_epoch > asked_at),
        )
        .await;
        if let Ok(Ok(_)) = answered {
            return self.inner().answer.take().unwrap_or(Answer::Declined);
        }
        // Timed out: the agent gets the page back, unanswered.
        self.state.send_modify(|state| {
            state.driver = Driver::Agent;
            state.handback_epoch += 1;
        });
        Answer::Declined
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn input_pauses_the_agent_and_hand_back_answers_the_question() {
        let control = std::sync::Arc::new(Control::new(true, Duration::from_secs(5)));
        control.person_input();
        assert_eq!(control.state().driver, Driver::Person { reason: None });
        control.hand_back(None);
        assert_eq!(control.state().driver, Driver::Agent);

        let asking = std::sync::Arc::clone(&control);
        let question = tokio::spawn(async move { asking.ask("sign in").await });
        tokio::task::yield_now().await;
        while control.state().driver == Driver::Agent {
            tokio::task::yield_now().await;
        }
        assert_eq!(
            control.state().driver,
            Driver::Person {
                reason: Some("sign in".to_owned())
            }
        );
        control.hand_back(Some("signed in".to_owned()));
        assert_eq!(
            question.await.unwrap(),
            Answer::HandedBack(Some("signed in".to_owned()))
        );
    }

    #[tokio::test]
    async fn a_decline_or_a_timeout_is_no_answer() {
        let control = std::sync::Arc::new(Control::new(true, Duration::from_millis(30)));
        assert_eq!(control.ask("captcha").await, Answer::Declined);
        assert_eq!(control.state().driver, Driver::Agent);
        let asking = std::sync::Arc::clone(&control);
        let question = tokio::spawn(async move { asking.ask("choose").await });
        while control.state().driver == Driver::Agent {
            tokio::task::yield_now().await;
        }
        control.decline();
        assert_eq!(question.await.unwrap(), Answer::Declined);
    }
}
