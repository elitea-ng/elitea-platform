//! Bounded inert repository transfer on the original job's existing fenced lifecycle.
use super::{
    DependencyContentClient, DockerSupervisor, JobLease, JobScope, LEASE_SECONDS, Phase,
    PreparedJob, Reconciliation, SignedSandboxJobGrantV1, SupervisorError, native_platform_permits,
};
use crate::{
    protocol::sandbox_grant::{
        AuthorizedCodeIntent, AuthorizedJob, AuthorizedSnapshotCompile, AuthorizedSnapshotExecute,
        AuthorizedSnapshotRead,
    },
    sandbox::{
        compiled_snapshot::{ContentSha256, Control, Descriptor, Purpose},
        dependency_content::{WorkspaceContentError, WorkspaceReadProof},
        runtime_workspace::{WorkspaceBatch, WorkspaceOperation, WorkspaceProbe},
        workspace::{WorkspaceManifest, WorkspaceMode},
    },
};
use adk_sandbox::workspace::docker::CodeJobIdentity;

pub(crate) enum WorkspaceAdmission<'a> {
    Plain {
        job: &'a AuthorizedJob,
        intent: &'a AuthorizedCodeIntent,
    },
    Compile {
        job: &'a AuthorizedSnapshotCompile,
        control: &'a Control,
    },
    CachedExecute {
        job: &'a AuthorizedSnapshotExecute,
        read: &'a AuthorizedSnapshotRead,
        control: &'a Control,
        descriptor: &'a Descriptor,
        canonical: &'a [u8],
        intent: &'a AuthorizedCodeIntent,
    },
}
pub(crate) struct WorkspaceCursor {
    pub manifest_sha256: String,
    pub next_file_index: u32,
    pub file_count: u32,
    pub ready: bool,
}
pub(crate) enum WorkspaceReconciliation {
    Progress(WorkspaceCursor),
    Outcome(Reconciliation),
    AlreadyDispatched,
}
impl WorkspaceAdmission<'_> {
    fn scope(&self) -> &JobScope {
        match self {
            Self::Plain { job, .. } => job.scope(),
            Self::Compile { job, .. } => job.scope(),
            Self::CachedExecute { job, .. } => job.scope(),
        }
    }
    fn compiled(&self) -> Option<(&Control, Purpose, Option<&[u8]>)> {
        match self {
            Self::Compile { control, .. } => Some((control, Purpose::Compile, None)),
            Self::CachedExecute {
                control, canonical, ..
            } => Some((control, Purpose::Execute, Some(canonical))),
            Self::Plain { .. } => None,
        }
    }
    fn activation(&self, now: i64) -> Result<&str, SupervisorError> {
        match self {
            Self::Plain { intent, .. } | Self::CachedExecute { intent, .. } => Ok(&intent
                .binding(self.scope(), now)
                .map_err(|_| SupervisorError::Invalid)?
                .dispatch_activation),
            Self::Compile { job, .. } => {
                if job.workspace_original_visit().is_none() {
                    return Err(SupervisorError::Invalid);
                }
                Ok(job.original_execution().2)
            }
        }
    }
    fn validate(
        &self,
        supervisor: &DockerSupervisor,
        job: &PreparedJob,
    ) -> Result<(), SupervisorError> {
        let now = chrono::Utc::now().timestamp_millis();
        if job.workspace().is_none()
            || !job.within_timeout(supervisor.runtime.code_job_timeout())
            || !native_platform_permits(job, supervisor.native_platform.as_ref())
            || !supervisor
                .admission_policy
                .as_ref()
                .is_some_and(|(policy, languages)| {
                    job.matches_runtime(supervisor.runtime.image_digest(), policy, languages)
                })
        {
            return Err(SupervisorError::Invalid);
        }
        match self {
            Self::Plain {
                job: authority,
                intent,
            } => {
                if !authority.permits(job, now)
                    || !job.matches_code_binding(
                        intent
                            .binding(authority.scope(), now)
                            .map_err(|_| SupervisorError::Invalid)?,
                    )
                {
                    return Err(SupervisorError::Invalid);
                }
            }
            Self::Compile {
                job: authority,
                control,
            } => {
                supervisor.validate_snapshot(authority.scope(), job, control)?;
                if !authority.permits(job, control, now)
                    || authority.workspace_original_visit().is_none()
                {
                    return Err(SupervisorError::Invalid);
                }
            }
            Self::CachedExecute {
                job: authority,
                read,
                control,
                descriptor,
                canonical,
                intent,
            } => {
                supervisor.validate_snapshot_execute(
                    authority, read, job, control, descriptor, canonical,
                )?;
                if !job.matches_code_binding(
                    intent
                        .binding(authority.scope(), now)
                        .map_err(|_| SupervisorError::Invalid)?,
                ) {
                    return Err(SupervisorError::Invalid);
                }
            }
        }
        let activation = self.activation(now)?;
        if activation.len() != 64
            || !activation
                .bytes()
                .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
        {
            return Err(SupervisorError::Invalid);
        }
        Ok(())
    }
}
#[allow(
    clippy::needless_pass_by_value,
    reason = "Preserve existing ownership and caller contracts."
)]
fn transfer(error: WorkspaceContentError) -> SupervisorError {
    match error{WorkspaceContentError::Busy=>SupervisorError::Busy,WorkspaceContentError::Authority|WorkspaceContentError::Integrity=>SupervisorError::Invalid,WorkspaceContentError::Unavailable=>SupervisorError::Runtime(adk_sandbox::SandboxError::ExecutionFailed("Code repository transfer was interrupted; original runtime reconciliation required".into()))}
}

impl DockerSupervisor {
    /// Each operation transfers at most32 files/4MiB. It never dispatches Code.
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn hydrate_workspace_authorized(
        &self,
        admission: WorkspaceAdmission<'_>,
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        grant: &SignedSandboxJobGrantV1,
        intent_json: Option<&[u8]>,
        index: u32,
        content: &DependencyContentClient,
    ) -> Result<WorkspaceReconciliation, SupervisorError> {
        admission.validate(self, job)?;
        if manifest.selection().mode != WorkspaceMode::Read
            || !job
                .workspace()
                .ok_or(SupervisorError::Invalid)?
                .matches(manifest)
                .map_err(|_| SupervisorError::Invalid)?
            || index as usize > manifest.files().len()
            || admission
                .compiled()
                .is_some_and(|(_, purpose, _)| purpose == Purpose::Compile)
                != intent_json.is_none()
        {
            return Err(SupervisorError::Invalid);
        }
        let _slot = self
            .capacity
            .try_acquire()
            .map_err(|_| SupervisorError::Busy)?;
        let scope = admission.scope();
        let record = self.ledger.reserve(scope).await?;
        if record.phase == Phase::Dispatched {
            return Ok(WorkspaceReconciliation::AlreadyDispatched);
        }
        if record.phase != Phase::Reserved {
            return self
                .terminal(scope, record)
                .await
                .map(WorkspaceReconciliation::Outcome);
        }
        let lease = self.claim_indexed_transfer(scope).await?;
        if lease.observed_phase != Phase::Reserved {
            self.release_failed_preparation(&lease).await?;
            return Ok(WorkspaceReconciliation::AlreadyDispatched);
        }
        let operation = self.hydrate_workspace_owned(
            scope,
            &lease,
            &admission,
            job,
            manifest,
            grant,
            intent_json,
            index,
            content,
        );
        tokio::pin!(operation);
        let mut heartbeat = tokio::time::interval(std::time::Duration::from_secs(20));
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        heartbeat.tick().await;
        let outcome = loop {
            tokio::select! {result=&mut operation=>break result,_=heartbeat.tick()=>{
             if let Some(stopped)=self.stop_if_requested(scope,&lease).await?{break Ok(WorkspaceReconciliation::Outcome(stopped))}
             if let Some(expired)=self.expire_hydration_if_needed(scope,&lease).await?{break Ok(WorkspaceReconciliation::Outcome(expired))}
             admission.validate(self,job)?;self.ledger.renew(&lease,LEASE_SECONDS).await?;
            }}
        };
        // Unknown provisioning keeps its lease until expiry; a measured original can hand off.
        if outcome.is_ok() || self.ledger.read(scope).await?.runtime_id.is_some() {
            self.release_failed_preparation(&lease).await?;
        }
        outcome
    }
    async fn workspace_fence(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        admission: &WorkspaceAdmission<'_>,
        job: &PreparedJob,
    ) -> Result<Option<WorkspaceReconciliation>, SupervisorError> {
        admission.validate(self, job)?;
        if let Some(stopped) = self.stop_if_requested(scope, lease).await? {
            return Ok(Some(WorkspaceReconciliation::Outcome(stopped)));
        }
        if let Some(expired) = self.expire_hydration_if_needed(scope, lease).await? {
            return Ok(Some(WorkspaceReconciliation::Outcome(expired)));
        }
        self.ledger.renew(lease, LEASE_SECONDS).await?;
        Ok(None)
    }
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    async fn hydrate_workspace_owned(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        admission: &WorkspaceAdmission<'_>,
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        grant: &SignedSandboxJobGrantV1,
        intent_json: Option<&[u8]>,
        requested: u32,
        content: &DependencyContentClient,
    ) -> Result<WorkspaceReconciliation, SupervisorError> {
        if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
            return Ok(outcome);
        }
        let proof = WorkspaceReadProof {
            grant,
            prepared: job,
            intent: intent_json,
        };
        // Actual signed Main current-fence/acquisition proof precedes volume creation.
        content
            .download_workspace_manifest(manifest, &proof)
            .await
            .map_err(transfer)?;
        if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
            return Ok(outcome);
        }
        let launch = match admission.compiled() {
            Some((_, purpose, _)) => job
                .compiled_manifest(purpose)
                .map_err(|_| SupervisorError::Invalid),
            None => job.manifest().map_err(|_| SupervisorError::Invalid),
        }?;
        if let Some((control, purpose, canonical)) = admission.compiled() {
            self.ledger
                .record_compiled_intent(lease, control, purpose, canonical)
                .await?;
        }
        let identity = self
            .provision_workspace(scope, lease, admission, &launch, manifest.root())
            .await?;
        let count = u32::try_from(manifest.files().len()).map_err(|_| SupervisorError::Invalid)?;
        if self
            .runtime
            .workspace_hydrated(&identity, manifest.root())
            .await
            .map_err(SupervisorError::Runtime)?
        {
            if requested == count
                && !self
                    .runtime
                    .workspace_prepared(&identity, manifest.root())
                    .await
                    .map_err(SupervisorError::Runtime)?
            {
                self.runtime
                    .complete_workspace(&identity, manifest.root(), &launch)
                    .await
                    .map_err(SupervisorError::Runtime)?;
            }
            let ready = self
                .runtime
                .workspace_prepared(&identity, manifest.root())
                .await
                .map_err(SupervisorError::Runtime)?;
            return Ok(WorkspaceReconciliation::Progress(WorkspaceCursor {
                manifest_sha256: manifest.root().into(),
                next_file_index: count,
                file_count: count,
                ready,
            }));
        }
        self.runtime
            .workspace_command(
                &identity,
                manifest.root(),
                WorkspaceOperation::Manifest,
                None,
                &manifest
                    .to_transport()
                    .map_err(|_| SupervisorError::Invalid)?,
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        let mut confirmed = self.workspace_probe(&identity, manifest).await?;
        let batch = WorkspaceBatch::plan(manifest, requested, confirmed.next_index)
            .map_err(SupervisorError::Runtime)?;
        for index in batch.start..batch.end {
            if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
                return Ok(outcome);
            }
            let bytes = content
                .download_workspace_file(manifest, index, &proof)
                .await
                .map_err(transfer)?;
            if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
                return Ok(outcome);
            }
            self.runtime
                .workspace_command(
                    &identity,
                    manifest.root(),
                    WorkspaceOperation::Write,
                    Some(u32::try_from(index).map_err(|_| SupervisorError::Invalid)?),
                    &bytes,
                )
                .await
                .map_err(SupervisorError::Runtime)?;
            confirmed = self.workspace_probe(&identity, manifest).await?;
            if confirmed.next_index as usize != index + 1 || confirmed.ready {
                return Err(SupervisorError::Receipt);
            }
        }
        if batch.finalize {
            if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
                return Ok(outcome);
            }
            self.runtime
                .workspace_command(
                    &identity,
                    manifest.root(),
                    WorkspaceOperation::Finalize,
                    None,
                    &[],
                )
                .await
                .map_err(SupervisorError::Runtime)?;
            confirmed = self.workspace_probe(&identity, manifest).await?;
            if !confirmed.ready || confirmed.next_index != count {
                return Err(SupervisorError::Receipt);
            }
            if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
                return Ok(outcome);
            }
            self.runtime
                .workspace_command(
                    &identity,
                    manifest.root(),
                    WorkspaceOperation::Release,
                    None,
                    &[],
                )
                .await
                .map_err(SupervisorError::Runtime)?;
            let stopped = async {
                while !self
                    .runtime
                    .workspace_hydrated(&identity, manifest.root())
                    .await
                    .map_err(SupervisorError::Runtime)?
                {
                    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                }
                Ok::<(), SupervisorError>(())
            };
            tokio::time::timeout(std::time::Duration::from_mins(1), stopped)
                .await
                .map_err(|_| SupervisorError::Receipt)??;
            if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
                return Ok(outcome);
            }
            self.runtime
                .complete_workspace(&identity, manifest.root(), &launch)
                .await
                .map_err(SupervisorError::Runtime)?;
        }
        if let Some(outcome) = self.workspace_fence(scope, lease, admission, job).await? {
            return Ok(outcome);
        }
        let ready = batch.finalize
            && self
                .runtime
                .workspace_prepared(&identity, manifest.root())
                .await
                .map_err(SupervisorError::Runtime)?;
        Ok(WorkspaceReconciliation::Progress(WorkspaceCursor {
            manifest_sha256: manifest.root().into(),
            next_file_index: confirmed.next_index,
            file_count: count,
            ready,
        }))
    }
    async fn workspace_probe(
        &self,
        identity: &CodeJobIdentity,
        manifest: &WorkspaceManifest,
    ) -> Result<WorkspaceProbe, SupervisorError> {
        let raw = self
            .runtime
            .workspace_command(
                identity,
                manifest.root(),
                WorkspaceOperation::Probe,
                None,
                &[],
            )
            .await
            .map_err(SupervisorError::Runtime)?;
        if raw.len() > 1024 {
            return Err(SupervisorError::Receipt);
        }
        let probe: WorkspaceProbe =
            serde_json::from_slice(&raw).map_err(|_| SupervisorError::Receipt)?;
        if probe.revision != 1
            || probe.next_index as usize > manifest.files().len()
            || probe.ready && probe.next_index as usize != manifest.files().len()
        {
            return Err(SupervisorError::Receipt);
        }
        Ok(probe)
    }
    async fn provision_workspace(
        &self,
        scope: &JobScope,
        lease: &JobLease,
        admission: &WorkspaceAdmission<'_>,
        launch: &adk_sandbox::workspace::Manifest,
        root: &str,
    ) -> Result<CodeJobIdentity, SupervisorError> {
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
            match admission.compiled() {
                Some((control, purpose, _)) => {
                    let bytes = control
                        .bytes(purpose)
                        .map_err(|_| SupervisorError::Invalid)?;
                    self.runtime
                        .prepare_compiled_workspace(
                            &identity,
                            launch,
                            if purpose == Purpose::Compile {
                                "compile"
                            } else {
                                "execute"
                            },
                            ContentSha256::of(&bytes).as_str(),
                            self.snapshot_launch_policy(control)?,
                            root,
                        )
                        .await
                        .map_err(SupervisorError::Runtime)?;
                }
                None => self
                    .runtime
                    .prepare_workspace(&identity, launch, root)
                    .await
                    .map_err(SupervisorError::Runtime)?,
            }
        }
        let runtime = self
            .runtime
            .instance(&identity)
            .await
            .map_err(SupervisorError::Runtime)?
            .ok_or(SupervisorError::Receipt)?;
        self.ledger.bind_runtime(lease, &runtime).await?;
        let identity = identity
            .with_runtime_id(runtime)
            .map_err(SupervisorError::Runtime)?;
        if let Some((control, purpose, _)) = admission.compiled() {
            self.verify_snapshot_launch(&identity, control, purpose)
                .await?;
        }
        let volume = self
            .runtime
            .workspace_volume_identity(&identity, root)
            .await
            .map_err(SupervisorError::Runtime)?;
        let receipt = volume.receipt(
            scope,
            &identity,
            admission.activation(chrono::Utc::now().timestamp_millis())?,
            root,
        )?;
        self.ledger.bind_workspace_runtime(lease, &receipt).await?;
        Ok(identity)
    }
    pub(super) async fn require_workspace_ready(
        &self,
        scope: &JobScope,
        job: &PreparedJob,
    ) -> Result<(), SupervisorError> {
        let binding = job.workspace().ok_or(SupervisorError::Invalid)?;
        let receipt = self
            .ledger
            .read_workspace_runtime(scope)
            .await?
            .ok_or(SupervisorError::Receipt)?;
        if receipt.manifest_sha256 != binding.manifest_sha256
            || !self
                .runtime
                .workspace_prepared(&self.bound_identity(scope).await?, &binding.manifest_sha256)
                .await
                .map_err(SupervisorError::Runtime)?
        {
            return Err(SupervisorError::Receipt);
        }
        Ok(())
    }
}
