//! Scope injection and filter translation.
//!
//! Every Qdrant filter this service sends has the shape
//!
//! ```text
//! must: [ project_id == P, <scope conditions>..., <the caller's filter, nested> ]
//! ```
//!
//! P is the verified project ([`crate::auth::Caller::project`]). The
//! caller's filter is one nested condition inside `must`, so its `should`
//! and `must_not` can only remove points from the scope, never add any. The
//! caller's filter cannot name `project_id`, `source`, `namespace_id`,
//! `generation`, `text` or any unknown key, at any nesting depth: a nested
//! filter is checked like the top-level one, and the whole tree is bounded
//! ([`MAX_DEPTH`] levels, [`MAX_TOTAL_CONDITIONS`] conditions).

use qdrant_client::qdrant::{Condition, Filter, Range};
use tonic::Status;

use crate::layout::{self, key};
use crate::pb;

/// The most conditions in one list of a caller's filter.
const MAX_CONDITIONS: usize = 64;
/// The most values in one `any` match.
const MAX_ANY_VALUES: usize = 1024;
/// The most namespaces one read names.
pub const MAX_NAMESPACES: usize = 64;
/// The deepest a caller's filter nests, counting the filter itself.
pub const MAX_DEPTH: usize = 8;
/// The most conditions in a caller's whole filter tree, nested filter
/// conditions included.
pub const MAX_TOTAL_CONDITIONS: usize = 1024;

/// The project, and the narrower scope conditions of one request.
#[derive(Clone, Debug)]
pub struct Scope {
    conditions: Vec<Condition>,
}

impl Scope {
    /// The whole of one project.
    #[must_use]
    pub fn project(project_id: i64) -> Self {
        Self {
            conditions: vec![Condition::matches(key::PROJECT_ID, project_id.to_string())],
        }
    }

    /// Narrows to a source.
    #[must_use]
    pub fn source(mut self, source: &str) -> Self {
        self.conditions
            .push(Condition::matches(key::SOURCE, source.to_owned()));
        self
    }

    /// Narrows to some namespaces (none narrows nothing).
    #[must_use]
    pub fn namespaces(mut self, namespace_ids: Vec<String>) -> Self {
        match namespace_ids.len() {
            0 => {}
            1 => self.conditions.push(Condition::matches(
                key::NAMESPACE_ID,
                namespace_ids.into_iter().next().unwrap_or_default(),
            )),
            _ => self
                .conditions
                .push(Condition::matches(key::NAMESPACE_ID, namespace_ids)),
        }
        self
    }

    /// Narrows to some namespaces, each listed in `active` to its active
    /// generation or to points without a generation; the others to the
    /// namespace alone.
    #[must_use]
    pub fn namespaces_with_active(
        mut self,
        namespace_ids: &[String],
        active: &[(String, String)],
    ) -> Self {
        let plain: Vec<String> = namespace_ids
            .iter()
            .filter(|id| !active.iter().any(|(namespace, _)| namespace == *id))
            .cloned()
            .collect();
        let mut either = Vec::with_capacity(active.len() + 1);
        match plain.len() {
            0 => {}
            1 => either.push(Condition::matches(
                key::NAMESPACE_ID,
                plain.into_iter().next().unwrap_or_default(),
            )),
            _ => either.push(Condition::matches(key::NAMESPACE_ID, plain)),
        }
        for (namespace_id, generation) in active {
            either.push(Condition::from(Filter {
                must: vec![Condition::matches(key::NAMESPACE_ID, namespace_id.clone())],
                should: vec![
                    Condition::matches(key::GENERATION, generation.clone()),
                    Condition::is_empty(key::GENERATION),
                ],
                ..Filter::default()
            }));
        }
        if !either.is_empty() {
            self.conditions
                .push(Condition::from(Filter::should(either)));
        }
        self
    }

    /// Narrows to one generation.
    #[must_use]
    pub fn generation(mut self, generation: Option<&str>) -> Self {
        if let Some(generation) = generation {
            self.conditions
                .push(Condition::matches(key::GENERATION, generation.to_owned()));
        }
        self
    }

    /// Narrows to the points one memory owner holds (`user:<id>` or
    /// `project`), as the token decides it. The only way an `owner` condition
    /// is ever built: no request carries one.
    #[must_use]
    pub fn owned_by(mut self, owner: &str) -> Self {
        self.conditions
            .push(Condition::matches(key::OWNER, owner.to_owned()));
        self
    }

    /// Narrows to one memory toolkit's points.
    #[must_use]
    pub fn toolkit(mut self, toolkit_id: i64) -> Self {
        self.conditions
            .push(Condition::matches(key::TOOLKIT_ID, toolkit_id.to_string()));
        self
    }

    /// Narrows to what a memory binding names: its owner and its toolkit.
    #[must_use]
    pub fn bound_to(self, binding: &crate::auth::MemoryBinding) -> Self {
        self.owned_by(&binding.owner).toolkit(binding.toolkit_id)
    }

    /// Every project (an administrator's cross-project operation); the
    /// callers add what they narrow by.
    #[must_use]
    pub fn every_project() -> Self {
        Self {
            conditions: Vec::new(),
        }
    }

    /// Adds one condition the service built (a delete selector).
    #[must_use]
    pub fn with(mut self, condition: Condition) -> Self {
        self.conditions.push(condition);
        self
    }

    /// Adds a `must_not` the service built (a delete selector).
    #[must_use]
    pub fn without(mut self, condition: Condition) -> Self {
        self.conditions
            .push(Condition::from(Filter::must_not([condition])));
        self
    }

    /// The Qdrant filter: the scope, and the caller's filter nested inside it.
    #[must_use]
    pub fn filter(&self, narrowing: Option<Filter>) -> Filter {
        let mut must = self.conditions.clone();
        if let Some(narrowing) = narrowing {
            must.push(Condition::from(narrowing));
        }
        Filter::must(must)
    }
}

/// Validates a read scope and turns it into a [`Scope`] for `project_id`.
///
/// `require_namespace` makes at least one namespace id mandatory.
///
/// # Errors
/// `INVALID_ARGUMENT` for a missing source, a bad or missing namespace id, a
/// bad generation, or bad active generations.
pub fn read_scope(
    project_id: i64,
    scope: &pb::Scope,
    require_namespace: bool,
) -> Result<Scope, Status> {
    let source = layout::source_keyword(scope.source)?;
    if scope.namespace_ids.len() > MAX_NAMESPACES {
        return Err(Status::invalid_argument("at most 64 namespace_ids"));
    }
    if require_namespace && scope.namespace_ids.is_empty() {
        return Err(Status::invalid_argument("namespace_ids is required"));
    }
    for namespace_id in &scope.namespace_ids {
        layout::namespace_uuid(namespace_id)?;
    }
    let generation = layout::generation(&scope.generation)?;
    let active = active_generations(scope)?;
    if active.is_empty() {
        return Ok(Scope::project(project_id)
            .source(source)
            .namespaces(scope.namespace_ids.clone())
            .generation(generation));
    }
    if generation.is_some() {
        return Err(Status::invalid_argument(
            "generation and active_generations cannot be combined",
        ));
    }
    Ok(Scope::project(project_id)
        .source(source)
        .namespaces_with_active(&scope.namespace_ids, &active))
}

/// Validates `active_generations`: each names one of the scope's
/// namespaces, at most once, with a valid, non-empty generation.
fn active_generations(scope: &pb::Scope) -> Result<Vec<(String, String)>, Status> {
    let mut active: Vec<(String, String)> = Vec::with_capacity(scope.active_generations.len());
    for entry in &scope.active_generations {
        if !scope.namespace_ids.contains(&entry.namespace_id) {
            return Err(Status::invalid_argument(
                "an active generation names a namespace the scope does not list",
            ));
        }
        if active
            .iter()
            .any(|(namespace, _)| *namespace == entry.namespace_id)
        {
            return Err(Status::invalid_argument(
                "at most one active generation per namespace",
            ));
        }
        let Some(generation) = layout::generation(&entry.generation)? else {
            return Err(Status::invalid_argument(
                "an active generation must not be empty",
            ));
        };
        active.push((entry.namespace_id.clone(), generation.to_owned()));
    }
    Ok(active)
}

/// Translates a caller's filter.
///
/// # Errors
/// `INVALID_ARGUMENT` for a key that is not allowed (at any depth), an
/// empty match or nested filter, a bad range, more conditions or values
/// than allowed, or a tree deeper than [`MAX_DEPTH`].
pub fn translate(filter: Option<&pb::Filter>) -> Result<Option<Filter>, Status> {
    let Some(filter) = filter else {
        return Ok(None);
    };
    if is_empty(filter) {
        return Ok(None);
    }
    let mut budget = MAX_TOTAL_CONDITIONS;
    Ok(Some(nested(filter, 1, &mut budget)?))
}

fn is_empty(filter: &pb::Filter) -> bool {
    filter.must.is_empty() && filter.should.is_empty() && filter.must_not.is_empty()
}

fn nested(filter: &pb::Filter, depth: usize, budget: &mut usize) -> Result<Filter, Status> {
    if depth > MAX_DEPTH {
        return Err(Status::invalid_argument(
            "a filter nests at most 8 levels deep",
        ));
    }
    Ok(Filter {
        must: conditions(&filter.must, depth, budget)?,
        should: conditions(&filter.should, depth, budget)?,
        must_not: conditions(&filter.must_not, depth, budget)?,
        ..Filter::default()
    })
}

fn conditions(
    list: &[pb::Condition],
    depth: usize,
    budget: &mut usize,
) -> Result<Vec<Condition>, Status> {
    if list.len() > MAX_CONDITIONS {
        return Err(Status::invalid_argument(
            "at most 64 conditions per filter list",
        ));
    }
    list.iter()
        .map(|item| condition(item, depth, budget))
        .collect()
}

fn condition(
    condition: &pb::Condition,
    depth: usize,
    budget: &mut usize,
) -> Result<Condition, Status> {
    *budget = budget
        .checked_sub(1)
        .ok_or_else(|| Status::invalid_argument("a filter holds at most 1024 conditions in all"))?;
    if let Some(pb::condition::Match::Filter(inner)) = &condition.r#match {
        if !condition.key.is_empty() {
            return Err(Status::invalid_argument(
                "a nested filter condition takes no key",
            ));
        }
        if is_empty(inner) {
            return Err(Status::invalid_argument(
                "a nested filter must not be empty",
            ));
        }
        return Ok(Condition::from(nested(inner, depth + 1, budget)?));
    }
    if !filterable_key(&condition.key) {
        return Err(Status::invalid_argument(format!(
            "filter key {:?} is not allowed",
            truncate(&condition.key)
        )));
    }
    let key = condition.key.clone();
    match &condition.r#match {
        Some(pb::condition::Match::Keyword(value)) => Ok(Condition::matches(key, value.clone())),
        Some(pb::condition::Match::Any(list)) => {
            if list.values.is_empty() || list.values.len() > MAX_ANY_VALUES {
                return Err(Status::invalid_argument("any holds 1 to 1024 values"));
            }
            Ok(Condition::matches(key, list.values.clone()))
        }
        Some(pb::condition::Match::Integer(value)) => Ok(Condition::matches(key, *value)),
        Some(pb::condition::Match::Boolean(value)) => Ok(Condition::matches(key, *value)),
        Some(pb::condition::Match::Range(range)) => {
            if !key.starts_with("metadata.") {
                return Err(Status::invalid_argument(
                    "a range applies to metadata.<path> keys only",
                ));
            }
            let bounds = [range.gt, range.gte, range.lt, range.lte];
            if bounds.iter().all(Option::is_none) {
                return Err(Status::invalid_argument("a range needs at least one bound"));
            }
            if bounds.iter().flatten().any(|bound| !bound.is_finite()) {
                return Err(Status::invalid_argument("range bounds must be finite"));
            }
            Ok(Condition::range(
                key,
                Range {
                    lt: range.lt,
                    gt: range.gt,
                    gte: range.gte,
                    lte: range.lte,
                },
            ))
        }
        Some(pb::condition::Match::IsEmpty(_)) => Ok(Condition::is_empty(key)),
        Some(pb::condition::Match::IsNull(_)) => Ok(Condition::is_null(key)),
        Some(pb::condition::Match::Filter(_)) | None => {
            Err(Status::invalid_argument("a condition needs a match"))
        }
    }
}

/// Whether a caller's filter may name `key`.
#[must_use]
pub fn filterable_key(key: &str) -> bool {
    if matches!(
        key,
        key::DOCUMENT_KEY
            | key::DOCUMENT_VERSION
            | key::PARENT_ID
            | key::CHUNK_ID
            | key::CHUNK_TYPE
            | key::ACL
    ) {
        return true;
    }
    let Some(path) = key
        .strip_prefix(key::METADATA)
        .and_then(|rest| rest.strip_prefix('.'))
    else {
        return false;
    };
    path.len() <= 256
        && path.split('.').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        })
}

fn truncate(value: &str) -> String {
    value.chars().take(64).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use qdrant_client::qdrant::condition::ConditionOneOf;

    fn keyword(key: &str, value: &str) -> pb::Condition {
        pb::Condition {
            key: key.to_owned(),
            r#match: Some(pb::condition::Match::Keyword(value.to_owned())),
        }
    }

    #[test]
    fn reserved_and_unknown_keys_are_refused() {
        for key in [
            "project_id",
            "source",
            "namespace_id",
            "generation",
            "owner",
            "toolkit_id",
            "text",
            "metadata",
            "metadata.",
            "metadata.a..b",
            "metadata.a-b",
            "other",
        ] {
            let filter = pb::Filter {
                should: vec![keyword(key, "x")],
                ..pb::Filter::default()
            };
            let status = translate(Some(&filter)).expect_err(key);
            assert_eq!(status.code(), tonic::Code::InvalidArgument, "{key}");
        }
        assert!(filterable_key("metadata.source.path_1"));
        assert!(filterable_key("chunk_type"));
    }

    #[test]
    fn the_callers_filter_is_nested_under_the_project() {
        let narrowing = translate(Some(&pb::Filter {
            should: vec![keyword("chunk_type", "code")],
            must_not: vec![keyword("acl", "x")],
            ..pb::Filter::default()
        }))
        .expect("valid");
        let filter = Scope::project(7).source("deepwiki").filter(narrowing);
        assert!(filter.should.is_empty() && filter.must_not.is_empty());
        assert_eq!(filter.must.len(), 3);
        let Some(ConditionOneOf::Field(first)) = &filter.must[0].condition_one_of else {
            panic!("the project comes first");
        };
        assert_eq!(first.key, "project_id");
        assert!(matches!(
            filter.must[2].condition_one_of,
            Some(ConditionOneOf::Filter(_))
        ));
    }

    #[test]
    fn read_scopes_validate_namespaces() {
        let scope = pb::Scope {
            source: pb::Source::ToolkitIndex as i32,
            ..pb::Scope::default()
        };
        assert!(read_scope(7, &scope, true).is_err());
        assert!(read_scope(7, &scope, false).is_ok());
        let bad = pb::Scope {
            namespace_ids: vec!["nope".to_owned()],
            ..scope.clone()
        };
        assert!(read_scope(7, &bad, false).is_err());
        let too_many = pb::Scope {
            namespace_ids: vec!["0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned(); 65],
            ..scope
        };
        assert!(read_scope(7, &too_many, false).is_err());
    }

    fn nest(filter: pb::Filter) -> pb::Condition {
        pb::Condition {
            key: String::new(),
            r#match: Some(pb::condition::Match::Filter(filter)),
        }
    }

    /// A filter `depth` levels deep, its innermost list holding `leaf`.
    fn deep(depth: usize, leaf: pb::Condition) -> pb::Filter {
        let mut filter = pb::Filter {
            must: vec![leaf],
            ..pb::Filter::default()
        };
        for _ in 1..depth {
            filter = pb::Filter {
                should: vec![nest(filter)],
                ..pb::Filter::default()
            };
        }
        filter
    }

    #[test]
    fn reserved_keys_are_refused_inside_nested_filters() {
        for key in ["project_id", "source", "namespace_id", "generation", "text"] {
            for depth in [2, 5, MAX_DEPTH] {
                let status = translate(Some(&deep(depth, keyword(key, "x")))).expect_err(key);
                assert_eq!(status.code(), tonic::Code::InvalidArgument, "{key} {depth}");
                assert!(status.message().contains("not allowed"), "{key} {depth}");
            }
            for leaf in [
                pb::Condition {
                    key: key.to_owned(),
                    r#match: Some(pb::condition::Match::IsEmpty(pb::IsEmpty {})),
                },
                pb::Condition {
                    key: key.to_owned(),
                    r#match: Some(pb::condition::Match::IsNull(pb::IsNull {})),
                },
            ] {
                assert!(translate(Some(&deep(3, leaf))).is_err(), "{key}");
            }
        }
        // A nested filter condition cannot carry a key either.
        let keyed = pb::Filter {
            must: vec![pb::Condition {
                key: "project_id".to_owned(),
                r#match: Some(pb::condition::Match::Filter(deep(1, keyword("acl", "x")))),
            }],
            ..pb::Filter::default()
        };
        assert!(translate(Some(&keyed)).is_err());
    }

    #[test]
    fn depth_and_total_conditions_are_capped() {
        assert!(translate(Some(&deep(MAX_DEPTH, keyword("acl", "x")))).is_ok());
        let status = translate(Some(&deep(MAX_DEPTH + 1, keyword("acl", "x")))).expect_err("deep");
        assert!(status.message().contains("8 levels"), "{status:?}");
        // 64 nested filters of 16 leaves each: 64 + 1024 conditions.
        let wide = pb::Filter {
            must: (0..64)
                .map(|_| {
                    nest(pb::Filter {
                        should: (0..16).map(|_| keyword("acl", "x")).collect(),
                        ..pb::Filter::default()
                    })
                })
                .collect(),
            ..pb::Filter::default()
        };
        let status = translate(Some(&wide)).expect_err("too many");
        assert!(status.message().contains("1024"), "{status:?}");
        let empty = pb::Filter {
            must: vec![nest(pb::Filter::default())],
            ..pb::Filter::default()
        };
        assert!(translate(Some(&empty)).is_err(), "an empty nested filter");
    }

    #[test]
    fn ranges_take_metadata_keys_and_finite_bounds() {
        let range = |key: &str, range: pb::Range| pb::Filter {
            must: vec![pb::Condition {
                key: key.to_owned(),
                r#match: Some(pb::condition::Match::Range(range)),
            }],
            ..pb::Filter::default()
        };
        let ok = pb::Range {
            gte: Some(1.0),
            lt: Some(2.5),
            ..pb::Range::default()
        };
        let translated = translate(Some(&range("metadata.year", ok))).expect("valid");
        let translated = translated.expect("some");
        let Some(ConditionOneOf::Field(field)) = &translated.must[0].condition_one_of else {
            panic!("a field condition");
        };
        let bounds = field.range.expect("range");
        assert_eq!((bounds.gte, bounds.lt), (Some(1.0), Some(2.5)));
        for (key, bad) in [
            ("document_key", ok),
            ("metadata.year", pb::Range::default()),
            (
                "metadata.year",
                pb::Range {
                    gt: Some(f64::NAN),
                    ..pb::Range::default()
                },
            ),
            (
                "metadata.year",
                pb::Range {
                    lte: Some(f64::INFINITY),
                    ..pb::Range::default()
                },
            ),
        ] {
            assert!(translate(Some(&range(key, bad))).is_err(), "{key} {bad:?}");
        }
    }

    #[test]
    fn active_generations_keep_unversioned_points_and_exclude_other_builds() {
        let other = "6f1e2d3c-4b5a-4968-8776-5a4b3c2d1e0f".to_owned();
        let first = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned();
        let scope = pb::Scope {
            source: pb::Source::ToolkitIndex as i32,
            namespace_ids: vec![first.clone(), other.clone()],
            active_generations: vec![pb::ActiveGeneration {
                namespace_id: first.clone(),
                generation: "g1".to_owned(),
            }],
            ..pb::Scope::default()
        };
        let filter = read_scope(7, &scope, true).expect("valid").filter(None);
        // project, source, then one "either" group over the namespaces.
        assert_eq!(filter.must.len(), 3);
        let Some(ConditionOneOf::Filter(either)) = &filter.must[2].condition_one_of else {
            panic!("a namespace group");
        };
        assert_eq!(either.should.len(), 2);
        let Some(ConditionOneOf::Filter(pinned)) = &either.should[1].condition_one_of else {
            panic!("the pinned namespace");
        };
        assert_eq!(pinned.should.len(), 2, "generation == g1 or no generation");
        assert!(matches!(
            pinned.should[1].condition_one_of,
            Some(ConditionOneOf::IsEmpty(_))
        ));
        for bad in [
            pb::Scope {
                generation: "g1".to_owned(),
                ..scope.clone()
            },
            pb::Scope {
                active_generations: vec![pb::ActiveGeneration {
                    namespace_id: "8a8a8a8a-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned(),
                    generation: "g1".to_owned(),
                }],
                ..scope.clone()
            },
            pb::Scope {
                active_generations: vec![
                    scope.active_generations[0].clone(),
                    scope.active_generations[0].clone(),
                ],
                ..scope.clone()
            },
            pb::Scope {
                active_generations: vec![pb::ActiveGeneration {
                    namespace_id: first.clone(),
                    generation: String::new(),
                }],
                ..scope.clone()
            },
        ] {
            assert!(read_scope(7, &bad, true).is_err(), "{bad:?}");
        }
    }
}
