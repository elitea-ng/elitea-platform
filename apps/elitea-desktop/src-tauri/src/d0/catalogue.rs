//! The chat models of a turn's project (#1228): the catalogue a model
//! chosen for one turn is checked against before the turn starts.
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
    pub max_output_tokens: Option<u32>,
    pub supports_reasoning: Option<bool>,
    pub openai_compatible: Option<bool>,
}

/// A project's chat models, its own first.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Catalogue {
    pub models: Vec<CatalogueModel>,
}

impl Catalogue {
    /// The model named `name`: the project's own, else a shared one.
    #[must_use]
    pub fn find(&self, name: &str) -> Option<&CatalogueModel> {
        self.models.iter().find(|model| model.name == name)
    }

    fn from_body(body: &Value, project_id: i64) -> Self {
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
                    max_output_tokens: item
                        .get("max_output_tokens")
                        .and_then(Value::as_u64)
                        .and_then(|tokens| u32::try_from(tokens).ok())
                        .filter(|tokens| *tokens > 0),
                    supports_reasoning: item.get("supports_reasoning").and_then(Value::as_bool),
                    openai_compatible: item.get("openai_compatible").and_then(Value::as_bool),
                })
            })
            .collect();
        models.sort_by_key(|model| model.project_id != project_id);
        Self { models }
    }
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
    let catalogue = match answer.status {
        200..=299 => Catalogue::from_body(&answer.body, project_id),
        404 => Catalogue::default(),
        status => {
            log::info!(
                "the model catalogue answered {status}; a chosen model is left to the gateway"
            );
            return None;
        }
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
        assert_eq!(catalogue.find("mine").unwrap().max_output_tokens, Some(512));
        assert!(catalogue.find("embed").is_none());
    }
}
