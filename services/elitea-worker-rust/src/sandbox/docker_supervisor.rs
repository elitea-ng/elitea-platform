//! Submit and reconcile durable jobs without restarting their containers.
use super::request::{Language, PreparedJob};
use crate::protocol::sandbox_grant::{AuthorizedCancellation, AuthorizedJob};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use super::runtime::CodeJobRuntime;
use tokio::sync::Semaphore;

use super::ledger::{JobLease, JobLedger, JobRecord, JobScope, LedgerError, Phase};

const LEASE_SECONDS: i32 = 60;
const MAX_JOB_AGE_SECONDS: i64 = 3660;

pub struct DockerSupervisor {
    ledger: JobLedger,
    runtime: Box<dyn CodeJobRuntime>,
    owner: String,
    capacity: Semaphore,
    stop_capacity: Semaphore,
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
        runtime: impl CodeJobRuntime + 'static,
        owner: String,
        concurrency: usize,
    ) -> Result<Self, SupervisorError> {
        Self::with_runtime(ledger, Box::new(runtime), owner, concurrency)
    }

    /// Select a deployment backend while retaining the same durable lifecycle.
    /// # Errors
    /// Returns `Invalid` for an invalid owner or concurrency bound.
    pub fn with_runtime(
        ledger: JobLedger,
        runtime: Box<dyn CodeJobRuntime>,
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
            stop_capacity: Semaphore::new(concurrency.min(16)),
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
                request.matches_runtime(self.runtime.image_digest(), policy, languages)
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

    /// Persist stop intent independently of dispatch capacity, then reconcile.
    /// # Errors
    /// Returns an expired grant, persistence, or runtime error.
    pub async fn cancel_authorized(
        &self,
        authorization: &AuthorizedCancellation,
    ) -> Result<Reconciliation, SupervisorError> {
        if !authorization.valid_at(chrono::Utc::now().timestamp_millis()) {
            return Err(SupervisorError::Invalid);
        }
        let scope = authorization.scope();
        let record = self.ledger.request_cancellation(scope, &self.owner).await?;
        if !matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
            return self.terminal(scope, record).await;
        }
        // Stop admission has bounded capacity separate from occupied execution
        // slots. An authenticated stop fences dispatch without waiting its lease.
        let Ok(_permit) = self.stop_capacity.try_acquire() else {
            return Ok(Reconciliation::OwnedElsewhere);
        };
        let Some(lease) = self
            .ledger
            .claim_cancellation(scope, &self.owner, LEASE_SECONDS)
            .await?
        else {
            let record = self.ledger.read(scope).await?;
            if matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
                return Ok(Reconciliation::OwnedElsewhere);
            }
            return self.terminal(scope, record).await;
        };
        self.run_owned(scope, &lease, None).await
    }

    /// Resume previously authorized stops after process loss, without a client retry.
    /// The stable deployment owner partitions discovery; leases still fence cleanup.
    /// # Errors
    /// Returns a persistence error while discovering the bounded batch.
    pub async fn reconcile_cancellations(&self) -> Result<(), SupervisorError> {
        for scope in self.ledger.pending_cancellations(&self.owner, 32).await? {
            match self.reconcile_dispatched(&scope).await {
                Ok(_) => {}
                Err(SupervisorError::Busy) => break,
                Err(error) => {
                    tracing::error!(error = %error, "sandbox stop remains pending; reconciliation will retry");
                }
            }
        }
        Ok(())
    }

    pub(super) async fn recover_cancellations(&self) {
        let mut interval = tokio::time::interval(Duration::from_secs(2));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            interval.tick().await;
            if let Err(error) = self.reconcile_cancellations().await {
                tracing::error!(error = %error, "sandbox pending stops could not be read; reconciliation will retry");
            }
        }
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
            Phase::Reserved | Phase::Dispatched => {}
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
        let identity = self.bound_identity(scope).await?;
        let observe = async {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped);
            }
            if lease.observed_phase == Phase::Reserved {
                let request = request.ok_or(SupervisorError::Invalid)?;
                if !self
                    .runtime
                    .exists(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    if identity.runtime_id().is_some() {
                        return Err(SupervisorError::Receipt);
                    }
                    let manifest = request.manifest().map_err(|_| SupervisorError::Invalid)?;
                    self.runtime
                        .prepare(&identity, &manifest)
                        .await
                        .map_err(SupervisorError::Runtime)?;
                }
                let runtime_id = self
                    .runtime
                    .instance(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                    .ok_or(SupervisorError::Receipt)?;
                self.ledger.bind_runtime(lease, &runtime_id).await?;
                let identity = identity
                    .with_runtime_id(runtime_id)
                    .map_err(SupervisorError::Runtime)?;
                // Do not overwrite a partial workspace: an earlier preparation
                // request may still be completing remotely after loss of its owner.
                while !self
                    .runtime
                    .prepared(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                        return Ok(stopped);
                    }
                    if self.ledger.readiness_age_seconds(scope).await? >= 60 {
                        return self
                            .fail_expired_job(scope, lease, "sandbox.preparation_incomplete")
                            .await;
                    }
                    tokio::time::sleep(Duration::from_millis(500)).await;
                }
                self.ledger.mark_dispatched(lease).await?;
                // Uncertain acknowledgement leaves the durable dispatched row;
                // recovery reads the existing runtime, never creates another one.
                self.runtime
                    .dispatch(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?;
            }
            let identity = self.bound_identity(scope).await?;
            let mut recover_signal = lease.observed_phase == Phase::Dispatched;
            loop {
                if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                    return Ok(stopped);
                }
                if let Some(bytes) = self
                    .runtime
                    .receipt(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    return self.persist_receipt(scope, lease, &bytes).await;
                }
                // Database time keeps the deadline stable across supervisor restarts.
                if self.ledger.execution_age_seconds(scope).await? >= MAX_JOB_AGE_SECONDS {
                    return self
                        .fail_expired_job(scope, lease, "sandbox.deadline_exceeded")
                        .await;
                }
                if recover_signal {
                    // The lease and durable dispatch authorize the existing
                    // runtime. PID 1 consumes its signal once; this never starts
                    // or recreates a container. Check receipt/deadline first.
                    self.ledger.renew(lease, LEASE_SECONDS).await?;
                    self.runtime
                        .dispatch(&identity)
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

    async fn fail_expired_job(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        code: &'static str,
    ) -> Result<Reconciliation, SupervisorError> {
        self.ledger.renew(lease, LEASE_SECONDS).await?;
        self.runtime
            .terminate(&self.bound_identity(scope).await?)
            .await
            .map_err(SupervisorError::Runtime)?;
        self.ledger
            .finish(lease, Phase::Failed, None, Some(code))
            .await?;
        self.terminal(scope, self.ledger.read(scope).await?).await
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
            .terminate(&self.bound_identity(scope).await?)
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

    async fn bound_identity(
        &self,
        scope: &JobScope,
    ) -> Result<adk_sandbox::workspace::docker::CodeJobIdentity, SupervisorError> {
        let identity = scope.runtime_identity()?;
        match self.ledger.read(scope).await?.runtime_id {
            Some(runtime_id) => identity
                .with_runtime_id(runtime_id)
                .map_err(SupervisorError::Runtime),
            None => Ok(identity), // Compatibility for already dispatched Docker receipts.
        }
    }

    async fn terminal(
        &self,
        scope: &JobScope,
        record: JobRecord,
    ) -> Result<Reconciliation, SupervisorError> {
        // A durable receipt is sufficient for recovery even when cleanup fails.
        // Never force-remove a live workload or discard the persisted result.
        let identity = self.bound_identity(scope).await?;
        let cleanup_pending = self.runtime.cleanup(&identity).await.is_err();
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
        "memory_limit" => (Phase::Failed, Some("sandbox.memory_limit")),
        "timeout" => (Phase::Failed, Some("sandbox.deadline_exceeded")),
        "output_limit" => (Phase::Failed, Some("sandbox.output_limit")),
        "capture_failed" => (Phase::Failed, Some("sandbox.capture_failed")),
        "launch_failed" => (Phase::Failed, Some("sandbox.launch_failed")),
        "invalid_request" => (Phase::Failed, Some("sandbox.invalid_request")),
        _ => return Err(SupervisorError::Receipt),
    })
}

#[cfg(test)]
#[path = "docker_deadline_tests.rs"]
mod deadline_tests;

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
        assert_eq!(
            classify_receipt(br#"{"status":"memory_limit"}"#).unwrap(),
            (Phase::Failed, Some("sandbox.memory_limit"))
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
