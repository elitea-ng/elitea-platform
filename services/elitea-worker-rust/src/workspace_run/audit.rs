//! The executor's audit events (ADR-0034 decision 12): `handoff.scan_blocked`
//! and `pull_request.opened`, sent to Main over the claim-bound
//! `WorkspaceRecordEvent` call, where `RecordExecutorEvent` writes the audit
//! row. Counts, rule ids and the file count only: the event type has no field
//! that could carry a path.
//!
//! Best effort, never silent: events go through a bounded queue to one task
//! per turn that retries each a few times. An audit failure never fails the
//! turn; an event that cannot be queued or delivered is logged (the log line
//! keeps the same data-free fields), never dropped without a trace. The turn
//! flushes the queue before it ends, while its claim still owns the thread.

use std::sync::Arc;
use std::time::Duration;

use tokio::sync::{mpsc, oneshot};

use super::store::{StoreCallError, WorkspaceStore};
use crate::protocol::elitea::runtime::v1::WorkspaceClaimV1;

/// One executor audit event.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum ExecutorEvent {
    /// The hydrated tree held credential-shaped content.
    ScanBlocked { findings: u32, rules: Vec<String> },
    /// `git_push` (or `open_pull_request`) created a branch on the forge.
    BranchPushed {
        toolkit: String,
        branch: String,
        base: String,
        /// The branch's web page.
        url: String,
        commits: u32,
    },
    /// `open_pull_request` opened one.
    PullRequestOpened {
        toolkit: String,
        branch: String,
        base: String,
        number: u64,
        url: Option<String>,
        files: u32,
    },
}

impl ExecutorEvent {
    #[must_use]
    pub(crate) const fn kind(&self) -> &'static str {
        match self {
            Self::ScanBlocked { .. } => "handoff.scan_blocked",
            Self::BranchPushed { .. } => "branch.pushed",
            Self::PullRequestOpened { .. } => "pull_request.opened",
        }
    }
}

/// How many events wait at most; the turn produces a handful.
pub(crate) const QUEUE_CAPACITY: usize = 32;
/// Attempts per event, and the first wait between them (doubling).
const ATTEMPTS: u32 = 3;
const FIRST_RETRY: Duration = Duration::from_millis(200);
/// How long a flush may wait at the end of the turn.
pub(crate) const FLUSH_TIMEOUT: Duration = Duration::from_secs(5);

enum Job {
    Event(ExecutorEvent),
    Flush(oneshot::Sender<()>),
}

/// One turn's audit queue.
pub(crate) struct ExecutorAudit {
    jobs: mpsc::Sender<Job>,
    execution_id: String,
    /// Events that could not be queued (each one logged).
    dropped: std::sync::atomic::AtomicU64,
}

impl ExecutorAudit {
    /// Start the turn's delivery task (it ends when the queue is dropped).
    #[must_use]
    pub(crate) fn spawn(
        store: Arc<dyn WorkspaceStore>,
        claim: WorkspaceClaimV1,
        execution_id: &str,
    ) -> Arc<Self> {
        let (jobs, mut receiver) = mpsc::channel(QUEUE_CAPACITY);
        let task_execution = execution_id.to_owned();
        tokio::spawn(async move {
            while let Some(job) = receiver.recv().await {
                match job {
                    Job::Event(event) => {
                        deliver(store.as_ref(), &claim, &event, &task_execution).await;
                    }
                    Job::Flush(done) => {
                        let _ = done.send(());
                    }
                }
            }
        });
        Arc::new(Self {
            jobs,
            execution_id: execution_id.to_owned(),
            dropped: std::sync::atomic::AtomicU64::new(0),
        })
    }

    /// Queue an event; a full queue logs the event instead.
    pub(crate) fn record(&self, event: ExecutorEvent) {
        if let Err(error) = self.jobs.try_send(Job::Event(event)) {
            let (mpsc::error::TrySendError::Full(Job::Event(event))
            | mpsc::error::TrySendError::Closed(Job::Event(event))) = error
            else {
                return;
            };
            self.dropped
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            tracing::warn!(
                event = "workspace.audit_dropped",
                audit = event.kind(),
                execution_id = self.execution_id.as_str(),
                reason = "queue_full",
                "an executor audit event could not be queued; it is not recorded in the audit trail"
            );
        }
    }

    /// Wait (at most `timeout`) until every event queued before the call was
    /// delivered or given up.
    pub(crate) async fn flush(&self, timeout: Duration) {
        let (done, wait) = oneshot::channel();
        let flushed = tokio::time::timeout(timeout, async {
            self.jobs.send(Job::Flush(done)).await.ok()?;
            wait.await.ok()
        })
        .await;
        if !matches!(flushed, Ok(Some(()))) {
            tracing::warn!(
                event = "workspace.audit_flush_timeout",
                execution_id = self.execution_id.as_str(),
                "executor audit events were still in flight when the turn ended"
            );
        }
    }
}

async fn deliver(
    store: &dyn WorkspaceStore,
    claim: &WorkspaceClaimV1,
    event: &ExecutorEvent,
    execution_id: &str,
) {
    let mut wait = FIRST_RETRY;
    for attempt in 1..=ATTEMPTS {
        match store.record_event(claim, event).await {
            Ok(()) => return,
            Err(error) if error.retryable() && attempt < ATTEMPTS => {
                tokio::time::sleep(wait).await;
                wait *= 2;
            }
            Err(error) => {
                log_undelivered(event, execution_id, &error);
                return;
            }
        }
    }
}

fn log_undelivered(event: &ExecutorEvent, execution_id: &str, error: &StoreCallError) {
    match event {
        ExecutorEvent::ScanBlocked { findings, rules } => tracing::warn!(
            event = "workspace.audit_undelivered",
            audit = event.kind(),
            execution_id,
            error_code = error.code(),
            findings,
            rules = rules.join(","),
            "an executor audit event was not recorded by Main"
        ),
        ExecutorEvent::BranchPushed {
            toolkit, commits, ..
        } => tracing::warn!(
            event = "workspace.audit_undelivered",
            audit = event.kind(),
            execution_id,
            error_code = error.code(),
            toolkit = toolkit.as_str(),
            commits,
            "an executor audit event was not recorded by Main"
        ),
        ExecutorEvent::PullRequestOpened {
            toolkit,
            number,
            files,
            ..
        } => tracing::warn!(
            event = "workspace.audit_undelivered",
            audit = event.kind(),
            execution_id,
            error_code = error.code(),
            toolkit = toolkit.as_str(),
            number,
            files,
            "an executor audit event was not recorded by Main"
        ),
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::sync::Mutex;
    use std::sync::atomic::Ordering;

    use async_trait::async_trait;

    use super::*;
    use crate::workspace_run::redact::SecretEnv;
    use crate::workspace_run::store::{FetchedThread, PublishSegment, PublishedSegment};

    /// Answers each record-event call from a script, then Ok; can hold every
    /// call until released.
    struct ScriptedStore {
        answers: Mutex<Vec<Result<(), StoreCallError>>>,
        calls: Mutex<Vec<ExecutorEvent>>,
        gate: tokio::sync::Semaphore,
    }

    #[async_trait]
    impl WorkspaceStore for ScriptedStore {
        async fn fetch(
            &self,
            _: &WorkspaceClaimV1,
            _: u64,
            _: u64,
            _: &Path,
        ) -> Result<FetchedThread, StoreCallError> {
            Err(StoreCallError::Malformed("unused"))
        }
        async fn upload(
            &self,
            _: &WorkspaceClaimV1,
            _: &str,
            _: u64,
            _: &Path,
        ) -> Result<bool, StoreCallError> {
            Err(StoreCallError::Malformed("unused"))
        }
        async fn publish(
            &self,
            _: &WorkspaceClaimV1,
            _: &PublishSegment,
        ) -> Result<PublishedSegment, StoreCallError> {
            Err(StoreCallError::Malformed("unused"))
        }
        async fn env_secret(&self, _: &WorkspaceClaimV1) -> Result<SecretEnv, StoreCallError> {
            Err(StoreCallError::Malformed("unused"))
        }
        async fn record_event(
            &self,
            _: &WorkspaceClaimV1,
            event: &ExecutorEvent,
        ) -> Result<(), StoreCallError> {
            let _permit = self.gate.acquire().await.expect("gate");
            self.calls.lock().expect("calls").push(event.clone());
            let mut answers = self.answers.lock().expect("answers");
            if answers.is_empty() {
                Ok(())
            } else {
                answers.remove(0)
            }
        }
    }

    fn scan() -> ExecutorEvent {
        ExecutorEvent::ScanBlocked {
            findings: 1,
            rules: vec!["aws-access-key".to_owned()],
        }
    }

    #[tokio::test]
    async fn a_retryable_failure_is_retried_and_a_refusal_is_given_up() {
        let store = Arc::new(ScriptedStore {
            answers: Mutex::new(vec![
                Err(StoreCallError::Transport { retryable: true }),
                Ok(()),
                Err(StoreCallError::Malformed("refused")),
            ]),
            calls: Mutex::new(Vec::new()),
            gate: tokio::sync::Semaphore::new(1_000),
        });
        let audit = ExecutorAudit::spawn(
            Arc::clone(&store) as Arc<dyn WorkspaceStore>,
            WorkspaceClaimV1::default(),
            "e",
        );
        audit.record(scan());
        audit.record(scan());
        audit.flush(Duration::from_secs(5)).await;
        // The first event: failed once, retried, delivered; the second: refused once, not retried.
        assert_eq!(store.calls.lock().expect("calls").len(), 3);
    }

    #[tokio::test]
    async fn a_full_queue_counts_and_logs_instead_of_blocking() {
        let store = Arc::new(ScriptedStore {
            answers: Mutex::new(Vec::new()),
            calls: Mutex::new(Vec::new()),
            gate: tokio::sync::Semaphore::new(0),
        });
        let audit = ExecutorAudit::spawn(
            Arc::clone(&store) as Arc<dyn WorkspaceStore>,
            WorkspaceClaimV1::default(),
            "e",
        );
        for _ in 0..QUEUE_CAPACITY + 5 {
            audit.record(scan());
        }
        assert!(
            audit.dropped.load(Ordering::Relaxed) >= 4,
            "the overflow is counted (and logged)"
        );
        // A flush never hangs the turn.
        audit.flush(Duration::from_millis(50)).await;
        store.gate.add_permits(1_000);
    }
}
