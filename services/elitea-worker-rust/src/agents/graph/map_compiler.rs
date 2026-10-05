//! Map owns one child worker. Fixed Parallel keeps its separate authority.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::graph::{
    Channel, Checkpointer, CompiledGraph, END, GraphError, START, State, StateGraph, StateSchema,
};
use ring::digest;
use serde_json::Value;

use super::{
    PipelineConfigurationError, PipelineDefinition, PipelineGraphBuilder, PipelineNodeDefinition,
    PipelineNodeRuntimes, builtin_state_key, state_value_matches, validate_node_state,
};
use crate::agents::graph::application::{
    ApplicationNode, PARALLEL_AGENT_INPUTS_STATE_KEY, PipelineApplicationResolver,
};
use crate::agents::graph::map_authority::MapCheckpointAuthority;
use crate::agents::graph::map_reduce::{
    DurableMapNode, MAX_CHECKPOINT_BYTES, MapDefinition, MapItemExecution,
    MapOccurrenceCheckpointer, MapWorkerGraphFactory, MapWorkerKind, MapWorkerTerminal, map_error,
    validate_state_boundary,
};
use crate::agents::graph::map_yaml::MapNodeDefinition;
use crate::agents::graph::node_events::{
    PIPELINE_NODE_EVENT_SCOPE_STATE_KEY, PipelineNodeEventSender,
};
use crate::agents::graph::state_modifier::StateModifierNode;

const MAP_INTEGRATION_READY: bool = false;

pub(crate) struct MapCompilerBinding {
    authority: Arc<dyn MapCheckpointAuthority>,
    occurrence: Arc<MapOccurrenceCheckpointer>,
    deadline: tokio::time::Instant,
}

impl MapCompilerBinding {
    pub(crate) fn new(
        authority: Arc<dyn MapCheckpointAuthority>,
        deadline: tokio::time::Instant,
    ) -> Result<Arc<Self>, PipelineConfigurationError> {
        if !MAP_INTEGRATION_READY {
            return Err(PipelineConfigurationError::Unsupported(
                "Map requires original-scope restart and pause acceptance",
            ));
        }
        if deadline <= tokio::time::Instant::now() {
            return Err(PipelineConfigurationError::Invalid(
                "the original Map execution deadline has expired",
            ));
        }
        Ok(Arc::new(Self::bound(authority, deadline)))
    }

    fn bound(authority: Arc<dyn MapCheckpointAuthority>, deadline: tokio::time::Instant) -> Self {
        Self {
            occurrence: Arc::new(MapOccurrenceCheckpointer::new(authority.clone())),
            authority,
            deadline,
        }
    }

    pub(crate) fn parent_checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.authority.clone()
    }

    #[cfg(test)]
    pub(crate) fn for_tests(
        authority: Arc<dyn MapCheckpointAuthority>,
        deadline: tokio::time::Instant,
    ) -> Arc<Self> {
        Arc::new(Self::bound(authority, deadline))
    }
}

impl PipelineNodeRuntimes {
    pub(crate) fn with_map(mut self, binding: Arc<MapCompilerBinding>) -> Self {
        self.map = Some(binding);
        self
    }
}

impl PipelineDefinition {
    pub(crate) fn has_map_nodes(&self) -> bool {
        self.nodes
            .iter()
            .any(|node| matches!(node, PipelineNodeDefinition::Map(_)))
    }

    pub(crate) fn map_terminal_json_keys(&self) -> BTreeSet<String> {
        self.nodes
            .iter()
            .filter_map(|node| match node {
                PipelineNodeDefinition::Map(node) if node.transition() == Some("END") => {
                    Some(node.runtime().destination.clone())
                }
                _ => None,
            })
            .collect()
    }

    /// This first cohort has no external dispatch, dynamic pause or authorization decision.
    pub(crate) fn map_nonpausing_effectfree(&self) -> bool {
        self.interrupt_before.is_empty()
            && self.interrupt_after.is_empty()
            && self.nodes.iter().all(|node| {
                matches!(
                    node,
                    PipelineNodeDefinition::StateModifier(_) | PipelineNodeDefinition::Router(_)
                )
            })
    }

    pub(super) fn bind_map_authority(
        &self,
        checkpointer: Arc<dyn Checkpointer>,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<(Arc<dyn Checkpointer>, PipelineNodeRuntimes), PipelineConfigurationError> {
        if !self.has_map_nodes() {
            return Ok((checkpointer, runtimes.clone()));
        }
        if self.has_parallel_nodes() {
            return Err(PipelineConfigurationError::Unsupported(
                "Map and fixed Parallel require separately accepted overlay composition",
            ));
        }
        let binding = runtimes
            .map
            .as_ref()
            .ok_or(PipelineConfigurationError::Unsupported(
                "the Map atomic checkpoint authority is not bound",
            ))?;
        if !Arc::ptr_eq(&checkpointer, &binding.parent_checkpointer()) {
            return Err(PipelineConfigurationError::Invalid(
                "Map parent and item authority do not share the original checkpointer",
            ));
        }
        let authority = match runtimes.application_scopes() {
            Some(scopes) => scopes
                .wrap_map_authority(
                    &runtimes.application_scope_path,
                    Arc::clone(&binding.authority),
                )
                .map_err(|_| {
                    PipelineConfigurationError::Invalid(
                        "the original Map application scope is not admitted",
                    )
                })?,
            None => Arc::clone(&binding.authority),
        };
        let bound = Arc::new(MapCompilerBinding::bound(authority, binding.deadline));
        let mut runtimes = runtimes.clone();
        let parent: Arc<dyn Checkpointer> = bound.occurrence.clone();
        runtimes.map = Some(bound);
        runtimes.map_authority_bound = true;
        Ok((parent, runtimes))
    }

    pub(super) fn bind_map_node(
        &self,
        mut builder: PipelineGraphBuilder,
        definition: &MapNodeDefinition,
        runtimes: &PipelineNodeRuntimes,
    ) -> Result<PipelineGraphBuilder, PipelineConfigurationError> {
        let binding = runtimes
            .map
            .as_ref()
            .filter(|_| runtimes.map_authority_bound)
            .ok_or(PipelineConfigurationError::Unsupported(
                "the Map authority was not composed",
            ))?;
        let worker = self
            .nodes
            .iter()
            .find(|node| node.id() == definition.runtime().worker)
            .ok_or(PipelineConfigurationError::Invalid(
                "the Map owned worker is missing",
            ))?;
        if let PipelineNodeDefinition::Application(node) = worker {
            node.validate_map_fixed_inputs()
                .map_err(PipelineConfigurationError::Graph)?;
        }
        let worker = worker.clone();
        let worker_digest = match &worker {
            PipelineNodeDefinition::StateModifier(node) => node.config_digest(),
            PipelineNodeDefinition::Application(node) => {
                let participant = runtimes.application.as_ref()
                    .ok_or(PipelineConfigurationError::Unsupported("the Map saved Agent resolver is unavailable"))?
                    .map_worker_definition_digest(node.id())
                    .map_err(|_| PipelineConfigurationError::Unsupported("the Map saved Agent identity or nonpausing effect-free cohort is unproved"))?;
                if participant == [0; 32] {
                    return Err(PipelineConfigurationError::Unsupported(
                        "the Map saved participant fingerprint is missing",
                    ));
                }
                let mut hash = digest::Context::new(&digest::SHA256);
                hash.update(b"elitea.graph.map.owned-agent.v1\0");
                hash.update(&node.config_digest());
                hash.update(&participant);
                let mut result = [0; 32];
                result.copy_from_slice(hash.finish().as_ref());
                result
            }
            _ => {
                return Err(PipelineConfigurationError::Unsupported(
                    "the Map owned worker kind is not admitted",
                ));
            }
        };
        let factory = Arc::new(CompilerMapWorker {
            worker,
            worker_digest,
            state_types: self.state.clone(),
            declaration_order: self.state_declaration_order.clone(),
            resolver: runtimes.application.clone(),
            events: runtimes.events.clone(),
        });
        let node = DurableMapNode::new(
            definition.runtime().clone(),
            Arc::clone(&binding.occurrence),
            binding.authority.clone(),
            factory,
        )
        .map_err(PipelineConfigurationError::Graph)?
        .with_deadline(binding.deadline);
        builder = builder.node(node);
        if let Some(target) = definition.transition() {
            builder = builder.edge(definition.id(), if target == "END" { END } else { target });
        }
        Ok(builder)
    }
}

pub(super) fn validate_map_ownership(
    nodes: &[PipelineNodeDefinition],
    state: &BTreeMap<String, String>,
    entry: &str,
    before: &[String],
    after: &[String],
    parallel_owned: &BTreeSet<String>,
) -> Result<BTreeSet<String>, PipelineConfigurationError> {
    let mut owned = BTreeSet::new();
    for definition in nodes.iter().filter_map(|node| match node {
        PipelineNodeDefinition::Map(node) => Some(node),
        _ => None,
    }) {
        let map = definition.runtime();
        if state.contains_key(&map.item)
            || state.contains_key(&map.index)
            || builtin_state_key(&map.item)
            || builtin_state_key(&map.index)
            || map
                .broadcast
                .iter()
                .any(|key| !state.contains_key(key) || builtin_state_key(key))
            || map
                .outputs
                .iter()
                .any(|key| !state.contains_key(key) || builtin_state_key(key))
        {
            return Err(PipelineConfigurationError::Invalid(
                "Map item channels are local; broadcast and selected outputs require declared business roots",
            ));
        }
        let worker = nodes.iter().find(|node| node.id() == map.worker).ok_or(
            PipelineConfigurationError::Invalid("the Map worker does not name a declared node"),
        )?;
        if !matches!(
            worker,
            PipelineNodeDefinition::StateModifier(_) | PipelineNodeDefinition::Application(_)
        ) || !worker.route_targets().is_empty()
            || worker.id() == entry
            || parallel_owned.contains(worker.id())
            || !owned.insert(worker.id().to_owned())
            || before.iter().chain(after).any(|id| id == worker.id())
            || map
                .outputs
                .iter()
                .any(|key| !worker.output_keys().contains(key))
        {
            return Err(PipelineConfigurationError::Invalid(
                "the Map owned worker has conflicting topology or outputs",
            ));
        }
        let allowed = map
            .broadcast
            .iter()
            .chain([&map.item, &map.index])
            .collect::<BTreeSet<_>>();
        if worker.input_keys().iter().any(|key| !allowed.contains(key))
            || worker
                .cleaned_keys()
                .iter()
                .any(|key| !worker.output_keys().contains(key))
        {
            return Err(PipelineConfigurationError::Invalid(
                "the Map worker can read only frozen item, index and broadcast channels",
            ));
        }
        let mut child = state.clone();
        // These markers enable existing declaration checks only. They do not type actual items.
        child.insert(map.item.clone(), "__map_opaque_json".to_owned());
        child.insert(map.index.clone(), "int".to_owned());
        validate_node_state(worker, &child)?;
        if let PipelineNodeDefinition::Application(node) = worker {
            // Guard fixed JSON before the complete pipeline hashes this worker definition.
            node.validate_map_fixed_inputs()
                .map_err(PipelineConfigurationError::Graph)?;
            if node
                .mapped_variables()
                .any(|key| !allowed.iter().any(|allowed| allowed.as_str() == key))
            {
                return Err(PipelineConfigurationError::Invalid(
                    "the Map Agent mapping reads outside its frozen item input",
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
            "a parent route cannot target a Map owned worker",
        ));
    }
    Ok(owned)
}

struct CompilerMapWorker {
    worker: PipelineNodeDefinition,
    worker_digest: [u8; 32],
    state_types: BTreeMap<String, String>,
    declaration_order: Vec<String>,
    resolver: Option<Arc<dyn PipelineApplicationResolver>>,
    events: Option<PipelineNodeEventSender>,
}

impl MapWorkerGraphFactory for CompilerMapWorker {
    fn validate_nonpausing(&self, definition: &MapDefinition) -> Result<(), GraphError> {
        if self.worker.id() != definition.worker
            || self.worker_digest == [0; 32]
            || !self.worker.route_targets().is_empty()
        {
            return Err(map_error("invalid_worker"));
        }
        Ok(())
    }
    fn owned_definition_digest(&self) -> [u8; 32] {
        self.worker_digest
    }
    fn worker_kind(&self) -> MapWorkerKind {
        match &self.worker {
            PipelineNodeDefinition::Application(_) => MapWorkerKind::Application,
            _ => MapWorkerKind::StateModifier,
        }
    }
    fn compile_item(
        &self,
        definition: &MapDefinition,
        execution: &MapItemExecution,
    ) -> Result<CompiledGraph, GraphError> {
        self.validate_nonpausing(definition)?;
        let mut channels = BTreeSet::from([
            definition.item.clone(),
            definition.index.clone(),
            "messages".to_owned(),
            "_pipeline_blocked".to_owned(),
            PARALLEL_AGENT_INPUTS_STATE_KEY.to_owned(),
            PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.to_owned(),
        ]);
        channels.extend(definition.broadcast.iter().cloned());
        channels.extend(self.worker.input_keys().iter().cloned());
        channels.extend(self.worker.output_keys().iter().cloned());
        if let PipelineNodeDefinition::Application(node) = &self.worker {
            channels.extend(node.variable_channels());
            channels.extend([
                crate::agents::graph::application::APPLICATION_TASK_STATE_KEY.to_owned(),
                crate::agents::graph::application::APPLICATION_MESSAGES_STATE_KEY.to_owned(),
                crate::agents::graph::application::APPLICATION_RESULT_STATE_KEY.to_owned(),
            ]);
        }
        let mut schema = StateSchema::new();
        for key in channels {
            let default =
                self.state_types
                    .get(&key)
                    .map_or(Value::Null, |kind| match kind.as_str() {
                        "str" => Value::String(String::new()),
                        "list" => serde_json::json!([]),
                        "dict" => serde_json::json!({}),
                        "int" | "float" => serde_json::json!(0),
                        "bool" => Value::Bool(false),
                        _ => Value::Null,
                    });
            schema
                .channels
                .insert(key.clone(), Channel::new(&key).with_default(default));
        }
        let graph = StateGraph::new(schema);
        let graph = match &self.worker {
            PipelineNodeDefinition::StateModifier(node) => {
                graph.add_node(execution.wrap_node(StateModifierNode::new(node.clone())))
            }
            PipelineNodeDefinition::Application(node) => {
                let resolver = self
                    .resolver
                    .as_ref()
                    .ok_or_else(|| map_error("invalid_worker"))?;
                let resolver = match self.events.as_ref() {
                    Some(events) => resolver
                        .for_map_events(
                            events
                                .for_map_item(execution)
                                .map_err(|_| map_error("invalid_item_scope"))?,
                        )
                        .map_err(|_| map_error("invalid_item_scope"))?,
                    None => Arc::clone(resolver),
                };
                // Existing exact task and variable mappings own the opaque item boundary.
                let node = ApplicationNode::new(
                    node.clone(),
                    self.state_types.clone(),
                    resolver.as_ref(),
                    execution.checkpointer(),
                )
                .map_err(|_| map_error("invalid_worker"))?
                .with_parallel_inputs();
                graph.add_node(execution.wrap_node(node))
            }
            _ => return Err(map_error("invalid_worker")),
        };
        graph
            .add_edge(START, self.worker.id())
            .add_edge(self.worker.id(), END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(execution.checkpointer())
                    .with_max_concurrency(1)
                    .with_strict_channels()
            })
    }
    fn runtime_input(
        &self,
        input: &State,
        _: &adk_rust::graph::NodeContext,
        _: &str,
    ) -> Result<State, GraphError> {
        validate_state_boundary(input, MAX_CHECKPOINT_BYTES)?;
        let mut result = input.clone();
        if let PipelineNodeDefinition::Application(node) = &self.worker {
            result.extend(node.freeze_parallel_input(input)?);
        }
        Ok(result)
    }
    fn project_result(
        &self,
        definition: &MapDefinition,
        state: &State,
    ) -> Result<MapWorkerTerminal, GraphError> {
        validate_state_boundary(state, MAX_CHECKPOINT_BYTES)?;
        if state.get("_pipeline_blocked").and_then(Value::as_bool) == Some(true) {
            return Ok(MapWorkerTerminal::Blocked);
        }
        let mut outputs = serde_json::Map::new();
        for key in &definition.outputs {
            let value = state.get(key).ok_or_else(|| map_error("invalid_result"))?;
            if !self
                .state_types
                .get(key)
                .is_some_and(|kind| state_value_matches(kind, value))
            {
                return Err(map_error("invalid_result"));
            }
            outputs.insert(key.clone(), value.clone());
        }
        Ok(MapWorkerTerminal::Completed(outputs))
    }
    fn validate_candidate(&self, candidate: &State) -> Result<(), GraphError> {
        validate_state_boundary(candidate, MAX_CHECKPOINT_BYTES)?;
        for key in &self.declaration_order {
            if !candidate.get(key).is_some_and(|value| {
                self.state_types
                    .get(key)
                    .is_some_and(|kind| state_value_matches(kind, value))
            }) {
                return Err(map_error("invalid_candidate"));
            }
        }
        Ok(())
    }
}

#[cfg(test)]
#[path = "map_compiler_tests.rs"]
mod tests;
