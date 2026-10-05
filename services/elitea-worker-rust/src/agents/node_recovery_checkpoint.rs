//! Exact pending Code frontier and independently fenced node attempt journal.

use crate::agents::graph::node_recovery_receipt::NodeRecoveryRequiredReceipt;
use crate::agents::graph::node_recovery_runtime::{NodeAttemptActivation, NodeAttemptJournal};

/// Returned only by frozen graph admission plus current root/journal inspection.
/// No start method, credential, invocation permit or business state is exposed.
pub(crate) struct OpenedNodeRecoveryVisit {
    pub(crate) activation: NodeAttemptActivation,
    pub(crate) journal: NodeAttemptJournal,
    checkpoint: super::session::ValidatedModelCheckpoint,
    receipt: NodeRecoveryRequiredReceipt,
    context: adk_rust::graph::NodeContext,
    projector: Option<std::sync::Arc<dyn super::graph::node_recovery_runtime::NodeResultRecovery>>,
}
impl OpenedNodeRecoveryVisit {
    pub(crate) async fn advance_no_effect_receipt(
        &mut self,
        receipt: NodeRecoveryRequiredReceipt,
    ) -> Result<(), adk_rust::graph::GraphError> {
        if !receipt.validate()
            || self.receipt.activation_id != receipt.activation_id
            || self.receipt.node_id != receipt.node_id
            || self.receipt.graph_thread != receipt.graph_thread
            || self.receipt.step != receipt.step
            || self.receipt.attempt != receipt.attempt
            || self.receipt.journal_revision.checked_add(1) != Some(receipt.journal_revision)
        {
            return Err(super::graph::node_recovery_runtime::recovery_error(
                "pipeline.node_recovery.continuation_mismatch",
            ));
        }
        self.journal
            .verify_pending_receipt(&self.activation, &receipt)
            .await?;
        self.receipt = receipt;
        Ok(())
    }
    pub(super) fn inspected(
        activation: NodeAttemptActivation,
        journal: NodeAttemptJournal,
        checkpoint: super::session::ValidatedModelCheckpoint,
        receipt: NodeRecoveryRequiredReceipt,
        context: adk_rust::graph::NodeContext,
        projector: Option<
            std::sync::Arc<dyn super::graph::node_recovery_runtime::NodeResultRecovery>,
        >,
    ) -> Self {
        Self {
            activation,
            journal,
            checkpoint,
            receipt,
            context,
            projector,
        }
    }
    pub(crate) fn matches_receipt(&self, receipt: &NodeRecoveryRequiredReceipt) -> bool {
        self.receipt == *receipt
    }
    pub(crate) fn checkpoint(&self) -> &super::session::ValidatedModelCheckpoint {
        &self.checkpoint
    }
    pub(crate) fn into_checkpoint(self) -> super::session::ValidatedModelCheckpoint {
        self.checkpoint
    }
    pub(crate) fn result_projection(
        &self,
    ) -> (
        &adk_rust::graph::NodeContext,
        Option<&dyn super::graph::node_recovery_runtime::NodeResultRecovery>,
    ) {
        (&self.context, self.projector.as_deref())
    }
}
