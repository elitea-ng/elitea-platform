//! Bounded, deterministic `aggregate` pipeline node.
//!
//! The binding contract is `docs/split-out-aggregate-contract.md` (§4). Errors
//! name only config field paths and input row indices. A limit hit while
//! charging a final output row carries no item, because output rows are not
//! input rows.

use std::collections::hash_map::Entry as HashEntry;
use std::collections::{BTreeSet, HashMap};
use std::slice;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;
use serde::Deserialize;
use serde_json::{Map, Value};

use super::data_shaping::{
    Budget, CanonicalKey, FieldSelection, MAX_SELECTIONS, MissingPolicy, NullPolicy, Pointer,
    RawLimits, RawRetain, RetainMode, RetainSpec, Selected, ShapingCode, ShapingConfigurationError,
    ShapingError, ShapingLimitKind, ShapingLimits, copy_digest, digest_field, exact_i64,
    parse_envelope, parse_node_yaml, valid_name,
};
use super::yaml::{valid_graph_id, valid_output_key};

const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.aggregate.config.v1\0";
static NULL: Value = Value::Null;
static RETAIN_ALL: RetainSpec = RetainSpec::All;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Layout {
    #[default]
    Plain,
    SplitOut,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Regroup {
    #[default]
    None,
    Parent,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum OperationKind {
    CountRows,
    CollectRows,
    Collect,
    SumInt,
    MinInt,
    MaxInt,
    First,
    Last,
}

impl OperationKind {
    const fn tag(self) -> u8 {
        match self {
            Self::CountRows => 0,
            Self::CollectRows => 1,
            Self::Collect => 2,
            Self::SumInt => 3,
            Self::MinInt => 4,
            Self::MaxInt => 5,
            Self::First => 6,
            Self::Last => 7,
        }
    }

    const fn is_integer(self) -> bool {
        matches!(self, Self::SumInt | Self::MinInt | Self::MaxInt)
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawAggregate {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    source: String,
    output: Vec<String>,
    #[serde(default)]
    layout: Layout,
    #[serde(default)]
    regroup: Regroup,
    #[serde(default)]
    group_by: Vec<RawGroupBy>,
    operations: Vec<RawOperation>,
    #[serde(default)]
    limits: Option<RawLimits>,
    #[serde(default)]
    transition: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawGroupBy {
    path: String,
    output: String,
    #[serde(default)]
    missing: MissingPolicy,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawOperation {
    operation: OperationKind,
    output: String,
    #[serde(default)]
    field: Option<RawField>,
    #[serde(default)]
    retain: Option<RawRetain>,
    #[serde(default)]
    merge_lists: Option<bool>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawField {
    path: String,
    #[serde(default)]
    missing: MissingPolicy,
    #[serde(default)]
    null: Option<NullPolicy>,
}

#[derive(Clone)]
struct GroupBy {
    pointer: Pointer,
    output: String,
    missing: MissingPolicy,
}

#[derive(Clone)]
struct Operation {
    kind: OperationKind,
    output: String,
    field: Option<FieldSelection>,
    /// The `null` policy as authored, for the digest only.
    authored_null: Option<NullPolicy>,
    /// `None` when `retain` was not authored (`all` applies).
    retain: Option<RetainSpec>,
    merge_lists: Option<bool>,
}

/// Strict, authority-free YAML definition of one `aggregate` node.
#[derive(Clone)]
pub(super) struct AggregateNodeDefinition {
    id: String,
    source: String,
    output: String,
    layout: Layout,
    regroup: Regroup,
    group_by: Vec<GroupBy>,
    operations: Vec<Operation>,
    /// First tokens of every operation field (used by `regroup: parent`).
    consumed: BTreeSet<String>,
    limits: ShapingLimits,
    transition: Option<String>,
}

impl AggregateNodeDefinition {
    pub(super) fn from_yaml(yaml: &str) -> Result<Self, ShapingConfigurationError> {
        Self::from_raw(parse_node_yaml(yaml)?)
    }

    fn from_raw(raw: RawAggregate) -> Result<Self, ShapingConfigurationError> {
        use ShapingConfigurationError::{Invalid, ResourceExhausted};
        if raw.node_type != "aggregate" {
            return Err(Invalid("the node type must be aggregate"));
        }
        if !valid_graph_id(&raw.id) {
            return Err(Invalid("the aggregate node ID is malformed"));
        }
        let [output] = <[String; 1]>::try_from(raw.output)
            .map_err(|_| Invalid("an aggregate needs exactly one output"))?;
        if !valid_output_key(&raw.source) || !valid_output_key(&output) || output == raw.source {
            return Err(Invalid("the aggregate source or output is malformed"));
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(Invalid("the aggregate transition is malformed"));
        }
        if raw.group_by.len() > MAX_SELECTIONS || raw.operations.len() > MAX_SELECTIONS {
            return Err(ResourceExhausted);
        }
        if raw.operations.is_empty() {
            return Err(Invalid("an aggregate needs at least one operation"));
        }
        if raw.regroup == Regroup::Parent
            && (raw.layout != Layout::SplitOut
                || !raw.group_by.is_empty()
                || raw
                    .operations
                    .iter()
                    .any(|operation| operation.operation == OperationKind::CollectRows))
        {
            return Err(Invalid(
                "regroup parent needs layout split_out, no group_by and no collect_rows",
            ));
        }

        let mut names = BTreeSet::new();
        let mut unique = |name: &str| valid_name(name) && names.insert(name.to_owned());
        let mut group_by = Vec::with_capacity(raw.group_by.len());
        for entry in raw.group_by {
            if entry.missing == MissingPolicy::Skip || !unique(&entry.output) {
                return Err(Invalid("a group_by entry is malformed"));
            }
            group_by.push(GroupBy {
                pointer: Pointer::parse(&entry.path)?,
                output: entry.output,
                missing: entry.missing,
            });
        }
        let mut operations = Vec::with_capacity(raw.operations.len());
        for operation in raw.operations {
            if !unique(&operation.output) {
                return Err(Invalid("aggregate output names must be unique and valid"));
            }
            operations.push(Operation::from_raw(operation)?);
        }
        let consumed = operations
            .iter()
            .filter_map(|operation| operation.field.as_ref())
            .map(|field| field.pointer.first_token().to_owned())
            .collect();

        Ok(Self {
            id: raw.id,
            source: raw.source,
            output,
            layout: raw.layout,
            regroup: raw.regroup,
            group_by,
            operations,
            consumed,
            limits: ShapingLimits::from_raw(raw.limits.as_ref(), true)?,
            transition: raw.transition,
        })
    }

    pub(super) fn id(&self) -> &str {
        &self.id
    }

    pub(super) fn input_keys(&self) -> &[String] {
        slice::from_ref(&self.source)
    }

    pub(super) fn output_keys(&self) -> &[String] {
        slice::from_ref(&self.output)
    }

    pub(super) fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    pub(super) fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.source.as_bytes());
        digest_field(&mut context, self.output.as_bytes());
        context.update(&[self.layout as u8, self.regroup as u8]);
        context.update(&(self.group_by.len() as u64).to_be_bytes());
        for entry in &self.group_by {
            digest_field(&mut context, entry.pointer.as_str().as_bytes());
            digest_field(&mut context, entry.output.as_bytes());
            context.update(&[entry.missing.tag()]);
        }
        context.update(&(self.operations.len() as u64).to_be_bytes());
        for operation in &self.operations {
            operation.digest_into(&mut context);
        }
        self.limits.digest_into(&mut context);
        match &self.transition {
            Some(target) => {
                context.update(&[1]);
                digest_field(&mut context, target.as_bytes());
            }
            None => context.update(&[0]),
        }
        copy_digest(context.finish().as_ref())
    }
}

impl Operation {
    fn from_raw(raw: RawOperation) -> Result<Self, ShapingConfigurationError> {
        use ShapingConfigurationError::Invalid;
        let kind = raw.operation;
        let takes_field = !matches!(kind, OperationKind::CountRows | OperationKind::CollectRows);
        if takes_field != raw.field.is_some()
            || (raw.retain.is_some() && kind != OperationKind::CollectRows)
            || (raw.merge_lists.is_some() && kind != OperationKind::Collect)
        {
            return Err(Invalid(
                "an aggregate operation has missing or foreign keys",
            ));
        }
        let retain = raw.retain.map(RetainSpec::from_raw).transpose()?;
        if retain
            .as_ref()
            .is_some_and(|retain| retain.mode() == RetainMode::None)
        {
            return Err(Invalid("collect_rows cannot retain none"));
        }
        let mut authored_null = None;
        let field = match raw.field {
            Some(field) => {
                authored_null = field.null;
                let null = match (field.null, kind.is_integer()) {
                    (Some(NullPolicy::Keep), true) => {
                        return Err(Invalid("integer operations cannot keep null"));
                    }
                    (Some(policy), _) => policy,
                    (None, true) => NullPolicy::Error,
                    (None, false) => NullPolicy::Keep,
                };
                Some(FieldSelection {
                    pointer: Pointer::parse(&field.path)?,
                    missing: field.missing,
                    null,
                })
            }
            None => None,
        };
        Ok(Self {
            kind,
            output: raw.output,
            field,
            authored_null,
            retain,
            merge_lists: raw.merge_lists,
        })
    }

    fn digest_into(&self, context: &mut digest::Context) {
        context.update(&[self.kind.tag()]);
        digest_field(context, self.output.as_bytes());
        match &self.field {
            Some(field) => {
                context.update(&[1]);
                digest_field(context, field.pointer.as_str().as_bytes());
                context.update(&[field.missing.tag()]);
            }
            None => context.update(&[0]),
        }
        match self.authored_null {
            Some(policy) => context.update(&[1, policy.tag()]),
            None => context.update(&[0]),
        }
        match &self.retain {
            Some(retain) => {
                context.update(&[1]);
                retain.digest_into(context);
            }
            None => context.update(&[0]),
        }
        match self.merge_lists {
            Some(merge) => context.update(&[1, u8::from(merge)]),
            None => context.update(&[0]),
        }
    }

    fn field_error(index: usize, code: ShapingCode) -> ShapingError {
        ShapingError::new(code, format!("operations[{index}].field"))
    }

    /// Selects this operation's field from a row view.
    fn select<'a>(&self, index: usize, view: &'a Value) -> Result<Selected<'a>, ShapingError> {
        match &self.field {
            Some(field) => field
                .select(view)
                .map_err(|code| Self::field_error(index, code)),
            None => Ok(Selected::Skip),
        }
    }

    fn integer(&self, index: usize, view: &Value) -> Result<Option<i64>, ShapingError> {
        match self.select(index, view)? {
            Selected::Skip => Ok(None),
            // `as_i64` succeeds only on plain integer text, with the exact value.
            Selected::Value(Value::Number(number)) => number
                .as_i64()
                .map_or_else(|| exact_i64(number), Ok)
                .map(Some)
                .map_err(|code| Self::field_error(index, code)),
            Selected::Value(_) | Selected::Null => {
                Err(Self::field_error(index, ShapingCode::TypeMismatch))
            }
        }
    }
}

// ---------------------------------------------------------------- runtime

/// Running result of one operation over one group.
enum Accumulator<'a> {
    Count(u64),
    Rows(Vec<Value>),
    Values(Vec<&'a Value>),
    Sum(i64),
    Min(Option<i64>),
    Max(Option<i64>),
    First(Option<&'a Value>),
    Last(Option<&'a Value>),
}

impl<'a> Accumulator<'a> {
    const fn new(kind: OperationKind) -> Self {
        match kind {
            OperationKind::CountRows => Self::Count(0),
            OperationKind::CollectRows => Self::Rows(Vec::new()),
            OperationKind::Collect => Self::Values(Vec::new()),
            OperationKind::SumInt => Self::Sum(0),
            OperationKind::MinInt => Self::Min(None),
            OperationKind::MaxInt => Self::Max(None),
            OperationKind::First => Self::First(None),
            OperationKind::Last => Self::Last(None),
        }
    }

    /// Adds one row view; `collected` is the lower-bound budget.
    fn add(
        &mut self,
        index: usize,
        operation: &Operation,
        view: &'a Value,
        collected: &mut Budget,
    ) -> Result<(), ShapingError> {
        match self {
            Self::Count(count) => *count += 1,
            Self::Rows(rows) => {
                let retain = operation.retain.as_ref().unwrap_or(&RETAIN_ALL);
                let projected = retain.project(view).map_err(|failure| {
                    let field = match failure.field {
                        Some(entry) => format!("operations[{index}].retain.fields[{entry}]"),
                        None => format!("operations[{index}].retain"),
                    };
                    ShapingError::new(failure.code, field)
                })?;
                let projected = Value::Object(projected);
                collected
                    .charge_value(&projected)
                    .map_err(ShapingError::limit)?;
                rows.push(projected);
            }
            Self::Values(values) => {
                let selected = match operation.select(index, view)? {
                    Selected::Skip => return Ok(()),
                    Selected::Null => &NULL,
                    Selected::Value(value) => value,
                };
                if operation.merge_lists == Some(true) {
                    let Value::Array(items) = selected else {
                        return Err(Operation::field_error(index, ShapingCode::TypeMismatch));
                    };
                    for item in items {
                        collected.charge_value(item).map_err(ShapingError::limit)?;
                        values.push(item);
                    }
                } else {
                    collected
                        .charge_value(selected)
                        .map_err(ShapingError::limit)?;
                    values.push(selected);
                }
            }
            Self::Sum(sum) => {
                if let Some(value) = operation.integer(index, view)? {
                    *sum = sum.checked_add(value).ok_or_else(|| {
                        Operation::field_error(index, ShapingCode::IntegerOverflow)
                    })?;
                }
            }
            Self::Min(current) => {
                if let Some(value) = operation.integer(index, view)? {
                    *current = Some(current.map_or(value, |current| current.min(value)));
                }
            }
            Self::Max(current) => {
                if let Some(value) = operation.integer(index, view)? {
                    *current = Some(current.map_or(value, |current| current.max(value)));
                }
            }
            Self::First(first) => {
                // Every row still passes the field policies.
                let selected = selected_value(operation.select(index, view)?);
                if first.is_none() {
                    *first = selected;
                }
            }
            Self::Last(last) => {
                if let Some(value) = selected_value(operation.select(index, view)?) {
                    *last = Some(value);
                }
            }
        }
        Ok(())
    }

    fn finish(self) -> Value {
        match self {
            Self::Count(count) => Value::from(count),
            Self::Rows(rows) => Value::Array(rows),
            Self::Values(values) => Value::Array(values.into_iter().cloned().collect()),
            Self::Sum(sum) => Value::from(sum),
            Self::Min(value) | Self::Max(value) => value.map_or(Value::Null, Value::from),
            Self::First(value) | Self::Last(value) => value.cloned().unwrap_or(Value::Null),
        }
    }
}

fn selected_value(selected: Selected<'_>) -> Option<&Value> {
    match selected {
        Selected::Skip => None,
        Selected::Null => Some(&NULL),
        Selected::Value(value) => Some(value),
    }
}

/// One output row under construction.
struct Group<'a> {
    item: usize,
    keys: Vec<&'a Value>,
    accumulators: Vec<Accumulator<'a>>,
}

impl<'a> Group<'a> {
    fn new(definition: &AggregateNodeDefinition, item: usize, keys: Vec<&'a Value>) -> Self {
        Self {
            item,
            keys,
            accumulators: definition
                .operations
                .iter()
                .map(|operation| Accumulator::new(operation.kind))
                .collect(),
        }
    }

    fn add(
        &mut self,
        definition: &AggregateNodeDefinition,
        view: &'a Value,
        collected: &mut Budget,
    ) -> Result<(), ShapingError> {
        for (index, (accumulator, operation)) in self
            .accumulators
            .iter_mut()
            .zip(&definition.operations)
            .enumerate()
        {
            accumulator.add(index, operation, view, collected)?;
        }
        Ok(())
    }

    /// Inserts the operation outputs into `row`.
    fn finish_into(
        self,
        definition: &AggregateNodeDefinition,
        row: &mut Map<String, Value>,
    ) -> Result<(), ShapingError> {
        for (index, (accumulator, operation)) in self
            .accumulators
            .into_iter()
            .zip(&definition.operations)
            .enumerate()
        {
            if row.contains_key(&operation.output) {
                return Err(ShapingError::new(
                    ShapingCode::FieldCollision,
                    format!("operations[{index}].output"),
                ));
            }
            row.insert(operation.output.clone(), accumulator.finish());
        }
        Ok(())
    }
}

/// Exact charge of the emitted list.
struct Emitter {
    budget: Budget,
    rows: Vec<Value>,
}

impl Emitter {
    fn new(limits: &ShapingLimits) -> Result<Self, ShapingError> {
        Ok(Self {
            budget: Budget::new(limits).map_err(ShapingError::limit)?,
            rows: Vec::new(),
        })
    }

    fn push(&mut self, row: Map<String, Value>) -> Result<(), ShapingError> {
        let row = Value::Object(row);
        self.budget.charge_row(&row).map_err(ShapingError::limit)?;
        self.rows.push(row);
        Ok(())
    }
}

/// Pure evaluation of one `aggregate` node over its `source` value.
pub(super) fn run(
    definition: &AggregateNodeDefinition,
    source: Option<&Value>,
) -> Result<Value, ShapingError> {
    let Some(Value::Array(rows)) = source else {
        return Err(ShapingError::new(ShapingCode::InvalidSource, "source"));
    };
    let limits = &definition.limits;
    limits
        .check_input(rows.len())
        .map_err(ShapingError::limit)?;
    let mut collected = Budget::new(limits).map_err(ShapingError::limit)?;
    let rows = match definition.regroup {
        Regroup::None => aggregate(definition, rows, &mut collected)?,
        Regroup::Parent => regroup(definition, rows, &mut collected)?,
    };
    Ok(Value::Array(rows))
}

fn at(item: usize) -> impl Fn(ShapingError) -> ShapingError {
    move |error| error.at_item(item as u64)
}

/// The value pointers resolve against: the row, or the envelope's `data`.
fn row_view(layout: Layout, row: &Value, item: usize) -> Result<&Value, ShapingError> {
    let view = match layout {
        Layout::Plain => row
            .is_object()
            .then_some(row)
            .ok_or(ShapingCode::InvalidRow),
        Layout::SplitOut => envelope(row)
            .map(|envelope| envelope.view)
            .ok_or(ShapingCode::InvalidEnvelope),
    };
    view.map_err(|code| ShapingError::new(code, "source").at_item(item as u64))
}

/// Maps a canonical-key failure: depth is the `limits.depth` budget.
fn key_error(code: ShapingCode, field: &str) -> ShapingError {
    if code == ShapingCode::LimitExceeded {
        ShapingError::limit(ShapingLimitKind::Depth)
    } else {
        ShapingError::new(code, field)
    }
}

/// Checks a newly inserted group (or parent) against `groups` and `output_items`.
fn check_new_group(limits: &ShapingLimits, count: usize) -> Result<(), ShapingError> {
    limits.check_groups(count).map_err(ShapingError::limit)?;
    if count > limits.output_items {
        return Err(ShapingError::limit(ShapingLimitKind::OutputItems));
    }
    Ok(())
}

/// Depth available to a value emitted as a row field (list 1, row 2).
const fn field_depth(limits: &ShapingLimits) -> usize {
    limits.depth.saturating_sub(2)
}

fn group_value<'a>(
    entry: &GroupBy,
    view: &'a Value,
    index: usize,
) -> Result<&'a Value, ShapingError> {
    match (entry.pointer.resolve(view), entry.missing) {
        (Some(value), _) => Ok(value),
        (None, MissingPolicy::Null) => Ok(&NULL),
        (None, _) => Err(ShapingError::new(
            ShapingCode::MissingField,
            format!("group_by[{index}]"),
        )),
    }
}

fn aggregate<'a>(
    definition: &AggregateNodeDefinition,
    rows: &'a [Value],
    collected: &mut Budget,
) -> Result<Vec<Value>, ShapingError> {
    let limits = &definition.limits;
    let max_depth = field_depth(limits);
    let mut slots = HashMap::<CanonicalKey, usize>::new();
    let mut groups = Vec::<Group<'a>>::new();
    if definition.group_by.is_empty() {
        groups.push(Group::new(definition, 0, Vec::new()));
    }
    for (item, row) in rows.iter().enumerate() {
        let view = row_view(definition.layout, row, item)?;
        let slot = if definition.group_by.is_empty() {
            0
        } else {
            let mut key = CanonicalKey::default();
            let mut keys = Vec::with_capacity(definition.group_by.len());
            for (index, entry) in definition.group_by.iter().enumerate() {
                let value = group_value(entry, view, index).map_err(at(item))?;
                key.push(value, max_depth)
                    .map_err(|code| key_error(code, &format!("group_by[{index}]")))
                    .map_err(at(item))?;
                keys.push(value);
            }
            match slots.entry(key) {
                HashEntry::Occupied(slot) => *slot.get(),
                HashEntry::Vacant(slot) => {
                    check_new_group(limits, groups.len() + 1).map_err(at(item))?;
                    groups.push(Group::new(definition, item, keys));
                    *slot.insert(groups.len() - 1)
                }
            }
        };
        if let Some(group) = groups.get_mut(slot) {
            group.add(definition, view, collected).map_err(at(item))?;
        }
    }
    let mut emitter = Emitter::new(limits)?;
    for group in groups {
        let mut row = Map::new();
        for (entry, value) in definition.group_by.iter().zip(&group.keys) {
            row.insert(entry.output.clone(), (*value).clone());
        }
        let item = group.item;
        // Names are unique across group_by and operations, so this never collides.
        group.finish_into(definition, &mut row).map_err(at(item))?;
        emitter.push(row)?;
    }
    Ok(emitter.rows)
}

/// One strict `{parent_index, position, data}` envelope row.
struct Member<'a> {
    parent_index: u64,
    position: u64,
    item: usize,
    view: &'a Value,
    data: &'a Map<String, Value>,
}

/// `parse_envelope` with a fast path: `as_u64` succeeds only on plain digit
/// text, which `exact_u64` accepts with the same value; anything else (`2.0`,
/// `1e1`, …) takes the exact core path.
fn envelope(row: &Value) -> Option<Member<'_>> {
    let object = row.as_object()?;
    let view = object.get("data")?;
    let data = view.as_object()?;
    let index = |key: &str| object.get(key).and_then(Value::as_u64);
    let (parent_index, position) = match (index("parent_index"), index("position")) {
        (Some(parent_index), Some(position)) if object.len() == 3 => (parent_index, position),
        _ => {
            let envelope = parse_envelope(row)?;
            (envelope.parent_index, envelope.position)
        }
    };
    Some(Member {
        parent_index,
        position,
        item: 0,
        view,
        data,
    })
}

fn regroup(
    definition: &AggregateNodeDefinition,
    rows: &[Value],
    collected: &mut Budget,
) -> Result<Vec<Value>, ShapingError> {
    let limits = &definition.limits;
    let mut members = Vec::with_capacity(rows.len());
    for (item, row) in rows.iter().enumerate() {
        let Some(member) = envelope(row) else {
            return Err(
                ShapingError::new(ShapingCode::InvalidEnvelope, "source").at_item(item as u64)
            );
        };
        members.push(Member { item, ..member });
    }
    members.sort_unstable_by_key(|member| (member.parent_index, member.position, member.item));
    let parents = members.chunk_by(|left, right| left.parent_index == right.parent_index);
    if let Err(error) = check_new_group(limits, parents.clone().count()) {
        return Err(first_excess_parent(limits, &members).unwrap_or(error));
    }
    // The reported duplicate is the earliest input row repeating a pair.
    let conflict = members
        .windows(2)
        .filter_map(|pair| match pair {
            [earlier, later]
                if earlier.parent_index == later.parent_index
                    && earlier.position == later.position =>
            {
                Some((later.item, later.position))
            }
            _ => None,
        })
        .min();
    if let Some((item, position)) = conflict {
        return Err(ShapingError::new(ShapingCode::RegroupConflict, "regroup")
            .at_item(item as u64)
            .at_position(position));
    }

    let max_depth = field_depth(limits);
    let mut emitter = Emitter::new(limits)?;
    for members in parents {
        let Some(base) = members.first() else {
            continue;
        };
        // Exact equality implies canonical equality; keys are built only on a mismatch.
        let mut base_key = None;
        let mut group = Group::new(definition, base.item, Vec::new());
        for member in members {
            if member.item != base.item
                && !residual(definition, member.data).eq(residual(definition, base.data))
            {
                let locate = located(member.item, member.position);
                let names_match = residual(definition, member.data)
                    .map(|(name, _)| name)
                    .eq(residual(definition, base.data).map(|(name, _)| name));
                if base_key.is_none() {
                    base_key = Some(
                        residual_key(definition, base.data, max_depth)
                            .map_err(located(base.item, base.position))?,
                    );
                }
                let member_key =
                    residual_key(definition, member.data, max_depth).map_err(&locate)?;
                let consistent = names_match && base_key.as_ref() == Some(&member_key);
                if !consistent {
                    return Err(locate(ShapingError::new(
                        ShapingCode::RegroupInconsistent,
                        "regroup",
                    )));
                }
            }
            group
                .add(definition, member.view, collected)
                .map_err(at(member.item))?;
        }
        let mut row = residual(definition, base.data)
            .map(|(name, value)| (name.clone(), value.clone()))
            .collect::<Map<_, _>>();
        group
            .finish_into(definition, &mut row)
            .map_err(located(base.item, base.position))?;
        emitter.push(row)?;
    }
    Ok(emitter.rows)
}

/// The parent-count failure at the input row that introduced the excess parent.
fn first_excess_parent(limits: &ShapingLimits, members: &[Member<'_>]) -> Option<ShapingError> {
    let mut by_item = members
        .iter()
        .map(|member| (member.item, member.parent_index))
        .collect::<Vec<_>>();
    by_item.sort_unstable();
    let mut seen = BTreeSet::new();
    by_item.into_iter().find_map(|(item, parent_index)| {
        if !seen.insert(parent_index) {
            return None;
        }
        check_new_group(limits, seen.len()).err().map(at(item))
    })
}

fn located(item: usize, position: u64) -> impl Fn(ShapingError) -> ShapingError {
    move |error| error.at_item(item as u64).at_position(position)
}

/// The `data` fields that are not consumed by an operation, in key order.
fn residual<'a>(
    definition: &'a AggregateNodeDefinition,
    data: &'a Map<String, Value>,
) -> impl Iterator<Item = (&'a String, &'a Value)> {
    data.iter()
        .filter(|(name, _)| !definition.consumed.contains(name.as_str()))
}

/// Canonical key of the residual values; callers compare the names separately.
fn residual_key(
    definition: &AggregateNodeDefinition,
    data: &Map<String, Value>,
    max_depth: usize,
) -> Result<CanonicalKey, ShapingError> {
    let mut key = CanonicalKey::default();
    for (_, value) in residual(definition, data) {
        key.push(value, max_depth)
            .map_err(|code| key_error(code, "regroup"))?;
    }
    Ok(key)
}

// ---------------------------------------------------------------- node

/// ADK graph node that writes the aggregate of `source` to `output`.
pub(super) struct AggregateNode {
    definition: AggregateNodeDefinition,
}

impl AggregateNode {
    pub(super) const fn new(definition: AggregateNodeDefinition) -> Self {
        Self { definition }
    }
}

#[async_trait]
impl Node for AggregateNode {
    fn name(&self) -> &str {
        self.definition.id()
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let value = run(&self.definition, context.get(&self.definition.source))
            .map_err(|error| error.into_graph_error(self.definition.id()))?;
        Ok(NodeOutput::new().with_update(&self.definition.output, value))
    }
}
