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
//! **What it may cost.** A remote embedder is paid from the project's
//! budget ([`crate::embed_caps`]): a run stops at the per-run cap or the
//! per-day cap ([`EmbedState::Capped`]), and at a `402` from the gateway
//! ([`EmbedState::BudgetExhausted`]). Neither a run cap nor a `402` is
//! retried until the person asks again (embed now, choosing the embedder
//! anew, or — for a run cap — changing the caps): a build, a wake or the
//! ten-minute sweep does not restart paid work, and a spent budget does not
//! come back by waiting ten minutes. A day cap is looked at again hourly
//! and goes on the next UTC day. A text larger than the smaller cap is
//! never sent (no run could hold it) and does not hold the others back. A
//! `429` is backed off inside the embedder, bounded and cancellable; when
//! the bound is passed the run ends in `Error` and is tried again after the
//! service's `Retry-After`, which only the person's own action cuts short.
//! Every run is recorded in `runs` with the tokens it used.
//!
//! Vectors are keyed by the chunk text's hash within a [`VectorSet`]
//! stamp (`origin:model:dim`): a refresh that keeps a chunk keeps its
//! vector, a chunk that changed gets a new one on the next pass, and the
//! vectors of another embedder (or of text no chunk has any more) are
//! pruned. Never in a build's transaction: a build in progress is waited
//! for.

use crate::dense::QueryEmbedder;
use crate::embed_caps::{self, Cap, EmbedCaps};
use crate::service::IndexService;
use crate::sqlite_store::{StoreError, VectorSet};
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
    /// The last attempt failed; retried after [`EmbedConfig::poll`] (or
    /// the rate limit's `Retry-After`).
    Error,
    /// A token cap stopped the run ([`EmbedStatus::cap`]); what it embedded
    /// is kept. A day cap goes on the next day; a run cap when the person
    /// asks again.
    Capped,
    /// The deployment's budget is spent (HTTP 402): nothing more is sent
    /// until the person asks again.
    BudgetExhausted,
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
    /// Estimated tokens this run has embedded (zero for a state that is not
    /// a run's).
    pub tokens: u64,
    /// The cap that stopped it (`run` or `day`) when [`EmbedState::Capped`].
    pub cap: Option<&'static str>,
}

/// The most recent paths kept per workspace for [`Embedding::prioritise`].
pub const MAX_PRIORITY_PATHS: usize = 200;

/// Where status changes go (`index://event` on the desktop).
pub type OnStatus = Arc<dyn Fn(&str, &EmbedStatus) + Send + Sync>;

/// When a workspace may be tried again.
#[derive(Debug, Clone, Copy)]
struct Retry {
    at: Instant,
    /// The gateway's `Retry-After`: only the person's own action (embed
    /// now, choosing the embedder) cuts it short, not a build or a wake.
    rate_limited: bool,
}

/// Why a workspace waits for the person.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Halt {
    /// The budget is spent (HTTP 402).
    Budget,
    /// The run cap stopped the last run.
    RunCap,
}

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
    /// Not retried before this (after an error, a rate limit, the day cap).
    retry_at: HashMap<String, Retry>,
    /// The paths to embed first, most recent first, per workspace.
    priorities: HashMap<String, Vec<String>>,
    /// Workspaces whose priorities changed since their pass was planned.
    reprioritised: HashSet<String>,
    /// Workspaces waiting for the person (a spent budget, a run cap):
    /// skipped, even by the sweep and a build's wake, until the person
    /// forces them or chooses an embedder again (a run cap: or changes the
    /// caps).
    halted: HashMap<String, Halt>,
    /// Workspaces to look at: added, woken (a build committed, a setting
    /// changed), forced, reprioritised, or with a pass under way or to
    /// retry. One that is done or off is not looked at again (no prune, no
    /// scan of its chunks) until something marks it.
    dirty: HashSet<String>,
    /// Bumped by every mark (add, wake, force, reprioritise). A pass that
    /// planned before a mark must not clear it: a wake that lands while
    /// `pick` is still reading the old setting would otherwise be erased by
    /// that stale "off" or "done", and the workspace would wait for the
    /// ten-minute sweep.
    marks: u64,
    /// When every workspace was last marked (a slow sweep: the policy may
    /// have changed what is allowed without telling anyone).
    swept: Option<Instant>,
    woken: bool,
}

impl State {
    /// A pass planned at `marks` found the workspace done or off: it is not
    /// looked at again until something marks it, unless something already
    /// did while the pass was planning (then it stays dirty, and the next
    /// pass reads the new setting).
    fn settle(&mut self, workspace_id: &str, marks: u64) {
        if self.marks == marks {
            self.dirty.remove(workspace_id);
        }
    }
}

/// How often every workspace is looked at again, dirty or not.
const SWEEP_EVERY: Duration = Duration::from_mins(10);

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
            // Removed (turned off, deleted) while a pass was under way: its
            // last batch reports nothing.
            if !state.services.contains_key(workspace_id) {
                return;
            }
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
    /// Texts larger than the smaller cap: never sent, counted in the total.
    skipped: u64,
    forced: bool,
    /// Paid for per token (a remote embedder): held to the caps.
    metered: bool,
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
        {
            let mut state = self.shared.state();
            let id = service.workspace_id().to_owned();
            state.services.insert(id.clone(), Arc::downgrade(service));
            state.dirty.insert(id);
            state.marks += 1;
        }
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
        state.halted.remove(workspace_id);
        state.dirty.remove(workspace_id);
    }

    /// Look at every workspace again now (a build committed, a setting
    /// changed). A workspace waiting for the person, or for the gateway's
    /// `Retry-After`, keeps waiting.
    pub fn wake(&self) {
        {
            let mut state = self.shared.state();
            state.retry_at.retain(|_, retry| retry.rate_limited);
            let all: Vec<String> = state.services.keys().cloned().collect();
            state.dirty.extend(all);
            state.marks += 1;
        }
        self.shared.notify();
    }

    /// Look at one workspace again now (its build committed). One waiting
    /// for the person ([`EmbedState::BudgetExhausted`], a run cap), or for
    /// the gateway's `Retry-After`, keeps waiting: a file edit must not
    /// restart paid embedding.
    pub fn wake_workspace(&self, workspace_id: &str) {
        {
            let mut state = self.shared.state();
            if state
                .retry_at
                .get(workspace_id)
                .is_some_and(|retry| !retry.rate_limited)
            {
                state.retry_at.remove(workspace_id);
            }
            state.dirty.insert(workspace_id.to_owned());
            state.marks += 1;
        }
        self.shared.notify();
    }

    /// The person changed the workspace's caps: a run a cap stopped looks
    /// again at once (a spent budget and a `Retry-After` still wait).
    pub fn caps_changed(&self, workspace_id: &str) {
        {
            let mut state = self.shared.state();
            if state.halted.get(workspace_id) == Some(&Halt::RunCap) {
                state.halted.remove(workspace_id);
            }
            if state
                .retry_at
                .get(workspace_id)
                .is_some_and(|retry| !retry.rate_limited)
            {
                state.retry_at.remove(workspace_id);
            }
            state.dirty.insert(workspace_id.to_owned());
            state.marks += 1;
        }
        self.shared.notify();
    }

    /// The person chose the workspace's embedder (anew): whatever it waited
    /// for, it is looked at again now (power conditions still hold it).
    pub fn ask_again(&self, workspace_id: &str) {
        {
            let mut state = self.shared.state();
            state.retry_at.remove(workspace_id);
            state.halted.remove(workspace_id);
            state.dirty.insert(workspace_id.to_owned());
            state.marks += 1;
        }
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
            state.dirty.insert(workspace_id.to_owned());
            state.marks += 1;
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
            state.halted.remove(workspace_id);
            state.dirty.insert(workspace_id.to_owned());
            state.marks += 1;
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

/// The workspaces to look at now (dirty, not waiting for a retry), each
/// with whether it is forced; every one is marked dirty once per sweep.
fn candidates(shared: &Shared) -> (u64, Vec<(String, Weak<IndexService>, bool)>) {
    let mut state = shared.state();
    state
        .services
        .retain(|_, service| service.strong_count() > 0);
    let now = Instant::now();
    if state
        .swept
        .is_none_or(|at| now.duration_since(at) >= SWEEP_EVERY)
    {
        state.swept = Some(now);
        let all: Vec<String> = state.services.keys().cloned().collect();
        state.dirty.extend(all);
    }
    let retry_at = state.retry_at.clone();
    let forced = state.forced.clone();
    let dirty = state.dirty.clone();
    let halted = state.halted.clone();
    let services = state
        .services
        .iter()
        .filter(|(id, _)| dirty.contains(*id))
        .filter(|(id, _)| !halted.contains_key(*id))
        .filter(|(id, _)| retry_at.get(*id).is_none_or(|retry| retry.at <= now))
        .map(|(id, service)| (id.clone(), service.clone(), forced.contains(id)))
        .collect();
    (state.marks, services)
}

/// The next workspace with chunks to embed (pruning what each no longer
/// needs on the way); a workspace found done or off is not dirty any more.
fn pick(shared: &Shared) -> Option<Job> {
    let (marks, mut candidates) = candidates(shared);
    // Forced workspaces first.
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
            state.settle(&workspace_id, marks);
            continue;
        };
        let stamp = set.stamp();
        let metered = set.origin == REMOTE_ORIGIN;
        let prepared = prepare(&service, &set, &stamp, metered);
        let (mut missing, done, skipped) = match prepared {
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
        if skipped > 0 {
            log::info!(
                "{skipped} index texts are larger than the embedding token cap and are not sent"
            );
        }
        put_first(shared, &service, &workspace_id, &mut missing);
        if missing.is_empty() {
            {
                let mut state = shared.state();
                state.forced.remove(&workspace_id);
                // Done: not looked at again until something changes (unless
                // something did while this pass was planning).
                state.settle(&workspace_id, marks);
            }
            shared.set_status(
                &workspace_id,
                &EmbedStatus {
                    choice,
                    state: EmbedState::Done,
                    done,
                    total: done + skipped,
                    held: None,
                    error: None,
                    forced: false,
                    eta_seconds: None,
                    tokens: 0,
                    cap: None,
                },
            );
            continue;
        }
        let total = done + missing.len() as u64 + skipped;
        if metered && day_cap_reached(shared, &service, &workspace_id, &choice, done, total) {
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
            skipped,
            forced,
            metered,
        });
    }
    None
}

/// A planned pass: the chunk texts without a vector, the vectors the set
/// has, and how many texts are too large to ever send.
type Plan = (Vec<(i64, String)>, u64, u64);

/// A workspace's pass, planned: the vectors of other sets pruned, and its
/// chunk texts without a vector in `stamp`, the vectors it has, and how many
/// texts are too large to ever send.
fn prepare(
    service: &IndexService,
    set: &VectorSet,
    stamp: &str,
    metered: bool,
) -> Result<Plan, StoreError> {
    let store = service.store();
    store.prune_vectors(Some(stamp))?;
    store.ensure_vector_set(set)?;
    let mut missing = store.chunks_missing_vectors(stamp)?;
    // A text larger than the smaller cap could never be sent: a fresh run
    // would stop at it with nothing embedded, every time, and hold back
    // every text after it. It is left without a vector until the caps are
    // raised.
    let mut skipped = 0;
    if metered {
        let caps = EmbedCaps::load(store);
        let over = store.chunk_hashes_over(caps.run.min(caps.day))?;
        let before = missing.len();
        missing.retain(|(_, hash)| !over.contains(hash));
        skipped = (before - missing.len()) as u64;
    }
    let done = store
        .vector_sets()?
        .into_iter()
        .find(|(known, _)| known.stamp() == stamp)
        .map_or(0, |(_, count)| count);
    Ok((missing, done, skipped))
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
    fail_after(
        shared,
        (workspace_id, choice, forced),
        (done, total),
        error,
        None,
    );
}

/// [`fail`], tried again after [`EmbedConfig::poll`], or after the
/// gateway's `Retry-After` (at least the poll) when `rate_limit` names one.
fn fail_after(
    shared: &Shared,
    (workspace_id, choice, forced): (&str, &str, bool),
    (done, total): (u64, u64),
    error: &str,
    rate_limit: Option<Duration>,
) {
    log::warn!("embedding the index of a workspace failed: {error}");
    let delay = rate_limit.unwrap_or(shared.config.poll);
    shared.state().retry_at.insert(
        workspace_id.to_owned(),
        Retry {
            at: Instant::now() + delay.max(shared.config.poll),
            rate_limited: rate_limit.is_some(),
        },
    );
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
            tokens: 0,
            cap: None,
        },
    );
}

/// The origin of the vector sets a remote embedder makes (the deployment's
/// route, paid per token).
const REMOTE_ORIGIN: &str = "remote";

/// How long a workspace that reached today's cap waits before it looks at
/// the day again (the counter starts over at 00:00 UTC).
const DAY_CAP_RECHECK: Duration = Duration::from_hours(1);

/// A metered workspace that has already used its day: reported `Capped`
/// and looked at again in an hour, without a run being started. The
/// person's "Embed now" does not get around it (raise the cap instead).
fn day_cap_reached(
    shared: &Shared,
    service: &IndexService,
    workspace_id: &str,
    choice: &str,
    done: u64,
    total: u64,
) -> bool {
    let caps = EmbedCaps::load(service.store());
    let used = service.store().embed_tokens_today().unwrap_or(0);
    if used < caps.day {
        return false;
    }
    shared.state().retry_at.insert(
        workspace_id.to_owned(),
        Retry {
            at: Instant::now() + DAY_CAP_RECHECK.max(shared.config.poll),
            rate_limited: false,
        },
    );
    shared.set_status(
        workspace_id,
        &EmbedStatus {
            choice: choice.to_owned(),
            state: EmbedState::Capped,
            done,
            total,
            held: None,
            error: None,
            forced: false,
            eta_seconds: None,
            tokens: 0,
            cap: Some(Cap::Day.name()),
        },
    );
    true
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

/// Why a batch ended the run.
enum Stop {
    /// A token cap: nothing more may be spent now.
    Capped(Cap),
    /// HTTP 402.
    Exhausted(String),
    /// HTTP 429 past the embedder's bounded backoff.
    RateLimited(Duration, String),
    /// A turn started or the index closed while the embedder waited.
    Paused,
    Failed(String),
}

impl From<EmbedError> for Stop {
    fn from(error: EmbedError) -> Self {
        match error {
            EmbedError::BudgetExhausted(message) => Self::Exhausted(message),
            EmbedError::RateLimited { retry_after } => Self::RateLimited(
                retry_after,
                EmbedError::RateLimited { retry_after }.to_string(),
            ),
            EmbedError::Paused => Self::Paused,
            other => Self::Failed(other.to_string()),
        }
    }
}

/// What one batch embedded.
struct Batch {
    vectors: u64,
    tokens: u64,
    /// The budget cut the batch short: the rest is for a later run.
    cut: Option<Cap>,
}

/// One pass's row in `runs`: created when its first batch is stored, grows
/// with every batch, and ends however the pass does (`stopped` unless the
/// pass says otherwise).
struct RunLog<'a> {
    job: &'a Job,
    id: Option<i64>,
    tokens: u64,
    ending: &'static str,
    error: Option<String>,
}

impl<'a> RunLog<'a> {
    const fn new(job: &'a Job) -> Self {
        Self {
            job,
            id: None,
            tokens: 0,
            ending: "stopped",
            error: None,
        }
    }

    /// A batch of `tokens` is stored.
    fn batch(&mut self, tokens: u64) {
        self.tokens += tokens;
        match self
            .job
            .service
            .store()
            .embedding_batch_done(self.id, tokens, self.job.metered)
        {
            Ok(id) => self.id = Some(id),
            Err(error) => log::warn!("an embedding run could not be recorded: {error}"),
        }
    }

    fn end(&mut self, ending: &'static str, error: Option<String>) {
        self.ending = ending;
        self.error = error;
    }
}

impl Drop for RunLog<'_> {
    fn drop(&mut self) {
        if let Some(id) = self.id
            && let Err(error) = self.job.service.store().finish_embedding_run(
                id,
                self.ending,
                self.error.as_deref(),
            )
        {
            log::warn!("an embedding run could not be closed: {error}");
        }
    }
}

/// Embed a job's chunks batch by batch, holding for turns and power.
fn work(shared: &Shared, job: &Job) {
    let total = job.done + job.missing.len() as u64 + job.skipped;
    let mut done = job.done;
    let mut run = RunLog::new(job);
    // Throughput of this pass, over the time spent embedding (holds and
    // waits are not counted).
    let (mut embedded, mut busy) = (0_u64, Duration::ZERO);
    let eta = |embedded: u64, busy: Duration, done: u64| {
        seconds_left(total.saturating_sub(done), embedded, busy)
    };
    let status = |state, held, done, eta_seconds, tokens| EmbedStatus {
        choice: job.choice.clone(),
        state,
        done,
        total,
        held,
        error: None,
        forced: job.forced,
        eta_seconds,
        tokens,
        cap: None,
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
                &status(EmbedState::Paused, Some("turn"), done, None, run.tokens),
            );
            shared.sleep(Duration::from_millis(500));
        }
        if job.service.is_closed()
            || job.service.is_building()
            || choice_of(&job.service) != job.choice
        {
            shared.set_status(
                &job.workspace_id,
                &status(EmbedState::Waiting, None, done, None, run.tokens),
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
                &status(EmbedState::Paused, Some(reason), done, None, run.tokens),
            );
            shared.sleep(shared.config.poll);
            return;
        }
        shared.set_status(
            &job.workspace_id,
            &status(
                EmbedState::Running,
                None,
                done,
                eta(embedded, busy, done),
                run.tokens,
            ),
        );
        // What the caps leave (a read per batch: a new day, or a cap the
        // person raised, counts at once).
        let budget = job.metered.then(|| {
            let today = job.service.store().embed_tokens_today().unwrap_or(0);
            EmbedCaps::load(job.service.store()).remaining(run.tokens, today)
        });
        let started = Instant::now();
        match embed_batch(shared, job, batch, budget) {
            Ok(batch) => {
                done += batch.vectors;
                embedded += batch.vectors;
                busy += started.elapsed();
                if batch.vectors > 0 {
                    run.batch(batch.tokens);
                }
                if let Some(cap) = batch.cut {
                    stop_capped(shared, job, &mut run, total, done, cap);
                    return;
                }
            }
            Err(stop) => {
                stop_for(shared, job, &mut run, (done, total), stop);
                return;
            }
        }
    }
    run.end("embedded", None);
    // Looked at again at once: Done, or what changed meanwhile.
    shared.notify();
}

/// A cap ended the run: reported, and looked at again later. A day cap
/// hourly (it goes on when the UTC day is over); a run cap not until the
/// person asks again ("Embed now", or changing the caps): the run cap is
/// what one unattended run may spend, so the next run is the person's call.
fn stop_capped(shared: &Shared, job: &Job, run: &mut RunLog<'_>, total: u64, done: u64, cap: Cap) {
    run.end("capped", None);
    let forced = {
        let mut state = shared.state();
        match cap {
            Cap::Day => {
                state.retry_at.insert(
                    job.workspace_id.clone(),
                    Retry {
                        at: Instant::now() + DAY_CAP_RECHECK.max(shared.config.poll),
                        rate_limited: false,
                    },
                );
                job.forced
            }
            Cap::Run => {
                state.halted.insert(job.workspace_id.clone(), Halt::RunCap);
                state.forced.remove(&job.workspace_id);
                false
            }
        }
    };
    shared.set_status(
        &job.workspace_id,
        &EmbedStatus {
            choice: job.choice.clone(),
            state: EmbedState::Capped,
            done,
            total,
            held: None,
            error: None,
            forced,
            eta_seconds: None,
            tokens: run.tokens,
            cap: Some(cap.name()),
        },
    );
}

/// A batch failed or was refused: what the run does next depends on why.
fn stop_for(
    shared: &Shared,
    job: &Job,
    run: &mut RunLog<'_>,
    (done, total): (u64, u64),
    stop: Stop,
) {
    let who = (job.workspace_id.as_str(), job.choice.as_str(), job.forced);
    match stop {
        Stop::Capped(cap) => stop_capped(shared, job, run, total, done, cap),
        Stop::Exhausted(message) => {
            // Not retried by the clock: a spent budget needs the person (or
            // an administrator) to act, and "Embed now" tries again. A
            // forced run is no longer forced: the person's earlier "Embed
            // now" is answered, and a forced workspace must not loop on 402s.
            log::warn!("the embeddings budget of a workspace is exhausted: {message}");
            run.end("budget_exhausted", Some(message.clone()));
            {
                let mut state = shared.state();
                state.halted.insert(job.workspace_id.clone(), Halt::Budget);
                state.forced.remove(&job.workspace_id);
            }
            shared.set_status(
                &job.workspace_id,
                &EmbedStatus {
                    choice: job.choice.clone(),
                    state: EmbedState::BudgetExhausted,
                    done,
                    total,
                    held: None,
                    error: Some(message),
                    forced: false,
                    eta_seconds: None,
                    tokens: run.tokens,
                    cap: None,
                },
            );
        }
        Stop::RateLimited(retry_after, message) => {
            run.end("rate_limited", Some(message.clone()));
            fail_after(shared, who, (done, total), &message, Some(retry_after));
        }
        Stop::Paused => {
            shared.set_status(
                &job.workspace_id,
                &EmbedStatus {
                    choice: job.choice.clone(),
                    state: EmbedState::Waiting,
                    done,
                    total,
                    held: None,
                    error: None,
                    forced: job.forced,
                    eta_seconds: None,
                    tokens: run.tokens,
                    cap: None,
                },
            );
            shared.notify();
        }
        Stop::Failed(message) => {
            run.end("error", Some(message.clone()));
            fail(shared, who.0, who.1, who.2, done, total, &message);
        }
    }
}

/// One batch: read the texts, embed, store. `budget` is what the caps leave
/// (the texts that do not fit are left for a later run).
fn embed_batch(
    shared: &Shared,
    job: &Job,
    batch: &[(i64, String)],
    budget: Option<(u64, Cap)>,
) -> Result<Batch, Stop> {
    let failed = |error: &dyn std::fmt::Display| Stop::Failed(error.to_string());
    let ids: Vec<i64> = batch.iter().map(|(id, _)| *id).collect();
    let texts: HashMap<i64, (String, String)> = job
        .service
        .store()
        .chunks_by_id(&ids)
        .map_err(|error| failed(&error))?
        .into_iter()
        .map(|chunk| (chunk.id, (chunk.text, chunk.text_hash)))
        .collect();
    // Chunks a build replaced meanwhile are skipped: gone, or an id reused
    // by another text (chunk ids are rowids, reused once the highest are
    // deleted). The next pass sees their successors. A vector is only ever
    // stored under the hash of the text it was made from.
    let mut present: Vec<(&str, &str)> = batch
        .iter()
        .filter_map(|(id, planned)| {
            let (text, hash) = texts.get(id)?;
            (hash == planned).then_some((text.as_str(), hash.as_str()))
        })
        .collect();
    if present.is_empty() {
        return Ok(Batch {
            vectors: 0,
            tokens: 0,
            cut: None,
        });
    }
    let mut cut = None;
    if let Some((left, cap)) = budget {
        let inputs: Vec<&str> = present.iter().map(|(text, _)| *text).collect();
        let fits = embed_caps::fit(&inputs, left);
        if fits == 0 {
            return Err(Stop::Capped(cap));
        }
        if fits < present.len() {
            present.truncate(fits);
            cut = Some(cap);
        }
    }
    let inputs: Vec<&str> = present.iter().map(|(text, _)| *text).collect();
    let tokens = inputs
        .iter()
        .map(|text| embed_caps::estimate_tokens(text))
        .sum();
    let embedder = shared
        .embedder(&job.choice, &job.workspace_id)
        .map_err(Stop::from)?;
    // A turn that starts while the embedder backs off a rate limit takes
    // precedence, as it does between batches.
    let stop = || shared.stop.load(Ordering::SeqCst) || shared.turns.load(Ordering::SeqCst) > 0;
    let vectors = embedder
        .embed_documents_until(&inputs, &stop)
        .map_err(Stop::from)?;
    if vectors.len() != present.len() {
        return Err(Stop::Failed(format!(
            "the embedder answered {} vectors for {} texts",
            vectors.len(),
            present.len()
        )));
    }
    let mut rows = Vec::with_capacity(vectors.len());
    for ((_, hash), values) in present.iter().zip(vectors) {
        if values.len() != job.dim {
            return Err(Stop::Failed(format!(
                "the embedder answered {} dimensions, expected {}",
                values.len(),
                job.dim
            )));
        }
        rows.push(((*hash).to_owned(), vector::quantize(&values)));
    }
    job.service
        .store()
        .put_vectors(&job.stamp, &rows)
        .map_err(|error| failed(&error))?;
    Ok(Batch {
        vectors: rows.len() as u64,
        tokens,
        cut,
    })
}

#[cfg(test)]
mod tests {
    use super::State;

    // The CI flake behind `vectors_join_the_search_once_they_exist`: the
    // pass read the setting as "off", the test turned embedding on and woke
    // the scheduler, and the stale "off" then cleared the mark the wake had
    // just set, so nothing looked at the workspace again for ten minutes.
    #[test]
    fn a_mark_made_while_a_pass_planned_survives_that_pass() {
        let mut state = State::default();
        state.dirty.insert("ws".to_owned());
        let planned_at = state.marks;
        // wake(): marks the workspace again while the pass is planning.
        state.dirty.insert("ws".to_owned());
        state.marks += 1;
        state.settle("ws", planned_at);
        assert!(state.dirty.contains("ws"), "the wake was lost");
        // The next pass plans after the wake and settles it.
        let planned_at = state.marks;
        state.settle("ws", planned_at);
        assert!(!state.dirty.contains("ws"));
    }
}
