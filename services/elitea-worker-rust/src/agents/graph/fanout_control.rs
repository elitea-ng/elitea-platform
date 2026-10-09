//! Control-plane primitives shared by the Parallel and Map runners.
//!
//! Lease loss and cancellation are control stops, not business failures: they
//! must never be recorded as a branch or item outcome, because a later claim
//! restores the same occurrence and decides again.

#![allow(dead_code)] // The claim lease probe fires the latch in the next slice.

use std::sync::atomic::{AtomicBool, Ordering};

use adk_rust::graph::GraphError;
use tokio::sync::Notify;

/// Latch fired by the owner of the execution.
///
/// The owner (the claim lease probe, on a durable Stop) calls [`cancel`]. The
/// fan-out never polls: it parks in `select!` on [`cancelled`] and wakes
/// exactly once the latch is set.
///
/// [`cancel`]: Self::cancel
/// [`cancelled`]: Self::cancelled
pub(crate) struct FanoutCancellation {
    cancelled: AtomicBool,
    notify: Notify,
}

impl FanoutCancellation {
    pub(crate) fn new() -> Self {
        Self {
            cancelled: AtomicBool::new(false),
            notify: Notify::new(),
        }
    }

    pub(crate) fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
        self.notify.notify_waiters();
    }

    pub(crate) fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire)
    }

    /// Resolve once the latch is set. Safe against a `cancel` racing the call:
    /// the waiter is registered before the flag is read.
    pub(crate) async fn cancelled(&self) {
        let notified = self.notify.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        if self.is_cancelled() {
            return;
        }
        notified.await;
    }
}

impl Default for FanoutCancellation {
    fn default() -> Self {
        Self::new()
    }
}

/// Wait on an optional latch; never resolves when there is none.
pub(crate) async fn latch_cancelled(latch: Option<&FanoutCancellation>) {
    match latch {
        Some(latch) => latch.cancelled().await,
        None => std::future::pending::<()>().await,
    }
}

/// True only for the typed `PostgreSQL` writer-fence loss.
pub(crate) fn is_lease_lost(error: &GraphError) -> bool {
    matches!(
        error,
        GraphError::CheckpointError(message)
            if message.starts_with("checkpoint.writer_not_current")
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::PostgresCheckpointError;

    #[test]
    fn only_writer_not_current_is_classified_as_lease_loss() {
        assert!(is_lease_lost(&GraphError::from(
            PostgresCheckpointError::WriterNotCurrent
        )));
        assert!(!is_lease_lost(&GraphError::from(
            PostgresCheckpointError::CheckpointConflict
        )));
        assert!(!is_lease_lost(&GraphError::from(
            PostgresCheckpointError::CorruptStoredState
        )));
        assert!(!is_lease_lost(&GraphError::Other(
            "checkpoint.writer_not_current".to_owned()
        )));
    }

    #[tokio::test]
    async fn a_latch_set_before_the_wait_resolves_immediately() {
        let latch = FanoutCancellation::new();
        latch.cancel();
        latch.cancelled().await;
        assert!(latch.is_cancelled());
    }
}
