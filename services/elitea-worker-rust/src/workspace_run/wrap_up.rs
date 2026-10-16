//! Graceful wrap-up of a cloud turn whose thread the desktop is taking back
//! (ADR-0034 decision 7). Main's lease poll carries `workspace_wrap_up` (the
//! same poll that carries cancellation); the lifecycle requests the wrap-up
//! here. From then on:
//!
//! * no new workspace tool call runs (the model is told why);
//! * a workspace tool call in flight finishes, but one that is still running
//!   [`COMMAND_GRACE`] after the request (a long `run_command`) is stopped;
//! * once no workspace tool call is in flight, the lifecycle stops the agent
//!   loop cooperatively, and the turn ends as COMPLETED: the work is
//!   committed and published once, and the answer ends with [`NOTE`].
//!
//! Main's 60 s grace and hard cancel stay the fallback when no worker wraps
//! up in time.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;

use tokio::sync::Notify;

/// The answer's closing note of a wrapped-up turn.
pub(crate) const NOTE: &str = "Taken back to the desktop. The work so far is saved and \
returns with the thread; this turn stopped here.";

/// What a workspace tool call that is refused, or stopped, answers.
pub(crate) const REFUSED: &str =
    "the work is being taken back to the desktop; no further workspace commands run in this turn";

/// How long a workspace tool call still running after the request may
/// continue before it is stopped.
pub(crate) const COMMAND_GRACE: Duration = Duration::from_secs(10);

/// One turn's wrap-up state, shared by the lifecycle and the tools.
#[derive(Debug, Default)]
pub(crate) struct WrapUp {
    requested: AtomicBool,
    in_flight: AtomicUsize,
    changed: Notify,
}

impl WrapUp {
    /// Request the wrap-up. `true` the first time.
    pub(crate) fn request(&self) -> bool {
        let first = !self.requested.swap(true, Ordering::AcqRel);
        self.changed.notify_waiters();
        first
    }

    #[must_use]
    pub(crate) fn requested(&self) -> bool {
        self.requested.load(Ordering::Acquire)
    }

    /// Wait until the wrap-up is requested.
    pub(crate) async fn requested_wait(&self) {
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.requested() {
                return;
            }
            notified.await;
        }
    }

    /// Start one workspace tool call; `None` once the wrap-up is requested.
    #[must_use]
    pub(crate) fn begin(self: &Arc<Self>) -> Option<InFlight> {
        self.in_flight.fetch_add(1, Ordering::AcqRel);
        if self.requested() {
            self.end_one();
            return None;
        }
        Some(InFlight(Arc::clone(self)))
    }

    /// Wait until no workspace tool call is in flight.
    pub(crate) async fn idle(&self) {
        loop {
            let notified = self.changed.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.in_flight.load(Ordering::Acquire) == 0 {
                return;
            }
            notified.await;
        }
    }

    fn end_one(&self) {
        self.in_flight.fetch_sub(1, Ordering::AcqRel);
        self.changed.notify_waiters();
    }
}

/// One workspace tool call in flight; ends on drop.
pub(crate) struct InFlight(Arc<WrapUp>);

impl Drop for InFlight {
    fn drop(&mut self) {
        self.0.end_one();
    }
}

/// Run `call` as a workspace tool call under `wrap_up`: refused once the
/// wrap-up is requested, stopped [`COMMAND_GRACE`] after a request that
/// arrives while it runs.
pub(crate) async fn guarded<T, F>(wrap_up: &Arc<WrapUp>, grace: Duration, call: F) -> Option<T>
where
    F: std::future::Future<Output = T>,
{
    let _in_flight = wrap_up.begin()?;
    tokio::select! {
        biased;
        result = call => Some(result),
        () = async {
            wrap_up.requested_wait().await;
            tokio::time::sleep(grace).await;
        } => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_call_in_flight_finishes_and_later_calls_are_refused() {
        let wrap_up = Arc::new(WrapUp::default());
        let gate = Arc::new(Notify::new());
        let running = tokio::spawn({
            let (wrap_up, gate) = (Arc::clone(&wrap_up), Arc::clone(&gate));
            async move {
                guarded(&wrap_up, Duration::from_secs(30), async {
                    gate.notified().await;
                    7
                })
                .await
            }
        });
        tokio::task::yield_now().await;
        assert!(wrap_up.request());
        assert!(!wrap_up.request(), "requested once");
        // Not idle while the call runs.
        assert!(
            tokio::time::timeout(Duration::from_millis(20), wrap_up.idle())
                .await
                .is_err()
        );
        gate.notify_one();
        assert_eq!(running.await.expect("joined"), Some(7));
        wrap_up.idle().await;
        assert_eq!(
            guarded(&wrap_up, Duration::from_secs(30), async { 1 }).await,
            None
        );
    }

    #[tokio::test]
    async fn a_long_call_is_stopped_after_the_grace() {
        let wrap_up = Arc::new(WrapUp::default());
        let running = tokio::spawn({
            let wrap_up = Arc::clone(&wrap_up);
            async move {
                guarded(
                    &wrap_up,
                    Duration::from_millis(30),
                    std::future::pending::<u8>(),
                )
                .await
            }
        });
        tokio::task::yield_now().await;
        wrap_up.request();
        assert_eq!(running.await.expect("joined"), None);
        tokio::time::timeout(Duration::from_secs(1), wrap_up.idle())
            .await
            .expect("idle once the stopped call is gone");
    }
}
