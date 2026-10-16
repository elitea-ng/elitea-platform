#![allow(dead_code)] // Composed by the next full YAML graph compiler slice.

//! Fixed Parallel as a planner on the shared fan-out runner (design
//! `services/elitea-worker-rust/docs/fanout-v2-runtime-design.md` §2).
//!
//! The node freezes its occurrence and restores every branch from its own
//! checkpoint in one batched preparation, then hands the branches to
//! [`FanoutRunner`] through [`ParallelChildren`]. Decisions arrive per
//! interrupt from a [`DecisionSource`]; the child's own checkpoint proves each
//! one. The parent is written at most twice per visit: freeze, then the join
//! or the ADK pause row.

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::io::{self, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, OnceLock};

use adk_rust::graph::checkpoint::Checkpointer;
use adk_rust::graph::{
    Checkpoint, CompiledGraph, ExecutionConfig, GraphError, Node, NodeContext, NodeOutput, State,
};
use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout::admission::ChildPermits;
use elitea_agent_runtime::graph::fanout::apply::{CardIdentity, ChildCheckpointView};
use elitea_agent_runtime::graph::fanout::identity::{InterruptKeyInput, interrupt_key};
use elitea_agent_runtime::graph::fanout::plan::{
    self, ChildFailure, ChildTerminal, FanoutKind as JoinKind, JoinOutcome,
};
use elitea_agent_runtime::graph::fanout::runner::{
    ChildRun, DecisionSource, FanoutChildren, FanoutEvents, FanoutRunError, FanoutRunner,
    FanoutRunnerConfig, FanoutStop, FetchedDecision, NoDecisions, NoEvents, RestoredChild,
    RunnerOutcome,
};
use elitea_agent_runtime::graph::fanout_budget::{
    MAX_CHILD_RESULT_BYTES, MAX_DECISION_VALUE_CHARS, MAX_OPEN_INTERRUPTS,
};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::fanout_control::{FanoutCancellation, is_lease_lost, latch_cancelled};
use super::fanout_trace::{self, ActivationOutcome, ChildOutcome, ChildStart, FanoutKind};
use super::state_reducers::StateReducer;
use super::yaml::{ParallelBranchDefinition, ParallelNodeDefinition};
use tracing::Instrument as _;

#[path = "parallel_checkpoint.rs"]
mod checkpoint;
use elitea_agent_runtime::graph::parallel_control as control;
#[path = "parallel_structure.rs"]
mod structure;
#[cfg(test)]
pub(crate) use checkpoint::MAX_PARENT_ROWS_PER_VISIT;
use checkpoint::{BranchReceipt, BranchReceiptCheckpointer, FrozenBranchInput};
pub(crate) use checkpoint::{ParallelBranchExecution, ParallelOccurrenceCheckpointer};
pub(in crate::agents::graph) use structure::{validate_state, validate_values};

const MAX_BRANCH_INPUT_BYTES: usize = 8 * 1024 * 1024;
const MAX_BRANCH_RESULT_BYTES: usize = MAX_CHILD_RESULT_BYTES;
const MAX_PAUSE_CARDS: usize = MAX_OPEN_INTERRUPTS;
const MAX_PAUSE_BYTES: usize = 512 * 1024;
const BRANCH_INPUT_DIGEST_DOMAIN: &[u8] = b"elitea.graph.parallel.branch-input.v1\0";
/// The parked interrupt payload (`libs/jsonschema/runtime/v1/fanout-parked.schema.json`).
pub(crate) const FANOUT_PARKED_SCHEMA: &str = "elitea.pipeline.fanout-parked.v1";
/// A branch whose Agent denied execution: a typed failure, never success.
pub(crate) const BRANCH_BLOCKED_CODE: &str = "graph.parallel.branch_blocked";
/// Earlier checkpoints of one child examined for an already answered pause.
const MAX_ANSWERED_PAUSE_SCAN: usize = 64;

/// Children running at once across this Worker process (every activation).
fn worker_child_permits() -> ChildPermits {
    static PERMITS: OnceLock<ChildPermits> = OnceLock::new();
    PERMITS.get_or_init(ChildPermits::default).clone()
}

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
/// One type for every fan-out family (Parallel branches and Map items).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FanoutChildOrigin {
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
}

/// The Parallel name for the shared fan-out origin.
pub(crate) type ParallelChildOrigin = FanoutChildOrigin;

impl FanoutChildOrigin {
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

pub(crate) enum ParallelNodeOutcome {
    /// Every admitted branch settled and the join landed on the output key.
    Completed(NodeOutput),
    /// Nothing can run and cards are open: the execution parks.
    Paused(NodeOutput),
}

#[derive(Clone)]
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

/// What one branch invocation, or one branch restored from its own
/// checkpoint, ended with.
pub(crate) enum ParallelBranchOutcome {
    Completed {
        outputs: serde_json::Map<String, Value>,
        checkpoint_id: String,
    },
    Paused {
        pause: ParallelBranchPause,
        cards: Vec<ParallelPauseCard>,
    },
    /// Typed, data-free. A blocked branch is a failure with
    /// [`BRANCH_BLOCKED_CODE`]. `checkpoint_id` is the saved failure receipt.
    Failed {
        failure: ChildFailure,
        checkpoint_id: Option<String>,
    },
    Cancelled,
    /// The writer fence was lost. A control stop, never a branch outcome.
    LeaseLost(GraphError),
}

pub(crate) struct PreparedParallelBranch {
    branch: ParallelBranchDefinition,
    ordinal: usize,
    input: State,
    /// The branch thread's own fenced store (reads and the receipt wrapper).
    store: Arc<dyn Checkpointer>,
    thread_id: String,
    admitted_threads: BTreeSet<String>,
    /// What the branch's own checkpoint already proves; `None` must run.
    replay: Option<ParallelBranchOutcome>,
}

impl PreparedParallelBranch {
    /// The same frozen child with nothing replayed: it runs when invoked.
    fn fresh(&self) -> Self {
        Self {
            branch: self.branch.clone(),
            ordinal: self.ordinal,
            input: self.input.clone(),
            store: Arc::clone(&self.store),
            thread_id: self.thread_id.clone(),
            admitted_threads: self.admitted_threads.clone(),
            replay: None,
        }
    }
}

#[async_trait]
pub(crate) trait ParallelBranchRuntime: Send + Sync {
    fn validate(&self, definition: &ParallelNodeDefinition) -> Result<(), GraphError>;

    /// Freeze the occurrence and restore every branch from its own checkpoint
    /// (one batched activation and one batched receipt read).
    async fn prepare(
        &self,
        activation: &mut ParallelActivation,
        definition: &ParallelNodeDefinition,
        context: &NodeContext,
    ) -> Result<Vec<PreparedParallelBranch>, GraphError>;

    /// Run one branch, or return what its checkpoint already proves.
    async fn invoke(
        &self,
        activation: &ParallelActivation,
        branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> ParallelBranchOutcome;

    fn pause_cards(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError>;

    fn project_result(
        &self,
        branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError>;

    /// Resume controls built from exactly this child's decisions.
    async fn resume_input(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError>;
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
    ) -> Result<Vec<PreparedParallelBranch>, GraphError> {
        self.validate(definition)?;
        let inputs = self.project_inputs(activation, definition, &context.state)?;
        let (origin, threads) = self.mint_lineage(activation, definition, &inputs)?;
        let occurrence = self
            .parent
            .freeze(activation, context, inputs, origin, threads)
            .await?;
        self.restore_branches(
            activation,
            definition,
            &occurrence.branches,
            &occurrence.origin,
            &occurrence.child_threads,
        )
        .await
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
        let checkpoint = Arc::new(BranchReceiptCheckpointer::new(
            Arc::clone(&branch.store),
            branch.thread_id.clone(),
        ));
        let result = self
            .invoke_inner(activation, branch, Arc::clone(&checkpoint), context)
            .await;
        // Lease loss outranks cancellation: the claim machinery must see it unchanged.
        match result {
            Err(error) if is_lease_lost(&error) => ParallelBranchOutcome::LeaseLost(error),
            _ if is_cancelled(context) => ParallelBranchOutcome::Cancelled,
            Ok(outcome) => outcome,
            Err(error) => {
                // A failed child saved a failure receipt before returning.
                let checkpoint_id = checkpoint
                    .last_saved()
                    .ok()
                    .flatten()
                    .filter(|saved| matches!(saved.receipt, Some(BranchReceipt::Failed { .. })))
                    .map(|saved| saved.checkpoint_id);
                ParallelBranchOutcome::Failed {
                    failure: branch_failure(graph_error_code(&error)),
                    checkpoint_id,
                }
            }
        }
    }

    fn pause_cards(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        let cards = self.graphs.pause_cards(branch, pause)?;
        validate_cards(&cards)?;
        Ok(cards)
    }

    fn project_result(
        &self,
        branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        self.graphs.project_result(branch, child)
    }

    async fn resume_input(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        self.graphs.resume_input(branch, pause, decisions).await
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

    async fn restore_branches(
        &self,
        activation: &ParallelActivation,
        definition: &ParallelNodeDefinition,
        inputs: &[FrozenBranchInput],
        origin: &ParallelChildOrigin,
        child_threads: &[String],
    ) -> Result<Vec<PreparedParallelBranch>, GraphError> {
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
        let mut prepared = Vec::with_capacity(inputs.len());
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
            let replay = match latest {
                Some(saved) => {
                    restored_outcome(self, &branch, &child.thread_id, &child.checkpointer, saved)?
                }
                None => None,
            };
            prepared.push(PreparedParallelBranch {
                branch,
                ordinal: input.ordinal,
                input: input.input,
                store: child.checkpointer,
                thread_id: child.thread_id,
                admitted_threads: child.admitted_threads,
                replay,
            });
        }
        Ok(prepared)
    }

    async fn invoke_inner(
        &self,
        activation: &ParallelActivation,
        branch: PreparedParallelBranch,
        checkpoint: Arc<BranchReceiptCheckpointer>,
        context: &NodeContext,
    ) -> Result<ParallelBranchOutcome, GraphError> {
        let graph = self.graphs.compile_branch(
            &branch.branch,
            ParallelBranchExecution::new(
                Arc::clone(&checkpoint),
                activation.clone(),
                branch.branch.node().to_owned(),
                branch.ordinal,
                branch.thread_id.clone(),
                branch.admitted_threads.clone(),
                context
                    .state
                    .get(super::node_events::PIPELINE_NODE_EVENT_SCOPE_STATE_KEY)
                    .cloned(),
            )
            .with_static_after_checkpoints(
                branch
                    .input
                    .get(super::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY)
                    .and_then(|value| serde_json::from_value(value.clone()).ok())
                    .unwrap_or_default(),
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
                let saved = checkpoint.last_saved()?.ok_or_else(|| {
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
                terminal_outcome(terminal, saved.checkpoint_id)
            }
            Err(GraphError::Interrupted(interrupted)) => {
                let saved = checkpoint.last_saved()?.ok_or_else(|| {
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
                    checkpointer: Arc::clone(&branch.store),
                };
                let cards = self.pause_cards(&branch.branch, &pause)?;
                Ok(ParallelBranchOutcome::Paused { pause, cards })
            }
            Err(error) => Err(error),
        }
    }
}

/// Classify one child's saved checkpoint by its lifecycle receipt only.
/// `None`: not started or found mid-run, so the child must run.
fn restored_outcome(
    runtime: &dyn ParallelBranchRuntime,
    branch: &ParallelBranchDefinition,
    thread_id: &str,
    store: &Arc<dyn Checkpointer>,
    saved: Checkpoint,
) -> Result<Option<ParallelBranchOutcome>, GraphError> {
    if saved.thread_id != thread_id {
        return Err(parallel_error(
            "graph.parallel.corrupt_receipt",
            "the child checkpoint belongs to another thread",
        ));
    }
    Ok(match BranchReceiptCheckpointer::receipt(&saved)? {
        Some(BranchReceipt::Completed) if saved.pending_nodes.is_empty() => Some(terminal_outcome(
            runtime.project_result(branch, &saved.state)?,
            saved.checkpoint_id,
        )?),
        Some(BranchReceipt::Paused { interrupt }) if !saved.pending_nodes.is_empty() => {
            let pause = ParallelBranchPause {
                thread_id: thread_id.to_owned(),
                checkpoint_id: saved.checkpoint_id,
                interrupt,
                checkpointer: Arc::clone(store),
            };
            let cards = runtime.pause_cards(branch, &pause)?;
            Some(ParallelBranchOutcome::Paused { pause, cards })
        }
        Some(BranchReceipt::Failed { code }) if valid_failure_code(&code) => {
            Some(ParallelBranchOutcome::Failed {
                failure: branch_failure(&code),
                checkpoint_id: Some(saved.checkpoint_id),
            })
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
    })
}

/// The member ordinal is 1-based (contract §3.1).
fn card_identities(
    activation: &ParallelActivation,
    ordinal: usize,
    thread_id: &str,
    pause_checkpoint_id: &str,
    cards: &[ParallelPauseCard],
) -> Result<Vec<CardIdentity>, GraphError> {
    let config_digest =
        elitea_agent_runtime::graph::fanout::identity::lowercase_hex(&activation.config_digest);
    let ordinal = u64::try_from(ordinal + 1).map_err(|_| invalid_pause())?;
    Ok(cards
        .iter()
        .map(|card| CardIdentity {
            interrupt_key: interrupt_key(&InterruptKeyInput {
                root_thread: &activation.root_thread_id,
                fanout_node_id: &activation.node_id,
                step: activation.step,
                config_digest: &config_digest,
                ordinal,
                child_thread: thread_id,
                child_paused_checkpoint_id: pause_checkpoint_id,
                interrupt_id: &card.interrupt_id,
                tool_call_id: &card.tool_call_id,
            }),
            interrupt_id: card.interrupt_id.clone(),
            child_paused_checkpoint_id: pause_checkpoint_id.to_owned(),
        })
        .collect())
}

/// The planner's children as [`FanoutRunner`] sees them. Every future it
/// returns owns its inputs (`'static`).
struct ParallelChildren {
    runtime: Arc<dyn ParallelBranchRuntime>,
    activation: ParallelActivation,
    context: Arc<NodeContext>,
    members: Vec<(String, String)>,
    branches: Vec<Arc<PreparedParallelBranch>>,
    /// Set while a child's batched preparation read is still its truth: its
    /// first run needs no second read (one fenced probe per fresh child).
    unread: Vec<AtomicBool>,
    restored: std::sync::Mutex<Option<Vec<RestoredChild>>>,
    /// The first infrastructure error a child adapter met; the node reports
    /// it instead of the runner's control code.
    fault: Arc<std::sync::Mutex<Option<GraphError>>>,
}

impl ParallelChildren {
    fn new(
        runtime: Arc<dyn ParallelBranchRuntime>,
        activation: ParallelActivation,
        context: Arc<NodeContext>,
        prepared: Vec<PreparedParallelBranch>,
    ) -> Result<Self, GraphError> {
        let mut members = Vec::with_capacity(prepared.len());
        let mut restored = Vec::with_capacity(prepared.len());
        let mut branches = Vec::with_capacity(prepared.len());
        let mut unread = Vec::with_capacity(prepared.len());
        for mut branch in prepared {
            members.push((
                branch.branch.id().to_owned(),
                branch.branch.node().to_owned(),
            ));
            unread.push(AtomicBool::new(branch.replay.is_none()));
            restored.push(match branch.replay.take() {
                None => RestoredChild::Pending,
                Some(replay) => match to_child_run(&activation, &branch, replay, "")? {
                    ChildRun::Settled { terminal, .. } => RestoredChild::Settled(terminal),
                    ChildRun::Paused {
                        checkpoint_id,
                        cards,
                    } => RestoredChild::Paused {
                        checkpoint_id,
                        cards,
                    },
                    ChildRun::LeaseLost | ChildRun::Cancelled => {
                        return Err(parallel_error(
                            "graph.parallel.corrupt_receipt",
                            "a restored child holds no lifecycle receipt",
                        ));
                    }
                },
            });
            branches.push(Arc::new(branch));
        }
        Ok(Self {
            runtime,
            activation,
            context,
            members,
            branches,
            unread,
            restored: std::sync::Mutex::new(Some(restored)),
            fault: Arc::new(std::sync::Mutex::new(None)),
        })
    }

    fn take_fault(&self) -> Option<GraphError> {
        self.fault.lock().ok().and_then(|mut fault| fault.take())
    }

    /// Record an adapter error and give the runner its control code.
    fn fault(&self, error: GraphError) -> FanoutRunError {
        let code = if is_lease_lost(&error) {
            FanoutRunError::LeaseLost
        } else {
            FanoutRunError::CorruptChild
        };
        record_fault(&self.fault, error);
        code
    }

    async fn view(&self, child: usize) -> Result<ChildCheckpointView, GraphError> {
        let branch = self.branch(child)?;
        let Some(saved) = branch.store.load(&branch.thread_id).await? else {
            return Ok(ChildCheckpointView::Running {
                checkpoint_id: None,
            });
        };
        let checkpoint_id = saved.checkpoint_id.clone();
        Ok(
            match restored_outcome(
                self.runtime.as_ref(),
                &branch.branch,
                &branch.thread_id,
                &branch.store,
                saved,
            )? {
                None => ChildCheckpointView::Running {
                    checkpoint_id: Some(checkpoint_id),
                },
                Some(ParallelBranchOutcome::Paused { pause, cards }) => {
                    ChildCheckpointView::Paused {
                        checkpoint_id: pause.checkpoint_id,
                        interrupt_ids: cards.into_iter().map(|card| card.interrupt_id).collect(),
                    }
                }
                Some(_) => ChildCheckpointView::Terminal { checkpoint_id },
            },
        )
    }

    fn branch(&self, child: usize) -> Result<&Arc<PreparedParallelBranch>, GraphError> {
        self.branches.get(child).ok_or_else(|| {
            parallel_error(
                "graph.parallel.corrupt_occurrence",
                "a fan-out child index is invalid",
            )
        })
    }

    async fn find_answered(
        &self,
        interrupt_key: &str,
    ) -> Result<Option<(usize, ChildCheckpointView)>, GraphError> {
        for (child, branch) in self.branches.iter().enumerate() {
            let history = branch.store.list(&branch.thread_id).await?;
            let start = history.len().saturating_sub(MAX_ANSWERED_PAUSE_SCAN);
            for saved in history.into_iter().skip(start) {
                if saved.thread_id != branch.thread_id || saved.pending_nodes.is_empty() {
                    continue;
                }
                let Some(BranchReceipt::Paused { interrupt }) =
                    BranchReceiptCheckpointer::receipt(&saved)?
                else {
                    continue;
                };
                let pause = ParallelBranchPause {
                    thread_id: branch.thread_id.clone(),
                    checkpoint_id: saved.checkpoint_id,
                    interrupt,
                    checkpointer: Arc::clone(&branch.store),
                };
                let cards = self.runtime.pause_cards(&branch.branch, &pause)?;
                let keys = card_identities(
                    &self.activation,
                    branch.ordinal,
                    &branch.thread_id,
                    &pause.checkpoint_id,
                    &cards,
                )?;
                if keys.iter().any(|card| card.interrupt_key == interrupt_key) {
                    return Ok(Some((child, self.view(child).await?)));
                }
            }
        }
        Ok(None)
    }
}

fn record_fault(slot: &std::sync::Mutex<Option<GraphError>>, error: GraphError) {
    if let Ok(mut fault) = slot.lock() {
        fault.get_or_insert(error);
    }
}

#[async_trait]
impl FanoutChildren for ParallelChildren {
    fn members(&self) -> &[(String, String)] {
        &self.members
    }

    async fn restore(&self) -> Result<Vec<RestoredChild>, FanoutRunError> {
        self.restored
            .lock()
            .ok()
            .and_then(|mut restored| restored.take())
            .ok_or(FanoutRunError::CorruptChild)
    }

    async fn latest(&self, child: usize) -> Result<ChildCheckpointView, FanoutRunError> {
        self.view(child).await.map_err(|error| self.fault(error))
    }

    fn run(
        &self,
        child: usize,
        decisions: Vec<FetchedDecision>,
    ) -> adk_rust::futures::future::BoxFuture<'static, ChildRun> {
        let runtime = Arc::clone(&self.runtime);
        let activation = self.activation.clone();
        let context = Arc::clone(&self.context);
        let fault = Arc::clone(&self.fault);
        let branch = self.branches.get(child).cloned();
        let unread = decisions.is_empty()
            && self
                .unread
                .get(child)
                .is_some_and(|unread| unread.swap(false, Ordering::AcqRel));
        Box::pin(async move {
            let Some(branch) = branch else {
                record_fault(
                    &fault,
                    parallel_error(
                        "graph.parallel.corrupt_occurrence",
                        "a fan-out child index is invalid",
                    ),
                );
                return ChildRun::Cancelled;
            };
            let span =
                fanout_trace::child_span(FanoutKind::Parallel, &activation.node_id, branch.ordinal);
            let run = if unread {
                // Prepared without a receipt moments ago: run it.
                fanout_trace::child_admitted(&span, ChildStart::Fresh);
                let outcome = runtime
                    .invoke(&activation, branch.fresh(), &context)
                    .instrument(span.clone())
                    .await;
                to_child_run(&activation, &branch, outcome, "")
            } else {
                run_child(&*runtime, &activation, &context, &branch, decisions, &span)
                    .instrument(span.clone())
                    .await
            };
            match run {
                Ok(run) => {
                    fanout_trace::child_finished(
                        &span,
                        match &run {
                            ChildRun::Settled {
                                terminal: ChildTerminal::Succeeded(_),
                                ..
                            } => ChildOutcome::Completed,
                            ChildRun::Settled { .. } => ChildOutcome::Failed,
                            ChildRun::Paused { .. } => ChildOutcome::Paused,
                            ChildRun::LeaseLost => ChildOutcome::LeaseLost,
                            ChildRun::Cancelled => ChildOutcome::Cancelled,
                        },
                    );
                    if matches!(run, ChildRun::LeaseLost) {
                        record_fault(&fault, lease_lost_error());
                    }
                    run
                }
                Err(error) => {
                    fanout_trace::child_finished(
                        &span,
                        if is_lease_lost(&error) {
                            ChildOutcome::LeaseLost
                        } else {
                            ChildOutcome::Failed
                        },
                    );
                    let lease_lost = is_lease_lost(&error);
                    record_fault(&fault, error);
                    if lease_lost {
                        ChildRun::LeaseLost
                    } else {
                        // Not a business outcome: the activation stops typed.
                        ChildRun::Cancelled
                    }
                }
            }
        })
    }

    async fn answered_pause(
        &self,
        interrupt_key: &str,
    ) -> Result<Option<(usize, ChildCheckpointView)>, FanoutRunError> {
        self.find_answered(interrupt_key)
            .await
            .map_err(|error| self.fault(error))
    }
}

/// One child run: replay what its own checkpoint proves, continue it, or
/// resume it with exactly its decisions.
async fn run_child(
    runtime: &dyn ParallelBranchRuntime,
    activation: &ParallelActivation,
    context: &NodeContext,
    branch: &PreparedParallelBranch,
    decisions: Vec<FetchedDecision>,
    span: &tracing::Span,
) -> Result<ChildRun, GraphError> {
    let latest = branch.store.load(&branch.thread_id).await?;
    let latest_id = latest
        .as_ref()
        .map(|saved| saved.checkpoint_id.clone())
        .unwrap_or_default();
    let restored = match latest {
        Some(saved) => restored_outcome(
            runtime,
            &branch.branch,
            &branch.thread_id,
            &branch.store,
            saved,
        )?,
        None => None,
    };
    if decisions.is_empty() {
        if let Some(replay) = restored {
            // Settled or paused already: never run completed work again.
            fanout_trace::child_admitted(span, ChildStart::Restored);
            return to_child_run(activation, branch, replay, &latest_id);
        }
        fanout_trace::child_admitted(span, ChildStart::Fresh);
        let outcome = runtime.invoke(activation, branch.fresh(), context).await;
        return to_child_run(activation, branch, outcome, &latest_id);
    }
    let Some(ParallelBranchOutcome::Paused { pause, cards }) = restored else {
        return Err(stale_decision());
    };
    let identities = card_identities(
        activation,
        branch.ordinal,
        &branch.thread_id,
        &pause.checkpoint_id,
        &cards,
    )?;
    let mut selected = Vec::with_capacity(decisions.len());
    for decision in &decisions {
        let card = identities
            .iter()
            .position(|identity| identity.interrupt_key == decision.interrupt_key)
            .and_then(|position| cards.get(position))
            .ok_or_else(stale_decision)?;
        selected.push(parallel_decision(card, decision)?);
    }
    let controls = runtime
        .resume_input(&branch.branch, &pause, &selected)
        .await?;
    if controls.keys().any(|key| !is_resume_key(key)) {
        return Err(parallel_error(
            "graph.parallel.invalid_resume",
            "a branch continuation contains a business update",
        ));
    }
    fanout_trace::child_admitted(span, ChildStart::Resumed);
    let mut resumed = branch.fresh();
    resumed.input.extend(controls);
    let outcome = runtime.invoke(activation, resumed, context).await;
    to_child_run(activation, branch, outcome, &pause.checkpoint_id)
}

/// `fallback` names the checkpoint the child stood at when its run left no
/// newer receipt (a failure before any save).
fn to_child_run(
    activation: &ParallelActivation,
    branch: &PreparedParallelBranch,
    outcome: ParallelBranchOutcome,
    fallback: &str,
) -> Result<ChildRun, GraphError> {
    Ok(match outcome {
        ParallelBranchOutcome::Completed {
            outputs,
            checkpoint_id,
        } => ChildRun::Settled {
            terminal: ChildTerminal::Succeeded(outputs),
            checkpoint_id,
        },
        ParallelBranchOutcome::Failed {
            failure,
            checkpoint_id,
        } => ChildRun::Settled {
            terminal: ChildTerminal::Failed(failure),
            checkpoint_id: checkpoint_id.unwrap_or_else(|| fallback.to_owned()),
        },
        ParallelBranchOutcome::Paused { pause, cards } => ChildRun::Paused {
            cards: card_identities(
                activation,
                branch.ordinal,
                &branch.thread_id,
                &pause.checkpoint_id,
                &cards,
            )?,
            checkpoint_id: pause.checkpoint_id,
        },
        ParallelBranchOutcome::Cancelled => ChildRun::Cancelled,
        ParallelBranchOutcome::LeaseLost(error) => return Err(error),
    })
}

/// A fetched decision for one card of this exact pause, bounded.
fn parallel_decision(
    card: &ParallelPauseCard,
    decision: &FetchedDecision,
) -> Result<ParallelDecision, GraphError> {
    let value = decision.value.clone().unwrap_or_default();
    if !matches!(
        decision.action.as_str(),
        "approve"
            | "reject"
            | "edit"
            | "block_with_comment"
            | "authorize"
            | "skip"
            | "answer"
            | "continue"
    ) || value.chars().count() > MAX_DECISION_VALUE_CHARS
        || value.contains('\0')
    {
        return Err(parallel_error(
            "graph.parallel.invalid_decision",
            "a fan-out decision is outside its contract",
        ));
    }
    Ok(ParallelDecision {
        interrupt_id: card.interrupt_id.clone(),
        tool_call_id: card.tool_call_id.clone(),
        action: decision.action.clone(),
        value,
    })
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
    cancellation: Option<Arc<FanoutCancellation>>,
    decisions: Arc<dyn DecisionSource>,
    events: Arc<dyn FanoutEvents>,
    permits: ChildPermits,
    /// The output key's declared reducer, if it declares one.
    output_reducer: Option<StateReducer>,
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
            cancellation: None,
            decisions: Arc::new(NoDecisions),
            events: Arc::new(NoEvents),
            permits: worker_child_permits(),
            output_reducer: None,
        }
    }

    /// Supply the owner's cancellation latch. The owner fires it; the node never polls.
    pub(crate) fn with_cancellation(mut self, cancellation: Arc<FanoutCancellation>) -> Self {
        self.cancellation = Some(cancellation);
        self
    }

    /// Supply the deadline from root execution authority at assembly.
    pub(crate) fn with_deadline(mut self, deadline: tokio::time::Instant) -> Self {
        self.deadline = Some(deadline);
        self
    }

    /// Main's claim-fenced decision ledger. Default: nothing is decided.
    pub(crate) fn with_decisions(mut self, decisions: Arc<dyn DecisionSource>) -> Self {
        self.decisions = decisions;
        self
    }

    /// Card and member frames. Default: none.
    pub(crate) fn with_events(mut self, events: Arc<dyn FanoutEvents>) -> Self {
        self.events = events;
        self
    }

    /// The child slots. Default: the process-wide pool.
    pub(crate) fn with_permits(mut self, permits: ChildPermits) -> Self {
        self.permits = permits;
        self
    }

    /// The output key's typed reducer, checked against the join at compile time.
    pub(crate) fn with_output_reducer(mut self, reducer: Option<StateReducer>) -> Self {
        self.output_reducer = reducer;
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
            false,
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
                Err(error) if is_lease_lost(error) => ActivationOutcome::LeaseLost,
                Err(GraphError::NodeExecutionFailed { message, .. })
                    if message.starts_with("graph.parallel.cancel") =>
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
        let stop = Arc::new(FanoutStop::default());
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
        let children = Arc::new(ParallelChildren::new(
            Arc::clone(&self.runtime),
            activation,
            Arc::new(run_context),
            prepared,
        )?);
        let max_concurrency = usize::try_from(self.definition.max_concurrency()).map_err(|_| {
            parallel_error(
                "graph.parallel.invalid_configuration",
                "the parallel concurrency does not fit this platform",
            )
        })?;
        let runner = FanoutRunner::new(
            children.clone(),
            Arc::clone(&self.decisions),
            Arc::clone(&self.events),
            self.permits.clone(),
            Arc::clone(&stop),
            FanoutRunnerConfig {
                max_concurrency,
                on_error: self.definition.on_error(),
                deadline: self.deadline,
            },
        )
        .map_err(|error| runner_error(error, None))?;
        let outcome = self.drive(runner, &stop, &cancel_signal).await;
        let outcome = outcome.map_err(|error| runner_error(error, children.take_fault()))?;
        // A stop that raced the last child still wins over its result.
        if is_cancelled(context)
            || self
                .cancellation
                .as_deref()
                .is_some_and(FanoutCancellation::is_cancelled)
        {
            return Err(cancelled_error());
        }
        match outcome {
            RunnerOutcome::Settled(settled) => self.land(settled, context),
            RunnerOutcome::Parked {
                open,
                decision_revision_seen,
            } => Ok(ParallelNodeOutcome::Paused(
                NodeOutput::interrupt_with_data(
                    "Parallel branches paused.",
                    json!({
                        "schema": FANOUT_PARKED_SCHEMA,
                        "decision_revision_seen": decision_revision_seen,
                        "pending": open
                            .iter()
                            .map(|card| json!({
                                "interrupt_key": card.interrupt_key,
                                "interrupt_id": card.interrupt_id,
                            }))
                            .collect::<Vec<_>>(),
                    }),
                ),
            )),
        }
    }

    /// Run the runner while relaying the owner's latch into the stop, and the
    /// stop into every child's cancellation flag. Nothing here polls.
    async fn drive(
        &self,
        runner: FanoutRunner,
        stop: &FanoutStop,
        cancel_signal: &AtomicBool,
    ) -> Result<RunnerOutcome, FanoutRunError> {
        let run = runner.run();
        tokio::pin!(run);
        let latch = self.cancellation.as_deref();
        let (mut latched, mut relayed) = (false, false);
        loop {
            tokio::select! {
                biased;
                () = latch_cancelled(latch), if !latched => {
                    latched = true;
                    stop.fire();
                }
                () = stop.fired(), if !relayed => {
                    relayed = true;
                    cancel_signal.store(true, Ordering::Release);
                }
                result = &mut run => return result,
            }
        }
    }

    fn land(
        &self,
        settled: Vec<plan::SettledChild>,
        context: &NodeContext,
    ) -> Result<ParallelNodeOutcome, GraphError> {
        let entries = match plan::join(JoinKind::Parallel, self.definition.on_error(), settled)
            .map_err(join_error)?
        {
            JoinOutcome::Joined(entries) => entries,
            JoinOutcome::Failed { key, failure } => {
                return Err(GraphError::NodeExecutionFailed {
                    node: self.definition.id().to_owned(),
                    message: format!(
                        "graph.parallel.branch_failed: branch '{key}' failed after all admitted branches drained ({})",
                        failure.code,
                    ),
                });
            }
        };
        let output_key = self.definition.output_key();
        let update = plan::land(
            self.definition.join(),
            self.output_reducer,
            JoinKind::Parallel,
            context.state.get(output_key),
            entries,
        )
        .map_err(join_error)?;
        Ok(ParallelNodeOutcome::Completed(
            NodeOutput::new().with_update(output_key, update),
        ))
    }

    fn check_running(&self, context: &NodeContext) -> Result<(), GraphError> {
        if is_cancelled(context)
            || self
                .cancellation
                .as_deref()
                .is_some_and(FanoutCancellation::is_cancelled)
        {
            return Err(cancelled_error());
        }
        if self
            .deadline
            .is_some_and(|deadline| deadline <= tokio::time::Instant::now())
        {
            return Err(deadline_error());
        }
        Ok(())
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

/// Runner control codes as typed node errors. `fault` is the adapter's own
/// error, which names the real cause (a fenced write, a corrupt receipt).
fn runner_error(error: FanoutRunError, fault: Option<GraphError>) -> GraphError {
    match error {
        FanoutRunError::LeaseLost => fault.filter(is_lease_lost).unwrap_or_else(lease_lost_error),
        FanoutRunError::Cancelled
        | FanoutRunError::CancelCleanupExceeded
        | FanoutRunError::CorruptChild
            if fault.is_some() =>
        {
            fault.unwrap_or_else(cancelled_error)
        }
        FanoutRunError::Cancelled => cancelled_error(),
        FanoutRunError::CancelCleanupExceeded => parallel_error(
            "graph.parallel.cancellation_cleanup_failed",
            "parallel cancellation cleanup exceeded its bound",
        ),
        FanoutRunError::DeadlineExceeded => deadline_error(),
        FanoutRunError::TooManyOpenInterrupts => parallel_error(
            "graph.parallel.pause_resource_exhausted",
            "the parallel pause card count exceeds its bound",
        ),
        FanoutRunError::InvalidConfiguration => parallel_error(
            "graph.parallel.invalid_configuration",
            "the parallel runner configuration is invalid",
        ),
        FanoutRunError::CorruptChild => parallel_error(
            "graph.parallel.corrupt_receipt",
            "a parallel child checkpoint contradicts its card",
        ),
    }
}

fn join_error(error: plan::FanoutPlanError) -> GraphError {
    parallel_error(
        &error.code(),
        "the parallel joined result cannot land on its output key",
    )
}

fn cancelled_error() -> GraphError {
    parallel_error(
        "graph.parallel.cancelled",
        "the parallel execution was cancelled",
    )
}

fn deadline_error() -> GraphError {
    parallel_error(
        "graph.parallel.deadline_exceeded",
        "the parallel execution exceeded its deadline",
    )
}

fn lease_lost_error() -> GraphError {
    GraphError::CheckpointError(
        "checkpoint.writer_not_current: the fan-out child writer is no longer current".to_owned(),
    )
}

fn stale_decision() -> GraphError {
    parallel_error(
        "graph.parallel.stale_decision",
        "the decision does not answer the child's current pause",
    )
}

fn invalid_pause() -> GraphError {
    parallel_error(
        "graph.parallel.invalid_pause",
        "the branch pause card identity is invalid",
    )
}

/// A typed child failure with a fixed safe message per code.
fn branch_failure(code: &str) -> ChildFailure {
    ChildFailure::new(
        code,
        if code == BRANCH_BLOCKED_CODE {
            "The branch was blocked."
        } else {
            "The branch failed."
        },
    )
}

fn terminal_outcome(
    terminal: ParallelBranchTerminal,
    checkpoint_id: String,
) -> Result<ParallelBranchOutcome, GraphError> {
    match terminal {
        ParallelBranchTerminal::Completed(outputs) => {
            validate_values(outputs.values())?;
            ensure_bounded_json(
                &Value::Object(outputs.clone()),
                MAX_BRANCH_RESULT_BYTES,
                "branch result",
            )?;
            Ok(ParallelBranchOutcome::Completed {
                outputs,
                checkpoint_id,
            })
        }
        ParallelBranchTerminal::Blocked => Ok(ParallelBranchOutcome::Failed {
            failure: branch_failure(BRANCH_BLOCKED_CODE),
            checkpoint_id: Some(checkpoint_id),
        }),
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
            !is_resume_key(key) && key.as_str() != "__elitea_pipeline_node_event_scope_v1"
        })
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect()
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
        if !valid_interrupt_id(&card.interrupt_id)
            || (!card.tool_call_id.is_empty() && !valid_card_identity(&card.tool_call_id))
            || !unique.insert((&card.interrupt_id, &card.tool_call_id))
        {
            return Err(invalid_pause());
        }
    }
    Ok(())
}

/// An interrupt id is printable ASCII, 1–512 bytes (contract §3.2): Web and
/// Main key cards by it. A tool call id only excludes control characters.
fn valid_interrupt_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 512
        && value.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
        && !value.starts_with(' ')
}

fn valid_card_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 512 && !value.chars().any(char::is_control)
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
