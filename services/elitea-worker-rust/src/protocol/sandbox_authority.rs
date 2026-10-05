//! Exact post-authorization claim binding for sandbox grants.
use super::{
    AcceptedAgentClaim, AgentControlClient, ClaimBoundRuntimeContextAuthority, ConstantTimeEq,
    ControlRpc, ExecutionFenceV1, ExecutionIdentityV1, ProtocolError, VerifiedAgentCommand,
    hex_lower, identity_from_command, verified_command_binding,
};
use crate::protocol::elitea::runtime::v1::{
    AuthorizeSandboxJobRequestV1, SignedWorkerCommandEnvelopeV1,
};
use zeroize::Zeroize;

/// No Clone/Debug: identity and fence cannot be reconstructed by graph code.
pub(super) struct SandboxClaimBinding {
    claim_id: String,
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    command_binding: [u8; 32],
}

impl SandboxClaimBinding {
    pub(super) fn from_claim(claim: &AcceptedAgentClaim) -> Self {
        Self {
            claim_id: claim.claim_id.clone(),
            identity: claim.identity.clone(),
            fence: claim.fence.clone(),
            command_binding: claim.command_binding,
        }
    }
}

impl Drop for SandboxClaimBinding {
    fn drop(&mut self) {
        self.fence.fence_token.zeroize();
    }
}

/// Share through Arc only within one invocation. Main still verifies the live
/// claim and desired state for each request; this value is not a signed grant.
pub(crate) struct ClaimBoundSandboxAuthority {
    claim: Box<SandboxClaimBinding>,
    signed_command: SignedWorkerCommandEnvelopeV1,
}

/// Stop-only authority never enables graph assembly or sandbox submission.
pub(crate) struct SandboxStopAuthority {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    signed_command: SignedWorkerCommandEnvelopeV1,
}
impl Drop for SandboxStopAuthority {
    fn drop(&mut self) {
        self.fence.fence_token.zeroize();
    }
}
impl SandboxStopAuthority {
    pub(crate) fn scope(
        &self,
    ) -> Result<crate::sandbox::dispatch::DispatchScope, crate::sandbox::dispatch::DispatchError>
    {
        crate::sandbox::dispatch::DispatchScope::from_identity(&self.identity)
    }
}
impl super::AgentOutputRecovery {
    pub(crate) fn sandbox_stop_authority(
        &self,
        verified: &VerifiedAgentCommand,
    ) -> Result<SandboxStopAuthority, ProtocolError> {
        if self.binding.identity != identity_from_command(verified)
            || self.binding.desired_state != super::DesiredExecutionState::Cancelled
        {
            return Err(ProtocolError::AuthorizationFailed(
                "sandbox stop recovery does not match cancelled execution",
            ));
        }
        Ok(SandboxStopAuthority {
            identity: self.binding.identity.clone(),
            fence: self.binding.fence.clone(),
            signed_command: verified.signed().clone(),
        })
    }
}

impl ClaimBoundRuntimeContextAuthority {
    pub(crate) fn sandbox_stop_authority(
        &self,
        verified: &VerifiedAgentCommand,
    ) -> Result<SandboxStopAuthority, ProtocolError> {
        let claim = self
            .sandbox
            .as_ref()
            .ok_or(ProtocolError::AuthorizationFailed(
                "sandbox stop authority is unavailable",
            ))?;
        if claim.identity != identity_from_command(verified)
            || !bool::from(
                claim
                    .command_binding
                    .ct_eq(&verified_command_binding(verified)),
            )
        {
            return Err(ProtocolError::AuthorizationFailed(
                "sandbox stop authority does not match command",
            ));
        }
        Ok(SandboxStopAuthority {
            identity: claim.identity.clone(),
            fence: claim.fence.clone(),
            signed_command: verified.signed().clone(),
        })
    }

    /// Extract sandbox request authority without consuming context redemption.
    /// Available only once and only for the exact authenticated command bytes.
    pub(crate) fn take_sandbox_authority(
        &mut self,
        verified: &VerifiedAgentCommand,
    ) -> Result<ClaimBoundSandboxAuthority, ProtocolError> {
        let claim = self
            .sandbox
            .as_ref()
            .ok_or(ProtocolError::AuthorizationFailed(
                "sandbox authority is unavailable for this invocation",
            ))?;
        if claim.identity != identity_from_command(verified)
            || !bool::from(
                claim
                    .command_binding
                    .ct_eq(&verified_command_binding(verified)),
            )
        {
            return Err(ProtocolError::AuthorizationFailed(
                "sandbox authority does not match its command",
            ));
        }
        let claim = self
            .sandbox
            .take()
            .ok_or(ProtocolError::AuthorizationFailed(
                "sandbox authority was already consumed",
            ))?;
        Ok(ClaimBoundSandboxAuthority {
            claim,
            signed_command: verified.signed().clone(),
        })
    }
}

impl ClaimBoundSandboxAuthority {
    /// Borrow the accepted claim headers without issuing another authority.
    pub(crate) fn debug_content_binding(&self) -> super::RuntimeContextRedemptionBinding<'_> {
        super::RuntimeContextRedemptionBinding {
            execution_id: &self.claim.identity.execution_id,
            generation: self.claim.identity.generation,
            claim_id: &self.claim.claim_id,
            fence_token: &self.claim.fence.fence_token,
            resource_project_id: &self.claim.identity.resource_project_id,
        }
    }

    #[cfg(test)]
    pub(crate) fn original_code_visit_conformance_fixture() -> Self {
        tests::original_code_visit_conformance_fixture()
    }
    /// Borrow only for current-fence Main intent admission. No new permit is issued.
    pub(crate) fn intent_content_binding(&self) -> (&str, u64, &str, &[u8]) {
        (
            &self.claim.identity.execution_id,
            self.claim.identity.generation,
            &self.claim.claim_id,
            &self.claim.fence.fence_token,
        )
    }
    /// Public trace identity only. This exposes no grant or fence material.
    pub(crate) fn trace_identity(&self) -> (&str, u64) {
        (
            &self.claim.identity.execution_id,
            self.claim.identity.generation,
        )
    }

    pub(crate) fn dispatch_scope(
        &self,
    ) -> Result<crate::sandbox::dispatch::DispatchScope, crate::sandbox::dispatch::DispatchError>
    {
        crate::sandbox::dispatch::DispatchScope::from_identity(&self.claim.identity)
    }

    pub(crate) fn compiled_binding(
        &self,
        profile: &crate::sandbox::compiled_snapshot::SnapshotProfile,
        job: &crate::sandbox::request::PreparedJob,
    ) -> Result<crate::sandbox::compiled_snapshot::Binding, crate::sandbox::client::SandboxCallError>
    {
        let project = self
            .claim
            .identity
            .resource_project_id
            .parse::<i32>()
            .map_err(|_| crate::sandbox::client::SandboxCallError::Rejected)?;
        profile
            .binding(job, &self.claim.identity.tenant_id, project)
            .map_err(|_| crate::sandbox::client::SandboxCallError::Invalid)
    }
    pub(crate) fn compilation_job_key(&self, activation: &[u8; 32]) -> [u8; 32] {
        let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
        hash.update(b"elitea.sandbox.activation.v1\0");
        for part in [
            self.claim.identity.execution_id.as_str(),
            hex_lower(activation).as_str(),
        ] {
            hash.update(&(part.len() as u64).to_be_bytes());
            hash.update(part.as_bytes());
        }
        let mut key = [0; 32];
        key.copy_from_slice(hash.finish().as_ref());
        key
    }
    pub(crate) fn compiled_request(
        &self,
        activation: &[u8; 32],
    ) -> crate::protocol::elitea::runtime::v1::AuthorizeRustCompiledSnapshotRequestV1 {
        crate::protocol::elitea::runtime::v1::AuthorizeRustCompiledSnapshotRequestV1 {
            identity: Some(self.claim.identity.clone()),
            fence: Some(self.claim.fence.clone()),
            signed_command: Some(self.signed_command.clone()),
            activation_id: hex_lower(activation),
            ..Default::default()
        }
    }

    /// Actual content digest and supervisor audience are filled by `SandboxClient`.
    pub(crate) fn request(&self, activation: &[u8; 32]) -> AuthorizeSandboxJobRequestV1 {
        AuthorizeSandboxJobRequestV1 {
            identity: Some(self.claim.identity.clone()),
            fence: Some(self.claim.fence.clone()),
            activation_id: hex_lower(activation),
            signed_command: Some(self.signed_command.clone()),
            request_digest: Vec::new(),
            audience: String::new(),
            cancel_only: false,
            dependency_bundle_sha256: Vec::new(),
        }
    }
}

impl<R: ControlRpc> AgentControlClient<R> {
    pub(crate) async fn stop_sandbox_job(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &SandboxStopAuthority,
        activation: &[u8; 32],
        digest: &[u8; 32],
    ) -> Result<
        crate::protocol::elitea::runtime::v1::SandboxJobStatusV1,
        crate::sandbox::client::SandboxCallError,
    > {
        if authority.fence.workload_session_id != self.workload_session_id
            || authority.fence.producer_id != self.producer_id
        {
            return Err(crate::sandbox::client::SandboxCallError::Rejected);
        }
        sandbox
            .cancel_digest(
                &self.control,
                AuthorizeSandboxJobRequestV1 {
                    identity: Some(authority.identity.clone()),
                    fence: Some(authority.fence.clone()),
                    activation_id: hex_lower(activation),
                    signed_command: Some(authority.signed_command.clone()),
                    cancel_only: true,
                    ..Default::default()
                },
                digest,
            )
            .await
    }

    pub(crate) async fn read_compiled_snapshot(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        binding: crate::sandbox::compiled_snapshot::Binding,
    ) -> Result<
        Option<crate::sandbox::compiled_snapshot::SelectedSnapshot>,
        crate::sandbox::client::SandboxCallError,
    > {
        self.require_sandbox_authority(authority)?;
        sandbox
            .read_compiled_snapshot(
                &self.control,
                authority.compiled_request(activation),
                job,
                binding,
            )
            .await
    }
    pub(crate) async fn submit_compiled_snapshot(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        selected: &crate::sandbox::compiled_snapshot::SelectedSnapshot,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
    ) -> Result<crate::sandbox::client::SandboxOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .submit_compiled_snapshot(
                &self.control,
                authority.compiled_request(activation),
                authority.request(activation),
                job,
                selected,
                bundle,
            )
            .await
    }
    pub(crate) async fn submit_whole_code(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        intent: &[u8],
    ) -> Result<crate::sandbox::client::SandboxOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .submit_whole_code(
                &self.control,
                authority.request(activation),
                job,
                bundle,
                intent,
            )
            .await
    }
    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn submit_compiled_whole_code(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        selected: &crate::sandbox::compiled_snapshot::SelectedSnapshot,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        intent: &[u8],
    ) -> Result<crate::sandbox::client::SandboxOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .submit_compiled_snapshot_with_intent(
                &self.control,
                authority.compiled_request(activation),
                authority.request(activation),
                job,
                selected,
                bundle,
                Some(intent),
            )
            .await
    }

    #[allow(
        dead_code,
        reason = "Retain required protocol foundations without enabling deferred execution paths."
    )]
    pub(crate) async fn compile_snapshot(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        binding: crate::sandbox::compiled_snapshot::Binding,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
    ) -> Result<crate::sandbox::client::CompilationOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.compile_snapshot_from_original_visit(
            sandbox, authority, activation, job, binding, bundle, None,
        )
        .await
    }
    #[allow(clippy::too_many_arguments)] // Compile workspace access retains original visit and current claim independently.
    pub(crate) async fn compile_snapshot_from_original_visit(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        binding: crate::sandbox::compiled_snapshot::Binding,
        bundle: Option<&crate::sandbox::dependency_bundle::DependencyBundle>,
        original_visit: Option<&crate::sandbox::code_recovery::OriginalCodeVisitRef>,
    ) -> Result<crate::sandbox::client::CompilationOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        let mut request = authority.compiled_request(activation);
        if job.workspace().is_some() {
            let reference = original_visit
                .filter(|reference| reference.valid())
                .ok_or(crate::sandbox::client::SandboxCallError::Rejected)?;
            request.original_code_visit = Some(
                crate::protocol::elitea::runtime::v1::OriginalCodeVisitRefV1 {
                    visit_id: reference.visit_id.clone(),
                    revision: reference.revision,
                    digest_sha256: reference.digest_sha256.clone(),
                },
            );
        }
        let outcome = sandbox
            .compile_snapshot(
                &self.control,
                request,
                authority.request(activation),
                job,
                binding,
                bundle,
            )
            .await?;
        if matches!(&outcome,crate::sandbox::client::CompilationOutcome::Captured{job_key,..} if *job_key!=authority.compilation_job_key(activation))
        {
            return Err(crate::sandbox::client::SandboxCallError::InvalidReceipt);
        }
        Ok(outcome)
    }
    #[allow(clippy::too_many_arguments)] // Carry separate signed roles alongside their exact immutable content proof.
    pub(crate) async fn publish_snapshot(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        binding: &crate::sandbox::compiled_snapshot::Binding,
        canonical: &[u8],
        phase: crate::protocol::elitea::runtime::v1::RustCompiledPublicationPhaseV1,
    ) -> Result<crate::sandbox::client::PublicationOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .publish_snapshot(
                &self.control,
                authority.compiled_request(activation),
                job,
                binding,
                canonical,
                &authority.compilation_job_key(activation),
                phase,
            )
            .await
    }
    /// Request a fresh Main grant and submit only under the sealed invocation.
    pub(crate) async fn submit_sandbox_job(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
    ) -> Result<crate::sandbox::client::SandboxOutcome, crate::sandbox::client::SandboxCallError>
    {
        if authority.claim.fence.workload_session_id != self.workload_session_id
            || authority.claim.fence.producer_id != self.producer_id
        {
            return Err(crate::sandbox::client::SandboxCallError::Rejected);
        }
        sandbox
            .submit(&self.control, authority.request(activation), job)
            .await
    }

    pub(crate) async fn submit_sandbox_job_with_dependencies(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: &crate::sandbox::dependency_bundle::DependencyBundle,
    ) -> Result<crate::sandbox::client::SandboxOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .submit_with_dependencies(&self.control, authority.request(activation), job, bundle)
            .await
    }

    pub(crate) async fn prepare_sandbox_dependencies(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::preparation::PreparationJob,
    ) -> Result<crate::sandbox::client::PreparationOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .prepare(&self.control, authority.request(activation), job)
            .await
    }

    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(crate) async fn hydrate_sandbox_dependencies(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::request::PreparedJob,
        bundle: &crate::sandbox::dependency_bundle::DependencyBundle,
        index: usize,
        intent: Option<&[u8]>,
    ) -> Result<bool, crate::sandbox::client::SandboxCallError> {
        self.require_sandbox_authority(authority)?;
        sandbox
            .hydrate_dependencies_with_intent(
                &self.control,
                authority.request(activation),
                job,
                bundle,
                index,
                intent,
            )
            .await
    }

    pub(crate) async fn publish_sandbox_dependencies(
        &self,
        sandbox: &crate::sandbox::client::SandboxClient,
        authority: &ClaimBoundSandboxAuthority,
        activation: &[u8; 32],
        job: &crate::sandbox::preparation::PreparationJob,
        bundle: &crate::sandbox::dependency_bundle::DependencyBundle,
        index: usize,
    ) -> Result<crate::sandbox::client::PublicationOutcome, crate::sandbox::client::SandboxCallError>
    {
        self.require_sandbox_authority(authority)?;
        sandbox
            .publish(
                &self.control,
                authority.request(activation),
                job,
                bundle,
                index,
            )
            .await
    }

    fn require_sandbox_authority(
        &self,
        authority: &ClaimBoundSandboxAuthority,
    ) -> Result<(), crate::sandbox::client::SandboxCallError> {
        if authority.claim.fence.workload_session_id != self.workload_session_id
            || authority.claim.fence.producer_id != self.producer_id
        {
            return Err(crate::sandbox::client::SandboxCallError::Rejected);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::super::{ClaimCommandResponseV1, parse_accepted_agent_claim};
    use super::*;
    use crate::protocol::command::{
        TestOnlyConformanceHmacAuthenticator, parse_and_verify_agent_command,
    };
    use prost::Message;

    fn vector(name: &str) -> Vec<u8> {
        let (_, hex) = include_str!("../../tests/fixtures/agent_control_vectors.txt")
            .lines()
            .filter_map(|line| line.split_once('='))
            .find(|(key, _)| *key == name)
            .unwrap();
        hex.as_bytes()
            .chunks_exact(2)
            .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
            .collect()
    }
    fn verified() -> VerifiedAgentCommand {
        parse_and_verify_agent_command(
            &vector("signed_command"),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .unwrap()
    }
    fn authority() -> ClaimBoundRuntimeContextAuthority {
        let verified = verified();
        let response = ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).unwrap();
        let claim = parse_accepted_agent_claim(
            &verified,
            response,
            "workload-1",
            "worker-1",
            1_700_000_000_000,
        )
        .unwrap();
        ClaimBoundRuntimeContextAuthority::from_claim(&claim)
    }
    pub(super) fn original_code_visit_conformance_fixture() -> ClaimBoundSandboxAuthority {
        use crate::protocol::elitea::runtime::v1::{
            SignedWorkerCommandEnvelopeV1, WorkerCommandV1,
        };
        let mut signed =
            SignedWorkerCommandEnvelopeV1::decode(vector("signed_command").as_slice()).unwrap();
        let mut body = WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).unwrap();
        let original_execution = body.execution_id.clone();
        body.execution_id = "1".repeat(32);
        if body.root_execution_id == original_execution {
            body.root_execution_id = body.execution_id.clone();
        }
        signed.worker_command_bytes = body.encode_to_vec();
        signed.worker_command_digest.as_mut().unwrap().value =
            ring::digest::digest(&ring::digest::SHA256, &signed.worker_command_bytes)
                .as_ref()
                .to_vec();
        // Existing public offline conformance key only; no production signer is exposed.
        signed.signature = ring::hmac::sign(
            &ring::hmac::Key::new(
                ring::hmac::HMAC_SHA256,
                b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET",
            ),
            &signed.worker_command_bytes,
        )
        .as_ref()
        .to_vec();
        let command = parse_and_verify_agent_command(
            &signed.encode_to_vec(),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .unwrap();
        let mut reply =
            ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).unwrap();
        let receipt = reply.receipt.as_mut().unwrap();
        receipt.identity.as_mut().unwrap().execution_id = body.execution_id;
        receipt.claim_id = "5".repeat(32);
        let decision = super::super::parse_agent_claim_decision(
            &command,
            reply,
            "workload-1",
            "worker-1",
            1_700_000_000_000,
        )
        .unwrap();
        let super::super::AgentClaimDecision::Accepted(claim) = decision else {
            panic!("actual conformance admission did not accept the original claim");
        };
        let mut authority = ClaimBoundRuntimeContextAuthority::from_claim(&claim);
        let sandbox = authority.take_sandbox_authority(&command).unwrap();
        assert!(authority.take_sandbox_authority(&command).is_err());
        sandbox
    }
    #[test]
    fn sandbox_authority_preserves_exact_command_and_fence_and_is_taken_once() {
        let mut owner = authority();
        let redemption_execution = owner.redemption_binding().execution_id.to_owned();
        let command = verified();
        let sandbox = owner.take_sandbox_authority(&command).unwrap();
        let request = sandbox.request(&[7; 32]);
        assert_eq!(
            request.identity.as_ref().unwrap().execution_id,
            redemption_execution
        );
        assert_eq!(request.signed_command.as_ref(), Some(command.signed()));
        assert_eq!(
            request.fence.as_ref().unwrap().workload_session_id,
            "workload-1"
        );
        assert_eq!(request.activation_id, "07".repeat(32));
        assert!(request.request_digest.is_empty());
        assert!(request.audience.is_empty());
        assert!(owner.take_sandbox_authority(&command).is_err());
        assert_eq!(
            owner.redemption_binding().execution_id,
            redemption_execution
        );
    }
    #[test]
    fn changed_claim_identity_or_exact_command_binding_cannot_mint_authority() {
        let mut owner = authority();
        owner.sandbox.as_mut().unwrap().command_binding[0] ^= 1;
        assert!(owner.sandbox_stop_authority(&verified()).is_err());
        assert!(owner.take_sandbox_authority(&verified()).is_err());
        assert!(owner.sandbox.is_some());
        let mut owner = authority();
        owner.sandbox.as_mut().unwrap().identity.generation += 1;
        assert!(owner.sandbox_stop_authority(&verified()).is_err());
        assert!(owner.take_sandbox_authority(&verified()).is_err());
    }

    #[test]
    fn recovery_stop_requires_cancelled_exact_execution() {
        let owner = authority();
        let claim = owner.sandbox.as_ref().unwrap();
        let mut recovery = super::super::AgentOutputRecovery {
            kind: super::super::AgentOutputRecoveryKind::Running,
            binding: super::super::RecoveryClaimBinding {
                identity: claim.identity.clone(),
                fence: claim.fence.clone(),
                lease_expires_at_unix_millis: 1_700_000_060_000,
                claim_id: "recovery".into(),
                claim_handoff_watermark: 0,
                desired_state: super::super::DesiredExecutionState::Running,
            },
        };
        assert!(recovery.sandbox_stop_authority(&verified()).is_err());
        recovery.binding.desired_state = super::super::DesiredExecutionState::Cancelled;
        assert!(recovery.sandbox_stop_authority(&verified()).is_ok());
        recovery.binding.identity.generation += 1;
        assert!(recovery.sandbox_stop_authority(&verified()).is_err());
    }

    #[tokio::test]
    async fn another_worker_cannot_submit_the_sealed_claim() {
        let stop_authority = authority().sandbox_stop_authority(&verified()).unwrap();
        let authority = authority().take_sandbox_authority(&verified()).unwrap();
        let channel = tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy();
        let control = AgentControlClient::from_channel(
            channel.clone(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: std::time::Duration::from_secs(1),
                workload_session_id: "different-workload".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap();
        let sandbox = crate::sandbox::client::SandboxClient::from_channel(
            channel,
            "dns:sandbox.test".into(),
            std::time::Duration::from_secs(1),
        )
        .unwrap();
        assert!(matches!(
            control
                .stop_sandbox_job(&sandbox, &stop_authority, &[7; 32], &[8; 32])
                .await,
            Err(crate::sandbox::client::SandboxCallError::Rejected)
        ));
        let job = crate::sandbox::request::PreparedJob::new(
            crate::sandbox::request::Language::Python,
            "7".into(),
            std::collections::BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "test-v1".into(),
            10,
        )
        .unwrap();
        assert!(matches!(
            control
                .submit_sandbox_job(&sandbox, &authority, &[7; 32], &job)
                .await,
            Err(crate::sandbox::client::SandboxCallError::Rejected)
        ));
        let preparation = crate::sandbox::preparation::PreparationJob::new(
            "7".into(),
            format!("sha256:{}", "a".repeat(64)),
            "test-v1".into(),
            10,
        )
        .unwrap();
        assert!(matches!(
            control
                .prepare_sandbox_dependencies(&sandbox, &authority, &[7; 32], &preparation)
                .await,
            Err(crate::sandbox::client::SandboxCallError::Rejected)
        ));
        let content = format!(
            r#"{{"revision":1,"runtime":"pyodide-0.29.0","requirements":[],"files":[{{"name":"elitea-python-lock.json","bytes":2,"sha256":"{}"}}]}}"#,
            "a".repeat(64)
        );
        let root = crate::sandbox::dependency_bundle::hex(
            ring::digest::digest(&ring::digest::SHA256, content.as_bytes()).as_ref(),
        );
        let bundle = crate::sandbox::dependency_bundle::DependencyBundle::parse_record(
            format!(r#"{},"digest":"{root}"}}"#, &content[..content.len() - 1]).as_bytes(),
        )
        .unwrap();
        assert!(matches!(
            control
                .submit_sandbox_job_with_dependencies(&sandbox, &authority, &[7; 32], &job, &bundle)
                .await,
            Err(crate::sandbox::client::SandboxCallError::Rejected)
        ));
        assert!(matches!(
            control
                .hydrate_sandbox_dependencies(
                    &sandbox, &authority, &[7; 32], &job, &bundle, 0, None
                )
                .await,
            Err(crate::sandbox::client::SandboxCallError::Rejected)
        ));
    }
}

#[path = "sandbox_workspace_authority.rs"]
mod workspace;
