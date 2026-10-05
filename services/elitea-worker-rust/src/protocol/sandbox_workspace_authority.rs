//! Fresh owning Main grants for one immutable repository cursor step.
use super::{AgentControlClient, ClaimBoundSandboxAuthority, ControlRpc};
use crate::{
    protocol::elitea::runtime::v1::{OriginalCodeVisitRefV1, RustCompiledSnapshotPurposeV1},
    sandbox::{
        client::{
            SandboxCallError, SandboxClient, WorkspaceHydrationMode, WorkspaceHydrationOutcome,
        },
        code_recovery::OriginalCodeVisitRef,
        compiled_snapshot::{Control, Purpose, SelectedSnapshot},
        request::PreparedJob,
        workspace::WorkspaceManifest,
    },
};

impl<R: ControlRpc> AgentControlClient<R> {
    #[allow(clippy::too_many_arguments)] // The exact visit and request remain beside the cursor and current claim.
    pub(crate) async fn hydrate_plain_code_workspace(
        &self,
        sandbox: &SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        intent: &[u8],
        index: u32,
    ) -> Result<WorkspaceHydrationOutcome, SandboxCallError> {
        self.require_sandbox_authority(authority)?;
        let mut request = authority.request(activation);
        request.request_digest = job
            .fingerprint()
            .map_err(|_| SandboxCallError::Invalid)?
            .to_vec();
        request.audience = sandbox.audience().into();
        let response = self.control.authorize_sandbox_job(request).await?;
        if response.rejection.is_some() {
            return Err(SandboxCallError::Rejected);
        }
        let grant = response.grant.ok_or(SandboxCallError::Rejected)?;
        sandbox
            .hydrate_workspace(
                &grant,
                job,
                manifest,
                WorkspaceHydrationMode::Plain { intent },
                index,
            )
            .await
    }
    #[allow(clippy::too_many_arguments)] // Compile uses its signed original visit, never a final Execute intent.
    pub(crate) async fn hydrate_compile_code_workspace(
        &self,
        sandbox: &SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        control: &Control,
        original_visit: &OriginalCodeVisitRef,
        index: u32,
    ) -> Result<WorkspaceHydrationOutcome, SandboxCallError> {
        self.require_sandbox_authority(authority)?;
        if !original_visit.valid() || control.descriptor_sha256.is_some() {
            return Err(SandboxCallError::Invalid);
        }
        control
            .binding
            .validate_job(job)
            .map_err(|_| SandboxCallError::Invalid)?;
        control
            .validate(Purpose::Compile)
            .map_err(|_| SandboxCallError::Invalid)?;
        let mut request = authority.compiled_request(activation);
        request.audience = sandbox.audience().into();
        request.purpose = RustCompiledSnapshotPurposeV1::Compile.into();
        request.prepared_job_json = job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        request.binding_json =
            serde_json::to_vec(&control.binding).map_err(|_| SandboxCallError::Invalid)?;
        request.original_code_visit = Some(OriginalCodeVisitRefV1 {
            visit_id: original_visit.visit_id.clone(),
            revision: original_visit.revision,
            digest_sha256: original_visit.digest_sha256.clone(),
        });
        let response = self
            .control
            .authorize_rust_compiled_snapshot(request)
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        let grant = response.grant.ok_or(SandboxCallError::Rejected)?;
        sandbox
            .hydrate_workspace(
                &grant,
                job,
                manifest,
                WorkspaceHydrationMode::Compile { control },
                index,
            )
            .await
    }
    #[allow(clippy::too_many_arguments)] // Cached Execute preserves its exact descriptor and separate pinned Read role.
    pub(crate) async fn hydrate_cached_code_workspace(
        &self,
        sandbox: &SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &PreparedJob,
        manifest: &WorkspaceManifest,
        selected: &SelectedSnapshot,
        intent: &[u8],
        index: u32,
    ) -> Result<WorkspaceHydrationOutcome, SandboxCallError> {
        self.require_sandbox_authority(authority)?;
        selected
            .control
            .binding
            .validate_job(job)
            .map_err(|_| SandboxCallError::Invalid)?;
        selected
            .descriptor
            .validate(&selected.control)
            .map_err(|_| SandboxCallError::Invalid)?;
        let mut request = authority.compiled_request(activation);
        request.audience = sandbox.audience().into();
        request.prepared_job_json = job.to_transport().map_err(|_| SandboxCallError::Invalid)?;
        request.binding_json =
            serde_json::to_vec(&selected.control.binding).map_err(|_| SandboxCallError::Invalid)?;
        request.selected_descriptor_sha256 = selected
            .control
            .descriptor_sha256
            .as_ref()
            .ok_or(SandboxCallError::Invalid)?
            .raw()
            .map_err(|_| SandboxCallError::Invalid)?
            .to_vec();
        request.purpose = RustCompiledSnapshotPurposeV1::Execute.into();
        let execute = self
            .control
            .authorize_rust_compiled_snapshot(request.clone())
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        selected
            .require_same_descriptor(&execute.descriptor_json)
            .map_err(|_| SandboxCallError::InvalidReceipt)?;
        request.purpose = RustCompiledSnapshotPurposeV1::Read.into();
        let read = self
            .control
            .authorize_rust_compiled_snapshot(request)
            .await?
            .ok_or(SandboxCallError::Rejected)?;
        selected
            .require_same_descriptor(&read.descriptor_json)
            .map_err(|_| SandboxCallError::InvalidReceipt)?;
        let execute = execute.grant.ok_or(SandboxCallError::Rejected)?;
        let read = read.grant.ok_or(SandboxCallError::Rejected)?;
        sandbox
            .hydrate_workspace(
                &execute,
                job,
                manifest,
                WorkspaceHydrationMode::CachedExecute {
                    control: &selected.control,
                    descriptor: &selected.descriptor,
                    canonical: &selected.descriptor_bytes,
                    read_grant: &read,
                    intent,
                },
                index,
            )
            .await
    }
}
