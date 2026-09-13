//! Checkpoint inspection is distinct from fresh invocation authority.

use super::{
    AcceptedAgentClaim, ClaimBoundSessionAuthority, ClaimCommandResponseV1, ClaimDispositionV1,
    ControlSemanticError, VerifiedAgentCommand, parse_input_claim_binding,
};

/// Authenticated immutable inputs and a session fence, with no model permit.
/// This value cannot be passed to `begin_agent_execution`.
#[allow(dead_code)] // Recovery dispatch remains disabled until output replacement is wired.
pub(crate) struct ModelCheckpointInspection {
    claim: AcceptedAgentClaim,
}

/// The remaining one-use claim after session inspection authority is issued.
#[allow(dead_code)]
pub(crate) struct InspectedModelCheckpointClaim {
    claim: AcceptedAgentClaim,
}

#[allow(dead_code)]
impl ModelCheckpointInspection {
    pub(crate) fn parse(
        verified: &VerifiedAgentCommand,
        response: ClaimCommandResponseV1,
        workload_session_id: &str,
        producer_id: &str,
        now_unix_millis: i64,
    ) -> Result<Self, ControlSemanticError> {
        let claim = parse_input_claim_binding(
            verified,
            response,
            workload_session_id,
            producer_id,
            now_unix_millis,
            ClaimDispositionV1::RecoverAgentModelCheckpoint,
        )?;
        Ok(Self { claim })
    }

    /// Issue session access once. No runtime credential or submission permit
    /// is created. The retained claim can authorize only the inspected model.
    pub(crate) fn into_session_inspection(
        self,
    ) -> (InspectedModelCheckpointClaim, ClaimBoundSessionAuthority) {
        let session = ClaimBoundSessionAuthority::from_claim(&self.claim);
        (InspectedModelCheckpointClaim { claim: self.claim }, session)
    }
}

#[cfg(test)]
mod tests {
    use super::super::parse_accepted_agent_claim;
    use super::*;
    use crate::protocol::command::{
        TestOnlyConformanceHmacAuthenticator, parse_and_verify_agent_command,
    };
    use crate::protocol::elitea::runtime::v1::DesiredExecutionStateV1;
    use prost::Message;

    fn vector(name: &str) -> Vec<u8> {
        let (_, hex) = include_str!("../../tests/fixtures/agent_control_vectors.txt")
            .lines()
            .filter_map(|line| line.split_once('='))
            .find(|(key, _)| *key == name)
            .expect("fixture");
        hex.as_bytes()
            .chunks_exact(2)
            .map(|pair| {
                u8::from_str_radix(std::str::from_utf8(pair).expect("hex"), 16).expect("byte")
            })
            .collect()
    }

    #[test]
    fn inspection_is_not_fresh_authority_and_preserves_the_session_fence() {
        let verified = parse_and_verify_agent_command(
            &vector("signed_command"),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .expect("command");
        let mut response =
            ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("response");
        let receipt = response.receipt.as_mut().expect("receipt");
        let fence = receipt.fence.clone().expect("fence");
        let now = receipt.lease_expires_at_unix_millis - 1000;
        assert!(
            ModelCheckpointInspection::parse(
                &verified,
                response.clone(),
                &fence.workload_session_id,
                &fence.producer_id,
                now
            )
            .is_err()
        );
        response.receipt.as_mut().expect("receipt").disposition =
            ClaimDispositionV1::RecoverAgentModelCheckpoint as i32;
        assert!(
            parse_accepted_agent_claim(
                &verified,
                response.clone(),
                &fence.workload_session_id,
                &fence.producer_id,
                now
            )
            .is_err()
        );
        let inspection = ModelCheckpointInspection::parse(
            &verified,
            response.clone(),
            &fence.workload_session_id,
            &fence.producer_id,
            now,
        )
        .expect("inspection");
        let (pending, session) = inspection.into_session_inspection();
        let binding = session.into_writer_binding();
        assert_eq!(binding.execution_id, verified.command().execution_id);
        assert_eq!(binding.claim_attempt, fence.claim_attempt);
        assert_eq!(binding.lease_epoch, fence.lease_epoch);
        assert_eq!(&binding.fence_token[..], &fence.fence_token);
        assert_eq!(pending.claim.fence, fence);
        for mutation in 0..5 {
            let mut invalid = response.clone();
            let receipt = invalid.receipt.as_mut().expect("receipt");
            match mutation {
                0 => receipt.identity.as_mut().expect("identity").generation += 1,
                1 => {
                    receipt.fence.as_mut().expect("fence").workload_session_id =
                        "other-session".into();
                }
                2 => receipt.lease_expires_at_unix_millis = now,
                3 => receipt.desired_state = DesiredExecutionStateV1::Cancelled as i32,
                _ => {
                    receipt
                        .input_bundle_ref
                        .as_mut()
                        .expect("reference")
                        .immutable_version = "changed".into();
                }
            }
            assert!(
                ModelCheckpointInspection::parse(
                    &verified,
                    invalid,
                    &fence.workload_session_id,
                    &fence.producer_id,
                    now
                )
                .is_err(),
                "mutation {mutation}"
            );
        }
        response.receipt.as_mut().expect("receipt").input_bundle = None;
        assert!(
            ModelCheckpointInspection::parse(
                &verified,
                response,
                &fence.workload_session_id,
                &fence.producer_id,
                now
            )
            .is_err()
        );
    }
}
