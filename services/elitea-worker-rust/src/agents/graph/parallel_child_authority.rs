//! A fixed Parallel inside a saved child pipeline (FU-249).
//!
//! The child graph saves through its own chain (the Agent node's activation
//! overlay over the parent's checkpointer). A Parallel's parent rows are atomic
//! appends on the store, which would bypass that chain and drop the activation
//! stamp, so a restart could hide the child's progress and run it again. This
//! authority keeps both: every read and ordinary save goes through the chain,
//! and every atomic append carries exactly the stamp the chain's save would
//! write, then goes to the parent's authority.

use std::borrow::Cow;
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;
use elitea_agent_runtime::graph::application_activation::ApplicationActivationCheckpointer;

use super::{
    ParallelActivation, ParallelBranchDefinition, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildOrigin, ParallelChildRequest, ParentHead, ParentSaveProbe,
    PreparedChildCheckpoint,
};

/// The child's checkpoint chain, with the parent's atomic authority behind it.
pub(crate) struct ChildParallelAuthority {
    /// What the child graph was given: the overlay, or the parent's checkpointer.
    chain: Arc<dyn Checkpointer>,
    /// The Agent node's overlay when it has one; its stamp goes on every append.
    overlay: Option<Arc<ApplicationActivationCheckpointer>>,
    /// The authority view of the parent's checkpointer.
    parent: Arc<dyn ParallelCheckpointAuthority>,
}

impl ChildParallelAuthority {
    pub(crate) fn new(
        chain: Arc<dyn Checkpointer>,
        overlay: Option<Arc<ApplicationActivationCheckpointer>>,
        parent: Arc<dyn ParallelCheckpointAuthority>,
    ) -> Self {
        Self {
            chain,
            overlay,
            parent,
        }
    }

    /// Whether a parent head is visible to the child's open activation (the
    /// overlay's own `load` rule, decided from the head alone).
    fn visible(&self, head: &ParentHead) -> Result<bool, GraphError> {
        match &self.overlay {
            Some(overlay) => overlay.admits(
                &head.thread_id,
                &head.metadata,
                !head.pending_nodes.is_empty(),
            ),
            None => Ok(true),
        }
    }

    /// The row an append that expects no latest row must follow: a new visit
    /// of the child sees none because the overlay hides the completed earlier
    /// visit, while the store still holds that row as latest. A visible latest
    /// row means the caller's view is stale.
    async fn physical_parent(
        &self,
        candidate: &Checkpoint,
    ) -> Result<Option<ParentHead>, GraphError> {
        let probe = self
            .parent
            .probe_parent(&candidate.thread_id, &candidate.checkpoint_id)
            .await?;
        match probe.latest {
            Some(head) if self.visible(&head)? => Err(GraphError::CheckpointError(
                "checkpoint.conflict: the expected parent checkpoint is no longer latest"
                    .to_owned(),
            )),
            latest => Ok(latest),
        }
    }

    fn stamped<'a>(&self, candidate: &'a Checkpoint) -> Result<Cow<'a, Checkpoint>, GraphError> {
        match &self.overlay {
            Some(overlay) => overlay.stamp(candidate),
            None => Ok(Cow::Borrowed(candidate)),
        }
    }
}

#[async_trait]
impl Checkpointer for ChildParallelAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.chain.save(checkpoint).await
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.chain.load(thread).await
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.chain.load_by_id(id).await
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.chain.list(thread).await
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.chain.delete(thread).await
    }

    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.chain.prune(thread, policy).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for ChildParallelAuthority {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        // An exact immutable replay keeps its original bytes and ordering.
        if self
            .parent
            .load_by_id(&candidate.checkpoint_id)
            .await?
            .is_some()
        {
            return self.parent.append_after(expected_latest, candidate).await;
        }
        let candidate = self.stamped(candidate)?;
        if expected_latest.is_none()
            && let Some(parent) = self.physical_parent(&candidate).await?
        {
            return self
                .parent
                .append_after_head(Some(&parent), &candidate)
                .await;
        }
        self.parent.append_after(expected_latest, &candidate).await
    }

    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        let mut probe = self.parent.probe_parent(thread_id, candidate_id).await?;
        if let Some(head) = &probe.latest
            && !self.visible(head)?
        {
            probe.latest = None;
        }
        Ok(probe)
    }

    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        // Exact replays arrive through `append_after` after the caller's probe;
        // the store refuses different bytes under an existing id here.
        let candidate = self.stamped(candidate)?;
        if expected.is_some() {
            return self.parent.append_after_head(expected, &candidate).await;
        }
        let physical = self.physical_parent(&candidate).await?;
        self.parent
            .append_after_head(physical.as_ref(), &candidate)
            .await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for ChildParallelAuthority {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError> {
        self.parent.child_origin(activation)
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.parent
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.parent
            .for_branch(activation, branch, ordinal, input_digest, origin)
            .await
    }

    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        self.parent
            .prepare_children(activation, children, origin)
            .await
    }
}

impl ParallelCheckpointAuthority for ChildParallelAuthority {}
