//! Submit and reconcile durable jobs without restarting their containers.
#[path = "docker_preparation.rs"]
mod preparation;
pub use preparation::PreparationReconciliation;
#[path = "docker_code_platform_owner.rs"]
mod code_platform_owner;
#[path = "docker_code_recovery_cleanup.rs"]
mod code_recovery_cleanup;
#[path = "docker_compiled.rs"]
mod compiled;
#[path = "docker_dependency_delivery.rs"]
mod dependency_delivery;
#[path = "docker_hydration.rs"]
mod hydration;
pub(crate) use compiled::SnapshotReconciliation;

use super::{
    dependency_content::{DependencyBundle, DependencyContentClient, DependencyContentError},
    request::{Language, PreparedJob},
};
use crate::protocol::{
    elitea::runtime::v1::SignedSandboxJobGrantV1,
    sandbox_grant::{AuthorizedCancellation, AuthorizedContent, AuthorizedJob},
};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use super::runtime::CodeJobRuntime;
use tokio::sync::Semaphore;

use super::ledger::{JobLease, JobLedger, JobRecord, JobScope, LedgerError, Phase};

pub(crate) const LEASE_SECONDS: i32 = 60;
pub(crate) const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(20);
const MAX_JOB_AGE_SECONDS: i64 = 3660;
const READINESS_SECONDS: i64 = 60;
// Largest admitted preparation timeout (3600) plus worker recovery allowance (90).
const MAX_HYDRATION_AGE_SECONDS: i64 = 3690;

pub struct DockerSupervisor {
    ledger: JobLedger,
    runtime: Box<dyn CodeJobRuntime>,
    owner: String,
    capacity: Semaphore,
    concurrency: usize,
    stop_capacity: Semaphore,
    admission_policy: Option<(String, Vec<Language>)>,
    preparation_policy: Option<String>,
    preparation_languages: Vec<Language>,
    native_platform: Option<super::native_bundle::NativePlatform>,
    compiled_profile: Option<std::sync::Arc<super::compiled_snapshot::SnapshotProfile>>,
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

enum InertJob {
    Ready(adk_sandbox::workspace::docker::CodeJobIdentity),
    Stopped(Reconciliation),
}

/// Verified content authority belongs to the exact execution request and supervisor.
#[derive(Clone, Copy)]
pub struct DependencyDelivery<'a> {
    pub client: &'a DependencyContentClient,
    pub grant: &'a SignedSandboxJobGrantV1,
    pub authorization: &'a AuthorizedContent,
    pub bundle: &'a DependencyBundle,
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
    #[error(transparent)]
    Content(#[from] DependencyContentError),
}

impl DockerSupervisor {
    pub(crate) async fn register_code_intent(
        &self,
        scope: &JobScope,
        intent: &crate::protocol::sandbox_grant::AuthorizedCodeIntent,
        job: &PreparedJob,
    ) -> Result<(), LedgerError> {
        if self.admission_policy.is_none() {
            return Err(LedgerError::Invalid);
        }
        // Only actual Execute admission calls this. Historical unbound rows
        // cannot acquire whole-Code evidence after they have started.
        self.ledger.reserve(scope).await?;
        self.ledger.register_code_intent(scope, intent).await?;
        self.ledger
            .register_code_platform_binding(scope, intent, job)
            .await
    }
    pub(crate) async fn observe_code_owner(
        &self,
        authority: &crate::protocol::sandbox_grant::AuthorizedCodeRecovery,
    ) -> Result<super::ledger::CodeOwnerObservation, LedgerError> {
        self.ledger.observe_code_owner(authority).await
    }
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
            concurrency,
            stop_capacity: Semaphore::new(concurrency.min(16)),
            admission_policy: None,
            preparation_policy: None,
            preparation_languages: vec![Language::Python],
            native_platform: None,
            compiled_profile: None,
        })
    }

    pub(crate) fn configured_concurrency(&self) -> usize {
        self.concurrency
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
        if self.preparation_policy.is_some()
            || languages.is_empty()
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

    /// Bind supplied native dependencies to one deployment-selected platform.
    /// # Errors
    /// Returns `Invalid` for an unsupported platform, empty languages, or Python admission.
    pub fn with_native_profile(
        mut self,
        platform: super::native_bundle::NativePlatform,
        languages: Vec<Language>,
    ) -> Result<Self, SupervisorError> {
        platform.validate().map_err(|_| SupervisorError::Invalid)?;
        if languages.is_empty() || languages.contains(&Language::Python) {
            return Err(SupervisorError::Invalid);
        }
        self.native_platform = Some(platform);
        if self.preparation_policy.is_some() {
            self.preparation_languages = languages;
        }
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
        self.submit_authorized_with_dependencies(authorization, request, None)
            .await
    }

    /// Submit execution after verifying any content delivery authority for the same job.
    /// Dispatched jobs only reconcile their original runtime and never download again.
    /// # Errors
    /// Returns invalid authority, capacity, persistence, content, or runtime reconciliation errors.
    pub async fn submit_authorized_with_dependencies(
        &self,
        authorization: &AuthorizedJob,
        request: &PreparedJob,
        delivery: Option<DependencyDelivery<'_>>,
    ) -> Result<Reconciliation, SupervisorError> {
        self.validate_execution(authorization, request, delivery)?;
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
        self.run_owned(scope, &lease, Some(request), delivery).await
    }

    fn validate_execution(
        &self,
        authorization: &AuthorizedJob,
        request: &PreparedJob,
        delivery: Option<DependencyDelivery<'_>>,
    ) -> Result<(), SupervisorError> {
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
        if !native_platform_permits(request, self.native_platform.as_ref()) {
            return Err(SupervisorError::Invalid);
        }
        match (request.dependency_bundle_root(), delivery) {
            (Some(root), Some(content))
                if content.authorization.scope() == authorization.scope()
                    && content.authorization.valid_at(now)
                    && content.bundle.root() == root
                    && request.matches_bundle(content.bundle)
                    && preparation::content_root(content.authorization.root()) == root => {}
            (None, None) => {}
            _ => return Err(SupervisorError::Invalid),
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
        Ok(())
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
        self.run_owned(scope, &lease, None, None).await
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

    /// Reclaim expired inert execution allocations without a client retry.
    /// Stable owners partition discovery; preparation profiles keep their own deadline.
    /// # Errors
    /// Returns a persistence error while discovering the bounded batch.
    pub async fn reconcile_expired_hydrations(&self) -> Result<(), SupervisorError> {
        if self.preparation_policy.is_some() {
            return Ok(());
        }
        for scope in self
            .ledger
            .expired_hydrations(&self.owner, MAX_HYDRATION_AGE_SECONDS, 32)
            .await?
        {
            match self.reconcile_expired_hydration(&scope).await {
                Ok(()) => {}
                Err(SupervisorError::Busy) => break,
                Err(error) => {
                    tracing::error!(error = %error, "sandbox inert allocation expiry remains pending; reconciliation will retry");
                }
            }
        }
        Ok(())
    }

    async fn reconcile_expired_hydration(&self, scope: &JobScope) -> Result<(), SupervisorError> {
        let _permit = self
            .stop_capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(());
        };
        // Discovery can race with dispatch. A reclaimed dispatched lease grants no expiry.
        let outcome = if lease.observed_phase == Phase::Reserved {
            self.expire_hydration_if_needed(scope, &lease)
                .await
                .map(|_| ())
        } else {
            Ok(())
        };
        self.release_failed_preparation(&lease).await?;
        outcome
    }

    pub(super) async fn recover_cancellations(&self) {
        let mut interval = tokio::time::interval(Duration::from_secs(2));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            interval.tick().await;
            if let Err(error) = self.reconcile_cancellations().await {
                tracing::error!(error = %error, "sandbox pending stops could not be read; reconciliation will retry");
            }
            if let Err(error) = self.reconcile_expired_hydrations().await {
                tracing::error!(error = %error, "sandbox expired inert allocations could not be read; reconciliation will retry");
            }
            if let Err(error) = self.reconcile_code_no_effect_cleanup().await {
                tracing::error!(error = %error, "sandbox sealed no-effect allocations could not be read; cleanup will retry");
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
        if self.preparation_policy.is_some() && !self.ledger.cancellation_requested(scope).await? {
            return Err(SupervisorError::Invalid);
        }
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
        self.run_owned(scope, &lease, None, None).await
    }

    async fn run_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: Option<&PreparedJob>,
        delivery: Option<DependencyDelivery<'_>>,
    ) -> Result<Reconciliation, SupervisorError> {
        let observe = async {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped);
            }
            if lease.observed_phase == Phase::Reserved {
                let request = request.ok_or(SupervisorError::Invalid)?;
                if request.workspace().is_some() {
                    self.require_workspace_ready(scope, request).await?;
                }
                if let Some(expired) = self.expire_hydration_if_needed(scope, lease).await? {
                    return Ok(expired);
                }
                let manifest = request.manifest().map_err(|_| SupervisorError::Invalid)?;
                let identity = match self.provision_inert(scope, lease, &manifest).await? {
                    InertJob::Ready(identity) => identity,
                    InertJob::Stopped(stopped) => return Ok(stopped),
                };
                self.hydrate_dependencies(scope, &identity, request, delivery)
                    .await?;
                if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                    return Ok(stopped);
                }
                if let Some(expired) = self.expire_hydration_if_needed(scope, lease).await? {
                    return Ok(expired);
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
        let mut heartbeat = tokio::time::interval(HEARTBEAT_INTERVAL);
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        loop {
            tokio::select! {
                result = &mut observe => return result,
                _ = heartbeat.tick() => {
                    if let Some(expired) = self.expire_hydration_if_needed(scope, lease).await? {
                        return Ok(expired);
                    }
                    self.ledger.renew(lease, LEASE_SECONDS).await?;
                }
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
        let (phase, code) = if self.ledger.cancellation_requested(scope).await? {
            (Phase::Cancelled, "sandbox.cancelled")
        } else {
            (Phase::Failed, code)
        };
        self.ledger.finish(lease, phase, None, Some(code)).await?;
        self.terminal(scope, self.ledger.read(scope).await?).await
    }

    async fn expire_hydration_if_needed(
        &self,
        scope: &JobScope,
        lease: &JobLease,
    ) -> Result<Option<Reconciliation>, SupervisorError> {
        if self.preparation_policy.is_some() {
            return Ok(None);
        }
        let record = self.ledger.read(scope).await?;
        if record.phase != Phase::Reserved {
            return Ok(None);
        }
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(Some(stopped));
        }
        if record.runtime_id.is_none()
            || self.ledger.hydration_age_seconds(scope).await? < MAX_HYDRATION_AGE_SECONDS
        {
            return Ok(None);
        }
        self.fail_expired_job(scope, lease, "sandbox.hydration_deadline_exceeded")
            .await
            .map(Some)
    }

    async fn await_prepared(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        identity: &adk_sandbox::workspace::docker::CodeJobIdentity,
    ) -> Result<Option<Reconciliation>, SupervisorError> {
        // An earlier provision request can still finish after its owner disconnects.
        // Retain that workspace while observing its original runtime.
        loop {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(Some(stopped));
            }
            if let Some(expired) = self.expire_hydration_if_needed(scope, lease).await? {
                return Ok(Some(expired));
            }
            if self
                .runtime
                .prepared(identity)
                .await
                .map_err(SupervisorError::Runtime)?
            {
                return Ok(None);
            }
            if self.ledger.readiness_age_seconds(scope).await? >= READINESS_SECONDS {
                return self
                    .fail_expired_job(scope, lease, "sandbox.preparation_incomplete")
                    .await
                    .map(Some);
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    async fn provision_inert(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        manifest: &adk_sandbox::workspace::Manifest,
    ) -> Result<InertJob, SupervisorError> {
        let identity = self.bound_identity(scope).await?;
        if !self
            .runtime
            .exists(&identity)
            .await
            .map_err(SupervisorError::Runtime)?
        {
            if identity.runtime_id().is_some() {
                return Err(SupervisorError::Receipt);
            }
            self.runtime
                .prepare(&identity, manifest)
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
        if let Some(launch) = self
            .ledger
            .code_platform_launch_for_admission(lease)
            .await?
        {
            if self.runtime.retained_code_platform_kind().is_none() || launch.len() > 4096 {
                return Err(SupervisorError::Invalid);
            }
            self.runtime
                .bind_code_platform_launch(&identity, &launch)
                .await
                .map_err(SupervisorError::Runtime)?;
            // Recheck the same live lease/cancel/phase after immutable inert binding.
            if self
                .ledger
                .code_platform_launch_for_admission(lease)
                .await?
                .as_deref()
                != Some(launch.as_slice())
            {
                return Err(SupervisorError::Receipt);
            }
        }
        match self.await_prepared(scope, lease, &identity).await? {
            Some(stopped) => Ok(InertJob::Stopped(stopped)),
            None => Ok(InertJob::Ready(identity)),
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
        let cleanup_pending = match self.ledger.terminal_workspace_runtime(scope).await? {
            Some(receipt) => self
                .runtime
                .cleanup_workspace(&identity, &receipt)
                .await
                .is_err(),
            None => self.runtime.cleanup(&identity).await.is_err(),
        };
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

fn native_platform_permits(
    request: &PreparedJob,
    platform: Option<&super::native_bundle::NativePlatform>,
) -> bool {
    request
        .native_dependencies()
        .is_none_or(|native| Some(&native.platform) == platform)
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
    fn native_platform_admission_keeps_plain_rust_and_rejects_unbound_native_jobs() {
        use crate::sandbox::{
            dependency_bundle::hex,
            native_bundle::{NativeKind, NativePlatform},
            request::NativeDependencies,
        };
        let platform = NativePlatform {
            os: "linux".into(),
            arch: "arm64".into(),
            abi: "gnu".into(),
        };
        let other = NativePlatform {
            arch: "amd64".into(),
            ..platform.clone()
        };
        let plain = PreparedJob::new(
            Language::Rust,
            "pub fn main() {}".into(),
            std::collections::BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "rust-offline-v2".into(),
            30,
        )
        .unwrap();
        assert!(native_platform_permits(&plain, None));
        assert!(native_platform_permits(&plain, Some(&platform)));
        let declaration = "[dependencies]\n";
        let native = plain
            .with_native_dependency_bundle(
                "b".repeat(64),
                NativeDependencies {
                    kind: NativeKind::Cargo,
                    platform: platform.clone(),
                    preparation_sha256: "c".repeat(64),
                    source_sha256: hex(ring::digest::digest(
                        &ring::digest::SHA256,
                        declaration.as_bytes(),
                    )
                    .as_ref()),
                    dependencies_toml: Some(declaration.into()),
                },
            )
            .unwrap();
        assert!(native_platform_permits(&native, Some(&platform)));
        assert!(!native_platform_permits(&native, Some(&other)));
        assert!(!native_platform_permits(&native, None));
    }

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

#[path = "docker_workspace.rs"]
mod workspace_hydration;
#[allow(
    unused_imports,
    reason = "Retain the deferred workspace cursor interface."
)]
pub(crate) use workspace_hydration::{
    WorkspaceAdmission, WorkspaceCursor, WorkspaceReconciliation,
};
