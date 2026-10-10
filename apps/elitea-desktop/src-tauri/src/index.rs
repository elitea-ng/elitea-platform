//! The local workspace index on the host (ADR-0029 decision 7): one
//! [`IndexService`] per workspace that has its index turned on, kept in an
//! [`IndexRegistry`], and the `index_*` commands (IPC.md, "Local index").
//!
//! An index lives in the workspace's host data,
//! `<app data>/workspaces/<id>/index/index.sqlite` (owner-only, never
//! inside the folder). It is turned on per workspace (`index_enable`) and
//! only while the policy allows both local work and the index
//! (`local_work.allowed && local_work.local_index`); otherwise every
//! command but `index_disable` and `index_remove` answers
//! `local_index_disabled` and a turn is offered no index tools.
//!
//! Indexes are kept across sign-out and `host_wipe`: they hold code
//! structure parsed from the person's own folders, no account data. The
//! person removes one with `index_remove` (or by removing the workspace);
//! the Doctor can too.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, PoisonError};

use elitea_local_index::service::{
    IndexError, IndexEvent, IndexEvents, IndexService, IndexStatus, limit_parser_threads,
};
use elitea_local_index::sqlite_store::{FILE_NAME, SqliteGraphStore};
use elitea_local_tools::policy::LocalWorkPolicy;
use tauri::State;

use crate::d0::turn::{PolicySource, TurnError};
use crate::local_commands::{IpcError, LocalState};
use crate::workspaces::{Workspace, WorkspaceStore};

/// The event every index reports on, to the main window only.
pub const EVENT_NAME: &str = "index://event";

/// The setting that records whether the person turned the index on.
const ENABLED: &str = "enabled";

impl From<IndexError> for TurnError {
    fn from(error: IndexError) -> Self {
        Self::new(error.code, error.message)
    }
}

fn disabled() -> TurnError {
    TurnError::new(
        "local_index_disabled",
        "The local index is turned off by your organisation's policy.",
    )
}

struct Open {
    /// The `path_deny` it was opened with: a policy change reopens it.
    path_deny: Vec<String>,
    service: Arc<IndexService>,
}

/// Every workspace's index service, opened on first use.
pub struct IndexRegistry {
    workspaces: Arc<WorkspaceStore>,
    policy: Arc<dyn PolicySource>,
    events: Arc<dyn IndexEvents>,
    open: Mutex<HashMap<String, Open>>,
}

impl IndexRegistry {
    /// The registry; caps the parsers at half the cores.
    pub fn new(
        workspaces: Arc<WorkspaceStore>,
        policy: Arc<dyn PolicySource>,
        events: Arc<dyn IndexEvents>,
    ) -> Self {
        limit_parser_threads();
        Self {
            workspaces,
            policy,
            events,
            open: Mutex::new(HashMap::new()),
        }
    }

    /// The policy, when it allows the index: `allowed && local_index`.
    fn gate(&self) -> Result<LocalWorkPolicy, TurnError> {
        let policy: LocalWorkPolicy = self
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        if policy.allowed && policy.local_index {
            Ok(policy)
        } else {
            Err(disabled())
        }
    }

    /// Where a workspace's index lives.
    #[must_use]
    pub fn index_dir(&self, workspace_id: &str) -> PathBuf {
        self.workspaces.data_dir(workspace_id).join("index")
    }

    fn workspace(&self, workspace_id: &str) -> Result<Workspace, TurnError> {
        self.workspaces
            .get(workspace_id)
            .map_err(|e| TurnError::new("storage", e.to_string()))?
            .ok_or_else(|| TurnError::new("workspace_unknown", "That workspace is not open."))
    }

    fn open_map(&self) -> std::sync::MutexGuard<'_, HashMap<String, Open>> {
        self.open.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// Whether the person turned the workspace's index on (and has not
    /// turned it off since). Opens nothing.
    fn enabled(&self, workspace_id: &str) -> bool {
        if let Some(open) = self.open_map().get(workspace_id) {
            return open
                .service
                .store()
                .setting(ENABLED)
                .ok()
                .flatten()
                .as_deref()
                != Some("false");
        }
        let path = self.index_dir(workspace_id).join(FILE_NAME);
        if !path.is_file() {
            return false;
        }
        // Read without keeping it open.
        SqliteGraphStore::open(&self.index_dir(workspace_id))
            .ok()
            .and_then(|store| {
                let value = store.setting(ENABLED).ok().flatten();
                store.close();
                value
            })
            .as_deref()
            != Some("false")
    }

    /// The workspace's service, opened (and refreshed in the background,
    /// the folder may have changed while it was closed) on first use.
    /// Blocking.
    fn service(
        &self,
        workspace_id: &str,
        policy: &LocalWorkPolicy,
    ) -> Result<Arc<IndexService>, TurnError> {
        let workspace = self.workspace(workspace_id)?;
        let mut open = self.open_map();
        if let Some(existing) = open.get(workspace_id) {
            if existing.path_deny == policy.path_deny {
                return Ok(existing.service.clone());
            }
            existing.service.close();
            open.remove(workspace_id);
        }
        let service = IndexService::open(
            workspace_id,
            std::path::Path::new(&workspace.path),
            &policy.path_deny,
            &self.index_dir(workspace_id),
            self.events.clone(),
        )?;
        open.insert(
            workspace_id.to_owned(),
            Open {
                path_deny: policy.path_deny.clone(),
                service: service.clone(),
            },
        );
        drop(open);
        if let Err(error) = service.start_refresh(false) {
            log::warn!("an index could not start its first refresh: {}", error.code);
        }
        Ok(service)
    }

    /// `index_status`. Blocking.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub fn status(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        self.workspace(workspace_id)?;
        if !self.enabled(workspace_id) {
            return Ok(IndexStatus::off());
        }
        Ok(self.service(workspace_id, &policy)?.status())
    }

    /// `index_enable`: turn the index on and build it. Blocking.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub fn enable(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        let service = self.service(workspace_id, &policy)?;
        service
            .store()
            .set_setting(ENABLED, "true")
            .map_err(IndexError::from)?;
        match service.start_refresh(false) {
            Ok(()) => {}
            Err(error) if error.code == "index_busy" => {}
            Err(error) => return Err(error.into()),
        }
        Ok(service.status())
    }

    /// `index_disable`: stop building and stop offering the index; what
    /// was built is kept for when it is turned on again. Allowed whatever
    /// the policy says. Blocking.
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, or the index could not be written.
    pub fn disable(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        self.workspace(workspace_id)?;
        let open = self.open_map().remove(workspace_id);
        if let Some(open) = open {
            open.service.cancel();
            let saved = open.service.store().set_setting(ENABLED, "false");
            open.service.close();
            saved.map_err(IndexError::from)?;
        } else if self.index_dir(workspace_id).join(FILE_NAME).is_file() {
            let store =
                SqliteGraphStore::open(&self.index_dir(workspace_id)).map_err(IndexError::from)?;
            let saved = store.set_setting(ENABLED, "false");
            store.close();
            saved.map_err(IndexError::from)?;
        }
        Ok(IndexStatus::off())
    }

    /// `index_refresh`. Blocking.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off` (not turned on), `index_busy`.
    pub fn refresh(&self, workspace_id: &str, full: bool) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        self.workspace(workspace_id)?;
        if !self.enabled(workspace_id) {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self.service(workspace_id, &policy)?;
        match service.start_refresh(full) {
            // The open just started one; a full rebuild waits for it.
            Err(error) if error.code == "index_busy" && !full => {}
            other => other?,
        }
        Ok(service.status())
    }

    /// `index_cancel`: `true` when a refresh was running.
    #[must_use]
    pub fn cancel(&self, workspace_id: &str) -> bool {
        self.open_map()
            .get(workspace_id)
            .is_some_and(|open| open.service.cancel())
    }

    /// Stop the workspace's index and close its database, if open: what a
    /// removal does before the directory goes.
    pub fn forget(&self, workspace_id: &str) {
        let open = self.open_map().remove(workspace_id);
        if let Some(open) = open {
            open.service.close();
        }
    }

    /// `index_remove`: delete the workspace's index (it can be built
    /// again). Asks for `confirm`. Allowed whatever the policy says.
    ///
    /// # Errors
    ///
    /// `confirmation_required`, `workspace_unknown`, or the directory could
    /// not be deleted.
    pub fn remove(&self, workspace_id: &str, confirm: bool) -> Result<(), TurnError> {
        if !confirm {
            return Err(TurnError::new(
                "confirmation_required",
                "Removing the index deletes what was built; confirm to remove it.",
            ));
        }
        self.workspace(workspace_id)?;
        self.forget(workspace_id);
        let dir = self.index_dir(workspace_id);
        match std::fs::remove_dir_all(&dir) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(TurnError::new(
                "storage",
                format!("could not remove the index: {error}"),
            )),
        }
    }

    /// The service whose tools a turn in the workspace is offered: when the
    /// policy allows the index, it is turned on, and a build exists (a
    /// stale or rebuilding one too; its answers say so). Blocking.
    #[must_use]
    pub fn for_turn(&self, workspace_id: &str) -> Option<Arc<IndexService>> {
        let policy = self.gate().ok()?;
        if !self.enabled(workspace_id) {
            return None;
        }
        match self.service(workspace_id, &policy) {
            Ok(service) => service.view().is_some().then_some(service),
            Err(error) => {
                log::warn!("a turn runs without its index: {}", error.code);
                None
            }
        }
    }

    /// A turn changed `files` files of the workspace.
    pub fn mark_changed(&self, workspace_id: &str, files: usize) {
        if let Some(open) = self.open_map().get(workspace_id) {
            open.service
                .mark_changed(u64::try_from(files).unwrap_or(u64::MAX));
        }
    }
}

/// Index events to the main window only.
pub struct MainWindowIndexEvents(pub tauri::AppHandle);

impl IndexEvents for MainWindowIndexEvents {
    fn emit(&self, event: IndexEvent) {
        use tauri::Emitter as _;
        if let Err(error) = self.0.emit_to("main", EVENT_NAME, &event) {
            log::warn!("could not deliver an index event: {error}");
        }
    }
}

async fn blocking<T: Send + 'static>(
    state: &State<'_, LocalState>,
    work: impl FnOnce(&IndexRegistry) -> Result<T, TurnError> + Send + 'static,
) -> Result<T, IpcError> {
    let index = state.index.clone();
    tokio::task::spawn_blocking(move || work(&index))
        .await
        .map_err(|_| IpcError::from(TurnError::new("internal", "the index stopped unexpectedly")))?
        .map_err(IpcError::from)
}

/// Where the workspace's index is (`off` when not turned on).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_status(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    blocking(&state, move |index| index.status(&workspace_id)).await
}

/// Turn the workspace's index on and start building it.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_enable(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    blocking(&state, move |index| index.enable(&workspace_id)).await
}

/// Turn the workspace's index off (what was built is kept).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_disable(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    blocking(&state, move |index| index.disable(&workspace_id)).await
}

/// Refresh the index (`full`: rebuild it from nothing).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_refresh(
    state: State<'_, LocalState>,
    workspace_id: String,
    full: Option<bool>,
) -> Result<IndexStatus, IpcError> {
    blocking(&state, move |index| {
        index.refresh(&workspace_id, full.unwrap_or(false))
    })
    .await
}

/// Stop the running refresh; `false` when none runs.
#[tauri::command(rename_all = "snake_case")]
pub fn index_cancel(state: State<'_, LocalState>, workspace_id: String) -> bool {
    state.index.cancel(&workspace_id)
}

/// Delete the workspace's index; `confirm` must be `true`.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_remove(
    state: State<'_, LocalState>,
    workspace_id: String,
    confirm: Option<bool>,
) -> Result<(), IpcError> {
    blocking(&state, move |index| {
        index.remove(&workspace_id, confirm.unwrap_or(false))
    })
    .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_local_index::service::{IndexState, NoEvents};
    use serde_json::{Value, json};

    struct Policy(Mutex<Option<Value>>);

    impl PolicySource for Policy {
        fn local_work(&self) -> Option<Value> {
            self.0.lock().unwrap().clone()
        }
    }

    fn registry(policy: Value) -> (IndexRegistry, String, tempfile::TempDir, tempfile::TempDir) {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let registry = IndexRegistry::new(
            workspaces,
            Arc::new(Policy(Mutex::new(Some(policy)))),
            Arc::new(NoEvents),
        );
        (registry, id, app, folder)
    }

    async fn until_built(registry: &Arc<IndexRegistry>, id: &str) -> IndexStatus {
        for _ in 0..500 {
            let registry = registry.clone();
            let id = id.to_owned();
            let status = tokio::task::spawn_blocking(move || registry.status(&id))
                .await
                .unwrap()
                .unwrap();
            if status.state == IndexState::Ready {
                return status;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        panic!("the index was not built");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_policy_gates_every_command_but_turning_it_off() {
        for policy in [
            json!({"allowed": true}),
            json!({"allowed": true, "local_index": false}),
            json!({"allowed": false, "local_index": true}),
        ] {
            let (registry, id, _app, _folder) = registry(policy.clone());
            assert_eq!(
                registry.status(&id).unwrap_err().code,
                "local_index_disabled"
            );
            assert_eq!(
                registry.enable(&id).unwrap_err().code,
                "local_index_disabled"
            );
            assert_eq!(
                registry.refresh(&id, false).unwrap_err().code,
                "local_index_disabled"
            );
            assert!(registry.for_turn(&id).is_none(), "{policy}");
            assert_eq!(registry.disable(&id).unwrap().state, IndexState::Off);
            assert!(registry.remove(&id, true).is_ok());
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_index_is_built_offered_turned_off_and_removed() {
        let (registry, id, _app, _folder) = registry(json!({"allowed": true, "local_index": true}));
        let registry = Arc::new(registry);
        assert_eq!(registry.status(&id).unwrap().state, IndexState::Off);
        assert!(registry.for_turn(&id).is_none(), "not turned on");
        assert_eq!(registry.refresh(&id, false).unwrap_err().code, "index_off");
        {
            let registry = registry.clone();
            let id = id.clone();
            tokio::task::spawn_blocking(move || registry.enable(&id))
                .await
                .unwrap()
                .unwrap();
        }
        let built = until_built(&registry, &id).await;
        assert!(built.entities >= 2, "{built:?}");
        let dir = registry.index_dir(&id);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&dir).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o700);
        }
        assert!(registry.for_turn(&id).is_some());

        // Off: not offered, the build kept; on again: offered at once.
        assert_eq!(registry.disable(&id).unwrap().state, IndexState::Off);
        assert_eq!(registry.status(&id).unwrap().state, IndexState::Off);
        assert!(registry.for_turn(&id).is_none());
        assert!(dir.join(FILE_NAME).is_file(), "turning it off keeps it");
        {
            let registry = registry.clone();
            let id = id.clone();
            tokio::task::spawn_blocking(move || registry.enable(&id))
                .await
                .unwrap()
                .unwrap();
        }
        assert!(registry.for_turn(&id).is_some(), "the kept build answers");
        until_built(&registry, &id).await;

        assert_eq!(
            registry.remove(&id, false).unwrap_err().code,
            "confirmation_required"
        );
        registry.remove(&id, true).unwrap();
        assert!(!dir.exists());
        assert_eq!(registry.status(&id).unwrap().state, IndexState::Off);
    }
}
