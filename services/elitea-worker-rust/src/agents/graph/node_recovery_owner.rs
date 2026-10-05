//! Server-owned effect evidence. A result is projected by its original node.

use super::node_recovery::ReplaySafety;
use super::node_recovery_receipt::NodeRecoveryRequiredReceipt;
use serde::{Deserialize, Serialize};

pub(crate) const HTTP_RECEIPT_AUDIENCE: &str = "elitea.runtime.http-action-receipt.v2";
#[derive(Clone, Copy, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum NodeRecoveryOwnerProofKind {
    VerifiedNoEffect,
    CommittedResult,
}
#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryResultReference {
    pub(crate) content_id: String,
    pub(crate) immutable_version: String,
    pub(crate) digest_sha256: String,
    pub(crate) byte_length: u64,
    pub(crate) media_type: String,
    pub(crate) required_grant_audience: String,
}
#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryOwnerProof {
    pub(crate) schema: String,
    pub(crate) kind: NodeRecoveryOwnerProofKind,
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
    pub(crate) activation_id: String,
    pub(crate) attempt: u16,
    pub(crate) expected_revision: u64,
    pub(crate) effect_id: String,
    pub(crate) owner_receipt_sha256: String,
    pub(crate) result_ref: Option<NodeRecoveryResultReference>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) owner_receipt_ref: Option<NodeRecoveryResultReference>,
}
impl NodeRecoveryOwnerProof {
    pub(crate) fn validates(
        &self,
        execution: &str,
        generation: u64,
        receipt: &NodeRecoveryRequiredReceipt,
    ) -> bool {
        if self.schema != "elitea.pipeline.node-recovery-owner-proof.v1"
            || !receipt.validate()
            || self.execution_id != execution
            || self.generation != generation
            || self.activation_id != receipt.activation_id
            || self.attempt != receipt.attempt
            || self.expected_revision != receipt.journal_revision
            || !hex64(&self.effect_id)
            || !hex64(&self.owner_receipt_sha256)
        {
            return false;
        }
        let original = match receipt.replay_safety {
            ReplaySafety::UnknownExternalEffect { effect_id } => Some(effect_id),
            ReplaySafety::CompletedExternalEffect { receipt_id } => Some(receipt_id),
            _ => None,
        };
        if original.is_none_or(|id| hex(&id) != self.effect_id) {
            return false;
        }
        match (self.kind, &self.result_ref) {
            (NodeRecoveryOwnerProofKind::VerifiedNoEffect, None) => {
                matches!(
                    receipt.replay_safety,
                    ReplaySafety::UnknownExternalEffect { .. }
                ) && self.owner_receipt_ref.as_ref().is_some_and(|reference| {
                    self.valid_reference(reference)
                        && reference.required_grant_audience
                            == crate::sandbox::code_recovery::CODE_RESULT_AUDIENCE
                })
            }
            (NodeRecoveryOwnerProofKind::CommittedResult, Some(reference)) => {
                self.owner_receipt_ref.is_none()
                    && self.valid_reference(reference)
                    && matches!(
                        reference.required_grant_audience.as_str(),
                        HTTP_RECEIPT_AUDIENCE | crate::sandbox::code_recovery::CODE_RESULT_AUDIENCE
                    )
            }
            _ => false,
        }
    }
    pub(crate) fn matches_result_bytes(&self, bytes: &[u8]) -> bool {
        let Some(reference) = &self.result_ref else {
            return false;
        };
        reference.byte_length == bytes.len() as u64
            && self.owner_receipt_sha256
                == hex(ring::digest::digest(&ring::digest::SHA256, bytes).as_ref())
    }
    fn valid_reference(&self, reference: &NodeRecoveryResultReference) -> bool {
        reference.content_id == self.effect_id
            && reference.immutable_version == self.owner_receipt_sha256
            && reference.digest_sha256 == self.owner_receipt_sha256
            && reference.byte_length > 0
            && reference.byte_length <= 1024 * 1024
            && reference.media_type == "application/json"
    }
    pub(crate) fn matches_owner_receipt_bytes(&self, bytes: &[u8]) -> bool {
        self.kind == NodeRecoveryOwnerProofKind::VerifiedNoEffect
            && self.result_ref.is_none()
            && self.owner_receipt_ref.as_ref().is_some_and(|reference| {
                self.valid_reference(reference)
                    && reference.required_grant_audience
                        == crate::sandbox::code_recovery::CODE_RESULT_AUDIENCE
                    && reference.byte_length == bytes.len() as u64
                    && self.owner_receipt_sha256
                        == hex(ring::digest::digest(&ring::digest::SHA256, bytes).as_ref())
            })
    }
}
fn hex64(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        && !value.bytes().all(|b| b == b'0')
}
fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut value = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(value, "{b:02x}");
    }
    value
}
