//! The tenancy key of the index: which platform project owns a wiki.
//!
//! Migration 0005. A `wiki_id` is derived from the repository
//! (`owner--repo--branch`, `artifact--bucket--prefix--branch`), so two
//! projects that index the same repository name derive the same id. Before
//! 0005 every index table was keyed by `wiki_id` alone, in one database
//! shared by all projects.
//!
//! Every index row now carries the owning project, and every statement
//! filters, inserts and deletes by `(project_id, wiki_id)`. The types here
//! make an unscoped access hard to write by accident:
//!
//! * [`ProjectScope`] has no `Default` and no public field. It is made by
//!   [`ProjectScope::new`] (a positive id) or
//!   [`ProjectScope::from_arguments`] (the host's reserved argument).
//! * [`WikiKey`] pairs a scope with a wiki id. The readers
//!   (`IndexReader`, `UnifiedDb`) and a build (`BuildSpace::begin`) take
//!   one, so no reader or build exists without a project.
//!
//! WHERE THE PROJECT COMES FROM. The Go sub-application host sets
//! [`PROJECT_ARG`] on every call to this socket, from the project of the
//! verified identity signature the platform facade put on the hop (the
//! project in the facade's URL, which the facade authorised the caller
//! for). The host overwrites a caller-supplied value. The engine trusts
//! the argument because only the host can reach this Unix socket; it never
//! reads the project from `llm_settings` or from the caller's parameters.

use crate::errors::{EngineError, ErrorType};
use serde_json::{Map, Value};

/// The reserved argument the host stamps with the authenticated project.
pub const PROJECT_ARG: &str = "_elitea_project_id";

/// The platform project that owns an index (`centry.project.id`, a
/// PostgreSQL `INTEGER`). Always positive.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct ProjectScope(i32);

impl ProjectScope {
    /// The scope of project `id`; `None` for zero or a negative id.
    #[must_use]
    pub fn new(id: i32) -> Option<Self> {
        (id > 0).then_some(Self(id))
    }

    /// The project id, as the `project_id` columns store it.
    #[must_use]
    pub fn id(self) -> i32 {
        self.0
    }

    /// Read [`PROJECT_ARG`] from a tool's arguments. The host sends it as a
    /// decimal string; a JSON integer is accepted too.
    ///
    /// # Errors
    ///
    /// A `ValueError` when the argument is absent or is not a positive
    /// 32-bit integer. The index is never read or written without it.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, EngineError> {
        let refused = || {
            EngineError::new(
                ErrorType::Value,
                format!(
                    "{PROJECT_ARG} is missing or not a positive project id; the sub-application host sets it from the authenticated project, and the wiki index is never read or written without it"
                ),
            )
        };
        let id = match arguments.get(PROJECT_ARG) {
            Some(Value::String(text)) => text.trim().parse::<i32>().ok(),
            Some(Value::Number(number)) => number.as_i64().and_then(|n| i32::try_from(n).ok()),
            _ => None,
        };
        id.and_then(Self::new).ok_or_else(refused)
    }
}

impl std::fmt::Display for ProjectScope {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.0)
    }
}

/// One wiki of one project: the key of every index row.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct WikiKey {
    project: ProjectScope,
    wiki_id: String,
}

impl WikiKey {
    /// The wiki `wiki_id` of `project`.
    pub fn new(project: ProjectScope, wiki_id: impl Into<String>) -> Self {
        Self {
            project,
            wiki_id: wiki_id.into(),
        }
    }

    /// The owning project.
    #[must_use]
    pub fn project(&self) -> ProjectScope {
        self.project
    }

    /// The project id, for a `project_id = $n` bind.
    #[must_use]
    pub fn project_id(&self) -> i32 {
        self.project.id()
    }

    /// The wiki id.
    #[must_use]
    pub fn wiki_id(&self) -> &str {
        &self.wiki_id
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn args(value: &Value) -> Map<String, Value> {
        value.as_object().cloned().unwrap_or_default()
    }

    #[test]
    fn only_a_positive_id_is_a_scope() {
        assert_eq!(ProjectScope::new(7).map(ProjectScope::id), Some(7));
        assert_eq!(ProjectScope::new(0), None);
        assert_eq!(ProjectScope::new(-3), None);
    }

    #[test]
    fn the_reserved_argument_is_read_as_the_host_sends_it() {
        let read = |value: Value| {
            ProjectScope::from_arguments(&args(&value))
                .ok()
                .map(ProjectScope::id)
        };
        assert_eq!(read(json!({ PROJECT_ARG: "42" })), Some(42));
        assert_eq!(read(json!({ PROJECT_ARG: 42 })), Some(42));
        for refused in [
            json!({}),
            json!({ PROJECT_ARG: "" }),
            json!({ PROJECT_ARG: "0" }),
            json!({ PROJECT_ARG: "-1" }),
            json!({ PROJECT_ARG: "1 OR 1=1" }),
            json!({ PROJECT_ARG: 4_294_967_296_i64 }),
            json!({ PROJECT_ARG: null }),
            json!({ "project_id": "42" }),
        ] {
            assert_eq!(read(refused.clone()), None, "{refused}");
        }
        let error = ProjectScope::from_arguments(&Map::new()).err();
        assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Value));
    }
}
