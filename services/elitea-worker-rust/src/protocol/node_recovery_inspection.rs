//! Recovery-only claims carry no ordinary invocation or credential permit.

use std::sync::Arc;

use super::{
    AcceptedAgentClaim, AgentClaimDecision, AgentControlClient, AgentControlError,
    ClaimBoundSessionAuthority, ClaimCommandResponseV1, ClaimDispositionV1, ClaimLeaseHandle,
    ControlSemanticError, VerifiedAgentCommand, parse_input_claim_binding,
};
use crate::agents::graph::node_recovery_receipt::NodeRecoveryRequiredReceipt;

pub(crate) enum NodeRecoveryClaimDecision {
    Ordinary(Box<AgentClaimDecision>),
    ModelInspection(Box<super::ModelCheckpointInspection>),
    Recover(Box<NodeRecoveryInspection>),
}

impl<R: crate::transport::ControlRpc> AgentControlClient<R> {
    /// Only the isolated recovery supervisor may opt in.
    pub(crate) async fn claim_node_recovery_delivery(
        &self,
        verified: &VerifiedAgentCommand,
        now_ms: i64,
    ) -> Result<NodeRecoveryClaimDecision, AgentControlError> {
        let mut request = super::build_agent_claim_request(
            verified,
            &self.workload_session_id,
            &self.producer_id,
        )?;
        request.node_recovery = true;
        request.agent_model_checkpoint_recovery = false;
        let response = self.control.claim_command(request).await?;
        match response.receipt.as_ref().map(|r| r.disposition) {
            Some(value) if value == ClaimDispositionV1::RecoverNodeVisit as i32 => {
                NodeRecoveryInspection::parse(
                    verified,
                    response,
                    &self.workload_session_id,
                    &self.producer_id,
                    now_ms,
                )
                .map(|claim| NodeRecoveryClaimDecision::Recover(Box::new(claim)))
                .map_err(Into::into)
            }
            Some(value) if value == ClaimDispositionV1::RecoverAgentModelCheckpoint as i32 => {
                super::ModelCheckpointInspection::parse(
                    verified,
                    response,
                    &self.workload_session_id,
                    &self.producer_id,
                    now_ms,
                )
                .map(|claim| NodeRecoveryClaimDecision::ModelInspection(Box::new(claim)))
                .map_err(Into::into)
            }
            _ => super::parse_agent_claim_decision(
                verified,
                response,
                &self.workload_session_id,
                &self.producer_id,
                now_ms,
            )
            .map(|claim| NodeRecoveryClaimDecision::Ordinary(Box::new(claim)))
            .map_err(Into::into),
        }
    }
}

/// Sealed from authenticated Main claim material. Intentionally not Clone/Debug.
pub(crate) struct NodeRecoveryInspection {
    claim: AcceptedAgentClaim,
    receipt: NodeRecoveryRequiredReceipt,
}
impl NodeRecoveryInspection {
    pub(crate) fn producer_id(&self) -> &str {
        self.claim.producer_id()
    }
    pub(crate) fn matches_output_transport(&self, session: &str, producer: &str) -> bool {
        self.claim.matches_output_transport(session, producer)
    }
    pub(crate) fn matches_output_identity(&self, frame: &super::ExecutionOutputFrameV1) -> bool {
        self.claim.matches_output_identity(frame.identity.as_ref())
    }
    pub(crate) fn output_watermark(&self) -> u64 {
        self.claim.claim_handoff_watermark()
    }
    fn parse(
        verified: &VerifiedAgentCommand,
        response: ClaimCommandResponseV1,
        session: &str,
        producer: &str,
        now_ms: i64,
    ) -> Result<Self, ControlSemanticError> {
        let bytes = &response
            .receipt
            .as_ref()
            .ok_or(ControlSemanticError::InvalidInput(
                "the node recovery claim is missing",
            ))?
            .node_recovery_receipt_json;
        if bytes.is_empty() || bytes.len() > 16 * 1024 {
            return Err(ControlSemanticError::ResourceExhausted(
                "the node recovery receipt exceeds its bound",
            ));
        }
        let receipt: NodeRecoveryRequiredReceipt = serde_json::from_slice(bytes).map_err(|_| {
            ControlSemanticError::InvalidInput("the node recovery receipt is invalid")
        })?;
        if !receipt.validate() {
            return Err(ControlSemanticError::InvalidInput(
                "the node recovery receipt is invalid",
            ));
        }
        let claim = parse_input_claim_binding(
            verified,
            response,
            session,
            producer,
            now_ms,
            ClaimDispositionV1::RecoverNodeVisit,
        )?;
        Ok(Self { claim, receipt })
    }
    pub(crate) fn into_recovery_supervision(
        self,
    ) -> (
        NodeRecoveryControlAuthority,
        ClaimLeaseHandle,
        ClaimBoundSessionAuthority,
    ) {
        let lease = ClaimLeaseHandle {
            identity: self.claim.identity.clone(),
            fence: self.claim.fence.clone(),
            claim_id: self.claim.claim_id.clone(),
            lease_expires_at_unix_millis: self.claim.lease_expires_at_unix_millis,
            renewal_sequence: 0,
        };
        let session = ClaimBoundSessionAuthority::from_claim(&self.claim);
        let control = NodeRecoveryControlAuthority {
            claim: self.claim,
            receipt: self.receipt,
        };
        (control, lease, session)
    }
}

/// Borrowed only by the exact mTLS control client and recovery journal owner.
pub(crate) struct NodeRecoveryControlAuthority {
    claim: AcceptedAgentClaim,
    receipt: NodeRecoveryRequiredReceipt,
}
impl NodeRecoveryControlAuthority {
    /// Advance only the immutable receipt under this same current claim. This
    /// grants no Begin/Invoke permit, lease extension, or checkpoint restore.
    pub(crate) async fn advance_no_effect_continuation(
        &mut self,
        reply: &crate::transport::input_content::NodeRecoveryAckReply,
        applied: &crate::agents::graph::node_recovery_runtime::AppliedNodeRecoveryAction,
        request: crate::agents::graph::node_recovery::OperatorRetryRequest,
        visit: &mut crate::agents::node_recovery_checkpoint::OpenedNodeRecoveryVisit,
    ) -> Result<(), ControlSemanticError> {
        let continuation =
            applied
                .continuation_receipt()
                .ok_or(ControlSemanticError::AuthorizationFailed(
                    "the owner reconciliation has no continuation",
                ))?;
        if !visit.matches_receipt(&self.receipt)
            || !applied.matches(request)
            || reply.recovery_resume_authorized
            || reply.resumption.is_some()
            || reply.terminal_settlement_authorized
            || reply.terminal_authorization.is_some()
            || reply.applied_revision != applied.applied_revision()
            || continuation.journal_revision != applied.applied_revision()
            || applied.terminal_stop_reason().is_some()
            || applied.failure_route_continuation().is_some()
        {
            return Err(ControlSemanticError::AuthorizationFailed(
                "the no-effect ACK does not preserve suspension",
            ));
        }
        visit
            .advance_no_effect_receipt(continuation.clone())
            .await
            .map_err(|_| {
                ControlSemanticError::AuthorizationFailed(
                    "the reconciled journal changed after ACK",
                )
            })?;
        self.receipt = continuation.clone();
        Ok(())
    }
    fn claim(&self) -> &AcceptedAgentClaim {
        &self.claim
    }
    pub(crate) fn execution_id(&self) -> &str {
        &self.claim().identity.execution_id
    }
    pub(crate) fn generation(&self) -> u64 {
        self.claim().identity.generation
    }
    pub(crate) fn receipt(&self) -> &NodeRecoveryRequiredReceipt {
        &self.receipt
    }
    pub(crate) fn claim_id(&self) -> &str {
        &self.claim().claim_id
    }
    pub(crate) fn fence_bytes(&self) -> &[u8] {
        &self.claim().fence.fence_token
    }
    pub(crate) fn input_content_authority(&self) -> Option<super::ClaimBoundInputAuthority<'_>> {
        self.claim()
            .input_content_authority_for_entry(&self.claim().request_entry.entry_id)
    }
    pub(crate) fn matches_command(&self, verified: &VerifiedAgentCommand) -> bool {
        self.claim().matches_verified_command(verified)
    }
    pub(crate) fn input_binding_parts(
        &self,
    ) -> (
        &super::ExecutionInputBundleV1,
        &super::ExecutionInputBundleReferenceV1,
        &super::ExecutionInputEntryV1,
    ) {
        (
            self.claim().input_bundle(),
            self.claim().input_bundle_ref(),
            self.claim().request_entry(),
        )
    }
    // This private copy transfers a bounded frozen claim only after ACK. The
    // consumed source authority is dropped and erases its original fence.
    fn claim_for_ack_transfer(&self) -> AcceptedAgentClaim {
        let claim = &self.claim;
        AcceptedAgentClaim {
            identity: claim.identity.clone(),
            command_binding: claim.command_binding,
            fence: claim.fence.clone(),
            lease_expires_at_unix_millis: claim.lease_expires_at_unix_millis,
            claim_started_at_unix_micros: claim.claim_started_at_unix_micros,
            claim_id: claim.claim_id.clone(),
            claim_handoff_watermark: claim.claim_handoff_watermark,
            input_bundle_ref: claim.input_bundle_ref.clone(),
            input_bundle: claim.input_bundle.clone(),
            request_entry: claim.request_entry.clone(),
            arguments_entry: claim.arguments_entry.clone(),
        }
    }
    /// The ACK reply can only be produced by the authenticated content client.
    /// Consume this recovery claim without issuing Begin/Invoke authority.
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(crate) async fn seal_resumption(
        self,
        reply: crate::transport::input_content::NodeRecoveryAckReply,
        applied: crate::agents::graph::node_recovery_runtime::AppliedNodeRecoveryAction,
        request: crate::agents::graph::node_recovery::OperatorRetryRequest,
        visit: crate::agents::node_recovery_checkpoint::OpenedNodeRecoveryVisit,
    ) -> Result<NodeRecoveryAckAuthorization, ControlSemanticError> {
        if !visit.matches_receipt(&self.receipt)
            || !visit
                .checkpoint()
                .matches_execution(self.execution_id(), self.generation())
        {
            return Err(ControlSemanticError::AuthorizationFailed(
                "the inspected visit differs from its recovery claim",
            ));
        }
        if let Some(reason) = applied.terminal_stop_reason() {
            let decision = reply.terminal_authorization.as_ref().ok_or(
                ControlSemanticError::AuthorizationFailed(
                    "the stopped journal lacks terminal authority",
                ),
            )?;
            if !applied.matches(request)
                || reply.recovery_resume_authorized
                || reply.resumption.is_some()
                || !reply.terminal_settlement_authorized
                || decision.schema != "elitea.pipeline.node-recovery-terminal-settlement.v1"
                || decision.execution_id != self.execution_id()
                || decision.generation != self.generation()
                || decision.request_id != reply.request_id
                || decision.activation_id != self.receipt.activation_id
                || decision.journal_revision != applied.applied_revision()
                || decision.journal_revision != reply.applied_revision
                || decision.claim_id != self.claim_id()
                || decision.stop_reason != reason
                || decision.receipt_sha256
                    != crate::transport::input_content::node_recovery_receipt_sha256(&self.receipt)
                        .map_err(|_| {
                            ControlSemanticError::AuthorizationFailed(
                                "the stopped receipt hash differs",
                            )
                        })?
            {
                return Err(ControlSemanticError::AuthorizationFailed(
                    "the terminal-only decision differs from its stopped journal",
                ));
            }
            visit
                .journal
                .verify_applied_revision(applied.applied_revision())
                .await
                .map_err(|_| {
                    ControlSemanticError::AuthorizationFailed(
                        "the stopped journal changed after ACK",
                    )
                })?;
            return Ok(NodeRecoveryAckAuthorization::Terminal(
                NodeRecoveryTerminalAuthorization {
                    claim: self.claim_for_ack_transfer(),
                },
            ));
        }
        let decision =
            reply
                .resumption
                .as_ref()
                .ok_or(ControlSemanticError::AuthorizationFailed(
                    "the recovery restore decision is absent",
                ))?;
        let reference = self.claim().input_bundle_ref();
        let digest = reference
            .digest
            .as_ref()
            .ok_or(ControlSemanticError::AuthorizationFailed(
                "the recovery manifest digest is absent",
            ))?;
        let expected_digest = hex(&digest.value);
        if !visit.matches_receipt(&self.receipt)
            || !visit
                .checkpoint()
                .matches_execution(self.execution_id(), self.generation())
            || !applied.matches(request)
            || !reply.recovery_resume_authorized
            || decision.schema != "elitea.pipeline.node-recovery-resumption.v1"
            || decision.execution_id != self.execution_id()
            || decision.generation != self.generation()
            || decision.request_id != reply.request_id
            || decision.activation_id != self.receipt.activation_id
            || decision.journal_revision != applied.applied_revision()
            || decision.journal_revision != reply.applied_revision
            || decision.input_bundle_id != reference.input_bundle_id
            || decision.input_manifest_sha256 != expected_digest
            || decision.receipt_sha256
                != crate::transport::input_content::node_recovery_receipt_sha256(&self.receipt)
                    .map_err(|_| {
                        ControlSemanticError::AuthorizationFailed(
                            "the recovery receipt digest is invalid",
                        )
                    })?
            || decision.claim_id != self.claim_id()
            || decision.failure_route_continuation.as_ref() != applied.failure_route_continuation()
        {
            return Err(ControlSemanticError::AuthorizationFailed(
                "the recovery restore decision differs from its durable visit",
            ));
        }
        visit
            .journal
            .verify_applied_revision(applied.applied_revision())
            .await
            .map_err(|_| {
                ControlSemanticError::AuthorizationFailed("the recovery journal changed after ACK")
            })?;
        Ok(NodeRecoveryAckAuthorization::Restore(
            NodeRecoveryResumption {
                claim: self.claim_for_ack_transfer(),
                receipt: self.receipt.clone(),
                journal_revision: applied.applied_revision(),
                checkpoint: visit.into_checkpoint(),
            },
        ))
    }
}
impl Drop for NodeRecoveryControlAuthority {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.claim.fence.fence_token.zeroize();
    }
}
fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut result = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        let _ = write!(result, "{byte:02x}");
    }
    result
}

/// One-use authority for restoring an inspected checkpoint, never fresh input.
/// Its claim and effect-sensitive selectors cannot be extracted by graph code.
pub(crate) struct NodeRecoveryResumption {
    claim: AcceptedAgentClaim,
    receipt: NodeRecoveryRequiredReceipt,
    journal_revision: u64,
    checkpoint: crate::agents::session::ValidatedModelCheckpoint,
}
#[allow(
    clippy::large_enum_variant,
    reason = "Keep bounded authenticated owner records inline without changing their contracts."
)]
pub(crate) enum NodeRecoveryAckAuthorization {
    Restore(NodeRecoveryResumption),
    Terminal(NodeRecoveryTerminalAuthorization),
}
pub(crate) struct NodeRecoveryTerminalAuthorization {
    claim: AcceptedAgentClaim,
}
impl NodeRecoveryTerminalAuthorization {
    pub(crate) fn into_output_authority(self) -> super::AgentExecutionOutputAuthority {
        super::AgentExecutionOutputAuthority { claim: self.claim }
    }
}
impl NodeRecoveryResumption {
    pub(crate) fn into_lifecycle_parts(
        self,
    ) -> (
        NodeRecoverySubmissionPermit,
        super::AgentExecutionOutputAuthority,
        super::ClaimBoundRuntimeContextAuthority,
        ClaimBoundSessionAuthority,
        NodeRecoveryAssemblyAuthorization,
    ) {
        let session = ClaimBoundSessionAuthority::from_claim(&self.claim);
        let runtime = super::ClaimBoundRuntimeContextAuthority::from_claim(&self.claim);
        (
            NodeRecoverySubmissionPermit { _sealed: () },
            super::AgentExecutionOutputAuthority { claim: self.claim },
            runtime,
            session,
            NodeRecoveryAssemblyAuthorization {
                checkpoint: self.checkpoint,
                receipt: self.receipt,
                journal_revision: self.journal_revision,
            },
        )
    }
}

/// Only the dedicated restored-checkpoint start path can consume this token.
pub(crate) struct NodeRecoverySubmissionPermit {
    _sealed: (),
}
pub(crate) struct NodeRecoveryAssemblyAuthorization {
    checkpoint: crate::agents::session::ValidatedModelCheckpoint,
    receipt: NodeRecoveryRequiredReceipt,
    journal_revision: u64,
}
impl NodeRecoveryAssemblyAuthorization {
    pub(crate) fn matches(
        &self,
        checkpoint: &crate::agents::session::ValidatedModelCheckpoint,
    ) -> bool {
        self.checkpoint.matches_checkpoint(checkpoint)
    }
    pub(crate) fn receipt(&self) -> &NodeRecoveryRequiredReceipt {
        &self.receipt
    }
    pub(crate) fn journal_revision(&self) -> u64 {
        self.journal_revision
    }
}

/// Keeps one claim alive for control polling and exact journal CAS only.
/// This probe is never passed to ordinary session/graph assembly.
pub(crate) struct NodeRecoveryJournalLease {
    live: std::sync::atomic::AtomicBool,
    expiry: std::sync::atomic::AtomicI64,
    clock: Arc<dyn crate::execution::agent_lease::UnixMillisClock>,
}
impl NodeRecoveryJournalLease {
    pub(crate) fn new(
        lease: &ClaimLeaseHandle,
        clock: Arc<dyn crate::execution::agent_lease::UnixMillisClock>,
    ) -> Self {
        Self {
            live: std::sync::atomic::AtomicBool::new(true),
            expiry: std::sync::atomic::AtomicI64::new(lease.lease_expires_at_unix_millis()),
            clock,
        }
    }
    pub(crate) fn revoke(&self) {
        self.live.store(false, std::sync::atomic::Ordering::Release);
    }
    pub(crate) async fn renew<R: crate::transport::ControlRpc>(
        &self,
        client: &AgentControlClient<R>,
        lease: &mut ClaimLeaseHandle,
    ) -> Result<(), AgentControlError> {
        let result = async {
            let renewed = client.renew_lease(lease).await?;
            let desired = client.observe_lease(lease).await?;
            if !matches!(
                renewed.desired_state,
                super::DesiredExecutionState::Suspended | super::DesiredExecutionState::Running
            ) || !matches!(
                desired,
                super::DesiredExecutionState::Suspended | super::DesiredExecutionState::Running
            ) || renewed
                .lease_expires_at_unix_millis
                .checked_sub(self.clock.now_unix_millis())
                .is_none_or(|ms| ms < 2000)
            {
                return Err(AgentControlError::Semantic(
                    ControlSemanticError::AuthorizationFailed(
                        "node recovery lease is no longer current",
                    ),
                ));
            }
            lease.commit_renewal(renewed);
            self.expiry.store(
                renewed.lease_expires_at_unix_millis,
                std::sync::atomic::Ordering::Release,
            );
            Ok(())
        }
        .await;
        if result.is_err() {
            self.revoke();
        }
        result
    }
}
impl crate::state::StateWriterLease for NodeRecoveryJournalLease {
    fn ensure_current(&self) -> Result<(), crate::state::StateWriterLeaseLost> {
        use std::sync::atomic::Ordering;
        if !self.live.load(Ordering::Acquire)
            || self.clock.now_unix_millis() <= 0
            || self
                .expiry
                .load(Ordering::Acquire)
                .checked_sub(self.clock.now_unix_millis())
                .is_none_or(|ms| ms < 2000)
        {
            return Err(crate::state::StateWriterLeaseLost);
        }
        Ok(())
    }
}

#[cfg(test)]
pub(crate) fn test_node_recovery_control_authority(
    receipt: NodeRecoveryRequiredReceipt,
) -> NodeRecoveryControlAuthority {
    NodeRecoveryControlAuthority {
        claim: super::test_lease_monitored_input_execution(32, [5; 32]).claim,
        receipt,
    }
}

#[cfg(test)]
mod tests {
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
    fn node_receipt() -> Vec<u8> {
        include_bytes!("../agents/graph/node_recovery_required_v1.fixture.json").to_vec()
    }
    #[test]
    fn suspended_node_claim_is_inspection_only_under_its_exact_current_fence() {
        for case in 0..6 {
            let verified = parse_and_verify_agent_command(
                &vector("signed_command"),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .expect("command");
            let mut response =
                ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("claim");
            let receipt = response.receipt.as_mut().expect("receipt");
            let fence = receipt.fence.clone().expect("fence");
            let now = receipt.lease_expires_at_unix_millis - 1000;
            receipt.disposition = ClaimDispositionV1::RecoverNodeVisit as i32;
            receipt.desired_state = DesiredExecutionStateV1::Suspended as i32;
            receipt.node_recovery_receipt_json = node_receipt();
            match case {
                1 => receipt.desired_state = DesiredExecutionStateV1::Running as i32,
                2 => receipt.desired_state = DesiredExecutionStateV1::Cancelled as i32,
                3 => {
                    receipt.fence.as_mut().expect("fence").producer_id =
                        "replacement-not-current".into();
                }
                4 => receipt.node_recovery_receipt_json.clear(),
                5 => receipt.node_recovery_receipt_json = br#"{"schema":"other"}"#.to_vec(),
                _ => {}
            }
            assert!(
                super::super::parse_accepted_agent_claim(
                    &verified,
                    response.clone(),
                    &fence.workload_session_id,
                    &fence.producer_id,
                    now
                )
                .is_err()
            );
            assert!(
                super::super::parse_agent_claim_decision(
                    &verified,
                    response.clone(),
                    &fence.workload_session_id,
                    &fence.producer_id,
                    now
                )
                .is_err()
            );
            let parsed = NodeRecoveryInspection::parse(
                &verified,
                response,
                &fence.workload_session_id,
                &fence.producer_id,
                now,
            );
            assert_eq!(parsed.is_ok(), case <= 1, "case {case}");
            if let Ok(inspection) = parsed {
                let (authority, lease, _session) = inspection.into_recovery_supervision();
                assert!(authority.matches_command(&verified));
                assert_eq!(authority.claim_id(), lease.claim_id);
                assert_eq!(authority.fence_bytes(), lease.fence.fence_token);
            }
        }
    }
    #[test]
    fn ordinary_claim_rejects_even_valid_recovery_receipt_metadata() {
        let verified = parse_and_verify_agent_command(
            &vector("signed_command"),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .expect("command");
        let mut response =
            ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("claim");
        let receipt = response.receipt.as_mut().expect("receipt");
        let fence = receipt.fence.clone().expect("fence");
        let now = receipt.lease_expires_at_unix_millis - 1000;
        receipt.node_recovery_receipt_json = node_receipt();
        assert!(
            super::super::parse_accepted_agent_claim(
                &verified,
                response,
                &fence.workload_session_id,
                &fence.producer_id,
                now
            )
            .is_err()
        );
    }
    struct ClaimRpc {
        response: ClaimCommandResponseV1,
        calls: Arc<std::sync::atomic::AtomicUsize>,
    }
    #[async_trait::async_trait]
    impl crate::transport::ControlRpc for ClaimRpc {
        async fn claim_command(
            &self,
            request: tonic::Request<super::super::ClaimCommandRequestV1>,
        ) -> Result<tonic::Response<ClaimCommandResponseV1>, tonic::Status> {
            assert!(request.get_ref().node_recovery);
            assert!(
                !request.get_ref().agent_model_checkpoint_recovery,
                "mutually exclusive Main opt-ins"
            );
            self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Ok(tonic::Response::new(self.response.clone()))
        }
        async fn begin_execution(
            &self,
            _: tonic::Request<super::super::BeginExecutionRequestV1>,
        ) -> Result<tonic::Response<super::super::BeginExecutionResponseV1>, tonic::Status>
        {
            panic!("ordinary Begin is forbidden")
        }
        async fn authorize_invocation(
            &self,
            _: tonic::Request<super::super::AuthorizeInvocationRequestV1>,
        ) -> Result<tonic::Response<super::super::AuthorizeInvocationResponseV1>, tonic::Status>
        {
            panic!("ordinary Invoke is forbidden")
        }
        async fn renew_lease(
            &self,
            _: tonic::Request<super::super::RenewLeaseRequestV1>,
        ) -> Result<tonic::Response<super::super::RenewLeaseResponseV1>, tonic::Status> {
            panic!("unexpected renewal")
        }
        async fn observe_desired_state(
            &self,
            _: tonic::Request<super::super::ObserveDesiredStateRequestV1>,
        ) -> Result<tonic::Response<super::super::ObserveDesiredStateResponseV1>, tonic::Status>
        {
            panic!("unexpected observation")
        }
        async fn prepare_settlement(
            &self,
            _: tonic::Request<super::super::PrepareSettlementRequestV1>,
        ) -> Result<tonic::Response<super::super::PrepareSettlementResponseV1>, tonic::Status>
        {
            panic!("unexpected settlement")
        }
    }
    #[tokio::test]
    async fn actual_node_claim_rpc_uses_exclusive_opt_in_and_routes_lost_ack_running_inspection() {
        for case in 0..4 {
            let verified = parse_and_verify_agent_command(
                &vector("signed_command"),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .expect("command");
            let mut response =
                ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("claim");
            let receipt = response.receipt.as_mut().expect("receipt");
            let fence = receipt.fence.clone().expect("fence");
            let now = receipt.lease_expires_at_unix_millis - 1000;
            if matches!(case, 1 | 2) {
                receipt.disposition = ClaimDispositionV1::RecoverNodeVisit as i32;
                receipt.desired_state = if case == 1 {
                    DesiredExecutionStateV1::Suspended
                } else {
                    DesiredExecutionStateV1::Running
                } as i32;
                receipt.node_recovery_receipt_json = node_receipt();
            } else if case == 3 {
                receipt.disposition = ClaimDispositionV1::RecoverAgentModelCheckpoint as i32;
            }
            let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let client = AgentControlClient::new(
                ClaimRpc {
                    response,
                    calls: calls.clone(),
                },
                crate::transport::ControlGrpcConfig {
                    deadline: std::time::Duration::from_secs(1),
                    workload_session_id: fence.workload_session_id,
                    producer_id: fence.producer_id,
                },
            )
            .expect("client");
            let decision = client
                .claim_node_recovery_delivery(&verified, now)
                .await
                .expect("decision");
            match decision {
                NodeRecoveryClaimDecision::Ordinary(_) => assert_eq!(case, 0),
                NodeRecoveryClaimDecision::Recover(_) => assert!(matches!(case, 1 | 2)),
                NodeRecoveryClaimDecision::ModelInspection(_) => assert_eq!(case, 3),
            }
            assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 1);
        }
    }
}
