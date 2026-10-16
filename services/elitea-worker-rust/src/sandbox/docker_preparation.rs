//! Resolve once, retain the original runtime, and publish the recorded Python bundle.
use super::{
    DockerSupervisor, InertJob, LEASE_SECONDS, Reconciliation, SupervisorError, classify_receipt,
};
use crate::{
    protocol::{
        elitea::runtime::v1::SignedSandboxJobGrantV1,
        sandbox_grant::{AuthorizedContent, AuthorizedPreparation},
    },
    sandbox::{
        dependency_content::{DependencyBundle, DependencyContentClient, DependencyContentError},
        ledger::{JobLease, JobRecord, JobScope, LedgerError, Phase},
        preparation::PreparationJob,
    },
};
use serde::Deserialize;
use std::{sync::Arc, time::Duration};
use tokio::io::AsyncWriteExt as _;

#[cfg(test)]
#[path = "docker_preparation_tests.rs"]
mod tests;

const MARKER_LIMIT: usize = 256 * 1024;

/// A resolution record is durable while shared publication remains pending.
/// Only a terminal completed receipt permits the worker to bind execution content.
pub enum PreparationReconciliation {
    Pending {
        bundle: Option<DependencyBundle>,
    },
    Terminal {
        record: JobRecord,
        cleanup_pending: bool,
    },
}

impl From<Reconciliation> for PreparationReconciliation {
    fn from(outcome: Reconciliation) -> Self {
        match outcome {
            Reconciliation::NeedsDispatch | Reconciliation::OwnedElsewhere => {
                Self::Pending { bundle: None }
            }
            Reconciliation::Terminal {
                record,
                cleanup_pending,
            } => Self::Terminal {
                record,
                cleanup_pending,
            },
        }
    }
}

impl DockerSupervisor {
    /// Admit only the image-owned Python preparer on this supervisor profile.
    /// Execution admission and preparation admission are mutually exclusive.
    /// # Errors
    /// Returns `Invalid` for execution admission, compilation, or an invalid policy revision.
    pub fn with_preparation_policy(mut self, revision: String) -> Result<Self, SupervisorError> {
        if self.admission_policy.is_some()
            || self.runtime.code_compilation_enabled()
            || revision.is_empty()
            || revision.len() > 128
            || !revision
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
        {
            return Err(SupervisorError::Invalid);
        }
        self.preparation_policy = Some(revision);
        Ok(self)
    }

    /// Lookups hold a permit of their own, so saturated execution never fails one.
    pub(crate) fn admit_frozen_lookup(
        &self,
    ) -> Result<tokio::sync::SemaphorePermit<'_>, SupervisorError> {
        self.lookup_capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)
    }

    /// Read an exact frozen Cargo profile without reserving a job or runtime.
    /// # Errors
    /// Rejects expired authority, recorded work, invalid profiles, and uncertain storage.
    pub(crate) async fn lookup_dependencies_authorized(
        &self,
        authorization: &AuthorizedContent,
        grant: &SignedSandboxJobGrantV1,
        request: &PreparationJob,
        client: &DependencyContentClient,
    ) -> Result<Option<DependencyBundle>, SupervisorError> {
        if request.language() != crate::sandbox::request::Language::Rust
            || !request.native()
            || !self.preparation_languages.contains(&request.language())
            || request.platform() != self.native_platform.as_ref()
            || !authorization.valid_at(chrono::Utc::now().timestamp_millis())
            || request
                .fingerprint()
                .map_err(|_| SupervisorError::Invalid)?
                != authorization.scope().digest
            || !request.within_timeout(self.runtime.code_job_timeout())
            || !self
                .preparation_policy
                .as_ref()
                .is_some_and(|policy| request.matches_runtime(self.runtime.image_digest(), policy))
        {
            return Err(SupervisorError::Invalid);
        }
        let _permit = self.admit_frozen_lookup()?;
        // Existing work must reconcile its original preparer and immutable root.
        match self.ledger.read(authorization.scope()).await {
            Err(LedgerError::Missing) => {}
            Ok(_) => return Err(SupervisorError::Invalid),
            Err(error) => return Err(error.into()),
        }
        let root = content_root(authorization.root());
        let selected = client.lookup_native(&root, grant).await?;
        if !authorization.valid_at(chrono::Utc::now().timestamp_millis()) {
            return Err(SupervisorError::Invalid);
        }
        // Valid different declarations/profiles remain fresh ordinary acquisitions.
        Ok(selected.filter(|bundle| request.matches_bundle(bundle)))
    }

    /// Admit or reconcile the exact preparation request without resolving it again.
    /// Source never runs in this operation. A resolved runtime retains its files for publication.
    /// # Errors
    /// Returns invalid authority, capacity, persistence, or original runtime observation errors.
    pub async fn prepare_authorized(
        &self,
        authorization: &AuthorizedPreparation,
        request: &PreparationJob,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        if !self.preparation_languages.contains(&request.language())
            || request.platform() != self.native_platform.as_ref()
            || !authorization.permits(request, chrono::Utc::now().timestamp_millis())
            || !request.within_timeout(self.runtime.code_job_timeout())
            || !self
                .preparation_policy
                .as_ref()
                .is_some_and(|policy| request.matches_runtime(self.runtime.image_digest(), policy))
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
            return self.preparation_terminal(scope, record).await;
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(PreparationReconciliation::Pending { bundle: None });
        };
        let outcome = self.prepare_owned(scope, &lease, request).await;
        if outcome.is_err() && self.ledger.read(scope).await?.phase == Phase::Dispatched {
            self.release_failed_preparation(&lease).await?;
        }
        outcome
    }

    /// Publish one recorded file, or the final metadata, with fresh content-only authority.
    /// This operation cannot reserve a job, provision a runtime, or resolve requirements.
    /// # Errors
    /// Returns invalid authority, capacity, persistence, content, or original runtime errors.
    pub async fn publish_authorized(
        &self,
        authorization: &AuthorizedContent,
        grant: &SignedSandboxJobGrantV1,
        index: u32,
        client: &DependencyContentClient,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        if self.preparation_policy.is_none()
            || !authorization.valid_at(chrono::Utc::now().timestamp_millis())
        {
            return Err(SupervisorError::Invalid);
        }
        let _permit = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let scope = authorization.scope();
        let record = self.ledger.read(scope).await?;
        let bundle = self.ledger.read_preparation_bundle(scope).await?;
        if let Some(bundle) = &bundle
            && (bundle.root() != content_root(authorization.root())
                || usize::try_from(index).map_or(true, |index| index > bundle.file_count()))
        {
            return Err(SupervisorError::Invalid);
        }
        if !matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
            return self.preparation_terminal(scope, record).await;
        }
        let bundle = bundle.ok_or(SupervisorError::Invalid)?;
        if record.phase != Phase::Dispatched {
            return Err(SupervisorError::Invalid);
        }
        let lease = self.claim_indexed_transfer(scope).await?;
        let outcome = self
            .publish_owned(scope, &lease, bundle, grant, index, client)
            .await;
        if outcome.is_err() {
            self.release_failed_preparation(&lease).await?;
        }
        outcome
    }

    pub(super) async fn claim_indexed_transfer(
        &self,
        scope: &JobScope,
    ) -> Result<JobLease, SupervisorError> {
        // Pending acknowledges the requested transfer index. A held lease proves no transfer.
        self.ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
            .ok_or_else(|| crate::sandbox::ledger::LedgerError::Fenced.into())
    }

    async fn publish_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        bundle: DependencyBundle,
        grant: &SignedSandboxJobGrantV1,
        index: u32,
        client: &DependencyContentClient,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        let transfer = async {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped.into());
            }
            let index = usize::try_from(index).map_err(|_| SupervisorError::Invalid)?;
            let identity = self.bound_identity(scope).await?;
            if identity.runtime_id().is_none() {
                return Err(SupervisorError::Receipt);
            }
            if client.confirm_publication(&bundle, grant).await? {
                return self
                    .complete_preparation(scope, lease, &identity, &bundle)
                    .await;
            }
            let staged = self
                .stage_preparation_index(&identity, &bundle, index, client)
                .await?;
            let published: Result<Option<Reconciliation>, SupervisorError> = async {
                self.ledger.renew(lease, LEASE_SECONDS).await?;
                if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                    return Ok(Some(stopped));
                }
                client
                    .publish_index(&bundle, staged.path(), index, grant)
                    .await?;
                Ok(None)
            }
            .await;
            drop(staged);
            if let Some(stopped) = published? {
                return Ok(stopped.into());
            }
            if index < bundle.file_count() {
                return self.handoff_preparation(scope, lease, bundle).await;
            }
            self.complete_preparation(scope, lease, &identity, &bundle)
                .await
        };
        tokio::pin!(transfer);
        let mut heartbeat = tokio::time::interval(Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        loop {
            tokio::select! {
                result = &mut transfer => return result,
                _ = heartbeat.tick() => self.ledger.renew(lease, LEASE_SECONDS).await?,
            }
        }
    }

    async fn stage_preparation_index(
        &self,
        identity: &adk_sandbox::workspace::docker::CodeJobIdentity,
        bundle: &DependencyBundle,
        index: usize,
        client: &DependencyContentClient,
    ) -> Result<Arc<tempfile::TempDir>, SupervisorError> {
        if index == bundle.file_count() {
            return Ok(Arc::new(client.stage_file(bundle, index)?));
        }
        if let Some(staged) = client.cached_file(bundle, index)? {
            return Ok(staged);
        }
        let staged = client.stage_file(bundle, index)?;
        let transfer: Result<(), SupervisorError> = async {
            if let Some(file) = bundle.files().get(index) {
                let mut writer = tokio::fs::OpenOptions::new()
                    .create_new(true)
                    .write(true)
                    .mode(0o600)
                    .open(staged.path().join(file.name()))
                    .await
                    .map_err(DependencyContentError::Staging)?;
                self.runtime
                    .export_preparation_bundle_dependency(
                        identity,
                        bundle,
                        file.name(),
                        file.bytes(),
                        &mut writer,
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                writer
                    .flush()
                    .await
                    .map_err(DependencyContentError::Staging)?;
                writer
                    .sync_all()
                    .await
                    .map_err(DependencyContentError::Staging)?;
                drop(writer);
            }
            Ok(())
        }
        .await;
        if let Err(error) = transfer {
            let _cleanup = staged.close();
            return Err(error);
        }
        client
            .cache_file(bundle, index, staged)
            .await
            .map_err(Into::into)
    }

    async fn complete_preparation(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        identity: &adk_sandbox::workspace::docker::CodeJobIdentity,
        bundle: &DependencyBundle,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(stopped.into());
        }
        self.ledger.renew(lease, LEASE_SECONDS).await?;
        // An unknown release acknowledgement still requires confirmed termination of the original runtime.
        let _release = self
            .runtime
            .release_bundle_preparation(identity, bundle)
            .await;
        self.runtime
            .terminate(identity)
            .await
            .map_err(SupervisorError::Runtime)?;
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(stopped.into());
        }
        let result =
            std::str::from_utf8(bundle.record_json()).map_err(|_| SupervisorError::Receipt)?;
        self.ledger
            .finish(lease, Phase::Completed, Some(result), None)
            .await?;
        self.preparation_terminal(scope, self.ledger.read(scope).await?)
            .await
    }

    async fn prepare_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: &PreparationJob,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        let observe = async {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped.into());
            }
            if lease.observed_phase == Phase::Reserved
                && let Some(stopped) = self.provision_preparer(scope, lease, request).await?
            {
                return Ok(stopped);
            }
            self.observe_preparer(scope, lease, request).await
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

    async fn provision_preparer(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: &PreparationJob,
    ) -> Result<Option<PreparationReconciliation>, SupervisorError> {
        let manifest = request.manifest().map_err(|_| SupervisorError::Invalid)?;
        let identity = match self.provision_inert(scope, lease, &manifest).await? {
            InertJob::Ready(identity) => identity,
            InertJob::Stopped(stopped) => return Ok(Some(stopped.into())),
        };
        self.ledger.mark_dispatched(lease).await?;
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(Some(stopped.into()));
        }
        self.runtime
            .dispatch(&identity)
            .await
            .map_err(SupervisorError::Runtime)?;
        Ok(None)
    }

    async fn observe_preparer(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        request: &PreparationJob,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        let identity = self.bound_identity(scope).await?;
        // Preparation has no compatibility path for an unbound dispatched runtime.
        if identity.runtime_id().is_none() {
            return Err(SupervisorError::Receipt);
        }
        let mut recover_signal = lease.observed_phase == Phase::Dispatched;
        loop {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(stopped.into());
            }
            if let Some(bundle) = self.ledger.read_preparation_bundle(scope).await? {
                return self.handoff_preparation(scope, lease, bundle).await;
            }
            if let Some(bytes) = self
                .runtime
                .bundle_preparation_marker(&identity, request.native())
                .await
                .map_err(SupervisorError::Runtime)?
            {
                let bundle =
                    match parse_marker(&bytes, request, chrono::Utc::now().timestamp_millis()) {
                        Ok(bundle) => bundle,
                        Err(SupervisorError::Receipt) => {
                            // Immutable invalid metadata cannot become publishable on reconciliation.
                            // Keep the job nonterminal until the original runtime is confirmed stopped.
                            return self
                                .fail_expired_job(
                                    scope,
                                    lease,
                                    "sandbox.preparation_invalid_receipt",
                                )
                                .await
                                .map(Into::into);
                        }
                        Err(error) => return Err(error),
                    };
                self.ledger
                    .record_preparation_bundle(lease, &bundle)
                    .await?;
                return self.handoff_preparation(scope, lease, bundle).await;
            }
            if let Some(bytes) = self
                .runtime
                .receipt(&identity)
                .await
                .map_err(SupervisorError::Runtime)?
            {
                let (phase, failure) = classify_receipt(&bytes)?;
                // Successful child exit is insufficient without durable shared content.
                let failure = match (phase, failure) {
                    (Phase::Completed, _) => "sandbox.preparation_unpublished",
                    (_, Some("sandbox.code_failed")) => "sandbox.preparation_failed",
                    (_, Some(code)) => code,
                    _ => return Err(SupervisorError::Receipt),
                };
                return self
                    .fail_expired_job(scope, lease, failure)
                    .await
                    .map(Into::into);
            }
            if self.ledger.execution_age_seconds(scope).await?
                >= i64::from(request.timeout_seconds()) + 60
            {
                return self
                    .fail_expired_job(scope, lease, "sandbox.deadline_exceeded")
                    .await
                    .map(Into::into);
            }
            if recover_signal {
                self.ledger.renew(lease, LEASE_SECONDS).await?;
                if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                    return Ok(stopped.into());
                }
                // The existing PID-1 marker consumes this signal once. No workload is recreated.
                self.runtime
                    .dispatch(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?;
                recover_signal = false;
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    async fn handoff_preparation(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        bundle: DependencyBundle,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(stopped.into());
        }
        self.ledger.release(lease).await?;
        Ok(PreparationReconciliation::Pending {
            bundle: Some(bundle),
        })
    }

    pub(super) async fn release_failed_preparation(
        &self,
        lease: &JobLease,
    ) -> Result<(), SupervisorError> {
        match self.ledger.release(lease).await {
            Ok(()) | Err(crate::sandbox::ledger::LedgerError::Fenced) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    async fn preparation_terminal(
        &self,
        scope: &JobScope,
        record: JobRecord,
    ) -> Result<PreparationReconciliation, SupervisorError> {
        if record.phase == Phase::Completed {
            let bundle = self
                .ledger
                .read_preparation_bundle(scope)
                .await?
                .ok_or(SupervisorError::Receipt)?;
            if record.result_json.as_deref().map(str::as_bytes) != Some(bundle.record_json()) {
                return Err(SupervisorError::Receipt);
            }
        }
        self.terminal(scope, record).await.map(Into::into)
    }
}

fn parse_marker(
    bytes: &[u8],
    request: &PreparationJob,
    now_unix_ms: i64,
) -> Result<DependencyBundle, SupervisorError> {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Marker {
        revision: u8,
        status: String,
        source_sha256: String,
        preparer_image_digest: String,
        policy_revision: String,
        timeout_seconds: u32,
        started_unix_ms: i64,
        deadline_unix_ms: i64,
        bundle: Box<serde_json::value::RawValue>,
    }
    if bytes.len() > MARKER_LIMIT {
        return Err(SupervisorError::Receipt);
    }
    let marker: Marker = serde_json::from_slice(bytes).map_err(|_| SupervisorError::Receipt)?;
    let source_digest = ring::digest::digest(&ring::digest::SHA256, request.source().as_bytes());
    if marker.revision != if request.native() { 2 } else { 1 }
        || marker.status != "resolved"
        || marker.source_sha256 != content_root(source_digest.as_ref())
        || marker.preparer_image_digest != request.image_digest()
        || marker.policy_revision != request.policy_revision()
        || marker.timeout_seconds != request.timeout_seconds()
        || marker.started_unix_ms <= 0
        || marker.started_unix_ms > now_unix_ms
        || marker.deadline_unix_ms <= now_unix_ms
        || marker.deadline_unix_ms.checked_sub(marker.started_unix_ms)
            != Some(i64::from(request.timeout_seconds()) * 1000)
    {
        return Err(SupervisorError::Receipt);
    }
    let bundle = DependencyBundle::parse_record(marker.bundle.get().as_bytes())
        .map_err(|_| SupervisorError::Receipt)?;
    if !request.matches_bundle(&bundle) {
        return Err(SupervisorError::Receipt);
    }
    Ok(bundle)
}

pub(super) fn content_root(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    bytes
        .iter()
        .flat_map(|byte| {
            [
                char::from(DIGITS[usize::from(byte >> 4)]),
                char::from(DIGITS[usize::from(byte & 15)]),
            ]
        })
        .collect()
}
