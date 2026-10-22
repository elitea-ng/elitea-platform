//! Translation of the SDK's filter JSON into `elitea.vector.v1` filters
//! (ADR-0030 decision 5, ADR-0031 decision 4).
//!
//! The SDK filters with langchain-postgres operators over a flat metadata
//! dict. `elitea-vector` takes a Qdrant-shaped filter: `must`, `should` and
//! `must_not` lists of conditions, where a condition is an equality on a
//! keyword, an integer or a boolean, a keyword-in-set, a numeric range, a
//! presence check (`is_empty`), or a nested filter. This module translates
//! what that can express and refuses the rest with a message that names the
//! operator and says why. It never approximates: a refused filter is an
//! error the model or user can read, not a search that silently returns
//! something else.
//!
//! # Where metadata lives
//!
//! A point's user metadata is the JSON object in its `metadata` payload.
//! `elitea-vector` lets a filter reach into it with the dotted key
//! `metadata.<path>`, where every dot-separated part matches
//! `[A-Za-z0-9_]+`. So a filter on the SDK key `author` becomes the
//! condition key `metadata.author`, and `config.timeout` becomes
//! `metadata.config.timeout`. A key that is already written
//! `metadata.<path>` is taken as it is. Six point fields are filterable
//! without the prefix (`document_key`, `document_version`, `parent_id`,
//! `chunk_id`, `chunk_type`, `acl`) because they are real payload keys. They
//! hold text, so an integer compared with one of them (`{"chunk_id": 3}`,
//! the SDK's chunkers number chunks) is matched as its decimal string, in
//! `$eq`, `$in`, `$ne` and `$nin` alike; a boolean or a decimal is refused,
//! since no stored value could equal it. Five
//! keys are the facade's own scope and are refused: `project_id`, `source`,
//! `namespace_id`, `generation` and `text` (so is a `text` match: BM25 over
//! the chunk text is `full_text_search`'s, not a filter's).
//!
//! # Operators
//!
//! | SDK | Translation |
//! | --- | --- |
//! | `$eq`, implicit equality | `keyword`, `integer` or `boolean` match |
//! | `$ne` | the key is present and the match does not hold |
//! | `$in` of strings | one `any` match |
//! | `$in` of integers or booleans | an `or` of matches |
//! | `$nin` | the key is present and the `$in` does not hold |
//! | `$lt` `$lte` `$gt` `$gte` | a `range` on a number (metadata keys only) |
//! | `$between` `[low, high]` | a `range` with both bounds, inclusive |
//! | `$exists` `true` / `false` | not `is_empty` / `is_empty` |
//! | `$and` / several keys | `must` |
//! | `$or` | `should`, nesting what a flat `should` cannot hold |
//! | `$not` | the negation, nested when it is not a single condition |
//! | `$like` `$ilike` | refused: Qdrant has no pattern condition |
//!
//! # Missing keys
//!
//! langchain-postgres compiles a filter to SQL, where a comparison with a
//! missing key is NULL and a NULL `WHERE` is false, also under `NOT`. This
//! module keeps that: every comparison holds only for a point that has the
//! key, so `{"a": {"$ne": 1}}` does not match a point without `a`, and
//! neither does `{"$not": {"a": 1}}`. (The translation tracks, for every
//! sub-filter, both when it is true and when it is false, as SQL's three
//! valued logic does.) "Missing" is Qdrant's `is_empty`: the key absent,
//! `null` or `[]`. A JSON `null` is SQL NULL in langchain-postgres too; an
//! empty array is the one difference (present there, empty here).
//!
//! # Limits
//!
//! A translated filter nests at most [`MAX_DEPTH`] levels, holds at most
//! 64 conditions per list and [`MAX_TOTAL_CONDITIONS`] in all, and an `any`
//! at most 1024 values: `elitea-vector`'s limits. A filter beyond them is
//! refused before the call. An empty filter object is refused anywhere but
//! as the whole filter, so `{"$not": {}}` is an error, never "no filter".

use serde_json::Value;

use crate::pb;

/// The most conditions `elitea-vector` accepts in one list.
pub const MAX_CONDITIONS: usize = 64;
/// The most values `elitea-vector` accepts in one `any` match.
const MAX_ANY_VALUES: usize = 1024;
/// The deepest filter `elitea-vector` accepts, counting the filter itself.
pub const MAX_DEPTH: usize = 8;
/// The most conditions `elitea-vector` accepts in one filter tree.
pub const MAX_TOTAL_CONDITIONS: usize = 1024;

/// Keys that are the facade's own scope. A caller's filter cannot name them.
const RESERVED_KEYS: [&str; 5] = ["project_id", "source", "namespace_id", "generation", "text"];
/// Payload keys a filter may name without the `metadata.` prefix.
const POINT_KEYS: [&str; 6] = [
    "document_key",
    "document_version",
    "parent_id",
    "chunk_id",
    "chunk_type",
    "acl",
];

/// A filter that cannot be translated.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum FilterError {
    /// The filter is not valid JSON, or not an object, or an operator got an
    /// argument of the wrong shape, or a sub-filter is empty.
    #[error("invalid filter: {0}")]
    Invalid(String),
    /// The operator is valid for the SDK but has no `elitea-vector`
    /// equivalent (or is unknown).
    #[error("filter operator {operator} is not supported: {reason}")]
    UnsupportedOperator {
        /// The operator as written, such as `$like`.
        operator: String,
        /// Why it is refused.
        reason: String,
    },
    /// The key is one of the facade's own scope keys.
    #[error(
        "filter key {0:?} is reserved: the vector service sets project_id, source, namespace_id, generation and text itself and no filter may name them"
    )]
    ReservedKey(String),
    /// The key cannot be written as a payload path.
    #[error(
        "filter key {0:?} cannot be filtered: each dot-separated part must use only letters, digits and underscores"
    )]
    InvalidKey(String),
    /// The filter is beyond the vector service's size or depth limits.
    #[error(
        "this filter cannot be expressed by the vector service: {0}. Rewrite it, or split the search"
    )]
    NotExpressible(String),
    /// A value type with no equivalent match.
    #[error("filter value {value} for {key:?} is not supported: {reason}")]
    UnsupportedValue {
        /// The qualified key.
        key: String,
        /// The value as JSON.
        value: String,
        /// Why it is refused.
        reason: String,
    },
}

/// A parsed filter tree, before it is lowered.
#[derive(Clone, Debug)]
enum Node {
    /// A comparison that holds only when the key is present.
    Leaf(pb::Condition),
    /// `$exists`: the key is present (`true`) or missing (`false`).
    Exists(String, bool),
    Not(Box<Node>),
    And(Vec<Node>),
    Or(Vec<Node>),
}

/// Reads a tool's `filter` argument: a JSON object, a JSON string holding
/// one, or nothing. An empty object, empty string and `null` mean "no
/// filter", as in the SDK (`if not filter: filter = None`).
///
/// # Errors
///
/// [`FilterError::Invalid`] when a string is not JSON or the filter is not
/// an object.
pub fn parse_input(input: &Value) -> Result<Option<Value>, FilterError> {
    let parsed = match input {
        Value::Null => return Ok(None),
        Value::String(text) if text.trim().is_empty() => return Ok(None),
        Value::String(text) => serde_json::from_str::<Value>(text).map_err(|error| {
            FilterError::Invalid(format!("the filter string is not JSON: {error}"))
        })?,
        other => other.clone(),
    };
    match parsed {
        Value::Object(map) if map.is_empty() => Ok(None),
        Value::Object(_) => Ok(Some(parsed)),
        Value::Null => Ok(None),
        other => Err(FilterError::Invalid(format!(
            "the filter must be a JSON object, not {}",
            kind(&other)
        ))),
    }
}

/// Translates an SDK filter (a dict or a JSON string) into the filter
/// `elitea-vector` takes. `None` means no narrowing.
///
/// # Errors
///
/// A [`FilterError`] naming the operator or key that cannot be translated.
pub fn translate(input: &Value) -> Result<Option<pb::Filter>, FilterError> {
    let Some(filter) = parse_input(input)? else {
        return Ok(None);
    };
    let node = parse_object(&filter)?;
    let lowered = lower(&node, false);
    check_limits(&lowered)?;
    Ok(Some(lowered))
}

/// Adds one `must` condition to a filter. Used for the chunk-type and
/// document narrowing of `extended_search`.
#[must_use]
pub fn with_must(filter: Option<&pb::Filter>, condition: pb::Condition) -> pb::Filter {
    let mut filter = filter.cloned().unwrap_or_default();
    filter.must.push(condition);
    filter
}

/// A keyword equality on a payload key (`chunk_type`, `document_key`, …).
#[must_use]
pub fn keyword(key: &str, value: &str) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Keyword(value.to_owned())),
    }
}

/// A keyword-in-set on a payload key.
#[must_use]
pub fn any(key: &str, values: Vec<String>) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Any(pb::StringList { values })),
    }
}

/// "The key is missing, null or `[]`".
#[must_use]
pub fn is_empty(key: &str) -> pb::Condition {
    pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
    }
}

/// `filter` as one condition: itself when it is a single positive
/// condition, else a nested filter.
#[must_use]
pub fn nest(filter: pb::Filter) -> pb::Condition {
    if filter.must.len() == 1 && filter.should.is_empty() && filter.must_not.is_empty() {
        return filter.must.into_iter().next().unwrap_or_default();
    }
    pb::Condition {
        key: String::new(),
        r#match: Some(pb::condition::Match::Filter(filter)),
    }
}

fn kind(value: &Value) -> &'static str {
    match value {
        Value::Null => "null",
        Value::Bool(_) => "a boolean",
        Value::Number(_) => "a number",
        Value::String(_) => "a string",
        Value::Array(_) => "an array",
        Value::Object(_) => "an object",
    }
}

fn parse_object(object: &Value) -> Result<Node, FilterError> {
    let Value::Object(map) = object else {
        return Err(FilterError::Invalid(format!(
            "expected a filter object, found {}",
            kind(object)
        )));
    };
    if map.is_empty() {
        return Err(FilterError::Invalid(
            "an empty filter object matches everything and is only allowed as the whole filter"
                .to_owned(),
        ));
    }
    let mut nodes = Vec::with_capacity(map.len());
    for (key, value) in map {
        if key.starts_with('$') {
            nodes.push(parse_logical(key, value)?);
        } else {
            nodes.push(parse_field(key, value)?);
        }
    }
    Ok(if nodes.len() == 1 {
        nodes.remove(0)
    } else {
        Node::And(nodes)
    })
}

fn parse_logical(operator: &str, value: &Value) -> Result<Node, FilterError> {
    match operator.to_lowercase().as_str() {
        "$and" | "$or" => {
            let Value::Array(items) = value else {
                return Err(FilterError::Invalid(format!(
                    "{operator} takes an array of filters, not {}",
                    kind(value)
                )));
            };
            if items.is_empty() {
                return Err(FilterError::Invalid(format!(
                    "{operator} needs at least one filter"
                )));
            }
            let children = items
                .iter()
                .map(parse_object)
                .collect::<Result<Vec<_>, _>>()?;
            Ok(if operator.eq_ignore_ascii_case("$and") {
                Node::And(children)
            } else {
                Node::Or(children)
            })
        }
        "$not" => match value {
            Value::Object(_) => Ok(Node::Not(Box::new(parse_object(value)?))),
            Value::Array(items) if !items.is_empty() => {
                // langchain-postgres: every listed filter is negated, and
                // the negations are ANDed.
                let negated = items
                    .iter()
                    .map(|item| parse_object(item).map(|node| Node::Not(Box::new(node))))
                    .collect::<Result<Vec<_>, _>>()?;
                Ok(Node::And(negated))
            }
            other => Err(FilterError::Invalid(format!(
                "$not takes a filter object or a non-empty array of them, not {}",
                kind(other)
            ))),
        },
        _ => Err(FilterError::UnsupportedOperator {
            operator: operator.to_owned(),
            reason: "it is not a logical operator the SDK defines ($and, $or, $not)".to_owned(),
        }),
    }
}

fn parse_field(field: &str, value: &Value) -> Result<Node, FilterError> {
    let key = qualify_key(field)?;
    let Value::Object(operators) = value else {
        return eq_leaf(&key, value);
    };
    if operators.len() != 1 {
        return Err(FilterError::Invalid(format!(
            "the condition for {field:?} must have exactly one operator, found {}",
            operators.len()
        )));
    }
    let (operator, operand) = operators.iter().next().expect("one entry");
    if !operator.starts_with('$') {
        return Err(FilterError::Invalid(format!(
            "expected an operator such as $eq for {field:?}, found {operator:?}"
        )));
    }
    let lowered = operator.to_lowercase();
    match lowered.as_str() {
        "$eq" => eq_leaf(&key, operand),
        "$ne" => Ok(Node::Not(Box::new(eq_leaf(&key, operand)?))),
        "$in" => in_node(&key, operand, "$in"),
        "$nin" => Ok(Node::Not(Box::new(in_node(&key, operand, "$nin")?))),
        "$lt" | "$lte" | "$gt" | "$gte" => {
            let bound = range_bound(&key, operand, operator)?;
            let mut range = pb::Range::default();
            match lowered.as_str() {
                "$lt" => range.lt = Some(bound),
                "$lte" => range.lte = Some(bound),
                "$gt" => range.gt = Some(bound),
                _ => range.gte = Some(bound),
            }
            Ok(range_leaf(&key, range))
        }
        "$between" => {
            let bounds = match operand {
                Value::Array(items) if items.len() == 2 => items,
                other => {
                    return Err(FilterError::Invalid(format!(
                        "$between takes an array of two numbers [low, high], not {}",
                        kind(other)
                    )));
                }
            };
            let low = range_bound(&key, &bounds[0], operator)?;
            let high = range_bound(&key, &bounds[1], operator)?;
            Ok(range_leaf(
                &key,
                pb::Range {
                    gte: Some(low),
                    lte: Some(high),
                    ..pb::Range::default()
                },
            ))
        }
        "$exists" => match operand {
            Value::Bool(present) => Ok(Node::Exists(key, *present)),
            other => Err(FilterError::Invalid(format!(
                "$exists takes true or false, not {}",
                kind(other)
            ))),
        },
        "$like" | "$ilike" => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "the vector service has no pattern condition on metadata".to_owned(),
        }),
        _ => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "it is not an operator the SDK filter defines".to_owned(),
        }),
    }
}

/// One bound of a range: a finite number, on a metadata key.
fn range_bound(key: &str, operand: &Value, operator: &str) -> Result<f64, FilterError> {
    if !key.starts_with("metadata.") {
        return Err(unsupported_value(
            key,
            operand,
            &format!("{operator} compares numbers, and this point field holds text"),
        ));
    }
    match operand.as_f64() {
        Some(number) if number.is_finite() => Ok(number),
        _ => Err(unsupported_value(
            key,
            operand,
            &format!("{operator} compares numbers only"),
        )),
    }
}

fn range_leaf(key: &str, range: pb::Range) -> Node {
    Node::Leaf(pb::Condition {
        key: key.to_owned(),
        r#match: Some(pb::condition::Match::Range(range)),
    })
}

fn in_node(key: &str, operand: &Value, operator: &str) -> Result<Node, FilterError> {
    let Value::Array(items) = operand else {
        return Err(FilterError::Invalid(format!(
            "{operator} takes an array, not {}",
            kind(operand)
        )));
    };
    if items.is_empty() {
        return Err(FilterError::Invalid(format!(
            "{operator} needs at least one value"
        )));
    }
    if items.iter().all(Value::is_string) || is_point_field(key) {
        if items.len() > MAX_ANY_VALUES {
            return Err(FilterError::NotExpressible(format!(
                "{operator} lists {} values, and the vector service accepts at most {MAX_ANY_VALUES}",
                items.len()
            )));
        }
        // On a point field every value is text: an integer is its decimal
        // string, anything else is refused.
        let values = items
            .iter()
            .map(|item| point_field_text(key, item))
            .collect::<Result<Vec<_>, _>>()?;
        return Ok(Node::Leaf(any(key, values)));
    }
    let leaves = items
        .iter()
        .map(|item| eq_leaf(key, item))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(Node::Or(leaves))
}

/// Whether `key` (as [`qualify_key`] returned it) is one of the point fields
/// a filter may name without the `metadata.` prefix. They hold text.
fn is_point_field(key: &str) -> bool {
    POINT_KEYS.contains(&key)
}

/// The text a value matches against a point field: a string as it is, an
/// integer as its decimal string. A boolean or a decimal can equal no stored
/// value and is refused rather than matched (or, under `$ne`, matched by
/// everything).
fn point_field_text(key: &str, value: &Value) -> Result<String, FilterError> {
    match value {
        Value::String(text) => Ok(text.clone()),
        Value::Number(number) if number.is_i64() || number.is_u64() => Ok(number.to_string()),
        Value::Number(_) | Value::Bool(_) => Err(unsupported_value(
            key,
            value,
            "this point field holds text; compare it with a string, or a whole number which is matched as its decimal string",
        )),
        Value::Null => Err(unsupported_value(
            key,
            value,
            "null is never equal to anything in the SDK's SQL; use $exists",
        )),
        Value::Array(_) | Value::Object(_) => Err(unsupported_value(
            key,
            value,
            "only a string or a whole number can be matched against a point field",
        )),
    }
}

fn eq_leaf(key: &str, value: &Value) -> Result<Node, FilterError> {
    if is_point_field(key) {
        return Ok(Node::Leaf(keyword(key, &point_field_text(key, value)?)));
    }
    let matched = match value {
        Value::String(text) => pb::condition::Match::Keyword(text.clone()),
        Value::Bool(flag) => pb::condition::Match::Boolean(*flag),
        Value::Number(number) => match number.as_i64() {
            Some(integer) => pb::condition::Match::Integer(integer),
            None => {
                return Err(unsupported_value(
                    key,
                    value,
                    "only whole numbers can be matched exactly; compare a decimal with $gte and $lte",
                ));
            }
        },
        Value::Null => {
            return Err(unsupported_value(
                key,
                value,
                "null is never equal to anything in the SDK's SQL; use $exists",
            ));
        }
        Value::Array(_) | Value::Object(_) => {
            return Err(unsupported_value(
                key,
                value,
                "only a string, a whole number or a boolean can be matched",
            ));
        }
    };
    Ok(Node::Leaf(pb::Condition {
        key: key.to_owned(),
        r#match: Some(matched),
    }))
}

fn unsupported_value(key: &str, value: &Value, reason: &str) -> FilterError {
    FilterError::UnsupportedValue {
        key: key.to_owned(),
        value: value.to_string(),
        reason: reason.to_owned(),
    }
}

/// Maps an SDK key to the condition key `elitea-vector` filters on.
fn qualify_key(field: &str) -> Result<String, FilterError> {
    if RESERVED_KEYS.contains(&field) {
        return Err(FilterError::ReservedKey(field.to_owned()));
    }
    if POINT_KEYS.contains(&field) {
        return Ok(field.to_owned());
    }
    let path = field.strip_prefix("metadata.").unwrap_or(field);
    let valid = !path.is_empty()
        && path.len() <= 256
        && path.split('.').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        });
    if !valid {
        return Err(FilterError::InvalidKey(field.to_owned()));
    }
    Ok(format!("metadata.{path}"))
}

/// The filter that holds when `node` is true (`negate` false) or when it is
/// false (`negate` true), in SQL's sense: a comparison on a missing key is
/// neither.
fn lower(node: &Node, negate: bool) -> pb::Filter {
    match node {
        Node::Leaf(condition) if negate => pb::Filter {
            must_not: vec![is_empty(&condition.key), condition.clone()],
            ..pb::Filter::default()
        },
        Node::Leaf(condition) => pb::Filter {
            must: vec![condition.clone()],
            ..pb::Filter::default()
        },
        Node::Exists(key, present) if *present != negate => pb::Filter {
            must_not: vec![is_empty(key)],
            ..pb::Filter::default()
        },
        Node::Exists(key, _) => pb::Filter {
            must: vec![is_empty(key)],
            ..pb::Filter::default()
        },
        Node::Not(inner) => lower(inner, !negate),
        // false(a and b) == false(a) or false(b)
        Node::And(children) if negate => or_of(lower_all(children, true)),
        Node::And(children) => and_of(lower_all(children, false)),
        // false(a or b) == false(a) and false(b)
        Node::Or(children) if negate => and_of(lower_all(children, true)),
        Node::Or(children) => or_of(lower_all(children, false)),
    }
}

fn lower_all(children: &[Node], negate: bool) -> Vec<pb::Filter> {
    children.iter().map(|child| lower(child, negate)).collect()
}

fn push_unique(list: &mut Vec<pb::Condition>, condition: pb::Condition) {
    if !list.contains(&condition) {
        list.push(condition);
    }
}

/// Every part holds. `must` and `must_not` merge; the first `should` group
/// merges, and a later one is nested.
fn and_of(parts: Vec<pb::Filter>) -> pb::Filter {
    let mut out = pb::Filter::default();
    for part in parts {
        if !part.should.is_empty() && !out.should.is_empty() {
            push_unique(&mut out.must, nest(part));
            continue;
        }
        for condition in part.must {
            push_unique(&mut out.must, condition);
        }
        for condition in part.must_not {
            push_unique(&mut out.must_not, condition);
        }
        out.should.extend(part.should);
    }
    out
}

/// At least one part holds. A single positive condition or a pure `should`
/// group joins the list; anything else is nested.
fn or_of(mut parts: Vec<pb::Filter>) -> pb::Filter {
    if parts.len() == 1 {
        return parts.remove(0);
    }
    let mut should = Vec::new();
    for part in parts {
        if part.must.is_empty() && part.must_not.is_empty() {
            for condition in part.should {
                push_unique(&mut should, condition);
            }
        } else {
            push_unique(&mut should, nest(part));
        }
    }
    pb::Filter {
        should,
        ..pb::Filter::default()
    }
}

/// Refuses a filter beyond `elitea-vector`'s limits: more than
/// [`MAX_CONDITIONS`] in one list, more than [`MAX_TOTAL_CONDITIONS`] in
/// all, or deeper than [`MAX_DEPTH`]. [`translate`] runs it on the caller's
/// filter; a search that adds conditions of its own (`extended_search`)
/// runs it again on the filter it finally sends.
///
/// # Errors
///
/// [`FilterError::NotExpressible`], naming the limit.
pub fn check_limits(filter: &pb::Filter) -> Result<(), FilterError> {
    let mut total = 0;
    walk(filter, 1, &mut total, true)?;
    if total > MAX_TOTAL_CONDITIONS {
        return Err(FilterError::NotExpressible(format!(
            "it needs {total} conditions, and the vector service accepts at most {MAX_TOTAL_CONDITIONS}"
        )));
    }
    Ok(())
}

/// How many conditions a filter holds in all, nested ones included: what
/// [`MAX_TOTAL_CONDITIONS`] bounds.
#[must_use]
pub fn condition_count(filter: &pb::Filter) -> usize {
    let mut total = 0;
    // Without the checks the walk cannot fail.
    let _ = walk(filter, 1, &mut total, false);
    total
}

fn walk(
    filter: &pb::Filter,
    depth: usize,
    total: &mut usize,
    check: bool,
) -> Result<(), FilterError> {
    if check && depth > MAX_DEPTH {
        return Err(FilterError::NotExpressible(format!(
            "it nests deeper than the {MAX_DEPTH} levels the vector service accepts"
        )));
    }
    for (name, list) in [
        ("must", &filter.must),
        ("should", &filter.should),
        ("must_not", &filter.must_not),
    ] {
        if check && list.len() > MAX_CONDITIONS {
            return Err(FilterError::NotExpressible(format!(
                "it needs {} conditions in one {name} list, and the vector service accepts at most {MAX_CONDITIONS}",
                list.len()
            )));
        }
        for condition in list {
            *total += 1;
            if let Some(pb::condition::Match::Filter(inner)) = &condition.r#match {
                walk(inner, depth + 1, total, check)?;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn must_keys(filter: &pb::Filter) -> Vec<&str> {
        filter.must.iter().map(|c| c.key.as_str()).collect()
    }

    fn kw(key: &str, value: &str) -> pb::Condition {
        keyword(key, value)
    }

    fn translated(input: &Value) -> pb::Filter {
        translate(input).expect("translates").expect("not empty")
    }

    fn refused(input: &Value) -> FilterError {
        translate(input).expect_err("refused")
    }

    fn nested(filter: pb::Filter) -> pb::Condition {
        pb::Condition {
            key: String::new(),
            r#match: Some(pb::condition::Match::Filter(filter)),
        }
    }

    /// "present and not `condition`".
    fn present_not(condition: pb::Condition) -> pb::Filter {
        pb::Filter {
            must_not: vec![is_empty(&condition.key.clone()), condition],
            ..pb::Filter::default()
        }
    }

    #[test]
    fn empty_filters_mean_no_narrowing() {
        for input in [json!(null), json!({}), json!(""), json!("  "), json!("{}")] {
            assert_eq!(translate(&input), Ok(None), "{input}");
        }
    }

    #[test]
    fn an_empty_negation_or_group_is_refused_not_ignored() {
        for input in [
            json!({"$not": {}}),
            json!({"$not": [{}]}),
            json!({"$not": [{"a": "1"}, {}]}),
            json!({"$not": []}),
            json!({"$not": {"$and": []}}),
            json!({"$not": {"$or": [{}]}}),
            json!({"$and": [{}]}),
            json!({"$or": [{"a": "1"}, {}]}),
            json!({"a": "1", "$not": {}}),
            json!(r#"{"$not": {}}"#),
        ] {
            assert!(
                matches!(refused(&input), FilterError::Invalid(_)),
                "{input}"
            );
        }
    }

    #[test]
    fn a_json_string_is_parsed() {
        let filter = translated(&json!(r#"{"author": "ann"}"#));
        assert_eq!(filter.must, vec![kw("metadata.author", "ann")]);
        assert!(matches!(
            translate(&json!("{not json")),
            Err(FilterError::Invalid(_))
        ));
        assert!(matches!(
            translate(&json!("[1]")),
            Err(FilterError::Invalid(_))
        ));
        assert!(matches!(translate(&json!(7)), Err(FilterError::Invalid(_))));
    }

    #[test]
    fn implicit_equality_and_eq_match_each_scalar_type() {
        let filter = translated(&json!({"a": "x", "b": {"$eq": 5}, "c": {"$eq": true}}));
        assert_eq!(filter.must.len(), 3);
        assert_eq!(filter.must[0], kw("metadata.a", "x"));
        assert_eq!(
            filter.must[1].r#match,
            Some(pb::condition::Match::Integer(5))
        );
        assert_eq!(
            filter.must[2].r#match,
            Some(pb::condition::Match::Boolean(true))
        );
        assert!(filter.should.is_empty() && filter.must_not.is_empty());
    }

    #[test]
    fn ne_requires_the_key_to_be_present() {
        let filter = translated(&json!({"status": {"$ne": "archived"}}));
        assert_eq!(filter, present_not(kw("metadata.status", "archived")));
    }

    #[test]
    fn in_of_strings_is_one_any_match() {
        let filter = translated(&json!({"lang": {"$in": ["en", "de"]}}));
        assert_eq!(
            filter.must,
            vec![any("metadata.lang", vec!["en".into(), "de".into()])]
        );
    }

    #[test]
    fn in_of_numbers_is_an_or_of_matches() {
        let filter = translated(&json!({"year": {"$in": [2024, 2025]}}));
        assert_eq!(filter.must, []);
        assert_eq!(filter.should.len(), 2);
        assert_eq!(
            filter.should[1].r#match,
            Some(pb::condition::Match::Integer(2025))
        );
    }

    #[test]
    fn nin_requires_the_key_and_negates_in() {
        let strings = translated(&json!({"lang": {"$nin": ["en", "de"]}}));
        assert_eq!(
            strings,
            present_not(any("metadata.lang", vec!["en".into(), "de".into()]))
        );
        // One presence check, not one per value.
        let numbers = translated(&json!({"year": {"$nin": [2024, 2025]}}));
        assert_eq!(numbers.must_not.len(), 3);
        assert_eq!(numbers.must_not[0], is_empty("metadata.year"));
        assert!(numbers.should.is_empty() && numbers.must.is_empty());
    }

    /// A stored point with `chunk_id` and `document_key`, for checking a
    /// translated filter against Qdrant's semantics.
    fn stored(chunk_id: &str) -> pb::Point {
        pb::Point {
            document_key: "d".to_owned(),
            chunk_id: chunk_id.to_owned(),
            chunk_type: "document".to_owned(),
            metadata_json: "{}".to_owned(),
            ..pb::Point::default()
        }
    }

    #[test]
    fn integers_on_point_fields_match_their_decimal_strings() {
        use crate::testing::matches_filter;
        let points: Vec<pb::Point> = ["1", "2", "3", "4"].into_iter().map(stored).collect();
        let found = |filter: &pb::Filter| -> Vec<&str> {
            points
                .iter()
                .filter(|point| matches_filter(point, Some(filter)))
                .map(|point| point.chunk_id.as_str())
                .collect()
        };
        let eq = translated(&json!({"chunk_id": 3}));
        assert_eq!(eq.must, vec![kw("chunk_id", "3")]);
        assert_eq!(found(&eq), ["3"]);
        let explicit = translated(&json!({"chunk_id": {"$eq": 3}}));
        assert_eq!(explicit, eq);
        let in_list = translated(&json!({"chunk_id": {"$in": [1, 2]}}));
        assert_eq!(
            in_list.must,
            vec![any("chunk_id", vec!["1".into(), "2".into()])]
        );
        assert_eq!(found(&in_list), ["1", "2"]);
        // Mixed strings and integers are one keyword set.
        let mixed = translated(&json!({"chunk_id": {"$in": ["1", 2]}}));
        assert_eq!(found(&mixed), ["1", "2"]);
        // The negations get the same conversion, so they no longer match
        // every point.
        let ne = translated(&json!({"chunk_id": {"$ne": 3}}));
        assert_eq!(ne, present_not(kw("chunk_id", "3")));
        assert_eq!(found(&ne), ["1", "2", "4"]);
        let nin = translated(&json!({"chunk_id": {"$nin": [1, 2]}}));
        assert_eq!(
            nin,
            present_not(any("chunk_id", vec!["1".into(), "2".into()]))
        );
        assert_eq!(found(&nin), ["3", "4"]);
        for key in POINT_KEYS {
            assert_eq!(
                translated(&json!({key: 7})).must,
                vec![kw(key, "7")],
                "{key}"
            );
        }
        // A metadata key keeps its integer match.
        assert_eq!(
            translated(&json!({"chunk": 3})).must[0].r#match,
            Some(pb::condition::Match::Integer(3))
        );
    }

    #[test]
    fn booleans_and_decimals_on_point_fields_are_refused() {
        for (filter, value) in [
            (json!({"chunk_id": true}), "true"),
            (json!({"chunk_id": 1.5}), "1.5"),
            (json!({"chunk_id": {"$eq": false}}), "false"),
            (json!({"chunk_id": {"$ne": true}}), "true"),
            (json!({"chunk_id": {"$in": [1, 2.5]}}), "2.5"),
            (json!({"chunk_id": {"$nin": [false]}}), "false"),
            (json!({"chunk_type": {"$in": ["document", true]}}), "true"),
        ] {
            match refused(&filter) {
                FilterError::UnsupportedValue {
                    key,
                    value: shown,
                    reason,
                } => {
                    assert_eq!(&key, filter.as_object().unwrap().keys().next().unwrap());
                    assert_eq!(shown, value, "{filter}");
                    assert!(reason.contains("holds text"), "{reason}");
                }
                other => panic!("{filter}: {other}"),
            }
        }
    }

    #[test]
    fn ranges_and_between_on_metadata() {
        let filter = translated(&json!({"year": {"$gte": 2020}, "score": {"$lt": 0.5}}));
        assert_eq!(must_keys(&filter), ["metadata.year", "metadata.score"]);
        assert_eq!(
            filter.must[0].r#match,
            Some(pb::condition::Match::Range(pb::Range {
                gte: Some(2020.0),
                ..pb::Range::default()
            }))
        );
        assert_eq!(
            filter.must[1].r#match,
            Some(pb::condition::Match::Range(pb::Range {
                lt: Some(0.5),
                ..pb::Range::default()
            }))
        );
        let between = translated(&json!({"year": {"$between": [2020, 2024]}}));
        assert_eq!(
            between.must[0].r#match,
            Some(pb::condition::Match::Range(pb::Range {
                gte: Some(2020.0),
                lte: Some(2024.0),
                ..pb::Range::default()
            }))
        );
        // A negated range still needs the key.
        let outside = translated(&json!({"$not": {"year": {"$gt": 2020}}}));
        assert_eq!(outside.must_not[0], is_empty("metadata.year"));
        for bad in [
            json!({"year": {"$gt": "2020"}}),
            json!({"year": {"$gt": null}}),
            json!({"chunk_id": {"$gt": 1}}),
            json!({"year": {"$between": [1]}}),
            json!({"year": {"$between": [1, "x"]}}),
        ] {
            assert!(
                matches!(
                    refused(&bad),
                    FilterError::UnsupportedValue { .. } | FilterError::Invalid(_)
                ),
                "{bad}"
            );
        }
    }

    #[test]
    fn exists_is_a_presence_check() {
        assert_eq!(
            translated(&json!({"type": {"$exists": true}})).must_not,
            vec![is_empty("metadata.type")]
        );
        assert_eq!(
            translated(&json!({"type": {"$exists": false}})).must,
            vec![is_empty("metadata.type")]
        );
        // not exists == missing
        assert_eq!(
            translated(&json!({"$not": {"type": {"$exists": true}}})).must,
            vec![is_empty("metadata.type")]
        );
        assert!(matches!(
            refused(&json!({"type": {"$exists": 1}})),
            FilterError::Invalid(_)
        ));
    }

    #[test]
    fn and_collects_into_must() {
        let filter =
            translated(&json!({"$and": [{"a": "1"}, {"b": {"$ne": "2"}}, {"c": {"$in": ["x"]}}]}));
        assert_eq!(must_keys(&filter), ["metadata.a", "metadata.c"]);
        assert_eq!(filter.must_not, present_not(kw("metadata.b", "2")).must_not);
    }

    #[test]
    fn or_of_equalities_is_should() {
        let filter = translated(&json!({"$or": [{"a": "1"}, {"b": "2"}]}));
        assert_eq!(filter.must, []);
        assert_eq!(
            filter.should,
            vec![kw("metadata.a", "1"), kw("metadata.b", "2")]
        );
    }

    #[test]
    fn or_beside_other_keys_keeps_them_in_must() {
        let filter = translated(&json!({"team": "x", "$or": [{"a": "1"}, {"b": "2"}]}));
        assert_eq!(filter.must, vec![kw("metadata.team", "x")]);
        assert_eq!(filter.should.len(), 2);
    }

    #[test]
    fn not_negates_with_presence() {
        // not (a == 1): a present and not 1
        let single = translated(&json!({"$not": {"a": "1"}}));
        assert_eq!(single, present_not(kw("metadata.a", "1")));
        // not (a == 1 or b == 2): both false, both present
        let either = translated(&json!({"$not": {"$or": [{"a": "1"}, {"b": "2"}]}}));
        assert_eq!(either.must_not.len(), 4);
        assert_eq!(either.should, []);
        // a list of filters negates each and ANDs them
        let listed = translated(&json!({"$not": [{"a": "1"}, {"b": "2"}]}));
        assert_eq!(listed, either);
        // not not a -> a
        let twice = translated(&json!({"$not": {"$not": {"a": "1"}}}));
        assert_eq!(twice.must, vec![kw("metadata.a", "1")]);
        // not (a != 1) -> a == 1 (a missing key is false under both)
        let flipped = translated(&json!({"$not": {"a": {"$ne": "1"}}}));
        assert_eq!(flipped.must, vec![kw("metadata.a", "1")]);
        assert_eq!(flipped.must_not, []);
    }

    #[test]
    fn a_negated_and_is_an_or_of_nested_negations() {
        let filter = translated(&json!({"$not": {"$and": [{"a": "1"}, {"b": "2"}]}}));
        assert_eq!(
            filter.should,
            vec![
                nested(present_not(kw("metadata.a", "1"))),
                nested(present_not(kw("metadata.b", "2"))),
            ]
        );
        // with ONE member it is just not a
        let one = translated(&json!({"$not": {"$and": [{"a": "1"}]}}));
        assert_eq!(one, present_not(kw("metadata.a", "1")));
    }

    #[test]
    fn or_of_negations_groups_and_two_ors_nest() {
        let negation = translated(&json!({"$or": [{"a": {"$ne": "1"}}, {"b": "2"}]}));
        assert_eq!(
            negation.should,
            vec![
                nested(present_not(kw("metadata.a", "1"))),
                kw("metadata.b", "2")
            ]
        );
        let group = translated(&json!({"$or": [{"$and": [{"a": "1"}, {"b": "2"}]}, {"c": "3"}]}));
        assert_eq!(
            group.should,
            vec![
                nested(pb::Filter {
                    must: vec![kw("metadata.a", "1"), kw("metadata.b", "2")],
                    ..pb::Filter::default()
                }),
                kw("metadata.c", "3")
            ]
        );
        let two = translated(&json!({"$and": [
            {"$or": [{"a": "1"}, {"b": "2"}]},
            {"$or": [{"c": "3"}, {"d": "4"}]}
        ]}));
        assert_eq!(
            two.should,
            vec![kw("metadata.a", "1"), kw("metadata.b", "2")]
        );
        assert_eq!(
            two.must,
            vec![nested(pb::Filter {
                should: vec![kw("metadata.c", "3"), kw("metadata.d", "4")],
                ..pb::Filter::default()
            })]
        );
    }

    #[test]
    fn or_beside_an_integer_in_nests_one_group() {
        // The integer $in is itself an "at least one of" group.
        let filter = translated(&json!({
            "year": {"$in": [2024, 2025]},
            "$or": [{"lang": "en"}, {"lang": "de"}]
        }));
        assert_eq!(filter.should.len(), 2);
        assert_eq!(filter.must.len(), 1);
        assert!(matches!(
            filter.must[0].r#match,
            Some(pb::condition::Match::Filter(_))
        ));
    }

    #[test]
    fn pattern_operators_are_refused() {
        for operator in ["$like", "$ilike"] {
            match refused(&json!({"n": {operator: "a%"}})) {
                FilterError::UnsupportedOperator {
                    operator: named,
                    reason,
                } => {
                    assert_eq!(named, operator);
                    assert_ne!(reason, "");
                }
                other => panic!("{operator}: {other}"),
            }
        }
    }

    #[test]
    fn unknown_operators_are_refused() {
        assert!(matches!(
            refused(&json!({"a": {"$regex": "x"}})),
            FilterError::UnsupportedOperator { .. }
        ));
        assert!(matches!(
            refused(&json!({"$xor": [{"a": "1"}]})),
            FilterError::UnsupportedOperator { .. }
        ));
        assert!(matches!(
            refused(&json!({"a": {"eq": "1"}})),
            FilterError::Invalid(_)
        ));
        assert!(matches!(
            refused(&json!({"a": {"$eq": "1", "$ne": "2"}})),
            FilterError::Invalid(_)
        ));
    }

    #[test]
    fn reserved_keys_are_refused_everywhere() {
        for key in ["project_id", "source", "namespace_id", "generation", "text"] {
            for filter in [
                json!({key: "x"}),
                json!({key: {"$ne": "x"}}),
                json!({key: {"$exists": false}}),
                json!({key: {"$gt": 1}}),
                json!({"$and": [{key: {"$in": ["x"]}}]}),
                json!({"$or": [{"a": "1"}, {key: "x"}]}),
                json!({"$not": {key: "x"}}),
                json!({"$not": {"$and": [{"a": "1"}, {"$or": [{"b": "2"}, {key: "x"}]}]}}),
            ] {
                assert_eq!(
                    refused(&filter),
                    FilterError::ReservedKey(key.to_owned()),
                    "{filter}"
                );
            }
        }
        // a nested path that merely starts with a reserved word is metadata
        let nested = translated(&json!({"source.path": "a"}));
        assert_eq!(nested.must[0].key, "metadata.source.path");
    }

    #[test]
    fn keys_are_qualified_under_metadata() {
        let filter = translated(&json!({
            "author": "a",
            "config.timeout": 30,
            "metadata.lang": "en",
            "chunk_type": "title",
            "document_key": "d",
        }));
        assert_eq!(
            must_keys(&filter),
            [
                "metadata.author",
                "metadata.config.timeout",
                "metadata.lang",
                "chunk_type",
                "document_key"
            ]
        );
    }

    #[test]
    fn keys_that_cannot_be_payload_paths_are_refused() {
        for key in [
            "a-b",
            "a b",
            "a..b",
            ".a",
            "a.",
            "metadata.",
            "\u{e9}t\u{e9}",
        ] {
            assert_eq!(
                refused(&json!({key: "x"})),
                FilterError::InvalidKey(key.to_owned()),
                "{key}"
            );
        }
    }

    #[test]
    fn unmatchable_values_are_refused() {
        for value in [
            json!(1.5),
            json!(null),
            json!([1]),
            json!({"x": 1}),
            json!(1.0e30),
        ] {
            let filter = json!({"a": value});
            assert!(
                matches!(
                    refused(&filter),
                    FilterError::UnsupportedValue { .. } | FilterError::Invalid(_)
                ),
                "{filter}"
            );
        }
        assert!(matches!(
            refused(&json!({"a": {"$in": []}})),
            FilterError::Invalid(_)
        ));
        assert!(matches!(
            refused(&json!({"a": {"$in": "x"}})),
            FilterError::Invalid(_)
        ));
        assert!(matches!(
            refused(&json!({"a": {"$in": [1.5]}})),
            FilterError::UnsupportedValue { .. }
        ));
    }

    #[test]
    fn list_depth_and_total_limits_are_enforced_before_the_call() {
        let many: Vec<Value> = (0..65).map(|n| json!({format!("k{n}"): "v"})).collect();
        assert!(matches!(
            refused(&json!({"$and": many})),
            FilterError::NotExpressible(_)
        ));
        let huge: Vec<String> = (0..1025).map(|n| n.to_string()).collect();
        assert!(matches!(
            refused(&json!({"a": {"$in": huge}})),
            FilterError::NotExpressible(_)
        ));
        // Each level is an $or whose first member is an $and holding the
        // previous level: one more nested filter per level.
        let mut deep = json!({"a": "1"});
        for level in 0..MAX_DEPTH {
            deep = json!({"$or": [{"$and": [deep, {format!("z{level}"): "1"}]}, {format!("k{level}"): "x"}]});
        }
        assert!(
            matches!(refused(&deep), FilterError::NotExpressible(ref m) if m.contains("levels")),
            "{deep}"
        );
        // 40 nested groups of 30 conditions: 1240 in all.
        let wide: Vec<Value> = (0..40)
            .map(|group| {
                let members: Vec<Value> = (0..30)
                    .map(|n| json!({format!("k{group}_{n}"): {"$ne": "x"}}))
                    .collect();
                json!({"$or": [{"$and": members}, {"z": "1"}]})
            })
            .collect();
        assert!(matches!(
            refused(&json!({"$and": wide})),
            FilterError::NotExpressible(ref m) if m.contains("conditions")
        ));
    }

    #[test]
    fn common_sdk_filter_shapes_translate() {
        for input in [
            json!({"file_path": "src/main.rs"}),
            json!({"file_path": {"$in": ["a.rs", "b.rs"]}}),
            json!({"$and": [{"type": "code"}, {"lang": {"$ne": "md"}}]}),
            json!({"$or": [{"author": "ann"}, {"author": "bob"}, {"reviewed": true}]}),
            json!({"updated_on": {"$gte": 1_700_000_000}, "type": {"$exists": true}}),
            json!({"$and": [
                {"$or": [{"a": "1"}, {"b": 2}]},
                {"$not": {"$and": [{"c": "3"}, {"d": {"$lt": 4}}]}},
                {"e": {"$nin": [1, 2]}}
            ]}),
            json!({"type": {"$exists": false}}),
            json!({"metadata.chunk_id": {"$between": [1, 10]}}),
        ] {
            assert!(translate(&input).is_ok(), "{input}");
        }
    }

    #[test]
    fn with_must_adds_beside_a_users_filter() {
        let user = translated(&json!({"$or": [{"a": "1"}, {"b": "2"}]}));
        let combined = with_must(Some(&user), keyword("chunk_type", "document"));
        assert_eq!(combined.must, vec![kw("chunk_type", "document")]);
        assert_eq!(combined.should.len(), 2);
        let alone = with_must(None, keyword("chunk_type", "title"));
        assert_eq!(alone.must.len(), 1);
    }
}
