//! The source an ingestion reads: the object elitea-main's facade expands
//! a source toolkit into, and the clone it maps to.
//!
//! The facade (`providerhost/material/source.go`,
//! `api/v2/inventory/sources.go`) sends it FLAT — the credential block and
//! the picked settings at the top level, the stored patterns as
//! `file_patterns` / `exclude_patterns`:
//!
//! ```json
//! {"toolkit_id": 42, "type": "github", "name": "platform",
//!  "github_configuration": {"base_url": "…", "access_token": "…"},
//!  "repository": "o/r", "active_branch": "main", "branch": "dev",
//!  "file_patterns": "*.py"}
//! ```
//!
//! The nested form the Python engine was written for (`settings`,
//! `whitelist`, `blacklist`) is read too, and wins where both are present.
//!
//! WHY A CLONE. The Python engine read a repository through the source
//! toolkit's API, one request per file. Here the repository is cloned once
//! (`elitea-repo-ingest`: shallow, the branch head, the credential as a
//! header, the host checked against `ELITEA_INVENTORY_GIT_ALLOWLIST` — the
//! allowlist the facade already applies to the same host — and the clone
//! limits). The same files are selected afterwards (`super::files`).

use elitea_engine_core::errors::{EngineError, ErrorType};
use serde_json::{Map, Value, json};

/// The source types read when `ELITEA_INVENTORY_SOURCE_TYPES` is unset.
pub const DEFAULT_SOURCE_TYPES: [&str; 2] = ["github", "ado_repos"];

/// Keys of the envelope, never part of the toolkit settings.
const ENVELOPE_KEYS: [&str; 9] = [
    "toolkit_id",
    "type",
    "name",
    "settings",
    "branch",
    "whitelist",
    "blacklist",
    "file_patterns",
    "exclude_patterns",
];

/// The branch read when neither the call nor the settings name one.
pub const DEFAULT_BRANCH: &str = "main";

/// A source type this engine can read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SourceKind {
    GitHub,
    AdoRepos,
}

impl SourceKind {
    /// The stored toolkit type.
    #[must_use]
    pub fn name(self) -> &'static str {
        match self {
            Self::GitHub => "github",
            Self::AdoRepos => "ado_repos",
        }
    }

    /// The settings key holding the credential block.
    #[must_use]
    pub fn credential_key(self) -> &'static str {
        match self {
            Self::GitHub => "github_configuration",
            Self::AdoRepos => "ado_configuration",
        }
    }
}

/// One expanded source.
#[derive(Debug, Clone, PartialEq)]
pub struct Source {
    pub toolkit_id: Value,
    pub kind: SourceKind,
    /// Keys every citation (`source_toolkit`) and the source's status.
    pub name: String,
    pub settings: Map<String, Value>,
    pub branch: Option<String>,
    pub whitelist: Option<Vec<String>>,
    pub blacklist: Option<Vec<String>>,
}

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message.into())
}

/// Python truthiness, for the `or` chains the legacy code used.
fn truthy(value: Option<&Value>) -> Option<&Value> {
    value.filter(|value| match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|n| n != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(fields) => !fields.is_empty(),
    })
}

fn text(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        other => other.to_string(),
    }
}

/// `_patterns`: a comma-separated string or a list; nothing left is `None`.
fn patterns(raw: &Map<String, Value>, key: &str) -> Result<Option<Vec<String>>, EngineError> {
    let items: Vec<String> = match raw.get(key) {
        None | Some(Value::Null) => return Ok(None),
        Some(Value::String(text)) => text
            .split(',')
            .map(str::trim)
            .filter(|item| !item.is_empty())
            .map(str::to_owned)
            .collect(),
        Some(Value::Array(values)) => values
            .iter()
            .map(|value| text(value).trim().to_owned())
            .filter(|item| !item.is_empty())
            .collect(),
        Some(other) => {
            return Err(invalid(format!(
                "source.{key} must be a list or a comma-separated string, got {}",
                json_type(other)
            )));
        }
    };
    Ok((!items.is_empty()).then_some(items))
}

fn json_type(value: &Value) -> &'static str {
    match value {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "str",
        Value::Array(_) => "list",
        Value::Object(_) => "dict",
    }
}

impl Source {
    /// What ingestion needs of this source: its name and its whitelist and
    /// blacklist ([`elitea_inventory_core::ingest::SourceSelection`]).
    #[must_use]
    pub fn selection(&self) -> elitea_inventory_core::ingest::SourceSelection {
        elitea_inventory_core::ingest::SourceSelection {
            name: self.name.clone(),
            whitelist: self.whitelist.clone(),
            blacklist: self.blacklist.clone(),
        }
    }

    /// Validate the expanded source object (`parse_source`), refusing
    /// anything else by name. `allowed` is the configured type list.
    ///
    /// # Errors
    ///
    /// A `ValueError`: no source, not an object, no or an unsupported type,
    /// malformed settings or patterns.
    pub fn parse(raw: Option<&Value>, allowed: &[String]) -> Result<Self, EngineError> {
        let Some(raw) = raw.filter(|raw| !raw.is_null()) else {
            return Err(invalid(
                "source is required: this service does not resolve toolkit references, the facade expands them. Re-run the tool through the platform rather than calling the provider directly.",
            ));
        };
        let Some(raw) = raw.as_object() else {
            return Err(invalid(format!(
                "source must be the expanded source object, got {}. A bare toolkit id is not resolved here.",
                json_type(raw)
            )));
        };
        let kind_name = raw
            .get("type")
            .filter(|value| !value.is_null())
            .map(|value| text(value).trim().to_lowercase())
            .unwrap_or_default();
        if kind_name.is_empty() {
            return Err(invalid(format!(
                "source.type is required (one of: {})",
                allowed.join(", ")
            )));
        }
        let kind = match kind_name.as_str() {
            "github" => Some(SourceKind::GitHub),
            "ado_repos" => Some(SourceKind::AdoRepos),
            _ => None,
        };
        let Some(kind) = kind.filter(|_| allowed.contains(&kind_name)) else {
            return Err(invalid(format!(
                "source.type '{kind_name}' is not ingestible by this service. Supported: {}.",
                allowed.join(", ")
            )));
        };
        let settings = match raw.get("settings") {
            None | Some(Value::Null) => raw
                .iter()
                .filter(|(key, _)| !ENVELOPE_KEYS.contains(&key.as_str()))
                .map(|(key, value)| (key.clone(), value.clone()))
                .collect(),
            Some(Value::Object(settings)) => settings.clone(),
            Some(other) => {
                return Err(invalid(format!(
                    "source.settings must be an object, got {}",
                    json_type(other)
                )));
            }
        };
        let toolkit_id = raw.get("toolkit_id").cloned().unwrap_or(Value::Null);
        let name = raw
            .get("name")
            .filter(|value| truthy(Some(value)).is_some())
            .map(|value| text(value).trim().to_owned())
            .filter(|name| !name.is_empty())
            .unwrap_or_else(|| {
                if toolkit_id.is_null() {
                    kind_name.clone()
                } else {
                    format!("toolkit_{}", text(&toolkit_id))
                }
            });
        Ok(Self {
            toolkit_id,
            kind,
            name,
            settings,
            branch: truthy(raw.get("branch")).map(text),
            whitelist: patterns(raw, "whitelist")?.or(patterns(raw, "file_patterns")?),
            blacklist: patterns(raw, "blacklist")?.or(patterns(raw, "exclude_patterns")?),
        })
    }

    /// The branch read: the call's, then the stored active and base
    /// branches, then `main`.
    #[must_use]
    pub fn active_branch(&self) -> String {
        self.branch
            .clone()
            .or_else(|| truthy(self.settings.get("active_branch")).map(text))
            .or_else(|| truthy(self.settings.get("base_branch")).map(text))
            .unwrap_or_else(|| DEFAULT_BRANCH.to_owned())
    }

    /// The `repo_config` `elitea-repo-ingest` clones.
    ///
    /// # Errors
    ///
    /// A `ValueError` when the credential block is absent (the facade did
    /// not forward the toolkit's settings), when a GitHub source names only
    /// a GitHub App (an app installation token is not minted here), or when
    /// the repository (and, for Azure DevOps, the project) is missing.
    pub fn repo_config(&self) -> Result<Value, EngineError> {
        let key = self.kind.credential_key();
        let Some(Value::Object(credentials)) = self.settings.get(key) else {
            return Err(invalid(format!(
                "the {} toolkit for source '{}' has no '{key}' in its settings, so it cannot authenticate; the facade must forward the toolkit's stored credentials",
                self.kind.name(),
                self.name
            )));
        };
        let field = |name: &str| truthy(self.settings.get(name)).map(text);
        match self.kind {
            SourceKind::GitHub => {
                let has_basic_or_token = ["access_token", "username", "password"]
                    .iter()
                    .any(|name| truthy(credentials.get(*name)).is_some());
                if !has_basic_or_token && truthy(credentials.get("app_id")).is_some() {
                    return Err(invalid(format!(
                        "source '{}' authenticates as a GitHub App, which this engine cannot clone with yet; give the toolkit an access token",
                        self.name
                    )));
                }
                let repository = field("repository").ok_or_else(|| {
                    invalid(format!("source '{}' names no repository", self.name))
                })?;
                Ok(json!({
                    "provider_type": "github",
                    "provider_config": credentials,
                    "repository": repository,
                    "branch": self.active_branch(),
                }))
            }
            SourceKind::AdoRepos => {
                let repository = field("repository_id")
                    .or_else(|| field("repository"))
                    .ok_or_else(|| {
                        invalid(format!("source '{}' names no repository_id", self.name))
                    })?;
                let project = field("project")
                    .ok_or_else(|| invalid(format!("source '{}' names no project", self.name)))?;
                Ok(json!({
                    "provider_type": "ado",
                    "provider_config": credentials,
                    "repository": repository,
                    "project": project,
                    "branch": self.active_branch(),
                }))
            }
        }
    }

    /// The source's status key: its toolkit id as text.
    #[must_use]
    pub fn status_key(&self) -> String {
        text(&self.toolkit_id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn allowed() -> Vec<String> {
        DEFAULT_SOURCE_TYPES
            .iter()
            .map(|t| (*t).to_owned())
            .collect()
    }

    fn parse(raw: &Value) -> Result<Source, EngineError> {
        Source::parse(Some(raw), &allowed())
    }

    #[test]
    fn the_facade_flat_shape_is_read_and_maps_to_a_clone() {
        let Ok(source) = parse(&json!({
            "toolkit_id": 42,
            "type": "GitHub",
            "name": "platform",
            "github_configuration": {"base_url": "https://api.github.com", "access_token": "t"},
            "repository": "o/r",
            "active_branch": "main",
            "file_patterns": "*.py, *.go",
            "exclude_patterns": "vendor/*",
            "branch": "dev",
        })) else {
            panic!("parses");
        };
        assert_eq!(source.kind, SourceKind::GitHub);
        assert_eq!(
            source.whitelist,
            Some(vec!["*.py".to_owned(), "*.go".to_owned()])
        );
        assert_eq!(source.blacklist, Some(vec!["vendor/*".to_owned()]));
        assert_eq!(source.active_branch(), "dev");
        assert_eq!(source.status_key(), "42");
        assert_eq!(
            source.repo_config().ok(),
            Some(json!({
                "provider_type": "github",
                "provider_config": {"base_url": "https://api.github.com", "access_token": "t"},
                "repository": "o/r",
                "branch": "dev",
            }))
        );
    }

    #[test]
    fn ado_maps_its_repository_id_and_project() {
        let Ok(source) = parse(&json!({
            "toolkit_id": 7,
            "type": "ado_repos",
            "ado_configuration": {"organization_url": "https://dev.azure.com/org", "token": "t"},
            "project": "P",
            "repository_id": "R",
            "base_branch": "develop",
        })) else {
            panic!("parses");
        };
        assert_eq!(source.name, "toolkit_7");
        let Ok(config) = source.repo_config() else {
            panic!("maps");
        };
        assert_eq!(config["repository"], json!("R"));
        assert_eq!(config["project"], json!("P"));
        assert_eq!(config["branch"], json!("develop"));
        assert_eq!(config["provider_type"], json!("ado"));
    }

    #[test]
    fn the_nested_shape_still_wins() {
        let Ok(source) = parse(&json!({
            "type": "github",
            "settings": {"github_configuration": {}, "repository": "a/b"},
            "repository": "ignored",
            "whitelist": ["a"],
            "file_patterns": "b",
        })) else {
            panic!("parses");
        };
        assert_eq!(source.settings.get("repository"), Some(&json!("a/b")));
        assert_eq!(source.whitelist, Some(vec!["a".to_owned()]));
    }

    #[test]
    fn refusals_name_what_is_wrong() {
        let refused = |raw: Option<&Value>, needle: &str| {
            let outcome = Source::parse(raw, &allowed());
            assert!(
                outcome.as_ref().is_err_and(|e| e.message.contains(needle)),
                "{raw:?}: {outcome:?}"
            );
        };
        refused(None, "source is required");
        refused(Some(&json!(42)), "A bare toolkit id");
        refused(Some(&json!({"type": ""})), "source.type is required");
        refused(Some(&json!({"type": "gitlab"})), "not ingestible");
        refused(
            Some(&json!({"type": "github", "settings": []})),
            "source.settings must be an object",
        );
        refused(
            Some(&json!({"type": "github", "whitelist": 3})),
            "source.whitelist must be a list",
        );
        let only = Source::parse(Some(&json!({"type": "ado_repos"})), &["github".to_owned()]);
        assert!(only.is_err(), "the configured list is what is enforced");

        let mapped = |raw: Value, needle: &str| {
            let outcome = parse(&raw).and_then(|source| source.repo_config());
            assert!(
                outcome.as_ref().is_err_and(|e| e.message.contains(needle)),
                "{raw}: {outcome:?}"
            );
        };
        mapped(
            json!({"type": "github", "repository": "o/r"}),
            "has no 'github_configuration'",
        );
        mapped(
            json!({"type": "github", "github_configuration": {"app_id": "1", "app_private_key": "k"}, "repository": "o/r"}),
            "GitHub App",
        );
        mapped(
            json!({"type": "github", "github_configuration": {}}),
            "names no repository",
        );
        mapped(
            json!({"type": "ado_repos", "ado_configuration": {}, "repository_id": "R"}),
            "names no project",
        );
    }

    #[test]
    fn empty_patterns_select_everything() {
        let Ok(source) = parse(&json!({"type": "github", "whitelist": "", "blacklist": ",  ,"}))
        else {
            panic!("parses");
        };
        assert_eq!(source.whitelist, None);
        assert_eq!(source.blacklist, None);
    }
}
