//! Safe node suspension facts. Main supplies all operator authority.

use serde::{Deserialize, Serialize};

use super::node_recovery::{
    AttemptOutcome, NodeAttemptLedger, NodeAttemptPhase, NodeFailureClass, RecoveryDecision,
    ReplaySafety, StopReason,
};
use super::node_recovery_runtime::NodeAttemptActivation;

pub(crate) const NODE_RECOVERY_RECEIPT_SCHEMA: &str = "elitea.pipeline.node-recovery-required.v1";

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum NodeRecoveryRequiredReason {
    OperatorRetryRequired,
    EffectReconciliationRequired,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum NodeRecoveryAction {
    Retry,
    Reconcile,
    ResumeResult,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct NodeRecoveryRequiredReceipt {
    pub(crate) schema: String,
    pub(crate) activation_id: String,
    pub(crate) journal_revision: u64,
    pub(crate) graph_thread: String,
    pub(crate) node_id: String,
    pub(crate) step: u64,
    pub(crate) attempt: u16,
    pub(crate) failure_class: NodeFailureClass,
    pub(crate) stop_reason: StopReason,
    pub(crate) replay_safety: ReplaySafety,
    pub(crate) allowed_actions: Vec<NodeRecoveryAction>,
}

impl NodeRecoveryRequiredReceipt {
    #[allow(
        clippy::match_same_arms,
        reason = "Keep distinct receipt and authority outcomes explicit."
    )]
    pub(crate) fn validate(&self) -> bool {
        if self.schema != NODE_RECOVERY_RECEIPT_SCHEMA
            || self.activation_id.len() != 64
            || !self
                .activation_id
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
            || self.activation_id.bytes().all(|c| c == b'0')
            || self.journal_revision == 0
            || self.journal_revision > i64::MAX as u64
            || self.step > i64::MAX as u64
            || !(1..=16).contains(&self.attempt)
            || !super::yaml::valid_graph_id(&self.node_id)
            || self.graph_thread.is_empty()
            || self.graph_thread.len() > 512
            || self
                .graph_thread
                .chars()
                .any(|c| c.is_control() || matches!(c, '\u{2028}' | '\u{2029}'))
            || matches!(
                self.failure_class,
                NodeFailureClass::AuthenticationDenied
                    | NodeFailureClass::AuthorizationDenied
                    | NodeFailureClass::SensitiveRejected
                    | NodeFailureClass::Cancelled
                    | NodeFailureClass::LeaseLost
            )
        {
            return false;
        }
        match (
            self.stop_reason,
            self.replay_safety,
            self.allowed_actions.as_slice(),
        ) {
            (
                StopReason::OperatorApprovalRequired,
                ReplaySafety::NoExternalEffect | ReplaySafety::IdempotentEffectNotCommitted { .. },
                [NodeRecoveryAction::Retry],
            ) => matches!(
                self.failure_class,
                NodeFailureClass::DependencyUnavailable
                    | NodeFailureClass::RateLimited
                    | NodeFailureClass::AttemptTimeout
                    | NodeFailureClass::WorkerInterrupted
            ),
            (
                StopReason::EffectReconciliationRequired,
                ReplaySafety::UnknownExternalEffect { .. } | ReplaySafety::Unclassified,
                [NodeRecoveryAction::Reconcile],
            ) => true,
            (
                StopReason::EffectReconciliationRequired,
                ReplaySafety::CompletedExternalEffect { .. },
                [NodeRecoveryAction::ResumeResult],
            ) => true,
            _ => false,
        }
    }
    pub(crate) fn from_ledger(
        activation: &NodeAttemptActivation,
        ledger: &NodeAttemptLedger,
    ) -> Option<Self> {
        let last = ledger.history().last()?;
        let AttemptOutcome::Failed { .. } = last.outcome else {
            return None;
        };
        let failure = ledger.effective_failure().ok()?;
        let reason = match ledger.phase() {
            NodeAttemptPhase::Failed(RecoveryDecision::Stop(
                StopReason::OperatorApprovalRequired,
            )) => NodeRecoveryRequiredReason::OperatorRetryRequired,
            NodeAttemptPhase::Failed(RecoveryDecision::Reconcile { .. }) => {
                NodeRecoveryRequiredReason::EffectReconciliationRequired
            }
            _ => return None,
        };
        let (stop_reason, allowed_actions) = match reason {
            NodeRecoveryRequiredReason::OperatorRetryRequired => (
                StopReason::OperatorApprovalRequired,
                vec![NodeRecoveryAction::Retry],
            ),
            NodeRecoveryRequiredReason::EffectReconciliationRequired => (
                StopReason::EffectReconciliationRequired,
                vec![match failure.replay {
                    ReplaySafety::CompletedExternalEffect { .. } => {
                        NodeRecoveryAction::ResumeResult
                    }
                    _ => NodeRecoveryAction::Reconcile,
                }],
            ),
        };
        Some(Self {
            schema: NODE_RECOVERY_RECEIPT_SCHEMA.to_owned(),
            activation_id: hex(&ledger.logical_activation()),
            journal_revision: ledger.revision(),
            graph_thread: activation.root_thread_id.clone(),
            node_id: activation.node_id.clone(),
            step: activation.step,
            attempt: last.attempt,
            failure_class: failure.class,
            stop_reason,
            replay_safety: failure.replay,
            allowed_actions,
        })
    }
    pub(crate) fn matches(
        &self,
        activation: &NodeAttemptActivation,
        ledger: &NodeAttemptLedger,
    ) -> bool {
        Self::from_ledger(activation, ledger).as_ref() == Some(self)
    }
}

fn hex(value: &[u8; 32]) -> String {
    use std::fmt::Write;
    let mut out = String::with_capacity(64);
    for byte in value {
        let _ = write!(out, "{byte:02x}");
    }
    out
}

#[cfg(test)]
mod canonical_fixture_tests {
    use super::*;
    #[test]
    fn main_receipt_fixture_is_canonical_and_hashes_identically() {
        let bytes = include_bytes!("node_recovery_required_v1.fixture.json");
        let receipt: NodeRecoveryRequiredReceipt = serde_json::from_slice(bytes).unwrap();
        assert!(receipt.validate());
        let canonical = serde_json::to_vec(&serde_json::to_value(receipt).unwrap()).unwrap();
        assert_eq!(canonical, bytes);
        let digest = ring::digest::digest(&ring::digest::SHA256, bytes);
        assert_eq!(
            hex(digest.as_ref().try_into().unwrap()),
            "8f592df47f70a2b2f5c4bf0bd97832856cd194d139a0eeab7dc37a82ca4b1351"
        );
    }
}
