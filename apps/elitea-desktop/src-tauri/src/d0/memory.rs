//! The desktop's [`MemoryStore`] (ADR-0029 decision 8, #1233): the
//! person's memories in the turn's project, over the public memory routes
//! with the turn's pinned credentials. The tools over it are the runtime's
//! (`elitea_agent_runtime::memory_tools`), the same the cloud worker will
//! offer.
//!
//! * `recall` answers what `start` computed for this turn: never a second
//!   fetch, and nothing kept past the turn (one store per turn).
//! * `save` is `createMemory`, `search` is `listMemories?q=` (the person's
//!   enabled entries only: a disabled one is kept out of turns, as recall
//!   keeps it out), `get` finds an id in a listing of every entry, and
//!   `delete` is `deleteMemory` after `get` shows the id is the person's
//!   (the route answers 204 for any id, so a wrong id would read as
//!   deleted).
//! * Without `local_work.memory_write` a save or a delete is refused here
//!   too, whatever the tools offered.
//!
//! Every saved and deleted id goes to the turn's [`Recorder`], for the
//! commit's `local_work` report.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::host::{
    HostError, HostErrorCode, MemoryEntry, MemoryRecall, MemoryStore,
};
use reqwest::Method;
use serde_json::{Value, json};

use super::api::{ApiError, PlatformApi};
use super::recorder::Recorder;

/// The memories of the person in one turn's project.
pub struct PlatformMemoryStore {
    api: Arc<PlatformApi>,
    project_id: i64,
    recall: MemoryRecall,
    writes: bool,
    recorder: Arc<Recorder>,
}

impl PlatformMemoryStore {
    /// The store of one turn: `recall` is what its `start` answered.
    #[must_use]
    pub fn new(
        api: Arc<PlatformApi>,
        project_id: i64,
        recall: MemoryRecall,
        writes: bool,
        recorder: Arc<Recorder>,
    ) -> Self {
        Self {
            api,
            project_id,
            recall,
            writes,
            recorder,
        }
    }

    fn list_path(&self) -> String {
        format!(
            "/api/v2/elitea_core/memories/prompt_lib/{}",
            self.project_id
        )
    }

    /// The person's enabled memories matching `query` (all of them for "").
    async fn list(&self, query: &str) -> Result<Vec<MemoryEntry>, HostError> {
        let mut path = self.list_path();
        if !query.is_empty() {
            path.push_str("?q=");
            path.extend(url::form_urlencoded::byte_serialize(query.as_bytes()));
        }
        let answer = self
            .api
            .send(Method::GET, &path, None, &[])
            .await
            .map_err(|error| host_error(&error))?;
        if !(200..300).contains(&answer.status) {
            return Err(status_error(answer.status));
        }
        let items = answer
            .body
            .get("items")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?;
        Ok(items
            .iter()
            .filter(|item| item.get("enabled").and_then(Value::as_bool) != Some(false))
            .filter_map(entry)
            .collect())
    }

    fn refuse_writes(&self) -> Result<(), HostError> {
        if self.writes {
            Ok(())
        } else {
            Err(HostError::new(
                HostErrorCode::AuthorizationFailed,
                "saving and deleting memories is turned off by policy",
            ))
        }
    }
}

/// One `MemoryEntry` of the platform (its `id` is a decimal string).
fn entry(item: &Value) -> Option<MemoryEntry> {
    let id = match item.get("id")? {
        Value::String(text) => text.parse().ok()?,
        other => other.as_u64()?,
    };
    Some(MemoryEntry {
        id,
        content: item.get("content")?.as_str()?.to_owned(),
    })
}

fn invalid_response() -> HostError {
    HostError::new(
        HostErrorCode::InvalidResponse,
        "the platform's memory answer did not have the expected shape",
    )
}

fn status_error(status: u16) -> HostError {
    match status {
        // A conflict, a body too large or a refused field: the memory, not
        // the store, is what the platform would not take.
        400 | 409 | 413 | 422 => HostError::new(
            HostErrorCode::InvalidInput,
            "the platform refused the memory",
        ),
        401 | 403 => HostError::new(
            HostErrorCode::AuthorizationFailed,
            "the platform refused the memory request",
        ),
        404 => HostError::new(HostErrorCode::NotFound, "no such memory"),
        // The deployment's minimum-version gate: not retryable, and the
        // person has to update the app (the sign-in's UpgradeRequired).
        426 => HostError::new(
            HostErrorCode::InvalidConfiguration,
            "this version of Elitea is older than the deployment allows; update the app",
        )
        .with_reason("upgrade_required"),
        _ => HostError::new(
            HostErrorCode::DependencyUnavailable,
            "the memory store is unavailable",
        ),
    }
}

fn host_error(error: &ApiError) -> HostError {
    match (error.status, error.code.as_str()) {
        (Some(status), _) => status_error(status),
        (None, "identity_changed" | "not_signed_in") => HostError::new(
            HostErrorCode::ExecutionEnded,
            "the session that started the turn is gone",
        ),
        (None, _) => HostError::new(
            HostErrorCode::DependencyUnavailable,
            "the platform could not be reached",
        ),
    }
}

#[async_trait]
impl MemoryStore for PlatformMemoryStore {
    async fn recall(&self, _input: &str) -> Result<MemoryRecall, HostError> {
        Ok(self.recall.clone())
    }

    async fn save(&self, content: &str) -> Result<MemoryEntry, HostError> {
        self.refuse_writes()?;
        let answer = self
            .api
            .send(
                Method::POST,
                &self.list_path(),
                Some(&json!({ "content": content })),
                &[],
            )
            .await
            .map_err(|error| host_error(&error))?;
        if !(200..300).contains(&answer.status) {
            return Err(status_error(answer.status));
        }
        let saved = entry(&answer.body).ok_or_else(invalid_response)?;
        self.recorder.memory_saved(saved.id);
        Ok(saved)
    }

    // The tools cut the answer to the model's limit.
    async fn search(&self, query: &str, _limit: u32) -> Result<Vec<MemoryEntry>, HostError> {
        self.list(query).await
    }

    async fn get(&self, id: u64) -> Result<Option<MemoryEntry>, HostError> {
        // Disabled entries are the person's too: look in every one.
        let answer = self
            .api
            .send(Method::GET, &self.list_path(), None, &[])
            .await
            .map_err(|error| host_error(&error))?;
        if !(200..300).contains(&answer.status) {
            return Err(status_error(answer.status));
        }
        Ok(answer
            .body
            .get("items")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?
            .iter()
            .filter_map(entry)
            .find(|known| known.id == id))
    }

    async fn delete(&self, id: u64) -> Result<(), HostError> {
        self.refuse_writes()?;
        // Checked again here, whoever calls: the route answers 204 for any id.
        if self.get(id).await?.is_none() {
            return Err(HostError::new(HostErrorCode::NotFound, "no such memory"));
        }
        let answer = self
            .api
            .send(
                Method::DELETE,
                &format!(
                    "/api/v2/elitea_core/memory/prompt_lib/{}/{id}",
                    self.project_id
                ),
                None,
                &[],
            )
            .await
            .map_err(|error| host_error(&error))?;
        if !(200..300).contains(&answer.status) {
            return Err(status_error(answer.status));
        }
        self.recorder.memory_deleted(id);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_refused_memory_is_invalid_and_an_old_app_is_told_to_update() {
        for status in [400, 409, 413, 422] {
            assert_eq!(
                status_error(status).code(),
                HostErrorCode::InvalidInput,
                "{status}"
            );
        }
        let old = status_error(426);
        assert_eq!(old.code(), HostErrorCode::InvalidConfiguration);
        assert_eq!(old.reason_code(), "upgrade_required");
        assert!(old.to_string().contains("update the app"));
        assert_eq!(
            status_error(503).code(),
            HostErrorCode::DependencyUnavailable
        );
    }
}
