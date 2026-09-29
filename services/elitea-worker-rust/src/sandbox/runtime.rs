//! Runtime operations below the durable supervisor. Connection loss never
//! authorizes replay. The supervisor owns leases and terminal receipts.
use adk_sandbox::{
    SandboxError,
    workspace::{DockerClient, Manifest, docker::CodeJobIdentity},
};
use std::time::Duration;

#[async_trait::async_trait]
pub trait CodeJobRuntime: Send + Sync {
    fn image_digest(&self) -> &str;
    fn code_compilation_enabled(&self) -> bool;
    fn code_job_timeout(&self) -> Duration;
    /// Observe the exact fingerprint; reject a conflicting workload.
    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError>;
    /// Provision once without executing code. Never overwrite an existing job.
    async fn prepare(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<(), SandboxError>;
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError>;
    /// Signal after durable dispatch. Repeated signals must not replay code.
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError>;
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError>;
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
    fn code_job_timeout(&self) -> Duration {
        self.code_job_timeout()
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
    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        self.code_job_prepared(identity).await
    }
    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.dispatch_code_job(identity).await
    }
    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        self.read_code_job_receipt(identity).await
    }
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.terminate_code_job(identity).await
    }
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.remove_code_job(identity).await
    }
}
