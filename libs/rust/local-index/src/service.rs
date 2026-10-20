//! One workspace's index, kept current: [`IndexService`].
//!
//! A refresh takes the store's lease, starts from the build it answers from
//! (its in-memory graph, when that is still the stored revision; loaded
//! from SQLite otherwise), lists the folder ([`LocalFolderSource`],
//! re-hashing only what changed since the last build), runs the shared
//! ingestion over it (`ingest_documents`: unchanged files skipped, changed
//! and deleted ones removed first, new and changed ones parsed from the
//! bytes the confined read returned), commits graph, versions, stat cache
//! and policy fingerprint in one transaction, and swaps the in-memory view
//! the tools read. It runs on the blocking pool, one at a time across every
//! workspace (a process-wide gate), reports progress through [`IndexEvents`], and
//! stops at its next checkpoint when cancelled; a cancelled or failed
//! refresh commits nothing, so the previous build stays what the tools
//! answer from. A cancelled one is recorded as such, never as a failure.
//!
//! A build is only served under the policy it was made with
//! ([`policy_fingerprint`]): one made before `path_deny` changed may hold
//! files the person has since denied, so it is never loaded, and the tools
//! are withheld ([`IndexState::StalePolicy`]) until a rebuild under the
//! current policy commits.
//!
//! A turn that changed files marks the index stale
//! ([`IndexService::mark_changed`]); the host schedules the refresh that
//! follows, because only it knows the policy that refresh must run under.
//!
//! Once closed (the index removed, turned off, or reopened under another
//! policy) a service writes, reports and answers nothing more.
//!
//! The tools never touch SQLite: they read the view held here.

use crate::chunks;
use crate::dense::{DenseSearch, QueryEmbedder, VectorMatrix};
use crate::search::{Answer, Dense, NameIndex, Request};
use crate::source::{LocalFolderSource, SOURCE_NAME};
use crate::sqlite_store::{ChunkUpdate, SqliteGraphStore, StoreError};
use elitea_content_source::content_version;
use elitea_engine_core::stream::{Context, Line, StopSignal};
use elitea_inventory_core::graph::Graph;
use elitea_inventory_core::ingest::SourceSelection;
use elitea_inventory_core::ingest::files::Selection;
use elitea_inventory_core::retrieval::view::GraphView;
use elitea_inventory_core::store::{
    Completion, GraphKey, GraphStore as _, RunCounts, SourceStatus,
};
use elitea_local_tools::deny::DenyList;
use elitea_local_tools::files::MAX_FILE_BYTES;
use elitea_local_tools::workspace::Workspace;
use serde::Serialize;
use std::collections::BTreeMap;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, Weak};
use std::time::{Duration, Instant};
use tokio::sync::Semaphore;

const KEY: GraphKey = GraphKey::LOCAL;

/// One refresh at a time, across every workspace of the process: a refresh
/// parses on half the cores ([`limit_parser_threads`]), so two at once
/// would take the whole machine. A refresh waits here (`queued`) before it
/// lists anything; a cancel or a close ends the wait.
static REFRESHES: Semaphore = Semaphore::const_new(1);

/// The fingerprint of what decides which files an index holds: the
/// workspace's `path_deny` (as a set), the largest file read, and the
/// selection (every supported document). A build is served only under the
/// fingerprint it was committed with.
#[must_use]
pub fn policy_fingerprint(path_deny: &[String]) -> String {
    let mut deny = path_deny.to_vec();
    deny.sort();
    deny.dedup();
    let inputs = serde_json::json!({
        // 2: the host's deny list (credentials, the app's own data) is left
        // out too; a build made before may hold them and is not answered.
        "version": 2,
        "path_deny": deny,
        "max_file_bytes": MAX_FILE_BYTES,
        "selection": "all",
    });
    content_version(inputs.to_string().as_bytes())
}

/// Where an index is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum IndexState {
    /// Not turned on (or turned off): nothing is built or offered.
    Off,
    /// A refresh runs (or waits its turn); the previous build (if any)
    /// still answers.
    Building,
    /// Built, and nothing is known to have changed since.
    Ready,
    /// Built, but files changed since (or the app has not checked yet).
    Stale,
    /// The build on disk was made under another policy (`path_deny`
    /// changed): it is not served, and the tools are withheld until a
    /// rebuild under the current policy commits.
    StalePolicy,
    /// The last refresh failed; the previous build (if any) still answers.
    Error,
}

/// What `index_status` answers.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct IndexStatus {
    pub state: IndexState,
    /// Documents of the last build.
    pub files: u64,
    pub entities: u64,
    pub relations: u64,
    /// Embedded entities (none until embeddings are turned on).
    pub vectors: u64,
    /// Search chunks of the last build (what embedding a folder costs).
    pub chunks: u64,
    /// When the last build was committed (UTC, ISO 8601).
    pub last_run: Option<String>,
    /// Files known to have changed since the last build.
    pub changed_files: u64,
    pub error: Option<String>,
    /// The policy turns the index off (the host still lets the person turn
    /// it off and remove it).
    pub policy_off: bool,
    /// An index exists on this computer (what turning it off and removing
    /// it act on).
    pub on_disk: bool,
}

impl IndexStatus {
    /// An index that is not turned on.
    #[must_use]
    pub fn off() -> Self {
        Self {
            state: IndexState::Off,
            files: 0,
            entities: 0,
            relations: 0,
            vectors: 0,
            chunks: 0,
            last_run: None,
            changed_files: 0,
            error: None,
            policy_off: false,
            on_disk: false,
        }
    }
}

/// What the last completed refresh did.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize)]
pub struct RunReport {
    /// Files parsed (new or changed).
    pub read: u64,
    /// Files whose version was unchanged.
    pub unchanged: u64,
    /// Files gone since the previous build.
    pub removed: u64,
    /// Files read to hash because their size or mtime changed.
    pub hashed: u64,
}

/// One progress report of an index (`index://event`).
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct IndexEvent {
    pub workspace_id: String,
    /// `queued` (waiting for another workspace's refresh), `listing`,
    /// `parsing`, `building`, `saving`, `ready`, `stale`, `cancelled` or
    /// `error`.
    pub phase: &'static str,
    pub message: Option<String>,
    pub status: IndexStatus,
}

/// Where an index reports to (the desktop's main window).
pub trait IndexEvents: Send + Sync {
    fn emit(&self, event: IndexEvent);
}

/// Reports to nobody.
#[derive(Debug, Default)]
pub struct NoEvents;

impl IndexEvents for NoEvents {
    fn emit(&self, _event: IndexEvent) {}
}

/// A refused or failed index operation, with a machine code.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IndexError {
    pub code: &'static str,
    pub message: String,
}

impl IndexError {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

impl std::fmt::Display for IndexError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for IndexError {}

impl From<StoreError> for IndexError {
    fn from(error: StoreError) -> Self {
        let code = match error {
            StoreError::NewerSchema(_) => "index_newer_schema",
            StoreError::Refused(_) => "index_refused",
            StoreError::Closed => "index_closed",
            _ => "index_storage",
        };
        Self::new(code, error.to_string())
    }
}

/// Cap the parsers' worker threads at half the cores (at least one): a
/// refresh shares the machine with the person's own work.
pub fn limit_parser_threads() {
    let cores = std::thread::available_parallelism().map_or(2, std::num::NonZero::get);
    elitea_code_parsers::set_parser_threads((cores / 2).max(1));
}

struct Inner {
    state: IndexState,
    view: Option<Arc<GraphView>>,
    error: Option<String>,
    changed_files: u64,
    files: u64,
    chunks: u64,
    last_run: Option<String>,
    report: Option<RunReport>,
    /// The running refresh's stop flag.
    running: Option<StopSignal>,
    /// The state, and the error, before the running refresh began: what a
    /// cancelled one goes back to.
    before: IndexState,
    error_before: Option<String>,
    /// `changed_files` when the running refresh began: changes reported
    /// during it may not be in what it reads.
    changed_at_start: u64,
    /// The build on disk is from another policy: the next refresh rebuilds
    /// from nothing, and nothing is served until it commits.
    policy_mismatch: bool,
    closed: bool,
    /// The graph's names for `search_workspace`, of the view's revision.
    names: Option<Arc<NameIndex>>,
    /// The files the last build listed but kept out (unreadable, not
    /// text, empty after extraction), at the stat version they had: they
    /// are no change for [`IndexService::unchanged`].
    skipped: std::collections::HashMap<String, String>,
    /// The running refresh found nothing changed and wrote nothing (no run
    /// recorded): a cancel that reaches it before it ends wins.
    shortcut: bool,
}

/// One workspace's index.
pub struct IndexService {
    workspace_id: String,
    workspace: Arc<Workspace>,
    store: SqliteGraphStore,
    events: Arc<dyn IndexEvents>,
    /// [`policy_fingerprint`] of the policy it was opened under.
    policy: String,
    inner: Mutex<Inner>,
    /// What embeds a search's query (the background embedder, once it
    /// knows this index).
    query_embedder: Mutex<Option<Weak<dyn QueryEmbedder>>>,
    /// The vectors of the dense side, cached.
    matrix: Mutex<Option<CachedMatrix>>,
}

/// A loaded vector matrix and what it was loaded at.
struct CachedMatrix {
    version: u64,
    /// The store's chunks version it was loaded at: another one, and its
    /// chunk ids are stale (never reused, whatever its age).
    chunks: u64,
    at: Instant,
    matrix: Arc<VectorMatrix>,
}

/// A matrix older than the vectors is reused for this long while only new
/// vectors were added (the background embedder writes a batch every second
/// or so); a build or a prune reloads it at once.
const MATRIX_RELOAD: Duration = Duration::from_secs(30);

impl std::fmt::Debug for IndexService {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("IndexService")
            .field("workspace_id", &self.workspace_id)
            .field("store", &self.store.path())
            .finish_non_exhaustive()
    }
}

fn block_on<F: std::future::Future>(future: F) -> Result<F::Output, IndexError> {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .build()
        .map_err(|error| IndexError::new("internal", error.to_string()))?;
    Ok(runtime.block_on(future))
}

/// What a completed build hands back: the view, its files, the report.
type Built = (Arc<GraphView>, u64, RunReport);

fn count(n: usize) -> u64 {
    u64::try_from(n).unwrap_or(u64::MAX)
}

fn closed_error() -> IndexError {
    IndexError::new(
        "index_closed",
        "The workspace index was removed or turned off.",
    )
}

impl IndexService {
    /// Open the index of the workspace at `root` (its `path_deny`
    /// applies) kept in `index_dir`, and load its last build. Blocking.
    ///
    /// It comes up [`IndexState::Stale`]: the folder may have changed while
    /// the app was closed (or it was never built); a refresh says. A build
    /// made under another `path_deny` is not loaded at all: the index comes
    /// up [`IndexState::StalePolicy`], with nothing to answer from.
    ///
    /// # Errors
    ///
    /// The folder cannot be opened, the index directory is inside it or is
    /// refused, or the index is damaged or from a newer app.
    ///
    /// Blocking: call it off the async runtime's threads.
    pub fn open(
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
        index_dir: &Path,
        events: Arc<dyn IndexEvents>,
    ) -> Result<Arc<Self>, IndexError> {
        Self::open_with_deny(workspace_id, root, path_deny, None, index_dir, events)
    }

    /// [`Self::open`], leaving out what the host's `deny` list covers too
    /// (credentials, the app's own data): never listed, read or indexed,
    /// wherever the workspace sits.
    ///
    /// # Errors
    ///
    /// As [`Self::open`].
    pub fn open_with_deny(
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
        deny: Option<Arc<DenyList>>,
        index_dir: &Path,
        events: Arc<dyn IndexEvents>,
    ) -> Result<Arc<Self>, IndexError> {
        let mut workspace = Workspace::open(root, path_deny)
            .map_err(|error| IndexError::new("workspace_unavailable", error.message()))?;
        if let Some(deny) = deny {
            workspace = workspace.with_deny_list(deny);
        }
        let store = SqliteGraphStore::open_for_workspace(index_dir, workspace.root())?;
        let policy = policy_fingerprint(path_deny);
        let built = store.revision_now(KEY)?.is_some();
        let policy_mismatch = built && store.policy()?.as_deref() != Some(policy.as_str());
        let last_run = store.last_completed_run(KEY)?;
        let chunks = if policy_mismatch {
            0
        } else {
            store.chunk_count()?
        };
        let (state, view, files) = if policy_mismatch {
            (IndexState::StalePolicy, None, 0)
        } else {
            let files = count(store.document_stats(KEY, SOURCE_NAME)?.len());
            let view = store
                .load_view_now(KEY)?
                .map(|read| Arc::new(GraphView::from_read(read)));
            // Turned on, never built yet, or built: not checked yet.
            (IndexState::Stale, view, files)
        };
        Ok(Arc::new(Self {
            workspace_id: workspace_id.to_owned(),
            workspace: Arc::new(workspace),
            store,
            events,
            policy,
            inner: Mutex::new(Inner {
                state,
                view,
                error: None,
                changed_files: 0,
                files,
                chunks,
                last_run,
                report: None,
                running: None,
                before: state,
                error_before: None,
                changed_at_start: 0,
                policy_mismatch,
                closed: false,
                names: None,
                skipped: std::collections::HashMap::new(),
                shortcut: false,
            }),
            query_embedder: Mutex::new(None),
            matrix: Mutex::new(None),
        }))
    }

    fn inner(&self) -> MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The workspace this index is of.
    #[must_use]
    pub fn workspace(&self) -> &Arc<Workspace> {
        &self.workspace
    }

    /// The workspace's id.
    #[must_use]
    pub fn workspace_id(&self) -> &str {
        &self.workspace_id
    }

    /// The store (for its path, settings and tests).
    #[must_use]
    pub fn store(&self) -> &SqliteGraphStore {
        &self.store
    }

    /// [`policy_fingerprint`] of the policy this service was opened under.
    #[must_use]
    pub fn policy(&self) -> &str {
        &self.policy
    }

    fn status_of(inner: &Inner) -> IndexStatus {
        let (entities, relations) = inner.view.as_ref().map_or((0, 0), |view| {
            (
                count(view.graph.node_count()),
                count(view.graph.edge_count()),
            )
        });
        IndexStatus {
            state: inner.state,
            files: inner.files,
            entities,
            relations,
            vectors: 0,
            chunks: inner.chunks,
            last_run: inner.last_run.clone(),
            changed_files: inner.changed_files,
            error: inner.error.clone(),
            policy_off: false,
            on_disk: true,
        }
    }

    /// What the last refresh of this session did, if one completed.
    #[must_use]
    pub fn last_report(&self) -> Option<RunReport> {
        self.inner().report
    }

    /// Where the index is now.
    #[must_use]
    pub fn status(&self) -> IndexStatus {
        Self::status_of(&self.inner())
    }

    /// Whether [`Self::close`] ran.
    #[must_use]
    pub fn is_closed(&self) -> bool {
        self.inner().closed
    }

    fn emit(&self, phase: &'static str, message: Option<String>) {
        let status = {
            let inner = self.inner();
            if inner.closed {
                return;
            }
            Self::status_of(&inner)
        };
        self.events.emit(IndexEvent {
            workspace_id: self.workspace_id.clone(),
            phase,
            message,
            status,
        });
    }

    /// The build the tools answer from, and the one-line notice an answer
    /// starts with when that build may be out of date. `None` until there
    /// is a build made under the current policy, and once closed.
    #[must_use]
    pub fn view(&self) -> Option<(Arc<GraphView>, Option<String>)> {
        let inner = self.inner();
        if inner.closed || inner.policy_mismatch {
            return None;
        }
        let view = inner.view.clone()?;
        let notice = match inner.state {
            IndexState::Ready | IndexState::Off | IndexState::StalePolicy => None,
            IndexState::Building => Some(
                "Note: the workspace index is being refreshed; this answer is from the previous build."
                    .to_owned(),
            ),
            IndexState::Stale if inner.changed_files > 0 => Some(format!(
                "Note: the workspace index may be out of date: {} file(s) changed since the last build.",
                inner.changed_files
            )),
            IndexState::Stale => Some(
                "Note: the workspace index may be out of date: the folder has not been checked since the app started."
                    .to_owned(),
            ),
            IndexState::Error => Some(format!(
                "Note: the last refresh of the workspace index failed ({}); this answer is from the previous build.",
                inner.error.as_deref().unwrap_or("unknown error")
            )),
        };
        Some((view, notice))
    }

    /// The name index of `view` (built on first use after a build).
    fn names_of(&self, view: &GraphView) -> Arc<NameIndex> {
        if let Some(names) = self.inner().names.clone()
            && names.revision == view.revision
        {
            return names;
        }
        let names = Arc::new(NameIndex::build(view));
        let mut inner = self.inner();
        if inner
            .view
            .as_ref()
            .is_some_and(|current| current.revision == view.revision)
        {
            inner.names = Some(Arc::clone(&names));
        }
        names
    }

    /// Embed this index's search queries with `embedder` (the background
    /// embedder calls this when it is given the index).
    pub fn attach_query_embedder(&self, embedder: Weak<dyn QueryEmbedder>) {
        *self
            .query_embedder
            .lock()
            .unwrap_or_else(PoisonError::into_inner) = Some(embedder);
    }

    /// The dense side of a search, when this index's setting names an
    /// embedder that is allowed now and vectors of it exist. Loads the
    /// vectors (blocking) when they changed, at most every 30 s while
    /// they are being written.
    #[must_use]
    pub fn dense(&self) -> Option<Arc<dyn Dense>> {
        let provider = self
            .query_embedder
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .as_ref()?
            .upgrade()?;
        let choice = crate::embedding::choice_of(self);
        if choice == crate::embedding::OFF || self.is_closed() {
            return None;
        }
        let set = provider.vector_set(&choice)?;
        let stamp = set.stamp();
        let version = self.store.vectors_version();
        let chunks = self.store.chunks_version();
        let mut cached = self.matrix.lock().unwrap_or_else(PoisonError::into_inner);
        let fresh = cached.as_ref().is_some_and(|known| {
            known.matrix.stamp() == stamp
                && known.chunks == chunks
                && (known.version == version
                    || (!known.matrix.is_empty() && known.at.elapsed() < MATRIX_RELOAD))
        });
        if !fresh {
            let matrix = VectorMatrix::load(&self.store, &set).ok()?;
            *cached = Some(CachedMatrix {
                version,
                chunks,
                at: Instant::now(),
                matrix: Arc::new(matrix),
            });
        }
        let matrix = Arc::clone(&cached.as_ref()?.matrix);
        drop(cached);
        if matrix.is_empty() {
            return None;
        }
        Some(Arc::new(DenseSearch {
            matrix,
            embedder: provider,
            choice,
            workspace_id: self.workspace_id.clone(),
        }))
    }

    /// `search_workspace` over the build the tools answer from (and the
    /// chunks committed with it), with the notice an answer starts with.
    /// Blocking (SQLite).
    ///
    /// # Errors
    ///
    /// `index_closed`, `index_not_ready` (no build under the current
    /// policy yet), or the store failed.
    pub fn search(
        &self,
        request: &Request,
        dense: Option<&dyn Dense>,
    ) -> Result<(Answer, Option<String>), IndexError> {
        if self.is_closed() {
            return Err(closed_error());
        }
        let (view, notice) = self.view().ok_or_else(|| {
            IndexError::new(
                "index_not_ready",
                "The workspace index has not been built yet.",
            )
        })?;
        let names = self.names_of(&view);
        let answer = crate::search::search_workspace(&self.store, &view, &names, request, dense)?;
        Ok((answer, notice))
    }

    /// Note that `files` files changed (a turn wrote them): a ready index
    /// becomes stale until the next refresh (which the host schedules).
    /// Never blocks on a refresh.
    pub fn mark_changed(&self, files: u64) {
        if files == 0 {
            return;
        }
        {
            let mut inner = self.inner();
            if inner.closed {
                return;
            }
            inner.changed_files = inner.changed_files.saturating_add(files);
            if inner.state == IndexState::Ready {
                inner.state = IndexState::Stale;
            }
        }
        self.emit("stale", None);
    }

    /// Whether a refresh runs (or waits its turn).
    #[must_use]
    pub fn is_building(&self) -> bool {
        self.inner().running.is_some()
    }

    /// Ask the running refresh to stop at its next checkpoint; `false` when
    /// none runs.
    pub fn cancel(&self) -> bool {
        self.inner()
            .running
            .as_ref()
            .map(StopSignal::request)
            .is_some()
    }

    /// Stop any refresh and close the database (its WAL folded in): what a
    /// removal, turning it off or a policy change does. Nothing is written,
    /// reported or answered afterwards: a refresh that ends later leaves
    /// the service as it is, and the tools answer `index.closed`.
    pub fn close(&self) {
        {
            let mut inner = self.inner();
            inner.closed = true;
            if let Some(stop) = &inner.running {
                stop.request();
            }
            inner.view = None;
            inner.state = IndexState::Off;
        }
        *self.matrix.lock().unwrap_or_else(PoisonError::into_inner) = None;
        self.store.close();
    }

    /// Start a refresh in the background (`full`: rebuild from nothing,
    /// re-reading every file) and return at once; progress and the end
    /// arrive as events.
    ///
    /// # Errors
    ///
    /// `index_busy` while one runs, `index_closed` after [`Self::close`].
    pub fn start_refresh(self: &Arc<Self>, full: bool) -> Result<(), IndexError> {
        let stop = self.begin()?;
        let this = Arc::clone(self);
        tokio::spawn(async move {
            let _ = this.run(full, stop).await;
        });
        Ok(())
    }

    /// Run a refresh to its end.
    ///
    /// # Errors
    ///
    /// `index_busy` while one runs, `index_closed` after [`Self::close`],
    /// `index_cancelled` when cancelled, or what failed.
    pub async fn refresh(self: &Arc<Self>, full: bool) -> Result<IndexStatus, IndexError> {
        let stop = self.begin()?;
        self.run(full, stop).await
    }

    fn begin(&self) -> Result<StopSignal, IndexError> {
        let mut inner = self.inner();
        if inner.closed {
            return Err(closed_error());
        }
        if inner.running.is_some() {
            return Err(IndexError::new(
                "index_busy",
                "The index is already being built.",
            ));
        }
        let stop = StopSignal::default();
        inner.running = Some(stop.clone());
        inner.before = inner.state;
        let fields = &mut *inner;
        fields.error_before.clone_from(&fields.error);
        inner.changed_at_start = inner.changed_files;
        inner.state = IndexState::Building;
        inner.error = None;
        Ok(stop)
    }

    async fn run(
        self: &Arc<Self>,
        full: bool,
        stop: StopSignal,
    ) -> Result<IndexStatus, IndexError> {
        // One refresh at a time across every workspace.
        let permit = if let Ok(permit) = REFRESHES.try_acquire() {
            Some(permit)
        } else {
            self.emit("queued", None);
            tokio::select! {
                permit = REFRESHES.acquire() => permit.ok(),
                () = stop.stopped() => None,
            }
        };
        self.inner().shortcut = false;
        let mut outcome = if permit.is_some() {
            self.emit("listing", None);
            let this = Arc::clone(self);
            let worker_stop = stop.clone();
            tokio::task::spawn_blocking(move || this.build(full, &worker_stop))
                .await
                .unwrap_or_else(|_| {
                    Err(IndexError::new(
                        "internal",
                        "the refresh stopped unexpectedly",
                    ))
                })
        } else {
            Err(IndexError::new("index_cancelled", "cancelled while queued"))
        };
        drop(permit);
        let (last_run, chunks) = if outcome.is_ok() {
            (
                self.store.last_completed_run(KEY).ok().flatten(),
                self.store.chunk_count().ok(),
            )
        } else {
            (None, None)
        };
        let phase = {
            let mut inner = self.inner();
            inner.running = None;
            // A run that wrote nothing honours a cancel that came while it
            // ran (under this lock, as `cancel` takes it): a cancel either
            // found the run and wins, or found none and answers `false`.
            if std::mem::take(&mut inner.shortcut) && stop.is_requested() && outcome.is_ok() {
                outcome = Err(IndexError::new("index_cancelled", "cancelled"));
            }
            if inner.closed {
                // Removed, turned off or reopened meanwhile: nothing it
                // built is kept or served, and nothing is reported.
                return Err(closed_error());
            }
            match &outcome {
                Ok((view, files, report)) => {
                    inner.view = Some(Arc::clone(view));
                    inner.files = *files;
                    inner.report = Some(*report);
                    if let Some(chunks) = chunks {
                        inner.chunks = chunks;
                    }
                    inner.policy_mismatch = false;
                    // Changes reported while it ran may not be in what it
                    // read: they keep it stale (their refresh follows).
                    inner.changed_files =
                        inner.changed_files.saturating_sub(inner.changed_at_start);
                    inner.state = if inner.changed_files == 0 {
                        IndexState::Ready
                    } else {
                        IndexState::Stale
                    };
                    inner.error = None;
                    if last_run.is_some() {
                        inner.last_run = last_run;
                    }
                    "ready"
                }
                Err(_) if stop.is_requested() => {
                    // The previous build stays, and so does where it was:
                    // its state and, after a failure, why it failed.
                    inner.error = inner.error_before.take();
                    inner.state = match inner.before {
                        IndexState::Ready if inner.changed_files > inner.changed_at_start => {
                            IndexState::Stale
                        }
                        before => before,
                    };
                    "cancelled"
                }
                Err(error) => {
                    inner.state = IndexState::Error;
                    inner.error = Some(error.message.clone());
                    "error"
                }
            }
        };
        let message = outcome.as_ref().err().map(|error| error.message.clone());
        self.emit(phase, message);
        match outcome {
            Ok(_) => Ok(self.status()),
            Err(_) if stop.is_requested() => Err(IndexError::new(
                "index_cancelled",
                "The refresh was cancelled; the previous build is kept.",
            )),
            Err(error) => Err(error),
        }
    }

    /// The refresh itself, on a blocking thread: the new view, its files,
    /// and what the run did.
    fn build(self: &Arc<Self>, full: bool, stop: &StopSignal) -> Result<Built, IndexError> {
        let lease = block_on(self.store.lease(KEY))??
            .ok_or_else(|| IndexError::new("index_busy", "The index is already being built."))?;
        let (lines, mut progress) = tokio::sync::mpsc::unbounded_channel::<Line>();
        let context = Context::new(lines, stop.clone());
        let reporter = {
            let this = Arc::clone(self);
            std::thread::Builder::new()
                .name("index-progress".into())
                .spawn(move || {
                    while let Some(line) = progress.blocking_recv() {
                        if let Line::Thinking(text) = line {
                            let phase = if text.starts_with("[extract]") {
                                "parsing"
                            } else {
                                "building"
                            };
                            this.emit(phase, Some(text));
                        }
                    }
                })
                .map_err(|error| IndexError::new("internal", error.to_string()))?
        };
        let (result, started) = self.build_with(full, &context);
        drop(context);
        let _ = reporter.join();
        if started && let Err(error) = &result {
            // A cancellation is not a failure: its run is recorded as
            // cancelled and the last completed one stays the source's.
            if stop.is_requested() {
                let _ = self.store.cancel_run(KEY, SOURCE_NAME);
            } else {
                let _ = block_on(self.store.fail(KEY, SOURCE_NAME, &error.message));
            }
        }
        drop(lease);
        result
    }

    /// The graph an incremental refresh starts from: the build the tools
    /// answer from, when it is still the stored revision (no SQLite read);
    /// else the stored graph.
    fn starting_graph(&self) -> Result<Graph, IndexError> {
        let view = self.inner().view.clone();
        let stored = self.store.revision_now(KEY)?;
        match view {
            // The view holds no vectors: a graph that has any is read from
            // the store, or the refresh would write it back without them.
            Some(view) if Some(view.revision) == stored && !view.has_embeddings() => {
                Ok(view.graph.clone())
            }
            _ => Ok(self
                .store
                .load_now(KEY)?
                .map(|(graph, _)| graph)
                .unwrap_or_default()),
        }
    }

    /// The build served now, when the folder has not changed since it: it
    /// is the stored revision, and a listing finds every file at its
    /// recorded `(size, mtime)` and none gone. Lists the folder (one `stat`
    /// per file); reads nothing.
    fn unchanged(
        &self,
        previous: &BTreeMap<String, String>,
        stats: &std::collections::HashMap<String, crate::sqlite_store::DocumentStat>,
    ) -> Result<Option<Built>, IndexError> {
        let Some(view) = self.inner().view.clone() else {
            return Ok(None);
        };
        if Some(view.revision) != self.store.revision_now(KEY)? {
            return Ok(None);
        }
        let source = LocalFolderSource::new(Arc::clone(&self.workspace))
            .with_previous(stats.clone())
            .selecting(Selection::default());
        let listed = source
            .list_now()
            .map_err(|error| IndexError::new("index_failed", error.to_string()))?;
        // An empty file is never a document: it stays out of `previous`.
        let documents: Vec<_> = listed
            .iter()
            .filter(|reference| reference.size > 0 && !reference.version.is_empty())
            .collect();
        let skipped = self.inner().skipped.clone();
        let mut kept = 0;
        let mut same = true;
        for reference in &documents {
            if previous.get(&reference.key) == Some(&reference.version) {
                kept += 1;
            } else if skipped.get(&reference.key) != Some(&reference.version) {
                same = false;
                break;
            }
        }
        let same = same && kept == previous.len();
        if !same {
            return Ok(None);
        }
        let files = count(previous.len());
        let report = RunReport {
            read: 0,
            unchanged: files,
            removed: 0,
            hashed: 0,
        };
        Ok(Some((view, files, report)))
    }

    /// The run, and whether it got as far as recording its start.
    fn build_with(&self, full: bool, context: &Context) -> (Result<Built, IndexError>, bool) {
        let mut started = false;
        let result = block_on(async {
            // A build from another policy is never the start of this one.
            let full = full || self.inner().policy_mismatch;
            // Chunks made by another chunker (or none yet, an index built
            // before chunks): every file is read once more to cut them;
            // the unchanged ones are not parsed again.
            let rechunk = self.store.chunk_generation()?.as_deref() != Some(chunks::GENERATION);
            let (previous, mut stat_cache) = if full {
                (BTreeMap::new(), std::collections::HashMap::new())
            } else {
                (
                    self.store.document_versions(KEY, SOURCE_NAME).await?,
                    self.store.document_stats(KEY, SOURCE_NAME)?,
                )
            };
            if rechunk {
                stat_cache.clear();
            }
            if !full
                && !rechunk
                && let Some(unchanged) = self.unchanged(&previous, &stat_cache)?
            {
                // Nothing to read and nothing gone: the build served now is
                // the folder's (no run, and no copy of its graph).
                self.inner().shortcut = true;
                return Ok(unchanged);
            }
            let mut graph = if full {
                Graph::new()
            } else {
                self.starting_graph()?
            };
            self.store.start(KEY, &source_status()).await?;
            started = true;
            self.store.clear_staged_chunks()?;
            let source = LocalFolderSource::new(Arc::clone(&self.workspace))
                .with_previous(stat_cache)
                .selecting(Selection::default())
                .chunking();
            // Chunking is CPU work as heavy as parsing: half the cores.
            let threads =
                std::thread::available_parallelism().map_or(2, std::num::NonZero::get) / 2;
            // Each batch's chunks go to the store's staging table as soon
            // as the batch is read; the commit moves them in.
            let stage = || {
                self.store
                    .stage_chunks(&source.take_staged(threads))
                    .map_err(engine_error)
            };
            let outcome = crate::ingest::ingest(
                &mut graph,
                &SourceSelection::all(SOURCE_NAME),
                &source,
                self.workspace.root(),
                &previous,
                context,
                stage,
            )
            .await
            .map_err(|error| IndexError::new("index_failed", error.message))?;
            context
                .checkpoint()
                .map_err(|error| IndexError::new("index_failed", error.message))?;
            let stats = source.stats();
            self.inner().skipped = skipped(&stats, &outcome.documents);
            self.emit("saving", None);
            let completion = Completion {
                toolkit_id: SOURCE_NAME,
                source_name: SOURCE_NAME,
                documents: &outcome.documents,
                counts: RunCounts {
                    entities: i64::try_from(graph.node_count()).unwrap_or(i64::MAX),
                    relations: i64::try_from(graph.edge_count()).unwrap_or(i64::MAX),
                    documents: i64::try_from(outcome.documents.len()).unwrap_or(i64::MAX),
                },
                commit_sha: None,
            };
            let revision = self.store.complete_with_chunks(
                KEY,
                &graph,
                &completion,
                &stats,
                Some(&self.policy),
                Some(ChunkUpdate {
                    generation: chunks::GENERATION,
                    // Every file was read again: none keeps old chunks.
                    replace: full || rechunk,
                }),
            )?;
            let report = RunReport {
                read: count(outcome.documents_processed),
                unchanged: count(outcome.unchanged),
                removed: count(outcome.removed_files),
                hashed: count(source.hashed()),
            };
            Ok::<_, IndexError>((
                Arc::new(GraphView::without_vectors(graph, revision)),
                count(outcome.documents.len()),
                report,
            ))
        });
        (result.and_then(|built| built), started)
    }
}

/// The folder's source row.
fn source_status() -> SourceStatus {
    SourceStatus {
        toolkit_id: SOURCE_NAME.to_owned(),
        toolkit_name: "Workspace folder".to_owned(),
        toolkit_type: "local_folder".to_owned(),
        branch: None,
    }
}

fn engine_error(error: impl std::fmt::Display) -> elitea_engine_core::errors::EngineError {
    elitea_engine_core::errors::EngineError::new(
        elitea_engine_core::errors::ErrorType::Runtime,
        error.to_string(),
    )
}

/// The files a build listed but holds no document for (empty, unreadable,
/// not text), by stat version: a later refresh that finds them unchanged
/// does not read them again.
fn skipped<V>(
    stats: &crate::sqlite_store::Stats,
    documents: &BTreeMap<String, V>,
) -> std::collections::HashMap<String, String> {
    stats
        .iter()
        .filter(|(key, _)| !documents.contains_key(*key))
        .map(|(key, (size, mtime))| (key.clone(), crate::source::stat_version(*size, *mtime)))
        .collect()
}
