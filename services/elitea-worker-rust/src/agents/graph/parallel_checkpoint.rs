//! Lifecycle metadata inside the existing claim-fenced ADK checkpoints.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Mutex};

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError, Node, NodeContext, NodeOutput, State};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use super::{
    PARALLEL_RESUME_STATE_KEY, ParallelActivation, ParallelBlocked, ParallelCheckpointAppender,
    ParallelChildOrigin, ParallelDecision, ParallelPauseCard, business_state, is_lease_lost,
    parallel_error, validate_state, validate_values,
};

pub(super) const OCCURRENCE_KEY: &str = "elitea.graph.parallel.occurrence.v2";
const LEGACY_OCCURRENCE_KEY: &str = "elitea.graph.parallel.occurrence.v1";
const BRANCH_RECEIPT_KEY: &str = "elitea.graph.parallel.branch-receipt.v1";

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct FrozenBranchInput {
    pub(super) branch_id: String,
    pub(super) node: String,
    pub(super) ordinal: usize,
    pub(super) owned_definition_digest: [u8; 32],
    pub(super) input_digest: [u8; 32],
    pub(super) input: State,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct FrozenOccurrence {
    pub(super) activation: ParallelActivation,
    pub(super) branches: Vec<FrozenBranchInput>,
    pub(super) origin: ParallelChildOrigin,
    pub(super) child_threads: Vec<String>,
    pub(super) cards: Vec<(usize, ParallelPauseCard)>,
    pub(super) decisions: Option<Vec<ParallelDecision>>,
    pub(super) resume_inputs: BTreeMap<usize, State>,
    pub(super) blocked: Option<ParallelBlocked>,
}

/// Give this wrapper to both the parent graph and the branch runtime.
/// The inner store must supply atomic expected-parent append and writer fencing.
pub(crate) struct ParallelOccurrenceCheckpointer {
    inner: Arc<dyn ParallelCheckpointAppender>,
}

impl ParallelOccurrenceCheckpointer {
    pub(crate) fn new(inner: Arc<dyn ParallelCheckpointAppender>) -> Self {
        Self { inner }
    }

    pub(super) async fn freeze(
        &self,
        activation: &ParallelActivation,
        context: &NodeContext,
        branches: Vec<FrozenBranchInput>,
        origin: ParallelChildOrigin,
        child_threads: Vec<String>,
    ) -> Result<FrozenOccurrence, GraphError> {
        let expected = FrozenOccurrence {
            activation: activation.clone(),
            branches,
            origin,
            child_threads,
            cards: Vec::new(),
            decisions: None,
            resume_inputs: BTreeMap::new(),
            blocked: None,
        };
        validate_state(&context.state)?;
        let latest = self.inner.load(&activation.root_thread_id).await?;
        if let Some(checkpoint) = latest.as_ref() {
            super::structure::validate_checkpoint(checkpoint)?;
            validate_freeze_parent(checkpoint, activation, context)?;
            if checkpoint.step == context.step {
                // Only this activation's own frontier can hold its legacy occurrence.
                refuse_legacy(checkpoint)?;
            }
            if checkpoint.step == context.step && checkpoint.metadata.contains_key(OCCURRENCE_KEY) {
                let frozen = occurrence_from(checkpoint, activation)?;
                if frozen.activation != expected.activation || frozen.branches != expected.branches
                {
                    return Err(occurrence_error());
                }
                return Ok(frozen);
            }
        }
        validate_lineage(&expected, activation)?;
        let mut checkpoint = match latest.as_ref() {
            Some(checkpoint) if checkpoint.step == context.step => checkpoint.clone(),
            _ => Checkpoint::new(
                &activation.root_thread_id,
                context.state.clone(),
                context.step,
                vec![activation.node_id.clone()],
            ),
        };
        refresh_identity(&mut checkpoint);
        checkpoint.metadata.insert(
            OCCURRENCE_KEY.to_owned(),
            serde_json::to_value(&expected).map_err(|_| occurrence_error())?,
        );
        super::structure::validate_checkpoint(&checkpoint)?;
        self.inner
            .append_after(latest.as_ref(), &checkpoint)
            .await?;
        Ok(expected)
    }

    pub(super) async fn record_pause(
        &self,
        activation: &ParallelActivation,
        cards: Vec<(usize, ParallelPauseCard)>,
    ) -> Result<(), GraphError> {
        let (parent, mut occurrence) = self.latest_occurrence(activation).await?;
        if occurrence.blocked.is_some() {
            return Err(occurrence_error());
        }
        if occurrence.cards == cards && occurrence.decisions.is_none() {
            return Ok(());
        }
        occurrence.cards = cards;
        occurrence.decisions = None;
        occurrence.resume_inputs.clear();
        self.save_occurrence(&parent, occurrence, None).await
    }

    pub(super) async fn record_decisions(
        &self,
        occurrence: &FrozenOccurrence,
        decisions: Vec<ParallelDecision>,
        resume_inputs: BTreeMap<usize, State>,
        context: &NodeContext,
    ) -> Result<(), GraphError> {
        let (parent, mut current) = self.latest_occurrence(&occurrence.activation).await?;
        if current != *occurrence || current.decisions.is_some() || current.blocked.is_some() {
            return Err(occurrence_error());
        }
        if context.config.thread_id != parent.thread_id
            || context.step != parent.step
            || business_state(&context.state) != business_state(&parent.state)
        {
            return Err(occurrence_error());
        }
        current.decisions = Some(decisions);
        current.resume_inputs = resume_inputs;
        self.save_occurrence(&parent, current, Some(&context.state))
            .await
    }

    pub(super) async fn record_blocked(
        &self,
        activation: &ParallelActivation,
        blocked: ParallelBlocked,
    ) -> Result<(), GraphError> {
        let (parent, mut occurrence) = self.latest_occurrence(activation).await?;
        if let Some(existing) = occurrence.blocked.as_ref() {
            return if existing == &blocked {
                Ok(())
            } else {
                Err(occurrence_error())
            };
        }
        occurrence.blocked = Some(blocked);
        self.save_occurrence(&parent, occurrence, None).await
    }

    /// Classify only the exact saved occurrence receipt, never error text.
    pub(crate) async fn blocked_at(
        &self,
        checkpoint_id: &str,
        activation: &ParallelActivation,
    ) -> Result<Option<ParallelBlocked>, GraphError> {
        let checkpoint = self
            .inner
            .load_by_id(checkpoint_id)
            .await?
            .ok_or_else(occurrence_error)?;
        if checkpoint.checkpoint_id != checkpoint_id {
            return Err(occurrence_error());
        }
        Ok(occurrence_from(&checkpoint, activation)?.blocked)
    }

    async fn latest_occurrence(
        &self,
        activation: &ParallelActivation,
    ) -> Result<(Checkpoint, FrozenOccurrence), GraphError> {
        let latest = self
            .inner
            .load(&activation.root_thread_id)
            .await?
            .ok_or_else(occurrence_error)?;
        let occurrence = occurrence_from(&latest, activation)?;
        Ok((latest, occurrence))
    }

    async fn save_occurrence(
        &self,
        parent: &Checkpoint,
        occurrence: FrozenOccurrence,
        resume: Option<&State>,
    ) -> Result<(), GraphError> {
        // Retain the checked snapshot. A second unchecked load would let a stale
        // receipt attach to a newer frontier. The store compares this exact parent.
        occurrence_from(parent, &occurrence.activation)?;
        super::structure::validate_checkpoint(parent)?;
        if let Some(resume) = resume {
            validate_state(resume)?;
        }
        let mut checkpoint = parent.clone();
        refresh_identity(&mut checkpoint);
        if let Some(resume) = resume {
            let value = resume
                .get(PARALLEL_RESUME_STATE_KEY)
                .ok_or_else(occurrence_error)?;
            checkpoint
                .state
                .insert(PARALLEL_RESUME_STATE_KEY.to_owned(), value.clone());
        }
        checkpoint.metadata.insert(
            OCCURRENCE_KEY.to_owned(),
            serde_json::to_value(occurrence).map_err(|_| occurrence_error())?,
        );
        super::structure::validate_checkpoint(&checkpoint)?;
        self.inner.append_after(Some(parent), &checkpoint).await?;
        Ok(())
    }
}

fn validate_freeze_parent(
    checkpoint: &Checkpoint,
    activation: &ParallelActivation,
    context: &NodeContext,
) -> Result<(), GraphError> {
    if checkpoint.thread_id != activation.root_thread_id
        || checkpoint.step > context.step
        || (checkpoint.step == context.step
            && (checkpoint.pending_nodes.as_slice() != [activation.node_id.as_str()]
                || business_state(&checkpoint.state) != business_state(&context.state)))
    {
        return Err(occurrence_error());
    }
    Ok(())
}

pub(super) fn occurrence_from(
    checkpoint: &Checkpoint,
    activation: &ParallelActivation,
) -> Result<FrozenOccurrence, GraphError> {
    super::structure::validate_checkpoint(checkpoint)?;
    refuse_legacy(checkpoint)?;
    let raw = checkpoint
        .metadata
        .get(OCCURRENCE_KEY)
        .ok_or_else(occurrence_error)?;
    let occurrence: FrozenOccurrence =
        serde_json::from_value(raw.clone()).map_err(|_| occurrence_error())?;
    if checkpoint.thread_id != activation.root_thread_id
        || occurrence.activation != *activation
        || u64::try_from(checkpoint.step).map_err(|_| occurrence_error())? != activation.step
        || checkpoint.pending_nodes.as_slice() != [activation.node_id.as_str()]
    {
        return Err(occurrence_error());
    }
    validate_lineage(&occurrence, activation)?;
    if let Some(blocked) = occurrence.blocked.as_ref()
        && !occurrence.branches.iter().any(|branch| {
            branch.ordinal == blocked.ordinal
                && branch.branch_id == blocked.branch_id
                && branch.node == blocked.node
        })
    {
        return Err(occurrence_error());
    }
    Ok(occurrence)
}

/// The previous occurrence format froze no child identity. Refuse it by type.
fn refuse_legacy(checkpoint: &Checkpoint) -> Result<(), GraphError> {
    if checkpoint.metadata.contains_key(LEGACY_OCCURRENCE_KEY) {
        return Err(parallel_error(
            "graph.parallel.unsupported_occurrence",
            "the parallel occurrence format is no longer supported",
        ));
    }
    Ok(())
}

fn validate_lineage(
    occurrence: &FrozenOccurrence,
    activation: &ParallelActivation,
) -> Result<(), GraphError> {
    occurrence
        .origin
        .validate()
        .map_err(|_| occurrence_error())?;
    let threads = &occurrence.child_threads;
    if threads.len() != occurrence.branches.len()
        || threads.iter().any(|thread| {
            thread.is_empty()
                || thread.len() > 512
                || thread.chars().any(char::is_control)
                || *thread == activation.root_thread_id
        })
        || threads.iter().collect::<BTreeSet<_>>().len() != threads.len()
    {
        return Err(occurrence_error());
    }
    Ok(())
}

fn refresh_identity(checkpoint: &mut Checkpoint) {
    let fresh = Checkpoint::new(
        &checkpoint.thread_id,
        checkpoint.state.clone(),
        checkpoint.step,
        checkpoint.pending_nodes.clone(),
    );
    // A new Parallel revision consumes the prior graph-call append marker.
    // Its graph-call receipts remain owned by the same exact scope.
    checkpoint
        .metadata
        .remove(crate::agents::pipeline::scope_receipts::GRAPH_CALL_REVISION_METADATA_KEY);
    checkpoint.checkpoint_id = fresh.checkpoint_id;
    checkpoint.created_at = fresh.created_at;
}

#[async_trait]
impl Checkpointer for ParallelOccurrenceCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        // An exact immutable replay must retain its original bytes. In particular,
        // do not attach a later occurrence envelope to a pre-freeze checkpoint ID.
        if self
            .inner
            .load_by_id(&checkpoint.checkpoint_id)
            .await?
            .is_some()
        {
            return self.inner.append_after(None, checkpoint).await;
        }
        super::structure::validate_checkpoint(checkpoint)?;
        let mut candidate = checkpoint.clone();
        let latest = self.inner.load(&checkpoint.thread_id).await?;
        if let Some(previous) = latest.as_ref() {
            super::structure::validate_checkpoint(previous)?;
            if previous.thread_id != checkpoint.thread_id || checkpoint.step < previous.step {
                return Err(occurrence_error());
            }
            if let Some(raw) = previous.metadata.get(OCCURRENCE_KEY) {
                let occurrence: FrozenOccurrence =
                    serde_json::from_value(raw.clone()).map_err(|_| occurrence_error())?;
                occurrence_from(previous, &occurrence.activation)?;
                let step =
                    usize::try_from(occurrence.activation.step).map_err(|_| occurrence_error())?;
                if checkpoint.step == step
                    && checkpoint.pending_nodes.as_slice()
                        == [occurrence.activation.node_id.as_str()]
                {
                    if business_state(&checkpoint.state) != business_state(&previous.state) {
                        return Err(occurrence_error());
                    }
                    candidate
                        .metadata
                        .insert(OCCURRENCE_KEY.to_owned(), raw.clone());
                } else if occurrence.blocked.is_some() {
                    return Err(occurrence_error());
                } else {
                    candidate.metadata.remove(OCCURRENCE_KEY);
                }
            }
        }
        super::structure::validate_checkpoint(&candidate)?;
        self.inner.append_after(latest.as_ref(), &candidate).await
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
pub(super) enum BranchReceipt {
    Completed,
    Paused { interrupt: Interrupt },
    Failed { code: String },
}

pub(super) struct BranchReceiptCheckpointer {
    inner: Arc<dyn Checkpointer>,
    thread_id: String,
    captured: Mutex<Option<BranchReceipt>>,
}

impl BranchReceiptCheckpointer {
    pub(super) fn new(inner: Arc<dyn Checkpointer>, thread_id: String) -> Self {
        Self {
            inner,
            thread_id,
            captured: Mutex::new(None),
        }
    }

    fn capture(&self, receipt: BranchReceipt) -> Result<(), GraphError> {
        *self.captured.lock().map_err(|_| receipt_error())? = Some(receipt);
        Ok(())
    }

    pub(super) fn receipt(checkpoint: &Checkpoint) -> Result<Option<BranchReceipt>, GraphError> {
        super::structure::validate_checkpoint(checkpoint)?;
        checkpoint
            .metadata
            .get(BRANCH_RECEIPT_KEY)
            .map(|raw| serde_json::from_value(raw.clone()).map_err(|_| receipt_error()))
            .transpose()
    }
}

#[async_trait]
impl Checkpointer for BranchReceiptCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        if checkpoint.thread_id != self.thread_id {
            return self.inner.save(checkpoint).await;
        }
        super::structure::validate_checkpoint(checkpoint)?;
        let receipt = self.captured.lock().map_err(|_| receipt_error())?.clone();
        let mut checkpoint = checkpoint.clone();
        if let Some(receipt) = receipt {
            if matches!(receipt, BranchReceipt::Completed) && !checkpoint.pending_nodes.is_empty() {
                return Err(receipt_error());
            }
            checkpoint.metadata.insert(
                BRANCH_RECEIPT_KEY.to_owned(),
                serde_json::to_value(receipt).map_err(|_| receipt_error())?,
            );
        }
        // Typed receipt wrappers add depth and values around pause data.
        // Validate what is actually saved, as replay validates the full metadata.
        super::structure::validate_checkpoint(&checkpoint)?;
        self.inner.save(&checkpoint).await
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

/// Compiler-owned branches contain exactly one wrapped Agent node.
/// The receipt is captured before ADK saves the pause or terminal checkpoint.
pub(crate) struct ParallelBranchExecution {
    checkpoint: Arc<BranchReceiptCheckpointer>,
    activation: ParallelActivation,
    owned_node: String,
    ordinal: usize,
    thread_id: String,
    admitted_threads: BTreeSet<String>,
    parent_event_scope: Option<serde_json::Value>,
}

impl ParallelBranchExecution {
    #[allow(clippy::too_many_arguments)] // Preserve the exact factory proof separately from business input.
    pub(super) fn new(
        checkpoint: Arc<BranchReceiptCheckpointer>,
        activation: ParallelActivation,
        owned_node: String,
        ordinal: usize,
        thread_id: String,
        admitted_threads: BTreeSet<String>,
        parent_event_scope: Option<serde_json::Value>,
    ) -> Self {
        Self {
            checkpoint,
            activation,
            owned_node,
            ordinal,
            thread_id,
            admitted_threads,
            parent_event_scope,
        }
    }

    pub(crate) fn activation_root_thread_id(&self) -> &str {
        &self.activation.root_thread_id
    }
    pub(crate) fn branch_thread_id(&self) -> &str {
        &self.thread_id
    }
    pub(crate) fn owned_node(&self) -> &str {
        &self.owned_node
    }
    pub(crate) fn branch_ordinal(&self) -> usize {
        self.ordinal
    }
    pub(crate) fn admitted_threads(&self) -> &BTreeSet<String> {
        &self.admitted_threads
    }
    pub(crate) fn parent_event_scope_value(&self) -> Option<&serde_json::Value> {
        self.parent_event_scope.as_ref()
    }

    pub(crate) fn checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.checkpoint.clone()
    }

    pub(crate) fn wrap_node<N: Node + 'static>(&self, node: N) -> ParallelBranchLifecycleNode<N> {
        ParallelBranchLifecycleNode {
            node,
            checkpoint: Arc::clone(&self.checkpoint),
        }
    }
}

pub(crate) struct ParallelBranchLifecycleNode<N> {
    node: N,
    checkpoint: Arc<BranchReceiptCheckpointer>,
}

#[async_trait]
impl<N: Node + 'static> Node for ParallelBranchLifecycleNode<N> {
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

    fn validate_against(&self, parent: &adk_rust::graph::StateSchema) -> Result<(), GraphError> {
        self.node.validate_against(parent)
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        validate_state(&context.state)?;
        match self.node.execute(context).await {
            Ok(output) => {
                validate_values(output.updates.values())?;
                if let Some(interrupt) = output.interrupt.as_ref() {
                    validate_interrupt(interrupt)?;
                }
                if output.goto_parent.is_some() {
                    return Err(parallel_error(
                        "graph.parallel.unsupported_parent_route",
                        "a parallel branch cannot route the parent graph",
                    ));
                }
                let receipt = output
                    .interrupt
                    .clone()
                    .map_or(BranchReceipt::Completed, |interrupt| {
                        BranchReceipt::Paused { interrupt }
                    });
                self.checkpoint.capture(receipt)?;
                Ok(output)
            }
            Err(GraphError::Interrupted(interrupted)) => {
                let interrupt = interrupted.interrupt;
                validate_interrupt(&interrupt)?;
                self.checkpoint.capture(BranchReceipt::Paused {
                    interrupt: interrupt.clone(),
                })?;
                Ok(NodeOutput::new().with_interrupt(interrupt))
            }
            Err(error) => {
                // Lease loss and cancellation are control stops: record nothing.
                if is_lease_lost(&error)
                    || context
                        .config
                        .parent_context
                        .as_ref()
                        .is_some_and(|parent| parent.is_cancelled())
                {
                    return Err(error);
                }
                let code = super::graph_error_code(&error).to_owned();
                self.checkpoint.capture(BranchReceipt::Failed { code })?;
                let checkpoint = Checkpoint::new(
                    &context.config.thread_id,
                    context.state.clone(),
                    context.step,
                    vec![self.name().to_owned()],
                );
                self.checkpoint.save(&checkpoint).await?;
                Err(error)
            }
        }
    }
}

fn occurrence_error() -> GraphError {
    parallel_error(
        "graph.parallel.stale_activation",
        "the parallel occurrence does not match its checkpoint",
    )
}

fn receipt_error() -> GraphError {
    parallel_error(
        "graph.parallel.corrupt_receipt",
        "the parallel child checkpoint receipt is invalid",
    )
}

fn validate_interrupt(interrupt: &Interrupt) -> Result<(), GraphError> {
    if let Interrupt::Dynamic {
        data: Some(value), ..
    } = interrupt
    {
        super::ensure_bounded_json(value, super::MAX_PAUSE_BYTES, "pause")?;
    }
    Ok(())
}

#[cfg(test)]
mod receipt_guard_tests {
    use super::*;
    use adk_rust::graph::MemoryCheckpointer;
    use serde_json::Value;

    #[tokio::test]
    async fn parallel_augmented_pause_receipt_rejects_raw_boundary_before_store_write() {
        let mut deep = Value::Null;
        for _ in 0..125 {
            deep = Value::Array(vec![deep]);
        }
        let wide = Value::Array(vec![Value::Null; 99_999]);
        for data in [deep, wide] {
            let store = Arc::new(MemoryCheckpointer::new());
            let checkpoint = BranchReceiptCheckpointer::new(store.clone(), "branch".into());
            let pause = Interrupt::Dynamic {
                message: "boundary".into(),
                data: Some(data),
            };
            validate_interrupt(&pause).unwrap();
            checkpoint
                .capture(BranchReceipt::Paused { interrupt: pause })
                .unwrap();
            let candidate = Checkpoint::new("branch", State::new(), 0, vec!["owned".into()]);
            super::super::structure::validate_checkpoint(&candidate).unwrap();
            assert!(checkpoint.save(&candidate).await.is_err());
            assert!(store.list("branch").await.unwrap().is_empty());
        }
    }
}
