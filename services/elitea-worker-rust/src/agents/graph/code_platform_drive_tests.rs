//! Worker-side test of the shared Code platform pump driver.
//!
//! It lives in the Worker because it exercises the Worker's sandbox client
//! outcomes (`Pending`, `ResourceExhausted`) across submission backoff, while
//! `drive` itself lives in `elitea_agent_runtime::graph::code_platform_drive`.

use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

use elitea_agent_runtime::graph::code_platform_drive::drive;
use tokio::sync::oneshot;

struct DropCount(Arc<AtomicUsize>);
impl Drop for DropCount {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
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
