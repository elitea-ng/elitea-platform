//! Map save-time validation. A Map runs on the shared fan-out adapter
//! (`parallel_compiler.rs` `bind_map_node`); this module checks that its one
//! owned worker and its item channels are well formed.

use std::collections::{BTreeMap, BTreeSet};

use super::{
    FanoutChildRefusal, PipelineConfigurationError, PipelineDefinition, PipelineNodeDefinition,
    builtin_state_key, validate_node_state,
};

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
            PipelineNodeDefinition::StateModifier(_)
                | PipelineNodeDefinition::Application(_)
                | PipelineNodeDefinition::Llm(_)
        ) {
            // Readable and typed so Wave 3 can admit each kind on its own.
            return Err(PipelineConfigurationError::FanoutChildUnsupported {
                node: worker.id().to_owned(),
                refusal: FanoutChildRefusal::Kind,
            });
        }
        if !worker.route_targets().is_empty()
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
        validate_worker_mappings(worker, &allowed)?;
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

/// The worker's mapped inputs read only the item, its index and the
/// broadcasts (its frozen item input): an Agent's and an LLM node's
/// `variable` mappings alike.
fn validate_worker_mappings(
    worker: &PipelineNodeDefinition,
    allowed: &BTreeSet<&String>,
) -> Result<(), PipelineConfigurationError> {
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
    // An LLM worker reads its mapped inputs from the item's frozen state
    // too: a variable outside it would reach the model as an empty value.
    if let PipelineNodeDefinition::Llm(node) = worker
        && node
            .mapped_variables()
            .any(|key| !allowed.iter().any(|allowed| allowed.as_str() == key))
    {
        return Err(PipelineConfigurationError::Invalid(
            "the Map LLM mapping reads outside its frozen item input",
        ));
    }
    Ok(())
}

#[cfg(test)]
#[path = "map_compiler_tests.rs"]
mod tests;
