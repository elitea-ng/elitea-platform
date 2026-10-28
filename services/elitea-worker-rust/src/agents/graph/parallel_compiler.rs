//! Fixed declared branches. Dynamic item Map and Reduce have separate contracts.

use async_trait::async_trait;

use super::{
    FanoutChildRefusal, PipelineConfigurationError, PipelineDefinition, PipelineGraphBuilder,
    PipelineNodeDefinition, PipelineNodeRuntimes, state_value_matches,
};
use crate::agents::graph::application::{
    APPLICATION_MESSAGES_STATE_KEY, APPLICATION_RESULT_STATE_KEY, APPLICATION_TASK_STATE_KEY,
    ApplicationNode, ApplicationNodeDefinition, PipelineApplicationResolver,
};
use crate::agents::graph::direct_tool::DIRECT_TOOL_RESUME_STATE_KEY;
use crate::agents::graph::fanout_control::FanoutCancellation;
use crate::agents::graph::hitl::HITL_RESUME_STATE_KEY;
use crate::agents::graph::llm::{
    LLM_TOOL_RESUME_STATE_KEY, LlmNode, LlmNodeDefinition, PARALLEL_LLM_INPUTS_STATE_KEY,
    PipelineLlmAgentFactory,
};
use crate::agents::graph::node_events::{
    PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventSender,
};
use crate::agents::graph::parallel::{
    AdkParallelBranchRuntime, DurableParallelNode, ParallelBranchExecution,
    ParallelBranchGraphFactory, ParallelBranchPause, ParallelBranchTerminal,
    ParallelCheckpointAuthority, ParallelDecision, ParallelOccurrenceCheckpointer,
    ParallelPauseCard,
};
use crate::agents::graph::yaml::ParallelNodeDefinition;
use adk_rust::graph::{
    Channel, Checkpoint, Checkpointer, CompiledGraph, END, GraphError, Node, NodeContext,
    NodeOutput, START, State, StateGraph, StateSchema,
};
use elitea_agent_runtime::graph::fanout::runner::DecisionSource;
use ring::digest;
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

/// The run's root session, read when a saved Agent member's own pause is
/// decided (FU-246): its forwarded confirmation and history are the proof the
/// Agent's nested decision registry resumes from.
#[async_trait::async_trait]
pub(crate) trait RootSessionSource: Send + Sync {
    async fn root_session(&self) -> Result<Box<dyn adk_rust::session::Session>, GraphError>;
}

pub(crate) struct ParallelCompilerBinding {
    authority: Arc<dyn ParallelCheckpointAuthority>,
    occurrence: Arc<ParallelOccurrenceCheckpointer>,
    deadline: tokio::time::Instant,
    /// One latch per run, fired by the run's durable Stop.
    cancellation: Arc<FanoutCancellation>,
    /// The claim's fenced decision ledger (none: nothing is ever decided).
    decisions: Option<Arc<dyn DecisionSource>>,
    /// The run's root session (none: a saved Agent member's pause cannot be resumed).
    root_session: Option<Arc<dyn RootSessionSource>>,
}

impl ParallelCompilerBinding {
    pub(crate) fn new(
        authority: Arc<dyn ParallelCheckpointAuthority>,
        deadline: tokio::time::Instant,
        decisions: Arc<dyn DecisionSource>,
    ) -> Arc<Self> {
        Arc::new(Self::bound(
            authority,
            deadline,
            Arc::default(),
            Some(decisions),
        ))
    }

    fn bound(
        authority: Arc<dyn ParallelCheckpointAuthority>,
        deadline: tokio::time::Instant,
        cancellation: Arc<FanoutCancellation>,
        decisions: Option<Arc<dyn DecisionSource>>,
    ) -> Self {
        Self {
            occurrence: Arc::new(ParallelOccurrenceCheckpointer::new(authority.clone())),
            authority,
            deadline,
            cancellation,
            decisions,
            root_session: None,
        }
    }

    /// The run's root session, for resuming a saved Agent member's own pause.
    pub(crate) fn with_root_session(
        mut self: Arc<Self>,
        source: Arc<dyn RootSessionSource>,
    ) -> Arc<Self> {
        if let Some(binding) = Arc::get_mut(&mut self) {
            binding.root_session = Some(source);
        }
        self
    }

    /// The run's fan-out latch; the run's durable Stop fires it.
    pub(crate) fn cancellation(&self) -> Arc<FanoutCancellation> {
        Arc::clone(&self.cancellation)
    }

    pub(crate) fn parent_checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.authority.clone()
    }

    #[cfg(test)]
    pub(crate) fn for_tests(
        authority: Arc<dyn ParallelCheckpointAuthority>,
        deadline: tokio::time::Instant,
        decisions: Option<Arc<dyn DecisionSource>>,
    ) -> Arc<Self> {
        Arc::new(Self::bound(authority, deadline, Arc::default(), decisions))
    }
}

impl PipelineNodeRuntimes {
    pub(crate) fn with_parallel_events(
        mut self,
        events: PipelineNodeEventSender,
    ) -> Result<Self, crate::agents::graph::application::ApplicationExecutionError> {
        if let Some(resolver) = self.application.as_ref() {
            self.application = Some(resolver.for_parallel_events(events.clone())?);
        }
        // An LLM node emits through its factory's sender: a member's saved
        // pipeline routes it through the branch's proved scope too.
        if let Some(factory) = self.llm.take() {
            self.llm = Some(Arc::new(ParallelLlmEvents {
                inner: factory,
                events: events.clone(),
            }));
        }
        self.events = Some(events);
        Ok(self)
    }

    pub(crate) fn with_parallel(mut self, binding: Arc<ParallelCompilerBinding>) -> Self {
        self.parallel = Some(binding);
        self
    }
}

/// An LLM factory whose node events go through a Parallel branch's sender.
struct ParallelLlmEvents {
    inner: Arc<dyn crate::agents::graph::llm::PipelineLlmAgentFactory>,
    events: PipelineNodeEventSender,
}

impl crate::agents::graph::llm::PipelineLlmAgentFactory for ParallelLlmEvents {
    fn build(
        &self,
        definition: &crate::agents::graph::llm::LlmNodeDefinition,
        input: &crate::agents::graph::llm::LlmExecutionInput,
        output_schema: Option<serde_json::Value>,
        replay: Option<&crate::agents::graph::llm::PipelineLlmReplayEnvelope>,
        scope: &crate::agents::graph::llm::PipelineModelScope,
    ) -> Result<
        crate::agents::graph::llm::PipelineLlmAgentBinding,
        crate::agents::graph::llm::LlmExecutionError,
    > {
        self.inner
            .build(definition, input, output_schema, replay, scope)
    }

    fn event_sender(&self) -> Option<PipelineNodeEventSender> {
        self.inner.event_sender().map(|_| self.events.clone())
    }

    fn for_fanout_branch(
        &self,
    ) -> Option<Arc<dyn crate::agents::graph::llm::PipelineLlmAgentFactory>> {
        self.inner.for_fanout_branch()
    }
}

impl PipelineDefinition {
    pub(crate) fn has_parallel_nodes(&self) -> bool {
        self.nodes
            .iter()
            .any(|node| matches!(node, PipelineNodeDefinition::Parallel(_)))
    }

    pub(crate) fn first_parallel_node(&self) -> Option<&str> {
        self.nodes.iter().find_map(|node| match node {
            PipelineNodeDefinition::Parallel(node) => Some(node.id()),
            _ => None,
        })
    }

    pub(super) fn bind_parallel_authority(
        &self,
        checkpointer: Arc<dyn Checkpointer>,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<(Arc<dyn Checkpointer>, PipelineNodeRuntimes), PipelineConfigurationError> {
        let mut runtimes = runtimes.clone();
        if !self.has_parallel_nodes() {
            return Ok((checkpointer, runtimes));
        }
        let binding = runtimes
            .parallel
            .as_ref()
            .ok_or(PipelineConfigurationError::Unsupported(
                "the fixed Parallel atomic checkpoint authority is not bound",
            ))?;
        if !Arc::ptr_eq(&checkpointer, &binding.parent_checkpointer()) {
            return Err(PipelineConfigurationError::Invalid(
                "fixed Parallel parent and child authority do not share the admitted checkpointer",
            ));
        }
        let authority = match runtimes.application_scopes() {
            Some(scopes) => scopes
                .wrap_parallel_authority(
                    &runtimes.application_scope_path,
                    Arc::clone(&binding.authority),
                )
                .map_err(|_| {
                    PipelineConfigurationError::Invalid("the fixed Parallel scope is not admitted")
                })?,
            None => Arc::clone(&binding.authority),
        };
        let bound = Arc::new(ParallelCompilerBinding {
            root_session: binding.root_session.clone(),
            ..ParallelCompilerBinding::bound(
                authority,
                binding.deadline,
                Arc::clone(&binding.cancellation),
                binding.decisions.clone(),
            )
        });
        let parent: Arc<dyn Checkpointer> = bound.occurrence.clone();
        runtimes.parallel = Some(bound);
        runtimes.parallel_authority_bound = true;
        Ok((parent, runtimes))
    }

    pub(super) fn bind_parallel_node(
        &self,
        mut builder: PipelineGraphBuilder,
        definition: &ParallelNodeDefinition,
        runtimes: &PipelineNodeRuntimes,
        _: &Arc<dyn Checkpointer>,
    ) -> Result<PipelineGraphBuilder, PipelineConfigurationError> {
        let binding = runtimes
            .parallel
            .as_ref()
            .filter(|_| runtimes.parallel_authority_bound)
            .ok_or(PipelineConfigurationError::Unsupported(
                "the fixed Parallel authority was not composed",
            ))?;
        let resolver = runtimes.application.clone();
        let owned = definition
            .branches()
            .iter()
            .map(|branch| {
                self.nodes
                    .iter()
                    .find_map(|node| match node {
                        PipelineNodeDefinition::Application(node) if node.id() == branch.node() => {
                            Some((branch.id().to_owned(), OwnedBranch::Agent(node.clone())))
                        }
                        PipelineNodeDefinition::Llm(node) if node.id() == branch.node() => {
                            Some((branch.id().to_owned(), OwnedBranch::Llm(node.clone())))
                        }
                        _ => None,
                    })
                    .ok_or(PipelineConfigurationError::Invalid(
                        "the fixed Parallel owned node is missing",
                    ))
            })
            .collect::<Result<BTreeMap<_, _>, _>>()?;
        reject_nested_parallel(&owned, runtimes)?;
        if resolver.is_none()
            && owned
                .values()
                .any(|branch| matches!(branch, OwnedBranch::Agent(_)))
        {
            return Err(PipelineConfigurationError::Unsupported(
                "the fixed Parallel Agent resolver is unavailable",
            ));
        }
        if owned
            .values()
            .any(|branch| matches!(branch, OwnedBranch::Llm(_)))
            && runtimes
                .llm
                .as_ref()
                .is_none_or(|factory| factory.for_fanout_branch().is_none())
        {
            return Err(PipelineConfigurationError::Unsupported(
                "the fixed Parallel LLM branch runtime is not bound to this compiler",
            ));
        }
        // The member tier shows the participant an Agent branch selects, and
        // the node id of an LLM branch (contract §3.3).
        let member_names = definition
            .branches()
            .iter()
            .map(|branch| match owned.get(branch.id()) {
                Some(OwnedBranch::Agent(node)) => node.selection().alias().to_owned(),
                Some(OwnedBranch::Llm(node)) => node.id().to_owned(),
                None => branch.node().to_owned(),
            })
            .collect();
        let factory = Arc::new(CompilerParallelBranches {
            owned,
            state_types: self.state.clone(),
            resolver,
            llm: runtimes.llm.clone(),
            events: runtimes.events.clone(),
            root_session: binding.root_session.clone(),
        });
        let runtime = Arc::new(AdkParallelBranchRuntime::new(
            binding.authority.clone(),
            factory,
            Arc::clone(&binding.occurrence),
        ));
        let mut node = DurableParallelNode::new(definition.clone(), runtime);
        if let Some(decisions) = &binding.decisions {
            node = node.with_decisions(Arc::clone(decisions));
        }
        builder = builder.node(
            node.with_deadline(binding.deadline)
                .with_cancellation(Arc::clone(&binding.cancellation))
                .with_node_events(runtimes.events.clone())
                .with_member_names(member_names)
                .with_output_reducer(self.state_reducers.get(definition.output_key()).copied()),
        );
        if let Some(target) = definition.transition() {
            builder = builder.edge(definition.id(), if target == "END" { END } else { target });
        }
        Ok(builder)
    }
}

fn reject_nested_parallel(
    owned: &BTreeMap<String, OwnedBranch>,
    runtimes: &PipelineNodeRuntimes,
) -> Result<(), PipelineConfigurationError> {
    // Only an Agent branch runs a saved composition; an LLM branch has none.
    let owned = owned
        .iter()
        .filter_map(|(key, branch)| match branch {
            OwnedBranch::Agent(node) => Some((key.clone(), node)),
            OwnedBranch::Llm(_) => None,
        })
        .collect::<BTreeMap<_, _>>();
    if let Some(definitions) = runtimes.composition_definitions() {
        for node in owned.values() {
            let path = if runtimes.application_scope_path.is_empty() {
                node.id().to_owned()
            } else {
                format!("{}/{}", runtimes.application_scope_path, node.id())
            };
            let descendants = format!("{path}/");
            // Code inside a fan-out child needs the Wave 3 Code-slot and
            // receipt work; refuse it by name until then, as Map does.
            if definitions.iter().any(|(key, definition)| {
                (key == &path || key.starts_with(&descendants)) && definition.has_code_nodes()
            }) {
                return Err(PipelineConfigurationError::FanoutChildUnsupported {
                    node: node.id().to_owned(),
                    refusal: FanoutChildRefusal::ContainsCode,
                });
            }
        }
        for node in owned.values() {
            let path = if runtimes.application_scope_path.is_empty() {
                node.id().to_owned()
            } else {
                format!("{}/{}", runtimes.application_scope_path, node.id())
            };
            let descendants = format!("{path}/");
            if definitions.iter().any(|(key, definition)| {
                (key == &path || key.starts_with(&descendants)) && definition.has_parallel_nodes()
            }) {
                return Err(PipelineConfigurationError::FanoutChildUnsupported {
                    node: node.id().to_owned(),
                    refusal: FanoutChildRefusal::ContainsParallel,
                });
            }
        }
    }
    Ok(())
}

/// FU-249: a Parallel binds its checkpoint authority to the started root only,
/// so a saved child pipeline (beneath an Agent node, a Parallel branch's
/// included) that holds one is refused by the path of its Parallel node.
pub(crate) fn refuse_child_parallel(
    runtimes: &PipelineNodeRuntimes,
) -> Result<(), PipelineConfigurationError> {
    let Some(definitions) = runtimes.composition_definitions() else {
        return Ok(());
    };
    for (path, definition) in definitions {
        if let Some(parallel) = definition.first_parallel_node() {
            return Err(PipelineConfigurationError::FanoutChildUnsupported {
                node: format!("{path}/{parallel}"),
                refusal: FanoutChildRefusal::ContainsParallel,
            });
        }
    }
    Ok(())
}

pub(super) fn validate_parallel_ownership(
    nodes: &[PipelineNodeDefinition],
    entry: &str,
    before: &[String],
    after: &[String],
) -> Result<BTreeSet<String>, PipelineConfigurationError> {
    let mut owned = BTreeSet::new();
    for parallel in nodes.iter().filter_map(|node| match node {
        PipelineNodeDefinition::Parallel(node) => Some(node),
        _ => None,
    }) {
        for branch in parallel.branches() {
            let (id, transition) = match nodes.iter().find(|node| node.id() == branch.node()) {
                Some(PipelineNodeDefinition::Application(node)) => (node.id(), node.transition()),
                // An inline LLM node runs as a branch too (T-2A D1).
                Some(PipelineNodeDefinition::Llm(node)) => (node.id(), node.transition()),
                // Other kinds run as branches once Wave 3 admits them one by one.
                Some(other) => {
                    return Err(PipelineConfigurationError::FanoutChildUnsupported {
                        node: other.id().to_owned(),
                        refusal: FanoutChildRefusal::Kind,
                    });
                }
                None => {
                    return Err(PipelineConfigurationError::Invalid(
                        "fixed Parallel branches must own declared Agent nodes",
                    ));
                }
            };
            if transition.is_some()
                || id == entry
                || !owned.insert(id.to_owned())
                || before.iter().chain(after).any(|other| other == id)
            {
                return Err(PipelineConfigurationError::Invalid(
                    "a fixed Parallel owned Agent has conflicting topology",
                ));
            }
        }
    }
    if nodes.iter().any(|node| {
        node.route_targets()
            .iter()
            .any(|target| owned.contains(*target))
    }) {
        return Err(PipelineConfigurationError::Invalid(
            "a parent route cannot target a fixed Parallel owned Agent",
        ));
    }
    Ok(owned)
}

/// The node a fixed Parallel branch owns: a saved Agent or Pipeline, or an
/// inline LLM node.
#[derive(Clone)]
enum OwnedBranch {
    Agent(ApplicationNodeDefinition),
    Llm(LlmNodeDefinition),
}

impl OwnedBranch {
    fn id(&self) -> &str {
        match self {
            Self::Agent(node) => node.id(),
            Self::Llm(node) => node.id(),
        }
    }

    fn output_keys(&self) -> &[String] {
        match self {
            Self::Agent(node) => node.output_keys(),
            Self::Llm(node) => node.output_keys(),
        }
    }
}

struct CompilerParallelBranches {
    owned: BTreeMap<String, OwnedBranch>,
    state_types: BTreeMap<String, String>,
    /// Present whenever an Agent branch exists (checked at compile time).
    resolver: Option<Arc<dyn PipelineApplicationResolver>>,
    llm: Option<Arc<dyn PipelineLlmAgentFactory>>,
    events: Option<PipelineNodeEventSender>,
    root_session: Option<Arc<dyn RootSessionSource>>,
}

impl CompilerParallelBranches {
    /// FU-246: a saved Agent member's own pause, decided. The ledger decision is
    /// bound to the confirmation the member forwarded into the root session
    /// (the same proof a browser continuation of a nested Agent uses), installed
    /// in the member scope's nested decision registry, and the branch re-enters
    /// its Agent node, which resumes the Agent with exactly that decision.
    async fn resume_agent_member(
        &self,
        definition: &crate::agents::graph::application::ApplicationNodeDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        let refused = |reason: &str| GraphError::NodeExecutionFailed {
            node: "parallel".to_owned(),
            message: format!("graph.parallel.decision_refused: {reason}"),
        };
        let [decision] = decisions else {
            return Err(refused(
                "an Agent member's pause takes exactly one decision",
            ));
        };
        let proven = super::super::parallel_cards::child_card(pause)?;
        if decision.interrupt_id != proven.card.interrupt_id
            || decision.tool_call_id != proven.card.tool_call_id
            || !proven.kind.accepts(&decision.action)
        {
            return Err(refused(
                "the decision does not answer the member's current pause",
            ));
        }
        let participant = self
            .resolver()?
            .resolve_node(
                definition.id(),
                definition.selection(),
                Arc::clone(&pause.checkpointer),
            )
            .map_err(|_| refused("the member's Agent is unavailable"))?;
        let crate::agents::graph::application::ResolvedApplicationParticipant::ScopedAgent {
            scope,
            ..
        } = &participant
        else {
            return Err(refused("the member's Agent is not a scoped Agent"));
        };
        let session = self
            .root_session
            .as_ref()
            .ok_or_else(|| refused("the run's root session is unavailable"))?
            .root_session()
            .await?;
        let value = Some(decision.value.as_str()).filter(|value| !value.is_empty());
        let set = crate::agents::direct_hitl::DirectHitlDecisionSet::fanout_member(
            &decision.interrupt_id,
            &decision.tool_call_id,
            proven.kind == super::super::parallel_cards::FanoutCardKind::AskUser,
            &decision.action,
            value,
        )
        .map_err(|error| refused(error.code().as_str()))?;
        let crate::agents::direct_hitl::ResolvedDirectHitlStart::Nested(resolved) = set
            .resolve(session.as_ref())
            .map_err(|error| refused(error.code().as_str()))?
        else {
            return Err(refused("the decision is not a nested Agent decision"));
        };
        let events = session.events().all();
        scope
            .applications
            .install_scope_resume(&events, resolved)
            .await
            .map_err(|_| refused("the member's nested decision could not be installed"))?;
        Ok(crate::agents::graph::resume::PipelineResume::empty().into_state())
    }

    fn resolver(&self) -> Result<&Arc<dyn PipelineApplicationResolver>, GraphError> {
        self.resolver.as_ref().ok_or_else(|| {
            GraphError::InvalidGraph("the fixed Parallel Agent resolver is unavailable".to_owned())
        })
    }

    /// An inline LLM branch: the owned LLM node alone, with its input frozen
    /// with the branch and a factory that keeps its events, failures and D2
    /// recovery inside the branch.
    fn compile_llm_branch(
        &self,
        definition: &LlmNodeDefinition,
        execution: &ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let factory = self
            .llm
            .as_ref()
            .and_then(|factory| factory.for_fanout_branch())
            .ok_or_else(|| {
                GraphError::InvalidGraph(
                    "the fixed Parallel LLM branch runtime is unavailable".to_owned(),
                )
            })?;
        let checkpointer = execution.checkpointer();
        let node = LlmNode::new(definition.clone(), self.state_types.clone(), factory)
            .with_parallel_inputs();
        let mut channels = BTreeSet::from([
            "input".to_owned(),
            "messages".to_owned(),
            "session_id".to_owned(),
            LLM_TOOL_RESUME_STATE_KEY.to_owned(),
            PARALLEL_LLM_INPUTS_STATE_KEY.to_owned(),
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
        ]);
        channels.extend(definition.input_keys().iter().cloned());
        channels.extend(definition.output_keys().iter().cloned());
        let mut schema = StateSchema::new();
        for channel in channels {
            schema
                .channels
                .insert(channel.clone(), Channel::new(&channel));
        }
        schema
            .channels
            .insert("messages".to_owned(), Channel::list("messages"));
        StateGraph::new(schema)
            .add_node(execution.wrap_node(BranchFrontierNode {
                node: Box::new(node),
                checkpointer: Arc::clone(&checkpointer),
            }))
            .add_edge(START, definition.id())
            .add_edge(definition.id(), END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(checkpointer)
                    .with_max_concurrency(1)
                    .with_strict_channels()
            })
    }

    fn definition(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
    ) -> Result<&OwnedBranch, GraphError> {
        self.owned
            .get(branch.id())
            .filter(|node| node.id() == branch.node())
            .ok_or_else(|| {
                GraphError::InvalidGraph(
                    "the fixed Parallel branch definition was not admitted".to_owned(),
                )
            })
    }
}

#[async_trait]
impl ParallelBranchGraphFactory for CompilerParallelBranches {
    fn validate_branch(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
    ) -> Result<(), GraphError> {
        self.owned_definition_digest(branch).map(|_| ())
    }

    fn owned_definition_digest(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        let declared = match self.definition(branch)? {
            OwnedBranch::Agent(node) => node,
            OwnedBranch::Llm(node) => {
                let mut digest = digest::Context::new(&digest::SHA256);
                digest.update(b"elitea.graph.parallel.owned-llm.v1\0");
                digest.update(&node.config_digest());
                let mut result = [0; 32];
                result.copy_from_slice(digest.finish().as_ref());
                return Ok(result);
            }
        };
        let participant = self
            .resolver()?
            .fixed_parallel_definition_digest(declared.id())
            .map_err(|_| {
                GraphError::InvalidGraph(
                    "the fixed Parallel saved participant fingerprint is unproved".to_owned(),
                )
            })?;
        if participant == [0; 32] {
            return Err(GraphError::InvalidGraph(
                "the fixed Parallel saved participant fingerprint is missing".to_owned(),
            ));
        }
        let mut digest = digest::Context::new(&digest::SHA256);
        digest.update(b"elitea.graph.parallel.owned-agent.v1\0");
        digest.update(&declared.config_digest());
        digest.update(&participant);
        let mut result = [0; 32];
        result.copy_from_slice(digest.finish().as_ref());
        Ok(result)
    }

    fn project_input(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError> {
        match self.definition(branch)? {
            OwnedBranch::Agent(node) => node.freeze_parallel_input(parent),
            OwnedBranch::Llm(node) => node.freeze_parallel_input(parent),
        }
    }

    fn compile_branch(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let definition = match self.definition(branch)? {
            OwnedBranch::Agent(node) => node.clone(),
            OwnedBranch::Llm(node) => return self.compile_llm_branch(node, &execution),
        };
        let mut failure_events = None;
        let resolver = match self.events.as_ref() {
            Some(events) => {
                let parent = super::super::node_events::PipelineNodeEventScope::from_state(
                    execution.parent_event_scope_value(),
                )
                .map_err(|_| {
                    GraphError::InvalidGraph(
                        "the fixed Parallel parent event scope is invalid".to_owned(),
                    )
                })?;
                let events = events
                    .for_parallel_branch(&execution, parent)
                    .map_err(|_| {
                        GraphError::InvalidGraph(
                            "the fixed Parallel event branch is unproved".to_owned(),
                        )
                    })?;
                // A branch's typed refusal reaches the run's terminal (RECOVERY_REFUSED).
                failure_events = Some(events.clone());
                self.resolver()?.for_parallel_events(events).map_err(|_| {
                    GraphError::InvalidGraph(
                        "the fixed Parallel event resolver is unavailable".to_owned(),
                    )
                })?
            }
            None => Arc::clone(self.resolver()?),
        };
        // A continued authored `after` pause re-arms only its proven checkpoint.
        let checkpointer = if execution.static_after_checkpoints().is_empty() {
            execution.checkpointer()
        } else {
            Arc::new(super::super::static_pause::StaticResumeCheckpointer::new(
                execution.checkpointer(),
                execution.static_after_checkpoints().clone(),
            ))
        };
        let frontier = Arc::clone(&checkpointer);
        let node = ApplicationNode::new(
            definition.clone(),
            self.state_types.clone(),
            resolver.as_ref(),
            checkpointer,
        )
        .map_err(|_| {
            GraphError::InvalidGraph(
                "the fixed Parallel Agent participant is unavailable".to_owned(),
            )
        })?
        .with_parallel_inputs()
        .with_failure_events(failure_events);
        let mut channels = BTreeSet::from([
            "input".to_owned(),
            "messages".to_owned(),
            "session_id".to_owned(),
            "_pipeline_blocked".to_owned(),
            APPLICATION_TASK_STATE_KEY.to_owned(),
            APPLICATION_MESSAGES_STATE_KEY.to_owned(),
            APPLICATION_RESULT_STATE_KEY.to_owned(),
            super::super::application::PARALLEL_AGENT_INPUTS_STATE_KEY.to_owned(),
            HITL_RESUME_STATE_KEY.to_owned(),
            DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
            LLM_TOOL_RESUME_STATE_KEY.to_owned(),
            "hitl_decisions".to_owned(),
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
            super::super::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY.to_owned(),
            super::super::static_pause::STATIC_TEXT_RESUME_STATE_KEY.to_owned(),
        ]);
        channels.extend(definition.input_keys().iter().cloned());
        channels.extend(definition.output_keys().iter().cloned());
        channels.extend(definition.variable_channels());
        let mut schema = StateSchema::new();
        for channel in channels {
            schema
                .channels
                .insert(channel.clone(), Channel::new(&channel));
        }
        schema
            .channels
            .insert("messages".to_owned(), Channel::list("messages"));
        StateGraph::new(schema)
            .add_node(execution.wrap_node(BranchFrontierNode {
                node: Box::new(node),
                checkpointer: frontier,
            }))
            .add_edge(START, definition.id())
            .add_edge(definition.id(), END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(execution.checkpointer())
                    .with_max_concurrency(1)
                    .with_strict_channels()
            })
    }

    fn project_result(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        state: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        crate::agents::graph::parallel::validate_state(state)?;
        if state
            .get("_pipeline_blocked")
            .and_then(serde_json::Value::as_bool)
            == Some(true)
        {
            return Ok(ParallelBranchTerminal::Blocked);
        }
        let mut outputs = serde_json::Map::new();
        for key in self
            .definition(branch)?
            .output_keys()
            .iter()
            .filter(|key| key.as_str() != "messages")
        {
            let value = state.get(key).ok_or_else(|| {
                GraphError::SerializationError(
                    "a fixed Parallel declared output is missing".to_owned(),
                )
            })?;
            if let Some(kind) = self.state_types.get(key)
                && !state_value_matches(kind, value)
            {
                return Err(GraphError::SerializationError(
                    "a fixed Parallel declared output has the wrong type".to_owned(),
                ));
            }
            outputs.insert(key.clone(), value.clone());
        }
        Ok(ParallelBranchTerminal::Completed(outputs))
    }

    fn pause_cards(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        self.definition(branch)?;
        Ok(vec![super::super::parallel_cards::child_card(pause)?.card])
    }

    async fn resume_input(
        &self,
        branch: &crate::agents::graph::ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        let OwnedBranch::Agent(definition) = self.definition(branch)? else {
            // An LLM branch's pause is its own node's card: no authored catalog.
            return super::super::parallel_cards::child_resume(pause, None, decisions).await;
        };
        if super::super::parallel_cards::is_application_member_pause(pause) {
            return self.resume_agent_member(definition, pause, decisions).await;
        }
        // The child participant's own authored pause catalog proves a static
        // pause; it is resolved from frozen saved versions, not from state.
        // By node, as the branch's own run resolves it: a fixed Parallel's
        // participants are scoped per owned node, not by alias.
        let participant = self
            .resolver()?
            .resolve_node(
                definition.id(),
                definition.selection(),
                Arc::clone(&pause.checkpointer),
            )
            .map_err(|_| {
                GraphError::InvalidGraph(
                    "the fixed Parallel Agent participant is unavailable".to_owned(),
                )
            })?;
        let catalog = match &participant {
            crate::agents::graph::application::ResolvedApplicationParticipant::Pipeline {
                static_pauses,
                ..
            } => Some(static_pauses),
            _ => None,
        };
        super::super::parallel_cards::child_resume(pause, catalog, decisions).await
    }
}

/// A scoped Agent call proves itself against its own thread's frontier (the
/// graph-call receipt, `prepare_graph_call`). A fresh branch child has saved
/// nothing yet, so save the frontier the call is admitted at before the node
/// runs, as the root graph does for its entry (`with_initial_checkpoint`).
struct BranchFrontierNode {
    node: Box<dyn Node>,
    checkpointer: Arc<dyn Checkpointer>,
}

#[async_trait]
impl Node for BranchFrontierNode {
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

    fn validate_against(&self, parent: &StateSchema) -> Result<(), GraphError> {
        self.node.validate_against(parent)
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let thread = &context.config.thread_id;
        if self.checkpointer.load(thread).await?.is_none() {
            let frontier = Checkpoint::new(
                thread,
                context.state.clone(),
                context.step,
                vec![self.node.name().to_owned()],
            );
            self.checkpointer.save(&frontier).await?;
        }
        self.node.execute(context).await
    }
}

#[cfg(test)]
#[path = "parallel_compiler_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "parallel_cards_tests.rs"]
mod card_tests;
