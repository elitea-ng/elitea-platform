//! The `generate_wiki` keyword set, as the Go host derives it
//! (`run.ArgumentsFor`): `query`, `llm_settings`, `embedding_model`,
//! `repo_config`, `active_branch`, `force_rebuild_index`,
//! `indexing_method`, `planner_mode`, `exclude_tests`, `run_in_subprocess`.
//! Python read them in `tool_operations.generate_wiki` and
//! `wiki_subprocess_worker.main`.
//!
//! # What the engine trusts, and why
//!
//! * **The wiki is named by the clone, never by the caller.** No argument
//!   names the wiki: the `wiki_id` of the result, of the build and of the
//!   publish is `normalize_wiki_id(repo:branch:sha8)` of the repository
//!   `repo_config` names and the branch and commit the clone actually
//!   checked out. A `wiki_id`, `path_prefix` or any other key a caller adds
//!   is ignored, as Python's `**kwargs` ignored it. `repo_config` itself is
//!   the host's (`ExtractRepoConfig`), and its host is checked against the
//!   git allowlist again before the credential is used (`ingest::admit`).
//!   The Phase 2 path prefixes are directories of the clone, bound as query
//!   parameters, never caller text.
//! * **`llm_settings.api_base` is the platform's.** elitea-main's facade
//!   replaces the block (`material.CallbackSettings`: `{platform}/llm/v1`,
//!   a short-lived callback bearer, the project as `organization`) and lifts
//!   only `max_tokens` / `temperature` from a client's own block
//!   (`LiftToolLLMSettings`), so a caller cannot point the engine's model
//!   calls, or the bearer, at another host. The engine therefore takes the
//!   base URL as given (an absolute http(s) URL without credentials, see
//!   `llm::settings`) and checks no allowlist of its own for it.
//!
//! Ignored on purpose: `force_rebuild_index` (there is no index cache; every
//! run builds), `indexing_method` (Python's wrapper forced `filesystem`
//! whatever was asked) and `run_in_subprocess` (generation always runs in
//! the worker child).

use crate::errors::{EngineError, ErrorType};
use crate::llm::{ModelSettings, embedding_model_name};
use crate::source::{py_str, py_truthy};
use crate::structure::PlannerChoice;
use serde_json::{Map, Value};

/// The variable Python's worker set from `planner_mode` and the planner
/// read when the request named none.
pub const PLANNER_ENV: &str = "DEEPWIKI_STRUCTURE_PLANNER";

/// One `generate_wiki` request, parsed.
///
/// `Debug` is safe to print: `repo_config` (it carries the repository
/// credential) is redacted and the model key is a secret.
#[derive(Clone)]
pub struct GenerateRequest {
    pub query: String,
    pub model: ModelSettings,
    pub embedding_model: String,
    /// The host's `repo_config` (an object; `{}` when absent).
    pub repo_config: Value,
    /// `active_branch or repo_config.branch or "main"`: shown in progress
    /// only. The clone checks out `repo_config.branch`, and the wiki is
    /// filed under the branch it checked out (Python's `actual_branch`).
    pub requested_branch: String,
    /// `planner_mode or planner_type`, as given (`None`: the environment).
    pub planner_mode: Option<String>,
    /// `exclude_tests` when the request set it (`None`: the environment).
    pub exclude_tests: Option<bool>,
}

impl std::fmt::Debug for GenerateRequest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GenerateRequest")
            .field("query", &self.query)
            .field("model", &self.model)
            .field("embedding_model", &self.embedding_model)
            .field("repo_config", &"<redacted>")
            .field("requested_branch", &self.requested_branch)
            .field("planner_mode", &self.planner_mode)
            .field("exclude_tests", &self.exclude_tests)
            .finish()
    }
}

fn value_error(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

impl GenerateRequest {
    /// Parse the keyword set.
    ///
    /// # Errors
    ///
    /// A `ValueError` for a missing query (Python's "Task parameter is
    /// required"), invalid `llm_settings`, a missing embedding model or a
    /// `repo_config` that is not an object.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, EngineError> {
        let query = arguments
            .get("query")
            .filter(|value| py_truthy(value))
            .map(py_str)
            .ok_or_else(|| value_error("Task parameter is required"))?;
        let model = ModelSettings::from_llm_settings(
            arguments
                .get("llm_settings")
                .filter(|value| py_truthy(value))
                .unwrap_or(&Value::Null),
        )?;
        let embedding_model =
            embedding_model_name(arguments.get("embedding_model").unwrap_or(&Value::Null))?;
        let repo_config = match arguments.get("repo_config") {
            Some(Value::Object(config)) => Value::Object(config.clone()),
            Some(value) if !py_truthy(value) => Value::Object(Map::new()),
            None => Value::Object(Map::new()),
            Some(_) => return Err(value_error("repo_config must be an object")),
        };
        let requested_branch = arguments
            .get("active_branch")
            .filter(|value| py_truthy(value))
            .or_else(|| repo_config.get("branch").filter(|value| py_truthy(value)))
            .map_or_else(|| "main".to_owned(), py_str);
        let planner_mode = ["planner_mode", "planner_type"]
            .iter()
            .find_map(|key| arguments.get(*key).filter(|value| py_truthy(value)))
            .map(py_str);
        let exclude_tests = arguments
            .get("exclude_tests")
            .filter(|value| !value.is_null())
            .map(py_truthy);
        Ok(Self {
            query,
            model,
            embedding_model,
            repo_config,
            requested_branch,
            planner_mode,
            exclude_tests,
        })
    }

    /// The structure planner: the request's `planner_mode`, else
    /// `DEEPWIKI_STRUCTURE_PLANNER` (`lookup`), else `auto`.
    #[must_use]
    pub fn planner(&self, lookup: impl Fn(&str) -> Option<String>) -> PlannerChoice {
        let from_env = lookup(PLANNER_ENV).filter(|value| !value.trim().is_empty());
        PlannerChoice::resolve(self.planner_mode.as_deref().or(from_env.as_deref()))
    }

    /// `repo_config.repository`, for progress before the clone target is
    /// derived.
    #[must_use]
    pub fn repository_label(&self) -> String {
        self.repo_config
            .get("repository")
            .filter(|value| py_truthy(value))
            .map(py_str)
            .unwrap_or_default()
    }

    /// `repo_config.provider_type or "github"`, for progress.
    #[must_use]
    pub fn provider_label(&self) -> String {
        self.repo_config
            .get("provider_type")
            .filter(|value| py_truthy(value))
            .map_or_else(|| "github".to_owned(), py_str)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn arguments(value: Value) -> Map<String, Value> {
        match value {
            Value::Object(map) => map,
            _ => Map::new(),
        }
    }

    fn base() -> Value {
        json!({
            "query": "Document it",
            "llm_settings": {"api_base": "http://gw/llm/v1", "api_key": "k", "model_name": "m"},
            "embedding_model": "e",
            "repo_config": {"provider_type": "github", "repository": "o/r", "branch": "dev"},
            "active_branch": "main",
            "force_rebuild_index": true,
            "indexing_method": "filesystem",
            "planner_mode": null,
            "exclude_tests": null,
            "run_in_subprocess": true,
        })
    }

    #[test]
    fn the_debug_form_holds_no_credential() {
        let mut value = base();
        value["llm_settings"]["api_key"] = json!("sk-model-secret-1");
        value["repo_config"]["provider_config"] =
            json!({"token": "ghp-repo-secret-2", "base_url": "https://u:pw-secret-3@git.example"});
        let Ok(request) = GenerateRequest::parse(&arguments(value)) else {
            panic!("refused");
        };
        let text = format!("{request:?} {request:#?}");
        for secret in ["sk-model-secret-1", "ghp-repo-secret-2", "pw-secret-3"] {
            assert!(!text.contains(secret), "{text}");
        }
        assert!(text.contains("Document it"), "{text}");
    }

    #[test]
    fn the_host_keyword_set_parses() {
        let Ok(request) = GenerateRequest::parse(&arguments(base())) else {
            panic!("refused");
        };
        assert_eq!(request.query, "Document it");
        assert_eq!(request.embedding_model, "e");
        assert_eq!(request.requested_branch, "main");
        assert_eq!(request.planner_mode, None);
        assert_eq!(request.exclude_tests, None);
        assert_eq!(request.repository_label(), "o/r");
        assert_eq!(request.provider_label(), "github");
        assert_eq!(request.planner(|_| None), PlannerChoice::Auto);
        assert_eq!(
            request.planner(|_| Some("cluster".to_owned())),
            PlannerChoice::Cluster
        );
    }

    #[test]
    fn the_request_choices_beat_the_environment() {
        let mut value = base();
        value["planner_type"] = json!("cluster");
        value["exclude_tests"] = json!(false);
        value["active_branch"] = json!("");
        let Ok(request) = GenerateRequest::parse(&arguments(value)) else {
            panic!("refused");
        };
        assert_eq!(
            request.planner(|_| Some("agent".to_owned())),
            PlannerChoice::Cluster
        );
        assert_eq!(request.exclude_tests, Some(false));
        assert_eq!(request.requested_branch, "dev");
    }

    #[test]
    fn refusals_are_value_errors() {
        for (key, value, message) in [
            ("query", json!(""), "Task parameter is required"),
            (
                "embedding_model",
                Value::Null,
                "embedding_model is required",
            ),
            ("repo_config", json!("o/r"), "repo_config must be an object"),
            (
                "llm_settings",
                json!({}),
                "llm_settings.api_base is required",
            ),
        ] {
            let mut request = base();
            request[key] = value;
            let error = GenerateRequest::parse(&arguments(request)).err();
            assert_eq!(
                error.map(|e| (e.error_type, e.message)),
                Some((ErrorType::Value, message.to_owned())),
                "{key}"
            );
        }
        let mut missing = arguments(base());
        missing.remove("query");
        assert!(GenerateRequest::parse(&missing).is_err());
    }

    #[test]
    fn a_caller_cannot_name_the_wiki() {
        let mut value = base();
        value["wiki_id"] = json!("victim--repo--main");
        value["path_prefix"] = json!("../../etc");
        let Ok(request) = GenerateRequest::parse(&arguments(value)) else {
            panic!("refused");
        };
        // Nothing of the request carries them on.
        assert!(!format!("{request:?}").contains("victim"));
        assert!(!format!("{request:?}").contains("../../etc"));
    }
}
