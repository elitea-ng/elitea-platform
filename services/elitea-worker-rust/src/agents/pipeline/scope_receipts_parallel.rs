//! Keep scoped receipt validation on the atomic Parallel checkpoint path.

use super::{
    Arc, Checkpoint, Checkpointer, GRAPH_CALL_RECEIPTS_METADATA_KEY, GraphError, Mutex,
    PipelineGraphReceiptCheckpointer, RetentionPolicy, async_trait, read_receipts, receipt_error,
    receipt_revision_parent, validate_graph_call_revision,
};
use crate::agents::graph::{
    ParallelActivation, ParallelBranchDefinition, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildRequest, ParentHead, ParentSaveProbe, PreparedChildCheckpoint,
};

/// One opaque authority supplies every checkpoint capability behind this overlay.
/// The constructor derives the ordinary adapter from that same authority Arc.
pub(crate) struct PipelineGraphReceiptAuthority {
    inner: Arc<dyn ParallelCheckpointAuthority>,
    receipts: PipelineGraphReceiptCheckpointer,
}

impl PipelineGraphReceiptAuthority {
    pub(crate) fn new(inner: Arc<dyn ParallelCheckpointAuthority>, gate: Arc<Mutex<()>>) -> Self {
        let checkpointer: Arc<dyn Checkpointer> = inner.clone();
        Self {
            inner,
            receipts: PipelineGraphReceiptCheckpointer::new(checkpointer, gate),
        }
    }
}

#[async_trait]
impl Checkpointer for PipelineGraphReceiptAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.receipts.save(checkpoint).await
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.receipts.load(thread).await
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.receipts.load_by_id(id).await
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.receipts.list(thread).await
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.receipts.delete(thread).await
    }

    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.receipts.prune(thread, policy).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for PipelineGraphReceiptAuthority {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let _guard = self.receipts.gate.lock().await;
        if expected_latest.is_some_and(|parent| parent.thread_id != candidate.thread_id) {
            return Err(receipt_error());
        }
        // An immutable replay retains its original metadata and latest ordering.
        if let Some(existing) = self.inner.load_by_id(&candidate.checkpoint_id).await? {
            if serde_json::to_value(&existing).map_err(|_| receipt_error())?
                != serde_json::to_value(candidate).map_err(|_| receipt_error())?
            {
                return Err(receipt_error());
            }
            return self.inner.append_after(expected_latest, candidate).await;
        }
        let candidate = retain_atomic_receipts(expected_latest, candidate)?;
        // The store checks the complete expected parent under its writer lock.
        // No ordinary save or unchecked second parent load replaces this append.
        self.inner.append_after(expected_latest, &candidate).await
    }

    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        self.inner.probe_parent(thread_id, candidate_id).await
    }

    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let _guard = self.receipts.gate.lock().await;
        if expected.is_some_and(|parent| parent.thread_id != candidate.thread_id) {
            return Err(receipt_error());
        }
        // Exact replays arrive through `append_after` after the caller's probe.
        // Here the store's exact-existing check under the writer lock refuses any
        // different payload under an existing id, so no second probe is needed.
        // Receipts live in metadata. Only a receipt revision compares the
        // parent's whole frontier, so only that rare path reads the full row.
        let candidate = match expected {
            None => retain_atomic_receipts(None, candidate)?,
            Some(head) if receipt_revision_parent(candidate)?.is_some() => {
                let parent = match head.snapshot.as_deref() {
                    Some(snapshot) => snapshot.clone(),
                    None => self
                        .inner
                        .load_by_id(&head.checkpoint_id)
                        .await?
                        .ok_or_else(receipt_error)?,
                };
                retain_atomic_receipts(Some(&parent), candidate)?
            }
            Some(head) => retain_atomic_receipts(Some(&head.stateless()), candidate)?,
        };
        // The store compares the expected parent identity under its writer lock.
        self.inner.append_after_head(expected, &candidate).await
    }
}

fn retain_atomic_receipts(
    parent: Option<&Checkpoint>,
    candidate: &Checkpoint,
) -> Result<Checkpoint, GraphError> {
    let mut candidate = candidate.clone();
    let revision = receipt_revision_parent(&candidate)?;
    read_receipts(&candidate)?;
    let Some(parent) = parent else {
        // An empty lineage has no admitted original graph call to retain.
        if revision.is_some()
            || candidate
                .metadata
                .contains_key(GRAPH_CALL_RECEIPTS_METADATA_KEY)
        {
            return Err(receipt_error());
        }
        return Ok(candidate);
    };
    if parent.thread_id != candidate.thread_id {
        return Err(receipt_error());
    }
    read_receipts(parent)?;
    if revision.is_some() {
        validate_graph_call_revision(parent, &candidate)?;
        return Ok(candidate);
    }
    match (
        parent.metadata.get(GRAPH_CALL_RECEIPTS_METADATA_KEY),
        candidate.metadata.get(GRAPH_CALL_RECEIPTS_METADATA_KEY),
    ) {
        (Some(original), Some(supplied)) if original != supplied => return Err(receipt_error()),
        (None, Some(_)) => return Err(receipt_error()),
        (Some(original), None) => {
            candidate.metadata.insert(
                GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
                original.clone(),
            );
        }
        _ => {}
    }
    Ok(candidate)
}

#[async_trait]
impl ParallelChildCheckpointerFactory for PipelineGraphReceiptAuthority {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<crate::agents::graph::ParallelChildOrigin, GraphError> {
        self.inner.child_origin(activation)
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.inner
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.inner
            .for_branch(activation, branch, ordinal, input_digest, origin)
            .await
    }

    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        self.inner
            .prepare_children(activation, children, origin)
            .await
    }
}

impl ParallelCheckpointAuthority for PipelineGraphReceiptAuthority {}

#[cfg(test)]
#[path = "scope_receipts_parallel_tests.rs"]
mod tests;
