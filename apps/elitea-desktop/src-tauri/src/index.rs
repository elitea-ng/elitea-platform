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
//! is locked only to find, add or drop an entry, never across I/O. Opening,
//! turning off and removing hold the workspace's slot for their whole
//! length, so a removal (`index_remove`, `workspace_remove`, the Doctor's
//! repairs) closes the index and deletes its files before any status read
//! or turn can open it again, and drops the workspace's entry afterwards.
//! A closed service answers no tool call. `index_cancel` and a turn's
//! change signal never take the slot: they reach the open service through
//! the entry, so neither waits behind an open loading a large graph.
//!
//! **The policy is live.** Every entry point reads the policy once
//! (`policy_now`) and checks it against the open service: turned off, the
//! service is closed; made under another `path_deny`, it is closed and
//! reopened under the new one, which rebuilds before it is served. No
//! refresh ever runs with the `Workspace` of an older policy.
//!
//! **Watched.** While an index is open and turned on, its folder is
//! watched for changes made outside the app (the person's editor, a
//! `git pull`): once the folder has been quiet for 3 s, and at most every
//! 30 s, the index refreshes incrementally — under the policy then, as
//! after a turn. Changes to ignored, denied or `.git` paths are not
//! changes (`elitea_local_index::watch`). The watch stops when the index
//! is turned off, removed or closed (the policy turned it off or changed)
//! and at sign-out; it starts again when the index opens, and at sign-in
//! for an index still open. `IndexStatus::watching` says whether it runs.
//!
//! **Rebuilt when idle.** Incremental refreshes drift from what a full
//! build makes; once more than `DRIFT_REBUILD` files changed since the last
//! full build (`IndexService::wants_full_rebuild`), the index is rebuilt
//! from nothing after [`IDLE_REBUILD`] with no refresh, no change and no
//! turn running.
//!
//! Indexes are kept across sign-out and `host_wipe`: they hold code
//! structure parsed from the person's own folders, no account data (only
//! their folder watches stop at sign-out: nothing refreshes for nobody). The
//! person removes one with `index_remove` (or by removing the workspace);
//! the Doctor can too.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, PoisonError, Weak};
use std::time::Duration;

use elitea_local_index::embed_caps::EmbedCaps;
use elitea_local_index::embedding::{EmbedStatus, Embedding, OFF, SETTING, TurnGuard, choice_of};

use crate::embeddings::{Allowed, Choice};
use elitea_local_index::service::{
    IndexError, IndexEvent, IndexEvents, IndexService, IndexState, IndexStatus,
    limit_parser_threads, policy_fingerprint,
};
use elitea_local_index::sqlite_store::{FILE_NAME, Peek, SqliteGraphStore, StoreError};
use elitea_local_index::watch::{FolderWatcher, WatchConfig};
use elitea_local_tools::deny::DenyList;
use elitea_local_tools::policy::LocalWorkPolicy;
use tauri::State;

use crate::d0::turn::{PolicySource, TurnError};
use crate::local_commands::{IpcError, LocalState};
use crate::workspaces::{Workspace, WorkspaceStore};

/// The event every index reports on, to the main window only.
pub const EVENT_NAME: &str = "index://event";

/// The setting that records whether the person turned the index on.
const ENABLED: &str = "enabled";

/// What embedding a workspace would take
/// ([`IndexRegistry::embedding_basis`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
pub struct EmbeddingBasis {
    /// Distinct chunk texts to embed.
    pub texts: u64,
    /// Their estimated tokens.
    pub tokens: u64,
    pub caps: EmbedCaps,
    /// Estimated tokens a remote embedder took today (UTC).
    pub day_used: u64,
}

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

/// How long after a turn's last change its index refreshes.
pub const DEFAULT_REFRESH_DELAY: Duration = Duration::from_secs(3);

/// How long after start-up an unfinished embedding pass resumes.
pub const RESUME_EMBEDDINGS_AFTER: Duration = Duration::from_secs(20);

/// How long a refresh that found another one running waits to try again.
const BUSY_RETRY: Duration = Duration::from_secs(1);

/// How long an index that wants a full rebuild must be idle (no refresh,
/// no change, no turn) before it gets one.
pub const IDLE_REBUILD: Duration = Duration::from_secs(60);

/// Held while a turn runs: background embedding waits between batches, and
/// no idle rebuild starts.
pub struct TurnHold {
    _embedding: Option<TurnGuard>,
    turns: Arc<AtomicUsize>,
}

impl Drop for TurnHold {
    fn drop(&mut self) {
        self.turns.fetch_sub(1, Ordering::SeqCst);
    }
}

/// What one workspace's slot knows.
#[derive(Default)]
struct Slot {
    /// The open service, and whether the index is turned on.
    open: Option<Open>,
    /// What a small read of the index file said, while it is not open
    /// (`Some(None)`: there is no index file). Cleared by every change.
    peeked: Option<Option<Peek>>,
    /// The entry was dropped from the registry (its index removed): a
    /// caller that waited for this slot looks the workspace up again.
    removed: bool,
}

struct Open {
    service: Arc<IndexService>,
    /// The `enabled` setting, as last written or read.
    enabled: bool,
    /// The folder's watch while the index is turned on; dropped (stopped)
    /// with the service.
    watcher: Option<FolderWatcher>,
}

impl Open {
    fn status(&self) -> IndexStatus {
        if self.enabled {
            self.service.status()
        } else {
            IndexStatus {
                on_disk: true,
                ..IndexStatus::off()
            }
        }
    }
}

/// One workspace: its slot, held across the I/O of opening, turning off
/// and removing its index, and what must never wait behind that I/O.
#[derive(Default)]
struct Entry {
    slot: Arc<tokio::sync::Mutex<Slot>>,
    /// The open service, readable without the slot: `index_cancel` and a
    /// turn's change signal never wait behind an open.
    current: Mutex<Option<Arc<IndexService>>>,
    /// Bumped by every change signal: the debounced refresh runs for the
    /// latest only.
    changes: AtomicU64,
    /// Bumped by every completed refresh: an idle rebuild is for the
    /// latest only.
    readies: AtomicU64,
}

impl Entry {
    fn current(&self) -> Option<Arc<IndexService>> {
        self.current
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    fn set_current(&self, service: Option<Arc<IndexService>>) {
        *self.current.lock().unwrap_or_else(PoisonError::into_inner) = service;
    }
}

type SlotGuard = tokio::sync::OwnedMutexGuard<Slot>;

fn is_enabled(peek: Option<&Peek>) -> bool {
    peek.is_some_and(|peek| peek.settings.get(ENABLED).map(String::as_str) != Some("false"))
}

/// What `index_status` answers for an index that is not open, from a read
/// of its file under the policy fingerprint `policy`.
fn peeked_status(peek: Option<&Peek>, policy: &str) -> IndexStatus {
    let on_disk = peek.is_some();
    let Some(peek) = peek.filter(|peek| is_enabled(Some(peek))) else {
        return IndexStatus {
            on_disk,
            ..IndexStatus::off()
        };
    };
    let stale_policy = peek.built && peek.policy.as_deref() != Some(policy);
    let mut status = IndexStatus {
        on_disk,
        ..IndexStatus::off()
    };
    status.last_run.clone_from(&peek.last_run);
    if stale_policy {
        status.state = IndexState::StalePolicy;
    } else {
        // Not checked since the app started (or never built).
        status.state = IndexState::Stale;
        status.files = peek.documents;
        status.entities = peek.entities;
        status.relations = peek.relations;
        status.chunks = peek.chunks;
    }
    status
}

/// Whether the index's embedding pass was left unfinished (the app quit
/// mid-pass): turned on, a model chosen, fewer vectors than distinct chunk
/// texts. Its next pass embeds only what is missing.
fn embedding_unfinished(peek: &Peek) -> bool {
    let chosen = peek
        .settings
        .get(SETTING)
        .is_some_and(|choice| !choice.is_empty() && choice != OFF);
    let kept = peek.vectors.values().copied().max().unwrap_or(0);
    is_enabled(Some(peek)) && peek.built && chosen && kept < peek.chunk_texts
}

/// The policy as it is now, read and parsed once: whether it allows the
/// index (`allowed && local_index`), and the fingerprint a build must have
/// been made under to be served.
struct PolicyNow {
    allowed: Option<LocalWorkPolicy>,
    fingerprint: String,
}

impl PolicyNow {
    fn gate(self) -> Result<(LocalWorkPolicy, String), TurnError> {
        match self.allowed {
            Some(policy) => Ok((policy, self.fingerprint)),
            None => Err(disabled()),
        }
    }
}

/// Every workspace's index service, opened on first use.
pub struct IndexRegistry {
    workspaces: Arc<WorkspaceStore>,
    policy: Arc<dyn PolicySource>,
    events: Arc<dyn IndexEvents>,
    /// Locked only to find, add or drop an entry.
    entries: Mutex<HashMap<String, Arc<Entry>>>,
    /// The debounce of the refresh after a turn's changes; `None`: none.
    refresh_delay: Option<Duration>,
    /// What no index lists, reads or keeps, wherever the workspace sits:
    /// the credentials and the app's own data (the agent's deny list).
    deny: Option<Arc<DenyList>>,
    /// How folders are watched (`None`: they are not).
    watch: Option<WatchConfig>,
    /// The background embedder (`None`: no embeddings at all).
    embedding: Option<Arc<Embedding>>,
    /// Turns running now ([`TurnHold`]).
    turns: Arc<AtomicUsize>,
    /// How long a drifted index must be idle before its full rebuild
    /// (`None`: never rebuilt on its own).
    idle_rebuild: Option<Duration>,
    /// The registry itself, for the watchers' callbacks (set by
    /// [`Self::shared`]; a registry made otherwise watches nothing).
    me: Weak<Self>,
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
            refresh_delay: Some(DEFAULT_REFRESH_DELAY),
            deny: None,
            watch: Some(WatchConfig::default()),
            embedding: None,
            turns: Arc::new(AtomicUsize::new(0)),
            idle_rebuild: Some(IDLE_REBUILD),
            me: Weak::new(),
        }
    }

    /// Embed the chunks of every open, turned-on index whose `embeddings`
    /// setting names an embedder (`src/embeddings.rs`); woken after every
    /// build.
    #[must_use]
    pub fn with_embedding(mut self, embedding: Arc<Embedding>) -> Self {
        self.events = Arc::new(WakeAfterBuild {
            inner: self.events,
            embedding: Arc::downgrade(&embedding),
        });
        self.embedding = Some(embedding);
        self
    }

    /// Hold background embedding, and any idle rebuild, while the guard
    /// lives (a turn runs).
    #[must_use]
    pub fn hold_for_turn(&self) -> TurnHold {
        self.turns.fetch_add(1, Ordering::SeqCst);
        TurnHold {
            _embedding: self.embedding.as_ref().map(|embedding| embedding.turn()),
            turns: Arc::clone(&self.turns),
        }
    }

    /// The registry, shared: the only form that watches folders and
    /// rebuilds drifted indexes when idle (both reach the registry through
    /// it).
    #[must_use]
    pub fn shared(self) -> Arc<Self> {
        Arc::new_cyclic(|me| Self {
            me: me.clone(),
            events: Arc::new(RebuildWhenIdle {
                inner: self.events.clone(),
                registry: me.clone(),
            }),
            ..self
        })
    }

    /// How long a drifted index must be idle before its full rebuild
    /// (`None`: never).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_idle_rebuild(mut self, idle: Option<Duration>) -> Self {
        self.idle_rebuild = idle;
        self
    }

    /// A refresh of the workspace completed: if its index wants a full
    /// rebuild, one starts once it has been idle for the idle delay.
    fn after_ready(self: &Arc<Self>, workspace_id: &str) {
        let Some(entry) = self.existing(workspace_id) else {
            return;
        };
        let ready = entry.readies.fetch_add(1, Ordering::SeqCst) + 1;
        if let (Some(idle), Ok(runtime)) =
            (self.idle_rebuild, tokio::runtime::Handle::try_current())
        {
            let this = Arc::clone(self);
            let id = workspace_id.to_owned();
            runtime.spawn(async move { this.rebuild_when_idle(&id, &entry, ready, idle).await });
        }
    }

    /// The full rebuild after refresh `ready`, once nothing happened for
    /// `idle`: none when another refresh completed or a change came since
    /// (that one decides), and none while a turn runs (it waits again).
    async fn rebuild_when_idle(
        &self,
        workspace_id: &str,
        entry: &Arc<Entry>,
        ready: u64,
        idle: Duration,
    ) {
        let changes = entry.changes.load(Ordering::SeqCst);
        loop {
            tokio::time::sleep(idle).await;
            if entry.readies.load(Ordering::SeqCst) != ready
                || entry.changes.load(Ordering::SeqCst) != changes
            {
                return;
            }
            let Some(service) = entry.current() else {
                return;
            };
            if service.is_building() {
                return;
            }
            if self.turns.load(Ordering::SeqCst) > 0 {
                continue;
            }
            let wants = {
                let service = service.clone();
                blocking(move || service.wants_full_rebuild()).await
            };
            if !matches!(wants, Ok(true)) {
                return;
            }
            // Under the policy now, as the refresh after a change.
            let now = self.policy_now();
            let Ok(workspace) = self.workspace(workspace_id).await else {
                return;
            };
            let (current, mut slot) = self.lock(workspace_id).await;
            if !Arc::ptr_eq(&current, entry) || !slot.open.as_ref().is_some_and(|open| open.enabled)
            {
                return;
            }
            let Some(policy) = now.allowed else {
                if let Err(error) = Self::close_open(&current, &mut slot).await {
                    log::warn!("an index could not be closed: {}", error.code);
                }
                return;
            };
            let service = match self
                .open_in(&workspace, &policy, &current, &mut slot, false)
                .await
            {
                Ok(service) => service,
                Err(error) => {
                    log::warn!("an index could not be rebuilt: {}", error.code);
                    return;
                }
            };
            drop(slot);
            if let Err(error) = service.start_refresh(true) {
                log::warn!("an index could not start its idle rebuild: {}", error.code);
            }
            return;
        }
    }

    /// How folders are watched (`None`: not at all).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_watch(mut self, watch: Option<WatchConfig>) -> Self {
        self.watch = watch;
        self
    }

    /// Start watching the folder of `service` (turned on): its changes
    /// refresh the index the way a turn's do, without the debounce (the
    /// watch waited for the folder to be quiet). `None` when the registry
    /// does not watch, or the system refused (logged; the index still
    /// refreshes on open and after turns).
    async fn watch(
        &self,
        workspace_id: &str,
        service: &Arc<IndexService>,
    ) -> Option<FolderWatcher> {
        let config = self.watch?;
        self.me.upgrade()?;
        let runtime = tokio::runtime::Handle::try_current().ok()?;
        let me = self.me.clone();
        let id = workspace_id.to_owned();
        let workspace = service.workspace().clone();
        let on_change = Box::new(move |files: u64| {
            if let Some(registry) = me.upgrade() {
                let _context = runtime.enter();
                registry.changed(&id, files, Some(Duration::ZERO));
            }
        });
        match blocking(move || FolderWatcher::start(workspace, config, on_change)).await {
            Ok(Ok(watcher)) => {
                service.set_watching(true);
                Some(watcher)
            }
            Ok(Err(error)) => {
                log::warn!("a workspace folder could not be watched: {error}");
                None
            }
            Err(_) => None,
        }
    }

    /// Leave out what `deny` covers from every index: never listed, read
    /// or kept (the agent's deny list: credentials, the app's own data).
    #[must_use]
    pub fn with_deny_list(mut self, deny: Arc<DenyList>) -> Self {
        self.deny = Some(deny);
        self
    }

    /// The debounce of the refresh that follows a turn's changes (`None`:
    /// no automatic refresh).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_refresh_delay(mut self, delay: Option<Duration>) -> Self {
        self.refresh_delay = delay;
        self
    }

    /// The policy now, parsed once.
    fn policy_now(&self) -> PolicyNow {
        let policy: LocalWorkPolicy = self
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        let fingerprint = policy_fingerprint(&policy.path_deny);
        PolicyNow {
            allowed: (policy.allowed && policy.local_index).then_some(policy),
            fingerprint,
        }
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

    fn existing(&self, workspace_id: &str) -> Option<Arc<Entry>> {
        self.entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .get(workspace_id)
            .cloned()
    }

    /// The workspace's entry and its slot, held. An entry dropped while
    /// this waited for it is looked up again.
    async fn lock(&self, workspace_id: &str) -> (Arc<Entry>, SlotGuard) {
        loop {
            let entry = self.entry(workspace_id);
            let slot = entry.slot.clone().lock_owned().await;
            if !slot.removed {
                return (entry, slot);
            }
        }
    }

    /// [`Self::lock`] from a blocking thread (outside any async context).
    fn lock_blocking(&self, workspace_id: &str) -> (Arc<Entry>, SlotGuard) {
        loop {
            let entry = self.entry(workspace_id);
            let slot = entry.slot.clone().blocking_lock_owned();
            if !slot.removed {
                return (entry, slot);
            }
        }
    }

    /// Drop the workspace's entry: its index is gone. Waiters on the slot
    /// see `removed` and look it up again.
    fn forget(&self, workspace_id: &str, entry: &Arc<Entry>, slot: &mut Slot) {
        slot.removed = true;
        let mut entries = self.entries.lock().unwrap_or_else(PoisonError::into_inner);
        if entries
            .get(workspace_id)
            .is_some_and(|known| Arc::ptr_eq(known, entry))
        {
            entries.remove(workspace_id);
        }
    }

    /// Take the open service out of the slot (it is closed by the caller).
    fn take_open(entry: &Entry, slot: &mut Slot) -> Option<Open> {
        entry.set_current(None);
        slot.open.take()
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

    /// Close the open service (if any): the policy turned the index off.
    async fn close_open(entry: &Entry, slot: &mut Slot) -> Result<(), TurnError> {
        if let Some(open) = Self::take_open(entry, slot) {
            blocking(move || open.service.close()).await?;
        }
        Ok(())
    }

    /// The workspace's service in its held slot, opened (and checked for
    /// what changed while it was closed: a refresh in the background, with
    /// `catch_up`) when it is not open, or open under another policy: a
    /// service is only ever refreshed with the `Workspace` of the policy
    /// now.
    async fn open_in(
        &self,
        workspace: &Workspace,
        policy: &LocalWorkPolicy,
        entry: &Entry,
        slot: &mut Slot,
        catch_up: bool,
    ) -> Result<Arc<IndexService>, TurnError> {
        let fingerprint = policy_fingerprint(&policy.path_deny);
        if let Some(open) = &slot.open
            && open.service.policy() == fingerprint
            && !open.service.is_closed()
        {
            return Ok(open.service.clone());
        }
        // Another policy (or closed): this one is closed — the tools a turn
        // holds answer that it is gone — and the index reopened under the
        // policy now.
        let enabled = match Self::take_open(entry, slot) {
            Some(open) => {
                let enabled = open.enabled;
                blocking(move || open.service.close()).await?;
                enabled
            }
            None => is_enabled(self.peek(&workspace.id, slot).await?.as_ref()),
        };
        let (id, root, deny, dir, events) = (
            workspace.id.clone(),
            PathBuf::from(&workspace.path),
            policy.path_deny.clone(),
            self.index_dir(&workspace.id),
            self.events.clone(),
        );
        let host_deny = self.deny.clone();
        let service = blocking(move || {
            IndexService::open_with_deny(&id, &root, &deny, host_deny, &dir, events)
        })
        .await??;
        slot.peeked = None;
        if enabled && let Some(embedding) = &self.embedding {
            embedding.add(&service);
        }
        let watcher = if enabled {
            self.watch(&workspace.id, &service).await
        } else {
            None
        };
        slot.open = Some(Open {
            service: service.clone(),
            enabled,
            watcher,
        });
        entry.set_current(Some(service.clone()));
        if catch_up
            && enabled
            && let Err(error) = service.start_refresh(false)
        {
            log::warn!("an index could not start its first refresh: {}", error.code);
        }
        Ok(service)
    }

    /// `index_status`. Never opens an index that is not open; an open one
    /// is checked against the policy now: turned off by it, it is closed
    /// (`off`, `policy_off`); made under another `path_deny`, it is reopened
    /// under the new one (`stale_policy`, rebuilding). With the policy off
    /// it answers `off` with `policy_off` and whether an index is on disk
    /// (turning it off and removing it stay allowed).
    ///
    /// # Errors
    ///
    /// `workspace_unknown`, or the index refused.
    pub async fn status(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let now = self.policy_now();
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        let Some(policy) = now.allowed else {
            Self::close_open(&entry, &mut slot).await?;
            slot.peeked = None;
            let file = self.index_dir(workspace_id).join(FILE_NAME);
            let on_disk = blocking(move || std::fs::symlink_metadata(file).is_ok()).await?;
            return Ok(IndexStatus {
                policy_off: true,
                on_disk,
                ..IndexStatus::off()
            });
        };
        if let Some(open) = &slot.open {
            if open.enabled && open.service.policy() != now.fingerprint {
                self.open_in(&workspace, &policy, &entry, &mut slot, true)
                    .await?;
            }
            return Ok(slot
                .open
                .as_ref()
                .map_or_else(IndexStatus::off, Open::status));
        }
        let peek = self.peek(workspace_id, &mut slot).await?;
        Ok(peeked_status(peek.as_ref(), &now.fingerprint))
    }

    /// `index_open`: open the workspace's index (its session page is open)
    /// and check the folder in the background; `off` when not turned on.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `workspace_unknown`, or the index refused.
    pub async fn open(&self, workspace_id: &str) -> Result<IndexStatus, TurnError> {
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Ok(peeked_status(slot.peeked.clone().flatten().as_ref(), ""));
        }
        self.open_in(&workspace, &policy, &entry, &mut slot, true)
            .await?;
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
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        let writer = service.clone();
        blocking(move || writer.store().set_setting(ENABLED, "true"))
            .await?
            .map_err(IndexError::from)?;
        let watcher = match slot.open.as_ref() {
            Some(open) if open.watcher.is_none() => self.watch(workspace_id, &service).await,
            _ => None,
        };
        if let Some(embedding) = &self.embedding {
            embedding.add(&service);
        }
        if let Some(open) = slot.open.as_mut() {
            open.enabled = true;
            if watcher.is_some() {
                open.watcher = watcher;
            }
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
        let (entry, mut slot) = self.lock(workspace_id).await;
        let open = Self::take_open(&entry, &mut slot);
        slot.peeked = None;
        // Its embedding status, Embed now and retry wait go with it.
        self.forget_embedding(workspace_id);
        let dir = self.index_dir(workspace_id);
        let on_disk = blocking(move || {
            if let Some(open) = open {
                open.service.cancel();
                let saved = open.service.store().set_setting(ENABLED, "false");
                open.service.close();
                saved.map(|()| true)
            } else if std::fs::symlink_metadata(dir.join(FILE_NAME)).is_ok() {
                let store = SqliteGraphStore::open(&dir)?;
                let saved = store.set_setting(ENABLED, "false");
                store.close();
                saved.map(|()| true)
            } else {
                Ok(false)
            }
        })
        .await?
        .map_err(IndexError::from)?;
        Ok(IndexStatus {
            on_disk,
            policy_off: self.policy_now().allowed.is_none(),
            ..IndexStatus::off()
        })
    }

    /// `index_refresh` (`full`: rebuild from nothing), under the policy now.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off` (not turned on), `index_busy`.
    pub async fn refresh(&self, workspace_id: &str, full: bool) -> Result<IndexStatus, TurnError> {
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        match service.start_refresh(full) {
            // An incremental refresh asked for while one runs: that one is it.
            Err(error) if error.code == "index_busy" && !full => {}
            other => other?,
        }
        Ok(service.status())
    }

    /// Set the workspace's embeddings (`off`, `local:<model>`,
    /// `remote:<dim>:<model>`): what the background embedder makes and the
    /// dense side of searches use. Off by default; a choice the policy does
    /// not allow now is refused (`embeddings_not_allowed`), an unknown one
    /// too (`embeddings_unknown`). Returns the value stored.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off`, `workspace_unknown`, the two
    /// above, or the index could not be written.
    pub async fn set_embeddings(
        &self,
        workspace_id: &str,
        value: &str,
    ) -> Result<String, TurnError> {
        let (policy, _) = self.policy_now().gate()?;
        let choice = Choice::parse(value);
        match &choice {
            Choice::Unknown => {
                return Err(TurnError::new(
                    "embeddings_unknown",
                    "That is not an embedder this app knows.",
                ));
            }
            Choice::Off => {}
            other if !Allowed::of(self.policy.as_ref()).allows(other) => {
                return Err(TurnError::new(
                    "embeddings_not_allowed",
                    "Your organization's policy does not allow these embeddings.",
                ));
            }
            _ => {}
        }
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        drop(slot);
        let stored = choice.value();
        let (writer, saved) = (service.clone(), stored.clone());
        blocking(move || writer.store().set_setting(SETTING, &saved))
            .await?
            .map_err(IndexError::from)?;
        if let Some(embedding) = &self.embedding {
            embedding.add(&service);
            // The person chose: a spent budget or a run cap is asked again.
            embedding.ask_again(workspace_id);
        }
        Ok(stored)
    }

    /// The workspace's embedding progress.
    #[must_use]
    pub fn embedding_status(&self, workspace_id: &str) -> Option<EmbedStatus> {
        self.embedding.as_ref()?.status(workspace_id)
    }

    /// Embed the workspace now whatever the power state (`full`: its
    /// vectors are dropped first and every chunk embedded again).
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off`, `embeddings_off` (the setting
    /// names no embedder), or the index could not be written.
    pub async fn rebuild_embeddings(
        &self,
        workspace_id: &str,
        full: bool,
    ) -> Result<(), TurnError> {
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        drop(slot);
        let Some(embedding) = &self.embedding else {
            return Err(TurnError::new(
                "embeddings_off",
                "Embeddings are not available.",
            ));
        };
        let reader = service.clone();
        if blocking(move || choice_of(&reader)).await? == OFF {
            return Err(TurnError::new(
                "embeddings_off",
                "Choose how to embed this workspace first.",
            ));
        }
        if full {
            let writer = service.clone();
            blocking(move || writer.store().prune_vectors(None))
                .await?
                .map_err(IndexError::from)?;
        }
        embedding.add(&service);
        embedding.force(workspace_id);
        Ok(())
    }

    /// What embedding the workspace would take: the distinct chunk texts
    /// that have no vector in `stamp` (`None`: every text, for an embedder
    /// not chosen yet), their estimated tokens, the caps and today's use.
    ///
    /// # Errors
    ///
    /// `local_index_disabled`, `index_off`, or the index could not be read.
    pub async fn embedding_basis(
        &self,
        workspace_id: &str,
        stamp: Option<String>,
    ) -> Result<EmbeddingBasis, TurnError> {
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        drop(slot);
        blocking(move || {
            let store = service.store();
            let (texts, tokens) = store.missing_text_stats(stamp.as_deref())?;
            Ok::<_, StoreError>(EmbeddingBasis {
                texts,
                tokens,
                caps: EmbedCaps::load(store),
                day_used: store.embed_tokens_today()?,
            })
        })
        .await?
        .map_err(|error| TurnError::from(IndexError::from(error)))
    }

    /// Set the workspace's embedding caps (estimated tokens per run and
    /// per UTC day, remote embedders only); a run stopped by a cap looks
    /// again at once.
    ///
    /// # Errors
    ///
    /// `embed_caps_invalid` (a cap is zero or absurd), `local_index_disabled`,
    /// `index_off`, or the index could not be written.
    pub async fn set_embedding_caps(
        &self,
        workspace_id: &str,
        caps: EmbedCaps,
    ) -> Result<EmbedCaps, TurnError> {
        if !caps.valid() {
            return Err(TurnError::new(
                "embed_caps_invalid",
                "A cap must be between 1 and 10 000 000 000 tokens.",
            ));
        }
        let (policy, _) = self.policy_now().gate()?;
        let workspace = self.workspace(workspace_id).await?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        if !self.enabled(workspace_id, &mut slot).await? {
            return Err(TurnError::new(
                "index_off",
                "Turn the index on for this workspace first.",
            ));
        }
        let service = self
            .open_in(&workspace, &policy, &entry, &mut slot, false)
            .await?;
        drop(slot);
        blocking(move || caps.save(service.store()))
            .await?
            .map_err(|message| TurnError::new("embed_caps_invalid", message))?;
        if let Some(embedding) = &self.embedding {
            embedding.caps_changed(workspace_id);
        }
        Ok(caps)
    }

    /// The workspace's embeddings setting (`off` when never set), from the
    /// open service or a small read of the index file.
    pub async fn embeddings(&self, workspace_id: &str) -> String {
        if let Some(service) = self
            .existing(workspace_id)
            .and_then(|entry| entry.current())
        {
            return blocking(move || choice_of(&service))
                .await
                .unwrap_or_else(|_| OFF.to_owned());
        }
        let (_, mut slot) = self.lock(workspace_id).await;
        match self.peek(workspace_id, &mut slot).await {
            Ok(Some(peek)) => peek
                .settings
                .get(SETTING)
                .filter(|choice| !choice.is_empty())
                .cloned()
                .unwrap_or_else(|| OFF.to_owned()),
            _ => OFF.to_owned(),
        }
    }

    /// Embed `paths` of the workspace first (most recent first): the files a
    /// turn changed, the files a thread references. A hint only.
    pub fn prioritise(&self, workspace_id: &str, paths: impl IntoIterator<Item = String>) {
        if let Some(embedding) = &self.embedding {
            embedding.prioritise(workspace_id, paths);
        }
    }

    /// At start-up: reopen every index whose embedding pass the app quit in
    /// the middle of, so the pass resumes (embedding only the chunks still
    /// missing a vector). Nothing when the policy turns indexes off.
    /// Answers how many resumed.
    pub async fn resume_embeddings(&self) -> usize {
        if self.embedding.is_none() {
            return 0;
        }
        let Ok((policy, _)) = self.policy_now().gate() else {
            return 0;
        };
        let workspaces = self.workspaces.clone();
        let Ok(Ok(listed)) = blocking(move || workspaces.list()).await else {
            return 0;
        };
        let mut resumed = 0;
        for workspace in listed {
            let (entry, mut slot) = self.lock(&workspace.id).await;
            if slot.open.is_some() {
                continue;
            }
            let unfinished = match self.peek(&workspace.id, &mut slot).await {
                Ok(peek) => peek.as_ref().is_some_and(embedding_unfinished),
                Err(_) => false,
            };
            if !unfinished {
                continue;
            }
            match self
                .open_in(&workspace, &policy, &entry, &mut slot, true)
                .await
            {
                Ok(_) => resumed += 1,
                Err(error) => log::warn!("an index could not resume embedding: {}", error.code),
            }
        }
        resumed
    }

    /// Whether the workspace's folder is watched now (the Doctor's
    /// staleness check). Never waits for the workspace's slot.
    #[must_use]
    pub fn is_watching(&self, workspace_id: &str) -> bool {
        self.existing(workspace_id)
            .and_then(|entry| entry.current())
            .is_some_and(|service| service.status().watching)
    }

    /// Every entry, for a pass over all workspaces.
    fn all_entries(&self) -> Vec<(String, Arc<Entry>)> {
        self.entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .iter()
            .map(|(id, entry)| (id.clone(), Arc::clone(entry)))
            .collect()
    }

    /// Sign-out: stop every folder watch. The indexes themselves stay (they
    /// are kept across sign-out, above); with the policy gone, the next
    /// entry point closes them. [`Self::resume_watching`] starts the
    /// watches again at sign-in.
    pub async fn stop_watching(&self) {
        let mut stopped = Vec::new();
        for (_, entry) in self.all_entries() {
            let mut slot = entry.slot.clone().lock_owned().await;
            if let Some(open) = slot.open.as_mut()
                && let Some(watcher) = open.watcher.take()
            {
                open.service.set_watching(false);
                stopped.push(watcher);
            }
        }
        // Each joins its thread, which notices the stop within a tick.
        if !stopped.is_empty() {
            let _ = blocking(move || drop(stopped)).await;
        }
    }

    /// Sign-in: watch the folder of every index still open and turned on
    /// again, when the policy now allows the index and the index was opened
    /// under it (one opened under another `path_deny` is reopened, with its
    /// watch, by the next entry point; a closed one when it opens).
    pub async fn resume_watching(&self) {
        let now = self.policy_now();
        if now.allowed.is_none() {
            return;
        }
        for (id, entry) in self.all_entries() {
            let mut slot = entry.slot.clone().lock_owned().await;
            let Some(open) = slot.open.as_ref() else {
                continue;
            };
            if !open.enabled
                || open.watcher.is_some()
                || open.service.is_closed()
                || open.service.policy() != now.fingerprint
            {
                continue;
            }
            let service = open.service.clone();
            let watcher = self.watch(&id, &service).await;
            if let Some(open) = slot.open.as_mut() {
                open.watcher = watcher;
            }
        }
    }

    /// `index_cancel`: `true` when a refresh was running. Never waits for
    /// the workspace's slot (an open in progress has nothing to cancel).
    #[must_use]
    pub fn cancel(&self, workspace_id: &str) -> bool {
        self.existing(workspace_id)
            .and_then(|entry| entry.current())
            .is_some_and(|service| service.cancel())
    }

    /// Close the workspace's index (if open) and run `work` — the removal
    /// of its files — holding its slot throughout, on the blocking pool: no
    /// status read or turn opens it again before `work` is done. The
    /// workspace's entry is dropped afterwards (its index is gone).
    pub async fn with_closed<T: Send + 'static>(
        &self,
        workspace_id: &str,
        work: impl FnOnce() -> T + Send + 'static,
    ) -> Result<T, TurnError> {
        let (entry, mut slot) = self.lock(workspace_id).await;
        let open = Self::take_open(&entry, &mut slot);
        slot.peeked = None;
        let done = blocking(move || {
            if let Some(open) = open {
                open.service.close();
            }
            work()
        })
        .await;
        self.forget(workspace_id, &entry, &mut slot);
        drop(slot);
        done
    }

    /// [`Self::with_closed`] for a caller on a blocking thread outside any
    /// async context (the Doctor's repairs), running `work` on that thread.
    pub fn with_closed_blocking<T>(&self, workspace_id: &str, work: impl FnOnce() -> T) -> T {
        let (entry, mut slot) = self.lock_blocking(workspace_id);
        if let Some(open) = Self::take_open(&entry, &mut slot) {
            open.service.close();
        }
        slot.peeked = None;
        let done = work();
        self.forget(workspace_id, &entry, &mut slot);
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
        self.forget_embedding(workspace_id);
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

    /// Drop what the embedder keeps for the workspace (its service, status,
    /// forced flag, retry wait and priorities).
    fn forget_embedding(&self, workspace_id: &str) {
        if let Some(embedding) = &self.embedding {
            embedding.remove(workspace_id);
        }
    }

    /// The service whose tools a turn in the workspace is offered: when the
    /// policy allows the index, it is turned on, and a build made under the
    /// current policy exists (a stale or rebuilding one too; its answers say
    /// so). Opens the index on first use, under the policy now.
    pub async fn for_turn(&self, workspace_id: &str) -> Option<Arc<IndexService>> {
        let now = self.policy_now();
        let workspace = self.workspace(workspace_id).await.ok()?;
        let (entry, mut slot) = self.lock(workspace_id).await;
        let Some(policy) = now.allowed else {
            if let Err(error) = Self::close_open(&entry, &mut slot).await {
                log::warn!("an index could not be closed: {}", error.code);
            }
            return None;
        };
        match self.enabled(workspace_id, &mut slot).await {
            Ok(true) => {}
            Ok(false) => return None,
            Err(error) => {
                log::warn!("a turn runs without its index: {}", error.code);
                return None;
            }
        }
        match self
            .open_in(&workspace, &policy, &entry, &mut slot, true)
            .await
        {
            Ok(service) => service.view().is_some().then_some(service),
            Err(error) => {
                log::warn!("a turn runs without its index: {}", error.code);
                None
            }
        }
    }

    /// A turn changed `files` files of the workspace. Never waits: the open
    /// index (if any) is marked stale at once, and a refresh follows once
    /// the changes stop for the refresh delay — under the policy then
    /// (closed if the policy turned the index off, reopened if `path_deny`
    /// changed).
    pub fn mark_changed(self: &Arc<Self>, workspace_id: &str, files: usize) {
        self.changed(
            workspace_id,
            u64::try_from(files).unwrap_or(u64::MAX),
            self.refresh_delay,
        );
    }

    /// `files` changed in the workspace's folder (a turn's writes, or the
    /// watch): mark the open service and refresh after `delay`.
    fn changed(self: &Arc<Self>, workspace_id: &str, files: u64, delay: Option<Duration>) {
        if files == 0 {
            return;
        }
        let Some(entry) = self.existing(workspace_id) else {
            return;
        };
        if let Some(service) = entry.current() {
            service.mark_changed(files);
        }
        let change = entry.changes.fetch_add(1, Ordering::SeqCst) + 1;
        if let (Some(delay), Ok(runtime)) = (delay, tokio::runtime::Handle::try_current()) {
            let this = Arc::clone(self);
            let id = workspace_id.to_owned();
            runtime
                .spawn(async move { this.refresh_after_change(&id, &entry, change, delay).await });
        }
    }

    /// The debounced refresh of change `change`, unless a later one came.
    async fn refresh_after_change(
        &self,
        workspace_id: &str,
        entry: &Arc<Entry>,
        change: u64,
        delay: Duration,
    ) {
        tokio::time::sleep(delay).await;
        loop {
            if entry.changes.load(Ordering::SeqCst) != change {
                return;
            }
            let now = self.policy_now();
            let Ok(workspace) = self.workspace(workspace_id).await else {
                return;
            };
            let (current, mut slot) = self.lock(workspace_id).await;
            if !Arc::ptr_eq(&current, entry) || !slot.open.as_ref().is_some_and(|open| open.enabled)
            {
                return;
            }
            let Some(policy) = now.allowed else {
                // Turned off by the policy since: closed, not refreshed.
                if let Err(error) = Self::close_open(&current, &mut slot).await {
                    log::warn!("an index could not be closed: {}", error.code);
                }
                return;
            };
            let service = match self
                .open_in(&workspace, &policy, &current, &mut slot, false)
                .await
            {
                Ok(service) => service,
                Err(error) => {
                    log::warn!("an index could not refresh after a turn: {}", error.code);
                    return;
                }
            };
            drop(slot);
            match service.start_refresh(false) {
                Err(error) if error.code == "index_busy" => {
                    tokio::time::sleep(delay.max(BUSY_RETRY)).await;
                }
                _ => return,
            }
        }
    }

    /// Whether the workspace's index is open (tests).
    #[cfg(test)]
    async fn is_open(&self, workspace_id: &str) -> bool {
        self.lock(workspace_id).await.1.open.is_some()
    }

    /// How many workspaces the registry keeps an entry for (tests).
    #[cfg(test)]
    fn entry_count(&self) -> usize {
        self.entries
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .len()
    }
}

/// Index events, and a wake-up for the embedder once a build is ready.
struct WakeAfterBuild {
    inner: Arc<dyn IndexEvents>,
    embedding: Weak<Embedding>,
}

impl IndexEvents for WakeAfterBuild {
    fn emit(&self, event: IndexEvent) {
        let ready = (event.phase == "ready").then(|| event.workspace_id.clone());
        self.inner.emit(event);
        if let Some(workspace_id) = ready
            && let Some(embedding) = self.embedding.upgrade()
        {
            embedding.wake_workspace(&workspace_id);
        }
    }
}

/// Index events, and the idle rebuild check after every completed refresh.
struct RebuildWhenIdle {
    inner: Arc<dyn IndexEvents>,
    registry: Weak<IndexRegistry>,
}

impl IndexEvents for RebuildWhenIdle {
    fn emit(&self, event: IndexEvent) {
        let ready = (event.phase == "ready").then(|| event.workspace_id.clone());
        self.inner.emit(event);
        if let Some(workspace_id) = ready
            && let Some(registry) = self.registry.upgrade()
        {
            registry.after_ready(&workspace_id);
        }
    }
}

/// Index events to the main window only.
pub struct MainWindowIndexEvents(pub tauri::AppHandle);

impl IndexEvents for MainWindowIndexEvents {
    fn emit(&self, event: IndexEvent) {
        use tauri::{Emitter as _, Manager as _};
        if let Err(error) = self.0.emit_to("main", EVENT_NAME, &event) {
            log::warn!("could not deliver an index event: {error}");
        }
        if let Some(activity) = self
            .0
            .try_state::<std::sync::Arc<crate::activity::Activity>>()
        {
            activity.index(
                &event.workspace_id,
                event.status.state == IndexState::Building,
                event.message.as_deref(),
            );
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

/// The workspace's embeddings setting (IPC.md, "Index embeddings").
#[tauri::command(rename_all = "snake_case")]
pub async fn index_embeddings_get(
    state: State<'_, LocalState>,
    workspace_id: String,
) -> Result<String, IpcError> {
    Ok(state.index.embeddings(&workspace_id).await)
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
    Ok(state.index.cancel(&workspace_id))
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
    use std::time::Instant;

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
        _folder: tempfile::TempDir,
    }

    fn registry_with(policy: Value, delay: Option<Duration>) -> Fixture {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(policy))));
        let registry = Arc::new(
            IndexRegistry::new(workspaces.clone(), policy.clone(), Arc::new(NoEvents))
                .with_refresh_delay(delay),
        );
        Fixture {
            registry,
            id,
            policy,
            workspaces,
            _app: app,
            _folder: folder,
        }
    }

    fn registry(policy: Value) -> Fixture {
        registry_with(policy, None)
    }

    fn on() -> Value {
        json!({"allowed": true, "local_index": true})
    }

    async fn until(
        registry: &IndexRegistry,
        id: &str,
        done: impl Fn(&IndexStatus) -> bool,
    ) -> IndexStatus {
        let mut status = registry.status(id).await.unwrap();
        for _ in 0..500 {
            if done(&status) {
                return status;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
            status = registry.status(id).await.unwrap();
        }
        panic!("never reached: {status:?}");
    }

    async fn until_built(registry: &IndexRegistry, id: &str) -> IndexStatus {
        until(registry, id, |status| status.state == IndexState::Ready).await
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
            let status = registry.status(id).await.unwrap();
            assert_eq!(
                (status.state, status.policy_off, status.on_disk),
                (IndexState::Off, true, false)
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
        assert!(built.on_disk && !built.policy_off);
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
        assert_eq!(registry.entry_count(), 0, "a removed index leaves no entry");
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
    /// recreated. Other workspaces are not held up meanwhile, and the
    /// removed workspace's entry is dropped.
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
    }

    /// The policy is checked against the open service at every entry point:
    /// a changed `path_deny` reopens it under the new one (the old build is
    /// never offered), and the policy turning the index off closes it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_open_service_follows_the_policy() {
        let f = registry(on());
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let held = f.registry.for_turn(&f.id).await.unwrap();
        *f.policy.0.lock().unwrap() =
            Some(json!({"allowed": true, "local_index": true, "path_deny": ["a.py"]}));
        // A status read finds the open service under the old policy: it is
        // closed and the index reopened (and rebuilt) under the new one.
        let status = f.registry.status(&f.id).await.unwrap();
        assert!(
            held.is_closed(),
            "the old Workspace is never refreshed again"
        );
        assert!(
            matches!(status.state, IndexState::StalePolicy | IndexState::Building),
            "{status:?}"
        );
        let rebuilt = until_built(&f.registry, &f.id).await;
        assert_eq!(rebuilt.entities, 0, "a.py is denied now: {rebuilt:?}");
        let reopened = f.registry.for_turn(&f.id).await.unwrap();
        assert_eq!(reopened.workspace().root(), held.workspace().root());

        // The policy turns the index off: the open service is closed.
        *f.policy.0.lock().unwrap() = Some(json!({"allowed": true, "local_index": false}));
        let off = f.registry.status(&f.id).await.unwrap();
        assert_eq!(
            (off.state, off.policy_off, off.on_disk),
            (IndexState::Off, true, true)
        );
        assert!(reopened.is_closed());
        assert!(!f.registry.is_open(&f.id).await);
    }

    /// A turn's changes refresh the index under the policy at the time of
    /// the refresh: here `path_deny` changed meanwhile, so the refresh runs
    /// on the index reopened under the new policy, never on the old
    /// service's `Workspace`.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_refresh_after_a_change_runs_under_the_policy_then() {
        let f = registry_with(on(), Some(Duration::from_millis(50)));
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let held = f.registry.for_turn(&f.id).await.unwrap();
        *f.policy.0.lock().unwrap() =
            Some(json!({"allowed": true, "local_index": true, "path_deny": ["a.py"]}));
        f.registry.mark_changed(&f.id, 1);
        assert_eq!(held.status().state, IndexState::Stale, "marked at once");
        // Watched without a status read (which would check the policy too).
        let mut rebuilt = None;
        for _ in 0..500 {
            if let Some(service) = f.registry.existing(&f.id).and_then(|entry| entry.current())
                && !Arc::ptr_eq(&service, &held)
                && service.status().state == IndexState::Ready
            {
                rebuilt = Some(service);
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let rebuilt = rebuilt.expect("the refresh ran on the index reopened under the new policy");
        assert_eq!(rebuilt.status().entities, 0, "a.py is denied now");
        assert!(held.is_closed());
    }

    /// A turn's changes schedule one refresh, a little after they stop.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn changes_schedule_one_debounced_refresh() {
        let f = registry_with(on(), Some(Duration::from_millis(100)));
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let service = f.registry.for_turn(&f.id).await.unwrap();
        std::fs::write(f._folder.path().join("b.py"), "def b():\n    pass\n").unwrap();
        for _ in 0..3 {
            f.registry.mark_changed(&f.id, 1);
        }
        assert_eq!(service.status().changed_files, 3);
        let status = until(&f.registry, &f.id, |status| {
            status.state == IndexState::Ready && status.changed_files == 0
        })
        .await;
        assert_eq!(
            service.last_report().unwrap().read,
            1,
            "incremental: {status:?}"
        );
        let runs = service
            .store()
            .runs(elitea_local_index::sqlite_store::GraphKey::LOCAL)
            .unwrap()
            .len();
        tokio::time::sleep(Duration::from_millis(300)).await;
        let after = service
            .store()
            .runs(elitea_local_index::sqlite_store::GraphKey::LOCAL)
            .unwrap()
            .len();
        assert_eq!(after, runs, "one refresh for three changes");
    }

    /// Neither `index_cancel` nor a turn's change signal waits for the
    /// workspace's slot (held here as a slow open would hold it).
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn cancel_and_the_change_signal_never_wait_for_the_slot() {
        let f = registry_with(on(), Some(Duration::from_millis(50)));
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let (release, wait) = std::sync::mpsc::channel::<()>();
        let holder = {
            let registry = f.registry.clone();
            let id = f.id.clone();
            tokio::spawn(async move {
                registry
                    .with_closed(&id, move || {
                        let _ = wait.recv_timeout(Duration::from_secs(10));
                    })
                    .await
            })
        };
        tokio::time::sleep(Duration::from_millis(100)).await;
        let started = Instant::now();
        let _ = f.registry.cancel(&f.id);
        f.registry.mark_changed(&f.id, 1);
        let took = started.elapsed();
        release.send(()).unwrap();
        holder.await.unwrap().unwrap();
        assert!(took < Duration::from_millis(500), "waited {took:?}");
    }

    /// An index drifted past the threshold is rebuilt from nothing once it
    /// has been idle; not while a turn runs (#1219).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_drifted_index_is_rebuilt_when_idle() {
        use elitea_local_index::service::DRIFT_REBUILD;
        use elitea_local_index::sqlite_store::GraphKey;
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(on()))));
        let idle = Duration::from_millis(300);
        let registry = IndexRegistry::new(workspaces, policy, Arc::new(NoEvents))
            .with_refresh_delay(None)
            .with_watch(None)
            .with_idle_rebuild(Some(idle))
            .shared();
        registry.enable(&id).await.unwrap();
        until_built(&registry, &id).await;
        let service = registry.for_turn(&id).await.unwrap();
        service.store().set_drift_files(DRIFT_REBUILD + 1).unwrap();
        let held = registry.hold_for_turn();
        registry.refresh(&id, false).await.unwrap();
        until_built(&registry, &id).await;
        // A turn runs: no rebuild however long it lasts.
        tokio::time::sleep(idle * 3).await;
        assert!(service.wants_full_rebuild(), "rebuilt during a turn");
        let runs = service.store().runs(GraphKey::LOCAL).unwrap().len();
        drop(held);
        for _ in 0..500 {
            if !service.wants_full_rebuild() && !service.is_building() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(!service.wants_full_rebuild(), "rebuilt once idle");
        assert_eq!(service.store().drift_files().unwrap(), 0);
        assert_eq!(
            service.store().runs(GraphKey::LOCAL).unwrap().len(),
            runs + 1
        );
        assert_eq!(
            service.last_report().unwrap().read,
            1,
            "every file read again"
        );
    }

    /// A change made outside the app (no turn, no command) refreshes a
    /// turned-on index once the folder is quiet; turned off, the folder is
    /// no longer watched.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_folder_watch_refreshes_a_turned_on_index() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(on()))));
        let registry = IndexRegistry::new(workspaces, policy, Arc::new(NoEvents))
            .with_refresh_delay(None)
            .with_watch(Some(WatchConfig {
                quiet: Duration::from_millis(200),
                min_interval: Duration::from_millis(500),
            }))
            .shared();
        registry.enable(&id).await.unwrap();
        let built = until_built(&registry, &id).await;
        std::fs::write(folder.path().join("b.py"), "class B:\n    pass\n").unwrap();
        let status = until(&registry, &id, |status| {
            status.state == IndexState::Ready && status.files > built.files
        })
        .await;
        assert_eq!(status.files, built.files + 1);
        registry.disable(&id).await.unwrap();
        std::fs::write(folder.path().join("c.py"), "class C:\n    pass\n").unwrap();
        tokio::time::sleep(Duration::from_millis(1000)).await;
        let dir = registry.index_dir(&id);
        let peek = SqliteGraphStore::peek(&dir).unwrap().unwrap();
        assert_eq!(peek.documents, status.files, "not refreshed while off");
    }

    /// The watch runs while the index is open and turned on, and the status
    /// says so: it stops on disable, sign-out and the policy turning the
    /// index off, and starts again on enable, sign-in and reopening
    /// (#1219).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn watcher_stops_on_disable_and_policy_off() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(on()))));
        let registry = IndexRegistry::new(workspaces, policy.clone(), Arc::new(NoEvents))
            .with_refresh_delay(None)
            .with_watch(Some(WatchConfig {
                quiet: Duration::from_millis(100),
                min_interval: Duration::from_millis(200),
            }))
            .shared();
        registry.enable(&id).await.unwrap();
        assert!(
            until_built(&registry, &id).await.watching,
            "watched once on"
        );
        assert!(registry.is_watching(&id));

        let off = registry.disable(&id).await.unwrap();
        assert!(!off.watching && !registry.is_watching(&id), "{off:?}");
        assert!(registry.enable(&id).await.unwrap().watching, "on again");

        // Sign-out stops the watch, keeping the index open; sign-in
        // starts it again.
        registry.stop_watching().await;
        assert!(!registry.status(&id).await.unwrap().watching);
        assert!(!registry.is_watching(&id));
        registry.resume_watching().await;
        assert!(registry.status(&id).await.unwrap().watching);

        // The policy turns the index off: closed, not watched, and a change
        // refreshes nothing.
        let built = until_built(&registry, &id).await;
        *policy.0.lock().unwrap() = None;
        let status = registry.status(&id).await.unwrap();
        assert!(status.policy_off && !status.watching, "{status:?}");
        assert!(!registry.is_watching(&id));
        std::fs::write(folder.path().join("b.py"), "class B:\n    pass\n").unwrap();
        tokio::time::sleep(Duration::from_millis(600)).await;
        let peek = SqliteGraphStore::peek(&registry.index_dir(&id))
            .unwrap()
            .unwrap();
        assert_eq!(peek.documents, built.files, "not refreshed while off");
        // Sign-in under no policy starts nothing.
        registry.resume_watching().await;
        assert!(!registry.is_watching(&id));

        // Allowed again: reopening watches it.
        *policy.0.lock().unwrap() = Some(on());
        assert!(registry.open(&id).await.unwrap().watching);
    }

    /// A turned-on index whose setting names an embedder is embedded
    /// after its build, and a turn holds the embedder.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_built_index_is_embedded_when_its_setting_says_so() {
        use elitea_local_embed::{EmbedError, Embedder};
        use elitea_local_index::embedding::{
            AlwaysRun, EmbedConfig, EmbedState, EmbedderSource, SETTING,
        };
        use elitea_local_index::sqlite_store::VectorSet;

        struct Fake;
        impl Embedder for Fake {
            fn stamp(&self) -> String {
                "local:fake".into()
            }
            fn dimension(&self) -> usize {
                2
            }
            fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError> {
                Ok(texts.iter().map(|_| vec![1.0, 0.0]).collect())
            }
            fn embed_query(&self, _: &str) -> Result<Vec<f32>, EmbedError> {
                Ok(vec![1.0, 0.0])
            }
        }
        struct Source;
        impl EmbedderSource for Source {
            fn vector_set(&self, choice: &str) -> Option<VectorSet> {
                (choice == "local:fake").then(|| VectorSet {
                    origin: "local".into(),
                    model: "fake".into(),
                    dim: 2,
                })
            }
            fn load(&self, _: &str, _: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
                Ok(Arc::new(Fake))
            }
        }

        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join("a.py"), "class A:\n    pass\n").unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(on()))));
        let embedding = Arc::new(
            Embedding::start(
                Arc::new(Source),
                Arc::new(AlwaysRun),
                EmbedConfig {
                    poll: Duration::from_millis(100),
                    ..EmbedConfig::default()
                },
                Arc::new(|_: &str, _: &elitea_local_index::embedding::EmbedStatus| {}),
            )
            .unwrap(),
        );
        let registry = IndexRegistry::new(workspaces, policy, Arc::new(NoEvents))
            .with_refresh_delay(None)
            .with_watch(None)
            .with_embedding(embedding.clone())
            .shared();
        let held = registry.hold_for_turn();
        let service = registry.for_turn(&id).await;
        assert!(service.is_none(), "not turned on yet");
        registry.enable(&id).await.unwrap();
        until_built(&registry, &id).await;
        let service = registry.for_turn(&id).await.unwrap();
        service.store().set_setting(SETTING, "local:fake").unwrap();
        embedding.wake();
        for _ in 0..200 {
            if embedding
                .status(&id)
                .is_some_and(|s| s.held == Some("turn"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(embedding.status(&id).unwrap().state, EmbedState::Paused);
        drop(held);
        for _ in 0..500 {
            if embedding
                .status(&id)
                .is_some_and(|s| s.state == EmbedState::Done)
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let (with, all) = service.store().vector_coverage("local:fake:2").unwrap();
        assert!(all > 0 && with == all, "{with}/{all}");
        assert_eq!(registry.embeddings(&id).await, "local:fake");

        // The app quits with the pass unfinished (here: no vectors left).
        service.store().prune_vectors(None).unwrap();
        service.close();
        drop((service, registry, embedding));
        let start = || {
            Arc::new(
                Embedding::start(
                    Arc::new(Source),
                    Arc::new(AlwaysRun),
                    EmbedConfig {
                        poll: Duration::from_millis(100),
                        ..EmbedConfig::default()
                    },
                    Arc::new(|_: &str, _: &elitea_local_index::embedding::EmbedStatus| {}),
                )
                .unwrap(),
            )
        };
        let embedding = start();
        let policy = Arc::new(Policy(Mutex::new(Some(on()))));
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let registry = IndexRegistry::new(workspaces, policy, Arc::new(NoEvents))
            .with_refresh_delay(None)
            .with_watch(None)
            .with_embedding(embedding.clone())
            .shared();
        // Not open yet: the setting is read from the file.
        assert_eq!(registry.embeddings(&id).await, "local:fake");
        assert_eq!(
            registry.resume_embeddings().await,
            1,
            "the next start resumes it"
        );
        assert_eq!(registry.resume_embeddings().await, 0, "already open");
        for _ in 0..500 {
            if embedding
                .status(&id)
                .is_some_and(|s| s.state == EmbedState::Done)
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let service = registry.for_turn(&id).await.unwrap();
        let (with, all) = service.store().vector_coverage("local:fake:2").unwrap();
        assert!(all > 0 && with == all, "resumed: {with}/{all}");
        assert!(embedding.status(&id).is_some());
        // Turned off: the embedder forgets it (no stale status).
        drop(service);
        registry.disable(&id).await.unwrap();
        assert!(
            embedding.status(&id).is_none(),
            "{:?}",
            embedding.status(&id)
        );
        registry.enable(&id).await.unwrap();
        until_built(&registry, &id).await;
        registry.remove(&id, true).await.unwrap();
        assert!(embedding.status(&id).is_none());
    }

    /// Embeddings are off until set, and only to a way the policy allows.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn embeddings_are_set_only_as_the_policy_allows() {
        let f = registry(json!({"allowed": true, "local_index": true,
            "index_local_embeddings": true}));
        let refused = f.registry.set_embeddings(&f.id, "off").await.unwrap_err();
        assert_eq!(refused.code, "index_off");
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        assert_eq!(f.registry.embeddings(&f.id).await, "off");
        let unknown = f
            .registry
            .set_embeddings(&f.id, "local:nope")
            .await
            .unwrap_err();
        assert_eq!(unknown.code, "embeddings_unknown");
        let remote = f
            .registry
            .set_embeddings(&f.id, "remote:8:text-embedding-3-small")
            .await
            .unwrap_err();
        assert_eq!(remote.code, "embeddings_not_allowed");
        let stored = f
            .registry
            .set_embeddings(&f.id, "local:multilingual-e5-small")
            .await
            .unwrap();
        assert_eq!(stored, "local:multilingual-e5-small");
        assert_eq!(f.registry.embeddings(&f.id).await, stored);
        // Local embeddings turned off by the policy: refused, off still is not.
        *f.policy.0.lock().unwrap() = Some(on());
        let local = f
            .registry
            .set_embeddings(&f.id, "local:multilingual-e5-small")
            .await
            .unwrap_err();
        assert_eq!(local.code, "embeddings_not_allowed");
        assert_eq!(
            f.registry.set_embeddings(&f.id, "off").await.unwrap(),
            "off"
        );
    }

    /// The estimate counts what a run would embed; the caps default, are
    /// kept per workspace and refuse nonsense.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_estimate_and_the_caps_are_per_workspace() {
        let f = registry(json!({"allowed": true, "local_index": true,
            "index_local_embeddings": true}));
        let off = f.registry.embedding_basis(&f.id, None).await.unwrap_err();
        assert_eq!(off.code, "index_off");
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        let basis = f.registry.embedding_basis(&f.id, None).await.unwrap();
        assert!(basis.texts > 0 && basis.tokens >= basis.texts, "{basis:?}");
        assert_eq!(basis.caps, EmbedCaps::default());
        assert_eq!(basis.day_used, 0);
        let kept = EmbedCaps {
            run: 5000,
            day: 900,
        };
        assert_eq!(
            f.registry.set_embedding_caps(&f.id, kept).await.unwrap(),
            kept
        );
        assert_eq!(
            f.registry.embedding_basis(&f.id, None).await.unwrap().caps,
            kept
        );
        for bad in [
            EmbedCaps { run: 0, day: 5 },
            EmbedCaps { run: 5, day: 0 },
            EmbedCaps {
                run: 5,
                day: u64::MAX,
            },
        ] {
            let refused = f.registry.set_embedding_caps(&f.id, bad).await.unwrap_err();
            assert_eq!(refused.code, "embed_caps_invalid");
        }
        assert_eq!(
            f.registry.embedding_basis(&f.id, None).await.unwrap().caps,
            kept,
            "a refused change changes nothing"
        );
    }

    /// `workspace_remove`'s path (`with_closed`) drops the workspace's
    /// entry with its files.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_removed_workspace_leaves_no_entry() {
        let f = registry(on());
        f.registry.enable(&f.id).await.unwrap();
        until_built(&f.registry, &f.id).await;
        assert_eq!(f.registry.entry_count(), 1);
        let workspaces = f.workspaces.clone();
        let id = f.id.clone();
        f.registry
            .with_closed(&f.id, move || workspaces.remove(&id))
            .await
            .unwrap()
            .unwrap();
        assert_eq!(f.registry.entry_count(), 0);
    }
}
