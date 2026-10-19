//! Keep the pipeline turn boundary on the typed fan-out checkpoint authority.
//!
//! `bind_parallel_authority` binds only when the root graph's checkpointer is
//! the very authority Arc the Parallel binding holds (`Arc::ptr_eq`). The
//! plain `TurnCheckpointer` erases that capability, so the turn boundary lives
//! here instead, inside the occurrence wrapper:
//!
//! ```text
//! ParallelOccurrenceCheckpointer -> FanoutTurnAuthority -> fenced store
//! ```
//!
//! Every new root-thread row this turn writes, the occurrence freeze and join
//! rows included, carries `elitea.pipeline.execution.v1` (spike finding 2). A
//! fresh turn sees only its own execution's root frontier; an explicit
//! continuation (a `fanout_wake` or a decision resume) reads the frontier an
//! earlier execution parked. Child threads and exact immutable replays pass
//! through unchanged.
#![allow(
    dead_code,
    reason = "bound to Parallel by the fan-out composition (2a-2 commit 10)"
)]

use std::collections::HashMap;
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;
use elitea_agent_runtime::graph::turn_checkpointer::TurnExecution;
use serde_json::Value;

use super::{
    ParallelActivation, ParallelBranchDefinition, ParallelCheckpointAppender,
    ParallelCheckpointAuthority, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildOrigin, ParallelChildRequest, ParentHead, ParentSaveProbe,
    PreparedChildCheckpoint,
};

/// The turn boundary of one pipeline execution over the admitted authority.
pub(crate) struct FanoutTurnAuthority {
    inner: Arc<dyn ParallelCheckpointAuthority>,
    root_thread: String,
    execution: TurnExecution,
    resume: bool,
}

impl FanoutTurnAuthority {
    /// `resume` is the admitted start kind: `true` only for an explicit
    /// continuation, exactly as `TurnCheckpointer`.
    pub(crate) fn new(
        inner: Arc<dyn ParallelCheckpointAuthority>,
        root_thread: &str,
        execution_id: &str,
        generation: u64,
        resume: bool,
    ) -> Result<Self, GraphError> {
        if root_thread.is_empty()
            || root_thread.len() > 512
            || root_thread.chars().any(char::is_control)
            || execution_id.is_empty()
            || execution_id.len() > 256
            || execution_id.chars().any(char::is_control)
            || generation == 0
        {
            return Err(GraphError::InvalidGraph(
                "graph.fanout.invalid_turn_authority: the turn identity is invalid".to_owned(),
            ));
        }
        Ok(Self {
            inner,
            root_thread: root_thread.to_owned(),
            execution: TurnExecution::new(execution_id, generation),
            resume,
        })
    }

    fn root(&self, thread: &str) -> bool {
        thread == self.root_thread
    }

    /// A root row this turn may see: any row on a continuation, only its own
    /// execution's rows on a fresh turn.
    fn visible(&self, thread: &str, metadata: &HashMap<String, Value>) -> bool {
        !self.root(thread) || self.resume || self.execution.wrote(metadata)
    }

    fn stamped(&self, candidate: &Checkpoint) -> Checkpoint {
        let mut candidate = candidate.clone();
        if self.root(&candidate.thread_id) {
            self.execution.stamp(&mut candidate);
        }
        candidate
    }

    /// A fresh turn appends its first root row after the latest physical row
    /// an earlier execution left: retained history stays one exact chain.
    /// A latest row of this execution means the caller's view is stale.
    async fn physical_parent(
        &self,
        candidate: &Checkpoint,
    ) -> Result<Option<ParentHead>, GraphError> {
        if !self.root(&candidate.thread_id) {
            return Ok(None);
        }
        let probe = self
            .inner
            .probe_parent(&candidate.thread_id, &candidate.checkpoint_id)
            .await?;
        match probe.latest {
            Some(head) if self.visible(&head.thread_id, &head.metadata) => {
                Err(turn_error("the fan-out turn parent is no longer latest"))
            }
            latest => Ok(latest),
        }
    }
}

#[async_trait]
impl Checkpointer for FanoutTurnAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        if self
            .inner
            .load_by_id(&checkpoint.checkpoint_id)
            .await?
            .is_some()
        {
            // The fenced store checks immutable identity and exact bytes.
            return self.inner.save(checkpoint).await;
        }
        self.inner.save(&self.stamped(checkpoint)).await
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .inner
            .load(thread)
            .await?
            .filter(|checkpoint| self.visible(&checkpoint.thread_id, &checkpoint.metadata)))
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load_by_id(id).await
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread).await
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.inner.delete(thread).await
    }

    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread, policy).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for FanoutTurnAuthority {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        // An exact immutable replay keeps its original bytes and ordering.
        if self
            .inner
            .load_by_id(&candidate.checkpoint_id)
            .await?
            .is_some()
        {
            return self.inner.append_after(expected_latest, candidate).await;
        }
        let candidate = self.stamped(candidate);
        if expected_latest.is_none()
            && let Some(parent) = self.physical_parent(&candidate).await?
        {
            return self
                .inner
                .append_after_head(Some(&parent), &candidate)
                .await;
        }
        self.inner.append_after(expected_latest, &candidate).await
    }

    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        let mut probe = self.inner.probe_parent(thread_id, candidate_id).await?;
        probe.latest = probe
            .latest
            .filter(|head| self.visible(&head.thread_id, &head.metadata));
        Ok(probe)
    }

    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        // Exact replays arrive through `append_after` after the caller's probe;
        // the store refuses different bytes under an existing id here.
        let candidate = self.stamped(candidate);
        if expected.is_some() {
            return self.inner.append_after_head(expected, &candidate).await;
        }
        let physical = self.physical_parent(&candidate).await?;
        self.inner
            .append_after_head(physical.as_ref(), &candidate)
            .await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for FanoutTurnAuthority {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError> {
        self.inner.child_origin(activation)
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
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
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.inner
            .for_branch(activation, branch, ordinal, input_digest, origin)
            .await
    }

    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        self.inner
            .prepare_children(activation, children, origin)
            .await
    }
}

impl ParallelCheckpointAuthority for FanoutTurnAuthority {}

/// The store's own conflict code, so the claim machinery treats it alike.
fn turn_error(message: &str) -> GraphError {
    GraphError::CheckpointError(format!("checkpoint.conflict: {message}"))
}

#[cfg(test)]
#[path = "fanout_turn_tests.rs"]
mod tests;
