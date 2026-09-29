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
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    command_binding: [u8; 32],
}

impl SandboxClaimBinding {
    pub(super) fn from_claim(claim: &AcceptedAgentClaim) -> Self {
        Self {
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

impl ClaimBoundRuntimeContextAuthority {
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
    pub(crate) fn dispatch_scope(
        &self,
    ) -> Result<crate::sandbox::dispatch::DispatchScope, crate::sandbox::dispatch::DispatchError>
    {
        crate::sandbox::dispatch::DispatchScope::from_identity(&self.claim.identity)
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
        }
    }
}

impl<R: ControlRpc> AgentControlClient<R> {
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
        assert!(owner.take_sandbox_authority(&verified()).is_err());
        assert!(owner.sandbox.is_some());
        let mut owner = authority();
        owner.sandbox.as_mut().unwrap().identity.generation += 1;
        assert!(owner.take_sandbox_authority(&verified()).is_err());
    }

    #[tokio::test]
    async fn another_worker_cannot_submit_the_sealed_claim() {
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
    }
}
