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
//! `generation`, `text` or any unknown key.

use qdrant_client::qdrant::{Condition, Filter};
use tonic::Status;

use crate::layout::{self, key};
use crate::pb;

/// The most conditions in one list of a caller's filter.
const MAX_CONDITIONS: usize = 64;
/// The most values in one `any` match.
const MAX_ANY_VALUES: usize = 1024;
/// The most namespaces one read names.
pub const MAX_NAMESPACES: usize = 64;

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

    /// Narrows to one generation.
    #[must_use]
    pub fn generation(mut self, generation: Option<&str>) -> Self {
        if let Some(generation) = generation {
            self.conditions
                .push(Condition::matches(key::GENERATION, generation.to_owned()));
        }
        self
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
/// `INVALID_ARGUMENT` for a missing source, a bad or missing namespace id, or
/// a bad generation.
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
    Ok(Scope::project(project_id)
        .source(source)
        .namespaces(scope.namespace_ids.clone())
        .generation(generation))
}

/// Translates a caller's filter.
///
/// # Errors
/// `INVALID_ARGUMENT` for a key that is not allowed, an empty match, or
/// more conditions or values than allowed.
pub fn translate(filter: Option<&pb::Filter>) -> Result<Option<Filter>, Status> {
    let Some(filter) = filter else {
        return Ok(None);
    };
    if filter.must.is_empty() && filter.should.is_empty() && filter.must_not.is_empty() {
        return Ok(None);
    }
    let translated = Filter {
        must: conditions(&filter.must)?,
        should: conditions(&filter.should)?,
        must_not: conditions(&filter.must_not)?,
        ..Filter::default()
    };
    Ok(Some(translated))
}

fn conditions(list: &[pb::Condition]) -> Result<Vec<Condition>, Status> {
    if list.len() > MAX_CONDITIONS {
        return Err(Status::invalid_argument(
            "at most 64 conditions per filter list",
        ));
    }
    list.iter().map(condition).collect()
}

fn condition(condition: &pb::Condition) -> Result<Condition, Status> {
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
        None => Err(Status::invalid_argument("a condition needs a match")),
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
}
