//! Live-lease boundary shared by native session and graph state writers.
//!
//! Main remains the claim/renewal authority in its own database. The worker's
//! supervised claim monitor implements this narrow interface so the separate
//! `agentstate` database can reject local work after cancellation, monitor
//! loss, or lease expiry without copying Main's business tables.

/// Opaque loss of state-writer authority.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct StateWriterLeaseLost;

/// Read-only proof that one supervised claim still permits native state work.
///
/// Implementations must fail closed after cancellation, fatal supervision
/// failure, monitor shutdown, or insufficient remaining lease lifetime.
pub(crate) trait StateWriterLease: Send + Sync {
    fn ensure_current(&self) -> Result<(), StateWriterLeaseLost>;

    /// The steering queue (client contract 1.27) fenced by the same claim:
    /// the live lease that permits state work also permits claiming the
    /// user messages queued against the turn. `None` (the default) binds no
    /// steering, as for a test lease.
    fn steering_source(&self) -> Option<std::sync::Arc<dyn TurnSteering>> {
        None
    }
}

/// The steering queue of one turn, as the agent loop and the model checkpoint
/// see it (client contract 1.27). The loop takes messages at its safe points
/// (`SteeringSource`). Main records them as claimed; it writes them as
/// conversation messages only when the worker acknowledges them, after the
/// model checkpoint that holds them is persisted. So a worker that stops in
/// between loses nothing: the replacement claim gets them again, or
/// acknowledges them when its restored checkpoint holds them.
#[async_trait::async_trait]
pub(crate) trait TurnSteering: adk_rust::agent::SteeringSource {
    /// A model checkpoint is bound: acknowledge after each persist. Without
    /// one, the messages are acknowledged as soon as they are taken.
    fn bind_checkpoint(&self);
    /// Every steering id this turn took, in order, for the checkpoint.
    fn checkpoint_ids(&self) -> Vec<String>;
    /// The ids of a restored checkpoint: the model request holds them.
    fn restored(&self, ids: &[String]);
    /// A checkpoint that holds `ids` is persisted. It never fails: an
    /// acknowledgement that is not confirmed is sent again on the next call.
    async fn persisted(&self, ids: &[String]);
}

#[cfg(test)]
pub(crate) struct TestStateWriterLease {
    current: std::sync::atomic::AtomicBool,
}

#[cfg(test)]
impl TestStateWriterLease {
    pub(crate) const fn current() -> Self {
        Self {
            current: std::sync::atomic::AtomicBool::new(true),
        }
    }

    pub(crate) fn revoke(&self) {
        self.current
            .store(false, std::sync::atomic::Ordering::SeqCst);
    }
}

#[cfg(test)]
impl StateWriterLease for TestStateWriterLease {
    fn ensure_current(&self) -> Result<(), StateWriterLeaseLost> {
        self.current
            .load(std::sync::atomic::Ordering::SeqCst)
            .then_some(())
            .ok_or(StateWriterLeaseLost)
    }
}
