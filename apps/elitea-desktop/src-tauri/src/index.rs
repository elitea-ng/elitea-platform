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
//! **Opened lazily.** `index_status` never opens an index nor starts a
//! refresh: until it is open it answers from a small read of the file
//! (`SqliteGraphStore::peek`, cached). An index opens — and checks the
//! folder for what changed while it was closed — on a turn in its
//! workspace, on `index_open` (the workspace's session page), or on an
//! explicit command. Refreshes run one at a time across workspaces (the
//! service's own gate).
//!
//! **Locking.** Each workspace has its own async slot; the registry's map
//! is locked only to find or add a slot, never across I/O. Opening, turning
//! off and removing hold the workspace's slot for their whole length, so a
//! removal (`index_remove`, `workspace_remove`, the Doctor's repairs) closes
//! the index and deletes its files before any status read or turn can open
//! it again. A closed service answers no tool call.
//!
//! Indexes are kept across sign-out and `host_wipe`: they hold code
//! structure parsed from the person's own folders, no account data. The
//! person removes one with `index_remove` (or by removing the workspace);
//! the Doctor can too.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, PoisonError};

use elitea_local_index::service::{
    IndexError, IndexEvent, IndexEvents, IndexService, IndexState, IndexStatus,
    limit_parser_threads, policy_fingerprint,
};
use elitea_local_index::sqlite_store::{FILE_NAME, Peek, SqliteGraphStore};
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

fn stopped() -> TurnError {
    TurnError::new("internal", "the index stopped unexpectedly")
}

/// Run blocking index work (SQLite, the disk) on the blocking pool.
async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> T + Send + 'static,
) -> Result<T, TurnError> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|_| stopped())
}

/// What one workspace's slot knows.
#[derive(Default)]
struct Slot {
    /// The open service, and whether the index is turned on.
    open: Option<Open>,
    /// What a small read of the index file said, while it is not open
    /// (`Some(None)`: there is no index file). Cleared by every change.
    peeked: Option<Option<Peek>>,
}

struct Open {
    service: Arc<IndexService>,
    /// The `enabled` setting, as last written or read.
    enabled: bool,
}

impl Open {
    fn status(&self) -> IndexStatus {
        if self.enabled {
            self.service.status()
        } else {
            IndexStatus::off()
        }
    }
}

/// One workspace's slot: held across the I/O of opening, turning off and
/// removing its index, and only by that workspace's commands.
#[derive(Default)]
struct Entry {
    slot: tokio::sync::Mutex<Slot>,
}

fn is_enabled(peek: Option<&Peek>) -> bool {
    peek.is_some_and(|peek| peek.settings.get(ENABLED).map(String::as_str) != Some("false"))
}

/// What `index_status` answers for an index that is not open, from a read
/// of its file under the policy fingerprint `policy`.
fn peeked_status(peek: Option<&Peek>, policy: &str) -> IndexStatus {
    let Some(peek) = peek.filter(|peek| is_enabled(Some(peek))) else {
        return IndexStatus::off();
    };
    let stale_policy = peek.built && peek.policy.as_deref() != Some(policy);
    let mut status = IndexStatus::off();
    status.last_run.clone_from(&peek.last_run);
    if stale_policy {
        status.state = IndexState::StalePolicy;
    } else {
        // Not checked since the app started (or never built).
        status.state = IndexState::Stale;
        status.files = peek.documents;
        status.entities = peek.entities;
        status.relations = peek.relations;
    }
    status
}

/// Every workspace's index service, opened on first use.
pub struct IndexRegistry {
    workspaces: Arc<WorkspaceStore>,
    policy: Arc<dyn PolicySource>,
    events: Arc<dyn IndexEvents>,
    /// Locked only to find or add a slot.
    entries: Mutex<HashMap<String, Arc<Entry>>>,
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
            entries: Mutex::new(HashMap::new()),
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

    /// The policy fingerprint an index is served under now.
    fn fingerprint(&self) -> String {
        let policy: LocalWorkPolicy = self
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        policy_fingerprint(&policy.path_deny)
    }

    /// Where a workspace's index lives.
    #[must_use]
    pub fn index_dir(&self, workspace_id: &str) -> PathBuf {
        self.workspaces.data_dir(workspace_id).join("index")
    }

    async fn workspace(&self, workspace_id: &str) -> Result<Workspace, TurnError> {
        let workspaces = self.workspaces.clone();
        let id = workspace_id.to_owned();
        blocking(move || workspaces.get(&id))
            .await?
            .map_err(|e| TurnError::new("storage", e.to_string()))?
            .ok_or_else(|| TurnError::new("workspace_unknown", "That workspace is not open."))
    }

    fn entry(&self, workspace_id: &str) -> Arc<Entry> {
        self.entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .entry(workspace_id.to_owned())
            .or_default()
            .clone()
    }

    /// What the index file says, read once and kept until something
    /// changes it (the slot is held).
    async fn peek(&self, workspace_id: &str, slot: &mut Slot) -> Result<Option<Peek>, TurnError> {
        if let Some(peeked) = &slot.peeked {
            return Ok(peeked.clone());
        }
        let dir = self.index_dir(workspace_id);
        let peeked = blocking(move || SqliteGraphStore::peek(&dir))
            .await?
            .map_err(IndexError::from)?;
        slot.peeked = Some(peeked.clone());
        Ok(peeked)
    }

    /// Whether the index is turned on, from the open service or the file.
    async fn enabled(&self, workspace_id: &str, slot: &mut Slot) -> Result<bool, TurnError> {
        match &slot.open {
            Some(open) => Ok(open.enabled),
            None => Ok(is_enabled(self.peek(workspace_id, slot).await?.as_ref())),
        }
    }

    /// The workspace's service in its held slot, opened (and checked for
    /// what changed while it was closed: a refresh in the background, with
    /// `catch_up`) when it is not open, or open under another policy.
    async fn open_in(
        &self,
        workspace: &Workspace,
        policy: &LocalWorkPolicy,
        slot: &mut Slot,
        catch_up: bool,
    ) -> Result<Arc<IndexService>, TurnError> {
        let fingerprint = policy_fingerprint(&policy.path_deny);
        if let Some(open) = &slot.open {
            if open.service.policy() == fingerprint && !open.service.is_closed() {
                return Ok(open.service.clone());
            }
            // Another policy: this one is closed (the tools a turn holds
            // answer that it is gone) and the index reopened under the new.
            let old = open.service.clone();
            blocking(move || old.close()).await?;
        }
        let enabled = match slot.open.take() {
            Some(open) => open.enabled,
            None => is_enabled(self.peek(&workspace.id, slot).await?.as_ref()),
        };
        let (id, root, deny, dir, events) = (
            workspace.id.clone(),
            PathBuf::from(&workspace.path),
            policy.path_deny.clone(),
            self.index_dir(&workspace.id),
            self.events.clone(),
        );
        let service =
            blocking(move || IndexService::open(&id, &root, &deny, &dir, events)).await??;
        slot.peeked = None;
        slot.open = Some(Open {
            service: service.clone(),
            enabled,
        });
        if catch_up
            && enabled
            && let Err(error) = service.start_refresh(false)
        {
            log::warn!("an index could not start its first refresh: {}", error.code);
        }
        Ok(service)
    }

    /// `index_status`: never opens the index nor starts a refresh.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub async fn status(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        self.gate()?;
        self.workspace(workspace_id).await?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        if let Some(open) = &slot.open {
            return Ok(open.status());
        }
        let peek = self.peek(workspace_id, &mut slot).await?;
        Ok(peeked_status(peek.as_ref(), &self.fingerprint()))
    }

    /// `index_open`: open the workspace's index (its session page is open)
    /// and check the folder in the background; `off` when not turned on.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub async fn open(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Ok(IndexStatus::off());
        }
        self.open_in(&workspace, &policy, &mut slot, true).await?;
        Ok(slot
            .open
            .as_ref()
            .map_or_else(IndexStatus::off, Open::status))
    }

    /// `index_enable`: turn the index on and build it.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub async fn enable(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        let service = self.open_in(&workspace, &policy, &mut slot, false).await?;
        let writer = service.clone();
        blocking(move || writer.store().set_setting(ENABLED, "true"))
            .await?
            .map_err(IndexError::from)?;
        if let Some(open) = slot.open.as_mut() {
            open.enabled = true;
        }
        match service.start_refresh(false) {
            Ok(()) => {}
            Err(error) if error.code == "index_busy" => {}
            Err(error) => return Err(error.into()),
        }
        Ok(service.status())
    }

    /// `index_disable`: stop building and stop offering the index; what
    /// was built is kept for when it is turned on again. Allowed whatever
    /// the policy says.
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, or the index could not be written.
    pub async fn disable(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        self.workspace(workspace_id).await?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        let open = slot.open.take();
        slot.peeked = None;
        let dir = self.index_dir(workspace_id);
        blocking(move || {
            if let Some(open) = open {
                open.service.cancel();
                let saved = open.service.store().set_setting(ENABLED, "false");
                open.service.close();
                saved
            } else if std::fs::symlink_metadata(dir.join(FILE_NAME)).is_ok() {
                let store = SqliteGraphStore::open(&dir)?;
                let saved = store.set_setting(ENABLED, "false");
                store.close();
                saved
            } else {
                Ok(())
            }
        })
        .await?
        .map_err(IndexError::from)?;
        Ok(IndexStatus::off())
    }

    /// `index_refresh` (`full`: rebuild from nothing).
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off` (not turned on), `index_busy`.
    pub async fn refresh(&self, workspace_id: &str, full: bool) -> Result<IndexStatus, TurnError> {
        let policy = self.gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self.open_in(&workspace, &policy, &mut slot, false).await?;
        match service.start_refresh(full) {
            // An incremental refresh asked for while one runs: that one is it.
            Err(error) if error.code == "index_busy" && !full => {}
            other => other?,
        }
        Ok(service.status())
    }

    /// `index_cancel`: `true` when a refresh was running.
    pub async fn cancel(&self, workspace_id: &str) -> bool {
        let entry = self
            .entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .get(workspace_id)
            .cloned();
        let Some(entry) = entry else { return false };
        let slot = entry.slot.lock().await;
        slot.open.as_ref().is_some_and(|open| open.service.cancel())
    }

    /// Close the workspace's index (if open) and run `work` — the removal
    /// of its files — holding its slot throughout, on the blocking pool: no
    /// status read or turn opens it again before `work` is done.
    pub async fn with_closed<T: Send + 'static>(
        &self,
        workspace_id: &str,
        work: impl FnOnce() -> T + Send + 'static,
    ) -> Result<T, TurnError> {
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        let open = slot.open.take();
        slot.peeked = None;
        let done = blocking(move || {
            if let Some(open) = open {
                open.service.close();
            }
            work()
        })
        .await;
        drop(slot);
        done
    }

    /// [`Self::with_closed`] for a caller on a blocking thread (the
    /// Doctor's repairs), running `work` on that thread.
    ///
    /// # Panics
    ///
    /// Called from an async context (`tokio`'s `blocking_lock`).
    pub fn with_closed_blocking<T>(&self, workspace_id: &str, work: impl FnOnce() -> T) -> T {
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.blocking_lock();
        if let Some(open) = slot.open.take() {
            open.service.close();
        }
        slot.peeked = None;
        let done = work();
        drop(slot);
        done
    }

    /// `index_remove`: delete the workspace's index (it can be built
    /// again). Asks for `confirm`. Allowed whatever the policy says.
    ///
    /// # Errors
    ///
    /// `confirmation_required`, `workspace_unknown`, or the directory could
    /// not be deleted.
    pub async fn remove(&self, workspace_id: &str, confirm: bool) -> Result<(), TurnError> {
        if !confirm {
            return Err(TurnError::new(
                "confirmation_required",
                "Removing the index deletes what was built; confirm to remove it.",
            ));
        }
        self.workspace(workspace_id).await?;
        let dir = self.index_dir(workspace_id);
        self.with_closed(workspace_id, move || match std::fs::remove_dir_all(&dir) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(TurnError::new(
                "storage",
                format!("could not remove the index: {error}"),
            )),
        })
        .await?
    }

    /// The service whose tools a turn in the workspace is offered: when the
    /// policy allows the index, it is turned on, and a build made under the
    /// current policy exists (a stale or rebuilding one too; its answers say
    /// so). Opens the index on first use.
    pub async fn for_turn(&self, workspace_id: &str) -> Option<Arc<IndexService>> {
        let policy = self.gate().ok()?;
        let workspace = self.workspace(workspace_id).await.ok()?;
        let entry = self.entry(workspace_id);
        let mut slot = entry.slot.lock().await;
        match self.enabled(workspace_id, &mut slot).await {
            Ok(true) => {}
            Ok(false) => return None,
            Err(error) => {
                log::warn!("a turn runs without its index: {}", error.code);
                return None;
            }
        }
        match self.open_in(&workspace, &policy, &mut slot, true).await {
            Ok(service) => service.view().is_some().then_some(service),
            Err(error) => {
                log::warn!("a turn runs without its index: {}", error.code);
                None
            }
        }
    }

    /// A turn changed `files` files of the workspace: its index (if open)
    /// is stale, and refreshes a few seconds after the changes stop.
    pub async fn mark_changed(&self, workspace_id: &str, files: usize) {
        let entry = self
            .entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .get(workspace_id)
            .cloned();
        let Some(entry) = entry else { return };
        let slot = entry.slot.lock().await;
        if let Some(open) = slot.open.as_ref().filter(|open| open.enabled) {
            open.service
                .mark_changed(u64::try_from(files).unwrap_or(u64::MAX));
        }
    }

    /// Whether the workspace's index is open (tests).
    #[cfg(test)]
    async fn is_open(&self, workspace_id: &str) -> bool {
        self.entry(workspace_id).slot.lock().await.open.is_some()
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

/// Where the workspace's index is (`off` when not turned on). Never opens
/// it: an index not open yet answers from a small read of its file.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_status(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    Ok(state.index.status(&workspace_id).await?)
}

/// Open the workspace's index and check the folder (its session page is
/// open).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_open(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    Ok(state.index.open(&workspace_id).await?)
}

/// Turn the workspace's index on and start building it.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_enable(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    Ok(state.index.enable(&workspace_id).await?)
}

/// Turn the workspace's index off (what was built is kept).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_disable(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<IndexStatus, IpcError> {
    Ok(state.index.disable(&workspace_id).await?)
}

/// Refresh the index (`full`: rebuild it from nothing).
#[tauri::command(rename_all = "snake_case")]
pub async fn index_refresh(
    state: State<'_, LocalState>,
    workspace_id: String,
    full: Option<bool>,
) -> Result<IndexStatus, IpcError> {
    Ok(state
        .index
        .refresh(&workspace_id, full.unwrap_or(false))
        .await?)
}

/// Stop the running refresh; `false` when none runs.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_cancel(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<bool, IpcError> {
    Ok(state.index.cancel(&workspace_id).await)
}

/// Delete the workspace's index; `confirm` must be `true`.
#[tauri::command(rename_all = "snake_case")]
pub async fn index_remove(
    state: State<'_, LocalState>,
    workspace_id: String,
    confirm: Option<bool>,
) -> Result<(), IpcError> {
    Ok(state
        .index
        .remove(&workspace_id, confirm.unwrap_or(false))
        .await?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_local_index::service::NoEvents;
    use serde_json::{Value, json};
    use std::time::Duration;

    struct Policy(Mutex<Option<Value>>);

    impl PolicySource for Policy {
        fn local_work(&self) -> Option<Value> {
            self.0.lock().unwrap().clone()
        }
    }

    struct Fixture {
        registry: Arc<IndexRegistry>,
        id: String,
        policy: Arc<Policy>,
        workspaces: Arc<WorkspaceStore>,
        _app: tempfile::TempDir,
        folder: tempfile::TempDir,
    }

    fn registry(policy: Value) -> Fixture {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(policy))));
        let registry = Arc::new(IndexRegistry::new(
            workspaces.clone(),
            policy.clone(),
            Arc::new(NoEvents),
        ));
        Fixture {
            registry,
            id,
            policy,
            workspaces,
            _app: app,
            folder,
        }
    }

    fn on() -> Value {
        json!({"allowed": true, "local_index": true})
    }

    async fn until_built(registry: &IndexRegistry, id: &str) -> IndexStatus {
        for _ in 0..500 {
            let status = registry.status(id).await.unwrap();
            if status.state == IndexState::Ready {
                return status;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
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
            let f = registry(policy.clone());
            let (registry, id) = (&f.registry, &f.id);
            assert_eq!(
                registry.status(id).await.unwrap_err().code,
                "local_index_disabled"
            );
            assert_eq!(
                registry.open(id).await.unwrap_err().code,
                "local_index_disabled"
            );
            assert_eq!(
                registry.enable(id).await.unwrap_err().code,
                "local_index_disabled"
            );
            assert_eq!(
                registry.refresh(id, false).await.unwrap_err().code,
                "local_index_disabled"
            );
            assert!(registry.for_turn(id).await.is_none(), "{policy}");
            assert_eq!(registry.disable(id).await.unwrap().state, IndexState::Off);
            assert!(registry.remove(id, true).await.is_ok());
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_index_is_built_offered_turned_off_and_removed() {
        let f = registry(on());
        let (registry, id) = (&f.registry, &f.id);
        assert_eq!(registry.status(id).await.unwrap().state, IndexState::Off);
        assert!(registry.for_turn(id).await.is_none(), "not turned on");
        assert_eq!(
            registry.refresh(id, false).await.unwrap_err().code,
            "index_off"
        );
        registry.enable(id).await.unwrap();
        let built = until_built(registry, id).await;
        assert!(built.entities >= 2, "{built:?}");
        let dir = registry.index_dir(id);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&dir).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o700);
        }
        let held = registry.for_turn(id).await.unwrap();

        // Off: not offered, the build kept; on again: offered at once.
        assert_eq!(registry.disable(id).await.unwrap().state, IndexState::Off);
        assert_eq!(registry.status(id).await.unwrap().state, IndexState::Off);
        assert!(registry.for_turn(id).await.is_none());
        assert!(dir.join(FILE_NAME).is_file(), "turning it off keeps it");
        assert!(held.is_closed(), "the turn's tools answer nothing more");
        registry.enable(id).await.unwrap();
        assert!(
            registry.for_turn(id).await.is_some(),
            "the kept build answers"
        );
        until_built(registry, id).await;

        assert_eq!(
            registry.remove(id, false).await.unwrap_err().code,
            "confirmation_required"
        );
        registry.remove(id, true).await.unwrap();
        assert!(!dir.exists());
        assert_eq!(registry.status(id).await.unwrap().state, IndexState::Off);
    }

    /// `index_status` answers from the file without opening the index or
    /// starting a refresh (every workspace row asks at launch); the index
    /// opens on `index_open`, a turn, or a command.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn status_never_opens_the_index_and_open_does() {
        let f = registry(on());
        f.registry.enable(&f.id).await.unwrap();
        let built = until_built(&f.registry, &f.id).await;

        // The app restarts.
        let registry =
            IndexRegistry::new(f.workspaces.clone(), f.policy.clone(), Arc::new(NoEvents));
        let runs = || {
            let store = SqliteGraphStore::open(&registry.index_dir(&f.id)).unwrap();
            let runs = store
                .runs(elitea_local_index::sqlite_store::GraphKey::LOCAL)
                .unwrap()
                .len();
            store.close();
            runs
        };
        let before = runs();
        let status = registry.status(&f.id).await.unwrap();
        assert_eq!(
            status.state,
            IndexState::Stale,
            "not checked since the start"
        );
        assert_eq!(status.entities, built.entities);
        assert_eq!(status.files, built.files);
        assert_eq!(status.last_run, built.last_run);
        registry.status(&f.id).await.unwrap();
        assert!(
            !registry.is_open(&f.id).await,
            "a status read opens nothing"
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(runs(), before, "and starts no refresh");

        let opened = registry.open(&f.id).await.unwrap();
        assert!(
            matches!(opened.state, IndexState::Building | IndexState::Ready),
            "{opened:?}"
        );
        assert!(registry.is_open(&f.id).await);
        until_built(&registry, &f.id).await;
    }

    /// A removal holds the workspace's slot from the close to the deletion:
    /// a turn asking meanwhile waits, then finds no index, and nothing is
    /// recreated. Other workspaces are not held up meanwhile.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn a_removal_holds_the_workspace_until_its_files_are_gone() {
        let f = registry(on());
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let held = f.registry.for_turn(&f.id).await.unwrap();
        let other_folder = tempfile::tempdir().unwrap();
        let other = f.workspaces.add(other_folder.path()).unwrap().id;

        let dir = f.registry.index_dir(&f.id);
        let runtime = tokio::runtime::Handle::current();
        let (registry, id) = (f.registry.clone(), f.id.clone());
        let (asked_early, turn, other_status) = f
            .registry
            .with_closed(&f.id, move || {
                let turn = runtime.spawn({
                    let registry = registry.clone();
                    async move { registry.for_turn(&id).await.is_some() }
                });
                // Another workspace's status is answered meanwhile.
                let other_status = runtime.block_on(async {
                    tokio::time::timeout(Duration::from_secs(5), registry.status(&other)).await
                });
                std::thread::sleep(Duration::from_millis(200));
                let asked_early = turn.is_finished();
                std::fs::remove_dir_all(&dir).unwrap();
                (asked_early, turn, other_status)
            })
            .await
            .unwrap();
        assert!(!asked_early, "the turn waited for the removal");
        assert!(!turn.await.unwrap(), "and found no index");
        assert!(
            !f.registry.index_dir(&f.id).exists(),
            "nothing recreated it"
        );
        assert_eq!(other_status.unwrap().unwrap().state, IndexState::Off);
        assert!(held.is_closed());
        let answer = elitea_local_index::tools::IndexToolset::new(held)
            .current_tools()
            .remove(0)
            .call(json!({"query": "A"}))
            .await;
        assert_eq!(answer["code"], "index.closed", "{answer}");
        drop(f.folder);
    }

    /// A policy change reopens the index under the new policy: the build
    /// made under the old one is not offered until a rebuild commits.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_policy_change_withholds_the_old_build() {
        let f = registry(on());
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let held = f.registry.for_turn(&f.id).await.unwrap();
        *f.policy.0.lock().unwrap() =
            Some(json!({"allowed": true, "local_index": true, "path_deny": ["a.py"]}));
        // The turn's old service is closed; the new one serves nothing old.
        let offered = f.registry.for_turn(&f.id).await;
        assert!(held.is_closed());
        if let Some(service) = &offered {
            let (view, _) = service.view().unwrap();
            assert!(
                !view.graph.nodes().any(|(_, node)| node["name"] == "A"),
                "only a build under the new policy is offered"
            );
        }
        let rebuilt = until_built(&f.registry, &f.id).await;
        assert_eq!(rebuilt.entities, 0, "{rebuilt:?}");
    }
}
