//! Bounded stored-pipeline document admission and ADK graph compilation.
//!
//! Node families are admitted only after their bounded business contract is
//! implemented. Unsupported Python branches still fail before graph or
//! credential construction.

#![allow(dead_code)] // Production pipeline assembly remains capability-gated.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::sync::Arc;

use adk_rust::graph::{
    Channel, Checkpointer, CompiledGraph, END, GraphAgent, GraphAgentBuilder, GraphError, Node,
    NodeContext, NodeOutput, Reducer, START, State, StateGraph, StateSchema,
};
use adk_rust::{Event, InvocationContext, Part};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde::de::{Deserializer, SeqAccess, Visitor};
use serde_json::json;
use thiserror::Error;

use super::application::{
    APPLICATION_MESSAGES_STATE_KEY, APPLICATION_RESULT_STATE_KEY, APPLICATION_TASK_STATE_KEY,
    ApplicationNode, ApplicationNodeDefinition, PipelineApplicationResolver,
    PipelineApplicationSelection,
};
use super::decision::{DecisionNode, DecisionNodeDefinition};
use super::direct_tool::{
    DIRECT_TOOL_RESUME_STATE_KEY, DirectToolInputMapping, DirectToolNode, DirectToolNodeDefinition,
    DirectToolSelection, PipelineDirectToolResolver,
};
use super::hitl::{HITL_RESUME_STATE_KEY, HitlNode, HitlNodeDefinition};
use super::llm::{
    LLM_TOOL_RESUME_STATE_KEY, LlmNode, LlmNodeDefinition, LlmToolkitSelection,
    PipelineLlmAgentFactory,
};
use super::node_events::PIPELINE_NODE_EVENT_SCOPE_STATE_KEY;
use super::printer::{
    PrinterInputMapping, PrinterNode, PrinterNodeDefinition, PrinterPauseCatalog, PrinterResetNode,
};
use super::resume::PipelineResume;
use super::router::{RouterNode, RouterNodeDefinition};
use super::state_modifier::{StateModifierNode, StateModifierNodeDefinition};
use super::yaml::{MAX_NODE_ID_BYTES, valid_graph_id, valid_output_key};
use super::{pipeline_completed_event, pipeline_result_event};

const MAX_PIPELINE_YAML_BYTES: usize = 512 * 1024;
const MAX_PIPELINE_NODES: usize = 128;
const MAX_PIPELINE_STATE_KEYS: usize = 256;
const MAX_STATIC_INTERRUPTS: usize = 128;
const PIPELINE_RECURSION_LIMIT: usize = 100;
const MAX_PIPELINE_RESULT_BYTES: usize = 512 * 1024;
const SUBGRAPH_RESULT_NODE: &str = "__elitea_subgraph_result_v1";
const PIPELINE_DIGEST_DOMAIN: &[u8] = b"elitea.graph.pipeline.config.v1\0";

type ValidatedState = (
    BTreeMap<String, String>,
    BTreeMap<String, serde_json::Value>,
    Vec<String>,
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
    entry_point: String,
    state: BTreeMap<String, String>,
    state_defaults: BTreeMap<String, serde_json::Value>,
    state_declaration_order: Vec<String>,
    nodes: Vec<PipelineNodeDefinition>,
    definition_digest: [u8; 32],
}

/// Invocation-owned dependencies for executable pipeline node families.
#[derive(Clone, Default)]
pub(crate) struct PipelineNodeRuntimes {
    llm: Option<Arc<dyn PipelineLlmAgentFactory>>,
    direct_tool: Option<Arc<dyn PipelineDirectToolResolver>>,
    application: Option<Arc<dyn PipelineApplicationResolver>>,
}

impl PipelineNodeRuntimes {
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
        }
    }
}

enum PipelineGraphBuilder {
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
        match self {
            Self::Agent(builder) => Self::Agent(Box::new((*builder).node(node))),
            Self::Subgraph { graph, terminal } => Self::Subgraph {
                graph: graph.add_node(TerminalRedirectNode::new(node, terminal)),
                terminal,
            },
        }
    }

    fn edge(self, source: &str, target: &str) -> Self {
        match self {
            Self::Agent(builder) => Self::Agent(Box::new((*builder).edge(source, target))),
            Self::Subgraph { graph, terminal } => Self::Subgraph {
                graph: graph.add_edge(source, terminal_target(target, terminal)),
                terminal,
            },
        }
    }

    fn into_agent(self) -> Result<GraphAgentBuilder, PipelineConfigurationError> {
        match self {
            Self::Agent(builder) => Ok(*builder),
            Self::Subgraph { .. } => Err(PipelineConfigurationError::Invalid(
                "an internal pipeline graph builder changed kind",
            )),
        }
    }

    fn into_subgraph(self) -> Result<StateGraph, PipelineConfigurationError> {
        match self {
            Self::Subgraph { graph, .. } => Ok(graph),
            Self::Agent(_) => Err(PipelineConfigurationError::Invalid(
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

#[derive(Clone)]
enum PipelineNodeDefinition {
    Application(ApplicationNodeDefinition),
    Decision(DecisionNodeDefinition),
    DirectTool(DirectToolNodeDefinition),
    Hitl(HitlNodeDefinition),
    Llm(LlmNodeDefinition),
    Printer(PrinterNodeDefinition),
    Router(RouterNodeDefinition),
    StateModifier(StateModifierNodeDefinition),
}

impl PipelineNodeDefinition {
    fn id(&self) -> &str {
        match self {
            Self::Application(node) => node.id(),
            Self::Decision(node) => node.id(),
            Self::DirectTool(node) => node.id(),
            Self::Hitl(node) => node.id(),
            Self::Llm(node) => node.id(),
            Self::Printer(node) => node.id(),
            Self::Router(node) => node.id(),
            Self::StateModifier(node) => node.id(),
        }
    }

    fn input_keys(&self) -> &[String] {
        match self {
            Self::Application(node) => node.input_keys(),
            Self::Decision(node) => node.input_keys(),
            Self::DirectTool(node) => node.input_keys(),
            Self::Hitl(node) => node.input_keys(),
            Self::Llm(node) => node.input_keys(),
            Self::Printer(_) => &[],
            Self::Router(node) => node.input_keys(),
            Self::StateModifier(node) => node.input_keys(),
        }
    }

    fn output_keys(&self) -> &[String] {
        match self {
            Self::Application(node) => node.output_keys(),
            Self::DirectTool(node) => node.output_keys(),
            Self::Decision(_) | Self::Hitl(_) | Self::Printer(_) | Self::Router(_) => &[],
            Self::Llm(node) => node.output_keys(),
            Self::StateModifier(node) => node.output_keys(),
        }
    }

    fn cleaned_keys(&self) -> &[String] {
        match self {
            Self::Application(_)
            | Self::Decision(_)
            | Self::DirectTool(_)
            | Self::Hitl(_)
            | Self::Llm(_)
            | Self::Printer(_)
            | Self::Router(_) => &[],
            Self::StateModifier(node) => node.variables_to_clean(),
        }
    }

    fn edit_state_key(&self) -> Option<&str> {
        match self {
            Self::Hitl(node) => node.edit_state_key(),
            Self::Application(_)
            | Self::Decision(_)
            | Self::DirectTool(_)
            | Self::Llm(_)
            | Self::Printer(_)
            | Self::Router(_)
            | Self::StateModifier(_) => None,
        }
    }

    fn route_targets(&self) -> Vec<&str> {
        match self {
            Self::Application(node) => node.transition().into_iter().collect(),
            Self::Decision(node) => node.route_targets().collect(),
            Self::DirectTool(node) => node.transition().into_iter().collect(),
            Self::Hitl(node) => node.route_targets().collect(),
            Self::Llm(node) => node.transition().into_iter().collect(),
            Self::Printer(node) => [node.transition()].into_iter().collect(),
            Self::Router(node) => node.route_targets().collect(),
            Self::StateModifier(node) => node.transition().into_iter().collect(),
        }
    }

    fn config_digest(&self) -> [u8; 32] {
        match self {
            Self::Application(node) => node.config_digest(),
            Self::Decision(node) => node.config_digest(),
            Self::DirectTool(node) => node.config_digest(),
            Self::Hitl(node) => node.config_digest(),
            Self::Llm(node) => node.config_digest(),
            Self::Printer(node) => node.config_digest(),
            Self::Router(node) => node.config_digest(),
            Self::StateModifier(node) => node.config_digest(),
        }
    }
}

impl PipelineDefinition {
    /// Parse and validate a complete frozen pipeline YAML document.
    pub(crate) fn from_yaml(yaml: &str) -> Result<Self, PipelineConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_PIPELINE_YAML_BYTES {
            return Err(PipelineConfigurationError::ResourceExhausted);
        }
        let mut document = serde_yaml_ng::from_str::<serde_yaml_ng::Value>(yaml)
            .map_err(|source| PipelineConfigurationError::MalformedYaml { source })?;
        let normalized_identifier_count = normalize_legacy_graph_identifiers(&mut document);
        let raw = serde_yaml_ng::from_value::<RawPipelineDefinition>(document)
            .map_err(|source| PipelineConfigurationError::MalformedYaml { source })?;
        if normalized_identifier_count > 0 {
            tracing::warn!(
                event = "pipeline_legacy_identifier_normalized",
                normalized_identifier_count,
                "normalized legacy pipeline graph identifiers for runtime compatibility"
            );
        }
        Self::from_raw(raw)
    }

    fn from_raw(raw: RawPipelineDefinition) -> Result<Self, PipelineConfigurationError> {
        if raw.nodes.is_empty() || raw.nodes.len() > MAX_PIPELINE_NODES {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline must contain between 1 and 128 nodes",
            ));
        }
        if !valid_graph_id(&raw.entry_point) {
            return Err(PipelineConfigurationError::Invalid(
                "the pipeline entry point is malformed",
            ));
        }
        let (state, state_defaults, state_declaration_order) = validate_state(raw.state)?;
        if !raw.interrupt_before.is_empty() || !raw.interrupt_after.is_empty() {
            return Err(PipelineConfigurationError::Unsupported(
                "static pipeline interrupts are not enabled in this compiler slice",
            ));
        }

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
        let definition_digest = definition_digest(
            &raw.entry_point,
            &state,
            &state_defaults,
            &state_declaration_order,
            &nodes,
        );
        Ok(Self {
            entry_point: raw.entry_point,
            state,
            state_defaults,
            state_declaration_order,
            nodes,
            definition_digest,
        })
    }

    #[must_use]
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

    pub(crate) fn printer_pause_catalog(&self) -> PrinterPauseCatalog {
        PrinterPauseCatalog::from_definition(
            self.definition_digest,
            self.nodes.iter().filter_map(|node| match node {
                PipelineNodeDefinition::Printer(node) => Some(node.clone()),
                _ => None,
            }),
        )
    }

    /// Exact node-scoped toolkit selections, retained without credentials.
    pub(crate) fn llm_tool_selections(&self) -> impl Iterator<Item = &LlmToolkitSelection> {
        self.nodes.iter().flat_map(|node| match node {
            PipelineNodeDefinition::Llm(node) => node.tool_selections(),
            PipelineNodeDefinition::Application(_)
            | PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::DirectTool(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::StateModifier(_) => &[],
        })
    }

    /// Exact selected actions resolved by direct Toolkit or MCP nodes.
    pub(crate) fn direct_tool_selections(&self) -> impl Iterator<Item = &DirectToolSelection> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::DirectTool(node) => Some(node.selection()),
            PipelineNodeDefinition::Application(_)
            | PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Llm(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::StateModifier(_) => None,
        })
    }

    /// Exact saved-application participants selected by Agent nodes.
    pub(crate) fn application_selections(
        &self,
    ) -> impl Iterator<Item = &PipelineApplicationSelection> {
        self.nodes.iter().filter_map(|node| match node {
            PipelineNodeDefinition::Application(node) => Some(node.selection()),
            PipelineNodeDefinition::Decision(_)
            | PipelineNodeDefinition::DirectTool(_)
            | PipelineNodeDefinition::Hitl(_)
            | PipelineNodeDefinition::Llm(_)
            | PipelineNodeDefinition::Printer(_)
            | PipelineNodeDefinition::Router(_)
            | PipelineNodeDefinition::StateModifier(_) => None,
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
        let mut builder = PipelineGraphBuilder::Agent(Box::new(
            GraphAgent::builder(agent_name)
                .description("Elitea stored pipeline")
                .state_schema(state_schema)
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
        ));
        for node in &self.nodes {
            builder = self.bind_node(builder, node, runtimes, &node_checkpointer)?;
        }
        let mut builder = builder.into_agent()?;
        let printer_interrupts = self
            .nodes
            .iter()
            .filter_map(|node| match node {
                PipelineNodeDefinition::Printer(node) => Some(node.id()),
                _ => None,
            })
            .collect::<Vec<_>>();
        if !printer_interrupts.is_empty() {
            builder = builder.interrupt_after(&printer_interrupts);
        }
        if let Some(resume) = resume {
            let resume_state = resume.into_state();
            builder = builder
                .input_mapper(move |context| invocation_state(context, None, Some(&resume_state)));
        } else {
            let defaults = self.state_defaults.clone();
            builder = builder
                .input_mapper(move |context| invocation_state(context, Some(&defaults), None));
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
        let state_schema = self.state_schema(self.runtime_channels());
        let result_policy = self.result_policy();
        let graph = StateGraph::new(state_schema).add_edge(START, &self.entry_point);
        let mut builder = PipelineGraphBuilder::Subgraph {
            graph,
            terminal: SUBGRAPH_RESULT_NODE,
        };
        for node in &self.nodes {
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
        let mut graph = builder
            .into_subgraph()?
            .add_edge(SUBGRAPH_RESULT_NODE, END)
            .compile()
            .map_err(PipelineConfigurationError::Graph)?
            .with_checkpointer_arc(checkpointer)
            .with_recursion_limit(PIPELINE_RECURSION_LIMIT)
            .with_max_concurrency(1);
        if !printer_interrupts.is_empty() {
            graph = graph.with_interrupt_after(&printer_interrupts);
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
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
        ]);
        channels.extend(self.state.keys().cloned());
        for node in &self.nodes {
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
            PipelineNodeDefinition::Application(node) => {
                self.bind_application_node(builder, node, runtimes, checkpointer)?
            }
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
                let mut next = builder.node(DirectToolNode::new(
                    node.clone(),
                    self.state.clone(),
                    resolver,
                ));
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
                let transition = node.transition().map(ToOwned::to_owned);
                let node_id = node.id().to_owned();
                let mut next = builder.node(StateModifierNode::new(node.clone()));
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
        })
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
            schema.channels.insert(
                channel.clone(),
                Channel::new(&channel).with_default(default),
            );
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
                PipelineNodeDefinition::DirectTool(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::Llm(node) => (node.transition(), node.output_keys()),
                PipelineNodeDefinition::StateModifier(node) => {
                    (node.transition(), node.output_keys())
                }
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

/// Normalize only graph identifiers that the current Python compiler rewrites.
///
/// Older `EliteaUI` versions persisted labels such as `Agent 1` directly as node
/// identifiers. The Python runtime passes every identifier and target through
/// `clean_string`, so those documents execute as `Agent1`. New UI versions
/// author strict identifiers, but they intentionally do not rewrite stored
/// documents on load. This compatibility pass is therefore runtime-local. It
/// preserves already-valid Rust identifiers, applies the Python transformation
/// only to legacy values, and leaves malformed or oversized values for the
/// normal validators to reject. Duplicate normalized node IDs are rejected by
/// `parse_pipeline_nodes`.
fn normalize_legacy_graph_identifiers(document: &mut serde_yaml_ng::Value) -> usize {
    let Some(root) = document.as_mapping_mut() else {
        return 0;
    };
    let mut count = 0;
    normalize_mapping_graph_identifier(root, "entry_point", &mut count);
    normalize_mapping_graph_identifier_sequence(root, "interrupt_before", &mut count);
    normalize_mapping_graph_identifier_sequence(root, "interrupt_after", &mut count);

    let Some(serde_yaml_ng::Value::Sequence(nodes)) = root.get_mut("nodes") else {
        return count;
    };
    for node in nodes {
        let Some(node) = node.as_mapping_mut() else {
            continue;
        };
        normalize_mapping_graph_identifier(node, "id", &mut count);
        normalize_mapping_graph_identifier(node, "transition", &mut count);
        normalize_mapping_graph_identifier(node, "default_output", &mut count);
        let node_type = node
            .get("type")
            .and_then(serde_yaml_ng::Value::as_str)
            .map(str::to_owned);
        match node_type.as_deref() {
            Some("decision") => {
                normalize_mapping_graph_identifier_sequence(node, "nodes", &mut count);
            }
            Some("router") => {
                normalize_mapping_graph_identifier_sequence(node, "routes", &mut count);
            }
            Some("hitl") => {
                if let Some(serde_yaml_ng::Value::Mapping(routes)) = node.get_mut("routes") {
                    for target in routes.values_mut() {
                        normalize_graph_identifier(target, &mut count);
                    }
                }
            }
            // Retain the identifier boundary for the separately gated parallel
            // node so its later compiler integration cannot regress legacy
            // branch labels.
            Some("parallel") => {
                if let Some(serde_yaml_ng::Value::Sequence(branches)) = node.get_mut("branches") {
                    for branch in branches {
                        let Some(branch) = branch.as_mapping_mut() else {
                            continue;
                        };
                        normalize_mapping_graph_identifier(branch, "id", &mut count);
                        normalize_mapping_graph_identifier(branch, "node", &mut count);
                    }
                }
            }
            _ => {}
        }
    }
    count
}

fn normalize_mapping_graph_identifier(
    mapping: &mut serde_yaml_ng::Mapping,
    field: &str,
    count: &mut usize,
) {
    if let Some(value) = mapping.get_mut(field) {
        normalize_graph_identifier(value, count);
    }
}

fn normalize_mapping_graph_identifier_sequence(
    mapping: &mut serde_yaml_ng::Mapping,
    field: &str,
    count: &mut usize,
) {
    let Some(serde_yaml_ng::Value::Sequence(values)) = mapping.get_mut(field) else {
        return;
    };
    for value in values {
        normalize_graph_identifier(value, count);
    }
}

fn normalize_graph_identifier(value: &mut serde_yaml_ng::Value, count: &mut usize) {
    let serde_yaml_ng::Value::String(identifier) = value else {
        return;
    };
    if valid_graph_id(identifier) || identifier.is_empty() || identifier.len() > MAX_NODE_ID_BYTES {
        return;
    }
    let normalized = identifier
        .bytes()
        .filter(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
        .map(|byte| if byte == b'.' { '_' } else { byte as char })
        .collect::<String>();
    if normalized.is_empty() || normalized.len() > MAX_NODE_ID_BYTES {
        return;
    }
    *identifier = normalized;
    *count += 1;
}

#[derive(Clone)]
pub(super) struct PipelineResultPolicy {
    pub(super) terminal_data_keys: Vec<String>,
    pub(super) fallback_data_keys: Vec<String>,
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

pub(super) fn select_pipeline_result(
    state: &State,
    policy: &PipelineResultPolicy,
) -> Option<String> {
    select_last_state_value(state, &policy.terminal_data_keys)
        .or_else(|| select_last_assistant_message(state.get("messages")))
        .or_else(|| select_last_state_value(state, &policy.fallback_data_keys))
        .filter(|content| content.len() <= MAX_PIPELINE_RESULT_BYTES)
}

fn select_last_state_value(state: &State, keys: &[String]) -> Option<String> {
    keys.iter()
        .rev()
        .filter_map(|key| state.get(key))
        .filter_map(normalize_pipeline_value)
        .find(|content| !content.trim().is_empty())
}

fn select_last_assistant_message(messages: Option<&serde_json::Value>) -> Option<String> {
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
        .filter_map(normalize_pipeline_value)
        .find(|content| !content.trim().is_empty())
}

fn normalize_pipeline_value(value: &serde_json::Value) -> Option<String> {
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

fn internal_result_key(key: &str) -> bool {
    INTERNAL_RESULT_KEYS.contains(&key)
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
        if node.id() == SUBGRAPH_RESULT_NODE {
            return Err(PipelineConfigurationError::Invalid(
                "a pipeline node identifier is reserved",
            ));
        }
        if !node_ids.insert(node.id().to_owned()) {
            return Err(PipelineConfigurationError::Invalid(
                "pipeline node identifiers must be unique",
            ));
        }
        validate_node_state(&node, state)?;
        nodes.push(node);
    }
    Ok((nodes, node_ids))
}

fn parse_pipeline_node(
    raw_node: &serde_yaml_ng::Value,
) -> Result<PipelineNodeDefinition, PipelineConfigurationError> {
    let node_type = yaml_string_field(raw_node, "type")?;
    let encoded = serde_yaml_ng::to_string(raw_node)
        .map_err(|source| PipelineConfigurationError::MalformedYaml { source })?;
    match node_type {
        "decision" => DecisionNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Decision)
            .map_err(|_| PipelineConfigurationError::Invalid("a Decision node is invalid")),
        "agent" => ApplicationNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Application)
            .map_err(|_| PipelineConfigurationError::Invalid("an Agent node is invalid")),
        "toolkit" | "mcp" => DirectToolNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::DirectTool)
            .map_err(|_| PipelineConfigurationError::Invalid("a direct-tool node is invalid")),
        "hitl" => HitlNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Hitl)
            .map_err(|_| PipelineConfigurationError::Invalid("a HITL node is invalid")),
        "llm" => LlmNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Llm)
            .map_err(|_| PipelineConfigurationError::Invalid("an LLM node is invalid")),
        "printer" => PrinterNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Printer)
            .map_err(|_| PipelineConfigurationError::Invalid("a Printer node is invalid")),
        "router" => RouterNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::Router)
            .map_err(|_| PipelineConfigurationError::Invalid("a Router node is invalid")),
        "state_modifier" => StateModifierNodeDefinition::from_yaml(&encoded)
            .map(PipelineNodeDefinition::StateModifier)
            .map_err(|_| PipelineConfigurationError::Invalid("a state modifier node is invalid")),
        _ => Err(PipelineConfigurationError::Unsupported(
            "the pipeline contains a node type that is not enabled",
        )),
    }
}

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
        PipelineNodeDefinition::Application(node) => {
            if let Some(key) = node.mapped_variable()
                && !builtin_state_key(key)
                && !state.contains_key(key)
            {
                return Err(PipelineConfigurationError::Invalid(
                    "an Agent task mapping variable is not declared in pipeline state",
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
        | PipelineNodeDefinition::StateModifier(_) => {}
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

fn validate_state(
    raw: serde_yaml_ng::Mapping,
) -> Result<ValidatedState, PipelineConfigurationError> {
    if raw.len() > MAX_PIPELINE_STATE_KEYS {
        return Err(PipelineConfigurationError::ResourceExhausted);
    }
    let mut state = BTreeMap::new();
    let mut defaults = BTreeMap::new();
    let mut declaration_order = Vec::with_capacity(raw.len());
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
        defaults.insert(key.clone(), value);
        declaration_order.push(key.clone());
        state.insert(key, normalized.to_owned());
    }
    Ok((state, defaults, declaration_order))
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

fn state_value_matches(kind: &str, value: &serde_json::Value) -> bool {
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

fn reserved_user_state_key(key: &str) -> bool {
    key == HITL_RESUME_STATE_KEY
        || key == DIRECT_TOOL_RESUME_STATE_KEY
        || key == LLM_TOOL_RESUME_STATE_KEY
        || key == PIPELINE_NODE_EVENT_SCOPE_STATE_KEY
        || matches!(
            key,
            APPLICATION_TASK_STATE_KEY
                | APPLICATION_MESSAGES_STATE_KEY
                | APPLICATION_RESULT_STATE_KEY
                | SUBGRAPH_RESULT_NODE
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
            PipelineNodeDefinition::Application(_) => b"agent".as_slice(),
            PipelineNodeDefinition::Decision(_) => b"decision".as_slice(),
            PipelineNodeDefinition::DirectTool(node) => {
                node.selection().kind().wire_name().as_bytes()
            }
            PipelineNodeDefinition::Hitl(_) => b"hitl".as_slice(),
            PipelineNodeDefinition::Llm(_) => b"llm".as_slice(),
            PipelineNodeDefinition::Printer(_) => b"printer".as_slice(),
            PipelineNodeDefinition::Router(_) => b"router".as_slice(),
            PipelineNodeDefinition::StateModifier(_) => b"state_modifier".as_slice(),
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

/// Stable, data-free stored-pipeline admission failure.
#[derive(Debug, Error)]
pub(crate) enum PipelineConfigurationError {
    #[error("the stored pipeline exceeds its resource bound")]
    ResourceExhausted,
    #[error("the stored pipeline YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the stored pipeline is invalid: {0}")]
    Invalid(&'static str),
    #[error("the stored pipeline requests an unavailable capability: {0}")]
    Unsupported(&'static str),
    #[error("the stored pipeline graph could not be compiled")]
    Graph(#[source] GraphError),
}

impl PipelineConfigurationError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::ResourceExhausted => "graph.pipeline.configuration_resource_exhausted",
            Self::MalformedYaml { .. } => "graph.pipeline.malformed_yaml",
            Self::Invalid(_) => "graph.pipeline.invalid_configuration",
            Self::Unsupported(_) => "graph.pipeline.unsupported_capability",
            Self::Graph(_) => "graph.pipeline.compile_failed",
        }
    }
}
