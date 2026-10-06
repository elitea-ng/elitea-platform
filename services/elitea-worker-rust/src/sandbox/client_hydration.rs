//! Indexed inert delivery uses fresh authority for each immutable file.
use super::{SandboxCallError, SandboxClient, preparation::authorize, submission_code};
use crate::{
    protocol::elitea::runtime::v1::{
        AuthorizeSandboxJobRequestV1, HydrateSandboxDependenciesRequestV1,
    },
    sandbox::{dependency_bundle::DependencyBundle, request::PreparedJob},
    transport::control_grpc::{ControlGrpcClient, ControlRpc},
};
use tonic::{Code, Request};

impl SandboxClient {
    /// A successful reply acknowledges this index. Ready permits exact-job submission.
    /// # Errors
    /// Rejects changed roots, excess indices, or unconfirmed authority and transport.
    pub async fn hydrate_dependencies<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        bundle: &DependencyBundle,
        index: usize,
    ) -> Result<bool, SandboxCallError> {
        self.hydrate_dependencies_with_intent(control, authorization, job, bundle, index, None)
            .await
    }
    pub(crate) async fn hydrate_dependencies_with_intent<R: ControlRpc>(
        &self,
        control: &ControlGrpcClient<R>,
        mut authorization: AuthorizeSandboxJobRequestV1,
        job: &PreparedJob,
        bundle: &DependencyBundle,
        index: usize,
        intent: Option<&[u8]>,
    ) -> Result<bool, SandboxCallError> {
        let intent = intent.unwrap_or_default();
        if intent.len() > 16 * 1024 || (job.platform_client().is_some() && intent.is_empty()) {
            return Err(SandboxCallError::Invalid);
        }
        if !job.matches_bundle(bundle) || index > bundle.file_count() {
            return Err(SandboxCallError::Invalid);
        }
        let digest = job.fingerprint().map_err(|_| SandboxCallError::Invalid)?;
        let content = self
            .authorize_content(control, authorization.clone(), &digest, bundle.root())
            .await?;
        authorization.request_digest = digest.to_vec();
        authorization.audience.clone_from(&self.audience);
        authorization.cancel_only = false;
        authorization.dependency_bundle_sha256.clear();
        let execute = authorize(control, authorization).await?;
        let mut request = Request::new(HydrateSandboxDependenciesRequestV1 {
            execution_grant: Some(execute),
            content_grant: Some(content),
            prepared_job_json: job.to_transport().map_err(|_| SandboxCallError::Invalid)?,
            bundle_json: bundle.record_json().to_vec(),
            index: u32::try_from(index).map_err(|_| SandboxCallError::Invalid)?,
            code_execution_intent_json: intent.to_vec(),
        });
        request.set_timeout(self.deadline);
        let response = tokio::time::timeout(
            self.deadline,
            self.rpc.clone().hydrate_sandbox_dependencies(request),
        )
        .await
        .map_err(|_| SandboxCallError::Submission {
            code: Code::DeadlineExceeded,
        })?
        .map_err(|status| SandboxCallError::Submission {
            code: submission_code(&status),
        })?
        .into_inner();
        if index == bundle.file_count() && !response.ready {
            return Err(SandboxCallError::InvalidReceipt);
        }
        Ok(response.ready)
    }
}
