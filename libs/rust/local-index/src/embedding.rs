//! The background embedder: vectors for the search chunks of every
//! workspace whose `embeddings` setting names an embedder.
//!
//! Embeddings are OFF unless the person turns them on for a workspace
//! (setting [`SETTING`]: `off`, `local:<model id>`, or `remote` where the
//! policy allows it); a lexical index needs none. Once on, one thread,
//! shared by every workspace, embeds what has no vector yet:
//!
//! * **at low priority** — the thread lowers its own priority, the
//!   embedder runs its CPU work on a capped pool (the source's choice), and
//!   chunks go in batches of [`EmbedConfig::batch`] (32);
//! * **out of a turn's way** — between batches it waits while any turn
//!   runs ([`Embedding::turn`]);
//! * **on mains power only** — and not in Low Power Mode, unless the
//!   person asked for it ([`Embedding::force`]); [`Conditions`] says;
//! * **one model in memory** — loaded on first use, dropped after
//!   [`EmbedConfig::idle_unload`] (two minutes) without use.
//!
//! **Resumable.** Vectors are stored batch by batch, so a pass stopped
//! (the app quit, a turn, the power) loses at most one batch: the next pass
//! — after a restart too — embeds only the chunks that still have none, and
//! a refresh that changed some files only theirs.
//!
//! **What the agent works on first.** The files a turn touched or
//! `@`-mentioned ([`Embedding::prioritise`]) are embedded before the rest;
//! a pass under way takes them up at its next batch. Each status carries an
//! ETA from the pass's own throughput.
//!
//! Vectors are keyed by the chunk text's hash within a [`VectorSet`]
//! stamp (`origin:model:dim`): a refresh that keeps a chunk keeps its
//! vector, a chunk that changed gets a new one on the next pass, and the
//! vectors of another embedder (or of text no chunk has any more) are
//! pruned. Never in a build's transaction: a build in progress is waited
//! for.

use crate::dense::QueryEmbedder;
use crate::service::IndexService;
use crate::sqlite_store::VectorSet;
use elitea_local_embed::{EmbedError, Embedder, vector};
use serde::Serialize;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, MutexGuard, PoisonError, Weak};
use std::time::{Duration, Instant};

/// The store setting naming a workspace's embedder.
pub const SETTING: &str = "embeddings";

/// What embeddings are off as.
pub const OFF: &str = "off";

/// Where embedders come from (the desktop: the model cache and candle, or
/// the platform's embeddings route).
pub trait EmbedderSource: Send + Sync {
    /// The vector set `choice` (a [`SETTING`] value) makes, without
    /// loading anything; `None` when it is unknown or not allowed now (the
    /// policy turned remote embeddings off): its vectors are dropped.
    fn vector_set(&self, choice: &str) -> Option<VectorSet>;

    /// Load the embedder of `choice` for `workspace_id`'s index. Blocking;
    /// may take seconds.
    ///
    /// # Errors
    ///
    /// The model is not downloaded, does not verify, or does not load.
    fn load(&self, choice: &str, workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError>;

    /// Whether `choice`'s embedder is made per workspace and cheap to make
    /// (a remote one, bound to the workspace's project): it is made for
    /// every use and never takes the one model slot.
    fn per_workspace(&self, choice: &str) -> bool {
        let _ = choice;
        false
    }
}

/// Whether the computer is in a state to embed in the background.
pub trait Conditions: Send + Sync {
    /// Why embedding should wait now (`on_battery`, `low_power`); `None`:
    /// it may run.
    fn hold(&self) -> Option<&'static str>;
}

/// No conditions (tests; computers that cannot tell).
#[derive(Debug, Default, Clone, Copy)]
pub struct AlwaysRun;

impl Conditions for AlwaysRun {
    fn hold(&self) -> Option<&'static str> {
        None
    }
}

/// How the background embedder paces itself.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct EmbedConfig {
    /// Chunks per batch.
    pub batch: usize,
    /// A loaded model is dropped after this long unused.
    pub idle_unload: Duration,
    /// How often an idle embedder looks again (it is also woken).
    pub poll: Duration,
}

impl Default for EmbedConfig {
    fn default() -> Self {
        Self {
            batch: 32,
            idle_unload: Duration::from_mins(2),
            poll: Duration::from_secs(30),
        }
    }
}

/// What a workspace's embedding is doing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum EmbedState {
    /// Waiting for its turn (another workspace embeds, a build runs).
    Waiting,
    Running,
    /// Held: a turn runs, or [`EmbedStatus::held`] says why.
    Paused,
    /// Every chunk has a vector.
    Done,
    /// The last attempt failed; retried after [`EmbedConfig::poll`].
    Error,
}

/// A workspace's embedding progress.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct EmbedStatus {
    /// The [`SETTING`] value.
    pub choice: String,
    pub state: EmbedState,
    /// Distinct chunk texts with a vector, of how many.
    pub done: u64,
    pub total: u64,
    /// Why it is paused: `turn`, `on_battery`, `low_power`.
    pub held: Option<&'static str>,
    pub error: Option<String>,
    /// The person asked for it whatever the power conditions.
    pub forced: bool,
    /// Seconds left at this pass's throughput (while it runs).
    pub eta_seconds: Option<u64>,
}

/// The most recent paths kept per workspace for [`Embedding::prioritise`].
pub const MAX_PRIORITY_PATHS: usize = 200;

/// Where status changes go (`index://event` on the desktop).
pub type OnStatus = Arc<dyn Fn(&str, &EmbedStatus) + Send + Sync>;

struct Loaded {
    choice: String,
    embedder: Arc<dyn Embedder>,
    used: Instant,
}

#[derive(Default)]
struct State {
    services: BTreeMap<String, Weak<IndexService>>,
    forced: HashSet<String>,
    statuses: HashMap<String, EmbedStatus>,
    /// Not retried before this (after an error).
    retry_at: HashMap<String, Instant>,
    /// The paths to embed first, most recent first, per workspace.
    priorities: HashMap<String, Vec<String>>,
    /// Workspaces whose priorities changed since their pass was planned.
    reprioritised: HashSet<String>,
    woken: bool,
}

struct Shared {
    source: Arc<dyn EmbedderSource>,
    conditions: Arc<dyn Conditions>,
    config: EmbedConfig,
    on_status: OnStatus,
    state: Mutex<State>,
    wake: Condvar,
    /// The loaded model (its own lock: a query embedding never waits for
    /// the scheduler's state).
    model: Mutex<Option<Loaded>>,
    turns: AtomicUsize,
    stop: AtomicBool,
}

impl Shared {
    fn state(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }

    fn notify(&self) {
        self.state().woken = true;
        self.wake.notify_all();
    }

    /// Sleep up to `timeout`, or until woken / stopped.
    fn sleep(&self, timeout: Duration) {
        let guard = self.state();
        if guard.woken || self.stop.load(Ordering::SeqCst) {
            drop(guard);
            self.state().woken = false;
            return;
        }
        let (mut guard, _) = self
            .wake
            .wait_timeout_while(guard, timeout, |state| {
                !state.woken && !self.stop.load(Ordering::SeqCst)
            })
            .unwrap_or_else(PoisonError::into_inner);
        guard.woken = false;
    }

    fn set_status(&self, workspace_id: &str, status: &EmbedStatus) {
        let changed = {
            let mut state = self.state();
            let before = state
                .statuses
                .insert(workspace_id.to_owned(), status.clone());
            before.as_ref() != Some(status)
        };
        if changed {
            (self.on_status)(workspace_id, status);
        }
    }

    /// The embedder of `choice`, loaded if it is not (another one is
    /// dropped first: one model in memory).
    fn embedder(&self, choice: &str, workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        if self.source.per_workspace(choice) {
            return self.source.load(choice, workspace_id);
        }
        let mut model = self.model.lock().unwrap_or_else(PoisonError::into_inner);
        if let Some(loaded) = model.as_mut()
            && loaded.choice == choice
        {
            loaded.used = Instant::now();
            return Ok(Arc::clone(&loaded.embedder));
        }
        *model = None;
        let embedder = self.source.load(choice, workspace_id)?;
        *model = Some(Loaded {
            choice: choice.to_owned(),
            embedder: Arc::clone(&embedder),
            used: Instant::now(),
        });
        Ok(embedder)
    }

    /// Drop the model when unused for the idle period; how long until it
    /// would be.
    fn unload_idle(&self) -> Option<Duration> {
        let mut model = self.model.lock().unwrap_or_else(PoisonError::into_inner);
        let idle = model.as_ref()?.used.elapsed();
        if idle >= self.config.idle_unload {
            *model = None;
            None
        } else {
            Some(self.config.idle_unload.saturating_sub(idle))
        }
    }
}

impl QueryEmbedder for Shared {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        self.source.vector_set(choice)
    }

    fn embedder(&self, choice: &str, workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        Shared::embedder(self, choice, workspace_id)
    }
}

/// One pass's work: a workspace's chunks without a vector.
struct Job {
    workspace_id: String,
    service: Arc<IndexService>,
    choice: String,
    stamp: String,
    dim: usize,
    missing: Vec<(i64, String)>,
    done: u64,
    forced: bool,
}

/// The background embedder of every workspace; dropping it stops it.
pub struct Embedding {
    shared: Arc<Shared>,
    thread: Mutex<Option<std::thread::JoinHandle<()>>>,
}

impl std::fmt::Debug for Embedding {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Embedding").finish_non_exhaustive()
    }
}

/// Held while a turn runs: the embedder waits between batches.
#[derive(Debug)]
pub struct TurnGuard(Weak<Shared>);

impl std::fmt::Debug for Shared {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Shared").finish_non_exhaustive()
    }
}

impl Drop for TurnGuard {
    fn drop(&mut self) {
        if let Some(shared) = self.0.upgrade() {
            shared.turns.fetch_sub(1, Ordering::SeqCst);
            shared.notify();
        }
    }
}

impl Embedding {
    /// Start the embedder thread.
    ///
    /// # Errors
    ///
    /// The thread could not be started.
    pub fn start(
        source: Arc<dyn EmbedderSource>,
        conditions: Arc<dyn Conditions>,
        config: EmbedConfig,
        on_status: OnStatus,
    ) -> std::io::Result<Self> {
        let shared = Arc::new(Shared {
            source,
            conditions,
            config,
            on_status,
            state: Mutex::new(State::default()),
            wake: Condvar::new(),
            model: Mutex::new(None),
            turns: AtomicUsize::new(0),
            stop: AtomicBool::new(false),
        });
        let worker = Arc::clone(&shared);
        let thread = std::thread::Builder::new()
            .name("index-embed".into())
            .spawn(move || {
                // Utility work: below the app's threads. Not fatal if the
                // system refuses.
                let _ = thread_priority::set_current_thread_priority(
                    thread_priority::ThreadPriority::Min,
                );
                run(&worker);
            })?;
        Ok(Self {
            shared,
            thread: Mutex::new(Some(thread)),
        })
    }

    /// Embed `service`'s chunks whenever its setting names an embedder
    /// (looked at on every pass; the service is held weakly).
    pub fn add(&self, service: &Arc<IndexService>) {
        let queries: Weak<dyn QueryEmbedder> =
            Arc::downgrade(&self.shared) as Weak<dyn QueryEmbedder>;
        service.attach_query_embedder(queries);
        self.shared
            .state()
            .services
            .insert(service.workspace_id().to_owned(), Arc::downgrade(service));
        self.shared.notify();
    }

    /// Stop looking at a workspace (its index closed or removed).
    pub fn remove(&self, workspace_id: &str) {
        let mut state = self.shared.state();
        state.services.remove(workspace_id);
        state.forced.remove(workspace_id);
        state.statuses.remove(workspace_id);
        state.retry_at.remove(workspace_id);
        state.priorities.remove(workspace_id);
        state.reprioritised.remove(workspace_id);
    }

    /// Look again now (a build committed, a setting changed).
    pub fn wake(&self) {
        self.shared.state().retry_at.clear();
        self.shared.notify();
    }

    /// Embed the chunks of `paths` (a turn touched or `@`-mentioned them)
    /// before the workspace's others; the most recent go first, and a pass
    /// under way takes them up at its next batch.
    pub fn prioritise<I>(&self, workspace_id: &str, paths: I)
    where
        I: IntoIterator<Item = String>,
    {
        let fresh: Vec<String> = paths.into_iter().collect();
        if fresh.is_empty() {
            return;
        }
        {
            let mut state = self.shared.state();
            let list = state.priorities.entry(workspace_id.to_owned()).or_default();
            list.retain(|path| !fresh.contains(path));
            let mut merged = fresh;
            merged.append(list);
            merged.truncate(MAX_PRIORITY_PATHS);
            *list = merged;
            state.reprioritised.insert(workspace_id.to_owned());
        }
        self.shared.notify();
    }

    /// Embed this workspace now whatever the power conditions (the
    /// person's "rebuild"), until it is done.
    pub fn force(&self, workspace_id: &str) {
        {
            let mut state = self.shared.state();
            state.forced.insert(workspace_id.to_owned());
            state.retry_at.remove(workspace_id);
        }
        self.shared.notify();
    }

    /// Pause between batches while the guard lives (a turn runs).
    #[must_use]
    pub fn turn(&self) -> TurnGuard {
        self.shared.turns.fetch_add(1, Ordering::SeqCst);
        TurnGuard(Arc::downgrade(&self.shared))
    }

    /// The workspace's progress (`None`: nothing to report yet).
    #[must_use]
    pub fn status(&self, workspace_id: &str) -> Option<EmbedStatus> {
        self.shared.state().statuses.get(workspace_id).cloned()
    }

    /// The embedder of `choice` for a query, loaded if needed (sharing the
    /// background's model; it counts as a use).
    ///
    /// # Errors
    ///
    /// As [`EmbedderSource::load`].
    pub fn embedder(
        &self,
        choice: &str,
        workspace_id: &str,
    ) -> Result<Arc<dyn Embedder>, EmbedError> {
        self.shared.embedder(choice, workspace_id)
    }

    /// The vector set `choice` makes now (`None`: unknown or not allowed).
    #[must_use]
    pub fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        self.shared.source.vector_set(choice)
    }

    /// Whether a model is in memory (tests, the Doctor).
    #[must_use]
    pub fn model_loaded(&self) -> bool {
        self.shared
            .model
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .is_some()
    }
}

impl Drop for Embedding {
    fn drop(&mut self) {
        self.shared.stop.store(true, Ordering::SeqCst);
        self.shared.notify();
        let thread = self
            .thread
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .take();
        if let Some(thread) = thread {
            let _ = thread.join();
        }
    }
}

/// The workspace's [`SETTING`], `off` when unset or unreadable.
#[must_use]
pub fn choice_of(service: &IndexService) -> String {
    service
        .store()
        .setting(SETTING)
        .ok()
        .flatten()
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| OFF.to_owned())
}

fn run(shared: &Shared) {
    while !shared.stop.load(Ordering::SeqCst) {
        if let Some(job) = pick(shared) {
            work(shared, &job);
        } else {
            let until_unload = shared.unload_idle();
            let wait = until_unload.map_or(shared.config.poll, |left| left.min(shared.config.poll));
            shared.sleep(wait);
        }
    }
}

/// The next workspace with chunks to embed (pruning what each no longer
/// needs on the way).
fn pick(shared: &Shared) -> Option<Job> {
    let candidates: Vec<(String, Weak<IndexService>, bool)> = {
        let mut state = shared.state();
        state
            .services
            .retain(|_, service| service.strong_count() > 0);
        let now = Instant::now();
        let retry_at = state.retry_at.clone();
        let forced = state.forced.clone();
        state
            .services
            .iter()
            .filter(|(id, _)| retry_at.get(*id).is_none_or(|at| *at <= now))
            .map(|(id, service)| (id.clone(), service.clone(), forced.contains(id)))
            .collect()
    };
    // Forced workspaces first.
    let mut candidates = candidates;
    candidates.sort_by_key(|(_, _, forced)| !forced);
    for (workspace_id, service, forced) in candidates {
        let Some(service) = service.upgrade() else {
            continue;
        };
        if service.is_closed() || service.is_building() {
            continue;
        }
        let choice = choice_of(&service);
        let set = (choice != OFF)
            .then(|| shared.source.vector_set(&choice))
            .flatten();
        let Some(set) = set else {
            // Off (or not allowed now): no vectors are kept.
            if let Err(error) = service.store().prune_vectors(None) {
                log::warn!("index vectors could not be dropped: {error}");
            }
            let mut state = shared.state();
            state.statuses.remove(&workspace_id);
            state.forced.remove(&workspace_id);
            continue;
        };
        let stamp = set.stamp();
        let prepared = service
            .store()
            .prune_vectors(Some(&stamp))
            .and_then(|_| service.store().ensure_vector_set(&set))
            .and_then(|()| service.store().chunks_missing_vectors(&stamp))
            .and_then(|missing| {
                let done = service
                    .store()
                    .vector_sets()?
                    .into_iter()
                    .find(|(known, _)| known.stamp() == stamp)
                    .map_or(0, |(_, count)| count);
                Ok((missing, done))
            });
        let (mut missing, done) = match prepared {
            Ok(found) => found,
            Err(error) => {
                fail(
                    shared,
                    &workspace_id,
                    &choice,
                    forced,
                    0,
                    0,
                    &error.to_string(),
                );
                continue;
            }
        };
        put_first(shared, &service, &workspace_id, &mut missing);
        if missing.is_empty() {
            shared.state().forced.remove(&workspace_id);
            shared.set_status(
                &workspace_id,
                &EmbedStatus {
                    choice,
                    state: EmbedState::Done,
                    done,
                    total: done,
                    held: None,
                    error: None,
                    forced: false,
                    eta_seconds: None,
                },
            );
            continue;
        }
        return Some(Job {
            workspace_id,
            service,
            choice,
            stamp,
            dim: set.dim,
            missing,
            done,
            forced,
        });
    }
    None
}

fn fail(
    shared: &Shared,
    workspace_id: &str,
    choice: &str,
    forced: bool,
    done: u64,
    total: u64,
    error: &str,
) {
    log::warn!("embedding the index of a workspace failed: {error}");
    shared
        .state()
        .retry_at
        .insert(workspace_id.to_owned(), Instant::now() + shared.config.poll);
    shared.set_status(
        workspace_id,
        &EmbedStatus {
            choice: choice.to_owned(),
            state: EmbedState::Error,
            done,
            total,
            held: None,
            error: Some(error.to_owned()),
            forced,
            eta_seconds: None,
        },
    );
}

/// Put the chunks of the workspace's priority paths first (in their
/// order; the rest keep theirs), and mark its priorities as applied.
fn put_first(
    shared: &Shared,
    service: &IndexService,
    workspace_id: &str,
    missing: &mut [(i64, String)],
) {
    let priorities = {
        let mut state = shared.state();
        state.reprioritised.remove(workspace_id);
        state
            .priorities
            .get(workspace_id)
            .cloned()
            .unwrap_or_default()
    };
    if priorities.is_empty() || missing.is_empty() {
        return;
    }
    match service.store().chunk_hashes_of(&priorities) {
        Ok(ranks) => {
            missing.sort_by_key(|(_, hash)| ranks.get(hash).copied().unwrap_or(usize::MAX));
        }
        Err(error) => log::warn!("the index's priorities could not be read: {error}"),
    }
}

/// Seconds left for `left` texts at `embedded` texts per `busy`.
#[allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss
)]
fn seconds_left(left: u64, embedded: u64, busy: Duration) -> Option<u64> {
    let seconds = busy.as_secs_f64();
    (embedded > 0 && seconds > 0.0).then(|| (left as f64 * seconds / embedded as f64).ceil() as u64)
}

/// Embed a job's chunks batch by batch, holding for turns and power.
fn work(shared: &Shared, job: &Job) {
    let total = job.done + job.missing.len() as u64;
    let mut done = job.done;
    // Throughput of this pass, over the time spent embedding (holds and
    // waits are not counted).
    let (mut embedded, mut busy) = (0_u64, Duration::ZERO);
    let eta = |embedded: u64, busy: Duration, done: u64| {
        seconds_left(total.saturating_sub(done), embedded, busy)
    };
    let status = |state, held, done, eta_seconds| EmbedStatus {
        choice: job.choice.clone(),
        state,
        done,
        total,
        held,
        error: None,
        forced: job.forced,
        eta_seconds,
    };
    for batch in job.missing.chunks(shared.config.batch.max(1)) {
        // Between batches: stop, a turn, a build, the setting, power.
        loop {
            if shared.stop.load(Ordering::SeqCst) {
                return;
            }
            if shared.turns.load(Ordering::SeqCst) == 0 {
                break;
            }
            shared.set_status(
                &job.workspace_id,
                &status(EmbedState::Paused, Some("turn"), done, None),
            );
            shared.sleep(Duration::from_millis(500));
        }
        if job.service.is_closed()
            || job.service.is_building()
            || choice_of(&job.service) != job.choice
        {
            shared.set_status(
                &job.workspace_id,
                &status(EmbedState::Waiting, None, done, None),
            );
            return;
        }
        if shared.state().reprioritised.contains(&job.workspace_id) {
            // Planned again with the new priorities first (at once).
            shared.notify();
            return;
        }
        let forced = job.forced || shared.state().forced.contains(&job.workspace_id);
        if !forced && let Some(reason) = shared.conditions.hold() {
            shared.set_status(
                &job.workspace_id,
                &status(EmbedState::Paused, Some(reason), done, None),
            );
            shared.sleep(shared.config.poll);
            return;
        }
        shared.set_status(
            &job.workspace_id,
            &status(EmbedState::Running, None, done, eta(embedded, busy, done)),
        );
        let started = Instant::now();
        match embed_batch(shared, job, batch) {
            Ok(n) => {
                done += n;
                embedded += n;
                busy += started.elapsed();
            }
            Err(error) => {
                fail(
                    shared,
                    &job.workspace_id,
                    &job.choice,
                    job.forced,
                    done,
                    total,
                    &error,
                );
                return;
            }
        }
    }
    // Looked at again at once: Done, or what changed meanwhile.
    shared.notify();
}

/// One batch: read the texts, embed, store. Returns the vectors written.
fn embed_batch(shared: &Shared, job: &Job, batch: &[(i64, String)]) -> Result<u64, String> {
    let ids: Vec<i64> = batch.iter().map(|(id, _)| *id).collect();
    let texts: HashMap<i64, String> = job
        .service
        .store()
        .chunks_by_id(&ids)
        .map_err(|error| error.to_string())?
        .into_iter()
        .map(|chunk| (chunk.id, chunk.text))
        .collect();
    // Chunks a build replaced meanwhile are skipped (the next pass sees
    // their successors).
    let present: Vec<&(i64, String)> = batch
        .iter()
        .filter(|(id, _)| texts.contains_key(id))
        .collect();
    if present.is_empty() {
        return Ok(0);
    }
    let inputs: Vec<&str> = present.iter().map(|(id, _)| texts[id].as_str()).collect();
    let embedder = shared
        .embedder(&job.choice, &job.workspace_id)
        .map_err(|error| error.to_string())?;
    let vectors = embedder
        .embed_documents(&inputs)
        .map_err(|error| error.to_string())?;
    if vectors.len() != present.len() {
        return Err(format!(
            "the embedder answered {} vectors for {} texts",
            vectors.len(),
            present.len()
        ));
    }
    let mut rows = Vec::with_capacity(vectors.len());
    for ((_, hash), values) in present.iter().zip(vectors) {
        if values.len() != job.dim {
            return Err(format!(
                "the embedder answered {} dimensions, expected {}",
                values.len(),
                job.dim
            ));
        }
        rows.push((hash.clone(), vector::quantize(&values)));
    }
    job.service
        .store()
        .put_vectors(&job.stamp, &rows)
        .map_err(|error| error.to_string())?;
    Ok(rows.len() as u64)
}
