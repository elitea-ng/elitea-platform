//! Service the retained Code process across the complete submission observation.

use std::future::Future;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;
use tokio::sync::Notify;

pub enum ObservationFailure<E> {
    Deadline,
    Platform(E),
}

/// Whether the platform pump was inside one Main step. A step that Main began
/// may have reached an effect, so a pump dropped while it was in flight leaves
/// its outcome unknown ([`StepTracker::interrupted`]).
#[derive(Clone, Default)]
pub struct StepTracker(Arc<TrackerState>);

#[derive(Default)]
struct TrackerState {
    in_flight: AtomicBool,
    interrupted: AtomicBool,
    settled: Notify,
}

impl StepTracker {
    /// Mark one step as started. Call [`StepGuard::finish`] once it returned;
    /// dropping the guard first (the pump future was dropped) marks the
    /// tracker interrupted for good.
    pub fn begin(&self) -> StepGuard {
        self.0.in_flight.store(true, Ordering::SeqCst);
        StepGuard {
            state: Some(self.0.clone()),
        }
    }

    #[must_use]
    pub fn in_flight(&self) -> bool {
        self.0.in_flight.load(Ordering::SeqCst)
    }

    /// A step was abandoned mid-flight: its effect on Main is not known.
    #[must_use]
    pub fn interrupted(&self) -> bool {
        self.0.interrupted.load(Ordering::SeqCst)
    }
}

/// One step in flight; see [`StepTracker::begin`].
#[must_use]
pub struct StepGuard {
    state: Option<Arc<TrackerState>>,
}

impl StepGuard {
    pub fn finish(mut self) {
        if let Some(state) = self.state.take() {
            state.in_flight.store(false, Ordering::SeqCst);
            state.settled.notify_one();
        }
    }
}

impl Drop for StepGuard {
    fn drop(&mut self) {
        if let Some(state) = self.state.take() {
            state.interrupted.store(true, Ordering::SeqCst);
            state.in_flight.store(false, Ordering::SeqCst);
            state.settled.notify_one();
        }
    }
}

/// Both futures belong to this attempt. Dropping the attempt drops both.
/// The caller retains the original dispatch identity and absolute deadline.
/// Submission includes nonterminal reconciliation and backoff. It yields only
/// a terminal receipt or refusal, so those waits cannot replace the pump.
pub async fn drive<S, P, T, E>(
    submission: S,
    platform: P,
    deadline: tokio::time::Instant,
) -> Result<T, ObservationFailure<E>>
where
    S: Future<Output = T>,
    P: Future<Output = E>,
{
    drive_tracked(
        submission,
        platform,
        deadline,
        &StepTracker::default(),
        Duration::ZERO,
    )
    .await
}

/// [`drive`] for a pump that reports its Main steps through `tracker`. When the
/// terminal receipt wins while a step is in flight, the pump keeps running for
/// at most `step_grace` so the step can reach its boundary; a step still in
/// flight then is dropped and the tracker reports it interrupted.
pub async fn drive_tracked<S, P, T, E>(
    submission: S,
    platform: P,
    deadline: tokio::time::Instant,
    tracker: &StepTracker,
    step_grace: Duration,
) -> Result<T, ObservationFailure<E>>
where
    S: Future<Output = T>,
    P: Future<Output = E>,
{
    tokio::pin!(submission, platform);
    tokio::time::timeout_at(deadline, async {
        tokio::select! {
            // A terminal receipt owns completion when both are ready.
            biased;
            outcome = &mut submission => {
                if tracker.in_flight() && !step_grace.is_zero() {
                    let _ = tokio::time::timeout(step_grace, async {
                        while tracker.in_flight() {
                            tokio::select! {
                                biased;
                                _ = &mut platform => return,
                                () = tracker.0.settled.notified() => {}
                            }
                        }
                    })
                    .await;
                }
                Ok(outcome)
            }
            failure = &mut platform => Err(ObservationFailure::Platform(failure)),
        }
    })
    .await
    .unwrap_or(Err(ObservationFailure::Deadline))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };
    use tokio::sync::oneshot;

    struct DropCount(Arc<AtomicUsize>);
    impl Drop for DropCount {
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[tokio::test]
    async fn services_platform_before_submit_receipt_without_resubmitting() {
        let starts = AtomicUsize::new(0);
        let effects = AtomicUsize::new(0);
        let (reply, receipt) = oneshot::channel();
        let submission = async {
            starts.fetch_add(1, Ordering::SeqCst);
            receipt.await.expect("platform reply")
        };
        let platform = async {
            effects.fetch_add(1, Ordering::SeqCst);
            reply.send(42).expect("live original submit");
            std::future::pending::<()>().await;
        };
        let outcome = drive(
            submission,
            platform,
            tokio::time::Instant::now() + std::time::Duration::from_secs(1),
        )
        .await;
        assert!(matches!(outcome, Ok(42)));
        assert_eq!(starts.load(Ordering::SeqCst), 1);
        assert_eq!(effects.load(Ordering::SeqCst), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn platform_refusal_during_backoff_stops_the_whole_observation() {
        let submissions = AtomicUsize::new(0);
        let dropped = Arc::new(AtomicUsize::new(0));
        let submit_guard = DropCount(dropped.clone());
        let pump_guard = DropCount(dropped.clone());
        let outcome = drive(
            async {
                let _guard = submit_guard;
                submissions.fetch_add(1, Ordering::SeqCst);
                tokio::time::sleep(std::time::Duration::from_secs(1)).await;
                submissions.fetch_add(1, Ordering::SeqCst);
                std::future::pending::<()>().await;
            },
            async {
                let _guard = pump_guard;
                tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                "authorization_denied"
            },
            tokio::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .await;
        assert!(matches!(
            outcome,
            Err(ObservationFailure::Platform("authorization_denied"))
        ));
        assert_eq!(submissions.load(Ordering::SeqCst), 1);
        assert_eq!(dropped.load(Ordering::SeqCst), 2);
    }

    #[tokio::test(start_paused = true)]
    async fn absolute_deadline_covers_all_nonterminal_backoffs() {
        let submissions = AtomicUsize::new(0);
        let dropped = Arc::new(AtomicUsize::new(0));
        let submit_guard = DropCount(dropped.clone());
        let pump_guard = DropCount(dropped.clone());
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_millis(2500);
        let outcome = drive(
            async {
                let _guard = submit_guard;
                loop {
                    submissions.fetch_add(1, Ordering::SeqCst);
                    tokio::time::sleep(std::time::Duration::from_secs(1)).await;
                }
            },
            async {
                let _guard = pump_guard;
                std::future::pending::<()>().await;
            },
            deadline,
        )
        .await;
        assert!(matches!(outcome, Err(ObservationFailure::Deadline)));
        assert_eq!(tokio::time::Instant::now(), deadline);
        assert_eq!(submissions.load(Ordering::SeqCst), 3);
        assert_eq!(dropped.load(Ordering::SeqCst), 2);
    }

    #[tokio::test(start_paused = true)]
    async fn preserves_absolute_deadline_and_drops_both_scoped_futures() {
        let dropped = Arc::new(AtomicUsize::new(0));
        let submit_guard = DropCount(dropped.clone());
        let pump_guard = DropCount(dropped.clone());
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(5);
        let outcome = drive(
            async move {
                let _guard = submit_guard;
                std::future::pending::<()>().await;
            },
            async move {
                let _guard = pump_guard;
                std::future::pending::<()>().await;
            },
            deadline,
        )
        .await;
        assert!(matches!(outcome, Err(ObservationFailure::Deadline)));
        assert_eq!(tokio::time::Instant::now(), deadline);
        assert_eq!(dropped.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn platform_refusal_cancels_the_scoped_submit() {
        let dropped = Arc::new(AtomicUsize::new(0));
        let guard = DropCount(dropped.clone());
        let outcome = drive(
            async move {
                let _guard = guard;
                std::future::pending::<()>().await;
            },
            async { "authorization_denied" },
            tokio::time::Instant::now() + std::time::Duration::from_secs(1),
        )
        .await;
        assert!(matches!(
            outcome,
            Err(ObservationFailure::Platform("authorization_denied"))
        ));
        assert_eq!(dropped.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn terminal_receipt_wins_over_a_simultaneously_ready_platform_refusal() {
        let outcome = drive(
            async { 42 },
            async { "late_platform_refusal" },
            tokio::time::Instant::now() + std::time::Duration::from_secs(1),
        )
        .await;
        assert!(matches!(outcome, Ok(42)));
    }

    #[tokio::test(start_paused = true)]
    async fn terminal_receipt_stops_pump_before_journal_finalization_wait() {
        let dropped = Arc::new(AtomicUsize::new(0));
        let pump_guard = DropCount(dropped.clone());
        let (reply, receipt) = oneshot::channel();
        let outcome = drive(
            async { receipt.await.expect("terminal RPC receipt") },
            async {
                let _guard = pump_guard;
                reply.send(42).expect("original observation is active");
                tokio::task::yield_now().await;
                "late_platform_refusal"
            },
            tokio::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .await;
        assert!(matches!(outcome, Ok(42)));
        assert_eq!(dropped.load(Ordering::SeqCst), 1);
        // Journal persistence follows the winning receipt, after pump shutdown.
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        assert_eq!(dropped.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn parent_cancellation_drops_submit_and_platform() {
        let dropped = Arc::new(AtomicUsize::new(0));
        let submit_guard = DropCount(dropped.clone());
        let pump_guard = DropCount(dropped.clone());
        let outer_deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(30);
        {
            let attempt = drive(
                async move {
                    let _guard = submit_guard;
                    std::future::pending::<()>().await;
                },
                async move {
                    let _guard = pump_guard;
                    std::future::pending::<()>().await;
                },
                outer_deadline,
            );
            tokio::pin!(attempt);
            tokio::select! {
                _ = &mut attempt => panic!("attempt is still active"),
                () = tokio::task::yield_now() => {},
            }
        }
        assert_eq!(dropped.load(Ordering::SeqCst), 2);
    }

    #[tokio::test(start_paused = true)]
    async fn a_step_in_flight_when_the_receipt_wins_finishes_inside_the_grace() {
        let tracker = StepTracker::default();
        let finished = Arc::new(AtomicUsize::new(0));
        let outcome = drive_tracked(
            async {
                tokio::time::sleep(Duration::from_millis(10)).await;
                7
            },
            async {
                let guard = tracker.begin();
                // The step outlasts the receipt but fits inside the grace.
                tokio::time::sleep(Duration::from_millis(500)).await;
                guard.finish();
                finished.fetch_add(1, Ordering::SeqCst);
                std::future::pending::<()>().await;
            },
            tokio::time::Instant::now() + Duration::from_secs(30),
            &tracker,
            Duration::from_secs(2),
        )
        .await;
        assert!(matches!(outcome, Ok(7)));
        assert_eq!(finished.load(Ordering::SeqCst), 1, "the step reached its boundary");
        assert!(!tracker.interrupted());
        assert!(!tracker.in_flight());
    }

    #[tokio::test(start_paused = true)]
    async fn a_step_that_outlasts_the_grace_is_dropped_and_reported_interrupted() {
        let tracker = StepTracker::default();
        let dropped = Arc::new(AtomicUsize::new(0));
        let pump_guard = DropCount(dropped.clone());
        let outcome = drive_tracked(
            async {
                tokio::time::sleep(Duration::from_millis(10)).await;
                7
            },
            async {
                let _pump = pump_guard;
                let _step = tracker.begin();
                tokio::time::sleep(Duration::from_mins(1)).await;
                std::future::pending::<()>().await;
            },
            tokio::time::Instant::now() + Duration::from_mins(5),
            &tracker,
            Duration::from_secs(2),
        )
        .await;
        assert!(matches!(outcome, Ok(7)));
        assert!(tracker.interrupted(), "a step dropped mid-flight is unknown");
        assert!(!tracker.in_flight());
        assert_eq!(dropped.load(Ordering::SeqCst), 1);
    }

    #[tokio::test(start_paused = true)]
    async fn a_pump_between_steps_is_dropped_at_once_and_is_not_interrupted() {
        let tracker = StepTracker::default();
        let started = tokio::time::Instant::now();
        let outcome = drive_tracked(
            async {
                tokio::time::sleep(Duration::from_millis(10)).await;
                7
            },
            async {
                tracker.begin().finish();
                // Idle sleep between steps: nothing is in flight.
                std::future::pending::<()>().await;
            },
            tokio::time::Instant::now() + Duration::from_secs(30),
            &tracker,
            Duration::from_secs(2),
        )
        .await;
        assert!(matches!(outcome, Ok(7)));
        assert!(!tracker.interrupted());
        assert_eq!(
            tokio::time::Instant::now() - started,
            Duration::from_millis(10),
            "no grace wait when no step was in flight"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn plain_drive_never_waits_for_a_step() {
        // `drive` keeps its original contract: the pump is dropped at once.
        let tracker = StepTracker::default();
        let started = tokio::time::Instant::now();
        let outcome = drive(
            async { 1 },
            async {
                let _step = tracker.begin();
                std::future::pending::<()>().await;
            },
            started + Duration::from_secs(30),
        )
        .await;
        assert!(matches!(outcome, Ok(1)));
        assert_eq!(tokio::time::Instant::now(), started);
    }
}
