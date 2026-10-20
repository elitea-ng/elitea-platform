//! The embedding models of the local index as the settings page sees them
//! (IPC.md, "Index embeddings"): the catalogue with sizes and memory, the
//! download the person consents to (progress on `index://event`), import
//! from a folder, removal, and the per-workspace embeddings commands that
//! need more than the registry (a remote model's width is probed once).
//!
//! Nothing is downloaded until the person picks a model and confirms.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, PoisonError};

use elitea_local_embed::EmbedError;
use elitea_local_embed::catalog::{self, HUGGING_FACE, ModelSpec};
use elitea_local_embed::manager::{ModelState, ModelStore, Progress};
use elitea_local_index::embedding::EmbedStatus;
use serde::Serialize;
use serde_json::json;
use tauri::State;

use crate::d0::turn::PolicySource;
use crate::embeddings::{Allowed, REMOTE, RemoteDeps, embed_remote, valid_model};
use crate::local_commands::{IpcError, LocalState};

/// Where index events go (the main window, or nowhere in tests).
pub type Emit = Arc<dyn Fn(serde_json::Value) + Send + Sync>;

/// One catalogue model, for the settings page.
#[derive(Debug, Clone, Serialize)]
pub struct ModelInfo {
    pub id: &'static str,
    pub display_name: &'static str,
    pub summary: &'static str,
    pub license: &'static str,
    /// Bytes to download.
    pub download_bytes: u64,
    /// Memory while it embeds, roughly (measured).
    pub ram_mb: u32,
    pub dimension: usize,
    #[serde(flatten)]
    pub state: ModelState,
    pub downloading: bool,
}

/// `index_models`.
#[derive(Debug, Clone, Serialize)]
pub struct Models {
    pub models: Vec<ModelInfo>,
    /// The policy lets the person embed on this device / through the
    /// deployment.
    pub local_allowed: bool,
    pub remote_allowed: bool,
    /// Bytes the model cache holds.
    pub cache_bytes: u64,
    /// The host models download from when the policy names a mirror
    /// (`None`: Hugging Face).
    pub mirror: Option<String>,
}

/// The model cache and its downloads.
pub struct ModelHost {
    store: ModelStore,
    policy: Arc<dyn PolicySource>,
    remote: Option<RemoteDeps>,
    downloads: Mutex<HashMap<&'static str, Arc<AtomicBool>>>,
    emit: Emit,
}

/// A running download's entry in [`ModelHost::running`], removed on drop.
struct RunningEntry {
    host: Arc<ModelHost>,
    id: &'static str,
}

impl Drop for RunningEntry {
    fn drop(&mut self) {
        self.host.running().remove(self.id);
    }
}

fn error(code: &str, message: impl Into<String>) -> IpcError {
    IpcError {
        code: code.to_owned(),
        message: message.into(),
    }
}

fn spec_of(model_id: &str) -> Result<&'static ModelSpec, IpcError> {
    catalog::model(model_id)
        .ok_or_else(|| error("model_unknown", "That is not a model this app offers."))
}

impl ModelHost {
    #[must_use]
    pub fn new(
        data_dir: &std::path::Path,
        policy: Arc<dyn PolicySource>,
        remote: Option<RemoteDeps>,
        emit: Emit,
    ) -> Self {
        Self {
            store: ModelStore::new(data_dir.join("models")),
            policy,
            remote,
            downloads: Mutex::new(HashMap::new()),
            emit,
        }
    }

    fn allowed(&self) -> Allowed {
        Allowed::of(self.policy.as_ref())
    }

    fn running(&self) -> std::sync::MutexGuard<'_, HashMap<&'static str, Arc<AtomicBool>>> {
        self.downloads
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
    }

    fn info(&self, spec: &'static ModelSpec) -> ModelInfo {
        ModelInfo {
            id: spec.id,
            display_name: spec.display_name,
            summary: spec.summary,
            license: spec.license,
            download_bytes: spec.download_bytes(),
            ram_mb: spec.ram_mb,
            dimension: spec.dimension,
            state: self.store.state(spec),
            downloading: self.running().contains_key(spec.id),
        }
    }

    /// `index_models`.
    #[must_use]
    pub fn models(&self) -> Models {
        let allowed = self.allowed();
        Models {
            models: catalog::MODELS
                .into_iter()
                .map(|spec| self.info(spec))
                .collect(),
            local_allowed: allowed.local,
            remote_allowed: allowed.remote && self.remote.is_some(),
            cache_bytes: self.store.disk_bytes(),
            mirror: crate::embeddings::model_base(self.policy.as_ref()).map(|base| {
                base.trim_start_matches("https://")
                    .split('/')
                    .next()
                    .unwrap_or_default()
                    .to_owned()
            }),
        }
    }

    fn report(&self, spec: &ModelSpec, phase: &str, done: u64, failure: Option<String>) {
        (self.emit)(json!({
            "kind": "model",
            "model": spec.id,
            "phase": phase,
            "done": done,
            "total": spec.download_bytes(),
            "error": failure,
        }));
    }

    /// `index_model_download`: start downloading (resuming) `model_id` in
    /// the background. Needs `confirm` (the person saw the size).
    ///
    /// # Errors
    ///
    /// `confirmation_required`, `model_unknown`, `embeddings_not_allowed`,
    /// `model_busy` (already downloading), `unavailable` (no HTTP client).
    pub fn download(
        self: &Arc<Self>,
        model_id: &str,
        confirm: bool,
    ) -> Result<ModelInfo, IpcError> {
        let spec = spec_of(model_id)?;
        if !confirm {
            return Err(error(
                "confirmation_required",
                format!(
                    "Downloading {} takes {} MB; confirm first.",
                    spec.display_name,
                    spec.download_bytes() / 1_000_000
                ),
            ));
        }
        if !self.allowed().local {
            return Err(error(
                "embeddings_not_allowed",
                "Your organization's policy does not allow on-device embeddings.",
            ));
        }
        let Some(remote) = self.remote.clone() else {
            return Err(error("unavailable", "Downloads are not available here."));
        };
        let cancel = Arc::new(AtomicBool::new(false));
        {
            let mut running = self.running();
            if running.contains_key(spec.id) {
                return Err(error("model_busy", "That model is already downloading."));
            }
            running.insert(spec.id, Arc::clone(&cancel));
        }
        let base = crate::embeddings::model_base(self.policy.as_ref())
            .unwrap_or_else(|| HUGGING_FACE.to_owned());
        let this = Arc::clone(self);
        // Cleared however the task ends (done, failed, cancelled, panicked
        // or dropped with the runtime): a model is never left "busy".
        let entry = RunningEntry {
            host: Arc::clone(self),
            id: spec.id,
        };
        remote.runtime.spawn(async move {
            let entry = entry;
            this.report(spec, "downloading", 0, None);
            let client = remote.http.client().await;
            let progress = |p: Progress| this.report(spec, "downloading", p.done, None);
            let outcome = this
                .store
                .download(spec, client, &base, &progress, &cancel)
                .await;
            drop(entry);
            match outcome {
                Ok(()) => this.report(spec, "ready", spec.download_bytes(), None),
                Err(EmbedError::Paused) => this.report(spec, "cancelled", 0, None),
                Err(failure) => {
                    log::warn!("the model {} did not download: {failure}", spec.id);
                    this.report(spec, "failed", 0, Some(failure.to_string()));
                }
            }
        });
        Ok(self.info(spec))
    }

    /// `index_model_cancel`: `true` when a download was running (its part
    /// is kept for the next attempt).
    #[must_use]
    pub fn cancel(&self, model_id: &str) -> bool {
        self.running().get(model_id).is_some_and(|cancel| {
            cancel.store(true, Ordering::SeqCst);
            true
        })
    }

    /// `index_model_import`: copy the model's files from `folder` (each
    /// verified against its pinned digest). Blocking.
    ///
    /// # Errors
    ///
    /// `model_unknown`, `embeddings_not_allowed`, `model_busy`,
    /// `model_import_failed` (a file missing or not matching).
    pub fn import(&self, model_id: &str, folder: &str) -> Result<ModelInfo, IpcError> {
        let spec = spec_of(model_id)?;
        if !self.allowed().local {
            return Err(error(
                "embeddings_not_allowed",
                "Your organization's policy does not allow on-device embeddings.",
            ));
        }
        if self.running().contains_key(spec.id) {
            return Err(error("model_busy", "That model is downloading."));
        }
        self.store
            .import(spec, &PathBuf::from(folder))
            .map_err(|failure| error("model_import_failed", failure.to_string()))?;
        Ok(self.info(spec))
    }

    /// `index_model_remove`: delete the model's files (a workspace using it
    /// stops embedding until it is downloaded again).
    ///
    /// # Errors
    ///
    /// `model_unknown`, `model_busy`, `storage`.
    pub fn remove(&self, model_id: &str) -> Result<ModelInfo, IpcError> {
        let spec = spec_of(model_id)?;
        if self.running().contains_key(spec.id) {
            return Err(error("model_busy", "Cancel the download first."));
        }
        self.store
            .remove(spec)
            .map_err(|failure| error("storage", failure.to_string()))?;
        Ok(self.info(spec))
    }

    /// The embedding models `project_id` can use through the deployment.
    ///
    /// # Errors
    ///
    /// `embeddings_not_allowed`, `workspace_unbound`, `models_unavailable`.
    pub async fn remote_models(
        &self,
        project_id: Option<i64>,
    ) -> Result<Vec<crate::embeddings::RemoteModel>, IpcError> {
        let (true, Some(remote)) = (self.allowed().remote, self.remote.as_ref()) else {
            return Err(error(
                "embeddings_not_allowed",
                "Your organization's policy does not allow embeddings through the deployment.",
            ));
        };
        let project_id = project_id.ok_or_else(|| {
            error(
                "workspace_unbound",
                "Bind the folder to a project first: its budget pays for the embeddings.",
            )
        })?;
        crate::embeddings::remote_models(remote, project_id)
            .await
            .map_err(|failure| error("models_unavailable", failure.to_string()))
    }

    /// A `remote:<model>` choice completed with its width, probed with one
    /// short embedding in the workspace's project (`remote:<dim>:<model>`
    /// and every other value pass through).
    ///
    /// # Errors
    ///
    /// `embeddings_not_allowed`, `embeddings_unknown` (a bad model name),
    /// `workspace_unbound`, `embeddings_probe_failed`.
    pub async fn resolve_choice(
        &self,
        project_id: Option<i64>,
        value: &str,
    ) -> Result<String, IpcError> {
        let Some(model) = value.strip_prefix(REMOTE) else {
            return Ok(value.to_owned());
        };
        if model
            .split_once(':')
            .is_some_and(|(dim, _)| dim.parse::<usize>().is_ok())
        {
            return Ok(value.to_owned());
        }
        if !valid_model(model) {
            return Err(error("embeddings_unknown", "That is not a model name."));
        }
        let (true, Some(remote)) = (self.allowed().remote, self.remote.as_ref()) else {
            return Err(error(
                "embeddings_not_allowed",
                "Your organization's policy does not allow embeddings through the deployment.",
            ));
        };
        let project_id = project_id.ok_or_else(|| {
            error(
                "workspace_unbound",
                "Bind the folder to a project first: its budget pays for the embeddings.",
            )
        })?;
        let vectors = embed_remote(remote, project_id, model, &["dimension probe"], None)
            .await
            .map_err(|failure| error("embeddings_probe_failed", failure.to_string()))?;
        let dim = vectors.first().map_or(0, Vec::len);
        if dim == 0 {
            return Err(error(
                "embeddings_probe_failed",
                "The model answered no vector.",
            ));
        }
        Ok(format!("{REMOTE}{dim}:{model}"))
    }
}

/// The catalogue, its state on this computer, and what the policy allows.
#[tauri::command(rename_all = "snake_case")]
pub fn index_models(host: State<'_, Arc<ModelHost>>) -> Models {
    host.models()
}

/// Start (or resume) a model download; progress on `index://event`.
#[tauri::command(rename_all = "snake_case")]
pub fn index_model_download(
    host: State<'_, Arc<ModelHost>>,
    model_id: String,
    confirm: Option<bool>,
) -> Result<ModelInfo, IpcError> {
    host.download(&model_id, confirm.unwrap_or(false))
}

/// Stop a running download (its part is kept).
#[tauri::command(rename_all = "snake_case")]
pub fn index_model_cancel(host: State<'_, Arc<ModelHost>>, model_id: String) -> bool {
    host.cancel(&model_id)
}

/// Import a model's files from a folder: `folder`, or (without one) the
/// folder the person picks in the native dialog — `null` when they
/// cancelled.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_model_import(
    app: tauri::AppHandle,
    host: State<'_, Arc<ModelHost>>,
    model_id: String,
    folder: Option<String>,
) -> Result<Option<ModelInfo>, IpcError> {
    spec_of(&model_id)?;
    let folder = match folder {
        Some(folder) => folder,
        None => match crate::local_commands::pick_folder(&app).await {
            Some(picked) => picked.display().to_string(),
            None => return Ok(None),
        },
    };
    let host = Arc::clone(&host);
    tokio::task::spawn_blocking(move || host.import(&model_id, &folder))
        .await
        .map_err(|_| error("internal", "the import stopped"))?
        .map(Some)
}

/// Delete a model's files.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_model_remove(
    host: State<'_, Arc<ModelHost>>,
    model_id: String,
) -> Result<ModelInfo, IpcError> {
    let host = Arc::clone(&host);
    tokio::task::spawn_blocking(move || host.remove(&model_id))
        .await
        .map_err(|_| error("internal", "the removal stopped"))?
}

/// The embedding models the workspace's project can use through the
/// deployment (for the picker; the person may still type a name).
///
/// # Errors
///
/// `embeddings_not_allowed`, `workspace_unbound`, `models_unavailable`
/// (signed out, unreachable or refused: the UI falls back to a typed name).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_remote_models(
    state: State<'_, LocalState>,
    host: State<'_, Arc<ModelHost>>,
    workspace_id: String,
) -> Result<Vec<crate::embeddings::RemoteModel>, IpcError> {
    let project_id = state
        .workspaces
        .get(&workspace_id)
        .ok()
        .flatten()
        .and_then(|workspace| workspace.project_id);
    host.remote_models(project_id).await
}

/// Set the workspace's embeddings; `remote:<model>` is probed for its
/// width first.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_embeddings_set(
    state: State<'_, LocalState>,
    host: State<'_, Arc<ModelHost>>,
    workspace_id: String,
    embeddings: String,
) -> Result<String, IpcError> {
    let project_id = state
        .workspaces
        .get(&workspace_id)
        .ok()
        .flatten()
        .and_then(|workspace| workspace.project_id);
    let value = host.resolve_choice(project_id, &embeddings).await?;
    Ok(state.index.set_embeddings(&workspace_id, &value).await?)
}

/// The workspace's embedding progress (`null` before the first pass).
#[tauri::command(rename_all = "snake_case")]
pub fn index_embeddings_status(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Option<EmbedStatus> {
    state.index.embedding_status(&workspace_id)
}

/// Embed the workspace now whatever the power state (`full`: drop its
/// vectors and embed everything again).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_embeddings_rebuild(
    state: State<'_, LocalState>,
    workspace_id: String,
    full: Option<bool>,
) -> Result<(), IpcError> {
    Ok(state
        .index
        .rebuild_embeddings(&workspace_id, full.unwrap_or(false))
        .await?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    struct Policy(Mutex<Option<Value>>);

    impl PolicySource for Policy {
        fn local_work(&self) -> Option<Value> {
            self.0.lock().unwrap().clone()
        }
    }

    fn host(policy: Value) -> (tempfile::TempDir, Arc<ModelHost>) {
        let dir = tempfile::tempdir().unwrap();
        let host = Arc::new(ModelHost::new(
            dir.path(),
            Arc::new(Policy(Mutex::new(Some(policy)))),
            None,
            Arc::new(|_| {}),
        ));
        (dir, host)
    }

    #[test]
    fn the_catalogue_offers_both_models_and_downloads_nothing_unasked() {
        let (dir, host) = host(json!({"allowed": true, "local_index": true,
            "index_local_embeddings": true}));
        let models = host.models();
        assert!(models.local_allowed && !models.remote_allowed);
        let ids: Vec<&str> = models.models.iter().map(|m| m.id).collect();
        assert_eq!(ids, ["multilingual-e5-small", "qwen3-embedding-0.6b"]);
        assert!(models.models.iter().all(|m| m.state == ModelState::Missing
            && m.download_bytes > 400_000_000
            && m.ram_mb > 0));
        assert_eq!(models.cache_bytes, 0);
        assert!(!dir.path().join("models").exists(), "nothing created");
        let unconfirmed = host.download("multilingual-e5-small", false).unwrap_err();
        assert_eq!(unconfirmed.code, "confirmation_required");
        assert_eq!(
            host.download("nope", true).unwrap_err().code,
            "model_unknown"
        );
        assert!(!host.cancel("multilingual-e5-small"));
        let import = host
            .import("multilingual-e5-small", &dir.path().display().to_string())
            .unwrap_err();
        assert_eq!(import.code, "model_import_failed");
    }

    #[test]
    fn downloads_name_the_organisation_mirror_when_the_policy_sets_one() {
        let (_dir, plain) = host(json!({"allowed": true, "local_index": true,
            "index_local_embeddings": true}));
        assert_eq!(plain.models().mirror, None, "Hugging Face");
        let (_dir, mirrored) = host(json!({"allowed": true, "local_index": true,
            "index_local_embeddings": true,
            "index_model_mirror_url": "https://models.example.com/hf/"}));
        assert_eq!(
            mirrored.models().mirror.as_deref(),
            Some("models.example.com")
        );
    }

    #[test]
    fn the_policy_can_refuse_on_device_models() {
        let (_dir, host) = host(json!({"allowed": true, "local_index": true}));
        assert!(!host.models().local_allowed);
        assert_eq!(
            host.download("multilingual-e5-small", true)
                .unwrap_err()
                .code,
            "embeddings_not_allowed"
        );
    }

    #[tokio::test]
    async fn a_remote_choice_needs_the_policy_and_a_project() {
        let (_dir, host) = host(json!({"allowed": true, "local_index": true}));
        assert_eq!(
            host.resolve_choice(Some(1), "local:multilingual-e5-small")
                .await
                .unwrap(),
            "local:multilingual-e5-small"
        );
        assert_eq!(
            host.resolve_choice(Some(1), "remote:8:m").await.unwrap(),
            "remote:8:m",
            "already probed"
        );
        assert_eq!(
            host.resolve_choice(Some(1), "remote:text-embedding-3-small")
                .await
                .unwrap_err()
                .code,
            "embeddings_not_allowed"
        );
        assert_eq!(
            host.resolve_choice(Some(1), "remote:bad name")
                .await
                .unwrap_err()
                .code,
            "embeddings_unknown"
        );
    }
}
