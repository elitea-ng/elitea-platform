//! Separate snapshot roles on the existing durable original-runtime lifecycle.
use super::{
    DependencyContentClient, DependencyContentError, DependencyDelivery, DockerSupervisor,
    Duration, JobLease, JobRecord, JobScope, LEASE_SECONDS, LedgerError, MAX_HYDRATION_AGE_SECONDS,
    MAX_JOB_AGE_SECONDS, Phase, PreparedJob, Reconciliation, SignedSandboxJobGrantV1,
    SupervisorError, classify_receipt, native_platform_permits,
};
use crate::{
    protocol::sandbox_grant::{
        AuthorizedSnapshotCompile, AuthorizedSnapshotExecute, AuthorizedSnapshotPublish,
        AuthorizedSnapshotRead,
    },
    sandbox::compiled_snapshot::{
        ContentSha256, Control, Descriptor, Purpose, SnapshotOperation, SnapshotProfile,
    },
};
use adk_sandbox::workspace::docker::CodeJobIdentity;
use std::{io::Cursor, sync::Arc};
use tokio::{fs, io::AsyncWriteExt as _};
#[path = "docker_compiled_hydration.rs"]
mod hydration;
#[path = "docker_compiled_observability.rs"]
mod observability;
use observability::{Context as DiagnosticContext, Observation, Stage, observe};
pub(crate) enum SnapshotReconciliation {
    Hydrated {
        ready: bool,
    },
    Captured {
        descriptor: Vec<u8>,
        job_key: [u8; 32],
    },
    Outcome(Reconciliation),
}
impl DockerSupervisor {
    /// Deployment assembly must supply the Main-verified exact image/profile attestation.
    /// No environment switch installs it; normal constructors remain disabled.
    #[allow(dead_code)] // Deployment assembly remains gated on verified profile producers.
    pub(crate) fn with_compiled_snapshots(mut self, profile: Arc<SnapshotProfile>) -> Self {
        self.compiled_profile = Some(profile);
        self
    }
    pub(super) fn validate_snapshot(
        &self,
        scope: &JobScope,
        job: &PreparedJob,
        control: &Control,
    ) -> Result<(), SupervisorError> {
        let profile = self
            .compiled_profile
            .as_ref()
            .ok_or(SupervisorError::Invalid)?;
        if profile
            .binding(job, &scope.tenant, scope.project)
            .map_err(|_| SupervisorError::Invalid)?
            != control.binding
            || !self.runtime.code_compilation_enabled()
            || !job.within_timeout(self.runtime.code_job_timeout())
            || !self
                .admission_policy
                .as_ref()
                .is_some_and(|(revision, languages)| {
                    job.matches_runtime(self.runtime.image_digest(), revision, languages)
                })
            || !native_platform_permits(job, self.native_platform.as_ref())
        {
            return Err(SupervisorError::Invalid);
        }
        Ok(())
    }
    pub(crate) async fn reconcile_snapshot_execute(
        &self,
        authority: &AuthorizedSnapshotExecute,
        job: &PreparedJob,
        control: &Control,
        canonical: &[u8],
    ) -> Result<Option<SnapshotReconciliation>, SupervisorError> {
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, chrono::Utc::now().timestamp_millis())
            || control.descriptor_sha256.as_ref() != Some(&ContentSha256::of(canonical))
        {
            return Err(SupervisorError::Invalid);
        }
        let record = match self.ledger.read(scope).await {
            Ok(record) => record,
            Err(LedgerError::Missing) => return Ok(None),
            Err(error) => return Err(error.into()),
        };
        // This path never reserves, provisions, downloads, or signals a Reserved job.
        if record.phase == Phase::Reserved {
            return Ok(None);
        }
        let proof = self
            .ledger
            .read_compiled(scope, control, Purpose::Execute)
            .await?;
        if proof.descriptor.as_deref() != Some(canonical) {
            return Err(SupervisorError::Receipt);
        }
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        if record.phase != Phase::Dispatched {
            return self
                .compiled_terminal(scope, record)
                .await
                .map(|outcome| Some(SnapshotReconciliation::Outcome(outcome)));
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(Some(SnapshotReconciliation::Outcome(
                Reconciliation::OwnedElsewhere,
            )));
        };
        self.snapshot_heartbeat(
            scope,
            &lease,
            self.observe_snapshot(scope, &lease, control, None, false),
            None,
        )
        .await
        .map(Some)
    }
    #[allow(clippy::too_many_arguments)] // Both indexed and final Execute admission use the same complete role and descriptor proof.
    pub(super) fn validate_snapshot_execute(
        &self,
        authority: &AuthorizedSnapshotExecute,
        read: &AuthorizedSnapshotRead,
        job: &PreparedJob,
        control: &Control,
        descriptor: &Descriptor,
        canonical: &[u8],
    ) -> Result<(), SupervisorError> {
        let now = chrono::Utc::now().timestamp_millis();
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, now)
            || !read.permits(job, control, now)
            || read.scope() != scope
            || read.root()
                != control
                    .descriptor_sha256
                    .as_ref()
                    .and_then(|value| value.raw().ok())
                    .as_ref()
        {
            return Err(SupervisorError::Invalid);
        }
        descriptor
            .validate(control)
            .map_err(|_| SupervisorError::Invalid)?;
        if descriptor.bytes().map_err(|_| SupervisorError::Invalid)? != canonical
            || control.descriptor_sha256.as_ref() != Some(&ContentSha256::of(canonical))
        {
            return Err(SupervisorError::Invalid);
        }
        Ok(())
    }
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    #[allow(clippy::too_many_arguments)] // Carry separate signed roles alongside their exact immutable content proof.
    pub(crate) async fn submit_snapshot_execute(
        &self,
        authority: &AuthorizedSnapshotExecute,
        read: &AuthorizedSnapshotRead,
        job: &PreparedJob,
        control: &Control,
        descriptor: &Descriptor,
        canonical: &[u8],
        grant: &SignedSandboxJobGrantV1,
        content: &DependencyContentClient,
        delivery: Option<DependencyDelivery<'_>>,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        self.validate_snapshot_execute(authority, read, job, control, descriptor, canonical)?;
        let scope = authority.scope();
        let base_scope = native_snapshot_scope(
            scope,
            job,
            delivery.map(|delivery| (delivery.authorization, delivery.bundle)),
        )?;
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let record = self.ledger.reserve(scope).await?;
        if !matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
            self.ledger
                .read_compiled(scope, control, Purpose::Execute)
                .await?;
            return self
                .compiled_terminal(scope, record)
                .await
                .map(SnapshotReconciliation::Outcome);
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(SnapshotReconciliation::Outcome(
                Reconciliation::OwnedElsewhere,
            ));
        };
        let operation = async {
            if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            if lease.observed_phase == Phase::Reserved {
                self.ledger
                    .record_compiled_intent(&lease, control, Purpose::Execute, Some(canonical))
                    .await?;
                let identity = self
                    .provision_snapshot(scope, &lease, job, control, Purpose::Execute)
                    .await?;
                let control_bytes = control
                    .bytes(Purpose::Execute)
                    .map_err(|_| SupervisorError::Invalid)?;
                self.import_snapshot(
                    &identity,
                    SnapshotOperation::Control,
                    &control_bytes,
                    &control_bytes,
                )
                .await?;
                self.hydrate_dependencies(&base_scope, &identity, job, delivery)
                    .await?;
                let (directory, mut executable) = content
                    .download_compiled(descriptor, canonical, grant)
                    .await?;
                self.import_snapshot(
                    &identity,
                    SnapshotOperation::Descriptor,
                    &control_bytes,
                    canonical,
                )
                .await?;
                self.runtime
                    .compiled_transfer(
                        &identity,
                        SnapshotOperation::Executable,
                        &control_bytes,
                        descriptor.executable_bytes,
                        Some(descriptor.executable_sha256.as_str()),
                        &mut executable,
                        &mut tokio::io::sink(),
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                drop(executable);
                directory.close().map_err(DependencyContentError::Staging)?;
                self.runtime
                    .compiled_transfer(
                        &identity,
                        SnapshotOperation::Finalize,
                        &control_bytes,
                        0,
                        None,
                        &mut tokio::io::empty(),
                        &mut tokio::io::sink(),
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                if let Some(stopped) = self.stop_if_requested(scope, &lease).await? {
                    return Ok(SnapshotReconciliation::Outcome(stopped));
                }
                self.ledger.mark_dispatched(&lease).await?;
                self.runtime
                    .dispatch(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?;
            } else {
                self.ledger
                    .read_compiled(scope, control, Purpose::Execute)
                    .await?;
            }
            self.observe_snapshot(scope, &lease, control, None, false)
                .await
        };
        self.snapshot_heartbeat(scope, &lease, operation, None)
            .await
    }
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    pub(crate) async fn submit_snapshot_compile(
        &self,
        authority: &AuthorizedSnapshotCompile,
        job: &PreparedJob,
        control: &Control,
        delivery: Option<DependencyDelivery<'_>>,
        content: &DependencyContentClient,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        let scope = authority.scope();
        self.validate_snapshot(scope, job, control)?;
        if !authority.permits(job, control, chrono::Utc::now().timestamp_millis()) {
            return Err(SupervisorError::Invalid);
        }
        let base_scope = native_snapshot_scope(
            scope,
            job,
            delivery.map(|delivery| (delivery.authorization, delivery.bundle)),
        )?;
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let record = self.ledger.reserve(scope).await?;
        if !matches!(record.phase, Phase::Reserved | Phase::Dispatched) {
            let proof = self
                .ledger
                .read_compiled(scope, control, Purpose::Compile)
                .await?;
            let completed = record.phase == Phase::Completed;
            let outcome = self.compiled_terminal(scope, record).await?;
            if completed {
                return Ok(SnapshotReconciliation::Captured {
                    descriptor: proof.descriptor.ok_or(SupervisorError::Receipt)?,
                    job_key: scope.key,
                });
            }
            return Ok(SnapshotReconciliation::Outcome(outcome));
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(SnapshotReconciliation::Outcome(
                Reconciliation::OwnedElsewhere,
            ));
        };
        let diagnostic = DiagnosticContext::capture(scope, &lease).claimed(&lease);
        let operation = async {
            if let Some(stopped) = observe(
                diagnostic,
                Stage::StopCheck,
                self.stop_if_requested(scope, &lease),
            )
            .await?
            {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            if lease.observed_phase == Phase::Reserved {
                observe(diagnostic, Stage::IntentRecord, async {
                    Ok(self
                        .ledger
                        .record_compiled_intent(&lease, control, Purpose::Compile, None)
                        .await?)
                })
                .await?;
                let identity = observe(
                    diagnostic,
                    Stage::Provision,
                    self.provision_snapshot(scope, &lease, job, control, Purpose::Compile),
                )
                .await?;
                observe(diagnostic, Stage::ControlImport, async {
                    let bytes = control
                        .bytes(Purpose::Compile)
                        .map_err(|_| SupervisorError::Invalid)?;
                    self.import_snapshot(&identity, SnapshotOperation::Control, &bytes, &bytes)
                        .await
                })
                .await?;
                observe(
                    diagnostic,
                    Stage::DependencyHydration,
                    self.hydrate_dependencies(&base_scope, &identity, job, delivery),
                )
                .await?;
                if let Some(stopped) = observe(
                    diagnostic,
                    Stage::StopCheck,
                    self.stop_if_requested(scope, &lease),
                )
                .await?
                {
                    return Ok(SnapshotReconciliation::Outcome(stopped));
                }
                observe(diagnostic, Stage::DispatchRecord, async {
                    Ok(self.ledger.mark_dispatched(&lease).await?)
                })
                .await?;
                observe(diagnostic, Stage::RuntimeDispatch, async {
                    self.runtime
                        .dispatch(&identity)
                        .await
                        .map_err(SupervisorError::Runtime)
                })
                .await?;
            } else {
                observe(diagnostic, Stage::CompiledRecordRead, async {
                    Ok(self
                        .ledger
                        .read_compiled(scope, control, Purpose::Compile)
                        .await?)
                })
                .await?;
            }
            self.capture_snapshot_owned(scope, &lease, control, content)
                .await
        };
        let result = self
            .snapshot_heartbeat(scope, &lease, operation, Some(diagnostic))
            .await?;
        if matches!(&result, SnapshotReconciliation::Captured { .. }) {
            observe(diagnostic, Stage::LeaseRelease, async {
                Ok(self.ledger.release(&lease).await?)
            })
            .await?;
        }
        Ok(result)
    }
    async fn capture_snapshot_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        control: &Control,
        content: &DependencyContentClient,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        let diagnostic = DiagnosticContext::capture(scope, lease);
        let recorded = observe(diagnostic, Stage::CompiledRecordRead, async {
            Ok(self
                .ledger
                .read_compiled(scope, control, Purpose::Compile)
                .await?)
        })
        .await?;
        let diagnostic = diagnostic.with_export_epoch(
            recorded
                .export_epoch
                .and_then(|epoch| u64::try_from(epoch).ok()),
        );
        let result = observe(
            diagnostic,
            Stage::CaptureObservation,
            self.observe_snapshot(scope, lease, control, recorded.descriptor.as_deref(), true),
        )
        .await?;
        if let SnapshotReconciliation::Captured { descriptor, .. } = &result {
            let value: Descriptor = Observation::start(diagnostic, Stage::DescriptorDecode)
                .finish(serde_json::from_slice(descriptor).map_err(|_| SupervisorError::Receipt))?;
            let identity = observe(
                diagnostic,
                Stage::RuntimeBinding,
                self.bound_identity(scope),
            )
            .await?;
            let directory = observe(
                diagnostic,
                Stage::Export,
                self.export_snapshot(&identity, control, &value, descriptor, content),
            )
            .await?;
            // The quiet export proof and both content hashes precede this fence.
            let export_epoch = recorded
                .export_epoch
                .or(Some(lease.epoch()))
                .and_then(|epoch| u64::try_from(epoch).ok());
            observability::observe_export_record(diagnostic, export_epoch, async {
                self.ledger
                    .record_compiled_export(
                        lease,
                        &value,
                        identity.runtime_id().ok_or(SupervisorError::Receipt)?,
                    )
                    .await?;
                Ok(())
            })
            .await?;
            Observation::start(
                diagnostic.with_export_epoch(export_epoch),
                Stage::StagingClose,
            )
            .finish(
                directory
                    .close()
                    .map_err(DependencyContentError::Staging)
                    .map_err(SupervisorError::Content),
            )?;
        }
        Ok(result)
    }
    async fn snapshot_heartbeat<T>(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        operation: impl std::future::Future<Output = Result<T, SupervisorError>>,
        diagnostic: Option<DiagnosticContext<'_>>,
    ) -> Result<T, SupervisorError> {
        tokio::pin!(operation);
        let mut heartbeat = tokio::time::interval(Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        loop {
            tokio::select! {
                result = &mut operation => return result,
                _ = heartbeat.tick() => {
                    observability::observe_optional(diagnostic, Stage::LeaseRenew, async {
                        Ok(self.ledger.renew(lease, LEASE_SECONDS).await?)
                    }).await?;
                    if observability::observe_optional(diagnostic, Stage::HydrationAgeRead, async {
                        Ok(self.ledger.hydration_age_seconds(scope).await?)
                    }).await? > MAX_HYDRATION_AGE_SECONDS && observability::observe_optional(diagnostic, Stage::PhaseRead, async {
                        Ok(self.ledger.read(scope).await?)
                    }).await?.phase == Phase::Reserved {
                        return match diagnostic {
                            Some(diagnostic) => Observation::start(diagnostic, Stage::HydrationAgeLimit).finish(Err(SupervisorError::Receipt)),
                            None => Err(SupervisorError::Receipt),
                        };
                    }
                }
            }
        }
    }
    pub(super) fn snapshot_launch_policy(
        &self,
        control: &Control,
    ) -> Result<&str, SupervisorError> {
        let (revision, _) = self
            .admission_policy
            .as_ref()
            .ok_or(SupervisorError::Invalid)?;
        let image = self.runtime.image_digest();
        let digest = image.rsplit_once('@').map_or(image, |(_, digest)| digest);
        if control.binding.policy_revision != *revision
            || control.binding.compilation_image_digest != digest
            || control.binding.execution_image_digest != digest
        {
            return Err(SupervisorError::Invalid);
        }
        Ok(revision)
    }
    pub(super) async fn verify_snapshot_launch(
        &self,
        identity: &CodeJobIdentity,
        control: &Control,
        purpose: Purpose,
    ) -> Result<(), SupervisorError> {
        let bytes = control
            .bytes(purpose)
            .map_err(|_| SupervisorError::Invalid)?;
        self.runtime
            .validate_compiled_launch(
                identity,
                match purpose {
                    Purpose::Compile => "compile",
                    Purpose::Execute => "execute",
                },
                ContentSha256::of(&bytes).as_str(),
                self.snapshot_launch_policy(control)?,
            )
            .await
            .map_err(SupervisorError::Runtime)
    }
    async fn provision_snapshot(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        job: &PreparedJob,
        control: &Control,
        purpose: Purpose,
    ) -> Result<CodeJobIdentity, SupervisorError> {
        if job.workspace().is_some() {
            self.require_workspace_ready(scope, job).await?;
        }
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
            let manifest = job
                .compiled_manifest(purpose)
                .map_err(|_| SupervisorError::Invalid)?;
            let bytes = control
                .bytes(purpose)
                .map_err(|_| SupervisorError::Invalid)?;
            self.runtime
                .prepare_compiled(
                    &identity,
                    &manifest,
                    match purpose {
                        Purpose::Compile => "compile",
                        Purpose::Execute => "execute",
                    },
                    ContentSha256::of(&bytes).as_str(),
                    self.snapshot_launch_policy(control)?,
                )
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
        self.verify_snapshot_launch(&identity, control, purpose)
            .await?;
        // Readiness failure remains a durable terminal receipt or a fenced observation.
        if self
            .await_prepared(scope, lease, &identity)
            .await?
            .is_some()
        {
            return Err(SupervisorError::Receipt);
        }
        Ok(identity)
    }
    async fn import_snapshot(
        &self,
        identity: &CodeJobIdentity,
        operation: SnapshotOperation,
        control: &[u8],
        bytes: &[u8],
    ) -> Result<(), SupervisorError> {
        self.runtime
            .compiled_transfer(
                identity,
                operation,
                control,
                bytes.len() as u64,
                Some(ContentSha256::of(bytes).as_str()),
                &mut Cursor::new(bytes),
                &mut tokio::io::sink(),
            )
            .await
            .map_err(SupervisorError::Runtime)
    }
    async fn observe_snapshot(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        control: &Control,
        expected: Option<&[u8]>,
        capture: bool,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        let identity = self.bound_identity(scope).await?;
        if identity.runtime_id().is_none() {
            return Err(SupervisorError::Receipt);
        }
        self.verify_snapshot_launch(
            &identity,
            control,
            if control.descriptor_sha256.is_some() {
                Purpose::Execute
            } else {
                Purpose::Compile
            },
        )
        .await?;
        let mut recover_signal = lease.observed_phase == Phase::Dispatched;
        loop {
            if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            if let Some(bytes) = self
                .runtime
                .receipt(&identity)
                .await
                .map_err(SupervisorError::Runtime)?
            {
                if control.descriptor_sha256.is_none()
                    && classify_receipt(&bytes)?.0 == Phase::Completed
                {
                    let expected = expected.ok_or(SupervisorError::Receipt)?;
                    validate_compile_receipt(&bytes, expected)?;
                }
                self.persist_receipt(scope, lease, &bytes).await?;
                return self
                    .compiled_terminal(scope, self.ledger.read(scope).await?)
                    .await
                    .map(SnapshotReconciliation::Outcome);
            }
            if self.ledger.execution_age_seconds(scope).await? >= MAX_JOB_AGE_SECONDS {
                return self
                    .fail_expired_job(scope, lease, "sandbox.deadline_exceeded")
                    .await
                    .map(SnapshotReconciliation::Outcome);
            }
            if capture
                && let Some(probe) = self
                    .runtime
                    .compiled_descriptor_probe(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?
            {
                let value: Descriptor =
                    serde_json::from_slice(&probe).map_err(|_| SupervisorError::Receipt)?;
                value
                    .validate(control)
                    .map_err(|_| SupervisorError::Receipt)?;
                if value.bytes().map_err(|_| SupervisorError::Receipt)? != probe {
                    return Err(SupervisorError::Receipt);
                }
                let mut proof = Vec::new();
                let control_bytes = control
                    .bytes(Purpose::Compile)
                    .map_err(|_| SupervisorError::Invalid)?;
                self.runtime
                    .compiled_transfer(
                        &identity,
                        SnapshotOperation::Status,
                        &control_bytes,
                        probe.len() as u64,
                        None,
                        &mut tokio::io::empty(),
                        &mut proof,
                    )
                    .await
                    .map_err(SupervisorError::Runtime)?;
                if proof != probe {
                    return Err(SupervisorError::Receipt);
                }
                return Ok(SnapshotReconciliation::Captured {
                    descriptor: proof,
                    job_key: scope.key,
                });
            }
            if recover_signal {
                self.ledger.renew(lease, LEASE_SECONDS).await?;
                self.runtime
                    .dispatch(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)?;
                recover_signal = false;
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }
    async fn export_snapshot(
        &self,
        identity: &CodeJobIdentity,
        control: &Control,
        descriptor: &Descriptor,
        canonical: &[u8],
        content: &DependencyContentClient,
    ) -> Result<tempfile::TempDir, SupervisorError> {
        let control_bytes = control
            .bytes(Purpose::Compile)
            .map_err(|_| SupervisorError::Invalid)?;
        let mut proof = Vec::new();
        self.runtime
            .compiled_transfer(
                identity,
                SnapshotOperation::ReadDescriptor,
                &control_bytes,
                canonical.len() as u64,
                Some(ContentSha256::of(canonical).as_str()),
                &mut tokio::io::empty(),
                &mut proof,
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        if proof != canonical {
            return Err(SupervisorError::Receipt);
        }
        let directory = content.stage_compiled()?;
        let mut output = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(directory.path().join("elitea-code-job"))
            .await
            .map_err(DependencyContentError::Staging)?;
        self.runtime
            .compiled_transfer(
                identity,
                SnapshotOperation::ReadExecutable,
                &control_bytes,
                descriptor.executable_bytes,
                Some(descriptor.executable_sha256.as_str()),
                &mut tokio::io::empty(),
                &mut output,
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        output
            .flush()
            .await
            .map_err(DependencyContentError::Staging)?;
        drop(output);
        drop(
            content
                .verify_compiled_export(directory.path(), descriptor)
                .await?,
        );
        Ok(directory)
    }
    async fn compiled_terminal(
        &self,
        scope: &JobScope,
        record: JobRecord,
    ) -> Result<Reconciliation, SupervisorError> {
        let identity = self.bound_identity(scope).await?;
        let runtime_id = identity
            .runtime_id()
            .ok_or(SupervisorError::Receipt)?
            .to_owned();
        let result = self.terminal(scope, record).await?;
        if matches!(
            &result,
            Reconciliation::Terminal {
                cleanup_pending: false,
                ..
            }
        ) {
            self.ledger
                .confirm_compiled_cleanup(scope, &runtime_id)
                .await?;
        }
        Ok(result)
    }
    #[allow(clippy::too_many_lines)] // Keep ordered authority checks and durable phase fences visible together.
    pub(crate) async fn publish_snapshot(
        &self,
        authority: &AuthorizedSnapshotPublish,
        grant: &SignedSandboxJobGrantV1,
        phase: i32,
        content: &DependencyContentClient,
    ) -> Result<SnapshotReconciliation, SupervisorError> {
        use crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1;
        let scope = authority.scope();
        if !authority.valid_at(chrono::Utc::now().timestamp_millis())
            || scope.key != authority.compilation_job_key
            || self.compiled_profile.is_none()
        {
            return Err(SupervisorError::Invalid);
        }
        let canonical = self.ledger.compiled_descriptor(scope).await?;
        let descriptor: Descriptor =
            serde_json::from_slice(&canonical).map_err(|_| SupervisorError::Receipt)?;
        if descriptor.bytes().map_err(|_| SupervisorError::Receipt)? != canonical
            || !authority.binds_descriptor(&descriptor)
        {
            return Err(SupervisorError::Invalid);
        }
        let control = Control {
            revision: 1,
            binding: descriptor.binding.clone(),
            snapshot_key_sha256: descriptor.snapshot_key_sha256.clone(),
            descriptor_sha256: None,
        };
        if control
            .intent_digest(Purpose::Compile)
            .map_err(|_| SupervisorError::Invalid)?
            != scope.digest
        {
            return Err(SupervisorError::Invalid);
        }
        let recorded = self
            .ledger
            .read_compiled(scope, &control, Purpose::Compile)
            .await?;
        let record = self.ledger.read(scope).await?;
        if !recorded.export_verified
            || recorded
                .export_epoch
                .and_then(|epoch| u64::try_from(epoch).ok())
                != Some(authority.compilation_lease_epoch)
            || record.runtime_id.as_deref() != Some(&authority.compilation_runtime_id)
        {
            return Err(SupervisorError::Invalid);
        }
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let phase = RustCompiledPublicationPhaseV1::try_from(phase)
            .map_err(|_| SupervisorError::Invalid)?;
        let diagnostic =
            DiagnosticContext::publication(scope, phase as i32, authority.compilation_lease_epoch);
        if phase == RustCompiledPublicationPhaseV1::Ready {
            if record.phase != Phase::Completed || !recorded.cleanup_confirmed {
                return Err(SupervisorError::Receipt);
            }
            validate_compile_receipt(
                record
                    .result_json
                    .as_deref()
                    .ok_or(SupervisorError::Receipt)?
                    .as_bytes(),
                &canonical,
            )?;
            observe(diagnostic, Stage::ReadyPublication, async {
                Ok(content.publish_compiled_ready(&canonical, grant).await?)
            })
            .await?;
            return Ok(SnapshotReconciliation::Outcome(Reconciliation::Terminal {
                record,
                cleanup_pending: false,
            }));
        }
        if !matches!(
            phase,
            RustCompiledPublicationPhaseV1::Executable | RustCompiledPublicationPhaseV1::Release
        ) {
            return Err(SupervisorError::Invalid);
        }
        if record.phase != Phase::Dispatched {
            if phase == RustCompiledPublicationPhaseV1::Release && record.phase == Phase::Completed
            {
                validate_compile_receipt(
                    record
                        .result_json
                        .as_deref()
                        .ok_or(SupervisorError::Receipt)?
                        .as_bytes(),
                    &canonical,
                )?;
                return self
                    .compiled_terminal(scope, record)
                    .await
                    .map(SnapshotReconciliation::Outcome);
            }
            return Err(SupervisorError::Receipt);
        }
        let Some(lease) = self
            .ledger
            .claim(scope, self.owner.clone(), LEASE_SECONDS)
            .await?
        else {
            return Ok(SnapshotReconciliation::Outcome(
                Reconciliation::OwnedElsewhere,
            ));
        };
        let diagnostic = diagnostic.claimed(&lease);
        let operation = async {
            if let Some(stopped) = observe(
                diagnostic,
                Stage::StopCheck,
                self.stop_if_requested(scope, &lease),
            )
            .await?
            {
                return Ok(SnapshotReconciliation::Outcome(stopped));
            }
            let identity = observe(
                diagnostic,
                Stage::RuntimeBinding,
                self.bound_identity(scope),
            )
            .await?;
            observe(
                diagnostic,
                Stage::LaunchVerification,
                self.verify_snapshot_launch(&identity, &control, Purpose::Compile),
            )
            .await?;
            if phase == RustCompiledPublicationPhaseV1::Executable {
                let directory = observe(
                    diagnostic,
                    Stage::Export,
                    self.export_snapshot(&identity, &control, &descriptor, &canonical, content),
                )
                .await?;
                observe(diagnostic, Stage::ContentStaging, async {
                    Ok(content
                        .stage_compiled_publication(
                            directory.path(),
                            &descriptor,
                            &canonical,
                            grant,
                        )
                        .await?)
                })
                .await?;
                Observation::start(diagnostic, Stage::StagingClose).finish(
                    directory
                        .close()
                        .map_err(DependencyContentError::Staging)
                        .map_err(SupervisorError::Content),
                )?;
                return Ok(SnapshotReconciliation::Captured {
                    descriptor: canonical,
                    job_key: scope.key,
                });
            }
            if observe(diagnostic, Stage::ReceiptRead, async {
                self.runtime
                    .receipt(&identity)
                    .await
                    .map_err(SupervisorError::Runtime)
            })
            .await?
            .is_none()
            {
                // Never release the only original copy before Main acknowledges verified staging.
                let directory = observe(
                    diagnostic,
                    Stage::Export,
                    self.export_snapshot(&identity, &control, &descriptor, &canonical, content),
                )
                .await?;
                observe(diagnostic, Stage::ContentStaging, async {
                    Ok(content
                        .stage_compiled_publication(
                            directory.path(),
                            &descriptor,
                            &canonical,
                            grant,
                        )
                        .await?)
                })
                .await?;
                Observation::start(diagnostic, Stage::StagingClose).finish(
                    directory
                        .close()
                        .map_err(DependencyContentError::Staging)
                        .map_err(SupervisorError::Content),
                )?;
                observe(diagnostic, Stage::ReleaseTransfer, async {
                    let bytes = control
                        .bytes(Purpose::Compile)
                        .map_err(|_| SupervisorError::Invalid)?;
                    self.runtime
                        .compiled_transfer(
                            &identity,
                            SnapshotOperation::Release,
                            &bytes,
                            0,
                            None,
                            &mut tokio::io::empty(),
                            &mut tokio::io::sink(),
                        )
                        .await
                        .map_err(SupervisorError::Runtime)
                })
                .await?;
            }
            observe(
                diagnostic,
                Stage::ReceiptObservation,
                self.observe_snapshot(scope, &lease, &control, Some(&canonical), false),
            )
            .await
        };
        let result = self
            .snapshot_heartbeat(scope, &lease, operation, Some(diagnostic))
            .await?;
        if matches!(&result, SnapshotReconciliation::Captured { .. }) {
            observe(diagnostic, Stage::LeaseRelease, async {
                Ok(self.ledger.release(&lease).await?)
            })
            .await?;
        }
        Ok(result)
    }
}
fn hex_root(bytes: &[u8; 32]) -> String {
    crate::sandbox::dependency_bundle::hex(bytes)
}
fn validate_compile_receipt(bytes: &[u8], canonical: &[u8]) -> Result<(), SupervisorError> {
    #[derive(serde::Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Output {
        revision: u8,
        compiled_artifact: Descriptor,
    }
    let receipt: serde_json::Value =
        serde_json::from_slice(bytes).map_err(|_| SupervisorError::Receipt)?;
    if receipt["revision"] != 1 || receipt["status"] != "completed" || receipt["exit_code"] != 0 {
        return Err(SupervisorError::Receipt);
    }
    let output: Output =
        serde_json::from_str(receipt["stdout"].as_str().ok_or(SupervisorError::Receipt)?)
            .map_err(|_| SupervisorError::Receipt)?;
    if output.revision != 1
        || output
            .compiled_artifact
            .bytes()
            .map_err(|_| SupervisorError::Receipt)?
            != canonical
    {
        return Err(SupervisorError::Receipt);
    }
    Ok(())
}

fn native_snapshot_scope(
    scope: &JobScope,
    job: &PreparedJob,
    content: Option<(
        &crate::protocol::sandbox_grant::AuthorizedContent,
        &super::DependencyBundle,
    )>,
) -> Result<JobScope, SupervisorError> {
    let base = JobScope::new(
        scope.tenant.clone(),
        scope.project,
        scope.key,
        job.fingerprint().map_err(|_| SupervisorError::Invalid)?,
    )?;
    match (job.dependency_bundle_root(), content) {
        (Some(root), Some((authorization, bundle)))
            if authorization.scope() == &base
                && authorization.valid_at(chrono::Utc::now().timestamp_millis())
                && hex_root(authorization.root()) == root
                && bundle.root() == root
                && job.matches_bundle(bundle) =>
        {
            Ok(base)
        }
        (None, None) => Ok(base),
        _ => Err(SupervisorError::Invalid),
    }
}
