//! Submit and reconcile durable jobs without restarting their containers.
use super::request::{Language, PreparedJob};
use crate::protocol::sandbox_grant::AuthorizedJob;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_sandbox::workspace::docker::DockerClient;
use tokio::sync::Semaphore;

use super::ledger::{JobLease, JobLedger, JobRecord, JobScope, LedgerError, Phase};

const LEASE_SECONDS: i32 = 60;
const MAX_JOB_AGE_SECONDS: i64 = 3660;

pub struct DockerSupervisor {
    ledger: JobLedger,
    runtime: DockerClient,
    owner: String,
    capacity: Semaphore,
    admission_policy: Option<(String, Vec<Language>)>,
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
            admission_policy: None,
        })
    }

    /// Enable submission for one deployment-selected immutable policy revision.
    /// # Errors
    /// Returns `Invalid` for malformed or duplicate languages, a malformed revision,
    /// or a compilation policy inconsistent with the admitted languages.
    pub fn with_admission_policy(
        mut self,
        revision: String,
        languages: Vec<Language>,
    ) -> Result<Self, SupervisorError> {
        if languages.is_empty()
            || languages.len() > 4
            || languages
                .iter()
                .enumerate()
                .any(|(i, language)| languages[..i].contains(language))
            || (languages.contains(&Language::Rust) && languages != [Language::Rust])
            || self.runtime.code_compilation_enabled() != (languages == [Language::Rust])
        {
            return Err(SupervisorError::Invalid);
        }
        if revision.is_empty()
            || revision.len() > 128
            || !revision
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
        {
            return Err(SupervisorError::Invalid);
        }
        self.admission_policy = Some((revision, languages));
        Ok(self)
    }

    /// Submit only the exact request covered by a freshly verified grant.
    /// The grant's expiry gates admission, not the lifetime of an admitted job.
    /// # Errors
    /// Returns `Invalid` for an expired/mismatched grant or runtime policy,
    /// `Busy` for admission overload, or a persistence/runtime reconciliation error.
    pub async fn submit_authorized(
        &self,
        authorization: &AuthorizedJob,
        request: &PreparedJob,
    ) -> Result<Reconciliation, SupervisorError> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .ok()
            .and_then(|time| i64::try_from(time.as_millis()).ok())
            .ok_or(SupervisorError::Invalid)?;
        if !authorization.permits(request, now)
            || !request.within_timeout(self.runtime.code_job_timeout())
        {
            return Err(SupervisorError::Invalid);
        }
        if !self
            .admission_policy
            .as_ref()
            .is_some_and(|(policy, languages)| {
                request.matches_runtime(&self.runtime.base_image, policy, languages)
            })
        {
            return Err(SupervisorError::Invalid);
        }
        let _permit = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let scope = authorization.scope();
        let record = self.ledger.reserve(scope).await?;
        if !matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
            return self.terminal(scope, record).await;
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(Reconciliation::OwnedElsewhere);
        };
        self.run_owned(scope, &lease, Some(request)).await
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
            Phase::Reserved if !self.ledger.cancellation_requested(scope).await? => {
                return Ok(Reconciliation::NeedsDispatch);
            }
            Phase::Reserved => {}
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
        if lease.observed_phase != Phase::Dispatched
            && !self.ledger.cancellation_requested(scope).await?
        {
            return Err(SupervisorError::Receipt);
        }
        self.run_owned(scope, &lease, None).await
    }

    async fn run_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: Option<&PreparedJob>,
    ) -> Result<Reconciliation, SupervisorError> {
        let identity = scope.runtime_identity()?;
        let observe = async {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped);
            }
            if lease.observed_phase == Phase::Reserved {
                let request = request.ok_or(SupervisorError::Invalid)?;
                if self
                    .runtime
                    .observe_code_job(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                    .is_none()
                {
                    let manifest = request.manifest().map_err(|_| SupervisorError::Invalid)?;
                    self.runtime
                        .provision_code_job(&identity, &manifest)
                        .await
                        .map_err(SupervisorError::Runtime)?;
                }
                // Do not overwrite a partial workspace: an earlier preparation
                // request may still be completing remotely after loss of its owner.
                while !self
                    .runtime
                    .code_job_prepared(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                        return Ok(stopped);
                    }
                    if self.ledger.age_seconds(scope).await? >= 60 {
                        self.ledger.renew(lease, LEASE_SECONDS).await?;
                        self.runtime
                            .terminate_code_job(&identity)
                            .await
                            .map_err(SupervisorError::Runtime)?;
                        self.ledger
                            .finish(
                                lease,
                                Phase::Failed,
                                None,
                                Some("sandbox.preparation_incomplete"),
                            )
                            .await?;
                        return self.terminal(scope, self.ledger.read(scope).await?).await;
                    }
                    tokio::time::sleep(Duration::from_millis(500)).await;
                }
                self.ledger.mark_dispatched(lease).await?;
                // Uncertain acknowledgement leaves the durable dispatched row;
                // recovery reads the existing runtime, never creates another one.
                self.runtime
                    .dispatch_code_job(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?;
            }
            let mut recover_signal = lease.observed_phase == Phase::Dispatched;
            loop {
                if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                    return Ok(stopped);
                }
                if let Some(bytes) = self
                    .runtime
                    .read_code_job_receipt(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    return self.persist_receipt(scope, lease, &bytes).await;
                }
                // Database time keeps the deadline stable across supervisor restarts.
                if self.ledger.age_seconds(scope).await? >= MAX_JOB_AGE_SECONDS {
                    self.ledger.renew(lease, LEASE_SECONDS).await?;
                    self.runtime
                        .terminate_code_job(&identity)
                        .await
                        .map_err(SupervisorError::Runtime)?;
                    self.ledger
                        .finish(
                            lease,
                            Phase::Failed,
                            None,
                            Some("sandbox.deadline_exceeded"),
                        )
                        .await?;
                    return self.terminal(scope, self.ledger.read(scope).await?).await;
                }
                if recover_signal {
                    // The lease and durable dispatch authorize the existing
                    // runtime. PID 1 consumes its signal once; this never starts
                    // or recreates a container. Check receipt/deadline first.
                    self.ledger.renew(lease, LEASE_SECONDS).await?;
                    self.runtime
                        .dispatch_code_job(&identity)
                        .await
                        .map_err(SupervisorError::Runtime)?;
                    recover_signal = false;
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
                _ = heartbeat.tick() => self.ledger.renew(lease, LEASE_SECONDS).await?,
            }
        }
    }

    async fn stop_if_requested(
        &self,
        scope: &JobScope,
        lease: &JobLease,
    ) -> Result<Option<Reconciliation>, SupervisorError> {
        if !self.ledger.cancellation_requested(scope).await? {
            return Ok(None);
        }
        // Keep the stop intent nonterminal until runtime termination is confirmed.
        // A crash at either boundary leaves the same job eligible for recovery.
        self.ledger.renew(lease, LEASE_SECONDS).await?;
        self.runtime
            .terminate_code_job(&scope.runtime_identity()?)
            .await
            .map_err(SupervisorError::Runtime)?;
        self.ledger
            .finish(lease, Phase::Cancelled, None, Some("sandbox.cancelled"))
            .await?;
        Ok(Some(
            self.terminal(scope, self.ledger.read(scope).await?).await?,
        ))
    }

    async fn persist_receipt(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        bytes: &[u8],
    ) -> Result<Reconciliation, SupervisorError> {
        let (phase, failure) = classify_receipt(bytes)?;
        let result = if phase == Phase::Completed {
            Some(std::str::from_utf8(bytes).map_err(|_| SupervisorError::Receipt)?)
        } else {
            None
        };
        self.ledger.finish(lease, phase, result, failure).await?;
        self.terminal(scope, self.ledger.read(scope).await?).await
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
