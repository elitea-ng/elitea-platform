//! Keep original scoped call receipts on the sealed Map checkpoint path.

use super::{
    Arc, Checkpoint, Checkpointer, GRAPH_CALL_RECEIPTS_METADATA_KEY, GraphError, Mutex,
    PipelineGraphReceiptCheckpointer, RetentionPolicy, async_trait, read_receipts, receipt_error,
    receipt_revision_parent, validate_graph_call_revision,
};
use crate::agents::graph::{
    FrozenMapItem, MapActivation, MapCheckpointAuthority, MapChildCheckpoint,
    MapChildCheckpointerFactory, MapExecutionIdentity, MapWorkerKind, ParallelCheckpointAppender,
};

/// One opaque authority supplies every checkpoint capability behind this overlay.
/// The constructor derives the ordinary adapter from that same authority Arc.
pub(crate) struct PipelineMapReceiptAuthority {
    inner: Arc<dyn MapCheckpointAuthority>,
    receipts: PipelineGraphReceiptCheckpointer,
}

impl PipelineMapReceiptAuthority {
    pub(crate) fn new(inner: Arc<dyn MapCheckpointAuthority>, gate: Arc<Mutex<()>>) -> Self {
        let checkpointer: Arc<dyn Checkpointer> = inner.clone();
        Self {
            inner,
            receipts: PipelineGraphReceiptCheckpointer::new(checkpointer, gate),
        }
    }
}

#[async_trait]
impl Checkpointer for PipelineMapReceiptAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        crate::agents::graph::map_reduce::validate_checkpoint_boundary(checkpoint)?;
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
impl ParallelCheckpointAppender for PipelineMapReceiptAuthority {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        crate::agents::graph::map_reduce::validate_checkpoint_boundary(candidate)?;
        if let Some(parent) = expected_latest {
            crate::agents::graph::map_reduce::validate_checkpoint_boundary(parent)?;
        }
        let _guard = self.receipts.gate.lock().await;
        if expected_latest.is_some_and(|parent| parent.thread_id != candidate.thread_id) {
            return Err(receipt_error());
        }
        // An immutable replay retains its original metadata and latest ordering.
        if let Some(existing) = self.inner.load_by_id(&candidate.checkpoint_id).await? {
            crate::agents::graph::map_reduce::validate_checkpoint_boundary(&existing)?;
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
        crate::agents::graph::map_reduce::validate_checkpoint_boundary(&candidate)?;
        self.inner.append_after(expected_latest, &candidate).await
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
impl MapChildCheckpointerFactory for PipelineMapReceiptAuthority {
    fn execution_identity(&self, root_thread: &str) -> Result<MapExecutionIdentity, GraphError> {
        self.inner.execution_identity(root_thread)
    }
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
    ) -> Result<MapChildCheckpoint, GraphError> {
        self.inner.for_item(activation, item, worker, kind).await
    }
}
impl crate::agents::graph::map_authority::sealed::Sealed for PipelineMapReceiptAuthority {}
impl MapCheckpointAuthority for PipelineMapReceiptAuthority {}

#[cfg(test)]
#[path = "scope_receipts_map_tests.rs"]
mod tests;
