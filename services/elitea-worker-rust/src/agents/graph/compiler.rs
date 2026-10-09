//! Bounded stored-pipeline document admission and ADK graph compilation.
//!
//! Node families are admitted only after their bounded business contract is
//! implemented. Unsupported Python branches still fail before graph or
//! credential construction.

#![allow(dead_code)] // Production pipeline assembly remains capability-gated.

#[path = "node_recovery_definition.rs"]
mod recovery_definition;

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::fmt;
use std::sync::Arc;

use super::code::CodeNodeDefinition;
use super::code_runtime::{CodeNode, CodeSandboxRuntime};
use adk_rust::graph::{
    Channel, Checkpoint, Checkpointer, CompiledGraph, END, Edge, EdgeTarget, GraphAgent,
    GraphAgentBuilder, GraphError, Node, NodeContext, NodeOutput, Reducer, START, State,
    StateGraph, StateSchema,
};
use adk_rust::{Event, InvocationContext, Part};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde::de::{Deserializer, SeqAccess, Visitor};
use serde_json::json;
use thiserror::Error;

use super::aggregate::{AggregateNode, AggregateNodeDefinition};
use super::application::{
    APPLICATION_MESSAGES_STATE_KEY, APPLICATION_RESULT_STATE_KEY, APPLICATION_TASK_STATE_KEY,
    ApplicationConfigurationError, ApplicationNode, ApplicationNodeDefinition,
    PipelineApplicationResolver, PipelineApplicationSelection,
};
use super::decision::{DecisionConfigurationError, DecisionNode, DecisionNodeDefinition};
use super::direct_tool::{
    DIRECT_TOOL_RESUME_STATE_KEY, DirectToolConfigurationError, DirectToolInputMapping,
    DirectToolNode, DirectToolNodeDefinition, DirectToolSelection, PipelineDirectToolResolver,
};
use super::hitl::{HITL_RESUME_STATE_KEY, HitlConfigurationError, HitlNode, HitlNodeDefinition};
use super::llm::{
    LLM_TOOL_RESUME_STATE_KEY, LlmConfigurationError, LlmNode, LlmNodeDefinition,
    LlmToolkitSelection, PipelineLlmAgentFactory,
};
use super::node_events::{PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventSender};
use super::pipeline_result::{
    PIPELINE_RESULT_TRACE_STATE_KEY, RenderedResult, ResultTrace, ResultTraceNode,
    ResultTraceOutputs, render_state_value, render_traced_keys,
};
use super::printer::{
    PrinterConfigurationError, PrinterInputMapping, PrinterNode, PrinterNodeDefinition,
    PrinterPauseCatalog, PrinterResetNode,
};
use super::resume::PipelineResume;
use super::router::{RouterConfigurationError, RouterNode, RouterNodeDefinition};
use super::split_out::{SplitOutNode, SplitOutNodeDefinition};
use super::state_modifier::{
    StateModifierConfigurationError, StateModifierNode, StateModifierNodeDefinition,
};
use super::state_reducers::{ReducerGuard, StateReducer, reducers_digest};
use super::static_pause::{StaticPauseCatalog, StaticResumeCheckpointer};
use super::yaml::{
    MAX_NODE_ID_BYTES, ParallelConfigurationError, ParallelNodeDefinition, valid_graph_id,
    valid_output_key,
};
use elitea_agent_runtime::bounded_yaml::{self, BoundedYamlError};

#[path = "map_compiler.rs"]
mod map_compiler;
#[path = "parallel_compiler.rs"]
mod parallel_compiler;
use super::map_yaml::MapNodeDefinition;
use super::{pipeline_completed_event, pipeline_result_event};
pub(crate) use map_compiler::MapCompilerBinding;
use map_compiler::validate_map_ownership;
use parallel_compiler::validate_parallel_ownership;
#[allow(unused_imports)] // Public continuation binding is staged behind the Parallel gate.
pub(crate) use parallel_compiler::{ParallelBranchContinuation, ParallelCompilerBinding};

pub(crate) const MAX_PIPELINE_YAML_BYTES: usize = 512 * 1024;
pub(crate) use elitea_agent_runtime::graph::PIPELINE_YAML_BUDGET;
// An alias-free document within the source bound stays inside the expansion budget
// (YAML escapes grow scalar text at most 1.5x).
const _: () = assert!(PIPELINE_YAML_BUDGET.scalar_bytes >= 2 * MAX_PIPELINE_YAML_BYTES);
const MAX_PIPELINE_NODES: usize = 128;
const MAX_PIPELINE_STATE_KEYS: usize = 256;
const MAX_STATIC_INTERRUPTS: usize = 128;
const PIPELINE_RECURSION_LIMIT: usize = 100;
const SUBGRAPH_RESULT_NODE: &str = "__elitea_subgraph_result_v1";
const SUBGRAPH_ENTRY_NODE: &str = "__elitea_subgraph_entry_v1";
const PIPELINE_DIGEST_DOMAIN: &[u8] = b"elitea.graph.pipeline.config.v1\0";

type ValidatedState = (
    BTreeMap<String, String>,
    BTreeMap<String, serde_json::Value>,
    Vec<String>,
    BTreeMap<String, StateReducer>,
);

const RUNTIME_STRING_CHANNELS: &[&str] = &[
    "input",
    "output",
    "result",
    "router_output",
    "elitea_response",
    "printer_output",
    "session_id",
];

const INTERNAL_RESULT_KEYS: &[&str] = &[
    "messages",
    "output",
    "input",
    "chat_history",
    "thread_id",
    "execution_finished",
    "context_info",
    "state_types",
    "hitl_decisions",
    "hitl_interrupt",
    "parallel_tasks",
    "parallel_parked",
    "parallel_dispatch",
    "dispatch_epoch",
    "elitea_response",
    "printer_output",
    "router_output",
    "_pipeline_blocked",
    "session_id",
    HITL_RESUME_STATE_KEY,
    DIRECT_TOOL_RESUME_STATE_KEY,
    LLM_TOOL_RESUME_STATE_KEY,
    super::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY,
    super::static_pause::STATIC_TEXT_RESUME_STATE_KEY,
    PIPELINE_RESULT_TRACE_STATE_KEY,
];

#[derive(Clone, Deserialize)]
#[serde(untagged)]
enum RawStateType {
    Name(String),
    Descriptor(RawStateTypeDescriptor),
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawStateTypeDescriptor {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default)]
    value: Option<serde_json::Value>,
    #[serde(default)]
    reducer: Option<String>,
}

impl RawStateType {
    fn name(&self) -> &str {
        match self {
            Self::Name(name) => name,
            Self::Descriptor(descriptor) => &descriptor.kind,
        }
    }

    fn configured_value(&self) -> Option<&serde_json::Value> {
        match self {
            Self::Name(_) => None,
            Self::Descriptor(descriptor) => descriptor.value.as_ref(),
        }
    }

    fn reducer(&self) -> Option<&str> {
        match self {
            Self::Name(_) => None,
            Self::Descriptor(descriptor) => descriptor.reducer.as_deref(),
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawPipelineDefinition {
    #[serde(default)]
    state: serde_yaml_ng::Mapping,
    entry_point: String,
    #[serde(deserialize_with = "deserialize_nodes")]
    nodes: Vec<serde_yaml_ng::Value>,
    #[serde(default, deserialize_with = "deserialize_static_interrupts")]
    interrupt_before: Vec<String>,
    #[serde(default, deserialize_with = "deserialize_static_interrupts")]
    interrupt_after: Vec<String>,
}

fn deserialize_nodes<'de, D>(deserializer: D) -> Result<Vec<serde_yaml_ng::Value>, D::Error>
where
    D: Deserializer<'de>,
{
    struct NodesVisitor;

    impl<'de> Visitor<'de> for NodesVisitor {
        type Value = Vec<serde_yaml_ng::Value>;

        fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
            formatter.write_str("between 1 and 128 pipeline node mappings")
        }

        fn visit_seq<A>(self, mut sequence: A) -> Result<Self::Value, A::Error>
        where
            A: SeqAccess<'de>,
        {
            let mut nodes = Vec::new();
            while let Some(node) = sequence.next_element()? {
                if nodes.len() == MAX_PIPELINE_NODES {
                    return Err(serde::de::Error::custom(
                        "the pipeline node count exceeds its resource bound",
                    ));
                }
                nodes.push(node);
            }
            Ok(nodes)
        }
    }

    deserializer.deserialize_seq(NodesVisitor)
}

fn deserialize_static_interrupts<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: Deserializer<'de>,
{
    struct InterruptsVisitor;

    impl<'de> Visitor<'de> for InterruptsVisitor {
        type Value = Vec<String>;

        fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
            formatter.write_str("at most 128 pipeline node identifiers")
        }

        fn visit_seq<A>(self, mut sequence: A) -> Result<Self::Value, A::Error>
        where
            A: SeqAccess<'de>,
        {
            let mut nodes = Vec::new();
            while let Some(node) = sequence.next_element()? {
                if nodes.len() == MAX_STATIC_INTERRUPTS {
                    return Err(serde::de::Error::custom(
                        "the static interrupt count exceeds its resource bound",
                    ));
                }
                nodes.push(node);
            }
            Ok(nodes)
        }
    }

    deserializer.deserialize_seq(InterruptsVisitor)
}

/// Validated initial stored-pipeline document.
///
/// The definition contains no execution authority and can be retained across
/// claim attempts. Its digest is safe to bind into session/checkpoint lineage.
#[derive(Clone)]
pub(crate) struct PipelineDefinition {
    recovery: recovery_definition::RecoveryCatalog,
    entry_point: String,
    state: BTreeMap<String, String>,
    state_defaults: BTreeMap<String, serde_json::Value>,
    state_declaration_order: Vec<String>,
    /// Typed channel reducers; absent channels overwrite.
    state_reducers: Arc<BTreeMap<String, StateReducer>>,
    nodes: Vec<PipelineNodeDefinition>,
    parallel_owned_nodes: BTreeSet<String>,
    map_owned_nodes: BTreeSet<String>,
    definition_digest: [u8; 32],
    interrupt_before: Vec<String>,
    interrupt_after: Vec<String>,
}

/// Invocation-owned dependencies for executable pipeline node families.
#[derive(Clone, Default)]
pub(crate) struct PipelineNodeRuntimes {
    events: Option<PipelineNodeEventSender>,
    llm: Option<Arc<dyn PipelineLlmAgentFactory>>,
    direct_tool: Option<Arc<dyn PipelineDirectToolResolver>>,
    application: Option<Arc<dyn PipelineApplicationResolver>>,
    code: Option<Arc<dyn CodeSandboxRuntime>>,
    node_recovery: Option<Arc<dyn super::node_recovery_runtime::NodeRecoveryFactory>>,
    saved_child_scope_provider: Option<
        Arc<dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider>,
    >,
    checkpoint_catalog:
        Option<Arc<crate::agents::pipeline::composition::PipelineCheckpointCatalog>>,
    composition_definitions: Option<Arc<BTreeMap<String, PipelineDefinition>>>,
    parallel: Option<Arc<ParallelCompilerBinding>>,
    parallel_authority_bound: bool,
    map: Option<Arc<MapCompilerBinding>>,
    map_authority_bound: bool,
    application_scope_path: String,
    application_scopes:
        Option<Arc<crate::agents::pipeline::scoped_applications::PipelineApplicationScopeRegistry>>,
}

impl PipelineNodeRuntimes {
    pub(crate) fn with_saved_child_scope_provider(
        mut self,
        provider: Arc<
            dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider,
        >,
    ) -> Self {
        self.saved_child_scope_provider = Some(provider);
        self
    }
    pub(crate) fn saved_child_scope_provider(
        &self,
    ) -> Option<
        &Arc<dyn crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider>,
    > {
        self.saved_child_scope_provider.as_ref()
    }

    pub(crate) fn with_application_scope_path(mut self, path: String) -> Self {
        self.application_scope_path = path;
        self
    }
    fn scoped_checkpointer(
        &self,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<Arc<dyn Checkpointer>, PipelineConfigurationError> {
        if self.parallel_authority_bound || self.map_authority_bound {
            return Ok(checkpointer);
        }
        match self.application_scopes() {
            Some(scopes) => scopes
                .wrap_checkpointer(&self.application_scope_path, checkpointer)
                .map_err(|_| {
                    PipelineConfigurationError::Invalid(
                        "the graph application scope was not admitted",
                    )
                }),
            None => Ok(checkpointer),
        }
    }

    pub(crate) fn with_application_scopes(
        mut self,
        scopes: Arc<crate::agents::pipeline::scoped_applications::PipelineApplicationScopeRegistry>,
    ) -> Self {
        self.application_scopes = Some(scopes);
        self
    }

    pub(crate) fn application_scopes(
        &self,
    ) -> Option<&crate::agents::pipeline::scoped_applications::PipelineApplicationScopeRegistry>
    {
        self.application_scopes.as_deref()
    }

    pub(crate) fn with_composition_definitions(
        mut self,
        definitions: BTreeMap<String, PipelineDefinition>,
    ) -> Self {
        self.composition_definitions = Some(Arc::new(definitions));
        self
    }

    pub(crate) fn checkpoint_catalog_arc(
        &self,
    ) -> Option<Arc<super::super::pipeline::composition::PipelineCheckpointCatalog>> {
        self.checkpoint_catalog.clone()
    }

    pub(crate) fn composition_definitions(&self) -> Option<&BTreeMap<String, PipelineDefinition>> {
        self.composition_definitions.as_deref()
    }

    pub(crate) fn with_checkpoint_catalog(
        mut self,
        catalog: crate::agents::pipeline::composition::PipelineCheckpointCatalog,
    ) -> Self {
        self.checkpoint_catalog = Some(Arc::new(catalog));
        self
    }

    pub(crate) fn checkpoint_catalog(
        &self,
    ) -> Option<&crate::agents::pipeline::composition::PipelineCheckpointCatalog> {
        self.checkpoint_catalog.as_deref()
    }

    pub(crate) fn with_node_recovery_authority(
        mut self,
        authority: Arc<dyn super::node_recovery_runtime::NodeRecoveryFactory>,
    ) -> Self {
        self.node_recovery = Some(authority);
        self
    }

    pub(in crate::agents) fn with_code(mut self, runtime: Arc<dyn CodeSandboxRuntime>) -> Self {
        self.code = Some(runtime);
        self
    }
    pub(crate) fn with_events(mut self, events: PipelineNodeEventSender) -> Self {
        self.events = Some(events);
        self
    }

    #[must_use]
    pub(crate) const fn new(
        llm: Option<Arc<dyn PipelineLlmAgentFactory>>,
        direct_tool: Option<Arc<dyn PipelineDirectToolResolver>>,
        application: Option<Arc<dyn PipelineApplicationResolver>>,
    ) -> Self {
        Self {
            llm,
            direct_tool,
            application,
            events: None,
            code: None,
            node_recovery: None,
            saved_child_scope_provider: None,
            checkpoint_catalog: None,
            composition_definitions: None,
            application_scopes: None,
            parallel: None,
            parallel_authority_bound: false,
            map: None,
            map_authority_bound: false,
            application_scope_path: String::new(),
        }
    }
}

/// The single registration point of every bound pipeline node.
///
/// `result_trace` maps each top-level node ID to its declared result outputs.
/// It is built once from the definition. A node found there is wrapped with
/// [`ResultTraceNode`]; runtime-owned helper nodes are bound unchanged.
struct PipelineGraphBuilder {
    target: PipelineGraphTarget,
    result_trace: BTreeMap<String, ResultTraceOutputs>,
    /// Present only when the definition declares a typed reducer; every bound
    /// node is then wrapped with [`ReducerGuard`].
    reducers: Option<Arc<BTreeMap<String, StateReducer>>>,
}

enum PipelineGraphTarget {
    Agent(Box<GraphAgentBuilder>),
    Subgraph {
        graph: StateGraph,
        terminal: &'static str,
    },
}

impl PipelineGraphBuilder {
    fn node<N>(self, node: N) -> Self
    where
        N: Node + 'static,
    {
        match self.reducers.clone() {
            Some(reducers) => self.trace(ReducerGuard::new(node, reducers)),
            None => self.trace(node),
        }
    }

    fn trace<N>(mut self, node: N) -> Self
    where
        N: Node + 'static,
    {
        match self.result_trace.remove(node.name()) {
            Some(outputs) => self.bind(ResultTraceNode::new(node, outputs)),
            None => self.bind(node),
        }
    }

    fn bind<N>(self, node: N) -> Self
    where
        N: Node + 'static,
    {
        let target = match self.target {
            PipelineGraphTarget::Agent(builder) => {
                PipelineGraphTarget::Agent(Box::new((*builder).node(node)))
            }
            PipelineGraphTarget::Subgraph { graph, terminal } => PipelineGraphTarget::Subgraph {
                graph: graph.add_node(TerminalRedirectNode::new(node, terminal)),
                terminal,
            },
        };
        Self {
            target,
            result_trace: self.result_trace,
            reducers: self.reducers,
        }
    }

    fn edge(self, source: &str, target: &str) -> Self {
        let target = match self.target {
            PipelineGraphTarget::Agent(builder) => {
                PipelineGraphTarget::Agent(Box::new((*builder).edge(source, target)))
            }
            PipelineGraphTarget::Subgraph { graph, terminal } => PipelineGraphTarget::Subgraph {
                graph: graph.add_edge(source, terminal_target(target, terminal)),
                terminal,
            },
        };
        Self {
            target,
            result_trace: self.result_trace,
            reducers: self.reducers,
        }
    }

    /// Every traced node must have been bound under its own ID; an unclaimed
    /// entry would silently drop that node's result.
    fn ensure_result_trace_bound(&self) -> Result<(), PipelineConfigurationError> {
        if self.result_trace.is_empty() {
            Ok(())
        } else {
            Err(PipelineConfigurationError::Invalid(
                "a pipeline node was not bound to its result trace",
            ))
        }
    }

    fn into_agent(self) -> Result<GraphAgentBuilder, PipelineConfigurationError> {
        self.ensure_result_trace_bound()?;
        match self.target {
            PipelineGraphTarget::Agent(builder) => Ok(*builder),
            PipelineGraphTarget::Subgraph { .. } => Err(PipelineConfigurationError::Invalid(
                "an internal pipeline graph builder changed kind",
            )),
        }
    }

    fn into_subgraph(self) -> Result<StateGraph, PipelineConfigurationError> {
        self.ensure_result_trace_bound()?;
        match self.target {
            PipelineGraphTarget::Subgraph { graph, .. } => Ok(graph),
            PipelineGraphTarget::Agent(_) => Err(PipelineConfigurationError::Invalid(
                "an internal pipeline graph builder changed kind",
            )),
        }
    }
}

struct TerminalRedirectNode<N> {
    inner: N,
    terminal: &'static str,
}

impl<N> TerminalRedirectNode<N> {
    const fn new(inner: N, terminal: &'static str) -> Self {
        Self { inner, terminal }
    }
}

#[async_trait]
impl<N> Node for TerminalRedirectNode<N>
where
    N: Node,
{
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn validate_against(&self, parent: &StateSchema) -> Result<(), GraphError> {
        self.inner.validate_against(parent)
    }

    fn validate(&self) -> Result<(), GraphError> {
        self.inner.validate()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let mut output = self.inner.execute(context).await?;
        if let Some(targets) = output.goto.as_mut() {
            for target in targets {
                if target == END {
                    self.terminal.clone_into(target);
                }
            }
        }
        Ok(output)
    }
}

fn terminal_target<'a>(target: &'a str, terminal: &'a str) -> &'a str {
    if target == END { terminal } else { target }
}

/// Adds the optional single transition edge; an absent transition adds none.
fn bind_transition(
    builder: PipelineGraphBuilder,
    node_id: &str,
    transition: Option<&str>,
) -> PipelineGraphBuilder {
    match transition {
        Some("END") => builder.edge(node_id, END),
        Some(target) => builder.edge(node_id, target),
        None => builder,
    }
}

/// Keep transitions that meet at one node exclusive.
///
/// A stored pipeline runs one node at a time and a node has at most one
/// transition, so of several transitions into one node only the one on the
/// taken branch arrives. `StateGraph::compile` turns a node reached by two or
/// more direct edges into a wait-for-all join that would wait for the branches
/// not taken and end the run without it. Each such transition becomes a
/// single-route conditional edge, which ADK never joins. Parallel and Map nodes
/// join their own branches inside one node and are unaffected.
fn exclusive_transitions(mut graph: StateGraph) -> StateGraph {
    let mut arrivals = HashMap::<String, usize>::new();
    for edge in &graph.edges {
        match edge {
            Edge::Direct {
                target: EdgeTarget::Node(target),
                ..
            } => *arrivals.entry(target.clone()).or_default() += 1,
            Edge::Entry { targets } => {
                for target in targets {
                    *arrivals.entry(target.clone()).or_default() += 1;
                }
            }
            Edge::Direct { .. } | Edge::Conditional { .. } => {}
        }
    }
    for edge in &mut graph.edges {
        if let Edge::Direct {
            source,
            target: EdgeTarget::Node(target),
        } = edge
            && arrivals
                .get(target.as_str())
                .is_some_and(|count| *count > 1)
        {
            let target = std::mem::take(target);
            let route = target.clone();
            *edge = Edge::Conditional {
                source: std::mem::take(source),
                router: Arc::new(move |_: &State| route.clone()),
                targets: HashMap::from([(target.clone(), EdgeTarget::Node(target))]),
            };
        }
    }
    graph
}

#[derive(Clone)]
enum PipelineNodeDefinition {
    Code(CodeNodeDefinition),
    Application(ApplicationNodeDefinition),
    Parallel(ParallelNodeDefinition),
    Map(MapNodeDefinition),
    Decision(DecisionNodeDefinition),
    DirectTool(DirectToolNodeDefinition),
    Hitl(HitlNodeDefinition),
    Llm(LlmNodeDefinition),
    Printer(PrinterNodeDefinition),
    Router(RouterNodeDefinition),
    StateModifier(StateModifierNodeDefinition),
    SplitOut(SplitOutNodeDefinition),
    Aggregate(AggregateNodeDefinition),
}

impl PipelineNodeDefinition {
    fn id(&self) -> &str {
        match self {
            Self::Code(node) => node.id(),
            Self::Application(node) => node.id(),
            Self::Parallel(node) => node.id(),
            Self::Map(node) => node.id(),
            Self::Decision(node) => node.id(),
            Self::DirectTool(node) => node.id(),
            Self::Hitl(node) => node.id(),
            Self::Llm(node) => node.id(),
            Self::Printer(node) => node.id(),
            Self::Router(node) => node.id(),
            Self::StateModifier(node) => node.id(),
            Self::SplitOut(node) => node.id(),
            Self::Aggregate(node) => node.id(),
        }
    }

    fn input_keys(&self) -> &[String] {
        match self {
            Self::Code(node) => node.input_keys(),
            Self::Application(node) => node.input_keys(),
            Self::Parallel(_) | Self::Map(_) | Self::Printer(_) => &[],
            Self::Decision(node) => node.input_keys(),
            Self::DirectTool(node) => node.input_keys(),
            Self::Hitl(node) => node.input_keys(),
            Self::Llm(node) => node.input_keys(),
            Self::Router(node) => node.input_keys(),
            Self::StateModifier(node) => node.input_keys(),
            Self::SplitOut(node) => node.input_keys(),
            Self::Aggregate(node) => node.input_keys(),
        }
    }

    fn output_keys(&self) -> &[String] {
        match self {
            Self::Code(node) => node.output_keys(),
            Self::Application(node) => node.output_keys(),
            Self::Parallel(node) => node.output_keys(),
            Self::Map(node) => node.output_keys(),
            Self::DirectTool(node) => node.output_keys(),
            Self::Decision(_) | Self::Hitl(_) | Self::Printer(_) | Self::Router(_) => &[],
            Self::Llm(node) => node.output_keys(),
            Self::StateModifier(node) => node.output_keys(),
            Self::SplitOut(node) => node.output_keys(),
            Self::Aggregate(node) => node.output_keys(),
        }
    }

    fn cleaned_keys(&self) -> &[String] {
        match self {
            Self::Code(_)
            | Self::Application(_)
            | Self::Parallel(_)
            | Self::Map(_)
            | Self::Decision(_)
            | Self::DirectTool(_)
            | Self::Hitl(_)
            | Self::Llm(_)
            | Self::Printer(_)
            | Self::Router(_)
            | Self::SplitOut(_)
            | Self::Aggregate(_) => &[],
            Self::StateModifier(node) => node.variables_to_clean(),
        }
    }

    fn edit_state_key(&self) -> Option<&str> {
        match self {
            Self::Hitl(node) => node.edit_state_key(),
            Self::Code(_)
            | Self::Application(_)
            | Self::Parallel(_)
            | Self::Map(_)
            | Self::Decision(_)
            | Self::DirectTool(_)
            | Self::Llm(_)
            | Self::Printer(_)
            | Self::Router(_)
            | Self::StateModifier(_)
            | Self::SplitOut(_)
            | Self::Aggregate(_) => None,
        }
    }

    fn route_targets(&self) -> Vec<&str> {
        match self {
            Self::Code(node) => node.transition().into_iter().collect(),
            Self::Application(node) => node.transition().into_iter().collect(),
            Self::Parallel(node) => node.transition().into_iter().collect(),
            Self::Map(node) => node.transition().into_iter().collect(),
            Self::Decision(node) => node.route_targets().collect(),
            Self::DirectTool(node) => node.transition().into_iter().collect(),
            Self::Hitl(node) => node.route_targets().collect(),
            Self::Llm(node) => node.transition().into_iter().collect(),
            Self::Printer(node) => [node.transition()].into_iter().collect(),
            Self::Router(node) => node.route_targets().collect(),
            Self::StateModifier(node) => node.transition().into_iter().collect(),
            Self::SplitOut(node) => node.transition().into_iter().collect(),
            Self::Aggregate(node) => node.transition().into_iter().collect(),
        }
    }

    fn config_digest(&self) -> [u8; 32] {
        match self {
            Self::Code(node) => node.validated_digest(),
            Self::Application(node) => node.config_digest(),
            Self::Parallel(node) => node.config_digest(),
            Self::Map(node) => node.config_digest(),
            Self::Decision(node) => node.config_digest(),
            Self::DirectTool(node) => node.config_digest(),
            Self::Hitl(node) => node.config_digest(),
            Self::Llm(node) => node.config_digest(),
            Self::Printer(node) => node.config_digest(),
            Self::Router(node) => node.config_digest(),
            Self::StateModifier(node) => node.config_digest(),
            Self::SplitOut(node) => node.config_digest(),
            Self::Aggregate(node) => node.config_digest(),
        }
    }
}

impl PipelineDefinition {
    #[cfg(test)]
    pub(super) fn original_code_definition_fixture(
        &self,
        node_id: &str,
    ) -> Option<CodeNodeDefinition> {
        self.nodes.iter().find_map(|node| match node {
            PipelineNodeDefinition::Code(code) if code.id() == node_id => Some(code.clone()),
            _ => None,
        })
    }
    pub(crate) fn declared_variable_types(&self) -> BTreeMap<String, String> {
        self.state
            .iter()
            .filter(|(key, _)| {
                !reserved_user_state_key(key) && !matches!(key.as_str(), "input" | "messages")
            })
            .map(|(key, kind)| (key.clone(), kind.clone()))
            .collect()
    }

    /// Parse and validate a complete frozen pipeline YAML document.
    pub(crate) fn from_yaml(yaml: &str) -> Result<Self, PipelineConfigurationError> {
        if yaml.is_empty() {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline YAML is empty",
            ));
        }
        if yaml.len() > MAX_PIPELINE_YAML_BYTES {
            return Err(PipelineConfigurationError::LimitExceeded(
                PipelineLimit::YamlBytes,
            ));
        }
        let mut document =
            bounded_yaml::from_str::<serde_yaml_ng::Value>(yaml, PIPELINE_YAML_BUDGET).map_err(
                |error| match error {
                    BoundedYamlError::BudgetExceeded(limit) => {
                        tracing::warn!(
                            event = "pipeline_yaml_budget_exceeded",
                            limit = limit.as_str(),
                            "refused a stored pipeline whose YAML exceeds its expansion budget"
                        );
                        PipelineConfigurationError::LimitExceeded(PipelineLimit::YamlExpansion)
                    }
                    BoundedYamlError::Malformed(source) => {
                        PipelineConfigurationError::MalformedYaml { source }
                    }
                },
            )?;
        // The typed parse below also bounds the node list, but it can only report
        // a generic parse failure. Count on the document so the limit is named.
        if document
            .get("nodes")
            .and_then(serde_yaml_ng::Value::as_sequence)
            .is_some_and(|nodes| nodes.len() > MAX_PIPELINE_NODES)
        {
            return Err(PipelineConfigurationError::LimitExceeded(
                PipelineLimit::NodeCount,
            ));
        }
        let normalized = normalize_graph_identifiers(&mut document)?;
        let raw = serde_yaml_ng::from_value::<RawPipelineDefinition>(document)
            .map_err(|source| PipelineConfigurationError::MalformedYaml { source })?;
        if normalized.legacy > 0 || normalized.numeric > 0 {
            tracing::warn!(
                event = "pipeline_legacy_identifier_normalized",
                normalized_identifier_count = normalized.legacy,
                numeric_identifier_count = normalized.numeric,
                "normalized legacy pipeline graph identifiers for runtime compatibility"
            );
        }
        let mut definition = Self::from_raw(raw)?;
        let mut yaml_pin = [0_u8; 32];
        yaml_pin.copy_from_slice(digest::digest(&digest::SHA256, yaml.as_bytes()).as_ref());
        for node in &mut definition.nodes {
            if let PipelineNodeDefinition::Code(code) = node {
                code.bind_debug_definition(definition.definition_digest, yaml_pin);
            }
        }
        Ok(definition)
    }

    fn from_raw(mut raw: RawPipelineDefinition) -> Result<Self, PipelineConfigurationError> {
        if raw.nodes.len() > MAX_PIPELINE_NODES {
            return Err(PipelineConfigurationError::LimitExceeded(
                PipelineLimit::NodeCount,
            ));
        }
        if raw.nodes.is_empty() {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline must contain between 1 and 128 nodes",
            ));
        }
        if !valid_graph_id(&raw.entry_point) {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline entry point is malformed",
            ));
        }
        let raw_recovery = recovery_definition::RawRecoveryCatalog::extract(&mut raw.nodes)?;
        let (state, state_defaults, state_declaration_order, state_reducers) =
            validate_state(raw.state, TYPED_REDUCERS_READY)?;
        let (nodes, node_ids) = parse_pipeline_nodes(raw.nodes, &state)?;
        if !node_ids.contains(&raw.entry_point) {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline entry point does not name a node",
            ));
        }
        for node in &nodes {
            for target in node.route_targets() {
                if target != "END" && !node_ids.contains(target) {
                    return Err(PipelineConfigurationError::Invalid(
                        "a pipeline route target does not name a node",
                    ));
                }
            }
        }
        for node in &nodes {
            if let PipelineNodeDefinition::Printer(printer) = node
                && node_ids.contains(&printer.reset_node_id())
            {
                return Err(PipelineConfigurationError::Invalid(
                    "a Printer reset node conflicts with a stored node identifier",
                ));
            }
        }
        let parallel_owned_nodes = validate_parallel_ownership(
            &nodes,
            &raw.entry_point,
            &raw.interrupt_before,
            &raw.interrupt_after,
        )?;
        let map_owned_nodes = validate_map_ownership(
            &nodes,
            &state,
            &raw.entry_point,
            &raw.interrupt_before,
            &raw.interrupt_after,
            &parallel_owned_nodes,
        )?;
        let recovery_owned = parallel_owned_nodes
            .union(&map_owned_nodes)
            .cloned()
            .collect();
        validate_overwrite_channels(&nodes, &state_reducers, &recovery_owned)?;
        let recovery = raw_recovery.admit(&nodes, &state, &raw.entry_point, &recovery_owned)?;
        let interrupt_before = validate_static_interrupts(raw.interrupt_before, &node_ids)?;
        let interrupt_after = validate_static_interrupts(raw.interrupt_after, &node_ids)?;
        let mut definition_digest = definition_digest(
            &raw.entry_point,
            &state,
            &state_defaults,
            &state_declaration_order,
            &nodes,
        );
        // Overwrite-only definitions keep existing definition bytes and checkpoint lineage.
        if !state_reducers.is_empty() {
            definition_digest = reducers_digest(definition_digest, &state_reducers);
        }
        // Empty policies keep existing definition bytes and checkpoint lineage.
        if !interrupt_before.is_empty() || !interrupt_after.is_empty() {
            definition_digest = super::static_pause::policy_digest(
                definition_digest,
                &interrupt_before,
                &interrupt_after,
            );
        }
        definition_digest = recovery.bind_digest(definition_digest);
        Ok(Self {
            recovery,
            entry_point: raw.entry_point,
            state,
            state_defaults,
            state_declaration_order,
            state_reducers: Arc::new(state_reducers),
            nodes,
            parallel_owned_nodes,
            map_owned_nodes,
            definition_digest,
            interrupt_before,
            interrupt_after,
        })
    }

    #[must_use]
    /// Recover deterministic nodes, model checkpoints, or durable sandbox receipts.
    pub(crate) fn recovery_frontier_supported(&self, pending: &[String]) -> bool {
        pending.iter().all(|id| {
            self.nodes.iter().any(|node| {
                node.id() == id
                    && !self.parallel_owned_nodes.contains(id)
                    && !self.map_owned_nodes.contains(id)
                    && matches!(
                        node,
                        PipelineNodeDefinition::Llm(_)
                            | PipelineNodeDefinition::Code(_)
                            | PipelineNodeDefinition::StateModifier(_)
                            | PipelineNodeDefinition::SplitOut(_)
                            | PipelineNodeDefinition::Aggregate(_)
                            | PipelineNodeDefinition::Decision(_)
                            | PipelineNodeDefinition::Router(_)
                            | PipelineNodeDefinition::Parallel(_)
                            | PipelineNodeDefinition::Map(_)
                    )
            })
        })
    }

    pub(crate) fn node_recovery_spec(
        &self,
        id: &str,
    ) -> Option<([u8; 32], super::node_recovery::NodeRecoveryPolicy)> {
        if self.parallel_owned_nodes.contains(id) || self.map_owned_nodes.contains(id) {
            return None;
        }
        match self.nodes.iter().find(|node| node.id() == id)? {
            PipelineNodeDefinition::Code(node) => Some((
                node.validated_digest(),
                self.recovery
                    .get(id)
                    .map(|binding| binding.policy.clone())
                    .unwrap_or_default(),
            )),
            _ => None,
        }
    }

    pub(crate) fn node_result_projector(
        &self,
        id: &str,
        execution_id: &str,
        generation: u64,
    ) -> Result<
        Option<Arc<dyn super::node_recovery_runtime::NodeResultRecovery>>,
        PipelineConfigurationError,
    > {
        if self.parallel_owned_nodes.contains(id) || self.map_owned_nodes.contains(id) {
            return Ok(None);
        }
        if let Some(PipelineNodeDefinition::Code(node)) =
            self.nodes.iter().find(|node| node.id() == id)
        {
            let projector = super::code_runtime::CodeCommittedProjector::new(
                node.clone(),
                self.declared_variable_types(),
                execution_id.to_owned(),
                generation,
                self.recovery.get(id).is_none(),
            )
            .map_err(|_| {
                PipelineConfigurationError::Invalid("The original Code result contract is invalid.")
            })?;
            return Ok(Some(Arc::new(projector)));
        }
        Ok(None)
    }

    pub(crate) fn entry_point(&self) -> &str {
        &self.entry_point
    }

    #[must_use]
    pub(crate) fn node_count(&self) -> usize {
        self.nodes.len()
    }

    #[must_use]
    pub(crate) const fn definition_digest(&self) -> [u8; 32] {
        self.definition_digest
    }

    pub(crate) fn has_static_interrupts(&self) -> bool {
        !self.interrupt_before.is_empty() || !self.interrupt_after.is_empty()
    }

    pub(crate) fn static_pause_catalog(&self) -> StaticPauseCatalog {
        StaticPauseCatalog::from_definition(
            self.definition_digest,
            &self.interrupt_before,
            &self.interrupt_after,
            self.nodes.iter().map(|node| {
                let targets = match node {
                    PipelineNodeDefinition::Printer(printer) => vec![printer.reset_node_id()],
                    _ => node
                        .route_targets()
                        .into_iter()
                        .map(str::to_owned)
                        .collect(),
                };
                (
                    node.id().to_owned(),
                    node.config_digest(),
                    targets,
                    matches!(node, PipelineNodeDefinition::Printer(_)),
                )
            }),
        )
    }

    pub(crate) fn printer_pause_catalog(&self) -> PrinterPauseCatalog {
        PrinterPauseCatalog::from_definition(
            self.definition_digest,
            self.nodes.iter().filter_map(|node| match node {
                PipelineNodeDefinition::Printer(node) => Some(node.clone()),
                _ => None,
            }),
        )
    }

    pub(crate) fn resolve_legacy_toolkit_aliases(&mut self, aliases: &BTreeMap<String, String>) {
        for node in &mut self.nodes {
            match node {
                PipelineNodeDefinition::DirectTool(node) => {
                    node.resolve_legacy_toolkit_aliases(aliases);
                }
                PipelineNodeDefinition::Llm(node) => node.resolve_legacy_toolkit_aliases(aliases),
                PipelineNodeDefinition::Application(_)
                | PipelineNodeDefinition::Parallel(_)
                | PipelineNodeDefinition::Map(_)
                | PipelineNodeDefinition::Decision(_)
                | PipelineNodeDefinition::Hitl(_)
                | PipelineNodeDefinition::Printer(_)
                | PipelineNodeDefinition::Router(_)
                | PipelineNodeDefinition::Code(_)
                | PipelineNodeDefinition::StateModifier(_)
                | PipelineNodeDefinition::SplitOut(_)
                | PipelineNodeDefinition::Aggregate(_) => {}
            }
        }
    }

    /// Exact node-scoped toolkit selections, retained without credentials.
    pub(crate) fn llm_tool_selections(&self) -> impl Iterator<Item = &LlmToolkitSelection> {
        self.nodes.iter().flat_map(|node| match node {
            PipelineNodeDefinition::Llm(node) => node.tool_selections(),
            PipelineNodeDefinition::Application(_)
            | PipelineNodeDefinition::Parallel(_)
            | PipelineNodeDefinition::Map(_)
            | PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::DirectTool(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::Code(_)
            | PipelineNodeDefinition::StateModifier(_)
            | PipelineNodeDefinition::SplitOut(_)
            | PipelineNodeDefinition::Aggregate(_) => &[],
        })
    }

    /// Exact selected actions resolved by direct Toolkit or MCP nodes.
    pub(crate) fn direct_tool_selections(&self) -> impl Iterator<Item = &DirectToolSelection> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::DirectTool(node) => Some(node.selection()),
            PipelineNodeDefinition::Application(_)
            | PipelineNodeDefinition::Parallel(_)
            | PipelineNodeDefinition::Map(_)
            | PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Llm(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::Code(_)
            | PipelineNodeDefinition::StateModifier(_)
            | PipelineNodeDefinition::SplitOut(_)
            | PipelineNodeDefinition::Aggregate(_) => None,
        })
    }

    /// Pair an admitted node path with its exact saved participant selection.
    pub(crate) fn application_node_digest(&self, node_id: &str) -> Option<[u8; 32]> {
        self.nodes.iter().find_map(|node| match node {
            PipelineNodeDefinition::Application(node) if node.id() == node_id => {
                Some(node.config_digest())
            }
            _ => None,
        })
    }

    pub(crate) fn application_nodes(
        &self,
    ) -> impl Iterator<Item = (&str, &PipelineApplicationSelection)> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::Application(node) => Some((node.id(), node.selection())),
            _ => None,
        })
    }

    /// Admitted node identifiers that can own an ADK subgraph thread.
    pub(crate) fn application_node_ids(&self) -> impl Iterator<Item = &str> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::Application(node) => Some(node.id()),
            _ => None,
        })
    }

    /// Exact saved-application participants selected by Agent nodes.
    pub(crate) fn application_selections(
        &self,
    ) -> impl Iterator<Item = &PipelineApplicationSelection> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::Application(node) => Some(node.selection()),
            PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::Parallel(_)
            | PipelineNodeDefinition::Map(_)
            | PipelineNodeDefinition::DirectTool(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Llm(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::Code(_)
            | PipelineNodeDefinition::StateModifier(_)
            | PipelineNodeDefinition::SplitOut(_)
            | PipelineNodeDefinition::Aggregate(_) => None,
        })
    }

    #[must_use]
    pub(crate) fn has_llm_nodes(&self) -> bool {
        self.nodes.iter().any(|node| {
            matches!(
                node,
                PipelineNodeDefinition::Decision(_) | PipelineNodeDefinition::Llm(_)
            )
        })
    }

    #[must_use]
    pub(crate) fn has_direct_tool_nodes(&self) -> bool {
        self.nodes
            .iter()
            .any(|node| matches!(node, PipelineNodeDefinition::DirectTool(_)))
    }

    #[must_use]
    pub(crate) fn has_application_nodes(&self) -> bool {
        self.nodes
            .iter()
            .any(|node| matches!(node, PipelineNodeDefinition::Application(_)))
    }

    pub(crate) fn llm_toolkit_aliases(&self) -> BTreeSet<String> {
        self.llm_tool_selections()
            .map(|selection| selection.alias().to_owned())
            .collect()
    }

    pub(crate) fn runtime_toolkit_aliases(&self) -> BTreeSet<String> {
        self.llm_toolkit_aliases()
            .into_iter()
            .chain(
                self.direct_tool_selections()
                    .map(|selection| selection.alias().to_owned()),
            )
            .chain(
                self.application_selections()
                    .map(|selection| selection.alias().to_owned()),
            )
            .collect()
    }

    /// Compile this immutable definition into one invocation-owned graph agent.
    pub(crate) fn compile(
        &self,
        agent_name: &str,
        checkpointer: Arc<dyn Checkpointer>,
        resume: Option<PipelineResume>,
    ) -> Result<GraphAgent, PipelineConfigurationError> {
        self.compile_with_runtime(
            agent_name,
            checkpointer,
            resume,
            &PipelineNodeRuntimes::default(),
        )
    }

    /// Compile with the invocation-owned model/tool factory required by LLM
    /// nodes. Pure/control graphs keep using [`Self::compile`].
    pub(crate) fn compile_with_llm_runtime(
        &self,
        agent_name: &str,
        checkpointer: Arc<dyn Checkpointer>,
        resume: Option<PipelineResume>,
        llm_factory: Option<&Arc<dyn PipelineLlmAgentFactory>>,
    ) -> Result<GraphAgent, PipelineConfigurationError> {
        self.compile_with_runtime(
            agent_name,
            checkpointer,
            resume,
            &PipelineNodeRuntimes::new(llm_factory.cloned(), None, None),
        )
    }

    /// Compile with invocation-owned model and direct-tool runtimes.
    pub(crate) fn compile_with_runtime(
        &self,
        agent_name: &str,
        checkpointer: Arc<dyn Checkpointer>,
        resume: Option<PipelineResume>,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<GraphAgent, PipelineConfigurationError> {
        if !valid_graph_id(agent_name) {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline agent name is malformed",
            ));
        }
        let (checkpointer, bound_runtimes) =
            self.bind_parallel_authority(checkpointer, runtimes)?;
        let (checkpointer, bound_runtimes) =
            self.bind_map_authority(checkpointer, &bound_runtimes)?;
        let runtimes = &bound_runtimes;
        let checkpointer: Arc<dyn Checkpointer> = Arc::new(StaticResumeCheckpointer::new(
            runtimes.scoped_checkpointer(checkpointer)?,
            resume
                .as_ref()
                .map(PipelineResume::static_after_checkpoints)
                .unwrap_or_default(),
        ));
        let state_schema = self.state_schema(self.runtime_channels());
        let result_policy = self.result_policy();
        // A root HITL decision directly to END runs no data-producing node.
        // Preserve its terminal value without presenting it as a new generation.
        let reuses_result = resume
            .as_ref()
            .and_then(PipelineResume::terminal_decision)
            .is_some_and(|(id, action)| {
                self.nodes.iter().any(|node| {
                    matches!(node, PipelineNodeDefinition::Hitl(hitl)
                    if hitl.id() == id && hitl.route(action) == Some("END"))
                })
            });
        let node_checkpointer = Arc::clone(&checkpointer);
        let mut builder = PipelineGraphBuilder {
            result_trace: self.result_trace_outputs(),
            reducers: self.typed_reducers(),
            target: PipelineGraphTarget::Agent(Box::new(
                GraphAgent::builder(agent_name)
                    .description("Elitea stored pipeline")
                    .state_schema(state_schema.clone())
                    .edge(START, &self.entry_point)
                    .checkpointer_arc(checkpointer)
                    .recursion_limit(PIPELINE_RECURSION_LIMIT)
                    .max_concurrency(1)
                    .output_mapper(move |state| {
                        let mut event = pipeline_completion_event_from_state(state, &result_policy);
                        if reuses_result {
                            event.provider_metadata.insert(
                                super::agent::PIPELINE_REUSED_RESULT_METADATA_KEY.to_owned(),
                                "v1".to_owned(),
                            );
                        }
                        vec![event]
                    }),
            )),
        };
        for node in self.nodes.iter().filter(|node| {
            !self.parallel_owned_nodes.contains(node.id())
                && !self.map_owned_nodes.contains(node.id())
        }) {
            builder = self.bind_node(builder, node, runtimes, &node_checkpointer)?;
        }
        let mut builder = builder.into_agent()?;
        let before_interrupts: Vec<&str> =
            self.interrupt_before.iter().map(String::as_str).collect();
        if !before_interrupts.is_empty() {
            builder = builder.interrupt_before(&before_interrupts);
        }
        let printer_interrupts = self
            .nodes
            .iter()
            .filter_map(|node| match node {
                PipelineNodeDefinition::Printer(node) => Some(node.id()),
                _ => None,
            })
            .collect::<Vec<_>>();
        let mut after_interrupts = printer_interrupts;
        after_interrupts.extend(self.interrupt_after.iter().map(String::as_str));
        after_interrupts.sort_unstable();
        after_interrupts.dedup();
        if !after_interrupts.is_empty() {
            builder = builder.interrupt_after(&after_interrupts);
        }
        if let Some(resume) = resume {
            let resume_state = resume.into_state();
            builder = builder
                .input_mapper(move |context| invocation_state(context, None, Some(&resume_state)));
        } else {
            builder = with_initial_checkpoint(
                builder,
                node_checkpointer,
                state_schema,
                self.state_defaults.clone(),
                Arc::clone(&self.state_reducers),
                self.entry_point.clone(),
            );
        }
        builder.build().map_err(PipelineConfigurationError::Graph)
    }

    /// Compile this definition for one exact saved-pipeline participant.
    ///
    /// The child keeps the same claim-fenced checkpointer but receives a
    /// namespaced thread from ADK [`SubgraphNode`](adk_rust::graph::subgraph::SubgraphNode).
    /// A compiler-owned terminal node projects the same public result policy
    /// into `elitea_response`, which is the only business result shared back to
    /// the parent Agent node.
    pub(crate) fn compile_subgraph_with_runtime(
        &self,
        checkpointer: Arc<dyn Checkpointer>,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<CompiledGraph, PipelineConfigurationError> {
        // A child restore re-merges its parent-mapped input through each
        // channel reducer, so typed channels would reduce twice (v1).
        if !self.state_reducers.is_empty() {
            return Err(PipelineConfigurationError::Unsupported(
                "typed state reducers are not supported in a nested pipeline",
            ));
        }
        let (checkpointer, bound_runtimes) =
            self.bind_parallel_authority(checkpointer, runtimes)?;
        let (checkpointer, bound_runtimes) =
            self.bind_map_authority(checkpointer, &bound_runtimes)?;
        let runtimes = &bound_runtimes;
        let checkpointer = runtimes.scoped_checkpointer(checkpointer)?;
        let state_schema = self.state_schema(self.runtime_channels());
        let result_policy = self.result_policy();
        // A completed no-op step persists initialized child input before any
        // business node starts. Existing checkpoints retain their own frontier.
        let graph = StateGraph::new(state_schema)
            .add_node(PipelineSubgraphEntryNode)
            .add_edge(START, SUBGRAPH_ENTRY_NODE)
            .add_edge(SUBGRAPH_ENTRY_NODE, &self.entry_point);
        let mut builder = PipelineGraphBuilder {
            target: PipelineGraphTarget::Subgraph {
                graph,
                terminal: SUBGRAPH_RESULT_NODE,
            },
            result_trace: self.result_trace_outputs(),
            reducers: None,
        };
        for node in self.nodes.iter().filter(|node| {
            !self.parallel_owned_nodes.contains(node.id())
                && !self.map_owned_nodes.contains(node.id())
        }) {
            builder = self.bind_node(builder, node, runtimes, &checkpointer)?;
            if node.route_targets().is_empty() {
                builder = builder.edge(node.id(), END);
            }
        }
        builder = builder.node(PipelineSubgraphResultNode { result_policy });
        let printer_interrupts = self
            .nodes
            .iter()
            .filter_map(|node| match node {
                PipelineNodeDefinition::Printer(node) => Some(node.id()),
                _ => None,
            })
            .collect::<Vec<_>>();
        let mut graph =
            exclusive_transitions(builder.into_subgraph()?.add_edge(SUBGRAPH_RESULT_NODE, END))
                .compile()
                .map_err(PipelineConfigurationError::Graph)?
                .with_checkpointer_arc(checkpointer)
                .with_recursion_limit(PIPELINE_RECURSION_LIMIT)
                .with_max_concurrency(1);
        let before_interrupts: Vec<&str> =
            self.interrupt_before.iter().map(String::as_str).collect();
        if !before_interrupts.is_empty() {
            graph = graph.with_interrupt_before(&before_interrupts);
        }
        let mut after_interrupts = printer_interrupts;
        after_interrupts.extend(self.interrupt_after.iter().map(String::as_str));
        after_interrupts.sort_unstable();
        after_interrupts.dedup();
        if !after_interrupts.is_empty() {
            graph = graph.with_interrupt_after(&after_interrupts);
        }
        Ok(graph)
    }

    fn runtime_channels(&self) -> BTreeSet<String> {
        let mut channels = BTreeSet::from([
            "input".to_owned(),
            "messages".to_owned(),
            "output".to_owned(),
            "result".to_owned(),
            "router_output".to_owned(),
            "elitea_response".to_owned(),
            "printer_output".to_owned(),
            "state_types".to_owned(),
            "context_info".to_owned(),
            "hitl_decisions".to_owned(),
            "hitl_interrupt".to_owned(),
            "parallel_tasks".to_owned(),
            "_pipeline_blocked".to_owned(),
            "session_id".to_owned(),
            APPLICATION_TASK_STATE_KEY.to_owned(),
            APPLICATION_MESSAGES_STATE_KEY.to_owned(),
            APPLICATION_RESULT_STATE_KEY.to_owned(),
            HITL_RESUME_STATE_KEY.to_owned(),
            DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
            LLM_TOOL_RESUME_STATE_KEY.to_owned(),
            super::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY.to_owned(),
            super::static_pause::STATIC_TEXT_RESUME_STATE_KEY.to_owned(),
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
            PIPELINE_RESULT_TRACE_STATE_KEY.to_owned(),
        ]);
        if self.has_parallel_nodes() {
            channels.insert(super::parallel::PARALLEL_RESUME_STATE_KEY.to_owned());
        }
        channels.extend(self.state.keys().cloned());
        for node in self
            .nodes
            .iter()
            .filter(|node| !self.map_owned_nodes.contains(node.id()))
        {
            if let PipelineNodeDefinition::Application(application) = node {
                channels.extend(application.variable_channels());
            }
            channels.extend(node.input_keys().iter().cloned());
            channels.extend(node.output_keys().iter().cloned());
            channels.extend(node.cleaned_keys().iter().cloned());
            if let Some(key) = node.edit_state_key() {
                channels.insert(key.to_owned());
            }
        }
        channels
    }

    fn bind_node(
        &self,
        builder: PipelineGraphBuilder,
        node: &PipelineNodeDefinition,
        runtimes: &PipelineNodeRuntimes,
        checkpointer: &Arc<dyn Checkpointer>,
    ) -> Result<PipelineGraphBuilder, PipelineConfigurationError> {
        Ok(match node {
            PipelineNodeDefinition::Code(node) => self.bind_code_node(builder, node, runtimes)?,
            PipelineNodeDefinition::Application(node) => {
                self.bind_application_node(builder, node, runtimes, checkpointer)?
            }
            PipelineNodeDefinition::Parallel(node) => {
                self.bind_parallel_node(builder, node, runtimes, checkpointer)?
            }
            PipelineNodeDefinition::Map(node) => self.bind_map_node(builder, node, runtimes)?,
            PipelineNodeDefinition::Decision(node) => {
                let Some(factory) = runtimes.llm.clone() else {
                    return Err(PipelineConfigurationError::Unsupported(
                        "the pipeline Decision runtime is not bound to this compiler",
                    ));
                };
                builder.node(DecisionNode::new(node.clone(), factory))
            }
            PipelineNodeDefinition::DirectTool(node) => {
                let Some(resolver) = runtimes.direct_tool.clone() else {
                    return Err(PipelineConfigurationError::Unsupported(
                        "the pipeline Toolkit runtime is not bound to this compiler",
                    ));
                };
                let transition = node.transition().map(ToOwned::to_owned);
                let node_id = node.id().to_owned();
                let mut direct = DirectToolNode::new(node.clone(), self.state.clone(), resolver)
                    .with_events(runtimes.events.clone());
                if let Some(authority) = runtimes.node_recovery.clone() {
                    direct = direct.with_node_recovery(authority);
                }
                let mut next = builder.node(direct);
                if let Some(transition) = transition {
                    let target = if transition == "END" {
                        END
                    } else {
                        transition.as_str()
                    };
                    next = next.edge(&node_id, target);
                }
                next
            }
            PipelineNodeDefinition::Hitl(node) => builder.node(HitlNode::new(node.clone())),
            PipelineNodeDefinition::Llm(node) => {
                let Some(factory) = runtimes.llm.clone() else {
                    return Err(PipelineConfigurationError::Unsupported(
                        "the pipeline LLM runtime is not bound to this compiler",
                    ));
                };
                let transition = node.transition().map(ToOwned::to_owned);
                let node_id = node.id().to_owned();
                let mut next =
                    builder.node(LlmNode::new(node.clone(), self.state.clone(), factory));
                if let Some(transition) = transition {
                    let target = if transition == "END" {
                        END
                    } else {
                        transition.as_str()
                    };
                    next = next.edge(&node_id, target);
                }
                next
            }
            PipelineNodeDefinition::Printer(node) => {
                let node_id = node.id().to_owned();
                let reset_node_id = node.reset_node_id();
                let transition = node.transition().to_owned();
                builder
                    .node(PrinterNode::new(node.clone()))
                    .node(PrinterResetNode::new(reset_node_id.clone()))
                    .edge(&node_id, &reset_node_id)
                    .edge(
                        &reset_node_id,
                        if transition == "END" {
                            END
                        } else {
                            transition.as_str()
                        },
                    )
            }
            PipelineNodeDefinition::Router(node) => builder.node(RouterNode::new(node.clone())),
            PipelineNodeDefinition::StateModifier(node) => {
                let next = builder.node(StateModifierNode::new(node.clone()));
                bind_transition(next, node.id(), node.transition())
            }
            PipelineNodeDefinition::SplitOut(node) => {
                let next = builder.node(SplitOutNode::new(node.clone()));
                bind_transition(next, node.id(), node.transition())
            }
            PipelineNodeDefinition::Aggregate(node) => {
                let next = builder.node(AggregateNode::new(node.clone()));
                bind_transition(next, node.id(), node.transition())
            }
        })
    }

    fn bind_code_node(
        &self,
        builder: PipelineGraphBuilder,
        node: &CodeNodeDefinition,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<PipelineGraphBuilder, PipelineConfigurationError> {
        let runtime = runtimes
            .code
            .clone()
            .ok_or(PipelineConfigurationError::Unsupported(
                "Code execution requires an admitted sandbox backend",
            ))?;
        let runtime = match runtimes.saved_child_scope_provider() {
            Some(provider) => {
                if runtimes.node_recovery.is_none() {
                    return Err(PipelineConfigurationError::Unsupported(
                        "Scoped Code requires its current fenced node writer",
                    ));
                }
                runtime
                    .bind_saved_child_scope(provider.clone())
                    .map_err(|_| {
                        PipelineConfigurationError::Unsupported(
                            "Scoped Code runtime authority is unavailable",
                        )
                    })?
            }
            None => runtime,
        };
        let mut bound = node.clone();
        bound.refresh_debug_compiler_digest(self.definition_digest);
        let executable = CodeNode::new(bound, self.state.clone(), runtime).map_err(|_| {
            PipelineConfigurationError::Invalid("Code state declarations are invalid")
        })?;
        let executable = executable.with_events(runtimes.events.clone());
        let binding = self.recovery.get(node.id());
        let mut next = if let Some(authority) = runtimes.node_recovery.clone() {
            let policy = binding
                .map(|binding| binding.policy.clone())
                .unwrap_or_default();
            let wrapper = super::node_recovery_runtime::RecoverableNode::new(
                Arc::new(executable),
                node.validated_digest(),
                policy,
                authority,
                binding.and_then(|binding| binding.error_route.clone()),
            );
            // A default-policy visit preserves the pre-existing effect identity.
            // Opting into bounded retries creates ordinal identities explicitly.
            builder.node(if binding.is_none() {
                wrapper.with_legacy_first_attempt()
            } else {
                wrapper
            })
        } else if binding.is_some() {
            return Err(PipelineConfigurationError::Unsupported(
                "node recovery requires a current fenced durable writer",
            ));
        } else {
            builder.node(executable)
        };
        if let Some(transition) = node.transition() {
            next = next.edge(
                node.id(),
                if transition == "END" { END } else { transition },
            );
        }
        Ok(next)
    }

    fn bind_application_node(
        &self,
        builder: PipelineGraphBuilder,
        node: &ApplicationNodeDefinition,
        runtimes: &PipelineNodeRuntimes,
        checkpointer: &Arc<dyn Checkpointer>,
    ) -> Result<PipelineGraphBuilder, PipelineConfigurationError> {
        let Some(resolver) = runtimes.application.as_ref() else {
            return Err(PipelineConfigurationError::Unsupported(
                "the pipeline Agent runtime is not bound to this compiler",
            ));
        };
        let transition = node.transition().map(ToOwned::to_owned);
        let node_id = node.id().to_owned();
        let application = ApplicationNode::new(
            node.clone(),
            self.state.clone(),
            resolver.as_ref(),
            Arc::clone(checkpointer),
        )
        .map_err(|_| {
            PipelineConfigurationError::Unsupported(
                "the selected pipeline Agent participant is unavailable",
            )
        })?;
        let mut next = builder.node(application);
        if let Some(transition) = transition {
            let target = if transition == "END" {
                END
            } else {
                transition.as_str()
            };
            next = next.edge(&node_id, target);
        }
        Ok(next)
    }

    /// Build the native ADK state schema for runtime-owned and user channels.
    fn state_schema(&self, channels: BTreeSet<String>) -> StateSchema {
        let mut schema = StateSchema::new();
        for channel in channels {
            let default = if channel == "state_types" {
                self.state_types_default()
            } else {
                self.state_defaults
                    .get(&channel)
                    .cloned()
                    .unwrap_or_else(|| runtime_channel_default(&channel))
            };
            let mut typed = Channel::new(&channel).with_default(default);
            if let Some(reducer) = self.state_reducers.get(&channel) {
                typed = typed.with_reducer(reducer.channel_reducer(&channel));
            }
            schema.channels.insert(channel.clone(), typed);
        }
        schema
            .channels
            .insert("messages".to_owned(), Channel::list("messages"));
        schema.channels.insert(
            "hitl_decisions".to_owned(),
            Channel::new("hitl_decisions")
                .with_default(json!([]))
                .with_reducer(Reducer::Custom(Arc::new(append_or_clear_list))),
        );
        schema.channels.insert(
            "parallel_tasks".to_owned(),
            Channel::new("parallel_tasks")
                .with_default(json!({}))
                .with_reducer(Reducer::Custom(Arc::new(merge_or_clear_object))),
        );
        schema
    }

    fn typed_reducers(&self) -> Option<Arc<BTreeMap<String, StateReducer>>> {
        (!self.state_reducers.is_empty()).then(|| Arc::clone(&self.state_reducers))
    }

    fn state_types_default(&self) -> serde_json::Value {
        let mut types = serde_json::Map::new();
        for key in &self.state_declaration_order {
            if key == "input" {
                continue;
            }
            if let Some(kind) = self.state.get(key) {
                types.insert(key.clone(), json!(kind));
            }
        }
        types.insert("state_types".to_owned(), json!("dict"));
        serde_json::Value::Object(types)
    }

    /// Declared result outputs of every top-level node, by node ID.
    ///
    /// Parallel- and Map-owned children run inside their parent node, so
    /// only the parent's own output is traced.
    fn result_trace_outputs(&self) -> BTreeMap<String, ResultTraceOutputs> {
        self.nodes
            .iter()
            .filter(|node| {
                !self.parallel_owned_nodes.contains(node.id())
                    && !self.map_owned_nodes.contains(node.id())
            })
            .filter_map(|node| {
                ResultTraceOutputs::from_declared(node.output_keys())
                    .map(|outputs| (node.id().to_owned(), outputs))
            })
            .collect()
    }

    /// Collect result candidates separately from graph control flow.
    ///
    /// `END` is only the ADK graph sink. The candidate belongs to the
    /// value-producing node whose explicit route targets that sink.
    fn result_policy(&self) -> PipelineResultPolicy {
        let mut keys = Vec::new();
        for node in &self.nodes {
            let (transition, output_keys) = match node {
                PipelineNodeDefinition::Application(node) => {
                    (node.transition(), node.output_keys())
                }
                PipelineNodeDefinition::Parallel(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Map(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Code(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::DirectTool(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Llm(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::StateModifier(node) => {
                    (node.transition(), node.output_keys())
                }
                PipelineNodeDefinition::SplitOut(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Aggregate(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Decision(_)
                | PipelineNodeDefinition::Hitl(_)
                | PipelineNodeDefinition::Printer(_)
                | PipelineNodeDefinition::Router(_) => continue,
            };
            if transition != Some("END") {
                continue;
            }
            if let Some(key) = output_keys.iter().find(|key| key.as_str() != "messages")
                && !internal_result_key(key)
                && !keys.contains(key)
            {
                keys.push(key.clone());
            }
        }
        let fallback_keys = self
            .state_declaration_order
            .iter()
            .filter(|key| !internal_result_key(key))
            .cloned()
            .collect();
        PipelineResultPolicy {
            terminal_data_keys: keys,
            fallback_data_keys: fallback_keys,
        }
    }
}

/// Largest integer magnitude a `JavaScript` number holds exactly (`2^53 - 1`).
///
/// The Web pipeline editor reads the same YAML with a `JavaScript` library, so an
/// integer identifier is canonical only inside this range. Beyond it the two
/// runtimes would print different decimal strings for one document.
const MAX_SAFE_INTEGER_IDENTIFIER: i64 = (1 << 53) - 1;

/// How many identifiers each compatibility rewrite changed in one document.
#[derive(Default)]
struct IdentifierNormalization {
    legacy: usize,
    numeric: usize,
}

/// Normalize graph identifiers before the strict typed parse.
///
/// Two compatibility rewrites run here, and both only produce strings:
///
/// * Older `EliteaUI` versions persisted labels such as `Agent 1` directly as
///   node identifiers. The Python runtime passes every identifier and target
///   through `clean_string`, so those documents execute as `Agent1`. New UI
///   versions author strict identifiers, but they intentionally do not rewrite
///   stored documents on load. The pass preserves already-valid identifiers,
///   applies the Python transformation only to legacy values, and leaves
///   malformed or oversized values for the normal validators to reject.
/// * The editor and hand-written YAML may leave a numeric id unquoted
///   (`id: 1`). An integer inside the `JavaScript` safe range becomes its
///   canonical decimal string, so `id: 1` and `id: "1"` are one pipeline with one
///   digest. Python crashes on such a document (`clean_string` is `re.sub` on an
///   int), so no behaviour is inherited here.
///
/// `null` is left alone in every position: the typed parse decides, exactly as
/// it did before, so an optional `transition: ~` stays absent and a required
/// null stays refused. Any other non-string value (a float, boolean, sequence,
/// mapping or tag) was already refused by the typed parse as a generic malformed
/// document; it now gets a typed refusal naming only the field. Duplicate
/// normalized node IDs are rejected by `parse_pipeline_nodes`.
fn normalize_graph_identifiers(
    document: &mut serde_yaml_ng::Value,
) -> Result<IdentifierNormalization, PipelineConfigurationError> {
    let mut counts = IdentifierNormalization::default();
    let Some(root) = document.as_mapping_mut() else {
        return Ok(counts);
    };
    normalize_mapping_graph_identifier(
        root,
        "entry_point",
        PipelineIdentifierField::EntryPoint,
        &mut counts,
    )?;
    normalize_mapping_graph_identifier_sequence(
        root,
        "interrupt_before",
        PipelineIdentifierField::InterruptBefore,
        &mut counts,
    )?;
    normalize_mapping_graph_identifier_sequence(
        root,
        "interrupt_after",
        PipelineIdentifierField::InterruptAfter,
        &mut counts,
    )?;

    let Some(serde_yaml_ng::Value::Sequence(nodes)) = root.get_mut("nodes") else {
        return Ok(counts);
    };
    for node in nodes {
        let Some(node) = node.as_mapping_mut() else {
            continue;
        };
        normalize_mapping_graph_identifier(
            node,
            "id",
            PipelineIdentifierField::NodeId,
            &mut counts,
        )?;
        normalize_mapping_graph_identifier(
            node,
            "transition",
            PipelineIdentifierField::Transition,
            &mut counts,
        )?;
        normalize_mapping_graph_identifier(
            node,
            "default_output",
            PipelineIdentifierField::DefaultOutput,
            &mut counts,
        )?;
        normalize_recovery_route(node, &mut counts)?;
        normalize_family_identifiers(node, &mut counts)?;
    }
    Ok(counts)
}

/// Identifier positions owned by one node family.
fn normalize_family_identifiers(
    node: &mut serde_yaml_ng::Mapping,
    counts: &mut IdentifierNormalization,
) -> Result<(), PipelineConfigurationError> {
    let node_type = node
        .get("type")
        .and_then(serde_yaml_ng::Value::as_str)
        .map(str::to_owned);
    match node_type.as_deref() {
        Some("decision") => {
            normalize_mapping_graph_identifier_sequence(
                node,
                "nodes",
                PipelineIdentifierField::DecisionNodes,
                counts,
            )?;
        }
        Some("router") => {
            normalize_mapping_graph_identifier_sequence(
                node,
                "routes",
                PipelineIdentifierField::RouterRoutes,
                counts,
            )?;
        }
        Some("hitl") => {
            if let Some(serde_yaml_ng::Value::Mapping(routes)) = node.get_mut("routes") {
                for target in routes.values_mut() {
                    normalize_graph_identifier(
                        target,
                        PipelineIdentifierField::HitlRoutes,
                        true,
                        counts,
                    )?;
                }
            }
        }
        // Retain the identifier boundary for the separately gated parallel
        // node so its later compiler integration cannot regress legacy
        // branch labels.
        Some("map") => {
            normalize_mapping_graph_identifier(
                node,
                "worker",
                PipelineIdentifierField::MapWorker,
                counts,
            )?;
        }
        Some("parallel") => {
            if let Some(serde_yaml_ng::Value::Sequence(branches)) = node.get_mut("branches") {
                for branch in branches {
                    let Some(branch) = branch.as_mapping_mut() else {
                        continue;
                    };
                    normalize_mapping_graph_identifier(
                        branch,
                        "id",
                        PipelineIdentifierField::ParallelBranchId,
                        counts,
                    )?;
                    normalize_mapping_graph_identifier(
                        branch,
                        "node",
                        PipelineIdentifierField::ParallelBranchNode,
                        counts,
                    )?;
                }
            }
        }
        _ => {}
    }
    Ok(())
}

/// The failure route of a node recovery policy names another node.
///
/// Recovery has no Python ancestor, so no stored document carries a legacy
/// label there: only the integer rewrite applies.
fn normalize_recovery_route(
    node: &mut serde_yaml_ng::Mapping,
    counts: &mut IdentifierNormalization,
) -> Result<(), PipelineConfigurationError> {
    let Some(serde_yaml_ng::Value::Mapping(recovery)) = node.get_mut("recovery") else {
        return Ok(());
    };
    let Some(serde_yaml_ng::Value::Mapping(on_failure)) = recovery.get_mut("on_failure") else {
        return Ok(());
    };
    if let Some(route) = on_failure.get_mut("route") {
        normalize_graph_identifier(route, PipelineIdentifierField::RecoveryRoute, false, counts)?;
    }
    Ok(())
}

fn normalize_mapping_graph_identifier(
    mapping: &mut serde_yaml_ng::Mapping,
    key: &str,
    field: PipelineIdentifierField,
    counts: &mut IdentifierNormalization,
) -> Result<(), PipelineConfigurationError> {
    if let Some(value) = mapping.get_mut(key) {
        normalize_graph_identifier(value, field, true, counts)?;
    }
    Ok(())
}

fn normalize_mapping_graph_identifier_sequence(
    mapping: &mut serde_yaml_ng::Mapping,
    key: &str,
    field: PipelineIdentifierField,
    counts: &mut IdentifierNormalization,
) -> Result<(), PipelineConfigurationError> {
    let Some(serde_yaml_ng::Value::Sequence(values)) = mapping.get_mut(key) else {
        return Ok(());
    };
    for value in values {
        normalize_graph_identifier(value, field, true, counts)?;
    }
    Ok(())
}

fn normalize_graph_identifier(
    value: &mut serde_yaml_ng::Value,
    field: PipelineIdentifierField,
    rewrite_legacy_labels: bool,
    counts: &mut IdentifierNormalization,
) -> Result<(), PipelineConfigurationError> {
    match value {
        serde_yaml_ng::Value::String(identifier) => {
            if rewrite_legacy_labels && normalize_legacy_label(identifier) {
                counts.legacy += 1;
            }
            Ok(())
        }
        serde_yaml_ng::Value::Null => Ok(()),
        serde_yaml_ng::Value::Number(number) => {
            let canonical = safe_integer_identifier(number)
                .ok_or(PipelineConfigurationError::InvalidIdentifier(field))?;
            *value = serde_yaml_ng::Value::String(canonical);
            counts.numeric += 1;
            Ok(())
        }
        serde_yaml_ng::Value::Bool(_)
        | serde_yaml_ng::Value::Sequence(_)
        | serde_yaml_ng::Value::Mapping(_)
        | serde_yaml_ng::Value::Tagged(_) => {
            Err(PipelineConfigurationError::InvalidIdentifier(field))
        }
    }
}

/// Canonical decimal string of an integer inside the `JavaScript` safe range.
///
/// Floats (including `1.0`, `.inf` and `.nan`) and integers beyond `2^53 - 1`
/// return `None`.
fn safe_integer_identifier(number: &serde_yaml_ng::Number) -> Option<String> {
    let value = match number.as_i64() {
        Some(value) => value,
        None => i64::try_from(number.as_u64()?).ok()?,
    };
    (-MAX_SAFE_INTEGER_IDENTIFIER..=MAX_SAFE_INTEGER_IDENTIFIER)
        .contains(&value)
        .then(|| value.to_string())
}

/// Rewrite one legacy UI label in place; `true` when it changed.
fn normalize_legacy_label(identifier: &mut String) -> bool {
    if valid_graph_id(identifier) || identifier.is_empty() || identifier.len() > MAX_NODE_ID_BYTES {
        return false;
    }
    let normalized = identifier
        .bytes()
        .filter(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
        .map(|byte| if byte == b'.' { '_' } else { byte as char })
        .collect::<String>();
    if normalized.is_empty() || normalized.len() > MAX_NODE_ID_BYTES {
        return false;
    }
    *identifier = normalized;
    true
}

#[derive(Clone)]
pub(super) struct PipelineResultPolicy {
    pub(super) terminal_data_keys: Vec<String>,
    pub(super) fallback_data_keys: Vec<String>,
}

struct PipelineSubgraphEntryNode;

#[async_trait]
impl Node for PipelineSubgraphEntryNode {
    fn name(&self) -> &str {
        SUBGRAPH_ENTRY_NODE
    }

    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, GraphError> {
        Ok(NodeOutput::new())
    }
}

struct PipelineSubgraphResultNode {
    result_policy: PipelineResultPolicy,
}

#[async_trait]
impl Node for PipelineSubgraphResultNode {
    fn name(&self) -> &str {
        SUBGRAPH_RESULT_NODE
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let result_text = select_pipeline_result(&context.state, &self.result_policy)
            .unwrap_or_else(|| super::agent::PIPELINE_COMPLETED_CONTENT.to_owned());
        Ok(NodeOutput::new().with_update("elitea_response", json!(result_text)))
    }
}

fn pipeline_completion_event_from_state(state: &State, policy: &PipelineResultPolicy) -> Event {
    if let Some(content) = select_pipeline_result(state, policy) {
        return pipeline_result_event(&content);
    }
    pipeline_completed_event()
}

/// Select the chat text of a finished pipeline turn.
///
/// The runtime trace names the node that wrote last. Its declared outputs
/// win, then its assistant message; a blank traced answer selects nothing.
/// Only without a trace does the static chain apply: terminal outputs, the
/// last assistant message, declared state.
/// The text is bounded; an oversized value is truncated, never dropped.
pub(super) fn select_pipeline_result(
    state: &State,
    policy: &PipelineResultPolicy,
) -> Option<String> {
    // A blocked or skipped tool stops the pipeline: its message is the answer, not
    // whatever a trace or the defaults of outputs that no node wrote would render.
    if let Some(blocked) = select_last_state_value(state, &["_pipeline_blocked".to_owned()]) {
        return Some(blocked.into_bounded_text());
    }
    // A trace proves which node wrote last; when it renders blank, no static
    // value may stand in for that node's answer.
    if let Some(trace) = ResultTrace::from_state(state) {
        return render_traced_keys(state, trace.keys())
            .or_else(|| {
                (trace.messages() && trace.keys().is_empty())
                    .then(|| select_last_assistant_message(state.get("messages")))
                    .flatten()
            })
            .map(RenderedResult::into_bounded_text);
    }
    select_last_state_value(state, &policy.terminal_data_keys)
        .or_else(|| select_last_assistant_message(state.get("messages")))
        .or_else(|| select_last_state_value(state, &policy.fallback_data_keys))
        .map(RenderedResult::into_bounded_text)
}

/// The static fallback has no proof that a node ran, so an empty collection
/// is an untouched default rather than an answer.
fn select_last_state_value(state: &State, keys: &[String]) -> Option<RenderedResult> {
    keys.iter()
        .rev()
        .filter_map(|key| state.get(key))
        .filter(|value| {
            !value.as_array().is_some_and(Vec::is_empty)
                && !value.as_object().is_some_and(serde_json::Map::is_empty)
        })
        .filter_map(render_state_value)
        .find(|content| !content.is_blank())
}

fn select_last_assistant_message(messages: Option<&serde_json::Value>) -> Option<RenderedResult> {
    messages?
        .as_array()?
        .iter()
        .rev()
        .filter(|message| {
            matches!(
                message.get("role").and_then(serde_json::Value::as_str),
                Some("assistant" | "ai")
            )
        })
        .filter_map(|message| message.get("content"))
        .filter_map(assistant_message_text)
        .find(|content| !content.trim().is_empty())
        .map(RenderedResult::Text)
}

/// Text of one assistant message content value: text blocks are joined.
fn assistant_message_text(value: &serde_json::Value) -> Option<String> {
    match value {
        serde_json::Value::Null => None,
        serde_json::Value::String(value) => Some(value.clone()),
        serde_json::Value::Array(blocks) => {
            let mut output = String::new();
            for block in blocks {
                match block {
                    serde_json::Value::String(text) => output.push_str(text),
                    serde_json::Value::Object(object)
                        if object.get("type").is_none()
                            || object.get("type").and_then(serde_json::Value::as_str)
                                == Some("text") =>
                    {
                        if let Some(text) = object.get("text").and_then(serde_json::Value::as_str) {
                            output.push_str(text);
                        }
                    }
                    _ => {}
                }
            }
            Some(output)
        }
        value => serde_json::to_string(value).ok(),
    }
}

pub(super) fn append_or_clear_list(
    current: serde_json::Value,
    update: serde_json::Value,
) -> serde_json::Value {
    if update.is_null() {
        return json!([]);
    }
    let mut values = match current {
        serde_json::Value::Array(values) => values,
        _ => Vec::new(),
    };
    if let serde_json::Value::Array(update) = update {
        values.extend(update);
    }
    serde_json::Value::Array(values)
}

pub(super) fn merge_or_clear_object(
    current: serde_json::Value,
    update: serde_json::Value,
) -> serde_json::Value {
    if update.is_null() {
        return json!({});
    }
    let mut values = match current {
        serde_json::Value::Object(values) => values,
        _ => serde_json::Map::new(),
    };
    if let serde_json::Value::Object(update) = update {
        values.extend(update);
    }
    serde_json::Value::Object(values)
}

fn runtime_channel_default(channel: &str) -> serde_json::Value {
    match channel {
        "messages" | "hitl_decisions" => json!([]),
        "parallel_tasks"
        | "state_types"
        | HITL_RESUME_STATE_KEY
        | DIRECT_TOOL_RESUME_STATE_KEY
        | LLM_TOOL_RESUME_STATE_KEY => {
            json!({})
        }
        APPLICATION_MESSAGES_STATE_KEY => json!([]),
        PIPELINE_NODE_EVENT_SCOPE_STATE_KEY
        | "context_info"
        | "hitl_interrupt"
        | "_pipeline_blocked" => serde_json::Value::Null,
        channel if RUNTIME_STRING_CHANNELS.contains(&channel) => json!(""),
        _ => serde_json::Value::Null,
    }
}

pub(super) fn internal_result_key(key: &str) -> bool {
    INTERNAL_RESULT_KEYS.contains(&key)
}

/// Persist the first frontier before any node can call a model or tool.
/// ADK restores it and receives no extra input, so append reducers run once.
/// A typed channel's default is its initial value, never an update applied to
/// that same default.
fn with_initial_checkpoint(
    builder: GraphAgentBuilder,
    checkpointer: Arc<dyn Checkpointer>,
    schema: StateSchema,
    defaults: BTreeMap<String, serde_json::Value>,
    reducers: Arc<BTreeMap<String, StateReducer>>,
    entry_point: String,
) -> GraphAgentBuilder {
    builder
        .before_agent_callback(move |context| {
            let checkpointer = checkpointer.clone();
            let schema = schema.clone();
            let defaults = defaults.clone();
            let reducers = Arc::clone(&reducers);
            let entry_point = entry_point.clone();
            async move {
                if checkpointer.load(context.session_id()).await?.is_none() {
                    let mut state = schema.initialize_state();
                    for (key, value) in invocation_state(context.as_ref(), Some(&defaults), None) {
                        if reducers.contains_key(&key) {
                            state.insert(key, value);
                        } else {
                            schema.apply_update(&mut state, &key, value);
                        }
                    }
                    let checkpoint =
                        Checkpoint::new(context.session_id(), state, 0, vec![entry_point]);
                    checkpointer.save(&checkpoint).await?;
                }
                Ok(())
            }
        })
        .input_mapper(|_| State::new())
}

fn invocation_state(
    context: &dyn InvocationContext,
    defaults: Option<&BTreeMap<String, serde_json::Value>>,
    resume: Option<&State>,
) -> State {
    let mut state: State = defaults
        .map(|defaults| defaults.clone().into_iter().collect())
        .unwrap_or_default();
    if resume.is_none() {
        let text = context
            .user_content()
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect::<Vec<_>>()
            .join("\n");
        if !text.is_empty() {
            state.insert("input".to_owned(), json!(text));
            state.insert(
                "messages".to_owned(),
                json!([{"role": "user", "content": text}]),
            );
        }
    }
    state.insert("session_id".to_owned(), json!(context.session_id()));
    if let Some(resume) = resume {
        state.extend(resume.clone());
    }
    state
}

fn parse_pipeline_nodes(
    raw_nodes: Vec<serde_yaml_ng::Value>,
    state: &BTreeMap<String, String>,
) -> Result<(Vec<PipelineNodeDefinition>, BTreeSet<String>), PipelineConfigurationError> {
    let mut nodes = Vec::with_capacity(raw_nodes.len());
    let mut node_ids = BTreeSet::new();
    for raw_node in raw_nodes {
        let node = parse_pipeline_node(&raw_node)?;
        if matches!(node.id(), SUBGRAPH_RESULT_NODE | SUBGRAPH_ENTRY_NODE) {
            return Err(PipelineConfigurationError::Invalid(
                "a pipeline node identifier is reserved",
            ));
        }
        if !node_ids.insert(node.id().to_owned()) {
            return Err(PipelineConfigurationError::Invalid(
                "pipeline node identifiers must be unique",
            ));
        }
        nodes.push(node);
    }
    let map_owned = nodes
        .iter()
        .filter_map(|node| match node {
            PipelineNodeDefinition::Map(map) => Some(map.runtime().worker.as_str()),
            _ => None,
        })
        .collect::<BTreeSet<_>>();
    for node in &nodes {
        if !map_owned.contains(node.id()) {
            validate_node_state(node, state)?;
            validate_shaping_channels(node, state)?;
        }
    }
    Ok((nodes, node_ids))
}

fn parse_pipeline_node(
    raw_node: &serde_yaml_ng::Value,
) -> Result<PipelineNodeDefinition, PipelineConfigurationError> {
    parse_pipeline_node_admitting(raw_node, SHAPING_INTEGRATION_READY)
}

/// The typed cause code of [`PipelineConfigurationError::NodeTypeNotAvailable`];
/// the lifecycle maps it to its own registered, data-free message.
pub(crate) const NODE_TYPE_NOT_AVAILABLE_CODE: &str = "graph.pipeline.node_type_not_available";

/// Production admission of data shaping nodes waits for deployed acceptance.
/// Only rehearsal builds (`graph-extensions-rehearsal`) admit them.
const SHAPING_INTEGRATION_READY: bool = cfg!(feature = "graph-extensions-rehearsal");

/// Production admission of typed state reducers waits for the Web reducer
/// editor (Wave 2). Only rehearsal builds admit the `reducer` key.
const TYPED_REDUCERS_READY: bool = cfg!(feature = "graph-extensions-rehearsal");

#[cfg(test)]
pub(super) fn shaping_node_admission(
    raw_node: &serde_yaml_ng::Value,
    shaping_admitted: bool,
) -> Result<(), &'static str> {
    parse_pipeline_node_admitting(raw_node, shaping_admitted)
        .map(|_| ())
        .map_err(|error| error.code())
}

fn parse_pipeline_node_admitting(
    raw_node: &serde_yaml_ng::Value,
    shaping_admitted: bool,
) -> Result<PipelineNodeDefinition, PipelineConfigurationError> {
    let node_type = yaml_string_field(raw_node, "type")?;
    let encoded = serde_yaml_ng::to_string(raw_node)
        .map_err(|source| PipelineConfigurationError::MalformedYaml { source })?;
    match node_type {
        "code" => CodeNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Code)
            .map_err(PipelineConfigurationError::Invalid),
        "decision" => DecisionNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Decision)
            .map_err(|error| match error {
                DecisionConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::Decision)
                }
                _ => PipelineConfigurationError::Invalid("a Decision node is invalid"),
            }),
        "map" => MapNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Map)
            .map_err(|_| PipelineConfigurationError::Invalid("a Map node is invalid")),
        "parallel" => ParallelNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Parallel)
            .map_err(|error| match error {
                ParallelConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::Parallel)
                }
                _ => PipelineConfigurationError::Invalid("a fixed Parallel node is invalid"),
            }),
        "agent" => ApplicationNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Application)
            .map_err(|error| match error {
                ApplicationConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::Agent)
                }
                _ => PipelineConfigurationError::Invalid("an Agent node is invalid"),
            }),
        "toolkit" | "mcp" => DirectToolNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::DirectTool)
            .map_err(|error| match error {
                DirectToolConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::DirectTool)
                }
                _ => PipelineConfigurationError::Invalid("a direct-tool node is invalid"),
            }),
        "hitl" => HitlNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Hitl)
            .map_err(|error| match error {
                HitlConfigurationError::ResourceExhausted => node_limit(PipelineNodeLimit::Hitl),
                _ => PipelineConfigurationError::Invalid("a HITL node is invalid"),
            }),
        "llm" => LlmNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Llm)
            .map_err(|error| match error {
                LlmConfigurationError::ResourceExhausted => node_limit(PipelineNodeLimit::Llm),
                _ => PipelineConfigurationError::Invalid("an LLM node is invalid"),
            }),
        "printer" => PrinterNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Printer)
            .map_err(|error| match error {
                PrinterConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::Printer)
                }
                _ => PipelineConfigurationError::Invalid("a Printer node is invalid"),
            }),
        "router" => RouterNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Router)
            .map_err(|error| match error {
                RouterConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::Router)
                }
                _ => PipelineConfigurationError::Invalid("a Router node is invalid"),
            }),
        "state_modifier" => StateModifierNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::StateModifier)
            .map_err(|error| match error {
                StateModifierConfigurationError::ResourceExhausted => {
                    node_limit(PipelineNodeLimit::StateModifier)
                }
                _ => PipelineConfigurationError::Invalid("a state modifier node is invalid"),
            }),
        "split_out" if shaping_admitted => SplitOutNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::SplitOut)
            .map_err(|_| PipelineConfigurationError::Invalid("a SplitOut node is invalid")),
        "aggregate" if shaping_admitted => AggregateNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Aggregate)
            .map_err(|_| PipelineConfigurationError::Invalid("an Aggregate node is invalid")),
        // Known to this runtime, not admitted by this build: named as a
        // deployment limit rather than as an unknown type.
        "split_out" => Err(PipelineConfigurationError::NodeTypeNotAvailable(
            PipelineGatedNodeType::SplitOut,
        )),
        "aggregate" => Err(PipelineConfigurationError::NodeTypeNotAvailable(
            PipelineGatedNodeType::Aggregate,
        )),
        _ => Err(PipelineConfigurationError::Unsupported(
            "the pipeline contains a node type that is not enabled",
        )),
    }
}

const fn node_limit(family: PipelineNodeLimit) -> PipelineConfigurationError {
    PipelineConfigurationError::LimitExceeded(PipelineLimit::Node(family))
}

#[allow(clippy::too_many_lines)] // One exhaustive arm per node family keeps each rule visible.
fn validate_node_state(
    node: &PipelineNodeDefinition,
    state: &BTreeMap<String, String>,
) -> Result<(), PipelineConfigurationError> {
    for key in node.input_keys() {
        if !builtin_state_key(key) && !state.contains_key(key) {
            return Err(PipelineConfigurationError::Invalid(
                "a node input key is not declared in pipeline state",
            ));
        }
    }
    for key in node.output_keys().iter().chain(node.cleaned_keys()) {
        if !builtin_state_key(key) && !state.contains_key(key) {
            return Err(PipelineConfigurationError::Invalid(
                "a node output or clean key is not declared in pipeline state",
            ));
        }
    }
    match node {
        PipelineNodeDefinition::Map(node) => {
            let map = node.runtime();
            if state.get(&map.source).map(String::as_str) != Some("list")
                || state.get(&map.destination).map(String::as_str) != Some("list")
                || builtin_state_key(&map.source)
                || builtin_state_key(&map.destination)
            {
                return Err(PipelineConfigurationError::Invalid(
                    "Map source and destination must be declared business list roots",
                ));
            }
        }
        PipelineNodeDefinition::Parallel(node) => {
            if builtin_state_key(node.output_key())
                || reserved_user_state_key(node.output_key())
                || state.get(node.output_key()).map(String::as_str) != Some("list")
            {
                return Err(PipelineConfigurationError::Invalid(
                    "a fixed Parallel output must be a declared list channel",
                ));
            }
        }
        PipelineNodeDefinition::Application(node) => {
            if node
                .mapped_variables()
                .any(|key| !builtin_state_key(key) && !state.contains_key(key))
            {
                return Err(PipelineConfigurationError::Invalid(
                    "an Agent input mapping variable is not declared in pipeline state",
                ));
            }
        }
        PipelineNodeDefinition::DirectTool(node) => {
            for mapping in node.input_mapping().values() {
                if let DirectToolInputMapping::Variable(key) = mapping
                    && !builtin_state_key(key)
                    && !state.contains_key(key)
                {
                    return Err(PipelineConfigurationError::Invalid(
                        "a direct-tool input mapping variable is not declared in pipeline state",
                    ));
                }
            }
        }
        PipelineNodeDefinition::Llm(node) => {
            node.output_schema(state).map_err(|_| {
                PipelineConfigurationError::Invalid("an LLM node output schema is invalid")
            })?;
            for mapping in node.input_mapping().values() {
                if let super::llm::LlmInputMapping::Variable(key) = mapping
                    && !builtin_state_key(key)
                    && !state.contains_key(key)
                {
                    return Err(PipelineConfigurationError::Invalid(
                        "an LLM input mapping variable is not declared in pipeline state",
                    ));
                }
            }
        }
        PipelineNodeDefinition::Printer(node) => {
            if let PrinterInputMapping::Variable(key) = node.mapping()
                && !builtin_state_key(key)
                && !state.contains_key(key)
            {
                return Err(PipelineConfigurationError::Invalid(
                    "a Printer input mapping variable is not declared in pipeline state",
                ));
            }
        }
        PipelineNodeDefinition::Decision(_)
        | PipelineNodeDefinition::Hitl(_)
        | PipelineNodeDefinition::Router(_)
        | PipelineNodeDefinition::Code(_)
        | PipelineNodeDefinition::StateModifier(_)
        | PipelineNodeDefinition::SplitOut(_)
        | PipelineNodeDefinition::Aggregate(_) => {}
    }
    if node
        .edit_state_key()
        .is_some_and(|key| !state.contains_key(key))
    {
        return Err(PipelineConfigurationError::Invalid(
            "the HITL edit key is not declared in pipeline state",
        ));
    }
    Ok(())
}

/// Shaping nodes read one user source and overwrite one distinct user list.
fn validate_shaping_channels(
    node: &PipelineNodeDefinition,
    state: &BTreeMap<String, String>,
) -> Result<(), PipelineConfigurationError> {
    let source_kind = match node {
        PipelineNodeDefinition::SplitOut(node) => node.source_state_type(),
        PipelineNodeDefinition::Aggregate(_) => "list",
        PipelineNodeDefinition::Code(_)
        | PipelineNodeDefinition::Application(_)
        | PipelineNodeDefinition::Parallel(_)
        | PipelineNodeDefinition::Map(_)
        | PipelineNodeDefinition::Decision(_)
        | PipelineNodeDefinition::DirectTool(_)
        | PipelineNodeDefinition::Hitl(_)
        | PipelineNodeDefinition::Llm(_)
        | PipelineNodeDefinition::Printer(_)
        | PipelineNodeDefinition::Router(_)
        | PipelineNodeDefinition::StateModifier(_) => return Ok(()),
    };
    let user_key = |key: &str| !builtin_state_key(key) && !reserved_user_state_key(key);
    let ([source], [output]) = (node.input_keys(), node.output_keys()) else {
        return Err(PipelineConfigurationError::Invalid(
            "a data shaping node needs one source and one output",
        ));
    };
    if source == output
        || !user_key(source)
        || !user_key(output)
        || state.get(source).map(String::as_str) != Some(source_kind)
        || state.get(output).map(String::as_str) != Some("list")
    {
        return Err(PipelineConfigurationError::Invalid(
            "a data shaping node needs a typed user source and a distinct user list output",
        ));
    }
    Ok(())
}

fn validate_state(
    raw: serde_yaml_ng::Mapping,
    reducers_admitted: bool,
) -> Result<ValidatedState, PipelineConfigurationError> {
    if raw.len() > MAX_PIPELINE_STATE_KEYS {
        return Err(PipelineConfigurationError::ResourceExhausted);
    }
    let mut state = BTreeMap::new();
    let mut defaults = BTreeMap::new();
    let mut declaration_order = Vec::with_capacity(raw.len());
    let mut reducers = BTreeMap::new();
    for (raw_key, raw_kind) in raw {
        let Some(key) = raw_key.as_str().map(ToOwned::to_owned) else {
            return Err(PipelineConfigurationError::Invalid(
                "a pipeline state key must be a string",
            ));
        };
        let kind = serde_yaml_ng::from_value::<RawStateType>(raw_kind).map_err(|_| {
            PipelineConfigurationError::Invalid("a pipeline state type is malformed")
        })?;
        if !valid_output_key(&key) || reserved_user_state_key(&key) {
            return Err(PipelineConfigurationError::Invalid(
                "a pipeline state key is malformed or reserved",
            ));
        }
        let normalized = match kind.name() {
            "str" | "string" => "str",
            "int" | "number" => "int",
            "float" => "float",
            "bool" => "bool",
            "list" => "list",
            "dict" => "dict",
            _ => {
                return Err(PipelineConfigurationError::Unsupported(
                    "the pipeline declares an unsupported state type",
                ));
            }
        };
        if (key == "input" && normalized != "str") || (key == "messages" && normalized != "list") {
            return Err(PipelineConfigurationError::Invalid(
                "a built-in pipeline state key has the wrong type",
            ));
        }
        let value = kind
            .configured_value()
            .cloned()
            .unwrap_or_else(|| default_state_value(normalized));
        if !state_value_matches(normalized, &value) {
            return Err(PipelineConfigurationError::Invalid(
                "a pipeline state default has the wrong type",
            ));
        }
        if let Some(reducer) = typed_reducer(&key, &kind, normalized, &value, reducers_admitted)? {
            reducers.insert(key.clone(), reducer);
        }
        defaults.insert(key.clone(), value);
        declaration_order.push(key.clone());
        state.insert(key, normalized.to_owned());
    }
    Ok((state, defaults, declaration_order, reducers))
}

/// The typed reducer one state declaration selects; `None` overwrites.
fn typed_reducer(
    key: &str,
    kind: &RawStateType,
    normalized: &str,
    default: &serde_json::Value,
    admitted: bool,
) -> Result<Option<StateReducer>, PipelineConfigurationError> {
    let Some(name) = kind.reducer() else {
        return Ok(None);
    };
    if !admitted {
        return Err(PipelineConfigurationError::Unsupported(
            "typed state reducers are not available in this deployment",
        ));
    }
    if matches!(key, "input" | "messages") {
        return Err(PipelineConfigurationError::Invalid(
            "a built-in pipeline state key cannot declare a reducer",
        ));
    }
    let Some(reducer) = StateReducer::parse(name).map_err(|_| {
        PipelineConfigurationError::Invalid("a pipeline state reducer is not supported")
    })?
    else {
        return Ok(None);
    };
    if reducer.state_type() != normalized {
        return Err(PipelineConfigurationError::Invalid(
            "a pipeline state reducer does not match its state type",
        ));
    }
    reducer.check_held(default).map_err(|_| {
        PipelineConfigurationError::Invalid("a pipeline state default exceeds its reducer bounds")
    })?;
    Ok(Some(reducer))
}

/// Channels whose writes must replace the value: a Map destination, a
/// Parallel output, a shaping output, a HITL edit, a `StateModifier` clean, and
/// every output of a node that runs inside a Parallel or Map parent (its
/// updates never pass the top-level [`ReducerGuard`]).
fn validate_overwrite_channels(
    nodes: &[PipelineNodeDefinition],
    reducers: &BTreeMap<String, StateReducer>,
    fanout_owned: &BTreeSet<String>,
) -> Result<(), PipelineConfigurationError> {
    if reducers.is_empty() {
        return Ok(());
    }
    for node in nodes {
        let replaced: Vec<&str> = if fanout_owned.contains(node.id()) {
            node.output_keys()
                .iter()
                .chain(node.cleaned_keys())
                .map(String::as_str)
                .chain(node.edit_state_key())
                .collect()
        } else {
            match node {
                PipelineNodeDefinition::Map(map) => vec![map.runtime().destination.as_str()],
                PipelineNodeDefinition::Parallel(parallel) => vec![parallel.output_key()],
                PipelineNodeDefinition::SplitOut(_) | PipelineNodeDefinition::Aggregate(_) => {
                    node.output_keys().iter().map(String::as_str).collect()
                }
                PipelineNodeDefinition::Hitl(_) => node.edit_state_key().into_iter().collect(),
                PipelineNodeDefinition::StateModifier(_) => {
                    node.cleaned_keys().iter().map(String::as_str).collect()
                }
                PipelineNodeDefinition::Code(_)
                | PipelineNodeDefinition::Application(_)
                | PipelineNodeDefinition::Decision(_)
                | PipelineNodeDefinition::DirectTool(_)
                | PipelineNodeDefinition::Llm(_)
                | PipelineNodeDefinition::Printer(_)
                | PipelineNodeDefinition::Router(_) => Vec::new(),
            }
        };
        if replaced.iter().any(|key| reducers.contains_key(*key)) {
            return Err(PipelineConfigurationError::Invalid(
                "this state channel must use the overwrite reducer",
            ));
        }
    }
    Ok(())
}

fn default_state_value(kind: &str) -> serde_json::Value {
    match kind {
        "str" => json!(""),
        "int" => json!(0),
        "float" => json!(0.0),
        "bool" => json!(false),
        "list" => json!([]),
        "dict" => json!({}),
        _ => serde_json::Value::Null,
    }
}

pub(super) fn state_value_matches(kind: &str, value: &serde_json::Value) -> bool {
    match kind {
        "str" => value.is_string(),
        "int" => value.as_i64().is_some() || value.as_u64().is_some(),
        "float" => value.as_f64().is_some(),
        "bool" => value.is_boolean(),
        "list" => value.is_array(),
        "dict" => value.is_object(),
        _ => false,
    }
}

fn builtin_state_key(key: &str) -> bool {
    matches!(
        key,
        "input"
            | "messages"
            | "output"
            | "result"
            | "router_output"
            | "elitea_response"
            | "printer_output"
            | "state_types"
            | "context_info"
            | "hitl_decisions"
            | "hitl_interrupt"
            | "parallel_tasks"
            | "_pipeline_blocked"
            | "session_id"
    )
}

pub(super) fn reserved_user_state_key(key: &str) -> bool {
    key.starts_with("__elitea_application_variable_")
        || key == HITL_RESUME_STATE_KEY
        || key == DIRECT_TOOL_RESUME_STATE_KEY
        || key == LLM_TOOL_RESUME_STATE_KEY
        || key == super::parallel::PARALLEL_RESUME_STATE_KEY
        || key == super::application::PARALLEL_AGENT_INPUTS_STATE_KEY
        || key == PIPELINE_NODE_EVENT_SCOPE_STATE_KEY
        || key == PIPELINE_RESULT_TRACE_STATE_KEY
        || matches!(
            key,
            APPLICATION_TASK_STATE_KEY
                | APPLICATION_MESSAGES_STATE_KEY
                | APPLICATION_RESULT_STATE_KEY
                | SUBGRAPH_RESULT_NODE
                | SUBGRAPH_ENTRY_NODE
        )
        || matches!(
            key,
            "output"
                | "result"
                | "router_output"
                | "elitea_response"
                | "printer_output"
                | "state_types"
                | "context_info"
                | "hitl_decisions"
                | "hitl_interrupt"
                | "parallel_tasks"
                | "_pipeline_blocked"
                | "session_id"
                | "thread_id"
                | "execution_finished"
                | "chat_history"
        )
}

fn yaml_string_field<'a>(
    value: &'a serde_yaml_ng::Value,
    field: &str,
) -> Result<&'a str, PipelineConfigurationError> {
    value
        .as_mapping()
        .and_then(|mapping| mapping.get(serde_yaml_ng::Value::String(field.to_owned())))
        .and_then(serde_yaml_ng::Value::as_str)
        .ok_or(PipelineConfigurationError::Invalid(
            "a pipeline node is missing a string type",
        ))
}

fn validate_static_interrupts(
    nodes: Vec<String>,
    known: &BTreeSet<String>,
) -> Result<Vec<String>, PipelineConfigurationError> {
    let mut unique = BTreeSet::new();
    for node in nodes {
        if !valid_graph_id(&node) || !known.contains(&node) || !unique.insert(node) {
            return Err(PipelineConfigurationError::Invalid(
                "a static interrupt must name one unique stored node",
            ));
        }
    }
    Ok(unique.into_iter().collect())
}

fn definition_digest(
    entry_point: &str,
    state: &BTreeMap<String, String>,
    state_defaults: &BTreeMap<String, serde_json::Value>,
    state_declaration_order: &[String],
    nodes: &[PipelineNodeDefinition],
) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(PIPELINE_DIGEST_DOMAIN);
    digest_field(&mut context, entry_point.as_bytes());
    for (key, kind) in state {
        digest_field(&mut context, key.as_bytes());
        digest_field(&mut context, kind.as_bytes());
        if let Some(value) = state_defaults.get(key) {
            let encoded = serde_json::to_vec(value).unwrap_or_default();
            digest_field(&mut context, &encoded);
        }
    }
    for key in state_declaration_order {
        digest_field(&mut context, key.as_bytes());
    }
    for node in nodes {
        digest_field(&mut context, node.id().as_bytes());
        let kind = match node {
            PipelineNodeDefinition::Code(_) => b"code".as_slice(),
            PipelineNodeDefinition::Application(_) => b"agent".as_slice(),
            PipelineNodeDefinition::Parallel(_) => b"parallel".as_slice(),
            PipelineNodeDefinition::Map(_) => b"map".as_slice(),
            PipelineNodeDefinition::Decision(_) => b"decision".as_slice(),
            PipelineNodeDefinition::DirectTool(node) => {
                node.selection().kind().wire_name().as_bytes()
            }
            PipelineNodeDefinition::Hitl(_) => b"hitl".as_slice(),
            PipelineNodeDefinition::Llm(_) => b"llm".as_slice(),
            PipelineNodeDefinition::Printer(_) => b"printer".as_slice(),
            PipelineNodeDefinition::Router(_) => b"router".as_slice(),
            PipelineNodeDefinition::StateModifier(_) => b"state_modifier".as_slice(),
            PipelineNodeDefinition::SplitOut(_) => b"split_out".as_slice(),
            PipelineNodeDefinition::Aggregate(_) => b"aggregate".as_slice(),
        };
        digest_field(&mut context, kind);
        digest_field(&mut context, &node.config_digest());
    }
    let digest = context.finish();
    let mut output = [0_u8; 32];
    output.copy_from_slice(digest.as_ref());
    output
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
}

/// The bound a stored pipeline exceeded. Names the limit, never a value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelineLimit {
    /// The whole YAML document is larger than [`MAX_PIPELINE_YAML_BYTES`].
    YamlBytes,
    /// More than the maximum number of nodes.
    NodeCount,
    /// The document expands past [`PIPELINE_YAML_BUDGET`] once anchors and aliases are applied.
    YamlExpansion,
    /// One node of the named family exceeds its own size or entry-count bound.
    Node(PipelineNodeLimit),
}

/// Node family whose per-node bound was exceeded.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelineNodeLimit {
    Agent,
    Decision,
    DirectTool,
    Hitl,
    Llm,
    Parallel,
    Printer,
    Router,
    StateModifier,
}

impl PipelineLimit {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::YamlBytes => "yaml_bytes",
            Self::NodeCount => "node_count",
            Self::YamlExpansion => "yaml_expansion",
            Self::Node(PipelineNodeLimit::Agent) => "nodes[].agent",
            Self::Node(PipelineNodeLimit::Decision) => "nodes[].decision",
            Self::Node(PipelineNodeLimit::DirectTool) => "nodes[].direct_tool",
            Self::Node(PipelineNodeLimit::Hitl) => "nodes[].hitl",
            Self::Node(PipelineNodeLimit::Llm) => "nodes[].llm",
            Self::Node(PipelineNodeLimit::Parallel) => "nodes[].parallel",
            Self::Node(PipelineNodeLimit::Printer) => "nodes[].printer",
            Self::Node(PipelineNodeLimit::Router) => "nodes[].router",
            Self::Node(PipelineNodeLimit::StateModifier) => "nodes[].state_modifier",
        }
    }
}

/// Graph-identifier position that held a value that cannot be an identifier.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelineIdentifierField {
    EntryPoint,
    InterruptBefore,
    InterruptAfter,
    NodeId,
    Transition,
    DefaultOutput,
    DecisionNodes,
    RouterRoutes,
    HitlRoutes,
    MapWorker,
    ParallelBranchId,
    ParallelBranchNode,
    RecoveryRoute,
}

impl PipelineIdentifierField {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::EntryPoint => "entry_point",
            Self::InterruptBefore => "interrupt_before[]",
            Self::InterruptAfter => "interrupt_after[]",
            Self::NodeId => "nodes[].id",
            Self::Transition => "nodes[].transition",
            Self::DefaultOutput => "nodes[].default_output",
            Self::DecisionNodes => "nodes[].nodes[]",
            Self::RouterRoutes => "nodes[].routes[]",
            Self::HitlRoutes => "nodes[].routes.*",
            Self::MapWorker => "nodes[].worker",
            Self::ParallelBranchId => "nodes[].branches[].id",
            Self::ParallelBranchNode => "nodes[].branches[].node",
            Self::RecoveryRoute => "nodes[].recovery.on_failure.route",
        }
    }
}

impl fmt::Display for PipelineLimit {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str((*self).as_str())
    }
}

impl fmt::Display for PipelineIdentifierField {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str((*self).as_str())
    }
}

/// A node type this runtime knows but admits only in graph-extensions
/// rehearsal builds (`SHAPING_INTEGRATION_READY`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelineGatedNodeType {
    SplitOut,
    Aggregate,
}

impl PipelineGatedNodeType {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::SplitOut => "split_out",
            Self::Aggregate => "aggregate",
        }
    }
}

impl std::fmt::Display for PipelineGatedNodeType {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// Stable, data-free stored-pipeline admission failure.
#[derive(Debug, Error)]
pub(crate) enum PipelineConfigurationError {
    #[error("the stored pipeline exceeds its resource bound")]
    ResourceExhausted,
    #[error("the stored pipeline exceeds a platform limit: {0}")]
    LimitExceeded(PipelineLimit),
    #[error("the stored pipeline has an invalid graph identifier in {0}")]
    InvalidIdentifier(PipelineIdentifierField),
    #[error("the stored pipeline YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the stored pipeline is invalid: {0}")]
    Invalid(&'static str),
    #[error("the stored pipeline requests an unavailable capability: {0}")]
    Unsupported(&'static str),
    #[error("the stored pipeline uses a node type this deployment does not admit: {0}")]
    NodeTypeNotAvailable(PipelineGatedNodeType),
    #[error("the stored pipeline graph could not be compiled")]
    Graph(#[source] GraphError),
}

impl PipelineConfigurationError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::ResourceExhausted => "graph.pipeline.configuration_resource_exhausted",
            Self::LimitExceeded(PipelineLimit::YamlBytes) => "graph.pipeline.yaml_bytes_exceeded",
            Self::LimitExceeded(PipelineLimit::NodeCount) => "graph.pipeline.node_count_exceeded",
            Self::LimitExceeded(PipelineLimit::YamlExpansion) => {
                "graph.pipeline.yaml_expansion_exceeded"
            }
            Self::LimitExceeded(PipelineLimit::Node(_)) => "graph.pipeline.node_limit_exceeded",
            Self::InvalidIdentifier(_) => "graph.pipeline.invalid_identifier",
            Self::MalformedYaml { .. } => "graph.pipeline.malformed_yaml",
            Self::Invalid(_) => "graph.pipeline.invalid_configuration",
            Self::Unsupported(_) => "graph.pipeline.unsupported_capability",
            Self::NodeTypeNotAvailable(_) => NODE_TYPE_NOT_AVAILABLE_CODE,
            Self::Graph(_) => "graph.pipeline.compile_failed",
        }
    }

    /// The limit or field this refusal names, for data-free diagnostics.
    #[must_use]
    pub(crate) const fn cause_detail(&self) -> Option<&'static str> {
        match self {
            Self::LimitExceeded(limit) => Some(limit.as_str()),
            Self::InvalidIdentifier(field) => Some(field.as_str()),
            Self::NodeTypeNotAvailable(node_type) => Some(node_type.as_str()),
            Self::ResourceExhausted
            | Self::MalformedYaml { .. }
            | Self::Invalid(_)
            | Self::Unsupported(_)
            | Self::Graph(_) => None,
        }
    }
}

#[cfg(test)]
mod result_trace_binding_tests {
    use super::{PipelineGraphBuilder, PipelineGraphTarget, ResultTraceOutputs, StateGraph};
    use adk_rust::graph::StateSchema;
    use std::collections::BTreeMap;

    #[test]
    fn an_unclaimed_result_trace_entry_refuses_the_graph() {
        let builder = PipelineGraphBuilder {
            target: PipelineGraphTarget::Subgraph {
                graph: StateGraph::new(StateSchema::new()),
                terminal: super::SUBGRAPH_RESULT_NODE,
            },
            result_trace: BTreeMap::from([(
                "renamed".to_owned(),
                ResultTraceOutputs::new(vec!["answer".to_owned()], false),
            )]),
            reducers: None,
        };
        let Err(error) = builder.into_subgraph() else {
            panic!("an unclaimed trace entry must refuse the graph");
        };
        assert_eq!(error.code(), "graph.pipeline.invalid_configuration");
    }
}
