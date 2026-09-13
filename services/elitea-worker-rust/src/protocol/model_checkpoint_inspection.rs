//! Checkpoint inspection is distinct from fresh invocation authority.

use super::{
    AcceptedAgentClaim, ClaimBoundSessionAuthority, ClaimCommandResponseV1, ClaimDispositionV1,
    ControlSemanticError, VerifiedAgentCommand, parse_input_claim_binding,
};

/// Agent-only claim result for workers that can inspect model checkpoints.
#[allow(dead_code)] // Enabled only after the complete recovery coordinator is connected.
pub(crate) enum CheckpointClaimDecision {
    Ordinary(Box<super::AgentClaimDecision>),
    Inspect(Box<ModelCheckpointInspection>),
}

impl<R: crate::transport::ControlRpc> super::AgentControlClient<R> {
    #[allow(dead_code)]
    pub(crate) async fn claim_agent_checkpoint_delivery(
        &self,
        verified: &VerifiedAgentCommand,
        now_unix_millis: i64,
    ) -> Result<CheckpointClaimDecision, super::AgentControlError> {
        let mut request = super::build_agent_claim_request(
            verified,
            &self.workload_session_id,
            &self.producer_id,
        )?;
        request.agent_model_checkpoint_recovery = true;
        let response = self.control.claim_command(request).await?;
        parse_checkpoint_claim_decision(
            verified,
            response,
            &self.workload_session_id,
            &self.producer_id,
            now_unix_millis,
        )
        .map_err(Into::into)
    }
}

fn parse_checkpoint_claim_decision(
    verified: &VerifiedAgentCommand,
    response: ClaimCommandResponseV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<CheckpointClaimDecision, ControlSemanticError> {
    if response.receipt.as_ref().is_some_and(|receipt| {
        receipt.disposition == ClaimDispositionV1::RecoverAgentModelCheckpoint as i32
    }) {
        ModelCheckpointInspection::parse(
            verified,
            response,
            workload_session_id,
            producer_id,
            now_unix_millis,
        )
        .map(|inspection| CheckpointClaimDecision::Inspect(Box::new(inspection)))
    } else {
        super::parse_agent_claim_decision(
            verified,
            response,
            workload_session_id,
            producer_id,
            now_unix_millis,
        )
        .map(|decision| CheckpointClaimDecision::Ordinary(Box::new(decision)))
    }
}

/// Authenticated immutable inputs and a session fence, with no model permit.
/// This value cannot be passed to `begin_agent_execution`.
#[allow(dead_code)] // Recovery dispatch remains disabled until output replacement is wired.
pub(crate) struct ModelCheckpointInspection {
    pub(super) claim: AcceptedAgentClaim,
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

    pub(crate) fn into_lease_supervision(
        self,
    ) -> (PendingModelCheckpointInspection, super::ClaimLeaseHandle) {
        let lease = super::ClaimLeaseHandle {
            identity: self.claim.identity.clone(),
            fence: self.claim.fence.clone(),
            claim_id: self.claim.claim_id.clone(),
            lease_expires_at_unix_millis: self.claim.lease_expires_at_unix_millis,
            renewal_sequence: 0,
        };
        (
            PendingModelCheckpointInspection { claim: self.claim },
            lease,
        )
    }

    #[cfg(test)]
    fn into_session_inspection(
        self,
    ) -> (InspectedModelCheckpointClaim, ClaimBoundSessionAuthority) {
        LiveModelCheckpointInspection { claim: self.claim }.into_session_inspection()
    }
}

#[allow(dead_code)]
pub(crate) struct PendingModelCheckpointInspection {
    claim: AcceptedAgentClaim,
}
#[allow(dead_code)]
impl PendingModelCheckpointInspection {
    pub(crate) fn into_live(self) -> LiveModelCheckpointInspection {
        LiveModelCheckpointInspection { claim: self.claim }
    }
}

#[allow(dead_code)]
pub(crate) struct LiveModelCheckpointInspection {
    claim: AcceptedAgentClaim,
}
#[allow(dead_code)]
impl LiveModelCheckpointInspection {
    /// Borrow only the frozen request binding while the inspection lease is live.
    pub(crate) fn input_content_authority(&self) -> Option<super::ClaimBoundInputAuthority<'_>> {
        self.claim
            .input_content_authority_for_entry(&self.claim.request_entry.entry_id)
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

/// Restored-model authority. The inspection phase already owns session access.
#[allow(dead_code)] // Consumed by the recovery coordinator integration.
pub(crate) struct AuthorizedModelCheckpoint {
    checkpoint: crate::agents::session::ValidatedModelCheckpoint,
    permit: super::InvocationSubmissionPermit,
    output: super::AgentExecutionOutputAuthority,
    runtime_context: super::ClaimBoundRuntimeContextAuthority,
}

impl AuthorizedModelCheckpoint {
    pub(crate) fn matches_checkpoint(
        &self,
        checkpoint: &crate::agents::session::ValidatedModelCheckpoint,
    ) -> bool {
        self.checkpoint.matches_checkpoint(checkpoint)
    }
}

/// Failed or uncertain authorization cannot be retried through this value.
#[allow(dead_code)]
pub(crate) struct ModelCheckpointAuthorizationFailure {
    claim: InspectedModelCheckpointClaim,
    error: super::AgentControlError,
}

#[allow(dead_code)]
impl InspectedModelCheckpointClaim {
    pub(crate) async fn authorize<R: crate::transport::ControlRpc>(
        self,
        control: &super::AgentControlClient<R>,
        checkpoint: crate::agents::session::ValidatedModelCheckpoint,
    ) -> Result<AuthorizedModelCheckpoint, ModelCheckpointAuthorizationFailure> {
        if !checkpoint.matches_execution(
            &self.claim.identity.execution_id,
            self.claim.identity.generation,
        ) {
            return Err(ModelCheckpointAuthorizationFailure {
                claim: self,
                error: ControlSemanticError::AuthorizationFailed(
                    "the checkpoint belongs to another execution",
                )
                .into(),
            });
        }
        let request =
            crate::protocol::elitea::runtime::v1::AuthorizeAgentModelCheckpointRequestV1 {
                identity: Some(self.claim.identity.clone()),
                fence: Some(self.claim.fence.clone()),
                checkpoint_digest: Some(super::DigestV1 {
                    algorithm: super::DigestAlgorithmV1::Sha256 as i32,
                    value: checkpoint.digest().to_vec(),
                }),
            };
        let response = match control
            .control
            .authorize_agent_model_checkpoint(request)
            .await
        {
            Ok(response) => response,
            Err(error) => {
                return Err(ModelCheckpointAuthorizationFailure {
                    claim: self,
                    error: error.into(),
                });
            }
        };
        let decision =
            super::parse_authorize_invocation_response(&super::AuthorizeInvocationResponseV1 {
                disposition: response.disposition,
                rejection: response.rejection,
            });
        match decision {
            Ok(super::AuthorizeInvocationDecision::AuthorizedNow) => {
                let runtime_context =
                    super::ClaimBoundRuntimeContextAuthority::from_claim(&self.claim);
                Ok(AuthorizedModelCheckpoint {
                    checkpoint,
                    permit: super::InvocationSubmissionPermit { _sealed: () },
                    output: super::AgentExecutionOutputAuthority { claim: self.claim },
                    runtime_context,
                })
            }
            other => {
                let error = match other {
                    Err(error) => error,
                    _ => ControlSemanticError::AuthorizationFailed(
                        "the checkpoint attempt is already authorized",
                    ),
                };
                Err(ModelCheckpointAuthorizationFailure {
                    claim: self,
                    error: error.into(),
                })
            }
        }
    }
}

#[cfg(test)]
pub(crate) fn test_checkpoint_authorizer(
    execution_id: &str,
    generation: u64,
    digest: [u8; 32],
) -> (
    InspectedModelCheckpointClaim,
    super::AgentControlClient<impl crate::transport::ControlRpc>,
) {
    tests::authorizer(execution_id, generation, digest)
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

    use crate::protocol::elitea::runtime::v1::*;
    use crate::transport::{ControlGrpcConfig, ControlRpc};
    use async_trait::async_trait;
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };
    use std::time::Duration;
    use tonic::{Request, Response, Status};

    struct CheckpointRpc {
        claim_response: Option<ClaimCommandResponseV1>,
        calls: Arc<AtomicUsize>,
        response: Option<AuthorizeAgentModelCheckpointResponseV1>,
        expected_digest: [u8; 32],
    }
    #[async_trait]
    impl ControlRpc for CheckpointRpc {
        async fn authorize_agent_model_checkpoint(
            &self,
            request: Request<AuthorizeAgentModelCheckpointRequestV1>,
        ) -> Result<Response<AuthorizeAgentModelCheckpointResponseV1>, Status> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            assert_eq!(
                request
                    .get_ref()
                    .checkpoint_digest
                    .as_ref()
                    .expect("digest")
                    .value,
                self.expected_digest.to_vec()
            );
            self.response
                .clone()
                .map(Response::new)
                .ok_or_else(|| Status::unavailable("lost response"))
        }
        async fn claim_command(
            &self,
            request: Request<ClaimCommandRequestV1>,
        ) -> Result<Response<ClaimCommandResponseV1>, Status> {
            assert!(request.get_ref().agent_model_checkpoint_recovery);
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(Response::new(
                self.claim_response.clone().expect("expected claim"),
            ))
        }
        async fn begin_execution(
            &self,
            _: Request<BeginExecutionRequestV1>,
        ) -> Result<Response<BeginExecutionResponseV1>, Status> {
            panic!("unexpected begin")
        }
        async fn authorize_invocation(
            &self,
            _: Request<AuthorizeInvocationRequestV1>,
        ) -> Result<Response<AuthorizeInvocationResponseV1>, Status> {
            panic!("unexpected ordinary authorization")
        }
        async fn renew_lease(
            &self,
            _: Request<RenewLeaseRequestV1>,
        ) -> Result<Response<RenewLeaseResponseV1>, Status> {
            panic!("unexpected renewal")
        }
        async fn observe_desired_state(
            &self,
            _: Request<ObserveDesiredStateRequestV1>,
        ) -> Result<Response<ObserveDesiredStateResponseV1>, Status> {
            panic!("unexpected observation")
        }
        async fn prepare_settlement(
            &self,
            _: Request<PrepareSettlementRequestV1>,
        ) -> Result<Response<PrepareSettlementResponseV1>, Status> {
            panic!("unexpected settlement")
        }
    }

    #[tokio::test]
    async fn checkpoint_claim_opt_in_preserves_ordinary_routes_and_validates_inspection() {
        for case in 0..3 {
            let verified = parse_and_verify_agent_command(
                &vector("signed_command"),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .expect("command");
            let mut response = ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice())
                .expect("response");
            let receipt = response.receipt.as_mut().expect("receipt");
            let fence = receipt.fence.clone().expect("fence");
            let now = receipt.lease_expires_at_unix_millis - 1000;
            if case != 0 {
                receipt.disposition = ClaimDispositionV1::RecoverAgentModelCheckpoint as i32;
            }
            if case == 2 {
                receipt.fence.as_mut().expect("fence").producer_id = "wrong-worker".into();
            }
            let calls = Arc::new(AtomicUsize::new(0));
            let client = super::super::AgentControlClient::new(
                CheckpointRpc {
                    claim_response: Some(response),
                    calls: calls.clone(),
                    response: None,
                    expected_digest: [0; 32],
                },
                ControlGrpcConfig {
                    deadline: Duration::from_secs(1),
                    workload_session_id: fence.workload_session_id,
                    producer_id: fence.producer_id,
                },
            )
            .expect("client");
            let outcome = client.claim_agent_checkpoint_delivery(&verified, now).await;
            match outcome {
                Ok(CheckpointClaimDecision::Ordinary(decision)) => {
                    assert_eq!(case, 0);
                    assert!(matches!(
                        *decision,
                        super::super::AgentClaimDecision::Accepted(_)
                    ));
                }
                Ok(CheckpointClaimDecision::Inspect(_)) => assert_eq!(case, 1),
                Err(_) => assert_eq!(case, 2),
            }
            assert_eq!(calls.load(Ordering::SeqCst), 1);
        }
    }

    #[tokio::test]
    async fn only_first_authorization_for_matching_checkpoint_grants_a_permit() {
        for case in 0..4 {
            let verified = parse_and_verify_agent_command(
                &vector("signed_command"),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .expect("command");
            let mut response = ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice())
                .expect("response");
            let receipt = response.receipt.as_mut().expect("receipt");
            receipt.disposition = ClaimDispositionV1::RecoverAgentModelCheckpoint as i32;
            let fence = receipt.fence.clone().expect("fence");
            let now = receipt.lease_expires_at_unix_millis - 1000;
            let inspection = ModelCheckpointInspection::parse(
                &verified,
                response,
                &fence.workload_session_id,
                &fence.producer_id,
                now,
            )
            .expect("inspection");
            let (pending, _session) = inspection.into_session_inspection();
            let calls = Arc::new(AtomicUsize::new(0));
            let rpc = CheckpointRpc {
                claim_response: None,
                expected_digest: [42; 32],
                calls: calls.clone(),
                response: (case != 2).then_some(AuthorizeAgentModelCheckpointResponseV1 {
                    disposition: if case == 1 {
                        AuthorizeInvocationDispositionV1::AlreadyAuthorized as i32
                    } else {
                        AuthorizeInvocationDispositionV1::AuthorizedNow as i32
                    },
                    rejection: None,
                }),
            };
            let client = super::super::AgentControlClient::new(
                rpc,
                ControlGrpcConfig {
                    deadline: Duration::from_secs(1),
                    workload_session_id: fence.workload_session_id,
                    producer_id: fence.producer_id,
                },
            )
            .expect("client");
            let evidence = crate::agents::session::ValidatedModelCheckpoint::test_evidence(
                if case == 3 {
                    "another-execution".into()
                } else {
                    verified.command().execution_id.clone()
                },
                verified.command().generation,
            );
            let outcome = pending.authorize(&client, evidence).await;
            assert_eq!(outcome.is_ok(), case == 0, "case {case}");
            assert_eq!(
                calls.load(Ordering::SeqCst),
                usize::from(case != 3),
                "case {case}"
            );
        }
    }

    pub(super) fn authorizer(
        execution_id: &str,
        generation: u64,
        digest: [u8; 32],
    ) -> (
        InspectedModelCheckpointClaim,
        super::super::AgentControlClient<impl ControlRpc>,
    ) {
        let verified = parse_and_verify_agent_command(
            &vector("signed_command"),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .expect("command");
        let response =
            ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("response");
        let fence = response
            .receipt
            .as_ref()
            .expect("receipt")
            .fence
            .clone()
            .expect("fence");
        let now = response
            .receipt
            .as_ref()
            .expect("receipt")
            .lease_expires_at_unix_millis
            - 1000;
        let mut claim = parse_accepted_agent_claim(
            &verified,
            response,
            &fence.workload_session_id,
            &fence.producer_id,
            now,
        )
        .expect("fixture claim");
        // Unit fixture identity matches the session test; no live claim is changed.
        claim.identity.execution_id = execution_id.into();
        claim.identity.generation = generation;
        let rpc = CheckpointRpc {
            claim_response: None,
            calls: Arc::new(AtomicUsize::new(0)),
            expected_digest: digest,
            response: Some(AuthorizeAgentModelCheckpointResponseV1 {
                disposition: AuthorizeInvocationDispositionV1::AuthorizedNow as i32,
                rejection: None,
            }),
        };
        let client = super::super::AgentControlClient::new(
            rpc,
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: fence.workload_session_id,
                producer_id: fence.producer_id,
            },
        )
        .expect("client");
        (InspectedModelCheckpointClaim { claim }, client)
    }

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
