//! One-attempt mTLS node recovery control. No redirects or business dispatch.

use super::{
    Body, CLAIM_HEADER, CONTENT_LENGTH, CONTENT_TYPE, Duration, FENCE_HEADER, InputContentClient,
    InputContentError, Method, PATH_SEGMENT, Request, StatusCode, Version, timeout,
    utf8_percent_encode,
};
use crate::agents::graph::node_recovery_receipt::{
    NodeRecoveryAction, NodeRecoveryRequiredReceipt,
};
use crate::protocol::control::NodeRecoveryControlAuthority;
use base64::Engine as _;
use http_body_util::BodyExt as _;
use serde::{Deserialize, Serialize};

const MAX_CONTROL_BYTES: usize = 16 * 1024;

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryControlReply {
    schema: String,
    execution_id: String,
    generation: u64,
    desired_state: String,
    pub(crate) receipt: NodeRecoveryRequiredReceipt,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) action: Option<NodeRecoveryControlAction>,
}
#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryControlAction {
    pub(crate) request_id: String,
    pub(crate) activation_id: String,
    pub(crate) expected_revision: u64,
    pub(crate) last_attempt: u16,
    pub(crate) action: NodeRecoveryAction,
    pub(crate) receipt_sha256: String,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) owner_proof:
        Option<crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProof>,
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryAckRequest<'a> {
    pub(crate) request_id: &'a str,
    pub(crate) activation_id: &'a str,
    pub(crate) expected_revision: u64,
    pub(crate) receipt_sha256: &'a str,
    pub(crate) applied_revision: u64,
    pub(crate) continuation_receipt: Option<&'a NodeRecoveryRequiredReceipt>,
    pub(crate) terminal_stop_reason: Option<crate::agents::graph::node_recovery::StopReason>,
    pub(crate) failure_route_continuation: Option<
        &'a crate::agents::graph::node_recovery_runtime::NodeRecoveryFailureRouteContinuation,
    >,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryAckReply {
    schema: String,
    execution_id: String,
    generation: u64,
    pub(crate) request_id: String,
    pub(crate) applied_revision: u64,
    // Replay alone grants no local authority; the exact journal CAS is rechecked.
    #[serde(rename = "replay")]
    _replay: bool,
    pub(crate) recovery_resume_authorized: bool,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) resumption: Option<NodeRecoveryResumptionReply>,
    pub(crate) terminal_settlement_authorized: bool,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) terminal_authorization: Option<NodeRecoveryTerminalReply>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryTerminalReply {
    pub(crate) schema: String,
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
    pub(crate) request_id: String,
    pub(crate) activation_id: String,
    pub(crate) journal_revision: u64,
    pub(crate) receipt_sha256: String,
    pub(crate) claim_id: String,
    pub(crate) stop_reason: crate::agents::graph::node_recovery::StopReason,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryResumptionReply {
    pub(crate) schema: String,
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
    pub(crate) request_id: String,
    pub(crate) activation_id: String,
    pub(crate) journal_revision: u64,
    pub(crate) input_bundle_id: String,
    pub(crate) input_manifest_sha256: String,
    pub(crate) receipt_sha256: String,
    pub(crate) claim_id: String,
    #[serde(deserialize_with = "required_nullable")]
    pub(crate) failure_route_continuation:
        Option<crate::agents::graph::node_recovery_runtime::NodeRecoveryFailureRouteContinuation>,
}

fn required_nullable<'de, D, T>(deserializer: D) -> Result<Option<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(deserializer)
}

impl InputContentClient {
    /// Apply one authenticated Retry action through the journal CAS before ACK.
    /// Failed/uncertain ACK retains the durable action for a replacement claim.
    pub(crate) async fn apply_node_recovery_action(
        &self,
        authority: &NodeRecoveryControlAuthority,
        journal: &crate::agents::graph::node_recovery_runtime::NodeAttemptJournal,
        activation: &crate::agents::graph::node_recovery_runtime::NodeAttemptActivation,
        context: &adk_rust::graph::NodeContext,
        projector: Option<&dyn crate::agents::graph::node_recovery_runtime::NodeResultRecovery>,
        now_ms: u64,
    ) -> Result<
        Option<(
            crate::agents::graph::node_recovery_runtime::AppliedNodeRecoveryAction,
            NodeRecoveryAckReply,
            crate::agents::graph::node_recovery::OperatorRetryRequest,
        )>,
        InputContentError,
    > {
        let poll = self.poll_node_recovery(authority).await?;
        let Some(action) = poll.action else {
            return Ok(None);
        };
        let request = operator_request(&action)?;
        let applied = match action.action {
            NodeRecoveryAction::Retry => {
                let authorizer = AuthenticatedRetry {
                    client: self,
                    authority,
                    action: &action,
                };
                journal
                    .resume_operator_retry(activation, request, &authorizer, now_ms)
                    .await
                    .map_err(|_| invalid())?
            }
            NodeRecoveryAction::Reconcile | NodeRecoveryAction::ResumeResult => {
                let proof = action.owner_proof.as_ref().ok_or_else(invalid)?;
                let projector = projector.ok_or(InputContentError::AuthorizationFailed(
                    "the owning result projector is unavailable",
                ))?;
                let wire = self.fetch_node_recovery_result(authority, proof).await?;
                let authorizer = AuthenticatedOwnerResult {
                    client: self,
                    authority,
                    action: &action,
                };
                match proof.kind {
                    crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::CommittedResult => journal.resume_owner_result(
                        activation,
                        context,
                        request,
                        proof,
                        &wire,
                        projector,
                        &authorizer,
                        now_ms,
                    )
                    .await
                    .map_err(|_| invalid())?,
                    crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::VerifiedNoEffect => {
                        if action.action != NodeRecoveryAction::Reconcile { return Err(invalid()); }
                        journal.reconcile_verified_no_effect(activation,context,request,proof,&wire,projector,&authorizer,now_ms).await.map_err(|_| invalid())?
                    }
                }
            }
        };
        if !applied.matches(request) {
            return Err(invalid());
        }
        let reply = self
            .ack_node_recovery(
                authority,
                &NodeRecoveryAckRequest {
                    request_id: &action.request_id,
                    activation_id: &action.activation_id,
                    expected_revision: action.expected_revision,
                    receipt_sha256: &action.receipt_sha256,
                    applied_revision: applied.applied_revision(),
                    continuation_receipt: applied.continuation_receipt(),
                    terminal_stop_reason: applied.terminal_stop_reason(),
                    failure_route_continuation: applied.failure_route_continuation(),
                },
            )
            .await?;
        Ok(Some((applied, reply, request)))
    }
    async fn fetch_node_recovery_result(
        &self,
        authority: &NodeRecoveryControlAuthority,
        proof: &crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<Vec<u8>, InputContentError> {
        if !proof.validates(
            authority.execution_id(),
            authority.generation(),
            authority.receipt(),
        ) {
            return Err(invalid());
        }
        let reference = match proof.kind {
            crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::CommittedResult => proof.result_ref.as_ref(),
            crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::VerifiedNoEffect => proof.owner_receipt_ref.as_ref(),
        }.ok_or_else(invalid)?;
        let operation = format!(
            "results/{}/versions/{}",
            utf8_percent_encode(&reference.content_id, PATH_SEGMENT),
            utf8_percent_encode(&reference.immutable_version, PATH_SEGMENT)
        );
        let bytes = self
            .node_recovery_post_with_reference(authority, &operation, Vec::new(), Some(reference))
            .await?;
        if !match proof.kind {
            crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::CommittedResult => proof.matches_result_bytes(&bytes),
            crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::VerifiedNoEffect => proof.matches_owner_receipt_bytes(&bytes),
        } {
            return Err(invalid());
        }
        Ok(bytes)
    }
    pub(crate) async fn poll_node_recovery(
        &self,
        authority: &NodeRecoveryControlAuthority,
    ) -> Result<NodeRecoveryControlReply, InputContentError> {
        let bytes = self
            .node_recovery_post(authority, "control", Vec::new())
            .await?;
        let reply: NodeRecoveryControlReply =
            serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        if reply.schema != "elitea.pipeline.node-recovery-control.v1"
            || reply.execution_id != authority.execution_id()
            || reply.generation != authority.generation()
            || !matches!(reply.desired_state.as_str(), "SUSPENDED" | "RUNNING")
            || !reply.receipt.validate()
            || reply.receipt != *authority.receipt()
        {
            return Err(invalid());
        }
        if let Some(action) = &reply.action {
            if !lower_hex(&action.request_id, 32)
                || action.activation_id != reply.receipt.activation_id
                || action.expected_revision != reply.receipt.journal_revision
                || action.last_attempt != reply.receipt.attempt
                || !reply.receipt.allowed_actions.contains(&action.action)
                || action.receipt_sha256 != receipt_sha256(&reply.receipt)?
                || (action.action == NodeRecoveryAction::Retry && action.owner_proof.is_some())
                || (action.action != NodeRecoveryAction::Retry && action.owner_proof.is_none())
            {
                return Err(invalid());
            }
            if action.owner_proof.as_ref().is_some_and(|proof| {
                !proof.validates(
                    authority.execution_id(),
                    authority.generation(),
                    &reply.receipt,
                )
            }) {
                return Err(invalid());
            }
        }
        Ok(reply)
    }
    pub(crate) async fn ack_node_recovery(
        &self,
        authority: &NodeRecoveryControlAuthority,
        request: &NodeRecoveryAckRequest<'_>,
    ) -> Result<NodeRecoveryAckReply, InputContentError> {
        if !lower_hex(request.request_id, 32)
            || request.activation_id != authority.receipt().activation_id
            || request.expected_revision != authority.receipt().journal_revision
            || request.expected_revision.checked_add(1) != Some(request.applied_revision)
            || request.receipt_sha256 != receipt_sha256(authority.receipt())?
            || !valid_ack_branch(request, authority.receipt())
        {
            return Err(invalid());
        }
        let payload = serde_json::to_vec(request).map_err(|_| invalid())?;
        let bytes = self.node_recovery_post(authority, "ack", payload).await?;
        let reply: NodeRecoveryAckReply = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        if reply.schema != "elitea.pipeline.node-recovery-ack.v1"
            || reply.execution_id != authority.execution_id()
            || reply.generation != authority.generation()
            || reply.request_id != request.request_id
            || reply.applied_revision != request.applied_revision
            || reply.resumption.as_ref().is_some_and(|resumption| {
                resumption.failure_route_continuation.as_ref() != request.failure_route_continuation
            })
            || request.continuation_receipt.is_some()
                && (request.terminal_stop_reason.is_some()
                    || request.failure_route_continuation.is_some())
            || request.failure_route_continuation.is_some()
                && request.terminal_stop_reason.is_some()
            || request.continuation_receipt.is_some()
                && (reply.recovery_resume_authorized
                    || reply.resumption.is_some()
                    || reply.terminal_settlement_authorized
                    || reply.terminal_authorization.is_some())
            || match request.terminal_stop_reason {
                None if request.continuation_receipt.is_none() => {
                    !reply.recovery_resume_authorized
                        || reply.resumption.is_none()
                        || reply.terminal_settlement_authorized
                        || reply.terminal_authorization.is_some()
                }
                Some(reason) => {
                    reply.recovery_resume_authorized
                        || reply.resumption.is_some()
                        || !reply.terminal_settlement_authorized
                        || reply
                            .terminal_authorization
                            .as_ref()
                            .is_none_or(|terminal| terminal.stop_reason != reason)
                }
                None => false,
            }
        {
            return Err(invalid());
        }
        Ok(reply)
    }
    async fn node_recovery_post(
        &self,
        authority: &NodeRecoveryControlAuthority,
        operation: &str,
        payload: Vec<u8>,
    ) -> Result<Vec<u8>, InputContentError> {
        self.node_recovery_post_with_reference(authority, operation, payload, None)
            .await
    }
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    async fn node_recovery_post_with_reference(
        &self,
        authority: &NodeRecoveryControlAuthority,
        operation: &str,
        payload: Vec<u8>,
        reference: Option<&crate::agents::graph::node_recovery_owner::NodeRecoveryResultReference>,
    ) -> Result<Vec<u8>, InputContentError> {
        if payload.len() > 8192 {
            return Err(InputContentError::ResourceExhausted(
                "the recovery control request exceeds its bound",
            ));
        }
        let uri = format!(
            "/executions/{}/generations/{}/node-recovery/{operation}",
            utf8_percent_encode(authority.execution_id(), PATH_SEGMENT),
            authority.generation()
        );
        let request = Request::builder()
            .method(Method::POST)
            .version(Version::HTTP_2)
            .uri(uri)
            .header(CLAIM_HEADER, authority.claim_id())
            .header(
                FENCE_HEADER,
                base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(authority.fence_bytes()),
            )
            .header(CONTENT_TYPE, "application/json")
            .header(CONTENT_LENGTH, payload.len())
            .body(Body::new(http_body_util::Full::new(bytes::Bytes::from(
                payload,
            ))))
            .map_err(|_| invalid())?;
        timeout(self.config.deadline.min(Duration::from_secs(10)), async {
            let mut response = self
                .rpc
                .get(request)
                .await
                .map_err(InputContentError::Transport)?;
            if matches!(
                response.status(),
                StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN | StatusCode::CONFLICT
            ) {
                return Err(InputContentError::AuthorizationFailed(
                    "the recovery control authority is no longer current",
                ));
            }
            if response.status() != StatusCode::OK
                || response.version() != Version::HTTP_2
                || response
                    .headers()
                    .get(CONTENT_TYPE)
                    .and_then(|h| h.to_str().ok())
                    .is_none_or(|h| h.split(';').next() != Some("application/json"))
            {
                return Err(invalid());
            }
            if let Some(reference) = reference
                && (response
                    .headers()
                    .get(CONTENT_LENGTH)
                    .and_then(|h| h.to_str().ok())
                    .and_then(|h| h.parse::<u64>().ok())
                    != Some(reference.byte_length)
                    || response
                        .headers()
                        .get("X-Elitea-Content-SHA256")
                        .and_then(|h| h.to_str().ok())
                        != Some(reference.digest_sha256.as_str()))
            {
                return Err(invalid());
            }
            let limit = reference.map_or(Ok(MAX_CONTROL_BYTES), |reference| {
                usize::try_from(reference.byte_length).map_err(|_| invalid())
            })?;
            let mut result = Vec::new();
            while let Some(frame) = response.body_mut().frame().await {
                let frame = frame.map_err(|_| {
                    InputContentError::DependencyUnavailable(
                        "the recovery control response was interrupted",
                    )
                })?;
                if frame.is_trailers() {
                    return Err(invalid());
                }
                if let Ok(bytes) = frame.into_data() {
                    if result
                        .len()
                        .checked_add(bytes.len())
                        .is_none_or(|n| n > limit)
                    {
                        return Err(InputContentError::ResourceExhausted(
                            "the recovery control response exceeds its bound",
                        ));
                    }
                    result.extend_from_slice(&bytes);
                }
            }
            if result.is_empty()
                || reference.is_some_and(|reference| reference.byte_length != result.len() as u64)
            {
                return Err(invalid());
            }
            Ok(result)
        })
        .await
        .map_err(|_| InputContentError::Timeout("the recovery control request timed out"))?
    }
}

fn valid_ack_branch(
    request: &NodeRecoveryAckRequest<'_>,
    original: &NodeRecoveryRequiredReceipt,
) -> bool {
    use crate::agents::graph::node_recovery::{ReplaySafety, StopReason};
    let selected = usize::from(request.continuation_receipt.is_some())
        + usize::from(request.terminal_stop_reason.is_some())
        + usize::from(request.failure_route_continuation.is_some());
    if selected > 1 {
        return false;
    }
    let terminal_reason = |reason| {
        matches!(
            reason,
            StopReason::AttemptsExhausted
                | StopReason::ElapsedLimit
                | StopReason::NotRetryable
                | StopReason::RetryDisabled
        )
    };
    if request
        .terminal_stop_reason
        .is_some_and(|reason| !terminal_reason(reason))
    {
        return false;
    }
    if let Some(next) = request.continuation_receipt
        && (!next.validate()
            || next.activation_id != original.activation_id
            || next.node_id != original.node_id
            || next.graph_thread != original.graph_thread
            || next.step != original.step
            || next.attempt != original.attempt
            || next.failure_class != original.failure_class
            || next.journal_revision != request.applied_revision
            || next.replay_safety != ReplaySafety::NoExternalEffect
            || next.stop_reason != StopReason::OperatorApprovalRequired)
    {
        return false;
    }
    request.failure_route_continuation.is_none_or(|route| {
        route.schema == "elitea.pipeline.node-recovery-failure-route.v1"
            && crate::agents::graph::yaml::valid_graph_id(&route.route_id)
            && route.failed.activation_id == original.activation_id
            && route.failed.attempt == original.attempt
            && route.failed.failure_class == original.failure_class
            && terminal_reason(route.failed.stop_reason)
    })
}

struct AuthenticatedOwnerResult<'a> {
    client: &'a InputContentClient,
    authority: &'a NodeRecoveryControlAuthority,
    action: &'a NodeRecoveryControlAction,
}
#[async_trait::async_trait]
impl crate::agents::graph::node_recovery_runtime::NodeRecoveryOwnerAuthorizer
    for AuthenticatedOwnerResult<'_>
{
    async fn authorize_verified_no_effect(
        &self,
        receipt: &NodeRecoveryRequiredReceipt,
        request: &crate::agents::graph::node_recovery::OperatorRetryRequest,
        proof: &crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<(), adk_rust::graph::GraphError> {
        if self.action.action != NodeRecoveryAction::Reconcile || proof.kind != crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProofKind::VerifiedNoEffect {
            return Err(crate::agents::graph::node_recovery_runtime::recovery_error("pipeline.node_recovery.no_effect_denied"));
        }
        self.authorize_committed_result(receipt, request, proof)
            .await
    }
    async fn authorize_committed_result(
        &self,
        receipt: &NodeRecoveryRequiredReceipt,
        request: &crate::agents::graph::node_recovery::OperatorRetryRequest,
        proof: &crate::agents::graph::node_recovery_owner::NodeRecoveryOwnerProof,
    ) -> Result<(), adk_rust::graph::GraphError> {
        let reject = || {
            crate::agents::graph::node_recovery_runtime::recovery_error(
                "pipeline.node_recovery.owner_authority_lost",
            )
        };
        if receipt != self.authority.receipt()
            || !proof.validates(
                self.authority.execution_id(),
                self.authority.generation(),
                receipt,
            )
            || operator_request(self.action).map_err(|_| reject())? != *request
            || self.action.owner_proof.as_ref() != Some(proof)
        {
            return Err(reject());
        }
        let polled = self
            .client
            .poll_node_recovery(self.authority)
            .await
            .map_err(|_| reject())?;
        let action = polled.action.ok_or_else(reject)?;
        if !matches!(
            action.action,
            NodeRecoveryAction::Reconcile | NodeRecoveryAction::ResumeResult
        ) || action.action != self.action.action
            || action.request_id != self.action.request_id
            || action.receipt_sha256 != self.action.receipt_sha256
            || action.owner_proof.as_ref() != Some(proof)
            || operator_request(&action).map_err(|_| reject())? != *request
        {
            return Err(reject());
        }
        Ok(())
    }
}
struct AuthenticatedRetry<'a> {
    client: &'a InputContentClient,
    authority: &'a NodeRecoveryControlAuthority,
    action: &'a NodeRecoveryControlAction,
}
#[async_trait::async_trait]
impl crate::agents::graph::node_recovery_runtime::NodeRecoveryOperatorAuthorizer
    for AuthenticatedRetry<'_>
{
    async fn authorize_retry(
        &self,
        receipt: &NodeRecoveryRequiredReceipt,
        request: &crate::agents::graph::node_recovery::OperatorRetryRequest,
    ) -> Result<(), adk_rust::graph::GraphError> {
        let reject = || {
            crate::agents::graph::node_recovery_runtime::recovery_error(
                "pipeline.node_recovery.operator_authority_lost",
            )
        };
        if receipt != self.authority.receipt()
            || !operator_request(self.action)
                .map_err(|_| reject())?
                .eq(request)
        {
            return Err(reject());
        }
        let polled = self
            .client
            .poll_node_recovery(self.authority)
            .await
            .map_err(|_| reject())?;
        let action = polled.action.ok_or_else(reject)?;
        if action.action != NodeRecoveryAction::Retry
            || action.owner_proof.is_some()
            || action.request_id != self.action.request_id
            || action.receipt_sha256 != self.action.receipt_sha256
            || operator_request(&action).map_err(|_| reject())? != *request
        {
            return Err(reject());
        }
        Ok(())
    }
}
fn operator_request(
    action: &NodeRecoveryControlAction,
) -> Result<crate::agents::graph::node_recovery::OperatorRetryRequest, InputContentError> {
    if !lower_hex(&action.request_id, 32) || !lower_hex(&action.activation_id, 64) {
        return Err(invalid());
    }
    let mut activation_id = [0; 32];
    for (index, byte) in activation_id.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&action.activation_id[index * 2..index * 2 + 2], 16)
            .map_err(|_| invalid())?;
    }
    let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
    hash.update(b"elitea.pipeline.node-recovery-operator-request.v1\0");
    hash.update(action.request_id.as_bytes());
    hash.update(&activation_id);
    hash.update(&action.expected_revision.to_be_bytes());
    hash.update(match action.action {
        NodeRecoveryAction::Retry => b"retry",
        NodeRecoveryAction::Reconcile => b"reconcile",
        NodeRecoveryAction::ResumeResult => b"resume_result",
    });
    Ok(crate::agents::graph::node_recovery::OperatorRetryRequest {
        request_id: hash.finish().as_ref().try_into().map_err(|_| invalid())?,
        activation_id,
        expected_revision: action.expected_revision,
    })
}

#[allow(
    clippy::items_after_statements,
    reason = "Keep local protocol types beside their exact validation checks."
)]
pub(crate) fn receipt_sha256(
    receipt: &NodeRecoveryRequiredReceipt,
) -> Result<String, InputContentError> {
    let value = serde_json::to_value(receipt).map_err(|_| invalid())?;
    let bytes = serde_json::to_vec(&value).map_err(|_| invalid())?;
    use std::fmt::Write;
    let mut out = String::with_capacity(64);
    for byte in ring::digest::digest(&ring::digest::SHA256, &bytes).as_ref() {
        let _ = write!(out, "{byte:02x}");
    }
    Ok(out)
}
fn lower_hex(value: &str, length: usize) -> bool {
    value.len() == length
        && value
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        && !value.bytes().all(|c| c == b'0')
}
fn invalid() -> InputContentError {
    InputContentError::InvalidInput("the node recovery control binding is invalid")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};
    fn receipt() -> NodeRecoveryRequiredReceipt {
        serde_json::from_slice(include_bytes!(
            "../agents/graph/node_recovery_required_v1.fixture.json"
        ))
        .expect("receipt")
    }
    fn poll() -> Value {
        json!({"schema":"elitea.pipeline.node-recovery-control.v1", "execution_id":"execution/one",
            "generation":2,"desired_state":"SUSPENDED","receipt":receipt(),"action":null})
    }
    #[test]
    fn control_and_ack_require_explicit_nullable_wire_fields() {
        let mut value = poll();
        assert!(serde_json::from_value::<NodeRecoveryControlReply>(value.clone()).is_ok());
        value.as_object_mut().expect("object").remove("action");
        assert!(serde_json::from_value::<NodeRecoveryControlReply>(value).is_err());
        let ack = json!({"schema":"elitea.pipeline.node-recovery-ack.v1","execution_id":"execution/one",
            "generation":2,"request_id":"1".repeat(32),"applied_revision":4,"replay":false,
            "recovery_resume_authorized":false,"resumption":null,
            "terminal_settlement_authorized":false,"terminal_authorization":null});
        assert!(serde_json::from_value::<NodeRecoveryAckReply>(ack.clone()).is_ok());
        for field in ["resumption", "terminal_authorization"] {
            let mut missing = ack.clone();
            missing.as_object_mut().expect("object").remove(field);
            assert!(
                serde_json::from_value::<NodeRecoveryAckReply>(missing).is_err(),
                "{field}"
            );
        }
    }
    #[test]
    fn operator_request_binds_action_kind_and_rejects_zero_or_noncanonical_selectors() {
        let mut action = NodeRecoveryControlAction {
            request_id: "1".repeat(32),
            activation_id: "2".repeat(64),
            expected_revision: 3,
            last_attempt: 1,
            action: NodeRecoveryAction::Retry,
            receipt_sha256: "3".repeat(64),
            owner_proof: None,
        };
        let retry = operator_request(&action).expect("retry");
        action.action = NodeRecoveryAction::Reconcile;
        assert_ne!(retry, operator_request(&action).expect("reconcile"));
        for invalid in ["0".repeat(32), "A".repeat(32), "1".repeat(31)] {
            action.request_id = invalid;
            assert!(operator_request(&action).is_err());
        }
    }
    #[test]
    fn canonical_main_receipt_digest_has_exact_cross_language_bytes() {
        assert_eq!(
            receipt_sha256(&receipt()).expect("digest"),
            "8f592df47f70a2b2f5c4bf0bd97832856cd194d139a0eeab7dc37a82ca4b1351"
        );
    }
    struct ReplyRpc {
        reply: Value,
        calls: std::sync::Arc<std::sync::atomic::AtomicUsize>,
        operation: &'static str,
        expected_ack: Option<Value>,
    }
    #[async_trait::async_trait]
    impl super::super::InputContentRpc for ReplyRpc {
        async fn get(
            &self,
            request: Request<Body>,
        ) -> Result<http::Response<Body>, super::super::InputContentTransportError> {
            self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            assert_eq!(request.method(), Method::POST);
            assert_eq!(request.version(), Version::HTTP_2);
            assert_eq!(
                request.uri().path(),
                format!(
                    "/executions/execution%2Fone/generations/2/node-recovery/{}",
                    self.operation
                )
            );
            assert_eq!(request.headers()[CLAIM_HEADER], "claim-1");
            assert_eq!(
                request.headers()[FENCE_HEADER],
                base64::engine::general_purpose::URL_SAFE_NO_PAD.encode([b'f'; 32])
            );
            let body = request
                .into_body()
                .collect()
                .await
                .expect("body")
                .to_bytes();
            if self.operation == "control" {
                assert!(body.is_empty());
            } else {
                let value: Value = serde_json::from_slice(&body).expect("ack body");
                if let Some(expected) = &self.expected_ack {
                    assert_eq!(&value, expected);
                } else {
                    assert!(
                        value
                            .get("continuation_receipt")
                            .expect("required nullable")
                            .is_null()
                    );
                    assert!(
                        value
                            .get("terminal_stop_reason")
                            .expect("required nullable")
                            .is_null()
                    );
                    assert!(
                        value
                            .get("failure_route_continuation")
                            .expect("required nullable")
                            .is_null()
                    );
                }
            }
            Ok(http::Response::builder()
                .status(200)
                .version(Version::HTTP_2)
                .header(CONTENT_TYPE, "application/json")
                .body(Body::new(http_body_util::Full::new(bytes::Bytes::from(
                    serde_json::to_vec(&self.reply).expect("json"),
                ))))
                .expect("response"))
        }
    }
    fn client(
        reply: Value,
        operation: &'static str,
        calls: std::sync::Arc<std::sync::atomic::AtomicUsize>,
    ) -> InputContentClient {
        InputContentClient::with_rpc(
            ReplyRpc {
                reply,
                calls,
                operation,
                expected_ack: None,
            },
            super::super::InputContentConfig {
                origin: "https://content.invalid".into(),
                deadline: Duration::from_secs(1),
                max_materialized_bytes: 8192,
            },
        )
        .expect("client")
    }
    #[tokio::test]
    async fn actual_control_poll_rejects_changed_visit_and_unowned_action_without_dispatch() {
        let authority = crate::protocol::control::test_node_recovery_control_authority(receipt());
        for case in 0..3 {
            let mut value = poll();
            if case == 1 {
                value["receipt"]["journal_revision"] = json!(2);
            }
            if case == 2 {
                value["action"] = json!({"request_id":"1".repeat(32),"activation_id":receipt().activation_id,
                "expected_revision":3,"last_attempt":1,"action":"retry","receipt_sha256":"3".repeat(64),"owner_proof":null});
            }
            let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let outcome = client(value, "control", calls.clone())
                .poll_node_recovery(&authority)
                .await;
            assert_eq!(outcome.is_ok(), case == 0, "case {case}");
            assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 1);
        }
    }
    #[tokio::test]
    async fn actual_ack_requires_exact_next_revision_and_exclusive_restore_authorization() {
        let authority = crate::protocol::control::test_node_recovery_control_authority(receipt());
        let request_id = "1".repeat(32);
        let sha = receipt_sha256(authority.receipt()).expect("hash");
        let valid = json!({"schema":"elitea.pipeline.node-recovery-ack.v1","execution_id":"execution/one","generation":2,
            "request_id":request_id,"applied_revision":4,"replay":false,"recovery_resume_authorized":true,
            "resumption":{"schema":"elitea.pipeline.node-recovery-resumption.v1","execution_id":"execution/one","generation":2,
                "request_id":request_id,"activation_id":receipt().activation_id,"journal_revision":4,
                "input_bundle_id":"frozen","input_manifest_sha256":"4".repeat(64),"receipt_sha256":sha,"claim_id":"claim-1","failure_route_continuation":null},
            "terminal_settlement_authorized":false,"terminal_authorization":null});
        for case in 0..3 {
            let mut reply = valid.clone();
            if case == 2 {
                reply["terminal_settlement_authorized"] = json!(true);
            }
            let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let request = NodeRecoveryAckRequest {
                request_id: &request_id,
                activation_id: &authority.receipt().activation_id,
                expected_revision: 3,
                receipt_sha256: &sha,
                applied_revision: if case == 1 { 5 } else { 4 },
                continuation_receipt: None,
                terminal_stop_reason: None,
                failure_route_continuation: None,
            };
            let result = client(reply, "ack", calls.clone())
                .ack_node_recovery(&authority, &request)
                .await;
            assert_eq!(result.is_ok(), case == 0, "case {case}");
            assert_eq!(
                calls.load(std::sync::atomic::Ordering::SeqCst),
                usize::from(case != 1)
            );
        }
    }

    #[tokio::test]
    async fn actual_no_effect_ack_retains_same_visit_suspension_and_refuses_mixed_branches_before_post()
     {
        use crate::agents::graph::node_recovery::{ReplaySafety, StopReason};
        let mut original = receipt();
        original.stop_reason = StopReason::EffectReconciliationRequired;
        original.replay_safety = ReplaySafety::UnknownExternalEffect { effect_id: [9; 32] };
        original.allowed_actions = vec![NodeRecoveryAction::Reconcile];
        assert!(original.validate());
        let authority =
            crate::protocol::control::test_node_recovery_control_authority(original.clone());
        let mut next = original.clone();
        next.journal_revision = 4;
        next.stop_reason = StopReason::OperatorApprovalRequired;
        next.replay_safety = ReplaySafety::NoExternalEffect;
        next.allowed_actions = vec![NodeRecoveryAction::Retry];
        assert!(next.validate());
        let id = "1".repeat(32);
        let sha = receipt_sha256(&original).unwrap();
        let valid = json!({"schema":"elitea.pipeline.node-recovery-ack.v1","execution_id":"execution/one",
            "generation":2,"request_id":id,"applied_revision":4,"replay":false,
            "recovery_resume_authorized":false,"resumption":null,
            "terminal_settlement_authorized":false,"terminal_authorization":null});
        for case in 0..4 {
            let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let mut continuation = next.clone();
            if case == 2 {
                continuation.graph_thread = "different-visit".into();
            }
            let request = NodeRecoveryAckRequest {
                request_id: &id,
                activation_id: &original.activation_id,
                expected_revision: 3,
                receipt_sha256: &sha,
                applied_revision: 4,
                continuation_receipt: Some(&continuation),
                terminal_stop_reason: (case == 3).then_some(StopReason::AttemptsExhausted),
                failure_route_continuation: None,
            };
            let mut reply = valid.clone();
            if case == 1 {
                reply["recovery_resume_authorized"] = json!(true);
            }
            let client = InputContentClient::with_rpc(
                ReplyRpc {
                    reply,
                    calls: calls.clone(),
                    operation: "ack",
                    expected_ack: Some(serde_json::to_value(&request).unwrap()),
                },
                super::super::InputContentConfig {
                    origin: "https://content.invalid".into(),
                    deadline: Duration::from_secs(1),
                    max_materialized_bytes: 8192,
                },
            )
            .unwrap();
            let result = client.ack_node_recovery(&authority, &request).await;
            assert_eq!(result.is_ok(), case == 0, "case {case}");
            assert_eq!(
                calls.load(std::sync::atomic::Ordering::SeqCst),
                usize::from(case < 2)
            );
            assert_eq!(authority.receipt(), &original);
        }
    }

    #[tokio::test]
    async fn actual_failure_route_ack_requires_exact_handler_continuation() {
        use crate::agents::graph::{
            node_recovery::StopReason,
            node_recovery_runtime::{
                NodeRecoveryFailureRouteContinuation, NodeRecoveryFailureRouteFailure,
            },
        };
        let original = receipt();
        let authority =
            crate::protocol::control::test_node_recovery_control_authority(original.clone());
        let route = NodeRecoveryFailureRouteContinuation {
            schema: "elitea.pipeline.node-recovery-failure-route.v1".into(),
            route_id: "handle_code_failure".into(),
            failed: NodeRecoveryFailureRouteFailure {
                activation_id: original.activation_id.clone(),
                attempt: original.attempt,
                failure_class: original.failure_class,
                stop_reason: StopReason::AttemptsExhausted,
            },
        };
        let id = "1".repeat(32);
        let sha = receipt_sha256(&original).unwrap();
        let request = NodeRecoveryAckRequest {
            request_id: &id,
            activation_id: &original.activation_id,
            expected_revision: 3,
            receipt_sha256: &sha,
            applied_revision: 4,
            continuation_receipt: None,
            terminal_stop_reason: None,
            failure_route_continuation: Some(&route),
        };
        let valid = json!({"schema":"elitea.pipeline.node-recovery-ack.v1","execution_id":"execution/one","generation":2,
            "request_id":id,"applied_revision":4,"replay":false,"recovery_resume_authorized":true,
            "resumption":{"schema":"elitea.pipeline.node-recovery-resumption.v1","execution_id":"execution/one","generation":2,
                "request_id":id,"activation_id":original.activation_id,"journal_revision":4,"input_bundle_id":"frozen",
                "input_manifest_sha256":"4".repeat(64),"receipt_sha256":sha,"claim_id":"claim-1",
                "failure_route_continuation":route},"terminal_settlement_authorized":false,"terminal_authorization":null});
        for case in 0..3 {
            let mut reply = valid.clone();
            if case == 1 {
                reply["resumption"]["failure_route_continuation"]["route_id"] =
                    json!("another_handler");
            }
            if case == 2 {
                reply["resumption"]["failure_route_continuation"] = Value::Null;
            }
            let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let client = InputContentClient::with_rpc(
                ReplyRpc {
                    reply,
                    calls: calls.clone(),
                    operation: "ack",
                    expected_ack: Some(serde_json::to_value(&request).unwrap()),
                },
                super::super::InputContentConfig {
                    origin: "https://content.invalid".into(),
                    deadline: Duration::from_secs(1),
                    max_materialized_bytes: 8192,
                },
            )
            .unwrap();
            assert_eq!(
                client.ack_node_recovery(&authority, &request).await.is_ok(),
                case == 0
            );
            assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 1);
        }
    }
}
