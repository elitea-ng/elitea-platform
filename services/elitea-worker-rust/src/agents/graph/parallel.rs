#![allow(dead_code)] // Composed by the next full YAML graph compiler slice.

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::io::{self, Write};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use adk_rust::futures::{StreamExt, stream::FuturesUnordered};
use adk_rust::graph::checkpoint::Checkpointer;
use adk_rust::graph::{
    Checkpoint, CompiledGraph, ExecutionConfig, GraphError, Node, NodeContext, NodeOutput, State,
};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::fanout_control::{FanoutCancellation, is_lease_lost, latch_cancelled};
use super::fanout_trace::{self, ActivationOutcome, ChildOutcome, ChildStart, FanoutKind};
use super::yaml::{ParallelBranchDefinition, ParallelNodeDefinition};
use tracing::Instrument as _;

#[path = "parallel_checkpoint.rs"]
mod checkpoint;
use elitea_agent_runtime::graph::parallel_control as control;
#[path = "parallel_structure.rs"]
mod structure;
pub(in crate::agents::graph) use structure::{validate_state, validate_values};
#[path = "parallel_published_resume.rs"]
mod published_resume;
#[cfg(test)]
pub(crate) use checkpoint::MAX_PARENT_ROWS_PER_VISIT;
use checkpoint::{BranchReceipt, BranchReceiptCheckpointer, FrozenBranchInput, FrozenOccurrence};
pub(crate) use checkpoint::{ParallelBranchExecution, ParallelOccurrenceCheckpointer};
#[allow(unused_imports)] // Parent proof is staged until continuation assembly is composed.
pub(crate) use published_resume::ParallelPublishedContinuation;

const MAX_BRANCH_INPUT_BYTES: usize = 8 * 1024 * 1024;
const MAX_BRANCH_RESULT_BYTES: usize = 512 * 1024;
const MAX_JOINED_RESULT_BYTES: usize = 8 * 1024 * 1024;
const MAX_PAUSE_CARDS: usize = 16;
const MAX_PAUSE_BYTES: usize = 512 * 1024;
pub(crate) const PARALLEL_RESUME_STATE_KEY: &str = "__elitea_parallel_resume_v1";
pub(crate) const PARALLEL_INTERRUPT_SCHEMA: &str = "elitea.graph.parallel-interrupt.v1";
const BRANCH_INPUT_DIGEST_DOMAIN: &[u8] = b"elitea.graph.parallel.branch-input.v1\0";

/// Stable activation of one parallel node visit.
///
/// The ADK step is restored unchanged while the node remains pending and moves
/// forward before a later loop visit. The child checkpoint factory adds its
/// opaque definition scope and the occurrence's frozen origin before deriving a
/// child thread.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ParallelActivation {
    pub(crate) root_thread_id: String,
    pub(crate) node_id: String,
    pub(crate) step: u64,
    pub(crate) config_digest: [u8; 32],
}

impl ParallelActivation {
    fn from_context(
        definition: &ParallelNodeDefinition,
        context: &NodeContext,
    ) -> Result<Self, GraphError> {
        if context.config.thread_id.is_empty()
            || context.config.thread_id.len() > 512
            || context.config.thread_id.chars().any(char::is_control)
        {
            return Err(parallel_error(
                "graph.parallel.invalid_invocation",
                "the root thread is malformed",
            ));
        }
        let step = u64::try_from(context.step).map_err(|_| {
            parallel_error(
                "graph.parallel.resource_exhausted",
                "the activation step exceeds its durable range",
            )
        })?;
        Ok(Self {
            root_thread_id: context.config.thread_id.clone(),
            node_id: definition.id().to_owned(),
            step,
            config_digest: definition.config_digest(),
        })
    }
}

/// The execution identity under which an occurrence first froze its children.
/// Restores, continuations and reclaims reuse it; they never re-read their own.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ParallelChildOrigin {
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
}

impl ParallelChildOrigin {
    pub(super) fn validate(&self) -> Result<(), GraphError> {
        if self.execution_id.is_empty()
            || self.execution_id.len() > 256
            || self.execution_id.chars().any(char::is_control)
            || self.generation == 0
        {
            return Err(parallel_error(
                "graph.parallel.corrupt_occurrence",
                "the frozen child origin is invalid",
            ));
        }
        Ok(())
    }
}

/// A child-thread checkpointer minted from the current opaque graph authority.
///
/// The thread ID is safe routing metadata. The checkpointer remains the sole
/// state authority and is bound to that exact descendant thread.
pub(crate) struct ParallelChildCheckpoint {
    pub(crate) thread_id: String,
    pub(crate) checkpointer: Arc<dyn Checkpointer>,
    pub(crate) admitted_threads: BTreeSet<String>,
}

#[async_trait]
pub(crate) trait ParallelChildCheckpointerFactory: Send + Sync {
    /// The current claim's identity, used only when an occurrence first freezes.
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError>;

    /// Pure derivation of a branch thread from a frozen origin. Activates nothing.
    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError>;

    /// Derive the thread from `origin`, then activate its writer under the
    /// current claim authority.
    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError>;

    /// Activate every admitted child writer and read each child's latest
    /// checkpoint, in request order. A durable store batches this into one
    /// activation and one read transaction regardless of the child count.
    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        let mut prepared = Vec::with_capacity(children.len());
        for request in children {
            let child = self
                .for_branch(
                    activation,
                    request.branch,
                    request.ordinal,
                    request.input_digest,
                    origin,
                )
                .await?;
            let latest = child.checkpointer.load(&child.thread_id).await?;
            prepared.push(PreparedChildCheckpoint { child, latest });
        }
        Ok(prepared)
    }
}

/// One admitted child of a frozen occurrence.
pub(crate) struct ParallelChildRequest<'a> {
    pub(crate) branch: &'a ParallelBranchDefinition,
    pub(crate) ordinal: usize,
    pub(crate) input_digest: &'a [u8; 32],
}

/// An activated child writer and the latest checkpoint it held at activation.
pub(crate) struct PreparedChildCheckpoint {
    pub(crate) child: ParallelChildCheckpoint,
    pub(crate) latest: Option<Checkpoint>,
}

/// The latest parent row without its business state: identity plus the
/// frontier and metadata an occurrence wrapper needs to stamp the next save.
#[derive(Clone, Debug)]
pub(crate) struct ParentHead {
    pub(crate) thread_id: String,
    pub(crate) checkpoint_id: String,
    /// The store's append order, when the store has one.
    pub(crate) save_ordinal: Option<i64>,
    pub(crate) step: usize,
    pub(crate) pending_nodes: Vec<String>,
    pub(crate) metadata: HashMap<String, Value>,
    /// The full row, kept only by stores that had to load it anyway, so the
    /// default append never loads it a second time.
    pub(crate) snapshot: Option<Box<Checkpoint>>,
}

impl ParentHead {
    pub(crate) fn of(checkpoint: Checkpoint) -> Self {
        Self {
            thread_id: checkpoint.thread_id.clone(),
            checkpoint_id: checkpoint.checkpoint_id.clone(),
            save_ordinal: None,
            step: checkpoint.step,
            pending_nodes: checkpoint.pending_nodes.clone(),
            metadata: checkpoint.metadata.clone(),
            snapshot: Some(Box::new(checkpoint)),
        }
    }

    /// The head as a checkpoint without business state, for metadata readers.
    pub(crate) fn stateless(&self) -> Checkpoint {
        let mut checkpoint = Checkpoint::new(
            &self.thread_id,
            State::new(),
            self.step,
            self.pending_nodes.clone(),
        );
        checkpoint.checkpoint_id.clone_from(&self.checkpoint_id);
        checkpoint.metadata.clone_from(&self.metadata);
        checkpoint
    }
}

/// What an occurrence wrapper needs before stamping one parent save.
pub(crate) struct ParentSaveProbe {
    /// The candidate id is already stored: an exact immutable replay.
    pub(crate) candidate_exists: bool,
    pub(crate) latest: Option<ParentHead>,
}

/// Atomic append on the existing parent checkpoint store. There is no fallback.
/// The implementation must prove the expected latest row under the same writer
/// lock as insertion. Rows are immutable per checkpoint id, so comparing the
/// latest row's identity is a complete comparison. For a new ID, `None`
/// requires no latest row. Exact immutable by-ID replay must return unchanged
/// without making it latest. The writer lock must also fence exact replay
/// before any parent comparison.
#[async_trait]
pub(crate) trait ParallelCheckpointAppender: Checkpointer {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError>;

    /// One read for a wrapped parent save: whether `candidate_id` is already
    /// stored, and the latest row's head. A durable store reads no state.
    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        let candidate_exists = self.load_by_id(candidate_id).await?.is_some();
        let latest = self.load(thread_id).await?;
        Ok(ParentSaveProbe {
            candidate_exists,
            latest: latest.map(ParentHead::of),
        })
    }

    /// Append only while `expected` is still the latest row, by identity.
    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        // The store's own atomic compare decides; a snapshot spares a reload.
        let Some(expected) = expected else {
            return self.append_after(None, candidate).await;
        };
        if let Some(snapshot) = expected.snapshot.as_deref() {
            return self.append_after(Some(snapshot), candidate).await;
        }
        let latest = self.load(&candidate.thread_id).await?;
        if latest.as_ref().map(|latest| latest.checkpoint_id.as_str())
            != Some(expected.checkpoint_id.as_str())
        {
            return Err(GraphError::CheckpointError(
                "checkpoint.conflict: the expected parent checkpoint is no longer latest"
                    .to_owned(),
            ));
        }
        self.append_after(latest.as_ref(), candidate).await
    }
}

/// One admitted authority supplies both parent append and branch capabilities.
/// Implement this only for the verified scoped holder. Do not add a blanket impl.
pub(crate) trait ParallelCheckpointAuthority:
    ParallelCheckpointAppender + ParallelChildCheckpointerFactory
{
}

/// Compiler seam for one admitted, owned Agent definition.
/// Mapping reads business state only. Resume projection proves the descendant
/// card against its checkpoint and durable session before dispatch.
#[async_trait]
pub(crate) trait ParallelBranchGraphFactory: Send + Sync {
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError>;

    fn owned_definition_digest(
        &self,
        branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError>;

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError>;

    fn project_input(
        &self,
        branch: &ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError>;

    fn project_result(
        &self,
        branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError>;

    fn pause_cards(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError>;

    async fn resume_input(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError>;
}

pub(crate) enum ParallelBranchTerminal {
    Completed(serde_json::Map<String, Value>),
    Blocked,
}

/// Typed denial receipt. Public status must prove this exact parent checkpoint.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ParallelBlocked {
    pub(crate) branch_id: String,
    pub(crate) node: String,
    pub(crate) ordinal: usize,
}

pub(crate) enum ParallelNodeOutcome {
    Completed(NodeOutput),
    Paused(NodeOutput),
    Blocked(ParallelBlocked),
}

pub(crate) struct ParallelBranchPause {
    pub(crate) thread_id: String,
    pub(crate) checkpoint_id: String,
    pub(crate) interrupt: adk_rust::graph::interrupt::Interrupt,
    pub(crate) checkpointer: Arc<dyn Checkpointer>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ParallelPauseCard {
    pub(crate) interrupt_id: String,
    pub(crate) tool_call_id: String,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ParallelDecision {
    pub(crate) interrupt_id: String,
    pub(crate) tool_call_id: String,
    pub(crate) action: String,
    pub(crate) value: String,
}

pub(crate) enum ParallelBranchOutcome {
    Completed(serde_json::Map<String, Value>),
    Paused(Vec<ParallelPauseCard>),
    Blocked,
    Failed(String),
    Cancelled,
    /// The writer fence was lost. A control stop, never a branch outcome.
    LeaseLost(GraphError),
}

pub(crate) enum PreparedParallelActivation {
    Ready(Vec<PreparedParallelBranch>),
    Blocked(ParallelBlocked),
}

pub(crate) struct PreparedParallelBranch {
    branch: ParallelBranchDefinition,
    ordinal: usize,
    input: State,
    checkpoint: Arc<BranchReceiptCheckpointer>,
    thread_id: String,
    admitted_threads: BTreeSet<String>,
    replay: Option<ParallelBranchOutcome>,
}

struct PreparedBranchSet {
    branches: Vec<PreparedParallelBranch>,
    pauses: BTreeMap<usize, ParallelBranchPause>,
    cards: Vec<(usize, ParallelPauseCard)>,
}

type OrderedBranchOutcome = (usize, ParallelBranchDefinition, ParallelBranchOutcome);

#[async_trait]
pub(crate) trait ParallelBranchRuntime: Send + Sync {
    fn validate(&self, definition: &ParallelNodeDefinition) -> Result<(), GraphError>;

    async fn prepare(
        &self,
        activation: &mut ParallelActivation,
        definition: &ParallelNodeDefinition,
        context: &NodeContext,
    ) -> Result<PreparedParallelActivation, GraphError>;

    async fn record_pause(
        &self,
        activation: &ParallelActivation,
        cards: Vec<(usize, ParallelPauseCard)>,
    ) -> Result<(), GraphError>;

    async fn record_blocked(
        &self,
        activation: &ParallelActivation,
        blocked: ParallelBlocked,
    ) -> Result<(), GraphError>;

    async fn invoke(
        &self,
        activation: &ParallelActivation,
        branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> ParallelBranchOutcome;
}

/// ADK-native branch runner with one terminal checkpoint lineage per branch.
pub(crate) struct AdkParallelBranchRuntime {
    checkpoints: Arc<dyn ParallelChildCheckpointerFactory>,
    graphs: Arc<dyn ParallelBranchGraphFactory>,
    parent: Arc<ParallelOccurrenceCheckpointer>,
}

impl AdkParallelBranchRuntime {
    pub(crate) fn new(
        checkpoints: Arc<dyn ParallelChildCheckpointerFactory>,
        graphs: Arc<dyn ParallelBranchGraphFactory>,
        parent: Arc<ParallelOccurrenceCheckpointer>,
    ) -> Self {
        Self {
            checkpoints,
            graphs,
            parent,
        }
    }

    pub(super) fn parent_checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.parent.clone()
    }

    pub(crate) fn occurrence_checkpointer(&self) -> Arc<ParallelOccurrenceCheckpointer> {
        Arc::clone(&self.parent)
    }
}

#[async_trait]
impl ParallelBranchRuntime for AdkParallelBranchRuntime {
    fn validate(&self, definition: &ParallelNodeDefinition) -> Result<(), GraphError> {
        definition.validate().map_err(|error| {
            parallel_error(error.code(), "the parallel node configuration is invalid")
        })?;
        if !(2..=16).contains(&definition.branches().len())
            || !(1..=8).contains(&definition.max_concurrency())
        {
            return Err(parallel_error(
                "graph.parallel.invalid_configuration",
                "the parallel contract requires 2-16 branches and concurrency 1-8",
            ));
        }
        for branch in definition.branches() {
            self.graphs.validate_branch(branch)?;
        }
        Ok(())
    }

    async fn prepare(
        &self,
        activation: &mut ParallelActivation,
        definition: &ParallelNodeDefinition,
        context: &NodeContext,
    ) -> Result<PreparedParallelActivation, GraphError> {
        self.validate(definition)?;
        let inputs = self.project_inputs(activation, definition, &context.state)?;
        let (origin, threads) = self.mint_lineage(activation, definition, &inputs)?;
        let occurrence = self
            .parent
            .freeze(activation, context, inputs, origin, threads)
            .await?;
        if let Some(blocked) = occurrence.blocked.clone() {
            return Ok(PreparedParallelActivation::Blocked(blocked));
        }
        let mut restored = self
            .restore_branches(
                activation,
                definition,
                &occurrence.branches,
                &occurrence.origin,
                &occurrence.child_threads,
            )
            .await?;
        self.prepare_resume(activation, &occurrence, &mut restored, context)
            .await?;
        Ok(PreparedParallelActivation::Ready(restored.branches))
    }

    async fn record_pause(
        &self,
        activation: &ParallelActivation,
        cards: Vec<(usize, ParallelPauseCard)>,
    ) -> Result<(), GraphError> {
        validate_expected_cards(&cards)?;
        self.parent.record_pause(activation, cards).await
    }

    async fn record_blocked(
        &self,
        activation: &ParallelActivation,
        blocked: ParallelBlocked,
    ) -> Result<(), GraphError> {
        self.parent.record_blocked(activation, blocked).await
    }

    async fn invoke(
        &self,
        activation: &ParallelActivation,
        mut branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> ParallelBranchOutcome {
        if let Some(replay) = branch.replay.take() {
            return replay;
        }
        if is_cancelled(context) {
            return ParallelBranchOutcome::Cancelled;
        }
        let result = self.invoke_inner(activation, branch, context).await;
        // Lease loss outranks cancellation: the claim machinery must see it unchanged.
        if let Err(error) = result {
            return if is_lease_lost(&error) {
                ParallelBranchOutcome::LeaseLost(error)
            } else if is_cancelled(context) {
                ParallelBranchOutcome::Cancelled
            } else {
                ParallelBranchOutcome::Failed(graph_error_code(&error).to_owned())
            };
        }
        if is_cancelled(context) {
            return ParallelBranchOutcome::Cancelled;
        }
        result.unwrap_or_else(|error| {
            ParallelBranchOutcome::Failed(graph_error_code(&error).to_owned())
        })
    }
}

impl AdkParallelBranchRuntime {
    /// Propose the lineage a brand-new occurrence freezes. An existing
    /// occurrence keeps its own and ignores this.
    fn mint_lineage(
        &self,
        activation: &ParallelActivation,
        definition: &ParallelNodeDefinition,
        inputs: &[FrozenBranchInput],
    ) -> Result<(ParallelChildOrigin, Vec<String>), GraphError> {
        let origin = self.checkpoints.child_origin(activation)?;
        let mut threads = Vec::with_capacity(inputs.len());
        for input in inputs {
            let branch = definition.branches().get(input.ordinal).ok_or_else(|| {
                parallel_error(
                    "graph.parallel.corrupt_occurrence",
                    "a frozen branch ordinal is invalid",
                )
            })?;
            threads.push(self.checkpoints.branch_thread_id(
                activation,
                branch,
                input.ordinal,
                &input.input_digest,
                &origin,
            )?);
        }
        Ok((origin, threads))
    }

    fn project_inputs(
        &self,
        activation: &mut ParallelActivation,
        definition: &ParallelNodeDefinition,
        parent: &State,
    ) -> Result<Vec<FrozenBranchInput>, GraphError> {
        // Validate before business-state cloning or any mapping/child admission.
        validate_input_state(parent)?;
        // Evaluate every mapping before any branch runs.
        let business = business_state(parent);
        let mut frozen = Vec::with_capacity(definition.branches().len());
        let mut config = digest::Context::new(&digest::SHA256);
        config.update(b"elitea.graph.parallel.owned-config.v1\0");
        config.update(&activation.config_digest);
        for (ordinal, branch) in definition.branches().iter().enumerate() {
            let input = self.graphs.project_input(branch, &business)?;
            validate_state(&input)?;
            if input != business_state(&input) {
                return Err(parallel_error(
                    "graph.parallel.invalid_mapping",
                    "branch business input contains continuation controls",
                ));
            }
            let owned_definition_digest = self.graphs.owned_definition_digest(branch)?;
            if owned_definition_digest == [0; 32] {
                return Err(parallel_error(
                    "graph.parallel.invalid_configuration",
                    "an owned branch definition digest is missing",
                ));
            }
            config.update(&owned_definition_digest);
            frozen.push(FrozenBranchInput {
                branch_id: branch.id().to_owned(),
                node: branch.node().to_owned(),
                ordinal,
                owned_definition_digest,
                input_digest: projected_input_digest(&input)?,
                input,
            });
        }
        activation
            .config_digest
            .copy_from_slice(config.finish().as_ref());
        let serialized_inputs = serde_json::to_value(&frozen).map_err(|_| {
            parallel_error(
                "graph.parallel.invalid_mapping",
                "the branch inputs cannot be encoded",
            )
        })?;
        ensure_bounded_json(&serialized_inputs, MAX_BRANCH_INPUT_BYTES, "input")?;
        Ok(frozen)
    }

    #[allow(clippy::too_many_lines)] // Keep child admission, frozen-identity and receipt replay checks together.
    async fn restore_branches(
        &self,
        activation: &ParallelActivation,
        definition: &ParallelNodeDefinition,
        inputs: &[FrozenBranchInput],
        origin: &ParallelChildOrigin,
        child_threads: &[String],
    ) -> Result<PreparedBranchSet, GraphError> {
        let mut prepared = Vec::with_capacity(inputs.len());
        let mut pauses = BTreeMap::new();
        let mut expected = Vec::new();
        let branches = inputs
            .iter()
            .map(|input| {
                definition
                    .branches()
                    .get(input.ordinal)
                    .ok_or_else(|| {
                        parallel_error(
                            "graph.parallel.corrupt_occurrence",
                            "a frozen branch ordinal is invalid",
                        )
                    })
                    .cloned()
            })
            .collect::<Result<Vec<_>, _>>()?;
        let requests = inputs
            .iter()
            .zip(&branches)
            .map(|(input, branch)| ParallelChildRequest {
                branch,
                ordinal: input.ordinal,
                input_digest: &input.input_digest,
            })
            .collect::<Vec<_>>();
        // One batched activation and one batched receipt read for every child.
        let children = self
            .checkpoints
            .prepare_children(activation, &requests, origin)
            .await?;
        if children.len() != inputs.len() {
            return Err(parallel_error(
                "graph.parallel.invalid_child_scope",
                "the prepared child set does not match the frozen occurrence",
            ));
        }
        for (position, ((input, branch), prepared_child)) in inputs
            .iter()
            .cloned()
            .zip(branches)
            .zip(children)
            .enumerate()
        {
            let PreparedChildCheckpoint { child, latest } = prepared_child;
            if child_threads.get(position) != Some(&child.thread_id) {
                return Err(parallel_error(
                    "graph.parallel.corrupt_occurrence",
                    "a restored branch does not match its frozen identity",
                ));
            }
            if !child.admitted_threads.contains(&child.thread_id)
                || child.admitted_threads.len() > 129
                || child.admitted_threads.iter().any(|thread| {
                    thread.is_empty() || thread.len() > 512 || thread.chars().any(char::is_control)
                })
            {
                return Err(parallel_error(
                    "graph.parallel.invalid_child_scope",
                    "the exact branch checkpoint family is invalid",
                ));
            }
            let checkpoint = Arc::new(BranchReceiptCheckpointer::new(
                child.checkpointer,
                child.thread_id.clone(),
            ));
            if latest
                .as_ref()
                .is_some_and(|saved| saved.thread_id != child.thread_id)
            {
                return Err(parallel_error(
                    "graph.parallel.corrupt_receipt",
                    "the child checkpoint belongs to another thread",
                ));
            }
            let replay = match latest {
                Some(saved) => match BranchReceiptCheckpointer::receipt(&saved)? {
                    Some(BranchReceipt::Completed) if saved.pending_nodes.is_empty() => Some(
                        terminal_outcome(self.graphs.project_result(&branch, &saved.state)?)?,
                    ),
                    Some(BranchReceipt::Paused { interrupt })
                        if !saved.pending_nodes.is_empty() =>
                    {
                        let pause = ParallelBranchPause {
                            thread_id: child.thread_id.clone(),
                            checkpoint_id: saved.checkpoint_id,
                            interrupt,
                            checkpointer: checkpoint.clone(),
                        };
                        let cards = self.graphs.pause_cards(&branch, &pause)?;
                        validate_cards(&cards)?;
                        expected.extend(cards.iter().cloned().map(|card| (input.ordinal, card)));
                        pauses.insert(input.ordinal, pause);
                        Some(ParallelBranchOutcome::Paused(cards))
                    }
                    Some(BranchReceipt::Failed { code }) if valid_failure_code(&code) => {
                        Some(ParallelBranchOutcome::Failed(code))
                    }
                    None if saved.pending_nodes.is_empty() => {
                        return Err(parallel_error(
                            "graph.parallel.corrupt_receipt",
                            "a terminal child has no lifecycle receipt",
                        ));
                    }
                    None => None,
                    Some(_) => {
                        return Err(parallel_error(
                            "graph.parallel.corrupt_receipt",
                            "the child receipt contradicts its frontier",
                        ));
                    }
                },
                None => None,
            };
            prepared.push(PreparedParallelBranch {
                branch,
                ordinal: input.ordinal,
                input: input.input,
                checkpoint,
                thread_id: child.thread_id,
                admitted_threads: child.admitted_threads,
                replay,
            });
        }
        Ok(PreparedBranchSet {
            branches: prepared,
            pauses,
            cards: expected,
        })
    }

    async fn prepare_resume(
        &self,
        activation: &ParallelActivation,
        occurrence: &FrozenOccurrence,
        restored: &mut PreparedBranchSet,
        context: &NodeContext,
    ) -> Result<(), GraphError> {
        let PreparedBranchSet {
            branches: prepared,
            pauses,
            cards: expected,
        } = restored;
        validate_expected_cards(expected)?;
        let published = if occurrence.cards.is_empty() {
            expected.as_slice()
        } else {
            occurrence.cards.as_slice()
        };
        let incoming = decisions_for_activation(context, activation, published)?;
        let published = published.to_vec();
        if let Some(accepted) = &occurrence.decisions {
            if incoming
                .as_ref()
                .is_some_and(|decisions| !same_decisions(decisions, accepted))
            {
                return Err(parallel_error(
                    "graph.parallel.stale_decision",
                    "the accepted parallel decision set changed",
                ));
            }
            for prepared in prepared.iter_mut() {
                // A later child pause must publish its new card before receiving decisions.
                let matches_original = expected
                    .iter()
                    .filter(|(ordinal, _)| *ordinal == prepared.ordinal)
                    .all(|card| occurrence.cards.contains(card));
                if pauses.contains_key(&prepared.ordinal)
                    && matches_original
                    && let Some(controls) = occurrence.resume_inputs.get(&prepared.ordinal)
                {
                    prepared.input.extend(controls.clone());
                    prepared.replay = None;
                }
            }
        } else if let Some(decisions) = incoming {
            // Resolve all descendant proofs before dispatching any resumed child.
            let mut resume_inputs = BTreeMap::new();
            for prepared in prepared.iter_mut() {
                if let Some(pause) = pauses.get(&prepared.ordinal) {
                    let selected = decisions
                        .iter()
                        .filter(|decision| {
                            expected.iter().any(|(ordinal, card)| {
                                *ordinal == prepared.ordinal && card_matches(card, decision)
                            })
                        })
                        .cloned()
                        .collect::<Vec<_>>();
                    let controls = self
                        .graphs
                        .resume_input(&prepared.branch, pause, &selected)
                        .await?;
                    if controls.keys().any(|key| !is_resume_key(key)) {
                        return Err(parallel_error(
                            "graph.parallel.invalid_resume",
                            "a branch continuation contains a business update",
                        ));
                    }
                    resume_inputs.insert(prepared.ordinal, controls.clone());
                    prepared.input.extend(controls);
                    prepared.replay = None;
                }
            }
            self.parent
                .record_decisions(occurrence, &published, decisions, resume_inputs, context)
                .await?;
        }
        Ok(())
    }

    async fn invoke_inner(
        &self,
        activation: &ParallelActivation,
        branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> Result<ParallelBranchOutcome, GraphError> {
        let graph = self.graphs.compile_branch(
            &branch.branch,
            ParallelBranchExecution::new(
                Arc::clone(&branch.checkpoint),
                activation.clone(),
                branch.branch.node().to_owned(),
                branch.ordinal,
                branch.thread_id.clone(),
                branch.admitted_threads.clone(),
                context
                    .state
                    .get(super::node_events::PIPELINE_NODE_EVENT_SCOPE_STATE_KEY)
                    .cloned(),
            ),
        )?;
        let mut config = ExecutionConfig::new(&branch.thread_id)
            .with_recursion_limit(context.config.recursion_limit);
        if let Some(parent) = &context.config.parent_context {
            config = config.with_parent_context(Arc::clone(parent));
        }
        match graph.invoke_detailed(branch.input, config).await {
            Ok(outcome) => {
                if outcome.goto_parent.is_some() {
                    return Err(parallel_error(
                        "graph.parallel.unsupported_parent_route",
                        "a parallel branch cannot route the parent graph",
                    ));
                }
                // The fenced terminal save this branch just made is the proof.
                let saved = branch.checkpoint.last_saved()?.ok_or_else(|| {
                    parallel_error(
                        "graph.parallel.corrupt_receipt",
                        "a completed child checkpoint is missing",
                    )
                })?;
                if !saved.terminal || !matches!(saved.receipt, Some(BranchReceipt::Completed)) {
                    return Err(parallel_error(
                        "graph.parallel.corrupt_receipt",
                        "the completed child checkpoint is not proven",
                    ));
                }
                validate_state(&outcome.state)?;
                let terminal = self.graphs.project_result(&branch.branch, &outcome.state)?;
                terminal_outcome(terminal)
            }
            Err(GraphError::Interrupted(interrupted)) => {
                let saved = branch.checkpoint.last_saved()?.ok_or_else(|| {
                    parallel_error(
                        "graph.parallel.corrupt_receipt",
                        "a paused child checkpoint is missing",
                    )
                })?;
                if saved.checkpoint_id != interrupted.checkpoint_id
                    || interrupted.thread_id != branch.thread_id
                    || !matches!(saved.receipt, Some(BranchReceipt::Paused { .. }))
                {
                    return Err(parallel_error(
                        "graph.parallel.corrupt_receipt",
                        "the paused child checkpoint is not proven",
                    ));
                }
                let pause = ParallelBranchPause {
                    thread_id: interrupted.thread_id,
                    checkpoint_id: interrupted.checkpoint_id,
                    interrupt: interrupted.interrupt,
                    checkpointer: branch.checkpoint.clone(),
                };
                let cards = self.graphs.pause_cards(&branch.branch, &pause)?;
                validate_cards(&cards)?;
                Ok(ParallelBranchOutcome::Paused(cards))
            }
            Err(error) => Err(error),
        }
    }
}

/// ADK custom node implementing bounded, deterministic `wait: all`.
///
/// Every branch is a small independently checkpointed ADK graph. ADK writes its
/// empty-frontier terminal checkpoint before `invoke_detailed` returns. If the
/// parent process then dies, replaying the same activation loads completed
/// branches without invoking their nodes again. External effects still need a
/// stable effect identity because a process can die after an effect but before
/// the branch graph reaches its checkpoint.
pub(crate) struct DurableParallelNode {
    definition: ParallelNodeDefinition,
    runtime: Arc<dyn ParallelBranchRuntime>,
    deadline: Option<tokio::time::Instant>,
    cleanup_timeout: Duration,
    cancellation: Option<Arc<FanoutCancellation>>,
}

impl DurableParallelNode {
    pub(crate) fn new(
        definition: ParallelNodeDefinition,
        runtime: Arc<dyn ParallelBranchRuntime>,
    ) -> Self {
        Self {
            definition,
            runtime,
            deadline: None,
            cleanup_timeout: Duration::from_secs(5),
            cancellation: None,
        }
    }

    /// Supply the owner's cancellation latch. The owner fires it; the node never polls.
    pub(crate) fn with_cancellation(mut self, cancellation: Arc<FanoutCancellation>) -> Self {
        self.cancellation = Some(cancellation);
        self
    }

    pub(crate) fn with_cleanup_timeout(mut self, cleanup_timeout: Duration) -> Self {
        self.cleanup_timeout = cleanup_timeout;
        self
    }

    /// Supply the deadline from root execution authority at assembly.
    pub(crate) fn with_deadline(mut self, deadline: tokio::time::Instant) -> Self {
        self.deadline = Some(deadline);
        self
    }

    pub(crate) async fn execute_outcome(
        &self,
        context: &NodeContext,
    ) -> Result<ParallelNodeOutcome, GraphError> {
        let span = fanout_trace::activation_span(
            FanoutKind::Parallel,
            self.definition.id(),
            context.step,
            Some(self.definition.branches().len()),
            usize::try_from(self.definition.max_concurrency()).unwrap_or(usize::MAX),
            context
                .state
                .get(PARALLEL_RESUME_STATE_KEY)
                .is_some_and(|value| !value.is_null()),
        );
        fanout_trace::activation_started(&span);
        let result = self
            .execute_outcome_inner(context)
            .instrument(span.clone())
            .await;
        fanout_trace::activation_finished(
            &span,
            match &result {
                Ok(ParallelNodeOutcome::Completed(_)) => ActivationOutcome::Joined,
                Ok(ParallelNodeOutcome::Paused(_)) => ActivationOutcome::Paused,
                Ok(ParallelNodeOutcome::Blocked(_)) => ActivationOutcome::Blocked,
                Err(error) if is_lease_lost(error) => ActivationOutcome::LeaseLost,
                Err(GraphError::NodeExecutionFailed { message, .. })
                    if message.starts_with("graph.parallel.cancelled") =>
                {
                    ActivationOutcome::Cancelled
                }
                Err(_) => ActivationOutcome::Failed,
            },
        );
        result
    }

    async fn execute_outcome_inner(
        &self,
        context: &NodeContext,
    ) -> Result<ParallelNodeOutcome, GraphError> {
        self.validate()?;
        validate_input_state(&context.state)?;
        self.check_running(context)?;
        let mut activation = ParallelActivation::from_context(&self.definition, context)?;
        let prepared = self
            .runtime
            .prepare(&mut activation, &self.definition, context)
            .await?;
        self.check_running(context)?;
        let prepared = match prepared {
            PreparedParallelActivation::Ready(branches) => branches,
            PreparedParallelActivation::Blocked(blocked) => {
                return Ok(ParallelNodeOutcome::Blocked(blocked));
            }
        };
        let ordered = self.drain_branches(&activation, prepared, context).await?;
        self.collect_outcomes(&activation, ordered, context).await
    }

    fn stop_requested(&self, context: &NodeContext) -> bool {
        is_cancelled(context)
            || self
                .cancellation
                .as_deref()
                .is_some_and(FanoutCancellation::is_cancelled)
            || self
                .deadline
                .is_some_and(|deadline| deadline <= tokio::time::Instant::now())
    }

    fn check_running(&self, context: &NodeContext) -> Result<(), GraphError> {
        if self.stop_requested(context) {
            return Err(parallel_error(
                "graph.parallel.cancelled",
                "the parallel execution was cancelled",
            ));
        }
        Ok(())
    }

    #[allow(clippy::too_many_lines)] // Keep admission, stop and lease-loss ordering in one select loop.
    async fn drain_branches(
        &self,
        activation: &ParallelActivation,
        prepared: Vec<PreparedParallelBranch>,
        context: &NodeContext,
    ) -> Result<Vec<OrderedBranchOutcome>, GraphError> {
        let max_concurrency = usize::try_from(self.definition.max_concurrency()).map_err(|_| {
            parallel_error(
                "graph.parallel.invalid_configuration",
                "the parallel concurrency does not fit this platform",
            )
        })?;
        let cancel_signal = Arc::new(AtomicBool::new(false));
        let mut run_context =
            NodeContext::new(context.state.clone(), context.config.clone(), context.step);
        if let Some(parent) = &context.config.parent_context {
            run_context.config.parent_context =
                Some(Arc::new(control::ParallelInvocationContext::new(
                    Arc::clone(parent),
                    Arc::clone(&cancel_signal),
                )));
        }
        let mut pending = prepared.into_iter();
        let mut inflight = FuturesUnordered::new();
        let mut ordered = Vec::with_capacity(self.definition.branches().len());
        let mut admission_open = true;
        let mut stopping = false;
        let mut cleanup_deadline = None;
        let mut lease_error = None;
        let latch = self.cancellation.as_deref();
        if self.stop_requested(context) {
            self.begin_stop(
                &mut admission_open,
                &mut stopping,
                &mut cleanup_deadline,
                &cancel_signal,
            );
        } else {
            for _ in 0..max_concurrency {
                if let Some(branch) = pending.next() {
                    inflight.push(self.invoke_branch(activation.clone(), branch, &run_context));
                }
            }
        }

        // The owner fires the latch and the deadline is absolute: nothing here polls.
        while !inflight.is_empty() {
            let outcome = tokio::select! {
                biased;
                () = latch_cancelled(latch), if !stopping => {
                    self.begin_stop(&mut admission_open, &mut stopping, &mut cleanup_deadline, &cancel_signal);
                    continue;
                }
                () = tokio::time::sleep_until(self.deadline.unwrap_or_else(tokio::time::Instant::now)),
                    if !stopping && self.deadline.is_some() => {
                    self.begin_stop(&mut admission_open, &mut stopping, &mut cleanup_deadline, &cancel_signal);
                    continue;
                }
                () = tokio::time::sleep_until(cleanup_deadline.unwrap_or_else(tokio::time::Instant::now)),
                    if stopping => {
                    return Err(parallel_error("graph.parallel.cancellation_cleanup_failed", "parallel cancellation cleanup exceeded its bound"));
                }
                outcome = inflight.next() => outcome,
            };
            let Some((ordinal, branch, result)) = outcome else {
                break;
            };
            let result = match result {
                ParallelBranchOutcome::Completed(_) | ParallelBranchOutcome::Paused(_) => {
                    Some(result)
                }
                ParallelBranchOutcome::Blocked | ParallelBranchOutcome::Failed(_) => {
                    admission_open = false;
                    Some(result)
                }
                ParallelBranchOutcome::Cancelled => {
                    self.begin_stop(
                        &mut admission_open,
                        &mut stopping,
                        &mut cleanup_deadline,
                        &cancel_signal,
                    );
                    Some(result)
                }
                ParallelBranchOutcome::LeaseLost(error) => {
                    lease_error.get_or_insert(error);
                    self.begin_stop(
                        &mut admission_open,
                        &mut stopping,
                        &mut cleanup_deadline,
                        &cancel_signal,
                    );
                    None
                }
            };
            if let Some(result) = result {
                ordered.push((ordinal, branch, result));
            }
            if !stopping && self.stop_requested(context) {
                self.begin_stop(
                    &mut admission_open,
                    &mut stopping,
                    &mut cleanup_deadline,
                    &cancel_signal,
                );
            }
            if admission_open
                && !stopping
                && let Some(branch) = pending.next()
            {
                inflight.push(self.invoke_branch(activation.clone(), branch, &run_context));
            }
        }

        ordered.sort_by_key(|(ordinal, _, _)| *ordinal);
        if let Some(error) = lease_error {
            return Err(error);
        }
        if stopping {
            return Err(parallel_error(
                "graph.parallel.cancelled",
                "the parallel execution was cancelled",
            ));
        }
        self.check_running(context)?;
        Ok(ordered)
    }

    /// Close admission, tell running children to stop and bound their cleanup.
    fn begin_stop(
        &self,
        admission_open: &mut bool,
        stopping: &mut bool,
        cleanup_deadline: &mut Option<tokio::time::Instant>,
        cancel_signal: &AtomicBool,
    ) {
        *admission_open = false;
        if !*stopping {
            *stopping = true;
            cancel_signal.store(true, Ordering::Release);
            *cleanup_deadline = Some(tokio::time::Instant::now() + self.cleanup_timeout);
        }
    }

    async fn collect_outcomes(
        &self,
        activation: &ParallelActivation,
        ordered: Vec<OrderedBranchOutcome>,
        context: &NodeContext,
    ) -> Result<ParallelNodeOutcome, GraphError> {
        if let Some((_, branch, code)) = ordered.iter().find_map(|(ordinal, branch, outcome)| {
            if let ParallelBranchOutcome::Failed(code) = outcome {
                Some((*ordinal, branch, code))
            } else {
                None
            }
        }) {
            return Err(GraphError::NodeExecutionFailed {
                node: self.definition.id().to_owned(),
                message: format!(
                    "graph.parallel.branch_failed: branch '{}' failed after all admitted branches drained ({})",
                    branch.id(),
                    code,
                ),
            });
        }

        if let Some((ordinal, branch, _)) = ordered
            .iter()
            .find(|(_, _, outcome)| matches!(outcome, ParallelBranchOutcome::Blocked))
        {
            let blocked = ParallelBlocked {
                branch_id: branch.id().to_owned(),
                node: branch.node().to_owned(),
                ordinal: *ordinal,
            };
            self.runtime
                .record_blocked(activation, blocked.clone())
                .await?;
            return Ok(ParallelNodeOutcome::Blocked(blocked));
        }

        let mut cards = Vec::new();
        let mut private_cards = Vec::new();
        for (ordinal, branch, outcome) in &ordered {
            if let ParallelBranchOutcome::Paused(paused) = outcome {
                for card in paused {
                    private_cards.push((*ordinal, card.clone()));
                    cards.push(json!({
                        "branch_id": branch.id(),
                        "node": branch.node(),
                        "ordinal": ordinal,
                        "interrupt_id": card.interrupt_id,
                        "tool_call_id": card.tool_call_id,
                    }));
                }
            }
        }
        if !cards.is_empty() {
            if cards.len() > MAX_PAUSE_CARDS {
                return Err(parallel_error(
                    "graph.parallel.pause_resource_exhausted",
                    "the parallel pause card count exceeds its bound",
                ));
            }
            let aggregate = json!({
                "schema": PARALLEL_INTERRUPT_SCHEMA,
                "parallel_node": self.definition.id(),
                "parallel_activation": activation_label(activation)?,
                "cards": cards,
            });
            ensure_bounded_json(&aggregate, MAX_PAUSE_BYTES, "pause")?;
            self.runtime.record_pause(activation, private_cards).await?;
            return Ok(ParallelNodeOutcome::Paused(
                NodeOutput::interrupt_with_data("Parallel branches paused.", aggregate),
            ));
        }

        let joined = Value::Array(
            ordered
                .into_iter()
                .map(|(_, branch, outcome)| {
                    if let ParallelBranchOutcome::Completed(outputs) = outcome {
                        Ok(json!({
                            "branch_id": branch.id(),
                            "node": branch.node(),
                            "outputs": outputs,
                        }))
                    } else {
                        Err(parallel_error(
                            "graph.parallel.invalid_outcome",
                            "a parallel branch did not complete",
                        ))
                    }
                })
                .collect::<Result<Vec<_>, _>>()?,
        );
        ensure_bounded_json(&joined, MAX_JOINED_RESULT_BYTES, "joined result")?;
        let mut output = NodeOutput::new().with_update(self.definition.output_key(), joined);
        if context.state.contains_key(PARALLEL_RESUME_STATE_KEY) {
            output = output.with_update(PARALLEL_RESUME_STATE_KEY, Value::Null);
        }
        Ok(ParallelNodeOutcome::Completed(output))
    }

    async fn invoke_branch(
        &self,
        activation: ParallelActivation,
        branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> OrderedBranchOutcome {
        let ordinal = branch.ordinal;
        let definition = branch.branch.clone();
        let start = if branch.replay.is_some() {
            ChildStart::Restored
        } else if branch.input.keys().any(|key| is_resume_key(key)) {
            ChildStart::Resumed
        } else {
            ChildStart::Fresh
        };
        let span = fanout_trace::child_span(FanoutKind::Parallel, self.definition.id(), ordinal);
        fanout_trace::child_admitted(&span, start);
        let result = self
            .runtime
            .invoke(&activation, branch, context)
            .instrument(span.clone())
            .await;
        fanout_trace::child_finished(
            &span,
            match &result {
                ParallelBranchOutcome::Completed(_) => ChildOutcome::Completed,
                ParallelBranchOutcome::Paused(_) => ChildOutcome::Paused,
                ParallelBranchOutcome::Blocked => ChildOutcome::Blocked,
                ParallelBranchOutcome::Failed(_) => ChildOutcome::Failed,
                ParallelBranchOutcome::Cancelled => ChildOutcome::Cancelled,
                ParallelBranchOutcome::LeaseLost(_) => ChildOutcome::LeaseLost,
            },
        );
        (ordinal, definition, result)
    }
}

#[async_trait]
impl Node for DurableParallelNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        match self.execute_outcome(context).await? {
            ParallelNodeOutcome::Completed(output) | ParallelNodeOutcome::Paused(output) => {
                Ok(output)
            }
            ParallelNodeOutcome::Blocked(_) => Err(parallel_error(
                "graph.parallel.blocked",
                "a parallel branch denied execution after admitted branches drained",
            )),
        }
    }

    fn validate(&self) -> Result<(), GraphError> {
        self.definition.validate().map_err(|error| {
            parallel_error(error.code(), "the parallel node configuration is invalid")
        })?;
        if !(2..=16).contains(&self.definition.branches().len())
            || !(1..=8).contains(&self.definition.max_concurrency())
        {
            return Err(parallel_error(
                "graph.parallel.invalid_configuration",
                "the parallel contract requires 2-16 branches and concurrency 1-8",
            ));
        }
        self.runtime.validate(&self.definition)
    }
}

fn terminal_outcome(terminal: ParallelBranchTerminal) -> Result<ParallelBranchOutcome, GraphError> {
    match terminal {
        ParallelBranchTerminal::Completed(outputs) => {
            validate_values(outputs.values())?;
            ensure_bounded_json(
                &Value::Object(outputs.clone()),
                MAX_BRANCH_RESULT_BYTES,
                "branch result",
            )?;
            Ok(ParallelBranchOutcome::Completed(outputs))
        }
        ParallelBranchTerminal::Blocked => Ok(ParallelBranchOutcome::Blocked),
    }
}

fn is_cancelled(context: &NodeContext) -> bool {
    context
        .config
        .parent_context
        .as_ref()
        .is_some_and(|parent| parent.is_cancelled())
}

fn is_resume_key(key: &str) -> bool {
    matches!(
        key,
        "hitl_decisions"
            | "__elitea_hitl_resume_v1"
            | "__elitea_tool_resume_v1"
            | "__elitea_llm_tool_resume_v1"
            | "__elitea_static_text_resume_v1"
            | "__elitea_static_after_checkpoints_v1"
    )
}

fn business_state(state: &State) -> State {
    state
        .iter()
        .filter(|(key, _)| {
            !is_resume_key(key)
                && key.as_str() != PARALLEL_RESUME_STATE_KEY
                && key.as_str() != "__elitea_pipeline_node_event_scope_v1"
        })
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect()
}

fn activation_label(activation: &ParallelActivation) -> Result<String, GraphError> {
    let raw = serde_json::to_vec(activation).map_err(|_| {
        parallel_error(
            "graph.parallel.invalid_activation",
            "the parallel activation cannot be encoded",
        )
    })?;
    let hashed = digest::digest(&digest::SHA256, &raw);
    Ok(hashed
        .as_ref()
        .iter()
        .fold(String::from("p1:"), |mut label, byte| {
            use std::fmt::Write as _;
            let _ = write!(label, "{byte:02x}");
            label
        }))
}

fn validate_cards(cards: &[ParallelPauseCard]) -> Result<(), GraphError> {
    let mut unique = BTreeSet::new();
    if cards.is_empty() || cards.len() > MAX_PAUSE_CARDS {
        return Err(parallel_error(
            "graph.parallel.invalid_pause",
            "the branch pause card count is invalid",
        ));
    }
    for card in cards {
        if !valid_card_identity(&card.interrupt_id)
            || (!card.tool_call_id.is_empty() && !valid_card_identity(&card.tool_call_id))
            || !unique.insert((&card.interrupt_id, &card.tool_call_id))
        {
            return Err(parallel_error(
                "graph.parallel.invalid_pause",
                "the branch pause card identity is invalid",
            ));
        }
    }
    Ok(())
}

fn validate_expected_cards(expected: &[(usize, ParallelPauseCard)]) -> Result<(), GraphError> {
    let unique = expected
        .iter()
        .map(|(_, card)| (&card.interrupt_id, &card.tool_call_id))
        .collect::<BTreeSet<_>>();
    if expected.len() > MAX_PAUSE_CARDS || unique.len() != expected.len() {
        return Err(parallel_error(
            "graph.parallel.invalid_pause",
            "the aggregate pause card set is invalid",
        ));
    }
    Ok(())
}

fn valid_card_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 512 && !value.chars().any(char::is_control)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ParallelResumeEnvelope {
    schema: String,
    parallel_activation: String,
    decisions: Vec<ParallelDecision>,
}

fn card_matches(card: &ParallelPauseCard, decision: &ParallelDecision) -> bool {
    card.interrupt_id == decision.interrupt_id && card.tool_call_id == decision.tool_call_id
}

fn same_decisions(left: &[ParallelDecision], right: &[ParallelDecision]) -> bool {
    left.len() == right.len() && left.iter().all(|decision| right.contains(decision))
}

fn decisions_for_activation(
    context: &NodeContext,
    activation: &ParallelActivation,
    expected: &[(usize, ParallelPauseCard)],
) -> Result<Option<Vec<ParallelDecision>>, GraphError> {
    let Some(raw) = context
        .state
        .get(PARALLEL_RESUME_STATE_KEY)
        .filter(|value| !value.is_null())
    else {
        return Ok(None);
    };
    ensure_bounded_json(raw, MAX_PAUSE_BYTES, "pause")?;
    let envelope: ParallelResumeEnvelope = serde_json::from_value(raw.clone()).map_err(|_| {
        parallel_error(
            "graph.parallel.invalid_resume",
            "the parallel decision envelope is invalid",
        )
    })?;
    if envelope.schema != PARALLEL_INTERRUPT_SCHEMA
        || envelope.parallel_activation != activation_label(activation)?
        || expected.is_empty()
        || envelope.decisions.len() != expected.len()
    {
        return Err(parallel_error(
            "graph.parallel.stale_decision",
            "the parallel decision set does not match the paused activation",
        ));
    }
    let mut unique = BTreeSet::new();
    for decision in &envelope.decisions {
        if !unique.insert((&decision.interrupt_id, &decision.tool_call_id))
            || !expected
                .iter()
                .any(|(_, card)| card_matches(card, decision))
            || !matches!(
                decision.action.as_str(),
                "approve"
                    | "reject"
                    | "edit"
                    | "block_with_comment"
                    | "authorize"
                    | "skip"
                    | "answer"
                    | "continue"
            )
            || decision.value.len() > 64 * 1024
            || decision.value.contains('\0')
        {
            return Err(parallel_error(
                "graph.parallel.stale_decision",
                "the parallel decision set is partial, duplicate, or foreign",
            ));
        }
    }
    Ok(Some(envelope.decisions))
}

pub(in crate::agents::graph) fn ensure_bounded_json(
    value: &Value,
    maximum: usize,
    kind: &'static str,
) -> Result<(), GraphError> {
    validate_values([value])?;
    let mut writer = CappedJsonWriter::new(maximum);
    if serde_json::to_writer(&mut writer, value).is_err() {
        return Err(parallel_error(
            "graph.parallel.resource_exhausted",
            match kind {
                "branch result" => "a parallel branch result exceeds its resource bound",
                _ => "the parallel joined result exceeds its resource bound",
            },
        ));
    }
    Ok(())
}

pub(in crate::agents::graph) fn validate_input_state(state: &State) -> Result<(), GraphError> {
    validate_state(state)?;
    let mut writer = CappedJsonWriter::new(MAX_BRANCH_INPUT_BYTES);
    serde_json::to_writer(&mut writer, state).map_err(|_| {
        parallel_error(
            "graph.parallel.input_resource_exhausted",
            "the parallel business snapshot exceeds its resource bound",
        )
    })
}

pub(super) fn projected_input_digest(input: &State) -> Result<[u8; 32], GraphError> {
    validate_state(input)?;
    let ordered = input
        .iter()
        .map(|(key, value)| (key.as_str(), value))
        .collect::<BTreeMap<_, _>>();
    let mut writer = CappedDigestWriter::new(MAX_BRANCH_INPUT_BYTES);
    if serde_json::to_writer(&mut writer, &ordered).is_err() {
        return Err(parallel_error(
            "graph.parallel.input_resource_exhausted",
            "a projected parallel branch input exceeds its resource bound",
        ));
    }
    Ok(writer.finish())
}

struct CappedDigestWriter {
    context: digest::Context,
    written: usize,
    maximum: usize,
}

impl CappedDigestWriter {
    fn new(maximum: usize) -> Self {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(BRANCH_INPUT_DIGEST_DOMAIN);
        Self {
            context,
            written: 0,
            maximum,
        }
    }

    fn finish(self) -> [u8; 32] {
        let mut value = [0_u8; 32];
        value.copy_from_slice(self.context.finish().as_ref());
        value
    }
}

impl Write for CappedDigestWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.maximum.saturating_sub(self.written) {
            return Err(io::Error::other(
                "parallel branch input JSON exceeds its resource bound",
            ));
        }
        self.context.update(bytes);
        self.written += bytes.len();
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

struct CappedJsonWriter {
    written: usize,
    maximum: usize,
}

impl CappedJsonWriter {
    const fn new(maximum: usize) -> Self {
        Self {
            written: 0,
            maximum,
        }
    }
}

impl Write for CappedJsonWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.maximum.saturating_sub(self.written) {
            return Err(io::Error::other("parallel JSON exceeds its resource bound"));
        }
        self.written += bytes.len();
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn graph_error_code(error: &GraphError) -> &'static str {
    match error {
        GraphError::InvalidGraph(_) => "graph.invalid",
        GraphError::NodeNotFound(_) => "graph.node_not_found",
        GraphError::EdgeTargetNotFound(_) => "graph.edge_target_not_found",
        GraphError::NoEntryPoint => "graph.no_entry_point",
        GraphError::RecursionLimitExceeded(_) => "graph.recursion_limit",
        GraphError::Interrupted(_) => "graph.interrupted",
        GraphError::NodeExecutionFailed { .. } => "graph.node_execution_failed",
        GraphError::NodeTimedOut { .. } => "graph.node_timed_out",
        GraphError::FanInTimedOut { .. } => "graph.fan_in_timed_out",
        GraphError::SerializationError(_) => "graph.serialization",
        GraphError::CheckpointError(_) => "graph.checkpoint",
        GraphError::UndeclaredChannel { .. } => "graph.undeclared_channel",
        GraphError::SubgraphChannelMismatch { .. } => "graph.subgraph_channel_mismatch",
        GraphError::UnknownRouteTarget(_) => "graph.unknown_route_target",
        GraphError::IoError(_) => "graph.io",
        GraphError::JsonError(_) => "graph.json",
        GraphError::Other(_) => "graph.other",
    }
}

fn valid_failure_code(code: &str) -> bool {
    matches!(
        code,
        "graph.invalid"
            | "graph.node_not_found"
            | "graph.edge_target_not_found"
            | "graph.no_entry_point"
            | "graph.recursion_limit"
            | "graph.interrupted"
            | "graph.node_execution_failed"
            | "graph.node_timed_out"
            | "graph.fan_in_timed_out"
            | "graph.serialization"
            | "graph.checkpoint"
            | "graph.undeclared_channel"
            | "graph.subgraph_channel_mismatch"
            | "graph.unknown_route_target"
            | "graph.io"
            | "graph.json"
            | "graph.other"
    )
}

fn parallel_error(code: &str, message: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "parallel".to_owned(),
        message: format!("{code}: {message}"),
    }
}
