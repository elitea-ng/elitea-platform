//! Strict Map YAML. Item values have no inferred global state type.

use adk_rust::graph::GraphError;
use ring::digest;
use serde::Deserialize;

use super::compiler::PIPELINE_YAML_BUDGET;
use super::map_reduce::{MapDefinition, MapReduction};
use crate::bounded_yaml;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawMapNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    worker: String,
    source: String,
    item: String,
    index: String,
    #[serde(default)]
    broadcast: Vec<String>,
    outputs: Vec<String>,
    destination: String,
    max_items: usize,
    max_concurrency: usize,
    reduction: MapReduction,
    #[serde(default)]
    transition: Option<String>,
}

#[derive(Clone)]
pub(crate) struct MapNodeDefinition {
    runtime: MapDefinition,
    output: Vec<String>,
    transition: Option<String>,
    digest: [u8; 32],
}

impl MapNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, GraphError> {
        if yaml.is_empty() || yaml.len() > 64 * 1024 {
            return Err(super::map_reduce::map_error("resource_exhausted"));
        }
        let raw: RawMapNodeDefinition =
            bounded_yaml::from_str_as_yaml_error(yaml, PIPELINE_YAML_BUDGET)
                .map_err(|_| super::map_reduce::map_error("invalid_configuration"))?;
        if raw.node_type != "map"
            || raw
                .transition
                .as_deref()
                .is_some_and(|target| target != "END" && !super::yaml::valid_graph_id(target))
        {
            return Err(super::map_reduce::map_error("invalid_configuration"));
        }
        let runtime = MapDefinition {
            id: raw.id,
            worker: raw.worker,
            source: raw.source,
            item: raw.item,
            index: raw.index,
            broadcast: raw.broadcast,
            outputs: raw.outputs,
            destination: raw.destination,
            max_items: raw.max_items,
            max_concurrency: raw.max_concurrency,
            reduction: raw.reduction,
        };
        runtime.validate()?;
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(b"elitea.graph.map.yaml.v1\0");
        context.update(
            &serde_json::to_vec(&runtime)
                .map_err(|_| super::map_reduce::map_error("invalid_configuration"))?,
        );
        context.update(&[0]);
        context.update(raw.transition.as_deref().unwrap_or_default().as_bytes());
        let mut digest = [0; 32];
        digest.copy_from_slice(context.finish().as_ref());
        Ok(Self {
            output: vec![runtime.destination.clone()],
            runtime,
            transition: raw.transition,
            digest,
        })
    }

    pub(crate) fn id(&self) -> &str {
        &self.runtime.id
    }

    pub(super) fn runtime(&self) -> &MapDefinition {
        &self.runtime
    }

    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        self.digest
    }
}
