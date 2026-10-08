//! Bounded `split_out` pipeline node: one envelope row per list element.
//!
//! The binding contract is `docs/split-out-aggregate-contract.md` (section 3).
//! The node is a pure function of its source value and configuration.

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::{Map, Value};

use super::data_shaping::{
    Budget, Pointer, RawLimits, RawRetain, RetainFailure, RetainMode, RetainSpec, ShapingCode,
    ShapingConfigurationError, ShapingError, ShapingLimits, copy_digest, digest_field,
    envelope_row, parse_node_yaml, valid_name,
};
use super::yaml::{valid_graph_id, valid_output_key};

const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.split_out.config.v1\0";

const fn default_true() -> bool {
    true
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum SplitMode {
    List,
    RowField,
    RowsField,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum ListPolicy {
    #[default]
    Error,
    Empty,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawSplit {
    mode: SplitMode,
    #[serde(default)]
    path: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawSplitOutNodeDefinition {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    source: String,
    output: Vec<String>,
    split: RawSplit,
    destination: String,
    #[serde(default)]
    retain: Option<RawRetain>,
    #[serde(default)]
    missing_list: ListPolicy,
    #[serde(default)]
    null_list: ListPolicy,
    #[serde(default = "default_true")]
    remove_source: bool,
    #[serde(default)]
    limits: Option<RawLimits>,
    #[serde(default)]
    transition: Option<String>,
}

/// Strict, authority-free YAML definition for one split-out node.
#[derive(Clone, Debug)]
pub(super) struct SplitOutNodeDefinition {
    id: String,
    source: Vec<String>,
    output: Vec<String>,
    mode: SplitMode,
    path: Option<Pointer>,
    destination: String,
    retain: RetainSpec,
    missing_list: ListPolicy,
    null_list: ListPolicy,
    remove_source: bool,
    limits: ShapingLimits,
    transition: Option<String>,
}

impl SplitOutNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, ShapingConfigurationError> {
        Self::from_raw(parse_node_yaml::<RawSplitOutNodeDefinition>(yaml)?)
    }

    fn from_raw(raw: RawSplitOutNodeDefinition) -> Result<Self, ShapingConfigurationError> {
        let invalid = ShapingConfigurationError::Invalid;
        if raw.node_type != "split_out" {
            return Err(invalid("the node type must be split_out"));
        }
        if !valid_graph_id(&raw.id) {
            return Err(invalid("the split_out node ID is malformed"));
        }
        let [output] = raw.output.as_slice() else {
            return Err(invalid("split_out output must hold exactly one variable"));
        };
        if !valid_output_key(&raw.source) || !valid_output_key(output) || *output == raw.source {
            return Err(invalid("a split_out state variable is malformed"));
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(invalid("the split_out transition is malformed"));
        }
        let path = match (raw.split.mode, raw.split.path.as_deref()) {
            (SplitMode::List, None) => None,
            (SplitMode::List, Some(_)) => {
                return Err(invalid("split.path is not allowed for the list mode"));
            }
            (_, None) => return Err(invalid("split.path is required")),
            (_, Some(text)) => Some(Pointer::parse(text)?),
        };
        if !valid_name(&raw.destination) {
            return Err(invalid("the split_out destination is malformed"));
        }
        let retain = raw
            .retain
            .map_or(Ok(RetainSpec::None), RetainSpec::from_raw)?;
        if raw.split.mode == SplitMode::List && retain.mode() != RetainMode::None {
            return Err(invalid("the list mode only allows retain none"));
        }
        if retain.outputs().any(|name| name == raw.destination) {
            return Err(invalid("a retained output equals the destination"));
        }
        let limits = ShapingLimits::from_raw(raw.limits.as_ref(), false)?;
        let [output] = <[String; 1]>::try_from(raw.output)
            .map_err(|_| invalid("split_out output must hold exactly one variable"))?;
        Ok(Self {
            id: raw.id,
            source: vec![raw.source],
            output: vec![output],
            mode: raw.split.mode,
            path,
            destination: raw.destination,
            retain,
            missing_list: raw.missing_list,
            null_list: raw.null_list,
            remove_source: raw.remove_source,
            limits,
            transition: raw.transition,
        })
    }

    pub(super) fn id(&self) -> &str {
        &self.id
    }

    pub(super) fn input_keys(&self) -> &[String] {
        &self.source
    }

    pub(super) fn output_keys(&self) -> &[String] {
        &self.output
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) const fn source_state_type(&self) -> &'static str {
        match self.mode {
            SplitMode::RowField => "dict",
            SplitMode::List | SplitMode::RowsField => "list",
        }
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        for key in self.source.iter().chain(&self.output) {
            digest_field(&mut context, key.as_bytes());
        }
        context.update(&[match self.mode {
            SplitMode::List => 0,
            SplitMode::RowField => 1,
            SplitMode::RowsField => 2,
        }]);
        match &self.path {
            Some(path) => {
                context.update(&[1]);
                digest_field(&mut context, path.as_str().as_bytes());
            }
            None => context.update(&[0]),
        }
        digest_field(&mut context, self.destination.as_bytes());
        self.retain.digest_into(&mut context);
        context.update(&[
            self.missing_list as u8,
            self.null_list as u8,
            u8::from(self.remove_source),
        ]);
        self.limits.digest_into(&mut context);
        if let Some(target) = &self.transition {
            context.update(&[1]);
            digest_field(&mut context, target.as_bytes());
        } else {
            context.update(&[0]);
            digest_field(&mut context, b"");
        }
        copy_digest(context.finish().as_ref())
    }
}

fn retain_error(failure: RetainFailure, item: u64) -> ShapingError {
    let field = match failure.field {
        Some(index) => format!("retain.fields[{index}]"),
        None => "retain".to_owned(),
    };
    ShapingError::new(failure.code, field).at_item(item)
}

/// Splits `source` into envelope rows. Pure; no row is returned on failure.
pub(super) fn run(
    definition: &SplitOutNodeDefinition,
    source: Option<&Value>,
) -> Result<Value, ShapingError> {
    let invalid_source = || ShapingError::new(ShapingCode::InvalidSource, "source");
    let source = source.ok_or_else(invalid_source)?;
    let parents: &[Value] = match (definition.mode, source) {
        (SplitMode::RowField, Value::Object(_)) => std::slice::from_ref(source),
        (SplitMode::List | SplitMode::RowsField, Value::Array(items)) => {
            if definition.mode == SplitMode::List {
                std::slice::from_ref(source)
            } else {
                items
            }
        }
        _ => return Err(invalid_source()),
    };
    let input_len = match (definition.mode, source) {
        (SplitMode::List, Value::Array(items)) => items.len(),
        _ => parents.len(),
    };
    definition
        .limits
        .check_input(input_len)
        .map_err(ShapingError::limit)?;
    let mut budget = Budget::new(&definition.limits).map_err(ShapingError::limit)?;
    let mut rows = Vec::with_capacity(input_len.min(definition.limits.output_items));
    for (parent_index, parent) in (0_u64..).zip(parents) {
        split_parent(definition, parent, parent_index, &mut budget, &mut rows)?;
    }
    Ok(Value::Array(rows))
}

fn split_parent(
    definition: &SplitOutNodeDefinition,
    parent: &Value,
    parent_index: u64,
    budget: &mut Budget,
    rows: &mut Vec<Value>,
) -> Result<(), ShapingError> {
    let at = |code, field: &str| ShapingError::new(code, field).at_item(parent_index);
    let list = match &definition.path {
        None => parent,
        Some(path) => match path.resolve(parent) {
            // An empty policy emits no rows but still checks the parent.
            None => {
                return match definition.missing_list {
                    ListPolicy::Empty => retained_map(definition, parent, parent_index).map(drop),
                    ListPolicy::Error => Err(at(ShapingCode::MissingField, "split.path")),
                };
            }
            Some(Value::Null) => {
                return match definition.null_list {
                    ListPolicy::Empty => retained_map(definition, parent, parent_index).map(drop),
                    ListPolicy::Error => Err(at(ShapingCode::NullValue, "split.path")),
                };
            }
            Some(value) => value,
        },
    };
    let Value::Array(elements) = list else {
        return Err(at(ShapingCode::TypeMismatch, "split.path"));
    };
    let retained = retained_map(definition, parent, parent_index)?;
    for (position, element) in (0_u64..).zip(elements) {
        let mut data = retained.clone();
        data.insert(definition.destination.clone(), element.clone());
        let row = envelope_row(parent_index, position, data);
        budget.charge_row(&row).map_err(|kind| {
            ShapingError::limit(kind)
                .at_item(parent_index)
                .at_position(position)
        })?;
        rows.push(row);
    }
    Ok(())
}

/// Computed once per parent; checks the destination collision.
fn retained_map(
    definition: &SplitOutNodeDefinition,
    parent: &Value,
    parent_index: u64,
) -> Result<Map<String, Value>, ShapingError> {
    let projected = match (&definition.path, definition.remove_source) {
        (Some(path), true) if definition.retain.mode() != RetainMode::None => {
            let mut copy = parent.clone();
            path.remove_from(&mut copy);
            definition.retain.project(&copy)
        }
        _ => definition.retain.project(parent),
    }
    .map_err(|failure| retain_error(failure, parent_index))?;
    if matches!(
        definition.retain.mode(),
        RetainMode::All | RetainMode::Except
    ) && projected.contains_key(&definition.destination)
    {
        return Err(
            ShapingError::new(ShapingCode::FieldCollision, "destination").at_item(parent_index),
        );
    }
    Ok(projected)
}

/// ADK graph node that emits one update to its single output channel.
pub(super) struct SplitOutNode {
    definition: SplitOutNodeDefinition,
}

impl SplitOutNode {
    pub(super) const fn new(definition: SplitOutNodeDefinition) -> Self {
        Self { definition }
    }
}

#[async_trait]
impl Node for SplitOutNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let source = self
            .definition
            .source
            .first()
            .and_then(|key| context.get(key));
        let value = run(&self.definition, source)
            .map_err(|error| error.into_graph_error(self.definition.id()))?;
        let mut output = NodeOutput::new();
        // `from_raw` admits exactly one output key.
        if let Some(key) = self.definition.output.first() {
            output = output.with_update(key, value);
        }
        Ok(output)
    }
}
