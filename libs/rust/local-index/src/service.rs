//! One workspace's index, kept current: [`IndexService`].
//!
//! A refresh takes the store's lease, loads the stored graph, lists the
//! folder ([`LocalFolderSource`], re-hashing only what changed since the
//! last build), runs the shared ingestion over it (`ingest_documents`:
//! unchanged files skipped, changed and deleted ones removed first, new and
//! changed ones parsed), commits graph, versions and stat cache in one
//! transaction, and swaps the in-memory view the tools read. It runs on
//! the blocking pool, reports progress through [`IndexEvents`], and stops
//! at its next checkpoint when cancelled; a cancelled or failed refresh
//! commits nothing, so the previous build stays what the tools answer
//! from.
//!
//! The tools never touch SQLite: they read the view held here.

use crate::source::{LocalFolderSource, SOURCE_NAME};
use crate::sqlite_store::{SqliteGraphStore, StoreError};
use elitea_engine_core::stream::{Context, Line, StopSignal};
use elitea_inventory_core::graph::Graph;
use elitea_inventory_core::ingest::files::Selection;
use elitea_inventory_core::ingest::{SourceSelection, ingest_documents};
use elitea_inventory_core::retrieval::view::GraphView;
use elitea_inventory_core::store::{
    Completion, GraphKey, GraphStore as _, RunCounts, SourceStatus,
};
use elitea_local_tools::workspace::Workspace;
use serde::Serialize;
use std::collections::BTreeMap;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

const KEY: GraphKey = GraphKey::LOCAL;

/// Where an index is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum IndexState {
    /// Not turned on (or turned off): nothing is built or offered.
    Off,
    /// A refresh runs; the previous build (if any) still answers.
    Building,
    /// Built, and nothing is known to have changed since.
    Ready,
    /// Built, but files changed since (or the app has not checked yet).
    Stale,
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
    /// When the last build was committed (UTC, ISO 8601).
    pub last_run: Option<String>,
    /// Files known to have changed since the last build.
    pub changed_files: u64,
    pub error: Option<String>,
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
            last_run: None,
            changed_files: 0,
            error: None,
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
    /// `listing`, `parsing`, `saving`, `ready`, `cancelled` or `error`.
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
    last_run: Option<String>,
    report: Option<RunReport>,
    /// The running refresh's stop flag.
    running: Option<StopSignal>,
    closed: bool,
}

/// One workspace's index.
pub struct IndexService {
    workspace_id: String,
    workspace: Arc<Workspace>,
    store: SqliteGraphStore,
    events: Arc<dyn IndexEvents>,
    inner: Mutex<Inner>,
}

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

impl IndexService {
    /// Open the index of the workspace at `root` (its `path_deny`
    /// applies) kept in `index_dir`, and load its last build. Blocking.
    ///
    /// It comes up [`IndexState::Stale`]: the folder may have changed while
    /// the app was closed (or it was never built); a refresh says.
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
        let workspace = Workspace::open(root, path_deny)
            .map_err(|error| IndexError::new("workspace_unavailable", error.message()))?;
        let store = SqliteGraphStore::open_for_workspace(index_dir, workspace.root())?;
        let loaded = store.load_now(KEY)?;
        let status = store.status_document_now(KEY)?;
        let files = store.document_stats(KEY, SOURCE_NAME)?.len();
        let source = &status["sources"][SOURCE_NAME];
        let last_run = (source["status"] == "completed")
            .then(|| source["last_updated"].as_str().map(str::to_owned))
            .flatten();
        let (state, view) = match loaded {
            Some((graph, revision)) => (
                IndexState::Stale,
                Some(Arc::new(GraphView::new(graph, revision))),
            ),
            // Turned on, never built yet.
            None => (IndexState::Stale, None),
        };
        Ok(Arc::new(Self {
            workspace_id: workspace_id.to_owned(),
            workspace: Arc::new(workspace),
            store,
            events,
            inner: Mutex::new(Inner {
                state,
                view,
                error: None,
                changed_files: 0,
                files: count(files),
                last_run,
                report: None,
                running: None,
                closed: false,
            }),
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
            last_run: inner.last_run.clone(),
            changed_files: inner.changed_files,
            error: inner.error.clone(),
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
    /// is a build.
    #[must_use]
    pub fn view(&self) -> Option<(Arc<GraphView>, Option<String>)> {
        let inner = self.inner();
        let view = inner.view.clone()?;
        let notice = match inner.state {
            IndexState::Ready | IndexState::Off => None,
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

    /// Note that `files` files changed (a turn wrote them): a ready index
    /// becomes stale until the next refresh.
    pub fn mark_changed(&self, files: u64) {
        if files == 0 {
            return;
        }
        {
            let mut inner = self.inner();
            inner.changed_files = inner.changed_files.saturating_add(files);
            if inner.state == IndexState::Ready {
                inner.state = IndexState::Stale;
            }
        }
        self.emit("stale", None);
    }

    /// Whether a refresh runs.
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
    /// removal does before it deletes the directory. Nothing is written or
    /// reported afterwards.
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
            return Err(IndexError::new("index_closed", "The index was removed."));
        }
        if inner.running.is_some() {
            return Err(IndexError::new(
                "index_busy",
                "The index is already being built.",
            ));
        }
        let stop = StopSignal::default();
        inner.running = Some(stop.clone());
        inner.state = IndexState::Building;
        inner.error = None;
        Ok(stop)
    }

    async fn run(
        self: &Arc<Self>,
        full: bool,
        stop: StopSignal,
    ) -> Result<IndexStatus, IndexError> {
        self.emit("listing", None);
        let this = Arc::clone(self);
        let worker_stop = stop.clone();
        let outcome = tokio::task::spawn_blocking(move || this.build(full, &worker_stop))
            .await
            .unwrap_or_else(|_| {
                Err(IndexError::new(
                    "internal",
                    "the refresh stopped unexpectedly",
                ))
            });
        let phase = {
            let mut inner = self.inner();
            inner.running = None;
            match &outcome {
                Ok((view, files, report)) => {
                    inner.view = Some(Arc::clone(view));
                    inner.files = *files;
                    inner.report = Some(*report);
                    inner.state = IndexState::Ready;
                    inner.changed_files = 0;
                    inner.error = None;
                    "ready"
                }
                Err(_) if stop.is_requested() => {
                    // The previous build stays; so does what was known of it.
                    inner.state = match (&inner.view, inner.changed_files) {
                        (Some(_), 0) if inner.last_run.is_some() => IndexState::Ready,
                        _ => IndexState::Stale,
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
        if phase == "ready" {
            let last_run = self.store.status_document_now(KEY).ok().and_then(|status| {
                status["sources"][SOURCE_NAME]["last_updated"]
                    .as_str()
                    .map(str::to_owned)
            });
            self.inner().last_run = last_run;
        }
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
        let result = self.build_with(full, &context);
        drop(context);
        let _ = reporter.join();
        if let Err(error) = &result {
            let reason = if stop.is_requested() {
                "cancelled"
            } else {
                error.message.as_str()
            };
            let _ = block_on(self.store.fail(KEY, SOURCE_NAME, reason));
        }
        drop(lease);
        result
    }

    fn build_with(&self, full: bool, context: &Context) -> Result<Built, IndexError> {
        block_on(async {
            let source_status = SourceStatus {
                toolkit_id: SOURCE_NAME.to_owned(),
                toolkit_name: "Workspace folder".to_owned(),
                toolkit_type: "local_folder".to_owned(),
                branch: None,
            };
            let (mut graph, previous, stat_cache) = if full {
                (
                    Graph::new(),
                    BTreeMap::new(),
                    std::collections::HashMap::new(),
                )
            } else {
                let graph = self.store.load(KEY).await?.map(|(graph, _)| graph);
                (
                    graph.unwrap_or_default(),
                    self.store.document_versions(KEY, SOURCE_NAME).await?,
                    self.store.document_stats(KEY, SOURCE_NAME)?,
                )
            };
            self.store.start(KEY, &source_status).await?;
            let source = LocalFolderSource::new(Arc::clone(&self.workspace))
                .with_previous(stat_cache)
                .selecting(Selection::default());
            let outcome = ingest_documents(
                &mut graph,
                &SourceSelection::all(SOURCE_NAME),
                &source,
                self.workspace.root(),
                &previous,
                context,
            )
            .await
            .map_err(|error| IndexError::new("index_failed", error.message))?;
            context
                .checkpoint()
                .map_err(|error| IndexError::new("index_failed", error.message))?;
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
            let revision =
                self.store
                    .complete_with_stats(KEY, &graph, &completion, &source.stats())?;
            let report = RunReport {
                read: count(outcome.documents_processed),
                unchanged: count(outcome.unchanged),
                removed: count(outcome.removed_files),
                hashed: count(source.hashed()),
            };
            Ok::<_, IndexError>((
                Arc::new(GraphView::new(graph, revision)),
                count(outcome.documents.len()),
                report,
            ))
        })?
    }
}
