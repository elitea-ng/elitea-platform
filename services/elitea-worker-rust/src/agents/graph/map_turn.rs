//! Preserve sealed Map capabilities through the original pipeline turn.
#![allow(dead_code)] // Production turn assembly remains behind the Map acceptance gate.

use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::map_reduce::{map_error, validate_checkpoint_boundary};
use super::{
    FrozenMapItem, MapActivation, MapCheckpointAuthority, MapChildCheckpoint,
    MapChildCheckpointerFactory, MapExecutionIdentity, MapWorkerKind, ParallelCheckpointAppender,
};

const EXECUTION_KEY: &str = "elitea.pipeline.execution.v1";

/// The original current writer remains separate from stable item routing identity.
pub(crate) struct MapTurnAuthority {
    inner: Arc<dyn MapCheckpointAuthority>,
    root_thread: String,
    execution: Value,
}

impl MapTurnAuthority {
    pub(crate) fn new(
        inner: Arc<dyn MapCheckpointAuthority>,
        original: &Arc<dyn Checkpointer>,
        root_thread: &str,
        execution_id: &str,
        generation: u64,
        resume: bool,
    ) -> Result<Self, GraphError> {
        let upcast: Arc<dyn Checkpointer> = inner.clone();
        if !Arc::ptr_eq(original, &upcast)
            || root_thread.is_empty()
            || root_thread.len() > 512
            || execution_id.is_empty()
            || execution_id.len() > 256
            || generation == 0
            || resume
        {
            return Err(map_error("invalid_turn_authority"));
        }
        let current = inner.execution_identity(root_thread)?;
        if current.execution_id != execution_id || current.generation != generation {
            return Err(map_error("invalid_turn_authority"));
        }
        Ok(Self {
            inner,
            root_thread: root_thread.to_owned(),
            execution: json!([execution_id, generation]),
        })
    }

    async fn candidate(&self, checkpoint: &Checkpoint) -> Result<Checkpoint, GraphError> {
        validate_checkpoint_boundary(checkpoint)?;
        if self
            .inner
            .load_by_id(&checkpoint.checkpoint_id)
            .await?
            .is_some()
        {
            // The original fenced store checks immutable identity and exact bytes.
            return Ok(checkpoint.clone());
        }
        let mut candidate = checkpoint.clone();
        if candidate.thread_id == self.root_thread {
            candidate
                .metadata
                .insert(EXECUTION_KEY.to_owned(), self.execution.clone());
        }
        validate_checkpoint_boundary(&candidate)?;
        Ok(candidate)
    }
}

#[async_trait]
impl Checkpointer for MapTurnAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.inner.save(&self.candidate(checkpoint).await?).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        let saved = self.inner.load(thread).await?;
        if let Some(checkpoint) = &saved {
            validate_checkpoint_boundary(checkpoint)?;
        }
        Ok(saved.filter(|checkpoint| {
            thread != self.root_thread
                || checkpoint.metadata.get(EXECUTION_KEY) == Some(&self.execution)
        }))
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
impl ParallelCheckpointAppender for MapTurnAuthority {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if let Some(parent) = expected {
            validate_checkpoint_boundary(parent)?;
        }
        // Exact immutable replay never replaces latest or receives a new stamp.
        if self
            .inner
            .load_by_id(&candidate.checkpoint_id)
            .await?
            .is_some()
        {
            return self.inner.append_after(expected, candidate).await;
        }
        let candidate = self.candidate(candidate).await?;
        if expected.is_none()
            && candidate.thread_id == self.root_thread
            && let Some(physical_parent) = self.inner.load(&self.root_thread).await?
        {
            validate_checkpoint_boundary(&physical_parent)?;
            if physical_parent.thread_id != self.root_thread
                || physical_parent.metadata.get(EXECUTION_KEY) == Some(&self.execution)
            {
                return Err(map_error("stale_activation"));
            }
            // Logical current-turn absence keeps exact retained-history CAS.
            return self
                .inner
                .append_after(Some(&physical_parent), &candidate)
                .await;
        }
        self.inner.append_after(expected, &candidate).await
    }
}
#[async_trait]
impl MapChildCheckpointerFactory for MapTurnAuthority {
    fn execution_identity(&self, root_thread: &str) -> Result<MapExecutionIdentity, GraphError> {
        if root_thread != self.root_thread {
            return Err(map_error("invalid_turn_authority"));
        }
        self.inner.execution_identity(root_thread)
    }
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
    ) -> Result<MapChildCheckpoint, GraphError> {
        if activation.root_thread_id != self.root_thread {
            return Err(map_error("invalid_turn_authority"));
        }
        self.inner.for_item(activation, item, worker, kind).await
    }
}
impl super::map_authority::sealed::Sealed for MapTurnAuthority {}
impl MapCheckpointAuthority for MapTurnAuthority {}
