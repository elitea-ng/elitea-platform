//! The Worker's claim-fenced client of Main's per-interrupt decision ledger.
//!
//! `FanoutDecisionClient` is the production [`DecisionSource`] the fan-out
//! runner fetches from (live tick, restore fetch, final fetch before park)
//! and ACKs to. Each call is one bounded attempt on the node-recovery control
//! transport; a refused claim or fence is lease loss (control stop), anything
//! else is transient and retried by the runner's next tick or fetch. Logs
//! carry stable codes only: never a decision value, credential reference,
//! interrupt key or claim.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout::runner::{
    AckResult, DecisionBatch, DecisionError, DecisionSource, FetchedDecision,
};

use crate::transport::input_content::{
    FanoutAckRequest, FanoutDecisionAuthority, InputContentClient, InputContentError,
};

pub(crate) struct FanoutDecisionClient {
    client: Arc<InputContentClient>,
    authority: FanoutDecisionAuthority,
}

impl FanoutDecisionClient {
    pub(crate) fn new(client: Arc<InputContentClient>, authority: FanoutDecisionAuthority) -> Self {
        Self { client, authority }
    }
}

fn classify(operation: &'static str, error: &InputContentError) -> DecisionError {
    let lease_lost = matches!(error, InputContentError::AuthorizationFailed(_));
    tracing::warn!(
        target: "elitea_worker::fanout_decisions",
        operation,
        error.code = error.code(),
        lease_lost,
        "fan-out decision call failed"
    );
    if lease_lost {
        DecisionError::LeaseLost
    } else {
        DecisionError::Unavailable
    }
}

#[async_trait]
impl DecisionSource for FanoutDecisionClient {
    async fn fetch(&self) -> Result<DecisionBatch, DecisionError> {
        let reply = self
            .client
            .fetch_fanout_decisions(&self.authority)
            .await
            .map_err(|error| classify("fetch", &error))?;
        Ok(DecisionBatch {
            decision_revision: reply.decision_revision,
            decisions: reply
                .decisions
                .into_iter()
                .map(|decision| FetchedDecision {
                    interrupt_key: decision.interrupt_key,
                    request_id: decision.request_id,
                    revision: decision.revision,
                    decision_sha256: decision.decision_sha256,
                    action: decision.action,
                    value: Some(decision.value),
                    credential_ref: decision.credential_ref,
                })
                .collect(),
        })
    }

    async fn ack(
        &self,
        decision: &FetchedDecision,
        result: &AckResult,
    ) -> Result<(), DecisionError> {
        let (outcome, child_checkpoint_id) = match result {
            AckResult::Applied {
                child_checkpoint_id,
            } => ("applied", Some(child_checkpoint_id.as_str())),
            AckResult::Stale => ("stale", None),
        };
        let request = FanoutAckRequest {
            child_checkpoint_id,
            decision_sha256: &decision.decision_sha256,
            interrupt_key: &decision.interrupt_key,
            outcome,
            request_id: &decision.request_id,
            revision: decision.revision,
        };
        // A conflicting replay means Main already resolved the card.
        self.client
            .ack_fanout_decision(&self.authority, &request)
            .await
            .map(|_| ())
            .map_err(|error| classify("ack", &error))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_refused_claim_is_lease_loss_and_everything_else_is_transient() {
        assert_eq!(
            classify("fetch", &InputContentError::AuthorizationFailed("x")),
            DecisionError::LeaseLost
        );
        for error in [
            InputContentError::DependencyUnavailable("x"),
            InputContentError::Timeout("x"),
            InputContentError::InvalidInput("x"),
            InputContentError::ResourceExhausted("x"),
        ] {
            assert_eq!(classify("ack", &error), DecisionError::Unavailable);
        }
    }
}
