//! Strict Map YAML. Item values have no inferred global state type.
//!
//! Two shapes are read. The v1 shape names `reduction: ordered_collection`
//! and keeps its meaning: stop on the first failed item, ordered collection.
//! It serializes, and therefore digests, as it always has. The v2 shape names
//! `on_error` and `join` like a Parallel node (`continue` and `append` by
//! default) and has no `reduction`.

use adk_rust::graph::GraphError;
use elitea_agent_runtime::graph::fanout::plan::{JoinMode, OnError};
use ring::digest;
use serde::Deserialize;

use super::compiler::PIPELINE_YAML_BUDGET;
use super::map_reduce::{MapDefinition, MapPolicy, MapReduction};
use crate::bounded_yaml;

/// The readable refusal of a Map node that names both `reduction` and `join`.
pub(crate) const MAP_REDUCTION_AND_JOIN: &str = "a Map node sets both `reduction` and `join`; remove `reduction`, which `join: append` replaces";

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
    /// v1 only: `ordered_collection`.
    #[serde(default)]
    reduction: Option<MapReduction>,
    /// `continue` (default in the v2 shape) or `stop`.
    #[serde(default)]
    on_error: Option<String>,
    /// `append` (default), `keyed` or `merge`. Not with `reduction`.
    #[serde(default)]
    join: Option<String>,
    #[serde(default)]
    transition: Option<String>,
}

/// Why a Map node's YAML was refused.
#[derive(Debug, Eq, PartialEq)]
pub(crate) enum MapYamlError {
    /// Malformed, out of bounds or otherwise invalid.
    Invalid,
    /// Both `reduction` and `join`: see [`MAP_REDUCTION_AND_JOIN`].
    ReductionAndJoin,
}

#[derive(Clone)]
pub(crate) struct MapNodeDefinition {
    runtime: MapDefinition,
    output: Vec<String>,
    transition: Option<String>,
    digest: [u8; 32],
}

impl MapNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, MapYamlError> {
        if yaml.is_empty() || yaml.len() > 64 * 1024 {
            return Err(MapYamlError::Invalid);
        }
        let raw: RawMapNodeDefinition =
            bounded_yaml::from_str_as_yaml_error(yaml, PIPELINE_YAML_BUDGET)
                .map_err(|_| MapYamlError::Invalid)?;
        if raw.node_type != "map"
            || raw
                .transition
                .as_deref()
                .is_some_and(|target| target != "END" && !super::yaml::valid_graph_id(target))
        {
            return Err(MapYamlError::Invalid);
        }
        let policy = policy(raw.reduction, raw.on_error.as_deref(), raw.join.as_deref())?;
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
            policy,
        };
        runtime
            .validate()
            .map_err(|_: GraphError| MapYamlError::Invalid)?;
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(b"elitea.graph.map.yaml.v1\0");
        context.update(&serde_json::to_vec(&runtime).map_err(|_| MapYamlError::Invalid)?);
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

/// The v1 shape keeps no policy (its meaning is fixed); the v2 shape always
/// records one. `reduction` with an explicit `on_error` is v2 with `append`.
fn policy(
    reduction: Option<MapReduction>,
    on_error: Option<&str>,
    join: Option<&str>,
) -> Result<Option<MapPolicy>, MapYamlError> {
    if reduction.is_some() && join.is_some() {
        return Err(MapYamlError::ReductionAndJoin);
    }
    if reduction.is_some() && on_error.is_none() {
        return Ok(None);
    }
    let on_error = OnError::parse(on_error).map_err(|_| MapYamlError::Invalid)?;
    let join = JoinMode::parse(join).map_err(|_| MapYamlError::Invalid)?;
    Ok(Some(MapPolicy {
        on_error: on_error.into(),
        join: join.into(),
    }))
}

#[cfg(test)]
#[path = "map_yaml_tests.rs"]
mod tests;
