//! The SDK's reranking rules (`VectorStoreWrapperBase._apply_reranking`).
//!
//! A config maps a metadata field to `{weight, rules}`. The rules:
//!
//! * `contains`: when the field and the rule are strings and the field
//!   holds the rule, case-insensitively, the score becomes
//!   `score * (1 + weight)`;
//! * `priority`: the same when `str(field)` equals `str(rule)`,
//!   case-insensitively;
//! * `sort`: `"asc"` or `"desc"` orders the results by the field (results
//!   without it first when ascending), ties broken by score.
//!
//! The boosts of one field apply once per result however many rules match
//! (the SDK computes each from the result's score at the start of that
//! field). Results are then ordered by score, descending, unless a `sort`
//! rule ordered them.

use std::cmp::Ordering;

use serde_json::{Map, Value};

use crate::error::{Error, Result};
use crate::hit::Hit;
use crate::pyfmt;

/// The rules of one metadata field.
#[derive(Clone, Debug, PartialEq)]
pub struct FieldRerank {
    /// The metadata key the rules read.
    pub field: String,
    /// The boost weight (default 1.0).
    pub weight: f64,
    /// `contains`, if configured.
    pub contains: Option<Value>,
    /// `priority`, if configured.
    pub priority: Option<Value>,
    /// `sort`, if configured.
    pub sort: Option<Value>,
}

/// Parses `reranking_config` or the legacy `reranker` argument.
///
/// # Errors
///
/// [`Error::InvalidArgument`] when a part has the wrong JSON type.
pub fn parse(config: &Value) -> Result<Vec<FieldRerank>> {
    let Value::Object(fields) = config else {
        return Err(Error::InvalidArgument(
            "the reranking config must be an object of {field: {weight, rules}}".to_owned(),
        ));
    };
    fields
        .iter()
        .map(|(field, entry)| {
            let Value::Object(entry) = entry else {
                return Err(Error::InvalidArgument(format!(
                    "the reranking config for {field:?} must be an object"
                )));
            };
            let weight = match entry.get("weight") {
                None => 1.0,
                Some(value) => value.as_f64().ok_or_else(|| {
                    Error::InvalidArgument(format!(
                        "the reranking weight for {field:?} must be a number"
                    ))
                })?,
            };
            let empty = Map::new();
            let rules = match entry.get("rules") {
                None => &empty,
                Some(Value::Object(rules)) => rules,
                Some(_) => {
                    return Err(Error::InvalidArgument(format!(
                        "the reranking rules for {field:?} must be an object"
                    )));
                }
            };
            Ok(FieldRerank {
                field: field.clone(),
                weight,
                contains: rules.get("contains").cloned(),
                priority: rules.get("priority").cloned(),
                sort: rules.get("sort").cloned(),
            })
        })
        .collect()
}

/// Picks the config the SDK would use: a non-empty `reranking_config`, else
/// a non-empty legacy `reranker`, else none.
///
/// # Errors
///
/// [`Error::InvalidArgument`] when the chosen config is malformed.
pub fn choose(
    reranking_config: Option<&Value>,
    reranker: Option<&Value>,
) -> Result<Vec<FieldRerank>> {
    for candidate in [reranking_config, reranker].into_iter().flatten() {
        if matches!(candidate, Value::Null) || candidate.as_object().is_some_and(Map::is_empty) {
            continue;
        }
        return parse(candidate);
    }
    Ok(Vec::new())
}

/// Applies the rules and orders the hits.
///
/// # Errors
///
/// [`Error::InvalidArgument`] for a `sort` rule that is not a string or
/// that would compare values of different types.
pub fn apply(mut hits: Vec<Hit>, rules: &[FieldRerank]) -> Result<Vec<Hit>> {
    if hits.is_empty() {
        return Ok(hits);
    }
    for rule in rules {
        for hit in &mut hits {
            let Some(value) = hit
                .metadata
                .get(&rule.field)
                .filter(|value| !value.is_null())
            else {
                continue;
            };
            let base = hit.score;
            let mut boosted = None;
            if let (Some(Value::String(wanted)), Value::String(actual)) = (&rule.contains, value)
                && actual.to_lowercase().contains(&wanted.to_lowercase())
            {
                boosted = Some(base * (1.0 + rule.weight));
            }
            if let Some(wanted) = &rule.priority
                && pyfmt::display(value).to_lowercase() == pyfmt::display(wanted).to_lowercase()
            {
                boosted = Some(base * (1.0 + rule.weight));
            }
            if let Some(score) = boosted {
                hit.score = score;
            }
        }
    }
    let mut sorted = false;
    for rule in rules {
        let Some(direction) = &rule.sort else {
            continue;
        };
        let Value::String(direction) = direction else {
            return Err(Error::InvalidArgument(format!(
                "the sort rule for {:?} must be \"asc\" or \"desc\"",
                rule.field
            )));
        };
        sort_by_field(&mut hits, &rule.field, direction.to_lowercase() == "desc")?;
        sorted = true;
    }
    if !sorted {
        hits.sort_by(|left, right| right.score.total_cmp(&left.score));
    }
    Ok(hits)
}

#[derive(PartialEq)]
enum Kind {
    Number,
    Text,
}

fn kind_of(value: &Value) -> Option<Kind> {
    match value {
        Value::Number(_) | Value::Bool(_) => Some(Kind::Number),
        Value::String(_) => Some(Kind::Text),
        _ => None,
    }
}

fn number_of(value: &Value) -> f64 {
    match value {
        Value::Bool(flag) => f64::from(u8::from(*flag)),
        other => other.as_f64().unwrap_or(f64::NAN),
    }
}

/// Python's `sort(key=(present, value, score), reverse=descending)`. The
/// sort is stable and `reverse` keeps equal keys in their original order,
/// which a reversed comparator on a stable sort does too.
fn sort_by_field(hits: &mut [Hit], field: &str, descending: bool) -> Result<()> {
    fn present<'a>(hit: &'a Hit, field: &str) -> Option<&'a Value> {
        hit.metadata.get(field).filter(|value| !value.is_null())
    }
    let mut first: Option<Kind> = None;
    for hit in hits.iter() {
        let Some(value) = present(hit, field) else {
            continue;
        };
        let Some(kind) = kind_of(value) else {
            return Err(Error::InvalidArgument(format!(
                "cannot sort by {field:?}: it holds a list or an object"
            )));
        };
        match &first {
            None => first = Some(kind),
            Some(seen) if *seen == kind => {}
            Some(_) => {
                return Err(Error::InvalidArgument(format!(
                    "cannot sort by {field:?}: its values are a mix of numbers and text"
                )));
            }
        }
    }
    let compare = |left: &Hit, right: &Hit| -> Ordering {
        let ordering = match (present(left, field), present(right, field)) {
            (None, None) => Ordering::Equal,
            (None, Some(_)) => Ordering::Less,
            (Some(_), None) => Ordering::Greater,
            (Some(a), Some(b)) => match (a, b) {
                (Value::String(a), Value::String(b)) => a.cmp(b),
                (a, b) => number_of(a).total_cmp(&number_of(b)),
            },
        };
        ordering.then_with(|| left.score.total_cmp(&right.score))
    };
    if descending {
        hits.sort_by(|left, right| compare(right, left));
    } else {
        hits.sort_by(compare);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn hit(key: &str, score: f64, metadata: &Value) -> Hit {
        Hit {
            key: key.to_owned(),
            page_content: key.to_owned(),
            metadata: metadata.as_object().cloned().unwrap_or_default(),
            score,
            document_key: key.to_owned(),
            chunk_id: "0".to_owned(),
        }
    }

    fn keys(hits: &[Hit]) -> Vec<&str> {
        hits.iter().map(|hit| hit.key.as_str()).collect()
    }

    fn rules(config: &Value) -> Vec<FieldRerank> {
        parse(config).expect("valid")
    }

    #[test]
    fn contains_multiplies_by_one_plus_weight() {
        let hits = vec![
            hit("a", 0.50, &json!({"title": "Release notes"})),
            hit("b", 0.40, &json!({"title": "Deployment guide"})),
            hit("c", 0.45, &json!({})),
        ];
        let config = rules(&json!({"title": {"weight": 0.5, "rules": {"contains": "DEPLOY"}}}));
        let out = apply(hits, &config).unwrap();
        assert_eq!(keys(&out), ["b", "a", "c"]);
        assert!((out[0].score - 0.6).abs() < 1e-12, "{}", out[0].score);
    }

    #[test]
    fn priority_matches_the_text_of_any_value_case_insensitively() {
        let hits = vec![
            hit("a", 0.30, &json!({"kind": "Spec"})),
            hit("b", 0.30, &json!({"kind": "note"})),
            hit("c", 0.20, &json!({"version": 3})),
            hit("d", 0.20, &json!({"flag": true})),
        ];
        let config = rules(&json!({
            "kind": {"weight": 1.0, "rules": {"priority": "spec"}},
            "version": {"weight": 1.0, "rules": {"priority": 3}},
            "flag": {"weight": 1.0, "rules": {"priority": "True"}}
        }));
        let out = apply(hits, &config).unwrap();
        assert_eq!(
            out.iter()
                .map(|h| (h.key.as_str(), h.score))
                .collect::<Vec<_>>(),
            [("a", 0.6), ("c", 0.4), ("d", 0.4), ("b", 0.3)]
        );
    }

    #[test]
    fn both_rules_of_one_field_boost_once() {
        let hits = vec![hit("a", 0.5, &json!({"t": "ops"}))];
        let config =
            rules(&json!({"t": {"weight": 1.0, "rules": {"contains": "op", "priority": "ops"}}}));
        let out = apply(hits, &config).unwrap();
        assert!((out[0].score - 1.0).abs() < 1e-12);
    }

    #[test]
    fn contains_needs_two_strings() {
        let hits = vec![hit("a", 0.5, &json!({"n": 12}))];
        let config = rules(&json!({"n": {"rules": {"contains": 1}}}));
        assert!((apply(hits, &config).unwrap()[0].score - 0.5).abs() < 1e-12);
    }

    #[test]
    fn boosts_of_two_fields_compound() {
        let hits = vec![hit("a", 0.2, &json!({"x": "y", "p": "q"}))];
        let config = rules(&json!({
            "x": {"weight": 1.0, "rules": {"priority": "y"}},
            "p": {"weight": 0.5, "rules": {"priority": "q"}},
        }));
        assert!((apply(hits, &config).unwrap()[0].score - 0.6).abs() < 1e-12);
    }

    #[test]
    fn sort_orders_by_a_field_and_breaks_ties_by_score() {
        let hits = vec![
            hit("a", 0.9, &json!({"year": 2022})),
            hit("b", 0.5, &json!({"year": 2024})),
            hit("c", 0.7, &json!({"year": 2024})),
            hit("d", 0.8, &json!({})),
        ];
        let desc = apply(
            hits.clone(),
            &rules(&json!({"year": {"rules": {"sort": "desc"}}})),
        )
        .unwrap();
        assert_eq!(keys(&desc), ["c", "b", "a", "d"]);
        let asc = apply(hits, &rules(&json!({"year": {"rules": {"sort": "ASC"}}}))).unwrap();
        assert_eq!(keys(&asc), ["d", "a", "b", "c"]);
    }

    #[test]
    fn sort_by_text_and_mixed_types() {
        let text = vec![
            hit("a", 0.1, &json!({"n": "pear"})),
            hit("b", 0.1, &json!({"n": "apple"})),
        ];
        let out = apply(text, &rules(&json!({"n": {"rules": {"sort": "asc"}}}))).unwrap();
        assert_eq!(keys(&out), ["b", "a"]);
        let mixed = vec![
            hit("a", 0.1, &json!({"n": 1})),
            hit("b", 0.1, &json!({"n": "x"})),
        ];
        assert!(apply(mixed, &rules(&json!({"n": {"rules": {"sort": "asc"}}}))).is_err());
        let bad = vec![hit("a", 0.1, &json!({"n": 1}))];
        assert!(apply(bad, &rules(&json!({"n": {"rules": {"sort": 1}}}))).is_err());
    }

    #[test]
    fn without_rules_results_sort_by_score() {
        let hits = vec![hit("a", 0.2, &json!({})), hit("b", 0.9, &json!({}))];
        assert_eq!(keys(&apply(hits, &[]).unwrap()), ["b", "a"]);
    }

    #[test]
    fn the_reranking_config_wins_over_the_legacy_reranker() {
        let new = json!({"a": {"rules": {"sort": "asc"}}});
        let old = json!({"b": {"rules": {"sort": "asc"}}});
        let chosen = choose(Some(&new), Some(&old)).unwrap();
        assert_eq!(chosen[0].field, "a");
        let fallback = choose(Some(&json!({})), Some(&old)).unwrap();
        assert_eq!(fallback[0].field, "b");
        assert!(choose(None, Some(&json!({}))).unwrap().is_empty());
    }

    #[test]
    fn malformed_configs_are_refused() {
        for config in [
            json!([1]),
            json!({"a": 1}),
            json!({"a": {"weight": "x"}}),
            json!({"a": {"rules": []}}),
        ] {
            assert!(parse(&config).is_err(), "{config}");
        }
    }
}
