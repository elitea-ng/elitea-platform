//! Service the retained Code process across the complete submission observation.

use std::future::Future;

pub(super) enum ObservationFailure<E> {
    Deadline,
    Platform(E),
}

/// Both futures belong to this attempt. Dropping the attempt drops both.
/// The caller retains the original dispatch identity and absolute deadline.
/// Submission includes nonterminal reconciliation and backoff. It yields only
/// a terminal receipt or refusal, so those waits cannot replace the pump.
pub(super) async fn drive<S, P, T, E>(
    submission: S,
    platform: P,
    deadline: tokio::time::Instant,
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
            outcome = &mut submission => Ok(outcome),
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
    async fn fast_pending_and_busy_preserve_one_multistage_step_across_backoff() {
        use crate::sandbox::client::{SandboxCallError, SandboxOutcome};

        let submissions = AtomicUsize::new(0);
        let pump_starts = AtomicUsize::new(0);
        let effects = AtomicUsize::new(0);
        let dropped = Arc::new(AtomicUsize::new(0));
        let pump_guard = DropCount(dropped.clone());
        let (reply, receipt) = oneshot::channel();
        let activation = [7; 32];
        let request = [8; 32];
        let submission = async {
            // Each RPC finishes before the broker's three IO stages. This is
            // transport reconciliation of one original job, not another run.
            for (index, outcome) in [
                Ok(SandboxOutcome::Pending),
                Err(SandboxCallError::Submission {
                    code: tonic::Code::ResourceExhausted,
                }),
                Ok(SandboxOutcome::Pending),
            ]
            .into_iter()
            .enumerate()
            {
                assert_eq!(submissions.fetch_add(1, Ordering::SeqCst), index);
                assert_eq!((activation, request), ([7; 32], [8; 32]));
                assert!(matches!(
                    outcome,
                    Ok(SandboxOutcome::Pending)
                        | Err(SandboxCallError::Submission {
                            code: tonic::Code::ResourceExhausted
                        })
                ));
                tokio::time::sleep(std::time::Duration::from_secs(1)).await;
            }
            submissions.fetch_add(1, Ordering::SeqCst);
            receipt.await.expect("original retained process receipt")
        };
        let platform = async {
            let _guard = pump_guard;
            pump_starts.fetch_add(1, Ordering::SeqCst);
            // Owner read, pending frame read, then committed reply publication.
            // The latter two stages cross separate submission backoff periods.
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
            tokio::time::sleep(std::time::Duration::from_secs(1)).await;
            tokio::time::sleep(std::time::Duration::from_secs(1)).await;
            effects.fetch_add(1, Ordering::SeqCst);
            reply.send(42).expect("original submit remains observed");
            std::future::pending::<()>().await;
        };
        let outcome = drive(
            submission,
            platform,
            tokio::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .await;
        assert!(matches!(outcome, Ok(42)));
        assert_eq!(submissions.load(Ordering::SeqCst), 4);
        assert_eq!(pump_starts.load(Ordering::SeqCst), 1);
        assert_eq!(effects.load(Ordering::SeqCst), 1);
        assert_eq!(dropped.load(Ordering::SeqCst), 1);
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
}
