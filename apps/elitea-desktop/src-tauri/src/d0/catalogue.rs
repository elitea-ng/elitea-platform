//! The chat models of a turn's project (#1228): the catalogue a model
//! chosen for one turn is checked against before the turn starts, the
//! rows the context limits are read from (#1217, `super::context`), and the
//! family flag that decides whether reasoning goes back to the provider.
//!
//! * **Read.** `GET /configurations/models/{project}?section=llm&include_shared=true`
//!   (the section holds the project's chat models only, its own and the
//!   shared ones), on the turn's pinned credentials, kept per session and
//!   project for [`CATALOGUE_TTL`]. A 404 is an empty catalogue; any other
//!   failure (network, 401, 403, 429, a 5xx) is not kept and answers
//!   "unknown" ([`read`] returns `None`).
//! * **Check.** A chosen model the catalogue answered without is refused
//!   (`model_unavailable`) before the local turn's start. When the
//!   catalogue could not answer, the choice goes to the gateway, which
//!   decides: governance stays on the server (the gateway refuses a model
//!   the project may not use, or a request over budget, per request).
//! * **Low tier.** The answer's `low_tier_default_model_name` and
//!   `low_tier_default_model_project_id` name the project's low-tier
//!   default, when one is configured and listed
//!   ([`Catalogue::low_tier_default`]): the compaction's summary model when
//!   its window is at least the agent's (`super::context`).
//! * **Family.** `openai_compatible: false` names a provider whose own API
//!   carries signed reasoning blocks back in history (Anthropic);
//!   [`ReasoningReplay`] says so. Every other model is spoken to through the
//!   OpenAI-compatible chat route, whose schema has no reasoning items.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use reqwest::Method;
use serde_json::Value;

use super::api::PlatformApi;

/// How long one session keeps a project's catalogue.
pub const CATALOGUE_TTL: Duration = Duration::from_secs(60);

/// One chat model of the catalogue.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CatalogueModel {
    pub name: String,
    pub project_id: i64,
    pub openai_compatible: Option<bool>,
    /// The row as the catalogue answered it (the context limits are read
    /// from it).
    pub row: Value,
}

/// A project's chat models, its own first.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Catalogue {
    pub models: Vec<CatalogueModel>,
    /// The low-tier default's place in `models` (module docs).
    low_tier: Option<usize>,
}

impl Catalogue {
    /// The model named `name`: the project's own, else a shared one.
    #[must_use]
    pub fn find(&self, name: &str) -> Option<&CatalogueModel> {
        self.models.iter().find(|model| model.name == name)
    }

    /// The project's low-tier default model, when the catalogue names one
    /// it lists.
    #[must_use]
    pub fn low_tier_default(&self) -> Option<&CatalogueModel> {
        self.low_tier.and_then(|index| self.models.get(index))
    }

    pub(super) fn from_body(body: &Value, project_id: i64) -> Self {
        let mut models: Vec<CatalogueModel> = body
            .get("items")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .filter_map(|item| {
                let name = item.get("name")?.as_str()?.to_owned();
                // A compat row names its type; only chat models qualify.
                if item
                    .get("type")
                    .and_then(Value::as_str)
                    .is_some_and(|kind| kind != "llm_model")
                {
                    return None;
                }
                Some(CatalogueModel {
                    name,
                    project_id: item.get("project_id").and_then(Value::as_i64).unwrap_or(0),
                    openai_compatible: item.get("openai_compatible").and_then(Value::as_bool),
                    row: item.clone(),
                })
            })
            .collect();
        models.sort_by_key(|model| model.project_id != project_id);
        // An empty name (and the `""` project id) is "none configured".
        let low_tier = body
            .get("low_tier_default_model_name")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty())
            .and_then(|name| {
                let owner = body
                    .get("low_tier_default_model_project_id")
                    .and_then(Value::as_i64);
                models.iter().position(|model| {
                    model.name == name && owner.is_none_or(|owner| model.project_id == owner)
                })
            });
        Self { models, low_tier }
    }
}

/// How a model's reasoning is replayed in a later request's history.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ReasoningReplay {
    /// OpenAI-compatible chat completions: reasoning is not part of the
    /// chat schema and is never sent back.
    #[default]
    Never,
    /// A provider that requires its signed reasoning blocks in history
    /// (Anthropic thinking with a `signature`).
    SignedBlocks,
}

impl ReasoningReplay {
    /// The family flag of `model` (`openai_compatible: false`: a provider
    /// of its own); an unknown model is OpenAI-compatible.
    #[must_use]
    pub fn of(model: Option<&CatalogueModel>) -> Self {
        match model.and_then(|model| model.openai_compatible) {
            Some(false) => Self::SignedBlocks,
            _ => Self::Never,
        }
    }
}

/// Whether the turn's model takes Anthropic `cache_control` breakpoints: a
/// provider of its own ([`ReasoningReplay::SignedBlocks`]) whose name is
/// Anthropic's. `openai_compatible: false` alone is not enough: it is the
/// flag's default, so any row that never set it carries it. The rule is
/// the server's own (`nativeAnthropic` in elitea-main's agent execution).
#[must_use]
pub fn takes_cache_hints(replay: ReasoningReplay, model_name: &str) -> bool {
    let name = model_name.to_ascii_lowercase();
    replay == ReasoningReplay::SignedBlocks
        && (name.contains("claude") || name.contains("anthropic"))
}

/// A catalogue status the session can keep: a 2xx, or a 404 (no catalogue
/// for the project: it describes no model). Any other (401, 403, 408, 429,
/// a 5xx) may answer differently on the next turn.
fn definitive(status: u16) -> bool {
    (200..300).contains(&status) || status == 404
}

/// A project's catalogue as one session read it.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct CatalogueKey {
    pub identity: String,
    pub project_id: i64,
}

/// The catalogues read in this app session, each for [`CATALOGUE_TTL`].
#[derive(Default)]
pub struct Catalogues {
    kept: Mutex<HashMap<CatalogueKey, (Instant, Arc<Catalogue>)>>,
}

/// Bound on the kept catalogues (a cache, not a store).
const MAX_KEPT: usize = 64;

impl Catalogues {
    fn locked(
        &self,
    ) -> std::sync::MutexGuard<'_, HashMap<CatalogueKey, (Instant, Arc<Catalogue>)>> {
        self.kept
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn get(&self, key: &CatalogueKey) -> Option<Arc<Catalogue>> {
        let kept = self.locked();
        let (at, catalogue) = kept.get(key)?;
        (at.elapsed() < CATALOGUE_TTL).then(|| catalogue.clone())
    }

    fn keep(&self, key: CatalogueKey, catalogue: Arc<Catalogue>) {
        let mut kept = self.locked();
        if kept.len() >= MAX_KEPT && !kept.contains_key(&key) {
            kept.clear();
        }
        kept.insert(key, (Instant::now(), catalogue));
    }

    /// Sign-out, a new sign-in, another deployment.
    pub fn clear(&self) {
        self.locked().clear();
    }
}

/// The chat models of `project_id`, from the cache or the platform; `None`
/// when the catalogue could not answer now (module docs).
pub async fn read(api: &PlatformApi, kept: &Catalogues, project_id: i64) -> Option<Arc<Catalogue>> {
    let key = api.credentials().identity().map(|identity| CatalogueKey {
        identity,
        project_id,
    });
    if let Some(catalogue) = key.as_ref().and_then(|key| kept.get(key)) {
        return Some(catalogue);
    }
    let answer = api
        .send(
            Method::GET,
            &format!("/api/v2/configurations/models/{project_id}?section=llm&include_shared=true"),
            None,
            &[],
        )
        .await
        .ok()?;
    if !definitive(answer.status) {
        log::info!(
            "the model catalogue answered {}; the turn uses the default context window, and a chosen model is left to the gateway",
            answer.status
        );
        return None;
    }
    let catalogue = if answer.status == 404 {
        Catalogue::default()
    } else {
        Catalogue::from_body(&answer.body, project_id)
    };
    let catalogue = Arc::new(catalogue);
    if let Some(key) = key {
        kept.keep(key, catalogue.clone());
    }
    Some(catalogue)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_projects_own_model_comes_first_and_only_chat_models_count() {
        let catalogue = Catalogue::from_body(
            &json!({"items": [
                {"name": "shared", "project_id": 2, "openai_compatible": false},
                {"name": "mine", "project_id": 1, "max_output_tokens": 512, "supports_reasoning": true},
                {"name": "embed", "project_id": 1, "type": "embedding_model"},
                {"name": "chat", "project_id": 1, "type": "llm_model"},
            ]}),
            1,
        );
        let names: Vec<&str> = catalogue.models.iter().map(|m| m.name.as_str()).collect();
        assert_eq!(names, ["mine", "chat", "shared"]);
        assert_eq!(
            catalogue.find("mine").unwrap().row["max_output_tokens"],
            512
        );
        assert_eq!(
            ReasoningReplay::of(catalogue.find("shared")),
            ReasoningReplay::SignedBlocks
        );
        assert_eq!(
            ReasoningReplay::of(catalogue.find("mine")),
            ReasoningReplay::Never
        );
        assert_eq!(ReasoningReplay::of(None), ReasoningReplay::Never);
        assert!(catalogue.find("embed").is_none());
    }

    #[test]
    fn the_low_tier_default_is_the_listed_model_it_names() {
        let body = |name: Value, project: Value| {
            json!({
                "items": [
                    {"name": "big", "project_id": 1},
                    {"name": "small", "project_id": 2},
                    {"name": "small", "project_id": 1},
                ],
                "low_tier_default_model_name": name,
                "low_tier_default_model_project_id": project,
            })
        };
        let shared = Catalogue::from_body(&body(json!("small"), json!(2)), 1);
        let chosen = shared.low_tier_default().unwrap();
        assert_eq!((chosen.name.as_str(), chosen.project_id), ("small", 2));
        // No project id: the project's own model first.
        let own = Catalogue::from_body(&body(json!("small"), json!("")), 1);
        assert_eq!(own.low_tier_default().unwrap().project_id, 1);
        // None configured, or one the catalogue does not list.
        for (name, project) in [
            (json!(""), json!("")),
            (json!(null), json!(null)),
            (json!("gone"), json!(1)),
            (json!("small"), json!(9)),
        ] {
            assert!(
                Catalogue::from_body(&body(name.clone(), project), 1)
                    .low_tier_default()
                    .is_none(),
                "{name}"
            );
        }
        assert!(Catalogue::default().low_tier_default().is_none());
    }

    #[test]
    fn cache_hints_go_only_to_a_native_anthropic_model() {
        let native = ReasoningReplay::SignedBlocks;
        assert!(takes_cache_hints(native, "claude-sonnet-4-5"));
        assert!(takes_cache_hints(native, "us.anthropic.claude-haiku-4-5"));
        assert!(takes_cache_hints(native, "Claude-Opus"));
        // A provider of its own that is not Anthropic's (the flag's default).
        assert!(!takes_cache_hints(native, "gemini-2.5-pro"));
        assert!(!takes_cache_hints(native, "gpt-5"));
        // Claude behind the OpenAI-compatible route takes no breakpoints.
        assert!(!takes_cache_hints(
            ReasoningReplay::Never,
            "claude-sonnet-4-5"
        ));
    }

    #[test]
    fn only_a_definitive_catalogue_answer_is_kept() {
        for status in [200, 204, 404] {
            assert!(definitive(status), "{status}");
        }
        for status in [401, 403, 408, 429, 500, 502, 503, 504] {
            assert!(!definitive(status), "{status}");
        }
    }
}
