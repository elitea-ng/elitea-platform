//! The Map definition, its limits and its frozen-item digests. A Map runs on
//! the shared fan-out adapter (`fanout_source.rs`).

use std::collections::{BTreeMap, BTreeSet};
use std::io::{self, Write};

use adk_rust::graph::{GraphError, State};
use elitea_agent_runtime::graph::fanout::plan::{JoinMode, OnError};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

pub(crate) const MAX_ITEMS: usize = 64;
const _: () = assert!(MAX_ITEMS == super::fanout_budget::MAX_FANOUT_CHILDREN);
pub(crate) const MAX_CONCURRENCY: usize = 8;
pub(crate) const MAX_ITEM_BYTES: usize = 512 * 1024;
pub(crate) const MAX_PLAN_BYTES: usize = 2 * 1024 * 1024;
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
    /// The v1 shape: ordered collection, stop on the first failed item. The
    /// parent schema owns its reducer. Absent in the v2 shape.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) reduction: Option<MapReduction>,
    /// The v2 shape's On Error and Join. Absent in a v1 document, so a v1
    /// definition serializes, and therefore digests, as it always has.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) policy: Option<MapPolicy>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum MapReduction {
    OrderedCollection,
}

/// On Error and Join of a v2 Map node, as the shared fan-out plan reads them.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct MapPolicy {
    pub(crate) on_error: MapOnError,
    pub(crate) join: MapJoin,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum MapOnError {
    Continue,
    Stop,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum MapJoin {
    Append,
    Keyed,
    Merge,
}

impl From<OnError> for MapOnError {
    fn from(value: OnError) -> Self {
        match value {
            OnError::Continue => Self::Continue,
            OnError::Stop => Self::Stop,
        }
    }
}

impl From<JoinMode> for MapJoin {
    fn from(value: JoinMode) -> Self {
        match value {
            JoinMode::Append => Self::Append,
            JoinMode::Keyed => Self::Keyed,
            JoinMode::Merge => Self::Merge,
        }
    }
}

impl MapDefinition {
    /// The node's On Error. A v1 document stops on the first failed item.
    pub(crate) fn on_error(&self) -> OnError {
        match self.policy.map(|policy| policy.on_error) {
            None | Some(MapOnError::Stop) => OnError::Stop,
            Some(MapOnError::Continue) => OnError::Continue,
        }
    }

    /// How the joined collection lands. A v1 document's ordered collection
    /// is `append`.
    pub(crate) fn join(&self) -> JoinMode {
        match self.policy.map(|policy| policy.join) {
            None | Some(MapJoin::Append) => JoinMode::Append,
            Some(MapJoin::Keyed) => JoinMode::Keyed,
            Some(MapJoin::Merge) => JoinMode::Merge,
        }
    }

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

    /// The Map's own configuration digest, bound into every item identity.
    pub(crate) fn config_digest(&self) -> Result<[u8; 32], GraphError> {
        hash_json(b"elitea.graph.map.config.v1\0", self, MAX_PLAN_BYTES)
    }

    /// How many items the source holds, refused over `max_items`; no walk of
    /// the items themselves (`source_items` validates them).
    pub(super) fn item_count(&self, state: &State) -> Result<usize, GraphError> {
        let count = state
            .get(&self.source)
            .and_then(Value::as_array)
            .ok_or_else(|| map_error("invalid_source"))?
            .len();
        if count > self.max_items {
            return Err(map_error("resource_exhausted"));
        }
        Ok(count)
    }

    /// The activation's source items, bounded and validated.
    pub(super) fn source_items<'a>(&self, state: &'a State) -> Result<&'a [Value], GraphError> {
        let items = state
            .get(&self.source)
            .and_then(Value::as_array)
            .ok_or_else(|| map_error("invalid_source"))?;
        if items.len() > self.max_items {
            return Err(map_error("resource_exhausted"));
        }
        validate_values(items.iter())?;
        bounded(items, MAX_PLAN_BYTES)?;
        Ok(items)
    }

    /// Every item's frozen input (the broadcast plus `{item, index}`) and its
    /// digest, in source order.
    pub(super) fn item_inputs(
        &self,
        state: &State,
        items: &[Value],
    ) -> Result<Vec<FrozenMapItem>, GraphError> {
        let mut broadcast = State::new();
        for key in &self.broadcast {
            let value = state.get(key).ok_or_else(|| map_error("invalid_mapping"))?;
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
        Ok(frozen)
    }
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct FrozenMapItem {
    pub(crate) index: usize,
    pub(crate) input_digest: [u8; 32],
    pub(crate) input: State,
}

fn has_duplicates(values: &[String]) -> bool {
    let mut seen = BTreeSet::new();
    values.iter().any(|value| !seen.insert(value))
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

/// The digest of a Map's frozen source items.
pub(super) fn source_digest(items: &[Value]) -> Result<[u8; 32], GraphError> {
    hash_json(b"elitea.graph.map.source.v1\0", items, MAX_PLAN_BYTES)
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

pub(super) fn validate_state_structure(state: &State) -> Result<(), GraphError> {
    validate_values_with_limits(state.values(), 64, 65_536)
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
