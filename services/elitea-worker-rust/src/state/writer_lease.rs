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

    /// The steering queue (client contract 1.26) fenced by the same claim:
    /// the live lease that permits state work also permits claiming the
    /// user messages queued against the turn. `None` (the default) binds no
    /// steering, as for a test lease.
    fn steering_source(&self) -> Option<std::sync::Arc<dyn adk_rust::agent::SteeringSource>> {
        None
    }
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
