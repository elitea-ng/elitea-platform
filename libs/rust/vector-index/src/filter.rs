//! Translation of the SDK's filter JSON into `elitea.vector.v1` filters
//! (ADR-0030 decision 5, ADR-0031 decision 4).
//!
//! The SDK filters with langchain-postgres operators over a flat metadata
//! dict. `elitea-vector` accepts something much smaller: three flat lists
//! of conditions (`must`, `should`, `must_not`), each condition an equality
//! on a keyword, an integer or a boolean, or a keyword-in-set. This module
//! translates exactly what that can express and refuses the rest with a
//! message that names the operator and says why. It never approximates: a
//! refused filter is an error the model or user can read, not a search that
//! silently returns something else.
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
//! `chunk_id`, `chunk_type`, `acl`) because they are real payload keys. Five
//! keys are the facade's own scope and are refused: `project_id`, `source`,
//! `namespace_id`, `generation` and `text`.
//!
//! # Operators
//!
//! | SDK | Translation |
//! | --- | --- |
//! | `$eq`, implicit equality | `keyword`, `integer` or `boolean` match |
//! | `$ne` | the same match in `must_not` |
//! | `$in` of strings | one `any` match |
//! | `$in` of integers or booleans | an `or` of matches (`should`) |
//! | `$nin` | the negation of `$in` |
//! | `$and` / several keys | conditions in `must` |
//! | `$or` | `should` |
//! | `$not` | De Morgan, then `must_not` |
//! | `$lt` `$lte` `$gt` `$gte` `$between` | refused: no range condition |
//! | `$like` `$ilike` | refused: no pattern condition |
//! | `$exists` | refused: no presence condition |
//!
//! A filter whose `$or`/`$not` structure cannot be written as one flat
//! `must`/`should`/`must_not` triple is refused too. (`should` is one
//! group: "at least one of", `AND`ed with the rest.)

use serde_json::Value;

use crate::pb;

/// The most conditions `elitea-vector` accepts in one list.
const MAX_CONDITIONS: usize = 64;
/// The most values `elitea-vector` accepts in one `any` match.
const MAX_ANY_VALUES: usize = 1024;

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
    /// argument of the wrong shape.
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
    /// The `$or`/`$not` structure has no flat `must`/`should`/`must_not` form.
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

/// A parsed filter tree, before it is flattened.
#[derive(Clone, Debug)]
enum Node {
    Leaf(pb::Condition),
    Not(Box<Node>),
    And(Vec<Node>),
    Or(Vec<Node>),
}

/// The flat form `elitea-vector` accepts.
#[derive(Clone, Debug, Default)]
struct Flat {
    must: Vec<pb::Condition>,
    should: Vec<pb::Condition>,
    must_not: Vec<pb::Condition>,
}

impl Flat {
    fn is_empty(&self) -> bool {
        self.must.is_empty() && self.should.is_empty() && self.must_not.is_empty()
    }

    fn is_single_positive(&self) -> bool {
        self.must.len() == 1 && self.should.is_empty() && self.must_not.is_empty()
    }

    fn is_pure_should(&self) -> bool {
        self.must.is_empty() && !self.should.is_empty() && self.must_not.is_empty()
    }
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
    let flat = lower(&node, false)?;
    if flat.is_empty() {
        return Ok(None);
    }
    for (name, list) in [
        ("must", &flat.must),
        ("should", &flat.should),
        ("must_not", &flat.must_not),
    ] {
        if list.len() > MAX_CONDITIONS {
            return Err(FilterError::NotExpressible(format!(
                "it needs {} conditions in {name}, and the vector service accepts at most {MAX_CONDITIONS}",
                list.len()
            )));
        }
    }
    Ok(Some(pb::Filter {
        must: flat.must,
        should: flat.should,
        must_not: flat.must_not,
    }))
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
    match operator.to_lowercase().as_str() {
        "$eq" => eq_leaf(&key, operand),
        "$ne" => Ok(Node::Not(Box::new(eq_leaf(&key, operand)?))),
        "$in" => in_node(&key, operand, "$in"),
        "$nin" => Ok(Node::Not(Box::new(in_node(&key, operand, "$nin")?))),
        "$lt" | "$lte" | "$gt" | "$gte" => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "the vector service has no range condition, and a comparison cannot be approximated with equality".to_owned(),
        }),
        "$between" => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "the vector service has no range condition, and a range cannot be approximated with equality".to_owned(),
        }),
        "$like" | "$ilike" => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "the vector service has no pattern condition on metadata".to_owned(),
        }),
        "$exists" => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "the vector service has no presence condition on metadata".to_owned(),
        }),
        _ => Err(FilterError::UnsupportedOperator {
            operator: operator.clone(),
            reason: "it is not an operator the SDK filter defines".to_owned(),
        }),
    }
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
    if items.iter().all(Value::is_string) {
        if items.len() > MAX_ANY_VALUES {
            return Err(FilterError::NotExpressible(format!(
                "{operator} lists {} values, and the vector service accepts at most {MAX_ANY_VALUES}",
                items.len()
            )));
        }
        let values = items
            .iter()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect();
        return Ok(Node::Leaf(pb::Condition {
            key: key.to_owned(),
            r#match: Some(pb::condition::Match::Any(pb::StringList { values })),
        }));
    }
    let leaves = items
        .iter()
        .map(|item| eq_leaf(key, item))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(Node::Or(leaves))
}

fn eq_leaf(key: &str, value: &Value) -> Result<Node, FilterError> {
    let matched = match value {
        Value::String(text) => pb::condition::Match::Keyword(text.clone()),
        Value::Bool(flag) => pb::condition::Match::Boolean(*flag),
        Value::Number(number) => match number.as_i64() {
            Some(integer) => pb::condition::Match::Integer(integer),
            None => {
                return Err(unsupported_value(
                    key,
                    value,
                    "only whole numbers can be matched exactly; a decimal cannot be compared for equality",
                ));
            }
        },
        Value::Null => {
            return Err(unsupported_value(
                key,
                value,
                "null cannot be matched; the vector service has no presence condition",
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

fn merge(parts: Vec<Flat>) -> Result<Flat, FilterError> {
    let mut merged = Flat::default();
    for part in parts {
        merged.must.extend(part.must);
        merged.must_not.extend(part.must_not);
        if !part.should.is_empty() {
            if !merged.should.is_empty() {
                return Err(FilterError::NotExpressible(
                    "two $or groups are combined with $and, and the vector service has one \"at least one of\" group per filter".to_owned(),
                ));
            }
            merged.should = part.should;
        }
    }
    Ok(merged)
}

/// Flattens `node` (negated when `negate`) into `must`/`should`/`must_not`.
fn lower(node: &Node, negate: bool) -> Result<Flat, FilterError> {
    match node {
        Node::Leaf(condition) => Ok(if negate {
            Flat {
                must_not: vec![condition.clone()],
                ..Flat::default()
            }
        } else {
            Flat {
                must: vec![condition.clone()],
                ..Flat::default()
            }
        }),
        Node::Not(inner) => lower(inner, !negate),
        // not (a and b) == (not a) or (not b)
        Node::And(children) if negate => lower_or(children, true),
        Node::And(children) => merge(lower_all(children, false)?),
        // not (a or b) == (not a) and (not b)
        Node::Or(children) if negate => merge(lower_all(children, true)?),
        Node::Or(children) => lower_or(children, false),
    }
}

fn lower_all(children: &[Node], negate: bool) -> Result<Vec<Flat>, FilterError> {
    children.iter().map(|child| lower(child, negate)).collect()
}

/// An "at least one of" group. Each member must itself be one positive
/// condition (or a group of them): `should` cannot hold a negation or an
/// `AND`.
fn lower_or(children: &[Node], negate: bool) -> Result<Flat, FilterError> {
    let mut parts = lower_all(children, negate)?;
    if parts.len() == 1 {
        return Ok(parts.remove(0));
    }
    let mut should = Vec::new();
    for part in parts {
        if part.is_single_positive() {
            should.extend(part.must);
        } else if part.is_pure_should() {
            should.extend(part.should);
        } else if part.is_empty() {
            return Err(FilterError::NotExpressible(
                "an $or contains an empty filter, which matches everything".to_owned(),
            ));
        } else {
            return Err(FilterError::NotExpressible(
                "an $or (or a negated $and) joins conditions that are negated or grouped with $and; the vector service can only match \"at least one of\" simple equality or $in conditions".to_owned(),
            ));
        }
    }
    Ok(Flat {
        should,
        ..Flat::default()
    })
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

    #[test]
    fn empty_filters_mean_no_narrowing() {
        for input in [json!(null), json!({}), json!(""), json!("  "), json!("{}")] {
            assert_eq!(translate(&input), Ok(None), "{input}");
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
    fn ne_is_a_negated_match() {
        let filter = translated(&json!({"status": {"$ne": "archived"}}));
        assert!(filter.must.is_empty());
        assert_eq!(filter.must_not, vec![kw("metadata.status", "archived")]);
    }

    #[test]
    fn in_of_strings_is_one_any_match() {
        let filter = translated(&json!({"lang": {"$in": ["en", "de"]}}));
        assert_eq!(
            filter.must[0].r#match,
            Some(pb::condition::Match::Any(pb::StringList {
                values: vec!["en".into(), "de".into()]
            }))
        );
    }

    #[test]
    fn in_of_numbers_is_an_or_of_matches() {
        let filter = translated(&json!({"year": {"$in": [2024, 2025]}}));
        assert!(filter.must.is_empty());
        assert_eq!(filter.should.len(), 2);
        assert_eq!(
            filter.should[1].r#match,
            Some(pb::condition::Match::Integer(2025))
        );
    }

    #[test]
    fn nin_negates_in() {
        let strings = translated(&json!({"lang": {"$nin": ["en", "de"]}}));
        assert!(strings.must.is_empty());
        assert_eq!(strings.must_not.len(), 1);
        assert!(matches!(
            strings.must_not[0].r#match,
            Some(pb::condition::Match::Any(_))
        ));
        let numbers = translated(&json!({"year": {"$nin": [2024, 2025]}}));
        assert_eq!(numbers.must_not.len(), 2);
        assert!(numbers.should.is_empty());
    }

    #[test]
    fn and_collects_into_must() {
        let filter =
            translated(&json!({"$and": [{"a": "1"}, {"b": {"$ne": "2"}}, {"c": {"$in": ["x"]}}]}));
        assert_eq!(must_keys(&filter), ["metadata.a", "metadata.c"]);
        assert_eq!(filter.must_not.len(), 1);
    }

    #[test]
    fn or_of_equalities_is_should() {
        let filter = translated(&json!({"$or": [{"a": "1"}, {"b": "2"}]}));
        assert!(filter.must.is_empty());
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
    fn not_applies_de_morgan() {
        // not (a == 1) -> must_not
        let single = translated(&json!({"$not": {"a": "1"}}));
        assert_eq!(single.must_not, vec![kw("metadata.a", "1")]);
        // not (a == 1 or b == 2) -> both in must_not
        let either = translated(&json!({"$not": {"$or": [{"a": "1"}, {"b": "2"}]}}));
        assert_eq!(either.must_not.len(), 2);
        assert!(either.should.is_empty());
        // a list of filters negates each and ANDs them
        let listed = translated(&json!({"$not": [{"a": "1"}, {"b": "2"}]}));
        assert_eq!(listed.must_not.len(), 2);
        // not not a -> a
        let twice = translated(&json!({"$not": {"$not": {"a": "1"}}}));
        assert_eq!(twice.must, vec![kw("metadata.a", "1")]);
        // not (a != 1) -> a == 1
        let flipped = translated(&json!({"$not": {"a": {"$ne": "1"}}}));
        assert_eq!(flipped.must, vec![kw("metadata.a", "1")]);
    }

    #[test]
    fn not_of_an_and_of_two_is_refused() {
        // not (a and b) == (not a) or (not b): "should" cannot hold a negation.
        let error = refused(&json!({"$not": {"$and": [{"a": "1"}, {"b": "2"}]}}));
        assert!(matches!(error, FilterError::NotExpressible(_)), "{error}");
        // but not (a and b) with ONE member is just not a
        assert!(translate(&json!({"$not": {"$and": [{"a": "1"}]}})).is_ok());
    }

    #[test]
    fn or_of_negations_or_groups_is_refused() {
        for filter in [
            json!({"$or": [{"a": {"$ne": "1"}}, {"b": "2"}]}),
            json!({"$or": [{"$and": [{"a": "1"}, {"b": "2"}]}, {"c": "3"}]}),
            json!({"$and": [{"$or": [{"a": "1"}, {"b": "2"}]}, {"$or": [{"c": "3"}, {"d": "4"}]}]}),
        ] {
            let error = refused(&filter);
            assert!(
                matches!(error, FilterError::NotExpressible(_)),
                "{filter}: {error}"
            );
        }
    }

    #[test]
    fn range_pattern_and_presence_operators_are_refused() {
        for operator in [
            "$lt", "$lte", "$gt", "$gte", "$between", "$like", "$ilike", "$exists",
        ] {
            let operand = match operator {
                "$between" => json!([1, 2]),
                "$like" | "$ilike" => json!("a%"),
                "$exists" => json!(true),
                _ => json!(3),
            };
            let filter = json!({"n": {operator: operand}});
            match refused(&filter) {
                FilterError::UnsupportedOperator {
                    operator: named,
                    reason,
                } => {
                    assert_eq!(named, operator);
                    assert!(!reason.is_empty());
                }
                other => panic!("{operator}: {other}"),
            }
        }
        // The old SDK collection filter used $exists; it is refused loudly.
        let message = refused(&json!({"type": {"$exists": false}})).to_string();
        assert!(message.contains("$exists"), "{message}");
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
                json!({"$and": [{key: {"$in": ["x"]}}]}),
                json!({"$or": [{"a": "1"}, {key: "x"}]}),
                json!({"$not": {key: "x"}}),
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
    fn list_limits_are_enforced_before_the_call() {
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
