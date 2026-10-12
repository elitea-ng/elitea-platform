//! Frozen item fan-out. Production compiler admission remains disabled.
#![allow(dead_code)] // The private compiler packet composes these explicit seams later.

use std::collections::{BTreeMap, BTreeSet};
use std::io::{self, Write};
use std::sync::Arc;

use adk_rust::futures::{StreamExt, stream::FuturesUnordered};
use adk_rust::graph::{
    CompiledGraph, ExecutionConfig, GraphError, Node, NodeContext, NodeOutput, State,
};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

use super::fanout_control::{FanoutCancellation, is_lease_lost, latch_cancelled};

#[path = "map_reduce_checkpoint.rs"]
mod checkpoint;
pub(crate) use checkpoint::{MapItemExecution, MapOccurrenceCheckpointer};
use checkpoint::{MapItemReceipt, MapReceiptCheckpointer};

pub(crate) const MAX_ITEMS: usize = 64;
pub(crate) const MAX_CONCURRENCY: usize = 8;
pub(crate) const MAX_ITEM_BYTES: usize = 512 * 1024;
pub(crate) const MAX_PLAN_BYTES: usize = 2 * 1024 * 1024;
pub(crate) const MAX_COLLECTED_BYTES: usize = 8 * 1024 * 1024;
pub(crate) const MAX_CHECKPOINT_BYTES: usize = 8 * 1024 * 1024;
// A typed state channel at its bound fits the whole-state fan-out boundary.
const _: () = assert!(super::state_reducers::MAX_REDUCED_BYTES <= MAX_CHECKPOINT_BYTES);

/// Sequence fields retain compiler declaration order. They are never sorted.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct MapDefinition {
    pub(crate) id: String,
    pub(crate) worker: String,
    pub(crate) source: String,
    pub(crate) item: String,
    pub(crate) index: String,
    pub(crate) broadcast: Vec<String>,
    pub(crate) outputs: Vec<String>,
    pub(crate) destination: String,
    pub(crate) max_items: usize,
    pub(crate) max_concurrency: usize,
    /// V1 supports ordered collection only. The parent schema owns its reducer.
    pub(crate) reduction: MapReduction,
}

#[derive(Clone, Copy, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum MapReduction {
    OrderedCollection,
}

impl MapDefinition {
    pub(crate) fn validate(&self) -> Result<(), GraphError> {
        if !super::yaml::valid_graph_id(&self.id)
            || !super::yaml::valid_graph_id(&self.worker)
            || self.id == self.worker
            || !(1..=MAX_ITEMS).contains(&self.max_items)
            || !(1..=MAX_CONCURRENCY).contains(&self.max_concurrency)
            || self.broadcast.len() > 64
            || self.outputs.is_empty()
            || self.outputs.len() > 64
        {
            return Err(map_error("invalid_configuration"));
        }
        for key in [&self.source, &self.item, &self.index, &self.destination]
            .into_iter()
            .chain(self.broadcast.iter())
            .chain(self.outputs.iter())
        {
            if !super::yaml::valid_output_key(key)
                || key == "messages"
                || super::compiler::reserved_user_state_key(key)
            {
                return Err(map_error("invalid_configuration"));
            }
        }
        if self.item == self.index
            || self.broadcast.contains(&self.item)
            || self.broadcast.contains(&self.index)
            || self.outputs.contains(&self.item)
            || self.outputs.contains(&self.index)
            || self.destination == self.item
            || self.destination == self.index
            || has_duplicates(&self.broadcast)
            || has_duplicates(&self.outputs)
        {
            return Err(map_error("invalid_configuration"));
        }
        Ok(())
    }

    pub(super) fn freeze(
        &self,
        context: &NodeContext,
        worker_digest: [u8; 32],
        origin: MapExecutionIdentity,
    ) -> Result<FrozenMap, GraphError> {
        self.validate()?;
        validate_state_structure(&context.state)?;
        if context.config.thread_id.is_empty()
            || context.config.thread_id.len() > 512
            || context.config.thread_id.chars().any(char::is_control)
            || worker_digest == [0; 32]
        {
            return Err(map_error("invalid_activation"));
        }
        let items = context
            .state
            .get(&self.source)
            .and_then(Value::as_array)
            .ok_or_else(|| map_error("invalid_source"))?;
        if items.len() > self.max_items {
            return Err(map_error("resource_exhausted"));
        }
        validate_values(items.iter())?;
        bounded(items, MAX_PLAN_BYTES)?;
        let source_digest = hash_json(b"elitea.graph.map.source.v1\0", items, MAX_PLAN_BYTES)?;
        let config_digest = hash_json(b"elitea.graph.map.config.v1\0", self, MAX_PLAN_BYTES)?;
        let activation = MapActivation {
            root_thread_id: context.config.thread_id.clone(),
            node_id: self.id.clone(),
            step: u64::try_from(context.step).map_err(|_| map_error("resource_exhausted"))?,
            config_digest,
            worker_digest,
            source_digest,
        };
        let mut broadcast = State::new();
        for key in &self.broadcast {
            let value = context
                .state
                .get(key)
                .ok_or_else(|| map_error("invalid_mapping"))?;
            validate_value(value)?;
            bounded(value, MAX_ITEM_BYTES)?;
            broadcast.insert(key.clone(), value.clone());
            bounded(&broadcast, MAX_ITEM_BYTES)?;
        }
        bounded(&broadcast, MAX_ITEM_BYTES)?;
        let mut frozen = Vec::with_capacity(items.len());
        for (index, item) in items.iter().enumerate() {
            let mut input = broadcast.clone();
            input.insert(self.item.clone(), item.clone());
            input.insert(self.index.clone(), json!(index));
            bounded(&input, MAX_ITEM_BYTES)?;
            let input_digest =
                hash_state(b"elitea.graph.map.item-input.v1\0", &input, MAX_ITEM_BYTES)?;
            frozen.push(FrozenMapItem {
                index,
                input_digest,
                input,
            });
            bounded(&frozen, MAX_PLAN_BYTES)?;
        }
        let plan = FrozenMap {
            activation,
            items: frozen,
            origin,
            // Minted by the child factory once the activation and items exist.
            item_threads: Vec::new(),
            stop: None,
        };
        bounded(&plan, MAX_PLAN_BYTES)?;
        Ok(plan)
    }
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct MapActivation {
    pub(crate) root_thread_id: String,
    pub(crate) node_id: String,
    pub(crate) step: u64,
    pub(crate) config_digest: [u8; 32],
    pub(crate) worker_digest: [u8; 32],
    pub(crate) source_digest: [u8; 32],
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FrozenMapItem {
    pub(crate) index: usize,
    pub(crate) input_digest: [u8; 32],
    pub(crate) input: State,
}

/// The opaque authority factory adds tenant, execution and generation binding.
/// This public digest label contains no item data and grants no authority.
pub(crate) fn item_identity(
    activation: &MapActivation,
    item: &FrozenMapItem,
) -> Result<[u8; 32], GraphError> {
    hash_json(
        b"elitea.graph.map.item-identity.v1\0",
        &(activation, item.index, item.input_digest),
        MAX_PLAN_BYTES,
    )
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FrozenMap {
    pub(crate) activation: MapActivation,
    pub(crate) items: Vec<FrozenMapItem>,
    /// The execution identity under which the occurrence first froze its items.
    pub(crate) origin: MapExecutionIdentity,
    /// Frozen child threads, index-aligned with `items`.
    pub(crate) item_threads: Vec<String>,
    pub(crate) stop: Option<MapStop>,
}

/// Typed non-success. Public classification must read the exact saved receipt.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(tag = "status", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum MapStop {
    Failed { index: usize },
    ResourceExhausted { index: usize },
    UnexpectedPause { index: usize },
    Blocked { index: usize },
    Cancelled,
    DeadlineExceeded,
}

pub(crate) enum MapNodeOutcome {
    Completed(NodeOutput),
    Stopped(MapStop),
}

pub(crate) struct MapChildCheckpoint {
    pub(crate) thread_id: String,
    pub(crate) checkpointer: Arc<dyn adk_rust::graph::Checkpointer>,
    pub(crate) admitted_threads: BTreeSet<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum MapWorkerKind {
    StateModifier,
    Application,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct MapExecutionIdentity {
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
}

/// The implementation mints descendant authority from the original parent claim.
/// Hashes and thread names provide identity only. They do not grant authority.
#[async_trait]
pub(crate) trait MapChildCheckpointerFactory: Send + Sync {
    fn execution_identity(&self, _root_thread: &str) -> Result<MapExecutionIdentity, GraphError> {
        Err(map_error("invalid_turn_authority"))
    }
    /// Pure derivation of an item thread from a frozen origin. Activates nothing.
    fn item_thread_id(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        origin: &MapExecutionIdentity,
    ) -> Result<String, GraphError>;
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
        origin: &MapExecutionIdentity,
    ) -> Result<MapChildCheckpoint, GraphError>;
}

/// The compiler owns one admitted worker. It reuses normal input/output mappings.
pub(crate) trait MapWorkerGraphFactory: Send + Sync {
    /// Reject possible pauses, parent routes, unreceipted effects and recursive maps.
    fn validate_nonpausing(&self, definition: &MapDefinition) -> Result<(), GraphError>;
    fn owned_definition_digest(&self) -> [u8; 32];
    fn worker_kind(&self) -> MapWorkerKind;
    /// Compile exactly one wrapped worker, then END, with this item checkpointer.
    fn compile_item(
        &self,
        definition: &MapDefinition,
        execution: &MapItemExecution,
    ) -> Result<CompiledGraph, GraphError>;
    /// Add only compiler-owned runtime controls. Business values remain frozen.
    fn runtime_input(
        &self,
        input: &State,
        context: &NodeContext,
        thread: &str,
    ) -> Result<State, GraphError>;
    fn project_result(
        &self,
        definition: &MapDefinition,
        state: &State,
    ) -> Result<MapWorkerTerminal, GraphError>;
    /// Validate the whole candidate after the destination reducer runs once.
    /// Keep the exact compiler state-declaration sequence during validation.
    fn validate_candidate(&self, candidate: &State) -> Result<(), GraphError>;
}

pub(crate) enum MapWorkerTerminal {
    Completed(Map<String, Value>),
    Blocked,
}

pub(crate) struct DurableMapNode {
    definition: MapDefinition,
    parent: Arc<MapOccurrenceCheckpointer>,
    children: Arc<dyn MapChildCheckpointerFactory>,
    graphs: Arc<dyn MapWorkerGraphFactory>,
    deadline: Option<tokio::time::Instant>,
    cancellation: Option<Arc<FanoutCancellation>>,
}

impl DurableMapNode {
    pub(crate) fn new(
        definition: MapDefinition,
        parent: Arc<MapOccurrenceCheckpointer>,
        children: Arc<dyn MapChildCheckpointerFactory>,
        graphs: Arc<dyn MapWorkerGraphFactory>,
    ) -> Result<Self, GraphError> {
        definition.validate()?;
        graphs.validate_nonpausing(&definition)?;
        Ok(Self {
            definition,
            parent,
            children,
            graphs,
            deadline: None,
            cancellation: None,
        })
    }

    /// Supply the owner's cancellation latch. The owner fires it; the node never polls.
    pub(crate) fn with_cancellation(mut self, cancellation: Arc<FanoutCancellation>) -> Self {
        self.cancellation = Some(cancellation);
        self
    }

    pub(crate) fn with_deadline(mut self, deadline: tokio::time::Instant) -> Self {
        self.deadline = Some(deadline);
        self
    }

    pub(crate) fn parent_checkpointer(&self) -> Arc<dyn adk_rust::graph::Checkpointer> {
        self.parent.clone()
    }

    fn is_cancelled(&self, context: &NodeContext) -> bool {
        cancelled(context)
            || self
                .cancellation
                .as_deref()
                .is_some_and(FanoutCancellation::is_cancelled)
    }

    /// Cancellation is a control stop: the owner decides again on the next
    /// claim, so it is never recorded. Every other stop stays durable.
    async fn settle_stop(
        &self,
        activation: &MapActivation,
        stop: MapStop,
    ) -> Result<MapNodeOutcome, GraphError> {
        if stop == MapStop::Cancelled {
            return Err(map_error("cancelled"));
        }
        self.parent.record_stop(activation, stop.clone()).await?;
        Ok(MapNodeOutcome::Stopped(stop))
    }

    fn control_stop(&self, context: &NodeContext) -> Option<MapStop> {
        if self.is_cancelled(context) {
            Some(MapStop::Cancelled)
        } else if self
            .deadline
            .is_some_and(|deadline| deadline <= tokio::time::Instant::now())
        {
            Some(MapStop::DeadlineExceeded)
        } else {
            None
        }
    }

    #[allow(clippy::too_many_lines)] // Keep freeze, bounded admission, drain and atomic collection ordering together.
    pub(crate) async fn execute_outcome(
        &self,
        context: &NodeContext,
    ) -> Result<MapNodeOutcome, GraphError> {
        let origin = self
            .children
            .execution_identity(&context.config.thread_id)?;
        let mut proposed = self.definition.freeze(
            context,
            self.graphs.owned_definition_digest(),
            origin.clone(),
        )?;
        for item in &proposed.items {
            proposed.item_threads.push(self.children.item_thread_id(
                &proposed.activation,
                item,
                &self.definition.worker,
                &origin,
            )?);
        }
        bounded(&proposed, MAX_PLAN_BYTES)?;
        let plan = self.parent.freeze(context, proposed).await?;
        if let Some(stop) = plan.stop {
            return Ok(MapNodeOutcome::Stopped(stop));
        }
        if let Some(stop) = self.control_stop(context) {
            return self.settle_stop(&plan.activation, stop).await;
        }
        let schema = context
            .parent_schema()
            .ok_or_else(|| map_error("missing_state_schema"))?;
        if !matches!(
            schema.get_reducer(&self.definition.destination),
            adk_rust::graph::Reducer::Overwrite | adk_rust::graph::Reducer::Append
        ) || !schema.channels.contains_key(&self.definition.destination)
        {
            return Err(map_error("invalid_reducer"));
        }
        let mut prepared = Vec::with_capacity(plan.items.len());
        let mut threads = BTreeSet::new();
        for (index, item) in plan.items.iter().enumerate() {
            if let Some(stop) = self.control_stop(context) {
                return self.settle_stop(&plan.activation, stop).await;
            }
            let admission = self.children.for_item(
                &plan.activation,
                item,
                &self.definition.worker,
                self.graphs.worker_kind(),
                &plan.origin,
            );
            let child = match self.deadline {
                Some(deadline) => {
                    if let Ok(child) = tokio::time::timeout_at(deadline, admission).await {
                        child?
                    } else {
                        self.parent
                            .record_stop(&plan.activation, MapStop::DeadlineExceeded)
                            .await?;
                        return Ok(MapNodeOutcome::Stopped(MapStop::DeadlineExceeded));
                    }
                }
                None => admission.await?,
            };
            if plan.item_threads.get(index) != Some(&child.thread_id) {
                return Err(map_error("corrupt_occurrence"));
            }
            if child.thread_id.is_empty()
                || child.thread_id.len() > 512
                || child.thread_id == plan.activation.root_thread_id
                || child.admitted_threads.is_empty()
                || child.admitted_threads.len() > 129
                || !child.admitted_threads.contains(&child.thread_id)
                || child
                    .admitted_threads
                    .contains(&plan.activation.root_thread_id)
                || (self.graphs.worker_kind() == MapWorkerKind::StateModifier
                    && child.admitted_threads.len() != 1)
                || child.admitted_threads.iter().any(|thread| {
                    thread != &child.thread_id
                        && !thread.starts_with(&format!(
                            "{}/{}/",
                            child.thread_id, self.definition.worker
                        ))
                        && thread != &format!("{}/{}", child.thread_id, self.definition.worker)
                })
                || !threads.insert(child.thread_id.clone())
            {
                return Err(map_error("invalid_child_scope"));
            }
            prepared.push((item.clone(), child));
        }
        // Fired on lease loss so admitted siblings stop instead of running to completion.
        let siblings = FanoutCancellation::new();
        let mut pending = prepared.into_iter();
        let mut running = FuturesUnordered::new();
        let mut outcomes = Vec::with_capacity(plan.items.len());
        let mut collected_bytes = 0usize;
        let expired = self
            .deadline
            .is_some_and(|deadline| deadline <= tokio::time::Instant::now());
        let mut admission_open = !self.is_cancelled(context) && !expired;
        if admission_open {
            for _ in 0..self.definition.max_concurrency {
                if let Some((item, child)) = pending.next() {
                    running.push(self.run_item(item, child, context, &siblings));
                }
            }
        }
        let mut lease_error = None;
        while let Some((index, result)) = running.next().await {
            let mut result = match result {
                Ok(result) => result,
                Err(error) => {
                    // Lease loss: close admission and stop the admitted siblings.
                    lease_error.get_or_insert(error);
                    admission_open = false;
                    siblings.cancel();
                    continue;
                }
            };
            if let Ok(outputs) = &result {
                match serialized_len(outputs, MAX_ITEM_BYTES) {
                    Ok(size)
                        if size.saturating_add(32)
                            <= MAX_COLLECTED_BYTES.saturating_sub(collected_bytes) =>
                    {
                        collected_bytes += size.saturating_add(32);
                    }
                    _ => result = Err(MapStop::ResourceExhausted { index }),
                }
            }
            if result.is_err() || self.is_cancelled(context) {
                admission_open = false;
            }
            outcomes.push((index, result));
            context.report_progress();
            if admission_open && let Some((item, child)) = pending.next() {
                running.push(self.run_item(item, child, context, &siblings));
            }
        }
        if let Some(error) = lease_error {
            return Err(error);
        }
        outcomes.sort_by_key(|(index, _)| *index);
        let stop = if let Some(stop) = self.control_stop(context) {
            Some(stop)
        } else {
            outcomes
                .iter()
                .find_map(|(_, result)| result.as_ref().err().cloned())
        };
        if let Some(stop) = stop {
            return self.settle_stop(&plan.activation, stop).await;
        }
        if outcomes.len() != plan.items.len() {
            return Err(map_error("incomplete_collection"));
        }
        let values = outcomes
            .into_iter()
            .map(|(index, result)| {
                result
                    .map(|outputs| json!({"index":index,"outputs":outputs}))
                    .map_err(|_| map_error("incomplete_collection"))
            })
            .collect::<Result<Vec<_>, _>>()?;
        let aggregate = Value::Array(values);
        validate_value(&aggregate)?;
        bounded(&aggregate, MAX_COLLECTED_BYTES)?;
        let mut candidate = context.state.clone();
        schema.apply_update(
            &mut candidate,
            &self.definition.destination,
            aggregate.clone(),
        );
        validate_state_boundary(&candidate, MAX_CHECKPOINT_BYTES)?;
        self.graphs.validate_candidate(&candidate)?;
        self.parent
            .validate_completion(&plan.activation, &candidate)
            .await?;
        // ADK applies this one delta later. The candidate above is validation only.
        Ok(MapNodeOutcome::Completed(
            NodeOutput::new().with_update(&self.definition.destination, aggregate),
        ))
    }

    async fn run_item(
        &self,
        item: FrozenMapItem,
        child: MapChildCheckpoint,
        context: &NodeContext,
        siblings: &FanoutCancellation,
    ) -> (
        usize,
        Result<Result<Map<String, Value>, MapStop>, GraphError>,
    ) {
        let index = item.index;
        let result = match self.run_item_inner(item, child, context, siblings).await {
            Err(error) if is_lease_lost(&error) => Err(error),
            Err(_) => Ok(Err(MapStop::Failed { index })),
            Ok(result) => Ok(result),
        };
        (index, result)
    }

    #[allow(clippy::too_many_lines)] // Keep receipt reconciliation, frozen input and original control cutoff in one ordered path.
    async fn run_item_inner(
        &self,
        item: FrozenMapItem,
        child: MapChildCheckpoint,
        context: &NodeContext,
        siblings: &FanoutCancellation,
    ) -> Result<Result<Map<String, Value>, MapStop>, GraphError> {
        let checkpoint = Arc::new(MapReceiptCheckpointer::new(
            child.checkpointer,
            child.thread_id.clone(),
            child.admitted_threads.clone(),
        ));
        if let Some(saved) = checkpoint.load_item().await? {
            if let Some(receipt) = MapReceiptCheckpointer::receipt(&saved)? {
                match receipt {
                    MapItemReceipt::Completed if saved.pending_nodes.is_empty() => {
                        return self.result(item.index, &saved.state);
                    }
                    MapItemReceipt::Completed => return Err(map_error("corrupt_receipt")),
                    MapItemReceipt::Failed => {
                        return Ok(Err(MapStop::Failed { index: item.index }));
                    }
                    MapItemReceipt::UnexpectedPause { .. } => {
                        return Ok(Err(MapStop::UnexpectedPause { index: item.index }));
                    }
                }
            } else if saved.pending_nodes.is_empty() {
                return Err(map_error("corrupt_receipt"));
            }
        }
        if self.is_cancelled(context) {
            return Ok(Err(MapStop::Cancelled));
        }
        let execution = MapItemExecution::new(
            Arc::clone(&checkpoint),
            self.definition.worker.clone(),
            context.config.thread_id.clone(),
            child.admitted_threads,
            context
                .state
                .get(super::node_events::PIPELINE_NODE_EVENT_SCOPE_STATE_KEY)
                .cloned(),
        );
        let graph = self.graphs.compile_item(&self.definition, &execution)?;
        let input = self
            .graphs
            .runtime_input(&item.input, context, &child.thread_id)?;
        validate_state_structure(&input)?;
        for (key, value) in &input {
            if !super::compiler::reserved_user_state_key(key)
                && key != "messages"
                && item.input.get(key) != Some(value)
            {
                return Err(map_error("invalid_mapping"));
            }
        }
        if item
            .input
            .iter()
            .any(|(key, value)| input.get(key) != Some(value))
        {
            return Err(map_error("invalid_mapping"));
        }
        bounded(&input, MAX_ITEM_BYTES)?;
        let mut config = ExecutionConfig::new(&child.thread_id)
            .with_recursion_limit(context.config.recursion_limit);
        if let Some(parent) = &context.config.parent_context {
            config = config.with_parent_context(Arc::clone(parent));
        }
        // A restored graph receives no business delta. Append reducers must not replay input.
        let input = if checkpoint.load_item().await?.is_some() {
            State::new()
        } else {
            input
        };
        // The deadline is absolute and the owner fires the latch: nothing here polls.
        let outcome = tokio::select! {
            biased;
            () = tokio::time::sleep_until(self.deadline.unwrap_or_else(tokio::time::Instant::now)),
                if self.deadline.is_some() => {
                return Ok(Err(MapStop::DeadlineExceeded));
            }
            () = latch_cancelled(self.cancellation.as_deref()) => {
                return Ok(Err(MapStop::Cancelled));
            }
            () = siblings.cancelled() => {
                return Ok(Err(MapStop::Cancelled));
            }
            outcome = graph.invoke_detailed(input, config) => outcome,
        };
        match outcome {
            Ok(outcome) => {
                if outcome.goto_parent.is_some() {
                    return Err(map_error("unsupported_parent_route"));
                }
                let saved = checkpoint
                    .load_item()
                    .await?
                    .ok_or_else(|| map_error("missing_receipt"))?;
                if !saved.pending_nodes.is_empty()
                    || !matches!(
                        MapReceiptCheckpointer::receipt(&saved)?,
                        Some(MapItemReceipt::Completed)
                    )
                {
                    return Err(map_error("missing_receipt"));
                }
                self.result(item.index, &saved.state)
            }
            Err(GraphError::Interrupted(_)) => {
                Ok(Err(MapStop::UnexpectedPause { index: item.index }))
            }
            Err(error) if is_lease_lost(&error) => Err(error),
            Err(_) => Ok(Err(MapStop::Failed { index: item.index })),
        }
    }

    fn result(
        &self,
        index: usize,
        state: &State,
    ) -> Result<Result<Map<String, Value>, MapStop>, GraphError> {
        match self.graphs.project_result(&self.definition, state)? {
            MapWorkerTerminal::Blocked => Ok(Err(MapStop::Blocked { index })),
            MapWorkerTerminal::Completed(outputs) => {
                if outputs.len() != self.definition.outputs.len()
                    || self
                        .definition
                        .outputs
                        .iter()
                        .any(|key| !outputs.contains_key(key))
                {
                    return Err(map_error("invalid_result"));
                }
                validate_values(outputs.values())?;
                bounded(&outputs, MAX_ITEM_BYTES)?;
                Ok(Ok(outputs))
            }
        }
    }
}

#[async_trait]
impl Node for DurableMapNode {
    fn name(&self) -> &str {
        &self.definition.id
    }
    fn validate(&self) -> Result<(), GraphError> {
        self.definition.validate()?;
        self.graphs.validate_nonpausing(&self.definition)
    }
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        match self.execute_outcome(context).await? {
            MapNodeOutcome::Completed(output) => Ok(output),
            MapNodeOutcome::Stopped(_) => Err(map_error("stopped")),
        }
    }
}

fn has_duplicates(values: &[String]) -> bool {
    let mut seen = BTreeSet::new();
    values.iter().any(|value| !seen.insert(value))
}

fn cancelled(context: &NodeContext) -> bool {
    context
        .config
        .parent_context
        .as_ref()
        .is_some_and(|parent| parent.is_cancelled())
}

pub(super) fn map_error(code: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "map".to_owned(),
        message: format!("graph.map.{code}"),
    }
}

pub(super) fn bounded<T: Serialize + ?Sized>(value: &T, maximum: usize) -> Result<(), GraphError> {
    serialized_len(value, maximum).map(|_| ())
}

fn serialized_len<T: Serialize + ?Sized>(value: &T, maximum: usize) -> Result<usize, GraphError> {
    let mut writer = CappedWriter { remaining: maximum };
    serde_json::to_writer(&mut writer, value).map_err(|_| map_error("resource_exhausted"))?;
    Ok(maximum - writer.remaining)
}

struct CappedWriter {
    remaining: usize,
}
impl Write for CappedWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.remaining {
            return Err(io::Error::other("map size limit"));
        }
        self.remaining -= bytes.len();
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn hash_json<T: Serialize + ?Sized>(
    domain: &[u8],
    value: &T,
    maximum: usize,
) -> Result<[u8; 32], GraphError> {
    bounded(value, maximum)?;
    let bytes = serde_json::to_vec(value).map_err(|_| map_error("invalid_value"))?;
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(domain);
    hash.update(&bytes);
    let mut result = [0; 32];
    result.copy_from_slice(hash.finish().as_ref());
    Ok(result)
}

pub(super) fn hash_state(
    domain: &[u8],
    state: &State,
    maximum: usize,
) -> Result<[u8; 32], GraphError> {
    validate_values(state.values())?;
    // State is semantically a keyed map. Config declaration sequences remain separate vectors.
    let ordered = state.iter().collect::<BTreeMap<_, _>>();
    hash_json(domain, &ordered, maximum)
}

pub(super) fn validate_value(value: &Value) -> Result<(), GraphError> {
    validate_values(std::iter::once(value))
}

pub(super) fn validate_values<'a>(
    values: impl Iterator<Item = &'a Value>,
) -> Result<(), GraphError> {
    validate_values_with_limits(values, 48, 32_768)
}

pub(super) fn validate_state_boundary(state: &State, maximum: usize) -> Result<(), GraphError> {
    validate_state_structure(state)?;
    bounded(state, maximum)
}

pub(super) fn validate_state_structure(state: &State) -> Result<(), GraphError> {
    validate_values_with_limits(state.values(), 64, 65_536)
}

pub(crate) fn validate_checkpoint_boundary(
    checkpoint: &adk_rust::graph::Checkpoint,
) -> Result<(), GraphError> {
    validate_values_with_limits(
        checkpoint
            .state
            .values()
            .chain(checkpoint.metadata.values())
            .chain(checkpoint.child_ledger.values()),
        64,
        65_536,
    )?;
    bounded(checkpoint, MAX_CHECKPOINT_BYTES)
}

pub(super) fn validate_metadata(value: &Value) -> Result<(), GraphError> {
    validate_values_with_limits(std::iter::once(value), 64, 65_536)
}

fn validate_values_with_limits<'a>(
    values: impl Iterator<Item = &'a Value>,
    maximum_depth: usize,
    maximum_values: usize,
) -> Result<(), GraphError> {
    fn visit(value: &Value, depth: usize, remaining: &mut usize) -> Result<(), GraphError> {
        if depth == 0 || *remaining == 0 {
            return Err(map_error("resource_exhausted"));
        }
        *remaining -= 1;
        match value {
            Value::Array(values) => {
                for value in values {
                    visit(value, depth - 1, remaining)?;
                }
            }
            Value::Object(values) => {
                for value in values.values() {
                    visit(value, depth - 1, remaining)?;
                }
            }
            _ => {}
        }
        Ok(())
    }
    let mut remaining = maximum_values;
    for value in values {
        visit(value, maximum_depth, &mut remaining)?;
    }
    Ok(())
}
