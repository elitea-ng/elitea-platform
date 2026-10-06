//! Runtime operations below the durable supervisor. Connection loss never
//! authorizes replay. The supervisor owns leases and terminal receipts.
use super::dependency_bundle::DependencyBundle;
use adk_sandbox::{
    SandboxError,
    workspace::{DockerClient, Manifest, docker::CodeJobIdentity},
};
use std::time::Duration;
#[derive(Clone, Copy)]
pub enum NativeContentOperation {
    PreparationRead,
    ExecutionRead,
    Import,
    Finalize,
    Release,
}
impl NativeContentOperation {
    pub(crate) fn command(self) -> &'static str {
        match self {
            Self::PreparationRead => "--native-dependency-read",
            Self::ExecutionRead => "--native-execution-dependency-read",
            Self::Import => "--native-dependency-write",
            Self::Finalize => "--native-dependency-finalize",
            Self::Release => "--native-preparation-release",
        }
    }
}
fn native_kind(bundle: &DependencyBundle) -> Option<&'static str> {
    bundle.native().map(|v| match v.record.kind {
        super::native_bundle::NativeKind::Deno => "deno",
        super::native_bundle::NativeKind::Cargo => "cargo",
    })
}

#[async_trait::async_trait]
pub trait CodeJobRuntime: Send + Sync {
    fn image_digest(&self) -> &str;
    fn code_compilation_enabled(&self) -> bool;
    fn code_job_timeout(&self) -> Duration;
    fn retained_code_platform_kind(
        &self,
    ) -> Option<super::code_platform_owner::RetainedRuntimeKind> {
        None
    }
    async fn bind_code_platform_launch(
        &self,
        _identity: &CodeJobIdentity,
        _launch: &[u8],
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "platform owner unavailable".into(),
        ))
    }
    async fn validate_code_platform_launch(
        &self,
        _identity: &CodeJobIdentity,
        _launch: &[u8],
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "platform owner unavailable".into(),
        ))
    }
    async fn read_code_platform_call(
        &self,
        _identity: &CodeJobIdentity,
        _launch: &[u8],
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "platform owner unavailable".into(),
        ))
    }
    async fn publish_code_platform_reply(
        &self,
        _identity: &CodeJobIdentity,
        _launch: &[u8],
        _reply: &[u8],
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "platform owner unavailable".into(),
        ))
    }
    /// Return the exact runtime instance for persistence before dispatch.
    async fn instance(&self, identity: &CodeJobIdentity) -> Result<Option<String>, SandboxError>;
    /// Observe the exact fingerprint; reject a conflicting workload.
    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError>;
    /// Provision once without executing code. Never overwrite an existing job.
    async fn prepare(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<(), SandboxError>;
    /// Provision an inert job with a job-owned read-only repository mount.
    /// Missing implementations reject before user execution.
    async fn prepare_workspace(
        &self,
        _identity: &CodeJobIdentity,
        _manifest: &Manifest,
        _root: &str,
    ) -> Result<(), SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    #[allow(clippy::too_many_arguments)] // Bind the exact compiled role and immutable repository in one operation.
    async fn prepare_compiled_workspace(
        &self,
        _identity: &CodeJobIdentity,
        _manifest: &Manifest,
        _purpose: &str,
        _control_sha256: &str,
        _policy_revision: &str,
        _root: &str,
    ) -> Result<(), SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    async fn workspace_command(
        &self,
        _identity: &CodeJobIdentity,
        _root: &str,
        _operation: super::runtime_workspace::WorkspaceOperation,
        _index: Option<u32>,
        _payload: &[u8],
    ) -> Result<Vec<u8>, SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    async fn workspace_prepared(
        &self,
        _identity: &CodeJobIdentity,
        _root: &str,
    ) -> Result<bool, SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    /// Complete launch files only after the original repository helper has exited.
    async fn complete_workspace(
        &self,
        _identity: &CodeJobIdentity,
        _root: &str,
        _manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    async fn workspace_hydrated(
        &self,
        _identity: &CodeJobIdentity,
        _root: &str,
    ) -> Result<bool, SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    async fn workspace_volume_identity(
        &self,
        _identity: &CodeJobIdentity,
        _root: &str,
    ) -> Result<super::runtime_workspace::WorkspaceVolumeIdentity, SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    async fn cleanup_workspace(
        &self,
        _identity: &CodeJobIdentity,
        _receipt: &super::ledger::WorkspaceRuntimeReceipt,
    ) -> Result<(), SandboxError> {
        Err(super::runtime_workspace::unavailable())
    }
    /// Compiled roles are inert unless the backend implements the fixed PID 1 contract.
    async fn prepare_compiled(
        &self,
        _identity: &CodeJobIdentity,
        _manifest: &Manifest,
        _purpose: &str,
        _control_sha256: &str,
        _policy_revision: &str,
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "compiled runtime unavailable".into(),
        ))
    }
    /// Verify immutable PID 1 launch values before reading or signalling an original runtime.
    async fn validate_compiled_launch(
        &self,
        _identity: &CodeJobIdentity,
        _purpose: &str,
        _control_sha256: &str,
        _policy_revision: &str,
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "compiled runtime unavailable".into(),
        ))
    }
    async fn compiled_descriptor_probe(
        &self,
        _identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "compiled runtime unavailable".into(),
        ))
    }
    #[allow(clippy::too_many_arguments)]
    async fn compiled_transfer(
        &self,
        _identity: &CodeJobIdentity,
        _operation: super::compiled_snapshot::SnapshotOperation,
        _control_json: &[u8],
        _bytes: u64,
        _sha256: Option<&str>,
        _reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        _writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "compiled runtime unavailable".into(),
        ))
    }
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError>;
    /// Signal after durable dispatch. Repeated signals must not replay code.
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError>;
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError>;
    /// Observe bounded preparation metadata without completing the retained workload.
    async fn preparation_marker(
        &self,
        _identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "dependency preparation is unavailable on this runtime".into(),
        ))
    }
    /// Export exact recorded preparation bytes. The consumer verifies SHA-256.
    async fn export_dependency(
        &self,
        _identity: &CodeJobIdentity,
        _name: &str,
        _bytes: u64,
        _writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "dependency export is unavailable on this runtime".into(),
        ))
    }
    /// Export imported execution bytes for hydration and pre-dispatch proofs.
    /// Preparation files never substitute for execution files.
    async fn export_execution_dependency(
        &self,
        _identity: &CodeJobIdentity,
        _name: &str,
        _bytes: u64,
        _writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "execution dependency export is unavailable on this runtime".into(),
        ))
    }
    /// Import verified bytes before dispatch. Existing content is never replaced.
    async fn import_dependency(
        &self,
        _identity: &CodeJobIdentity,
        _name: &str,
        _bytes: u64,
        _reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "dependency import is unavailable on this runtime".into(),
        ))
    }
    /// Release the retained preparer after shared publication succeeds.
    async fn release_preparation(&self, _identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "dependency preparation release is unavailable on this runtime".into(),
        ))
    }
    async fn bundle_preparation_marker(
        &self,
        identity: &CodeJobIdentity,
        native: bool,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if native {
            Err(SandboxError::ExecutionFailed(
                "native preparation unavailable".into(),
            ))
        } else {
            self.preparation_marker(identity).await
        }
    }
    #[allow(clippy::too_many_arguments)] // Keep immutable content identity beside both binary streams.
    async fn native_transfer(
        &self,
        _identity: &CodeJobIdentity,
        _kind: &str,
        _root: &str,
        _name: &str,
        _bytes: u64,
        _operation: NativeContentOperation,
        _reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        _writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        Err(SandboxError::ExecutionFailed(
            "native delivery unavailable".into(),
        ))
    }
    async fn export_preparation_bundle_dependency(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        if let Some(kind) = native_kind(bundle) {
            self.native_transfer(
                identity,
                kind,
                bundle.root(),
                name,
                bytes,
                NativeContentOperation::PreparationRead,
                &mut tokio::io::empty(),
                writer,
            )
            .await
        } else {
            self.export_dependency(identity, name, bytes, writer).await
        }
    }
    async fn export_bundle_dependency(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        if let Some(kind) = native_kind(bundle) {
            self.native_transfer(
                identity,
                kind,
                bundle.root(),
                name,
                bytes,
                NativeContentOperation::ExecutionRead,
                &mut tokio::io::empty(),
                writer,
            )
            .await
        } else {
            self.export_execution_dependency(identity, name, bytes, writer)
                .await
        }
    }
    async fn import_bundle_dependency(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
        name: &str,
        bytes: u64,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        if let Some(kind) = native_kind(bundle) {
            self.native_transfer(
                identity,
                kind,
                bundle.root(),
                name,
                bytes,
                NativeContentOperation::Import,
                reader,
                &mut tokio::io::sink(),
            )
            .await
        } else {
            self.import_dependency(identity, name, bytes, reader).await
        }
    }
    async fn finalize_bundle(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
    ) -> Result<(), SandboxError> {
        if let Some(kind) = native_kind(bundle) {
            self.native_transfer(
                identity,
                kind,
                bundle.root(),
                bundle.metadata_name(),
                0,
                NativeContentOperation::Finalize,
                &mut tokio::io::empty(),
                &mut tokio::io::sink(),
            )
            .await
        } else {
            Ok(())
        }
    }
    async fn release_bundle_preparation(
        &self,
        identity: &CodeJobIdentity,
        bundle: &DependencyBundle,
    ) -> Result<(), SandboxError> {
        if let Some(kind) = native_kind(bundle) {
            self.native_transfer(
                identity,
                kind,
                bundle.root(),
                bundle.metadata_name(),
                0,
                NativeContentOperation::Release,
                &mut tokio::io::empty(),
                &mut tokio::io::sink(),
            )
            .await
        } else {
            self.release_preparation(identity).await
        }
    }
    /// Success confirms termination. Deletion acknowledgement is insufficient
    /// when an unreachable remote node may still execute the workload.
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError>;
    /// Cleanup follows receipt persistence. Never force-delete a live workload.
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError>;
}

#[async_trait::async_trait]
impl CodeJobRuntime for DockerClient {
    fn image_digest(&self) -> &str {
        &self.base_image
    }
    fn code_compilation_enabled(&self) -> bool {
        self.code_compilation_enabled()
    }
    fn retained_code_platform_kind(
        &self,
    ) -> Option<super::code_platform_owner::RetainedRuntimeKind> {
        self.code_platform_profile_enabled()
            .then_some(super::code_platform_owner::RetainedRuntimeKind::Docker)
    }
    async fn bind_code_platform_launch(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<(), SandboxError> {
        DockerClient::bind_code_platform_launch(self, identity, launch).await
    }
    async fn validate_code_platform_launch(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<(), SandboxError> {
        DockerClient::read_code_platform_call(self, identity, launch)
            .await
            .map(|_| ())
    }
    async fn read_code_platform_call(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        DockerClient::read_code_platform_call(self, identity, launch).await
    }
    async fn publish_code_platform_reply(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
        reply: &[u8],
    ) -> Result<(), SandboxError> {
        DockerClient::publish_code_platform_reply(self, identity, launch, reply).await
    }

    fn code_job_timeout(&self) -> Duration {
        self.code_job_timeout()
    }
    async fn instance(&self, identity: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        Ok(self
            .observe_code_job(identity)
            .await?
            .map(|job| job.container_id))
    }
    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        Ok(self.observe_code_job(identity).await?.is_some())
    }
    async fn prepare(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        self.provision_code_job(identity, manifest)
            .await
            .map(|_| ())
    }
    async fn prepare_workspace(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        root: &str,
    ) -> Result<(), SandboxError> {
        super::runtime_workspace::validate_launch_manifest(manifest)?;
        let repository = self.prepare_code_repository(identity, root).await?;
        self.provision_code_job_with_repository(identity, manifest, &repository)
            .await
            .map(|_| ())
    }
    async fn prepare_compiled_workspace(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
        root: &str,
    ) -> Result<(), SandboxError> {
        super::runtime_workspace::validate_launch_manifest(manifest)?;
        let repository = self.prepare_code_repository(identity, root).await?;
        self.provision_compiled_code_job_with_repository(
            identity,
            manifest,
            purpose,
            control_sha256,
            policy_revision,
            Some(&repository),
        )
        .await
        .map(|_| ())
    }
    async fn workspace_command(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
        operation: super::runtime_workspace::WorkspaceOperation,
        index: Option<u32>,
        payload: &[u8],
    ) -> Result<Vec<u8>, SandboxError> {
        let header = super::runtime_workspace::header(
            identity,
            root,
            operation,
            index,
            payload.len(),
            None,
        )?;
        self.code_repository_command(identity, root, operation.command(), &header, payload)
            .await
    }
    async fn workspace_prepared(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<bool, SandboxError> {
        if !self.code_repository_helper_stopped(identity, root).await? {
            return Ok(false);
        }
        CodeJobRuntime::prepared(self, identity).await
    }
    async fn workspace_hydrated(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<bool, SandboxError> {
        self.code_repository_helper_stopped(identity, root).await
    }
    async fn complete_workspace(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
        manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        super::runtime_workspace::validate_launch_manifest(manifest)?;
        if !self.code_repository_helper_stopped(identity, root).await?
            || !CodeJobRuntime::prepared(self, identity).await?
        {
            return Err(super::runtime_workspace::unavailable());
        }
        Ok(())
    }
    async fn workspace_volume_identity(
        &self,
        identity: &CodeJobIdentity,
        root: &str,
    ) -> Result<super::runtime_workspace::WorkspaceVolumeIdentity, SandboxError> {
        let mount = self.validate_code_repository(identity, root).await?;
        Ok(super::runtime_workspace::WorkspaceVolumeIdentity {
            backend: super::ledger::WorkspaceBackend::Docker,
            volume_name: mount.volume_name().into(),
            owner_token: mount.owner_token().into(),
            creation_token: mount.creation_token().into(),
        })
    }
    async fn cleanup_workspace(
        &self,
        identity: &CodeJobIdentity,
        receipt: &super::ledger::WorkspaceRuntimeReceipt,
    ) -> Result<(), SandboxError> {
        if receipt.backend != super::ledger::WorkspaceBackend::Docker
            || identity.runtime_id() != Some(receipt.original_runtime_id.as_str())
            || identity.job_key() != receipt.job_key
            || identity.request_digest() != receipt.request_digest
        {
            return Err(super::runtime_workspace::unavailable());
        }
        self.cleanup_code_repository_receipt(
            identity,
            &receipt.manifest_sha256,
            &receipt.volume_name,
            &receipt.volume_owner_token,
            &receipt.volume_creation_token,
        )
        .await
    }
    async fn prepare_compiled(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<(), SandboxError> {
        self.provision_compiled_code_job(
            identity,
            manifest,
            purpose,
            control_sha256,
            policy_revision,
        )
        .await
        .map(|_| ())
    }
    async fn validate_compiled_launch(
        &self,
        identity: &CodeJobIdentity,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<(), SandboxError> {
        self.validate_compiled_code_job_launch(identity, purpose, control_sha256, policy_revision)
            .await
    }
    async fn compiled_descriptor_probe(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        self.compiled_descriptor_probe(identity).await
    }
    async fn compiled_transfer(
        &self,
        identity: &CodeJobIdentity,
        operation: super::compiled_snapshot::SnapshotOperation,
        control_json: &[u8],
        bytes: u64,
        sha256: Option<&str>,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        use super::compiled_snapshot::{ContentSha256, Control, Purpose, SnapshotOperation};
        let control: Control = serde_json::from_slice(control_json)
            .map_err(|_| SandboxError::ExecutionFailed("invalid snapshot control".into()))?;
        control
            .validate(if control.descriptor_sha256.is_some() {
                Purpose::Execute
            } else {
                Purpose::Compile
            })
            .map_err(|_| SandboxError::ExecutionFailed("invalid snapshot control".into()))?;
        self.validate_compiled_launch(
            identity,
            if control.descriptor_sha256.is_some() {
                "execute"
            } else {
                "compile"
            },
            ContentSha256::of(control_json).as_str(),
            &control.binding.policy_revision,
        )
        .await?;
        let hash = sha256
            .map(|value| ContentSha256::parse(value.into()))
            .transpose()
            .map_err(|_| SandboxError::ExecutionFailed("invalid snapshot hash".into()))?;
        let header = operation
            .header(
                &control,
                if matches!(operation, SnapshotOperation::Status) {
                    0
                } else {
                    bytes
                },
                hash.as_ref(),
                None,
            )
            .map_err(|_| SandboxError::ExecutionFailed("invalid snapshot transfer".into()))?;
        self.compiled_code_transfer(
            identity,
            operation.command(),
            &header,
            bytes,
            reader,
            writer,
            operation.importing(),
        )
        .await
    }
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        self.code_job_prepared(identity).await
    }
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.dispatch_code_job(identity).await
    }
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        self.read_code_job_receipt(identity).await
    }
    async fn preparation_marker(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        self.read_code_preparation_marker(identity).await
    }
    async fn export_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        self.export_code_dependency(identity, name, bytes, writer)
            .await
    }
    async fn export_execution_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        self.export_code_execution_dependency(identity, name, bytes, writer)
            .await
    }
    async fn import_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        self.import_code_dependency(identity, name, bytes, reader)
            .await
    }
    async fn release_preparation(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.release_code_preparation(identity).await
    }
    async fn bundle_preparation_marker(
        &self,
        identity: &CodeJobIdentity,
        native: bool,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if native {
            self.read_native_preparation_marker(identity).await
        } else {
            self.read_code_preparation_marker(identity).await
        }
    }
    async fn native_transfer(
        &self,
        identity: &CodeJobIdentity,
        kind: &str,
        root: &str,
        name: &str,
        bytes: u64,
        operation: NativeContentOperation,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        self.native_code_dependency(
            identity,
            kind,
            root,
            name,
            bytes,
            operation.command(),
            matches!(operation, NativeContentOperation::Import),
            reader,
            writer,
        )
        .await
    }
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        if let Some(root) = self.code_repository_root(identity).await? {
            self.terminate_code_repository_helper(identity, &root)
                .await?;
        }
        self.terminate_code_job(identity).await
    }
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        // Repository deletion needs the original job's durable measured provenance.
        if self.code_repository_root(identity).await?.is_some()
            || self.code_repository_volume_exists(identity).await?
        {
            return Err(super::runtime_workspace::unavailable());
        }
        self.remove_code_job(identity).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn runtime_binding_is_immutable_and_rejects_invalid_identifiers() {
        let identity = CodeJobIdentity::new("a".repeat(64), "b".repeat(64)).unwrap();
        assert!(identity.clone().with_runtime_id(String::new()).is_err());
        assert!(
            identity
                .clone()
                .with_runtime_id("space separated".into())
                .is_err()
        );
        let bound = identity
            .with_runtime_id("original-container".into())
            .unwrap();
        assert_eq!(bound.runtime_id(), Some("original-container"));
        assert!(
            bound
                .clone()
                .with_runtime_id("original-container".into())
                .is_ok()
        );
        assert!(
            bound
                .with_runtime_id("replacement-container".into())
                .is_err()
        );
    }
}
