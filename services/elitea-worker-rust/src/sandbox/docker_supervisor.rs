//! Reconcile dispatched jobs without executing their code again.
use std::time::Duration;

use adk_sandbox::workspace::docker::DockerClient;
use tokio::sync::Semaphore;

use super::ledger::{JobLedger, JobRecord, JobScope, LedgerError, Phase};

const LEASE_SECONDS: i32 = 60;
const MAX_JOB_AGE_SECONDS: i64 = 3660;

pub struct DockerSupervisor {
    ledger: JobLedger,
    runtime: DockerClient,
    owner: String,
    capacity: Semaphore,
}

/// Receipts contain untrusted code output. Graph state projection remains a
/// separate worker operation after reading the durable terminal receipt.
pub enum Reconciliation {
    NeedsDispatch,
    OwnedElsewhere,
    Terminal {
        record: JobRecord,
        cleanup_pending: bool,
    },
}

#[derive(Debug, thiserror::Error)]
pub enum SupervisorError {
    #[error("sandbox supervisor capacity or identity is invalid")]
    Invalid,
    #[error("sandbox supervisor is at capacity; retry admission later")]
    Busy,
    #[error(transparent)]
    Ledger(#[from] LedgerError),
    #[error("sandbox runtime observation failed; the existing job must be reconciled")]
    Runtime(#[source] adk_sandbox::SandboxError),
    #[error("sandbox terminal receipt is invalid; completion remains unconfirmed")]
    Receipt,
}

impl DockerSupervisor {
    /// # Errors
    /// Returns `Invalid` for an invalid owner or concurrency bound.
    pub fn new(
        ledger: JobLedger,
        runtime: DockerClient,
        owner: String,
        concurrency: usize,
    ) -> Result<Self, SupervisorError> {
        if concurrency == 0
            || concurrency > 1024
            || owner.is_empty()
            || owner.len() > 128
            || owner.contains('\0')
        {
            return Err(SupervisorError::Invalid);
        }
        Ok(Self {
            ledger,
            runtime,
            owner,
            capacity: Semaphore::new(concurrency),
        })
    }

    /// Bounded admission prevents unbounded tasks waiting on the Docker daemon.
    /// Dropping this future leaves the runtime intact and ownership recoverable
    /// after lease expiry. No detached heartbeat survives the operation.
    /// # Errors
    /// Returns `Busy` on overload, or a ledger/runtime error requiring reconciliation.
    pub async fn reconcile_dispatched(
        &self,
        scope: &JobScope,
    ) -> Result<Reconciliation, SupervisorError> {
        let _permit = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let record = self.ledger.read(scope).await?;
        match record.phase {
            Phase::Reserved => return Ok(Reconciliation::NeedsDispatch),
            Phase::Dispatched => {}
            _ => return self.terminal(scope, record).await,
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(Reconciliation::OwnedElsewhere);
        };
        // The claim is the authoritative phase observation, not the earlier read.
        if lease.observed_phase != Phase::Dispatched {
            return Err(SupervisorError::Receipt);
        }
        let identity = scope.runtime_identity()?;
        let observe = async {
            loop {
                if let Some(bytes) = self
                    .runtime
                    .read_code_job_receipt(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    let (phase, failure) = classify_receipt(&bytes)?;
                    let result = if phase == Phase::Completed {
                        Some(std::str::from_utf8(&bytes).map_err(|_| SupervisorError::Receipt)?)
                    } else {
                        None
                    };
                    self.ledger.finish(&lease, phase, result, failure).await?;
                    return self.terminal(scope, self.ledger.read(scope).await?).await;
                }
                // Database time keeps the deadline stable across supervisor restarts.
                if self.ledger.age_seconds(scope).await? >= MAX_JOB_AGE_SECONDS {
                    self.ledger.renew(&lease, LEASE_SECONDS).await?;
                    self.runtime
                        .terminate_code_job(&identity)
                        .await
                        .map_err(SupervisorError::Runtime)?;
                    self.ledger
                        .finish(
                            &lease,
                            Phase::Failed,
                            None,
                            Some("sandbox.deadline_exceeded"),
                        )
                        .await?;
                    return self.terminal(scope, self.ledger.read(scope).await?).await;
                }
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
        };
        tokio::pin!(observe);
        let mut heartbeat = tokio::time::interval(Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        loop {
            tokio::select! {
                result = &mut observe => return result,
                _ = heartbeat.tick() => self.ledger.renew(&lease, LEASE_SECONDS).await?,
            }
        }
    }

    async fn terminal(
        &self,
        scope: &JobScope,
        record: JobRecord,
    ) -> Result<Reconciliation, SupervisorError> {
        // A durable receipt is sufficient for recovery even when cleanup fails.
        // Never force-remove a live workload or discard the persisted result.
        let identity = scope.runtime_identity()?;
        let cleanup_pending = self.runtime.remove_code_job(&identity).await.is_err();
        if cleanup_pending {
            tracing::warn!(
                operation = "sandbox.runtime_cleanup",
                job = ?identity,
                "Sandbox receipt is durable; stopped runtime cleanup needs retry"
            );
        }
        Ok(Reconciliation::Terminal {
            record,
            cleanup_pending,
        })
    }
}

fn classify_receipt(bytes: &[u8]) -> Result<(Phase, Option<&'static str>), SupervisorError> {
    if bytes.len() > 524_288 {
        return Ok((Phase::Failed, Some("sandbox.receipt_limit")));
    }
    let value: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| SupervisorError::Receipt)?;
    let status = value
        .get("status")
        .and_then(serde_json::Value::as_str)
        .ok_or(SupervisorError::Receipt)?;
    Ok(match status {
        "completed" => (Phase::Completed, None),
        "failed" => (Phase::Failed, Some("sandbox.code_failed")),
        "timeout" => (Phase::Failed, Some("sandbox.deadline_exceeded")),
        "output_limit" => (Phase::Failed, Some("sandbox.output_limit")),
        "capture_failed" => (Phase::Failed, Some("sandbox.capture_failed")),
        "launch_failed" => (Phase::Failed, Some("sandbox.launch_failed")),
        "invalid_request" => (Phase::Failed, Some("sandbox.invalid_request")),
        _ => return Err(SupervisorError::Receipt),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn terminal_failures_do_not_become_successful_graph_results() {
        assert_eq!(
            classify_receipt(br#"{"status":"completed"}"#).unwrap(),
            (Phase::Completed, None)
        );
        assert_eq!(
            classify_receipt(br#"{"status":"failed"}"#).unwrap(),
            (Phase::Failed, Some("sandbox.code_failed"))
        );
        assert_eq!(
            classify_receipt(br#"{"status":"output_limit"}"#).unwrap(),
            (Phase::Failed, Some("sandbox.output_limit"))
        );
        assert!(classify_receipt(br#"{"status":"unknown"}"#).is_err());
        assert!(classify_receipt(b"not-json").is_err());
    }

    #[test]
    fn oversized_receipt_is_rejected_without_truncating_json() {
        assert_eq!(
            classify_receipt(&vec![b' '; 524_289]).unwrap(),
            (Phase::Failed, Some("sandbox.receipt_limit"))
        );
    }
}
