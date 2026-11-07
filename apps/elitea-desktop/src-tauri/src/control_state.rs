//! The mechanism shared by the person-in-charge state machines of the
//! agent's drivers: the browser pane's control pill (`browser_control`,
//! ADR-0048 decision 4) and the computer session (`computer_control`,
//! ADR-0047 decision 6).
//!
//! An [`Observed`] state changes in one step ([`Observed::change`]: decide
//! and record together, so two changes never interleave), tells its UI only
//! when the view changed, and lets a call wait for a state
//! ([`Observed::wait_for`]): a tool call races its work against "the person
//! did something" and answers at once, not when the work ends.
//!
//! **One turn owns the control** in both: each turn's toolset has a
//! [`TurnId`], and only the owner's end resets the state, so an older turn
//! of the thread finishing late changes nothing.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use tokio::sync::watch;

/// A turn's identity for a control.
pub type TurnId = u64;

/// A new [`TurnId`], unique for the app session.
#[must_use]
pub fn next_turn_id() -> TurnId {
    static NEXT: AtomicU64 = AtomicU64::new(1);
    NEXT.fetch_add(1, Ordering::Relaxed)
}

pub fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// A state with a view its UI receives.
pub trait Viewed {
    type View: Clone + PartialEq;
    fn view(&self) -> Self::View;
}

/// Who hears each change of the view.
pub type Notify<V> = Arc<dyn Fn(&V) + Send + Sync>;

/// A state, its UI and its waiters.
pub struct Observed<S: Viewed> {
    state: watch::Sender<S>,
    notify: Mutex<Option<Notify<S::View>>>,
}

impl<S: Viewed> Observed<S> {
    pub fn new(state: S) -> Self {
        Self {
            state: watch::channel(state).0,
            notify: Mutex::new(None),
        }
    }

    pub fn set_notify(&self, notify: Notify<S::View>) {
        *lock(&self.notify) = Some(notify);
    }

    pub fn view(&self) -> S::View {
        self.state.borrow().view()
    }

    /// Read the state now.
    pub fn read<R>(&self, read: impl FnOnce(&S) -> R) -> R {
        read(&self.state.borrow())
    }

    /// Change the state in one step; the UI hears it when the view
    /// changed, after the change (never under the state's lock). Answers
    /// what `work` answers.
    pub fn change<R>(&self, work: impl FnOnce(&mut S) -> R) -> R {
        let mut out = None;
        let mut changed = None;
        self.state.send_modify(|state| {
            let before = state.view();
            out = Some(work(state));
            let after = state.view();
            if before != after {
                changed = Some(after);
            }
        });
        if let Some(after) = changed {
            let notify = lock(&self.notify).clone();
            if let Some(notify) = notify {
                notify(&after);
            }
        }
        out.expect("send_modify runs its closure")
    }

    /// Resolves once `holds` holds (at once when it does now).
    pub async fn wait_for(&self, holds: impl FnMut(&S) -> bool) {
        let mut changes = self.state.subscribe();
        let _ = changes.wait_for(holds).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Default)]
    struct Count {
        shown: u32,
        hidden: u32,
    }

    impl Viewed for Count {
        type View = u32;
        fn view(&self) -> u32 {
            self.shown
        }
    }

    #[test]
    fn the_ui_hears_a_change_of_the_view_only() {
        let heard = Arc::new(Mutex::new(Vec::new()));
        let observed = Observed::new(Count::default());
        let sink = heard.clone();
        observed.set_notify(Arc::new(move |view: &u32| lock(&sink).push(*view)));
        observed.change(|c| c.shown += 1);
        observed.change(|c| c.hidden += 1);
        assert_eq!(observed.change(|c| c.shown), 1);
        assert_eq!(*lock(&heard), [1]);
        assert_eq!(observed.read(|c| c.hidden), 1);
    }

    #[tokio::test]
    async fn a_waiter_wakes_when_its_state_holds() {
        let observed = Arc::new(Observed::new(Count::default()));
        let waiting = observed.clone();
        let woken = tokio::spawn(async move { waiting.wait_for(|c| c.hidden > 0).await });
        tokio::task::yield_now().await;
        assert!(!woken.is_finished());
        observed.change(|c| c.hidden = 1);
        woken.await.unwrap();
        // A state that holds now resolves at once.
        observed.wait_for(|c| c.hidden == 1).await;
        assert_ne!(next_turn_id(), next_turn_id());
    }
}
