//! Map receipts use the existing claim-fenced checkpoint transport.

use std::sync::{Arc, Mutex};

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use super::super::parallel::ParallelCheckpointAppender;
use super::{
    FrozenMap, MAX_CHECKPOINT_BYTES, MAX_ITEM_BYTES, MapActivation, MapStop, bounded, map_error,
    validate_checkpoint_boundary, validate_metadata, validate_state_boundary, validate_value,
    validate_values,
};

const OCCURRENCE_KEY: &str = "elitea.graph.map.occurrence.v1";
const ITEM_RECEIPT_KEY: &str = "elitea.graph.map.item-receipt.v1";

/// Compose this wrapper with both the ADK parent and the Map node.
/// The existing appender compares the exact parent under its database writer lock.
pub(crate) struct MapOccurrenceCheckpointer {
    inner: Arc<dyn ParallelCheckpointAppender>,
}

impl MapOccurrenceCheckpointer {
    pub(crate) fn new(inner: Arc<dyn ParallelCheckpointAppender>) -> Self {
        Self { inner }
    }

    pub(super) async fn freeze(
        &self,
        context: &NodeContext,
        plan: FrozenMap,
    ) -> Result<FrozenMap, GraphError> {
        validate_state_boundary(&context.state, MAX_CHECKPOINT_BYTES)?;
        for item in &plan.items {
            validate_values(item.input.values())?;
        }
        let latest = self.inner.load(&plan.activation.root_thread_id).await?;
        if let Some(saved) = latest.as_ref() {
            validate_checkpoint_boundary(saved)?;
            if saved.thread_id != context.config.thread_id
                || saved.step != context.step
                || saved.pending_nodes.as_slice() != [plan.activation.node_id.as_str()]
                || saved.state != context.state
            {
                return Err(map_error("stale_activation"));
            }
            if saved.metadata.contains_key(OCCURRENCE_KEY) {
                let existing = occurrence(saved, &plan.activation)?;
                if existing.items != plan.items {
                    return Err(map_error("stale_activation"));
                }
                return Ok(existing);
            }
        }
        let mut candidate = latest.as_ref().map_or_else(
            || {
                Checkpoint::new(
                    &context.config.thread_id,
                    context.state.clone(),
                    context.step,
                    vec![plan.activation.node_id.clone()],
                )
            },
            Clone::clone,
        );
        refresh_identity(&mut candidate);
        candidate.metadata.insert(
            OCCURRENCE_KEY.to_owned(),
            serde_json::to_value(&plan).map_err(|_| map_error("corrupt_occurrence"))?,
        );
        validate_checkpoint_boundary(&candidate)?;
        self.inner.append_after(latest.as_ref(), &candidate).await?;
        Ok(plan)
    }

    pub(super) async fn record_stop(
        &self,
        activation: &MapActivation,
        stop: MapStop,
    ) -> Result<(), GraphError> {
        let parent = self
            .inner
            .load(&activation.root_thread_id)
            .await?
            .ok_or_else(|| map_error("missing_occurrence"))?;
        let mut plan = occurrence(&parent, activation)?;
        if let Some(existing) = plan.stop {
            return if existing == stop {
                Ok(())
            } else {
                Err(map_error("stale_activation"))
            };
        }
        match stop {
            MapStop::Failed { index }
            | MapStop::ResourceExhausted { index }
            | MapStop::UnexpectedPause { index }
            | MapStop::Blocked { index }
                if index >= plan.items.len() =>
            {
                return Err(map_error("corrupt_occurrence"));
            }
            _ => {}
        }
        plan.stop = Some(stop);
        let mut candidate = parent.clone();
        refresh_identity(&mut candidate);
        candidate.metadata.insert(
            OCCURRENCE_KEY.to_owned(),
            serde_json::to_value(plan).map_err(|_| map_error("corrupt_occurrence"))?,
        );
        validate_checkpoint_boundary(&candidate)?;
        self.inner.append_after(Some(&parent), &candidate).await?;
        Ok(())
    }

    pub(super) async fn validate_completion(
        &self,
        activation: &MapActivation,
        state: &State,
    ) -> Result<(), GraphError> {
        validate_state_boundary(state, MAX_CHECKPOINT_BYTES)?;
        let parent = self
            .inner
            .load(&activation.root_thread_id)
            .await?
            .ok_or_else(|| map_error("missing_occurrence"))?;
        if occurrence(&parent, activation)?.stop.is_some() {
            return Err(map_error("stale_activation"));
        }
        let mut candidate = parent;
        candidate.state.clone_from(state);
        // Include the frozen plan until the actual ADK transition removes it.
        validate_checkpoint_boundary(&candidate)
    }

    /// Read only an exact original receipt. Error text grants no terminal status.
    pub(crate) async fn stopped_at(
        &self,
        checkpoint_id: &str,
        activation: &MapActivation,
    ) -> Result<Option<MapStop>, GraphError> {
        let saved = self
            .inner
            .load_by_id(checkpoint_id)
            .await?
            .ok_or_else(|| map_error("missing_occurrence"))?;
        if saved.checkpoint_id != checkpoint_id {
            return Err(map_error("corrupt_occurrence"));
        }
        Ok(occurrence(&saved, activation)?.stop)
    }
}

fn occurrence(saved: &Checkpoint, activation: &MapActivation) -> Result<FrozenMap, GraphError> {
    validate_checkpoint_boundary(saved)?;
    let plan: FrozenMap = serde_json::from_value(
        saved
            .metadata
            .get(OCCURRENCE_KEY)
            .ok_or_else(|| map_error("missing_occurrence"))?
            .clone(),
    )
    .map_err(|_| map_error("corrupt_occurrence"))?;
    if plan.activation != *activation
        || saved.thread_id != activation.root_thread_id
        || u64::try_from(saved.step).map_err(|_| map_error("corrupt_occurrence"))?
            != activation.step
        || saved.pending_nodes.as_slice() != [activation.node_id.as_str()]
        || plan.items.len() > super::MAX_ITEMS
        || plan
            .items
            .iter()
            .enumerate()
            .any(|(index, item)| item.index != index)
    {
        return Err(map_error("corrupt_occurrence"));
    }
    Ok(plan)
}

fn refresh_identity(checkpoint: &mut Checkpoint) {
    let fresh = Checkpoint::new(
        &checkpoint.thread_id,
        State::new(),
        checkpoint.step,
        Vec::new(),
    );
    checkpoint.checkpoint_id = fresh.checkpoint_id;
    checkpoint.created_at = fresh.created_at;
}

#[async_trait]
impl Checkpointer for MapOccurrenceCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        validate_checkpoint_boundary(checkpoint)?;
        // Exact replay retains its immutable bytes and cannot advance latest.
        if self
            .inner
            .load_by_id(&checkpoint.checkpoint_id)
            .await?
            .is_some()
        {
            return self.inner.append_after(None, checkpoint).await;
        }
        let parent = self.inner.load(&checkpoint.thread_id).await?;
        if let Some(parent) = parent.as_ref() {
            validate_checkpoint_boundary(parent)?;
        }
        let mut candidate = checkpoint.clone();
        if let Some(parent) = parent.as_ref() {
            if parent.thread_id != checkpoint.thread_id || checkpoint.step < parent.step {
                return Err(map_error("stale_activation"));
            }
            if let Some(raw) = parent.metadata.get(OCCURRENCE_KEY) {
                let plan: FrozenMap = serde_json::from_value(raw.clone())
                    .map_err(|_| map_error("corrupt_occurrence"))?;
                occurrence(parent, &plan.activation)?;
                if checkpoint.step == parent.step
                    && checkpoint.pending_nodes.as_slice() == [plan.activation.node_id.as_str()]
                {
                    if checkpoint.state != parent.state {
                        return Err(map_error("stale_activation"));
                    }
                    candidate
                        .metadata
                        .insert(OCCURRENCE_KEY.to_owned(), raw.clone());
                } else if plan.stop.is_some() {
                    return Err(map_error("stale_activation"));
                } else {
                    candidate.metadata.remove(OCCURRENCE_KEY);
                }
            }
        }
        validate_checkpoint_boundary(&candidate)?;
        self.inner.append_after(parent.as_ref(), &candidate).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load(thread).await
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

#[derive(Clone, Deserialize, Serialize)]
#[serde(tag = "status", rename_all = "snake_case", deny_unknown_fields)]
pub(super) enum MapItemReceipt {
    Completed,
    Failed,
    UnexpectedPause {
        interrupt: adk_rust::graph::interrupt::Interrupt,
    },
}

pub(super) struct MapReceiptCheckpointer {
    inner: Arc<dyn Checkpointer>,
    thread: String,
    captured: Mutex<Option<MapItemReceipt>>,
    admitted_threads: std::collections::BTreeSet<String>,
}
impl MapReceiptCheckpointer {
    pub(super) fn new(
        inner: Arc<dyn Checkpointer>,
        thread: String,
        admitted_threads: std::collections::BTreeSet<String>,
    ) -> Self {
        Self {
            inner,
            thread,
            captured: Mutex::new(None),
            admitted_threads,
        }
    }
    pub(super) async fn load_item(&self) -> Result<Option<Checkpoint>, GraphError> {
        let saved = self.inner.load(&self.thread).await?;
        if saved
            .as_ref()
            .is_some_and(|saved| saved.thread_id != self.thread)
        {
            return Err(map_error("invalid_child_scope"));
        }
        if let Some(saved) = saved.as_ref() {
            validate_checkpoint_boundary(saved)?;
        }
        Ok(saved)
    }
    pub(super) fn receipt(checkpoint: &Checkpoint) -> Result<Option<MapItemReceipt>, GraphError> {
        checkpoint
            .metadata
            .get(ITEM_RECEIPT_KEY)
            .map(|raw| {
                validate_metadata(raw)?;
                serde_json::from_value(raw.clone()).map_err(|_| map_error("corrupt_receipt"))
            })
            .transpose()
    }
    fn capture(&self, receipt: MapItemReceipt) -> Result<(), GraphError> {
        *self
            .captured
            .lock()
            .map_err(|_| map_error("corrupt_receipt"))? = Some(receipt);
        Ok(())
    }
}

#[async_trait]
impl Checkpointer for MapReceiptCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        validate_checkpoint_boundary(checkpoint)?;
        if !self.admitted_threads.contains(&checkpoint.thread_id) {
            return Err(map_error("invalid_child_scope"));
        }
        let mut candidate = checkpoint.clone();
        if checkpoint.thread_id == self.thread
            && let Some(receipt) = self
                .captured
                .lock()
                .map_err(|_| map_error("corrupt_receipt"))?
                .clone()
        {
            if matches!(receipt, MapItemReceipt::Completed) && !candidate.pending_nodes.is_empty() {
                return Err(map_error("corrupt_receipt"));
            }
            candidate.metadata.insert(
                ITEM_RECEIPT_KEY.to_owned(),
                serde_json::to_value(receipt).map_err(|_| map_error("corrupt_receipt"))?,
            );
        }
        validate_checkpoint_boundary(&candidate)?;
        self.inner.save(&candidate).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        if !self.admitted_threads.contains(thread) {
            return Err(map_error("invalid_child_scope"));
        }
        if thread == self.thread {
            return self.load_item().await;
        }
        let saved = self.inner.load(thread).await?;
        if let Some(saved) = saved.as_ref() {
            if saved.thread_id != thread {
                return Err(map_error("invalid_child_scope"));
            }
            validate_checkpoint_boundary(saved)?;
        }
        Ok(saved)
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        let saved = self.inner.load_by_id(id).await?;
        if saved
            .as_ref()
            .is_some_and(|saved| !self.admitted_threads.contains(&saved.thread_id))
        {
            return Err(map_error("invalid_child_scope"));
        }
        if let Some(saved) = saved.as_ref() {
            validate_checkpoint_boundary(saved)?;
        }
        Ok(saved)
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        if !self.admitted_threads.contains(thread) {
            return Err(map_error("invalid_child_scope"));
        }
        self.inner.list(thread).await
    }
    async fn delete(&self, _thread: &str) -> Result<(), GraphError> {
        Err(map_error("immutable_receipt"))
    }
    async fn prune(&self, _thread: &str, _policy: &RetentionPolicy) -> Result<usize, GraphError> {
        Err(map_error("immutable_receipt"))
    }
}

pub(crate) struct MapItemExecution {
    checkpoint: Arc<MapReceiptCheckpointer>,
    owned_worker: String,
    parent_thread: String,
    admitted_threads: std::collections::BTreeSet<String>,
    parent_event_scope: Option<serde_json::Value>,
}
impl MapItemExecution {
    pub(super) fn new(
        checkpoint: Arc<MapReceiptCheckpointer>,
        owned_worker: String,
        parent_thread: String,
        admitted_threads: std::collections::BTreeSet<String>,
        parent_event_scope: Option<serde_json::Value>,
    ) -> Self {
        Self {
            checkpoint,
            owned_worker,
            parent_thread,
            admitted_threads,
            parent_event_scope,
        }
    }
    pub(crate) fn parent_thread_id(&self) -> &str {
        &self.parent_thread
    }
    pub(crate) fn thread_id(&self) -> &str {
        &self.checkpoint.thread
    }
    pub(crate) fn worker(&self) -> &str {
        &self.owned_worker
    }
    pub(crate) fn admitted_threads(&self) -> &std::collections::BTreeSet<String> {
        &self.admitted_threads
    }
    pub(crate) fn parent_event_scope_value(&self) -> Option<&serde_json::Value> {
        self.parent_event_scope.as_ref()
    }
    pub(crate) fn checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.checkpoint.clone()
    }
    pub(crate) fn wrap_node<N: Node + 'static>(&self, node: N) -> MapLifecycleNode<N> {
        MapLifecycleNode {
            node,
            checkpoint: Arc::clone(&self.checkpoint),
        }
    }
}

pub(crate) struct MapLifecycleNode<N> {
    node: N,
    checkpoint: Arc<MapReceiptCheckpointer>,
}
#[async_trait]
impl<N: Node + 'static> Node for MapLifecycleNode<N> {
    fn name(&self) -> &str {
        self.node.name()
    }
    fn description(&self) -> &str {
        self.node.description()
    }
    fn capabilities(&self) -> adk_rust::AgentCapabilities {
        self.node.capabilities()
    }
    fn validate(&self) -> Result<(), GraphError> {
        self.node.validate()
    }
    fn validate_against(&self, schema: &adk_rust::graph::StateSchema) -> Result<(), GraphError> {
        self.node.validate_against(schema)
    }
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        match self.node.execute(context).await {
            Ok(output) if output.goto_parent.is_none() && output.goto.is_none() => {
                validate_values(output.updates.values())?;
                if let Some(interrupt) = output.interrupt.as_ref() {
                    validate_interrupt(interrupt)?;
                }
                for event in &output.events {
                    validate_event(event)?;
                }
                self.checkpoint.capture(
                    output
                        .interrupt
                        .clone()
                        .map_or(MapItemReceipt::Completed, |interrupt| {
                            MapItemReceipt::UnexpectedPause { interrupt }
                        }),
                )?;
                Ok(output)
            }
            Err(GraphError::Interrupted(interrupted)) => {
                validate_interrupt(&interrupted.interrupt)?;
                self.checkpoint.capture(MapItemReceipt::UnexpectedPause {
                    interrupt: interrupted.interrupt.clone(),
                })?;
                Ok(NodeOutput::new().with_interrupt(interrupted.interrupt))
            }
            _ => {
                validate_state_boundary(&context.state, MAX_CHECKPOINT_BYTES)?;
                self.checkpoint.capture(MapItemReceipt::Failed)?;
                let saved = Checkpoint::new(
                    &context.config.thread_id,
                    context.state.clone(),
                    context.step,
                    vec![self.name().to_owned()],
                );
                self.checkpoint.save(&saved).await?;
                Err(map_error("item_failed"))
            }
        }
    }
}

fn validate_interrupt(interrupt: &adk_rust::graph::interrupt::Interrupt) -> Result<(), GraphError> {
    if let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    } = interrupt
    {
        validate_value(data)?;
    }
    bounded(interrupt, MAX_ITEM_BYTES)
}

fn validate_event(event: &adk_rust::graph::stream::StreamEvent) -> Result<(), GraphError> {
    use adk_rust::graph::stream::StreamEvent;
    match event {
        StreamEvent::State { state, .. } | StreamEvent::Done { state, .. } => {
            validate_values(state.values())?;
        }
        StreamEvent::Updates { updates, .. } => validate_values(updates.values())?,
        StreamEvent::Custom { data, .. }
        | StreamEvent::Debug { data, .. }
        | StreamEvent::NodeInterrupt {
            data: Some(data), ..
        } => validate_value(data)?,
        _ => {}
    }
    bounded(event, MAX_ITEM_BYTES)
}

#[cfg(test)]
#[path = "map_item_scope_tests.rs"]
mod item_scope_tests;
