//! A turn's commit, after its `done` (#1213 stage 5): one FIFO lane per
//! conversation, so the person sees the answer at once while the commits
//! land in order, and the next turn of a conversation starts only once its
//! predecessor's commit is settled (memory recall's "previous turn" rule,
//! `localturn.go`, reads the committed conversation).
//!
//! A commit is retried with backoff on the same execution id and body
//! (idempotent on both; `409 local_turn_already_committed` is a success);
//! a refusal that a retry cannot change is not retried.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures::future::BoxFuture;
use tokio::sync::{mpsc, watch};

use super::api::{ApiError, PlatformApi};

/// One conversation's lane: its jobs, and how many are queued or running.
struct Lane {
    jobs: mpsc::UnboundedSender<BoxFuture<'static, ()>>,
    pending: Arc<watch::Sender<usize>>,
}

/// The commit lanes of every conversation, keyed by [`lane_key`].
#[derive(Default)]
pub struct CommitQueues {
    lanes: Mutex<HashMap<String, Lane>>,
}

/// A conversation's lane: its project and UUID.
#[must_use]
pub fn lane_key(project_id: i64, conversation_uuid: &str) -> String {
    format!("{project_id}/{conversation_uuid}")
}

impl CommitQueues {
    fn lanes(&self) -> std::sync::MutexGuard<'_, HashMap<String, Lane>> {
        self.lanes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Run `job` after every job queued on `key` before it (on a task of
    /// the lane's own, so it outlives the turn that queued it).
    pub fn push(&self, key: &str, job: BoxFuture<'static, ()>) {
        let mut lanes = self.lanes();
        let lane = lanes.entry(key.to_owned()).or_insert_with(|| {
            let (jobs, mut queued) = mpsc::unbounded_channel::<BoxFuture<'static, ()>>();
            let pending = Arc::new(watch::Sender::new(0_usize));
            let counter = pending.clone();
            tokio::spawn(async move {
                while let Some(job) = queued.recv().await {
                    job.await;
                    counter.send_modify(|count| *count = count.saturating_sub(1));
                }
            });
            Lane { jobs, pending }
        });
        lane.pending.send_modify(|count| *count += 1);
        if lane.jobs.send(job).is_err() {
            lane.pending
                .send_modify(|count| *count = count.saturating_sub(1));
        }
    }

    /// Until every job queued on `key` so far has run.
    pub async fn idle(&self, key: &str) {
        let pending = self.lanes().get(key).map(|lane| lane.pending.subscribe());
        if let Some(mut pending) = pending {
            let _ = pending.wait_for(|count| *count == 0).await;
        }
    }

    /// How many commits are queued or running, over every lane.
    #[must_use]
    pub fn pending(&self) -> usize {
        self.lanes()
            .values()
            .map(|lane| *lane.pending.borrow())
            .sum()
    }
}

/// How often, and how patiently, a commit is retried.
#[derive(Clone, Copy, Debug)]
pub struct CommitRetry {
    pub attempts: u32,
    /// The first wait; each later one doubles.
    pub delay: Duration,
}

impl CommitRetry {
    /// From the host's remote-call policy: at most 4 attempts, the first
    /// wait at most half a second.
    #[must_use]
    pub fn from_remote(retry: super::remote_tools::RetryPolicy) -> Self {
        Self {
            attempts: retry.attempts.clamp(1, 4),
            delay: retry.delay.min(Duration::from_millis(500)),
        }
    }
}

/// Whether another attempt may succeed where this one failed.
fn retryable(error: &ApiError) -> bool {
    match error.status {
        None => !matches!(
            error.code.as_str(),
            "identity_changed" | "not_signed_in" | "invalid_request"
        ),
        Some(status) => status == 429 || status >= 500,
    }
}

/// Commit `body` for `execution_id`, retrying what a retry can fix.
///
/// # Errors
///
/// The last attempt's refusal or failure.
pub async fn commit(
    api: &PlatformApi,
    project_id: i64,
    execution_id: &str,
    body: &[u8],
    retry: CommitRetry,
) -> Result<(), ApiError> {
    let mut wait = retry.delay;
    let mut attempt = 1;
    loop {
        match api.commit_turn(project_id, execution_id, body).await {
            Ok(_) => return Ok(()),
            Err(error) if error.code == "local_turn_already_committed" => return Ok(()),
            Err(error) if attempt < retry.attempts && retryable(&error) => {
                log::info!(
                    "a local turn's commit is retried (attempt {attempt} failed: {})",
                    error.code
                );
                tokio::time::sleep(wait).await;
                wait = wait.saturating_mul(2);
                attempt += 1;
            }
            Err(error) => return Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::FutureExt as _;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[tokio::test]
    async fn a_lane_runs_its_jobs_in_order_and_says_when_it_is_idle() {
        let queues = Arc::new(CommitQueues::default());
        let order = Arc::new(Mutex::new(Vec::new()));
        let gate = Arc::new(tokio::sync::Notify::new());
        for (n, wait) in [(1, true), (2, false), (3, false)] {
            let (order, gate) = (order.clone(), gate.clone());
            queues.push(
                "1/c",
                async move {
                    if wait {
                        gate.notified().await;
                    }
                    order.lock().unwrap().push(n);
                }
                .boxed(),
            );
        }
        // Another conversation's lane is not held up by this one.
        let other = Arc::new(AtomicUsize::new(0));
        let counted = other.clone();
        queues.push(
            "1/d",
            async move {
                counted.fetch_add(1, Ordering::SeqCst);
            }
            .boxed(),
        );
        queues.idle("1/d").await;
        assert_eq!(other.load(Ordering::SeqCst), 1);
        assert_eq!(queues.pending(), 3);
        let idle = tokio::spawn({
            let queues = queues.clone();
            async move { queues.idle("1/c").await }
        });
        tokio::task::yield_now().await;
        assert!(!idle.is_finished());
        gate.notify_one();
        idle.await.unwrap();
        assert_eq!(*order.lock().unwrap(), [1, 2, 3]);
        assert_eq!(queues.pending(), 0);
        // A lane never used is idle.
        queues.idle("2/x").await;
    }

    #[test]
    fn only_what_a_retry_can_fix_is_retried() {
        let error = |status: Option<u16>, code: &str| ApiError {
            status,
            code: code.into(),
            message: String::new(),
        };
        assert!(retryable(&error(None, "network")));
        assert!(retryable(&error(None, "timeout")));
        assert!(retryable(&error(Some(503), "local_turn_unavailable")));
        assert!(retryable(&error(Some(429), "rate_limited")));
        assert!(!retryable(&error(None, "identity_changed")));
        assert!(!retryable(&error(Some(410), "local_turn_expired")));
        assert!(!retryable(&error(Some(403), "local_work_disabled")));
    }
}
